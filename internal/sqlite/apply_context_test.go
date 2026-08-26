package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
	"modernc.org/sqlite"
)

func TestApplyContextBoundsContendedWriterByCallerDeadline(t *testing.T) {
	// Deliberately serial: this test asserts the caller deadline fires before
	// SQLite's busy timeout, an ordering between two wall-clock durations.
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
	// Consumers classify contention from the error chain, so a wait that was
	// genuinely contended must always carry SQLite's busy evidence — whether an
	// attempt surfaced it directly, an earlier attempt's evidence was joined,
	// or the post-expiry probe proved the lock still held.
	var busyEvidence *sqlite.Error
	if !errors.As(applyErr, &busyEvidence) || !isBusyResultCode(busyEvidence.Code()) {
		t.Fatalf("deadline-aware contended Apply error carries no SQLite contention evidence: %v", applyErr)
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

// TestApplyWithoutDeadlineIsBoundedByBusyTimeoutNotTheCaller pins the
// no-deadline contract: an Apply with no caller deadline is bounded by SQLite's
// own busy handler, so it fails with a BUSY result code while the contended
// writer lock is still held, rather than waiting for the lock or for a caller
// deadline that does not exist.
//
// The oracle is the observable outcome — a BUSY error, no context error, the
// lock never released — not a wall-clock window. The contender's single pooled
// connection carries a deliberately small busy_timeout so the arm proves the
// dependency on that pragma and costs a fraction of a second; the only time in
// the test is an outer guard that fails a hang instead of measuring a duration.
func TestApplyWithoutDeadlineIsBoundedByBusyTimeoutNotTheCaller(t *testing.T) {
	t.Parallel()
	const contenderBusyTimeoutMS = 150
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

	// One connection makes the connection Apply leases provably the connection
	// configured here, so the busy handler under test is the one this test set.
	contender.db.SetMaxOpenConns(1)
	configured := takePoolScope(t, contender)
	if _, err := configured.conn.ExecContext(configured.ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", contenderBusyTimeoutMS)); err != nil {
		configured.release()
		t.Fatalf("configure the contender connection's busy timeout: %v", err)
	}
	configured.release()

	lock := takePoolScope(t, owner)
	defer lock.release()
	if _, err := lock.conn.ExecContext(lock.ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("acquire deterministic writer lock: %v", err)
	}
	defer func() { _, _ = lock.conn.ExecContext(context.Background(), "ROLLBACK") }()

	applied := make(chan error, 1)
	go func() {
		_, err := contender.Apply(journal.OperationInput{
			OperationID: "apply-no-deadline", ActorID: actor, CommandDigest: []byte("command"),
			Effects: []journal.Effect{{Sort: journal.EffectBootstrapAuthority, BootstrapLabel: "no-deadline", ResultSlot: "authority"}},
		})
		applied <- err
	}()

	var applyErr error
	select {
	case applyErr = <-applied:
	case <-time.After(noDeadlineApplyGuard):
		t.Fatalf("Apply without a deadline never returned while the writer lock was held; it must be bounded by the connection's %dms busy_timeout", contenderBusyTimeoutMS)
	}

	// The lock is still held: nothing in this test released it, so a returned Apply
	// can only have been ended by SQLite's busy handler.
	if applyErr == nil {
		t.Fatal("Apply without a deadline acquired the contended writer lock; the lock owner still holds it")
	}
	if errors.Is(applyErr, context.DeadlineExceeded) || errors.Is(applyErr, context.Canceled) {
		t.Fatalf("Apply without a deadline failed with a context error (%v); a no-deadline caller must not bound the wait", applyErr)
	}
	if !isBusyError(applyErr) {
		t.Fatalf("Apply without a deadline error = %v, want a SQLite BUSY/LOCKED result from the busy handler", applyErr)
	}

	// The governing pragma is unchanged: with no caller deadline there is nothing
	// to cap it to, so the connection still carries the value configured above.
	governing := takePoolScope(t, contender)
	defer governing.release()
	var busyTimeout int64
	if err := governing.conn.QueryRowContext(governing.ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("read the contender connection's busy timeout: %v", err)
	}
	if busyTimeout != contenderBusyTimeoutMS {
		t.Fatalf("contender busy_timeout = %dms after a no-deadline Apply, want the configured %dms", busyTimeout, contenderBusyTimeoutMS)
	}
}

// noDeadlineApplyGuard fails a hung Apply. It is a liveness backstop, not the
// oracle: the assertions above are on the returned error and the lock's state.
const noDeadlineApplyGuard = 60 * time.Second

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
