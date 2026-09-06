package provenance_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	p "github.com/dayvidpham/provenance"
	"github.com/google/uuid"
	"modernc.org/sqlite"
)

// This adapter observes the real Modernc driver. It does not return canned rows
// or replace SQL: all operations, transactions and rows reach real SQLite.
// The counters are private to one caller-owned pool. The mutex protects records
// from database/sql connection callbacks, not any production invariant.
type assignmentSQLTrace struct {
	mu      sync.Mutex
	entries []*assignmentSQLTraceEntry
}
type assignmentSQLTraceEntry struct {
	sql   string
	rows  int
	query bool
}

func (s *assignmentSQLTrace) record(query string, read bool) *assignmentSQLTraceEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &assignmentSQLTraceEntry{sql: query, query: read}
	s.entries = append(s.entries, e)
	return e
}
func (s *assignmentSQLTrace) reset() { s.mu.Lock(); defer s.mu.Unlock(); s.entries = nil }

type assignmentTraceDriver struct {
	inner driver.Driver
	trace *assignmentSQLTrace
}

func (d assignmentTraceDriver) Open(dsn string) (driver.Conn, error) {
	c, e := d.inner.Open(dsn)
	if e != nil {
		return nil, e
	}
	return &assignmentTraceConn{Conn: c, trace: d.trace}, nil
}

type assignmentTraceConn struct {
	driver.Conn
	trace *assignmentSQLTrace
}

func (c *assignmentTraceConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	e := c.trace.record(q, true)
	r, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	return &assignmentTraceRows{Rows: r, entry: e, trace: c.trace}, nil
}
func (c *assignmentTraceConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.trace.record(q, false)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c *assignmentTraceConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
}
func (c *assignmentTraceConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}
func (c *assignmentTraceConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}

type assignmentTraceRows struct {
	driver.Rows
	entry *assignmentSQLTraceEntry
	trace *assignmentSQLTrace
}

func (r *assignmentTraceRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil {
		r.trace.mu.Lock()
		r.entry.rows++
		r.trace.mu.Unlock()
	}
	return err
}

func TestAssignmentStartsPhysicalQueryEnvelope(t *testing.T) {
	trace := &assignmentSQLTrace{}
	driverName := "assignment-trace-" + uuid.NewString()
	sql.Register(driverName, assignmentTraceDriver{inner: &sqlite.Driver{}, trace: trace})
	db, err := sql.Open(driverName, "file:"+filepath.Join(t.TempDir(), "cost.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tr, err := p.OpenBorrowedSQLite(db, p.WithModelRegistry(p.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	a, b, _ := assignmentQueryFixture(t, tr)
	var starts []p.JournalID
	var ids []p.AssignmentID
	var tasks []p.TaskID
	for i := 0; i < 65; i++ {
		task, err := tr.As(a, b).Create("assignment-query", fmt.Sprintf("task-%d", i), "", p.TaskTypeTask, p.PriorityMedium, p.PhaseUnscoped)
		if err != nil {
			t.Fatal(err)
		}
		id := p.AssignmentID(fmt.Sprintf("assignment-%d", i))
		ids = append(ids, id)
		tasks = append(tasks, task.ID)
		starts = append(starts, assignmentQueryStart(t, tr, a, b, task.ID, id).FactID)
	}
	var lastEnd p.JournalID
	for i, id := range ids {
		result := publicFactApply(t, tr, p.OperationInput{OperationID: p.OperationID(fmt.Sprintf("end-%d", i)), ActorID: a, AuthorityJournalID: &b, CommandDigest: []byte(id), Effects: []p.Effect{{Sort: p.EffectAssignmentEnd, ResultSlot: "end", AssignmentID: id, TaskID: tasks[i], SlotID: p.SlotOwnerResponsibility}}})
		lastEnd = result.FactID
	}
	api := assignmentQueryAPI(t, tr)
	trace.reset()
	page, err := api.QueryAssignmentStarts(p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 64, SnapshotPinned: true, SnapshotMaxJournalID: lastEnd, AfterJournalID: starts[len(starts)-1]}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 0 || page.Next == nil {
		t.Fatalf("65 ends must consume a nonterminal empty prefix: %+v", page)
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	var queries, queryRows, execs, topQueries, topRows, priorQueries, priorRows int
	for _, e := range trace.entries {
		if e.query {
			queries++
			queryRows += e.rows
		} else {
			execs++
		}
		if strings.Contains(e.sql, "UNION SELECT journal_id FROM journal_authorities") {
			topQueries++
			topRows += e.rows
		}
		if strings.Contains(e.sql, "WHERE assignment_id=?1 AND journal_id<?2") {
			priorQueries++
			priorRows += e.rows
		}
	}
	if topQueries != 1 || topRows != 65 || priorQueries != 65 || priorRows != 65 {
		t.Fatalf("actual top=%d/%d prior=%d/%d", topQueries, topRows, priorQueries, priorRows)
	}
	t.Logf("real SQLite driver calls: query=%d fetchedRows=%d exec=%d; top-level=%d queries/%d rows; prior-start=%d queries/%d rows", queries, queryRows, execs, topQueries, topRows, priorQueries, priorRows)
	// Query rows here count rows delivered by the driver, not rows visited by
	// SQLite's UNION/order/joins or marker subqueries. Do not call this total DB
	// cost 65: each diagnostic also probes <=2 same-marker rows inside SQLite.
}

var _ driver.Rows = (*assignmentTraceRows)(nil)
