package persistence

import (
	"testing"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
)

// TestApplyFingerprintIdentity covers the economics-based identity
// reconciliation used by UpsertBatch without touching the database.
func TestApplyFingerprintIdentity(t *testing.T) {
	newItem := func(recordID, fingerprint string) brokerage.Activity {
		return brokerage.Activity{ID: recordID, SourceRecordID: recordID, SourceFingerprint: fingerprint}
	}

	t.Run("ignores items without a fingerprint", func(t *testing.T) {
		known := map[string]string{}
		item := newItem("id-1", "")
		applyFingerprintIdentity(&item, known)
		if item.NeedsReview {
			t.Fatal("item without fingerprint must not be flagged")
		}
		if len(known) != 0 {
			t.Fatal("item without fingerprint must not seed the identity map")
		}
	})

	t.Run("seeds unknown fingerprints without flagging", func(t *testing.T) {
		known := map[string]string{}
		item := newItem("id-1", "fp-1")
		applyFingerprintIdentity(&item, known)
		if item.NeedsReview {
			t.Fatal("first record for a fingerprint must not be flagged")
		}
		if known["fp-1"] != "id-1" {
			t.Fatalf("fingerprint must resolve to first identity, got %q", known["fp-1"])
		}
	})

	t.Run("treats matching identity as idempotent re-sync", func(t *testing.T) {
		known := map[string]string{"fp-1": "id-1"}
		item := newItem("id-1", "fp-1")
		applyFingerprintIdentity(&item, known)
		if item.NeedsReview {
			t.Fatal("re-sync of a known identity must not be flagged")
		}
		if item.SourceRecordID != "id-1" {
			t.Fatalf("identity must be preserved, got %q", item.SourceRecordID)
		}
	})

	t.Run("flags distinct identities sharing one fingerprint and keeps both", func(t *testing.T) {
		known := map[string]string{"fp-1": "id-1"}
		item := newItem("id-2", "fp-1")
		applyFingerprintIdentity(&item, known)
		if !item.NeedsReview {
			t.Fatal("colliding identity must be flagged for review")
		}
		if item.SourceRecordID != "id-2" {
			t.Fatalf("colliding identity must keep its own ID, got %q", item.SourceRecordID)
		}
		if known["fp-1"] != "id-1" {
			t.Fatal("first identity must keep winning the fingerprint")
		}
	})

	t.Run("flags in-batch twins consistently", func(t *testing.T) {
		known := map[string]string{}
		first := newItem("id-1", "fp-1")
		second := newItem("id-2", "fp-1")
		applyFingerprintIdentity(&first, known)
		applyFingerprintIdentity(&second, known)
		if first.NeedsReview {
			t.Fatal("first twin must not be flagged")
		}
		if !second.NeedsReview {
			t.Fatal("second twin must be flagged for review")
		}
	})
}
