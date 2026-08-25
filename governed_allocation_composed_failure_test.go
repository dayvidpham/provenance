package provenance_test

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	provenance "github.com/dayvidpham/provenance"
	_ "modernc.org/sqlite"
)

// Every top-level test in this file is parallel under the isolation proof
// documented above openGovernedTracker in governed_allocation_integration_test.go:
// this test owns a private t.TempDir database, reopened only by itself.

func TestComposedActivityChronologyRejectsTamperingAcrossReplayAndReopen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "composed-activity-chronology.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	open := func() *provenance.FusedGovernedAllocator {
		allocator, err := provenance.OpenFusedGovernedAllocator(ctx, provenance.FusedGovernedAllocatorConfig{
			SQLiteDSN: dsn, AppName: "composed-activity-chronology", ApplicationVersion: "test-v1", Logger: slog.Default(),
		})
		if err != nil {
			t.Fatalf("open fused allocator: %v", err)
		}
		if err := allocator.Launch(); err != nil {
			_ = allocator.Close(30 * time.Second)
			t.Fatalf("launch fused allocator: %v", err)
		}
		return allocator
	}

	allocator := open()
	actor := registerGovernedActor(t, allocator.Tracker(), "composed-activity-chronology")
	root := initializeFusedRoot(t, allocator, actor, "composed-activity-chronology")
	request := composedGovernedRequest("composed-activity-chronology", actor, root, 1)
	if _, err := allocator.RunAllocateComposed(ctx, "chronology-first", root.AssignmentRow.JournalID, request); err != nil {
		t.Fatalf("commit composed allocation: %v", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open inspection database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	activity := request.SupplementalEffects[3].ActivityID.String()
	var original int64
	if err := db.QueryRow(`SELECT started_at FROM activities WHERE id=?1`, activity).Scan(&original); err != nil {
		t.Fatalf("read activity start: %v", err)
	}
	if _, err := db.Exec(`UPDATE activities SET started_at=?1,ended_at=?2 WHERE id=?3`, original+1, original+2, activity); err != nil {
		t.Fatalf("tamper activity chronology: %v", err)
	}
	if _, err := allocator.RunAllocateComposed(ctx, "chronology-distinct", root.AssignmentRow.JournalID, request); err == nil {
		t.Fatal("distinct-workflow replay authenticated a tampered activity start")
	} else {
		mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
	}
	if err := allocator.Close(30 * time.Second); err != nil {
		t.Fatalf("close first allocator: %v", err)
	}

	reopened := open()
	t.Cleanup(func() { _ = reopened.Close(30 * time.Second) })
	if _, err := reopened.RunAllocateComposed(ctx, "chronology-first", root.AssignmentRow.JournalID, request); err == nil {
		t.Fatal("same-workflow replay after reopen authenticated a tampered activity start")
	} else {
		mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
	}

	// A later end is legitimate history. Restoring only the exact start must make
	// reconstruction succeed without requiring ended_at to remain NULL.
	if _, err := db.Exec(`UPDATE activities SET started_at=?1 WHERE id=?2`, original, activity); err != nil {
		t.Fatalf("restore activity start while retaining later end: %v", err)
	}
	if _, err := reopened.RunAllocateComposed(ctx, "chronology-legitimate-end", root.AssignmentRow.JournalID, request); err != nil {
		t.Fatalf("replay rejected legitimate later activity end: %v", err)
	}
}
