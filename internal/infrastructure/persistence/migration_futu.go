// Futu universal-account data migration: merge retired per-market account
// rows (futu-<AccID>-<market>) into one universal row (futu-<AccID>).
//
// Background: ListAccounts used to fan one real Futu account out to one row
// per authorized market, duplicating shared funds/positions/history across
// them. Fresh snapshots now emit a single futu-<AccID> row, but upgraded
// installations retain the old rows beside it. This migration folds them in:
//
//   - Activities move to the merged account, deduplicated by fill: the same
//     fill stored under two market legs collapses to the primary leg's copy,
//     and a fill already imported under the new account-scoped ID scheme
//     keeps the new row while the old copy is dropped.
//   - Holdings merge to primary-leg balances plus the union of positions.
//   - The merged account carries the primary leg's descriptors and totals;
//     sync_enabled is the AND over all legs (a user-disabled leg is never
//     re-enabled by the merge); sync-progress timestamps take the max.
//   - Old account and holdings rows are deleted.
//
// The migration is idempotent (reruns find no old-pattern rows) and runs in
// a single transaction per call. It must run after AutoMigrate converges the
// schema; database.Migrate invokes it via the DataMigrator interface.

package persistence

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
)

// futuLegIDPattern matches retired per-market account IDs, capturing the
// real account ID and the market slug: futu-281000123-hk.
var futuLegIDPattern = regexp.MustCompile(`^futu-([0-9]+)-([a-z][a-z0-9_]*)$`)

// futuSlugPriority orders market legs for primary selection, mirroring the
// market preference used at fetch time (HK first, then US, CN, SG, JP).
func futuSlugPriority(slug string) int {
	switch slug {
	case "hk":
		return 0
	case "hk_fund":
		return 1
	case "us":
		return 2
	case "us_fund":
		return 3
	case "cn":
		return 4
	case "sg":
		return 5
	case "jp":
		return 6
	default:
		return 99
	}
}

// futuLeg is one retired per-market account row plus its market slug.
type futuLeg struct {
	account AccountPO
	slug    string
}

// MigrateData runs row-level data migrations after AutoMigrate.
func (Migrator) MigrateData(ctx context.Context, db *gorm.DB) error {
	if err := migrateFutuUniversalAccounts(ctx, db); err != nil {
		return fmt.Errorf("futu universal accounts: %w", err)
	}
	return nil
}

func migrateFutuUniversalAccounts(ctx context.Context, db *gorm.DB) error {
	var accounts []AccountPO
	if err := db.WithContext(ctx).Where("id LIKE ?", `futu-%-%`).Find(&accounts).Error; err != nil {
		return fmt.Errorf("list futu accounts: %w", err)
	}
	groups := make(map[string][]futuLeg)
	for _, a := range accounts {
		m := futuLegIDPattern.FindStringSubmatch(a.ID)
		if m == nil {
			continue
		}
		groups[m[1]] = append(groups[m[1]], futuLeg{account: a, slug: m[2]})
	}
	if len(groups) == 0 {
		return nil
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for accID, legs := range groups {
			if err := mergeFutuLegs(ctx, tx, accID, legs); err != nil {
				return err
			}
		}
		return nil
	})
}

// mergeFutuAccountRow folds retired legs into the universal row: primary-leg
// descriptors and totals, sync_enabled ANDed across legs (a disabled leg is
// never re-enabled), completion ANDed across legs with timestamps unset
// while the combined state is incomplete. A single completed leg must not
// promote the merged account to complete.
func mergeFutuAccountRow(newID string, legs []futuLeg) AccountPO {
	primary := legs[0].account
	merged := primary
	merged.ID = newID
	syncEnabled, txDone, holdDone := true, true, true
	for _, l := range legs {
		syncEnabled = syncEnabled && l.account.SyncEnabled
		txDone = txDone && l.account.InitialTxSyncDone
		holdDone = holdDone && l.account.InitialHoldingsDone
		merged.IsPaper = merged.IsPaper || l.account.IsPaper
	}
	merged.SyncEnabled = syncEnabled
	merged.InitialTxSyncDone = txDone
	merged.InitialHoldingsDone = holdDone
	if txDone {
		merged.LastTxSync = maxTimeOf(legs, func(a AccountPO) *time.Time { return a.LastTxSync })
	} else {
		merged.LastTxSync = nil
	}
	if holdDone {
		merged.LastHoldingsSync = maxTimeOf(legs, func(a AccountPO) *time.Time { return a.LastHoldingsSync })
	} else {
		merged.LastHoldingsSync = nil
	}
	merged.FirstTxDate = minTimeOf(legs, func(a AccountPO) *time.Time { return a.FirstTxDate })
	return merged
}

// mergeFutuLegs folds one real account's retired market legs into its
// universal row, creating the row when no post-upgrade sync has yet.
func mergeFutuLegs(ctx context.Context, tx *gorm.DB, accID string, legs []futuLeg) error {
	newID := "futu-" + accID
	sort.SliceStable(legs, func(i, j int) bool {
		return futuSlugPriority(legs[i].slug) < futuSlugPriority(legs[j].slug)
	})
	legIDs := make([]string, 0, len(legs))
	for _, l := range legs {
		legIDs = append(legIDs, l.account.ID)
	}

	var existing []AccountPO
	if err := tx.WithContext(ctx).Where("id = ?", newID).Find(&existing).Error; err != nil {
		return fmt.Errorf("load merged account: %w", err)
	}

	syncEnabled := true
	for _, l := range legs {

		syncEnabled = syncEnabled && l.account.SyncEnabled
	}

	if len(existing) > 0 {
		// A post-upgrade sync already created the merged row: never
		// re-enable a user-disabled account, leave fresh values alone.
		if !syncEnabled && existing[0].SyncEnabled {
			if err := tx.WithContext(ctx).Model(&AccountPO{}).
				Where("id = ?", newID).
				Update("sync_enabled", false).Error; err != nil {
				return fmt.Errorf("preserve disabled sync: %w", err)
			}
		}
	} else {
		merged := mergeFutuAccountRow(newID, legs)
		wantSync := merged.SyncEnabled
		// NOTE: sync_enabled rides an explicit Update below, not the
		// Create: the column carries a `default:true` tag, and GORM
		// writes the tag default over an explicit false on insert.
		merged.SyncEnabled = true
		if err := tx.WithContext(ctx).Create(&merged).Error; err != nil {
			return fmt.Errorf("create merged account: %w", err)
		}
		if !wantSync {
			if err := tx.WithContext(ctx).Model(&AccountPO{}).
				Where("id = ?", newID).
				Update("sync_enabled", false).Error; err != nil {
				return fmt.Errorf("preserve disabled sync: %w", err)
			}
		}
	}

	if err := mergeFutuActivities(ctx, tx, newID, legs); err != nil {
		return err
	}
	if err := mergeFutuHoldings(ctx, tx, newID, legs, len(existing) > 0); err != nil {
		return err
	}

	if err := tx.WithContext(ctx).Where("account_id IN ?", legIDs).Delete(&HoldingsSnapshotPO{}).Error; err != nil {
		return fmt.Errorf("delete retired holdings: %w", err)
	}
	if err := tx.WithContext(ctx).Where("id IN ?", legIDs).Delete(&AccountPO{}).Error; err != nil {
		return fmt.Errorf("delete retired accounts: %w", err)
	}
	return nil
}

// futuActivityKey identifies an activity row for migration planning.
type futuActivityKey struct {
	ID             string `gorm:"column:id"`
	AccountID      string `gorm:"column:account_id"`
	SourceRecordID string `gorm:"column:source_record_id"`
}

// planFutuActivityMoves decides which old-leg rows move to the merged
// account and which are dropped: same-fill copies across legs collapse to
// the primary leg's row (or the first copy when the primary leg lacks the
// fill), and fills already imported under the new account-scoped ID scheme
// keep the new row.
func planFutuActivityMoves(olds []futuActivityKey, primaryID string, newSources map[string]bool) (moveIDs, dropIDs []string) {
	bySource := make(map[string][]futuActivityKey)
	for _, o := range olds {
		bySource[o.SourceRecordID] = append(bySource[o.SourceRecordID], o)
	}
	for _, rows := range bySource {
		if newSources[rows[0].SourceRecordID] {
			for _, r := range rows {
				dropIDs = append(dropIDs, r.ID)
			}
			continue
		}
		keep := 0
		for i, r := range rows {
			if r.AccountID == primaryID {
				keep = i
				break
			}
		}
		for i, r := range rows {
			if i == keep {
				moveIDs = append(moveIDs, r.ID)
			} else {
				dropIDs = append(dropIDs, r.ID)
			}
		}
	}
	return moveIDs, dropIDs
}

// mergeFutuActivities repoints old-leg activities at the merged account,
// dropping cross-leg and both-scheme duplicates.
func mergeFutuActivities(ctx context.Context, tx *gorm.DB, newID string, legs []futuLeg) error {
	legIDs := make([]string, 0, len(legs))
	for _, l := range legs {
		legIDs = append(legIDs, l.account.ID)
	}
	newSources := make(map[string]bool)
	var newRows []string
	if err := tx.WithContext(ctx).Model(&ActivityPO{}).
		Where("account_id = ?", newID).
		Pluck("source_record_id", &newRows).Error; err != nil {
		return fmt.Errorf("list merged activities: %w", err)
	}
	for _, s := range newRows {
		newSources[s] = true
	}
	var olds []futuActivityKey
	if err := tx.WithContext(ctx).Model(&ActivityPO{}).
		Where("account_id IN ?", legIDs).
		Select("id, account_id, source_record_id").
		Scan(&olds).Error; err != nil {
		return fmt.Errorf("list retired activities: %w", err)
	}
	moveIDs, dropIDs := planFutuActivityMoves(olds, legs[0].account.ID, newSources)
	for _, chunk := range chunkStrings(dropIDs, 2000) {
		if err := tx.WithContext(ctx).Where("id IN ?", chunk).Delete(&ActivityPO{}).Error; err != nil {
			return fmt.Errorf("drop duplicate activities: %w", err)
		}
	}
	for _, chunk := range chunkStrings(moveIDs, 2000) {
		if err := tx.WithContext(ctx).Model(&ActivityPO{}).
			Where("id IN ?", chunk).
			Update("account_id", newID).Error; err != nil {
			return fmt.Errorf("repoint activities: %w", err)
		}
	}
	return nil
}

// mergeFutuHoldings folds retired legs' snapshots into the merged row, using
// primary-leg balances plus the union of positions. When the merged account
// already holds a fresh post-upgrade snapshot, it wins untouched.
func mergeFutuHoldings(ctx context.Context, tx *gorm.DB, newID string, legs []futuLeg, freshExists bool) error {
	legIDs := make([]string, 0, len(legs))
	for _, l := range legs {
		legIDs = append(legIDs, l.account.ID)
	}
	if freshExists {
		var count int64
		if err := tx.WithContext(ctx).Model(&HoldingsSnapshotPO{}).
			Where("account_id = ?", newID).Count(&count).Error; err != nil {
			return fmt.Errorf("check merged holdings: %w", err)
		}
		if count > 0 {
			return nil
		}
	}
	var snaps []HoldingsSnapshotPO
	if err := tx.WithContext(ctx).Where("account_id IN ?", legIDs).Find(&snaps).Error; err != nil {
		return fmt.Errorf("list retired holdings: %w", err)
	}
	merged, ok := unionFutuHoldings(newID, legs, snaps)
	if !ok {
		return nil
	}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "account_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"captured_at", "balances", "positions", "options",
		}),
	}).Create(&merged).Error; err != nil {
		return fmt.Errorf("store merged holdings: %w", err)
	}
	return nil
}

// unionFutuHoldings combines retired snapshots: primary-leg balances,
// union of positions/options by identity, latest capture time. It reports
// false when no leg holds a snapshot.
func unionFutuHoldings(newID string, legs []futuLeg, snaps []HoldingsSnapshotPO) (HoldingsSnapshotPO, bool) {
	byAccount := make(map[string]HoldingsSnapshotPO, len(snaps))
	for _, s := range snaps {
		byAccount[s.AccountID] = s
	}
	var base *HoldingsSnapshotPO
	for _, l := range legs {
		if s, ok := byAccount[l.account.ID]; ok {
			base = &s
			break
		}
	}
	if base == nil {
		return HoldingsSnapshotPO{}, false
	}
	merged := brokerage.Holdings{AccountID: newID, CapturedAt: base.CapturedAt}
	if h, err := base.ToDomain(); err == nil {
		merged.Balances = h.Balances
		merged.Positions = append([]brokerage.Position(nil), h.Positions...)
		merged.OptionPositions = append([]brokerage.OptionPosition(nil), h.OptionPositions...)
	} else {
		return HoldingsSnapshotPO{}, false
	}
	seenPos := make(map[string]bool, len(merged.Positions))
	for _, p := range merged.Positions {
		seenPos[p.Symbol.Symbol] = true
	}
	seenOpt := make(map[string]bool, len(merged.OptionPositions))
	for _, o := range merged.OptionPositions {
		seenOpt[optionIdentity(o)] = true
	}
	for _, l := range legs {
		s, ok := byAccount[l.account.ID]
		if !ok || s.AccountID == base.AccountID {
			continue
		}
		h, err := s.ToDomain()
		if err != nil {
			continue
		}
		for _, p := range h.Positions {
			if !seenPos[p.Symbol.Symbol] {
				seenPos[p.Symbol.Symbol] = true
				merged.Positions = append(merged.Positions, p)
			}
		}
		for _, o := range h.OptionPositions {
			if id := optionIdentity(o); !seenOpt[id] {
				seenOpt[id] = true
				merged.OptionPositions = append(merged.OptionPositions, o)
			}
		}
		if h.CapturedAt.After(merged.CapturedAt) {
			merged.CapturedAt = h.CapturedAt
		}
	}
	po, err := holdingsFromDomain(merged)
	if err != nil {
		return HoldingsSnapshotPO{}, false
	}
	return po, true
}

// optionIdentity keys an option position for union dedupe.
func optionIdentity(o brokerage.OptionPosition) string {
	return o.OptionSymbol.Ticker + "|" + string(o.OptionSymbol.OptionType) + "|" +
		o.OptionSymbol.ExpirationDate.Format("2006-01-02")
}

// maxTimeOf returns the latest non-nil timestamp selected from legs.
func maxTimeOf(legs []futuLeg, pick func(AccountPO) *time.Time) *time.Time {
	var best *time.Time
	for _, l := range legs {
		if t := pick(l.account); t != nil && (best == nil || t.After(*best)) {
			best = t
		}
	}
	return best
}

// minTimeOf returns the earliest non-nil timestamp selected from legs.
func minTimeOf(legs []futuLeg, pick func(AccountPO) *time.Time) *time.Time {
	var best *time.Time
	for _, l := range legs {
		if t := pick(l.account); t != nil && (best == nil || t.Before(*best)) {
			best = t
		}
	}
	return best
}

// chunkStrings splits IDs into bounded batches for IN-list statements.
func chunkStrings(ids []string, size int) [][]string {
	var out [][]string
	for len(ids) > 0 {
		n := size
		if n > len(ids) {
			n = len(ids)
		}
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	return out
}
