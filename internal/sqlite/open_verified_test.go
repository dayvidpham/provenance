package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// The tests below prove the verified-as-of ledger (open_verified.go): an
// unchanged store skips VerifyIntegrity and ReplayProjections at open, the two
// projection-damage classes are still refused at open, and an absent or stale
// ledger falls back to the full path. They observe the skip through the ledger's
// own verified_count, which only a whole-store verification advances, rather
// than through timing.
//
// None of them calls t.Parallel(): each opens and closes unnamed stores on its
// own file, and the ledger helpers below read the same file from a second
// read-only connection while the store is closed.

// ledgerRow reads the persisted ledger through an independent read-only
// connection. The store must be closed, so the reading connection sees the
// committed row and not an open pool's write.
func ledgerRow(t *testing.T, path string) (count int64, present bool) {
	t.Helper()
	raw, err := sql.Open(sqliteDriverName, "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open read-only ledger reader on %q: %v", path, err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close ledger reader: %v", err)
		}
	}()
	if err := raw.QueryRow("SELECT verified_count FROM provenance_verified_store WHERE id = 1").Scan(&count); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false
		}
		t.Fatalf("read verified_count from %q: %v", path, err)
	}
	return count, true
}

// bumpStoreGeneration advances the DML change counter directly, which is what a
// write to any covered table does through its trigger. It changes nothing about
// the store's content, so the next open must take the full path and still accept
// the store.
func bumpStoreGeneration(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open(sqliteDriverName, "file:"+path)
	if err != nil {
		t.Fatalf("open generation bumper on %q: %v", path, err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close generation bumper: %v", err)
		}
	}()
	if _, err := raw.Exec("UPDATE " + storeGenerationTable + " SET generation = generation + 1 WHERE id = 1"); err != nil {
		t.Fatalf("bump store generation: %v", err)
	}
}

// dropVerifiedLedger deletes the ledger row, the state a crash between a write
// and its ledger update leaves behind.
func dropVerifiedLedger(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open(sqliteDriverName, "file:"+path)
	if err != nil {
		t.Fatalf("open ledger dropper on %q: %v", path, err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close ledger dropper: %v", err)
		}
	}()
	if _, err := raw.Exec("DELETE FROM " + verifiedStoreTable); err != nil {
		t.Fatalf("drop verified ledger: %v", err)
	}
}

// seedLedgerStore activates a store and journals exactly one task through the
// production fold, so the store has a journal-anchored task whose projection a
// test can damage out of band. It then reopens the store once: the writes it just
// made are a store change the ledger recorded before them, so that reopen is what
// leaves the ledger describing the store as it stands — the state an unchanged
// store's open must be able to skip from. It returns the task and the ledger's
// verified_count at that point, which only a whole-store verification advances.
func seedLedgerStore(t *testing.T, path string) (ptypes.TaskID, int64) {
	t.Helper()
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("activate ledger store on %q: %v", path, err)
	}
	actor, err := db.RegisterSoftwareAgent("verified", "writer", "0", "open_verified_test")
	if err != nil {
		t.Fatalf("register writer: %v", err)
	}
	boot := genesisBoot(t, db, actor.ID)
	task := ptypes.TaskID{Namespace: "verified", UUID: uuid.Must(uuid.NewV7())}
	if _, err := db.Apply(journal.OperationInput{
		OperationID:        "op-verified-create",
		ActorID:            actor.ID,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("verified-create-c"),
		MutationDigest:     []byte("verified-create-m"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort: journal.EffectTaskCreate, TaskID: task, Title: "verified task",
			Description: "task for the verified-as-of ledger tests",
			Type:        ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseWorkerSlices,
		}},
	}); err != nil {
		t.Fatalf("journal-create the fixture task: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close ledger store after seeding: %v", err)
	}
	// The seeding writes made the ledger stale, so this reopen runs the full path
	// once and re-persists the ledger for the state the store is left in.
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("reopen to establish a current ledger: %v", err)
	}
	count, present := ledgerRow(t, path)
	if !present {
		t.Fatal("the reopen did not persist a verified-as-of ledger")
	}
	return task, count
}

// journalEmptyTailTask journals one task creation, which writes the first
// journal rows into a store that had none.
func journalEmptyTailTask(t *testing.T, db *DB, actor journal.ActorID, boot journal.JournalID) {
	t.Helper()
	task := ptypes.TaskID{Namespace: "verified", UUID: uuid.Must(uuid.NewV7())}
	if _, err := db.Apply(journal.OperationInput{
		OperationID:        "op-empty-tail-create",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("empty-tail-c"),
		MutationDigest:     []byte("empty-tail-m"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort: journal.EffectTaskCreate, TaskID: task, Title: "first journaled task",
			Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped,
		}},
	}); err != nil {
		t.Fatalf("journal the first task row: %v", err)
	}
}

// reopenFileBacked opens and closes path through the production file-backed
// entry point.
func reopenFileBacked(t *testing.T, path string) error {
	t.Helper()
	db, err := Open(path, nil)
	if err != nil {
		return err
	}
	return db.Close()
}

// TestVerifiedLedgerCoveredTablesMatchActivationObjects pins the ledger's covered
// table list to the tables this build's activation is expected to create. The
// list is written out as literals (every statement this package executes must be
// decidable from source), so this test is what keeps it from drifting away from
// expectedActivationObjects: a table activation creates but the ledger does not
// cover would let a change to that table pass the fast path unnoticed, and a
// table the ledger covers that activation does not create would make a fresh
// store look permanently stale.
func TestVerifiedLedgerCoveredTablesMatchActivationObjects(t *testing.T) {
	want := make([]string, 0, len(expectedActivationObjects))
	for _, object := range expectedActivationObjects {
		if object.kind != "table" || object.name == storeGenerationTable || object.name == verifiedStoreTable {
			continue
		}
		want = append(want, object.name)
	}
	if !slices.Equal(verifiedCoveredTableNames, want) {
		t.Fatalf("verifiedCoveredTableNames = %v, want the activation tables %v", verifiedCoveredTableNames, want)
	}
	if len(verifiedTriggerEvents) == 0 {
		t.Fatal("no trigger events; the change trigger set would be empty and nothing would advance the generation")
	}
	for _, event := range verifiedTriggerEvents {
		if event != strings.ToUpper(event) {
			t.Fatalf("trigger event %q is not upper case; SQLite identifiers are case-insensitive, so a case-only difference could collide", event)
		}
	}
}

// TestVerifiedLedgerFactContextRelationStillRejectsForeignTriggers proves the
// ledger did not widen the fact-context shape contract into accepting a
// trigger-driven relation. The classifier now tolerates triggers that carry the
// ledger's own name prefix; any other trigger on journal_decision_contexts or
// journal_evidence_contexts is still refused, so a relation whose static foreign
// key was replaced by a trigger is rejected exactly as before.
func TestVerifiedLedgerFactContextRelationStillRejectsForeignTriggers(t *testing.T) {
	path := t.TempDir() + "/fact-context-trigger.db"
	seedLedgerStore(t, path)

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen before injecting the trigger: %v", err)
	}
	scope := takePoolScope(t, db)
	if _, err := scope.conn.ExecContext(scope.ctx,
		"CREATE TRIGGER injected_relation_trigger BEFORE INSERT ON journal_decision_contexts BEGIN SELECT RAISE(ABORT, 'injected'); END"); err != nil {
		scope.release()
		_ = db.Close()
		t.Fatalf("inject the foreign trigger: %v", err)
	}
	scope.release()
	if err := db.Close(); err != nil {
		t.Fatalf("close after injecting the trigger: %v", err)
	}

	err = reopenFileBacked(t, path)
	if !errors.Is(err, journal.ErrFactContextIntegrity) {
		t.Fatalf("opening a store whose fact-context relation carries a foreign trigger = %v, want journal.ErrFactContextIntegrity", err)
	}
}

// TestVerifiedLedgerFastPathOnAStoreWithNoJournalRows covers the store shape the
// ledger has to treat specially: one with no journal rows at all, where the
// verified identity is the zero journal id and the empty tail digest. A fresh
// store's second open must still be the fast path, and adding the first journal
// row must take it back to the full path.
func TestVerifiedLedgerFastPathOnAStoreWithNoJournalRows(t *testing.T) {
	path := t.TempDir() + "/empty-journal.db"
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("activate an empty store: %v", err)
	}
	actor, err := db.RegisterSoftwareAgent("verified", "writer", "0", "open_verified_test")
	if err != nil {
		t.Fatalf("register writer: %v", err)
	}
	// The registration is the store's only write, and it happened after activation
	// persisted the ledger, so one reopen records the state the store is left in.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("reopen to establish the ledger: %v", err)
	}
	count, present := ledgerRow(t, path)
	if !present {
		t.Fatal("the ledger is missing")
	}
	// The journal is still empty: the recorded identity is the zero tail.
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open an empty-journal store: %v", err)
	}
	if after, _ := ledgerRow(t, path); after != count {
		t.Fatalf("an unchanged empty-journal store's open moved verified_count to %d, want %d", after, count)
	}
	// The first journal row makes the tail real and must send the next open back
	// down the full path.
	db, err = Open(path, nil)
	if err != nil {
		t.Fatalf("reopen to journal the first row: %v", err)
	}
	boot := genesisBoot(t, db, actor.ID)
	journalEmptyTailTask(t, db, actor.ID, boot)
	if err := db.Close(); err != nil {
		t.Fatalf("close after the first journal row: %v", err)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after the first journal row: %v", err)
	}
	if after, _ := ledgerRow(t, path); after != count+1 {
		t.Fatalf("verified_count after the tail became non-empty = %d, want %d; the full path must have run", after, count+1)
	}
}

// TestVerifiedLedgerPresenceOfCoveredTablesIsPartOfItsIdentity pins the
// covered-table presence digest the ledger records. A covered table that
// disappears — raw DDL, no data write, so the DML generation is untouched — must
// stop the ledger matching on its own, without help from the currency probe's
// object check, and the next open must run the full checks. A covered table that
// appears afterwards is the same mismatch in the other direction, because the
// digest is over the set.
func TestVerifiedLedgerPresenceOfCoveredTablesIsPartOfItsIdentity(t *testing.T) {
	path := t.TempDir() + "/presence.db"
	_, seeded := seedLedgerStore(t, path)

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen to read the ledger: %v", err)
	}
	scope := takePoolScope(t, db)
	before, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		scope.release()
		_ = db.Close()
		t.Fatalf("read the store identity: %v", err)
	}
	if current, err := scope.verifiedLedgerCurrent(); err != nil || !current {
		scope.release()
		_ = db.Close()
		t.Fatalf("a freshly re-verified store must be current: current=%v err=%v", current, err)
	}
	// Raw DDL drops a covered table. DDL fires no change trigger, so the store's
	// DML generation is provably unchanged by it. governed_allocation_genesis is
	// empty, so dropping it violates no foreign key, and activation recreates it.
	if _, err := scope.conn.ExecContext(scope.ctx, "DROP TABLE governed_allocation_genesis"); err != nil {
		scope.release()
		_ = db.Close()
		t.Fatalf("drop a covered table: %v", err)
	}
	after, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		scope.release()
		_ = db.Close()
		t.Fatalf("read the store identity after the drop: %v", err)
	}
	currentAfter, err := scope.verifiedLedgerCurrent()
	scope.release()
	if err != nil {
		t.Fatalf("ledger check after the drop: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if after.generation != before.generation || after.journalID != before.journalID || after.digest != before.digest {
		t.Fatalf("the drop advanced the DML identity; this test needs DDL that fires no change trigger: before=%+v after=%+v", before, after)
	}
	if after.tablesDigest == before.tablesDigest {
		t.Fatalf("covered-table presence digest unchanged after dropping a covered table: %q", after.tablesDigest)
	}
	if currentAfter {
		t.Fatal("the ledger still matched a store missing a covered table it was written with")
	}

	// The digest is over the set, so a table appearing is the same comparison in
	// the other direction.
	present := slices.Clone(verifiedCoveredTableNames)
	slices.Sort(present)
	if verifiedTablesDigest(present) != before.tablesDigest {
		t.Fatalf("the digest of the full covered set %q is not what the ledger recorded (%q)", verifiedTablesDigest(present), before.tablesDigest)
	}
	reduced := slices.DeleteFunc(slices.Clone(present), func(name string) bool { return name == "governed_allocation_genesis" })
	if verifiedTablesDigest(reduced) == before.tablesDigest {
		t.Fatal("removing a covered table did not change the presence digest")
	}
	appeared := append(slices.Clone(reduced), "governed_allocation_genesis")
	if verifiedTablesDigest(appeared) != before.tablesDigest {
		t.Fatal("adding a covered table back did not restore the presence digest; the digest is not set-based")
	}

	// The ledger check above is what makes the fast path unavailable. The open must
	// therefore take the full path, and activation must repair the store: the
	// covered table is recreated and the ledger describes the store again.
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after a covered table disappeared: %v", err)
	}
	if count, ok := ledgerRow(t, path); !ok || count < seeded {
		t.Fatalf("ledger row after the repair: present=%v count=%d, want present with count >= %d", ok, count, seeded)
	}
	repaired, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen the repaired store: %v", err)
	}
	scope = takePoolScope(t, repaired)
	var recreated bool
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='governed_allocation_genesis')").Scan(&recreated); err != nil {
		scope.release()
		_ = repaired.Close()
		t.Fatalf("probe the recreated covered table: %v", err)
	}
	repairedCurrent, err := scope.verifiedLedgerCurrent()
	scope.release()
	if err != nil {
		_ = repaired.Close()
		t.Fatalf("ledger check after the repair: %v", err)
	}
	if err := repaired.Close(); err != nil {
		t.Fatalf("close the repaired store: %v", err)
	}
	if !recreated {
		t.Fatal("the open did not repair the store: the covered table is still missing")
	}
	if !repairedCurrent {
		t.Fatal("the ledger does not describe the repaired store")
	}
}

// TestVerifiedLedgerCreatesNoTriggerForAnAbsentCoveredTable pins the rule that
// makes the ledger's trigger set safe while a caller completes a store's schema:
// trigger creation covers only the covered tables that exist. A table this step
// does not own is never created to satisfy its own triggers, and an absent
// covered table leaves no trigger behind — its absence alone fails the currency
// probe, and the presence digest recorded in the row (see
// TestVerifiedLedgerPresenceOfCoveredTablesIsPartOfItsIdentity) makes its later
// reappearance stop the ledger matching too.
func TestVerifiedLedgerCreatesNoTriggerForAnAbsentCoveredTable(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/partial-store.db"
	pool, err := sql.Open(sqliteDriverName, "file:"+path)
	if err != nil {
		t.Fatalf("open partial pool: %v", err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("close partial pool: %v", err)
		}
	}()
	pool.SetMaxOpenConns(1)
	// A deliberately partial store: only two of the covered tables exist.
	for _, statement := range []string{
		`CREATE TABLE tasks (id TEXT PRIMARY KEY, namespace TEXT NOT NULL, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', status_id INTEGER NOT NULL DEFAULT 0, priority_id INTEGER NOT NULL DEFAULT 2, type_id INTEGER NOT NULL DEFAULT 2, phase_id INTEGER NOT NULL, owner_id TEXT, notes TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, closed_at INTEGER, close_reason TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE agents (id TEXT PRIMARY KEY, kind_id INTEGER NOT NULL)`,
	} {
		if _, err := pool.Exec(statement); err != nil {
			t.Fatalf("build partial store: %v", err)
		}
	}

	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("lease partial connection: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close partial connection: %v", err)
		}
	}()
	scope := borrowConnScope(conn, projectionTargetLive)
	if err := scope.ensureVerifiedLedgerSchema(); err != nil {
		t.Fatalf("ensureVerifiedLedgerSchema on a store missing covered tables: %v", err)
	}
	for _, want := range []struct {
		trigger string
		present bool
	}{
		{"provenance_store_generation_on_tasks_INSERT", true},
		{"provenance_store_generation_on_agents_DELETE", true},
		{"provenance_store_generation_on_comments_INSERT", false},
		{"provenance_store_generation_on_journal_UPDATE", false},
	} {
		var exists bool
		if err := scope.conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='trigger' AND name=?1)", want.trigger).Scan(&exists); err != nil {
			t.Fatalf("probe trigger %s: %v", want.trigger, err)
		}
		if exists != want.present {
			t.Fatalf("trigger %s present=%v, want %v", want.trigger, exists, want.present)
		}
	}
	// The step created no covered table it does not own; it created only its own
	// two ledger tables.
	for _, want := range []struct {
		table   string
		present bool
	}{
		{"comments", false},
		{"journal", false},
		{storeGenerationTable, true},
		{verifiedStoreTable, true},
	} {
		var exists bool
		if err := scope.conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?1)", want.table).Scan(&exists); err != nil {
			t.Fatalf("probe table %s: %v", want.table, err)
		}
		if exists != want.present {
			t.Fatalf("table %s present=%v, want %v", want.table, exists, want.present)
		}
	}
}

// TestVerifiedLedgerSkipsWholeStoreChecksOnUnchangedStore is the fast-path proof.
// The ledger's verified_count advances only when a whole-store verification runs,
// so an unchanged store's second open leaving the count untouched is direct
// evidence that VerifyIntegrity and ReplayProjections did not run. The third open
// is the control: advancing the generation alone (no content change) makes the
// next open run the full path, and it then leaves the store current again.
func TestVerifiedLedgerSkipsWholeStoreChecksOnUnchangedStore(t *testing.T) {
	path := t.TempDir() + "/unchanged.db"
	_, countAfterSeed := seedLedgerStore(t, path)

	// Unchanged store: the whole-store checks must be skipped, so the ledger is
	// left byte-identical and its count does not move.
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open unchanged store: %v", err)
	}
	if count, _ := ledgerRow(t, path); count != countAfterSeed {
		t.Fatalf("an unchanged store's open moved verified_count from %d to %d; VerifyIntegrity/ReplayProjections ran on the fast path", countAfterSeed, count)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("second open of the unchanged store: %v", err)
	}
	if count, _ := ledgerRow(t, path); count != countAfterSeed {
		t.Fatalf("repeated open of an unchanged store moved verified_count to %d, want %d", count, countAfterSeed)
	}

	// Control: with the store's content untouched but its generation advanced, the
	// ledger no longer matches, so the open must run the full path — and because
	// the store is genuinely converged, that verification passes and advances the
	// count.
	bumpStoreGeneration(t, path)
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open a store whose generation moved: %v", err)
	}
	countAfterStale, present := ledgerRow(t, path)
	if !present {
		t.Fatal("the full path did not re-persist the ledger")
	}
	if countAfterStale != countAfterSeed+1 {
		t.Fatalf("verified_count after a stale-ledger open = %d, want %d; the full path did not run exactly once", countAfterStale, countAfterSeed+1)
	}

	// Current again: the re-persisted ledger makes the next open a skip.
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after the ledger was re-persisted: %v", err)
	}
	if count, _ := ledgerRow(t, path); count != countAfterStale {
		t.Fatalf("verified_count moved to %d after a re-persisted ledger; want %d", count, countAfterStale)
	}
}

// TestVerifiedLedgerRefusesProjectionDamageAtOpen proves both named damage
// classes are still refused at open even though the ledger exists. Both are
// out-of-band writes to covered projection tables, so each one advances the
// generation and sends the next open down the full path, where the convergence
// guard refuses it.
func TestVerifiedLedgerRefusesProjectionDamageAtOpen(t *testing.T) {
	t.Run("owner projection disagrees with the folded journal", func(t *testing.T) {
		path := t.TempDir() + "/owner-damage.db"
		task, seeded := seedLedgerStore(t, path)

		db, err := Open(path, nil)
		if err != nil {
			t.Fatalf("reopen before damaging: %v", err)
		}
		other, err := db.RegisterSoftwareAgent("verified", "other-owner", "0", "open_verified_test")
		if err != nil {
			t.Fatalf("register the other owner: %v", err)
		}
		if err := db.AdversarialCorruptTaskProjection(task, AdversarialFieldOwner, other.ID.String()); err != nil {
			t.Fatalf("corrupt the owner projection: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close the damaged store: %v", err)
		}

		before, present := ledgerRow(t, path)
		if !present {
			t.Fatal("the ledger is missing before the damaged open")
		}
		if before != seeded {
			t.Fatalf("the pre-damage open moved verified_count to %d (want %d): it must have taken the skipped path, so that the damage alone sends the next open down the refusal path", before, seeded)
		}
		err = reopenFileBacked(t, path)
		if !errors.Is(err, journal.ErrProjectionDivergence) {
			t.Fatalf("opening a store whose owner projection disagrees with the folded journal = %v, want journal.ErrProjectionDivergence", err)
		}
		if after, _ := ledgerRow(t, path); after != before {
			t.Fatalf("a refused open re-persisted the ledger (count %d -> %d); a refused store must not be recorded as verified", before, after)
		}
	})

	t.Run("out-of-range tasks.phase_id", func(t *testing.T) {
		path := t.TempDir() + "/phase-damage.db"
		task, seeded := seedLedgerStore(t, path)

		db, err := Open(path, nil)
		if err != nil {
			t.Fatalf("reopen before damaging: %v", err)
		}
		if count, _ := ledgerRow(t, path); count != seeded {
			t.Fatalf("the pre-damage open moved verified_count to %d (want %d): it must have taken the skipped path", count, seeded)
		}
		scope := takePoolScope(t, db)
		// A phase id the fold never derives for this task: a legal foreign key, so
		// only the from-empty replay convergence guard can refuse it.
		if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE tasks SET phase_id = ?1 WHERE id = ?2", int64(ptypes.PhaseUnscoped), task.String()); err != nil {
			scope.release()
			t.Fatalf("corrupt the stored phase: %v", err)
		}
		scope.release()
		if err := db.Close(); err != nil {
			t.Fatalf("close the damaged store: %v", err)
		}

		err = reopenFileBacked(t, path)
		if !errors.Is(err, journal.ErrProjectionDivergence) {
			t.Fatalf("opening a store with an out-of-range tasks.phase_id = %v, want journal.ErrProjectionDivergence", err)
		}
	})
}

// TestVerifiedLedgerAbsentOrStaleTakesFullPath is the crash/partial-state proof:
// a marker that is absent, or a store that moved after the marker was written,
// must run the full checks rather than skip, and a damaged store must still be
// refused when its marker is missing entirely.
func TestVerifiedLedgerAbsentOrStaleTakesFullPath(t *testing.T) {
	t.Run("absent marker on a clean store", func(t *testing.T) {
		path := t.TempDir() + "/absent.db"
		seedLedgerStore(t, path)
		dropVerifiedLedger(t, path)

		if err := reopenFileBacked(t, path); err != nil {
			t.Fatalf("open with an absent ledger: %v", err)
		}
		count, present := ledgerRow(t, path)
		if !present {
			t.Fatal("an absent ledger was not re-persisted by the full path")
		}
		if count != 1 {
			t.Fatalf("verified_count after re-persisting an absent ledger = %d, want 1; a fresh ledger row must start at one run", count)
		}
	})

	t.Run("absent marker on a damaged store is still refused", func(t *testing.T) {
		path := t.TempDir() + "/absent-damaged.db"
		task, _ := seedLedgerStore(t, path)

		db, err := Open(path, nil)
		if err != nil {
			t.Fatalf("reopen before damaging: %v", err)
		}
		other, err := db.RegisterSoftwareAgent("verified", "other-owner", "0", "open_verified_test")
		if err != nil {
			t.Fatalf("register the other owner: %v", err)
		}
		if err := db.AdversarialCorruptTaskProjection(task, AdversarialFieldOwner, other.ID.String()); err != nil {
			t.Fatalf("corrupt the owner projection: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close the damaged store: %v", err)
		}
		// The crash shape: the damage committed and the ledger update did not.
		dropVerifiedLedger(t, path)
		bumpStoreGeneration(t, path)

		err = reopenFileBacked(t, path)
		if !errors.Is(err, journal.ErrProjectionDivergence) {
			t.Fatalf("opening a damaged store with no ledger = %v, want journal.ErrProjectionDivergence", err)
		}
		if _, present := ledgerRow(t, path); present {
			t.Fatal("a refused open persisted a ledger row")
		}
	})

	t.Run("unreadable marker takes the full path", func(t *testing.T) {
		path := t.TempDir() + "/unreadable.db"
		seedLedgerStore(t, path)

		// An older-version row is one this build must not trust: rewrite the
		// recorded shape id to a v1 marker that no live shape id can equal.
		raw, err := sql.Open(sqliteDriverName, "file:"+path)
		if err != nil {
			t.Fatalf("open shape rewriter on %q: %v", path, err)
		}
		if _, err := raw.Exec("UPDATE " + verifiedStoreTable + " SET shape_id = 'provenance.verified-store.v1' WHERE id = 1"); err != nil {
			raw.Close()
			t.Fatalf("rewrite the shape id: %v", err)
		}
		if err := raw.Close(); err != nil {
			t.Fatalf("close shape rewriter: %v", err)
		}
		before, present := ledgerRow(t, path)
		if !present {
			t.Fatal("the ledger is missing before the older-version open")
		}

		if err := reopenFileBacked(t, path); err != nil {
			t.Fatalf("open a store whose ledger records an older version: %v", err)
		}
		after, present := ledgerRow(t, path)
		if !present {
			t.Fatal("an older-version ledger was not re-persisted by the full path")
		}
		if after != before+1 {
			t.Fatalf("verified_count after an older-version ledger = %d, want %d; the row must be refused and the full path must run", after, before+1)
		}
	})
}

// TestVerifiedLedgerFastPathTakesNoWriteLock proves the skip is also write-free:
// with a deterministic holder owning the file's write lock for the whole call, a
// current store opens without waiting for it. The full path would have to wait
// (or fail) because re-persisting the ledger needs the write lock.
func TestVerifiedLedgerFastPathTakesNoWriteLock(t *testing.T) {
	path := t.TempDir() + "/no-write-lock.db"
	seedLedgerStore(t, path)

	pool := borrowedPool(t, path)
	release := holdWriteLock(t, path)
	start := time.Now()
	store, err := OpenBorrowed(pool, nil)
	elapsed := time.Since(start)
	release()
	if err != nil {
		t.Fatalf("current borrowed open while the write lock was held: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Errorf("close current borrowed store: %v", err)
	}
	if elapsed >= activationHoldWindow {
		t.Fatalf("current borrowed open took %s while the write lock was held; the ledger fast path must not take the write lock", elapsed)
	}
}
