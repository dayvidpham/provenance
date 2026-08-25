package provenance_test

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dayvidpham/provenance"
	_ "modernc.org/sqlite"
)

// TestFusedGovernedAllocationComposedForcedTransitionsSurviveReopen pins the
// production composition path to the shared journal replay contract. The second
// start is deliberately invalid under the ordinary FSM (in_progress ->
// in_progress), so replay can converge only when both forced events preserve
// their durable marker.
func TestFusedGovernedAllocationComposedForcedTransitionsSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "composed-forced-replay.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	open := func() *provenance.FusedGovernedAllocator {
		allocator, err := provenance.OpenFusedGovernedAllocator(ctx, provenance.FusedGovernedAllocatorConfig{
			SQLiteDSN: dsn, AppName: "provenance-composed-forced-replay", ApplicationVersion: "test-v1", Logger: slog.Default(),
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

	firstAllocator := open()
	actor := registerGovernedActor(t, firstAllocator.Tracker(), "composed-forced-replay")
	root := initializeFusedRoot(t, firstAllocator, actor, "composed-forced-replay-root")
	request := composedGovernedRequest("composed-forced-replay", actor, root, 1)
	child := request.Allocation.Children[0]
	request.SupplementalEffects = []provenance.Effect{
		{Sort: provenance.EffectTaskEvent, TaskID: child.TaskID, EventKind: provenance.EventKindTaskStarted, Forced: true, ResultSlot: "forced-legal"},
		{Sort: provenance.EffectTaskEvent, TaskID: child.TaskID, EventKind: provenance.EventKindTaskStarted, Forced: true, ResultSlot: "forced-invalid"},
	}

	first, err := firstAllocator.RunAllocateComposed(ctx, "composed-forced-replay-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("run composed forced transitions: %v", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("open marker inspection database: %v", err)
	}
	var marked int
	if err := db.QueryRow(`SELECT COUNT(*) FROM journal_task_events WHERE task_id=?1 AND event_kind=?2 AND json_extract(payload, '$.forced')=1`, child.TaskID.String(), string(provenance.EventKindTaskStarted)).Scan(&marked); err != nil {
		_ = db.Close()
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("inspect forced transition markers: %v", err)
	}
	if marked != 2 {
		_ = db.Close()
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("persisted forced transition markers=%d, want 2", marked)
	}
	if err := db.Close(); err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("close marker inspection database: %v", err)
	}
	if err := firstAllocator.Close(30 * time.Second); err != nil {
		t.Fatalf("close first fused allocator: %v", err)
	}

	reopened := open()
	t.Cleanup(func() { _ = reopened.Close(30 * time.Second) })
	if _, err := reopened.Tracker().Journal().ReplayProjections(); err != nil {
		t.Fatalf("replay projections after reopen: %v", err)
	}
	projected, err := reopened.Tracker().Show(child.TaskID)
	if err != nil {
		t.Fatalf("show replayed child: %v", err)
	}
	if projected.Status != provenance.StatusInProgress {
		t.Fatalf("replayed child status=%v, want %v", projected.Status, provenance.StatusInProgress)
	}
	replayed, err := reopened.RunAllocateComposed(ctx, "composed-forced-replay-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("exact composed retry after reopen: %v", err)
	}
	assertSameClosure(t, first.Closure(), replayed.Closure())
	if !reflect.DeepEqual(first.SupplementalResultSlots(), replayed.SupplementalResultSlots()) || !reflect.DeepEqual(first.SupplementalEmittedEvents(), replayed.SupplementalEmittedEvents()) {
		t.Fatalf("reopen changed exact composed receipt: first=%+v replayed=%+v", first, replayed)
	}
}
