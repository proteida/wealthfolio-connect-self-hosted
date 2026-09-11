// Package ton implements a BrokerClient that tracks native TON and Jetton
// balances plus full transfer history for a few configured wallets via the
// TON Center API v3 (https://toncenter.com/api/v3):
//
//	GET /accountStates                                    native balance
//	GET /jetton/wallets?owner_address=...                 Jetton balances
//	GET /jetton/masters?address=...                       symbol/decimals per Jetton
//	GET /transactions?account=...                         native history
//	GET /jetton/transfers?owner_address=&jetton_master=.. Jetton history
//
// Decoded/indexed fields (symbol, decimals, transfer parties) are used
// directly; raw BOC messages are never decoded by hand. History endpoints
// are paginated with limit/offset until a short page ends the collection.
//
// Authentication is an optional API key sent as X-API-Key (anonymous quota
// is 1 RPS). HTTP 429/5xx responses and indexer timeout envelopes are
// retried with exponential backoff honoring Retry-After.
package ton

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	domainsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/sync"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/cexcommon"
)

// HTTPDoer mirrors http.Client.Do so tests can stub it.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

const (
	defaultBaseURL = "https://toncenter.com/api/v3"
	nanotonsPerTON = 1e9

	walletPageLimit   = 1000
	masterChunkSize   = 100
	defaultMaxRetries = 5
	defaultRetryBase  = time.Second
	maxRetryDelay     = 30 * time.Second
)

// errHistoryTruncated reports that pagination hit the safety page cap, so the
// returned history is a prefix and transaction sync must stay incomplete.
var errHistoryTruncated = fmt.Errorf("ton: history pagination hit the page cap")

// Client reads TON Center v3 data for a fixed set of wallets.
type Client struct {
	apiKey     string
	wallets    []string
	baseURL    string
	http       HTTPDoer
	log        zerolog.Logger
	now        func() time.Time
	sleep      func(time.Duration)
	retryBase  time.Duration
	maxRetries int
	// value overrides USD pricing (tests); nil uses the CoinGecko pricer.
	value valueUSD
	// history is the optional stored-ledger source for reconciliation:
	// retiring superseded synthetic rows and rebuilding cost basis when a
	// multi-run backfill completes. Nil disables both (window-only mode).
	history repository.ActivityRepository
	// cursors is the durable backfill state. tailOffset is tentative
	// per-wallet progress in actions: it persists only in
	// SnapshotCommitted, after the data it covers is stored. An
	// uncommitted tentative is dropped on the next Fetch so a failed
	// write replays windows instead of skipping them.
	cursors       repository.CursorRepository
	mu            sync.Mutex
	tailOffset    map[string]int
	tailCommitted map[string]bool
	// retireVaults stages superseded synthetic-buy IDs per account for
	// deletion in SnapshotCommitted, after the replacement activities they
	// yield to are persisted.
	retireVaults map[string][]string
}

// tailScope is the cursor-store scope prefix for per-wallet tail backfills.
const tailScopePrefix = "ton-tail-"

// ConfigureCursors sets the durable backfill state. Call during
// construction, before starting synchronization.
func (c *Client) ConfigureCursors(cursors repository.CursorRepository) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursors = cursors
}

// SnapshotCommitted persists tentative tail progress after the snapshot it
// covers is stored. Failed persistence leaves durable state behind so the
// next run replays the same windows instead of skipping them.
func (c *Client) SnapshotCommitted() {
	c.mu.Lock()
	tails := make(map[string]int, len(c.tailOffset))
	for account, offset := range c.tailOffset {
		tails[account] = offset
	}
	if c.tailCommitted == nil {
		c.tailCommitted = make(map[string]bool)
	}
	for account := range tails {
		c.tailCommitted[account] = true
	}
	store := c.cursors
	c.mu.Unlock()
	ctx := context.Background()
	if store != nil {
		for account, offset := range tails {
			scope := tailScopePrefix + account
			if offset <= 0 {
				// Best-effort: a failed clear only replays rows, which
				// upserts absorb idempotently.
				_ = store.Delete(ctx, scope) //nolint:errcheck // replay-safe
				continue
			}
			_ = store.Set(ctx, repository.SyncCursor{ //nolint:errcheck // replay-safe
				Scope: scope, Position: strconv.Itoa(offset),
			})
		}
	}
	c.mu.Lock()
	retire := c.retireVaults
	c.retireVaults = nil
	history := c.history
	c.mu.Unlock()
	// Physical retirement runs here — after every snapshot write the sync
	// loop performs — so a superseded fallback is never deleted before its
	// verified replacement lands. Failures only defer: the next run stages
	// the same IDs again, and the published basis already excludes them.
	for account, ids := range retire {
		if history == nil || len(ids) == 0 {
			continue
		}
		if err := history.Delete(ctx, account, ids); err != nil {
			c.log.Warn().Err(err).Str("account", account).Msg("ton: retiring superseded vault fallback rows failed")
		}
	}
}

// New builds a client. Pass an empty baseURL for production and nil h for a
// default 30s HTTP client. Empty wallet entries are ignored at fetch time.
func New(apiKey string, wallets []string, baseURL string, h HTTPDoer) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		apiKey:     apiKey,
		wallets:    append([]string(nil), wallets...),
		baseURL:    baseURL,
		http:       h,
		log:        zerolog.Nop(),
		now:        time.Now,
		sleep:      time.Sleep,
		retryBase:  defaultRetryBase,
		maxRetries: defaultMaxRetries,
	}
}

// SetLogger attaches structured logging for per-wallet fetch failures.
// Safe to call before/after construction.
func (c *Client) SetLogger(log zerolog.Logger) { c.log = log }

// ConfigureHistory sets the optional stored-ledger source for
// reconciliation (vault-fallback retirement, multi-window basis). Call
// during construction, before starting synchronization.
func (c *Client) ConfigureHistory(history repository.ActivityRepository) {
	c.history = history
}

// ID returns the slug used by sync orchestration.
func (c *Client) ID() string { return "ton" }

// Fetch retrieves balances and history for every configured wallet and folds
// them into a BrokerSnapshot with one account per wallet. Unreachable wallets
// are skipped best-effort with a warning; history failures preserve balances
// while leaving transaction sync incomplete.
func (c *Client) Fetch(ctx context.Context) (domainsync.BrokerSnapshot, error) {
	wallets := slices.DeleteFunc(append([]string(nil), c.wallets...), func(s string) bool {
		return strings.TrimSpace(s) == ""
	})
	if len(wallets) == 0 {
		// No wallets configured → do not surface an empty "TON"
		// connection in the desktop UI.
		return domainsync.BrokerSnapshot{Activities: map[string][]brokerageActivity{}}, nil
	}
	out := make([]TonWalletData, 0, len(wallets))
	type pending struct {
		data    TonWalletData
		forms   []string
		masters map[string]tokenMeta
	}
	var pendings []pending
	seen := make(map[string]bool)
	for _, w := range wallets {
		data, forms, masters, err := c.fetchBalances(ctx, strings.TrimSpace(w))
		if err != nil {
			c.log.Warn().Err(err).Str("address", w).Msg("ton: skipping wallet after fetch failure")
			continue
		}
		if seen[data.Canonical] {
			continue // same wallet configured twice in different forms
		}
		seen[data.Canonical] = true
		pendings = append(pendings, pending{data: data, forms: forms, masters: masters})
	}
	// Counterparty attribution needs no global set anymore: every token
	// movement is a transfer, swaps and stakes carry their own legs.
	tracked := make(map[string]bool)
	for _, p := range pendings {
		for _, f := range p.forms {
			tracked[f] = true
		}
	}
	for _, p := range pendings {
		jettonWallets := make(map[string]bool)
		for _, t := range p.data.Tokens {
			if t.WalletAddr != "" {
				jettonWallets[t.WalletAddr] = true
			}
		}
		startedTail := c.tailCursor(p.forms[0])
		activities, outflows, allowFallback, txErr := c.walletHistory(ctx, p.forms, p.masters, c.valueOrPricer(), jettonWallets, tracked)
		if txErr != nil {
			c.log.Warn().Err(txErr).Str("address", p.data.Address).Msg("ton: transaction history incomplete")
		}
		p.data.Activities = activities
		p.data.MasterOutflows = outflows
		p.data.AllowVaultFallback = allowFallback
		p.data.ActivitiesFetched = txErr == nil
		c.reconcileWallet(ctx, &p.data, startedTail)
		out = append(out, p.data)
	}
	return TranslateTON(out), nil
}

// staleVaultIDs returns the stable synthetic-buy IDs this wallet no longer
// needs: masters with valued outflows whose shares now have verified inbound
// history. A previously stored `vault:<master>` fallback for any of these
// is superseded and must be retired. Freshly emitted fallbacks of this run
// (same stable IDs) are excluded by construction: they only exist for
// symbols without inbound history, which never appear here.
func staleVaultIDs(data *TonWalletData) []string {
	inbound := make(map[string]bool)
	for _, a := range data.Activities {
		if a.Symbol == nil {
			continue
		}
		// Synthetic fallbacks carry vault:<master> source IDs; they are
		// conclusions, never evidence of verified history.
		if strings.HasPrefix(a.SourceRecordID, "vault:") {
			continue
		}
		switch a.Type {
		case brokerage.ActivityBuy, brokerage.ActivityDeposit, brokerage.ActivityTransferIn:
			inbound[cexcommon.NormalizeAsset(a.Symbol.Symbol)] = true
		}
	}
	if len(inbound) == 0 {
		return nil
	}
	symbols := make(map[string]string, len(data.Tokens))
	for _, t := range data.Tokens {
		if t.TokenAddr != "" && t.Quantity > 0 {
			symbols[t.TokenAddr] = cexcommon.NormalizeAsset(t.Symbol)
		}
	}
	var out []string
	for master, spend := range data.MasterOutflows {
		if spend.Amount <= 0 {
			continue
		}
		sym, ok := symbols[master]
		if !ok || !inbound[sym] {
			continue
		}
		out = append(out, "vault:"+master)
	}
	return out
}

// reconcileWallet stages superseded synthetic fallback rows for post-commit
// retirement and, when a multi-run backfill just completed, rebuilds cost
// basis from the stored ledger merged with the current window. It is
// best-effort: failures only defer to the next run, and rows re-derive
// deterministically.
func (c *Client) reconcileWallet(ctx context.Context, data *TonWalletData, startedTail int) {
	if c.history == nil {
		return
	}
	accountID := walletAccountID(canonicalWallet(*data))
	stale := staleVaultIDs(data)
	if len(stale) > 0 {
		c.stageVaultRetirement(accountID, stale)
	}
	// A completion that follows an in-flight tail merged only the refreshed
	// head and the final tail: middle pages live in storage, not in this
	// window. Rebuild lots from the union so the published basis covers the
	// full ledger.
	if !data.ActivitiesFetched || startedTail <= 0 {
		return
	}
	stored, err := listAllActivities(ctx, c.history, accountID)
	if err != nil {
		c.log.Warn().Err(err).Str("account", accountID).Msg("ton: loading stored ledger for basis failed")
		return
	}
	// Exclude superseded synthetic rows whether or not their physical
	// deletion has landed: the verified replacement must never double-count
	// with the fallback it replaces. Without this guard a failed delete
	// would publish an inflated basis.
	retired := make(map[string]bool, len(stale))
	for _, id := range stale {
		retired[id] = true
	}
	seen := make(map[string]bool, len(data.Activities))
	for _, a := range data.Activities {
		seen[a.ID] = true
	}
	merged := make([]brokerageActivity, 0, len(data.Activities)+len(stored))
	merged = append(merged, data.Activities...)
	for _, a := range stored {
		if !seen[a.ID] && !retired[a.SourceRecordID] {
			merged = append(merged, a)
		}
	}
	data.Basis = averageBasis(merged)
}

// stageVaultRetirement queues synthetic-buy IDs for deletion in
// SnapshotCommitted, after the snapshot they yield to is persisted.
func (c *Client) stageVaultRetirement(accountID string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retireVaults == nil {
		c.retireVaults = make(map[string][]string)
	}
	known := make(map[string]bool, len(c.retireVaults[accountID]))
	for _, id := range c.retireVaults[accountID] {
		known[id] = true
	}
	for _, id := range ids {
		if !known[id] {
			known[id] = true
			c.retireVaults[accountID] = append(c.retireVaults[accountID], id)
		}
	}
}

// listAllActivities pages through every stored activity for an account.
func listAllActivities(ctx context.Context, history repository.ActivityRepository, accountID string) ([]brokerageActivity, error) {
	const pageSize = 2000
	var out []brokerageActivity
	for offset := 0; ; {
		rows, total, err := history.List(ctx, repository.ActivityFilter{
			AccountID: accountID, Offset: offset, Limit: pageSize,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		offset += len(rows)
		if offset >= total || len(rows) == 0 {
			return out, nil
		}
	}
}

// valueOrPricer returns the injected USD pricer or the CoinGecko one.
func (c *Client) valueOrPricer() valueUSD {
	if c.value != nil {
		return c.value
	}
	return pricerFor(c)
}

// pricerFor shares the client's transport with the USD pricer without
// leaking the TON Center key: neither upstream receives auth headers.
// Pricing fails fast (3 attempts, short backoff) so a throttled feed
// degrades to unvalued legs instead of stalling the sync.
func pricerFor(c *Client) *pricer {
	return newPricer("", "", c.http, c.sleep, 500*time.Millisecond, 3)
}

// fetchBalances pulls the native state and Jetton balances with metadata.
// History is fetched separately per wallet afterwards.
func (c *Client) fetchBalances(ctx context.Context, address string) (TonWalletData, []string, map[string]tokenMeta, error) {
	fail := func(err error) (TonWalletData, []string, map[string]tokenMeta, error) {
		return TonWalletData{}, nil, nil, err
	}
	state, friendly, err := c.accountState(ctx, address)
	if err != nil {
		return fail(err)
	}
	forms := walletForms(address, state.Address, friendly)
	tokens, masters, err := c.walletTokens(ctx, address)
	if err != nil {
		return fail(err)
	}
	data := TonWalletData{Address: address, Canonical: state.Address, Friendly: friendly, Tokens: tokens}
	if native := nanotonsToTON(state.Balance); native > 0 {
		data.Tokens = append([]TonToken{{
			Symbol:   "TON",
			Quantity: native,
			Decimals: 9,
		}}, data.Tokens...)
	}
	return data, forms, masters, nil
}

// walletForms returns every accepted spelling of a wallet for transfer
// attribution. Comparisons stay case-sensitive: TON addresses are case
// sensitive and must never be lower-cased.
func walletForms(configured, canonical, friendly string) []string {
	forms := []string{configured, canonical, friendly}
	return slices.Compact(slices.Sorted(slices.Values(slices.DeleteFunc(forms, func(s string) bool {
		return s == ""
	}))))
}

// nanotonsToTON converts an integer nanotons string to TON. Unparsable input
// yields zero so callers can skip the row instead of failing the wallet.
func nanotonsToTON(raw string) float64 {
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return float64(n) / nanotonsPerTON
}

// scaledAmount converts a raw integer token amount using Jetton decimals.
// Empty values are normal on TON (zero-value notifications) and map to zero.
// The protocol allows up to 120-bit values (VarUInteger 16), far beyond
// uint64, so parsing uses arbitrary precision and only the final ratio
// rounds to float64.
func scaledAmount(raw string, decimals int) (float64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok || n.Sign() < 0 {
		return 0, fmt.Errorf("ton: invalid token amount %q", raw)
	}
	if decimals < 0 {
		return 0, fmt.Errorf("ton: invalid decimals %d", decimals)
	}
	denom := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	f, _ := new(big.Rat).SetFrac(n, denom).Float64()
	return f, nil
}

// ─── v3 envelope types ───

type tokenInfo struct {
	Symbol string         `json:"symbol"`
	Name   string         `json:"name"`
	Extra  map[string]any `json:"extra"`
}

type addressMeta struct {
	TokenInfo []tokenInfo `json:"token_info"`
}

type accountStateRow struct {
	Address string `json:"address"`
	Balance string `json:"balance"`
	Status  string `json:"status"`
}

type accountStatesResponse struct {
	Error    string            `json:"error"`
	Accounts []accountStateRow `json:"accounts"`
	Book     map[string]struct {
		Friendly string `json:"user_friendly"`
	} `json:"address_book"`
}

func (r accountStatesResponse) envelopeError() string { return r.Error }

type jettonWalletRow struct {
	Address string `json:"address"`
	Balance string `json:"balance"`
	Owner   string `json:"owner"`
	Jetton  string `json:"jetton"`
}

type jettonWalletsResponse struct {
	Error   string                 `json:"error"`
	Wallets []jettonWalletRow      `json:"jetton_wallets"`
	Meta    map[string]addressMeta `json:"metadata"`
}

func (r jettonWalletsResponse) envelopeError() string { return r.Error }

type jettonMasterRow struct {
	Address string `json:"address"`
	Content struct {
		Symbol   string `json:"symbol"`
		Name     string `json:"name"`
		Decimals string `json:"decimals"`
	} `json:"jetton_content"`
}

type jettonMastersResponse struct {
	Error   string                 `json:"error"`
	Masters []jettonMasterRow      `json:"jetton_masters"`
	Meta    map[string]addressMeta `json:"metadata"`
}

func (r jettonMastersResponse) envelopeError() string { return r.Error }

// ─── endpoint getters ───

// accountState returns the native state row plus the friendly address from
// the address book. A missing row means the wallet is unknown to the indexer.
func (c *Client) accountState(ctx context.Context, address string) (accountStateRow, string, error) {
	q := url.Values{"address": {address}}
	var env accountStatesResponse
	if err := c.get(ctx, "/accountStates", q, &env); err != nil {
		return accountStateRow{}, "", err
	}
	if len(env.Accounts) == 0 {
		return accountStateRow{}, "", fmt.Errorf("ton: no account state for %s", address)
	}
	row := env.Accounts[0]
	return row, env.Book[row.Address].Friendly, nil
}

// jettonWalletPage fetches one page of non-zero Jetton wallets plus the
// indexer's token metadata for the page.
func (c *Client) jettonWalletPage(ctx context.Context, owner string, limit, offset int) ([]jettonWalletRow, map[string]addressMeta, error) {
	q := url.Values{
		"owner_address":        {owner},
		"exclude_zero_balance": {"true"},
		"limit":                {strconv.Itoa(limit)},
		"offset":               {strconv.Itoa(offset)},
	}
	var env jettonWalletsResponse
	if err := c.get(ctx, "/jetton/wallets", q, &env); err != nil {
		return nil, nil, err
	}
	return env.Wallets, env.Meta, nil
}

// jettonMasters resolves symbol/decimals for a chunk of Jetton masters,
// merging on-chain content with the indexer's token metadata.
func (c *Client) jettonMasters(ctx context.Context, masters []string) (map[string]tokenMeta, error) {
	q := url.Values{}
	for _, m := range masters {
		q.Add("address", m)
	}
	var env jettonMastersResponse
	if err := c.get(ctx, "/jetton/masters", q, &env); err != nil {
		return nil, err
	}
	out := make(map[string]tokenMeta, len(env.Masters))
	for _, m := range env.Masters {
		meta := tokenMeta{Address: m.Address, Symbol: m.Content.Symbol, Name: m.Content.Name}
		if meta.Symbol == "" {
			if info := firstTokenInfo(env.Meta[m.Address].TokenInfo); info != nil {
				meta.Symbol, meta.Name = info.Symbol, info.Name
				meta.Decimals, meta.DecimalsKnown = decimalsFromExtra(info.Extra)
			}
		} else {
			meta.Decimals, meta.DecimalsKnown = parseDecimals(m.Content.Decimals)
		}
		out[m.Address] = meta
	}
	return out, nil
}

// walletTokens collects every non-zero Jetton balance with resolved metadata.
func (c *Client) walletTokens(ctx context.Context, owner string) ([]TonToken, map[string]tokenMeta, error) {
	var rows []jettonWalletRow
	meta := make(map[string]addressMeta)
	for offset := 0; ; offset += walletPageLimit {
		page, pageMeta, err := c.jettonWalletPage(ctx, owner, walletPageLimit, offset)
		if err != nil {
			return nil, nil, err
		}
		rows = append(rows, page...)
		for addr, m := range pageMeta {
			meta[addr] = m
		}
		if len(page) < walletPageLimit {
			break
		}
	}
	masters := make([]string, 0, len(rows))
	seen := make(map[string]bool)
	for _, row := range rows {
		if row.Jetton != "" && !seen[row.Jetton] {
			seen[row.Jetton] = true
			masters = append(masters, row.Jetton)
		}
	}
	resolved := make(map[string]tokenMeta, len(masters))
	for chunk := range slices.Chunk(masters, masterChunkSize) {
		got, err := c.jettonMasters(ctx, chunk)
		if err != nil {
			return nil, nil, err
		}
		for addr, m := range got {
			// Prefer the fresher page metadata when the master lookup is thin.
			if m.Symbol == "" {
				if info := firstTokenInfo(meta[addr].TokenInfo); info != nil && info.Symbol != "" {
					m.Symbol, m.Name = info.Symbol, info.Name
					m.Decimals, m.DecimalsKnown = decimalsFromExtra(info.Extra)
				}
			}
			resolved[addr] = m
		}
	}
	tokens := make([]TonToken, 0, len(rows))
	for _, row := range rows {
		m := resolved[row.Jetton]
		if m.Symbol == "" {
			c.log.Warn().Str("jetton", row.Jetton).Str("address", owner).
				Msg("ton: skipping Jetton balance without symbol metadata")
			continue
		}
		decimals := m.Decimals
		assumed := false
		if !m.DecimalsKnown {
			decimals, assumed = 9, true
		}
		qty, err := scaledAmount(row.Balance, decimals)
		if err != nil || qty <= 0 {
			continue
		}
		tokens = append(tokens, TonToken{
			Symbol:          cexcommon.NormalizeAsset(m.Symbol),
			Name:            m.Name,
			TokenAddr:       row.Jetton,
			WalletAddr:      row.Address,
			Quantity:        qty,
			Decimals:        decimals,
			DecimalsAssumed: assumed,
		})
	}
	return tokens, resolved, nil
}

// walletHistory collects every action page, resolves asset metadata the
// balances did not cover, verifies multi-leg and deposit traces against raw
// message trees, and classifies each trace. Any collection error aborts the
// rest so partial history is never completed. Trace verification failures
// degrade to interpreted legs and allow the valued fallback.
func (c *Client) walletHistory(ctx context.Context, forms []string, masters map[string]tokenMeta, value valueUSD, jettonWallets map[string]bool, tracked map[string]bool) ([]brokerageActivity, map[string]VaultSpend, bool, error) {
	primary := forms[0]
	actions, actionMeta, histTruncated, histErr := c.walletActionHistory(ctx, primary)
	if histErr != nil && len(actions) == 0 {
		return nil, nil, false, histErr
	}
	if len(actions) > 0 && (histTruncated || histErr != nil) {
		// The oldest group may straddle the window edge (or a failed
		// page): drop it so a split trace never emits as a complete
		// economic operation. It re-fetches whole on a later run.
		groups := groupActionsByTrace(actions)
		if len(groups) > 0 {
			boundary := make(map[string]bool, len(groups[len(groups)-1]))
			for _, a := range groups[len(groups)-1] {
				boundary[a.ActionID] = true
			}
			kept := actions[:0]
			for _, a := range actions {
				if !boundary[a.ActionID] {
					kept = append(kept, a)
				}
			}
			actions = kept
		}
	}
	groups := groupActionsByTrace(actions)
	trees, vaults, enrichFailed := c.verifyTraces(ctx, primary, forms, groups)
	var extraActions []tonAction
	for _, tree := range trees {
		extraActions = append(extraActions, tree.Actions...)
	}
	extra, err := c.resolveActionAssets(ctx, append(actions, extraActions...), masters, actionMeta)
	if err != nil {
		return nil, nil, false, err
	}
	for addr, m := range extra {
		masters[addr] = m
	}
	// Burns and vault mints reference masters the listings never touch
	// (receipts credit jetton wallets, not the wallet itself).
	if extra, err := c.resolveTraceAssets(ctx, trees, masters, actionMeta); err != nil {
		return nil, nil, false, err
	} else {
		for addr, m := range extra {
			masters[addr] = m
		}
	}
	var out []brokerageActivity
	seen := make(map[string]bool)
	outflows := make(map[string]VaultSpend)
	for _, group := range groups {
		var tractx *traceContext
		if len(group) > 0 {
			if tree, ok := trees[group[0].TraceID]; ok {
				tractx = &traceContext{tree: tree, vaults: vaults}
			} else if len(vaults) > 0 {
				tractx = &traceContext{vaults: vaults}
			}
		}
		legs, err := classifyActionGroup(ctx, primary, forms, masters, value, outflows, group, tractx, jettonWallets, tracked)
		if err != nil {
			return nil, nil, false, err
		}
		for _, act := range legs {
			if act != nil && !seen[act.ID] {
				seen[act.ID] = true
				out = append(out, *act)
			}
		}
	}
	// A truncated or partially failed collection still yields its complete
	// groups, but truncation alone must surface as incomplete: Fetch marks
	// ActivitiesFetched from the returned error, and only a short window
	// proves exhaustion.
	if histTruncated && histErr == nil {
		histErr = errHistoryTruncated
	}
	return out, outflows, enrichFailed || len(outflows) > 0, histErr
}

// tailCursor returns the committed backfill offset (in actions) for a
// wallet, seeding from durable state on first use after a restart.
// Uncommitted tentative progress is dropped so failed writes replay.
func (c *Client) tailCursor(account string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if offset, ok := c.tailOffset[account]; ok && c.tailCommitted[account] {
		return offset
	}
	offset := 0
	if c.cursors != nil {
		if cur, err := c.cursors.Get(context.Background(), tailScopePrefix+account); err == nil {
			offset, _ = strconv.Atoi(cur.Position) //nolint:errcheck // corrupt positions restart the backfill
		}
	}
	if c.tailOffset == nil {
		c.tailOffset = make(map[string]int)
	}
	c.tailOffset[account] = offset
	if c.tailCommitted == nil {
		c.tailCommitted = make(map[string]bool)
	}
	c.tailCommitted[account] = true
	return offset
}

// setTailCursor records the backfill offset; zero clears a finished backfill.
// Values stay tentative until SnapshotCommitted persists them.
func (c *Client) setTailCursor(account string, offset int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tailOffset == nil {
		c.tailOffset = make(map[string]int)
	}
	if c.tailCommitted == nil {
		c.tailCommitted = make(map[string]bool)
	}
	if offset <= 0 {
		c.tailOffset[account] = 0
	} else {
		c.tailOffset[account] = offset
	}
	c.tailCommitted[account] = false
}

// walletActionHistory collects a wallet's actions within the per-run budget:
// the newest window refreshes every run, and a tail window advances the
// backfill while one is in flight. It returns the merged newest-first
// actions (deduped by ActionID across offset shifts), whether more actions
// remain beyond the budget, and any collection error. Callers drop the
// boundary-oldest group while truncated or errored.
func (c *Client) walletActionHistory(ctx context.Context, account string) ([]tonAction, map[string]addressMeta, bool, error) {
	newest, newestMeta, newestTruncated, err := c.fetchActionWindow(ctx, account, 0, newestWindowPages)
	if err != nil {
		return newest, newestMeta, false, err
	}
	meta := make(map[string]addressMeta, len(newestMeta))
	maps.Copy(meta, newestMeta)
	if !newestTruncated {
		c.setTailCursor(account, 0)
		return newest, meta, false, nil
	}
	startPage := c.tailCursor(account) / actionPageLimit
	if startPage < newestWindowPages {
		startPage = newestWindowPages
	}
	tail, tailMeta, tailTruncated, err := c.fetchActionWindow(ctx, account, startPage, tailWindowPages)
	maps.Copy(meta, tailMeta)
	merged := make([]tonAction, 0, len(newest)+len(tail))
	seen := make(map[string]bool, len(newest)+len(tail))
	for _, a := range newest {
		if !seen[a.ActionID] {
			seen[a.ActionID] = true
			merged = append(merged, a)
		}
	}
	for _, a := range tail {
		if !seen[a.ActionID] {
			seen[a.ActionID] = true
			merged = append(merged, a)
		}
	}
	if err != nil {
		return merged, meta, true, err
	}
	if tailTruncated {
		// Rewind one page so the boundary group re-fetches whole next run.
		c.setTailCursor(account, startPage*actionPageLimit+len(tail)-actionPageLimit)
		return merged, meta, true, nil
	}
	c.setTailCursor(account, 0)
	return merged, meta, false, nil
}

// needsTraceVerification reports whether a trace group warrants raw message
// tree verification: every multi-leg trace plus every single wallet outflow
// (a deposit candidate whose receipt may hide downstream).
func needsTraceVerification(group []tonAction, forms []string) bool {
	if len(group) > 1 {
		return true
	}
	if len(group) == 0 {
		return false
	}
	a := group[0]
	switch a.Type {
	case "ton_transfer":
		var d tonTransferDetails
		if err := json.Unmarshal(a.Details, &d); err != nil {
			return false
		}
		return slices.Contains(forms, d.Source) && !slices.Contains(forms, d.Destination)
	case "jetton_transfer":
		var d jettonTransferDetails
		if err := json.Unmarshal(a.Details, &d); err != nil {
			return false
		}
		return slices.Contains(forms, d.Sender) && !slices.Contains(forms, d.Receiver)
	default:
		return false
	}
}

// verifyTraces fetches raw message trees for verification-worthy traces and
// merges per-transaction decoded detail into them. Unknown traces keep
// interpreted legs silently; fetch errors allow the valued fallback.
// Vault-intake markers are recorded for outflow actions whose subtree provably
// enters a protocol.
func (c *Client) verifyTraces(ctx context.Context, wallet string, forms []string, groups [][]tonAction) (map[string]*traceEnvelope, map[string]vaultMarker, bool) {
	trees := make(map[string]*traceEnvelope)
	vaults := make(map[string]vaultMarker)
	failed := false
	enriched := 0
	for _, group := range groups {
		if len(group) == 0 || group[0].TraceID == "" {
			continue
		}
		traceID := group[0].TraceID
		if _, done := trees[traceID]; done {
			continue
		}
		if !needsTraceVerification(group, forms) {
			continue
		}
		if enriched >= maxTraceEnrich {
			c.log.Warn().Str("address", wallet).Msg("ton: trace verification hit the call cap; remaining traces keep interpreted legs")
			failed = true
			break
		}
		enriched++
		tree, err := c.fetchTraceByID(ctx, traceID)
		if err != nil {
			c.log.Warn().Err(err).Str("trace", traceID).Msg("ton: trace verification failed; keeping interpreted legs")
			failed = true
			continue
		}
		if tree == nil || len(tree.Txs) == 0 {
			continue
		}
		if root := walletOutflowTx(tree, forms); root != "" {
			if txActs, err := c.actionsByTx(ctx, root); err != nil {
				c.log.Warn().Err(err).Str("trace", traceID).Msg("ton: per-transaction actions failed; keeping tree actions")
				failed = true
			} else {
				known := make(map[string]bool)
				for _, a := range tree.Actions {
					known[a.ActionID] = true
				}
				for _, a := range txActs {
					if !known[a.ActionID] {
						tree.Actions = append(tree.Actions, a)
					}
				}
			}
			if m := detectVaultPattern(tree, root, forms, jettonReservoirs(group)); m.vault != "" {
				for _, a := range group {
					if isWalletOutflow(a, forms) {
						vaults[a.ActionID] = m
					}
				}
			}
		}
		trees[traceID] = tree
	}
	return trees, vaults, failed
}

// isWalletOutflow reports whether a transfer action spends from the wallet.
func isWalletOutflow(a tonAction, forms []string) bool {
	switch a.Type {
	case "ton_transfer":
		var d tonTransferDetails
		if err := json.Unmarshal(a.Details, &d); err != nil {
			return false
		}
		return slices.Contains(forms, d.Source) && !slices.Contains(forms, d.Destination)
	case "jetton_transfer":
		var d jettonTransferDetails
		if err := json.Unmarshal(a.Details, &d); err != nil {
			return false
		}
		return slices.Contains(forms, d.Sender) && !slices.Contains(forms, d.Receiver)
	default:
		return false
	}
}

// resolveActionAssets fetches metadata for swap/stake/transfer assets that
// never appeared in wallet balances (e.g. fully spent tokens), preferring
// the actions response metadata when master lookups are thin.
func (c *Client) resolveActionAssets(ctx context.Context, actions []tonAction, known map[string]tokenMeta, extra map[string]addressMeta) (map[string]tokenMeta, error) {
	var missing []string
	seen := make(map[string]bool)
	consider := func(asset string) {
		if asset == "" || isNativeAsset(asset) || seen[asset] {
			return
		}
		seen[asset] = true
		if _, ok := known[asset]; !ok {
			missing = append(missing, asset)
		}
	}
	for _, a := range actions {
		if !a.Success {
			continue
		}
		switch a.Type {
		case "jetton_transfer":
			var d jettonTransferDetails
			if err := json.Unmarshal(a.Details, &d); err == nil {
				consider(d.Asset)
			}
		case "jetton_swap":
			var d swapDetails
			if err := json.Unmarshal(a.Details, &d); err == nil {
				consider(d.AssetIn)
				consider(d.AssetOut)
			}
		case "stake_deposit":
			var d stakeDetails
			if err := json.Unmarshal(a.Details, &d); err == nil {
				consider(d.Asset)
			}
		case "jetton_burn":
			var d jettonBurnDetails
			if err := json.Unmarshal(a.Details, &d); err == nil {
				consider(d.Asset)
			}
		}
	}
	out := make(map[string]tokenMeta, len(missing))
	for chunk := range slices.Chunk(missing, masterChunkSize) {
		got, err := c.jettonMasters(ctx, chunk)
		if err != nil {
			return nil, err
		}
		for addr, m := range got {
			if m.Symbol == "" {
				if info := firstTokenInfo(extra[addr].TokenInfo); info != nil && info.Symbol != "" {
					m.Symbol, m.Name = info.Symbol, info.Name
					m.Decimals, m.DecimalsKnown = decimalsFromExtra(info.Extra)
				}
			}
			out[addr] = m
		}
	}
	return out, nil
}

// resolveTraceAssets fetches metadata for burn assets and vault mint masters
// found only in raw trace trees, preferring the actions metadata when master
// lookups are thin.
func (c *Client) resolveTraceAssets(ctx context.Context, trees map[string]*traceEnvelope, known map[string]tokenMeta, extra map[string]addressMeta) (map[string]tokenMeta, error) {
	var missing []string
	seen := make(map[string]bool)
	consider := func(asset string) {
		if asset == "" || isNativeAsset(asset) || seen[asset] {
			return
		}
		seen[asset] = true
		if _, ok := known[asset]; !ok {
			missing = append(missing, asset)
		}
	}
	for _, tree := range trees {
		for _, a := range tree.Actions {
			if !a.Success || a.Type != "jetton_burn" {
				continue
			}
			var d jettonBurnDetails
			if err := json.Unmarshal(a.Details, &d); err == nil {
				consider(d.Asset)
			}
		}
		for _, m := range traceMints(tree) {
			consider(m.master)
		}
	}
	out := make(map[string]tokenMeta, len(missing))
	for chunk := range slices.Chunk(missing, masterChunkSize) {
		got, err := c.jettonMasters(ctx, chunk)
		if err != nil {
			return nil, err
		}
		for addr, m := range got {
			if m.Symbol == "" {
				if info := firstTokenInfo(extra[addr].TokenInfo); info != nil && info.Symbol != "" {
					m.Symbol, m.Name = info.Symbol, info.Name
					m.Decimals, m.DecimalsKnown = decimalsFromExtra(info.Extra)
				}
			}
			out[addr] = m
		}
	}
	return out, nil
}

// isNativeAsset reports TON-denominated swap legs, which carry no master.
func isNativeAsset(asset string) bool {
	return strings.EqualFold(strings.TrimSpace(asset), "TON")
}

// get performs one authenticated GET with retry on rate limits, indexer
// timeouts and transient failures. Indexer timeouts arrive as HTTP 200 with
// an {"error": ...} envelope, so the envelope is checked on every attempt.
func (c *Client) get(ctx context.Context, path string, query url.Values, into any) error {
	var lastErr error
	var after time.Duration
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			c.sleep(c.backoff(attempt-1, after))
		}
		// Decode into a fresh value per attempt: reusing the caller's struct
		// would leave a previous attempt's envelope error in place when the
		// next response omits the field.
		target := freshTarget(into)
		var retry bool
		retry, after, lastErr = c.getOnce(ctx, path, query, target)
		if lastErr == nil {
			if into != nil {
				reflect.ValueOf(into).Elem().Set(reflect.ValueOf(target).Elem())
			}
			return nil
		}
		if !retry {
			return lastErr
		}
	}
	return lastErr
}

// freshTarget returns a new zero pointer of the same type for decoding one
// attempt, or nil when there is nothing to decode into.
func freshTarget(into any) any {
	if into == nil {
		return nil
	}
	return reflect.New(reflect.TypeOf(into).Elem()).Interface()
}

// getOnce performs a single attempt, returning retryability and the
// Retry-After delay advertised by the response (zero when absent).
func (c *Client) getOnce(ctx context.Context, path string, query url.Values, into any) (bool, time.Duration, error) {
	requestPath := path
	if len(query) > 0 {
		requestPath += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+requestPath, nil)
	if err != nil {
		return false, 0, err
	}
	// The key stays server-side: this client never runs in a browser and the
	// value only travels in an HTTPS header, never in URLs or logs.
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return true, 0, fmt.Errorf("ton %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return true, 0, fmt.Errorf("ton %s response: %w", path, err)
	}
	after := retryAfter(resp.Header, c.now())
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5 {
		return true, after, fmt.Errorf("ton %s: http %d, retrying", path, resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		return false, 0, fmt.Errorf("ton %s: http %d: %s", path, resp.StatusCode, truncate(string(raw), 200))
	}
	if into == nil {
		return false, 0, nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return true, 0, fmt.Errorf("ton %s decode: %w", path, err)
	}
	if envelope, ok := into.(interface{ envelopeError() string }); ok {
		if msg := envelope.envelopeError(); msg != "" {
			return true, 0, fmt.Errorf("ton %s: indexer error: %s", path, msg)
		}
	}
	return false, 0, nil
}

// backoff returns the delay before the next attempt: exponential growth from
// the base delay, raised to Retry-After when the server asked to wait longer.
func (c *Client) backoff(failedAttempts int, after time.Duration) time.Duration {
	return computeBackoff(c.retryBase, failedAttempts, after)
}

// retryAfter parses the Retry-After response header (delay seconds or an
// HTTP date) relative to now. Absent or invalid values yield zero.
func retryAfter(header http.Header, now time.Time) time.Duration {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(raw); err == nil {
		if delay := retryAt.Sub(now); delay > 0 {
			return delay
		}
	}
	return 0
}

// truncate shortens long upstream bodies for error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// firstTokenInfo returns the first token entry with a symbol, if any.
func firstTokenInfo(infos []tokenInfo) *tokenInfo {
	for i := range infos {
		if infos[i].Symbol != "" {
			return &infos[i]
		}
	}
	return nil
}

// parseDecimals parses on-chain decimals content ("9").
func parseDecimals(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 36 {
		return 0, false
	}
	return n, true
}

// decimalsFromExtra reads decimals from indexer token metadata extras.
func decimalsFromExtra(extra map[string]any) (int, bool) {
	if extra == nil {
		return 0, false
	}
	raw, ok := extra["decimals"]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case string:
		return parseDecimals(v)
	case float64:
		if v == math.Trunc(v) && v >= 0 && v <= 36 {
			return int(v), true
		}
	}
	return 0, false
}
