package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/cache"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/config"
)

func TestCache(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cache Suite")
}

var _ = Describe("Redis current-price cache", func() {
	var (
		ctx    context.Context
		server *miniredis.Miniredis
		c      interface {
			Get(context.Context, string) (float64, bool, error)
			Set(context.Context, string, float64, time.Duration) error
			GetRaw(context.Context, string) ([]byte, bool, error)
			SetRaw(context.Context, string, []byte, time.Duration) error
			Delete(context.Context, string) error
		}
	)

	BeforeEach(func() {
		var err error
		server, err = miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		ctx = context.Background()
		c = cache.NewCurrentPriceCache(&config.Config{RedisAddr: server.Addr()})
	})

	AfterEach(func() {
		server.Close()
	})

	It("round-trips prices", func() {
		Expect(c.Set(ctx, "px:current:BTC:USD", 60000, time.Hour)).To(Succeed())
		got, found, err := c.Get(ctx, "px:current:BTC:USD")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(got).To(Equal(60000.0))
	})

	It("misses unknown keys", func() {
		_, found, err := c.Get(ctx, "px:current:NOPE:USD")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})

	It("expires entries after their TTL", func() {
		Expect(c.Set(ctx, "px:current:ETH:USD", 3000, time.Hour)).To(Succeed())
		server.FastForward(59 * time.Minute)
		_, found, err := c.Get(ctx, "px:current:ETH:USD")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		server.FastForward(2 * time.Minute)
		_, found, err = c.Get(ctx, "px:current:ETH:USD")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})

	It("overwrites on repeated sets", func() {
		Expect(c.Set(ctx, "k", 1, time.Hour)).To(Succeed())
		Expect(c.Set(ctx, "k", 2, time.Hour)).To(Succeed())
		got, found, err := c.Get(ctx, "k")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(got).To(Equal(2.0))
	})

	It("treats unparseable values as a miss", func() {
		Expect(server.Set("k", "not-a-number")).To(Succeed())
		_, found, err := c.Get(ctx, "k")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})

	It("pings", func() {
		Expect(c.(*cache.Client).Ping(ctx)).To(Succeed())
		bad := cache.NewCurrentPriceCache(&config.Config{RedisAddr: "127.0.0.1:1"})
		Expect(bad.(*cache.Client).Ping(ctx)).NotTo(Succeed())
	})

	It("round-trips raw blobs and deletes them", func() {
		Expect(c.SetRaw(ctx, "px:candle:BTC:USD:2026-09-10", []byte(`{"close":60500}`), time.Hour)).To(Succeed())
		raw, found, err := c.GetRaw(ctx, "px:candle:BTC:USD:2026-09-10")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(string(raw)).To(Equal(`{"close":60500}`))
		Expect(c.Delete(ctx, "px:candle:BTC:USD:2026-09-10")).To(Succeed())
		_, found, err = c.GetRaw(ctx, "px:candle:BTC:USD:2026-09-10")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
		// Deleting a missing key is a no-op, never an error.
		Expect(c.Delete(ctx, "px:candle:BTC:USD:2026-09-10")).To(Succeed())
	})
})

var _ = Describe("Disabled cache", func() {
	It("misses reads and no-ops writes without an address", func() {
		ctx := context.Background()
		c := cache.NewCurrentPriceCache(&config.Config{})
		_, found, err := c.Get(ctx, "k")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
		Expect(c.Set(ctx, "k", 1, time.Hour)).To(Succeed())
		_, found, err = c.GetRaw(ctx, "k")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
		Expect(c.SetRaw(ctx, "k", []byte("x"), time.Hour)).To(Succeed())
		Expect(c.Delete(ctx, "k")).To(Succeed())
	})

	It("treats a nil config as disabled", func() {
		c := cache.NewCurrentPriceCache(nil)
		_, found, err := c.Get(context.Background(), "k")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})

	It("surfaces connection errors from a dead server", func() {
		ctx := context.Background()
		dead, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		addr := dead.Addr()
		dead.Close()
		c := cache.NewCurrentPriceCache(&config.Config{RedisAddr: addr})
		_, _, err = c.Get(ctx, "k")
		Expect(err).To(HaveOccurred())
		Expect(c.Set(ctx, "k", 1, time.Hour)).NotTo(Succeed())
		_, _, err = c.GetRaw(ctx, "k")
		Expect(err).To(HaveOccurred())
		Expect(c.SetRaw(ctx, "k", []byte("x"), time.Hour)).NotTo(Succeed())
		Expect(c.Delete(ctx, "k")).NotTo(Succeed())
	})
})
