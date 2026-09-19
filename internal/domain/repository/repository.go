// Package repository defines the persistence ports (interfaces) consumed by
// the application layer. Concrete implementations live in
// infrastructure/persistence.
package repository

//go:generate go run go.uber.org/mock/mockgen -source=repository.go -destination=mocks/mock_repository.go -package=mocks

import (
	"context"
	"errors"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
)

// ErrNotFound is returned by repositories when a lookup yields no record.
var ErrNotFound = errors.New("repository: not found")

// ConnectionRepository persists brokerage connections.
type ConnectionRepository interface {
	List(ctx context.Context) ([]brokerage.Connection, error)
	Upsert(ctx context.Context, conn brokerage.Connection) error
}

// AccountRepository persists brokerage accounts.
type AccountRepository interface {
	List(ctx context.Context) ([]brokerage.Account, error)
	Get(ctx context.Context, id string) (brokerage.Account, error)
	Upsert(ctx context.Context, acc brokerage.Account) error
	UpdateSyncStatus(ctx context.Context, accountID string, txSync, holdingsSync *time.Time) error
	UpdateActivitySyncProgress(ctx context.Context, accountID string, progress ActivitySyncProgress) error
	// SetSyncEnabled flips the sync_enabled flag for one account. Returns
	// ErrNotFound when the account does not exist.
	SetSyncEnabled(ctx context.Context, accountID string, enabled bool) error
}

// ActivitySyncProgress is the durable checkpoint for a paged history import.
// CompletedAt is nil for an intermediate page and non-nil only after the final
// page has been persisted successfully.
type ActivitySyncProgress struct {
	NextOffset           int
	FirstTransactionDate *time.Time
	CompletedAt          *time.Time
}

// ActivityFilter narrows down a paginated activity query. StartDate is an
// inclusive lower bound; EndDate is an exclusive upper bound (inclusive
// calendar dates are converted to the next-day boundary by callers).
type ActivityFilter struct {
	AccountID string
	StartDate *time.Time
	EndDate   *time.Time
	Offset    int
	Limit     int
}

// ActivityRepository persists trade history.
type ActivityRepository interface {
	// List returns activities, the total matching the filter and an error.
	List(ctx context.Context, f ActivityFilter) ([]brokerage.Activity, int, error)
	// UpsertBatch deduplicates by source_record_id within an account.
	UpsertBatch(ctx context.Context, accountID string, items []brokerage.Activity) error
	// Delete removes the named source records within an account. Used to
	// retire superseded synthetic rows (e.g. a vault-fallback purchase
	// replaced by verified history); deleting unknown IDs is a no-op.
	Delete(ctx context.Context, accountID string, sourceRecordIDs []string) error
}

// HoldingRepository persists snapshots.
type HoldingRepository interface {
	GetLatest(ctx context.Context, accountID string) (brokerage.Holdings, error)
	Replace(ctx context.Context, snapshot brokerage.Holdings) error
}

// SyncCursor is durable incremental-sync progress for one scope (e.g.
// "okx-fills", "ton-tail-<wallet>"). Position is an opaque resume marker;
// Complete records a proven-exhausted range. Cursors advance only after the
// data they cover is persisted (see domainsync.SnapshotCommitter), so a
// failed write replays instead of skipping rows.
type SyncCursor struct {
	Scope     string
	Position  string
	Complete  bool
	UpdatedAt time.Time
}

// CursorRepository persists sync cursors.
type CursorRepository interface {
	// Get returns the cursor for scope, or ErrNotFound when absent.
	Get(ctx context.Context, scope string) (SyncCursor, error)
	// Set creates or replaces the cursor for scope.
	Set(ctx context.Context, c SyncCursor) error
	// Delete clears the cursor for scope.
	Delete(ctx context.Context, scope string) error
}

// TokenMetadata is the audit row stored each time a JWT is issued.
type TokenMetadata struct {
	TokenID   string
	Subject   string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// TokenRepository persists access-token audit records.
type TokenRepository interface {
	Insert(ctx context.Context, t TokenMetadata) error
}

// SteamInventorySnapshot is one persisted inventory state. Complete is
// false for partial fetches, which must never replace the last complete
// snapshot or delete previously known history.
type SteamInventorySnapshot struct {
	ID       string
	SteamID  string
	TakenAt  time.Time
	Complete bool
	Assets   []SteamAssetRow
}

// SteamAssetRow is one asset line inside a snapshot.
type SteamAssetRow struct {
	AssetID        string
	ClassID        string
	InstanceID     string
	MarketHashName string
	Amount         int
}

// SteamAssetRepository persists CS2 inventory state, acquisition lots and
// reconciliation outcomes. Every import is idempotent: reruns upsert the
// same rows and never duplicate them.
type SteamAssetRepository interface {
	// LatestSnapshot returns the newest complete snapshot, or ErrNotFound.
	LatestSnapshot(ctx context.Context, steamID string) (SteamInventorySnapshot, error)
	// SaveSnapshot stores a snapshot with its asset rows atomically.
	SaveSnapshot(ctx context.Context, snap SteamInventorySnapshot) error
	// RecordEvents stores normalized inventory-history rows, skipping known
	// external IDs. Unmatched rows stay for later reconciliation.
	RecordEvents(ctx context.Context, steamID string, events []SteamEventRow) error
	// UnmatchedEvents returns stored events not yet bound to an asset.
	UnmatchedEvents(ctx context.Context, steamID string) ([]SteamEventRow, error)
	// MarkEventsMatched flags events consumed by a reconciliation.
	MarkEventsMatched(ctx context.Context, steamID string, externalIDs []string) error
	// SaveMarketTransactions stores normalized market rows, skipping known IDs.
	SaveMarketTransactions(ctx context.Context, steamID string, txs []SteamMarketRow) error
	// SaveTrades stores normalized trades, skipping known trade IDs.
	SaveTrades(ctx context.Context, steamID string, trades []SteamTradeRow) error
	// SaveLots replaces the whole lot set for the account: stale lots
	// absent from the new set are cleared. Callers must only invoke it
	// for complete snapshots.
	SaveLots(ctx context.Context, steamID string, lots []SteamLotRow) error
	// SaveAcquisitions upserts per-asset acquisition records with an
	// upgrade-only rule: stored evidence is replaced solely by
	// equal-or-stronger evidence, never downgraded.
	SaveAcquisitions(ctx context.Context, steamID string, acquisitions []SteamAcquisitionRow) error
	// CurrentAssets returns the latest known asset states for valuation.
	CurrentAssets(ctx context.Context, steamID string) ([]SteamAssetState, error)
	// AcquisitionsForAssets returns stored acquisition records for the
	// given asset IDs, independently of any snapshot baseline, so assets
	// first seen in a partial sync keep recoverable evidence.
	AcquisitionsForAssets(ctx context.Context, steamID string, assetIDs []string) ([]SteamAcquisitionRow, error)
}

// SteamEventRow is one stored inventory-history event.
type SteamEventRow struct {
	ExternalID     string
	Timestamp      time.Time
	Kind           string
	MarketHashName string
	Quantity       int
	AssetID        string
	Matched        bool
}

// SteamMarketRow is one stored market transaction.
type SteamMarketRow struct {
	ExternalID     string
	Type           string
	Timestamp      time.Time
	MarketHashName string
	ClassID        string
	InstanceID     string
	Quantity       int
	Gross          float64
	Net            *float64
	Currency       string
}

// SteamTradeRow is one stored trade with both asset sides as JSON.
type SteamTradeRow struct {
	TradeID      string
	Timestamp    time.Time
	OtherSteamID string
	Status       string
	GivenJSON    []byte
	ReceivedJSON []byte
}

// SteamLotRow is one stored acquisition lot.
type SteamLotRow struct {
	ID             string
	MarketHashName string
	Quantity       int
	AcquiredAt     time.Time
	UnitCost       *float64
	CostCurrency   string
	Source         string
	Reference      string
}

// SteamAcquisitionRow binds one assetid to its acquisition evidence.
type SteamAcquisitionRow struct {
	AssetID         string
	MarketHashName  string
	AcquiredAt      *time.Time
	Type            string
	Reference       string
	CostBasis       *float64
	CostCurrency    string
	MatchMethod     string
	MatchConfidence string
}

// SteamAssetState is the stored per-asset record served for valuation.
type SteamAssetState struct {
	SteamAssetRow
	FirstSeenAt     time.Time
	LastSeenAt      time.Time
	AcquiredAt      *time.Time
	Type            string
	Reference       string
	CostBasis       *float64
	CostCurrency    string
	MatchMethod     string
	MatchConfidence string
}

// HistoricalPrice is one market quote stored without expiration: asset and
// currency are upper-cased codes ("BTC", "USD"), Timestamp is the provider
// quote time, Source names the provider ("tonapi", "coingecko", "binance"),
// and UpdatedAt records when the row was last written. Rows are updateable
// because providers may later correct data.
type HistoricalPrice struct {
	Asset     string
	Timestamp time.Time
	Currency  string
	Price     float64
	Source    string
	UpdatedAt time.Time
}

// PriceHistoryRepository is the durable store for historical market prices
// and finalized daily candles. Records never expire.
type PriceHistoryRepository interface {
	// Get returns the latest stored quote at or before at, or ErrNotFound.
	Get(ctx context.Context, asset, currency string, at time.Time) (HistoricalPrice, error)
	// List returns stored quotes in [from, to], oldest first.
	List(ctx context.Context, asset, currency string, from, to time.Time) ([]HistoricalPrice, error)
	// Upsert inserts quotes, overwriting any row with the same
	// (asset, timestamp, currency, source) so provider corrections land.
	Upsert(ctx context.Context, prices []HistoricalPrice) error
}

// CurrentPriceCache is the short-lived store for latest prices and today's
// incomplete daily candles. Implementations expire entries (about an hour
// for current prices, end of day for candles) and fail open: cache errors
// must never block a provider fetch.
type CurrentPriceCache interface {
	// Get returns the cached price, or found=false on a miss or error.
	Get(ctx context.Context, key string) (price float64, found bool, err error)
	// Set stores a price for ttl.
	Set(ctx context.Context, key string, price float64, ttl time.Duration) error
	// GetRaw returns a cached blob (e.g. a JSON-encoded daily candle).
	GetRaw(ctx context.Context, key string) (raw []byte, found bool, err error)
	// SetRaw stores a blob for ttl.
	SetRaw(ctx context.Context, key string, raw []byte, ttl time.Duration) error
	// Delete drops a key, e.g. after a daily candle is finalized.
	Delete(ctx context.Context, key string) error
}
