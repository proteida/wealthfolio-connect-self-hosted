package steam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
)

// Steam market pricing plugs into the generic price layer: current quotes
// go through Redis with a configurable TTL, historical points persist in
// the database. No third-party skin service is involved.

// priceOverview mirrors /market/priceoverview.
type priceOverview struct {
	Success     bool   `json:"success"`
	LowestPrice string `json:"lowest_price"`
	MedianPrice string `json:"median_price"`
	Volume      string `json:"volume"`
}

// priceHistory mirrors /market/pricehistory: prices is a list of
// [date-string, median, volume] triples.
type priceHistory struct {
	Success bool    `json:"success"`
	Prices  [][]any `json:"prices"`
}

// normalizeSteamName collapses whitespace runs to single spaces and trims,
// so "Gamma  2 Case" and "Gamma 2 Case" share one price key. Case folding
// happens in PriceAssetKey; this handles the spacing half.
func normalizeSteamName(marketHashName string) string {
	return strings.Join(strings.Fields(marketHashName), " ")
}

// parseSteamMoney parses "$31.28" / "31,28€" / "1,234.56" / "1.234,56"
// style strings into a float. Both US and European decimal conventions
// are handled: when both separators are present the rightmost one wins;
// with only a comma present, a trailing ",d" or ",dd" is the decimal
// part, otherwise commas are thousands separators.
// Currency symbols are stripped; the currency itself comes from the caller,
// which is USD-only until FX conversion exists (see New).
func parseSteamMoney(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "$€£₽฿¥₴₹₩złKč")
	s = strings.TrimRight(s, "$€£₽฿¥₴₹₩złKč")
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "\u00a0", "")
	s = strings.ReplaceAll(s, "'", "")
	s = strings.ReplaceAll(s, "--", "")
	if s == "" || s == "-" {
		return 0, false
	}
	hasDot := strings.Contains(s, ".")
	hasComma := strings.Contains(s, ",")
	switch {
	case hasDot && hasComma:
		// Rightmost separator is decimal.
		if strings.LastIndex(s, ".") > strings.LastIndex(s, ",") {
			// US style: "1,234.56" -> "1234.56".
			s = strings.ReplaceAll(s, ",", "")
		} else {
			// European style: "1.234,56" -> "1234.56".
			s = strings.ReplaceAll(s, ".", "")
			s = strings.ReplaceAll(s, ",", ".")
		}
	case hasComma:
		// Only commas: trailing ",d" or ",dd" is a decimal mark
		// ("31,28" -> "31.28", "1,234" stays thousands).
		if i := strings.LastIndex(s, ","); i >= 0 {
			frac := s[i+1:]
			if len(frac) >= 1 && len(frac) <= 2 && isDigits(frac) {
				s = s[:i] + "." + frac
			} else {
				s = strings.ReplaceAll(s, ",", "")
			}
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// isDigits reports whether s is all ASCII digits (non-empty).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// priceSourceQuote tags current-quote rows written by pricing;
// priceSourceHistory tags full pricehistory series fetched with an
// explicit USD currency request and validated against a fresh quote.
// Legacy "steam_history"/"steam_market" rows predate the currency
// request and may be denominated in the session wallet currency; they
// are purged on startup (see migrateSteamPriceCurrency) and never
// counted as coverage.
const (
	priceSourceQuote   = "steam_quote"
	priceSourceHistory = "steam_history_usd"
)

// historyQuoteDriftBand bounds legitimate disagreement between a fresh
// priceoverview quote and the tail of a pricehistory series. Anything
// beyond it means the series is denominated in another currency (or
// garbage) and must not be stored as USD.
const historyQuoteDriftBand = 5.0

// ErrNoPrice reports that Steam answered but has no usable price for the
// item (success=false or unparseable amounts). It is permanent, not
// transient: callers map it to "not found", never to a retryable failure.
var ErrNoPrice = errors.New("steam: no usable market price")

// PriceAssetKey namespaces a market quote for the shared price stores:
// appid, name and currency travel in the key so nothing collides across
// games. Exported so handlers and backfill share the exact mapping.
// Names are whitespace-collapsed and upper-cased; the currency is the
// caller-provided wallet code (coerced to USD by New).
func PriceAssetKey(marketHashName string, currency int) (asset, curr string) {
	return "steam:730:" + strings.ToUpper(normalizeSteamName(marketHashName)), fmt.Sprintf("STEAM_%d", currency)
}

// priceDedupeKey normalizes a market name for within-sync quote dedupe:
// the same mapping as the asset half of PriceAssetKey, so spelling
// variants share one provider call.
func priceDedupeKey(marketHashName string) string {
	asset, _ := PriceAssetKey(marketHashName, 1)
	return asset
}

// priceKey namespaces a market quote for the shared cache: appid, name
// and currency travel in the key so nothing collides across games.
func priceKey(marketHashName string, currency int) (asset, curr string) {
	return PriceAssetKey(marketHashName, currency)
}

// cacheAsset namespaces the shared Redis key per CacheNamespace so roles
// with different TTLs (sync 20m vs public 24h) never read each other's
// entries. The database key is untouched: only the cache lookup changes.
func (c *Client) cacheAsset(asset string) string {
	if c.cfg.CacheNamespace == "" {
		return asset
	}
	return "ns:" + c.cfg.CacheNamespace + ":" + asset
}

// CurrentPrice returns the median Steam market price (lowest fallback)
// with its observation time. Resolution order: database recency (a stored
// point fresher than the TTL answers without any HTTP, which is what
// makes caching effective across syncs longer than the TTL), then the
// shared Redis cache (namespaced per role, see cacheAsset), then a live
// fetch whose quote is UPSERTed for future recency checks with its
// observation time preserved.
//
// ok=false with err=nil means Steam has no usable price for the item.
// err!=nil means the fetch failed transiently (throttle, 5xx, network)
// and callers must not treat it as "no price".
func (c *Client) CurrentPrice(ctx context.Context, marketHashName string) (float64, string, time.Time, bool, error) { //nolint:gocritic,unnamedResult // price tuple (value, kind, observed, ok, err) is positional across the Steam pricing paths; names would collide with the err locals in every branch.
	asset, curr := priceKey(marketHashName, c.cfg.Currency)
	if c.priceHistory != nil && marketHashName != "" {
		if p, err := c.priceHistory.Get(ctx, asset, curr, time.Now().UTC()); err == nil {
			if time.Since(p.Timestamp) < c.cfg.PriceTTL {
				return p.Price, "steam_history", p.Timestamp, true, nil
			}
		}
	}
	var observed time.Time
	fetch := func(ctx context.Context) (float64, error) {
		v, err := c.fetchOverview(ctx, marketHashName)
		if err != nil {
			// Only transient failures consume breaker budget. A
			// permanent no-price answer (unlisted item) must never
			// trip the breaker and silence every item after it.
			if !errors.Is(err, ErrNoPrice) {
				c.priceFails.Add(1)
			}
			return 0, err
		}
		c.priceFails.Store(0)
		observed = time.Now().UTC()
		return v, nil
	}
	remember := func(v float64) {
		if c.priceHistory == nil || marketHashName == "" || observed.IsZero() {
			// Served from the shared cache without a fresh observation:
			// never re-stamp a possibly stale value with a new
			// timestamp; the original row keeps its observation time.
			return
		}
		now := time.Now().UTC()
		if err := c.priceHistory.Upsert(ctx, []repository.HistoricalPrice{{
			Asset: asset, Timestamp: observed, Currency: curr,
			Price: v, Source: priceSourceQuote, UpdatedAt: now,
		}}); err != nil {
			// Best-effort quote cache: a failed write only means the
			// next lookup refetches instead of serving this row.
			c.log.Debug().Err(err).Str("name", marketHashName).Msg("steam quote cache store failed")
		}
	}
	if c.prices == nil {
		v, err := fetch(ctx)
		if err != nil {
			if errors.Is(err, ErrNoPrice) {
				return 0, "", time.Time{}, false, nil
			}
			return 0, "", time.Time{}, false, err
		}
		remember(v)
		return v, "steam_median", observed, true, nil
	}
	v, err := c.prices.GetCurrentWithTTL(ctx, c.cacheAsset(asset), curr, c.cfg.PriceTTL, fetch)
	if err != nil {
		if errors.Is(err, ErrNoPrice) {
			return 0, "", time.Time{}, false, nil
		}
		return 0, "", time.Time{}, false, err
	}
	remember(v)
	if observed.IsZero() {
		// Shared-cache hit: Redis stores only the price, so no
		// observation timestamp exists. Return zero rather than serve
		// time, letting callers fall back to the real stored timestamp;
		// stamping now would make a TTL-old quote look freshly observed.
		return v, "steam_median", time.Time{}, true, nil
	}
	return v, "steam_median", observed, true, nil
}

// priceOverviewSpacing bounds priceoverview pressure below Steam's
// abuse throttling: those reads are spaced a multiple of the general
// community MinInterval (which also covers inventory and history).
// Single-rate callers that hammered priceoverview at 1/s burned most of
// a sync in Retry-After waits and left throttled items unvalued.
const priceOverviewSpacing = 3

// fetchOverview performs one priceoverview read: median preferred, lowest
// fallback. Requests are deduplicated by callers (one call per distinct
// market_hash_name per sync), never once per asset.
func (c *Client) fetchOverview(ctx context.Context, marketHashName string) (float64, error) {
	if err := c.throttlePrice(ctx); err != nil {
		return 0, err
	}
	q := url.Values{
		"appid":            {"730"},
		"currency":         {strconv.Itoa(c.cfg.Currency)},
		"market_hash_name": {marketHashName},
	}
	var env priceOverview
	if err := c.getCommunity(ctx, "/market/priceoverview/", q, &env); err != nil {
		return 0, err
	}
	if !env.Success {
		return 0, fmt.Errorf("%w for %q: success=false", ErrNoPrice, marketHashName)
	}
	if v, ok := parseSteamMoney(env.MedianPrice); ok {
		return v, nil
	}
	if v, ok := parseSteamMoney(env.LowestPrice); ok {
		return v, nil
	}
	return 0, fmt.Errorf("%w for %q", ErrNoPrice, marketHashName)
}

// SyncPriceHistory fetches a market's full price history, normalizes it
// and UPSERTs it through the shared history store. Steam serves the
// available history rather than a from/to range, so callers persist
// everything and rely on key dedupe; old points are effectively immutable
// and are never refetched once stored (the store answers first).
//
// The request carries an explicit USD currency: without it Steam answers
// in the session wallet currency, which previously stored e.g. UAH
// values labeled as USD. As a second guard, the series tail must agree
// with a fresh priceoverview quote (quote <= 0 means unavailable):
// without a quote to validate against, or on wild disagreement, nothing
// is stored — a history gap is always safer than a currency mismatch.
func (c *Client) SyncPriceHistory(ctx context.Context, marketHashName string, store repository.PriceHistoryRepository, quote float64, haveQuote bool) (int, error) {
	if store == nil {
		return 0, nil
	}
	q := url.Values{
		"appid":            {"730"},
		"currency":         {strconv.Itoa(c.cfg.Currency)},
		"market_hash_name": {marketHashName},
	}
	var env priceHistory
	if err := c.getCommunityAuthed(ctx, "/market/pricehistory/", q, &env); err != nil {
		return 0, err
	}
	if !env.Success {
		return 0, steamErr("pricehistory", fmt.Errorf("success=false for %q", marketHashName))
	}
	asset, curr := priceKey(marketHashName, c.cfg.Currency)
	now := time.Now().UTC()
	var rows []repository.HistoricalPrice
	for _, triple := range env.Prices {
		if len(triple) != 3 {
			continue
		}
		at := parseSteamTime(strVal(triple[0]))
		if at.IsZero() {
			continue
		}
		median, ok := tripleToFloat(triple[1])
		if !ok {
			continue
		}
		rows = append(rows, repository.HistoricalPrice{
			Asset: asset, Timestamp: at, Currency: curr,
			Price: median, Source: priceSourceHistory, UpdatedAt: now,
		})
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if !haveQuote || quote <= 0 {
		c.log.Info().Str("name", marketHashName).Msg("steam price history skipped: no fresh quote to validate currency against")
		return 0, nil
	}
	if seriesDrifted(rows, quote) {
		c.log.Warn().Str("name", marketHashName).Float64("quote", quote).Msg("steam price history rejected: series disagrees with quote, likely wrong currency")
		return 0, nil
	}
	if err := store.Upsert(ctx, rows); err != nil {
		return 0, steamErr("pricehistory store", err)
	}
	return len(rows), nil
}

// seriesDrifted reports whether the tail of a fetched series disagrees
// with a fresh quote beyond any legitimate variance. It compares the
// median of up to the five most recent points so one thin-market print
// cannot condemn a series, while a systematic currency mismatch (tens
// of times off on every point) always trips it.
func seriesDrifted(rows []repository.HistoricalPrice, quote float64) bool {
	if len(rows) == 0 || quote <= 0 {
		return false
	}
	start := 0
	if len(rows) > 5 {
		start = len(rows) - 5
	}
	prices := make([]float64, 0, len(rows)-start)
	for _, p := range rows[start:] {
		prices = append(prices, p.Price)
	}
	sortFloats(prices)
	median := prices[len(prices)/2]
	return median > historyQuoteDriftBand*quote || quote > historyQuoteDriftBand*median
}

func sortFloats(v []float64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func tripleToFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		if t > 0 {
			return t, true
		}
	case json.Number:
		// Decoders with UseNumber deliver numbers as strings.
		return parseSteamMoney(t.String())
	case string:
		return parseSteamMoney(t)
	}
	return 0, false
}

// getCommunityAuthed is getCommunity but requires a session: the history
// endpoint may demand authentication. Without any session it fails fast
// instead of burning retries on 403s. A refresh-token authenticator counts
// as a session (its jar carries the cookies).
func (c *Client) getCommunityAuthed(ctx context.Context, path string, q url.Values, into any) error {
	if c.cfg.Session == "" && c.auth == nil {
		return steamErr("auth", fmt.Errorf("%s requires a Steam session", path))
	}
	return c.getCommunity(ctx, path, q, into)
}

// HistoricalPrice reads one past quote through the shared history store.
func (c *Client) HistoricalPrice(ctx context.Context, store repository.PriceHistoryRepository, marketHashName string, at time.Time) (float64, bool) {
	if store == nil {
		return 0, false
	}
	asset, curr := priceKey(marketHashName, c.cfg.Currency)
	p, err := store.Get(ctx, asset, curr, at)
	if err != nil {
		return 0, false
	}
	return p.Price, true
}
