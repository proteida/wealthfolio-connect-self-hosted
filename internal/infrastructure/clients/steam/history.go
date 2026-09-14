package steam

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Inventory history event kinds. The inventory-history endpoint renders
// rows as HTML (`div.tradehistoryrow`); kinds below are the observed row
// verbs, matched by prefix. Anything else passes through verbatim as
// "other:<verb>" and is stored unmatched rather than dropped.
const (
	EventMarketBuy    = "Purchased on Community Market"
	EventMarketList   = "Listed on Community Market"
	EventMarketReturn = "Returned by Community Market"
	EventTraded       = "Traded"
	EventReceived     = "Received"
	EventCrafted      = "Crafted"
	EventEarned       = "Earned"
	EventUsed         = "Used"
	EventUnboxed      = "Unboxed"
)

// historyCursor is an opaque pagination cursor: its fields are passed back
// exactly as received, never constructed. Observed shape:
// {"time": 1774720493, "time_frac": 737000000, "s": "30933525991"}.
type historyCursor struct {
	Time     any `json:"time"`
	TimeFrac any `json:"time_frac"`
	S        any `json:"s"`
}

func (c historyCursor) empty() bool {
	return strVal(c.Time) == "" && strVal(c.TimeFrac) == "" && strVal(c.S) == ""
}

func (c historyCursor) key() string {
	return strVal(c.Time) + "|" + strVal(c.TimeFrac) + "|" + strVal(c.S)
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

// historyDesc is one description entry (keyed classid_instanceid).
type historyDesc struct {
	MarketHashName string
}

// historyPage mirrors the ajax inventory-history envelope:
// {success, html, num, descriptions: {appid: {classid_instanceid: {...}}},
// apps, cursor}. Rows live in html; there is no more-flag, so pagination
// follows the cursor until rows stop or it stalls.
type historyPage struct {
	Success      any                                   `json:"success"`
	HTML         string                                `json:"html"`
	Num          int                                   `json:"num"`
	Descriptions map[string]map[string]json.RawMessage `json:"descriptions"`
	Cursor       json.RawMessage                       `json:"cursor"`
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

var (
	rowSplitHistoryRe = regexp.MustCompile(`<div class="tradehistoryrow"`)
	rowAttrRe         = regexp.MustCompile(`data-([a-zA-Z0-9_-]+)="([^"]*)"`)
	// rowTextLead matches "> 3 Aug, 2026 1:36am " at the start of row text.
	rowDateRe = regexp.MustCompile(`^\s*>?\s*(\d{1,2}\s+\w+,\s+\d{4}\s+\d{1,2}:\d{2}(?:am|pm))`)
	// rowSplitSign splits "KIND WORDS - ITEM" / "KIND WORDS + ITEM".
	rowSignRe = regexp.MustCompile(`^(.*?)\s+([+-])\s+(.*)$`)
)

// normalizeHistoryKind maps observed row verbs to event kinds. Unknown
// verbs pass through as "other:<verb>" so new Steam phrasing stays visible
// instead of collapsing into a lie.
func normalizeHistoryKind(verb string) string {
	verb = strings.TrimSpace(verb)
	lower := strings.ToLower(verb)
	for _, known := range []string{
		EventMarketBuy, EventMarketList, EventMarketReturn,
		EventTraded, EventReceived, EventCrafted,
		EventEarned, EventUsed, EventUnboxed,
	} {
		if strings.HasPrefix(lower, strings.ToLower(known)) {
			return known
		}
	}
	// Short verbs without the full phrasing still map when unambiguous.
	switch {
	case strings.HasPrefix(lower, "purchased"):
		return EventMarketBuy
	case strings.HasPrefix(lower, "listed"):
		return EventMarketList
	case strings.HasPrefix(lower, "traded"):
		return EventTraded
	case strings.HasPrefix(lower, "received"):
		return EventReceived
	case strings.HasPrefix(lower, "crafted"):
		return EventCrafted
	case strings.HasPrefix(lower, "earned"):
		return EventEarned
	case strings.HasPrefix(lower, "unboxed"):
		return EventUnboxed
	case strings.HasPrefix(lower, "deleted"), strings.HasPrefix(lower, "destroyed"),
		strings.HasPrefix(lower, "used"), strings.HasPrefix(lower, "consumed"):
		return EventUsed
	}
	if verb == "" {
		return "other"
	}
	return "other:" + verb
}

// parseHistoryRows normalizes one page's HTML rows. descs resolves names
// by classid_instanceid; rows without any usable identity are still kept
// (kind + date) with synthetic IDs rather than dropped.
func parseHistoryRows(html string, descs map[string]historyDesc) []historyEvent {
	parts := rowSplitHistoryRe.Split(html, -1)
	if len(parts) <= 1 {
		return nil
	}
	var out []historyEvent
	seen := map[string]bool{}
	for _, part := range parts[1:] {
		attrs := map[string]string{}
		for _, m := range rowAttrRe.FindAllStringSubmatch(part, -1) {
			attrs[strings.ToLower(m[1])] = m[2]
		}
		// Splitting on the row marker leaves the opening tag's attribute
		// tail ahead of the content; cut a leading fragment without "<"
		// up to its closing ">" (attributes were extracted above).
		if i := strings.Index(part, ">"); i >= 0 && !strings.Contains(part[:i], "<") {
			part = part[i+1:]
		}
		text := stripTags(part)
		dateStr := rowDateRe.FindStringSubmatch(text)
		if dateStr == nil {
			continue
		}
		at := parseSteamTime(dateStr[1])
		rest := text[len(dateStr[0]):]
		kind, item := "", ""
		if m := rowSignRe.FindStringSubmatch(strings.TrimSpace(rest)); m != nil {
			kind, item = m[1], m[3]
		} else {
			kind = strings.TrimSpace(rest)
		}
		classID, instanceID := attrs["classid"], attrs["instanceid"]
		name := ""
		if d, ok := descs[classID+"_"+instanceID]; ok {
			name = d.MarketHashName
		}
		if name == "" {
			name = strings.TrimSpace(item)
		}
		qty := 1
		if q, err := strconv.Atoi(attrs["amount"]); err == nil && q > 0 {
			qty = q
		}
		ev := historyEvent{
			Timestamp:      at,
			Kind:           normalizeHistoryKind(kind),
			MarketHashName: name,
			Quantity:       qty,
			AssetID:        attrs["assetid"],
			ClassID:        classID,
			InstanceID:     instanceID,
		}
		ev.ExternalID = eventExternalID(ev.Kind, at, ev.AssetID, classID, instanceID, name, qty)
		if seen[ev.ExternalID] {
			continue
		}
		seen[ev.ExternalID] = true
		out = append(out, ev)
	}
	return out
}

// parseHistoryDescs flattens descriptions.{appid}.{classid_instanceid} to
// names. Only market_hash_name is extracted; the rest stays server-side.
func parseHistoryDescs(raw map[string]map[string]json.RawMessage) map[string]historyDesc {
	out := map[string]historyDesc{}
	for _, byClass := range raw {
		for key, entry := range byClass {
			var d struct {
				MarketHashName string `json:"market_hash_name"`
			}
			if err := json.Unmarshal(entry, &d); err != nil || d.MarketHashName == "" {
				continue
			}
			out[key] = historyDesc{MarketHashName: d.MarketHashName}
		}
	}
	return out
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
		"2 Jan, 2006", "Jan 2 2006", "2 Jan, 2006 3:04pm",
		// /market/pricehistory/ date shape, e.g. "Sep 13 2025 01: +0".
		"Jan 02 2006 15: +0", "Jan 2 2006 15: +0",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// fetchHistory walks inventory history from the newest page backwards,
// following the returned cursor exactly. startTime bounds the import when
// set (>0). It returns events newest-first, the description map, and
// whether the range completed (false on page failure, cursor stall or page
// cap: callers must not treat the result as exhaustive).
func (c *Client) fetchHistory(ctx context.Context, startTime int64) ([]historyEvent, map[string]historyDesc, bool, error) {
	var all []historyEvent
	descs := map[string]historyDesc{}
	var cursor *historyCursor
	seenCursor := map[string]bool{}
	seenEvents := map[string]bool{}
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
			return all, descs, false, err
		}
		for k, d := range parseHistoryDescs(env.Descriptions) {
			descs[k] = d
		}
		rows := parseHistoryRows(env.HTML, descs)
		if len(rows) == 0 {
			return all, descs, true, nil
		}
		for _, ev := range rows {
			if seenEvents[ev.ExternalID] {
				continue // duplicated event: idempotent import
			}
			seenEvents[ev.ExternalID] = true
			all = append(all, ev)
		}
		if len(env.Cursor) == 0 || string(env.Cursor) == "null" {
			return all, descs, true, nil
		}
		var next historyCursor
		if err := json.Unmarshal(env.Cursor, &next); err != nil || next.empty() {
			return all, descs, false, nil
		}
		if seenCursor[next.key()] {
			return all, descs, false, nil
		}
		seenCursor[next.key()] = true
		cursor = &next
	}
	return all, descs, false, nil
}
