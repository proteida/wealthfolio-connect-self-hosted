package persistence_test

import (
	"context"
	"database/sql/driver"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/persistence"
)

var _ = Describe("SteamAssetRepository", func() {
	var (
		ctx  context.Context
		mock sqlmock.Sqlmock
		repo repository.SteamAssetRepository
		now  time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		db, m, _, err := newMockDB()
		Expect(err).NotTo(HaveOccurred())
		mock = m
		repo = persistence.NewSteamAssetRepository(db)
		now = time.Now().UTC().Truncate(time.Second)
	})

	It("saves snapshots atomically and reads the latest complete one", func() {
		mock.ExpectBegin()
		mock.ExpectExec(rx(`INSERT INTO "steam_snapshots"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(rx(`INSERT INTO "steam_snapshot_assets"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		err := repo.SaveSnapshot(ctx, repository.SteamInventorySnapshot{
			ID: "s1", SteamID: "765", TakenAt: now, Complete: true,
			Assets: []repository.SteamAssetRow{
				{AssetID: "100", ClassID: "1", InstanceID: "0", MarketHashName: "AK", Amount: 1},
			},
		})
		Expect(err).NotTo(HaveOccurred())

		mock.ExpectQuery(rx(`FROM "steam_snapshots"`)).
			WillReturnRows(sqlmock.NewRows([]string{"id", "steam_id", "taken_at", "complete"}).
				AddRow([]driver.Value{"s1", "765", now, true}...))
		mock.ExpectQuery(rx(`FROM "steam_snapshot_assets"`)).
			WillReturnRows(sqlmock.NewRows([]string{"snapshot_id", "assetid", "classid", "instanceid", "market_hash_name", "amount"}).
				AddRow([]driver.Value{"s1", "100", "1", "0", "AK", 1}...))
		got, err := repo.LatestSnapshot(ctx, "765")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ID).To(Equal("s1"))
		Expect(got.Assets).To(HaveLen(1))
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("returns ErrNotFound without snapshots", func() {
		mock.ExpectQuery(rx(`FROM "steam_snapshots"`)).
			WillReturnRows(sqlmock.NewRows([]string{"id", "steam_id", "taken_at", "complete"}))
		_, err := repo.LatestSnapshot(ctx, "765")
		Expect(err).To(MatchError(repository.ErrNotFound))
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("records events idempotently and tracks matched flags", func() {
		mock.ExpectExec(rx(`INSERT INTO "steam_events"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		Expect(repo.RecordEvents(ctx, "765", []repository.SteamEventRow{
			{ExternalID: "e1", Timestamp: now, Kind: "Traded", MarketHashName: "AK", Quantity: 1, AssetID: "100"},
		})).To(Succeed())

		mock.ExpectQuery(rx(`FROM "steam_events"`)).
			WillReturnRows(sqlmock.NewRows([]string{"steam_id", "external_id", "ts", "kind", "market_hash_name", "quantity", "assetid", "matched"}).
				AddRow([]driver.Value{"765", "e1", now, "Traded", "AK", 1, "100", false}...))
		unmatched, err := repo.UnmatchedEvents(ctx, "765")
		Expect(err).NotTo(HaveOccurred())
		Expect(unmatched).To(HaveLen(1))

		mock.ExpectExec(rx(`UPDATE "steam_events"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		Expect(repo.MarkEventsMatched(ctx, "765", []string{"e1"})).To(Succeed())
		Expect(repo.MarkEventsMatched(ctx, "765", nil)).To(Succeed())
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("saves market transactions, trades, lots and acquisitions", func() {
		mock.ExpectExec(rx(`INSERT INTO "steam_market_transactions"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		Expect(repo.SaveMarketTransactions(ctx, "765", []repository.SteamMarketRow{
			{ExternalID: "m1", Type: "buy", Timestamp: now, MarketHashName: "AK", Quantity: 1, Gross: 23.41, Currency: "USD"},
		})).To(Succeed())

		mock.ExpectExec(rx(`INSERT INTO "steam_trades"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		Expect(repo.SaveTrades(ctx, "765", []repository.SteamTradeRow{
			{TradeID: "t1", Timestamp: now, OtherSteamID: "1", Status: "complete", GivenJSON: []byte(`[]`), ReceivedJSON: []byte(`[]`)},
		})).To(Succeed())

		mock.ExpectBegin()
		mock.ExpectExec(rx(`DELETE FROM "steam_acquisition_lots"`)).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(rx(`INSERT INTO "steam_acquisition_lots"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		basis := 23.41
		Expect(repo.SaveLots(ctx, "765", []repository.SteamLotRow{
			{ID: "lot:AK", MarketHashName: "AK", Quantity: 10, AcquiredAt: now, UnitCost: &basis, CostCurrency: "USD", Source: "steam_market", Reference: "m1"},
		})).To(Succeed())

		mock.ExpectBegin()
		mock.ExpectQuery(rx(`FROM "steam_asset_acquisitions"`)).
			WillReturnRows(sqlmock.NewRows([]string{"steam_id", "assetid", "market_hash_name", "acquired_at", "type", "reference", "cost_basis", "cost_currency", "match_method", "match_confidence"}))
		mock.ExpectExec(rx(`INSERT INTO "steam_asset_acquisitions"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		Expect(repo.SaveAcquisitions(ctx, "765", []repository.SteamAcquisitionRow{
			{AssetID: "100", MarketHashName: "AK", Type: "steam_market", Reference: "m1", CostBasis: &basis, CostCurrency: "USD", MatchMethod: "inventory_history+market_history", MatchConfidence: "high"},
		})).To(Succeed())
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("serves current assets joined with acquisitions", func() {
		mock.ExpectQuery(rx(`FROM "steam_snapshots"`)).
			WillReturnRows(sqlmock.NewRows([]string{"id", "steam_id", "taken_at", "complete"}).
				AddRow([]driver.Value{"s1", "765", now, true}...))
		mock.ExpectQuery(rx(`FROM "steam_snapshot_assets"`)).
			WillReturnRows(sqlmock.NewRows([]string{"snapshot_id", "assetid", "classid", "instanceid", "market_hash_name", "amount"}).
				AddRow([]driver.Value{"s1", "100", "1", "0", "AK", 1}...))
		mock.ExpectQuery(rx(`FROM "steam_asset_acquisitions"`)).
			WillReturnRows(sqlmock.NewRows([]string{"steam_id", "assetid", "market_hash_name", "acquired_at", "type", "reference", "cost_basis", "cost_currency", "match_method", "match_confidence"}).
				AddRow([]driver.Value{"765", "100", "AK", now, "steam_market", "m1", 23.41, "USD", "inventory_history+market_history", "high"}...))
		got, err := repo.CurrentAssets(ctx, "765")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(*got[0].CostBasis).To(BeNumerically("~", 23.41, 1e-9))
		Expect(got[0].MatchConfidence).To(Equal("high"))
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("joins current assets through complete snapshots only", func() { // The snapshots lookup must filter on complete, or an asset
		// omitted by a truncated page would lose its acquisition join.
		mock.ExpectQuery(`(?i)FROM "steam_snapshots".*complete`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "steam_id", "taken_at", "complete"}).
				AddRow([]driver.Value{"s9", "765", now, true}...))
		mock.ExpectQuery(rx(`FROM "steam_snapshot_assets"`)).
			WillReturnRows(sqlmock.NewRows([]string{"snapshot_id", "assetid", "classid", "instanceid", "market_hash_name", "amount"}).
				AddRow([]driver.Value{"s9", "100", "1", "0", "AK", 1}...))
		mock.ExpectQuery(rx(`FROM "steam_asset_acquisitions"`)).
			WillReturnRows(sqlmock.NewRows([]string{"steam_id", "assetid", "market_hash_name", "acquired_at", "type", "reference", "cost_basis", "cost_currency", "match_method", "match_confidence"}).
				AddRow([]driver.Value{"765", "100", "AK", now, "steam_market", "m1", 23.41, "USD", "inventory_history+market_history", "high"}...))
		got, err := repo.CurrentAssets(ctx, "765")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].MatchConfidence).To(Equal("high"))
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("looks up acquisitions by asset IDs without a snapshot", func() {
		mock.ExpectQuery(rx(`FROM "steam_asset_acquisitions"`)).
			WillReturnRows(sqlmock.NewRows([]string{"steam_id", "assetid", "market_hash_name", "acquired_at", "type", "reference", "cost_basis", "cost_currency", "match_method", "match_confidence"}).
				AddRow([]driver.Value{"765", "100", "AK", now, "steam_market", "m1", 23.41, "USD", "inventory_history+market_history", "high"}...))
		got, err := repo.AcquisitionsForAssets(ctx, "765", []string{"100"})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].AssetID).To(Equal("100"))
		Expect(got[0].Reference).To(Equal("m1"))
		Expect(*got[0].CostBasis).To(BeNumerically("~", 23.41, 1e-9))
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("skips the acquisition lookup for empty ID sets", func() {
		got, err := repo.AcquisitionsForAssets(ctx, "765", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})
})
