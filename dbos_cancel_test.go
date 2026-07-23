package provenance_test

// dbos_cancel_test.go covers the cancellation contract (issue #6): an
// already-cancelled context starts nothing; cancellation while gated returns
// ApplyWaitCanceledError promptly, leaves durable work running (a later replay
// retrieves the same result), calls no CancelWorkflow, and leaks no waiter.

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance"
)

// blockingJournal gates the FIRST domain fold on a release channel so a test can
// cancel the caller while the adapter awaits, then let the durable step complete.
type blockingJournal struct {
	provenance.JournalAPI
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (b *blockingJournal) Apply(in provenance.OperationInput) (provenance.CommittedResult, error) {
	b.startOnce.Do(func() { close(b.started) })
	<-b.release
	return b.JournalAPI.Apply(in)
}

type blockingTracker struct {
	provenance.Tracker
	journal *blockingJournal
}

func (b *blockingTracker) Journal() provenance.JournalAPI { return b.journal }

func TestCancel_AlreadyCancelled_StartsNothing(t *testing.T) {
	s := newDBOSStack(t, nil)
	before := journalMax(t, s.tracker)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.adapter.Apply(ctx, s.createTaskOp("op-precancel", "aura", "pc"))

	var wce *provenance.ApplyWaitCanceledError
	if !errors.As(err, &wce) {
		t.Fatalf("err = %v, want *ApplyWaitCanceledError", err)
	}
	if wce.Stage != provenance.DBOSDiagStageApplyPreStart {
		t.Errorf("Stage = %q, want %q", wce.Stage, provenance.DBOSDiagStageApplyPreStart)
	}
	if after := journalMax(t, s.tracker); after != before {
		t.Errorf("a workflow ran despite a pre-cancelled context: max %d → %d", before, after)
	}
}

func TestCancel_WhileGated_DurableWorkContinues(t *testing.T) {
	var bj *blockingJournal
	s := newDBOSStack(t, func(real provenance.Tracker) provenance.Tracker {
		bj = &blockingJournal{
			JournalAPI: real.Journal(),
			started:    make(chan struct{}),
			release:    make(chan struct{}),
		}
		return &blockingTracker{Tracker: real, journal: bj}
	})
	// Baseline AFTER Launch so DBOS's persistent background workers are not counted:
	// the check isolates the adapter's own await-waiter goroutine.
	assertNoLeak := leakCheck(t)

	op := s.createTaskOp("op-cancelgated", "aura", "cg")
	ctx, cancel := context.WithCancel(context.Background())

	type applyResult struct {
		res provenance.CommittedResult
		err error
	}
	done := make(chan applyResult, 1)
	go func() {
		res, err := s.adapter.Apply(ctx, op)
		done <- applyResult{res, err}
	}()

	// Wait for the durable step to begin, then cancel the CALLER.
	select {
	case <-bj.started:
	case <-time.After(10 * time.Second):
		t.Fatal("step never started")
	}
	cancel()

	// The caller await returns ApplyWaitCanceledError promptly, before the durable
	// step is released.
	select {
	case r := <-done:
		var wce *provenance.ApplyWaitCanceledError
		if !errors.As(r.err, &wce) {
			t.Fatalf("err = %v, want *ApplyWaitCanceledError", r.err)
		}
		if wce.Stage != provenance.DBOSDiagStageWorkflowAwait && wce.Stage != provenance.DBOSDiagStageWorkflowRetrieve {
			t.Errorf("Stage = %q, want an await stage", wce.Stage)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Apply did not return after caller cancellation")
	}

	// Release the durable step; the workflow completes even though the caller left.
	close(bj.release)

	// A later replay on a fresh context retrieves the SAME committed result — the
	// durable work was never cancelled.
	replayed, err := s.adapter.Apply(context.Background(), op)
	if err != nil {
		t.Fatalf("replay Apply: %v", err)
	}
	if replayed.Kind != provenance.CommittedExact {
		t.Errorf("replay Kind = %v, want CommittedExact", replayed.Kind)
	}
	looked, err := s.tracker.Journal().LookupCommitted(op.OperationID)
	if err != nil {
		t.Fatalf("LookupCommitted: %v", err)
	}
	if !reflect.DeepEqual(replayed, looked) {
		t.Fatalf("cancellation replay complete result=%#v want=%#v", replayed, looked)
	}
	task, err := s.tracker.Show(op.Effects[0].TaskID)
	if err != nil {
		t.Fatalf("Show after cancellation recovery: %v", err)
	}
	if task.ID != op.Effects[0].TaskID || task.Title != op.Effects[0].Title || task.Description != op.Effects[0].Description || task.Type != op.Effects[0].Type || task.Priority != op.Effects[0].Priority || task.Phase != op.Effects[0].Phase || task.Status != provenance.StatusOpen || task.Owner != nil || task.Notes != "" || task.CreatedAt.UnixNano() != op.RecordedAt || !task.UpdatedAt.Equal(task.CreatedAt) || task.ClosedAt != nil || task.CloseReason != "" {
		t.Fatalf("cancellation recovery complete task tuple drifted: %#v", task)
	}
	assertNoLeak()
}

func TestCancel_DeadlineWhileGated(t *testing.T) {
	t.Parallel()
	var bj *blockingJournal
	s := newDBOSStack(t, func(real provenance.Tracker) provenance.Tracker {
		bj = &blockingJournal{
			JournalAPI: real.Journal(),
			started:    make(chan struct{}),
			release:    make(chan struct{}),
		}
		return &blockingTracker{Tracker: real, journal: bj}
	})
	defer func() { close(bj.release) }()

	op := s.createTaskOp("op-deadline", "aura", "dl")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := s.adapter.Apply(ctx, op)
	var wce *provenance.ApplyWaitCanceledError
	if !errors.As(err, &wce) {
		t.Fatalf("err = %v, want *ApplyWaitCanceledError on deadline", err)
	}
}

func TestNewDBOSAdapter_VersionMismatch(t *testing.T) {
	t.Parallel()
	// A mismatch rejects before registration/write (fresh root, unregistered).
	rootA, trackerA := newUnlaunchedRoot(t, "app-v1")
	if _, err := provenance.NewDBOSAdapter(rootA, trackerA, provenance.DBOSAdapterConfig{
		ExpectedApplicationVersion: "not-the-version",
	}); err == nil {
		t.Fatal("expected a version-mismatch rejection")
	}

	// A matching expectation is accepted on a separate fresh root (one registration).
	rootB, trackerB := newUnlaunchedRoot(t, "app-v1")
	if _, err := provenance.NewDBOSAdapter(rootB, trackerB, provenance.DBOSAdapterConfig{
		ExpectedApplicationVersion: "app-v1",
	}); err != nil {
		t.Errorf("matching version rejected: %v", err)
	}
}
