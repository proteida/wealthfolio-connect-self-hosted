package ton

import (
	"context"
	"encoding/json"
	"math/big"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
)

func mustDetails(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	return raw
}

func tonTransfer(source, dest, value string) tonAction {
	return tonAction{ActionID: "a-" + source + dest, TraceID: "tr",
		Start: 1700000000, End: 1700000000, Success: true, Type: "ton_transfer",
		Details: mustDetails(map[string]any{
			"source": source, "destination": dest, "value": value,
		})}
}

func jettonTransfer(asset, sender, receiver, amount string) tonAction {
	return tonAction{ActionID: "a-" + asset + sender + receiver, TraceID: "tr",
		Start: 1700000000, End: 1700000000, Success: true, Type: "jetton_transfer",
		Details: mustDetails(map[string]any{
			"asset": asset, "sender": sender, "receiver": receiver, "amount": amount,
		})}
}

var _ = Describe("Action transfer mapping", func() {
	forms := []string{testWallet, testRaw}
	masters := map[string]tokenMeta{
		testMaster: {Address: testMaster, Symbol: "tsTON", Decimals: 9, DecimalsKnown: true},
	}

	It("maps every token movement to transfers", func() {
		in, err := transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil,
			tonTransfer("0:STRANGER", testRaw, "2000000000"))
		Expect(err).NotTo(HaveOccurred())
		Expect(in.Type).To(Equal(brokerage.ActivityTransferIn))
		Expect(in.RawType).To(Equal("TON_TRANSFER_IN"))
		Expect(in.Units).To(Equal(2.0))
		Expect(in.Price).To(Equal(2.0))
		Expect(in.Amount).To(Equal(4.0))
		Expect(in.Description).To(ContainSubstring("cost basis"))
		out, err := transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil,
			tonTransfer(testRaw, "0:STRANGER", "1000000000"))
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Type).To(Equal(brokerage.ActivityTransferOut))
		Expect(out.RawType).To(Equal("TON_TRANSFER_OUT"))
	})

	It("keeps exact units on transfers of every asset", func() {
		act, err := transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil,
			jettonTransfer(testMaster, testRaw, "0:OTHER", "250500000000"))
		Expect(err).NotTo(HaveOccurred())
		Expect(act.Type).To(Equal(brokerage.ActivityTransferOut))
		Expect(act.Units).To(Equal(250.5))
		Expect(act.Amount).To(Equal(501.0))
		Expect(act.Price).To(Equal(2.0))
		// Same input twice → same row, so re-syncs update in place.
		again, err := transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil,
			jettonTransfer(testMaster, testRaw, "0:OTHER", "250500000000"))
		Expect(err).NotTo(HaveOccurred())
		Expect(again.ID).To(Equal(act.ID))
		// Sub-cent transfers survive for every asset, stablecoins included.
		dust, err := transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil,
			jettonTransfer(testMaster, testRaw, "0:OTHER", "1000000"))
		Expect(err).NotTo(HaveOccurred())
		Expect(dust.Units).To(Equal(0.001))
	})

	It("flags only untracked counterparties as external", func() {
		tracked := map[string]bool{testRaw: true, "0:FRIEND": true}
		in, err := transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, tracked,
			tonTransfer("0:FRIEND", testRaw, "1000000000"))
		Expect(err).NotTo(HaveOccurred())
		Expect(in.Type).To(Equal(brokerage.ActivityTransferIn))
		Expect(in.IsExternal).To(BeFalse())
		out, err := transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, tracked,
			tonTransfer(testRaw, "0:STRANGER", "1000000000"))
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Type).To(Equal(brokerage.ActivityTransferOut))
		Expect(out.IsExternal).To(BeTrue())
	})

	It("maps tracked-counterparty flows to transfers", func() {
		act, err := transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil,
			tonTransfer(testRaw, "0:FRIEND", "1000000000"))
		Expect(err).NotTo(HaveOccurred())
		Expect(act.Type).To(Equal(brokerage.ActivityTransferOut))
	})

	It("maps Jetton legs with metadata and skips dust and self-moves", func() {
		act, err := transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil,
			jettonTransfer(testMaster, "0:OTHER", testRaw, "5000000000"))
		Expect(err).NotTo(HaveOccurred())
		Expect(act.Type).To(Equal(brokerage.ActivityTransferIn))
		Expect(act.RawType).To(Equal("JETTON_TRANSFER_IN"))
		Expect(act.Symbol.Symbol).To(Equal("TSTON"))
		Expect(act.Units).To(Equal(5.0))
		none, err := transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil,
			tonTransfer("0:X", testRaw, "1"))
		Expect(err).NotTo(HaveOccurred())
		Expect(none).To(BeNil())
		none, err = transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil,
			jettonTransfer(testMaster, testRaw, testRaw, "1"))
		Expect(err).NotTo(HaveOccurred())
		Expect(none).To(BeNil())
	})

	It("fails on malformed details and unresolved Jettons", func() {
		bad := tonTransfer("0:X", testRaw, "1")
		bad.Details = json.RawMessage(`broken`)
		_, err := transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil, bad)
		Expect(err).To(HaveOccurred())
		_, err = transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil,
			jettonTransfer("0:UNKNOWN", "0:X", testRaw, "1"))
		Expect(err).To(HaveOccurred())
		bogus := tonTransfer("0:X", testRaw, "bogus")
		_, err = transferActivity(context.Background(), testRaw, forms, masters, stubValue{unit: 2, unitOK: true}, nil, nil, nil, bogus)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Action grouping", func() {
	mk := func(id, trace, typ string, success bool) tonAction {
		return tonAction{ActionID: id, TraceID: trace, Success: success, Type: typ,
			Start: 1, End: 1, Details: mustDetails(map[string]any{})}
	}

	It("keeps successful actions grouped by trace in order", func() {
		groups := groupActionsByTrace([]tonAction{
			mk("a1", "t1", "ton_transfer", true),
			mk("a2", "t1", "jetton_transfer", true),
			mk("bad", "t1", "ton_transfer", false),
			mk("a3", "", "ton_transfer", true),
			mk("a4", "t2", "call_contract", true),
		})
		Expect(groups).To(HaveLen(3))
		Expect(groups[0]).To(HaveLen(2))
		Expect(groups[1]).To(HaveLen(1))
		Expect(groups[2]).To(HaveLen(1))
	})
})

var _ = Describe("Swap and stake mapping", func() {
	forms := []string{testWallet, testRaw}
	masters := map[string]tokenMeta{
		testMaster: {Address: testMaster, Symbol: "tsTON", Decimals: 9, DecimalsKnown: true},
		usdMaster:  {Address: usdMaster, Symbol: "USDT", Decimals: 6, DecimalsKnown: true},
	}
	price := stubValue{v: 100, ok: true}

	swap := func() tonAction {
		return tonAction{ActionID: "sw", TraceID: "tsw", Start: 1700000000,
			End: 1700000000, Success: true, Type: "jetton_swap",
			Details: mustDetails(map[string]any{
				"dex": "stonfi", "sender": testRaw,
				"asset_in":  usdMaster,
				"asset_out": testMaster,
				"dex_incoming_transfer": map[string]any{
					"asset": usdMaster, "source": testRaw,
					"destination": "0:POOL", "amount": "100000000",
				},
				"dex_outgoing_transfer": map[string]any{
					"asset": testMaster, "source": "0:POOL",
					"destination": testRaw, "amount": "5000000000",
				},
			})}
	}

	It("routes swaps through USD on both legs", func() {
		legs, _, err := swapActivities(context.Background(), testRaw, forms, masters, price, swap(), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(legs).To(HaveLen(2))
		Expect(legs[0].Type).To(Equal(brokerage.ActivitySell))
		Expect(legs[0].Symbol.Symbol).To(Equal("USDT"))
		Expect(legs[0].Units).To(Equal(100.0))
		Expect(legs[0].Amount).To(Equal(100.0))
		Expect(legs[0].Price).To(Equal(1.0))
		Expect(legs[1].Type).To(Equal(brokerage.ActivityBuy))
		Expect(legs[1].Symbol.Symbol).To(Equal("TSTON"))
		Expect(legs[1].Units).To(Equal(5.0))
		Expect(legs[1].Amount).To(Equal(100.0))
		// Buy price nudged down so quantity × price can never exceed the
		// sell total once the app canonicalizes amounts in decimal.
		Expect(legs[1].Price).To(BeNumerically("<", 20.0))
		Expect(legs[1].Price).To(BeNumerically("~", 20.0, 1e-8))
		Expect(legs[1].TradeDate.Unix()).To(Equal(legs[0].TradeDate.Unix() + 1))
		Expect(legs[0].Description).To(ContainSubstring("100 USDT"))
		Expect(legs[0].Description).To(ContainSubstring("stonfi"))
	})

	It("leaves swaps unvalued without a priced side", func() {
		legs, _, err := swapActivities(context.Background(), testRaw, forms, masters, stubValue{ok: false}, swap(), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(legs[0].Amount).To(BeZero())
		Expect(legs[0].Price).To(BeZero())
		Expect(legs[1].Amount).To(BeZero())
	})

	It("orders app-recomputed pair totals buy below sell", func() {
		// The app canonicalizes trade amounts as quantity × unit_price in
		// exact decimal arithmetic. Emulate that with big rationals over
		// shortest-repr strings (what Decimal::from_f64 sees) and prove the
		// stored buy total stays strictly below the stored sell total while
		// both stay within a cent of the shared USD value.
		rat := func(f float64) *big.Rat {
			r, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
			Expect(ok).To(BeTrue())
			return r
		}
		check := func(inQty, outQty, usd float64) {
			sellPrice, buyPrice := biasedPrices(inQty, outQty, usd, true)
			sellStored := new(big.Rat).Mul(rat(round8(inQty)), rat(sellPrice))
			buyStored := new(big.Rat).Mul(rat(round8(outQty)), rat(buyPrice))
			total := rat(usd)
			// The cash net of a pair can never dip below zero: an exact
			// $1 leg contributes exactly, a nudged leg stays strictly on
			// its side of the total.
			Expect(sellStored.Cmp(buyStored)).To(BeNumerically(">=", 0))
			Expect(buyStored.Cmp(total)).To(BeNumerically("<=", 0))
			Expect(sellStored.Cmp(total)).To(BeNumerically(">=", 0))
			five := big.NewRat(5, 1000) // half-cent canonicalization tolerance
			gap := new(big.Rat).Sub(total, buyStored)
			gap.Abs(gap)
			Expect(gap.Cmp(five)).To(Equal(-1))
			gap.Sub(sellStored, total)
			gap.Abs(gap)
			Expect(gap.Cmp(five)).To(Equal(-1))
		}
		check(9987.235406, 9773.54689423, 9987.235406) // staking1
		check(21, 20.55117042, 21)                     // staking2
		check(19.5729299, 19.970967, 19.970967)        // unstaking3
		check(100, 0.417, 100)                         // small-quantity buy
		check(0.000001, 0.0000025, 0.000001)           // dust quantities
		check(1e6, 5e5, 1e6)                           // whale quantities
		check(37.5, 12.25, 41.8)                       // feed-priced sell
		// Exact $1 legs stay exact with no nudge.
		sell, buy := biasedPrices(100, 100, 100, true)
		Expect(sell).To(Equal(1.0))
		Expect(buy).To(Equal(1.0))
		sell, buy = biasedPrices(0, 0, 0, false)
		Expect(sell).To(BeZero())
		Expect(buy).To(BeZero())
	})

	It("skips swaps the wallet did not initiate", func() {
		other := swap()
		var d swapDetails
		Expect(json.Unmarshal(other.Details, &d)).To(Succeed())
		d.Sender = "0:STRANGER"
		d.Incoming.Source = "0:STRANGER"
		raw, err := json.Marshal(d)
		Expect(err).NotTo(HaveOccurred())
		other.Details = raw
		legs, _, err := swapActivities(context.Background(), testRaw, forms, masters, price, other, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(legs).To(BeEmpty())
	})

	It("consumes only the swap's own primitive legs", func() {
		sw := swap()
		incoming := jettonTransfer(usdMaster, testRaw, "0:POOL", "100000000")
		outgoing := jettonTransfer(testMaster, "0:POOL", testRaw, "5000000000")
		stranger := jettonTransfer(usdMaster, testRaw, "0:FRIEND", "7000000")
		group := []tonAction{sw, incoming, outgoing, stranger}
		legs, consumed, err := swapActivities(context.Background(), testRaw, forms, masters, price, sw, group)
		Expect(err).NotTo(HaveOccurred())
		Expect(legs).To(HaveLen(2))
		Expect(consumed).To(HaveLen(2))
		Expect(consumed[incoming.ActionID]).To(BeTrue())
		Expect(consumed[outgoing.ActionID]).To(BeTrue())
		Expect(consumed[stranger.ActionID]).To(BeFalse())
	})

	It("keeps a recipient transfer when the swap belongs to another wallet", func() {
		// Another wallet's swap pays us directly: the swap emits no legs
		// for us, and our inbound transfer must survive classification
		// instead of being suppressed with the foreign swap.
		other := swap()
		var d swapDetails
		Expect(json.Unmarshal(other.Details, &d)).To(Succeed())
		d.Sender = "0:STRANGER"
		d.Incoming.Source = "0:STRANGER"
		d.Incoming.Destination = "0:STRANGER"
		raw, err := json.Marshal(d)
		Expect(err).NotTo(HaveOccurred())
		other.Details = raw
		receipt := jettonTransfer(testMaster, "0:POOL", testRaw, "5000000000")
		legs, err := classifyActionGroup(context.Background(), testRaw, forms, masters,
			stubValue{unit: 2, unitOK: true}, make(map[string]VaultSpend),
			[]tonAction{other, receipt}, nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(legs).To(HaveLen(1))
		Expect(legs[0].Units).To(Equal(5.0))
	})

	It("keeps change accompanying a mapped swap", func() {
		sw := swap()
		incoming := jettonTransfer(usdMaster, testRaw, "0:POOL", "100000000")
		outgoing := jettonTransfer(testMaster, "0:POOL", testRaw, "5000000000")
		change := jettonTransfer(usdMaster, "0:POOL", testRaw, "1000000")
		legs, err := classifyActionGroup(context.Background(), testRaw, forms, masters,
			stubValue{unit: 2, unitOK: true}, make(map[string]VaultSpend),
			[]tonAction{sw, incoming, outgoing, change}, nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		// 2 swap legs + the surviving change transfer.
		Expect(legs).To(HaveLen(3))
	})

	It("fails swaps with unresolvable assets", func() {
		bad := swap()
		var d swapDetails
		Expect(json.Unmarshal(bad.Details, &d)).To(Succeed())
		d.AssetIn = "0:UNKNOWN"
		raw, err := json.Marshal(d)
		Expect(err).NotTo(HaveOccurred())
		bad.Details = raw
		_, _, err = swapActivities(context.Background(), testRaw, forms, map[string]tokenMeta{}, price, bad, nil)
		Expect(err).To(HaveOccurred())
	})

	It("renders stakes as sell-plus-buy through USD", func() {
		a := tonAction{ActionID: "st", TraceID: "tst", Start: 1700000000,
			End: 1700000000, Success: true, Type: "stake_deposit",
			Details: mustDetails(map[string]any{
				"provider": "liquid_staking", "stake_holder": testRaw, "pool": "0:POOL",
				"amount": "10000000000", "tokens_minted": "9000000000", "asset": testMaster,
			})}
		legs, _, err := stakeActivities(context.Background(), testRaw, forms, masters, price, a, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(legs).To(HaveLen(2))
		Expect(legs[0].Type).To(Equal(brokerage.ActivitySell))
		Expect(legs[0].RawType).To(Equal("STAKE_SELL"))
		Expect(legs[0].Units).To(Equal(10.0))
		Expect(legs[0].Amount).To(Equal(100.0))
		Expect(legs[1].Type).To(Equal(brokerage.ActivityBuy))
		Expect(legs[1].RawType).To(Equal("STAKE_BUY"))
		Expect(legs[1].Symbol.Symbol).To(Equal("TSTON"))
		Expect(legs[1].Units).To(Equal(9.0))
		Expect(legs[1].Amount).To(Equal(100.0))
	})
})

var _ = Describe("TON translation", func() {
	It("keeps unpriced positions and completes only fetched histories", func() {
		snap := TranslateTON([]TonWalletData{{
			Address: testWallet, Canonical: testRaw, Friendly: testWallet,
			Tokens: []TonToken{
				{Symbol: "TON", Quantity: 0.4, Decimals: 9},
				{Symbol: "tsTON", Name: "Tonstakers TON", TokenAddr: testMaster, Quantity: 214.4, Decimals: 9},
			},
			Activities:        []brokerage.Activity{{ID: "x"}},
			ActivitiesFetched: true,
		}})
		Expect(snap.Connection.BrokerageSlug).To(Equal("ton"))
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Accounts[0].ID).To(Equal("ton-" + testRaw))
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
		Expect(snap.Accounts[0].LastTxSync).NotTo(BeNil())
		Expect(snap.Holdings[0].Positions).To(HaveLen(2))
		Expect(snap.Holdings[0].Positions[0].Symbol.Symbol).To(Equal("TON"))
		Expect(snap.Holdings[0].Positions[0].Price).To(BeZero())
	})

	It("omits empty activity buckets and incomplete flags", func() {
		snap := TranslateTON([]TonWalletData{{
			Address: testWallet, Tokens: []TonToken{{Symbol: "TON", Quantity: 1, Decimals: 9}},
		}})
		Expect(snap.Activities).To(BeEmpty())
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeFalse())
		Expect(snap.Accounts[0].LastTxSync).To(BeNil())
	})

	It("values stablecoin holdings as cash instead of omitting them", func() {
		snap := TranslateTON([]TonWalletData{{
			Address: testWallet,
			Tokens: []TonToken{
				{Symbol: "USDT", Quantity: 100, Decimals: 6},
				{Symbol: "TON", Quantity: 1, Decimals: 9},
			},
		}})
		Expect(snap.Holdings[0].Balances[0].Cash).To(Equal(100.0))
		Expect(snap.Accounts[0].BalanceTotal).To(Equal(100.0))
	})

	It("emits a synthetic buy for attributed vault shares", func() {
		snap := TranslateTON([]TonWalletData{{
			Address: testWallet, Canonical: testRaw,
			Tokens: []TonToken{{
				Symbol: "VAULT", TokenAddr: "0:VAULT", Quantity: 861.24, Decimals: 9,
			}},
			MasterOutflows:     map[string]VaultSpend{"0:VAULT": {Amount: 876.55, Time: 1700000000}},
			AllowVaultFallback: true,
		}})
		acts := snap.Activities["ton-"+testRaw]
		Expect(acts).To(HaveLen(1))
		Expect(acts[0].Type).To(Equal(brokerage.ActivityBuy))
		Expect(acts[0].Symbol.Symbol).To(Equal("VAULT"))
		Expect(acts[0].Units).To(Equal(861.24))
		Expect(acts[0].Amount).To(Equal(876.55))
		Expect(snap.Holdings[0].Positions[0].AveragePurchasePrice).To(BeNumerically("~", 1.01778, 1e-4))
	})

	It("suppresses synthetic buys without the fallback flag", func() {
		snap := TranslateTON([]TonWalletData{{
			Address: testWallet, Canonical: testRaw,
			Tokens: []TonToken{{
				Symbol: "VAULT", TokenAddr: "0:VAULT", Quantity: 861.24, Decimals: 9,
			}},
			MasterOutflows: map[string]VaultSpend{"0:VAULT": {Amount: 876.55, Time: 1700000000}},
		}})
		Expect(snap.Activities).To(BeEmpty())
		Expect(snap.Holdings[0].Positions[0].AveragePurchasePrice).To(BeZero())
	})

	It("averages inbound cost bases onto positions", func() {
		mk := func(typ brokerage.ActivityType, symbol string, units, amount float64) brokerage.Activity {
			return brokerage.Activity{ID: string(typ) + symbol, Type: typ, Units: units, Amount: amount,
				Symbol: &brokerage.Symbol{Symbol: symbol}}
		}
		got := averageBasis([]brokerage.Activity{
			mk(brokerage.ActivityBuy, "TSTON", 20, 50),
			mk(brokerage.ActivityBuy, "TSTON", 10, 40),
			mk(brokerage.ActivityDeposit, "TON", 2, 4),
			mk(brokerage.ActivitySell, "TSTON", 5, 100),   // partial sells keep the per-unit average
			mk(brokerage.ActivityDeposit, "SPYX", 1, 0),   // unvalued legs skipped
			mk(brokerage.ActivityTransferIn, "TON", 0, 0), // zero legs skipped
			{ID: "nosym", Type: brokerage.ActivityBuy, Units: 1, Amount: 1},
		})
		Expect(got["TSTON"]).To(Equal(3.0))
		Expect(got["TON"]).To(Equal(2.0))
		Expect(got).NotTo(HaveKey("SPYX"))
		snap := TranslateTON([]TonWalletData{{
			Address: testWallet,
			Tokens:  []TonToken{{Symbol: "TSTON", Quantity: 25, Decimals: 9}},
			Activities: []brokerage.Activity{
				mk(brokerage.ActivityBuy, "TSTON", 20, 50),
				mk(brokerage.ActivityBuy, "TSTON", 10, 40),
			},
			ActivitiesFetched: true,
		}})
		Expect(snap.Holdings[0].Positions[0].AveragePurchasePrice).To(Equal(3.0))
	})

	It("relieves the basis on disposal so a rebuy reprices", func() {
		mk := func(typ brokerage.ActivityType, symbol string, units, amount float64, ts int64) brokerage.Activity {
			return brokerage.Activity{ID: string(typ) + symbol, Type: typ, Units: units, Amount: amount,
				TradeDate: time.Unix(ts, 0).UTC(), Symbol: &brokerage.Symbol{Symbol: symbol}}
		}
		// Buy 10 @ $1, sell all, buy 10 @ $3 → $3, not lifetime $2.
		got := averageBasis([]brokerage.Activity{
			mk(brokerage.ActivityBuy, "TON", 10, 10, 1000),
			mk(brokerage.ActivitySell, "TON", 10, 25, 2000),
			mk(brokerage.ActivityBuy, "TON", 10, 30, 3000),
		})
		Expect(got["TON"]).To(Equal(3.0))
		// Over-selling with no tracked lots floors at zero, never negative.
		got = averageBasis([]brokerage.Activity{
			mk(brokerage.ActivitySell, "TON", 5, 50, 1000),
		})
		Expect(got).NotTo(HaveKey("TON"))
	})
})

var _ = Describe("Helpers", func() {
	It("scales amounts and parses metadata strictly", func() {
		qty, err := scaledAmount("214365209487", 9)
		Expect(err).NotTo(HaveOccurred())
		Expect(qty).To(BeNumerically("~", 214.365209487, 1e-9))
		_, err = scaledAmount("bogus", 9)
		Expect(err).To(HaveOccurred())
		_, err = scaledAmount("-5", 9)
		Expect(err).To(HaveOccurred())
		// 20 tokens at 18 decimals overflows uint64 but is a valid amount.
		qty, err = scaledAmount("20000000000000000000", 18)
		Expect(err).NotTo(HaveOccurred())
		Expect(qty).To(BeNumerically("~", 20.0, 1e-9))
		qty, err = scaledAmount("", 9)
		Expect(err).NotTo(HaveOccurred())
		Expect(qty).To(BeZero())
		Expect(nanotonsToTON("400000000")).To(Equal(0.4))
		Expect(nanotonsToTON("bogus")).To(BeZero())
		dec, ok := parseDecimals("9")
		Expect(ok).To(BeTrue())
		Expect(dec).To(Equal(9))
		for _, raw := range []string{"", "bogus", "-1", "37"} {
			_, ok = parseDecimals(raw)
			Expect(ok).To(BeFalse())
		}
		dec, ok = decimalsFromExtra(map[string]any{"decimals": "6"})
		Expect(ok).To(BeTrue())
		Expect(dec).To(Equal(6))
		_, ok = decimalsFromExtra(nil)
		Expect(ok).To(BeFalse())
	})

	It("keeps wallet address case and drops empties", func() {
		Expect(walletForms(testWallet, testRaw, "")).To(Equal([]string{testRaw, testWallet}))
		Expect(walletAccountID(testRaw)).To(Equal("ton-" + testRaw))
		Expect(shortAddr("abc")).To(Equal("abc"))
	})

	It("recognizes native assets case-insensitively", func() {
		Expect(isNativeAsset("TON")).To(BeTrue())
		Expect(isNativeAsset("ton")).To(BeTrue())
		Expect(isNativeAsset(testMaster)).To(BeFalse())
	})

	It("rounds float tails without destroying dust", func() {
		Expect(round8(100.00000000000004)).To(Equal(100.0))
		Expect(round8(0)).To(BeZero())
		Expect(round8(1e-9)).To(Equal(1e-9))
	})
})

var _ = Describe("Vault share basis", func() {
	vault := "0:VAULT"
	mkToken := func(symbol, addr string, qty float64) TonToken {
		return TonToken{Symbol: symbol, TokenAddr: addr, Quantity: qty, Decimals: 9}
	}
	mkAct := func(typ brokerage.ActivityType, symbol string, units float64) brokerage.Activity {
		return brokerage.Activity{ID: string(typ) + symbol, Type: typ, Units: units,
			Symbol: &brokerage.Symbol{Symbol: symbol}}
	}

	It("keeps the attribution identity stable when the balance changes", func() {
		w := TonWalletData{
			Tokens:         []TonToken{mkToken("VAULT", vault, 861.24)},
			MasterOutflows: map[string]VaultSpend{vault: {Amount: 876.55, Time: 1700000000}},
		}
		first := vaultAttributions(w)[0].buy(testRaw)
		grown := TonWalletData{
			Tokens:         []TonToken{mkToken("VAULT", vault, 1722.48)},
			MasterOutflows: map[string]VaultSpend{vault: {Amount: 1753.10, Time: 1700000000}},
		}
		second := vaultAttributions(grown)[0].buy(testRaw)
		// Same wallet+master → same row, refreshed values, no duplicate.
		Expect(second.ID).To(Equal(first.ID))
		Expect(second.SourceRecordID).To(Equal(first.SourceRecordID))
		Expect(second.Units).To(Equal(1722.48))
		Expect(second.Amount).To(Equal(1753.10))
	})

	It("flags superseded synthetic fallbacks for retirement", func() {
		mkLeg := func(id, src string, typ brokerage.ActivityType, symbol string, units float64) brokerage.Activity {
			return brokerage.Activity{ID: id, SourceRecordID: src, Type: typ, Units: units,
				Symbol: &brokerage.Symbol{Symbol: symbol}}
		}
		w := TonWalletData{
			Address: testWallet, Canonical: testRaw,
			Tokens:         []TonToken{mkToken("VAULT", vault, 10)},
			MasterOutflows: map[string]VaultSpend{vault: {Amount: 100, Time: 1700000000}},
			Activities: []brokerage.Activity{
				// Verified inbound for the shares: any stored fallback is superseded.
				mkLeg("trace-buy", "trace-buy", brokerage.ActivityBuy, "VAULT", 10),
				// The old synthetic row itself is never evidence.
				mkLeg("vault:"+vault, "vault:"+vault, brokerage.ActivityBuy, "VAULT", 10),
			},
		}
		Expect(staleVaultIDs(&w)).To(Equal([]string{"vault:" + vault}))
		// No inbound history → fallback still live, nothing to retire.
		w.Activities = nil
		Expect(staleVaultIDs(&w)).To(BeEmpty())
	})

	It("attributes master-bound outflows to shares with no inbound history", func() {
		w := TonWalletData{
			Tokens:         []TonToken{mkToken("VAULT", vault, 861.24)},
			MasterOutflows: map[string]VaultSpend{vault: {Amount: 876.55, Time: 1700000000}},
		}
		attrs := vaultAttributions(w)
		Expect(attrs).To(HaveLen(1))
		Expect(attrs[0].symbol).To(Equal("VAULT"))
		Expect(attrs[0].price).To(BeNumerically("~", 1.01778, 1e-4))
		buy := attrs[0].buy(testRaw)
		Expect(buy.Type).To(Equal(brokerage.ActivityBuy))
		Expect(buy.RawType).To(Equal("STAKE_BUY"))
		Expect(buy.Units).To(Equal(861.24))
		Expect(buy.Amount).To(Equal(876.55))
		Expect(buy.TradeDate.Unix()).To(Equal(int64(1700000000)))
		Expect(buy.Description).To(ContainSubstring("no mint transfer indexed"))
		// Stable attribution ID: same input twice, same row.
		Expect(attrs[0].buy(testRaw).ID).To(Equal(buy.ID))
	})

	It("skips gifted tokens and tokens with real inbound legs", func() {
		gifted := TonWalletData{
			Tokens: []TonToken{mkToken("AIR", "0:AIR", 10)},
			Activities: []brokerage.Activity{
				mkAct(brokerage.ActivityTransferIn, "AIR", 10),
			},
			MasterOutflows: map[string]VaultSpend{"0:AIR": {Amount: 5}},
		}
		Expect(vaultAttributions(gifted)).To(BeEmpty())
		empty := TonWalletData{}
		Expect(vaultAttributions(empty)).To(BeEmpty())
	})

	It("records master-bound outflows during transfer mapping", func() {
		forms := []string{testWallet, testRaw}
		masters := map[string]tokenMeta{
			vault:      {Address: vault, Symbol: "VAULT", Decimals: 9, DecimalsKnown: true},
			testMaster: {Address: testMaster, Symbol: "tsTON", Decimals: 9, DecimalsKnown: true},
		}
		mkTransfer := func(receiver string) tonAction {
			return tonAction{ActionID: "w-" + receiver, TraceID: "tw", Start: 1700000000,
				End: 1700000000, Success: true, Type: "jetton_transfer",
				Details: mustDetails(map[string]any{
					"asset": testMaster, "sender": testRaw,
					"receiver": receiver, "amount": "1000000000",
				})}
		}
		outflows := make(map[string]VaultSpend)
		// Outflow straight into a master contract is recorded with its value.
		_, err := transferActivity(context.Background(), testRaw, forms, masters,
			stubValue{unit: 1, unitOK: true}, outflows, nil, nil, mkTransfer(vault))
		Expect(err).NotTo(HaveOccurred())
		Expect(outflows[vault].Amount).To(Equal(1.0))
		Expect(outflows[vault].Time).To(Equal(int64(1700000000)))
		// Ordinary transfers are not.
		_, err = transferActivity(context.Background(), testRaw, forms, masters,
			stubValue{unit: 1, unitOK: true}, outflows, nil, nil, mkTransfer("0:STRANGER"))
		Expect(err).NotTo(HaveOccurred())
		Expect(outflows).To(HaveLen(1))
	})
})
