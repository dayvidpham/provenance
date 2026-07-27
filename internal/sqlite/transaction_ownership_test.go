package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestImmediateTransactionOwnsWriterWhileReaderKeepsSnapshot(t *testing.T) {
	db := openPoolFileDB(t)
	writer := takePoolScope(t, db)
	reader := takePoolScope(t, db)
	defer writer.release()
	defer reader.release()
	if _, err := writer.conn.ExecContext(writer.ctx, "CREATE TABLE immediate_rows (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("create test table: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runImmediateTransaction(writer.ctx, writer.conn, func() error {
			if _, err := writer.conn.ExecContext(writer.ctx, "INSERT INTO immediate_rows (id) VALUES (1)"); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("writer exited before ownership check: %v", err)
	case <-time.After(poolTestTimeout):
		t.Fatal("writer did not enter BEGIN IMMEDIATE transaction")
	}
	var count int
	if err := reader.conn.QueryRowContext(reader.ctx, "SELECT COUNT(*) FROM immediate_rows").Scan(&count); err != nil || count != 0 {
		t.Fatalf("WAL reader during uncommitted write = (%d, %v), want (0, nil)", count, err)
	}
	contenderCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	contender := takePoolScope(t, db)
	defer contender.release()
	if err := runImmediateTransaction(contenderCtx, contender.conn, func() error { return nil }); err == nil {
		t.Fatal("contending BEGIN IMMEDIATE unexpectedly acquired write ownership")
	}
	var busyTimeout int
	if err := contender.conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("read contender busy timeout after canceled BEGIN IMMEDIATE: %v", err)
	}
	if busyTimeout != busyTimeoutMS {
		t.Fatalf("contender busy_timeout after canceled BEGIN IMMEDIATE = %d, want %d", busyTimeout, busyTimeoutMS)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("writer commit: %v", err)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("writer did not commit")
	}
	if err := runImmediateTransaction(context.Background(), contender.conn, func() error {
		_, err := contender.conn.ExecContext(context.Background(), "INSERT INTO immediate_rows (id) VALUES (2)")
		return err
	}); err != nil {
		t.Fatalf("writer after ownership release: %v", err)
	}
}

func TestImmediateTransactionRollsBackOnCallbackFailure(t *testing.T) {
	db := openPoolFileDB(t)
	scope := takePoolScope(t, db)
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "CREATE TABLE rollback_rows (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("create test table: %v", err)
	}
	sentinel := errors.New("injected rollback")
	err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO rollback_rows (id) VALUES (1)"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback transaction error = %v, want sentinel", err)
	}
	var count int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM rollback_rows").Scan(&count); err != nil {
		t.Fatalf("count rollback rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("rollback left %d rows, want 0", count)
	}
}
