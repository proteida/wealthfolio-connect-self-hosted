package prices_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/prices"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
)

func TestPrices(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Prices Suite")
}

// fakeHistory is an in-memory PriceHistoryRepository with UPSERT semantics.
type fakeHistory struct {
	mu     sync.Mutex
	rows   map[string]repository.HistoricalPrice
	getErr error
	upErr  error
	gets   int
	puts   int
}

func historyKey(p repository.HistoricalPrice) string {
	return p.Asset + "|" + p.Timestamp.UTC().Format(time.RFC3339) + "|" + p.Currency + "|" + p.Source
}

func newFakeHistory() *fakeHistory {
	return &fakeHistory{rows: make(map[string]repository.HistoricalPrice)}
}

func (f *fakeHistory) Get(_ context.Context, asset, currency string, at time.Time) (repository.HistoricalPrice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return repository.HistoricalPrice{}, f.getErr
	}
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

func (f *fakeHistory) List(_ context.Context, asset, currency string, from, to time.Time) ([]repository.HistoricalPrice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []repository.HistoricalPrice
	for _, r := range f.rows {
		if r.Asset == asset && r.Currency == currency && !r.Timestamp.Before(from) && !r.Timestamp.After(to) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

func (f *fakeHistory) Upsert(_ context.Context, ps []repository.HistoricalPrice) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upErr != nil {
		return f.upErr
	}
	f.puts++
	for _, p := range ps {
		f.rows[historyKey(p)] = p
	}
	return nil
}

// fakeCache is an in-memory CurrentPriceCache.
type fakeCache struct {
	mu     sync.Mutex
	prices map[string]float64
	raw    map[string][]byte
	ttls   map[string]time.Duration
	getErr error
	gets   int
	sets   int
}

func newFakeCache() *fakeCache {
	return &fakeCache{prices: make(map[string]float64), raw: make(map[string][]byte), ttls: make(map[string]time.Duration)}
}

func (f *fakeCache) Get(_ context.Context, key string) (float64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return 0, false, f.getErr
	}
	v, ok := f.prices[key]
	return v, ok, nil
}

func (f *fakeCache) Set(_ context.Context, key string, price float64, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	f.prices[key] = price
	f.ttls[key] = ttl
	return nil
}

func (f *fakeCache) GetRaw(_ context.Context, key string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.raw[key]
	return v, ok, nil
}

func (f *fakeCache) SetRaw(_ context.Context, key string, raw []byte, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw[key] = raw
	f.ttls[key] = ttl
	return nil
}

func (f *fakeCache) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.prices, key)
	delete(f.raw, key)
	return nil
}

var _ = Describe("Historical prices", func() {
	var (
		ctx     context.Context
		hist    *fakeHistory
		svc     *prices.Service
		fetches int
	)
	day := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	fetch := func(pts ...repository.HistoricalPrice) func(context.Context) ([]repository.HistoricalPrice, error) {
		return func(context.Context) ([]repository.HistoricalPrice, error) {
			fetches++
			return pts, nil
		}
	}

	BeforeEach(func() {
		ctx = context.Background()
		hist = newFakeHistory()
		svc = prices.NewService(hist, nil)
		fetches = 0
	})

	It("returns the stored quote without calling the provider", func() {
		Expect(hist.Upsert(ctx, []repository.HistoricalPrice{
			{Asset: "BTC", Timestamp: day, Currency: "USD", Price: 60000, Source: "test"},
		})).To(Succeed())
		got, err := svc.GetHistorical(ctx, "btc", "usd", day.Add(time.Hour), fetch())
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Price).To(Equal(60000.0))
		Expect(fetches).To(Equal(0))
	})

	It("fetches, UPSERTs and returns on a miss", func() {
		pts := []repository.HistoricalPrice{
			{Asset: "BTC", Timestamp: day, Currency: "USD", Price: 60000, Source: "test"},
			{Asset: "BTC", Timestamp: day.Add(2 * time.Hour), Currency: "USD", Price: 61000, Source: "test"},
		}
		got, err := svc.GetHistorical(ctx, "BTC", "USD", day.Add(3*time.Hour), fetch(pts...))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Price).To(Equal(61000.0))
		Expect(fetches).To(Equal(1))
		Expect(hist.puts).To(Equal(1))
		// Second read is a DB hit: provider stays quiet.
		got, err = svc.GetHistorical(ctx, "BTC", "USD", day.Add(3*time.Hour), fetch())
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Price).To(Equal(61000.0))
		Expect(fetches).To(Equal(1))
	})

	It("propagates fetch errors and stores nothing", func() {
		boom := errors.New("provider down")
		_, err := svc.GetHistorical(ctx, "BTC", "USD", day, func(context.Context) ([]repository.HistoricalPrice, error) {
			return nil, boom
		})
		Expect(err).To(MatchError(boom))
		Expect(hist.puts).To(Equal(0))
	})

	It("returns ErrNotFound when the store and the fetch are both empty", func() {
		_, err := svc.GetHistorical(ctx, "BTC", "USD", day, fetch())
		Expect(err).To(MatchError(repository.ErrNotFound))
	})

	It("refreshes unconditionally, letting providers correct data", func() {
		Expect(hist.Upsert(ctx, []repository.HistoricalPrice{
			{Asset: "BTC", Timestamp: day, Currency: "USD", Price: 60000, Source: "test"},
		})).To(Succeed())
		got, err := svc.RefreshHistorical(ctx, "BTC", "USD", day.Add(time.Hour), fetch(
			repository.HistoricalPrice{Asset: "BTC", Timestamp: day, Currency: "USD", Price: 60500, Source: "test"},
		))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Price).To(Equal(60500.0))
		Expect(fetches).To(Equal(1))
		stored, err := hist.Get(ctx, "BTC", "USD", day.Add(time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.Price).To(Equal(60500.0))
	})

	It("surfaces store lookup errors instead of fetching", func() {
		hist.getErr = errors.New("db down")
		_, err := svc.GetHistorical(ctx, "BTC", "USD", day, fetch())
		Expect(err).To(MatchError(ContainSubstring("history lookup")))
		Expect(fetches).To(Equal(0))
	})
})

var _ = Describe("Current prices", func() {
	var (
		ctx     context.Context
		cache   *fakeCache
		svc     *prices.Service
		fetches int
	)

	BeforeEach(func() {
		ctx = context.Background()
		cache = newFakeCache()
		svc = prices.NewService(nil, cache)
		fetches = 0
	})
	fetch := func(v float64) func(context.Context) (float64, error) {
		return func(context.Context) (float64, error) {
			fetches++
			return v, nil
		}
	}

	It("returns the cached price without calling the provider", func() {
		Expect(cache.Set(ctx, prices.CurrentKey("BTC", "USD"), 60000, time.Hour)).To(Succeed())
		got, err := svc.GetCurrent(ctx, "BTC", "USD", fetch(1))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(60000.0))
		Expect(fetches).To(Equal(0))
	})

	It("fetches, caches with ~1h TTL and returns on a miss", func() {
		got, err := svc.GetCurrent(ctx, "ETH", "USD", fetch(3000))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(3000.0))
		Expect(fetches).To(Equal(1))
		Expect(cache.ttls[prices.CurrentKey("ETH", "USD")]).To(Equal(prices.CurrentPriceTTL))
		got, err = svc.GetCurrent(ctx, "ETH", "USD", fetch(1))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(3000.0))
		Expect(fetches).To(Equal(1))
	})

	It("fails open to the provider on cache errors", func() {
		cache.getErr = errors.New("redis down")
		got, err := svc.GetCurrent(ctx, "ETH", "USD", fetch(3000))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(3000.0))
		Expect(fetches).To(Equal(1))
	})

	It("propagates fetch errors", func() {
		boom := errors.New("provider down")
		_, err := svc.GetCurrent(ctx, "ETH", "USD", func(context.Context) (float64, error) {
			return 0, boom
		})
		Expect(err).To(MatchError(boom))
	})

	It("works provider-only without any store", func() {
		svc := prices.NewService(nil, nil)
		got, err := svc.GetCurrent(ctx, "BTC", "USD", fetch(60000))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(60000.0))
		_, err = svc.GetHistorical(ctx, "BTC", "USD", time.Now(), fetchHist(60000))
		Expect(err).NotTo(HaveOccurred())
	})

	It("keeps historical and current paths separate", func() {
		histSvc := prices.NewService(newFakeHistory(), cache)
		_, err := histSvc.GetHistorical(ctx, "BTC", "USD", time.Now(), fetchHist(60000))
		Expect(err).NotTo(HaveOccurred())
		// The historical write must not populate the current-price key.
		_, found, err := cache.Get(ctx, prices.CurrentKey("BTC", "USD"))
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})
})

func fetchHist(v float64) func(context.Context) ([]repository.HistoricalPrice, error) {
	return func(context.Context) ([]repository.HistoricalPrice, error) {
		return []repository.HistoricalPrice{{Asset: "BTC", Timestamp: time.Now().UTC(), Currency: "USD", Price: v, Source: "test"}}, nil
	}
}

var _ = Describe("Daily candles", func() {
	var (
		ctx   context.Context
		hist  *fakeHistory
		cache *fakeCache
		svc   *prices.Service
	)
	day := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	BeforeEach(func() {
		ctx = context.Background()
		hist = newFakeHistory()
		cache = newFakeCache()
		svc = prices.NewServiceWithClock(hist, cache, func() time.Time { return day })
	})

	It("round-trips the incomplete candle through Redis", func() {
		Expect(svc.PutCandle(ctx, prices.Candle{
			Asset: "BTC", Currency: "USD", Day: day,
			Open: 60000, High: 61000, Low: 59000, Close: 60500, Source: "test",
		})).To(Succeed())
		got, found, err := svc.GetCandle(ctx, "BTC", "USD", day)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(got.Close).To(Equal(60500.0))
		Expect(got.High).To(Equal(61000.0))
	})

	It("finalizes the close into history and drops the cached copy", func() {
		Expect(svc.PutCandle(ctx, prices.Candle{
			Asset: "BTC", Currency: "USD", Day: day,
			Open: 60000, High: 61000, Low: 59000, Close: 60500, Source: "test",
		})).To(Succeed())
		Expect(svc.FinalizeCandle(ctx, "BTC", "USD", day)).To(Succeed())
		stored, err := hist.Get(ctx, "BTC", "USD", day.Add(25*time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.Price).To(Equal(60500.0))
		_, found, err := svc.GetCandle(ctx, "BTC", "USD", day)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})

	It("finalizing without a cached candle only drops the key", func() {
		Expect(svc.FinalizeCandle(ctx, "ETH", "USD", day)).To(Succeed())
		_, err := hist.Get(ctx, "ETH", "USD", day.Add(25*time.Hour))
		Expect(err).To(MatchError(repository.ErrNotFound))
	})
})

var _ = Describe("Raw blobs", func() {
	var (
		ctx   context.Context
		cache *fakeCache
		svc   *prices.Service
	)

	BeforeEach(func() {
		ctx = context.Background()
		cache = newFakeCache()
		svc = prices.NewService(nil, cache)
	})

	It("serves stored bytes without calling fetch", func() {
		Expect(cache.SetRaw(ctx, "hl:marks", []byte(`{"BTC":60000}`), time.Hour)).To(Succeed())
		got, err := svc.GetBlob(ctx, "hl:marks", time.Hour, func(context.Context) ([]byte, error) {
			return nil, errors.New("must not fetch")
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal(`{"BTC":60000}`))
	})

	It("stores fetch output for ttl on a miss", func() {
		got, err := svc.GetBlob(ctx, "hl:marks", time.Hour, func(context.Context) ([]byte, error) {
			return []byte(`{"BTC":60000}`), nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal(`{"BTC":60000}`))
		Expect(cache.ttls["hl:marks"]).To(Equal(time.Hour))
	})
})
