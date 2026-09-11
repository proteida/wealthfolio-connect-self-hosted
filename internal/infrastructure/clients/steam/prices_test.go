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
		v, kind, ok := newClient().CurrentPrice(context.Background(), "AK-47 | Redline (Field-Tested)")
		Expect(ok).To(BeTrue())
		Expect(kind).To(Equal("steam_median"))
		Expect(v).To(BeNumerically("~", 31.28, 1e-9))
		Expect(calls).To(Equal(1))
	})

	It("falls back to lowest and rejects garbage", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"success": true, "lowest_price": "$30.00"})
		}
		v, _, ok := newClient().CurrentPrice(context.Background(), "AK")
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(30.0))
	})

	It("fails when nothing usable returns", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"success": false})
		}
		_, _, ok := newClient().CurrentPrice(context.Background(), "AK")
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
		_, ok = parseSteamMoney("--")
		Expect(ok).To(BeFalse())
		_, ok = parseSteamMoney("")
		Expect(ok).To(BeFalse())
	})

	It("syncs price history through the store", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Path).To(Equal("/market/pricehistory/"))
			writeJSON(w, map[string]any{"success": true, "prices": []any{
				[]any{"May 08 2025", 31.28, "12"},
				[]any{"May 09 2025", "32.00", "7"},
				[]any{"bogus"},
			}})
		}
		store := newStubPriceStore()
		c := newClient()
		c.cfg.Session = "sessionid=x"
		n, err := c.SyncPriceHistory(context.Background(), "AK", store)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(2))
		Expect(store.puts).To(Equal(1))
	})

	It("requires a session for price history", func() {
		c := newClient()
		c.cfg.Session = ""
		_, err := c.SyncPriceHistory(context.Background(), "AK", newStubPriceStore())
		Expect(err).To(HaveOccurred())
		Expect(calls).To(Equal(0))
	})
})

type stubPriceStore struct {
	puts int
	rows []repository.HistoricalPrice
}

func newStubPriceStore() *stubPriceStore { return &stubPriceStore{} }

func (s *stubPriceStore) Get(_ context.Context, _, _ string, _ time.Time) (repository.HistoricalPrice, error) {
	return repository.HistoricalPrice{}, repository.ErrNotFound
}
func (s *stubPriceStore) List(_ context.Context, _, _ string, _, _ time.Time) ([]repository.HistoricalPrice, error) {
	return nil, nil
}
func (s *stubPriceStore) Upsert(_ context.Context, ps []repository.HistoricalPrice) error {
	s.puts++
	s.rows = append(s.rows, ps...)
	return nil
}
