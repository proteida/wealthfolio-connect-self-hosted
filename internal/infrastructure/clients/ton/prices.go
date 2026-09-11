package ton

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file values swap legs through USD. Stablecoins convert 1:1; market
// prices come from TonAPI's chart first (wide window, per-token cache) with
// CoinGecko history as fallback. Anything unpriced stays unvalued rather
// than invented.

const (
	coingeckoBaseURL = "https://api.coingecko.com/api/v3"
	coingeckoCoinID  = "the-open-network"
	tonAPIBaseURL    = "https://tonapi.io/v2/rates"
	daySeconds       = 86400
	// tonAPIHistoryDays bounds the TonAPI chart window. Retention reaches
	// ~150 days; older timestamps fall through to CoinGecko (365 days).
	tonAPIHistoryDays = 180
	tonAPIPoints      = 200
)

// coinIDs maps upper-cased tickers to CoinGecko coin IDs for receipt-time
// valuation (deposits) and TON legs. Stablecoins bypass the feed at $1.
var coinIDs = map[string]string{
	"TON":    "the-open-network",
	"USDT":   "tether",
	"USDC":   "usd-coin",
	"TSTON":  "tonstakers",
	"AAPLX":  "apple-xstock",
	"WAAPLX": "wrapped-apple-xstock",
	"SPYX":   "sp500-xstock",
}

// valueUSD resolves USD values for swap legs and cost bases for single legs.
// Production code passes *pricer; tests pass a stub.
type valueUSD interface {
	swapValue(ctx context.Context, inSymbol, inAddr string, inQty float64, outSymbol, outAddr string, outQty float64, at int64) (float64, bool)
	unitPrice(ctx context.Context, symbol, addr string, at int64) (float64, bool)
}

// pricePoint is one hourly CoinGecko quote.
type pricePoint struct {
	time  int64
	price float64
}

// pricer fetches market prices with per-upstream caching and the same
// retry/backoff discipline as the TON Center client (without sending the
// TON Center key to third parties).
//
// Free-tier limits shape the fallback chain: TonAPI covers ~180 days,
// CoinGecko 365 days, bursts throttle — failed lookups are remembered so one
// throttled sync degrades to unvalued legs instead of stalling.
type pricer struct {
	tonAPIBase string
	geckoBase  string
	http       HTTPDoer
	sleep      func(time.Duration)
	retryBase  time.Duration
	maxRetries int

	mu          sync.Mutex
	tonPoints   map[string][]pricePoint
	tonFailed   map[string]error
	geckoCache  map[string][]pricePoint
	geckoFailed map[string]error
}

// newPricer builds a pricer sharing the client's transport tuning.
func newPricer(tonAPIBase, geckoBase string, h HTTPDoer, sleep func(time.Duration), retryBase time.Duration, maxRetries int) *pricer {
	if tonAPIBase == "" {
		tonAPIBase = tonAPIBaseURL
	}
	if geckoBase == "" {
		geckoBase = coingeckoBaseURL
	}
	return &pricer{
		tonAPIBase:  strings.TrimRight(tonAPIBase, "/"),
		geckoBase:   strings.TrimRight(geckoBase, "/"),
		http:        h,
		sleep:       sleep,
		retryBase:   retryBase,
		maxRetries:  maxRetries,
		tonPoints:   make(map[string][]pricePoint),
		tonFailed:   make(map[string]error),
		geckoCache:  make(map[string][]pricePoint),
		geckoFailed: make(map[string]error),
	}
}

// swapValue implements valueUSD: stable side first, else the first side with
// a market price.
func (p *pricer) swapValue(ctx context.Context, inSymbol, inAddr string, inQty float64, outSymbol, outAddr string, outQty float64, at int64) (float64, bool) {
	if isStablecoin(inSymbol) {
		return inQty, true
	}
	if isStablecoin(outSymbol) {
		return outQty, true
	}
	if v, ok := p.tokenValue(ctx, inSymbol, inAddr, inQty, at); ok {
		return v, true
	}
	if v, ok := p.tokenValue(ctx, outSymbol, outAddr, outQty, at); ok {
		return v, true
	}
	return 0, false
}

// unitPrice implements valueUSD for single legs: $1 for stablecoins,
// market price for mapped tokens, unknown otherwise. Airdrops and gifts
// therefore book fair-market-value at receipt, the standard tracker
// convention when no purchase price exists.
func (p *pricer) unitPrice(ctx context.Context, symbol, addr string, at int64) (float64, bool) {
	if isStablecoin(symbol) {
		return 1, true
	}
	return p.tokenValue(ctx, symbol, addr, 1, at)
}

// tokenValue prices qty of a token at a unix time: TonAPI first, CoinGecko
// fallback.
func (p *pricer) tokenValue(ctx context.Context, symbol, addr string, qty float64, at int64) (float64, bool) {
	if price, ok := p.tonAPIPrice(ctx, symbol, addr, at); ok {
		return qty * price, true
	}
	id, ok := coinIDs[strings.ToUpper(symbol)]
	if !ok {
		return 0, false
	}
	price, err := p.coinPrice(ctx, id, at)
	if err != nil {
		return 0, false
	}
	return qty * price, true
}

// tonAPIPrice resolves one unit via TonAPI's chart: TON/GRAM and tickers
// like USDT directly, anything else by Jetton master address.
func (p *pricer) tonAPIPrice(ctx context.Context, symbol, addr string, at int64) (float64, bool) {
	id := addr
	switch upper := strings.ToUpper(symbol); {
	case upper == "TON" || upper == "GRAM":
		id = "TON"
	case addr == "":
		id = upper
	}
	points, err := p.chartPoints(ctx, id)
	if err != nil || len(points) == 0 {
		return 0, false
	}
	// TonAPI serves newest-first: the first point at or before the timestamp
	// wins. Timestamps older than the window stay unvalued here and fall
	// through to the CoinGecko fallback.
	for _, point := range points {
		if point.time <= at {
			return point.price, true
		}
	}
	return 0, false
}

// chartPoints returns cached TonAPI chart points for a token id, fetching
// one wide window per token per sync.
func (p *pricer) chartPoints(ctx context.Context, id string) ([]pricePoint, error) {
	p.mu.Lock()
	points, ok := p.tonPoints[id]
	failure, failed := p.tonFailed[id]
	p.mu.Unlock()
	if failed {
		return nil, failure
	}
	if ok {
		return points, nil
	}
	now := time.Now().Unix()
	points, err := p.fetchChart(ctx, id, now-tonAPIHistoryDays*daySeconds, now)
	p.mu.Lock()
	if err != nil {
		p.tonFailed[id] = err
	} else {
		p.tonPoints[id] = points
	}
	p.mu.Unlock()
	return points, err
}

// fetchChart pulls TonAPI chart points for [from, to] with retry on limits.
func (p *pricer) fetchChart(ctx context.Context, id string, from, to int64) ([]pricePoint, error) {
	q := url.Values{
		"token":        {id},
		"currency":     {"USD"},
		"start_date":   {strconv.FormatInt(from, 10)},
		"end_date":     {strconv.FormatInt(to, 10)},
		"points_count": {"200"},
	}
	var lastErr error
	var after time.Duration
	for attempt := 0; attempt < p.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			p.sleep(computeBackoff(p.retryBase, attempt-1, after))
		}
		var retry bool
		var points []pricePoint
		retry, after, points, lastErr = p.chartOnce(ctx, q)
		if lastErr == nil {
			return points, nil
		}
		if !retry {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// chartOnce performs one chart request. Empty point sets are terminal:
// the token simply has no indexed history.
func (p *pricer) chartOnce(ctx context.Context, q url.Values) (bool, time.Duration, []pricePoint, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.tonAPIBase+"/chart?"+q.Encode(), nil)
	if err != nil {
		return false, 0, nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return true, 0, nil, fmt.Errorf("tonapi chart: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return true, 0, nil, fmt.Errorf("tonapi chart response: %w", err)
	}
	after := retryAfter(resp.Header, time.Now())
	if resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusUnauthorized || resp.StatusCode/100 == 5 {
		return true, after, nil, fmt.Errorf("tonapi chart: http %d, retrying", resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		return false, 0, nil, fmt.Errorf("tonapi chart: http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env struct {
		Error  string      `json:"Error"`
		Points [][]float64 `json:"points"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return true, 0, nil, fmt.Errorf("tonapi chart decode: %w", err)
	}
	if env.Error != "" {
		return false, 0, nil, fmt.Errorf("tonapi chart: %s", env.Error)
	}
	points := make([]pricePoint, 0, len(env.Points))
	for _, row := range env.Points {
		if len(row) != 2 || row[1] <= 0 {
			continue
		}
		points = append(points, pricePoint{time: int64(row[0]), price: row[1]})
	}
	return false, 0, points, nil
}

// tonUSD returns the TON/USD price at a unix time.
func (p *pricer) tonUSD(ctx context.Context, at int64) (float64, error) {
	return p.coinPrice(ctx, coingeckoCoinID, at)
}

// coinPrice returns a coin's price at a unix time: the latest cached hourly
// point at or before it, else the earliest known point.
func (p *pricer) coinPrice(ctx context.Context, id string, at int64) (float64, error) {
	points, err := p.dayPoints(ctx, id, at)
	if err != nil {
		return 0, err
	}
	best := -1
	for i, point := range points {
		if point.time <= at {
			best = i
		} else {
			break
		}
	}
	if best >= 0 {
		return points[best].price, nil
	}
	if len(points) > 0 {
		return points[0].price, nil
	}
	return 0, fmt.Errorf("ton: no TON price near %d", at)
}

// dayPoints returns cached hourly quotes covering the UTC day of at,
// fetching the [day, day+day] range once per day. Failed days are remembered
// so one throttled sync does not hammer the free quota on every swap.
func (p *pricer) dayPoints(ctx context.Context, id string, at int64) ([]pricePoint, error) {
	day := at - (at % daySeconds)
	key := id + ":" + strconv.FormatInt(day, 10)
	p.mu.Lock()
	points, ok := p.geckoCache[key]
	failure, failed := p.geckoFailed[key]
	p.mu.Unlock()
	if failed {
		return nil, failure
	}
	if ok {
		return points, nil
	}
	points, err := p.fetchRange(ctx, id, day, day+daySeconds)
	p.mu.Lock()
	if err != nil {
		p.geckoFailed[key] = err
	} else {
		p.geckoCache[key] = points
	}
	p.mu.Unlock()
	return points, err
}

// fetchRange pulls hourly quotes for [from, to] with retry on rate limits.
func (p *pricer) fetchRange(ctx context.Context, id string, from, to int64) ([]pricePoint, error) {
	q := url.Values{
		"vs_currency": {"usd"},
		"from":        {strconv.FormatInt(from, 10)},
		"to":          {strconv.FormatInt(to, 10)},
		"precision":   {"full"},
	}
	var lastErr error
	var after time.Duration
	for attempt := 0; attempt < p.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			p.sleep(computeBackoff(p.retryBase, attempt-1, after))
		}
		var retry bool
		var points []pricePoint
		retry, after, points, lastErr = p.fetchOnce(ctx, id, q)
		if lastErr == nil {
			return points, nil
		}
		if !retry {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// fetchOnce performs one range request, returning retryability, the
// Retry-After delay and the parsed points.
func (p *pricer) fetchOnce(ctx context.Context, id string, q url.Values) (bool, time.Duration, []pricePoint, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.geckoBase+"/coins/"+id+"/market_chart/range?"+q.Encode(), nil)
	if err != nil {
		return false, 0, nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return true, 0, nil, fmt.Errorf("ton price: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return true, 0, nil, fmt.Errorf("ton price response: %w", err)
	}
	after := retryAfter(resp.Header, time.Now())
	// CoinGecko's free tier throttles burst traffic as 429 and, once the
	// burst budget is spent, as 401 — both are retried, then remembered.
	if resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusUnauthorized || resp.StatusCode/100 == 5 {
		return true, after, nil, fmt.Errorf("ton price: http %d, retrying", resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		return false, 0, nil, fmt.Errorf("ton price: http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env struct {
		Prices [][]float64 `json:"prices"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return true, 0, nil, fmt.Errorf("ton price decode: %w", err)
	}
	points := make([]pricePoint, 0, len(env.Prices))
	for _, row := range env.Prices {
		if len(row) != 2 || row[1] <= 0 {
			continue
		}
		points = append(points, pricePoint{time: int64(row[0] / 1000), price: row[1]})
	}
	if len(points) == 0 {
		return true, 0, nil, fmt.Errorf("ton price: empty range response")
	}
	return false, 0, points, nil
}

// computeBackoff is exponential growth from base, raised to Retry-After.
func computeBackoff(base time.Duration, failedAttempts int, after time.Duration) time.Duration {
	delay := base * time.Duration(1<<min(failedAttempts, 10))
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	return max(delay, after)
}

// formatUSD renders a compact USD figure for descriptions.
func formatUSD(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}
