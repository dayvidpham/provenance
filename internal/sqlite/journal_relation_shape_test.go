package sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// journalSchemaObjects returns SQLite's own stored definitions of the journal
// supertype and everything attached to it, keyed by object name. The
// journal_attributed view is matched by its own name: a view's
// sqlite_master.tbl_name is the view's name, not its base table's, so
// tbl_name = 'journal' alone would leave the migration's view recreation
// outside the convergence comparison.
func journalSchemaObjects(t *testing.T, db *DB) map[string]string {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	objects := map[string]string{}
	if err := scope.queryRows(
		`SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE tbl_name = 'journal' OR name = 'journal_attributed' ORDER BY name`,
		nil,
		func(rows *sql.Rows) error {
			var name, statement string
			if err := rows.Scan(&name, &statement); err != nil {
				return err
			}
			objects[name] = statement
			return nil
		},
	); err != nil {
		t.Fatalf("read journal schema objects: %v", err)
	}
	return objects
}

func journalForeignKeyColumns(t *testing.T, db *DB) map[string]string {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	keys := map[string]string{}
	if err := scope.queryRows("PRAGMA foreign_key_list(journal)", nil, func(rows *sql.Rows) error {
		var id, sequence int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &sequence, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return err
		}
		keys[from] = table + "(" + to + ")"
		return nil
	}); err != nil {
		t.Fatalf("read journal foreign keys: %v", err)
	}
	return keys
}

// bareActivationScope leases one connection on an empty, process-private
// in-memory database through the same helpers Open uses, so a schema layer can
// be exercised on its own rather than through a completed activation.
func bareActivationScope(t *testing.T) *connScope {
	t.Helper()
	target, err := resolveOpenTarget(":memory:")
	if err != nil {
		t.Fatalf("resolve in-memory target: %v", err)
	}
	pool, err := openConfiguredSQLDB(target.activationDSN, 1)
	if err != nil {
		t.Fatalf("open bare in-memory pool: %v", err)
	}
	conn, err := pool.Conn(t.Context())
	if err != nil {
		_ = pool.Close()
		t.Fatalf("lease bare connection: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close bare connection: %v", err)
		}
		if err := pool.Close(); err != nil {
			t.Errorf("close bare pool: %v", err)
		}
	})
	return borrowConnScope(conn, projectionTargetLive)
}

// TestJournalBaseLayerCreatesTheCompletedRelation is the direct proof that a
// database created by this build never needs the journal rebuild: after the
// journal-base layer alone has run on an empty database, the
// produced_by_operation_journal_id foreign key is already present, which is
// exactly the condition completeJournalOperationFK returns early on. The
// rebuild's drop/copy/rename/reparse cost is therefore paid only by databases
// written before the foreign key existed.
func TestJournalBaseLayerCreatesTheCompletedRelation(t *testing.T) {
	t.Parallel()
	scope := bareActivationScope(t)

	if err := scope.ensureJournalSchema(); err != nil {
		t.Fatalf("ensureJournalSchema on an empty database: %v", err)
	}

	present, err := scope.journalProducedByFKPresent()
	if err != nil {
		t.Fatalf("journalProducedByFKPresent: %v", err)
	}
	if !present {
		t.Error("the journal-base layer created a journal relation without the produced_by_operation_journal_id " +
			"foreign key, so every activation on a newly created database would rebuild the table it just created")
	}

	objects := map[string]struct{}{}
	if err := scope.queryRows("SELECT name FROM sqlite_master WHERE tbl_name = 'journal'", nil, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		objects[name] = struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("read journal schema objects: %v", err)
	}
	for _, index := range []string{"idx_journal_kind", "idx_journal_actor", "idx_journal_pboj", "idx_journal_recorded_at"} {
		if _, ok := objects[index]; !ok {
			t.Errorf("the journal-base layer did not create index %q, which the rebuild would have added", index)
		}
	}
}

// TestMigratedJournalConvergesOnTheFreshShape pins the anti-drift property that
// makes the fresh create safe: a database written before the foreign key
// existed, once migrated, carries the same columns, foreign keys, and indexes as
// one created today. Without this, skipping the rebuild on fresh databases could
// hide a divergence between the two shapes.
func TestMigratedJournalConvergesOnTheFreshShape(t *testing.T) {
	t.Parallel()
	// Both databases live under this test's own directory, so the pair is
	// isolated from every other test.
	dir := t.TempDir()

	fresh, err := Open(filepath.Join(dir, "fresh.db"), nil)
	if err != nil {
		t.Fatalf("open fresh database: %v", err)
	}
	defer func() {
		if err := fresh.Close(); err != nil {
			t.Errorf("close fresh database: %v", err)
		}
	}()

	legacyPath := filepath.Join(dir, "legacy.db")
	legacy, err := Open(legacyPath, nil)
	if err != nil {
		t.Fatalf("open database to downgrade: %v", err)
	}
	if err := legacy.AdversarialRemoveJournalOperationFK(); err != nil {
		t.Fatalf("install the pre-foreign-key journal shape: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close downgraded database: %v", err)
	}
	migrated, err := Open(legacyPath, nil)
	if err != nil {
		t.Fatalf("reopen downgraded database (this open runs the migration): %v", err)
	}
	defer func() {
		if err := migrated.Close(); err != nil {
			t.Errorf("close migrated database: %v", err)
		}
	}()

	freshObjects := journalSchemaObjects(t, fresh)
	migratedObjects := journalSchemaObjects(t, migrated)
	if len(freshObjects) != len(migratedObjects) {
		t.Fatalf("migrated journal carries %d schema objects (%v), fresh carries %d (%v)",
			len(migratedObjects), sortedNames(migratedObjects), len(freshObjects), sortedNames(freshObjects))
	}
	for name, freshStatement := range freshObjects {
		migratedStatement, ok := migratedObjects[name]
		if !ok {
			t.Errorf("migrated journal is missing schema object %q", name)
			continue
		}
		// SQLite rewrites the renamed table's stored statement: it quotes the new
		// name and drops the create-time IF NOT EXISTS. Neither is a shape
		// difference, so both sides are normalised before comparison.
		if normalizeJournalDDL(freshStatement) != normalizeJournalDDL(migratedStatement) {
			t.Errorf("schema object %q differs after migration:\n fresh:    %s\n migrated: %s",
				name, normalizeJournalDDL(freshStatement), normalizeJournalDDL(migratedStatement))
		}
	}

	freshKeys := journalForeignKeyColumns(t, fresh)
	migratedKeys := journalForeignKeyColumns(t, migrated)
	if len(freshKeys) != len(migratedKeys) {
		t.Fatalf("migrated journal has %d foreign keys (%v), fresh has %d (%v)",
			len(migratedKeys), migratedKeys, len(freshKeys), freshKeys)
	}
	for column, parent := range freshKeys {
		if migratedKeys[column] != parent {
			t.Errorf("foreign key on %q is %q after migration, want %q", column, migratedKeys[column], parent)
		}
	}
}

func normalizeJournalDDL(statement string) string {
	statement = strings.ReplaceAll(statement, "IF NOT EXISTS ", "")
	statement = strings.ReplaceAll(statement, `"journal"`, "journal")
	return strings.Join(strings.Fields(statement), " ")
}

func sortedNames(objects map[string]string) []string {
	names := make([]string, 0, len(objects))
	for name := range objects {
		names = append(names, name)
	}
	return names
}
