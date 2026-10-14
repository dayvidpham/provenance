package provenance

// borrowed_currency_convergence_test.go pins the borrowed-open currency fast path
// to activation's own convergence checks. The fast path skips the activation write
// transaction when a store's schema and closed lookups already match this build,
// but activation also refuses a store whose stored projections disagree with a
// from-empty re-derivation of the folded journal. The probe must therefore run
// that same comparison, or a diverged store is served silently with the damage
// still in place — exactly the safety hole this file regresses.
//
// Each case seeds a CONVERGED file-backed store through the real production
// writer, damages one projection out of band (no supported writer produces the
// damaged shape), then reopens through the production borrowed path and asserts
// the store is refused at open, before any gate read.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// seedConvergedBorrowedFile activates a fresh file-backed borrowed store and
// commits a well-formed journaled history: a bootstrap authority, a task born
// through the fold, and an owner episode that makes the occupant the fold-derived
// owner. It returns the still-open borrowed pool (so a test can damage the file
// out of band), the task, its fold-derived owner, and a second registered actor
// the damage cases can substitute as owner.
func seedConvergedBorrowedFile(t *testing.T) (db *sql.DB, task TaskID, owner, intruder ActorID) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "currency-convergence.db")
	pool, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open borrowed pool: %v", err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })

	tr, err := OpenBorrowedSQLite(pool, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatalf("activate borrowed file: %v", err)
	}
	ownerAgent, err := tr.RegisterSoftwareAgent("provenance-test", "currency-owner", "0", "test")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	intruderAgent, err := tr.RegisterSoftwareAgent("provenance-test", "currency-intruder", "0", "test")
	if err != nil {
		t.Fatalf("register intruder: %v", err)
	}
	owner = ownerAgent.ID
	digest := func(s string) []byte { return []byte("currency-digest--" + s) }
	gen, err := tr.Journal().Apply(OperationInput{
		OperationID: "op-currency-genesis", ActorID: owner,
		CommandDigest: digest("gc"), MutationDigest: digest("gm"),
		Effects: []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("genesis: %v", err)
	}
	boot, ok := slotJournalID(gen, "auth")
	if !ok {
		t.Fatal("genesis produced no bootstrap authority slot")
	}
	task = newCorpusTaskID()
	if _, err := tr.Journal().Apply(OperationInput{
		OperationID: "op-currency-create", ActorID: owner, AuthorityJournalID: &boot,
		CommandDigest: digest("cc"), MutationDigest: digest("cm"),
		Effects: []Effect{{Sort: EffectTaskCreate, TaskID: task, Title: "currency task", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}},
	}); err != nil {
		t.Fatalf("journaled create: %v", err)
	}
	if _, err := tr.Journal().Apply(OperationInput{
		OperationID: "op-currency-start", ActorID: owner, AuthorityJournalID: &boot,
		CommandDigest: digest("sc"), MutationDigest: digest("sm"),
		Effects: []Effect{{Sort: EffectAssignmentStart, AssignmentID: "CURRENCY-A", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: owner}},
	}); err != nil {
		t.Fatalf("start episode: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("close seeding tracker: %v", err)
	}
	return pool, task, owner, intruderAgent.ID
}

// TestBorrowedOpenRefusesProjectionDamageActivationWouldRefuse is the regression
// for the currency probe: a store whose schema is current but whose projection
// damage activation's convergence check refuses must NOT be served by the skip.
// Reopening must fail at open with the same "journal replay" refusal, for both
// reproduced divergence classes.
func TestBorrowedOpenRefusesProjectionDamageActivationWouldRefuse(t *testing.T) {
	t.Run("unmapped-phase", func(t *testing.T) {
		db, task, _, _ := seedConvergedBorrowedFile(t)
		// The insert exists because tasks.phase_id references phases.id, so the
		// update would otherwise violate a live foreign key. The value is one no
		// ordered journal history derives for this task.
		if _, err := db.Exec(`INSERT INTO phases(id,name) VALUES (99,'test-unmapped-phase')`); err != nil {
			t.Fatalf("insert out-of-range phase: %v", err)
		}
		if _, err := db.Exec(`UPDATE tasks SET phase_id=99 WHERE id=?`, task.String()); err != nil {
			t.Fatalf("corrupt task phase: %v", err)
		}
		reopened, err := OpenBorrowedSQLite(db, WithModelRegistry(NewRegistry(nil)))
		if reopened != nil {
			_ = reopened.Close()
		}
		if err == nil {
			t.Fatal("a current-schema store with an out-of-range phase must be refused at open, not served by the currency skip")
		}
		if !strings.Contains(err.Error(), "journal replay") || !strings.Contains(err.Error(), "field phase") {
			t.Fatalf("out-of-range phase refusal = %v, want a journal-replay projection divergence naming field phase", err)
		}
	})

	t.Run("projection-mismatch", func(t *testing.T) {
		db, task, _, intruder := seedConvergedBorrowedFile(t)
		if _, err := db.Exec(`UPDATE tasks SET owner_id=? WHERE id=?`, intruder.String(), task.String()); err != nil {
			t.Fatalf("corrupt task owner: %v", err)
		}
		reopened, err := OpenBorrowedSQLite(db, WithModelRegistry(NewRegistry(nil)))
		if reopened != nil {
			_ = reopened.Close()
		}
		if err == nil {
			t.Fatal("a current-schema store whose owner disagrees with the folded journal must be refused at open, not served by the currency skip")
		}
		if !strings.Contains(err.Error(), "journal replay") || !strings.Contains(err.Error(), "field owner") {
			t.Fatalf("owner divergence refusal = %v, want a journal-replay projection divergence naming field owner", err)
		}
	})
}
