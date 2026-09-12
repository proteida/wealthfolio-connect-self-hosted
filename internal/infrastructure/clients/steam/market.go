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

	domainsteam "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/steam"
)

// Market history parsing is intentionally structural, not visual: Steam
// renders transaction rows as HTML and the markup changes. The parser below
// keys on data attributes and stable row anchors, extracts what each row
// provably contains, and degrades per-row (a row that yields no timestamp
// or name is skipped) instead of failing the import. Anything matched is
// normalized; nothing is invented (net amounts especially are only set
// when explicitly present, never derived by fee math).

var (
	// rowSplit cuts results_html into per-listing blocks. Both selectors
	// below are observational; if neither matches, the whole payload is
	// treated as one block rather than zero rows.
	rowSplitRe = regexp.MustCompile(`(?i)(?:market_listing_row|history_row)`)
	// data attributes observed in market rows. Names normalize hyphens
	// to underscores ("market-hash-name" reads as market_hash_name).
	attrRe = regexp.MustCompile(`data-([a-zA-Z0-9_-]+)="([^"]*)"`)
	// money finds currency amounts in either order ("$31.28", "31,28€").
	moneyRe = regexp.MustCompile(`([$\x{20AC}\x{00A3}\x{20BD}\x{0E3F}\x{00A5}\x{20B4}])\s*([\d,]+\.?\d*)|([\d,]+\.?\d*)\s*([$\x{20AC}\x{00A3}\x{20BD}\x{0E3F}\x{00A5}\x{20B4}])`)
	// dateText finds "8 May, 2025" / "May 8, 2025" style dates.
	dateTextRe = regexp.MustCompile(`(?i)(?:\d{1,2}\s+[A-Za-z]{3,9},?\s+\d{4}|[A-Za-z]{3,9}\s+\d{1,2},?\s+\d{4})`)
	// nameAnchor finds the item name element.
	nameAnchorRe = regexp.MustCompile(`(?is)market_listing_item_name[^>]*>(.*?)<`)
	tagRe        = regexp.MustCompile(`(?s)<[^>]*>`)
)

// marketPage mirrors the myhistory render envelope.
type marketPage struct {
	Success     bool            `json:"success"`
	TotalCount  int             `json:"total_count"`
	Start       int             `json:"start"`
	ResultsHTML json.RawMessage `json:"results_html"`
	// Keys records the envelope's top-level keys for drift visibility.
	Keys []string `json:"-"`
}

// UnmarshalJSON decodes known fields and records the envelope shape.
func (p *marketPage) UnmarshalJSON(raw []byte) error {
	type plain marketPage
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

func stripTags(s string) string {
	return strings.Join(strings.Fields(tagRe.ReplaceAllString(s, " ")), " ")
}

func parseMoney(s string) (symbol string, amount float64, ok bool) {
	m := moneyRe.FindStringSubmatch(s)
	if m == nil {
		return "", 0, false
	}
	// m[1],m[2] are the leading-symbol shape; m[3],m[4] the trailing one.
	num, sym := m[2], m[1]
	if num == "" {
		num, sym = m[3], m[4]
	}
	v, err := strconv.ParseFloat(normalizeDecimal(num), 64)
	if err != nil {
		return "", 0, false
	}
	return sym, v, true
}

// normalizeDecimal handles both "1,234.56" (thousands) and "31,28"
// (European decimal comma): a lone comma followed by exactly two digits
// is a decimal separator, otherwise commas are noise.
func normalizeDecimal(num string) string {
	if !strings.Contains(num, ",") {
		return num
	}
	if !strings.Contains(num, ".") {
		if parts := strings.Split(num, ","); len(parts) == 2 && len(parts[1]) == 2 {
			return parts[0] + "." + parts[1]
		}
	}
	return strings.ReplaceAll(num, ",", "")
}

func currencyFor(symbol string) string {
	switch symbol {
	case "$":
		return "USD"
	case "€":
		return "EUR"
	case "£":
		return "GBP"
	case "₽":
		return "RUB"
	case "฿":
		return "THB"
	case "¥":
		return "CNY"
	case "₴":
		return "UAH"
	default:
		return symbol
	}
}

// classifyMarketRow maps row text markers to a normalized type. Signs win
// over words: a leading "-" is a buy/cost, "+" a sale/proceed.
func classifyMarketRow(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "cancel"):
		return "cancel"
	case strings.Contains(lower, "list"):
		return "listing"
	case strings.Contains(lower, "purchas") || strings.Contains(lower, "bought") || strings.HasPrefix(strings.TrimSpace(text), "-"):
		return "buy"
	case strings.Contains(lower, "sold") || strings.HasPrefix(strings.TrimSpace(text), "+"):
		return "sell"
	default:
		return "other"
	}
}

// parseMarketBlock normalizes one row block. ok=false means the block held
// nothing usable (not an error).
func parseMarketBlock(block string) (domainsteam.MarketTransaction, bool) {
	var tx domainsteam.MarketTransaction
	attrs := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(block, -1) {
		attrs[strings.ToLower(strings.ReplaceAll(m[1], "-", "_"))] = m[2]
	}
	// Splitting on the row marker leaves the opening tag's attribute tail
	// (e.g. `" data-listingid="7" ...>`) ahead of the content. That debris
	// contains "listing" and would misclassify every row, so cut a leading
	// fragment without "<" up to its closing ">". Attributes were already
	// extracted above and survive the cut.
	if i := strings.Index(block, ">"); i >= 0 && !strings.Contains(block[:i], "<") {
		block = block[i+1:]
	}
	text := stripTags(block)
	if strings.TrimSpace(text) == "" && len(attrs) == 0 {
		return tx, false
	}
	name := ""
	if m := nameAnchorRe.FindStringSubmatch(block); m != nil {
		name = stripTags(m[1])
	}
	if name == "" {
		name = attrs["market_hash_name"]
	}
	if name == "" {
		name = attrs["market_name"]
	}
	var at time.Time
	if ts := attrs["timestamp"]; ts != "" {
		at = parseSteamTime(ts)
	}
	if at.IsZero() {
		if d := dateTextRe.FindString(text); d != "" {
			at = parseSteamTime(d)
		}
	}
	if name == "" || at.IsZero() {
		return tx, false
	}
	tx.Type = classifyMarketRow(text)
	tx.Timestamp = at
	tx.MarketHashName = name
	tx.Quantity = 1
	// Listings and cancellations carry no amount; buys and sells without
	// one are useless and skipped.
	sym, amount, ok := parseMoney(text)
	if !ok {
		if tx.Type == "buy" || tx.Type == "sell" {
			return tx, false
		}
	} else {
		tx.Gross = amount
		tx.Currency = currencyFor(sym)
	}
	if id := attrs["listingid"]; id != "" {
		tx.ExternalID = "market:" + id
	} else if id := attrs["transaction_id"]; id != "" {
		tx.ExternalID = "market:" + id
	} else {
		tx.ExternalID = fmt.Sprintf("market:%d:%s:%d:%.2f", at.Unix(), name, tx.Quantity, amount)
	}
	return tx, true
}

// parseMarketHTML splits the rendered payload into normalized rows.
func parseMarketHTML(html string) []domainsteam.MarketTransaction {
	parts := rowSplitRe.Split(html, -1)
	if len(parts) <= 1 {
		parts = []string{html}
	}
	var out []domainsteam.MarketTransaction
	seen := map[string]bool{}
	for _, part := range parts {
		tx, ok := parseMarketBlock(part)
		if !ok || seen[tx.ExternalID] {
			continue
		}
		seen[tx.ExternalID] = true
		out = append(out, tx)
	}
	return out
}

// fetchMarketHistory pages myhistory until total_count is covered, the
// pages stop advancing, or the page cap hits. Partial pages are returned
// with complete=false so the importer never mistakes them for exhaustive.
func (c *Client) fetchMarketHistory(ctx context.Context) ([]domainsteam.MarketTransaction, bool, error) {
	const pageSize = 500
	var all []domainsteam.MarketTransaction
	seen := map[string]bool{}
	total := -1
	for page := 0; page < c.cfg.MaxPages; page++ {
		start := page * pageSize
		if total >= 0 && start >= total {
			return all, true, nil
		}
		q := url.Values{
			"query":    {""},
			"start":    {strconv.Itoa(start)},
			"count":    {strconv.Itoa(pageSize)},
			"norender": {"1"},
		}
		var env marketPage
		if err := c.getCommunity(ctx, "/market/myhistory/render/", q, &env); err != nil {
			return all, false, err
		}
		if total < 0 {
			total = env.TotalCount
		}
		var html string
		if len(env.ResultsHTML) > 0 {
			if err := json.Unmarshal(env.ResultsHTML, &html); err != nil {
				html = string(env.ResultsHTML)
			}
		}
		rows := parseMarketHTML(html)
		if len(rows) == 0 {
			c.log.Info().Strs("shape", env.Keys).Int("html_bytes", len(html)).Msg("steam market history empty page")
			// Empty page before total_count: Steam truncated the range.
			// Keep what we have; do not claim completeness.
			return all, start >= total, nil
		}
		for _, tx := range rows {
			if seen[tx.ExternalID] {
				continue
			}
			seen[tx.ExternalID] = true
			all = append(all, tx)
		}
		if len(rows) < pageSize {
			return all, true, nil
		}
	}
	return all, false, nil
}
