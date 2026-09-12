package steam

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Inventory history event kinds surfaced by the community endpoint. The set
// is observational (Steam documents nothing here): unknown kinds pass
// through verbatim and are stored unmatched rather than dropped.
const (
	EventMarketBuy    = "Purchased on Community Market"
	EventMarketList   = "Listed on Community Market"
	EventMarketReturn = "Returned by Community Market"
	EventTraded       = "Traded"
	EventReceived     = "Received"
	EventCrafted      = "Crafted"
	EventEarned       = "Earned"
	EventUsed         = "Used"
)

// historyCursor is an opaque pagination cursor: its fields are passed back
// exactly as received, never constructed.
type historyCursor struct {
	Time     any `json:"time"`
	TimeFrac any `json:"time_frac"`
	S        any `json:"s"`
}

func (c historyCursor) empty() bool {
	return strVal(c.Time) == "" && strVal(c.TimeFrac) == "" && strVal(c.S) == ""
}

func (c historyCursor) values() url.Values {
	q := url.Values{}
	if v := strVal(c.Time); v != "" {
		q.Set("cursor[time]", v)
	}
	if v := strVal(c.TimeFrac); v != "" {
		q.Set("cursor[time_frac]", v)
	}
	if v := strVal(c.S); v != "" {
		q.Set("cursor[s]", v)
	}
	return q
}

// historyEvent is one normalized inventory-history row.
type historyEvent struct {
	ExternalID     string
	Timestamp      time.Time
	Kind           string
	MarketHashName string
	Quantity       int
	AssetID        string
	ClassID        string
	InstanceID     string
}

// historyPage mirrors the ajax inventory-history envelope. Rows arrive in
// heterogeneous shapes across Steam revisions, so decoding is defensive:
// known fields are extracted, unknown shapes are kept with what they have
// rather than failing the page.
type historyPage struct {
	Success   any               `json:"success"`
	Events    []json.RawMessage `json:"events"`
	History   []json.RawMessage `json:"history"`
	Cursor    json.RawMessage   `json:"cursor"`
	HasMore   any               `json:"more"`
	More      any               `json:"has_more"`
	StartTime any               `json:"start_time"`
	// Keys records the envelope's top-level keys for drift visibility.
	Keys []string `json:"-"`
}

// UnmarshalJSON decodes known fields and records the envelope shape.
func (p *historyPage) UnmarshalJSON(raw []byte) error {
	type plain historyPage
	if err := json.Unmarshal(raw, (*plain)(p)); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	p.Keys = p.Keys[:0]
	for k, v := range keys {
		p.Keys = append(p.Keys, k+"["+kindOf(v)+"]")
	}
	return nil
}

func kindOf(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "empty"
	}
	switch s[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

func eventExternalID(kind string, at time.Time, asset, class, instance, name string, qty int) string {
	return fmt.Sprintf("hist:%s:%d:%s:%s:%s:%s:%d",
		kind, at.UTC().Unix(), asset, class, instance, name, qty)
}

func parseHistoryEvent(raw json.RawMessage) historyEvent {
	var generic map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return historyEvent{ExternalID: fmt.Sprintf("hist:unparsed:%x", rawSnippet(raw))}
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := generic[k]; ok {
				if s := strVal(v); s != "" {
					return s
				}
			}
		}
		return ""
	}
	kind := str("event_name", "type", "action", "name")
	name := str("market_hash_name", "market_name", "item_name")
	asset := str("assetid", "asset_id", "new_assetid")
	classID := str("classid", "class_id")
	instanceID := str("instanceid", "instance_id")
	qty := intVal(generic["quantity"])
	if qty == 0 {
		qty = intVal(generic["amount"])
	}
	if qty == 0 {
		qty = 1
	}
	at := parseSteamTime(str("time", "timestamp", "date", "time_created", "created"))
	ev := historyEvent{
		Timestamp:      at,
		Kind:           kind,
		MarketHashName: name,
		Quantity:       qty,
		AssetID:        asset,
		ClassID:        classID,
		InstanceID:     instanceID,
	}
	ev.ExternalID = str("id", "event_id", "transaction_id", "listingid", "tradeid")
	if ev.ExternalID == "" {
		ev.ExternalID = eventExternalID(kind, at, asset, classID, instanceID, name, qty)
	}
	return ev
}

func rawSnippet(raw json.RawMessage) []byte {
	if len(raw) > 16 {
		return raw[:16]
	}
	return raw
}

// parseSteamTime accepts unix seconds ("1730000000") and common datetime
// shapes; unparseable input yields the zero time rather than an error.
func parseSteamTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		if secs > 1e12 {
			secs /= 1000
		}
		return time.Unix(secs, 0).UTC()
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05", time.RFC3339, "Jan 2, 2006",
		"2 Jan, 2006", "Jan 2 2006",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// fetchHistory walks inventory history from the newest page backwards,
// following the returned cursor exactly. startTime bounds the import when
// set (>0). It returns events newest-first plus whether the range completed
// (false on page failure, cursor stall or page cap: callers must not treat
// the result as exhaustive).
func (c *Client) fetchHistory(ctx context.Context, startTime int64) ([]historyEvent, bool, error) {
	var all []historyEvent
	var cursor *historyCursor
	seen := map[string]bool{}
	for page := 0; page < c.cfg.MaxPages; page++ {
		q := url.Values{"ajax": {"1"}, "app[]": {"730"}}
		if startTime > 0 {
			q.Set("start_time", strconv.FormatInt(startTime, 10))
		}
		if cursor != nil {
			for k, v := range cursor.values() {
				q[k] = v
			}
		}
		var env historyPage
		if err := c.getCommunity(ctx, "/my/inventoryhistory/", q, &env); err != nil {
			return all, false, err
		}
		rows := append(env.Events, env.History...)
		if len(rows) == 0 {
			// Shape visibility only (keys, never content): Steam varies
			// this envelope and silent emptiness hides it.
			c.log.Info().Strs("shape", env.Keys).Msg("steam inventory history empty page")
			return all, true, nil
		}
		for _, raw := range rows {
			ev := parseHistoryEvent(raw)
			if seen[ev.ExternalID] {
				continue // duplicated event: idempotent import
			}
			seen[ev.ExternalID] = true
			all = append(all, ev)
		}
		if !truthy(env.HasMore) && !truthy(env.More) {
			return all, true, nil
		}
		if len(env.Cursor) == 0 || string(env.Cursor) == "null" {
			return all, false, nil
		}
		var next historyCursor
		if err := json.Unmarshal(env.Cursor, &next); err != nil || next.empty() {
			return all, false, nil
		}
		cursor = &next
	}
	return all, false, nil
}
