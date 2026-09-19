// Package steam tracks CS2 inventories as collectible assets through
// Steam itself: community inventory, inventory history, market history,
// trade history and market prices. No third-party skin service is used.
//
// Steam endpoints are unofficial and can 429/403/5xx, paginate oddly or
// return partial data. Every fetcher below therefore has timeouts, bounded
// retries with backoff and jitter, pagination guards and idempotent
// imports; a partial fetch never destroys previously known history.
//
// Credentials (Web API key, community session cookies) are sensitive: they
// travel in requests but are never logged, never appear in errors and are
// never persisted to normal tables.
package steam

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	appprices "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/prices"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	domainsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/sync"
)

// HTTPDoer mirrors http.Client.Do so tests can stub it.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ClientConfig tunes the Steam client. Zero values select defaults;
// endpoint URLs are fields (not constants) so tests and mirrors work.
type ClientConfig struct {
	SteamID string
	APIKey  string
	// Session is a static community Cookie header, kept as a fallback when
	// no refresh token is configured.
	Session string
	// RefreshToken is the WebBrowser refresh token minted by the one-time
	// Node CLI. When set, web cookies derive automatically and the static
	// session is not used.
	RefreshToken  string
	CommunityBase string // https://steamcommunity.com
	StoreBase     string // https://api.steampowered.com
	// MinInterval floors spacing between community requests. It is a
	// politeness default, not a documented Steam guarantee. Values <= 0
	// select the default; tests use a millisecond.
	MinInterval time.Duration
	// MaxPages bounds every pagination loop.
	MaxPages int
	// MaxRetries bounds retries of 429/5xx with backoff and jitter.
	MaxRetries int
	// PriceTTL bounds caching of current market prices.
	PriceTTL time.Duration
	// Currency is the Steam wallet currency code for market prices (1 = USD).
	Currency int
	// HistoryBudget caps price-history backfills per sync (distinct names).
	// Zero disables historical backfill; the operational default (5)
	// comes from STEAM_HISTORY_BUDGET.
	HistoryBudget int
	// MinItemValueUSD drops stacks valued at or below this USD total
	// from positions and activities; only stacks whose combined
	// quantity × price (amounts aggregated by market name) sits strictly
	// above the threshold are emitted. There is
	// no grandfathering (a price drop hides the stack again) and no
	// unpriced exception: stacks without a current or stored quote are
	// excluded too, because unknown value proves nothing. Filtered items
	// stay in the persisted inventory snapshot, so a later price rise
	// re-admits them, and their stale activities are retracted on sync.
	// Zero disables the filter (unpriced items sync as zero-price
	// positions); the operational default (10) comes from
	// STEAM_MIN_ITEM_VALUE_USD. The threshold is denominated in the price
	// currency, USD by default.
	MinItemValueUSD float64
	// CacheNamespace isolates the shared Redis current-price cache between
	// roles with different TTLs (sync vs public): entries are only ever
	// read back by the role that wrote them, so a 24h public entry can
	// never satisfy a 20m sync freshness check. Empty shares the default
	// namespace; the public provider sets its own.
	CacheNamespace string
}

func defaultClientConfig() ClientConfig {
	return ClientConfig{
		CommunityBase:   "https://steamcommunity.com",
		StoreBase:       "https://api.steampowered.com",
		MinInterval:     time.Second,
		MaxPages:        20,
		MaxRetries:      3,
		PriceTTL:        20 * time.Minute,
		Currency:        1,
		HistoryBudget:   0,
		MinItemValueUSD: 0,
	}
}

// Client reads one Steam account's CS2 inventory and its provenance.
type Client struct {
	cfg     ClientConfig
	http    HTTPDoer
	log     zerolog.Logger
	mu      chan struct{}
	lastReq time.Time

	history repository.ActivityRepository
	cursors repository.CursorRepository
	prices  *appprices.Service
	// lastPrice tracks the most recent priceoverview call so those
	// abuse-sensitive reads can be spaced wider than other community
	// calls (see priceOverviewSpacing).
	lastPrice time.Time
	// auth derives web cookies from the refresh token. Non-nil means the
	// static session Cookie header stays off (the jar owns cookies).
	auth *SteamAuthClient
	// store is the optional Steam inventory store (snapshots, provenance,
	// lots). Nil skips steam-specific persistence; the generic sync still
	// persists holdings and activities.
	store repository.SteamAssetRepository
	// priceHistory is the optional market-price history store for bounded
	// backfills. Nil disables them.
	priceHistory repository.PriceHistoryRepository

	pendingCursors []repository.SyncCursor
	pendingDirty   bool
	// emptyUnconfirmed marks a complete empty inventory that must fail
	// closed after recording: holdings stay untouched until a second
	// consecutive observation confirms the wipe. Reset every Fetch.
	emptyUnconfirmed bool
	// priceFails counts consecutive market-price fetch failures. At
	// priceFailBreaker the sync stops pricing and leaves the rest
	// unvalued instead of burning minutes on a throttled endpoint.
	priceFails atomic.Int64
}

// priceFailBreaker trips the pricing circuit breaker.
const priceFailBreaker = 10

// SetPriceService attaches market valuation (Redis current prices,
// database history). Nil leaves items unvalued.
func (c *Client) SetPriceService(p *appprices.Service) { c.prices = p }

// New builds a Steam client. A nil HTTPDoer gets a 15s-timeout client.
// Valuations are USD-only until FX conversion exists: a non-USD currency
// is coerced to 1 (USD) so holdings are never mislabeled.
func New(cfg ClientConfig, h HTTPDoer) *Client {
	def := defaultClientConfig()
	if cfg.CommunityBase == "" {
		cfg.CommunityBase = def.CommunityBase
	}
	if cfg.StoreBase == "" {
		cfg.StoreBase = def.StoreBase
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = def.MinInterval
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = def.MaxPages
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = def.MaxRetries
	}
	if cfg.PriceTTL <= 0 {
		cfg.PriceTTL = def.PriceTTL
	}
	if cfg.HistoryBudget < 0 {
		cfg.HistoryBudget = 0
	}
	if cfg.MinItemValueUSD < 0 {
		cfg.MinItemValueUSD = 0
	}
	if cfg.Currency <= 0 {
		cfg.Currency = def.Currency
	}
	if cfg.Currency != 1 {
		cfg.Currency = 1
	}
	if h == nil {
		// Fresh connections per request: Steam's abuse-sensitive
		// history endpoints throttle reused keep-alive sessions that
		// browsers/curl (fresh connections) sail through. Throughput
		// cost is acceptable for a polling sync client.
		h = &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		}
	}
	return &Client{cfg: cfg, http: h, log: zerolog.Nop(), mu: make(chan struct{}, 1)}
}

// ID returns the slug used by sync orchestration.
func (c *Client) ID() string { return "steam" }

// Currency returns the configured Steam wallet currency code.
func (c *Client) Currency() int {
	if c.cfg.Currency <= 0 {
		return 1
	}
	return c.cfg.Currency
}

// SetLogger attaches structured logging.
func (c *Client) SetLogger(log zerolog.Logger) { c.log = log }

// ConfigureHistory sets the optional stored-ledger source for overlap
// detection. Nil disables it.
func (c *Client) ConfigureHistory(history repository.ActivityRepository) {
	c.history = history
}

// ConfigureCursors sets the durable sync state. Nil disables it.
func (c *Client) ConfigureCursors(cursors repository.CursorRepository) {
	c.cursors = cursors
}

// SetSteamStore attaches Steam inventory persistence. Nil disables it.
func (c *Client) SetSteamStore(store repository.SteamAssetRepository) { c.store = store }

// SetPriceHistoryStore attaches the market-price history store used for
// bounded backfills. Nil disables backfills.
func (c *Client) SetPriceHistoryStore(store repository.PriceHistoryRepository) {
	c.priceHistory = store
}

// ensureAuth builds the refresh-token authenticator on first use and swaps
// the transport for the cookie-jar one. Static sessions keep working when
// no refresh token is configured.
func (c *Client) ensureAuth(ctx context.Context) error {
	if strings.TrimSpace(c.cfg.RefreshToken) == "" || c.auth != nil {
		return nil
	}
	auth := NewSteamAuthClient(c.cfg.SteamID, c.cfg.RefreshToken, c.http, "")
	auth.SetLogger(c.log)
	auth.SetLogger(c.log)
	authed, err := auth.AuthenticatedClient(ctx)
	if err != nil {
		return err
	}
	c.auth = auth
	c.http = authed
	return nil
}

// SnapshotCommitted persists tentative progress after the snapshot it
// covers is stored; failed persistence replays instead of skipping.
func (c *Client) SnapshotCommitted() {
	if !c.pendingDirty {
		return
	}
	c.pendingDirty = false
	if c.cursors == nil {
		return
	}
	for _, cur := range c.pendingCursors {
		_ = c.cursors.Set(context.Background(), cur) //nolint:errcheck // replay-safe
	}
}

// steamErr reports endpoint failures without credentials: paths and status
// codes only, never cookies, keys or bodies.
func steamErr(op string, err error) error {
	return fmt.Errorf("steam %s: %w", op, err)
}

// backoff sleeps exponentially with jitter before attempt n (0-based).
func backoff(n int) time.Duration {
	base := 500 * time.Millisecond
	delay := base * time.Duration(1<<min(n, 4))
	if delay > 8*time.Second {
		delay = 8 * time.Second
	}
	return delay/2 + time.Duration(rand.Int63n(int64(delay)/2+1)) //nolint:gosec // jitter only
}

// throttle spaces community requests by MinInterval.
func (c *Client) throttle(ctx context.Context) error {
	select {
	case c.mu <- struct{}{}:
		defer func() { <-c.mu }()
		if wait := c.cfg.MinInterval - time.Since(c.lastReq); wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		c.lastReq = time.Now()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// throttlePrice spaces priceoverview reads by priceOverviewSpacing times
// the community MinInterval. It shares the request mutex with throttle
// (never nested inside it) and refreshes lastReq too, so the following
// community call keeps its normal spacing from this one.
func (c *Client) throttlePrice(ctx context.Context) error {
	select {
	case c.mu <- struct{}{}:
		defer func() { <-c.mu }()
		now := time.Now()
		wait := c.cfg.MinInterval - now.Sub(c.lastReq)
		if gap := time.Duration(priceOverviewSpacing)*c.cfg.MinInterval - now.Sub(c.lastPrice); gap > wait {
			wait = gap
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		c.lastReq = time.Now()
		c.lastPrice = c.lastReq
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ domainsync.BrokerClient = (*Client)(nil)
