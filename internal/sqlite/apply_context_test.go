package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

func TestApplyContextBoundsContendedWriterByCallerDeadline(t *testing.T) {
	path := t.TempDir() + "/apply-context.db"
	owner, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open lock owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	contender, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	contender.db.SetMaxOpenConns(1)

	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	seedActor(t, owner, actor)
	input := journal.OperationInput{
		OperationID: "apply-context-deadline", ActorID: actor,
		CommandDigest: []byte("apply-context-command"),
		Effects:       []journal.Effect{{Sort: journal.EffectBootstrapAuthority, BootstrapLabel: "deadline-test", ResultSlot: "authority"}},
	}

	lock := takePoolScope(t, owner)
	if _, err := lock.conn.ExecContext(lock.ctx, "BEGIN IMMEDIATE"); err != nil {
		lock.release()
		t.Fatalf("acquire deterministic writer lock: %v", err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			_, _ = lock.conn.ExecContext(context.Background(), "ROLLBACK")
		}
		lock.release()
	})

	deadlineWindow := 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadlineWindow)
	started := time.Now()
	_, applyErr := contender.ApplyContext(ctx, input)
	deadlineElapsed := time.Since(started)
	cancel()
	if !errors.Is(applyErr, context.DeadlineExceeded) {
		t.Fatalf("deadline-aware contended Apply error = %v, want typed context deadline", applyErr)
	}
	if deadlineElapsed >= 2*time.Second || deadlineElapsed >= time.Duration(busyTimeoutMS)*time.Millisecond/2 {
		t.Fatalf("deadline-aware contended Apply took %v, want well below 2s and the %dms busy timeout", deadlineElapsed, busyTimeoutMS)
	}
	if deadlineElapsed < deadlineWindow/2 {
		t.Fatalf("deadline-aware contended Apply took only %v for %v budget; contention test did not exercise lock waiting", deadlineElapsed, deadlineWindow)
	}
	t.Logf("deadline-aware lock wait: %v (legacy busy_timeout: %dms)", deadlineElapsed, busyTimeoutMS)
	lookup, err := contender.LookupCommitted(input.OperationID)
	if err != nil || lookup.Kind != journal.CommittedAbsent {
		t.Fatalf("deadline expiry lookup = (%v, %v), want absent operation with no partial write", lookup.Kind, err)
	}

	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	started = time.Now()
	_, expiredErr := contender.ApplyContext(expired, input)
	expire()
	if !errors.Is(expiredErr, context.DeadlineExceeded) {
		t.Fatalf("already-expired Apply error = %v, want typed context deadline", expiredErr)
	}
	if elapsed := time.Since(started); elapsed >= deadlineWindow/2 {
		t.Fatalf("already-expired Apply took %v, want immediate failure", elapsed)
	}

	if _, err := lock.conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("release deterministic writer lock: %v", err)
	}
	locked = false
	if _, err := contender.Apply(input); err != nil {
		t.Fatalf("legacy Apply after expired call failed; pinned connection may retain a transaction: %v", err)
	}
}

func TestApplyWithoutDeadlineRetainsBusyTimeout(t *testing.T) {
	path := t.TempDir() + "/apply-no-deadline.db"
	owner, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open lock owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	contender, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	seedActor(t, owner, actor)

	lock := takePoolScope(t, owner)
	defer lock.release()
	if _, err := lock.conn.ExecContext(lock.ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("acquire deterministic writer lock: %v", err)
	}
	defer func() { _, _ = lock.conn.ExecContext(context.Background(), "ROLLBACK") }()

	started := time.Now()
	_, applyErr := contender.Apply(journal.OperationInput{
		OperationID: "apply-no-deadline", ActorID: actor, CommandDigest: []byte("command"),
		Effects: []journal.Effect{{Sort: journal.EffectBootstrapAuthority, BootstrapLabel: "no-deadline", ResultSlot: "authority"}},
	})
	elapsed := time.Since(started)
	if applyErr == nil {
		t.Fatal("legacy Apply unexpectedly acquired contended writer lock")
	}
	busyTimeout := time.Duration(busyTimeoutMS) * time.Millisecond
	if elapsed < busyTimeout*3/4 || elapsed > busyTimeout*2 {
		t.Fatalf("legacy Apply lock wait = %v, want behavior governed by %v busy_timeout", elapsed, busyTimeout)
	}
	t.Logf("legacy no-deadline lock wait: %v (busy_timeout: %v)", elapsed, busyTimeout)
}

func seedActor(t *testing.T, db *DB, actor journal.ActorID) {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO agents (id,kind_id) VALUES (?1,?2)", actor.String(), int(ptypes.AgentKindSoftware)); err != nil {
		t.Fatalf("seed actor base row: %v", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO agents_software (agent_id,name,version,source) VALUES (?1,?2,?3,?4)", actor.String(), "apply-context", "0", "test"); err != nil {
		t.Fatalf("seed software actor row: %v", err)
	}
}
