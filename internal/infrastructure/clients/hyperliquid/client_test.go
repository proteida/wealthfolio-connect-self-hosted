package hyperliquid_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/alicebob/miniredis/v2"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/prices"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/cache"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/hyperliquid"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/config"
)

func TestHyperliquid(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Hyperliquid Client Suite")
}

func newServer(handler http.HandlerFunc) (*httptest.Server, *http.Client) {
	srv := httptest.NewServer(handler)
	return srv, srv.Client()
}

var _ = Describe("Hyperliquid Client", func() {
	It("returns slug hyperliquid", func() {
		Expect(hyperliquid.New("0xabc", "", nil).ID()).To(Equal("hyperliquid"))
	})

	It("fails when wallet address is empty", func() {
		_, err := hyperliquid.New("", "http://x", nil).Fetch(context.Background())
		Expect(err).To(HaveOccurred())
	})

	It("merges spot balances and perp account value into a single snapshot", func() {
		srv, hc := newServer(func(w http.ResponseWriter, r *http.Request) {
			Expect(r.Method).To(Equal(http.MethodPost))
			Expect(r.URL.Path).To(Equal("/info"))
			var body struct {
				Type string `json:"type"`
				User string `json:"user"`
			}
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			switch body.Type {
			case "spotClearinghouseState":
				Expect(body.User).To(Equal("0xabc"))
				_ = json.NewEncoder(w).Encode(map[string]any{
					"balances": []any{
						map[string]any{"coin": "USDC", "total": "100", "entryNtl": "100"},
						map[string]any{"coin": "PURR", "total": "10", "entryNtl": "20"},
						map[string]any{"coin": "ZERO", "total": "0", "entryNtl": "0"},
					},
				})
			case "spotMetaAndAssetCtxs":
				_ = json.NewEncoder(w).Encode([]any{
					map[string]any{
						"tokens": []any{
							map[string]any{"name": "USDC", "index": 0},
							map[string]any{"name": "PURR", "index": 1},
						},
						"universe": []any{
							// Market names ("PURR/USDC", "@1") never join
							// prices; the base token index does.
							map[string]any{"name": "PURR/USDC", "tokens": []any{1, 0}, "index": 0},
						},
					},
					[]any{
						map[string]any{"markPx": "3"},
					},
				})
			case "clearinghouseState":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"marginSummary": map[string]any{"accountValue": "500"},
				})
			default:
				http.NotFound(w, r)
			}
		})
		defer srv.Close()

		snap, err := hyperliquid.New("0xabc", srv.URL, hc).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Connection.BrokerageSlug).To(Equal("hyperliquid"))
		// USDC spot ($100) + USDC perp synthetic ($500) → cash 600
		Expect(snap.Holdings[0].Balances[0].Cash).To(Equal(600.0))
		// PURR is the only non-stable position; price comes from markPx
		// (3), not entry notional (20/10 = 2).
		Expect(snap.Holdings).To(HaveLen(1))
		Expect(snap.Holdings[0].Partial).To(BeFalse())
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions[0].Symbol.Symbol).To(Equal("PURR"))
		Expect(snap.Holdings[0].Positions[0].Price).To(Equal(3.0))
	})

	It("falls back to entry notional and marks partial when meta is missing", func() {
		srv, hc := newServer(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Type string `json:"type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch body.Type {
			case "spotMetaAndAssetCtxs":
				http.Error(w, "boom", http.StatusInternalServerError)
			case "clearinghouseState":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"marginSummary": map[string]any{"accountValue": "0"},
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"balances": []any{
						map[string]any{"coin": "PURR", "total": "10", "entryNtl": "20"},
					},
				})
			}
		})
		defer srv.Close()

		snap, err := hyperliquid.New("0xabc", srv.URL, hc).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Partial).To(BeTrue())
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions[0].Price).To(Equal(2.0))
	})

	It("keeps zero-entry-notional tokens as unvalued positions", func() {
		srv, hc := newServer(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Type string `json:"type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch body.Type {
			case "spotMetaAndAssetCtxs":
				_ = json.NewEncoder(w).Encode([]any{
					map[string]any{
						"tokens":   []any{},
						"universe": []any{},
					},
					[]any{},
				})
			case "clearinghouseState":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"marginSummary": map[string]any{"accountValue": "0"},
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"balances": []any{
						map[string]any{"coin": "AIRDROP", "total": "5", "entryNtl": "0"},
					},
				})
			}
		})
		defer srv.Close()

		snap, err := hyperliquid.New("0xabc", srv.URL, hc).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions[0].Symbol.Symbol).To(Equal("AIRDROP"))
		Expect(snap.Holdings[0].Positions[0].Units).To(Equal(5.0))
		Expect(snap.Holdings[0].Positions[0].Price).To(BeZero())
	})

	It("still returns spot data when the perp endpoint fails", func() {
		srv, hc := newServer(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Type string `json:"type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Type == "clearinghouseState" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"balances": []any{
					map[string]any{"coin": "USDC", "total": "42", "entryNtl": "42"},
				},
			})
		})
		defer srv.Close()

		snap, err := hyperliquid.New("0xabc", srv.URL, hc).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Balances[0].Cash).To(Equal(42.0))
	})

	It("fails when both spot and perp calls fail", func() {
		srv, hc := newServer(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		defer srv.Close()

		_, err := hyperliquid.New("0xabc", srv.URL, hc).Fetch(context.Background())
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Hyperliquid marks cache", func() {
	It("serves spot marks from the cache without refetching", func() {
		var metaCalls int
		srv, hc := newServer(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Type string `json:"type"`
			}
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			switch body.Type {
			case "spotClearinghouseState":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"balances": []any{
						map[string]any{"coin": "PURR", "total": "10", "entryNtl": "20"},
					},
				})
			case "spotMetaAndAssetCtxs":
				metaCalls++
				_ = json.NewEncoder(w).Encode([]any{
					map[string]any{
						"tokens": []any{
							map[string]any{"name": "PURR", "index": 1},
						},
						"universe": []any{
							map[string]any{"name": "PURR/USDC", "tokens": []any{1, 0}, "index": 0},
						},
					},
					[]any{
						map[string]any{"markPx": "3"},
					},
				})
			case "clearinghouseState":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"marginSummary": map[string]any{"accountValue": "0"},
				})
			default:
				http.NotFound(w, r)
			}
		})
		defer srv.Close()

		msrv, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		defer msrv.Close()

		svc := prices.NewService(nil, cache.NewCurrentPriceCache(&config.Config{RedisAddr: msrv.Addr()}))
		c := hyperliquid.New("0xabc", srv.URL, hc)
		c.SetPriceService(svc)

		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions[0].Price).To(Equal(3.0))
		Expect(metaCalls).To(Equal(1))

		// Second sync reuses the cached marks: no meta request.
		snap, err = c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions[0].Price).To(Equal(3.0))
		Expect(metaCalls).To(Equal(1))
	})

	It("fetches every sync without a price service", func() {
		var metaCalls int
		srv, hc := newServer(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Type string `json:"type"`
			}
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			if body.Type == "spotMetaAndAssetCtxs" {
				metaCalls++
			}
			http.NotFound(w, r)
		})
		defer srv.Close()

		c := hyperliquid.New("0xabc", srv.URL, hc)
		_, _ = c.Fetch(context.Background())
		_, _ = c.Fetch(context.Background())
		Expect(metaCalls).To(Equal(2))
	})
})
