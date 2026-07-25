package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const applyPoolTestTimeout = 10 * time.Second

// transaction_ownership_test.go tests the BEGIN IMMEDIATE write-ownership
// semantics for standalone Apply calls and related conflict/retry behavior.

// TestApplyStandaloneBeginImmediateWritesAndCommits verifies that standalone
// Apply uses BEGIN IMMEDIATE and commits successfully on a file-backed database.
func TestApplyStandaloneBeginImmediateWritesAndCommits(t *testing.T) {
	t.Parallel()
	dbPath := t.TempDir() + "/tx_ownership.db"
	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)

	opID := journal.OperationID("standalone-write-" + uuid.Must(uuid.NewV7()).String())
	res, err := db.Apply(journal.OperationInput{
		OperationID:        opID,
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("cmd"),
		Effects:            []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "provenance.test.event"}},
	})
	if err != nil || res.Kind != journal.CommittedExact {
		t.Fatalf("standalone Apply: err=%v kind=%v", err, res.Kind)
	}
	if res.AnchorJournalID <= 0 {
		t.Fatalf("standalone Apply: AnchorJournalID=%d", res.AnchorJournalID)
	}
}

func TestApplyOwnsOnePoolLeaseWhileWALReadersProceedAndWritersAreBounded(t *testing.T) {
	dbPath := t.TempDir() + "/apply_pool_ownership.db"
	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	before := journalRowCount(t, db)

	writer := takeApplyTestScope(t, db)
	applyConn := takeApplyTestScope(t, db)
	// The probe owns the reader and all remaining runtime leases derived from
	// the scopes already held here. It keeps those leases held while Apply owns
	// the trigger-carrying connection and while the contender return is proved.
	contenderProbe := newApplyContenderLeaseProbe(t, db, writer, applyConn)
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	if err := applyConn.conn.CreateFunction("apply_test_barrier", &zs.FunctionImpl{
		NArgs: 0,
		Scalar: func(zs.Context, []zs.Value) (zs.Value, error) {
			enteredOnce.Do(func() { close(entered) })
			<-release
			return zs.IntegerValue(0), nil
		},
	}); err != nil {
		t.Fatalf("register Apply barrier: %v", err)
	}
	if err := sqlitex.ExecuteTransient(applyConn.conn, `CREATE TEMP TRIGGER apply_test_barrier_trigger
		BEFORE INSERT ON main.journal WHEN NEW.kind_id = 0
		BEGIN SELECT apply_test_barrier(); END`, nil); err != nil {
		t.Fatalf("create Apply barrier trigger: %v", err)
	}
	applyConn.release()

	applyDone := make(chan error, 1)
	go func() {
		_, applyErr := db.Apply(journal.OperationInput{
			OperationID: "pooled-owner", ActorID: actor, AuthorityJournalID: &boot,
			CommandDigest: []byte("pooled-owner"),
			Effects:       []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "pool.owner"}},
		})
		applyDone <- applyErr
	}()
	waitApplySignal(t, "Apply transaction barrier", entered)

	var during int
	if err := sqlitex.Execute(contenderProbe.readerConn(), "SELECT COUNT(*) FROM journal", &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
		during = stmt.ColumnInt(0)
		return nil
	}}); err != nil {
		t.Fatalf("independent WAL reader during Apply: %v", err)
	}
	if during != before {
		t.Fatalf("WAL reader observed uncommitted rows: got %d, want %d", during, before)
	}
	if err := sqlitex.ExecuteTransient(writer.conn, "PRAGMA busy_timeout=1", nil); err != nil {
		t.Fatalf("bound competing writer timeout: %v", err)
	}
	writer.release()
	_, busyErr := db.Apply(journal.OperationInput{
		OperationID: "pooled-contender", ActorID: actor, AuthorityJournalID: &boot,
		CommandDigest: []byte("pooled-contender"),
		Effects:       []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "pool.contender"}},
	})
	if code := zs.ErrCode(busyErr); code != zs.ResultBusy {
		t.Fatalf("competing Apply error=%v code=%v, want SQLITE_BUSY", busyErr, code)
	}
	// Apply owns applyConn while it is stopped at the trigger. The probe still
	// owns the reader and every spare scope, so its bind is the only operation
	// that can observe a free lease. A successful bind therefore proves the
	// SQLITE_BUSY contender returned its lease; a leaked contender makes this
	// bind fail loudly instead of letting an early reader release make it
	// vacuous. The probe owns the bind and keeps its holders until this point.
	contenderProbe.proveContenderReturned(t)

	close(release)
	if err := waitApplyError(t, "barrier Apply", applyDone); err != nil {
		t.Fatalf("barrier Apply: %v", err)
	}
	contenderProbe.releaseAfterApply(t)
	assertApplyPoolLeasesAvailable(t, db)
}

// applyContenderLeaseProbe owns the reader and spare leases that must stay
// held while the barrier Apply owns its trigger-carrying connection. It exposes
// the reader connection for the WAL observation, but keeps scope release inside
// the probe protocol so a holder cannot be released before the contender bind.
type applyContenderLeaseProbe struct {
	db     *DB
	reader *connScope
	spares []*connScope
	probed bool
}

func newApplyContenderLeaseProbe(t *testing.T, db *DB, held ...*connScope) *applyContenderLeaseProbe {
	t.Helper()
	reader := takeApplyTestScope(t, db)
	t.Cleanup(reader.release)
	held = append(held, reader)
	probe := &applyContenderLeaseProbe{
		db:     db,
		reader: reader,
		spares: holdRemainingApplyLeases(t, db, held...),
	}
	t.Cleanup(probe.releaseHeld)
	return probe
}

func (probe *applyContenderLeaseProbe) readerConn() *zs.Conn {
	return probe.reader.conn
}

func (probe *applyContenderLeaseProbe) proveContenderReturned(t *testing.T) {
	t.Helper()
	returnedContender := takeApplyTestScope(t, probe.db)
	returnedContender.release()
	probe.probed = true
}

func (probe *applyContenderLeaseProbe) releaseAfterApply(t *testing.T) {
	t.Helper()
	if !probe.probed {
		t.Fatal("releaseAfterApply: cannot release reader and spare leases before the contender-return probe succeeds")
	}
	probe.releaseHeld()
}

func (probe *applyContenderLeaseProbe) releaseHeld() {
	probe.reader.release()
	releaseScopes(probe.spares)
}

// holdRemainingApplyLeases takes every runtime lease the caller has not already
// taken, so that once the caller returns one of its own scopes the pool's free
// capacity is exactly one connection.
//
// The already-held count is DERIVED from the scopes the caller passes in, never
// restated as a literal: a restated count silently drifts when a scope is added
// or dropped above the call site, and drift either over-subscribes the pool
// (turning an assertion into a bindScope timeout) or leaves a second lease free
// (making the "the barrier connection is the only lease Apply can acquire"
// precondition vacuously true while the test still passes).
func holdRemainingApplyLeases(t *testing.T, db *DB, held ...*connScope) []*connScope {
	t.Helper()
	remaining := runtimePoolSize - len(held)
	if remaining <= 0 {
		t.Fatalf(
			"holdRemainingApplyLeases: caller already holds %d of the %d runtime leases, so no lease is left to hold; "+
				"the pool would already be fully subscribed and the 'exactly one free lease' precondition cannot be established, "+
				"making the probe that depends on it vacuous; "+
				"fix: drop a takeApplyTestScope above this call site or raise runtimePoolSize",
			len(held), runtimePoolSize,
		)
	}
	spares := make([]*connScope, 0, remaining)
	for range remaining {
		spares = append(spares, takeApplyTestScope(t, db))
	}
	return spares
}

func TestApplyRollsBackBeforeReturningFailedLease(t *testing.T) {
	dbPath := t.TempDir() + "/apply_pool_rollback.db"
	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	before := journalRowCount(t, db)

	held := []*connScope{takeApplyTestScope(t, db), takeApplyTestScope(t, db)}
	for _, scope := range held {
		defer scope.release()
	}
	applyConn := takeApplyTestScope(t, db)
	defer applyConn.release()
	// Hold the rest of the pool so the failure-trigger connection is the only
	// lease the Apply under test can acquire once applyConn is returned. The
	// outstanding set is built from the scopes actually taken above rather than
	// restated as a count, so the helper cannot drift out of agreement with it.
	outstanding := make([]*connScope, 0, len(held)+1)
	outstanding = append(outstanding, held...)
	outstanding = append(outstanding, applyConn)
	spares := holdRemainingApplyLeases(t, db, outstanding...)
	defer releaseScopes(spares)
	if err := sqlitex.ExecuteTransient(applyConn.conn, `CREATE TEMP TRIGGER apply_test_fail_trigger
		BEFORE INSERT ON main.journal WHEN NEW.kind_id != 0
		BEGIN SELECT RAISE(ABORT, 'injected subordinate-row failure'); END`, nil); err != nil {
		t.Fatalf("create Apply failure trigger: %v", err)
	}
	applyConn.release()

	_, applyErr := db.Apply(journal.OperationInput{
		OperationID: "pooled-rollback", ActorID: actor, AuthorityJournalID: &boot,
		CommandDigest: []byte("pooled-rollback"),
		Effects:       []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "pool.rollback"}},
	})
	if applyErr == nil {
		t.Fatal("Apply with injected subordinate-row failure succeeded")
	}
	for _, scope := range held {
		scope.release()
	}
	releaseScopes(spares)
	if after := journalRowCount(t, db); after != before {
		t.Fatalf("failed Apply left durable delta: before=%d after=%d", before, after)
	}
	committed, err := db.LookupCommitted("pooled-rollback")
	if err != nil || committed.Kind != journal.CommittedAbsent {
		t.Fatalf("failed Apply lookup=%+v err=%v, want absent", committed, err)
	}
	assertApplyPoolLeasesAvailable(t, db)
}

func takeApplyTestScope(t *testing.T, db *DB) *connScope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), applyPoolTestTimeout)
	t.Cleanup(cancel)
	scope, err := db.bindScope(ctx, projectionTargetLive)
	if err != nil {
		t.Fatalf("take Apply test scope: %v", err)
	}
	return scope
}

func assertApplyPoolLeasesAvailable(t *testing.T, db *DB) {
	t.Helper()
	scopes := make([]*connScope, 0, runtimePoolSize)
	for range runtimePoolSize {
		scopes = append(scopes, takeApplyTestScope(t, db))
	}
	for _, scope := range scopes {
		scope.release()
	}
}

func waitApplySignal(t *testing.T, operation string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(applyPoolTestTimeout):
		t.Fatalf("%s did not occur within %v", operation, applyPoolTestTimeout)
	}
}

func waitApplyError(t *testing.T, operation string, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(applyPoolTestTimeout):
		t.Fatalf("%s did not finish within %v", operation, applyPoolTestTimeout)
		return nil
	}
}

// TestApplyConcurrentCurrentFactHasOneWinnerOneDomainLoser verifies that when
// two goroutines race a CurrentFact condition, one wins and the other receives
// a typed ConditionFailure (not BUSY).
func TestApplyConcurrentCurrentFactHasOneWinnerOneDomainLoser(t *testing.T) {
	t.Parallel()
	dbPath := t.TempDir() + "/concurrent_condition.db"
	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)

	decisionKind := journal.DecisionKind("fixture.concurrent.v1")
	decOp := journal.OperationID("dec-base-" + uuid.Must(uuid.NewV7()).String())
	if _, err := db.Apply(journal.OperationInput{
		OperationID: decOp, ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("cmd-dec"),
		Effects: []journal.Effect{{Sort: journal.EffectDecision, DecisionKind: decisionKind, Payload: []byte(`{}`)}},
	}); err != nil {
		t.Fatalf("Apply base decision: %v", err)
	}

	k, args, err := buildSelectorArgs(journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: decisionKind,
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	}, 0)
	if err != nil {
		t.Fatalf("build base decision selector: %v", err)
	}
	scope := takeApplyTestScope(t, db)
	baseDecisionJID, found, err := latestFactSelector(scope, k, args)
	scope.release()
	if err != nil {
		t.Fatalf("query base decision: %v", err)
	}
	if !found {
		t.Fatal("base decision not found")
	}

	type result struct {
		res journal.CommittedResult
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)

	makeRacer := func(opSuffix string) func() {
		return func() {
			<-start
			opID := journal.OperationID("race-" + opSuffix)
			res, err := db.Apply(journal.OperationInput{
				OperationID:        opID,
				ActorID:            actor,
				AuthorityJournalID: &boot,
				CommandDigest:      []byte("cmd-race-" + opSuffix),
				Conditions: []journal.Condition{{
					Kind: journal.ConditionCurrentFact,
					Selector: journal.FactSelector{
						Kind: journal.FactDecision, DecisionKind: decisionKind,
						Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
					},
					AssertedJournalID: baseDecisionJID,
				}},
				Effects: []journal.Effect{
					{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "provenance.test.event"},
					{Sort: journal.EffectDecision, DecisionKind: decisionKind, Payload: []byte(`{}`)},
				},
			})
			results <- result{res, err}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); makeRacer("gA")() }()
	go func() { defer wg.Done(); makeRacer("gB")() }()
	close(start)
	wg.Wait()
	close(results)

	var wins, condFails int
	for r := range results {
		if r.err == nil && r.res.Kind == journal.CommittedExact {
			wins++
		} else if errors.Is(r.err, journal.ErrConditionFailed) {
			condFails++
		} else {
			t.Errorf("unexpected result: err=%v kind=%v", r.err, r.res.Kind)
		}
	}
	if wins+condFails != 2 {
		t.Fatalf("expected wins+condFails=2, got wins=%d condFails=%d", wins, condFails)
	}
	if wins == 0 {
		t.Fatal("no winner: at least one Apply must succeed")
	}
}

// TestApplyAcquisitionBusyIsDistinctFromDomainFailure verifies that typed
// errors remain distinct: ConditionFailure is not a BUSY error.
func TestApplyAcquisitionBusyIsDistinctFromDomainFailure(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)

	// Successful Apply must not produce ErrConditionFailed.
	if _, err := db.Apply(journal.OperationInput{
		OperationID: "acq-test-1", ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("cmd"),
		Effects: []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "provenance.test.event"}},
	}); err != nil {
		t.Fatalf("successful Apply: %v", err)
	}

	// A condition failure must not be BUSY.
	_, condErr := db.Apply(journal.OperationInput{
		OperationID:        "acq-test-cond",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("cmd2"),
		Conditions: []journal.Condition{{
			Kind:              journal.ConditionExactFact,
			Selector:          journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.v1", Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}},
			AssertedJournalID: 9999,
		}},
		Effects: []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "provenance.test.event"}},
	})
	if !errors.Is(condErr, journal.ErrConditionFailed) {
		t.Fatalf("condition failure: expected ErrConditionFailed, got: %v", condErr)
	}
	// Must not look like a BUSY error.
	if condErr != nil && (condErr.Error() == "BUSY" || condErr.Error() == "SQLITE_BUSY") {
		t.Fatalf("condition failure mistaken as BUSY: %v", condErr)
	}
}

// TestApplyExactRetryReturnsOriginalResultZeroDelta verifies that an exact
// retry returns the original committed result and appends zero new journal rows.
func TestApplyExactRetryReturnsOriginalResultZeroDelta(t *testing.T) {
	t.Parallel()
	dbPath := t.TempDir() + "/retry_zero_delta.db"
	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)

	in := journal.OperationInput{
		OperationID:        "retry-zero-delta",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("cmd-retry"),
		Effects:            []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "provenance.test.created", ResultSlot: "evt"}},
	}
	res1, err := db.Apply(in)
	if err != nil || res1.Kind != journal.CommittedExact {
		t.Fatalf("first Apply: err=%v kind=%v", err, res1.Kind)
	}
	before := journalRowCount(t, db)

	res2, err2 := db.Apply(in)
	if err2 != nil || res2.Kind != journal.CommittedExact {
		t.Fatalf("retry Apply: err=%v kind=%v", err2, res2.Kind)
	}
	if !res2.ShortCircuited {
		t.Fatal("retry Apply: ShortCircuited must be true")
	}
	if res2.AnchorJournalID != res1.AnchorJournalID {
		t.Fatalf("retry Apply: AnchorJournalID mismatch: first=%d retry=%d", res1.AnchorJournalID, res2.AnchorJournalID)
	}
	if after := journalRowCount(t, db); after != before {
		t.Fatalf("retry Apply: journal changed: before=%d after=%d", before, after)
	}
}

// TestApplyChangedActorReturnsConflictWithoutWrites verifies ConflictActor axis
// and zero persisted delta.
func TestApplyChangedActorReturnsConflictWithoutWrites(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)

	actor2ID := actor
	actor2ID.UUID = uuid.Must(uuid.NewV7())
	scope := takeApplyTestScope(t, db)
	if err := sqlitex.Execute(scope.conn, `INSERT OR IGNORE INTO agents (id, kind_id) VALUES (?1, ?2)`,
		&sqlitex.ExecOptions{Args: []any{actor2ID.String(), int(ptypes.AgentKindSoftware)}}); err != nil {
		scope.release()
		t.Fatalf("seed conflicting actor: %v", err)
	}
	if err := sqlitex.Execute(scope.conn, `INSERT OR IGNORE INTO agents_software (agent_id, name, version, source) VALUES (?1,?2,?3,?4)`,
		&sqlitex.ExecOptions{Args: []any{actor2ID.String(), "test2", "0", "test"}}); err != nil {
		scope.release()
		t.Fatalf("seed conflicting software actor: %v", err)
	}
	scope.release()

	in := journal.OperationInput{
		OperationID:        "conflict-actor-test",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("cmd-c"),
		Effects:            []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "provenance.test.event"}},
	}
	if _, err := db.Apply(in); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	before := journalRowCount(t, db)

	in2 := in
	in2.ActorID = actor2ID
	res, err2 := db.Apply(in2)
	if err2 == nil {
		t.Fatal("changed actor: Apply should fail")
	}
	if !errors.Is(err2, journal.ErrOperationConflict) {
		t.Fatalf("changed actor: expected ErrOperationConflict, got: %v", err2)
	}
	var oc *journal.OperationConflict
	if !errors.As(err2, &oc) || oc.Axis != journal.ConflictActor || oc.Index != -1 {
		t.Fatalf("changed actor: OperationConflict=%+v err=%v", oc, err2)
	}
	if res.Kind != journal.CommittedConflict || res.Conflict == nil {
		t.Fatalf("changed actor: result not CommittedConflict: %+v", res)
	}
	if after := journalRowCount(t, db); after != before {
		t.Fatalf("changed actor: wrote rows, want 0 delta: before=%d after=%d", before, after)
	}
}

// TestSchemaMigrationFreshOpenWorks verifies that opening a fresh database
// applies the complete schema including journal_activity_creations.
func TestSchemaMigrationFreshOpenWorks(t *testing.T) {
	t.Parallel()
	dbPath := t.TempDir() + "/fresh_migration.db"
	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open fresh: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// journal_activity_creations must exist.
	scope := takeApplyTestScope(t, db)
	var tableExists bool
	if err := sqlitex.Execute(scope.conn,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='journal_activity_creations'`,
		&sqlitex.ExecOptions{ResultFunc: func(*zs.Stmt) error { tableExists = true; return nil }}); err != nil {
		scope.release()
		t.Fatalf("check table: %v", err)
	}
	scope.release()
	if !tableExists {
		t.Fatal("journal_activity_creations table not created on fresh Open")
	}

	// Supported migration: reopening the same database must succeed.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = db2.Close()
}

// TestJournalActivityCreationsSchemaConstraints verifies static SQL constraints
// on the journal_activity_creations table: FK to activities and UNIQUE activity_id.
func TestJournalActivityCreationsSchemaConstraints(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	scope := takeApplyTestScope(t, db)
	defer scope.release()

	// Get a valid journal_id (genesis bootstrap operation anchor).
	var validJournalID int64
	_ = sqlitex.Execute(scope.conn, `SELECT journal_id FROM journal LIMIT 1`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			validJournalID = stmt.ColumnInt64(0)
			return nil
		}})

	// FK constraint: activity_id must reference activities.id.
	err := sqlitex.Execute(scope.conn,
		`INSERT INTO journal_activity_creations (journal_id, activity_id) VALUES (?1, ?2)`,
		&sqlitex.ExecOptions{Args: []any{validJournalID, "nonexistent--00000000-0000-0000-0000-000000000001"}})
	if err == nil {
		t.Error("journal_activity_creations FK constraint not enforced: non-existent activity_id should fail")
	}
}
