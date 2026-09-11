// White-box assertions on upsert conflict column lists (what survives a
// re-sync is a data-correctness property, so it is pinned here directly)
// and on option contract persistence (strike, expiry, underlying and side
// must survive a domain round trip).

package persistence

import (
	"slices"
	"testing"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
)

func TestActivityConflictColumnsRefreshNormalization(t *testing.T) {
	cols := activityConflictColumns()
	for _, want := range []string{
		"price", "units", "amount", "currency_code", "currency_name",
		"type", "subtype", "raw_type", "description",
		"trade_date", "fee", "fee_asset",
		"symbol_ticker", "symbol_raw",
		"source_group_id", "needs_review", "is_external",
	} {
		if !slices.Contains(cols, want) {
			t.Errorf("activityConflictColumns is missing %q: re-syncs would leave it stale", want)
		}
	}
	for _, immutable := range []string{"id", "account_id", "source_record_id", "external_reference_id"} {
		if slices.Contains(cols, immutable) {
			t.Errorf("activityConflictColumns must not contain identity column %q", immutable)
		}
	}
}

func TestOptionSymbolRoundTrip(t *testing.T) {
	expiry := time.Date(2026, 1, 16, 0, 0, 0, 0, time.UTC)
	in := brokerage.Activity{
		ID: "e1", AccountID: "ibkr-U1", SourceRecordID: "e1",
		Type:      brokerage.ActivityBuy,
		TradeDate: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		OptionSymbol: &brokerage.OptionSymbol{
			Ticker:         "AAPL",
			OptionType:     brokerage.OptionCall,
			StrikePrice:    250,
			ExpirationDate: expiry,
			Underlying:     brokerage.Symbol{Symbol: "AAPL", RawSymbol: "AAPL"},
		},
	}
	out := activityFromDomain("ibkr-U1", in).ToDomain()
	if out.OptionSymbol == nil {
		t.Fatal("OptionSymbol lost in persistence round trip")
	}
	got := out.OptionSymbol
	if got.Ticker != "AAPL" || got.OptionType != brokerage.OptionCall ||
		got.StrikePrice != 250 || !got.ExpirationDate.Equal(expiry) ||
		got.Underlying.Symbol != "AAPL" {
		t.Errorf("OptionSymbol corrupted in round trip: %+v", got)
	}
	// Absent option legs stay nil, never empty structs.
	plain := brokerage.Activity{ID: "x"}
	if out := activityFromDomain("a", plain).ToDomain(); out.OptionSymbol != nil {
		t.Errorf("non-option activity gained an OptionSymbol: %+v", out.OptionSymbol)
	}
}
