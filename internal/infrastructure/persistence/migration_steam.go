// Steam account-scoped uniqueness migration: fold the SteamID into every
// account-owned unique index.
//
// Background: the account-owned Steam tables constrained only the
// business key (event/transaction/trade/lot/asset IDs) while SteamID was
// a plain index. Lot IDs are explicitly `lot:<name>`, so two retained
// accounts holding the same item collide; trade and acquisition rows for
// a second account are silently skipped or attributed to the first
// account by the single-column conflict clauses. The models now declare
// composite (steam_id, key) unique indexes, which AutoMigrate creates on
// fresh installs. This migration converges upgraded installations:
//
//   - Drops the retired single-column unique indexes (if present).
//   - Creates the composite unique indexes (if absent).
//
// It is idempotent (reruns find no work) and runs in a single
// transaction. Existing rows always satisfy the weaker composite
// constraint, so no data movement is needed. It must run after
// AutoMigrate converges the schema; database.Migrate invokes it via the
// DataMigrator interface before the Futu migration.
package persistence

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// steamAccountIndexStmts rebuilds Steam uniqueness around the account:
// drop retired single-column indexes, then create composite ones.
var steamAccountIndexStmts = []string{
	`DROP INDEX IF EXISTS steam_events_uk`,
	`DROP INDEX IF EXISTS steam_market_txs_uk`,
	`DROP INDEX IF EXISTS steam_trades_uk`,
	`DROP INDEX IF EXISTS steam_lots_uk`,
	`DROP INDEX IF EXISTS steam_acquisitions_uk`,
	`CREATE UNIQUE INDEX IF NOT EXISTS steam_events_steam_uk ON steam_events (steam_id, external_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS steam_market_txs_steam_uk ON steam_market_transactions (steam_id, external_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS steam_trades_steam_uk ON steam_trades (steam_id, trade_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS steam_lots_steam_uk ON steam_acquisition_lots (steam_id, id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS steam_acquisitions_steam_uk ON steam_asset_acquisitions (steam_id, assetid)`,
}

// migrateSteamAccountIndexes converges Steam uniqueness to account-scoped
// composite indexes. Idempotent: reruns find no work.
func migrateSteamAccountIndexes(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, stmt := range steamAccountIndexStmts {
			if err := tx.Exec(stmt).Error; err != nil {
				return fmt.Errorf("steam index migration %q: %w", stmt, err)
			}
		}
		return nil
	})
}
