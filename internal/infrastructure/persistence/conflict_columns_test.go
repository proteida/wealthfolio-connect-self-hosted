// White-box assertions on option contract persistence: strike, expiry,
// underlying and side must survive a domain round trip.

package persistence

import (
	"testing"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
)

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
