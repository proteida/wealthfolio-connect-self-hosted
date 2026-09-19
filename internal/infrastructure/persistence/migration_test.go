package persistence_test

import (
	"context"
	"database/sql/driver"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/persistence"
)

var activityKeyCols = []string{"id", "account_id", "source_record_id"}

// steamIndexStmts pins the account-scoped uniqueness migration, which
// runs first inside MigrateData.
var steamIndexStmts = []string{
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

// expectSteamPricePurge stages the legacy currency purge, which runs
// inside MigrateData after the index migration.
func expectSteamPricePurge(m sqlmock.Sqlmock) {
	m.ExpectExec(rx(`DELETE FROM "historical_prices"`)).WillReturnResult(sqlmock.NewResult(0, 0))
}

// expectSteamAccountIndexes stages the Steam uniqueness migration.
func expectSteamAccountIndexes(m sqlmock.Sqlmock) {
	m.ExpectBegin()
	for _, stmt := range steamIndexStmts {
		m.ExpectExec(rx(stmt)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectCommit()
}

var _ = Describe("MigrateData futu universal accounts", func() {
	var (
		ctx  context.Context
		mock sqlmock.Sqlmock
		now  time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		db, m, _, err := newMockDB()
		Expect(err).NotTo(HaveOccurred())
		mock = m
		_ = db
		now = time.Now().UTC().Truncate(time.Second)
	})

	It("is a no-op when no retired legs exist", func() {
		db, m, _, err := newMockDB()
		Expect(err).NotTo(HaveOccurred())
		mock = m
		expectSteamAccountIndexes(mock)
		expectSteamPricePurge(mock)
		mock.ExpectQuery(rx(`FROM "accounts"`)).
			WillReturnRows(sqlmock.NewRows(accountCols))
		Expect(persistence.Migrator{}.MigrateData(ctx, db)).To(Succeed())
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("preserves a disabled leg on an already-merged account", func() {
		db, m, _, err := newMockDB()
		Expect(err).NotTo(HaveOccurred())
		mock = m
		expectSteamAccountIndexes(mock)
		expectSteamPricePurge(mock)

		hkRow := accountRow("futu-1-hk", now)
		usRow := accountRow("futu-1-us", now)
		usRow[10] = false
		newRow := accountRow("futu-1", now)

		mock.ExpectQuery(rx(`FROM "accounts"`)).
			WillReturnRows(sqlmock.NewRows(accountCols).
				AddRow(hkRow...).
				AddRow(usRow...))
		mock.ExpectBegin()
		// Merged row already exists (post-upgrade sync) and enabled.
		mock.ExpectQuery(rx(`FROM "accounts"`)).
			WillReturnRows(sqlmock.NewRows(accountCols).AddRow(newRow...))
		// Disabled leg wins: the merged row is switched off, fresh values kept.
		mock.ExpectExec(rx(`UPDATE "accounts" SET`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(rx(`FROM "activities"`)).
			WillReturnRows(sqlmock.NewRows([]string{"source_record_id"}))
		mock.ExpectQuery(rx(`FROM "activities"`)).
			WillReturnRows(sqlmock.NewRows(activityKeyCols))
		// Fresh post-upgrade holdings win untouched.
		mock.ExpectQuery(rx(`count(*) FROM "holdings_snapshot"`)).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		mock.ExpectExec(rx(`DELETE FROM "holdings_snapshot"`)).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectExec(rx(`DELETE FROM "accounts"`)).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectCommit()

		Expect(persistence.Migrator{}.MigrateData(ctx, db)).To(Succeed())
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("merges retired legs into one universal account", func() {
		db, m, _, err := newMockDB()
		Expect(err).NotTo(HaveOccurred())
		mock = m
		expectSteamAccountIndexes(mock)
		expectSteamPricePurge(mock)

		hkRow := accountRow("futu-1-hk", now)
		usRow := accountRow("futu-1-us", now)
		usRow[10] = false // user disabled the US leg: merged stays disabled
		usRow[5] = "USD"

		// 1. Retired-leg discovery.
		mock.ExpectQuery(rx(`FROM "accounts"`)).
			WillReturnRows(sqlmock.NewRows(accountCols).
				AddRow(hkRow...).
				AddRow(usRow...))
		mock.ExpectBegin()
		// 2. No merged row yet.
		mock.ExpectQuery(rx(`FROM "accounts"`)).
			WillReturnRows(sqlmock.NewRows(accountCols))
		// 3. Merged account row. sync_enabled=false cannot ride the
		// Create (the column default tag would win), so it lands via an
		// explicit update preserving the disabled US leg.
		mock.ExpectExec(rx(`INSERT INTO "accounts"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(rx(`UPDATE "accounts" SET`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		// 4. No new-scheme activities to dedupe against.
		mock.ExpectQuery(rx(`FROM "activities"`)).
			WillReturnRows(sqlmock.NewRows([]string{"source_record_id"}))
		// 5. Old-leg activity keys: f1 duplicated across legs, f2 once.
		mock.ExpectQuery(rx(`FROM "activities"`)).
			WillReturnRows(sqlmock.NewRows(activityKeyCols).
				AddRow([]driver.Value{"h1", "futu-1-hk", "f1"}...).
				AddRow([]driver.Value{"u1", "futu-1-us", "f1"}...).
				AddRow([]driver.Value{"h2", "futu-1-hk", "f2"}...))
		// 6. Drop the non-primary duplicate, move the survivors.
		mock.ExpectExec(rx(`DELETE FROM "activities"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(rx(`UPDATE "activities"`)).
			WillReturnResult(sqlmock.NewResult(0, 2))
		// 7. Holdings snapshots from both legs, then the merged row.
		mock.ExpectQuery(rx(`FROM "holdings_snapshot"`)).
			WillReturnRows(sqlmock.NewRows(holdingsCols).
				AddRow([]driver.Value{"futu-1-hk", now,
					`[{"Currency":{"Code":"HKD"},"Cash":5000}]`,
					`[{"Symbol":{"Symbol":"0700.HK"},"Units":100}]`, `[]`}...).
				AddRow([]driver.Value{"futu-1-us", now,
					`[{"Currency":{"Code":"USD"},"Cash":700}]`,
					`[{"Symbol":{"Symbol":"0700.HK"},"Units":100},{"Symbol":{"Symbol":"AAPL"},"Units":10}]`, `[]`}...))
		mock.ExpectQuery(rx(`INSERT INTO "holdings_snapshot"`)).
			WillReturnRows(sqlmock.NewRows([]string{"balances", "positions", "options"}).
				AddRow([]driver.Value{`[]`, `[]`, `[]`}...))
		// 8. Retired rows removed.
		mock.ExpectExec(rx(`DELETE FROM "holdings_snapshot"`)).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectExec(rx(`DELETE FROM "accounts"`)).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectCommit()

		Expect(persistence.Migrator{}.MigrateData(ctx, db)).To(Succeed())
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})
})

var _ = Describe("MigrateData steam account indexes", func() {
	It("drops retired single-column indexes and creates composites", func() {
		db, m, _, err := newMockDB()
		Expect(err).NotTo(HaveOccurred())
		expectSteamAccountIndexes(m)
		expectSteamPricePurge(m)
		// No retired Futu legs: the discovery query returns nothing.
		m.ExpectQuery(rx(`FROM "accounts"`)).
			WillReturnRows(sqlmock.NewRows(accountCols))
		Expect(persistence.Migrator{}.MigrateData(context.Background(), db)).To(Succeed())
		Expect(m.ExpectationsWereMet()).To(Succeed())
	})
})

var _ = Describe("MigrateData steam price currency", func() {
	It("purges legacy unknown-denomination rows and keeps quotes", func() {
		db, m, _, err := newMockDB()
		Expect(err).NotTo(HaveOccurred())
		expectSteamAccountIndexes(m)
		m.ExpectExec(rx(`DELETE FROM "historical_prices"`)).
			WillReturnResult(sqlmock.NewResult(0, 18209))
		// No retired Futu legs: the discovery query returns nothing.
		m.ExpectQuery(rx(`FROM "accounts"`)).
			WillReturnRows(sqlmock.NewRows(accountCols))
		Expect(persistence.Migrator{}.MigrateData(context.Background(), db)).To(Succeed())
		Expect(m.ExpectationsWereMet()).To(Succeed())
	})
})
