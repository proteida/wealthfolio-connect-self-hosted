// Package persistence implements the PostgreSQL repositories and
// data migrations backing the sync engine. Steam price-currency purge
// below: drop history rows of unknown denomination.
//
// Background: SyncPriceHistory used to request /market/pricehistory/
// without a currency parameter, so Steam answered in the session wallet
// currency while rows were labeled as USD (STEAM_1). Any non-USD session
// backfilled series off by tens of times (observed ~44x), and downstream
// consumers (dust filter, stale fallback, charts) treated them as USD.
// Fixed code requests currency=1 explicitly, validates each series
// against a fresh quote before storing, and tags new rows
// "steam_history_usd".
//
// This migration deletes the legacy rows (sources "steam_history" and
// the pre-tagging "steam_market") for STEAM assets exactly once in
// effect: reruns match zero rows because new code never writes those
// tags. Quote rows ("steam_quote", explicit USD priceoverview) and other
// providers' rows are never touched. History rebuilds gradually through
// the bounded backfill; temporarily raising STEAM_HISTORY_BUDGET speeds
// recovery.
//
// It must run after AutoMigrate converges the schema;
// database.Migrate invokes it via the DataMigrator interface.
package persistence

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// steamPriceLegacySources predate the explicit USD currency request; their
// denomination is the session wallet currency, not USD. Keep in sync with
// the steam client's retired source tags.
var steamPriceLegacySources = []string{"steam_history", "steam_market"}

// migrateSteamPriceCurrency deletes legacy unknown-denomination Steam
// history rows. Idempotent: reruns match no rows.
func migrateSteamPriceCurrency(ctx context.Context, db *gorm.DB) error {
	if err := db.WithContext(ctx).
		Where("asset LIKE ? AND source IN ?", "STEAM:%", steamPriceLegacySources).
		Delete(&HistoricalPricePO{}).Error; err != nil {
		return fmt.Errorf("steam price currency purge: %w", err)
	}
	return nil
}
