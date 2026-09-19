package ton

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/cexcommon"
)

// This file implements wallet history on top of the TON Center v3 Actions
// API: decoded, human-readable events (transfers, swaps, staking) grouped by
// trace. Derived legs (jetton_swap, stake_deposit) take precedence over the
// primitive transfer actions in the same trace so nothing is double counted.
// Standalone technical actions (call_contract, contract_deploy, jetton_burn,
// ...) carry no wallet value flow and are ignored.

const actionPageLimit = 1000

// maxActionPages caps history collection; hitting it marks sync incomplete.
const maxActionPages = 20

// tonAction is one decoded v3 action. Details decode per type on demand.
type tonAction struct {
	ActionID string          `json:"action_id"`
	TraceID  string          `json:"trace_id"`
	Start    int64           `json:"start_utime"`
	End      int64           `json:"end_utime"`
	Success  bool            `json:"success"`
	Type     string          `json:"type"`
	Details  json.RawMessage `json:"details"`
}

type actionsResponse struct {
	Error   string                 `json:"error"`
	Actions []tonAction            `json:"actions"`
	Meta    map[string]addressMeta `json:"metadata"`
}

func (r actionsResponse) envelopeError() string { return r.Error }

// tonTransferDetails is a native value movement. Value is in nanotons.
type tonTransferDetails struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Value       string `json:"value"`
}

// jettonTransferDetails is a Jetton movement. Amount is in raw units. The
// jetton-wallet legs identify intermediate reservoir contracts, never the
// economic counterparties.
type jettonTransferDetails struct {
	Asset                string `json:"asset"`
	Sender               string `json:"sender"`
	Receiver             string `json:"receiver"`
	Amount               string `json:"amount"`
	QueryID              string `json:"query_id"`
	SenderJettonWallet   string `json:"sender_jetton_wallet"`
	ReceiverJettonWallet string `json:"receiver_jetton_wallet"`
}

// swapLeg is one side of a DEX swap. Asset is a Jetton master or nativeTONSymbol.
type swapLeg struct {
	Asset       string `json:"asset"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Amount      string `json:"amount"`
}

// swapDetails is a decoded DEX swap with exact in/out legs.
type swapDetails struct {
	Dex      string  `json:"dex"`
	Sender   string  `json:"sender"`
	AssetIn  string  `json:"asset_in"`
	AssetOut string  `json:"asset_out"`
	Incoming swapLeg `json:"dex_incoming_transfer"`
	Outgoing swapLeg `json:"dex_outgoing_transfer"`
}

// stakeDetails is a liquid-staking deposit: TON in, staking tokens out.
// Amount is nanotons staked, TokensMinted raw units of Asset.
type stakeDetails struct {
	Provider     string `json:"provider"`
	StakeHolder  string `json:"stake_holder"`
	Pool         string `json:"pool"`
	Amount       string `json:"amount"`
	TokensMinted string `json:"tokens_minted"`
	Asset        string `json:"asset"`
}

// actionPage fetches one page of actions for an account, newest first,
// returning the page and its token metadata.
func (c *Client) actionPage(ctx context.Context, account string, limit, offset int) ([]tonAction, map[string]addressMeta, error) {
	q := url.Values{
		"account": {account},
		"limit":   {strconv.Itoa(limit)},
		"offset":  {strconv.Itoa(offset)},
		"sort":    {"desc"},
	}
	var env actionsResponse
	if err := c.get(ctx, "/actions", q, &env); err != nil {
		return nil, nil, err
	}
	return env.Actions, env.Meta, nil
}

// newestWindowPages refreshes the newest actions on every run; tailWindowPages
// advances an in-flight backfill. Together they respect the maxActionPages
// per-run budget while letting large histories converge over several runs.
const newestWindowPages = 2
const tailWindowPages = maxActionPages - newestWindowPages

// fetchActionWindow pages [startPage, startPage+maxPages), stopping early on
// a short page or a collection error. truncated reports a full final page:
// more actions may exist beyond the window.
func (c *Client) fetchActionWindow(ctx context.Context, account string, startPage, maxPages int) (all []tonAction, meta map[string]addressMeta, truncated bool, err error) {
	meta = make(map[string]addressMeta)
	for page := startPage; page < startPage+maxPages; page++ {
		actions, pageMeta, err := c.actionPage(ctx, account, actionPageLimit, page*actionPageLimit)
		if err != nil {
			return all, meta, false, err
		}
		if len(actions) == 0 {
			return all, meta, false, nil
		}
		all = append(all, actions...)
		for addr, m := range pageMeta {
			meta[addr] = m
		}
		if len(actions) < actionPageLimit {
			return all, meta, false, nil
		}
	}
	return all, meta, true, nil
}

// groupActionsByTrace clusters successful actions by trace_id preserving
// first-seen order. Failed actions never reach classification.
func groupActionsByTrace(actions []tonAction) [][]tonAction {
	var groups [][]tonAction
	index := make(map[string]int)
	for i, a := range actions {
		if !a.Success {
			continue
		}
		key := a.TraceID
		if key == "" {
			key = "\x00" + a.ActionID + "\x00" + strconv.Itoa(i)
		}
		if at, ok := index[key]; ok {
			groups[at] = append(groups[at], a)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, []tonAction{a})
	}
	return groups
}

// actionKind resolves a token descriptor for swap/stake legs: native TON or
// a Jetton master looked up in the resolved metadata.
func actionKind(asset string, masters map[string]tokenMeta) (symbol, tokenAddr string, decimals int, ok bool) {
	if isNativeAsset(asset) {
		return nativeTONSymbol, "", 9, true
	}
	m, found := masters[asset]
	if !found || m.Symbol == "" {
		return "", "", 0, false
	}
	return cexcommon.NormalizeAsset(m.Symbol), m.Address, m.Decimals, true
}

// traceContext carries optional verification data for one trace group: the
// raw message tree plus vault-intake markers for outflow actions. A nil tree
// means the trace was not (or could not be) verified.
type traceContext struct {
	tree *traceEnvelope
	// vaults maps outflow action IDs to protocol intake markers.
	vaults map[string]vaultMarker
}

// classifyActionGroup turns one trace into activities. Explicit swap and
// staking legs take precedence; otherwise a causally downstream receipt of a
// different asset in the verified trace tree pairs the outflow as a
// conversion; otherwise each transfer action maps on its own and technical
// actions are ignored.
func classifyActionGroup(ctx context.Context, wallet string, forms []string, masters map[string]tokenMeta, value valueUSD, outflows map[string]VaultSpend, group []tonAction, tractx *traceContext, jettonWallets map[string]bool, tracked map[string]bool,
) ([]*brokerageActivity, error) {
	var swaps []tonAction
	var stakes []tonAction
	var transfers []tonAction
	for _, a := range group {
		switch a.Type {
		case "jetton_swap":
			swaps = append(swaps, a)
		case "stake_deposit":
			stakes = append(stakes, a)
		case opTonTransfer, opJettonTransfer, opJettonBurn:
			transfers = append(transfers, a)
		default:
			// call_contract, contract_deploy, ... carry no
			// wallet value flow on their own.
		}
	}
	var out []*brokerageActivity
	if len(swaps)+len(stakes) > 0 {
		consumedAll := make(map[string]bool)
		for _, a := range swaps {
			legs, consumed, err := swapActivities(ctx, wallet, forms, masters, value, a, group)
			if err != nil {
				return nil, err
			}
			out = append(out, legs...)
			maps.Copy(consumedAll, consumed)
		}
		for _, a := range stakes {
			legs, consumed, err := stakeActivities(ctx, wallet, forms, masters, value, a, group)
			if err != nil {
				return nil, err
			}
			out = append(out, legs...)
			maps.Copy(consumedAll, consumed)
		}
		if tractx == nil || verifyDerived(tractx.tree, group) {
			// Suppress only primitives proven to be components of the
			// emitted legs. Anything else in the trace — change, extra
			// transfers, another wallet's flows touching us — classifies
			// on its own below instead of disappearing.
			var rest []tonAction
			for _, a := range transfers {
				if !consumedAll[a.ActionID] {
					rest = append(rest, a)
				}
			}
			transfers = rest
		} else {
			// A complete trace tree positively contradicts the interpreted
			// legs (claimed protocol party absent): downgrade to primitives.
			out = nil
		}
	}
	if tractx != nil {
		legs, consumed, err := correlateDeposit(ctx, wallet, forms, masters, value, group, tractx.tree, jettonWallets)
		if err != nil {
			return nil, err
		}
		if len(legs) > 0 {
			// Leftover primitives (e.g. excess change) still classify,
			// minus the legs consumed by the conversion.
			var rest []tonAction
			for _, a := range transfers {
				if !consumed[a.ActionID] {
					rest = append(rest, a)
				}
			}
			transfers = rest
			out = append(out, legs...)
			if len(transfers) == 0 {
				return out, nil
			}
		}
	}
	var vaults map[string]vaultMarker
	if tractx != nil {
		vaults = tractx.vaults
	}
	for _, a := range transfers {
		act, err := transferActivity(ctx, wallet, forms, masters, value, outflows, vaults, tracked, a)
		if err != nil {
			return nil, err
		}
		if act != nil {
			out = append(out, act)
		}
	}
	return out, nil
}

// verifyDerived checks interpreted swap/stake legs against a complete trace
// tree: every claimed protocol counterparty (dex legs, stake pool) must
// participate in the tree. Missing or incomplete trees keep interpreted legs;
// only a complete tree entirely lacking the protocol party downgrades.
func verifyDerived(tree *traceEnvelope, group []tonAction) bool {
	if !tree.complete() {
		return true
	}
	parties := make(map[string]bool)
	for _, a := range group {
		switch a.Type {
		case "jetton_swap":
			var d swapDetails
			if err := json.Unmarshal(a.Details, &d); err != nil {
				continue
			}
			parties[d.Incoming.Destination] = true
			parties[d.Outgoing.Source] = true
		case "stake_deposit":
			var d stakeDetails
			if err := json.Unmarshal(a.Details, &d); err != nil {
				continue
			}
			parties[d.Pool] = true
		}
	}
	if len(parties) == 0 {
		return true
	}
	for _, tx := range tree.Txs {
		if parties[tx.Account] {
			return true
		}
	}
	return false
}

// minNativeTON drops sub-dust native legs (empty notifications, spam) that
// carry no economic meaning.
const minNativeTON = 1e-6

// transferActivity maps one ton_transfer/jetton_transfer action to a single
// leg: internal counterparty → TRANSFER_*, otherwise DEPOSIT/WITHDRAWAL.
// Cost basis (Price/Amount) comes from the USD pricer when the token has a
// feed (stablecoins, TON); anything else stays unvalued. Outflows proven by
// the trace tree to enter a protocol carry vault-bound labeling while staying
// separate transfers until a receipt is causally established. Legs whose
// counterparty is none of the tracked wallets are flagged external.
func transferActivity(ctx context.Context, wallet string, forms []string, masters map[string]tokenMeta, value valueUSD, outflows map[string]VaultSpend, vaults map[string]vaultMarker, tracked map[string]bool, a tonAction) (*brokerageActivity, error) {
	inWallet := func(addr string) bool { return slices.Contains(forms, addr) }
	var symbol, tokenAddr, counterparty string
	var qty float64
	var inbound, burned bool
	var err error
	switch a.Type {
	case opTonTransfer:
		var d tonTransferDetails
		if derr := json.Unmarshal(a.Details, &d); derr != nil {
			return nil, fmt.Errorf("ton: invalid ton_transfer details %s: %w", a.ActionID, derr)
		}
		from, to := inWallet(d.Source), inWallet(d.Destination)
		if !from && !to {
			return nil, nil
		}
		if from && to {
			return nil, nil
		}
		if qty, err = scaledAmount(d.Value, 9); err != nil {
			return nil, fmt.Errorf("ton: invalid transfer value %s: %w", a.ActionID, err)
		}
		if qty < minNativeTON {
			return nil, nil
		}
		symbol, inbound, counterparty = nativeTONSymbol, to, d.Source
		if from {
			counterparty = d.Destination
		}
	case opJettonTransfer:
		var d jettonTransferDetails
		if derr := json.Unmarshal(a.Details, &d); derr != nil {
			return nil, fmt.Errorf("ton: invalid jetton_transfer details %s: %w", a.ActionID, derr)
		}
		from, to := inWallet(d.Sender), inWallet(d.Receiver)
		if !from && !to {
			return nil, nil
		}
		if from && to {
			return nil, nil
		}
		m, ok := masters[d.Asset]
		if !ok || m.Symbol == "" {
			return nil, fmt.Errorf("ton: unresolved Jetton %s in %s", d.Asset, a.ActionID)
		}
		if qty, err = scaledAmount(d.Amount, m.Decimals); err != nil {
			return nil, fmt.Errorf("ton: invalid Jetton amount %s: %w", a.ActionID, err)
		}
		if qty <= 0 {
			return nil, nil
		}
		symbol, tokenAddr, inbound, counterparty = cexcommon.NormalizeAsset(m.Symbol), m.Address, to, d.Sender
		if from {
			counterparty = d.Receiver
		}
	case opJettonBurn:
		var d jettonBurnDetails
		if derr := json.Unmarshal(a.Details, &d); derr != nil {
			return nil, fmt.Errorf("ton: invalid jetton_burn details %s: %w", a.ActionID, derr)
		}
		if !inWallet(d.Owner) {
			return nil, nil
		}
		m, ok := masters[d.Asset]
		if !ok || m.Symbol == "" {
			return nil, fmt.Errorf("ton: unresolved burn asset %s in %s", d.Asset, a.ActionID)
		}
		if qty, err = scaledAmount(d.Amount, m.Decimals); err != nil {
			return nil, fmt.Errorf("ton: invalid burn amount %s: %w", a.ActionID, err)
		}
		if qty <= 0 {
			return nil, nil
		}
		// Burns destroy shares outright; they never fund outflows for
		// basis attribution the way master-bound deposits do.
		symbol, tokenAddr, inbound, counterparty = cexcommon.NormalizeAsset(m.Symbol), m.Address, false, d.Asset
		burned = true
	default:
		return nil, nil
	}
	prefix := nativeTONSymbol
	if tokenAddr != "" {
		prefix = "JETTON"
	}
	typ, rawType := brokerage.ActivityTransferIn, prefix+"_TRANSFER_IN"
	if !inbound {
		typ, rawType = brokerage.ActivityTransferOut, prefix+"_TRANSFER_OUT"
	}
	var amount, price float64
	note := ""
	if unit, ok := value.unitPrice(ctx, symbol, tokenAddr, a.End); ok {
		price, amount = unit, qty*unit
		note = fmt.Sprintf("cost basis ~$%s (%s rate %s)",
			formatUSD(amount), symbol, time.Unix(a.End, 0).UTC().Format("2006-01-02"))
	}
	if m, ok := vaults[a.ActionID]; ok && !inbound && m.vault != "" {
		if note != "" {
			note += "; "
		}
		note += fmt.Sprintf("protocol deposit via %s (receipt settles asynchronously)", shortAddr(m.vault))
	}
	act := buildActivity(wallet, symbol, tokenAddr, qty, typ, rawType,
		counterparty, a.ActionID, a.TraceID, a.End, amount, price, note)
	if act != nil {
		act.IsExternal = !tracked[counterparty]
	}
	if act != nil && !inbound && !burned && outflows != nil {
		// Outflows straight into a Jetton master (vault/pool deposits) fund
		// shares that arrive without any transfer action; record the spend
		// so positions can attribute it as basis.
		if _, isMaster := masters[counterparty]; isMaster && amount > 0 {
			prev := outflows[counterparty]
			prev.Amount += amount
			prev.Time = max(prev.Time, a.End)
			outflows[counterparty] = prev
		}
	}
	return act, nil
}

// swapActivities renders a jetton_swap action as SELL-in + BUY-out legs
// routed through USD: both legs share the USD value resolved from the best
// priced side (stablecoin at $1, else TON at its historical price).
func swapActivities(ctx context.Context, wallet string, forms []string, masters map[string]tokenMeta, value valueUSD, a tonAction, group []tonAction) ([]*brokerageActivity, map[string]bool, error) { //nolint:gocritic,unnamedResult // classifier triple (legs, consumed, err) is positional throughout the TON pipeline; names would collide with the err locals in every branch.
	var d swapDetails
	if err := json.Unmarshal(a.Details, &d); err != nil {
		return nil, nil, fmt.Errorf("ton: invalid jetton_swap details %s: %w", a.ActionID, err)
	}
	inWallet := func(addr string) bool { return slices.Contains(forms, addr) }
	if !inWallet(d.Incoming.Source) || !inWallet(d.Outgoing.Destination) {
		return nil, nil, nil // not our swap; the legs will classify on their own
	}
	inSymbol, inAddr, inDec, ok := actionKind(d.AssetIn, masters)
	if !ok {
		return nil, nil, fmt.Errorf("ton: unresolved swap asset %s in %s", d.AssetIn, a.ActionID)
	}
	outSymbol, outAddr, outDec, ok := actionKind(d.AssetOut, masters)
	if !ok {
		return nil, nil, fmt.Errorf("ton: unresolved swap asset %s in %s", d.AssetOut, a.ActionID)
	}
	inQty, err := scaledAmount(d.Incoming.Amount, inDec)
	if err != nil || inQty <= 0 {
		return nil, nil, fmt.Errorf("ton: invalid swap input %s: %w", a.ActionID, err)
	}
	outQty, err := scaledAmount(d.Outgoing.Amount, outDec)
	if err != nil || outQty <= 0 {
		return nil, nil, fmt.Errorf("ton: invalid swap output %s: %w", a.ActionID, err)
	}
	usd, ok := value.swapValue(ctx, inSymbol, inAddr, inQty, outSymbol, outAddr, outQty, a.End)
	note := fmt.Sprintf("swap %g %s → %g %s via %s", inQty, inSymbol, outQty, outSymbol, d.Dex)
	legs := conversionLegs(wallet, inSymbol, inAddr, inQty, outSymbol, outAddr, outQty, a.End,
		"SWAP_SELL", "SWAP_BUY", d.Incoming.Destination, d.Outgoing.Source,
		a.ActionID, a.TraceID, note, usd, ok)
	return legs, consumeSwapLegs(group, forms, d), nil
}

// consumeSwapLegs marks group transfer actions proven to be the on-chain
// legs of an emitted swap: same asset, same raw amount, same direction
// between the wallet and the DEX party named in the swap details. Matching
// is exact strings from the same indexer page — never fuzzy amounts — so
// unrelated transfers (change, third-party flows) stay unmarked and
// classify on their own.
func consumeSwapLegs(group []tonAction, forms []string, d swapDetails) map[string]bool {
	inWallet := func(addr string) bool { return slices.Contains(forms, addr) }
	out := make(map[string]bool)
	match := func(asset, source, dest, amount string) {
		for _, a := range group {
			switch a.Type {
			case opTonTransfer:
				if asset != nativeTONSymbol {
					continue
				}
				var t tonTransferDetails
				if json.Unmarshal(a.Details, &t) != nil {
					continue
				}
				if t.Source == source && t.Destination == dest && t.Value == amount {
					out[a.ActionID] = true
				}
			case opJettonTransfer:
				var t jettonTransferDetails
				if json.Unmarshal(a.Details, &t) != nil {
					continue
				}
				if t.Asset == asset && t.Sender == source && t.Receiver == dest && t.Amount == amount {
					out[a.ActionID] = true
				}
			}
		}
	}
	if inWallet(d.Incoming.Source) {
		match(d.AssetIn, d.Incoming.Source, d.Incoming.Destination, d.Incoming.Amount)
	}
	if inWallet(d.Outgoing.Destination) {
		match(d.AssetOut, d.Outgoing.Source, d.Outgoing.Destination, d.Outgoing.Amount)
	}
	return out
}

// priceBias is the relative nudge applied to conversion prices by role.
// The app canonicalizes every trade amount as quantity × unit_price in exact
// decimal arithmetic, so a rounded price re-times quantity into dust that
// lands in cash (e.g. −$0.00000392 → "cash went negative"). Nudging the sell
// price up and the buy price down guarantees the stored buy total stays
// below the stored sell total, while the deviation stays far below the app's
// half-cent canonicalization tolerance.
const priceBias = 1e-10

// priceSigDigits caps conversion prices at twelve significant digits so the
// app's exact quantity × price product always fits its decimal type. The cap
// perturbs prices ~200× less than the role bias, so the ordering guarantee
// survives it.
const priceSigDigits = 12

// biasedPrices derives a conversion pair's unit prices from the shared USD
// total: the sell leg rounds up, the buy leg rounds down. Exact $1 stablecoin
// legs skip the nudge — their product is already exact.
func biasedPrices(inQty, outQty, usd float64, ok bool) (sell, buy float64) {
	sell, buy = usdPrice(inQty, usd, ok), usdPrice(outQty, usd, ok)
	if !ok {
		return 0, 0
	}
	if sell != 1 {
		sell = roundSig(sell*(1+priceBias), priceSigDigits)
	}
	if buy != 1 {
		buy = roundSig(buy*(1-priceBias), priceSigDigits)
	}
	return sell, buy
}

// roundSig rounds v to sig significant digits, preserving the value when it
// is already short, zero or non-finite.
func roundSig(v float64, sig int) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	f, err := strconv.ParseFloat(strconv.FormatFloat(v, 'g', sig, 64), 64)
	if err != nil {
		return v
	}
	return f
}

// conversionLegs renders a proven conversion as SELL-in + BUY-out legs routed
// through one shared USD value: $TOKEN_IN → USD, USD → $TOKEN_OUT. The BUY
// leg carries +1s so date-sorted views keep causal order where equal
// timestamps would tie.
func conversionLegs(wallet, inSymbol, inAddr string, inQty float64, outSymbol, outAddr string, outQty float64, at int64,
	sellRaw, buyRaw, sellCp, buyCp, actionID, traceID, note string, usd float64, ok bool,
) []*brokerageActivity {
	if ok {
		note += fmt.Sprintf(" (~$%s USD)", formatUSD(usd))
	}
	sellPrice, buyPrice := biasedPrices(inQty, outQty, usd, ok)
	return []*brokerageActivity{
		buildActivity(wallet, inSymbol, inAddr, inQty,
			brokerage.ActivitySell, sellRaw, sellCp,
			actionID, traceID, at, usd, sellPrice, note),
		// +1s orders the pair deterministically (SELL, then BUY) in
		// date-sorted views where equal timestamps would tie.
		buildActivity(wallet, outSymbol, outAddr, outQty,
			brokerage.ActivityBuy, buyRaw, buyCp,
			actionID, traceID, at+1, usd, buyPrice, note),
	}
}

// correlateDeposit pairs a wallet outflow (transfer or burn) with a receipt
// (inbound transfer or vault mint) proven by the trace tree, rendering a
// USD-routed STAKE_SELL/STAKE_BUY conversion. Proof hierarchy per pair:
//  1. explicit query_id echo (deposit transfer ↔ mint message),
//  2. structural: receipt from protocol intake or downstream, mint credited
//     to the user's jetton wallet or attributed via response_address.
//
// Same-trace only, same-asset returns and upstream gas change never pair, and
// amounts/timestamps are never compared — only trace ancestry. Unproven
// traces yield nothing so legs classify separately. Matched group actions
// return as consumed so primitives left over (e.g. excess change) still
// classify on their own.
func correlateDeposit(ctx context.Context, wallet string, forms []string, masters map[string]tokenMeta, value valueUSD, group []tonAction, tree *traceEnvelope, jettonWallets map[string]bool) ([]*brokerageActivity, map[string]bool, error) { //nolint:gocritic,unnamedResult // classifier triple (legs, consumed, err) is positional throughout the TON pipeline; names would collide with the err locals in every branch.
	if tree == nil || len(tree.Txs) == 0 {
		return nil, nil, nil
	}
	outflow, err := depositOutflow(group, forms, masters)
	if err != nil {
		return nil, nil, err
	}
	if outflow == nil {
		return nil, nil, nil
	}
	if outflow.action.TraceID == "" || outflow.action.TraceID != tree.TraceID {
		return nil, nil, nil // never pair across traces, only within one tree
	}
	root := walletOutflowTx(tree, forms)
	if root == "" {
		return nil, nil, nil
	}
	// The receipt must originate at or downstream of protocol intake:
	// change and gas returns (excess) flow back from upstream hops and
	// are never conversions.
	vault := detectVaultPattern(tree, root, forms, jettonReservoirs(group)).vault
	if vault == "" {
		return nil, nil, nil
	}
	reached, _ := downstream(tree, root)
	vaultTx := ""
	for _, h := range reached {
		if tx, ok := tree.Txs[h]; ok && tx.Account == vault {
			vaultTx = h
			break
		}
	}
	if vaultTx == "" {
		return nil, nil, nil
	}
	_, allowedSenders := downstream(tree, vaultTx)
	receipt, receiptAction := depositReceipt(tree, forms, masters, outflow, vault, allowedSenders, jettonWallets)
	if receipt == nil {
		return nil, nil, nil
	}
	verb := "deposit"
	if outflow.burn {
		verb = "unstake"
	}
	usd, ok := value.swapValue(ctx, outflow.symbol, outflow.tokenAddr, outflow.qty,
		receipt.symbol, receipt.tokenAddr, receipt.qty, outflow.action.End)
	note := fmt.Sprintf("%s %g %s → %g %s via %s (trace-correlated)",
		verb, outflow.qty, outflow.symbol, receipt.qty, receipt.symbol, shortAddr(outflow.counterparty))
	legs := conversionLegs(wallet, outflow.symbol, outflow.tokenAddr, outflow.qty,
		receipt.symbol, receipt.tokenAddr, receipt.qty, outflow.action.End,
		"STAKE_SELL", "STAKE_BUY", outflow.counterparty, receipt.sender,
		outflow.action.ActionID, outflow.action.TraceID, note, usd, ok)
	consumed := map[string]bool{outflow.action.ActionID: true}
	if receiptAction != nil {
		consumed[receiptAction.ActionID] = true
	}
	return legs, consumed, nil
}

// outflowLeg is one side of a proven conversion: what the wallet gave up.
type outflowLeg struct {
	action            tonAction
	symbol, tokenAddr string
	qty               float64
	counterparty      string
	queryID           string
	burn              bool
}

// depositOutflow finds the wallet's disposal in a group: a transfer out, or
// a burn it owns (unstaking). Burns carry exact on-chain amounts.
func depositOutflow(group []tonAction, forms []string, masters map[string]tokenMeta) (*outflowLeg, error) {
	inWallet := func(addr string) bool { return slices.Contains(forms, addr) }
	for _, a := range group {
		switch a.Type {
		case opTonTransfer:
			var d tonTransferDetails
			if err := json.Unmarshal(a.Details, &d); err != nil {
				return nil, fmt.Errorf("ton: invalid ton_transfer details %s: %w", a.ActionID, err)
			}
			if !inWallet(d.Source) || inWallet(d.Destination) {
				continue
			}
			qty, err := scaledAmount(d.Value, 9)
			if err != nil {
				return nil, fmt.Errorf("ton: invalid transfer value %s: %w", a.ActionID, err)
			}
			if qty < minNativeTON {
				continue
			}
			return &outflowLeg{action: a, symbol: nativeTONSymbol, qty: qty, counterparty: d.Destination}, nil
		case opJettonTransfer:
			var d jettonTransferDetails
			if err := json.Unmarshal(a.Details, &d); err != nil {
				return nil, fmt.Errorf("ton: invalid jetton_transfer details %s: %w", a.ActionID, err)
			}
			if !inWallet(d.Sender) || inWallet(d.Receiver) {
				continue
			}
			m, ok := masters[d.Asset]
			if !ok || m.Symbol == "" {
				return nil, fmt.Errorf("ton: unresolved Jetton %s in %s", d.Asset, a.ActionID)
			}
			qty, err := scaledAmount(d.Amount, m.Decimals)
			if err != nil {
				return nil, fmt.Errorf("ton: invalid Jetton amount %s: %w", a.ActionID, err)
			}
			if qty <= 0 {
				continue
			}
			return &outflowLeg{action: a, symbol: cexcommon.NormalizeAsset(m.Symbol),
				tokenAddr: m.Address, qty: qty, counterparty: d.Receiver, queryID: d.QueryID}, nil
		case opJettonBurn:
			var d jettonBurnDetails
			if err := json.Unmarshal(a.Details, &d); err != nil {
				return nil, fmt.Errorf("ton: invalid jetton_burn details %s: %w", a.ActionID, err)
			}
			if !inWallet(d.Owner) {
				continue
			}
			m, ok := masters[d.Asset]
			if !ok || m.Symbol == "" {
				return nil, fmt.Errorf("ton: unresolved burn asset %s in %s", d.Asset, a.ActionID)
			}
			qty, err := scaledAmount(d.Amount, m.Decimals)
			if err != nil {
				return nil, fmt.Errorf("ton: invalid burn amount %s: %w", a.ActionID, err)
			}
			if qty <= 0 {
				continue
			}
			return &outflowLeg{action: a, symbol: cexcommon.NormalizeAsset(m.Symbol),
				tokenAddr: m.Address, qty: qty, counterparty: d.Asset, burn: true}, nil
		}
	}
	return nil, nil
}

// receiptLeg is what the wallet got back: an inbound transfer, or vault
// shares minted to its jetton wallet.
type receiptLeg struct {
	symbol, tokenAddr string
	qty               float64
	sender            string
}

// depositReceipt proves the return leg. Query_id echo between the outflow
// transfer and a mint message wins outright; otherwise an inbound transfer
// from intake-or-downstream, or a mint credited to the user's jetton wallet
// (or attributed via response_address) with the vault as source, pairs.
func depositReceipt(tree *traceEnvelope, forms []string, masters map[string]tokenMeta, outflow *outflowLeg, vault string, allowedSenders map[string]bool, jettonWallets map[string]bool) (*receiptLeg, *tonAction) {
	// Explicit link first: the vault echoes the deposit query_id in the
	// mint message.
	if outflow.queryID != "" {
		for _, m := range traceMints(tree) {
			if m.queryID != outflow.queryID {
				continue
			}
			if r := mintReceipt(m, masters, jettonWallets, forms); r != nil {
				return r, nil
			}
		}
	}
	for _, a := range tree.Actions {
		if !a.Success {
			continue
		}
		switch a.Type {
		case opTonTransfer, opJettonTransfer:
			r, ok := inboundReceipt(a, forms, masters, outflow)
			if !ok || r == nil {
				continue
			}
			if !allowedSenders[r.sender] {
				continue // receipt must come from protocol intake or downstream
			}
			cp := a
			return r, &cp
		}
	}
	// Structural mint fallback: vault-sourced mint to the user's wallet
	// without a query_id echo.
	for _, m := range traceMints(tree) {
		if m.master != vault {
			continue
		}
		if r := mintReceipt(m, masters, jettonWallets, forms); r != nil {
			return r, nil
		}
	}
	return nil, nil
}

// inboundReceipt parses a wallet-inbound transfer of a different asset than
// the outflow. Same-asset returns (excess/change) never qualify.
func inboundReceipt(a tonAction, forms []string, masters map[string]tokenMeta, outflow *outflowLeg) (*receiptLeg, bool) {
	inWallet := func(addr string) bool { return slices.Contains(forms, addr) }
	var symbol, tokenAddr, sender string
	var qty float64
	switch a.Type {
	case opTonTransfer:
		var d tonTransferDetails
		if err := json.Unmarshal(a.Details, &d); err != nil {
			return nil, false
		}
		if !inWallet(d.Destination) || inWallet(d.Source) {
			return nil, false
		}
		var err error
		if qty, err = scaledAmount(d.Value, 9); err != nil || qty < minNativeTON {
			return nil, false
		}
		symbol, sender = nativeTONSymbol, d.Source
	case opJettonTransfer:
		var d jettonTransferDetails
		if err := json.Unmarshal(a.Details, &d); err != nil {
			return nil, false
		}
		if !inWallet(d.Receiver) || inWallet(d.Sender) {
			return nil, false
		}
		m, ok := masters[d.Asset]
		if !ok || m.Symbol == "" {
			return nil, false
		}
		var err error
		if qty, err = scaledAmount(d.Amount, m.Decimals); err != nil || qty <= 0 {
			return nil, false
		}
		symbol, tokenAddr, sender = cexcommon.NormalizeAsset(m.Symbol), m.Address, d.Sender
	default:
		return nil, false
	}
	if symbol == outflow.symbol && tokenAddr == outflow.tokenAddr {
		return nil, false
	}
	return &receiptLeg{symbol: symbol, tokenAddr: tokenAddr, qty: qty, sender: sender}, true
}

// mintReceipt resolves a vault mint into a receipt leg: the master must
// resolve and the credit must demonstrably belong to the wallet — destination
// in its jetton-wallet set, or response_address naming it.
func mintReceipt(m mintInfo, masters map[string]tokenMeta, jettonWallets map[string]bool, forms []string) *receiptLeg {
	meta, ok := masters[m.master]
	if !ok || meta.Symbol == "" {
		return nil
	}
	inWallet := func(addr string) bool { return slices.Contains(forms, addr) }
	if !jettonWallets[m.jettonWallet] && !inWallet(m.beneficiary) {
		return nil
	}
	qty, err := scaledAmount(m.amountRaw, meta.Decimals)
	if err != nil || qty <= 0 {
		return nil
	}
	symbol := cexcommon.NormalizeAsset(meta.Symbol)
	if symbol == "" {
		return nil
	}
	return &receiptLeg{symbol: symbol, tokenAddr: meta.Address, qty: qty, sender: m.master}
}

// stakeActivities renders a stake_deposit action as SELL staked TON + BUY
// liquid-staking tokens, priced through USD like swaps.
func stakeActivities(ctx context.Context, wallet string, forms []string, masters map[string]tokenMeta, value valueUSD, a tonAction, group []tonAction) ([]*brokerageActivity, map[string]bool, error) { //nolint:gocritic,unnamedResult // classifier triple (legs, consumed, err) is positional throughout the TON pipeline; names would collide with the err locals in every branch.
	var d stakeDetails
	if err := json.Unmarshal(a.Details, &d); err != nil {
		return nil, nil, fmt.Errorf("ton: invalid stake_deposit details %s: %w", a.ActionID, err)
	}
	if !slices.Contains(forms, d.StakeHolder) {
		return nil, nil, nil
	}
	m, ok := masters[d.Asset]
	if !ok || m.Symbol == "" {
		return nil, nil, fmt.Errorf("ton: unresolved stake asset %s in %s", d.Asset, a.ActionID)
	}
	inQty, err := scaledAmount(d.Amount, 9)
	if err != nil || inQty <= 0 {
		return nil, nil, fmt.Errorf("ton: invalid stake amount %s: %w", a.ActionID, err)
	}
	outQty, err := scaledAmount(d.TokensMinted, m.Decimals)
	if err != nil || outQty <= 0 {
		return nil, nil, fmt.Errorf("ton: invalid minted amount %s: %w", a.ActionID, err)
	}
	outSymbol := cexcommon.NormalizeAsset(m.Symbol)
	usd, ok := value.swapValue(ctx, nativeTONSymbol, "", inQty, outSymbol, m.Address, outQty, a.End)
	note := fmt.Sprintf("stake %g TON → %g %s via %s", inQty, outQty, outSymbol, d.Provider)
	legs := conversionLegs(wallet, nativeTONSymbol, "", inQty, outSymbol, m.Address, outQty, a.End,
		"STAKE_SELL", "STAKE_BUY", d.Pool, d.Pool,
		a.ActionID, a.TraceID, note, usd, ok)
	return legs, consumeStakeLegs(group, forms, d), nil
}

// consumeStakeLegs marks group transfers proven to be the staking legs: the
// exact TON inflow to the pool, plus — for synchronous mints only — the
// exact minted-token payout back to the wallet. Async mints land in other
// traces and are never consumed here.
func consumeStakeLegs(group []tonAction, forms []string, d stakeDetails) map[string]bool {
	inWallet := func(addr string) bool { return slices.Contains(forms, addr) }
	out := make(map[string]bool)
	for _, a := range group {
		switch a.Type {
		case opTonTransfer:
			var t tonTransferDetails
			if json.Unmarshal(a.Details, &t) != nil {
				continue
			}
			if inWallet(t.Source) && t.Destination == d.Pool && t.Value == d.Amount {
				out[a.ActionID] = true
			}
		case opJettonTransfer:
			var t jettonTransferDetails
			if json.Unmarshal(a.Details, &t) != nil {
				continue
			}
			if t.Asset == d.Asset && t.Sender == d.Pool && inWallet(t.Receiver) && t.Amount == d.TokensMinted {
				out[a.ActionID] = true
			}
		}
	}
	return out
}

// usdPrice spreads a USD value across units, or zero when unvalued.
func usdPrice(qty, usd float64, ok bool) float64 {
	if !ok || qty <= 0 {
		return 0
	}
	return usd / qty
}
