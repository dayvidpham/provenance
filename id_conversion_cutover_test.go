package provenance_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	provenance "github.com/dayvidpham/provenance"
)

// These tests fix the two dependency-owned probes the Pasture URI cutover hit:
//
//	PROBE-A  ConvertIDsInTx on a pre-operations legacy store:
//	         "SQL logic error: no such table: governed_allocation_operations (1)"
//	PROBE-B  ConvertIDsInTx on a partly-converted store:
//	         `retired identity "…/…-…" has no non-empty namespace and final
//	          two-dash UUID, or a slash follows that separator`
//
// The fixes are EnsureSchemaInTx (schema completed inside the caller's
// BEGIN IMMEDIATE), PauseForeignKeys (a verified connection-level FK bracket,
// because an in-transaction PRAGMA foreign_keys=OFF is a no-op), and slash-form
// tolerance in the identity converter.

func readForeignKeys(t *testing.T, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) int {
	t.Helper()
	var value int
	if err := q.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&value); err != nil {
		t.Fatalf("read PRAGMA foreign_keys: %v", err)
	}
	return value
}

func openCutoverDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rwc&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// TestPauseForeignKeysBracketVerified proves the bracket is real: SQLite ignores
// PRAGMA foreign_keys inside a transaction, so only a write before BEGIN takes
// effect; the exported bracket performs that write and restores it, verifying
// both by reading the pragma back.
func TestPauseForeignKeysBracketVerified(t *testing.T) {
	ctx := context.Background()
	db := openCutoverDB(t, filepath.Join(t.TempDir(), "fk.db"))
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if got := readForeignKeys(t, conn); got != 1 {
		t.Fatalf("initial foreign_keys=%d, want 1 from the pool DSN", got)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	if got := readForeignKeys(t, conn); got != 1 {
		t.Fatalf("in-transaction PRAGMA foreign_keys=OFF changed the value to %d; SQLite ignores it inside a transaction, so the bracket must wrap BEGIN", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	restoreFK, err := provenance.PauseForeignKeys(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if got := readForeignKeys(t, conn); got != 0 {
		t.Fatalf("after PauseForeignKeys foreign_keys=%d, want 0", got)
	}
	if err := restoreFK(nil); err != nil {
		t.Fatal(err)
	}
	if got := readForeignKeys(t, conn); got != 1 {
		t.Fatalf("after the FK bracket restore foreign_keys=%d, want 1", got)
	}
}

// legacyCutoverStore builds a pre-operations Provenance store: a legacy tasks
// table and a legacy journal table carrying produced_by_operation_journal_id
// WITHOUT its foreign key, plus a child row that references journal so the
// schema rebuild must move rows past the constraint. It has neither
// governed_allocation_operations nor journal_operations, so it reproduces
// PROBE-A when the schema is not completed first.
func legacyCutoverStore(t *testing.T) *sql.DB {
	t.Helper()
	db := openCutoverDB(t, filepath.Join(t.TempDir(), "legacy.db"))
	for _, stmt := range []string{
		`CREATE TABLE tasks (id TEXT PRIMARY KEY, namespace TEXT NOT NULL, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', status_id INTEGER NOT NULL DEFAULT 0, priority_id INTEGER NOT NULL DEFAULT 2, type_id INTEGER NOT NULL DEFAULT 2, phase_id INTEGER NOT NULL, owner_id TEXT, notes TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, closed_at INTEGER, close_reason TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE agents (id TEXT PRIMARY KEY, kind_id INTEGER NOT NULL)`,
		`CREATE TABLE journal (journal_id INTEGER PRIMARY KEY AUTOINCREMENT, kind_id INTEGER NOT NULL, actor_id TEXT, recorded_at INTEGER NOT NULL, produced_by_operation_journal_id INTEGER)`,
		`CREATE TABLE journal_task_events (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id), task_id TEXT NOT NULL, event_kind TEXT NOT NULL, payload TEXT NOT NULL)`,
		`INSERT INTO tasks (id,namespace,title,phase_id,created_at,updated_at) VALUES ('https://github.com/dayvidpham/provenance--01968a3c-9d4f-7c8a-bc12-feedfacecafe','https://github.com/dayvidpham/provenance','legacy',9,1,1)`,
		`INSERT INTO agents (id,kind_id) VALUES ('https://github.com/dayvidpham/provenance--00000000-0000-0000-0000-000000000001',3)`,
		`INSERT INTO journal (kind_id,actor_id,recorded_at) VALUES (2,'https://github.com/dayvidpham/provenance--00000000-0000-0000-0000-000000000001',1)`,
		`INSERT INTO journal_task_events (journal_id,task_id,event_kind,payload) VALUES (1,'https://github.com/dayvidpham/provenance--01968a3c-9d4f-7c8a-bc12-feedfacecafe','created','{}')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("build legacy store: %v", err)
		}
	}
	return db
}

func TestLegacyStoreSchemaCompletionEnablesConversion(t *testing.T) {
	ctx := context.Background()
	db := legacyCutoverStore(t)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// The journal produced-by FK completion rebuilds journal, which is only
	// possible with enforcement OFF before BEGIN. With the pool's FK ON and an
	// in-transaction PRAGMA (a no-op), completion fails rather than silently
	// corrupting the child rows.
	naive, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := naive.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	if err := provenance.EnsureSchemaInTx(ctx, naive, nil); err == nil {
		t.Fatal("schema completion succeeded with foreign keys ON; the in-transaction pragma must not have been effective")
	}
	if err := naive.Rollback(); err != nil {
		t.Fatal(err)
	}

	// The bracketed sequence: FK OFF on the connection, then BEGIN, then schema
	// completion, then conversion, atomically.
	restoreFK, err := provenance.PauseForeignKeys(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	committed, restored := false, false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
		if !restored {
			_ = restoreFK(nil)
		}
	}()

	if err := provenance.EnsureSchemaInTx(ctx, tx, nil); err != nil {
		t.Fatalf("ensure schema in tx: %v", err)
	}
	for _, table := range []string{"governed_allocation_operations", "journal_operations"} {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)", table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("EnsureSchemaInTx did not create %s", table)
		}
	}

	report, err := provenance.ConvertIDsInTx(ctx, tx, provenance.IDConversionOptions{})
	if err != nil {
		t.Fatalf("convert legacy store after schema completion: %v", err)
	}
	if report.AlreadyCurrent || report.ChangedFields == 0 {
		t.Fatalf("legacy conversion report: %+v", report)
	}
	var taskID, actorID string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM tasks").Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT actor_id FROM journal").Scan(&actorID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{taskID, actorID} {
		if !strings.Contains(id, "/") || strings.LastIndex(id, "--") > strings.LastIndex(id, "/") {
			t.Fatalf("converted identity %q is not the slash form", id)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	committed = true
	if err := restoreFK(nil); err != nil {
		t.Fatal(err)
	}
	restored = true
	if got := readForeignKeys(t, conn); got != 1 {
		t.Fatalf("foreign keys not restored after the cutover: got %d", got)
	}
}

// TestPartlyConvertedStoreConvertsToSlash fixes PROBE-B: a store whose keys are
// partly in the new slash form (an interrupted conversion) must still convert,
// rather than being refused because the current-form values are not two-dash.
func TestPartlyConvertedStoreConvertsToSlash(t *testing.T) {
	ctx := context.Background()
	_, db := oldCorpusCopy(t)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	// Convert exactly one task key in place, leaving canonical state retired.
	var alreadySlash string
	if err := tx.QueryRowContext(ctx, `UPDATE tasks SET id=namespace||'/'||SUBSTR(id,LENGTH(namespace)+3) WHERE id=(SELECT MIN(id) FROM tasks) RETURNING id`).Scan(&alreadySlash); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(alreadySlash, "/") || strings.LastIndex(alreadySlash, "--") > strings.LastIndex(alreadySlash, "/") {
		t.Fatalf("fixture precondition: %q is not the slash form", alreadySlash)
	}

	report, err := provenance.ConvertIDsInTx(ctx, tx, provenance.IDConversionOptions{})
	if err != nil {
		t.Fatalf("partly-converted store refused: %v", err)
	}
	if report.AlreadyCurrent || report.ChangedMutations == 0 {
		t.Fatalf("partly-converted conversion report: %+v", report)
	}
	// The already-current key is retained and every remaining key is slash form.
	var retained int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE id=?", alreadySlash).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 1 {
		t.Fatalf("already-current task key was rewritten or lost: count=%d", retained)
	}
	var retiredKeys int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE id NOT LIKE namespace || '/%'").Scan(&retiredKeys); err != nil {
		t.Fatal(err)
	}
	if retiredKeys != 0 {
		t.Fatalf("%d task keys remain in the retired form after conversion", retiredKeys)
	}
	if err := provenance.VerifyIDConversionInTx(ctx, tx); err != nil {
		t.Fatalf("owner verification rejected the partly-converted store: %v", err)
	}
}
