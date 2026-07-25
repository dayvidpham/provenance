package sqlite

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/google/uuid"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestQueryTaskEventsUsesPoolLeaseIndependentOfHeldScope(t *testing.T) {
	db := openPoolFileDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	appendEventsOp(t, db, boot, actor, task, "pool-read", []opEvent{{
		kind:       journal.EventKindTaskUpdated,
		recordedAt: time.Now().UTC(),
	}})

	held, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bind independent held scope: %v", err)
	}
	defer held.release()
	done := make(chan error, 1)
	go func() {
		_, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("QueryTaskEvents while an independent scope was held: %v", err)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("QueryTaskEvents waited on a held connection instead of leasing an independent pool scope")
	}
	held.release()
	assertAllRuntimeScopesAvailable(t, db, "successful journal read")
}

func TestOwnedReadReplayIntegrityExitsReturnAllLeases(t *testing.T) {
	t.Run("journal read and integrity", func(t *testing.T) {
		db := openPoolFileDB(t)
		actor, task := seedActorAndTask(t, db)
		boot := genesisBoot(t, db, actor)
		appendEventsOp(t, db, boot, actor, task, "lease-success", []opEvent{{
			kind:       journal.EventKindTaskUpdated,
			recordedAt: time.Now().UTC(),
		}})

		if _, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID}); err != nil {
			t.Fatalf("QueryTaskEvents: %v", err)
		}
		assertAllRuntimeScopesAvailable(t, db, "successful QueryTaskEvents")
		if err := db.VerifyIntegrity(); err != nil {
			t.Fatalf("VerifyIntegrity: %v", err)
		}
		assertAllRuntimeScopesAvailable(t, db, "successful VerifyIntegrity")
	})

	t.Run("replay", func(t *testing.T) {
		db := openPoolFileDB(t)
		if _, err := db.ReplayProjections(); err != nil {
			t.Fatalf("ReplayProjections: %v", err)
		}
		assertAllRuntimeScopesAvailable(t, db, "successful ReplayProjections")
	})
}

func TestMigrationWriterPreservesConcurrentWALReadSnapshot(t *testing.T) {
	db, err := Open(t.TempDir()+"/migration-wal.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	system := registerSoftwareActor(t, db, "pool-system")
	boot := genesisBoot(t, db, system)
	legacy := journal.LegacyTaskRow{
		ID:        journal.TaskID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())},
		Status:    journal.TaskStatusOpen,
		CreatedAt: time.Unix(100, 0).UTC(),
		UpdatedAt: time.Unix(200, 0).UTC(),
	}
	if err := db.SeedLegacyTask(legacy); err != nil {
		t.Fatalf("SeedLegacyTask: %v", err)
	}

	reader, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bind reader: %v", err)
	}
	var readErr error
	endRead := sqlitex.Transaction(reader.conn)
	before, err := scopedJournalRowCount(reader.conn)
	if err != nil {
		endRead(&readErr)
		reader.release()
		t.Fatalf("establish read snapshot: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := db.MigrateLegacyBaseline(journal.MigrationInput{
			System: system, BootstrapAuthority: boot, Legacy: []journal.LegacyTaskRow{legacy},
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			endRead(&readErr)
			reader.release()
			t.Fatalf("MigrateLegacyBaseline with WAL reader: %v", err)
		}
	case <-time.After(poolTestTimeout):
		endRead(&readErr)
		reader.release()
		t.Fatal("migration writer did not complete while an independent WAL reader held a snapshot")
	}

	during, err := scopedJournalRowCount(reader.conn)
	if err != nil {
		endRead(&readErr)
		reader.release()
		t.Fatalf("read pinned snapshot after migration: %v", err)
	}
	endRead(&readErr)
	reader.release()
	if readErr != nil {
		t.Fatalf("finish read snapshot: %v", readErr)
	}
	if during != before {
		t.Fatalf("reader snapshot changed during migration: before=%d during=%d", before, during)
	}

	fresh, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bind fresh reader: %v", err)
	}
	after, err := scopedJournalRowCount(fresh.conn)
	fresh.release()
	if err != nil {
		t.Fatalf("read post-migration journal count: %v", err)
	}
	if after <= before {
		t.Fatalf("fresh reader count=%d, want greater than pinned pre-migration count=%d", after, before)
	}
	assertAllRuntimeScopesAvailable(t, db, "successful migration and WAL read")
}

func TestMigrationFaultRollsBackExactDurableSnapshotAndReturnsAllLeases(t *testing.T) {
	db := openPoolFileDB(t)
	system := registerSoftwareActor(t, db, "fault-system")
	boot := genesisBoot(t, db, system)
	base := time.Unix(100, 0).UTC()
	legacy := []journal.LegacyTaskRow{
		{ID: journal.TaskID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}, Status: journal.TaskStatusOpen, CreatedAt: base, UpdatedAt: base},
		{ID: journal.TaskID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}, Status: journal.TaskStatusClosed, CreatedAt: base.Add(time.Second), UpdatedAt: base.Add(2 * time.Second)},
	}
	for _, row := range legacy {
		if err := db.SeedLegacyTask(row); err != nil {
			t.Fatalf("SeedLegacyTask %s: %v", row.ID, err)
		}
	}

	before := snapshotAllDurableTables(t, db)
	beforeTasks := make(map[journal.TaskID]durableTaskTuple, len(legacy))
	for _, row := range legacy {
		beforeTasks[row.ID] = snapshotLegacyTaskTuple(t, db, row.ID)
	}

	_, err := db.AdversarialMigrateWithFault(journal.MigrationInput{
		System: system, BootstrapAuthority: boot, Legacy: legacy,
	}, 1)
	if !errors.Is(err, journal.ErrMigrationFault) {
		t.Fatalf("AdversarialMigrateWithFault error=%v, want ErrMigrationFault", err)
	}
	after := snapshotAllDurableTables(t, db)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("migration fault changed durable table state\nbefore: %#v\nafter:  %#v", before, after)
	}
	for _, row := range legacy {
		afterTask := snapshotLegacyTaskTuple(t, db, row.ID)
		if afterTask != beforeTasks[row.ID] {
			t.Fatalf("legacy task %s changed across rollback: before=%+v after=%+v", row.ID, beforeTasks[row.ID], afterTask)
		}
		assertMigrationOperationAbsent(t, db, journal.MigrationBaselineOperationID(row.ID))
	}
	assertAllRuntimeScopesAvailable(t, db, "migration fault")
}

func TestReplayAndOwnedReadErrorsReturnAllLeases(t *testing.T) {
	t.Run("replay", func(t *testing.T) {
		db := openPoolFileDB(t)
		actor, task := seedActorAndTask(t, db)
		boot := genesisBoot(t, db, actor)
		appendEventsOp(t, db, boot, actor, task, "replay-error", []opEvent{{
			kind:       journal.EventKindTaskUpdated,
			recordedAt: time.Now().UTC(),
		}})
		if err := db.AdversarialCorruptTaskProjection(task, AdversarialFieldStatus, int(journal.TaskStatusClosed)); err != nil {
			t.Fatalf("corrupt projection: %v", err)
		}
		if _, err := db.ReplayProjections(); !errors.Is(err, journal.ErrProjectionDivergence) {
			t.Fatalf("ReplayProjections error=%v, want ErrProjectionDivergence", err)
		}
		assertAllRuntimeScopesAvailable(t, db, "replay error")
	})

	t.Run("journal read", func(t *testing.T) {
		db := openPoolFileDB(t)
		if err := db.AdversarialDropTable(AdversarialDropJournalTable); err != nil {
			t.Fatalf("drop journal: %v", err)
		}
		if _, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID}); err == nil {
			t.Fatal("QueryTaskEvents succeeded after journal was dropped")
		}
		assertAllRuntimeScopesAvailable(t, db, "journal read error")
	})

	t.Run("integrity", func(t *testing.T) {
		db := openPoolFileDB(t)
		seedActorAndTask(t, db)
		if err := db.VerifyIntegrity(); !errors.Is(err, journal.ErrWatermarkMissing) {
			t.Fatalf("VerifyIntegrity error=%v, want ErrWatermarkMissing", err)
		}
		assertAllRuntimeScopesAvailable(t, db, "integrity error")
	})
}

func TestAppendBareJournalRowCloseInterruptsAndDrainsActiveLease(t *testing.T) {
	dbPath := t.TempDir() + "/append-close.db"
	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	actor := registerSoftwareActor(t, db, "append-close-actor")
	before := snapshotAllDurableTables(t, db)

	started := make(chan struct{})
	releaseWriter := make(chan struct{})
	markerName := "p1d_append_started_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var markerOnce, releaseOnce sync.Once
	releaseWriterCallback := func() { releaseOnce.Do(func() { close(releaseWriter) }) }
	setup := takeAllRuntimeScopes(t, db, "configure close marker")
	for _, scope := range setup.scopes {
		if err := scope.conn.CreateFunction(markerName, &zs.FunctionImpl{
			NArgs:         0,
			AllowIndirect: true,
			Scalar: func(zs.Context, []zs.Value) (zs.Value, error) {
				markerOnce.Do(func() { close(started) })
				<-releaseWriter
				return zs.IntegerValue(1), nil
			},
		}); err != nil {
			setup.release()
			t.Fatalf("install scalar marker on %p: %v", scope.conn, err)
		}
	}
	triggerSQL := fmt.Sprintf(`CREATE TRIGGER p1d_append_close_block
		BEFORE INSERT ON journal BEGIN
			SELECT %s();
			SELECT sum(x) FROM (
				WITH RECURSIVE spin(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM spin)
				SELECT x FROM spin
			);
		END`, quoteSQLiteIdentifier(markerName))
	if err := sqlitex.ExecuteTransient(setup.scopes[0].conn, triggerSQL, nil); err != nil {
		setup.release()
		t.Fatalf("install persistent blocking trigger: %v", err)
	}
	setup.release()

	writerDone := make(chan error, 1)
	closeDone := make(chan error, 1)
	readDone := make(chan error, 1)
	writerJoined, closeStarted, closeJoined, readStarted, readJoined := false, false, false, false, false
	t.Cleanup(func() {
		releaseWriterCallback()
		if !writerJoined {
			select {
			case <-writerDone:
				writerJoined = true
			case <-time.After(poolTestTimeout):
				t.Errorf("cleanup did not join AppendBareJournalRow")
			}
		}
		if readStarted && !readJoined {
			select {
			case <-readDone:
				readJoined = true
			case <-time.After(poolTestTimeout):
				t.Errorf("cleanup did not join exported read observer")
			}
		}
		if closeStarted && !closeJoined {
			select {
			case <-closeDone:
				closeJoined = true
			case <-time.After(poolTestTimeout):
				t.Errorf("cleanup did not join Close")
			}
		}
	})
	go func() {
		_, err := db.AppendBareJournalRow(journal.JournalKindDecision, actor, time.Unix(123, 0).UTC())
		writerDone <- err
	}()
	select {
	case <-started:
	case err := <-writerDone:
		writerJoined = true
		t.Fatalf("AppendBareJournalRow exited before scalar start marker: %v", err)
	case <-time.After(poolTestTimeout):
		t.Fatal("AppendBareJournalRow did not enter the persistent trigger")
	}

	closeStarted = true
	go func() { closeDone <- db.Close() }()
	readStarted = true
	go func() {
		for {
			_, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID})
			if err != nil && strings.Contains(err.Error(), "QueryTaskEvents: lease connection:") {
				readDone <- err
				return
			}
		}
	}()
	select {
	case err := <-readDone:
		readJoined = true
		if !strings.Contains(err.Error(), "pool closed") {
			t.Fatalf("read acquisition after Close began returned %v, want pool closed", err)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("exported read did not observe pool closure")
	}
	select {
	case err := <-closeDone:
		closeJoined = true
		t.Fatalf("Close returned while the scalar callback still held the writer lease: %v", err)
	default:
	}

	releaseWriterCallback()
	var writerErr error
	select {
	case writerErr = <-writerDone:
		writerJoined = true
	case <-time.After(poolTestTimeout):
		t.Fatal("AppendBareJournalRow did not unwind after its held callback was released")
	}
	if code := zs.ErrCode(writerErr); code != zs.ResultInterrupt {
		t.Fatalf("AppendBareJournalRow error=%v (%v), want SQLITE_INTERRUPT", writerErr, code)
	}
	select {
	case err := <-closeDone:
		closeJoined = true
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("Close did not drain after AppendBareJournalRow returned its lease")
	}

	after := snapshotAllDurableTablesFromPath(t, dbPath)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("interrupted AppendBareJournalRow changed durable state\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func assertAllRuntimeScopesAvailable(t *testing.T, db *DB, after string) {
	t.Helper()
	scopes := takeAllRuntimeScopes(t, db, "after "+after)
	scopes.release()
}

type heldRuntimeScopes struct {
	scopes []*connScope
	cancel context.CancelFunc
}

func (held *heldRuntimeScopes) release() {
	for _, scope := range held.scopes {
		scope.release()
	}
	held.cancel()
}

func takeAllRuntimeScopes(t *testing.T, db *DB, operation string) *heldRuntimeScopes {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), poolTestTimeout)
	scopes := make([]*connScope, 0, runtimePoolSize)
	seen := make(map[*zs.Conn]struct{}, runtimePoolSize)
	for len(scopes) < runtimePoolSize {
		scope, err := db.bindScope(ctx, projectionTargetLive)
		if err != nil {
			releaseScopes(scopes)
			cancel()
			t.Fatalf("acquire runtime scope %d/%d %s: %v", len(scopes)+1, runtimePoolSize, operation, err)
		}
		if _, duplicate := seen[scope.conn]; duplicate {
			scope.release()
			releaseScopes(scopes)
			cancel()
			t.Fatalf("pool returned duplicate simultaneous connection %p %s", scope.conn, operation)
		}
		seen[scope.conn] = struct{}{}
		scopes = append(scopes, scope)
	}
	return &heldRuntimeScopes{scopes: scopes, cancel: cancel}
}

func releaseScopes(scopes []*connScope) {
	for _, scope := range scopes {
		scope.release()
	}
}

type durableTableSnapshot map[string][]string

func snapshotAllDurableTables(t *testing.T, db *DB) durableTableSnapshot {
	t.Helper()
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bind durable snapshot scope: %v", err)
	}
	defer scope.release()
	return snapshotAllDurableTablesConn(t, scope.conn)
}

func snapshotAllDurableTablesFromPath(t *testing.T, dbPath string) durableTableSnapshot {
	t.Helper()
	conn, err := zs.OpenConn(dbPath, zs.OpenReadOnly)
	if err != nil {
		t.Fatalf("open durable snapshot after Close: %v", err)
	}
	defer conn.Close()
	return snapshotAllDurableTablesConn(t, conn)
}

func snapshotAllDurableTablesConn(t *testing.T, conn *zs.Conn) durableTableSnapshot {
	t.Helper()
	var tables []string
	if err := sqlitex.Execute(conn, "SELECT name FROM main.sqlite_schema WHERE type=?1 AND name NOT LIKE ?2 ORDER BY name", &sqlitex.ExecOptions{
		Args: []any{"table", "sqlite_%"},
		ResultFunc: func(stmt *zs.Stmt) error {
			tables = append(tables, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatalf("enumerate durable tables: %v", err)
	}
	snapshot := make(durableTableSnapshot, len(tables))
	for _, table := range tables {
		var rows []string
		query := "SELECT * FROM main." + quoteSQLiteIdentifier(table)
		if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			rows = append(rows, encodeSQLiteRow(stmt))
			return nil
		}}); err != nil {
			t.Fatalf("snapshot durable table %q: %v", table, err)
		}
		sort.Strings(rows)
		snapshot[table] = rows
	}
	return snapshot
}

type durableTaskTuple struct {
	Row       string
	Watermark string
}

func snapshotLegacyTaskTuple(t *testing.T, db *DB, task journal.TaskID) durableTaskTuple {
	t.Helper()
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bind legacy task snapshot: %v", err)
	}
	defer scope.release()
	found := false
	var tuple durableTaskTuple
	if err := sqlitex.Execute(scope.conn, "SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM tasks WHERE id=?1", &sqlitex.ExecOptions{
		Args: []any{task.String()},
		ResultFunc: func(stmt *zs.Stmt) error {
			found = true
			tuple.Row = encodeSQLiteRow(stmt)
			tuple.Watermark = encodeSQLiteValue(stmt, 14)
			return nil
		},
	}); err != nil {
		t.Fatalf("snapshot legacy task %s: %v", task, err)
	}
	if !found {
		t.Fatalf("snapshot legacy task %s: row missing", task)
	}
	return tuple
}

func assertMigrationOperationAbsent(t *testing.T, db *DB, operation journal.OperationID) {
	t.Helper()
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bind migration OperationID check: %v", err)
	}
	defer scope.release()
	var count int
	if err := sqlitex.Execute(scope.conn, "SELECT COUNT(*) FROM journal_operations WHERE operation_id=?1", &sqlitex.ExecOptions{
		Args: []any{string(operation)},
		ResultFunc: func(stmt *zs.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("query migration OperationID %q: %v", operation, err)
	}
	if count != 0 {
		t.Fatalf("migration OperationID %q survived rollback (%d rows)", operation, count)
	}
}

func encodeSQLiteRow(stmt *zs.Stmt) string {
	values := make([]string, stmt.ColumnCount())
	for i := range values {
		values[i] = encodeSQLiteValue(stmt, i)
	}
	return strings.Join(values, "|")
}

func encodeSQLiteValue(stmt *zs.Stmt, column int) string {
	switch stmt.ColumnType(column) {
	case zs.TypeNull:
		return "n:"
	case zs.TypeInteger:
		return fmt.Sprintf("i:%d", stmt.ColumnInt64(column))
	case zs.TypeFloat:
		return fmt.Sprintf("f:%016x", math.Float64bits(stmt.ColumnFloat(column)))
	case zs.TypeText:
		return "t:" + base64.StdEncoding.EncodeToString([]byte(stmt.ColumnText(column)))
	case zs.TypeBlob:
		return "b:" + base64.StdEncoding.EncodeToString(readBlob(stmt, column))
	default:
		panic("unknown SQLite column type")
	}
}

func quoteSQLiteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func scopedJournalRowCount(conn *zs.Conn) (int, error) {
	var count int
	err := sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *zs.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	})
	return count, err
}
