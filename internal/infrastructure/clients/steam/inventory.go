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

func descKey(classID, instanceID string) string { return classID + "\x00" + instanceID }

// joinInventory binds assets to descriptions by classid+instanceid.
// Descriptions without assets and assets without descriptions are both
// kept: the former may describe stragglers from a partial page, the latter
// stay visible with empty market names instead of vanishing.
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
// following more_items/last_assetid. It returns partial results with an
// error when a page fails mid-way so callers can mark the snapshot
// partial instead of discarding good pages.
func (c *Client) fetchInventory(ctx context.Context) ([]InventoryItem, bool, error) {
	var all []InventoryItem
	start := ""
	for page := 0; page < c.cfg.MaxPages; page++ {
		q := url.Values{"l": {"english"}, "count": {"2000"}}
		if start != "" {
			q.Set("start_assetid", start)
		}
		var env inventoryPage
		if err := c.getCommunity(ctx, "/inventory/"+c.cfg.SteamID+"/730/2", q, &env); err != nil {
			return all, false, err
		}
		all = append(all, joinInventory(env)...)
		if !truthy(env.MoreItems) {
			return all, true, nil
		}
		next := strVal(env.LastAssetID)
		if next == "" || next == start {
			// The cursor stalled: more data is claimed but no progress is
			// possible. Keep what we have and mark partial.
			return all, false, nil
		}
		start = next
	}
	return all, false, nil
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
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(attempt - 1)):
			}
		}
		if err := c.throttle(ctx); err != nil {
			return err
		}
		retry, err := c.getOnce(ctx, rawURL, community, into)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			return err
		}
	}
	return lastErr
}

func (c *Client) getOnce(ctx context.Context, rawURL string, community bool, into any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, steamErr("request", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wealthfolio-connect/1.0")
	if community && c.cfg.Session != "" {
		req.Header.Set("Cookie", c.cfg.Session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return true, steamErr("http", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return true, steamErr("body", err)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5:
		return true, steamErr("transient", fmt.Errorf("http %d", resp.StatusCode))
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, steamErr("auth", fmt.Errorf("http %d (session rejected?)", resp.StatusCode))
	case resp.StatusCode/100 != 2:
		return false, steamErr("http", fmt.Errorf("http %d", resp.StatusCode))
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(into); err != nil {
		return false, steamErr("decode", err)
	}
	return false, nil
}
