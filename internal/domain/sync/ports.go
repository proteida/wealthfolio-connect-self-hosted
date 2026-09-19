// Package sync defines the ports (interfaces) used by the sync engine to
// reach out to upstream broker / exchange APIs and persist normalized
// snapshots into the domain repositories.
package sync

import (
	"context"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
)

// BrokerSnapshot bundles a single sync result for one upstream broker.
type BrokerSnapshot struct {
	Connection brokerage.Connection
	// Connections is used by integrations that can expose more than one
	// independent brokerage authorization. Existing clients may continue to
	// populate Connection only.
	Connections []brokerage.Connection
	Accounts    []brokerage.Account
	Holdings    []brokerage.Holdings
	Activities  map[string][]brokerage.Activity // keyed by Account ID
	// RetractedActivities lists stale source_record_ids per account that
	// the client filtered out of this snapshot (e.g. Steam dust below the
	// value threshold). The sync engine deletes them so UpsertBatch-only
	// accumulation cannot keep superseded rows visible forever. Empty
	// means nothing to retract; clients that emit incremental history
	// leave it nil so past rows are never garbage-collected.
	RetractedActivities map[string][]string // keyed by Account ID
}

// ActivitySyncState describes the locally persisted progress for one account.
// Paged clients use it to choose between an initial and incremental import.
type ActivitySyncState struct {
	AccountID            string
	InitialSyncCompleted bool
	LastSuccessfulSync   *time.Time
	NextOffset           int
}

// ActivityPage is one bounded batch emitted by a PagedActivityClient.
type ActivityPage struct {
	AccountID            string
	Items                []brokerage.Activity
	NextOffset           int
	Complete             bool
	FirstTransactionDate *time.Time
}

// ActivitySink persists a page before the client requests the next page.
type ActivitySink func(ctx context.Context, page ActivityPage) error

// BrokerClient is implemented by every concrete upstream integration
// (Futu, IBKR, OKX, Binance, Bitget, Hyperliquid, EVM DeFi, ...).
type BrokerClient interface {
	// ID is a stable slug used in logging and connection rows.
	ID() string
	// Fetch reaches out to the upstream service and returns a fully
	// translated BrokerSnapshot. Fetch should be safe to call concurrently
	// with other clients but does not need to be reentrant for itself.
	Fetch(ctx context.Context) (BrokerSnapshot, error)
}

// SnapshotCommitter is an optional acknowledgement for clients with incremental
// progress. Called only after all data and sync status in a snapshot are saved.
// Failed persistence must leave client progress unchanged so retries replay data.
type SnapshotCommitter interface {
	SnapshotCommitted()
}

// PagedActivityClient is an optional, backward-compatible extension for
// integrations whose history can be too large to hold in one BrokerSnapshot.
type PagedActivityClient interface {
	BrokerClient
	FetchAccountSnapshot(ctx context.Context) (BrokerSnapshot, error)
	StreamActivities(ctx context.Context, states []ActivitySyncState, sink ActivitySink) error
}

// ScheduledBrokerClient optionally supplies a client-specific cadence. The
// sync service still owns scheduling and enforces one run per client at a time.
type ScheduledBrokerClient interface {
	BrokerClient
	SyncInterval() time.Duration
}
