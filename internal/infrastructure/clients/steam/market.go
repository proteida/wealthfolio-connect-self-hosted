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

// Market history parsing prefers the structured norender=1 format
// ({events, purchases, listings, assets}) over rendered HTML: names,
// amounts and linkage arrive as data, not markup. The HTML parser below
// stays as a fallback for responses that still render rows. Either way,
// rows that yield no timestamp or identity are skipped per-row, never
// fatally, and anything matched is normalized without invention (net
// amounts especially are only set when explicitly present).

var (
	// rowSplit cuts rendered HTML into per-listing blocks. Both selectors
	// are observational; if neither matches, the whole payload is treated
	// as one block rather than zero rows.
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

// marketPage mirrors the myhistory render envelope, structured or rendered.
type marketPage struct {
	Success     bool                       `json:"success"`
	TotalCount  int                        `json:"total_count"`
	Start       int                        `json:"start"`
	PageSize    int                        `json:"pagesize"`
	ResultsHTML json.RawMessage            `json:"results_html"`
	Events      []json.RawMessage          `json:"events"`
	Purchases   map[string]json.RawMessage `json:"purchases"`
	Listings    map[string]json.RawMessage `json:"listings"`
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

// marketAsset mirrors purchase/listing asset linkage: full identity plus
// the post-trade identity when Steam exposes it.
type marketAsset struct {
	ClassID      any `json:"classid"`
	InstanceID   any `json:"instanceid"`
	Amount       any `json:"amount"`
	NewID        any `json:"new_id"`
	NewContextID any `json:"new_contextid"`
}

type marketPurchase struct {
	ListingID      string      `json:"listingid"`
	PurchaseID     string      `json:"purchaseid"`
	TimeSold       int64       `json:"time_sold"`
	SteamIDBuyer   string      `json:"steamid_purchaser"`
	Failed         int         `json:"failed"`
	NeedsRollback  int         `json:"needs_rollback"`
	Asset          marketAsset `json:"asset"`
	PaidAmount     int64       `json:"paid_amount"`
	CurrencyID     any         `json:"currencyid"`
	ReceivedAmount int64       `json:"received_amount"`
}

type marketListing struct {
	ListingID  string      `json:"listingid"`
	Price      int64       `json:"price"`
	CurrencyID any         `json:"currencyid"`
	Asset      marketAsset `json:"asset"`
}

// steamCurrency maps Steam currencyid to ISO code and minor-unit decimals.
// Observed ids are 2000 + wallet currency (2001 = USD, 2003 = EUR, ...);
// unknown ids keep a C<code> label with 2 decimals rather than failing.
func steamCurrency(id any) (code string, decimals int) {
	n, err := strconv.Atoi(strVal(id))
	if err != nil {
		return "C" + strVal(id), 2
	}
	wallet := n
	if n >= 2000 {
		wallet = n - 2000
	}
	codes := map[int]string{
		1: "USD", 2: "GBP", 3: "EUR", 4: "CHF", 5: "RUB", 6: "PLN",
		7: "BRL", 8: "JPY", 9: "NOK", 10: "IDR", 11: "MYR", 12: "PHP",
		13: "SGD", 14: "THB", 15: "VND", 16: "KRW", 17: "TRY", 18: "UAH",
		19: "MXN", 20: "CAD", 21: "AUD", 22: "NZD", 23: "CNY", 24: "INR",
		25: "CLP", 26: "PEN", 27: "COP", 28: "ZAR", 29: "HKD", 30: "TWD",
		31: "SAR", 32: "AED", 33: "SEK", 34: "ARS", 35: "ILS", 36: "BYN",
		37: "KZT", 38: "KWD", 39: "QAR", 40: "HRK", 41: "CZK", 42: "BGN",
		43: "BDT", 44: "MNT",
	}
	code, ok := codes[wallet]
	if !ok {
		return "C" + strconv.Itoa(n), 2
	}
	decimals = 2
	switch code {
	case "JPY", "KRW", "VND", "CLP", "IDR", "MNT":
		decimals = 0
	}
	return code, decimals
}

func minorToMajor(amount int64, decimals int) float64 {
	div := 1.0
	for i := 0; i < decimals; i++ {
		div *= 10
	}
	return float64(amount) / div
}

// purchaseToTx normalizes one purchase record. Direction comes from the
// buyer identity (never from event_type integers, whose meanings are
// undocumented): buyer == me is a buy with paid cost; otherwise a sell
// with received proceeds (paid when received is absent).
func purchaseToTx(p marketPurchase, mySteamID string, names map[string]string) (domainsteam.MarketTransaction, bool) {
	var tx domainsteam.MarketTransaction
	classID, instanceID := strVal(p.Asset.ClassID), strVal(p.Asset.InstanceID)
	name := names[classID+"_"+instanceID]
	qty := intVal(p.Asset.Amount)
	if qty <= 0 {
		qty = 1
	}
	code, decimals := steamCurrency(p.CurrencyID)
	tx.MarketHashName = name
	tx.ClassID = classID
	tx.InstanceID = instanceID
	tx.Quantity = qty
	tx.Currency = code
	tx.Timestamp = time.Unix(p.TimeSold, 0).UTC()
	if p.SteamIDBuyer != "" && p.SteamIDBuyer == mySteamID {
		tx.Type = "buy"
		tx.ExternalID = "market:buy:" + p.PurchaseID
		tx.Gross = minorToMajor(p.PaidAmount, decimals)
	} else {
		tx.Type = "sell"
		tx.ExternalID = "market:sell:" + p.ListingID
		tx.Gross = minorToMajor(p.ReceivedAmount, decimals)
		if p.ReceivedAmount <= 0 {
			tx.Gross = minorToMajor(p.PaidAmount, decimals)
		}
	}
	if p.Failed != 0 || p.NeedsRollback != 0 {
		// Failed/rolled-back purchases are evidence, not acquisitions.
		tx.Type = "other"
	}
	if tx.ExternalID == "market:buy:" || tx.ExternalID == "market:sell:" {
		return tx, false
	}
	return tx, true
}

// listingToTx normalizes one sell-listing row. Listings carry price but no
// class identity, so names arrive through the linked purchase when one
// exists; otherwise the row persists as evidence for later joins.
func listingToTx(l marketListing, purchase *marketPurchase, names map[string]string) (domainsteam.MarketTransaction, bool) {
	var tx domainsteam.MarketTransaction
	classID, instanceID := "", ""
	if purchase != nil {
		classID, instanceID = strVal(purchase.Asset.ClassID), strVal(purchase.Asset.InstanceID)
	}
	tx.Type = "listing"
	tx.ExternalID = "market:listing:" + l.ListingID
	tx.MarketHashName = names[classID+"_"+instanceID]
	tx.ClassID = classID
	tx.InstanceID = instanceID
	tx.Quantity = 1
	code, decimals := steamCurrency(l.CurrencyID)
	tx.Currency = code
	tx.Gross = minorToMajor(l.Price, decimals)
	if l.ListingID == "" {
		return tx, false
	}
	return tx, true
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

// parseMarketStructured normalizes the norender=1 format: purchases (with
// buyer-side direction), then listings (inheriting identity from a linked
// purchase when one exists). names resolves classid_instanceid from
// inventory-history descriptions; mySteamID decides buy vs sell.
func parseMarketStructured(env marketPage, mySteamID string, names map[string]string) []domainsteam.MarketTransaction {
	var out []domainsteam.MarketTransaction
	seen := map[string]bool{}
	byListing := map[string]marketPurchase{}
	for _, raw := range env.Purchases {
		var p marketPurchase
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if p.ListingID != "" {
			byListing[p.ListingID] = p
		}
		tx, ok := purchaseToTx(p, mySteamID, names)
		if !ok || seen[tx.ExternalID] {
			continue
		}
		seen[tx.ExternalID] = true
		out = append(out, tx)
	}
	for _, raw := range env.Listings {
		var l marketListing
		if err := json.Unmarshal(raw, &l); err != nil {
			continue
		}
		var linked *marketPurchase
		if p, ok := byListing[l.ListingID]; ok {
			linked = &p
		}
		tx, ok := listingToTx(l, linked, names)
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
// names resolves classid_instanceid (from inventory-history descriptions)
// because purchase rows carry identity but no market names.
func (c *Client) fetchMarketHistory(ctx context.Context, mySteamID string, names map[string]string) ([]domainsteam.MarketTransaction, bool, error) {
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
		rows := parseMarketStructured(env, mySteamID, names)
		if len(rows) == 0 {
			var html string
			if len(env.ResultsHTML) > 0 {
				if err := json.Unmarshal(env.ResultsHTML, &html); err != nil {
					html = string(env.ResultsHTML)
				}
			}
			if htmlRows := parseMarketHTML(html); len(htmlRows) > 0 {
				rows = htmlRows
			}
		}
		if len(rows) == 0 {
			c.log.Info().Strs("shape", env.Keys).Msg("steam market history empty page")
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
