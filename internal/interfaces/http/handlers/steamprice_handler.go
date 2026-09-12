package handlers

import (
	"encoding/json"
	"net/http"
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
// bounded by STEAM_PRICE_TTL_MINUTES); history resolves from the database
// store populated by sync backfills.
type SteamPriceHandler struct {
	client  *steam.Client
	history repository.PriceHistoryRepository
}

// NewSteamPriceHandler builds the handler with a public Steam client: the
// priceoverview endpoint needs no credentials, and the configured session
// (if any) extends coverage to authenticated history.
func NewSteamPriceHandler(cfg *config.Config, prices *appprices.Service, history repository.PriceHistoryRepository) *SteamPriceHandler {
	c := steam.New(steam.ClientConfig{
		SteamID:         cfg.Steam.SteamID,
		Session:         cfg.Steam.Session,
		RefreshToken:    cfg.Steam.RefreshToken,
		PriceTTL:        cfg.Steam.PriceTTL,
		Currency:        cfg.Steam.Currency,
		HistoryBudget:   0,
		MinItemValueUSD: 0,
	}, nil)
	if prices != nil {
		c.SetPriceService(prices)
	}
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
	MarketHashName string  `json:"market_hash_name"`
	Price          float64 `json:"price"`
	PriceType      string  `json:"price_type"`
	Currency       string  `json:"currency"`
}

// GetCurrent returns the current Steam market price for ?name=.
func (h *SteamPriceHandler) GetCurrent(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		middleware.WriteError(w, http.StatusBadRequest, "invalid_request", "MISSING_NAME", "name query parameter is required")
		return
	}
	price, priceType, ok := h.client.CurrentPrice(r.Context(), name)
	if !ok {
		middleware.WriteError(w, http.StatusNotFound, "not_found", "PRICE_NOT_FOUND", "no current price for "+name)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(steamCurrentPriceDTO{
		MarketHashName: name, Price: price, PriceType: priceType, Currency: "USD",
	})
}

type steamPricePointDTO struct {
	Timestamp time.Time `json:"timestamp"`
	Price     float64   `json:"price"`
}

type steamHistoryDTO struct {
	MarketHashName string               `json:"market_hash_name"`
	Currency       string               `json:"currency"`
	Points         []steamPricePointDTO `json:"points"`
}

// GetHistory returns stored historical points for ?name=&from=&to=
// (RFC3339 bounds, both optional; defaults to the last 90 days).
func (h *SteamPriceHandler) GetHistory(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		middleware.WriteError(w, http.StatusBadRequest, "invalid_request", "MISSING_NAME", "name query parameter is required")
		return
	}
	to := time.Now().UTC()
	from := to.Add(-90 * 24 * time.Hour)
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			middleware.WriteError(w, http.StatusBadRequest, "invalid_request", "INVALID_FROM", "from must be RFC3339")
			return
		}
		from = t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			middleware.WriteError(w, http.StatusBadRequest, "invalid_request", "INVALID_TO", "to must be RFC3339")
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
		out.Points = append(out.Points, steamPricePointDTO{Timestamp: p.Timestamp, Price: p.Price})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
