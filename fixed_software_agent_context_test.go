package provenance

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// innerLeaseConnector observes actual driver BEGIN calls, not registration logic.
type innerLeaseConnector struct {
	dsn          string
	begin        atomic.Int64
	beginSuccess atomic.Int64
	beginEntered atomic.Pointer[chan struct{}]
	bodyReads    atomic.Int64
	cancelInsert atomic.Pointer[fixedInsertCancellation]
	rollbacks    atomic.Int64
	commitGate   atomic.Pointer[fixedCommitGate]
	cleanupFault atomic.Pointer[fixedCleanupFault]
	opens        atomic.Int64
	closes       atomic.Int64
}

type fixedCleanupFault struct {
	query   string
	cause   error
	reached chan struct{}
	release chan struct{}
}

type fixedCommitGate struct {
	after   bool
	reached chan struct{}
	release chan struct{}
	result  chan error
}

type fixedInsertCancellation struct {
	prefix    string
	cancel    context.CancelCauseFunc
	cause     error
	completed atomic.Int64
}

func (c *innerLeaseConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c *innerLeaseConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	c.opens.Add(1)
	return &innerLeaseConn{Conn: conn, owner: c}, nil
}

type innerLeaseConn struct {
	driver.Conn
	owner *innerLeaseConnector
}

func (c *innerLeaseConn) Close() error {
	c.owner.closes.Add(1)
	return c.Conn.Close()
}

func (c *innerLeaseConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if fault := c.owner.cleanupFault.Load(); fault != nil && query == fault.query && c.owner.cleanupFault.CompareAndSwap(fault, nil) {
		close(fault.reached)
		<-fault.release
		return nil, fault.cause
	}
	if query == "BEGIN IMMEDIATE" {
		c.owner.begin.Add(1)
		if entered := c.owner.beginEntered.Swap(nil); entered != nil {
			close(*entered)
		}
	}
	var gate *fixedCommitGate
	if query == "COMMIT" {
		gate = c.owner.commitGate.Swap(nil)
		if gate != nil && !gate.after {
			close(gate.reached)
			<-gate.release
		}
	}
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if gate != nil {
		gate.result <- err
		if gate.after {
			close(gate.reached)
			<-gate.release
		}
	}
	if query == "BEGIN IMMEDIATE" && err == nil {
		c.owner.beginSuccess.Add(1)
	}
	if query == "ROLLBACK" && err == nil {
		c.owner.rollbacks.Add(1)
	}
	if err == nil {
		if injection := c.owner.cancelInsert.Load(); injection != nil && strings.HasPrefix(query, injection.prefix) {
			injection.completed.Add(1)
			injection.cancel(injection.cause)
		}
	}
	return result, err
}

func (c *innerLeaseConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.HasPrefix(query, "SELECT namespace, claimant_id, range_min, range_max, codec FROM actor_namespace_claims") {
		c.owner.bodyReads.Add(1)
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *innerLeaseConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}

func TestFixedSoftwareAgentContextForeignKeyRestoreFailureAfterCommit(t *testing.T) {
	t.Parallel()
	testFixedCleanupFailure(t, false, false, false)
}

func TestFixedSoftwareAgentContextForeignKeyRestoreFailureAfterRollback(t *testing.T) {
	t.Parallel()
	testFixedCleanupFailure(t, false, true, false)
}

func TestFixedSoftwareAgentContextBusyTimeoutRestoreFailureAfterCommit(t *testing.T) {
	t.Parallel()
	testFixedCleanupFailure(t, true, false, false)
}

func TestFixedSoftwareAgentContextRollbackFailureRetiresConnection(t *testing.T) {
	t.Parallel()
	testFixedCleanupFailure(t, false, true, true)
}

func testFixedCleanupFailure(t *testing.T, busy, failOperation, failRollback bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cleanup-fault.db")
	connector := &innerLeaseConnector{dsn: "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(120000)"}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	tr, err := OpenBorrowedSQLite(pool, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	query := "PRAGMA foreign_keys=OFF"
	if busy {
		query = "PRAGMA busy_timeout=120000"
	}
	if failRollback {
		query = "ROLLBACK"
	}
	faultCause := errors.New("injected cleanup restoration failure")
	fault := &fixedCleanupFault{query: query, cause: faultCause, reached: make(chan struct{}), release: make(chan struct{})}
	connector.cleanupFault.Store(fault)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	if busy {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, time.Minute)
		defer stop()
	}
	operationCause := errors.New("injected operation cancellation")
	var injection *fixedInsertCancellation
	if failOperation {
		injection = &fixedInsertCancellation{prefix: "INSERT INTO actor_namespace_claims ", cancel: cancel, cause: operationCause}
		connector.cancelInsert.Store(injection)
	}
	beforeOpen, beforeClose, beforeRollback := connector.opens.Load(), connector.closes.Load(), connector.rollbacks.Load()
	type outcome struct {
		agent SoftwareAgent
		err   error
	}
	result := make(chan outcome, 1)
	finished := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { cancel(operationCause); release.Do(func() { close(fault.release) }); <-finished })
	reg := testFixedSoftwareAgentRegistration()
	go func() {
		defer close(finished)
		agent, err := tr.(FixedSoftwareAgentRegistrarContext).RegisterFixedSoftwareAgentContext(ctx, reg)
		result <- outcome{agent: agent, err: err}
	}()
	ceiling := time.NewTimer(10 * time.Second)
	defer ceiling.Stop()
	select {
	case <-fault.reached:
	case got := <-result:
		t.Fatalf("returned before cleanup fault: %v", got.err)
	case <-ceiling.C:
		t.Fatal("cleanup restore was not attempted")
	}
	select {
	case got := <-result:
		t.Fatalf("returned before cleanup settled: %v", got.err)
	default:
	}
	release.Do(func() { close(fault.release) })
	var got outcome
	select {
	case got = <-result:
	case <-ceiling.C:
		t.Fatal("cleanup failure did not settle")
	}
	<-finished
	if got.agent != (SoftwareAgent{}) || !errors.Is(got.err, faultCause) || !strings.Contains(got.err.Error(), "retired") || !strings.Contains(got.err.Error(), "inspect the store") {
		t.Fatalf("cleanup outcome = (%+v, %v), want actionable retirement and cause", got.agent, got.err)
	}
	want := [4]int{1, 1, 1, 1}
	if failOperation {
		want = [4]int{}
		wantRollbacks := int64(1)
		if failRollback {
			wantRollbacks = 0
			if !strings.Contains(got.err.Error(), "uncertain") {
				t.Fatalf("failed rollback did not report uncertainty: %v", got.err)
			}
		}
		if injection.completed.Load() != 1 || connector.rollbacks.Load()-beforeRollback != wantRollbacks || !errors.Is(got.err, operationCause) || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("operation failure or rollback outcome lost: %v", got.err)
		}
	}
	if connector.closes.Load() <= beforeClose {
		t.Fatal("unusable physical connection was not closed")
	}
	if err := pool.PingContext(context.Background()); err != nil {
		t.Fatalf("pool recovery: %v", err)
	}
	if connector.opens.Load() <= beforeOpen {
		t.Fatal("pool reused the retired connection")
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	assertFixedAgentRowCounts(t, path, want)
	reopened, err := OpenSQLite(path, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatalf("reopen after cleanup failure: %v", err)
	}
	defer reopened.Close()
	assertFixedAgentRowCounts(t, path, want)
	if _, err := reopened.(FixedSoftwareAgentRegistrarContext).RegisterFixedSoftwareAgentContext(context.Background(), reg); err != nil {
		t.Fatalf("healthy registration/replay: %v", err)
	}
}

func TestFixedSoftwareAgentContextCancelBeforeCommitDelegation(t *testing.T) {
	t.Parallel()
	testFixedCommitCancellation(t, false)
}

func TestFixedSoftwareAgentContextCancelAfterSuccessfulCommit(t *testing.T) {
	t.Parallel()
	testFixedCommitCancellation(t, true)
}

func testFixedCommitCancellation(t *testing.T, after bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "commit.db")
	connector := &innerLeaseConnector{dsn: "file:" + path + "?_pragma=journal_mode(WAL)"}
	pool := sql.OpenDB(connector)
	t.Cleanup(func() { _ = pool.Close() })
	tr, err := OpenBorrowedSQLite(pool, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	gate := &fixedCommitGate{after: after, reached: make(chan struct{}), release: make(chan struct{}), result: make(chan error, 1)}
	connector.commitGate.Store(gate)
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("cancel at fixed registration commit")
	type outcome struct {
		agent SoftwareAgent
		err   error
	}
	result := make(chan outcome, 1)
	finished := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { cancel(cause); release.Do(func() { close(gate.release) }); <-finished })
	reg := testFixedSoftwareAgentRegistration()
	beforeRollback := connector.rollbacks.Load()
	go func() {
		defer close(finished)
		agent, err := tr.(FixedSoftwareAgentRegistrarContext).RegisterFixedSoftwareAgentContext(ctx, reg)
		result <- outcome{agent: agent, err: err}
	}()
	ceiling := time.NewTimer(10 * time.Second)
	defer ceiling.Stop()
	select {
	case <-gate.reached:
	case got := <-result:
		t.Fatalf("registration returned before commit boundary: %v", got.err)
	case <-ceiling.C:
		t.Fatal("registration did not reach commit boundary")
	}
	if after {
		if err := <-gate.result; err != nil {
			t.Fatalf("underlying commit did not succeed: %v", err)
		}
	}
	cancel(cause)
	select {
	case got := <-result:
		t.Fatalf("registration returned before commit settlement was released: %v", got.err)
	default:
	}
	release.Do(func() { close(gate.release) })
	if !after {
		select {
		case err := <-gate.result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("underlying commit = %v, want cancellation failure", err)
			}
		case <-ceiling.C:
			t.Fatal("underlying commit did not settle")
		}
	}
	var got outcome
	select {
	case got = <-result:
	case <-ceiling.C:
		t.Fatal("registration did not settle")
	}
	<-finished
	if after {
		if got.err != nil || got.agent.ID != AgentID(reg.Entry.ActorID) {
			t.Fatalf("committed registration = (%+v, %v), want success", got.agent, got.err)
		}
		if connector.rollbacks.Load() != beforeRollback {
			t.Fatal("successful commit was followed by rollback")
		}
	} else {
		if got.agent != (SoftwareAgent{}) || !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, cause) {
			t.Fatalf("failed commit = (%+v, %v), want cancellation cause", got.agent, got.err)
		}
		if connector.rollbacks.Load()-beforeRollback != 1 {
			t.Fatal("failed commit did not report a successful rollback")
		}
	}
	if err := pool.PingContext(context.Background()); err != nil {
		t.Fatalf("borrowed pool after commit settlement: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	want := [4]int{}
	if after {
		want = [4]int{1, 1, 1, 1}
	}
	assertFixedAgentRowCounts(t, path, want)
	reopened, err := OpenSQLite(path, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatalf("reopen after commit settlement: %v", err)
	}
	defer reopened.Close()
	assertFixedAgentRowCounts(t, path, want)
	if _, err := reopened.(FixedSoftwareAgentRegistrarContext).RegisterFixedSoftwareAgentContext(context.Background(), reg); err != nil {
		t.Fatalf("healthy registration or replay after settlement: %v", err)
	}
	assertFixedAgentRowCounts(t, path, [4]int{1, 1, 1, 1})
}

func TestFixedSoftwareAgentContextCancelAfterSuccessfulInsert(t *testing.T) {
	t.Parallel()
	for _, tc := range loadFixedAgentFixture(t).CancellationCases {
		t.Run(tc.Name, func(t *testing.T) {
			var prefix string
			switch tc.Boundary {
			case "claim":
				prefix = "INSERT INTO actor_namespace_claims "
			case "agent":
				prefix = "INSERT INTO agents ("
			case "software":
				prefix = "INSERT INTO agents_software "
			case "manifest":
				prefix = "INSERT INTO fixed_actor_manifest_entries "
			default:
				t.Fatalf("unknown cancellation boundary %q", tc.Boundary)
			}
			path := filepath.Join(t.TempDir(), "cancel-insert.db")
			connector := &innerLeaseConnector{dsn: "file:" + path + "?_pragma=journal_mode(WAL)"}
			pool := sql.OpenDB(connector)
			t.Cleanup(func() { _ = pool.Close() })
			tr, err := OpenBorrowedSQLite(pool, WithModelRegistry(NewRegistry(nil)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tr.Close() })
			assertFixedAgentRowCounts(t, path, [4]int{})
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			cause := errors.New("cancel after successful " + tc.Boundary + " insert")
			injection := &fixedInsertCancellation{prefix: prefix, cancel: cancel, cause: cause}
			connector.cancelInsert.Store(injection)
			beforeRollback := connector.rollbacks.Load()
			reg := testFixedSoftwareAgentRegistration()
			agent, err := tr.(FixedSoftwareAgentRegistrarContext).RegisterFixedSoftwareAgentContext(ctx, reg)
			if agent != (SoftwareAgent{}) || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
				t.Fatalf("registration = (%+v, %v), want zero agent and cancellation cause", agent, err)
			}
			if got := injection.completed.Load(); got != 1 {
				t.Fatalf("successful target inserts = %d, want one", got)
			}
			if got := connector.rollbacks.Load() - beforeRollback; got != 1 {
				t.Fatalf("confirmed rollbacks = %d, want one", got)
			}
			connector.cancelInsert.Store(nil)
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			if err := pool.Close(); err != nil {
				t.Fatal(err)
			}
			assertFixedAgentRowCounts(t, path, [4]int{})
			reopened, err := OpenSQLite(path, WithModelRegistry(NewRegistry(nil)))
			if err != nil {
				t.Fatalf("reopen after confirmed rollback: %v", err)
			}
			defer reopened.Close()
			if _, err := reopened.Agent(AgentID(reg.Entry.ActorID)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("reopened store has canceled actor: %v", err)
			}
			assertFixedAgentRowCounts(t, path, [4]int{})
			api := reopened.(FixedSoftwareAgentRegistrarContext)
			first, err := api.RegisterFixedSoftwareAgentContext(context.Background(), reg)
			if err != nil {
				t.Fatalf("healthy registration after rollback: %v", err)
			}
			second, err := reopened.RegisterFixedSoftwareAgent(reg)
			if err != nil || first != second {
				t.Fatalf("legacy exact replay = (%+v, %v), want %+v", second, err, first)
			}
			assertFixedAgentRowCounts(t, path, [4]int{1, 1, 1, 1})
		})
	}
}

func TestFixedSoftwareAgentContextCancellationAndRecovery(t *testing.T) {
	tr, err := OpenMemory(WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	api, ok := tr.(FixedSoftwareAgentRegistrarContext)
	if !ok {
		t.Fatal("memory tracker lacks fixed registration context capability")
	}
	reg := testFixedSoftwareAgentRegistration()
	if _, err := api.RegisterFixedSoftwareAgentContext(nil, reg); err == nil {
		t.Fatal("nil context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := api.RegisterFixedSoftwareAgentContext(ctx, reg); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled registration: %v", err)
	}
	if _, err := tr.Agent(AgentID(reg.Entry.ActorID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("canceled registration created agent: %v", err)
	}
	if _, err := api.RegisterFixedSoftwareAgentContext(context.Background(), reg); err != nil {
		t.Fatalf("healthy registration after cancellation: %v", err)
	}
}

func TestFixedSoftwareAgentContextBorrowedInnerLeaseCancellation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "inner-lease.db")
	connector := &innerLeaseConnector{dsn: "file:" + path + "?_pragma=journal_mode(WAL)"}
	pool := sql.OpenDB(connector)
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(1)
	tr, err := OpenBorrowedSQLite(pool, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	borrowed := tr.(*borrowedTracker)
	// The callback runs only after the real liveness Ping has returned its
	// lease. Holding that same pool's only connection makes the next wait the
	// inner registration scope, not the borrowed preflight.
	leased := make(chan *sql.Conn, 1)
	leaseErr := make(chan error, 1)
	borrowed.fixedRegistrationAfterPreflight = func() {
		conn, err := pool.Conn(context.Background())
		if err != nil {
			leaseErr <- err
			return
		}
		leased <- conn
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("cancel inner registration lease")
	finished := make(chan struct{})
	result := make(chan error, 1)
	var holder *sql.Conn
	var release sync.Once
	t.Cleanup(func() {
		cancel(cause)
		if holder != nil {
			release.Do(func() { _ = holder.Close() })
		}
		<-finished
		if holder == nil {
			select {
			case conn := <-leased:
				_ = conn.Close()
			default:
			}
		}
	})
	reg := testFixedSoftwareAgentRegistration()
	before := pool.Stats().WaitCount
	beginBefore := connector.begin.Load()
	go func() {
		defer close(finished)
		_, err := tr.(FixedSoftwareAgentRegistrarContext).RegisterFixedSoftwareAgentContext(ctx, reg)
		result <- err
	}()
	ceiling := time.NewTimer(5 * time.Second)
	defer ceiling.Stop()
	select {
	case holder = <-leased:
	case err := <-leaseErr:
		t.Fatalf("lease after successful preflight: %v", err)
	case <-ceiling.C:
		t.Fatal("registration did not complete borrowed preflight")
	}
	for pool.Stats().WaitCount == before {
		select {
		case err := <-result:
			t.Fatalf("registration returned before inner lease waited: %v", err)
		case <-ceiling.C:
			t.Fatal("inner scope did not wait for the held pool lease")
		default:
			runtime.Gosched()
		}
	}
	cancel(cause)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
			t.Fatalf("inner lease cancellation lost cause: %v", err)
		}
	case <-ceiling.C:
		t.Fatal("registration did not settle while the pool lease remained held")
	}
	<-finished
	release.Do(func() {
		if err := holder.Close(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	})
	// The invocation has returned and joined before releasing the pool; any
	// delayed transaction would now be able to run. Inspect all activation rows.
	assertFixedAgentRowCounts(t, path, [4]int{})
	if got := connector.begin.Load(); got != beginBefore {
		t.Fatalf("canceled inner lease entered %d transactions, want none", got-beginBefore)
	}
	borrowed.fixedRegistrationAfterPreflight = nil
	if _, err := tr.(FixedSoftwareAgentRegistrarContext).RegisterFixedSoftwareAgentContext(context.Background(), reg); err != nil {
		t.Fatalf("healthy registration after canceled lease: %v", err)
	}
	assertFixedAgentRowCounts(t, path, [4]int{1, 1, 1, 1})
}

func TestFixedSoftwareAgentContextWriterLockCancellation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "writer-lock.db")
	connector := &innerLeaseConnector{dsn: "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"}
	pool := sql.OpenDB(connector)
	t.Cleanup(func() { _ = pool.Close() })
	tr, err := OpenBorrowedSQLite(pool, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	holderDB, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holderDB.Close() })
	holder, err := holderDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("acquire independent writer lock: %v", err)
	}
	var unlock sync.Once
	release := func() {
		unlock.Do(func() {
			if _, err := holder.ExecContext(context.Background(), "ROLLBACK"); err != nil {
				t.Errorf("release independent writer lock: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("cancel fixed registration writer wait")
	result := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() { cancel(cause); release(); <-finished })
	entered := make(chan struct{})
	connector.beginEntered.Store(&entered)
	beginBefore, successBefore, readsBefore := connector.begin.Load(), connector.beginSuccess.Load(), connector.bodyReads.Load()
	reg := testFixedSoftwareAgentRegistration()
	go func() {
		defer close(finished)
		_, err := tr.(FixedSoftwareAgentRegistrarContext).RegisterFixedSoftwareAgentContext(ctx, reg)
		result <- err
	}()
	ceiling := time.NewTimer(15 * time.Second)
	defer ceiling.Stop()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("registration returned before writer attempt: %v", err)
	case <-ceiling.C:
		t.Fatal("registration did not reach writer attempt")
	}
	// Keep the independent writer locked until the contender settles. SQLite's
	// own busy policy may finish the attempt after cancellation; no latency is
	// inferred from that outcome.
	cancel(cause)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
			t.Fatalf("writer wait did not preserve cancellation cause: %v", err)
		}
	case <-ceiling.C:
		t.Fatal("writer contender did not settle while the lock was held")
	}
	<-finished
	release()
	if got := connector.begin.Load() - beginBefore; got != 1 {
		t.Fatalf("contender attempted BEGIN %d times, want one", got)
	}
	if got := connector.beginSuccess.Load() - successBefore; got != 0 {
		t.Fatalf("contender acquired %d transactions after cancellation, want none", got)
	}
	if got := connector.bodyReads.Load() - readsBefore; got != 0 {
		t.Fatalf("contender entered registration body %d times", got)
	}
	assertFixedAgentRowCounts(t, path, [4]int{})
	if _, err := tr.(FixedSoftwareAgentRegistrarContext).RegisterFixedSoftwareAgentContext(context.Background(), reg); err != nil {
		t.Fatalf("registration after canceled writer wait: %v", err)
	}
	assertFixedAgentRowCounts(t, path, [4]int{1, 1, 1, 1})
}

func TestFixedSoftwareAgentContextBorrowedPreflightPoolWait(t *testing.T) {
	t.Parallel()
	pool, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "pool.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	tr, err := OpenBorrowedSQLite(pool, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	holder, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() { cancel(); _ = holder.Close(); <-finished })
	before := pool.Stats().WaitCount
	go func() {
		defer close(finished)
		_, err := tr.(FixedSoftwareAgentRegistrarContext).RegisterFixedSoftwareAgentContext(ctx, testFixedSoftwareAgentRegistration())
		done <- err
	}()
	ceiling := time.NewTimer(5 * time.Second)
	defer ceiling.Stop()
	for pool.Stats().WaitCount == before {
		select {
		case <-ceiling.C:
			t.Fatal("borrowed preflight did not wait for the pool")
		default:
			runtime.Gosched()
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("preflight cancellation: %v", err)
		}
	case <-ceiling.C:
		t.Fatal("preflight cancellation did not settle while lease was held")
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	reg := testFixedSoftwareAgentRegistration()
	if _, err := tr.Agent(AgentID(reg.Entry.ActorID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("canceled preflight left actor: %v", err)
	}
	if _, err := tr.(FixedSoftwareAgentRegistrarContext).RegisterFixedSoftwareAgentContext(context.Background(), reg); err != nil {
		t.Fatalf("healthy registration: %v", err)
	}
}

func TestFixedSoftwareAgentContextBorrowedOpeningContext(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "borrowed.db")
	pool, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	openCtx, cancelOpen := context.WithCancel(context.Background())
	tr, err := OpenBorrowedSQLiteContext(openCtx, pool, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		cancelOpen()
		t.Fatal(err)
	}
	defer tr.Close()
	cancelOpen()
	api, ok := tr.(FixedSoftwareAgentRegistrarContext)
	if !ok {
		t.Fatal("borrowed tracker lacks context capability")
	}
	reg := testFixedSoftwareAgentRegistration()
	if _, err := api.RegisterFixedSoftwareAgentContext(nil, reg); err == nil {
		t.Fatal("nil context accepted")
	}
	cause := errors.New("caller canceled fixed registration")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	if _, err := api.RegisterFixedSoftwareAgentContext(ctx, reg); !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("lost cancellation cause: %v", err)
	}
	first, err := api.RegisterFixedSoftwareAgentContext(context.Background(), reg)
	if err != nil {
		t.Fatalf("live call after canceled opening context: %v", err)
	}
	second, err := tr.RegisterFixedSoftwareAgent(reg)
	if err != nil || first != second {
		t.Fatalf("legacy replay = (%+v, %v), want %+v", second, err, first)
	}
	reg.AgentName = "different"
	if _, err := api.RegisterFixedSoftwareAgentContext(context.Background(), reg); !errors.Is(err, ErrAgentAlreadyExists) {
		t.Fatalf("conflicting agent: %v", err)
	}
}

func TestFixedSoftwareAgentContextRollbackAfterEachInsert(t *testing.T) {
	t.Parallel()
	for _, tc := range loadFixedAgentFixture(t).RollbackCases {
		t.Run(tc.Name, func(t *testing.T) {
			tr, path := newFixedAgentSQLTracker(t)
			defer tr.Close()
			var table string
			switch tc.Boundary {
			case "claim":
				table = "actor_namespace_claims"
			case "agent":
				table = "agents"
			case "software":
				table = "agents_software"
			case "manifest":
				table = "fixed_actor_manifest_entries"
			default:
				t.Fatalf("unknown insert boundary %q", tc.Boundary)
			}
			withRawSQLiteTestConn(t, path, func(conn *rawSQLiteConn) {
				if err := rawExecuteTransient(conn, "CREATE TRIGGER fail_after_fixed_insert AFTER INSERT ON "+table+" BEGIN SELECT RAISE(ABORT, 'injected after insert'); END", nil); err != nil {
					t.Fatal(err)
				}
			})
			api := tr.(FixedSoftwareAgentRegistrarContext)
			reg := testFixedSoftwareAgentRegistration()
			if _, err := api.RegisterFixedSoftwareAgentContext(context.Background(), reg); err == nil {
				t.Fatal("registration succeeded despite injected failure")
			}
			assertFixedAgentRowCounts(t, path, [4]int{})
			withRawSQLiteTestConn(t, path, func(conn *rawSQLiteConn) {
				if err := rawExecuteTransient(conn, "DROP TRIGGER fail_after_fixed_insert", nil); err != nil {
					t.Fatal(err)
				}
			})
			if _, err := api.RegisterFixedSoftwareAgentContext(context.Background(), reg); err != nil {
				t.Fatalf("healthy registration after rollback: %v", err)
			}
			assertFixedAgentRowCounts(t, path, [4]int{1, 1, 1, 1})
		})
	}
}
