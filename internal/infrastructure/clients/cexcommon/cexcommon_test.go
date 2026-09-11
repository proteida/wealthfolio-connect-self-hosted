package cexcommon_test

import (
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/cexcommon"
)

func TestCEXCommon(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "cexcommon Suite")
}

var _ = Describe("Translate", func() {
	It("collapses stablecoins into the cash balance", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Balances: []cexcommon.Balance{
				{Asset: "USDT", Quantity: 100, PriceUSD: 1, USDValue: 100},
				{Asset: "USDC", Quantity: 50, PriceUSD: 1, USDValue: 50},
			},
		})
		Expect(snap.Holdings).To(HaveLen(1))
		Expect(snap.Holdings[0].Balances[0].Cash).To(Equal(150.0))
		Expect(snap.Holdings[0].Positions).To(BeEmpty())
		Expect(snap.Accounts[0].BalanceTotal).To(Equal(150.0))
	})

	It("creates a position per non-stable asset and skips dust under $1", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Balances: []cexcommon.Balance{
				{Asset: "BTC", Quantity: 0.5, PriceUSD: 60000, USDValue: 30000},
				{Asset: "DUST", Quantity: 1000, PriceUSD: 0.0001, USDValue: 0.1},
			},
		})
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions[0].Symbol.Symbol).To(Equal("BTC"))
		Expect(snap.Holdings[0].Positions[0].Units).To(Equal(0.5))
	})

	It("keeps non-USD quote denomination instead of fabricating USD", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Trades: []cexcommon.Trade{
				{ID: "t1", Symbol: "ETH-BTC", BaseAsset: "ETH", QuoteAsset: "BTC",
					Side: "buy", Price: 0.05, Quantity: 1, Fee: 0.001, FeeAsset: "BNB",
					Timestamp: time.Now()},
			},
		})
		acts := snap.Activities["test-spot"]
		Expect(acts).To(HaveLen(2))
		// Quote leg: SELL 0.05 BTC, denominated in BTC at 1:1.
		Expect(acts[0].Symbol.Symbol).To(Equal("BTC"))
		Expect(acts[0].Units).To(Equal(0.05))
		Expect(acts[0].Amount).To(Equal(0.05))
		Expect(acts[0].Currency.Code).To(Equal("BTC"))
		// Base leg: BUY 1 ETH for 0.05 BTC, not a $0.05 purchase.
		Expect(acts[1].Symbol.Symbol).To(Equal("ETH"))
		Expect(acts[1].Price).To(Equal(0.05))
		Expect(acts[1].Currency.Code).To(Equal("BTC"))
		// No FeePrice hook: the BNB-denominated fee normalizes to 0 USD
		// instead of passing through raw.
		Expect(acts[1].Fee).To(Equal(0.0))
		Expect(acts[1].FeeAsset).To(Equal("USD"))
		for _, a := range acts {
			Expect(a.NeedsReview).To(BeTrue())
			Expect(a.Description).To(ContainSubstring("Quoted in BTC"))
		}
	})

	It("resolves symbol-only pairs into their quote denomination", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Trades: []cexcommon.Trade{
				{ID: "t1", Symbol: "ETHBTC", Side: "buy",
					Price: 0.05, Quantity: 1, Timestamp: time.Now()},
			},
		})
		acts := snap.Activities["test-spot"]
		Expect(acts).To(HaveLen(2))
		Expect(acts[1].Symbol.Symbol).To(Equal("ETH"))
		Expect(acts[1].Currency.Code).To(Equal("BTC"))
		Expect(acts[1].NeedsReview).To(BeTrue())
	})

	It("keeps USD denomination and no review flag for stable quotes", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Trades: []cexcommon.Trade{
				{ID: "t1", Symbol: "BTC-USDT", BaseAsset: "BTC", QuoteAsset: "USDT",
					Side: "buy", Price: 60000, Quantity: 0.1, Timestamp: time.Now()},
			},
		})
		acts := snap.Activities["test-spot"]
		Expect(acts).To(HaveLen(2))
		Expect(acts[1].Currency.Code).To(Equal("USD"))
		Expect(acts[1].NeedsReview).To(BeFalse())
	})

	It("surfaces trade notes in the description for review", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Trades: []cexcommon.Trade{
				{ID: "t1", Symbol: "BTC-USDT", BaseAsset: "BTC", QuoteAsset: "USDT",
					Side: "buy", Price: 60000, Quantity: 0.1,
					Note: "Maker rebate 0.5 USDT excluded from fee", Timestamp: time.Now()},
			},
		})
		acts := snap.Activities["test-spot"]
		Expect(acts[1].Description).To(ContainSubstring("Maker rebate"))
		Expect(acts[1].NeedsReview).To(BeTrue())
	})

	It("translates trades into BUY/SELL activities", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Trades: []cexcommon.Trade{
				{ID: "t1", Symbol: "BTC-USDT", Side: "buy", Price: 60000, Quantity: 0.1, Timestamp: time.Now()},
				{ID: "t2", Symbol: "BTC-USDT", Side: "SELL", Price: 65000, Quantity: 0.05, Timestamp: time.Now()},
			},
		})
		acts := snap.Activities["test-spot"]
		Expect(acts).To(HaveLen(4))
		Expect(acts[0].Symbol.Symbol).To(Equal("USDT"))
		Expect(string(acts[0].Type)).To(Equal("SELL"))
		Expect(acts[1].Symbol.Symbol).To(Equal("BTC"))
		Expect(string(acts[1].Type)).To(Equal("BUY"))
		Expect(acts[2].Symbol.Symbol).To(Equal("BTC"))
		Expect(string(acts[2].Type)).To(Equal("SELL"))
		Expect(acts[3].Symbol.Symbol).To(Equal("USDT"))
		Expect(string(acts[3].Type)).To(Equal("BUY"))
	})

	It("normalizes USD₮0 legs to USDT with the source leg first", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Trades: []cexcommon.Trade{
				{ID: "s1", Symbol: "BTCUSD₮0", BaseAsset: "BTC", QuoteAsset: "USD₮0", Side: "sell", Price: 60000, Quantity: 0.1, Timestamp: time.Now()},
			},
		})
		acts := snap.Activities["test-spot"]
		Expect(acts).To(HaveLen(2))
		Expect(acts[0].Symbol.Symbol).To(Equal("BTC"))
		Expect(string(acts[0].Type)).To(Equal("SELL"))
		Expect(acts[1].Symbol.Symbol).To(Equal("USDT"))
		Expect(string(acts[1].Type)).To(Equal("BUY"))
	})

	It("splits dashed pairs on the separator for any quote asset", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Trades: []cexcommon.Trade{
				{ID: "x1", Symbol: "XRP-DOT", Side: "buy", Price: 0.5, Quantity: 100, Timestamp: time.Now()},
			},
		})
		acts := snap.Activities["test-spot"]
		Expect(acts).To(HaveLen(2))
		Expect(acts[0].Symbol.Symbol).To(Equal("DOT"))
		Expect(string(acts[0].Type)).To(Equal("SELL"))
		Expect(acts[1].Symbol.Symbol).To(Equal("XRP"))
		Expect(string(acts[1].Type)).To(Equal("BUY"))
	})

	It("skips concatenated pairs with unlisted quote assets", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Trades: []cexcommon.Trade{
				{ID: "x1", Symbol: "XRPDOT", Side: "buy", Price: 0.5, Quantity: 100, Timestamp: time.Now()},
			},
		})
		Expect(snap.Activities["test-spot"]).To(BeEmpty())
	})

	It("marks an empty successful trade query as synced", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{ActivitiesFetched: true})
		Expect(snap.Activities).To(BeEmpty())
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
		Expect(snap.Accounts[0].LastTxSync).NotTo(BeNil())
	})

	It("uses stable connection IDs derived from the slug", func() {
		snap := cexcommon.Translate("okx", "OKX", cexcommon.Snapshot{})
		Expect(snap.Connection.ID).To(Equal("okx-conn"))
		Expect(snap.Accounts[0].ID).To(Equal("okx-spot"))
	})
})

var _ = Describe("Translate fee normalization", func() {
	tradeActs := func(trades []cexcommon.Trade, feePrice func(string, time.Time) float64) []struct {
		Fee      float64
		FeeAsset string
	} {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Trades:   trades,
			FeePrice: feePrice,
		})
		acts := snap.Activities["test-spot"]
		// Each trade emits a quote leg and a base leg; the normalized fee
		// rides on the base leg only.
		out := make([]struct {
			Fee      float64
			FeeAsset string
		}, 0, len(acts))
		for _, a := range acts {
			if !strings.HasSuffix(a.SourceRecordID, "-base") {
				continue
			}
			out = append(out, struct {
				Fee      float64
				FeeAsset string
			}{Fee: a.Fee, FeeAsset: a.FeeAsset})
		}
		return out
	}

	It("passes USD fees through unchanged", func() {
		got := tradeActs([]cexcommon.Trade{
			{ID: "t1", Symbol: "BTC-USDT", Side: "buy", Price: 60000, Quantity: 0.1, Fee: 5, FeeAsset: "USD", Timestamp: time.Now()},
		}, nil)
		Expect(got[0].Fee).To(Equal(5.0))
		Expect(got[0].FeeAsset).To(Equal("USD"))
	})

	It("passes USD-pegged stablecoin fees through 1:1", func() {
		got := tradeActs([]cexcommon.Trade{
			{ID: "t1", Symbol: "BTC-USDT", Side: "buy", Price: 60000, Quantity: 0.1, Fee: 5, FeeAsset: "USDT", Timestamp: time.Now()},
		}, nil)
		Expect(got[0].Fee).To(Equal(5.0))
		Expect(got[0].FeeAsset).To(Equal("USD"))
	})

	It("reuses the trade price when the fee is paid in the traded asset", func() {
		got := tradeActs([]cexcommon.Trade{
			{ID: "t1", Symbol: "BTC-USDT", Side: "buy", Price: 60000, Quantity: 0.1, Fee: 0.0001, FeeAsset: "BTC", Timestamp: time.Now()},
		}, nil)
		Expect(got[0].Fee).To(BeNumerically("~", 6.0, 1e-9))
		Expect(got[0].FeeAsset).To(Equal("USD"))
	})

	It("parses concatenated pair symbols when matching the fee asset", func() {
		got := tradeActs([]cexcommon.Trade{
			{ID: "t1", Symbol: "BTCUSDT", Side: "buy", Price: 60000, Quantity: 0.1, Fee: 0.0001, FeeAsset: "btc", Timestamp: time.Now()},
		}, nil)
		Expect(got[0].Fee).To(BeNumerically("~", 6.0, 1e-9))
		Expect(got[0].FeeAsset).To(Equal("USD"))
	})

	It("zeroes same-asset fees without a timestamp", func() {
		got := tradeActs([]cexcommon.Trade{
			{ID: "t1", Symbol: "BTC-USDT", Side: "buy", Price: 60000, Quantity: 0.1, Fee: 0.0001, FeeAsset: "BTC"},
		}, nil)
		Expect(got[0].Fee).To(Equal(0.0))
		Expect(got[0].FeeAsset).To(Equal("USD"))
	})

	It("prices differing fee assets via the hook and zeroes them when unavailable", func() {
		hook := func(asset string, at time.Time) float64 {
			if asset == "BNB" {
				return 600
			}
			return 0
		}
		got := tradeActs([]cexcommon.Trade{
			{ID: "t1", Symbol: "BTC-USDT", Side: "buy", Price: 60000, Quantity: 0.1, Fee: 0.01, FeeAsset: "BNB", Timestamp: time.Now()},
			{ID: "t2", Symbol: "BTC-USDT", Side: "buy", Price: 60000, Quantity: 0.1, Fee: 0.01, FeeAsset: "CAKE", Timestamp: time.Now()},
		}, hook)
		Expect(got[0].Fee).To(BeNumerically("~", 6.0, 1e-9))
		Expect(got[0].FeeAsset).To(Equal("USD"))
		Expect(got[1].Fee).To(Equal(0.0))
		Expect(got[1].FeeAsset).To(Equal("USD"))
	})

	It("zeroes differing fee assets without a price hook", func() {
		got := tradeActs([]cexcommon.Trade{
			{ID: "t1", Symbol: "BTC-USDT", Side: "buy", Price: 60000, Quantity: 0.1, Fee: 0.01, FeeAsset: "BNB", Timestamp: time.Now()},
		}, nil)
		Expect(got[0].Fee).To(Equal(0.0))
		Expect(got[0].FeeAsset).To(Equal("USD"))
	})

	It("preserves the previous assume-USD behaviour for unknown denominations", func() {
		got := tradeActs([]cexcommon.Trade{
			{ID: "t1", Symbol: "BTC-USDT", Side: "buy", Price: 60000, Quantity: 0.1, Fee: 5, Timestamp: time.Now()},
		}, nil)
		Expect(got[0].Fee).To(Equal(5.0))
		Expect(got[0].FeeAsset).To(Equal("USD"))
	})
})
var _ = Describe("IsStablecoin", func() {
	It("recognizes common USD-pegged coins regardless of case", func() {
		Expect(cexcommon.IsStablecoin("USDT")).To(BeTrue())
		Expect(cexcommon.IsStablecoin("usdc")).To(BeTrue())
		Expect(cexcommon.IsStablecoin("DAI")).To(BeTrue())
		Expect(cexcommon.IsStablecoin("BTC")).To(BeFalse())
		Expect(cexcommon.IsStablecoin("USD₮0")).To(BeTrue())
	})
})

var _ = Describe("NormalizeAsset", func() {
	It("maps USD₮0 to USDT", func() {
		Expect(cexcommon.NormalizeAsset("USD₮0")).To(Equal("USDT"))
		Expect(cexcommon.NormalizeAsset("USD₮")).To(Equal("USDT"))
		Expect(cexcommon.NormalizeAsset("usdt")).To(Equal("USDT"))
	})

	It("maps Affluent vault shares to one position", func() {
		Expect(cexcommon.NormalizeAsset("affUSDe")).To(Equal("AFFSENTORAENT"))
	})
})
