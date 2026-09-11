// White-box assertions on upsert conflict column lists: what survives a
// re-sync is a data-correctness property, so it is pinned here directly.

package persistence

import (
	"slices"
	"testing"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
)

func TestActivityConflictColumnsRefreshNormalization(t *testing.T) {
	cols := activityConflictColumns()
	for _, want := range []string{
		"price", "units", "amount", "currency_code", "currency_name",
		"type", "subtype", "raw_type", "description",
		"trade_date", "fee", "fee_asset",
		"symbol_ticker", "symbol_raw",
		"source_group_id", "needs_review", "is_external",
	} {
		if !slices.Contains(cols, want) {
			t.Errorf("activityConflictColumns is missing %q: re-syncs would leave it stale", want)
		}
	}
	for _, immutable := range []string{"id", "account_id", "source_record_id", "external_reference_id"} {
		if slices.Contains(cols, immutable) {
			t.Errorf("activityConflictColumns must not contain identity column %q", immutable)
		}
	}
}

func TestDeduplicateActivitiesIsStableAndLastWins(t *testing.T) {
	items := []brokerage.Activity{
		{ID: "first-a", SourceRecordID: "a", Description: "old", TradeDate: time.Unix(1, 0)},
		{ID: "first-b", SourceRecordID: "b", TradeDate: time.Unix(2, 0)},
		{ID: "latest-a", SourceRecordID: "a", Description: "new", TradeDate: time.Unix(3, 0)},
	}

	got := deduplicateActivities("account", items)
	if len(got) != 2 {
		t.Fatalf("deduplicateActivities returned %d rows, want 2", len(got))
	}
	if got[0].SourceRecordID != "a" || got[0].ID != "latest-a" || got[0].Description != "new" {
		t.Errorf("first row = %#v, want the latest value for source a", got[0])
	}
	if got[1].SourceRecordID != "b" {
		t.Errorf("second source record = %q, want b", got[1].SourceRecordID)
	}
}
