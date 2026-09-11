// Package cexcommon contains the shared translation helpers used by every
// CEX client (Binance, OKX, Bitget, Hyperliquid). Each individual client
// only needs to fetch raw balances and let cexcommon turn them into a
// BrokerSnapshot using a uniform set of conventions:
//
//   - Stablecoins (USDT, USDC, DAI, ...) collapse into the cash balance.
//   - Tiny dust positions (USD value < $1) are dropped.
//   - One brokerage Account per exchange (slug + "-spot").
package cexcommon

import (
	"strings"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	domainsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/sync"
)

// Balance is the normalised per-asset row that each CEX client produces.
type Balance struct {
	Asset    string  // e.g. "BTC"
	Quantity float64 // free + locked
	PriceUSD float64
	USDValue float64 // priceUSD * quantity, supplied by the upstream when available
}

// Trade is one historical trade row. Optional — most CEX clients only fill
// balances on the initial pass.
type Trade struct {
	ID        string
	Symbol    string // e.g. "BTC-USDT"
	Side      string // "buy" or "sell"
	Price     float64
	Quantity  float64
	Fee       float64
	FeeAsset  string // denomination of Fee, e.g. "USDT"; empty when unknown
	Timestamp time.Time
}

// Snapshot bundles balances + optional trades for one CEX.
type Snapshot struct {
	Balances          []Balance
	Trades            []Trade
	ActivitiesFetched bool
	// FeePrice returns the USD price of asset at the given time, or a
	// non-positive value when the price is unavailable. It is only
	// consulted for trade fees denominated in a non-USD asset that also
	// differs from the traded asset; a nil hook means every such fee
	// normalises to 0.
	FeePrice func(asset string, at time.Time) float64
}

// Translate folds a CEX snapshot into a BrokerSnapshot. The resulting
// connection/account IDs are derived from the supplied slug ("okx",
// "binance", ...) so callers can produce stable rows.
func Translate(slug, displayName string, s Snapshot) domainsync.BrokerSnapshot {
	now := time.Now().UTC()
	accountID := slug + "-spot"

	var totalUSD float64
	for _, b := range s.Balances {
		totalUSD += b.USDValue
	}

	account := brokerage.Account{
		ID:                     accountID,
		Name:                   displayName + " Spot",
		Type:                   brokerage.AccountTypeCryptocurrency,
		RawType:                "CRYPTO_SPOT",
		Currency:               "USD",
		BalanceTotal:           totalUSD,
		BalanceCurrency:        "USD",
		BrokerageAuthorization: slug + "-auth",
		InstitutionName:        displayName,
		SyncEnabled:            true,
		Status:                 "open",
		CreatedDate:            now,
		LastHoldingsSync:       &now,
		InitialTxSyncDone:      s.ActivitiesFetched,
		InitialHoldingsDone:    true,
	}
	if s.ActivitiesFetched {
		account.LastTxSync = &now
	}

	connection := brokerage.Connection{
		ID:              slug + "-conn",
		AuthorizationID: slug + "-auth",
		BrokerageName:   displayName,
		BrokerageSlug:   slug,
		DisplayName:     displayName,
		Name:            displayName,
		Status:          brokerage.ConnectionActive,
		UpdatedAt:       now,
	}

	cashBalance := brokerage.Balance{
		Currency: brokerage.Currency{Code: "USD"},
	}
	positions := make([]brokerage.Position, 0, len(s.Balances))
	for _, b := range s.Balances {
		if b.Quantity == 0 || b.USDValue < 1 {
			continue
		}
		asset := strings.ToUpper(b.Asset)
		if IsStablecoin(asset) {
			cashBalance.Cash += b.USDValue
			continue
		}
		positions = append(positions, brokerage.Position{
			Symbol: brokerage.Symbol{
				Symbol:      asset,
				RawSymbol:   asset,
				Description: asset,
				Name:        asset,
				Type:        brokerage.SymbolType{Code: "CRYPTO", IsSupported: true, Description: "Cryptocurrency"},
				Exchange:    brokerage.Exchange{Code: strings.ToUpper(slug), Name: displayName},
				Currency:    brokerage.Currency{Code: "USD"},
			},
			Units:                b.Quantity,
			Price:                b.PriceUSD,
			AveragePurchasePrice: b.PriceUSD,
			Currency:             brokerage.Currency{Code: "USD"},
		})
	}

	holding := brokerage.Holdings{
		AccountID:  accountID,
		Balances:   []brokerage.Balance{cashBalance},
		Positions:  positions,
		CapturedAt: now,
	}

	activities := map[string][]brokerage.Activity{}
	if len(s.Trades) > 0 {
		acts := make([]brokerage.Activity, 0, len(s.Trades))
		for _, t := range s.Trades {
			actType := brokerage.ActivityBuy
			if strings.EqualFold(t.Side, "sell") {
				actType = brokerage.ActivitySell
			}
			fee, feeAsset := normalizeFee(t, s.FeePrice)
			acts = append(acts, brokerage.Activity{
				ID:        t.ID,
				AccountID: accountID,
				Type:      actType,
				TradeDate: t.Timestamp,
				Price:     t.Price,
				Units:     t.Quantity,
				Amount:    t.Price * t.Quantity,
				Fee:       fee,
				FeeAsset:  feeAsset,
				Currency:  brokerage.Currency{Code: "USD"},
				Symbol: &brokerage.Symbol{
					Symbol:    t.Symbol,
					RawSymbol: t.Symbol,
					Type:      brokerage.SymbolType{Code: "CRYPTO", IsSupported: true},
					Exchange:  brokerage.Exchange{Code: strings.ToUpper(slug)},
					Currency:  brokerage.Currency{Code: "USD"},
				},
				RawType:        strings.ToUpper(t.Side),
				ProviderType:   slug,
				SourceSystem:   slug,
				SourceRecordID: t.ID,
			})
		}
		activities[accountID] = acts
	}

	return domainsync.BrokerSnapshot{
		Connection: connection,
		Accounts:   []brokerage.Account{account},
		Holdings:   []brokerage.Holdings{holding},
		Activities: activities,
	}
}

// IsStablecoin returns true for the most common USD-pegged stablecoins.
func IsStablecoin(s string) bool {
	switch strings.ToUpper(s) {
	case "USDT", "USDC", "DAI", "BUSD", "TUSD", "FRAX", "USD", "USDD", "PYUSD":
		return true
	}
	return false
}

// quoteSuffixes are stripped (longest first) to recover the base asset from
// concatenated pair symbols such as "BTCUSDT" that carry no separator.
var quoteSuffixes = []string{
	"USDT", "USDC", "FDUSD", "USDS", "TUSD", "DAI", "FRAX",
	"BUSD", "USDD", "PYUSD", "USD", "EUR", "GBP",
	"BTC", "ETH", "BNB", "SOL",
}

// tradeBaseAsset extracts the traded (base) asset from a pair symbol in
// either "BTC-USDT" or "BTCUSDT" form. It returns "" when the base cannot
// be determined.
func tradeBaseAsset(symbol string) string {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if i := strings.Index(sym, "-"); i > 0 {
		return sym[:i]
	}
	for _, q := range quoteSuffixes {
		if len(sym) > len(q) && strings.HasSuffix(sym, q) {
			return strings.TrimSuffix(sym, q)
		}
	}
	return ""
}

// normalizeFee converts a trade fee into USD following a fixed fallback
// chain so downstream consumers always see a USD-denominated amount:
//
//  1. Zero fee, or an unknown (empty) denomination, passes through untouched
//     and keeps the previous assume-USD behaviour.
//  2. USD and USD-pegged stablecoins pass through 1:1.
//  3. A fee paid in the traded asset itself reuses the trade's own fill
//     price, provided the trade carries a timestamp; without one the fee
//     normalises to 0.
//  4. Any other asset is priced via priceFn; when the hook is nil or the
//     price is unavailable the fee normalises to 0.
//
// The returned asset is always "USD" once a value was derived, so the
// amount and its label stay consistent.
func normalizeFee(t Trade, priceFn func(asset string, at time.Time) float64) (float64, string) {
	if t.Fee == 0 {
		if strings.TrimSpace(t.FeeAsset) == "" {
			return 0, "USD"
		}
		return 0, strings.ToUpper(strings.TrimSpace(t.FeeAsset))
	}
	feeAsset := strings.ToUpper(strings.TrimSpace(t.FeeAsset))
	if feeAsset == "" {
		return t.Fee, "USD"
	}
	if IsStablecoin(feeAsset) {
		return t.Fee, "USD"
	}
	if base := tradeBaseAsset(t.Symbol); base != "" && feeAsset == base {
		if t.Timestamp.IsZero() {
			return 0, "USD"
		}
		return t.Fee * t.Price, "USD"
	}
	if priceFn != nil {
		if p := priceFn(feeAsset, t.Timestamp); p > 0 {
			return t.Fee * p, "USD"
		}
	}
	return 0, "USD"
}
