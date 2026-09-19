// Package steamprices owns Steam market-price orchestration: tracking
// gates, TTLs and quote timestamps. HTTP handlers stay thin (request →
// service → response) per AGENTS.md; the Steam provider itself lives in
// infrastructure and is injected behind the Provider interface so this
// package depends only on domain contracts.
package steamprices

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/fx"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
)

// Module wires the Steam price service.
var Module = fx.Module("application.steamprices", fx.Provide(NewService))

// PublicPriceTTL bounds how long a fetched current quote is trusted
// without a fresh provider read. 24h keeps public reads off the throttled
// priceoverview endpoint; the sync loop keeps its own shorter TTL.
const PublicPriceTTL = 24 * time.Hour

// Provider supplies live Steam quotes. Implemented by the infrastructure
// Steam client; tests stub it.
type Provider interface {
	// Currency returns the Steam wallet currency code (1 = USD).
	Currency() int
	// CurrentPrice returns the live median price for a market_hash_name
	// with its observation time. ok=false with err=nil means Steam has
	// no price; err!=nil means a transient upstream failure.
	CurrentPrice(ctx context.Context, marketHashName string) (price float64, priceType string, observed time.Time, ok bool, err error)
}

// Service orchestrates tracked-item gating and quote timestamps over the
// durable price-history store.
type Service struct {
	history  repository.PriceHistoryRepository
	provider Provider
}

// NewService builds the orchestrator. Either dependency may be nil in
// tests; production wires both.
func NewService(history repository.PriceHistoryRepository, provider Provider) *Service {
	return &Service{history: history, provider: provider}
}

// AssetKey namespaces a market quote: appid, name and currency travel in
// the key so nothing collides across games. Mirrors the infrastructure
// mapping without importing it: names are whitespace-collapsed and
// upper-cased so spelling variants share one key.
func AssetKey(marketHashName string, currency int) (asset, curr string) {
	return "steam:730:" + strings.ToUpper(strings.Join(strings.Fields(marketHashName), " ")), fmt.Sprintf("STEAM_%d", currency)
}

// Quote is a current price with its observation timestamp.
type Quote struct {
	MarketHashName string
	Price          float64
	PriceType      string
	Currency       string
	Timestamp      time.Time
}

// Point is one historical price point.
type Point struct {
	Timestamp time.Time
	Price     float64
}

// GetCurrent returns the current quote for a tracked item. Unknown names
// fail with ErrNotTracked without any provider call; known names resolve
// through the provider and report the provider's observation time (never
// serve time), falling back to the stored row and then to now only when
// the provider reports no observation. Transient provider failures fail
// with ErrUpstream (mapped to 502), never with a not-found.
func (s *Service) GetCurrent(ctx context.Context, name string) (Quote, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Quote{}, fmt.Errorf("steamprices: missing name")
	}
	currency := 1
	if s.provider != nil {
		currency = s.provider.Currency()
	}
	asset, curr := AssetKey(name, currency)
	var stored repository.HistoricalPrice
	tracked := false
	if s.history != nil {
		if p, err := s.history.Get(ctx, asset, curr, time.Now().UTC()); err == nil {
			stored, tracked = p, true
		} else if !isNotFound(err) {
			return Quote{}, fmt.Errorf("steamprices: history lookup: %w", err)
		}
	}
	if !tracked {
		return Quote{}, ErrNotTracked
	}
	if s.provider == nil {
		return Quote{}, repository.ErrNotFound
	}
	price, priceType, observed, ok, err := s.provider.CurrentPrice(ctx, name)
	if err != nil {
		return Quote{}, fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	if !ok {
		return Quote{}, repository.ErrNotFound
	}
	ts := observed
	if ts.IsZero() {
		ts = stored.Timestamp
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return Quote{
		MarketHashName: name, Price: price, PriceType: priceType,
		Currency: "USD", Timestamp: ts.UTC(),
	}, nil
}

// displayCurrency is the public currency code. Storage stays namespaced
// by Steam wallet currency (STEAM_1) so nothing collides across games;
// the API always exposes USD until FX conversion exists.
const displayCurrency = "USD"

// GetHistory returns stored points in [from, to].
func (s *Service) GetHistory(ctx context.Context, name string, from, to time.Time) ([]Point, string, error) {
	currency := 1
	if s.provider != nil {
		currency = s.provider.Currency()
	}
	asset, curr := AssetKey(name, currency)
	if s.history == nil {
		return nil, displayCurrency, nil
	}
	rows, err := s.history.List(ctx, asset, curr, from, to)
	if err != nil {
		return nil, displayCurrency, fmt.Errorf("steamprices: history lookup: %w", err)
	}
	out := make([]Point, 0, len(rows))
	for _, p := range rows {
		out = append(out, Point{Timestamp: p.Timestamp.UTC(), Price: p.Price})
	}
	return out, displayCurrency, nil
}

func isNotFound(err error) bool {
	return err != nil && (err == repository.ErrNotFound || strings.Contains(err.Error(), "not found"))
}
