// Package prices is the generic caching/storage layer every market-data
// provider uses: historical quotes live in the main database without
// expiration (checked before any external call, UPSERTed after a fetch),
// while latest prices and today's incomplete daily candles live in Redis
// with short TTLs. All external calls stay in provider-supplied fetch
// functions, so this package never talks to a vendor API directly.
package prices

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/fx"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
)

// Module wires the price caching service.
var Module = fx.Module("application.prices",
	fx.Provide(NewService),
)

const (
	// CurrentPriceTTL bounds how long a latest price is trusted without a
	// fresh provider read.
	CurrentPriceTTL = time.Hour
	// maxCandleAge caps the Redis lifetime of today's incomplete candle:
	// one full day plus a buffer so late finalization still finds it.
	maxCandleAge = 25 * time.Hour
)

// Candle is one daily OHLC record. While its day is active it lives in
// Redis (mutable); FinalizeCandle persists its close as the day's
// historical price and drops the cached copy.
type Candle struct {
	Asset    string
	Currency string
	Day      time.Time // any instant within the day (UTC)
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Source   string
}

// Service orchestrates database-first historical reads and Redis-first
// current reads over provider fetch functions. A nil history store skips
// persistence (provider-only); a nil current cache skips short-lived
// caching. Both nil reduce every call to a direct fetch, preserving the
// pre-caching behavior of existing providers.
type Service struct {
	history repository.PriceHistoryRepository
	current repository.CurrentPriceCache
	now     func() time.Time
}

// NewService builds the orchestrator. Either store may be nil.
func NewService(history repository.PriceHistoryRepository, current repository.CurrentPriceCache) *Service {
	return NewServiceWithClock(history, current, time.Now)
}

// NewServiceWithClock is NewService with an explicit clock (tests).
func NewServiceWithClock(history repository.PriceHistoryRepository, current repository.CurrentPriceCache, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{history: history, current: current, now: now}
}

// CurrentKey derives the Redis key for a latest price.
func CurrentKey(asset, currency string) string {
	return "px:current:" + strings.ToUpper(strings.TrimSpace(asset)) + ":" + strings.ToUpper(strings.TrimSpace(currency))
}

// candleKey derives the Redis key for one asset/currency/day.
func candleKey(asset, currency string, day time.Time) string {
	return "px:candle:" + strings.ToUpper(strings.TrimSpace(asset)) + ":" +
		strings.ToUpper(strings.TrimSpace(currency)) + ":" + day.UTC().Format("2006-01-02")
}

// startOfDay truncates to the UTC calendar day.
func startOfDay(t time.Time) time.Time {
	return time.Date(t.UTC().Year(), t.UTC().Month(), t.UTC().Day(), 0, 0, 0, 0, time.UTC)
}

// GetHistorical returns the latest stored quote at or before at. On a miss
// it calls fetch, UPSERTs every returned point, and selects from the fresh
// rows with the same rule. Fetch errors propagate and store nothing, so a
// failed provider never poisons the database.
func (s *Service) GetHistorical(ctx context.Context, asset, currency string, at time.Time, fetch func(ctx context.Context) ([]repository.HistoricalPrice, error)) (repository.HistoricalPrice, error) {
	asset = strings.ToUpper(strings.TrimSpace(asset))
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if s.history != nil {
		if p, err := s.history.Get(ctx, asset, currency, at); err == nil {
			return p, nil
		} else if !isNotFound(err) {
			return repository.HistoricalPrice{}, fmt.Errorf("prices: history lookup: %w", err)
		}
	}
	if fetch == nil {
		return repository.HistoricalPrice{}, repository.ErrNotFound
	}
	points, err := fetch(ctx)
	if err != nil {
		return repository.HistoricalPrice{}, err
	}
	if s.history != nil && len(points) > 0 {
		if err := s.history.Upsert(ctx, points); err != nil {
			return repository.HistoricalPrice{}, fmt.Errorf("prices: history store: %w", err)
		}
	}
	return selectAtOrBefore(points, at)
}

// RefreshHistorical fetches unconditionally and UPSERTs, letting providers
// correct previously stored data. It returns the same selection as
// GetHistorical over the fresh rows.
func (s *Service) RefreshHistorical(ctx context.Context, asset, currency string, at time.Time, fetch func(ctx context.Context) ([]repository.HistoricalPrice, error)) (repository.HistoricalPrice, error) {
	if fetch == nil {
		return repository.HistoricalPrice{}, repository.ErrNotFound
	}
	points, err := fetch(ctx)
	if err != nil {
		return repository.HistoricalPrice{}, err
	}
	if s.history != nil && len(points) > 0 {
		if err := s.history.Upsert(ctx, points); err != nil {
			return repository.HistoricalPrice{}, fmt.Errorf("prices: history store: %w", err)
		}
	}
	return selectAtOrBefore(points, at)
}

// selectAtOrBefore picks the latest point at or before at, else the
// earliest known point, else ErrNotFound. It mirrors the selection rules
// providers applied to in-memory windows before persistence existed.
func selectAtOrBefore(points []repository.HistoricalPrice, at time.Time) (repository.HistoricalPrice, error) {
	var best *repository.HistoricalPrice
	var earliest *repository.HistoricalPrice
	for i := range points {
		p := &points[i]
		if earliest == nil || p.Timestamp.Before(earliest.Timestamp) {
			earliest = p
		}
		if !p.Timestamp.After(at) && (best == nil || p.Timestamp.After(best.Timestamp)) {
			best = p
		}
	}
	if best != nil {
		return *best, nil
	}
	if earliest != nil {
		return *earliest, nil
	}
	return repository.HistoricalPrice{}, repository.ErrNotFound
}

// GetCurrent returns the cached latest price, falling through to fetch on a
// miss and persisting the fresh value for CurrentPriceTTL. Cache errors fail
// open to the provider; a fetch error propagates.
func (s *Service) GetCurrent(ctx context.Context, asset, currency string, fetch func(ctx context.Context) (float64, error)) (float64, error) {
	return s.GetCurrentWithTTL(ctx, asset, currency, CurrentPriceTTL, fetch)
}

// GetCurrentWithTTL is GetCurrent with an explicit TTL (tests and candles).
func (s *Service) GetCurrentWithTTL(ctx context.Context, asset, currency string, ttl time.Duration, fetch func(ctx context.Context) (float64, error)) (float64, error) {
	key := CurrentKey(asset, currency)
	if s.current != nil {
		if price, found, err := s.current.Get(ctx, key); err == nil && found {
			return price, nil
		}
	}
	if fetch == nil {
		return 0, repository.ErrNotFound
	}
	price, err := fetch(ctx)
	if err != nil {
		return 0, err
	}
	if s.current != nil {
		_ = s.current.Set(ctx, key, price, ttl)
	}
	return price, nil
}

// GetBlob is the raw-bytes analogue of GetCurrent for providers whose
// cacheable unit is a document rather than a number (e.g. Hyperliquid's
// whole mark map). Hit returns the stored bytes; on a miss fetch runs and
// its output is stored for ttl. Cache errors fail open to fetch.
func (s *Service) GetBlob(ctx context.Context, key string, ttl time.Duration, fetch func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if s.current != nil {
		if raw, found, err := s.current.GetRaw(ctx, key); err == nil && found {
			return raw, nil
		}
	}
	if fetch == nil {
		return nil, repository.ErrNotFound
	}
	raw, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	if s.current != nil {
		_ = s.current.SetRaw(ctx, key, raw, ttl)
	}
	return raw, nil
}

// PutCandle stores today's incomplete candle in Redis, overwriting the
// previous snapshot of the same day. The entry expires on its own; callers
// must FinalizeCandle once the day closes.
func (s *Service) PutCandle(ctx context.Context, c Candle) error {
	if s.current == nil {
		return nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("prices: candle encode: %w", err)
	}
	ttl := startOfDay(c.Day).Add(maxCandleAge).Sub(s.now())
	if ttl <= 0 {
		return fmt.Errorf("prices: candle day already closed")
	}
	return s.current.SetRaw(ctx, candleKey(c.Asset, c.Currency, c.Day), raw, ttl)
}

// GetCandle returns today's cached candle, or found=false.
func (s *Service) GetCandle(ctx context.Context, asset, currency string, day time.Time) (Candle, bool, error) {
	if s.current == nil {
		return Candle{}, false, nil
	}
	raw, found, err := s.current.GetRaw(ctx, candleKey(asset, currency, day))
	if err != nil || !found {
		return Candle{}, false, err
	}
	var c Candle
	if err := json.Unmarshal(raw, &c); err != nil {
		return Candle{}, false, fmt.Errorf("prices: candle decode: %w", err)
	}
	return c, true, nil
}

// FinalizeCandle persists a day's close as its historical price (UPSERT, so
// re-finalization after a provider correction is safe) and drops the Redis
// copy. Without a history store it only drops the cached copy.
func (s *Service) FinalizeCandle(ctx context.Context, asset, currency string, day time.Time) error {
	asset = strings.ToUpper(strings.TrimSpace(asset))
	currency = strings.ToUpper(strings.TrimSpace(currency))
	key := candleKey(asset, currency, day)
	var close float64
	var source string
	haveCandle := false
	if s.current != nil {
		if c, found, err := s.GetCandle(ctx, asset, currency, day); err != nil {
			return fmt.Errorf("prices: candle read: %w", err)
		} else if found {
			close, source, haveCandle = c.Close, c.Source, true
		}
		if err := s.current.Delete(ctx, key); err != nil {
			return fmt.Errorf("prices: candle drop: %w", err)
		}
	}
	if !haveCandle || s.history == nil {
		return nil
	}
	return s.history.Upsert(ctx, []repository.HistoricalPrice{{
		Asset: asset, Timestamp: startOfDay(day), Currency: currency,
		Price: close, Source: source, UpdatedAt: s.now().UTC(),
	}})
}

func isNotFound(err error) bool {
	return err != nil && (err == repository.ErrNotFound || strings.Contains(err.Error(), "not found"))
}
