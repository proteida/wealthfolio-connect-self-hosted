package steam

import (
	"context"
	"encoding/json"
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

// parseSteamMoney parses "$31.28" / "31,28€"-style strings into a float.
// Currency symbols are stripped; the currency itself comes from the caller.
func parseSteamMoney(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "$€£₽฿¥₴₹₩")
	s = strings.TrimRight(s, "$€£₽฿¥₴₹₩")
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, "--", "")
	if s == "" || s == "-" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// PriceAssetKey namespaces a market quote for the shared price stores:
// appid, name and currency travel in the key so nothing collides across
// games. Exported so handlers and backfill share the exact mapping.
func PriceAssetKey(marketHashName string, currency int) (asset, curr string) {
	return "steam:730:" + strings.ToUpper(strings.TrimSpace(marketHashName)), fmt.Sprintf("STEAM_%d", currency)
}

// priceKey namespaces a market quote for the shared cache: appid, name
// and currency travel in the key so nothing collides across games.
func priceKey(marketHashName string, currency int) (asset, curr string) {
	return PriceAssetKey(marketHashName, currency)
}

// CurrentPrice returns the median Steam market price (lowest fallback).
// Resolution order: database recency (a stored point fresher than the TTL
// answers without any HTTP, which is what makes caching effective across
// syncs longer than the TTL), then the shared Redis cache, then a live
// fetch whose quote is UPSERTed for future recency checks.
func (c *Client) CurrentPrice(ctx context.Context, marketHashName string) (float64, string, bool) {
	asset, curr := priceKey(marketHashName, c.cfg.Currency)
	if c.priceHistory != nil && marketHashName != "" {
		if p, err := c.priceHistory.Get(ctx, asset, curr, time.Now().UTC()); err == nil {
			if time.Since(p.Timestamp) < c.cfg.PriceTTL {
				return p.Price, "steam_history", true
			}
		}
	}
	fetch := func(ctx context.Context) (float64, error) {
		return c.fetchOverview(ctx, marketHashName)
	}
	remember := func(v float64) {
		if c.priceHistory == nil || marketHashName == "" {
			return
		}
		now := time.Now().UTC()
		_ = c.priceHistory.Upsert(ctx, []repository.HistoricalPrice{{
			Asset: asset, Timestamp: now, Currency: curr,
			Price: v, Source: "steam_market", UpdatedAt: now,
		}})
	}
	if c.prices == nil {
		v, err := fetch(ctx)
		if err != nil {
			return 0, "", false
		}
		remember(v)
		return v, "steam_median", true
	}
	v, err := c.prices.GetCurrentWithTTL(ctx, asset, curr, c.cfg.PriceTTL, fetch)
	if err != nil {
		return 0, "", false
	}
	remember(v)
	return v, "steam_median", true
}

// fetchOverview performs one priceoverview read: median preferred, lowest
// fallback. Requests are deduplicated by callers (one call per distinct
// market_hash_name per sync), never once per asset.
func (c *Client) fetchOverview(ctx context.Context, marketHashName string) (float64, error) {
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
		return 0, steamErr("priceoverview", fmt.Errorf("success=false for %q", marketHashName))
	}
	if v, ok := parseSteamMoney(env.MedianPrice); ok {
		return v, nil
	}
	if v, ok := parseSteamMoney(env.LowestPrice); ok {
		return v, nil
	}
	return 0, steamErr("priceoverview", fmt.Errorf("no usable price for %q", marketHashName))
}

// SyncPriceHistory fetches a market's full price history, normalizes it
// and UPSERTs it through the shared history store. Steam serves the
// available history rather than a from/to range, so callers persist
// everything and rely on key dedupe; old points are effectively immutable
// and are never refetched once stored (the store answers first).
func (c *Client) SyncPriceHistory(ctx context.Context, marketHashName string, store repository.PriceHistoryRepository) (int, error) {
	if store == nil {
		return 0, nil
	}
	q := url.Values{
		"appid":            {"730"},
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
			Price: median, Source: "steam_market", UpdatedAt: now,
		})
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if err := store.Upsert(ctx, rows); err != nil {
		return 0, steamErr("pricehistory store", err)
	}
	return len(rows), nil
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
