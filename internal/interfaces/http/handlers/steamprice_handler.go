package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	appprices "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/prices"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/steam"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/config"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/interfaces/http/middleware"
)

// SteamPriceHandler serves CS2 market prices by market_hash_name. Current
// quotes resolve through the shared Redis cache (provider fetch on miss,
// cached 24h) backed by the database store; history resolves from the
// database store populated by sync backfills. Only items the system
// already tracks (a stored price row from sync) are served: unknown names
// are rejected without any Steam call so probing random skins cannot
// burn the Steam rate limit (429).
type SteamPriceHandler struct {
	client  *steam.Client
	history repository.PriceHistoryRepository
}

// priceTTL is how long a fetched current quote is trusted without a fresh
// provider read (Redis TTL and DB-recency threshold). 24h keeps public
// reads off the throttled priceoverview endpoint; the sync loop keeps its
// own shorter TTL for freshness.
const steamPublicPriceTTL = 24 * time.Hour

// NewSteamPriceHandler builds the handler with a public Steam client: the
// priceoverview endpoint needs no credentials, and the configured session
// (if any) extends coverage to authenticated history. Fetched current
// quotes are remembered through the shared history store for 24h.
func NewSteamPriceHandler(cfg *config.Config, prices *appprices.Service, history repository.PriceHistoryRepository) *SteamPriceHandler {
	c := steam.New(steam.ClientConfig{
		SteamID:         cfg.Steam.SteamID,
		Session:         cfg.Steam.Session,
		RefreshToken:    cfg.Steam.RefreshToken,
		PriceTTL:        steamPublicPriceTTL,
		Currency:        cfg.Steam.Currency,
		HistoryBudget:   0,
		MinItemValueUSD: 0,
	}, nil)
	if prices != nil {
		c.SetPriceService(prices)
	}
	c.SetPriceHistoryStore(history)
	return &SteamPriceHandler{client: c, history: history}
}

// RegisterPublicAPIRoutes mounts the steam price paths. Market prices are
// public data with no account scope, so these stay reachable without a
// Bearer token (like subscription plans).
func (h *SteamPriceHandler) RegisterPublicAPIRoutes(r chi.Router) {
	r.Get("/steam/prices/current", h.GetCurrent)
	r.Get("/steam/prices/history", h.GetHistory)
}

type steamCurrentPriceDTO struct {
	MarketHashName string    `json:"market_hash_name"`
	Price          float64   `json:"price"`
	PriceType      string    `json:"price_type"`
	Currency       string    `json:"currency"`
	Timestamp      time.Time `json:"timestamp"`
	TimestampUnix  int64     `json:"timestamp_unix"`
	Date           string    `json:"date"`
}

// GetCurrent returns the current Steam market price for ?name=. Names
// never recorded by the system (no stored price row from sync) are
// rejected without any Steam call to protect the rate limit.
func (h *SteamPriceHandler) GetCurrent(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		middleware.WriteError(w, http.StatusBadRequest, "invalid_request", "MISSING_NAME", "name query parameter is required")
		return
	}
	if h.history != nil {
		asset, curr := steam.PriceAssetKey(name, h.client.Currency())
		if _, err := h.history.Get(r.Context(), asset, curr, time.Now().UTC()); err != nil {
			if isPriceNotFound(err) {
				middleware.WriteError(w, http.StatusNotFound, "not_found", "ITEM_NOT_TRACKED", "item not tracked; sync inventory first")
				return
			}
			middleware.WriteError(w, http.StatusInternalServerError, "internal", "HISTORY_READ_FAILED", "history lookup failed")
			return
		}
	}
	price, priceType, ok := h.client.CurrentPrice(r.Context(), name)
	if !ok {
		middleware.WriteError(w, http.StatusNotFound, "not_found", "PRICE_NOT_FOUND", "no current price for "+name)
		return
	}
	now := time.Now().UTC()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(steamCurrentPriceDTO{
		MarketHashName: name, Price: price, PriceType: priceType, Currency: "USD",
		Timestamp: now, TimestampUnix: now.Unix(), Date: now.Format("2006-01-02"),
	})
}

type steamPricePointDTO struct {
	Timestamp     time.Time `json:"timestamp"`
	TimestampUnix int64     `json:"timestamp_unix"`
	Date          string    `json:"date"`
	Price         float64   `json:"price"`
}

type steamHistoryDTO struct {
	MarketHashName string               `json:"market_hash_name"`
	Currency       string               `json:"currency"`
	Points         []steamPricePointDTO `json:"points"`
}

// GetHistory returns stored historical points for ?name=&from=&to=
// (bounds are optional; defaults to the last 90 days). Each bound
// accepts RFC3339 (2025-05-01T00:00:00Z), unix seconds (1746057600)
// or a calendar date (2025-05-01, midnight UTC).
func (h *SteamPriceHandler) GetHistory(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		middleware.WriteError(w, http.StatusBadRequest, "invalid_request", "MISSING_NAME", "name query parameter is required")
		return
	}
	to := time.Now().UTC()
	from := to.Add(-90 * 24 * time.Hour)
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := parseTimeBound(v)
		if err != nil {
			middleware.WriteError(w, http.StatusBadRequest, "invalid_request", "INVALID_FROM", "from must be RFC3339, unix seconds or YYYY-MM-DD")
			return
		}
		from = t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := parseTimeBound(v)
		if err != nil {
			middleware.WriteError(w, http.StatusBadRequest, "invalid_request", "INVALID_TO", "to must be RFC3339, unix seconds or YYYY-MM-DD")
			return
		}
		to = t
	}
	asset, curr := steam.PriceAssetKey(name, h.client.Currency())
	rows, err := h.history.List(r.Context(), asset, curr, from, to)
	if err != nil {
		middleware.WriteError(w, http.StatusInternalServerError, "internal", "HISTORY_READ_FAILED", "history lookup failed")
		return
	}
	out := steamHistoryDTO{MarketHashName: name, Currency: curr, Points: []steamPricePointDTO{}}
	for _, p := range rows {
		ts := p.Timestamp.UTC()
		out.Points = append(out.Points, steamPricePointDTO{
			Timestamp: ts, TimestampUnix: ts.Unix(), Date: ts.Format("2006-01-02"), Price: p.Price,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// parseTimeBound accepts RFC3339, unix seconds (%s) or YYYY-MM-DD
// (midnight UTC). Anything else is an error.
func parseTimeBound(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, strconv.ErrSyntax
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, strconv.ErrSyntax
}

// isPriceNotFound reports a missing price row (unknown item) as opposed
// to a store failure.
func isPriceNotFound(err error) bool {
	return err != nil && (err == repository.ErrNotFound || strings.Contains(err.Error(), "not found"))
}
