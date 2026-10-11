package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestActivationCleanupSharesOneBoundedContextAndDiscards(t *testing.T) {
	pool, _ := activationContextPool(t)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var before any
	if err := conn.Raw(func(raw any) error { before = raw; return nil }); err != nil {
		t.Fatal(err)
	}
	scope := borrowConnScopeContext(context.Background(), conn, projectionTargetLive)
	scope.activationConfig = &BorrowedActivationConfig{BusyTimeout: time.Millisecond}
	defer scope.endCleanup()
	restore, err := captureBorrowedActivationPragmas(scope)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := scope.cleanupContext()
	if scope.cleanupContext() != cleanup {
		t.Fatal("cleanup received a second budget")
	}
	deadline, ok := cleanup.Deadline()
	if !ok || time.Until(deadline) > 200*time.Microsecond {
		t.Fatal("tiny-tier cleanup budget widened")
	}
	// Expiration is explicit, not a scheduling sleep. Restoration cannot prove
	// the original configuration, so it must retire the underlying connection.
	scope.cleanupCancel()
	if err := restore(); err == nil || !errors.Is(err, context.Canceled) || !scope.discarded {
		t.Fatalf("restore=%v discarded=%v", err, scope.discarded)
	}
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
	replacement, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := replacement.Raw(func(raw any) error {
		if raw == before {
			t.Error("bad connection returned to pool")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledFreshActivationRollsBackSchemaAndSeeds(t *testing.T) {
	for _, phase := range []string{"integrity", "replay"} {
		t.Run(phase, func(t *testing.T) {
			pool, path := activationContextPool(t)
			before := activationSnapshot(t, pool)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store, err := OpenBorrowedContext(ctx, pool, nil, BorrowedActivationConfig{Path: path, BusyTimeout: 500 * time.Millisecond, Observer: func(p string) {
				if p == phase {
					cancel()
				}
			}})
			if store != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("store=%v err=%v", store, err)
			}
			after := activationSnapshot(t, pool)
			if len(before) != 0 || len(after) != 0 {
				t.Fatalf("incomplete activation left tables: before=%v after=%v", before, after)
			}
			assertActivationPoolRestored(t, pool)
			store, err = OpenBorrowed(pool, nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = store.Close()
		})
	}
}

func TestBorrowedActivationSucceedsAfterObservedContention(t *testing.T) {
	pool, path := activationContextPool(t)
	seed, err := OpenBorrowed(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()
	release := holdWriteLock(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	attempts, contentions := 0, 0
	store, err := OpenBorrowedContext(ctx, pool, nil, BorrowedActivationConfig{Path: path, BusyTimeout: 50 * time.Millisecond, Observer: func(phase string) {
		if phase == "attempt" {
			attempts++
		}
		if phase == "contended" {
			contentions++
			release()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if attempts != 2 || contentions != 1 {
		t.Fatalf("attempts=%d contentions=%d", attempts, contentions)
	}
	assertActivationPoolRestored(t, pool)
}

func TestBoundedActivationInjectedRetryExhaustion(t *testing.T) {
	pool, path := activationContextPool(t)
	seed, err := OpenBorrowed(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()
	holdWriteLock(t, path)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	scope := borrowConnScopeContext(context.Background(), conn, projectionTargetLive)
	defer scope.endCleanup()
	var starts []time.Time
	scope.activationPath = path
	scope.activationConfig = &BorrowedActivationConfig{BusyTimeout: 20 * time.Millisecond, Observer: func(phase string) {
		if phase == "attempt" {
			starts = append(starts, time.Now())
		}
	}}
	restore, err := captureBorrowedActivationPragmas(scope)
	if err != nil {
		t.Fatal(err)
	}
	policy := activationRetryPolicy{attemptBusyTimeout: 20 * time.Millisecond, ceiling: 60 * time.Millisecond, initialDelay: 5 * time.Millisecond, maxDelay: 10 * time.Millisecond}
	started := time.Now()
	err = activateSchemaWithRetry(scope, nil, policy)
	if err == nil || !isBusyError(err) {
		t.Fatalf("exhaustion lost last cause: %v", err)
	}
	if len(starts) < 2 {
		t.Fatalf("no retry: %v", starts)
	}
	for _, at := range starts {
		if at.Sub(started) >= policy.ceiling {
			t.Fatalf("attempt began past ceiling: %s", at.Sub(started))
		}
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
}

// lostActivationCommitResult runs a real COMMIT but loses its acknowledgement.
// It delegates all other statements to the real caller-owned SQLite connection.
type lostActivationCommitResult struct {
	*sql.Conn
	cause   error
	commits int
}

func (conn *lostActivationCommitResult) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	result, err := conn.Conn.ExecContext(ctx, query, args...)
	if query == "COMMIT" {
		conn.commits++
		if err == nil {
			return nil, conn.cause
		}
	}
	return result, err
}

func TestActivationLostCommitAcknowledgementReportsUncertainState(t *testing.T) {
	pool, _ := activationContextPool(t)
	if _, err := pool.Exec("CREATE TABLE commit_evidence (value INTEGER)"); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	scope := borrowConnScopeContext(context.Background(), conn, projectionTargetLive)
	scope.activationConfig = &BorrowedActivationConfig{BusyTimeout: 500 * time.Millisecond}
	defer scope.endCleanup()
	cause := errors.New("lost commit acknowledgement")
	injected := &lostActivationCommitResult{Conn: conn, cause: cause}
	err = runScopedTransaction(scope.ctx, injected, "BEGIN IMMEDIATE", func() error {
		_, err := conn.ExecContext(scope.ctx, "INSERT INTO commit_evidence VALUES (1)")
		return err
	}, scope)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "outcome uncertain") || !scope.discarded || injected.commits != 1 {
		t.Fatalf("err=%v discarded=%v commits=%d", err, scope.discarded, injected.commits)
	}
	var count int
	if err := pool.QueryRow("SELECT count(*) FROM commit_evidence").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("commit evidence count=%d", count)
	}
}
