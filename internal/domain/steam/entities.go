// Package steam is the collectible-assets bounded context: Steam CS2
// inventory tracked as owned items with acquisition provenance, not as
// currency balances. Items carry identity (assetid/classid/instanceid),
// acquisition records with match confidence, and P&L against Steam market
// prices. Asset IDs are not stable across transfers; reconciliation binds
// current assetids to history through lots and evidence, never by fiat.
package steam

import "time"

// CS2AppID is the Steam application ID for Counter-Strike 2.
const CS2AppID = 730

// AcquisitionType names why an item entered the inventory.
type AcquisitionType string

const (
	AcquisitionUnknown    AcquisitionType = ""
	AcquisitionMarketBuy  AcquisitionType = "steam_market"
	AcquisitionTrade      AcquisitionType = "trade"
	AcquisitionCraft      AcquisitionType = "craft"
	AcquisitionDrop       AcquisitionType = "drop"
	AcquisitionGift       AcquisitionType = "gift"
	AcquisitionReturned   AcquisitionType = "market_returned"
	AcquisitionUnresolved AcquisitionType = "unresolved"
)

// MatchConfidence grades how an asset was bound to its acquisition record.
// Exact means a unique asset identifier agreed; anything weaker must never
// be presented as exact.
type MatchConfidence string

const (
	MatchExact      MatchConfidence = "exact"
	MatchHigh       MatchConfidence = "high"
	MatchMedium     MatchConfidence = "medium"
	MatchLow        MatchConfidence = "low"
	MatchUnresolved MatchConfidence = "unresolved"
)

// MatchMethod records which evidence combined into the match.
type MatchMethod string

const (
	MatchSnapshotEvent    MatchMethod = "snapshot+inventory_history"
	MatchNewAssetID       MatchMethod = "trade_new_assetid"
	MatchHistoryMarket    MatchMethod = "inventory_history+market_history"
	MatchAttributes       MatchMethod = "attributes+proximity"
	MatchLotOnly          MatchMethod = "lot_only"
	MatchMethodUnresolved MatchMethod = "unresolved"
)

// SteamAsset is one currently owned item.
type SteamAsset struct {
	SteamID        string
	AssetID        string
	ClassID        string
	InstanceID     string
	MarketHashName string
	Amount         int

	FirstSeenAt time.Time
	LastSeenAt  time.Time

	// Acquisition is nil when unresolved. CostBasis nil means unknown;
	// non-nil zero means known-zero (drop/free), which P&L treats
	// differently from unknown.
	AcquiredAt      *time.Time
	AcquisitionType AcquisitionType
	AcquisitionRef  string
	CostBasis       *float64
	CostCurrency    string
	MatchMethod     MatchMethod
	MatchConfidence MatchConfidence
}

// AcquisitionLot groups fungible items (same market_hash_name) whose
// individual assetids cannot be told apart in history. Resolution assigns
// assets from lots only where evidence permits; the lot itself always
// carries quantity, unit cost and source.
type AcquisitionLot struct {
	ID             string
	SteamID        string
	MarketHashName string
	Quantity       int
	AcquiredAt     time.Time
	UnitCost       *float64
	CostCurrency   string
	Source         AcquisitionType
	Reference      string
}

// PricedAsset is a SteamAsset joined with its valuation.
type PricedAsset struct {
	Asset SteamAsset

	CurrentPrice  *float64
	PriceType     string
	CurrentValue  *float64
	UnrealizedPnL *float64
	// UnrealizedPnLPercent is nil whenever the cost basis is unknown or
	// zero: no percentage is fabricated for free or unpriced items.
	UnrealizedPnLPercent *float64
}

// PricePoint is one normalized historical market quote.
type PricePoint struct {
	Timestamp time.Time
	Price     float64
	Volume    int64
	Currency  string
}

// MarketTransaction is one normalized Community Market ledger row.
type MarketTransaction struct {
	ExternalID     string
	Type           string // buy | sell | listing | cancel | other
	Timestamp      time.Time
	MarketHashName string
	ClassID        string
	InstanceID     string
	Quantity       int
	Gross          float64
	Net            *float64
	Currency       string
}

// TradeRecord is one normalized Steam trade.
type TradeRecord struct {
	TradeID      string
	Timestamp    time.Time
	OtherSteamID string
	Status       string
	Given        []TradeAsset
	Received     []TradeAsset
}

// TradeAsset is one side of a trade, with the optional post-trade identity.
type TradeAsset struct {
	AssetID    string
	ClassID    string
	InstanceID string
	Quantity   int
	NewAssetID string
	NewContext string
}

// InventoryEvent is one normalized inventory-history row, kept even when
// nothing matches it yet so later reconciliation can use it.
type InventoryEvent struct {
	ExternalID     string
	Timestamp      time.Time
	Kind           string
	MarketHashName string
	Quantity       int
	AssetID        string
	Matched        bool
}

// PnL derives the priced view: value, unrealized P&L and its percentage.
// Unknown or zero cost basis yields nil percentages; unknown basis also
// yields nil P&L (never zero, which would claim break-even).
func PnL(asset SteamAsset, currentPrice *float64, priceType string) PricedAsset {
	out := PricedAsset{Asset: asset, PriceType: priceType, CurrentPrice: currentPrice}
	if currentPrice == nil {
		return out
	}
	value := float64(asset.Amount) * *currentPrice
	out.CurrentValue = &value
	if asset.CostBasis == nil {
		return out
	}
	basis := float64(asset.Amount) * *asset.CostBasis
	pnl := value - basis
	out.UnrealizedPnL = &pnl
	if basis != 0 {
		pct := pnl / basis * 100
		out.UnrealizedPnLPercent = &pct
	}
	return out
}
