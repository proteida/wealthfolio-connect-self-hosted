package binance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	binsdk "github.com/adshao/go-binance/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rs/zerolog"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
)

var _ = Describe("Spot history", func() {
	var server *httptest.Server
	var fetcher *realFetcher
	var handler http.HandlerFunc
	BeforeEach(func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer GinkgoRecover(); handler(w, r) }))
		sdk := binsdk.NewClient("key", "secret")
		sdk.BaseURL = server.URL
		sdk.HTTPClient = server.Client()
		fetcher = &realFetcher{client: sdk, symbols: []string{"ETHBTC"}}
	})
	AfterEach(func() { server.Close() })
	market := binsdk.Symbol{Symbol: "ETHBTC", BaseAsset: "ETH", QuoteAsset: "BTC", IsSpotTradingAllowed: true}
	fill := func(id int64) *binsdk.TradeV3 {
		return &binsdk.TradeV3{ID: id, Symbol: "ETHBTC", Price: "0.05", Quantity: "2", Commission: "0.01", CommissionAsset: "BNB", Time: 1700000000123, IsBuyer: id%2 == 0}
	}
	It("paginates configured pairs and maps trades idempotently", func() {
		var cursors []string
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v3/exchangeInfo":
				Expect(r.URL.Query().Get("symbols")).To(Equal(`["ETHBTC"]`))
				json.NewEncoder(w).Encode(binsdk.ExchangeInfo{Symbols: []binsdk.Symbol{market, {Symbol: "FUTURE"}}})
			case "/api/v3/account":
				json.NewEncoder(w).Encode(map[string]any{"balances": []any{map[string]string{"asset": "USDT", "free": "15", "locked": "0"}}})
			case "/api/v3/ticker/price":
				fmt.Fprint(w, `[]`)
			case "/api/v3/myTrades":
				q := r.URL.Query()
				Expect(q.Get("symbol")).To(Equal("ETHBTC"))
				Expect(q.Get("limit")).To(Equal("1000"))
				Expect(q.Get("startTime")).To(BeEmpty())
				Expect(q.Get("signature")).NotTo(BeEmpty())
				Expect(r.Header.Get("X-MBX-APIKEY")).To(Equal("key"))
				cursors = append(cursors, q.Get("fromId"))
				if q.Get("fromId") == "0" {
					rows := make([]*binsdk.TradeV3, 1000)
					for i := range rows {
						rows[i] = fill(int64(i))
					}
					json.NewEncoder(w).Encode(rows)
				} else {
					json.NewEncoder(w).Encode([]*binsdk.TradeV3{fill(999), fill(1000)})
				}
			default:
				Fail(r.URL.Path)
			}
		}
		client := New("key", "secret", fetcher)
		first, err := client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		second, err := client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		acts := first.Activities["binance-spot"]
		Expect(acts).To(HaveLen(2002))
		Expect(second.Activities).To(Equal(first.Activities))
		Expect(cursors).To(Equal([]string{"0", "1000", "0", "1000"}))
		Expect(first.Accounts[0].InitialTxSyncDone).To(BeTrue())
		Expect(first.Accounts[0].LastTxSync).NotTo(BeNil())
		Expect(first.Holdings[0].Balances[0].Cash).To(Equal(15.0))
		Expect(acts[0].ID).To(Equal("binance:ETHBTC:0-quote"))
		Expect(acts[0].Type).To(Equal(brokerage.ActivitySell))
		Expect(acts[0].Symbol.Symbol).To(Equal("BTC"))
		Expect(acts[0].Units).To(Equal(0.1))
		Expect(acts[0].Fee).To(BeZero())
		Expect(acts[0].FeeAsset).To(BeEmpty())
		Expect(acts[1].ID).To(Equal("binance:ETHBTC:0-base"))
		Expect(acts[1].Type).To(Equal(brokerage.ActivityBuy))
		Expect(acts[1].Symbol.Symbol).To(Equal("ETH"))
		Expect(acts[1].Price).To(Equal(0.05))
		Expect(acts[1].Units).To(Equal(2.0))
		Expect(acts[1].Amount).To(Equal(0.1))
		Expect(acts[1].Fee).To(Equal(0.01))
		Expect(acts[1].FeeAsset).To(Equal("BNB"))
		// Non-USD quotes keep their denomination instead of fabricated USD.
		Expect(acts[1].Currency.Code).To(Equal("BTC"))
		Expect(acts[1].NeedsReview).To(BeTrue())
		Expect(acts[1].Description).To(ContainSubstring("Quoted in BTC"))
		Expect(acts[0].Currency.Code).To(Equal("BTC"))
		Expect(acts[1].TradeDate).To(Equal(time.UnixMilli(1700000000123).UTC()))
	})
	It("retains balances without completion on a history failure", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v3/account":
				fmt.Fprint(w, `{"balances":[{"asset":"USDT","free":"1","locked":"0"}]}`)
			case "/api/v3/ticker/price":
				fmt.Fprint(w, `[]`)
			case "/api/v3/exchangeInfo":
				json.NewEncoder(w).Encode(binsdk.ExchangeInfo{Symbols: []binsdk.Symbol{market}})
			default:
				w.WriteHeader(429)
				fmt.Fprint(w, `{"code":-1003,"msg":"rate limited"}`)
			}
		}
		var logs bytes.Buffer
		c := New("k", "s", fetcher)
		c.SetLogger(zerolog.New(&logs))
		snap, err := c.Fetch(context.Background())
		Expect(logs.String()).To(ContainSubstring("transaction history incomplete"))
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeFalse())
		Expect(snap.Accounts[0].LastTxSync).To(BeNil())
		Expect(snap.Holdings[0].Balances[0].Cash).To(Equal(1.0))
	})
	It("completes an empty history and rejects broken pagination", func() {
		calls := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v3/exchangeInfo" {
				json.NewEncoder(w).Encode(binsdk.ExchangeInfo{Symbols: []binsdk.Symbol{market}})
				return
			}
			calls++
			if calls == 1 {
				fmt.Fprint(w, `[]`)
				return
			}
			rows := make([]*binsdk.TradeV3, 1000)
			for i := range rows {
				rows[i] = fill(0)
			}
			json.NewEncoder(w).Encode(rows)
		}
		trades, err := fetcher.Trades(context.Background(), nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(trades).To(BeEmpty())
		_, err = fetcher.Trades(context.Background(), nil, nil)
		Expect(err).To(MatchError(ContainSubstring("did not advance")))
	})
	It("keeps a heuristic empty scope incomplete instead of complete", func() {
		unscoped := &realFetcher{client: fetcher.client}
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v3/exchangeInfo" {
				json.NewEncoder(w).Encode(binsdk.ExchangeInfo{Symbols: []binsdk.Symbol{}})
				return
			}
			Fail(r.URL.Path)
		}
		// USDT-only balance: no pairs guessed, no trades. A guessed scope
		// cannot distinguish an empty account from fully-sold assets.
		trades, err := unscoped.Trades(context.Background(),
			[]RawBalance{{Asset: "USDT", Free: 10}}, map[string]float64{})
		Expect(err).To(MatchError(ContainSubstring("more history remains")))
		Expect(trades).To(BeEmpty())
	})

	It("keeps a heuristic scope incomplete even when it finds trades", func() {
		unscoped := &realFetcher{client: fetcher.client}
		usdtMarket := binsdk.Symbol{Symbol: "ETHUSDT", BaseAsset: "ETH", QuoteAsset: "USDT", IsSpotTradingAllowed: true}
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v3/exchangeInfo":
				json.NewEncoder(w).Encode(binsdk.ExchangeInfo{Symbols: []binsdk.Symbol{usdtMarket}})
			case "/api/v3/myTrades":
				json.NewEncoder(w).Encode([]*binsdk.TradeV3{fill(7)})
			default:
				Fail(r.URL.Path)
			}
		}
		// Held ETH yields one guessed ETHUSDT pair with a fill, but a
		// fully-sold BTC position would stay outside the guessed scope.
		trades, err := unscoped.Trades(context.Background(),
			[]RawBalance{{Asset: "ETH", Free: 1}}, map[string]float64{"ETHUSDT": 3000})
		Expect(err).To(MatchError(ContainSubstring("more history remains")))
		Expect(trades).To(HaveLen(1))
	})

	It("honors cancellation before requesting history", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		handler = func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(binsdk.ExchangeInfo{Symbols: []binsdk.Symbol{market}})
		}
		cancel()
		_, err := fetcher.Trades(ctx, nil, nil)
		Expect(err).To(HaveOccurred())
	})
	It("rejects malformed trade fields and namespaces IDs by market", func() {
		for _, value := range []string{"bad", "NaN", "+Inf", "-1"} {
			raw := fill(1)
			raw.Price = value
			_, err := mapTrade(raw, market)
			Expect(err).To(HaveOccurred())
		}
		raw := fill(1)
		raw.Time = 0
		_, err := mapTrade(raw, market)
		Expect(err).To(HaveOccurred())
		_, err = mapTrade(nil, market)
		Expect(err).To(HaveOccurred())
		first, err := mapTrade(fill(1), market)
		Expect(err).NotTo(HaveOccurred())
		other := market
		other.Symbol = "ETHUSDT"
		second, err := mapTrade(fill(1), other)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.ID).NotTo(Equal(second.ID))
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v3/exchangeInfo" {
				json.NewEncoder(w).Encode(binsdk.ExchangeInfo{Symbols: []binsdk.Symbol{market}})
				return
			}
			raw := fill(1)
			raw.Price = "bad"
			json.NewEncoder(w).Encode([]*binsdk.TradeV3{raw})
		}
		_, err = fetcher.Trades(context.Background(), nil, nil)
		Expect(err).To(HaveOccurred())
	})
	It("propagates exchange metadata failures", func() {
		handler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }
		_, err := fetcher.Trades(context.Background(), nil, nil)
		Expect(err).To(HaveOccurred())
	})
	It("fetches SDK balances and prices with numeric parsing", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v3/account" {
				fmt.Fprint(w, `{"balances":[{"asset":"BTC","free":"1","locked":"2"},{"asset":"ETH","free":"0","locked":"0"}]}`)
				return
			}
			fmt.Fprint(w, `[{"symbol":"BTCUSDT","price":"3"},{"symbol":"BAD","price":"bad"}]`)
		}
		balances, err := fetcher.Account(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(balances).To(Equal([]RawBalance{{Asset: "BTC", Free: 1, Locked: 2}}))
		prices, err := fetcher.Prices(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(prices).To(Equal(map[string]float64{"BTCUSDT": 3}))
	})
	It("rejects overflowing trade IDs", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v3/exchangeInfo" {
				json.NewEncoder(w).Encode(binsdk.ExchangeInfo{Symbols: []binsdk.Symbol{market}})
				return
			}
			id, _ := strconv.ParseInt("9223372036854775807", 10, 64)
			json.NewEncoder(w).Encode([]*binsdk.TradeV3{fill(id)})
		}
		_, err := fetcher.Trades(context.Background(), nil, nil)
		Expect(err).To(MatchError(ContainSubstring("overflow")))
	})
})
