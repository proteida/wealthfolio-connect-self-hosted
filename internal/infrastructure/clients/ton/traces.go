package ton

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
)

// This file verifies decoded actions against raw TON Center trace trees so
// staking deposits, vault deposits and other DeFi conversions are paired by
// causality instead of by amount or timestamp similarity.
//
// A trace is causal by construction: every transaction in it exists only
// because a message chain led to it. Correlation therefore keys on the
// message tree (in_msg/out_msgs edges walked downstream from the wallet's
// outflow) within one trace_id. Amounts and timestamps are never comparison
// inputs; they only flow into quantities and pricing after a link is proven.
// Async receipts that settle in a different trace (e.g. epoch vault mints)
// have no causal edge and are deliberately kept separate.
//
// Primary data comes from TON Center: GET /traces?tx_hash=...&include_actions
// (message tree plus decoded actions) and GET /actions?tx_hash=...&
// include_transactions=true (per-transaction decoded detail). TonAPI is never
// consulted here; it only prices legs afterwards.

// maxTraceEnrich caps per-wallet trace verification calls so one deep wallet
// cannot stall the sync. Hitting the cap degrades to interpreted legs.
const maxTraceEnrich = 200

// traceMsg is one raw message edge: parent out_msgs link to child in_msg by
// hash, with source/destination accounts for attribution. TON Center v3
// embeds fully decoded payloads for known opcodes, so token amounts ride
// along without any hand BOC decoding.
type traceMsg struct {
	Hash        string         `json:"hash"`
	Source      string         `json:"source"`
	Destination string         `json:"destination"`
	Value       string         `json:"value"`
	Content     messageContent `json:"message_content"`
}

// messageContent carries the indexer's decoded payload, if any.
type messageContent struct {
	Decoded decodedBody `json:"decoded"`
}

// decodedBody is a decoded jetton_internal_transfer payload. Amount is raw
// units; ResponseAddress attributes the mint to its beneficiary.
type decodedBody struct {
	Type            string `json:"@type"`
	QueryID         string `json:"query_id"`
	Amount          string `json:"amount"`
	From            string `json:"from"`
	ResponseAddress string `json:"response_address"`
}

// UnmarshalJSON flattens TON Center's typed wrappers (var_uint, addr_std)
// into plain strings, tolerating absent, string or differently shaped
// fields so zero values round-trip and API drift degrades to empty.
func (d *decodedBody) UnmarshalJSON(raw []byte) error {
	var env struct {
		Type            string `json:"@type"`
		QueryID         string `json:"query_id"`
		Amount          any    `json:"amount"`
		From            any    `json:"from"`
		ResponseAddress any    `json:"response_address"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	d.Type, d.QueryID = env.Type, env.QueryID
	d.Amount = stringField(env.Amount, "value")
	d.From = addrField(env.From)
	d.ResponseAddress = addrField(env.ResponseAddress)
	return nil
}

// stringField reads a var_uint wrapper or a bare string.
func stringField(v any, key string) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		s, ok := t[key].(string)
		if !ok {
			return ""
		}
		return s
	default:
		return ""
	}
}

// addrField reads a decoded addr_* wrapper into raw form, passing bare
// strings through.
func addrField(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		return stdAddress(t)
	default:
		return ""
	}
}

// stdAddress reads a decoded addr_std wrapper into raw form
// (workchain:hex). Absent or empty wrappers yield "".
func stdAddress(v map[string]any) string {
	if v == nil {
		return ""
	}
	if t, ok := v["@type"].(string); ok && t == "addr_none" {
		return ""
	}
	addr, ok := v["address"].(string)
	if !ok || addr == "" {
		return ""
	}
	wc := "0"
	switch w := v["workchain_id"].(type) {
	case string:
		if w != "" {
			wc = w
		}
	case float64:
		wc = strconv.Itoa(int(w))
	}
	return wc + ":" + addr
}

// mintInfo is one vault mint inside a trace: freshly issued Jettons credited
// to a jetton wallet, attributed to a beneficiary via response_address and
// linked to the deposit via the echoed query_id.
type mintInfo struct {
	master       string // Jetton master == minting vault
	jettonWallet string // credited contract
	beneficiary  string // decoded response_address (raw form)
	queryID      string
	amountRaw    string
	msgHash      string
}

// traceMints extracts vault mints from a trace tree: decoded
// jetton_internal_transfer payloads emitted by a protocol account. Sender and
// receiver jetton-wallet hops are excluded by requiring the source to be the
// vault/master itself. Duplicates (same message seen as out_msg and in_msg)
// collapse by message hash.
func traceMints(tree *traceEnvelope) []mintInfo {
	if tree == nil {
		return nil
	}
	var out []mintInfo
	seen := make(map[string]bool)
	consider := func(m traceMsg, source string) {
		dec := m.Content.Decoded
		if dec.Type != "jetton_internal_transfer" || dec.Amount == "" {
			return
		}
		if m.Hash != "" {
			if seen[m.Hash] {
				return
			}
			seen[m.Hash] = true
		}
		out = append(out, mintInfo{
			master:       source,
			jettonWallet: m.Destination,
			beneficiary:  dec.ResponseAddress,
			queryID:      dec.QueryID,
			amountRaw:    dec.Amount,
			msgHash:      m.Hash,
		})
	}
	for _, tx := range tree.Txs {
		for _, m := range tx.Out {
			consider(m, tx.Account)
		}
	}
	return out
}

// jettonBurnDetails is a decoded burn: the owner destroys Amount raw units
// of Asset. Burns are disposals with exact on-chain amounts.
type jettonBurnDetails struct {
	Owner             string `json:"owner"`
	OwnerJettonWallet string `json:"owner_jetton_wallet"`
	Asset             string `json:"asset"`
	Amount            string `json:"amount"`
}

// traceTx is one raw transaction in a trace tree.
type traceTx struct {
	Account string     `json:"account"`
	Hash    string     `json:"hash"`
	In      traceMsg   `json:"in_msg"`
	Out     []traceMsg `json:"out_msgs"`
}

// traceInfo carries the indexer's completeness verdict for a trace.
type traceInfo struct {
	State string `json:"trace_state"`
}

// traceEnvelope is one decoded trace: the ordered message tree plus the
// decoded actions embedded via include_actions=true.
type traceEnvelope struct {
	TraceID string             `json:"trace_id"`
	Info    traceInfo          `json:"trace_info"`
	Order   []string           `json:"transactions_order"`
	Txs     map[string]traceTx `json:"transactions"`
	Actions []tonAction        `json:"actions"`
}

// complete reports whether the indexer considers the trace fully executed.
// Only complete trees may contradict interpreted legs; incomplete trees keep
// interpreted classification and allow the valued fallback.
func (t *traceEnvelope) complete() bool {
	return t != nil && t.Info.State == "complete"
}

type tracesResponse struct {
	Error  string          `json:"error"`
	Traces []traceEnvelope `json:"traces"`
}

func (r tracesResponse) envelopeError() string { return r.Error }

type actionsByTxResponse struct {
	Error   string      `json:"error"`
	Actions []tonAction `json:"actions"`
}

func (r actionsByTxResponse) envelopeError() string { return r.Error }

// fetchTraceByID returns the message tree for a trace, or nil when the
// indexer knows nothing about it. Unknown traces are not errors: the caller
// keeps interpreted legs.
func (c *Client) fetchTraceByID(ctx context.Context, traceID string) (*traceEnvelope, error) {
	q := url.Values{
		"trace_id":        {traceID},
		"include_actions": {"true"},
	}
	var env tracesResponse
	if err := c.get(ctx, "/traces", q, &env); err != nil {
		return nil, err
	}
	if len(env.Traces) == 0 {
		return nil, nil
	}
	out := env.Traces[0]
	return &out, nil
}

// actionsByTx returns decoded actions for one transaction hash, enriching
// the tree's embedded actions with per-transaction detail. Unknown hashes
// yield no actions without an error.
func (c *Client) actionsByTx(ctx context.Context, txHash string) ([]tonAction, error) {
	q := url.Values{
		"tx_hash":              {txHash},
		"include_transactions": {"true"},
	}
	var env actionsByTxResponse
	if err := c.get(ctx, "/actions", q, &env); err != nil {
		return nil, err
	}
	return env.Actions, nil
}

// downstream walks the message tree from the start transaction following
// out_msg → in_msg hash edges, returning reached transaction hashes in visit
// order plus every account seen. Unmatched edges fall back to destination
// account matching later in the indexer's topological order, so reordered or
// hash-less envelopes still resolve.
func downstream(tree *traceEnvelope, startHash string) (hashes []string, accounts map[string]bool) {
	byInHash := make(map[string]string)
	for h, tx := range tree.Txs {
		if tx.In.Hash != "" {
			byInHash[tx.In.Hash] = h
		}
	}
	order := make(map[string]int)
	for i, h := range tree.Order {
		order[h] = i
	}
	var reached []string
	seenAccounts := make(map[string]bool)
	visited := make(map[string]bool)
	queue := []string{startHash}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if visited[h] {
			continue
		}
		visited[h] = true
		tx, ok := tree.Txs[h]
		if !ok {
			continue
		}
		reached = append(reached, h)
		if tx.Account != "" {
			seenAccounts[tx.Account] = true
		}
		for _, m := range tx.Out {
			next, ok := byInHash[m.Hash]
			if !ok && m.Destination != "" {
				// Fall back to the destination account's next
				// transaction at or after this one in tree order.
				best := ""
				for cand, ctx := range tree.Txs {
					if ctx.Account != m.Destination || visited[cand] {
						continue
					}
					if order[cand] >= order[h] && (best == "" || order[cand] < order[best]) {
						best = cand
					}
				}
				next, ok = best, best != ""
			}
			if ok && !visited[next] {
				queue = append(queue, next)
			}
		}
	}
	return reached, seenAccounts
}

// walletOutflowTx finds the wallet's originating transaction in a trace: the
// first ordered transaction owned by one of the wallet's address forms. The
// deposit's causal subtree roots here.
func walletOutflowTx(tree *traceEnvelope, forms []string) string {
	inWallet := func(addr string) bool { return slices.Contains(forms, addr) }
	ordered := tree.Order
	if len(ordered) == 0 {
		for h := range tree.Txs {
			ordered = append(ordered, h)
		}
		slices.Sort(ordered)
	}
	for _, h := range ordered {
		if tx, ok := tree.Txs[h]; ok && inWallet(tx.Account) {
			return h
		}
	}
	return ""
}

// vaultMarker records that an outflow demonstrably entered a protocol: its
// subtree notifies a non-wallet contract (typical vault/pool intake). The
// outflow leg keeps honest vault-bound labeling while staying a separate
// transfer until a receipt is causally proven.
type vaultMarker struct {
	vault string
}

// detectVaultPattern reports the protocol contract when the outflow subtree
// delivers to a non-wallet account. Intermediate jetton-wallet reservoirs
// (skip) and the wallet's own forms never count: only genuine protocol
// intake qualifies. Pure wallet-to-wallet chains yield "".
func detectVaultPattern(tree *traceEnvelope, startHash string, forms []string, skip map[string]bool) vaultMarker {
	inWallet := func(addr string) bool { return slices.Contains(forms, addr) }
	reached, _ := downstream(tree, startHash)
	// Skip the originating wallet transaction itself: only downstream
	// protocol intake counts.
	for _, h := range reached[1:] {
		tx, ok := tree.Txs[h]
		if !ok || tx.Account == "" || inWallet(tx.Account) || skip[tx.Account] {
			continue
		}
		// A downstream non-wallet, non-reservoir transaction consuming a
		// message from this trace is protocol intake.
		return vaultMarker{vault: tx.Account}
	}
	return vaultMarker{}
}

// jettonReservoirs collects intermediate jetton-wallet contracts from a
// group's transfer and burn details so vault detection looks past them.
func jettonReservoirs(group []tonAction) map[string]bool {
	out := make(map[string]bool)
	for _, a := range group {
		switch a.Type {
		case opJettonTransfer:
			var d jettonTransferDetails
			if err := json.Unmarshal(a.Details, &d); err != nil {
				continue
			}
			if d.SenderJettonWallet != "" {
				out[d.SenderJettonWallet] = true
			}
			if d.ReceiverJettonWallet != "" {
				out[d.ReceiverJettonWallet] = true
			}
		case opJettonBurn:
			var d jettonBurnDetails
			if err := json.Unmarshal(a.Details, &d); err != nil {
				continue
			}
			if d.OwnerJettonWallet != "" {
				out[d.OwnerJettonWallet] = true
			}
		}
	}
	return out
}
