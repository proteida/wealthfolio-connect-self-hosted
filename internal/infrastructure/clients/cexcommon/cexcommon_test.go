package cexcommon_test

import (
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

	It("translates trades into BUY/SELL activities", func() {
		snap := cexcommon.Translate("test", "Test", cexcommon.Snapshot{
			Trades: []cexcommon.Trade{
				{ID: "t1", Symbol: "BTC-USDT", Side: "buy", Price: 60000, Quantity: 0.1, Timestamp: time.Now()},
				{ID: "t2", Symbol: "BTC-USDT", Side: "SELL", Price: 65000, Quantity: 0.05, Timestamp: time.Now()},
			},
		})
		acts := snap.Activities["test-spot"]
		Expect(acts).To(HaveLen(2))
		Expect(string(acts[0].Type)).To(Equal("BUY"))
		Expect(string(acts[1].Type)).To(Equal("SELL"))
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
		out := make([]struct {
			Fee      float64
			FeeAsset string
		}, len(acts))
		for i, a := range acts {
			out[i].Fee = a.Fee
			out[i].FeeAsset = a.FeeAsset
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
	})
})
