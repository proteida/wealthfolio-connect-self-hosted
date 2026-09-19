package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appprices "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/prices"
	steamprices "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/steamprices"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/cache"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/steam"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/config"
)

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

func writePriceJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type fakePriceHistory struct {
	rows []repository.HistoricalPrice
}

func (f *fakePriceHistory) Get(_ context.Context, asset, currency string, at time.Time) (repository.HistoricalPrice, error) {
	var best *repository.HistoricalPrice
	for _, r := range f.rows {
		r := r
		if r.Asset != asset || r.Currency != currency || r.Timestamp.After(at) {
			continue
		}
		if best == nil || r.Timestamp.After(best.Timestamp) {
			best = &r
		}
	}
	if best == nil {
		return repository.HistoricalPrice{}, repository.ErrNotFound
	}
	return *best, nil
}
func (f *fakePriceHistory) List(_ context.Context, asset, currency string, from, to time.Time) ([]repository.HistoricalPrice, error) {
	var out []repository.HistoricalPrice
	for _, r := range f.rows {
		if r.Asset != asset || r.Currency != currency {
			continue
		}
		if r.Timestamp.Before(from) || r.Timestamp.After(to) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
func (f *fakePriceHistory) Upsert(_ context.Context, ps []repository.HistoricalPrice) error {
	f.rows = append(f.rows, ps...)
	return nil
}

// failingProvider is a steamprices.Provider whose upstream is down.
type failingProvider struct{}

func (failingProvider) Currency() int { return 1 }
func (failingProvider) CurrentPrice(_ context.Context, _ string) (float64, string, time.Time, bool, error) {
	return 0, "", time.Time{}, false, errUpstream()
}

func errUpstream() error { return errors.New("steam transient: http 429") }

var _ = Describe("Steam price endpoints", func() {
	var (
		srv     *httptest.Server
		handler http.HandlerFunc
		msrv    *miniredis.Miniredis
		h       *SteamPriceHandler
		r       chi.Router
	)

	BeforeEach(func() {
		var err error
		msrv, err = miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			defer GinkgoRecover()
			handler(w, req)
		}))
		svc := appprices.NewService(nil, cache.NewCurrentPriceCache(&config.Config{RedisAddr: msrv.Addr()}))
		c := steam.New(steam.ClientConfig{SteamID: "1", PriceTTL: time.Hour, Currency: 1, CommunityBase: srv.URL}, srv.Client())
		c.SetPriceService(svc)
		hist := &fakePriceHistory{rows: []repository.HistoricalPrice{
			{Asset: "steam:730:AK", Timestamp: time.Date(2025, 5, 8, 0, 0, 0, 0, time.UTC), Currency: "STEAM_1", Price: 30, Source: "steam_market"},
			{Asset: "steam:730:AK-47 | REDLINE", Timestamp: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Currency: "STEAM_1", Price: 1, Source: "steam_market"},
		}}
		c.SetPriceHistoryStore(hist)
		h = NewSteamPriceHandler(steamprices.NewService(hist, c))
		r = chi.NewRouter()
		h.RegisterPublicAPIRoutes(r)
	})

	AfterEach(func() {
		srv.Close()
		msrv.Close()
	})

	It("serves current prices by name, cached on repeat", func() {
		calls := 0
		handler = func(w http.ResponseWriter, req *http.Request) {
			calls++
			writePriceJSON(w, map[string]any{"success": true, "median_price": "$31.28", "volume": "5"})
		}
		get := func() map[string]any {
			req := httptest.NewRequest(http.MethodGet, "/steam/prices/current?name="+urlQueryEscape("AK-47 | Redline"), nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			Expect(rec.Code).To(Equal(http.StatusOK))
			var out map[string]any
			Expect(json.Unmarshal(rec.Body.Bytes(), &out)).To(Succeed())
			return out
		}
		Expect(get()["price"]).To(BeNumerically("~", 31.28, 1e-9))
		Expect(get()["price"]).To(BeNumerically("~", 31.28, 1e-9))
		Expect(calls).To(Equal(1)) // second read served from Redis
	})

	It("rejects missing names", func() {
		req := httptest.NewRequest(http.MethodGet, "/steam/prices/current", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		Expect(rec.Code).To(Equal(http.StatusBadRequest))
	})

	It("serves history from the database", func() {
		req := httptest.NewRequest(http.MethodGet, "/steam/prices/history?name=AK&from=2025-05-01T00:00:00Z&to=2025-06-01T00:00:00Z", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		Expect(rec.Code).To(Equal(http.StatusOK))
		var out map[string]any
		Expect(json.Unmarshal(rec.Body.Bytes(), &out)).To(Succeed())
		Expect(out["points"]).To(HaveLen(1))
		// Storage stays namespaced (STEAM_1); the API exposes USD.
		Expect(out["currency"]).To(Equal("USD"))
	})

	It("rejects bad date bounds", func() {
		req := httptest.NewRequest(http.MethodGet, "/steam/prices/history?name=AK&from=nope", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		Expect(rec.Code).To(Equal(http.StatusBadRequest))
	})

	It("rejects untracked names without calling Steam", func() {
		calls := 0
		handler = func(w http.ResponseWriter, req *http.Request) {
			calls++
			writePriceJSON(w, map[string]any{"success": true, "median_price": "$1.00"})
		}
		req := httptest.NewRequest(http.MethodGet, "/steam/prices/current?name="+urlQueryEscape("Never Synced Skin XYZ"), nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		Expect(rec.Code).To(Equal(http.StatusNotFound))
		Expect(calls).To(Equal(0))
	})

	It("accepts unix seconds and YYYY-MM-DD bounds", func() {
		for _, q := range []string{
			"/steam/prices/history?name=AK&from=1746057600&to=1780272000",
			"/steam/prices/history?name=AK&from=2025-05-01&to=2025-06-01",
			"/steam/prices/history?name=AK&from=2025-05-01T00:00:00Z&to=2025-06-01",
		} {
			req := httptest.NewRequest(http.MethodGet, q, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			Expect(rec.Code).To(Equal(http.StatusOK))
		}
	})

	It("reports fresh observation time for refetched quotes", func() {
		handler = func(w http.ResponseWriter, req *http.Request) {
			writePriceJSON(w, map[string]any{"success": true, "median_price": "$31.28", "volume": "5"})
		}
		req := httptest.NewRequest(http.MethodGet, "/steam/prices/current?name="+urlQueryEscape("AK-47 | Redline"), nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		Expect(rec.Code).To(Equal(http.StatusOK))
		var out map[string]any
		Expect(json.Unmarshal(rec.Body.Bytes(), &out)).To(Succeed())
		ts, err := time.Parse(time.RFC3339, out["timestamp"].(string))
		Expect(err).NotTo(HaveOccurred())
		// The stored row is from 2020: a live refetch must not reuse it.
		Expect(time.Since(ts)).To(BeNumerically("<", 5*time.Minute))
	})

	It("maps upstream failures to 502, not 404", func() {
		hist := &fakePriceHistory{rows: []repository.HistoricalPrice{
			{Asset: "steam:730:AK", Timestamp: time.Now().UTC().Add(-time.Minute), Currency: "STEAM_1", Price: 30, Source: "steam_quote"},
		}}
		h := NewSteamPriceHandler(steamprices.NewService(hist, failingProvider{}))
		r := chi.NewRouter()
		h.RegisterPublicAPIRoutes(r)
		req := httptest.NewRequest(http.MethodGet, "/steam/prices/current?name=AK", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		Expect(rec.Code).To(Equal(http.StatusBadGateway))
	})
})
