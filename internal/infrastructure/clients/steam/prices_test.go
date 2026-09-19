package steam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
)

var _ = Describe("Steam market prices", func() {
	var server *httptest.Server
	var handler http.HandlerFunc
	var calls int
	BeforeEach(func() {
		calls = 0
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			calls++
			handler(w, r)
		}))
	})
	AfterEach(func() { server.Close() })

	newClient := func() *Client {
		c := testClient(server)
		c.cfg.CommunityBase = server.URL
		return c
	}

	It("prefers median over lowest", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Query().Get("appid")).To(Equal("730"))
			Expect(r.URL.Query().Get("currency")).To(Equal("1"))
			Expect(r.URL.Query().Get("market_hash_name")).To(Equal("AK-47 | Redline (Field-Tested)"))
			writeJSON(w, map[string]any{"success": true, "median_price": "$31.28", "lowest_price": "$30.00", "volume": "12"})
		}
		v, kind, _, ok, err := newClient().CurrentPrice(context.Background(), "AK-47 | Redline (Field-Tested)")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(kind).To(Equal("steam_median"))
		Expect(v).To(BeNumerically("~", 31.28, 1e-9))
		Expect(calls).To(Equal(1))
	})

	It("falls back to lowest and rejects garbage", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"success": true, "lowest_price": "$30.00"})
		}
		v, _, _, ok, err := newClient().CurrentPrice(context.Background(), "AK")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(30.0))
	})

	It("fails when nothing usable returns", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"success": false})
		}
		_, _, _, ok, err := newClient().CurrentPrice(context.Background(), "AK")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("namespaces cache keys per game and currency", func() {
		asset, curr := priceKey("AK", 1)
		Expect(asset).To(ContainSubstring("730"))
		other, _ := priceKey("AK", 3)
		Expect(other).NotTo(Equal(curr))
	})

	It("parses money tolerantly", func() {
		v, ok := parseSteamMoney("$31.28")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(31.28))
		v, ok = parseSteamMoney("31,28€")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(31.28))
		v, ok = parseSteamMoney("1,234.56")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(1234.56))
		v, ok = parseSteamMoney("1.234,56")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(1234.56))
		v, ok = parseSteamMoney("1,234")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(1234.0))
		_, ok = parseSteamMoney("--")
		Expect(ok).To(BeFalse())
		_, ok = parseSteamMoney("")
		Expect(ok).To(BeFalse())
	})

	It("normalizes price keys across case and whitespace", func() {
		a1, c1 := PriceAssetKey("Gamma 2 Case", 1)
		a2, c2 := PriceAssetKey("  gamma  2  CASE ", 1)
		Expect(a1).To(Equal(a2))
		Expect(c1).To(Equal(c2))
		Expect(priceDedupeKey("Gamma  2 Case")).To(Equal(priceDedupeKey("gamma 2 case")))
	})

	It("syncs price history through the store", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Path).To(Equal("/market/pricehistory/"))
			Expect(r.URL.Query().Get("currency")).To(Equal("1"))
			writeJSON(w, map[string]any{"success": true, "prices": []any{
				[]any{"May 08 2025", 31.28, "12"},
				[]any{"May 09 2025", "32.00", "7"},
				[]any{"bogus"},
			}})
		}
		store := newStubPriceStore()
		c := newClient()
		c.cfg.Session = "sessionid=x"
		n, err := c.SyncPriceHistory(context.Background(), "AK", store, 31.5, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(2))
		Expect(store.puts).To(Equal(1))
	})

	It("rejects history series in the wrong currency", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			// Session-currency series (~44x the USD quote): must never
			// land in the USD store.
			writeJSON(w, map[string]any{"success": true, "prices": []any{
				[]any{"May 08 2025", 1380.0, "12"},
				[]any{"May 09 2025", 1408.0, "7"},
			}})
		}
		store := newStubPriceStore()
		c := newClient()
		c.cfg.Session = "sessionid=x"
		n, err := c.SyncPriceHistory(context.Background(), "AK", store, 31.5, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0))
		Expect(store.puts).To(Equal(0))
	})

	It("skips history without a fresh quote to validate against", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"success": true, "prices": []any{
				[]any{"May 08 2025", 31.28, "12"},
			}})
		}
		store := newStubPriceStore()
		c := newClient()
		c.cfg.Session = "sessionid=x"
		n, err := c.SyncPriceHistory(context.Background(), "AK", store, 0, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0))
		Expect(store.puts).To(Equal(0))
	})

	It("requires a session for price history", func() {
		c := newClient()
		c.cfg.Session = ""
		_, err := c.SyncPriceHistory(context.Background(), "AK", newStubPriceStore(), 31.5, true)
		Expect(err).To(HaveOccurred())
		Expect(calls).To(Equal(0))
	})
})

type stubPriceStore struct {
	puts int
	rows []repository.HistoricalPrice
}

func newStubPriceStore() *stubPriceStore { return &stubPriceStore{} }

func (s *stubPriceStore) Get(_ context.Context, asset, currency string, at time.Time) (repository.HistoricalPrice, error) {
	var best *repository.HistoricalPrice
	for _, r := range s.rows {
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
func (s *stubPriceStore) List(_ context.Context, asset, currency string, from, to time.Time) ([]repository.HistoricalPrice, error) {
	var out []repository.HistoricalPrice
	for _, r := range s.rows {
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
func (s *stubPriceStore) Upsert(_ context.Context, ps []repository.HistoricalPrice) error {
	s.puts++
	s.rows = append(s.rows, ps...)
	return nil
}

var _ = Describe("Current price recency", func() {
	var server *httptest.Server
	var calls int
	BeforeEach(func() {
		calls = 0
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			calls++
			writeJSON(w, map[string]any{"success": true, "median_price": "$31.28"})
		}))
	})
	AfterEach(func() { server.Close() })

	newClient := func(store *stubPriceStore) *Client {
		c := testClient(server)
		c.cfg.CommunityBase = server.URL
		c.SetPriceHistoryStore(store)
		return c
	}

	It("serves fresh stored points without HTTP", func() {
		store := newStubPriceStore()
		Expect(store.Upsert(context.Background(), []repository.HistoricalPrice{
			{Asset: "steam:730:AK", Timestamp: time.Now().UTC().Add(-time.Minute), Currency: "STEAM_1", Price: 30, Source: "steam_market"},
		})).To(Succeed())
		v, kind, _, ok, err := newClient(store).CurrentPrice(context.Background(), "AK")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(kind).To(Equal("steam_history"))
		Expect(v).To(Equal(30.0))
		Expect(calls).To(Equal(0))
	})

	It("refetches and remembers when stored points are stale", func() {
		store := newStubPriceStore()
		Expect(store.Upsert(context.Background(), []repository.HistoricalPrice{
			{Asset: "steam:730:AK", Timestamp: time.Now().UTC().Add(-time.Hour), Currency: "STEAM_1", Price: 30, Source: "steam_market"},
		})).To(Succeed())
		c := newClient(store)
		c.cfg.PriceTTL = time.Minute
		v, kind, _, ok, err := c.CurrentPrice(context.Background(), "AK")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(kind).To(Equal("steam_median"))
		Expect(v).To(BeNumerically("~", 31.28, 1e-9))
		Expect(calls).To(Equal(1))
	})
})
