// Package okx implements two BrokerClients backed by the OKX REST API:
//
//   - CEX  : /api/v5/account/balance — the regular spot/funding account
//   - Web3 : /api/v6/dex/balance/all-token-balances-by-address and
//     /api/v6/dex/post-transaction/transactions-by-address — wallet
//     balances and history on every supported EVM chain
//
// Both clients share the same v5 HMAC-SHA256 signing scheme (the Web3
// product issues its own API key but signing is identical):
//
//	OK-ACCESS-SIGN = base64(HMAC_SHA256(timestamp + method + path + body, secret))
//
// where timestamp is an ISO 8601 string with milliseconds.
package okx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

func pairBase(instID string) string {
	parts := strings.SplitN(instID, "-", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.ToUpper(parts[0])
}

func pairQuote(instID string) string {
	parts := strings.SplitN(instID, "-", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.ToUpper(parts[1])
}

// isTONWallet identifies TON user-friendly and raw addresses. The OKX
// all-token-balances-by-address endpoint only supports EVM addresses, so TON
// wallets must be ignored before making a request.
func isTONWallet(address string) bool {
	address = strings.TrimSpace(address)
	if address == "" {
		return false
	}
	upper := strings.ToUpper(address)
	return strings.HasPrefix(upper, "EQ") ||
		strings.HasPrefix(upper, "UQ") ||
		strings.HasPrefix(address, "0:") ||
		strings.HasPrefix(address, "-1:")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// Credentials are the per-request fields used to sign every authenticated
// call. Both the CEX and Web3 products use the same shape.
type Credentials struct {
	APIKey     string
	Secret     string
	Passphrase string
}

const (
	defaultBaseURL = "https://www.okx.com"
	// web3BaseURL is the host for the OKX Web3/OnchainOS product. It is
	// separate from the CEX host: v6 DEX endpoints live here.
	web3BaseURL = "https://web3.okx.com"
)

// ============================== CEX client ==============================

// CEXClient reads spot + funding balances from /api/v5/account/balance.
type CEXClient struct {
	baseURL string
	creds   Credentials
	http    HTTPDoer
	// history is the durable source of saved fills (overlap detection).
	// cursors is the durable backfill state (resume position + proven
	// completion). fillsAfter / pendingCursor are tentative progress: they
	// persist only in SnapshotCommitted, after the data they cover is
	// stored. An uncommitted tentative is dropped on the next Fetch so a
	// failed write replays rows instead of skipping them.
	history             repository.ActivityRepository
	cursors             repository.CursorRepository
	fillsAfter          string
	fillsAfterCommitted bool
	pendingCursor       repository.SyncCursor
	pendingDirty        bool
}

// fillsScope is the cursor-store scope for the CEX fills backfill.
const fillsScope = "okx-fills"

// ConfigureHistory sets the durable source of saved fills. Call during
// construction, before starting synchronization.
func (c *CEXClient) ConfigureHistory(history repository.ActivityRepository) {
	c.history = history
}

// ConfigureCursors sets the durable backfill state. Call during
// construction, before starting synchronization.
func (c *CEXClient) ConfigureCursors(cursors repository.CursorRepository) {
	c.cursors = cursors
}

// SnapshotCommitted persists tentative backfill progress after the snapshot
// it covers is stored. Failed persistence leaves the durable cursor behind
// and drops the tentative, so the next run replays the same rows instead of
// skipping them.
func (c *CEXClient) SnapshotCommitted() {
	if !c.pendingDirty {
		return
	}
	c.pendingDirty = false
	c.fillsAfterCommitted = true
	if c.cursors == nil {
		return
	}
	// Best-effort: a failed write only replays rows on the next run, which
	// upserts absorb idempotently.
	_ = c.cursors.Set(context.Background(), c.pendingCursor) //nolint:errcheck // replay-safe
}

// NewCEX builds a CEX client. Pass nil http to use a default 15s client.
func NewCEX(creds Credentials, baseURL string, h HTTPDoer) *CEXClient {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	// Trim trailing slashes so callers can configure either form without
	// producing double-slash URLs (e.g. https://www.okx.com//api/v5/...).
	baseURL = strings.TrimRight(baseURL, "/")
	if h == nil {
		h = &http.Client{Timeout: 15 * time.Second}
	}
	return &CEXClient{baseURL: baseURL, creds: creds, http: h}
}

// ID returns the slug used by sync orchestration.
func (c *CEXClient) ID() string { return "okx" }

// Fetch retrieves balances and trade history, then translates them into a BrokerSnapshot.
func (c *CEXClient) Fetch(ctx context.Context) (domainsync.BrokerSnapshot, error) {
	if c.creds.APIKey == "" || c.creds.Secret == "" || c.creds.Passphrase == "" {
		return domainsync.BrokerSnapshot{}, errors.New("okx: API credentials not fully configured")
	}
	type asset struct {
		Ccy     string `json:"ccy"`
		EqUSD   string `json:"eqUsd"`
		CashBal string `json:"cashBal"`
	}
	type detail struct {
		Details []asset `json:"details"`
	}
	type envelope struct {
		Code string   `json:"code"`
		Msg  string   `json:"msg"`
		Data []detail `json:"data"`
	}
	var env envelope
	if err := c.signedGet(ctx, "/api/v5/account/balance", nil, &env); err != nil {
		return domainsync.BrokerSnapshot{}, fmt.Errorf("okx: balance: %w", err)
	}
	if env.Code != "0" && env.Code != "" {
		return domainsync.BrokerSnapshot{}, fmt.Errorf("okx: api error %s: %s", env.Code, env.Msg)
	}
	snap := cexcommon.Snapshot{}
	for _, d := range env.Data {
		for _, a := range d.Details {
			qty := atof(a.CashBal)
			usd := atof(a.EqUSD)
			if qty == 0 {
				continue
			}
			price := 0.0
			if qty > 0 {
				price = usd / qty
			}
			snap.Balances = append(
				snap.Balances, cexcommon.Balance{
					Asset:    cexcommon.NormalizeAsset(a.Ccy),
					Quantity: qty,
					PriceUSD: price,
					USDValue: usd,
				},
			)
		}
	}

	// Fetch recent trade fills (best-effort; failures don't block the snapshot).
	// Successful pages are retained even when older history remains pending.
	trades, err := c.getFillsHistory(ctx)
	if err == nil {
		snap.Trades = trades
		snap.ActivitiesFetched = true
	} else if errors.Is(err, errFillsPending) {
		snap.Trades = trades
	}

	return cexcommon.Translate("okx", "OKX", snap), nil
}

// errFillsPending signals older fills remain for a later sync. The returned
// trades are still usable; ActivitiesFetched stays false until the
// accessible range is exhausted.
var errFillsPending = errors.New("okx: more fills remain for a later sync")

// fillsPageSize is the fills-history page width we request; fillsPageBudget
// bounds per-run requests while the in-memory cursor advances the backfill.
const fillsPageSize = 100
const fillsPageBudget = 3

// getFillsHistory retrieves the recent 90-day trade fills from OKX.
// OKX /api/v5/trade/fills-history returns up to 100 fills per page; we
// paginate up to 3 pages (300 fills) to capture recent history.
func (c *CEXClient) getFillsHistory(ctx context.Context) ([]cexcommon.Trade, error) {
	type fill struct {
		BillID string `json:"billId"`
		InstID string `json:"instId"`
		Side   string `json:"side"`
		FillPx string `json:"fillPx"`
		FillSz string `json:"fillSz"`
		Size   string `json:"sz"`
		AccSz  string `json:"accFillSz"`
		Fee    string `json:"fee"` // negative means fee charged
		FeeCcy string `json:"feeCcy"`
		TS     string `json:"ts"`
	}
	type envelope struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []fill `json:"data"`
	}

	var all []cexcommon.Trade
	// Resume a committed backfill first; drop uncommitted tentative progress
	// so a failed write replays rows. Otherwise start at the newest fills —
	// except when resuming from durable state after a restart, in which case
	// the stored position continues the backfill (fills that arrive meanwhile
	// import once the range exhausts). Overlap with saved history proves
	// completion only when a previous run durably proved exhaustion: saved
	// data shows a prefix exists, never that older history was exhausted.
	after := ""
	completeProven := false
	if c.fillsAfter != "" && c.fillsAfterCommitted {
		after = c.fillsAfter
	} else {
		c.fillsAfter = ""
		durable := c.loadFillsCursor(ctx)
		completeProven = durable.Complete
		if !completeProven && durable.Position != "" {
			after = durable.Position
		}
	}
	oldest := ""
	if after == "" {
		oldest = c.oldestSavedBillID(ctx)
	}
	complete := false
	for page := 0; page < fillsPageBudget; page++ {
		q := url.Values{}
		q.Set("instType", "SPOT")
		q.Set("limit", strconv.Itoa(fillsPageSize))
		if after != "" {
			q.Set("after", after)
		}
		var env envelope
		if err := c.signedGet(ctx, "/api/v5/trade/fills-history", q, &env); err != nil {
			// Retain successful pages; the next run resumes from the
			// persisted cursor instead of losing them.
			if len(all) > 0 {
				c.fillsAfter = after
				c.fillsAfterCommitted = false
				c.pendingCursor = repository.SyncCursor{Scope: fillsScope, Position: after}
				c.pendingDirty = true
				return all, errFillsPending
			}
			return all, err
		}
		if env.Code != "0" && env.Code != "" {
			if len(all) > 0 {
				c.fillsAfter = after
				c.fillsAfterCommitted = false
				c.pendingCursor = repository.SyncCursor{Scope: fillsScope, Position: after}
				c.pendingDirty = true
				return all, errFillsPending
			}
			return all, fmt.Errorf("okx fills api error %s: %s", env.Code, env.Msg)
		}
		if len(env.Data) == 0 {
			complete = true
			break
		}
		for _, f := range env.Data {
			quantity := firstNonEmpty(f.FillSz, f.Size, f.AccSz)
			if quantity == "" {
				continue
			}
			ts := time.UnixMilli(int64(atof(f.TS))).UTC()
			// OKX signs fills: negative values are fees charged to the
			// account, positive values are maker rebates paid out. A rebate
			// is income, not cost, so it must not land in Fee as a charge;
			// it is preserved as a note instead.
			fee, note := 0.0, ""
			if rawFee := atof(f.Fee); rawFee < 0 {
				fee = -rawFee
			} else if rawFee > 0 {
				note = "Maker rebate " + f.Fee + " " + f.FeeCcy + " excluded from fee"
			}
			all = append(all, cexcommon.Trade{
				ID:         f.BillID,
				Symbol:     f.InstID,
				BaseAsset:  pairBase(f.InstID),
				QuoteAsset: pairQuote(f.InstID),
				Side:       f.Side,
				Price:      atof(f.FillPx),
				Quantity:   atof(quantity),
				Fee:        fee,
				FeeAsset:   f.FeeCcy,
				Note:       note,
				Timestamp:  ts,
			})
		}
		after = env.Data[len(env.Data)-1].BillID
		if oldest != "" && completeProven {
			for _, f := range env.Data {
				if f.BillID == oldest {
					// A previous run proved exhaustion and this contiguous
					// walk from the top rejoined saved history: the range
					// is fully covered.
					complete = true
					break
				}
			}
			if complete {
				break
			}
		}
		if len(env.Data) < fillsPageSize {
			complete = true
			break
		}
	}
	if !complete {
		c.fillsAfter = after
		c.fillsAfterCommitted = false
		c.pendingCursor = repository.SyncCursor{Scope: fillsScope, Position: after}
		c.pendingDirty = true
		return all, errFillsPending
	}
	c.fillsAfter = ""
	c.fillsAfterCommitted = true
	c.pendingCursor = repository.SyncCursor{Scope: fillsScope, Complete: true}
	c.pendingDirty = true
	return all, nil
}

// loadFillsCursor reads durable backfill state; absence or failure means a
// fresh walk (safe: it only replays rows).
func (c *CEXClient) loadFillsCursor(ctx context.Context) repository.SyncCursor {
	if c.cursors == nil {
		return repository.SyncCursor{}
	}
	cur, err := c.cursors.Get(ctx, fillsScope)
	if err != nil {
		return repository.SyncCursor{}
	}
	return cur
}

// oldestSavedBillID returns the oldest persisted fill billID for the spot
// account, or "" when history is absent or unreadable. It is the durable
// cursor that lets a restarted backfill stop at already-saved history
// instead of rewalking it.
func (c *CEXClient) oldestSavedBillID(ctx context.Context) string {
	if c.history == nil {
		return ""
	}
	_, total, err := c.history.List(ctx, repository.ActivityFilter{AccountID: "okx-spot", Limit: 1})
	if err != nil || total <= 0 {
		return ""
	}
	rows, _, err := c.history.List(ctx, repository.ActivityFilter{AccountID: "okx-spot", Offset: total - 1, Limit: 1})
	if err != nil || len(rows) == 0 {
		return ""
	}
	id := rows[0].SourceRecordID
	id = strings.TrimSuffix(id, "-quote")
	id = strings.TrimSuffix(id, "-base")
	if id == "" {
		return ""
	}
	return id
}

func (c *CEXClient) signedGet(ctx context.Context, path string, query url.Values, into any) error {
	return signedRequest(ctx, c.http, c.baseURL, http.MethodGet, path, query, nil, c.creds, into)
}

// ============================== Web3 client ==============================

// Web3Client reads on-chain balances per (address, chain) tuple via OKX's
// DEX product. It is wired separately from the CEX client because:
//
//   - It uses a *different* set of API credentials issued under OKX Web3.
//   - The endpoint expects an `address` + `chains` query string instead of
//     pulling balances from the user's centralized account.
//
// We aggregate every configured wallet into a single connection ("okx-web3")
// with one brokerage Account per (chain, wallet).
type Web3Client struct {
	baseURL    string
	creds      Credentials
	wallets    []Wallet
	http       HTTPDoer
	log        zerolog.Logger
	chainMu    sync.Mutex
	chainCache map[string]chainCacheEntry
	now        func() time.Time
}

// Wallet is one configured user wallet driven through OKX Web3.
type Wallet struct {
	Address string   // EVM hex address, lower or mixed case
	Chains  []string // OKX chainIndex strings: "1" eth, "56" bsc, "137" matic, "42161" arb, ...
	Label   string   // optional human label
}

// NewWeb3 builds a Web3 client.
func NewWeb3(creds Credentials, wallets []Wallet, baseURL string, h HTTPDoer) *Web3Client {
	if baseURL == "" {
		baseURL = web3BaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	return &Web3Client{baseURL: baseURL, creds: creds, wallets: wallets, http: h, log: zerolog.Nop(), chainCache: make(map[string]chainCacheEntry), now: time.Now}
}

// SetLogger attaches a structured logger so transient per-wallet failures
// in Fetch can be surfaced to operators instead of being silently dropped.
// Safe to call before/after construction; nil resets to a no-op logger.
func (c *Web3Client) SetLogger(log zerolog.Logger) {
	c.log = log
}

// ID returns the slug "okx_web3".
func (c *Web3Client) ID() string { return "okx_web3" }

// Web3Token is one balance row returned by the OKX DEX API.
type Web3Token struct {
	Symbol     string // e.g. "ETH"
	TokenAddr  string // contract address (empty for native)
	ChainIndex string // OKX chainIndex
	Quantity   float64
	PriceUSD   float64 // per-token USD price
}

// Fetch retrieves balances for every configured wallet and translates them
// into a BrokerSnapshot with one account per wallet. Wallets configured
// but unreachable are skipped on a best-effort basis (the rest of the
// snapshot still goes through), and each failure is logged so operators
// can investigate before the data quietly goes stale.
func (c *Web3Client) Fetch(ctx context.Context) (domainsync.BrokerSnapshot, error) {
	if c.creds.APIKey == "" || c.creds.Secret == "" || c.creds.Passphrase == "" {
		return domainsync.BrokerSnapshot{}, errors.New("okx_web3: API credentials not fully configured")
	}
	if len(c.wallets) == 0 {
		// No wallets configured → do not surface an empty "OKX Web3"
		// connection in the desktop UI.
		return domainsync.BrokerSnapshot{Activities: map[string][]brokerage.Activity{}}, nil
	}
	out := make([]Web3WalletData, 0, len(c.wallets))
	for _, w := range c.wallets {
		if isTONWallet(w.Address) {
			// The OKX balance and history endpoints are EVM-only.
			// Never send requests for TON wallets.
			continue
		}
		chains, historyChains, err := c.resolveChains(ctx, w)
		if err != nil {
			c.log.Warn().Err(err).Str("address", w.Address).Str("label", w.Label).
				Msg("okx_web3: skipping wallet after chain discovery failure")
			continue
		}
		w.Chains = chains
		if len(chains) == 0 && len(historyChains) == 0 {
			// No funded chains and no known-active chains: the history
			// scope is unknown, so it stays incomplete instead of reading
			// as completed empty history.
			out = append(out, Web3WalletData{Wallet: w})
			continue
		}
		if hasUnsupportedChain(chains) {
			// OKX's balance and transaction endpoints both reject TON,
			// Solana, and Bitcoin chain IDs. Keep the configured wallet
			// account, but do not send unsupported requests.
			out = append(out, Web3WalletData{Wallet: w})
			continue
		}
		var tokens []Web3Token
		if len(chains) > 0 {
			// Balances run over funded chains only: with no funded chains
			// there is nothing to value, and an empty chain list would
			// send one unrestricted balance request.
			var err error
			tokens, err = c.fetchWallet(ctx, w)
			if err != nil {
				c.log.Warn().Err(err).Str("address", w.Address).Str("label", w.Label).
					Msg("okx_web3: skipping wallet after fetch failure")
				continue
			}
		}
		// History runs over the active-chain scope, not the funded-chain
		// scope, so withdrawals that emptied a chain still import. An empty
		// scope can never read as complete.
		hw := w
		hw.Chains = historyChains
		if len(hw.Chains) == 0 {
			hw.Chains = chains
		}
		transactions, txErr := c.fetchTransactions(ctx, hw)
		if txErr != nil {
			c.log.Warn().Err(txErr).Str("address", w.Address).Msg("okx_web3: transaction history incomplete")
		}
		out = append(out, Web3WalletData{Wallet: w, Tokens: tokens, Activities: transactions, ActivitiesFetched: txErr == nil && len(hw.Chains) > 0})
	}
	return TranslateWeb3(out), nil
}

// Web3WalletData bundles one wallet's payload for the translator.
type Web3WalletData struct {
	Wallet            Wallet
	Tokens            []Web3Token
	Activities        []brokerage.Activity
	ActivitiesFetched bool
}

func (c *Web3Client) fetchWallet(ctx context.Context, w Wallet) ([]Web3Token, error) {
	if len(w.Chains) == 0 {
		// Discovery probe or empty resolution: one unrestricted request.
		return c.fetchBalanceBatch(ctx, w)
	}
	var all []Web3Token
	for chains := range slices.Chunk(w.Chains, 50) {
		batch := w
		batch.Chains = chains
		tokens, err := c.fetchBalanceBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		all = append(all, tokens...)
	}
	return all, nil
}

func (c *Web3Client) fetchBalanceBatch(ctx context.Context, w Wallet) ([]Web3Token, error) {
	type tokenRow struct {
		Symbol       string `json:"symbol"`
		TokenAddress string `json:"tokenContractAddress"`
		ChainIndex   string `json:"chainIndex"`
		Balance      string `json:"balance"`
		TokenPrice   string `json:"tokenPrice"`
		IsRiskToken  bool   `json:"isRiskToken"`
	}
	type assetGroup struct {
		TokenAssets []tokenRow `json:"tokenAssets"`
	}
	type envelope struct {
		Code string       `json:"code"`
		Msg  string       `json:"msg"`
		Data []assetGroup `json:"data"`
	}
	q := url.Values{}
	q.Set("address", w.Address)
	if len(w.Chains) > 0 {
		q.Set("chains", strings.Join(w.Chains, ","))
	}
	q.Set("excludeRiskToken", "0") // documented v6 value: filter risky tokens
	var env envelope
	if err := signedRequest(
		ctx, c.http, c.baseURL,
		http.MethodGet, "/api/v6/dex/balance/all-token-balances-by-address",
		q, nil, c.creds, &env,
	); err != nil {
		return nil, err
	}
	if env.Code != "0" && env.Code != "" {
		return nil, fmt.Errorf("okx_web3 api error %s: %s", env.Code, env.Msg)
	}
	out := make([]Web3Token, 0)
	for _, g := range env.Data {
		for _, t := range g.TokenAssets {
			if t.IsRiskToken || !slices.Contains(w.Chains, t.ChainIndex) {
				continue
			}
			qty, err := finiteAmount(t.Balance)
			if err != nil {
				return nil, fmt.Errorf("okx_web3 balance %s: %w", t.ChainIndex, err)
			}
			var price float64
			if t.TokenPrice != "" {
				price, err = finiteAmount(t.TokenPrice)
				if err != nil {
					return nil, fmt.Errorf("okx_web3 token price %s: %w", t.ChainIndex, err)
				}
			}
			if qty == 0 {
				continue
			}
			out = append(
				out, Web3Token{
					Symbol:     cexcommon.NormalizeAsset(t.Symbol),
					TokenAddr:  t.TokenAddress,
					ChainIndex: t.ChainIndex,
					Quantity:   qty,
					PriceUSD:   price,
				},
			)
		}
	}
	return out, nil
}

// TranslateWeb3 folds every wallet's tokens into a single connection with
// one account per wallet. Public for testability.
func TranslateWeb3(wallets []Web3WalletData) domainsync.BrokerSnapshot {
	now := time.Now().UTC()
	connection := brokerage.Connection{
		ID:              "okx-web3-conn",
		AuthorizationID: "okx-web3-auth",
		BrokerageName:   "OKX Web3",
		BrokerageSlug:   "okx_web3",
		DisplayName:     "OKX Web3 (DeFi)",
		Name:            "OKX Web3",
		Status:          brokerage.ConnectionActive,
		UpdatedAt:       now,
	}

	accounts := make([]brokerage.Account, 0, len(wallets))
	holdings := make([]brokerage.Holdings, 0, len(wallets))
	for _, w := range wallets {
		accountID := "okxweb3-" + strings.ToLower(w.Wallet.Address)
		var totalUSD, stableCash float64
		var positions []brokerage.Position
		for _, t := range w.Tokens {
			usd := t.Quantity * t.PriceUSD
			// Stablecoins must be swept into the cash balance *before* the
			// dust filter runs, otherwise tiny stable balances (e.g. 0.5
			// USDC) get dropped instead of contributing to cash.
			if cexcommon.IsStablecoin(t.Symbol) {
				stableCash += usd
				totalUSD += usd
				continue
			}
			if usd < 1 {
				continue
			}
			totalUSD += usd
			positions = append(
				positions, brokerage.Position{
					Symbol: brokerage.Symbol{
						Symbol:      t.Symbol,
						RawSymbol:   t.Symbol,
						Name:        t.Symbol,
						Description: t.Symbol,
						Type:        brokerage.SymbolType{Code: "CRYPTO", IsSupported: true},
						Exchange:    brokerage.Exchange{Code: chainName(t.ChainIndex), Name: chainName(t.ChainIndex)},
						Currency:    brokerage.Currency{Code: "USD"},
					},
					Units: t.Quantity,
					Price: t.PriceUSD,
					// Basis unknown from balances alone; never stamp the
					// market price as cost basis.
					Currency: brokerage.Currency{Code: "USD"},
				},
			)
		}
		label := w.Wallet.Label
		if label == "" {
			label = "Wallet " + shortAddr(w.Wallet.Address)
		}
		acc := brokerage.Account{
			ID:                     accountID,
			Name:                   label,
			AccountNumber:          shortAddr(w.Wallet.Address),
			Type:                   brokerage.AccountTypeCryptocurrency,
			RawType:                "CRYPTO_WALLET",
			Currency:               "USD",
			BalanceTotal:           totalUSD,
			BalanceCurrency:        "USD",
			BrokerageAuthorization: "okx-web3-auth",
			InstitutionName:        "OKX Web3",
			SyncEnabled:            true,
			Status:                 "open",
			CreatedDate:            now,
			LastHoldingsSync:       &now,
			InitialHoldingsDone:    true,
		}
		if w.ActivitiesFetched {
			acc.InitialTxSyncDone = true
			acc.LastTxSync = &now
		}
		accounts = append(accounts, acc)
		holdings = append(
			holdings, brokerage.Holdings{
				AccountID: accountID,
				Balances: []brokerage.Balance{
					{Currency: brokerage.Currency{Code: "USD"}, Cash: stableCash},
				},
				Positions:  positions,
				CapturedAt: now,
			},
		)
	}
	activities := make(map[string][]brokerage.Activity)
	for _, w := range wallets {
		if len(w.Activities) > 0 {
			activities[walletAccountID(w.Wallet.Address)] = w.Activities
		}
	}
	return domainsync.BrokerSnapshot{
		Connection: connection,
		Accounts:   accounts,
		Holdings:   holdings,
		Activities: activities,
	}
}

func hasUnsupportedChain(chains []string) bool {
	for _, chain := range chains {
		if unsupportedWalletChains[chain] {
			return true
		}
	}
	return false
}

// chainName maps OKX chainIndex → human-friendly exchange code.
// Reference: https://www.okx.com/web3/build/docs/waas/dex-supported-chain
func chainName(idx string) string {
	switch idx {
	case "1":
		return "ETHEREUM"
	case "56":
		return "BSC"
	case "137":
		return "POLYGON"
	case "42161":
		return "ARBITRUM"
	case "10":
		return "OPTIMISM"
	case "8453":
		return "BASE"
	case "43114":
		return "AVALANCHE"
	case "324":
		return "ZKSYNC"
	case "59144":
		return "LINEA"
	case "5000":
		return "MANTLE"
	default:
		return "CHAIN-" + idx
	}
}

func shortAddr(a string) string {
	if len(a) < 10 {
		return a
	}
	return a[:6] + "..." + a[len(a)-4:]
}

// ============================== shared signing ==============================

// signedRequest signs an OKX v5 request and decodes its JSON body into into.
// Both CEX and Web3 endpoints accept the exact same signing scheme.
func signedRequest(
	ctx context.Context,
	doer HTTPDoer,
	baseURL, method, path string,
	query url.Values,
	body []byte,
	creds Credentials,
	into any,
) error {
	requestPath := path
	if len(query) > 0 {
		requestPath = path + "?" + query.Encode()
	}
	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	prehash := timestamp + method + requestPath
	if len(body) > 0 {
		prehash += string(body)
	}
	mac := hmac.New(sha256.New, []byte(creds.Secret))
	_, _ = mac.Write([]byte(prehash))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, method, baseURL+requestPath, bodyReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("OK-ACCESS-KEY", creds.APIKey)
	req.Header.Set("OK-ACCESS-SIGN", sign)
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", creds.Passphrase)
	req.Header.Set("Content-Type", "application/json")

	resp, err := doer.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(raw))
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(raw, into)
}

func bodyReader(b []byte) io.Reader {
	if len(b) == 0 {
		return http.NoBody
	}
	return strings.NewReader(string(b))
}

func atof(s string) float64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64) //nolint:errcheck // exchange returns numeric strings; treat unparsable as zero
	return v
}
