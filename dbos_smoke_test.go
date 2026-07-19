package provenance_test

import (
	"context"
	"testing"

	"github.com/dayvidpham/provenance"
)

// TestDBOSSmoke_ApplyCreatesTask is the end-to-end wiring proof: a real launched
// DBOS root, a borrowed tracker over the shared file, one Apply of a task-create
// operation, and a read-back through the same shared database.
func TestDBOSSmoke_ApplyCreatesTask(t *testing.T) {
	s := newDBOSStack(t, nil)

	op := s.createTaskOp("op-smoke-1", "aura", "REQUEST: smoke")
	res, err := s.adapter.Apply(context.Background(), op)
	if err != nil {
		t.Fatalf("adapter.Apply: %v", err)
	}
	if res.Kind != provenance.CommittedExact {
		t.Fatalf("res.Kind = %v, want CommittedExact", res.Kind)
	}
	if res.AnchorJournalID == 0 {
		t.Errorf("AnchorJournalID is zero")
	}

	// The task is visible through the shared database.
	taskID := op.Effects[0].TaskID
	task, err := s.tracker.Show(taskID)
	if err != nil {
		t.Fatalf("Show(%s): %v", taskID, err)
	}
	if task.Status != provenance.StatusOpen {
		t.Errorf("task.Status = %v, want StatusOpen", task.Status)
	}

	// The birth stays reproducible from journal history.
	if _, err := s.tracker.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections: %v", err)
	}
}
