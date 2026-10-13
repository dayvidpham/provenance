package provenance_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dayvidpham/provenance"
	"modernc.org/sqlite"
)

// Observe the production BEGIN entry, not a test-only registration handler.
type registrationConnector struct {
	dsn               string
	begin             atomic.Pointer[chan struct{}]
	attempts          atomic.Int32
	cancelAfterInsert atomic.Pointer[context.CancelFunc]
}

func (c *registrationConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c *registrationConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &registrationConn{Conn: conn, owner: c}, nil
}

type registrationConn struct {
	driver.Conn
	owner *registrationConnector
}

func (c *registrationConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if query == "BEGIN IMMEDIATE" {
		c.owner.attempts.Add(1)
		if entered := c.owner.begin.Swap(nil); entered != nil {
			close(*entered)
		}
	}
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err == nil && query == "INSERT INTO agents (id, kind_id) VALUES (?1, ?2)" {
		if cancel := c.owner.cancelAfterInsert.Swap(nil); cancel != nil {
			(*cancel)()
		}
	}
	return result, err
}
func (c *registrationConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}
func (c *registrationConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }

func TestSoftwareAgentContextColdRegistrationCancellation(t *testing.T) {
	t.Parallel()
	const tier = 250 * time.Millisecond
	path := filepath.Join(t.TempDir(), "cold.db")
	connector := &registrationConnector{dsn: fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)", path, tier.Milliseconds())}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(1)
	defer pool.Close()
	tracker, err := provenance.OpenBorrowedSQLite(pool, provenance.WithModelRegistry(provenance.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tracker.Close()
	registrar := tracker.(provenance.SoftwareAgentRegistrarContext)
	holder, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	conn, err := holder.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	entered := make(chan struct{})
	connector.begin.Store(&entered)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		<-finished
	})
	go func() {
		defer close(finished)
		agent, err := registrar.RegisterSoftwareAgentContext(ctx, "test", "cold", "1", "source")
		if agent != (provenance.SoftwareAgent{}) {
			err = fmt.Errorf("canceled registration returned an agent: %+v", agent)
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("registration never reached BEGIN")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want canceled, got %v", err)
		}
	case <-time.After(tier + 30*time.Millisecond):
		// Unblock and join before failing; the holder stays locked until the bound.
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		<-done
		t.Fatal("registration did not stop within one busy tier")
	}
	var count int
	if err := conn.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM agents").Scan(&count); err != nil || count != 0 {
		t.Fatalf("registration debris: count=%d err=%v", count, err)
	}
	if _, err := conn.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	// Cancellation is invocation-scoped; both new and legacy APIs still work.
	agent, err := registrar.RegisterSoftwareAgentContext(t.Context(), "test", "live", "2", "source")
	if err != nil {
		t.Fatal(err)
	}
	got, err := tracker.SoftwareAgent(agent.ID)
	if err != nil || got != agent {
		t.Fatalf("readback=%+v err=%v", got, err)
	}
	if _, err := tracker.RegisterSoftwareAgent("test", "legacy", "1", "source"); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Journal().VerifyIntegrity(); err != nil {
		t.Fatal(err)
	}
	// A live deadline longer than the tier does not turn registration into a
	// private retry loop. Keep the independent holder locked until return.
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	before := connector.attempts.Load()
	deadlineCtx, deadlineCancel := context.WithTimeout(t.Context(), 10*tier)
	defer deadlineCancel()
	_, err = registrar.RegisterSoftwareAgentContext(deadlineCtx, "test", "contended", "1", "source")
	if err == nil || deadlineCtx.Err() != nil {
		t.Fatalf("want escaped contention while caller live: %v, ctx=%v", err, deadlineCtx.Err())
	}
	if attempts := connector.attempts.Load() - before; attempts != 1 {
		t.Fatalf("registration attempted BEGIN %d times", attempts)
	}
	if _, err := conn.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	// Cancel after BEGIN and the base insert actually succeeded. The software
	// subtype and base row must roll back together on the production path.
	if err := pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM agents").Scan(&count); err != nil {
		t.Fatal(err)
	}
	beforeRows := count
	insertCtx, insertCancel := context.WithCancel(t.Context())
	defer insertCancel()
	connector.cancelAfterInsert.Store(&insertCancel)
	_, err = registrar.RegisterSoftwareAgentContext(insertCtx, "test", "rollback", "1", "source")
	if !errors.Is(err, context.Canceled) || connector.cancelAfterInsert.Load() != nil {
		t.Fatalf("insert cancellation not observed: %v", err)
	}
	if err := pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM agents").Scan(&count); err != nil || count != beforeRows {
		t.Fatalf("partial registration: rows=%d want=%d err=%v", count, beforeRows, err)
	}
}

func TestSoftwareAgentContextPoolCancellation(t *testing.T) {
	t.Parallel()
	pool, _ := openFileDB(t)
	pool.SetMaxOpenConns(1)
	tracker, err := provenance.OpenBorrowedSQLite(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer tracker.Close()
	cause := errors.New("registration caller stopped")
	canceled, stop := context.WithCancelCause(t.Context())
	stop(cause)
	if _, err := tracker.(provenance.SoftwareAgentRegistrarContext).RegisterSoftwareAgentContext(canceled, "test", "canceled", "1", "source"); !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("borrowed cancellation cause: %v", err)
	}
	conn, err := pool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = tracker.(provenance.SoftwareAgentRegistrarContext).RegisterSoftwareAgentContext(ctx, "test", "pool", "1", "source")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pool cancellation: %v", err)
	}
	if _, unavailable := provenance.AsStoreUnavailable(err); unavailable {
		t.Fatalf("live pool cancellation classified as unavailable store: %v", err)
	}
	if pool.Stats().WaitCount == 0 {
		t.Fatal("test did not wait for pool capacity")
	}
}

func TestSoftwareAgentContextStandalone(t *testing.T) {
	t.Parallel()
	tracker, err := provenance.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer tracker.Close()
	registrar := tracker.(provenance.SoftwareAgentRegistrarContext)
	if _, err := registrar.RegisterSoftwareAgentContext(nil, "test", "nil", "1", "source"); err == nil {
		t.Fatal("nil context accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := registrar.RegisterSoftwareAgentContext(ctx, "test", "canceled", "1", "source"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled: %v", err)
	}
	if _, err := registrar.RegisterSoftwareAgentContext(t.Context(), "test", "live", "1", "source"); err != nil {
		t.Fatal(err)
	}
}
