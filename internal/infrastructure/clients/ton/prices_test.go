package ton

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
)

func testPricer(srv *httptest.Server) *pricer {
	return newPricer(srv.URL, srv.URL, srv.Client(), func(time.Duration) {}, time.Millisecond, 5)
}

func rangeBody(points ...any) map[string]any {
	rows := make([]any, 0, len(points))
	for _, p := range points {
		rows = append(rows, p)
	}
	return map[string]any{"prices": rows}
}

func chartBody(points ...any) map[string]any {
	rows := make([]any, 0, len(points))
	for _, p := range points {
		rows = append(rows, p)
	}
	return map[string]any{"points": rows}
}

var _ = Describe("TonAPI pricer", func() {
	var server *httptest.Server
	var handler http.HandlerFunc
	var calls []string
	BeforeEach(func() {
		calls = nil
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			calls = append(calls, r.URL.Path+"?"+r.URL.RawQuery)
			handler(w, r)
		}))
	})
	AfterEach(func() { server.Close() })

	// 2026-07-10 points, newest-first like the real endpoint.
	chartPoints := func() map[string]any {
		return chartBody(
			[]any{1783648800.0, 1.7},
			[]any{1783645200.0, 1.65},
			[]any{1783641600.0, 1.6},
		)
	}

	It("serves any timestamp from one wide window cached per token", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Path).To(Equal("/chart"))
			Expect(r.URL.Query().Get("token")).To(Equal("TON"))
			Expect(r.URL.Query().Get("currency")).To(Equal("USD"))
			Expect(r.URL.Query().Get("points_count")).To(Equal("200"))
			writeJSON(w, chartPoints())
		}
		p := testPricer(server)
		u, ok := p.unitPrice(context.Background(), "TON", "", 1783647000)
		Expect(ok).To(BeTrue())
		Expect(u).To(Equal(1.65))
		u, ok = p.unitPrice(context.Background(), "GRAM", "", 1783648000)
		Expect(ok).To(BeTrue())
		Expect(u).To(Equal(1.65))
		Expect(calls).To(HaveLen(1)) // GRAM aliases TON's cache entry
	})

	It("addresses Jettons by master and retries limits", func() {
		attempts := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Query().Get("token")).To(Equal(testMaster))
			attempts++
			if attempts == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			writeJSON(w, chartPoints())
		}
		u, ok := testPricer(server).unitPrice(context.Background(), "TSTON", testMaster, 1783647000)
		Expect(ok).To(BeTrue())
		Expect(u).To(Equal(1.65))
		Expect(attempts).To(Equal(2))
	})

	It("falls back to CoinGecko when TonAPI has no history", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/chart" {
				writeJSON(w, chartBody())
				return
			}
			Expect(r.URL.Path).To(Equal("/coins/apple-xstock/market_chart/range"))
			writeJSON(w, rangeBody(
				[]any{1783641600000.0, 316.0},
			))
		}
		u, ok := testPricer(server).unitPrice(context.Background(), "AAPLX", "0:MASTER", 1783647000)
		Expect(ok).To(BeTrue())
		Expect(u).To(Equal(316.0))
	})

	It("remembers failed tokens instead of hammering the quota", func() {
		calls := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusTooManyRequests)
		}
		p := testPricer(server)
		_, ok := p.unitPrice(context.Background(), "TON", "", 1783647000)
		Expect(ok).To(BeFalse())
		_, ok = p.unitPrice(context.Background(), "TON", "", 1783647000)
		Expect(ok).To(BeFalse())
		Expect(calls).To(Equal(10)) // chart round then CoinGecko round, each cached
	})

	It("values stables at one without any calls", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/chart" {
				writeJSON(w, chartBody())
				return
			}
			Fail(r.URL.Path)
		}
		p := testPricer(server)
		v, ok := p.swapValue(context.Background(), "USDT", "", 100, "SPYX", "0:X", 50, 1783647000)
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(100.0))
		u, ok := p.unitPrice(context.Background(), "USDT", "", 1783647000)
		Expect(ok).To(BeTrue())
		Expect(u).To(Equal(1.0))
		Expect(calls).To(BeEmpty())
		// Unknown tokens each cost one chart lookup, then fail terminally.
		_, ok = p.swapValue(context.Background(), "AAA", "", 1, "BBB", "", 2, 1783647000)
		Expect(ok).To(BeFalse())
		Expect(calls).To(HaveLen(2))
	})
})

var _ = Describe("CoinGecko pricer", func() {
	var server *httptest.Server
	var handler http.HandlerFunc
	var calls []string
	BeforeEach(func() {
		calls = nil
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			calls = append(calls, r.URL.Path+"?"+r.URL.RawQuery)
			handler(w, r)
		}))
	})
	AfterEach(func() { server.Close() })

	// 2026-07-10 00:00 UTC = 1783641600.
	dayPoints := func() map[string]any {
		return rangeBody(
			[]any{1783641600000.0, 1.6},
			[]any{1783645200000.0, 1.65},
			[]any{1783648800000.0, 1.7},
		)
	}

	It("picks the latest point at or before the timestamp and caches per day", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Path).To(Equal("/coins/the-open-network/market_chart/range"))
			Expect(r.URL.Query().Get("vs_currency")).To(Equal("usd"))
			writeJSON(w, dayPoints())
		}
		p := testPricer(server)
		price, err := p.tonUSD(context.Background(), 1783647000)
		Expect(err).NotTo(HaveOccurred())
		Expect(price).To(Equal(1.65))
		// Same UTC day hits the cache; a later point reuses it too.
		price, err = p.tonUSD(context.Background(), 1783648000)
		Expect(err).NotTo(HaveOccurred())
		Expect(price).To(Equal(1.65))
		Expect(calls).To(HaveLen(1))
	})

	It("falls back to the earliest point before range start", func() {
		handler = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, dayPoints()) }
		price, err := testPricer(server).tonUSD(context.Background(), 1783641000)
		Expect(err).NotTo(HaveOccurred())
		Expect(price).To(Equal(1.6))
	})

	It("retries rate limits and fails on empty ranges", func() {
		attempts := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			attempts++
			if attempts == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if attempts == 2 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeJSON(w, dayPoints())
		}
		price, err := testPricer(server).tonUSD(context.Background(), 1783647000)
		Expect(err).NotTo(HaveOccurred())
		Expect(price).To(Equal(1.65))
		Expect(attempts).To(Equal(3))
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, rangeBody())
		}
		_, err = testPricer(server).tonUSD(context.Background(), 1783647000)
		Expect(err).To(HaveOccurred())
	})

	It("remembers failed days instead of hammering the quota", func() {
		calls := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusTooManyRequests)
		}
		p := testPricer(server)
		_, err := p.tonUSD(context.Background(), 1783647000)
		Expect(err).To(HaveOccurred())
		_, err = p.tonUSD(context.Background(), 1783647000)
		Expect(err).To(HaveOccurred())
		Expect(calls).To(Equal(5)) // one exhausted round, then cached failure
	})

	It("uses CoinGecko day cache for the fallback feed", func() {
		handler = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, dayPoints()) }
		p := testPricer(server)
		_, ok := p.swapValue(context.Background(), "AAA", "", 1, "BBB", "", 2, 1783647000)
		Expect(ok).To(BeFalse())
		price, err := p.coinPrice(context.Background(), "the-open-network", 1783647000)
		Expect(err).NotTo(HaveOccurred())
		Expect(price).To(Equal(1.65))
		_, err = p.coinPrice(context.Background(), "the-open-network", 1783648000)
		Expect(err).NotTo(HaveOccurred())
		Expect(calls).To(HaveLen(3)) // two empty chart lookups plus one range
	})
})

var _ = Describe("Price helpers", func() {
	It("spreads USD across units and formats figures", func() {
		Expect(usdPrice(10, 30, true)).To(Equal(3.0))
		Expect(usdPrice(10, 0, false)).To(BeZero())
		Expect(usdPrice(0, 30, true)).To(BeZero())
		Expect(formatUSD(100)).To(Equal("100.00"))
	})

	It("backs off exponentially honoring Retry-After", func() {
		Expect(computeBackoff(time.Millisecond, 0, 0)).To(Equal(time.Millisecond))
		Expect(computeBackoff(time.Millisecond, 1, 0)).To(Equal(2 * time.Millisecond))
		Expect(computeBackoff(time.Millisecond, 0, 5*time.Second)).To(Equal(5 * time.Second))
		Expect(computeBackoff(time.Hour, 100, 0)).To(Equal(maxRetryDelay))
	})

	It("parses Retry-After seconds and dates", func() {
		now := time.Unix(1800000000, 0)
		Expect(retryAfter(http.Header{"Retry-After": {"120"}}, now)).To(Equal(2 * time.Minute))
		Expect(retryAfter(http.Header{"Retry-After": {now.Add(time.Minute).UTC().Format(http.TimeFormat)}}, now)).To(Equal(time.Minute))
		Expect(retryAfter(http.Header{}, now)).To(BeZero())
		Expect(retryAfter(http.Header{"Retry-After": {"bogus"}}, now)).To(BeZero())
	})
})

type stubPriceHistory struct {
	rows   map[string]repository.HistoricalPrice
	getErr error
	lists  int
	puts   int
}

func priceKey(p repository.HistoricalPrice) string {
	return p.Asset + "|" + p.Timestamp.UTC().Format(time.RFC3339) + "|" + p.Currency + "|" + p.Source
}

func newStubPriceHistory() *stubPriceHistory {
	return &stubPriceHistory{rows: make(map[string]repository.HistoricalPrice)}
}

func (s *stubPriceHistory) Get(_ context.Context, asset, currency string, at time.Time) (repository.HistoricalPrice, error) {
	if s.getErr != nil {
		return repository.HistoricalPrice{}, s.getErr
	}
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

func (s *stubPriceHistory) List(_ context.Context, asset, currency string, from, to time.Time) ([]repository.HistoricalPrice, error) {
	s.lists++
	var out []repository.HistoricalPrice
	for _, r := range s.rows {
		if r.Asset == asset && r.Currency == currency && !r.Timestamp.Before(from) && !r.Timestamp.After(to) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

func (s *stubPriceHistory) Upsert(_ context.Context, ps []repository.HistoricalPrice) error {
	s.puts++
	for _, p := range ps {
		s.rows[priceKey(p)] = p
	}
	return nil
}

var _ = Describe("Pricer history store", func() {
	var server *httptest.Server
	var handler http.HandlerFunc
	var calls []string
	BeforeEach(func() {
		calls = nil
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			calls = append(calls, r.URL.Path)
			handler(w, r)
		}))
	})
	AfterEach(func() { server.Close() })

	dayBody := func() map[string]any {
		return rangeBody(
			[]any{1783641600000.0, 1.6},
			[]any{1783645200000.0, 1.65},
			[]any{1783648800000.0, 1.7},
		)
	}
	emptyChart := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chart" {
			writeJSON(w, chartBody())
			return
		}
		writeJSON(w, dayBody())
	}

	It("serves TonAPI windows from the store without HTTP", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Fail("provider must not be called on a store hit: " + r.URL.Path)
		}
		store := newStubPriceHistory()
		recent := time.Now().UTC().Truncate(time.Hour)
		Expect(store.Upsert(context.Background(), []repository.HistoricalPrice{
			{Asset: "TON", Timestamp: recent.Add(-3 * time.Hour), Currency: "USD", Price: 1.6, Source: "tonapi"},
			{Asset: "TON", Timestamp: recent.Add(-2 * time.Hour), Currency: "USD", Price: 1.65, Source: "tonapi"},
		})).To(Succeed())
		p := testPricer(server)
		p.history = store
		u, ok := p.unitPrice(context.Background(), "TON", "", recent.Add(-time.Hour).Unix())
		Expect(ok).To(BeTrue())
		Expect(u).To(Equal(1.65))
		Expect(calls).To(BeEmpty())
	})

	It("serves CoinGecko days from the store without HTTP", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Fail("provider must not be called on a store hit: " + r.URL.Path)
		}
		store := newStubPriceHistory()
		// Recent chart points force the TonAPI leg to miss by selection
		// (nothing at or before the old timestamp) without any HTTP.
		recent := time.Now().UTC().Truncate(time.Hour)
		oldDay := time.Unix(1783641600, 0).UTC()
		Expect(store.Upsert(context.Background(), []repository.HistoricalPrice{
			{Asset: "TON", Timestamp: recent.Add(-time.Hour), Currency: "USD", Price: 1.9, Source: "tonapi"},
			{Asset: "the-open-network", Timestamp: oldDay, Currency: "USD", Price: 1.6, Source: "coingecko"},
			{Asset: "the-open-network", Timestamp: oldDay.Add(time.Hour), Currency: "USD", Price: 1.65, Source: "coingecko"},
		})).To(Succeed())
		p := testPricer(server)
		p.history = store
		u, ok := p.unitPrice(context.Background(), "TON", "", 1783647000)
		Expect(ok).To(BeTrue())
		Expect(u).To(Equal(1.65))
		Expect(calls).To(BeEmpty())
	})

	It("UPSERTs fetched ranges for the next sync", func() {
		handler = emptyChart
		store := newStubPriceHistory()
		p := testPricer(server)
		p.history = store
		u, ok := p.unitPrice(context.Background(), "TON", "", 1783647000)
		Expect(ok).To(BeTrue())
		Expect(u).To(Equal(1.65))
		Expect(calls).To(HaveLen(2)) // empty chart, then the range
		Expect(store.puts).To(Equal(1))
		// A fresh pricer over the same store reuses the day: only the
		// (empty) chart is refetched, never the range.
		p2 := testPricer(server)
		p2.history = store
		u, ok = p2.unitPrice(context.Background(), "TON", "", 1783647000)
		Expect(ok).To(BeTrue())
		Expect(u).To(Equal(1.65))
		Expect(calls).To(HaveLen(3))
	})

	It("behaves memory-only without a store", func() {
		handler = emptyChart
		p := testPricer(server)
		Expect(p.history).To(BeNil())
		u, ok := p.unitPrice(context.Background(), "TON", "", 1783647000)
		Expect(ok).To(BeTrue())
		Expect(u).To(Equal(1.65))
		Expect(calls).To(HaveLen(2))
	})
})
