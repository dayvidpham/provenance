package provenance

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type startupFixture struct {
	task, target                                                                 TaskID
	comment                                                                      CommentID
	anchor, event1, event2                                                       JournalID
	bootstrap, supportAnchor, assignmentStart, assignmentEnd, decision, evidence JournalID
}

type startupBaseline struct {
	path    string
	bytes   []byte
	digest  [sha256.Size]byte
	fixture startupFixture
}

type startupCorruptionHandle struct {
	conn *sqlite.Conn
}

type startupCorruptionCase func(*testing.T, *startupCorruptionHandle, startupFixture)

func buildStartupFixture(t *testing.T, path string) (Tracker, startupFixture) {
	t.Helper()
	tr, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := tr.RegisterSoftwareAgent("matrix", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tr.RegisterSoftwareAgent("matrix", "other", "1", "test"); err != nil {
		t.Fatal(err)
	}
	gen, err := tr.Journal().Apply(OperationInput{OperationID: "matrix-genesis", ActorID: actor.ID, CommandDigest: []byte("c"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}})
	if err != nil {
		t.Fatal(err)
	}
	boot, _ := slotJournalID(gen, "authority")
	s := tr.As(actor.ID, boot)
	task, err := s.Create("matrix", "title", "description", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID(pinnedCreateOperationID(2)))
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.Create("matrix", "target", "target", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID(pinnedCreateOperationID(3)))
	if err != nil {
		t.Fatal(err)
	}
	notes := "notes"
	if _, err = s.Update(task.ID, UpdateFields{Notes: &notes}, WithOperationID("matrix-update")); err != nil {
		t.Fatal(err)
	}
	if err = s.AddEdge(task.ID, target.ID.String(), EdgeDerivedFrom); err != nil {
		t.Fatal(err)
	}
	if err = s.AddLabel(task.ID, "label"); err != nil {
		t.Fatal(err)
	}
	comment, err := s.AddComment(task.ID, actor.ID, "body")
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := TaskContext(task.ID)
	res, err := tr.Journal().Apply(OperationInput{OperationID: "matrix-events", ActorID: actor.ID, AuthorityJournalID: &boot, CommandDigest: []byte("c"), RecordedAt: 123, Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task.ID, EventKind: "matrix.event", Contexts: []EventContext{ctx}, ResultSlot: "one"}, {Sort: EffectTaskEvent, TaskID: task.ID, EventKind: "matrix.other", ResultSlot: "two"}}})
	if err != nil {
		t.Fatal(err)
	}
	support, err := tr.Journal().Apply(OperationInput{OperationID: "matrix-support", ActorID: actor.ID, AuthorityJournalID: &boot, CommandDigest: []byte("support"), Effects: []Effect{{Sort: EffectAssignmentStart, TaskID: target.ID, AssignmentID: "matrix-assignment", SlotID: SlotOwnerResponsibility, Occupant: actor.ID, ResultSlot: "start"}, {Sort: EffectAssignmentEnd, TaskID: target.ID, AssignmentID: "matrix-assignment", SlotID: SlotOwnerResponsibility, ResultSlot: "end"}, {Sort: EffectDecision, TaskID: task.ID, DecisionKind: "matrix.decision", Payload: []byte(`{"decision":1}`), ResultSlot: "decision"}, {Sort: EffectEvidence, TaskID: task.ID, EvidenceKind: "matrix.evidence", ContentDigest: []byte{1, 2}, Payload: []byte(`{"evidence":1}`), ResultSlot: "evidence"}}})
	if err != nil {
		t.Fatal(err)
	}
	start, _ := slotJournalID(support, "start")
	end, _ := slotJournalID(support, "end")
	decision, _ := slotJournalID(support, "decision")
	evidence, _ := slotJournalID(support, "evidence")
	return tr, startupFixture{task: task.ID, target: target.ID, comment: comment.ID, anchor: res.AnchorJournalID, event1: res.EmittedEvents[0], event2: res.EmittedEvents[1], bootstrap: boot, supportAnchor: support.AnchorJournalID, assignmentStart: start, assignmentEnd: end, decision: decision, evidence: evidence}
}

func checkpointAndCloseStartupFixture(t *testing.T, tr Tracker) {
	t.Helper()
	db := tr.(*sqliteTracker).db
	db.Lock()
	checkpointRows := 0
	busy, logFrames, checkpointedFrames := 0, 0, 0
	err := sqlitex.ExecuteTransient(db.Conn(), `PRAGMA wal_checkpoint(TRUNCATE)`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		checkpointRows++
		busy = stmt.ColumnInt(0)
		logFrames = stmt.ColumnInt(1)
		checkpointedFrames = stmt.ColumnInt(2)
		return nil
	}})
	db.Unlock()
	if err != nil {
		t.Fatalf("checkpoint startup baseline before close: %v", err)
	}
	if checkpointRows != 1 || busy != 0 || logFrames != checkpointedFrames {
		t.Fatalf("checkpoint startup baseline returned rows/busy/log/checkpointed=%d/%d/%d/%d, want 1/0/N/N", checkpointRows, busy, logFrames, checkpointedFrames)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("close checkpointed startup baseline: %v", err)
	}
}

func requireNoSQLiteSidecars(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Fatalf("closed immutable SQLite baseline still requires sidecar %q", path+suffix)
		} else if !os.IsNotExist(err) {
			t.Fatalf("inspect closed SQLite baseline sidecar %q: %v", path+suffix, err)
		}
	}
}

func requireValidStartupFixtureIdentity(t *testing.T, fixture startupFixture) {
	t.Helper()
	if fixture.task == (TaskID{}) || fixture.target == (TaskID{}) || fixture.task == fixture.target || fixture.comment == (CommentID{}) {
		t.Fatalf("startup baseline has invalid task/comment fixture identity: %#v", fixture)
	}
	journalIDs := []JournalID{fixture.anchor, fixture.event1, fixture.event2, fixture.bootstrap, fixture.supportAnchor, fixture.assignmentStart, fixture.assignmentEnd, fixture.decision, fixture.evidence}
	seen := make(map[JournalID]struct{}, len(journalIDs))
	for _, id := range journalIDs {
		if id == 0 {
			t.Fatalf("startup baseline has zero journal fixture identity: %#v", fixture)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("startup baseline reuses journal fixture identity %d: %#v", id, fixture)
		}
		seen[id] = struct{}{}
	}
}

func buildValidatedStartupBaseline(t *testing.T, path string, builds *int) startupBaseline {
	t.Helper()
	(*builds)++
	tr, fixture := buildStartupFixture(t, path)
	requireValidStartupFixtureIdentity(t, fixture)
	if err := tr.Journal().VerifyIntegrity(); err != nil {
		t.Fatalf("verify startup baseline integrity after fixture writes: %v", err)
	}
	checkpointAndCloseStartupFixture(t, tr)
	requireNoSQLiteSidecars(t, path)

	validated, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("production validation rejected closed startup baseline: %v", err)
	}
	if err := validated.Journal().VerifyIntegrity(); err != nil {
		t.Fatalf("verify startup baseline integrity after production reopen: %v", err)
	}
	checkpointAndCloseStartupFixture(t, validated)
	requireNoSQLiteSidecars(t, path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read validated closed startup baseline: %v", err)
	}
	baseline := startupBaseline{path: path, bytes: data, digest: sha256.Sum256(data), fixture: fixture}

	copyPath := filepath.Join(t.TempDir(), "validation-copy.sqlite")
	writeStartupBaselineCopy(t, baseline, copyPath)
	copied, err := OpenSQLite(copyPath)
	if err != nil {
		t.Fatalf("production validation rejected copied startup baseline: %v", err)
	}
	checkpointAndCloseStartupFixture(t, copied)
	requireNoSQLiteSidecars(t, copyPath)
	copiedBytes, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("read production-validated startup baseline copy: %v", err)
	}
	if !bytes.Equal(copiedBytes, baseline.bytes) {
		t.Fatal("production validation changed copied startup baseline main database bytes")
	}
	return baseline
}

func writeStartupBaselineCopy(t *testing.T, baseline startupBaseline, path string) {
	t.Helper()
	if path == baseline.path {
		t.Fatal("startup baseline copy path aliases immutable baseline")
	}
	if err := os.WriteFile(path, baseline.bytes, 0o600); err != nil {
		t.Fatalf("write private startup baseline copy: %v", err)
	}
}

// corruptSQL and corruptDDL remain the production-open corruption helpers used
// by canonical tests whose contract includes a successful production open.
func corruptSQL(t *testing.T, tr Tracker, statement string, args ...any) {
	t.Helper()
	db := tr.(*sqliteTracker).db
	db.Lock()
	defer db.Unlock()
	if err := sqlitex.Execute(db.Conn(), `PRAGMA foreign_keys=OFF`, nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.Execute(db.Conn(), `PRAGMA ignore_check_constraints=ON`, nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.Execute(db.Conn(), statement, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatal(err)
	}
	changed := 0
	if err := sqlitex.ExecuteTransient(db.Conn(), `SELECT changes()`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { changed = stmt.ColumnInt(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("corruption statement changed %d rows, want exactly one: %s", changed, statement)
	}
}

func corruptDDL(t *testing.T, tr Tracker, statement string) {
	t.Helper()
	db := tr.(*sqliteTracker).db
	db.Lock()
	defer db.Unlock()
	if err := sqlitex.ExecuteTransient(db.Conn(), statement, nil); err != nil {
		t.Fatal(err)
	}
}

func openStartupCorruptionHandle(t *testing.T, path string) *startupCorruptionHandle {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenURI)
	if err != nil {
		t.Fatalf("open existing private startup baseline for raw test corruption: %v", err)
	}
	for _, pragma := range []string{`PRAGMA foreign_keys=OFF`, `PRAGMA ignore_check_constraints=ON`} {
		if err := sqlitex.Execute(conn, pragma, nil); err != nil {
			_ = conn.Close()
			t.Fatalf("configure raw startup corruption connection with %q: %v", pragma, err)
		}
	}
	return &startupCorruptionHandle{conn: conn}
}

func closeStartupCorruptionHandle(t *testing.T, handle *startupCorruptionHandle) {
	t.Helper()
	checkpointRows := 0
	busy, logFrames, checkpointedFrames := 0, 0, 0
	err := sqlitex.ExecuteTransient(handle.conn, `PRAGMA wal_checkpoint(TRUNCATE)`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		checkpointRows++
		busy = stmt.ColumnInt(0)
		logFrames = stmt.ColumnInt(1)
		checkpointedFrames = stmt.ColumnInt(2)
		return nil
	}})
	if err != nil {
		_ = handle.conn.Close()
		t.Fatalf("checkpoint raw startup corruption before close: %v", err)
	}
	if checkpointRows != 1 || busy != 0 || logFrames != checkpointedFrames {
		_ = handle.conn.Close()
		t.Fatalf("raw startup corruption checkpoint returned rows/busy/log/checkpointed=%d/%d/%d/%d, want 1/0/N/N", checkpointRows, busy, logFrames, checkpointedFrames)
	}
	if err := handle.conn.Close(); err != nil {
		t.Fatalf("close raw startup corruption connection: %v", err)
	}
}

func corruptStartupSQL(t *testing.T, handle *startupCorruptionHandle, statement string, args ...any) {
	t.Helper()
	if err := sqlitex.Execute(handle.conn, statement, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatal(err)
	}
	changed := 0
	if err := sqlitex.ExecuteTransient(handle.conn, `SELECT changes()`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { changed = stmt.ColumnInt(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("corruption statement changed %d rows, want exactly one: %s", changed, statement)
	}
}

func corruptDropTrigger(t *testing.T, handle *startupCorruptionHandle) {
	t.Helper()
	const trigger = "journal_operations_canonical_update"
	triggerCount := func() int {
		count := 0
		if err := sqlitex.ExecuteTransient(handle.conn, `SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND name=?1`, &sqlitex.ExecOptions{Args: []any{trigger}, ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		}}); err != nil {
			t.Fatalf("inspect raw corruption trigger %q: %v", trigger, err)
		}
		return count
	}
	if count := triggerCount(); count != 1 {
		t.Fatalf("raw corruption trigger %q exists %d times before drop, want exactly one", trigger, count)
	}
	if err := sqlitex.ExecuteTransient(handle.conn, `DROP TRIGGER journal_operations_canonical_update`, nil); err != nil {
		t.Fatal(err)
	}
	if count := triggerCount(); count != 0 {
		t.Fatalf("raw corruption trigger %q exists %d times after drop, want zero", trigger, count)
	}
}

func corruptCanonicalWire(t *testing.T, handle *startupCorruptionHandle, anchor JournalID, mutate func([]byte) []byte) {
	t.Helper()
	var wire []byte
	err := sqlitex.Execute(handle.conn, `SELECT canonical_mutation FROM journal_operations WHERE journal_id=?1`, &sqlitex.ExecOptions{Args: []any{int64(anchor)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		wire = make([]byte, stmt.ColumnLen(0))
		stmt.ColumnBytes(0, wire)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	corruptDropTrigger(t, handle)
	corruptStartupSQL(t, handle, `UPDATE journal_operations SET canonical_mutation=?1 WHERE journal_id=?2`, mutate(wire), int64(anchor))
}

func TestStartupCorruptionMatrixLeavesBytesUnchanged(t *testing.T) {
	t.Parallel()
	cases := map[string]startupCorruptionCase{
		"task-namespace": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET namespace='wrong' WHERE id=?1`, f.task.String())
		},
		"task-title": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET title='wrong' WHERE id=?1`, f.task.String())
		},
		"task-description": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET description='wrong' WHERE id=?1`, f.task.String())
		},
		"task-status": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET status_id=2 WHERE id=?1`, f.task.String())
		},
		"task-priority": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET priority_id=1 WHERE id=?1`, f.task.String())
		},
		"task-type": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET type_id=1 WHERE id=?1`, f.task.String())
		},
		"task-phase": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET phase_id=9 WHERE id=?1`, f.task.String())
		},
		"task-owner": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET owner_id=(SELECT id FROM agents LIMIT 1) WHERE id=?1`, f.task.String())
		},
		"task-notes": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET notes='wrong' WHERE id=?1`, f.task.String())
		},
		"task-created": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET created_at=created_at+1 WHERE id=?1`, f.task.String())
		},
		"task-updated": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET updated_at=updated_at+1 WHERE id=?1`, f.task.String())
		},
		"task-closed": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET closed_at=1 WHERE id=?1`, f.task.String())
		},
		"task-reason": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET close_reason='wrong' WHERE id=?1`, f.task.String())
		},
		"task-watermark": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE tasks SET last_journal_id=1 WHERE id=?1`, f.task.String())
		},
		"task-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM tasks WHERE id=?1`, f.task.String())
		},
		"task-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO tasks SELECT 'matrix--018f0000-0000-7000-8000-000000000099',namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM tasks WHERE id=?1`, f.task.String())
		},
		"attribution": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM task_attributions WHERE task_id=?1`, f.task.String())
		},
		"attribution-actor": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE task_attributions SET actor_id=(SELECT id FROM agents WHERE id<>actor_id LIMIT 1) WHERE task_id=?1`, f.task.String())
		},
		"attribution-journal": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE task_attributions SET first_journal_id=?1 WHERE task_id=?2`, int64(f.event2), f.task.String())
		},
		"attribution-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO task_attributions(task_id,actor_id,first_journal_id) SELECT ?1,agent_id,?2 FROM agents_software WHERE name='other' LIMIT 1`, f.target.String(), int64(f.anchor))
		},
		"edge-created": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE edges SET created_at=created_at+1 WHERE source_id=?1`, f.task.String())
		},
		"edge-source": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE edges SET source_id=?1 WHERE source_id=?2`, f.target.String(), f.task.String())
		},
		"edge-target": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE edges SET target_id='wrong-target' WHERE source_id=?1`, f.task.String())
		},
		"edge-kind": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE edges SET kind_id=2 WHERE source_id=?1`, f.task.String())
		},
		"edge-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM edges WHERE source_id=?1`, f.task.String())
		},
		"edge-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO edges(source_id,target_id,kind_id,created_at) SELECT ?1,?2,kind_id,created_at FROM edges WHERE source_id=?2`, f.target.String(), f.task.String())
		},
		"label-name": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE labels SET name='wrong' WHERE task_id=?1`, f.task.String())
		},
		"label-task": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE labels SET task_id=?1 WHERE task_id=?2`, f.target.String(), f.task.String())
		},
		"label-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM labels WHERE task_id=?1`, f.task.String())
		},
		"label-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO labels(task_id,name) VALUES(?1,'spurious')`, f.target.String())
		},
		"comment-id": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE comments SET id='matrix--018f0000-0000-7000-8000-000000000098' WHERE id=?1`, f.comment.String())
		},
		"comment-task": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE comments SET task_id=?1 WHERE id=?2`, f.target.String(), f.comment.String())
		},
		"comment-author": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE comments SET author_id=(SELECT id FROM agents WHERE id<>author_id LIMIT 1) WHERE id=?1`, f.comment.String())
		},
		"comment-body": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE comments SET body='wrong' WHERE id=?1`, f.comment.String())
		},
		"comment-created": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE comments SET created_at=created_at+1 WHERE id=?1`, f.comment.String())
		},
		"comment-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM comments WHERE id=?1`, f.comment.String())
		},
		"comment-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO comments(id,task_id,author_id,body,created_at) SELECT 'matrix--018f0000-0000-7000-8000-000000000097',task_id,author_id,body,created_at FROM comments WHERE id=?1`, f.comment.String())
		},
		"result-slot-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM journal_operation_result_slots WHERE journal_id=?1 AND result_slot_id='one'`, int64(f.anchor))
		},
		"result-slot-redirected": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_operation_result_slots SET produced_journal_id=?1 WHERE journal_id=?2 AND result_slot_id='one'`, int64(f.event2), int64(f.anchor))
		},
		"result-slot-renamed": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_operation_result_slots SET result_slot_id='renamed' WHERE journal_id=?1 AND result_slot_id='one'`, int64(f.anchor))
		},
		"result-slot-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO journal_operation_result_slots(journal_id,result_slot_id,produced_journal_id) VALUES(?1,'spurious',?2)`, int64(f.anchor), int64(f.event1))
		},
		"context-attached-by": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_task_event_contexts SET attached_by_journal_id=?1 WHERE event_journal_id=?2`, int64(f.event2), int64(f.event1))
		},
		"context-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM journal_task_event_contexts WHERE event_journal_id=?1`, int64(f.event1))
		},
		"context-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO journal_task_event_contexts(event_journal_id,context_kind,context_identity,attached_by_journal_id) VALUES(?1,'task',?2,?1)`, int64(f.event2), f.target.String())
		},
		"context-kind": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_task_event_contexts SET context_kind='actor' WHERE event_journal_id=?1`, int64(f.event1))
		},
		"context-identity": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_task_event_contexts SET context_identity=?1 WHERE event_journal_id=?2`, f.target.String(), int64(f.event1))
		},
		"effect-timestamp": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal SET recorded_at=recorded_at+1 WHERE journal_id=?1`, int64(f.event1))
		},
		"missing-subtype": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM journal_task_events WHERE journal_id=?1`, int64(f.event1))
		},
		"task-event-task": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_task_events SET task_id=?1 WHERE journal_id=?2`, f.target.String(), int64(f.event1))
		},
		"task-event-kind": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_task_events SET event_kind='matrix.changed' WHERE journal_id=?1`, int64(f.event1))
		},
		"task-event-payload": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_task_events SET payload='{"changed":true}' WHERE journal_id=?1`, int64(f.event1))
		},
		"task-event-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO journal_task_events(journal_id,task_id,event_kind,payload) VALUES(?1,?2,'matrix.spurious','{}')`, int64(f.decision), f.task.String())
		},
		"operation-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM journal_operations WHERE journal_id=?1`, int64(f.supportAnchor))
		},
		"operation-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO journal_operations(journal_id,operation_id,authority_journal_id,command_digest,mutation_digest,mutation_encoding_version,canonical_mutation) VALUES(?1,'spurious-operation',NULL,X'01',X'02',NULL,NULL)`, int64(f.event1))
		},
		"operation-authority": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_operations SET authority_journal_id=NULL WHERE journal_id=?1`, int64(f.supportAnchor))
		},
		"operation-mutation-digest": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_operations SET mutation_digest=X'09' WHERE journal_id=?1`, int64(f.supportAnchor))
		},
		"authority-kind": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authorities SET authority_kind_id=1 WHERE journal_id=?1`, int64(f.bootstrap))
		},
		"authority-identity": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authorities SET operation_authority_id='changed' WHERE journal_id=?1`, int64(f.bootstrap))
		},
		"authority-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM journal_authorities WHERE journal_id=?1`, int64(f.bootstrap))
		},
		"authority-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO journal_authorities(journal_id,authority_kind_id,operation_authority_id) VALUES(?1,0,'spurious')`, int64(f.decision))
		},
		"bootstrap-label": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authority_bootstraps SET label='changed' WHERE journal_id=?1`, int64(f.bootstrap))
		},
		"bootstrap-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM journal_authority_bootstraps WHERE journal_id=?1`, int64(f.bootstrap))
		},
		"bootstrap-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO journal_authority_bootstraps(journal_id,label) VALUES(?1,'spurious')`, int64(f.decision))
		},
		"assignment-transition": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authority_assignment_transitions SET transition_id=99 WHERE journal_id=?1`, int64(f.assignmentStart))
		},
		"assignment-start-id": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authority_assignment_transitions SET assignment_id='missing-start' WHERE journal_id=?1`, int64(f.assignmentStart))
		},
		"assignment-end-id": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authority_assignment_transitions SET assignment_id='missing-end' WHERE journal_id=?1`, int64(f.assignmentEnd))
		},
		"assignment-end-transition": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authority_assignment_transitions SET transition_id=99 WHERE journal_id=?1`, int64(f.assignmentEnd))
		},
		"assignment-transition-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM journal_authority_assignment_transitions WHERE journal_id=?1`, int64(f.assignmentStart))
		},
		"assignment-end-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM journal_authority_assignment_transitions WHERE journal_id=?1`, int64(f.assignmentEnd))
		},
		"assignment-transition-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO journal_authority_assignment_transitions(journal_id,assignment_id,transition_id) VALUES(?1,'spurious-assignment',0)`, int64(f.decision))
		},
		"assignment-task": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authority_assignment_episodes SET task_id=?1 WHERE assignment_id='matrix-assignment'`, f.task.String())
		},
		"assignment-slot": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authority_assignment_episodes SET slot_id=99 WHERE assignment_id='matrix-assignment'`)
		},
		"assignment-actor": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authority_assignment_episodes SET actor_id=(SELECT agent_id FROM agents_software WHERE name='other') WHERE assignment_id='matrix-assignment'`)
		},
		"assignment-predecessor": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authority_assignment_episodes SET predecessor_assignment_id='missing' WHERE assignment_id='matrix-assignment'`)
		},
		"assignment-parent": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authority_assignment_episodes SET parent_assignment_id='missing' WHERE assignment_id='matrix-assignment'`)
		},
		"assignment-episode-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM journal_authority_assignment_episodes WHERE assignment_id='matrix-assignment'`)
		},
		"assignment-id": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_authority_assignment_episodes SET assignment_id='renamed-assignment' WHERE assignment_id='matrix-assignment'`)
		},
		"assignment-episode-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO journal_authority_assignment_episodes(assignment_id,task_id,slot_id,actor_id) SELECT 'spurious-episode',?1,0,agent_id FROM agents_software WHERE name='actor'`, f.task.String())
		},
		"decision-kind": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_decisions SET decision_kind='matrix.changed' WHERE journal_id=?1`, int64(f.decision))
		},
		"decision-kind-malformed": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_decisions SET decision_kind='unnamespaced' WHERE journal_id=?1`, int64(f.decision))
		},
		"decision-task": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_decisions SET task_id=?1 WHERE journal_id=?2`, f.target.String(), int64(f.decision))
		},
		"decision-payload": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_decisions SET payload='{"changed":true}' WHERE journal_id=?1`, int64(f.decision))
		},
		"decision-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM journal_decisions WHERE journal_id=?1`, int64(f.decision))
		},
		"decision-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO journal_decisions(journal_id,decision_kind,task_id,payload) VALUES(?1,'matrix.spurious',?2,'{}')`, int64(f.evidence), f.task.String())
		},
		"evidence-kind": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_evidence SET evidence_kind='matrix.changed' WHERE journal_id=?1`, int64(f.evidence))
		},
		"evidence-kind-malformed": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_evidence SET evidence_kind='unnamespaced' WHERE journal_id=?1`, int64(f.evidence))
		},
		"evidence-task": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_evidence SET task_id=?1 WHERE journal_id=?2`, f.target.String(), int64(f.evidence))
		},
		"evidence-digest": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_evidence SET content_digest=X'09' WHERE journal_id=?1`, int64(f.evidence))
		},
		"evidence-payload": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_evidence SET payload='{"changed":true}' WHERE journal_id=?1`, int64(f.evidence))
		},
		"evidence-missing": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `DELETE FROM journal_evidence WHERE journal_id=?1`, int64(f.evidence))
		},
		"evidence-spurious": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `INSERT INTO journal_evidence(journal_id,evidence_kind,task_id,content_digest,payload) VALUES(?1,'matrix.spurious',?2,X'01','{}')`, int64(f.decision), f.task.String())
		},
		"canonical-version-only": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptDropTrigger(t, tr)
			corruptStartupSQL(t, tr, `UPDATE journal_operations SET canonical_mutation=NULL WHERE journal_id=?1`, int64(f.anchor))
		},
		"canonical-bytes-only": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptDropTrigger(t, tr)
			corruptStartupSQL(t, tr, `UPDATE journal_operations SET mutation_encoding_version=NULL WHERE journal_id=?1`, int64(f.anchor))
		},
		"canonical-unknown-version": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptStartupSQL(t, tr, `UPDATE journal_operations SET mutation_encoding_version='unknown.v9' WHERE journal_id=?1`, int64(f.anchor))
		},
		"canonical-effect-limit": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptCanonicalWire(t, tr, f.anchor, func(wire []byte) []byte {
				return bytes.Replace(wire, []byte("effect-count:1:2\n"), []byte(fmt.Sprintf("effect-count:3:%d\n", MaxCanonicalEffects+1)), 1)
			})
		},
		"canonical-context-limit": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptCanonicalWire(t, tr, f.anchor, func(wire []byte) []byte {
				return bytes.Replace(wire, []byte("effect.0.context-count:1:1\n"), []byte(fmt.Sprintf("effect.0.context-count:2:%d\n", MaxCanonicalContextsPerEffect+1)), 1)
			})
		},
		"canonical-field-limit": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptCanonicalWire(t, tr, f.anchor, func(wire []byte) []byte {
				return bytes.Replace(wire, []byte("effect.0.family:10:task_event\n"), []byte(fmt.Sprintf("effect.0.family:%d:", MaxCanonicalFieldBytes+1)), 1)
			})
		},
		"canonical-total-limit": func(t *testing.T, tr *startupCorruptionHandle, f startupFixture) {
			corruptCanonicalWire(t, tr, f.anchor, func([]byte) []byte { return bytes.Repeat([]byte{'x'}, MaxCanonicalMutationBytes+1) })
		},
	}
	const expectedCorruptionCases = 98
	if len(cases) != expectedCorruptionCases {
		t.Fatalf("startup corruption matrix has %d cases, want exactly %d", len(cases), expectedCorruptionCases)
	}
	baselineBuilds := 0
	baseline := buildValidatedStartupBaseline(t, filepath.Join(t.TempDir(), "baseline.sqlite"), &baselineBuilds)
	if baselineBuilds != 1 {
		t.Fatalf("startup corruption baseline built %d times, want exactly one", baselineBuilds)
	}
	copyPaths := make(map[string]struct{}, expectedCorruptionCases)
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db.sqlite")
			if _, duplicate := copyPaths[path]; duplicate {
				t.Fatalf("startup corruption case reused private copy path %q", path)
			}
			copyPaths[path] = struct{}{}
			writeStartupBaselineCopy(t, baseline, path)
			handle := openStartupCorruptionHandle(t, path)
			mutate(t, handle, baseline.fixture)
			closeStartupCorruptionHandle(t, handle)
			requireNoSQLiteSidecars(t, path)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(before, baseline.bytes) {
				t.Fatal("raw corruption left private main database bytes equal to pristine baseline")
			}
			opened, openErr := OpenSQLite(path)
			if opened != nil {
				_ = opened.Close()
				t.Fatal("corrupt database returned a non-nil tracker")
			}
			if openErr == nil {
				t.Fatal("corrupt database opened")
			}
			projectionCase := strings.HasPrefix(name, "task-") || strings.HasPrefix(name, "edge-") || strings.HasPrefix(name, "label-") || strings.HasPrefix(name, "comment-") || strings.HasPrefix(name, "attribution")
			if projectionCase {
				var divergence *ProjectionDivergenceError
				if !errors.As(openErr, &divergence) && !errors.Is(openErr, ErrProjectionDivergence) && !errors.Is(openErr, ErrSubtypeIntegrity) {
					t.Fatalf("projection corruption returned %T, want typed divergence/topology error: %v", openErr, openErr)
				}
			}
			supportCase := strings.HasPrefix(name, "result-slot") || strings.HasPrefix(name, "context-") || strings.HasPrefix(name, "effect-") || name == "missing-subtype" || strings.HasPrefix(name, "task-event") || strings.HasPrefix(name, "operation-") || strings.HasPrefix(name, "authority-") || strings.HasPrefix(name, "bootstrap-") || strings.HasPrefix(name, "assignment-") || strings.HasPrefix(name, "decision-") || strings.HasPrefix(name, "evidence-")
			if supportCase && !errors.Is(openErr, ErrProjectionDivergence) && !errors.Is(openErr, ErrSubtypeIntegrity) {
				t.Fatalf("support corruption returned untyped error: %T %v", openErr, openErr)
			}
			token := strings.Split(name, "-")[0]
			if strings.HasPrefix(name, "result-slot") {
				token = "result slot"
			}
			if name == "missing-subtype" {
				token = "subtype"
			}
			if !strings.Contains(strings.ToLower(openErr.Error()), strings.ReplaceAll(token, "_", " ")) {
				t.Fatalf("error does not identify %q corruption: %v", token, openErr)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed Open changed corrupt database bytes")
			}
		})
	}
	if baselineBuilds != 1 {
		t.Fatalf("startup corruption baseline built %d times after cases, want exactly one", baselineBuilds)
	}
	if len(copyPaths) != expectedCorruptionCases {
		t.Fatalf("startup corruption matrix used %d private copy paths, want exactly %d", len(copyPaths), expectedCorruptionCases)
	}
	baselineBytes, err := os.ReadFile(baseline.path)
	if err != nil {
		t.Fatalf("read immutable startup baseline after corruption cases: %v", err)
	}
	if digest := sha256.Sum256(baselineBytes); digest != baseline.digest || sha256.Sum256(baseline.bytes) != baseline.digest {
		t.Fatal("immutable startup baseline digest changed during corruption matrix")
	}
	requireNoSQLiteSidecars(t, baseline.path)
}
