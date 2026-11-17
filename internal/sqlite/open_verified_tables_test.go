package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
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

// orderedAgentIDs reads one ordered agent-id column. A nil result becomes an
// empty slice so a missing set compares equal to a missing registry.
func orderedAgentIDs(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("read agent ids %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan agent id from %q: %v", query, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read agent ids %q: %v", query, err)
	}
	return ids
}

// requireVerifiedAgentSetEqualsRegistry fails unless the persisted proof names
// exactly the current registry and the recorded digest is that set's digest.
func requireVerifiedAgentSetEqualsRegistry(t *testing.T, db *sql.DB) (registry []string) {
	t.Helper()
	registry = orderedAgentIDs(t, db, "SELECT id FROM agents ORDER BY id")
	verified := orderedAgentIDs(t, db, "SELECT agent_id FROM "+verifiedAgentProofTable+" ORDER BY agent_id")
	if !slices.Equal(registry, verified) {
		t.Fatalf("verified agent set = %v, want the registry %v", verified, registry)
	}
	var recorded string
	if err := db.QueryRow("SELECT agent_digest FROM " + verifiedAgentMetaTable + " WHERE id = 1").Scan(&recorded); err != nil {
		t.Fatalf("read verified agent digest: %v", err)
	}
	if want := verifiedAgentSetDigest(registry); recorded != want {
		t.Fatalf("verified agent digest = %q, want %q", recorded, want)
	}
	return registry
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
	// The insert-only recovery records the new registry as verified.
	for _, want := range []string{"writer", "partial-agent"} {
		var found int
		if err := mirror.QueryRow("SELECT COUNT(*) FROM agents_software s JOIN "+verifiedAgentProofTable+" v ON v.agent_id = s.agent_id WHERE s.name=?1", want).Scan(&found); err != nil {
			t.Fatalf("read verified agent %q: %v", want, err)
		}
		if found < 1 {
			t.Fatalf("verified agent set does not contain %q (found=%d)", want, found)
		}
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
	// Adoption seeds the verified agent set from the current registry, exactly.
	raw := openVerifiedTablesTestDB(t, path)
	registry := requireVerifiedAgentSetEqualsRegistry(t, raw)
	if len(registry) == 0 {
		t.Fatal("adoption seeded an empty verified agent set")
	}
	var agentID string
	if err := raw.QueryRow("SELECT agent_id FROM agents_software WHERE name='writer'").Scan(&agentID); err != nil {
		t.Fatalf("read the adopted writer agent: %v", err)
	}
	if _, err := raw.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	if _, err := raw.Exec("DELETE FROM agents_software WHERE agent_id=?1", agentID); err != nil {
		t.Fatalf("delete the adopted writer subtype: %v", err)
	}
	if _, err := raw.Exec("DELETE FROM agents WHERE id=?1", agentID); err != nil {
		t.Fatalf("delete the adopted writer agent: %v", err)
	}
	err := reopenFileBacked(t, path)
	if !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("opening an adopted store after deleting a verified agent = %v, want ErrSubtypeIntegrity", err)
	}
	if _, countAfterDelete := readLedgerGeneration(t, path); countAfterDelete != seeded {
		t.Fatalf("a refused open advanced verified_count to %d, want %d", countAfterDelete, seeded)
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
	if scope.persistVerifiedChangedTablesIfUnchanged(before) {
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

// simulateV3Ledger restores the pre-vector shape: it drops the vector tables,
// the verified agent set, and the agent-set digest row, sets the singleton
// counter to the given generation, and recreates the singleton-advancing
// trigger bodies.
func simulateV3Ledger(t *testing.T, path string, generation int64) error {
	t.Helper()
	db := openVerifiedTablesTestDB(t, path)
	for _, statement := range []string{
		"DROP TABLE " + verifiedAgentMetaTable,
		"DROP TABLE " + verifiedAgentProofTable,
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

// TestVerifiedLedgerPartialPathRefusesDanglingAgentReference is the blocking
// regression from the elegance review: deleting an agent and its subtype out of
// band leaves the journal anchors and task attributions pointing at a missing
// agent. The partial path must check the referencing side, take the full path,
// and let the whole-store foreign-key check refuse the store.
func TestVerifiedLedgerPartialPathRefusesDanglingAgentReference(t *testing.T) {
	path := t.TempDir() + "/dangling-agent.db"
	_, seeded := seedLedgerStore(t, path)
	db := openVerifiedTablesTestDB(t, path)
	if _, err := db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	var agentID string
	if err := db.QueryRow("SELECT agent_id FROM agents_software WHERE name='writer'").Scan(&agentID); err != nil {
		t.Fatalf("read the fixture writer agent: %v", err)
	}
	if _, err := db.Exec("DELETE FROM agents_software WHERE agent_id=?1", agentID); err != nil {
		t.Fatalf("delete the writer subtype row: %v", err)
	}
	if _, err := db.Exec("DELETE FROM agents WHERE id=?1", agentID); err != nil {
		t.Fatalf("delete the writer agent row: %v", err)
	}
	err := reopenFileBacked(t, path)
	if !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("opening a store with a dangling journal actor = %v, want ErrSubtypeIntegrity", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded {
		t.Fatalf("a refused open advanced verified_count to %d, want %d", count, seeded)
	}
}

// TestVerifiedLedgerPartialPathRefusesDanglingFixedActor covers the actor-claim
// branch: a fixed-actor manifest row pointing at a missing agent/namespace must
// not be blessed by the table-local path.
func TestVerifiedLedgerPartialPathRefusesDanglingFixedActor(t *testing.T) {
	path := t.TempDir() + "/dangling-fixed-actor.db"
	_, seeded := seedLedgerStore(t, path)
	db := openVerifiedTablesTestDB(t, path)
	if _, err := db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	if _, err := db.Exec("INSERT INTO fixed_actor_manifest_entries(actor_id, namespace, kind_id, name, metadata) VALUES ('https://example.test/missing', 'missing', 2, 'missing', '{}')"); err != nil {
		t.Fatalf("insert the dangling fixed actor: %v", err)
	}
	err := reopenFileBacked(t, path)
	if !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("opening a store with a dangling fixed actor = %v, want ErrSubtypeIntegrity", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded {
		t.Fatalf("a refused open advanced verified_count to %d, want %d", count, seeded)
	}
}

// TestVerifiedLedgerPartialPathRefusesMissingSeed covers the seed branch: a
// deleted closed-lookup row forces the full path, which re-seeds it.
func TestVerifiedLedgerPartialPathRefusesMissingSeed(t *testing.T) {
	path := t.TempDir() + "/missing-seed.db"
	_, seeded := seedLedgerStore(t, path)
	db := openVerifiedTablesTestDB(t, path)
	if _, err := db.Exec("DELETE FROM priorities WHERE id=4"); err != nil {
		t.Fatalf("delete the backlog priority: %v", err)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after deleting a seed row: %v", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded+1 {
		t.Fatalf("verified_count after a missing seed = %d, want %d; the full path did not run exactly once", count, seeded+1)
	}
	var name string
	if err := db.QueryRow("SELECT name FROM priorities WHERE id=4").Scan(&name); err != nil {
		t.Fatalf("read the re-seeded priority: %v", err)
	}
	if name != "backlog" {
		t.Fatalf("re-seeded priority = %q, want backlog", name)
	}
}

// TestVerifiedLedgerRepairsVectorNameDrift proves the id-to-name map is movable:
// activation repairs a drifted row and resets its counter, the changed sum sends
// the open down the full path once, and the next open is current again.
func TestVerifiedLedgerRepairsVectorNameDrift(t *testing.T) {
	path := t.TempDir() + "/vector-drift.db"
	_, seeded := seedLedgerStore(t, path)
	db := openVerifiedTablesTestDB(t, path)
	if _, err := db.Exec("UPDATE provenance_table_generation SET table_name='drifted', generation=7 WHERE table_id=?1", verifiedTableID("tasks")); err != nil {
		t.Fatalf("drift the tasks vector row: %v", err)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after vector name drift: %v", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded+1 {
		t.Fatalf("verified_count after vector drift = %d, want %d; the full path did not run exactly once", count, seeded+1)
	}
	var name string
	var generation int64
	if err := db.QueryRow("SELECT table_name, generation FROM provenance_table_generation WHERE table_id=?1", verifiedTableID("tasks")).Scan(&name, &generation); err != nil {
		t.Fatalf("read the repaired vector row: %v", err)
	}
	if name != "tasks" || generation != 0 {
		t.Fatalf("repaired vector row = (%q,%d), want (tasks,0)", name, generation)
	}
}

// TestVerifiedTriggerActivationMatchesHelper pins activation's inline trigger
// literal to the helper the coverage check and exact replay compare against.
func TestVerifiedTriggerActivationMatchesHelper(t *testing.T) {
	path := t.TempDir() + "/trigger-match.db"
	seedLedgerStore(t, path)
	db := openVerifiedTablesTestDB(t, path)
	for _, table := range verifiedCoveredTableNames {
		for _, event := range verifiedTriggerEvents {
			var stored string
			if err := db.QueryRow("SELECT COALESCE(sql, '') FROM sqlite_master WHERE type='trigger' AND name=?1", verifiedTriggerName(table, event)).Scan(&stored); err != nil {
				t.Fatalf("read trigger %s on %s: %v", event, table, err)
			}
			if stored != verifiedTriggerDDL(table, event) {
				t.Fatalf("stored trigger %s on %s = %q, want the helper form %q", event, table, stored, verifiedTriggerDDL(table, event))
			}
		}
	}
}

// partialCoveredTableNames is the closed set of tables whose whole-store
// coverage is table-local. TestPartialCoveredTablesMatchRouting pins it to
// metadataChecksFor, and TestVerifiedLedgerNonLocalTablesTakeFullPath uses it
// to prove every other covered table still takes the whole-store path.
var partialCoveredTableNames = []string{
	"agents", "agents_human", "agents_ml", "agents_software",
	"actor_namespace_claims", "fixed_actor_manifest_entries",
	"ml_models", "provenance_id_format",
}

func TestPartialCoveredTablesMatchRouting(t *testing.T) {
	scope := &connScope{}
	for _, table := range verifiedCoveredTableNames {
		_, local := scope.metadataChecksFor([]int{verifiedTableID(table)}, nil)
		want := slices.Contains(partialCoveredTableNames, table)
		if local != want {
			t.Fatalf("metadataChecksFor(%q) local=%v, want %v", table, local, want)
		}
	}
}

// TestVerifiedLedgerBorrowedPartialPath exercises the borrowed open, which has
// no read-only preflight: the partial path is the only gate. It also proves the
// partial path refuses a dangling agent reference.
func TestVerifiedLedgerBorrowedPartialPath(t *testing.T) {
	path := t.TempDir() + "/borrowed-partial.db"
	_, seeded := seedLedgerStore(t, path)
	pool := borrowedPool(t, path)
	db, err := OpenBorrowed(pool, nil)
	if err != nil {
		t.Fatalf("borrowed open before the direct write: %v", err)
	}
	if _, err := db.RegisterSoftwareAgent("verified", "borrowed-writer", "0", "open_verified_tables_test"); err != nil {
		t.Fatalf("register the direct agent: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close after the direct write: %v", err)
	}
	db, err = OpenBorrowed(pool, nil)
	if err != nil {
		t.Fatalf("borrowed open after the direct write: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close after the partial recovery: %v", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded {
		t.Fatalf("borrowed partial recovery moved verified_count to %d, want %d", count, seeded)
	}

	// Corrupt the fixture writer's references out of band, then prove the
	// borrowed open reaches the partial path and refuses it.
	raw := openVerifiedTablesTestDB(t, path)
	if _, err := raw.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	var agentID string
	if err := raw.QueryRow("SELECT agent_id FROM agents_software WHERE name='writer'").Scan(&agentID); err != nil {
		t.Fatalf("read the fixture writer: %v", err)
	}
	if _, err := raw.Exec("DELETE FROM agents_software WHERE agent_id=?1", agentID); err != nil {
		t.Fatalf("delete the writer subtype: %v", err)
	}
	if _, err := raw.Exec("DELETE FROM agents WHERE id=?1", agentID); err != nil {
		t.Fatalf("delete the writer agent: %v", err)
	}
	if _, err := OpenBorrowed(pool, nil); !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("borrowed open with a dangling journal actor = %v, want ErrSubtypeIntegrity", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded {
		t.Fatalf("refused borrowed open advanced verified_count to %d, want %d", count, seeded)
	}
}

// TestVerifiedLedgerNonLocalTablesTakeFullPath pins the fall-through list: every
// covered table outside the table-local set must force the whole-store path.
func TestVerifiedLedgerNonLocalTablesTakeFullPath(t *testing.T) {
	for _, table := range verifiedCoveredTableNames {
		if slices.Contains(partialCoveredTableNames, table) {
			continue
		}
		t.Run(table, func(t *testing.T) {
			path := t.TempDir() + "/full-" + table + ".db"
			_, seeded := seedLedgerStore(t, path)
			db := openVerifiedTablesTestDB(t, path)
			if _, err := db.Exec("UPDATE provenance_table_generation SET generation = generation + 1 WHERE table_id = ?1", verifiedTableID(table)); err != nil {
				t.Fatalf("bump %s: %v", table, err)
			}
			if err := reopenFileBacked(t, path); err != nil {
				t.Fatalf("open after bumping %s: %v", table, err)
			}
			if _, count := readLedgerGeneration(t, path); count != seeded+1 {
				t.Fatalf("%s took the table-local path (verified_count %d, want %d); it must take the whole-store path", table, count, seeded+1)
			}
		})
	}
}

// TestVerifiedLedgerRepairsTornMirror proves a missing as-of row is not a
// permanent failure: the full path rewrites the mirror.
func TestVerifiedLedgerRepairsTornMirror(t *testing.T) {
	path := t.TempDir() + "/torn-mirror.db"
	_, seeded := seedLedgerStore(t, path)
	db := openVerifiedTablesTestDB(t, path)
	if _, err := db.Exec("DELETE FROM provenance_verified_table WHERE table_id = ?1", verifiedTableID("tasks")); err != nil {
		t.Fatalf("delete the tasks mirror row: %v", err)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open with a torn mirror: %v", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded+1 {
		t.Fatalf("verified_count after a torn mirror = %d, want %d; the full path did not run exactly once", count, seeded+1)
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM provenance_verified_table").Scan(&rows); err != nil {
		t.Fatalf("count the mirror: %v", err)
	}
	if rows != verifiedVectorLength() {
		t.Fatalf("mirror rows after repair = %d, want %d", rows, verifiedVectorLength())
	}
}

// TestVerifiedLedgerRepairsVectorLayoutDrift proves a missing generation row and
// an out-of-range mirror row are repaired by clearing and reseeding, not by an
// in-place rename that could collide on the UNIQUE table_name.
func TestVerifiedLedgerRepairsVectorLayoutDrift(t *testing.T) {
	path := t.TempDir() + "/vector-layout.db"
	_, seeded := seedLedgerStore(t, path)
	db := openVerifiedTablesTestDB(t, path)
	if _, err := db.Exec("DELETE FROM provenance_table_generation WHERE table_id = 2"); err != nil {
		t.Fatalf("delete a generation row: %v", err)
	}
	if _, err := db.Exec("INSERT INTO provenance_verified_table(table_id, generation) VALUES (?1, 0)", verifiedVectorLength()); err != nil {
		t.Fatalf("insert an out-of-range mirror row: %v", err)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after vector layout drift: %v", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded+1 {
		t.Fatalf("verified_count after layout drift = %d, want %d; the full path did not run exactly once", count, seeded+1)
	}
	var generationRows, mirrorRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM provenance_table_generation").Scan(&generationRows); err != nil {
		t.Fatalf("count generation rows: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM provenance_verified_table").Scan(&mirrorRows); err != nil {
		t.Fatalf("count mirror rows: %v", err)
	}
	if generationRows != verifiedVectorLength() || mirrorRows != verifiedVectorLength() {
		t.Fatalf("repaired rows = generation %d / mirror %d, want %d", generationRows, mirrorRows, verifiedVectorLength())
	}
}

// TestVerifiedLedgerPartialPathHealsMissingModel covers the model-registry
// branch: the caller's registry notices a missing seeded model, so the full path
// runs and activation re-seeds it.
func TestVerifiedLedgerPartialPathHealsMissingModel(t *testing.T) {
	path := t.TempDir() + "/missing-model.db"
	models := []ptypes.ModelEntry{{Provider: "openai", Name: "gpt-test", DisplayName: "GPT Test"}}
	seedLedgerStoreWithModels(t, path, models)
	db := openVerifiedTablesTestDB(t, path)
	providerName, modelName := "openai", "gpt-test"
	_, seeded := readLedgerGeneration(t, path)
	if _, err := db.Exec("DELETE FROM ml_models WHERE name=?1 AND provider_id=(SELECT id FROM providers WHERE name=?2)", modelName, providerName); err != nil {
		t.Fatalf("delete the seeded model: %v", err)
	}
	if err := reopenWithModels(t, path, models); err != nil {
		t.Fatalf("open after deleting the model: %v", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded+1 {
		t.Fatalf("verified_count after a missing model = %d, want %d; the full path did not run exactly once", count, seeded+1)
	}
	var restored int
	if err := db.QueryRow("SELECT COUNT(*) FROM ml_models WHERE name=?1 AND provider_id=(SELECT id FROM providers WHERE name=?2)", modelName, providerName).Scan(&restored); err != nil {
		t.Fatalf("count the restored model: %v", err)
	}
	if restored != 1 {
		t.Fatalf("restored model rows = %d, want 1", restored)
	}
}

func reopenWithModels(t *testing.T, path string, models []ptypes.ModelEntry) error {
	t.Helper()
	db, err := Open(path, models)
	if err != nil {
		return err
	}
	return db.Close()
}

// TestVerifiedLedgerAgentSetTornFallsBack proves an empty verified set on a
// populated registry is treated as unknown: the partial path runs the full
// orphan probes (fail-safe) and rewrites the set.
func TestVerifiedLedgerAgentSetTornFallsBack(t *testing.T) {
	path := t.TempDir() + "/torn-agent-set.db"
	_, seeded := seedLedgerStore(t, path)
	db := openVerifiedTablesTestDB(t, path)
	if _, err := db.Exec("DELETE FROM " + verifiedAgentProofTable); err != nil {
		t.Fatalf("clear the verified agent set: %v", err)
	}
	store, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen with a torn agent set: %v", err)
	}
	if _, err := store.RegisterSoftwareAgent("verified", "torn-set-agent", "0", "open_verified_tables_test"); err != nil {
		t.Fatalf("register the direct agent: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close after the direct registration: %v", err)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after the torn-set registration: %v", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded {
		t.Fatalf("verified_count after a torn-set recovery = %d, want %d", count, seeded)
	}
	registry := requireVerifiedAgentSetEqualsRegistry(t, db)
	var named int
	if err := db.QueryRow("SELECT COUNT(*) FROM agents_software s JOIN " + verifiedAgentProofTable + " v ON v.agent_id = s.agent_id WHERE s.name='torn-set-agent'").Scan(&named); err != nil {
		t.Fatalf("read torn-set-agent in the rewritten set: %v", err)
	}
	if named != 1 {
		t.Fatalf("rewritten verified set does not contain torn-set-agent (found=%d); registry=%v", named, registry)
	}
}

// TestVerifiedLedgerAgentSetTornRefusesDanglingReference proves the empty-set
// fallback runs the full orphan probes: a deleted agent with live journal
// references is refused even when the verified set is empty.
func TestVerifiedLedgerAgentSetTornRefusesDanglingReference(t *testing.T) {
	path := t.TempDir() + "/torn-agent-set-dangling.db"
	_, seeded := seedLedgerStore(t, path)
	db := openVerifiedTablesTestDB(t, path)
	if _, err := db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	var agentID string
	if err := db.QueryRow("SELECT agent_id FROM agents_software WHERE name='writer'").Scan(&agentID); err != nil {
		t.Fatalf("read the fixture writer agent: %v", err)
	}
	if _, err := db.Exec("DELETE FROM agents_software WHERE agent_id=?1", agentID); err != nil {
		t.Fatalf("delete the writer subtype row: %v", err)
	}
	if _, err := db.Exec("DELETE FROM agents WHERE id=?1", agentID); err != nil {
		t.Fatalf("delete the writer agent row: %v", err)
	}
	if _, err := db.Exec("DELETE FROM " + verifiedAgentProofTable); err != nil {
		t.Fatalf("clear the verified agent set: %v", err)
	}
	var recordedDigest string
	if err := db.QueryRow("SELECT agent_digest FROM " + verifiedAgentMetaTable + " WHERE id = 1").Scan(&recordedDigest); err != nil {
		t.Fatalf("read the verified agent digest before the refused open: %v", err)
	}
	err := reopenFileBacked(t, path)
	if !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("opening a torn-set store with a dangling journal actor = %v, want ErrSubtypeIntegrity", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded {
		t.Fatalf("a refused open advanced verified_count to %d, want %d", count, seeded)
	}
	var stillPresent int
	if err := db.QueryRow("SELECT COUNT(*) FROM agents WHERE id=?1", agentID).Scan(&stillPresent); err != nil {
		t.Fatalf("read the deleted agent after the refused open: %v", err)
	}
	if stillPresent != 0 {
		t.Fatalf("refused open restored deleted agent %s", agentID)
	}
	var proofRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + verifiedAgentProofTable).Scan(&proofRows); err != nil {
		t.Fatalf("count the verified agent set after the refused open: %v", err)
	}
	if proofRows != 0 {
		t.Fatalf("refused open replaced the empty verified set with %d rows", proofRows)
	}
	var proofHit int
	if err := db.QueryRow("SELECT COUNT(*) FROM "+verifiedAgentProofTable+" WHERE agent_id=?1", agentID).Scan(&proofHit); err != nil {
		t.Fatalf("read the deleted agent in the verified set: %v", err)
	}
	if proofHit != 0 {
		t.Fatalf("refused open recorded deleted agent %s in the verified set", agentID)
	}
	var digestAfter string
	if err := db.QueryRow("SELECT agent_digest FROM " + verifiedAgentMetaTable + " WHERE id = 1").Scan(&digestAfter); err != nil {
		t.Fatalf("read the verified agent digest after the refused open: %v", err)
	}
	if digestAfter != recordedDigest {
		t.Fatalf("refused open rewrote verified agent digest %q -> %q", recordedDigest, digestAfter)
	}
	err = reopenFileBacked(t, path)
	if !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("second open of a torn-set store with a dangling journal actor = %v, want ErrSubtypeIntegrity", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded {
		t.Fatalf("second refused open advanced verified_count to %d, want %d", count, seeded)
	}
}

// TestVerifiedLedgerAgentSetDigestMismatchForcesFullProbes is the partial-set
// regression: deleting one agent and only its proof row, while other proof rows
// remain, must not be trusted as a smaller verified registry. The recorded
// digest no longer matches, so the open runs the full orphan probes and
// refuses the dangling journal actor without advancing verified_count.
func TestVerifiedLedgerAgentSetDigestMismatchForcesFullProbes(t *testing.T) {
	path := t.TempDir() + "/agent-digest-mismatch.db"
	_, seeded := seedLedgerStore(t, path)
	store, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen before the second registration: %v", err)
	}
	if _, err := store.RegisterSoftwareAgent("verified", "kept-agent", "0", "open_verified_tables_test"); err != nil {
		t.Fatalf("register the second agent: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close after the second registration: %v", err)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open to persist the two-agent set: %v", err)
	}

	db := openVerifiedTablesTestDB(t, path)
	registry := requireVerifiedAgentSetEqualsRegistry(t, db)
	if len(registry) < 2 {
		t.Fatalf("verified agent set = %v, want at least two agents", registry)
	}
	var agentID string
	if err := db.QueryRow("SELECT agent_id FROM agents_software WHERE name='writer'").Scan(&agentID); err != nil {
		t.Fatalf("read the fixture writer agent: %v", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	if _, err := db.Exec("DELETE FROM agents_software WHERE agent_id=?1", agentID); err != nil {
		t.Fatalf("delete the writer subtype row: %v", err)
	}
	if _, err := db.Exec("DELETE FROM agents WHERE id=?1", agentID); err != nil {
		t.Fatalf("delete the writer agent row: %v", err)
	}
	if _, err := db.Exec("DELETE FROM "+verifiedAgentProofTable+" WHERE agent_id=?1", agentID); err != nil {
		t.Fatalf("delete only the writer proof row: %v", err)
	}
	remaining := orderedAgentIDs(t, db, "SELECT agent_id FROM "+verifiedAgentProofTable+" ORDER BY agent_id")
	if len(remaining) == 0 || slices.Contains(remaining, agentID) {
		t.Fatalf("remaining proof rows = %v, want the other agents and not %s", remaining, agentID)
	}
	var recorded string
	if err := db.QueryRow("SELECT agent_digest FROM " + verifiedAgentMetaTable + " WHERE id = 1").Scan(&recorded); err != nil {
		t.Fatalf("read the unchanged verified agent digest: %v", err)
	}
	if recorded == verifiedAgentSetDigest(remaining) {
		t.Fatalf("verified agent digest %q matches the torn proof set; the mismatch precondition did not hold", recorded)
	}

	err = reopenFileBacked(t, path)
	if !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("opening a digest-mismatched set with a dangling journal actor = %v, want ErrSubtypeIntegrity", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded {
		t.Fatalf("a refused open advanced verified_count to %d, want %d", count, seeded)
	}
}

// TestVerifiedLedgerAgentSetSameCardinalitySubstitutionForcesFullProbes is the
// substitution a count cannot see: delete one agent and its proof row, then
// insert a different proof ID so the proof-row count is unchanged. The stored
// digest still differs from the recomputed digest, so the open runs the full
// orphan probes, finds the deleted agent's journal references, and refuses
// without advancing verified_count. A cardinality guard would trust the set.
func TestVerifiedLedgerAgentSetSameCardinalitySubstitutionForcesFullProbes(t *testing.T) {
	const substituted = "https://example.test/substituted"
	path := t.TempDir() + "/agent-digest-substitution.db"
	_, seeded := seedLedgerStore(t, path)
	store, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen before the second registration: %v", err)
	}
	if _, err := store.RegisterSoftwareAgent("verified", "kept-agent", "0", "open_verified_tables_test"); err != nil {
		t.Fatalf("register the second agent: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close after the second registration: %v", err)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open to persist the two-agent set: %v", err)
	}

	db := openVerifiedTablesTestDB(t, path)
	registry := requireVerifiedAgentSetEqualsRegistry(t, db)
	if len(registry) < 2 {
		t.Fatalf("verified agent set = %v, want at least two agents", registry)
	}
	proofBefore := len(registry)
	var agentID string
	if err := db.QueryRow("SELECT agent_id FROM agents_software WHERE name='writer'").Scan(&agentID); err != nil {
		t.Fatalf("read the fixture writer agent: %v", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	if _, err := db.Exec("DELETE FROM agents_software WHERE agent_id=?1", agentID); err != nil {
		t.Fatalf("delete the writer subtype row: %v", err)
	}
	if _, err := db.Exec("DELETE FROM agents WHERE id=?1", agentID); err != nil {
		t.Fatalf("delete the writer agent row: %v", err)
	}
	if _, err := db.Exec("DELETE FROM "+verifiedAgentProofTable+" WHERE agent_id=?1", agentID); err != nil {
		t.Fatalf("delete the writer proof row: %v", err)
	}
	if _, err := db.Exec("INSERT INTO "+verifiedAgentProofTable+" (agent_id) VALUES (?1)", substituted); err != nil {
		t.Fatalf("insert the substituted proof row: %v", err)
	}
	remaining := orderedAgentIDs(t, db, "SELECT agent_id FROM "+verifiedAgentProofTable+" ORDER BY agent_id")
	if len(remaining) != proofBefore || !slices.Contains(remaining, substituted) || slices.Contains(remaining, agentID) {
		t.Fatalf("substituted proof rows = %v, want %d rows including %q and not %s", remaining, proofBefore, substituted, agentID)
	}
	var recorded string
	if err := db.QueryRow("SELECT agent_digest FROM " + verifiedAgentMetaTable + " WHERE id = 1").Scan(&recorded); err != nil {
		t.Fatalf("read the unchanged verified agent digest: %v", err)
	}
	if recorded == verifiedAgentSetDigest(remaining) {
		t.Fatalf("verified agent digest %q matches the substituted proof set; a count would also match, so this is not the substitution case", recorded)
	}

	err = reopenFileBacked(t, path)
	if !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("opening a same-cardinality substituted set with a dangling journal actor = %v, want ErrSubtypeIntegrity", err)
	}
	if _, count := readLedgerGeneration(t, path); count != seeded {
		t.Fatalf("a refused open advanced verified_count to %d, want %d", count, seeded)
	}
}
