package steam

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// InventoryItem is one owned CS2 item with its market description joined.
// HasDescription reports whether a description row was found for the
// asset: false means the market name/flags are absent because the join
// missed, not because the item is unmarketable.
type InventoryItem struct {
	AssetID        string
	ClassID        string
	InstanceID     string
	Amount         int
	MarketHashName string
	Marketable     bool
	Tradable       bool
	Name           string
	Type           string
	Tags           map[string]string // category -> internal_name
	HasDescription bool
}

// rawInventory mirrors the community inventory envelope. Amounts arrive as
// strings or numbers depending on the endpoint; anything unparseable keeps
// the item with amount 0 rather than dropping it.
type rawAsset struct {
	AssetID    any `json:"assetid"`
	ClassID    any `json:"classid"`
	InstanceID any `json:"instanceid"`
	Amount     any `json:"amount"`
}

type rawDescription struct {
	ClassID        any             `json:"classid"`
	InstanceID     any             `json:"instanceid"`
	MarketHashName string          `json:"market_hash_name"`
	Marketable     int             `json:"marketable"`
	Tradable       int             `json:"tradable"`
	Name           string          `json:"name"`
	Type           string          `json:"type"`
	Tags           []rawTag        `json:"tags"`
	Raw            json.RawMessage `json:"-"`
}

type rawTag struct {
	Category     string `json:"category"`
	InternalName string `json:"internal_name"`
}

type inventoryPage struct {
	Assets       []rawAsset       `json:"assets"`
	Descriptions []rawDescription `json:"descriptions"`
	MoreItems    any              `json:"more_items"`
	LastAssetID  any              `json:"last_assetid"`
	TotalCount   int              `json:"total_inventory_count"`
	Success      any              `json:"success"`
}

func strVal(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}

func intVal(v any) int {
	s := strVal(v)
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case json.Number:
		return t.String() != "" && t.String() != "0"
	default:
		s := strings.TrimSpace(fmt.Sprint(t))
		return s != "" && s != "0" && !strings.EqualFold(s, "false")
	}
}

func descKey(classID, instanceID string) string { return classID + "\x00" + normInstance(instanceID) }

// normInstance canonicalizes Steam's fungible-instance spellings: generic
// items (notably containers such as the Gamma 2 Case) arrive with
// instanceid "0" on one side of the payload and "" on the other. Both mean
// "no instance" and must join to the same description.
func normInstance(instanceID string) string {
	if instanceID == "" {
		return "0"
	}
	return instanceID
}

// joinInventory binds assets to descriptions by classid+instanceid, with
// "" and "0" instanceids treated as equivalent. Descriptions without assets
// and assets without descriptions are both kept: the former may describe
// stragglers from a partial page, the latter stay visible with empty market
// names instead of vanishing.
func joinInventory(page inventoryPage) []InventoryItem {
	byKey := make(map[string]rawDescription, len(page.Descriptions))
	for _, d := range page.Descriptions {
		byKey[descKey(strVal(d.ClassID), strVal(d.InstanceID))] = d
	}
	out := make([]InventoryItem, 0, len(page.Assets))
	for _, a := range page.Assets {
		classID, instanceID := strVal(a.ClassID), strVal(a.InstanceID)
		it := InventoryItem{
			AssetID:    strVal(a.AssetID),
			ClassID:    classID,
			InstanceID: instanceID,
			Amount:     intVal(a.Amount),
		}
		if d, ok := byKey[descKey(classID, instanceID)]; ok {
			it.HasDescription = true
			it.MarketHashName = d.MarketHashName
			it.Marketable = d.Marketable != 0
			it.Tradable = d.Tradable != 0
			it.Name = d.Name
			it.Type = d.Type
			if len(d.Tags) > 0 {
				it.Tags = make(map[string]string, len(d.Tags))
				for _, t := range d.Tags {
					it.Tags[t.Category] = t.InternalName
				}
			}
		}
		out = append(out, it)
	}
	return out
}

// fetchInventory pulls every inventory page for the configured steamid,
// following more_items/last_assetid. Descriptions are joined globally after
// all pages land: Steam may split an asset and its description across page
// boundaries, and per-page joins would leave such assets nameless.
// Duplicate assetids across overlapping pages are collapsed. It returns
// partial results with an error when a page fails mid-way so callers can
// mark the snapshot partial instead of discarding good pages.
func (c *Client) fetchInventory(ctx context.Context) ([]InventoryItem, bool, error) {
	var rawAssets []rawAsset
	var rawDescs []rawDescription
	seen := map[string]bool{}
	totalCount := 0
	start := ""
	pages := 0
	complete := false
	var pageErr error
	for page := 0; page < c.cfg.MaxPages; page++ {
		q := url.Values{"l": {"english"}, "count": {"2000"}}
		if start != "" {
			q.Set("start_assetid", start)
		}
		var env inventoryPage
		if err := c.getCommunity(ctx, "/inventory/"+c.cfg.SteamID+"/730/2", q, &env); err != nil {
			pageErr = err
			break
		}
		if !truthy(env.Success) {
			pageErr = steamErr("inventory", fmt.Errorf("success=false"))
			break
		}
		pages++
		if env.TotalCount > totalCount {
			totalCount = env.TotalCount
		}
		for _, a := range env.Assets {
			id := strVal(a.AssetID)
			if id != "" && seen[id] {
				continue
			}
			if id != "" {
				seen[id] = true
			}
			rawAssets = append(rawAssets, a)
		}
		rawDescs = append(rawDescs, env.Descriptions...)
		if !truthy(env.MoreItems) {
			complete = true
			break
		}
		next := strVal(env.LastAssetID)
		if next == "" || next == start {
			// The cursor stalled: more data is claimed but no progress is
			// possible. Keep what we have and mark partial.
			break
		}
		start = next
	}
	all := joinInventory(inventoryPage{Assets: rawAssets, Descriptions: rawDescs})
	c.logInventoryFetch(pages, totalCount, len(rawAssets), all)
	if pageErr != nil {
		return all, false, pageErr
	}
	if !complete {
		return all, false, nil
	}
	if totalCount > 0 && len(rawAssets) < totalCount {
		// The server claims more items than it delivered with no cursor to
		// follow: treat as truncated, never as whole.
		c.log.Warn().
			Int("total_count", totalCount).
			Int("received", len(rawAssets)).
			Int("pages", pages).
			Msg("steam inventory truncated: server total exceeds delivered assets")
		return all, false, nil
	}
	return all, true, nil
}

// logInventoryFetch records pagination counts and join health. Only
// assets with no description row count as join misses: described items
// with an empty market name are genuinely unmarketable, not missing.
// Asset and class identifiers plus market names only: never cookies,
// keys or bodies.
func (c *Client) logInventoryFetch(pages, totalCount, raw int, joined []InventoryItem) {
	missCount := 0
	sample := make([]string, 0, 8)
	for _, it := range joined {
		if it.HasDescription {
			continue
		}
		missCount++
		if len(sample) < 8 {
			sample = append(sample, it.AssetID+"/"+it.ClassID+"/"+it.InstanceID)
		}
	}
	c.log.Info().
		Int("pages", pages).
		Int("total_count", totalCount).
		Int("assets", raw).
		Int("joined", len(joined)).
		Int("join_misses", missCount).
		Strs("miss_sample", sample).
		Msg("steam inventory fetched")
}

// getCommunity performs an authenticated-when-configured GET against the
// community base with bounded retries on 429/5xx. 401/403 are terminal:
// the session is missing or rejected, retrying cannot help.
func (c *Client) getCommunity(ctx context.Context, path string, q url.Values, into any) error {
	return c.getWithRetry(ctx, c.cfg.CommunityBase+path+"?"+q.Encode(), true, into)
}

// getStore performs a GET against the Web API base with the API key.
func (c *Client) getStore(ctx context.Context, path string, q url.Values, into any) error {
	q = cloneValues(q)
	if c.cfg.APIKey != "" {
		q.Set("key", c.cfg.APIKey)
	}
	return c.getWithRetry(ctx, c.cfg.StoreBase+path+"?"+q.Encode(), false, into)
}

func cloneValues(q url.Values) url.Values {
	out := make(url.Values, len(q))
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (c *Client) getWithRetry(ctx context.Context, rawURL string, community bool, into any) error {
	var lastErr error
	var after time.Duration
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt - 1)
			// Honor the server's asked delay (429 Retry-After), capped so
			// a hostile header cannot stall the sync.
			if after > wait {
				wait = min(after, 60*time.Second)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if err := c.throttle(ctx); err != nil {
			return err
		}
		var retry bool
		retry, after, lastErr = c.getOnce(ctx, rawURL, community, into)
		if lastErr == nil {
			return nil
		}
		if !retry {
			return lastErr
		}
	}
	return lastErr
}

func (c *Client) getOnce(ctx context.Context, rawURL string, community bool, into any) (bool, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return false, 0, steamErr("request", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wealthfolio-connect/1.0")
	if c.auth == nil && community && c.cfg.Session != "" {
		// Static session only: refresh-token mode owns cookies via its jar.
		req.Header.Set("Cookie", c.cfg.Session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return true, 0, steamErr("http", sanitizeHTTPError(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return true, 0, steamErr("body", err)
	}
	if strings.Contains(rawURL, "inventoryhistory") || strings.Contains(rawURL, "myhistory") {
		// Operational visibility only: counts and status, never bodies
		// (history holds the user's item names) or secrets.
		c.log.Info().Str("path", req.URL.Path).Int("status", resp.StatusCode).Int("bytes", len(raw)).Int("cookies", len(req.Cookies())).Msg("steam history response")
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5:
		return true, retryAfter(resp.Header), steamErr("transient", fmt.Errorf("http %d", resp.StatusCode))
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, 0, steamErr("auth", fmt.Errorf("http %d (session rejected?)", resp.StatusCode))
	case resp.StatusCode/100 != 2:
		return false, 0, steamErr("http", fmt.Errorf("http %d", resp.StatusCode))
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(into); err != nil {
		return false, 0, steamErr("decode", err)
	}
	return false, 0, nil
}

// retryAfter parses the Retry-After response header (seconds or HTTP
// date); zero when absent or unparseable.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := time.Parse(http.TimeFormat, v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
