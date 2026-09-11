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
	// SetSyncEnabled flips the sync_enabled flag for one account. Returns
	// ErrNotFound when the account does not exist.
	SetSyncEnabled(ctx context.Context, accountID string, enabled bool) error
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
