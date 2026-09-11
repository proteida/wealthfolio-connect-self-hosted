package okx_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rs/zerolog"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/okx"
)

func TestOKX(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OKX Client Suite")
}

func newServer(handler http.HandlerFunc) (*httptest.Server, *http.Client) {
	srv := httptest.NewServer(handler)
	return srv, srv.Client()
}

var _ = Describe("CEXClient", func() {
	It("returns slug okx", func() {
		Expect(okx.NewCEX(okx.Credentials{}, "", nil).ID()).To(Equal("okx"))
	})

	It("fails when credentials are missing", func() {
		_, err := okx.NewCEX(okx.Credentials{}, "http://x", nil).Fetch(context.Background())
		Expect(err).To(HaveOccurred())
	})

	It("signs the request and translates a balance payload", func() {
		var capturedHeaders http.Header
		srv, hc := newServer(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v5/account/balance":
				capturedHeaders = r.Header
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": "0",
					"data": []any{
						map[string]any{
							"details": []any{
								map[string]any{"ccy": "BTC", "cashBal": "0.5", "eqUsd": "30000"},
								map[string]any{"ccy": "USDT", "cashBal": "200", "eqUsd": "200"},
							},
						},
					},
				})
			case "/api/v5/trade/fills-history":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": "0",
					"data": []any{},
				})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		defer srv.Close()

		c := okx.NewCEX(okx.Credentials{
			APIKey: "k", Secret: "s", Passphrase: "p",
		}, srv.URL, hc)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(capturedHeaders.Get("OK-ACCESS-KEY")).To(Equal("k"))
		Expect(capturedHeaders.Get("OK-ACCESS-PASSPHRASE")).To(Equal("p"))
		Expect(capturedHeaders.Get("OK-ACCESS-SIGN")).NotTo(BeEmpty())
		Expect(capturedHeaders.Get("OK-ACCESS-TIMESTAMP")).NotTo(BeEmpty())
		Expect(snap.Connection.BrokerageSlug).To(Equal("okx"))
		Expect(snap.Holdings[0].Balances[0].Cash).To(Equal(200.0)) // USDT
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions[0].Symbol.Symbol).To(Equal("BTC"))
	})

	It("surfaces non-zero API error codes", func() {
		srv, hc := newServer(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": "50100", "msg": "API key invalid",
			})
		})
		defer srv.Close()
		_, err := okx.NewCEX(okx.Credentials{
			APIKey: "k", Secret: "s", Passphrase: "p",
		}, srv.URL, hc).Fetch(context.Background())
		Expect(err).To(MatchError(ContainSubstring("50100")))
	})

	It("falls back to the order size when fill size is empty", func() {
		srv, hc := newServer(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v5/account/balance" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": "0",
					"data": []any{map[string]any{"details": []any{
						map[string]any{"ccy": "USDT", "cashBal": "10", "eqUsd": "10"},
					}}},
				})
				return
			}
			if r.URL.Query().Get("after") != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": "0",
				"data": []any{map[string]any{
					"billId": "fill-1", "instId": "BTC-USDT", "side": "buy",
					"fillSz": "", "sz": "0.25", "fillPx": "100",
					"fee": "0", "feeCcy": "USDT", "ts": "1700000000000",
				}},
			})
		})
		defer srv.Close()

		c := okx.NewCEX(okx.Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, srv.URL, hc)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Activities["okx-spot"]).To(HaveLen(2))
		Expect(snap.Activities["okx-spot"][1].Units).To(Equal(0.25))
	})

	It("records rebates as notes instead of charged fees", func() {
		srv, hc := newServer(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v5/account/balance" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": "0",
					"data": []any{map[string]any{"details": []any{
						map[string]any{"ccy": "USDT", "cashBal": "10", "eqUsd": "10"},
					}}},
				})
				return
			}
			if r.URL.Query().Get("after") != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": "0",
				"data": []any{
					map[string]any{
						"billId": "fill-charge", "instId": "BTC-USDT", "side": "buy",
						"fillSz": "0.1", "fillPx": "100",
						"fee": "-0.01", "feeCcy": "USDT", "ts": "1700000000000",
					},
					map[string]any{
						"billId": "fill-rebate", "instId": "BTC-USDT", "side": "buy",
						"fillSz": "0.1", "fillPx": "100",
						"fee": "0.005", "feeCcy": "USDT", "ts": "1700000001000",
					},
				},
			})
		})
		defer srv.Close()

		c := okx.NewCEX(okx.Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, srv.URL, hc)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		acts := snap.Activities["okx-spot"]
		Expect(acts).To(HaveLen(4))
		Expect(acts[1].Fee).To(Equal(0.01))
		Expect(acts[1].FeeAsset).To(Equal("USD"))
		// Positive fee values are rebates: never recorded as a charged fee.
		Expect(acts[3].Fee).To(BeZero())
		Expect(acts[3].Description).To(ContainSubstring("rebate"))
		Expect(acts[3].NeedsReview).To(BeTrue())
	})
})

var _ = Describe("Web3Client", func() {
	It("returns slug okx_web3", func() {
		Expect(okx.NewWeb3(okx.Credentials{}, nil, "", nil).ID()).To(Equal("okx_web3"))
	})

	It("fails when credentials are missing", func() {
		_, err := okx.NewWeb3(okx.Credentials{}, nil, "http://x", nil).Fetch(context.Background())
		Expect(err).To(HaveOccurred())
	})

	It("returns an empty snapshot when no wallets are configured", func() {
		c := okx.NewWeb3(okx.Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, nil, "http://x", nil)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(BeEmpty())
	})

	It("translates token balances per wallet, dropping risk tokens and dust", func() {
		srv, hc := newServer(func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Query().Get("address")).To(Equal("0xabc"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": "0",
				"data": []any{
					map[string]any{
						"tokenAssets": []any{
							map[string]any{
								"symbol": "ETH", "tokenAddress": "", "chainIndex": "1",
								"balance": "1.5", "tokenPrice": "3000", "isRiskToken": false,
							},
							map[string]any{
								"symbol": "USDC", "chainIndex": "1",
								"balance": "1000", "tokenPrice": "1", "isRiskToken": false,
							},
							map[string]any{
								"symbol": "SCAM", "chainIndex": "1",
								"balance": "1000000", "tokenPrice": "0.001", "isRiskToken": true,
							},
							map[string]any{
								"symbol": "DUST", "chainIndex": "1",
								"balance": "0.0001", "tokenPrice": "0.5", "isRiskToken": false,
							},
						},
					},
				},
			})
		})
		defer srv.Close()

		c := okx.NewWeb3(okx.Credentials{APIKey: "k", Secret: "s", Passphrase: "p"},
			[]okx.Wallet{{Address: "0xabc", Chains: []string{"1"}}}, srv.URL, hc)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Holdings[0].Balances[0].Cash).To(Equal(1000.0)) // USDC stable cash
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))           // ETH only; SCAM filtered, DUST dropped
		Expect(snap.Holdings[0].Positions[0].Symbol.Symbol).To(Equal("ETH"))
		Expect(snap.Holdings[0].Positions[0].Symbol.Exchange.Code).To(Equal("ETHEREUM"))
	})

	It("does not request TON wallets", func() {
		requests := 0
		srv, hc := newServer(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []any{}})
		})
		defer srv.Close()

		c := okx.NewWeb3(okx.Credentials{APIKey: "k", Secret: "s", Passphrase: "p"},
			[]okx.Wallet{{Address: "UQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}, srv.URL, hc)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(requests).To(Equal(0))
		Expect(snap.Accounts).To(BeEmpty())
	})
})

var _ = Describe("TranslateWeb3", func() {
	It("returns an empty connection-only snapshot for nil input", func() {
		snap := okx.TranslateWeb3(nil)
		Expect(snap.Connection.BrokerageSlug).To(Equal("okx_web3"))
		Expect(snap.Accounts).To(BeEmpty())
	})

	It("SetLogger does not panic and is safe to call repeatedly", func() {
		c := okx.NewWeb3(okx.Credentials{}, nil, "", nil)
		c.SetLogger(zerolog.New(io.Discard))
		c.SetLogger(zerolog.Nop())
	})
})
