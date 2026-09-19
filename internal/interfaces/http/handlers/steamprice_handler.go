package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/steamprices"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/interfaces/http/middleware"
)

// SteamPriceHandler serves CS2 market prices by market_hash_name. All
// orchestration (tracking gate, TTL, timestamps) lives in the application
// service; this handler only translates request → service → response.
type SteamPriceHandler struct {
	prices *steamprices.Service
}

// NewSteamPriceHandler builds the handler around the application service.
func NewSteamPriceHandler(prices *steamprices.Service) *SteamPriceHandler {
	return &SteamPriceHandler{prices: prices}
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

// RegisterPublicAPIRoutes mounts the steam price paths. Market prices are
// public data with no account scope, so these stay reachable without a
// Bearer token (like subscription plans).
func (h *SteamPriceHandler) RegisterPublicAPIRoutes(r chi.Router) {
	r.Get("/steam/prices/current", h.GetCurrent)
	r.Get("/steam/prices/history", h.GetHistory)
}

// GetCurrent returns the current Steam market price for ?name=. Names
// never recorded by the system are rejected without any Steam call.
func (h *SteamPriceHandler) GetCurrent(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if strings.TrimSpace(name) == "" {
		middleware.WriteError(w, http.StatusBadRequest, "invalid_request", "MISSING_NAME", "name query parameter is required")
		return
	}
	q, err := h.prices.GetCurrent(r.Context(), name)
	if err != nil {
		if errors.Is(err, steamprices.ErrNotTracked) {
			middleware.WriteError(w, http.StatusNotFound, "not_found", "ITEM_NOT_TRACKED", "item not tracked; sync inventory first")
			return
		}
		if errors.Is(err, steamprices.ErrUpstream) {
			middleware.WriteError(w, http.StatusBadGateway, "upstream", "PRICE_UPSTREAM_FAILED", "steam market temporarily unavailable")
			return
		}
		if errors.Is(err, repository.ErrNotFound) || strings.Contains(err.Error(), "not found") {
			middleware.WriteError(w, http.StatusNotFound, "not_found", "PRICE_NOT_FOUND", "no current price for "+name)
			return
		}
		if strings.Contains(err.Error(), "history lookup") {
			middleware.WriteError(w, http.StatusInternalServerError, "internal", "HISTORY_READ_FAILED", "history lookup failed")
			return
		}
		middleware.WriteError(w, http.StatusNotFound, "not_found", "PRICE_NOT_FOUND", "no current price for "+name)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(steamCurrentPriceDTO{ //nolint:errcheck // headers already sent; a client disconnect mid-body has no recovery path.
		MarketHashName: q.MarketHashName, Price: q.Price, PriceType: q.PriceType, Currency: q.Currency,
		Timestamp: q.Timestamp, TimestampUnix: q.Timestamp.Unix(), Date: q.Timestamp.Format("2006-01-02"),
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
	if strings.TrimSpace(name) == "" {
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
	points, curr, err := h.prices.GetHistory(r.Context(), name, from, to)
	if err != nil {
		middleware.WriteError(w, http.StatusInternalServerError, "internal", "HISTORY_READ_FAILED", "history lookup failed")
		return
	}
	out := steamHistoryDTO{MarketHashName: name, Currency: curr, Points: []steamPricePointDTO{}}
	for _, p := range points {
		ts := p.Timestamp.UTC()
		out.Points = append(out.Points, steamPricePointDTO{
			Timestamp: ts, TimestampUnix: ts.Unix(), Date: ts.Format("2006-01-02"), Price: p.Price,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out) //nolint:errcheck // headers already sent; a client disconnect mid-body has no recovery path.
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
