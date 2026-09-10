package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	binsdk "github.com/adshao/go-binance/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	repomocks "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository/mocks"
	"go.uber.org/mock/gomock"
)

var _ = Describe("Bounded incremental Binance history", func() {
	var server *httptest.Server
	var f *realFetcher
	var c *Client
	var repo *repomocks.MockActivityRepository
	var handler http.HandlerFunc
	BeforeEach(func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer GinkgoRecover(); handler(w, r) }))
		sdk := binsdk.NewClient("k", "s")
		sdk.BaseURL = server.URL
		sdk.HTTPClient = server.Client()
		f = &realFetcher{client: sdk}
		c = New("k", "s", f)
		repo = repomocks.NewMockActivityRepository(gomock.NewController(GinkgoT()))
	})
	AfterEach(func() { server.Close() })
	It("selects only held USDT pairs and pairs with saved trades by default", func() {
		symbols, err := f.historySymbols([]RawBalance{{Asset: "ETH", Free: 1}, {Asset: "USDT", Free: 100}, {Asset: "BTC"}, {Asset: "LDUNKNOWN", Free: 4}}, map[string]float64{"ETHUSDT": 3, "ETHBTC": 1, "BTCUSDT": 4, "ENJBTC": 4}, map[string]int64{"BNBETH": 12})
		Expect(err).NotTo(HaveOccurred())
		Expect(symbols).To(Equal([]string{"BNBETH", "ETHUSDT"}))
		c.ConfigureHistory(repo, []string{" ethbtc ", "ETHBTC", "BTCUSDC"})
		symbols, err = f.historySymbols(nil, nil, map[string]int64{"BNBETH": 12})
		Expect(err).NotTo(HaveOccurred())
		Expect(symbols).To(Equal([]string{"BTCUSDC", "ETHBTC"}))
	})
	It("makes no history or metadata calls when no relevant pairs exist", func() {
		handler = func(w http.ResponseWriter, r *http.Request) { Fail("unexpected call " + r.URL.Path) }
		// Heuristic discovery over a cash-only account cannot prove
		// emptiness: pending, with zero upstream calls.
		rows, err := f.Trades(context.Background(), []RawBalance{{Asset: "USDT", Free: 10}}, map[string]float64{})
		Expect(err).To(MatchError(ContainSubstring("more history remains")))
		Expect(rows).To(BeEmpty())
		_, err = f.Trades(context.Background(), nil, nil)
		Expect(err).To(HaveOccurred())
	})
	It("resumes only saved fills, replays failed writes, and survives a client restart", func() {
		c.ConfigureHistory(repo, []string{"ETHBTC"})
		saved := []brokerage.Activity{}
		repo.EXPECT().List(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(func(_ context.Context, filter repository.ActivityFilter) ([]brokerage.Activity, int, error) {
			Expect(filter.AccountID).To(Equal("binance-spot"))
			end := min(filter.Offset+filter.Limit, len(saved))
			return saved[filter.Offset:end], len(saved), nil
		})
		var fromIDs []string
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v3/account":
				fmt.Fprint(w, `{"balances":[{"asset":"USDT","free":"10","locked":"0"}]}`)
			case "/api/v3/ticker/price":
				fmt.Fprint(w, `[]`)
			case "/api/v3/exchangeInfo":
				Expect(r.URL.Query().Get("symbols")).To(Equal(`["ETHBTC"]`))
				json.NewEncoder(w).Encode(binsdk.ExchangeInfo{Symbols: []binsdk.Symbol{{Symbol: "ETHBTC", BaseAsset: "ETH", QuoteAsset: "BTC"}, {Symbol: "ENJBTC", BaseAsset: "ENJ", QuoteAsset: "BTC"}}})
			case "/api/v3/myTrades":
				Expect(r.URL.Query().Get("symbol")).To(Equal("ETHBTC"))
				fromIDs = append(fromIDs, r.URL.Query().Get("fromId"))
				from, _ := strconv.Atoi(r.URL.Query().Get("fromId"))
				rows := []*binsdk.TradeV3{}
				if from < 2000 {
					for i := 0; i < 1000; i++ {
						rows = append(rows, &binsdk.TradeV3{ID: int64(from + i), Time: 1700000000000, Price: "2", Quantity: "1", Commission: "0", Symbol: "ETHBTC"})
					}
				}
				json.NewEncoder(w).Encode(rows)
			default:
				Fail(r.URL.Path)
			}
		}
		first, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Accounts[0].InitialTxSyncDone).To(BeFalse())
		Expect(first.Activities["binance-spot"]).To(HaveLen(4000))
		retry, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(retry.Activities).To(Equal(first.Activities))
		Expect(fromIDs).To(Equal([]string{"0", "1000", "0", "1000"}))
		saved = first.Activities["binance-spot"]
		c.SnapshotCommitted()
		next, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(next.Accounts[0].InitialTxSyncDone).To(BeTrue())
		Expect(next.Activities).To(BeEmpty())
		Expect(fromIDs[len(fromIDs)-1]).To(Equal("2000"))
		restarted := New("k", "s", &realFetcher{client: f.client})
		restarted.ConfigureHistory(repo, []string{"ETHBTC"})
		_, err = restarted.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(fromIDs[len(fromIDs)-1]).To(Equal("2000"))
	})
	It("bounds history to 20 requests and rotates only after persistence acknowledgement", func() {
		symbols := make([]string, 25)
		for i := range symbols {
			symbols[i] = fmt.Sprintf("A%02dUSDT", i)
		}
		c.ConfigureHistory(nil, symbols)
		var requested []string
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v3/exchangeInfo" {
				var selected []string
				Expect(json.Unmarshal([]byte(r.URL.Query().Get("symbols")), &selected)).To(Succeed())
				Expect(selected).To(HaveLen(20))
				rows := make([]binsdk.Symbol, 0, len(selected))
				for _, symbol := range selected {
					rows = append(rows, binsdk.Symbol{Symbol: symbol, BaseAsset: strings.TrimSuffix(symbol, "USDT"), QuoteAsset: "USDT"})
				}
				json.NewEncoder(w).Encode(binsdk.ExchangeInfo{Symbols: rows})
				return
			}
			requested = append(requested, r.URL.Query().Get("symbol"))
			fmt.Fprint(w, `[]`)
		}
		_, err := f.Trades(context.Background(), nil, nil)
		Expect(err).To(MatchError(errHistoryPending))
		Expect(requested).To(Equal(symbols[:20]))
		requested = nil
		_, err = f.Trades(context.Background(), nil, nil)
		Expect(err).To(MatchError(errHistoryPending))
		Expect(requested).To(Equal(symbols[:20]))
		c.SnapshotCommitted()
		requested = nil
		_, err = f.Trades(context.Background(), nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(requested).To(HaveLen(20))
		Expect(requested[:5]).To(Equal(symbols[20:]))
	})
	It("does not expand missing metadata into a full-exchange lookup", func() {
		c.ConfigureHistory(nil, []string{"MISSING"})
		calls := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			Expect(r.URL.Query().Get("symbols")).To(Equal(`["MISSING"]`))
			fmt.Fprint(w, `{"symbols":[]}`)
		}
		_, err := f.Trades(context.Background(), nil, nil)
		Expect(err).To(HaveOccurred())
		Expect(calls).To(Equal(1))
	})
	It("rejects unreadable or invalid durable cursors instead of starting a full backfill", func() {
		c.ConfigureHistory(repo, []string{"ETHBTC"})
		repo.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, 0, errors.New("database failed"))
		_, err := f.savedCursors(context.Background())
		Expect(err).To(MatchError(ContainSubstring("database failed")))
		for _, id := range []string{"binance:ETHBTC:bad", "binance:ETHBTC:-1", "binance:ETHBTC:9223372036854775807"} {
			repo.EXPECT().List(gomock.Any(), gomock.Any()).Return([]brokerage.Activity{{SourceRecordID: id}}, 1, nil)
			_, err = f.savedCursors(context.Background())
			Expect(err).To(HaveOccurred())
		}
		repo.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, 2, nil)
		_, err = f.savedCursors(context.Background())
		Expect(err).To(MatchError(ContainSubstring("stalled")))
		repo.EXPECT().List(gomock.Any(), gomock.Any()).Return([]brokerage.Activity{{SourceRecordID: "legacy"}, {SourceRecordID: "binance:ETHBTC:3"}, {SourceRecordID: "binance:ETHBTC:8"}}, 3, nil)
		cursors, err := f.savedCursors(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cursors).To(Equal(map[string]int64{"ETHBTC": 9}))
	})
})
