package binance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var errRequestDeferred = errors.New("binance: request deferred")

// weightTransport accounts for every Binance request, including balances and
// prices, before sending it. Requests are deferred (not queued with signatures
// that would expire) when the local or observed shared-IP budget is exhausted.
type weightTransport struct {
	base         http.RoundTripper
	mu           sync.Mutex
	now          func() time.Time
	events       []weightEvent
	blockedUntil time.Time
}

type weightEvent struct {
	at     time.Time
	weight int
}

func newWeightTransport(base http.RoundTripper) *weightTransport {
	return &weightTransport{base: base, now: time.Now}
}

// RoundTrip enforces the local weight budget and upstream cooldown before sending.
func (t *weightTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Serialize response headers too, so callers cannot race a cooldown update.
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	now := t.now()
	if now.Before(t.blockedUntil) {
		return nil, fmt.Errorf("%w until %s", errRequestDeferred, t.blockedUntil.UTC().Format(time.RFC3339))
	}
	weight := 20 // account, exchangeInfo, myTrades
	if req.URL.Path == "/api/v3/ticker/price" {
		weight = 4
	}
	active := t.events[:0]
	used := 0
	for _, event := range t.events {
		if now.Sub(event.at) < time.Minute {
			active = append(active, event)
			used += event.weight
		}
	}
	t.events = active
	// 10% of the advertised 6000/minute limit, across all this client's calls.
	// Other programs sharing the IP are observed through response headers.
	if used+weight > 600 {
		return nil, fmt.Errorf("%w: local 600 request-weight/minute budget exhausted", errRequestDeferred)
	}
	t.events = append(t.events, weightEvent{at: now, weight: weight})
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	now = t.now()
	if observed, parseErr := strconv.Atoi(resp.Header.Get("X-MBX-USED-WEIGHT-1M")); parseErr == nil && observed >= 5000 {
		t.blockedUntil = now.Add(time.Minute)
	}
	limited := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusTeapot
	// Some Binance -1003 errors use other HTTP error statuses. Preserve the
	// error body for the SDK while applying the same cooldown.
	if resp.StatusCode >= 400 {
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("binance rate-limit response: %w", readErr)
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		var apiError struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(body, &apiError) == nil && apiError.Code == -1003 {
			limited = true
		}
	}
	if limited {
		delay := time.Minute
		if resp.StatusCode == http.StatusTeapot {
			delay = 5 * time.Minute
		}
		if seconds, parseErr := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 32); parseErr == nil && seconds > 0 {
			delay = max(delay, time.Duration(seconds)*time.Second)
		} else if retryAt, dateErr := http.ParseTime(resp.Header.Get("Retry-After")); dateErr == nil {
			delay = max(delay, retryAt.Sub(now))
		}
		t.blockedUntil = maxTime(t.blockedUntil, now.Add(delay))
	}
	return resp, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
