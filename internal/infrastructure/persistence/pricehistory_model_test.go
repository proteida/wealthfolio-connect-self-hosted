package persistence_test

import (
	"context"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/persistence"
)

var priceCols = []string{"asset", "ts", "currency", "price", "source", "updated_at"}

var _ = Describe("PriceHistoryRepository", func() {
	var (
		ctx  context.Context
		mock sqlmock.Sqlmock
		repo repository.PriceHistoryRepository
		now  time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		db, m, _, err := newMockDB()
		Expect(err).NotTo(HaveOccurred())
		mock = m
		repo = persistence.NewPriceHistoryRepository(db)
		now = time.Now().UTC().Truncate(time.Second)
	})

	It("returns the latest quote at or before the requested time", func() {
		mock.ExpectQuery(rx(`FROM "historical_prices"`)).
			WillReturnRows(sqlmock.NewRows(priceCols).AddRow(
				[]driver.Value{"BTC", now, "USD", 60000.0, "test", now}...))
		got, err := repo.Get(ctx, "btc", "usd", now.Add(time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Asset).To(Equal("BTC"))
		Expect(got.Price).To(Equal(60000.0))
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("returns ErrNotFound when nothing is stored", func() {
		mock.ExpectQuery(rx(`FROM "historical_prices"`)).
			WillReturnRows(sqlmock.NewRows(priceCols))
		_, err := repo.Get(ctx, "BTC", "USD", now)
		Expect(err).To(MatchError(repository.ErrNotFound))
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("lists a window oldest first", func() {
		mock.ExpectQuery(rx(`FROM "historical_prices"`)).
			WillReturnRows(sqlmock.NewRows(priceCols).
				AddRow([]driver.Value{"BTC", now.Add(-time.Hour), "USD", 59000.0, "test", now}...).
				AddRow([]driver.Value{"BTC", now, "USD", 60000.0, "test", now}...))
		got, err := repo.List(ctx, "BTC", "USD", now.Add(-2*time.Hour), now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(2))
		Expect(got[0].Price).To(Equal(59000.0))
		Expect(got[1].Price).To(Equal(60000.0))
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("upserts with conflict refresh so corrections overwrite", func() {
		mock.ExpectExec(`(?i)INSERT INTO "historical_prices".*ON CONFLICT.*DO UPDATE`).
			WillReturnResult(sqlmock.NewResult(0, 2))
		err := repo.Upsert(ctx, []repository.HistoricalPrice{
			{Asset: "BTC", Timestamp: now, Currency: "USD", Price: 60000, Source: "test"},
			{Asset: "BTC", Timestamp: now, Currency: "USD", Price: 60500, Source: "test"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("wraps upsert errors", func() {
		mock.ExpectExec(rx(`INSERT INTO "historical_prices"`)).
			WillReturnError(errors.New("db"))
		err := repo.Upsert(ctx, []repository.HistoricalPrice{
			{Asset: "BTC", Timestamp: now, Currency: "USD", Price: 1, Source: "test"},
		})
		Expect(err).To(MatchError(ContainSubstring("price history upsert")))
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("is a no-op for empty batches", func() {
		Expect(repo.Upsert(ctx, nil)).To(Succeed())
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})
})
