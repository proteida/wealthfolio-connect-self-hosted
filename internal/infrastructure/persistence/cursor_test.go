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

var cursorCols = []string{"scope", "position", "complete", "updated_at"}

var _ = Describe("CursorRepository", func() {
	var (
		ctx  context.Context
		mock sqlmock.Sqlmock
		repo repository.CursorRepository
		now  time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		db, m, _, err := newMockDB()
		Expect(err).NotTo(HaveOccurred())
		mock = m
		repo = persistence.NewCursorRepository(db)
		now = time.Now().UTC().Truncate(time.Second)
	})

	It("round-trips a cursor", func() {
		mock.ExpectQuery(rx(`FROM "sync_state"`)).
			WillReturnRows(sqlmock.NewRows(cursorCols).AddRow(
				[]driver.Value{"okx-fills", "abc", true, now}...))
		got, err := repo.Get(ctx, "okx-fills")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Scope).To(Equal("okx-fills"))
		Expect(got.Position).To(Equal("abc"))
		Expect(got.Complete).To(BeTrue())
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("returns ErrNotFound when the scope was never stored", func() {
		mock.ExpectQuery(rx(`FROM "sync_state"`)).
			WillReturnRows(sqlmock.NewRows(cursorCols))
		_, err := repo.Get(ctx, "missing")
		Expect(err).To(MatchError(repository.ErrNotFound))
	})

	It("upserts cursor state", func() {
		mock.ExpectExec(rx(`INSERT INTO "sync_state"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		Expect(repo.Set(ctx, repository.SyncCursor{
			Scope: "okx-fills", Position: "abc", Complete: true, UpdatedAt: now,
		})).To(Succeed())
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("propagates set errors", func() {
		mock.ExpectExec(rx(`INSERT INTO "sync_state"`)).WillReturnError(errors.New("db"))
		Expect(repo.Set(ctx, repository.SyncCursor{Scope: "x"})).To(MatchError(ContainSubstring("db")))
	})

	It("deletes cursor state", func() {
		mock.ExpectExec(rx(`DELETE FROM "sync_state"`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		Expect(repo.Delete(ctx, "okx-fills")).To(Succeed())
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})
})
