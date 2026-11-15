package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
)

// TestVerifiedShapeIDMatchesReleasedV3 pins the ledger shape to the value a
// released v3 ledger recorded. The per-table vector is ledger machinery, not a
// data-contract change, so it must not move this digest: if it ever does, a
// current v3 ledger would be rejected and an upgrade would re-verify the whole
// store, which is the cost this change exists to remove.
func TestVerifiedShapeIDMatchesReleasedV3(t *testing.T) {
	const releasedV3Shape = "a00013c31183c3511131f520544899e36565b5a7a821ec7f92e5edd0ce1f5eb0"
	if got := verifiedShapeID(); got != releasedV3Shape {
		t.Fatalf("verifiedShapeID = %q, want the released v3 shape %q; the per-table vector must not invalidate existing ledgers", got, releasedV3Shape)
	}
}

// openVerifiedTablesTestDB opens a direct handle for out-of-band fixture SQL.
func openVerifiedTablesTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqliteDriverName, "file:"+path)
	if err != nil {
		t.Fatalf("open fixture handle on %q: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// readLedgerGeneration reads the ledger row's global generation and count.
func readLedgerGeneration(t *testing.T, path string) (generation, count int64) {
	t.Helper()
	db := openVerifiedTablesTestDB(t, path)
	if err := db.QueryRow("SELECT generation, verified_count FROM "+verifiedStoreTable+" WHERE id = 1").Scan(&generation, &count); err != nil {
		t.Fatalf("read ledger row: %v", err)
	}
	return generation, count
}

// readVectorSum reads the current global generation from the per-table vector.
func readVectorSum(t *testing.T, path string) int64 {
	t.Helper()
	db := openVerifiedTablesTestDB(t, path)
	var sum int64
	if err := db.QueryRow("SELECT COALESCE(SUM(generation), 0) FROM " + verifiedTableGenerationTable).Scan(&sum); err != nil {
		t.Fatalf("read vector sum: %v", err)
	}
	return sum
}

// TestVerifiedLedgerPartialPathVerifiesAgentRegistryOnly is the measured case:
// a direct software-agent registration moves agents and agents_software without
// touching the journal. The next open must verify only those tables, leave
// verified_count alone, and leave the ledger current.
func TestVerifiedLedgerPartialPathVerifiesAgentRegistryOnly(t *testing.T) {
	path := t.TempDir() + "/partial-agents.db"
	_, seeded := seedLedgerStore(t, path)

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen before the direct registration: %v", err)
	}
	if _, err := db.RegisterSoftwareAgent("verified", "partial-agent", "0", "open_verified_tables_test"); err != nil {
		t.Fatalf("register the direct agent: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close after the direct registration: %v", err)
	}

	generationBefore, _ := readLedgerGeneration(t, path)
	if generationBefore == 0 {
		t.Fatal("fixture ledger generation is zero")
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after the direct registration: %v", err)
	}
	generationAfter, countAfter := readLedgerGeneration(t, path)
	if countAfter != seeded {
		t.Fatalf("verified_count moved %d -> %d; the agent registration took the whole-store path instead of the table-local one", seeded, countAfter)
	}
	if generationAfter == generationBefore {
		t.Fatalf("ledger generation did not advance past %d after a direct write", generationBefore)
	}
	if sum := readVectorSum(t, path); sum != generationAfter {
		t.Fatalf("ledger generation %d does not equal the vector sum %d", generationAfter, sum)
	}
	// The as-of mirror is complete and the next open is still the skip.
	mirror := openVerifiedTablesTestDB(t, path)
	var mirrored int
	if err := mirror.QueryRow("SELECT COUNT(*) FROM " + verifiedTableProofTable).Scan(&mirrored); err != nil {
		t.Fatalf("count the as-of mirror: %v", err)
	}
	if mirrored != verifiedVectorLength() {
		t.Fatalf("as-of mirror rows = %d, want %d", mirrored, verifiedVectorLength())
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("reopen after the partial recovery: %v", err)
	}
	if _, countAgain := readLedgerGeneration(t, path); countAgain != seeded {
		t.Fatalf("verified_count moved to %d on the follow-up open; the partial recovery did not leave the ledger current", countAgain)
	}
}

// TestVerifiedLedgerPartialPathRefusesTaskMutation proves the routing's other
// half: a task projection change still takes the whole-store path, where the
// replay convergence guard refuses it. The partial path must never be a way to
// bless a changed projection table.
func TestVerifiedLedgerPartialPathRefusesTaskMutation(t *testing.T) {
	path := t.TempDir() + "/partial-tasks.db"
	task, seeded := seedLedgerStore(t, path)

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen before the task mutation: %v", err)
	}
	other, err := db.RegisterSoftwareAgent("verified", "task-mutator", "0", "open_verified_tables_test")
	if err != nil {
		t.Fatalf("register the other owner: %v", err)
	}
	if err := db.AdversarialCorruptTaskProjection(task, AdversarialFieldOwner, other.ID.String()); err != nil {
		t.Fatalf("corrupt the owner projection: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close after the task mutation: %v", err)
	}

	err = reopenFileBacked(t, path)
	if !errors.Is(err, journal.ErrProjectionDivergence) {
		t.Fatalf("opening a task-mutated store = %v, want projection divergence", err)
	}
	if _, countAfter := readLedgerGeneration(t, path); countAfter != seeded {
		t.Fatalf("a refused open advanced verified_count to %d, want %d", countAfter, seeded)
	}
}

// TestVerifiedLedgerAdoptsCurrentV3Ledger simulates the upgrade from a v3 store:
// the per-table tables and the singleton-advancing triggers are restored to the
// old shape, with the singleton set to the ledger's generation. The open must
// adopt the proof, not re-verify, and must install the vector.
func TestVerifiedLedgerAdoptsCurrentV3Ledger(t *testing.T) {
	path := t.TempDir() + "/adopt-v3.db"
	_, seeded := seedLedgerStore(t, path)
	generation, _ := readLedgerGeneration(t, path)
	if generation == 0 {
		t.Fatal("fixture ledger generation is zero")
	}
	if err := simulateV3Ledger(t, path, generation); err != nil {
		t.Fatalf("simulate the v3 ledger: %v", err)
	}

	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open a current v3 ledger: %v", err)
	}
	generationAfter, countAfter := readLedgerGeneration(t, path)
	if countAfter != seeded {
		t.Fatalf("verified_count moved %d -> %d; a current v3 ledger was re-verified instead of adopted", seeded, countAfter)
	}
	if generationAfter != generation {
		t.Fatalf("adopted ledger generation = %d, want the v3 generation %d", generationAfter, generation)
	}
	if sum := readVectorSum(t, path); sum != generation {
		t.Fatalf("vector sum after adoption = %d, want %d", sum, generation)
	}
	// The triggers must be this build's per-table bodies, not the v3 singleton
	// bodies the simulation restored.
	var body string
	if err := openVerifiedTablesTestDB(t, path).QueryRow(
		"SELECT COALESCE(sql, '') FROM sqlite_master WHERE type='trigger' AND name=?1",
		verifiedTriggerName("tasks", "INSERT")).Scan(&body); err != nil {
		t.Fatalf("read adopted trigger: %v", err)
	}
	if body != verifiedTriggerDDL("tasks", "INSERT") {
		t.Fatalf("adopted trigger body = %q, want the per-table body", body)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after adoption: %v", err)
	}
	if _, countAgain := readLedgerGeneration(t, path); countAgain != seeded {
		t.Fatalf("verified_count moved to %d after adoption; the adopted ledger is not current", countAgain)
	}
}

// TestVerifiedLedgerDoesNotAdoptStaleV3Ledger proves adoption is identity-bound:
// one extra v3 generation bump with no data change forces the full path, which
// then re-persists the ledger at the current generation.
func TestVerifiedLedgerDoesNotAdoptStaleV3Ledger(t *testing.T) {
	path := t.TempDir() + "/stale-v3.db"
	_, seeded := seedLedgerStore(t, path)
	generation, _ := readLedgerGeneration(t, path)
	if err := simulateV3Ledger(t, path, generation+1); err != nil {
		t.Fatalf("simulate the stale v3 ledger: %v", err)
	}

	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open a stale v3 ledger: %v", err)
	}
	_, countAfter := readLedgerGeneration(t, path)
	if countAfter != seeded+1 {
		t.Fatalf("verified_count after a stale v3 open = %d, want %d; the full path did not run exactly once", countAfter, seeded+1)
	}
}

// TestVerifiedLedgerPartialPersistRefusesMovedStore pins the compare-and-set:
// a write that lands between the partial checks and the ledger commit must not
// be recorded.
func TestVerifiedLedgerPartialPersistRefusesMovedStore(t *testing.T) {
	path := t.TempDir() + "/partial-cas.db"
	seedLedgerStore(t, path)

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	scope := takePoolScope(t, db)
	defer scope.release()
	before, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		t.Fatalf("read before identity: %v", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx,
		"UPDATE "+verifiedTableGenerationTable+" SET generation = generation + 1 WHERE table_id = ?1",
		verifiedTableID("tasks")); err != nil {
		t.Fatalf("move the store: %v", err)
	}
	if scope.persistVerifiedChangedTablesIfUnchanged(before, []int{verifiedTableID("tasks")}) {
		t.Fatal("partial persist recorded a store that moved after the checks")
	}
}

// TestVerifiedLedgerCoverageRepairsAlteredTriggerBody proves the fast path
// refuses a trigger whose body is not this build's, and activation restores it
// without re-running the whole-store checks when the data is unchanged.
func TestVerifiedLedgerCoverageRepairsAlteredTriggerBody(t *testing.T) {
	path := t.TempDir() + "/altered-trigger.db"
	_, seeded := seedLedgerStore(t, path)
	db := openVerifiedTablesTestDB(t, path)
	name := verifiedTriggerName("tasks", "INSERT")
	if _, err := db.Exec("DROP TRIGGER " + name); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := db.Exec("CREATE TRIGGER " + name + " AFTER INSERT ON tasks BEGIN SELECT 1; END"); err != nil {
		t.Fatalf("install altered trigger: %v", err)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open with an altered trigger body: %v", err)
	}
	if _, countAfter := readLedgerGeneration(t, path); countAfter != seeded {
		t.Fatalf("verified_count moved to %d while repairing a trigger body, want %d", countAfter, seeded)
	}
	var body string
	if err := db.QueryRow("SELECT COALESCE(sql, '') FROM sqlite_master WHERE type='trigger' AND name=?1", name).Scan(&body); err != nil {
		t.Fatalf("read repaired trigger: %v", err)
	}
	if body != verifiedTriggerDDL("tasks", "INSERT") {
		t.Fatalf("repaired trigger body = %q, want the per-table body", body)
	}
}

// simulateV3Ledger restores the pre-vector shape: it drops the two vector
// tables, sets the singleton counter to the given generation, and recreates the
// singleton-advancing trigger bodies.
func simulateV3Ledger(t *testing.T, path string, generation int64) error {
	t.Helper()
	db := openVerifiedTablesTestDB(t, path)
	for _, statement := range []string{
		"DROP TABLE " + verifiedTableProofTable,
		"DROP TABLE " + verifiedTableGenerationTable,
		"UPDATE " + storeGenerationTable + " SET generation = ?1 WHERE id = 1",
	} {
		args := []any(nil)
		if statement == "UPDATE "+storeGenerationTable+" SET generation = ?1 WHERE id = 1" {
			args = []any{generation}
		}
		if _, err := db.Exec(statement, args...); err != nil {
			return fmt.Errorf("simulate v3 ledger %q: %w", statement, err)
		}
	}
	for _, table := range verifiedCoveredTableNames {
		for _, event := range verifiedTriggerEvents {
			name := verifiedTriggerName(table, event)
			if _, err := db.Exec("DROP TRIGGER IF EXISTS " + name); err != nil {
				return fmt.Errorf("drop v4 trigger %s: %w", name, err)
			}
			oldBody := fmt.Sprintf("CREATE TRIGGER %s AFTER %s ON %s BEGIN UPDATE %s SET generation = generation + 1 WHERE id = 1; END",
				name, event, table, storeGenerationTable)
			if _, err := db.Exec(oldBody); err != nil {
				return fmt.Errorf("create v3 trigger %s: %w", name, err)
			}
		}
	}
	return nil
}
