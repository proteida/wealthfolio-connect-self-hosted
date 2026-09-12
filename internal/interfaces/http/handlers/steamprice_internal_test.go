package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appprices "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/prices"
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

func (f *fakePriceHistory) Get(_ context.Context, _, _ string, _ time.Time) (repository.HistoricalPrice, error) {
	return repository.HistoricalPrice{}, repository.ErrNotFound
}
func (f *fakePriceHistory) List(_ context.Context, _, _ string, _, _ time.Time) ([]repository.HistoricalPrice, error) {
	return f.rows, nil
}
func (f *fakePriceHistory) Upsert(_ context.Context, ps []repository.HistoricalPrice) error {
	f.rows = append(f.rows, ps...)
	return nil
}

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
		h = &SteamPriceHandler{client: c, history: &fakePriceHistory{rows: []repository.HistoricalPrice{
			{Asset: "steam:730:AK", Timestamp: time.Date(2025, 5, 8, 0, 0, 0, 0, time.UTC), Currency: "STEAM_1", Price: 30, Source: "steam_market"},
		}}}
		r = chi.NewRouter()
		h.RegisterAPIRoutes(r)
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
	})

	It("rejects bad date bounds", func() {
		req := httptest.NewRequest(http.MethodGet, "/steam/prices/history?name=AK&from=nope", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		Expect(rec.Code).To(Equal(http.StatusBadRequest))
	})
})
