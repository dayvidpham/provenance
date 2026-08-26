package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// scriptedBeginQueryer passes every statement through to a real pinned
// connection except the transaction-opening BEGIN, whose attempts it scripts:
// the first attempt returns a real harvested SQLITE_BUSY, and every later
// attempt blocks until the caller's context expires and returns that context
// error — the exact shape modernc produces when its interrupt watcher fires
// while SQLite is still waiting for the writer lock.
type scriptedBeginQueryer struct {
	conn          *sql.Conn
	busy          error
	beginAttempts atomic.Int64
	zeroBudget    atomic.Int64
}

func (q *scriptedBeginQueryer) ExecContext(ctx context.Context, statement string, args ...any) (sql.Result, error) {
	if statement == "PRAGMA busy_timeout=0" {
		q.zeroBudget.Add(1)
	}
	if strings.HasPrefix(statement, "BEGIN") {
		switch q.beginAttempts.Add(1) {
		case 1:
			return nil, q.busy
		default:
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	return q.conn.ExecContext(ctx, statement, args...)
}

func (q *scriptedBeginQueryer) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	return q.conn.QueryContext(ctx, statement, args...)
}

func (q *scriptedBeginQueryer) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	return q.conn.QueryRowContext(ctx, statement, args...)
}

// TestDeadlineJoinsBusyEvidenceFromEarlierAttempt pins the lastBusy mechanism
// on its own: when an earlier BEGIN attempt observed SQLITE_BUSY and a later
// attempt ends with the context error, the returned error must carry both the
// typed context error and the observed busy evidence — without falling back to
// the post-expiry probe.
func TestDeadlineJoinsBusyEvidenceFromEarlierAttempt(t *testing.T) {
	// Parallel-safe: private file database, private connections, no process
	// state; the only timing dependence is the caller's own deadline.
	t.Parallel()
	path := t.TempDir() + "/busy-evidence.db"
	owner, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open lock owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close() })

	// Open both handles BEFORE taking the writer lock: the second Open runs
	// schema activation, which itself needs the write lock.
	contender, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	scope := takePoolScope(t, contender)
	defer scope.release()

	ownerScope := takePoolScope(t, owner)
	defer ownerScope.release()
	if _, err := ownerScope.conn.ExecContext(ownerScope.ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("acquire writer lock: %v", err)
	}
	defer func() { _, _ = ownerScope.conn.ExecContext(context.Background(), "ROLLBACK") }()

	// Harvest a genuine SQLITE_BUSY under a zero budget against the held lock,
	// so the scripted attempt returns exactly what the driver would.
	harvestCtx, cancelHarvest := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelHarvest()
	if _, err := scope.conn.ExecContext(harvestCtx, "PRAGMA busy_timeout=0"); err != nil {
		t.Fatalf("zero the harvest busy budget: %v", err)
	}
	_, busyErr := scope.conn.ExecContext(harvestCtx, "BEGIN IMMEDIATE")
	if !isBusyError(busyErr) {
		t.Fatalf("harvested error = %v, want SQLITE_BUSY", busyErr)
	}
	_, _ = scope.conn.ExecContext(harvestCtx, "ROLLBACK")
	if _, err := scope.conn.ExecContext(harvestCtx, "PRAGMA busy_timeout=5000"); err != nil {
		t.Fatalf("restore the harvest busy budget: %v", err)
	}

	scripted := &scriptedBeginQueryer{conn: scope.conn, busy: busyErr}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err = runScopedTransaction(ctx, scripted, "BEGIN IMMEDIATE", func() error { return nil })

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scripted contended transaction error = %v, want typed context deadline", err)
	}
	var evidence *sqlite.Error
	if !errors.As(err, &evidence) || !isBusyResultCode(evidence.Code()) {
		t.Fatalf("scripted contended transaction error carries no busy evidence: %v", err)
	}
	if attempts := scripted.beginAttempts.Load(); attempts != 2 {
		t.Fatalf("BEGIN attempts = %d, want exactly 2 (one busy, one interrupted)", attempts)
	}
	if probes := scripted.zeroBudget.Load(); probes != 0 {
		t.Fatalf("post-expiry probe ran %d times; the earlier attempt's evidence should have been joined instead", probes)
	}
}
