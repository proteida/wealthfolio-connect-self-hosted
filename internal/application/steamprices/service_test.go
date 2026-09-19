package steamprices_test

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/steamprices"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
)

func TestSteamPrices(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "SteamPrices Suite")
}

var errBoom = errors.New("boom")

type stubHistory struct {
	get  repository.HistoricalPrice
	err  error
	list []repository.HistoricalPrice
	lerr error
}

func (s stubHistory) Get(_ context.Context, _, _ string, _ time.Time) (repository.HistoricalPrice, error) {
	return s.get, s.err
}
func (s stubHistory) List(_ context.Context, _, _ string, _, _ time.Time) ([]repository.HistoricalPrice, error) {
	return s.list, s.lerr
}
func (s stubHistory) Upsert(_ context.Context, _ []repository.HistoricalPrice) error { return nil }

type stubProvider struct {
	currency int
	price    float64
	observed time.Time
	ok       bool
	err      error
}

func (s stubProvider) Currency() int { return s.currency }
func (s stubProvider) CurrentPrice(_ context.Context, _ string) (float64, string, time.Time, bool, error) {
	return s.price, "steam_median", s.observed, s.ok, s.err
}

func liveProvider(at time.Time) stubProvider {
	return stubProvider{currency: 1, price: 31.28, observed: at, ok: true}
}

var _ = Describe("Steam price service", func() {
	stored := time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC)
	fresh := time.Date(2025, 5, 9, 12, 0, 0, 0, time.UTC)

	It("reports the provider observation time, not the stale stored time", func() {
		svc := steamprices.NewService(stubHistory{get: repository.HistoricalPrice{
			Asset: "steam:730:AK", Currency: "STEAM_1", Price: 30, Timestamp: stored,
		}}, liveProvider(fresh))
		q, err := svc.GetCurrent(context.Background(), "ak")
		Expect(err).NotTo(HaveOccurred())
		Expect(q.Price).To(BeNumerically("~", 31.28, 1e-9))
		Expect(q.Timestamp).To(Equal(fresh))
	})

	It("falls back to the stored time when the provider reports none", func() {
		svc := steamprices.NewService(stubHistory{get: repository.HistoricalPrice{
			Asset: "steam:730:AK", Currency: "STEAM_1", Price: 30, Timestamp: stored,
		}}, stubProvider{currency: 1, price: 31.28, ok: true})
		q, err := svc.GetCurrent(context.Background(), "ak")
		Expect(err).NotTo(HaveOccurred())
		Expect(q.Timestamp).To(Equal(stored))
	})

	It("rejects untracked items without a provider call", func() {
		svc := steamprices.NewService(stubHistory{err: repository.ErrNotFound}, liveProvider(fresh))
		_, err := svc.GetCurrent(context.Background(), "never synced")
		Expect(err).To(MatchError(steamprices.ErrNotTracked))
	})

	It("rejects blank names", func() {
		svc := steamprices.NewService(stubHistory{}, liveProvider(fresh))
		_, err := svc.GetCurrent(context.Background(), "   ")
		Expect(err).To(HaveOccurred())
	})

	It("maps transient provider failures to upstream, not not-found", func() {
		svc := steamprices.NewService(stubHistory{get: repository.HistoricalPrice{
			Asset: "steam:730:AK", Currency: "STEAM_1", Price: 30, Timestamp: stored,
		}}, stubProvider{currency: 1, err: errBoom})
		_, err := svc.GetCurrent(context.Background(), "ak")
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, steamprices.ErrUpstream)).To(BeTrue())
	})

	It("maps provider misses to not-found", func() {
		svc := steamprices.NewService(stubHistory{get: repository.HistoricalPrice{
			Asset: "steam:730:AK", Currency: "STEAM_1", Price: 30, Timestamp: stored,
		}}, stubProvider{currency: 1})
		_, err := svc.GetCurrent(context.Background(), "ak")
		Expect(errors.Is(err, repository.ErrNotFound)).To(BeTrue())
	})

	It("returns stored history points in USD", func() {
		t0 := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
		t1 := t0.Add(24 * time.Hour)
		svc := steamprices.NewService(stubHistory{list: []repository.HistoricalPrice{
			{Timestamp: t0, Price: 10.5},
			{Timestamp: t1, Price: 11.0},
		}}, liveProvider(fresh))
		pts, curr, err := svc.GetHistory(context.Background(), "ak", t0, t1)
		Expect(err).NotTo(HaveOccurred())
		Expect(curr).To(Equal("USD"))
		Expect(pts).To(HaveLen(2))
		Expect(pts[0].Price).To(Equal(10.5))
		Expect(pts[1].Timestamp).To(Equal(t1))
	})

	It("returns empty history without a store", func() {
		svc := steamprices.NewService(nil, liveProvider(fresh))
		pts, curr, err := svc.GetHistory(context.Background(), "ak", time.Now().Add(-time.Hour), time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(pts).To(BeEmpty())
		Expect(curr).To(Equal("USD"))
	})

	It("surfaces history lookup errors", func() {
		svc := steamprices.NewService(stubHistory{lerr: errBoom}, liveProvider(fresh))
		_, _, err := svc.GetHistory(context.Background(), "ak", time.Now().Add(-time.Hour), time.Now())
		Expect(err).To(HaveOccurred())
	})
})
