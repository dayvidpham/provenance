package sqlite

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
)

// prefixFixture is a file-backed store whose ledger and verified prefix are
// current: genesis, one task, and one operation carrying a decision and an
// evidence fact with contexts.
type prefixFixture struct {
	path     string
	actor    journal.ActorID
	task     journal.TaskID
	boot     journal.JournalID
	anchor   int64
	decision int64
	evidence int64
}

func buildPrefixFixture(t *testing.T) prefixFixture {
	t.Helper()
	path := t.TempDir() + "/verified-prefix.db"
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open the prefix fixture: %v", err)
	}
	actor, task, boot := newFactContextEnvironment(t, db)
	anchor, decision, evidence := applyPrefixFacts(t, db, actor, task, boot, "prefix-base")
	if err := db.Close(); err != nil {
		t.Fatalf("close the prefix fixture: %v", err)
	}
	// The direct agent insert above made the ledger stale; the reopen is the full
	// path that records the ledger and the prefix for the state the store is in.
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("reopen to record the prefix: %v", err)
	}
	fixture := prefixFixture{path: path, actor: actor, task: task, boot: boot, anchor: anchor, decision: decision, evidence: evidence}
	through, _, ok := readPrefixRow(t, path)
	if !ok {
		t.Fatal("the reopen recorded no verified prefix")
	}
	if want := ledgerJournalID(t, path); through != want {
		t.Fatalf("recorded prefix through %d, want the ledger journal id %d", through, want)
	}
	return fixture
}

// applyPrefixFacts journals one operation with a decision and an evidence fact,
// each carrying a task and an actor context.
func applyPrefixFacts(t *testing.T, db *DB, actor journal.ActorID, task journal.TaskID, boot journal.JournalID, id string) (anchor, decision, evidence int64) {
	t.Helper()
	taskContext, err := journal.TaskContext(task)
	if err != nil {
		t.Fatal(err)
	}
	actorContext, err := journal.ActorContext(actor)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.Apply(journal.OperationInput{
		OperationID:        journal.OperationID(id),
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte(id),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{
			{Sort: journal.EffectDecision, ResultSlot: "decision", TaskID: task, DecisionKind: "fixture.prefix.decision", Payload: []byte(`{}`), Contexts: []journal.EventContext{actorContext, taskContext}},
			{Sort: journal.EffectEvidence, ResultSlot: "evidence", TaskID: task, EvidenceKind: "fixture.prefix.evidence", ContentDigest: []byte(id), Payload: []byte(`{}`), Contexts: []journal.EventContext{taskContext}},
		},
	})
	if err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}
	return int64(result.AnchorJournalID), int64(factContextResultSlot(t, result, "decision")), int64(factContextResultSlot(t, result, "evidence"))
}

func readPrefixRow(t *testing.T, path string) (through int64, digests verifiedPrefixDigests, ok bool) {
	t.Helper()
	db := openVerifiedTablesTestDB(t, path)
	err := db.QueryRow("SELECT through_journal_id, journal_digest, contexts_digest FROM "+verifiedPrefixTable+" WHERE id = 1").
		Scan(&through, &digests.journal, &digests.contexts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, verifiedPrefixDigests{}, false
	}
	if err != nil {
		t.Fatalf("read the verified prefix row: %v", err)
	}
	return through, digests, true
}

func ledgerJournalID(t *testing.T, path string) int64 {
	t.Helper()
	var id int64
	if err := openVerifiedTablesTestDB(t, path).QueryRow("SELECT journal_id FROM " + verifiedStoreTable + " WHERE id = 1").Scan(&id); err != nil {
		t.Fatalf("read the ledger journal id: %v", err)
	}
	return id
}

func maxJournalID(t *testing.T, path string) int64 {
	t.Helper()
	var id int64
	if err := openVerifiedTablesTestDB(t, path).QueryRow("SELECT COALESCE(MAX(journal_id), 0) FROM journal").Scan(&id); err != nil {
		t.Fatalf("read the journal tail: %v", err)
	}
	return id
}

// markLedgerStale moves one covered counter on the scope's connection, which is
// what any write outside a sanctioned fold does, so a following Apply is not
// eligible to advance the ledger and the ledger stays behind the store.
func markLedgerStale(t *testing.T, scope *connScope) {
	t.Helper()
	if _, err := scope.conn.ExecContext(scope.ctx,
		"UPDATE "+verifiedTableGenerationTable+" SET generation = generation + 1 WHERE table_id = ?1", verifiedTableID("tasks")); err != nil {
		t.Fatalf("mark the ledger stale: %v", err)
	}
}

// staleTailFixture is a store whose ledger is stale and whose journal has one
// operation above the verified prefix.
type staleTailFixture struct {
	prefixFixture
	db        *DB
	prefixTop int64
	tail      int64
}

func buildStaleTailFixture(t *testing.T) staleTailFixture {
	t.Helper()
	fixture := buildPrefixFixture(t)
	db, err := Open(fixture.path, nil)
	if err != nil {
		t.Fatalf("open for the tail append: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	top := maxJournalID(t, fixture.path)
	scope := takePoolScope(t, db)
	markLedgerStale(t, scope)
	scope.release()
	tail, _, _ := applyPrefixFacts(t, db, fixture.actor, fixture.task, fixture.boot, "prefix-tail")
	if tail <= top {
		t.Fatalf("tail anchor %d is not above the prefix top %d", tail, top)
	}
	return staleTailFixture{prefixFixture: fixture, db: db, prefixTop: top, tail: tail}
}

// TestVerifiedPrefixDigestCoversEveryColumn mutates each column and table the
// design lists and requires the matching digest to change, and the other to stay.
func TestVerifiedPrefixDigestCoversEveryColumn(t *testing.T) {
	fixture := buildPrefixFixture(t)
	db, err := Open(fixture.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	scope := takePoolScope(t, db)
	defer scope.release()
	through := ledgerJournalID(t, fixture.path)
	base, err := scope.computeVerifiedPrefixDigests(through)
	if err != nil {
		t.Fatalf("baseline digests: %v", err)
	}
	if again, err := scope.computeVerifiedPrefixDigests(through); err != nil || again != base {
		t.Fatalf("digests are not deterministic: %+v then %+v (%v)", base, again, err)
	}
	recorded, _, _ := readPrefixRow(t, fixture.path)
	if recorded != through {
		t.Fatalf("recorded through = %d, want %d", recorded, through)
	}
	_, row, _ := readPrefixRow(t, fixture.path)
	if row != base {
		t.Fatalf("recorded digests %+v differ from a fresh computation %+v", row, base)
	}

	const (
		journalChanges  = 1
		contextsChanges = 2
	)
	for _, test := range []struct {
		name string
		sql  string
		args []any
		want int
	}{
		{"journal.journal_id", "UPDATE journal SET journal_id=journal_id+1000000 WHERE journal_id=?1", []any{fixture.decision}, journalChanges},
		{"journal.kind_id", "UPDATE journal SET kind_id=kind_id+1 WHERE journal_id=?1", []any{fixture.decision}, journalChanges},
		{"journal.actor_id", "UPDATE journal SET actor_id='someone-else' WHERE journal_id=?1", []any{fixture.anchor}, journalChanges},
		{"journal.actor_id null", "UPDATE journal SET actor_id=NULL WHERE journal_id=?1", []any{fixture.anchor}, journalChanges},
		{"journal.recorded_at", "UPDATE journal SET recorded_at=recorded_at+1 WHERE journal_id=?1", []any{fixture.decision}, journalChanges},
		{"journal.produced_by", "UPDATE journal SET produced_by_operation_journal_id=produced_by_operation_journal_id+1 WHERE journal_id=?1", []any{fixture.decision}, journalChanges},
		{"journal.produced_by null", "UPDATE journal SET produced_by_operation_journal_id=NULL WHERE journal_id=?1", []any{fixture.decision}, journalChanges},
		{"journal_operations.journal_id", "UPDATE journal_operations SET journal_id=journal_id+1000000 WHERE journal_id=?1", []any{fixture.anchor}, journalChanges},
		{"journal_operations.operation_id", "UPDATE journal_operations SET operation_id='renamed' WHERE journal_id=?1", []any{fixture.anchor}, journalChanges},
		{"journal_operations.authority_journal_id", "UPDATE journal_operations SET authority_journal_id=NULL WHERE journal_id=?1", []any{fixture.anchor}, journalChanges},
		{"journal_operations.command_digest", "UPDATE journal_operations SET command_digest=x'00' WHERE journal_id=?1", []any{fixture.anchor}, journalChanges},
		{"journal_operations.mutation_digest", "UPDATE journal_operations SET mutation_digest=x'00' WHERE journal_id=?1", []any{fixture.anchor}, journalChanges},
		{"journal_operations.mutation_encoding_version", "UPDATE journal_operations SET mutation_encoding_version=mutation_encoding_version||'x' WHERE journal_id=?1", []any{fixture.anchor}, journalChanges},
		{"journal_operations.canonical_mutation", "UPDATE journal_operations SET canonical_mutation=CAST(canonical_mutation||x'00' AS BLOB) WHERE journal_id=?1", []any{fixture.anchor}, journalChanges},
		{"journal_task_events.event_kind", "UPDATE journal_task_events SET event_kind=event_kind||'x' WHERE journal_id=(SELECT MIN(journal_id) FROM journal_task_events)", nil, journalChanges},
		{"journal_task_events.payload", "UPDATE journal_task_events SET payload='{\"changed\":true}' WHERE journal_id=(SELECT MIN(journal_id) FROM journal_task_events)", nil, journalChanges},
		{"journal_task_events.task_id", "UPDATE journal_task_events SET task_id=task_id||'x' WHERE journal_id=(SELECT MIN(journal_id) FROM journal_task_events)", nil, journalChanges},
		{"journal_decisions.decision_kind", "UPDATE journal_decisions SET decision_kind=decision_kind||'x' WHERE journal_id=?1", []any{fixture.decision}, journalChanges},
		{"journal_decisions.payload", "UPDATE journal_decisions SET payload='{\"changed\":true}' WHERE journal_id=?1", []any{fixture.decision}, journalChanges},
		{"journal_decisions.task_id", "UPDATE journal_decisions SET task_id=NULL WHERE journal_id=?1", []any{fixture.decision}, journalChanges},
		{"journal_evidence.evidence_kind", "UPDATE journal_evidence SET evidence_kind=evidence_kind||'x' WHERE journal_id=?1", []any{fixture.evidence}, journalChanges},
		{"journal_evidence.content_digest", "UPDATE journal_evidence SET content_digest=x'09' WHERE journal_id=?1", []any{fixture.evidence}, journalChanges},
		{"journal_evidence.payload", "UPDATE journal_evidence SET payload='{\"changed\":true}' WHERE journal_id=?1", []any{fixture.evidence}, journalChanges},
		{"journal_operation_result_slots.slot", "UPDATE journal_operation_result_slots SET result_slot_id=result_slot_id||'x' WHERE journal_id=?1 AND result_slot_id='decision'", []any{fixture.anchor}, journalChanges},
		{"journal_operation_result_slots.produced", "UPDATE journal_operation_result_slots SET produced_journal_id=produced_journal_id+1000000 WHERE journal_id=?1 AND result_slot_id='decision'", []any{fixture.anchor}, journalChanges},
		{"journal_authorities.operation_authority_id", "UPDATE journal_authorities SET operation_authority_id=operation_authority_id||'x' WHERE journal_id=?1", []any{int64(fixture.boot)}, journalChanges},
		{"journal_authorities.authority_kind_id", "UPDATE journal_authorities SET authority_kind_id=authority_kind_id+1 WHERE journal_id=?1", []any{int64(fixture.boot)}, journalChanges},
		{"journal_authority_bootstraps.label", "UPDATE journal_authority_bootstraps SET label=label||'x' WHERE journal_id=?1", []any{int64(fixture.boot)}, journalChanges},
		{"decision context parent", "UPDATE journal_decision_contexts SET decision_journal_id=decision_journal_id+1000000 WHERE decision_journal_id=?1", []any{fixture.decision}, contextsChanges},
		{"decision context kind", "UPDATE journal_decision_contexts SET context_kind=context_kind||'x' WHERE decision_journal_id=?1", []any{fixture.decision}, contextsChanges},
		{"decision context identity", "UPDATE journal_decision_contexts SET context_identity=context_identity||'x' WHERE decision_journal_id=?1", []any{fixture.decision}, contextsChanges},
		{"decision context delete", "DELETE FROM journal_decision_contexts WHERE decision_journal_id=?1", []any{fixture.decision}, contextsChanges},
		{"decision context insert", "INSERT INTO journal_decision_contexts (decision_journal_id,context_kind,context_identity) VALUES (?1,'zz','zz')", []any{fixture.decision}, contextsChanges},
		{"evidence context parent", "UPDATE journal_evidence_contexts SET evidence_journal_id=evidence_journal_id+1000000 WHERE evidence_journal_id=?1", []any{fixture.evidence}, contextsChanges},
		{"evidence context kind", "UPDATE journal_evidence_contexts SET context_kind=context_kind||'x' WHERE evidence_journal_id=?1", []any{fixture.evidence}, contextsChanges},
		{"evidence context identity", "UPDATE journal_evidence_contexts SET context_identity=context_identity||'x' WHERE evidence_journal_id=?1", []any{fixture.evidence}, contextsChanges},
		{"evidence context delete", "DELETE FROM journal_evidence_contexts WHERE evidence_journal_id=?1", []any{fixture.evidence}, contextsChanges},
		{"evidence context insert", "INSERT INTO journal_evidence_contexts (evidence_journal_id,context_kind,context_identity) VALUES (?1,'zz','zz')", []any{fixture.evidence}, contextsChanges},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, statement := range []string{"PRAGMA foreign_keys=OFF", "PRAGMA ignore_check_constraints=ON", "SAVEPOINT prefix_mutation"} {
				if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			defer func() {
				for _, statement := range []string{"ROLLBACK TO prefix_mutation", "RELEASE prefix_mutation", "PRAGMA ignore_check_constraints=OFF", "PRAGMA foreign_keys=ON"} {
					if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
						t.Fatalf("%s: %v", statement, err)
					}
				}
			}()
			result, err := scope.conn.ExecContext(scope.ctx, test.sql, test.args...)
			if err != nil {
				t.Fatalf("mutate: %v", err)
			}
			if n, _ := result.RowsAffected(); n == 0 {
				t.Fatal("the mutation matched no row")
			}
			got, err := scope.computeVerifiedPrefixDigests(through)
			if err != nil {
				t.Fatalf("digests after the mutation: %v", err)
			}
			journalMoved, contextsMoved := got.journal != base.journal, got.contexts != base.contexts
			if journalMoved != (test.want == journalChanges) || contextsMoved != (test.want == contextsChanges) {
				t.Fatalf("after %q: journal digest moved=%v contexts digest moved=%v, want only the %d family", test.name, journalMoved, contextsMoved, test.want)
			}
		})
	}
}

// TestVerifiedPrefixTailValidatesOnlyTheTail proves an append-only tail decodes
// only the tail operations, and that the result of the shortened walk equals the
// full walk.
func TestVerifiedPrefixTailValidatesOnlyTheTail(t *testing.T) {
	fixture := buildStaleTailFixture(t)
	scope := takePoolScope(t, fixture.db)
	defer scope.release()

	trust := scope.resolveVerifiedPrefix()
	if !trust.journal || !trust.contexts || trust.through != fixture.prefixTop {
		t.Fatalf("trust = %+v, want both families trusted through %d (the digest still matches)", trust, fixture.prefixTop)
	}

	var totalOps int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM journal_operations").Scan(&totalOps); err != nil {
		t.Fatal(err)
	}

	release := scope.trustVerifiedPrefix()
	if err := scope.verifyFactContextIntegrity(); err != nil {
		t.Fatalf("tail fact-context validation: %v", err)
	}
	if err := scope.validateCanonicalOperations(); err != nil {
		t.Fatalf("tail canonical validation: %v", err)
	}
	release()
	if scope.canonicalOperationsVisited != 1 || scope.factContextOperationsVisited != 1 {
		t.Fatalf("trusted walk visited %d canonical and %d fact-context operations, want only the one tail operation", scope.canonicalOperationsVisited, scope.factContextOperationsVisited)
	}

	scope.canonicalOperationsVisited, scope.factContextOperationsVisited = 0, 0
	if err := scope.verifyFactContextIntegrity(); err != nil {
		t.Fatalf("full fact-context validation: %v", err)
	}
	if err := scope.validateCanonicalOperations(); err != nil {
		t.Fatalf("full canonical validation: %v", err)
	}
	if scope.canonicalOperationsVisited != totalOps || scope.factContextOperationsVisited < 3 {
		t.Fatalf("an unarmed scope visited %d canonical (want %d) and %d fact-context operations, want the whole store", scope.canonicalOperationsVisited, totalOps, scope.factContextOperationsVisited)
	}
}

// TestVerifiedPrefixOpenRecordsTheNewTail runs the production open over a stale
// ledger with an appended tail: it must accept the store, advance the ledger and
// the prefix to the new tail, and record digests equal to a fresh computation.
func TestVerifiedPrefixOpenRecordsTheNewTail(t *testing.T) {
	fixture := buildStaleTailFixture(t)
	if err := fixture.db.Close(); err != nil {
		t.Fatal(err)
	}
	_, countBefore := readLedgerGeneration(t, fixture.path)
	if err := reopenFileBacked(t, fixture.path); err != nil {
		t.Fatalf("open the stale ledger with an appended tail: %v", err)
	}
	if _, countAfter := readLedgerGeneration(t, fixture.path); countAfter != countBefore+1 {
		t.Fatalf("verified_count = %d, want %d: the tail should take exactly one whole-store verification", countAfter, countBefore+1)
	}
	through, recorded, ok := readPrefixRow(t, fixture.path)
	top := maxJournalID(t, fixture.path)
	if !ok || through != top || ledgerJournalID(t, fixture.path) != top {
		t.Fatalf("prefix through %d (present=%v), ledger %d, want both at the tail %d", through, ok, ledgerJournalID(t, fixture.path), top)
	}
	db, err := Open(fixture.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	scope := takePoolScope(t, db)
	defer scope.release()
	fresh, err := scope.computeVerifiedPrefixDigests(top)
	if err != nil || fresh != recorded {
		t.Fatalf("recorded digests %+v, fresh %+v (%v)", recorded, fresh, err)
	}
	if _, countAgain := readLedgerGeneration(t, fixture.path); countAgain != countBefore+1 {
		t.Fatalf("a further open advanced verified_count to %d; the ledger is not current", countAgain)
	}
}

// TestVerifiedPrefixOldRowMutationRefusesTheOpen mutates one old row of each
// covered family out of band. The digest must mismatch, the open must run the
// full validation, and corruption must be refused without advancing the ledger.
func TestVerifiedPrefixOldRowMutationRefusesTheOpen(t *testing.T) {
	for _, test := range []struct {
		name      string
		statement string
	}{
		{"journal_operations", "UPDATE journal_operations SET canonical_mutation=CAST(canonical_mutation||x'00' AS BLOB) WHERE journal_id=?1"},
		{"journal", "UPDATE journal SET recorded_at=recorded_at+1 WHERE journal_id=?1"},
		{"context", "DELETE FROM journal_decision_contexts WHERE decision_journal_id=?1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := buildPrefixFixture(t)
			_, countBefore := readLedgerGeneration(t, fixture.path)
			raw := openVerifiedTablesTestDB(t, fixture.path)
			arg := fixture.anchor
			switch test.name {
			case "journal":
				arg = fixture.decision
			case "context":
				arg = fixture.decision
			}
			if _, err := raw.Exec(test.statement, arg); err != nil {
				t.Fatalf("mutate an old %s row: %v", test.name, err)
			}

			// The in-process view: the digest names the damaged family.
			db, err := Open(fixture.path, nil)
			if err == nil {
				_ = db.Close()
				t.Fatalf("opening a store with a mutated old %s row succeeded", test.name)
			}
			if _, countAfter := readLedgerGeneration(t, fixture.path); countAfter != countBefore {
				t.Fatalf("a refused open advanced verified_count %d -> %d", countBefore, countAfter)
			}
			if through, _, ok := readPrefixRow(t, fixture.path); !ok || through != ledgerJournalID(t, fixture.path) {
				t.Fatalf("a refused open disturbed the prefix row (through %d, present %v)", through, ok)
			}
		})
	}
}

// TestVerifiedPrefixOldRowMutationIsNotTrusted checks the trust decision itself:
// a mutated old row makes the matching family untrusted.
func TestVerifiedPrefixOldRowMutationIsNotTrusted(t *testing.T) {
	for _, test := range []struct {
		name         string
		statement    string
		arg          func(prefixFixture) int64
		wantJournal  bool
		wantContexts bool
	}{
		{"operation", "UPDATE journal_operations SET mutation_digest=x'00' WHERE journal_id=?1", func(f prefixFixture) int64 { return f.anchor }, false, true},
		{"journal", "UPDATE journal SET recorded_at=recorded_at+1 WHERE journal_id=?1", func(f prefixFixture) int64 { return f.decision }, false, true},
		{"context", "UPDATE journal_evidence_contexts SET context_identity=context_identity||'x' WHERE evidence_journal_id=?1", func(f prefixFixture) int64 { return f.evidence }, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := buildPrefixFixture(t)
			db, err := Open(fixture.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			scope := takePoolScope(t, db)
			defer scope.release()
			if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=OFF"); err != nil {
				t.Fatal(err)
			}
			if _, err := scope.conn.ExecContext(scope.ctx, test.statement, test.arg(fixture)); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			trust := scope.resolveVerifiedPrefix()
			if trust.journal != test.wantJournal || trust.contexts != test.wantContexts {
				t.Fatalf("trust = %+v, want journal=%v contexts=%v", trust, test.wantJournal, test.wantContexts)
			}
			// The untrusted family runs the full walk, which refuses the damage.
			release := scope.trustVerifiedPrefix()
			defer release()
			errOps := scope.validateCanonicalOperations()
			errContexts := scope.verifyFactContextIntegrity()
			if errOps == nil && errContexts == nil {
				t.Fatal("neither validator refused the mutated old row")
			}
		})
	}
}

// TestVerifiedPrefixMissingTornOrForgedRowRunsFullDecode covers every unusable
// cache row: the trust is the zero value, so the walk covers every operation.
func TestVerifiedPrefixMissingTornOrForgedRowRunsFullDecode(t *testing.T) {
	for _, test := range []struct {
		name      string
		statement string
	}{
		{"missing", "DELETE FROM provenance_verified_prefix"},
		{"empty journal digest", "UPDATE provenance_verified_prefix SET journal_digest=''"},
		{"empty contexts digest", "UPDATE provenance_verified_prefix SET contexts_digest=''"},
		{"forged older through", "UPDATE provenance_verified_prefix SET through_journal_id=through_journal_id-1"},
		{"forged newer through", "UPDATE provenance_verified_prefix SET through_journal_id=through_journal_id+1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := buildPrefixFixture(t)
			db, err := Open(fixture.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			scope := takePoolScope(t, db)
			defer scope.release()
			if _, err := scope.conn.ExecContext(scope.ctx, test.statement); err != nil {
				t.Fatalf("damage the prefix row: %v", err)
			}
			if trust := scope.resolveVerifiedPrefix(); trust != (verifiedPrefixTrust{}) {
				t.Fatalf("trust = %+v, want none", trust)
			}
			release := scope.trustVerifiedPrefix()
			if err := scope.validateCanonicalOperations(); err != nil {
				t.Fatalf("full validation: %v", err)
			}
			release()
			var total int
			if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM journal_operations").Scan(&total); err != nil {
				t.Fatal(err)
			}
			if scope.canonicalOperationsVisited != total {
				t.Fatalf("visited %d operations, want all %d", scope.canonicalOperationsVisited, total)
			}
		})
	}
}

// TestVerifiedPrefixMissingRowOnStaleLedgerIsRewrittenByTheOpen proves a stale
// ledger with no cache takes the full decode once and records the cache.
func TestVerifiedPrefixMissingRowOnStaleLedgerIsRewrittenByTheOpen(t *testing.T) {
	fixture := buildPrefixFixture(t)
	raw := openVerifiedTablesTestDB(t, fixture.path)
	if _, err := raw.Exec("DELETE FROM " + verifiedPrefixTable); err != nil {
		t.Fatal(err)
	}
	bumpStoreGeneration(t, fixture.path)
	_, countBefore := readLedgerGeneration(t, fixture.path)
	if err := reopenFileBacked(t, fixture.path); err != nil {
		t.Fatalf("open a stale ledger without a prefix: %v", err)
	}
	if _, countAfter := readLedgerGeneration(t, fixture.path); countAfter != countBefore+1 {
		t.Fatalf("verified_count = %d, want %d", countAfter, countBefore+1)
	}
	if through, _, ok := readPrefixRow(t, fixture.path); !ok || through != ledgerJournalID(t, fixture.path) {
		t.Fatalf("the full verification did not record the prefix (through %d, present %v)", through, ok)
	}
}

// TestVerifiedPrefixPartialPathCarriesThePrefix proves an agent registration
// takes the table-local path, decodes nothing, and leaves the prefix row alone.
func TestVerifiedPrefixPartialPathCarriesThePrefix(t *testing.T) {
	fixture := buildPrefixFixture(t)
	throughBefore, digestsBefore, _ := readPrefixRow(t, fixture.path)
	_, countBefore := readLedgerGeneration(t, fixture.path)

	db, err := Open(fixture.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RegisterSoftwareAgent("verified", "prefix-partial-agent", "0", "verified_prefix_test"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reopenFileBacked(t, fixture.path); err != nil {
		t.Fatalf("open after the agent change: %v", err)
	}
	if _, countAfter := readLedgerGeneration(t, fixture.path); countAfter != countBefore {
		t.Fatalf("verified_count %d -> %d; the agent change took the whole-store path", countBefore, countAfter)
	}
	throughAfter, digestsAfter, ok := readPrefixRow(t, fixture.path)
	if !ok || throughAfter != throughBefore || digestsAfter != digestsBefore {
		t.Fatalf("the partial path changed the prefix: %d %+v -> %d %+v (present %v)", throughBefore, digestsBefore, throughAfter, digestsAfter, ok)
	}
}

// TestVerifiedPrefixAdoptionSeedsWithoutADecode simulates a current v0.10.7
// store, which has no prefix table. Adoption must create and seed it, and must
// not take the whole-store path.
func TestVerifiedPrefixAdoptionSeedsWithoutADecode(t *testing.T) {
	fixture := buildPrefixFixture(t)
	generation, countBefore := readLedgerGeneration(t, fixture.path)
	if err := simulateV3Ledger(t, fixture.path, generation); err != nil {
		t.Fatalf("simulate the v0.10.7 ledger: %v", err)
	}
	if err := reopenFileBacked(t, fixture.path); err != nil {
		t.Fatalf("open the current v0.10.7 store: %v", err)
	}
	if _, countAfter := readLedgerGeneration(t, fixture.path); countAfter != countBefore {
		t.Fatalf("verified_count %d -> %d; adoption ran the whole-store verification", countBefore, countAfter)
	}
	through, recorded, ok := readPrefixRow(t, fixture.path)
	if !ok || through != ledgerJournalID(t, fixture.path) {
		t.Fatalf("adoption seeded through %d (present %v), want the ledger journal id %d", through, ok, ledgerJournalID(t, fixture.path))
	}
	db, err := Open(fixture.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	scope := takePoolScope(t, db)
	defer scope.release()
	fresh, err := scope.computeVerifiedPrefixDigests(through)
	if err != nil || fresh != recorded {
		t.Fatalf("seeded digests %+v, fresh %+v (%v)", recorded, fresh, err)
	}
}

// TestVerifiedPrefixConcurrentWriterRefusesTheLedgerWrite pins the
// compare-and-set: a store that moves after the verification is never recorded,
// so the prefix row is not rewritten either.
func TestVerifiedPrefixConcurrentWriterRefusesTheLedgerWrite(t *testing.T) {
	fixture := buildPrefixFixture(t)
	db, err := Open(fixture.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	scope := takePoolScope(t, db)
	defer scope.release()
	before, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		t.Fatal(err)
	}
	throughBefore, digestsBefore, _ := readPrefixRow(t, fixture.path)
	markLedgerStale(t, scope)
	if scope.persistVerifiedLedgerIfUnchanged(before) {
		t.Fatal("the ledger was recorded for a store that moved during the verification")
	}
	throughAfter, digestsAfter, _ := readPrefixRow(t, fixture.path)
	if throughAfter != throughBefore || digestsAfter != digestsBefore {
		t.Fatal("a refused ledger write rewrote the prefix row")
	}
}

// TestVerifiedPrefixDifferentialMatchesFullDecode compares the shortened walk
// with the full walk over tails that are clean and tails that are damaged: the
// outcome, error text, and first-error anchor must be identical.
func TestVerifiedPrefixDifferentialMatchesFullDecode(t *testing.T) {
	for _, test := range []struct {
		name      string
		statement string
		args      func(staleTailFixture) []any
	}{
		{"clean tail", "", nil},
		{"tail context removed", "DELETE FROM journal_decision_contexts WHERE decision_journal_id>?1", func(f staleTailFixture) []any { return []any{f.prefixTop} }},
		{"tail context added", "INSERT INTO journal_evidence_contexts (evidence_journal_id,context_kind,context_identity) SELECT journal_id,'zz','zz' FROM journal_evidence WHERE journal_id>?1", func(f staleTailFixture) []any { return []any{f.prefixTop} }},
		{"tail digest forged", "UPDATE journal_operations SET mutation_digest=x'00' WHERE journal_id=?1", func(f staleTailFixture) []any { return []any{f.tail} }},
		{"tail bytes forged", "UPDATE journal_operations SET canonical_mutation=x'00' WHERE journal_id=?1", func(f staleTailFixture) []any { return []any{f.tail} }},
		{"tail row claimed by a prefix operation", "UPDATE journal SET produced_by_operation_journal_id=?2 WHERE journal_id=(SELECT MAX(journal_id) FROM journal)", func(f staleTailFixture) []any { return []any{f.tail, f.anchor} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := buildStaleTailFixture(t)
			scope := takePoolScope(t, fixture.db)
			defer scope.release()
			if test.statement != "" {
				for _, statement := range []string{"PRAGMA foreign_keys=OFF", "PRAGMA ignore_check_constraints=ON"} {
					if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
						t.Fatal(err)
					}
				}
				var args []any
				if test.args != nil {
					args = test.args(fixture)
				}
				if strings.Contains(test.statement, "?2") && len(args) < 2 {
					t.Fatal("statement needs two arguments")
				}
				if _, err := scope.conn.ExecContext(scope.ctx, test.statement, args...); err != nil {
					t.Fatalf("damage the tail: %v", err)
				}
			}
			trust := scope.resolveVerifiedPrefix()
			if !trust.journal || !trust.contexts {
				t.Fatalf("trust = %+v; the prefix is untouched so it must be trusted", trust)
			}

			run := func(armed bool) (string, string) {
				if armed {
					release := scope.trustVerifiedPrefix()
					defer release()
				}
				var integrity, replay string
				if err := scope.verifyIntegrity(); err != nil {
					integrity = err.Error()
				}
				if _, err := scope.replayProjections(); err != nil {
					replay = err.Error()
				}
				return integrity, replay
			}
			fullIntegrity, fullReplay := run(false)
			shortIntegrity, shortReplay := run(true)
			if fullIntegrity != shortIntegrity || fullReplay != shortReplay {
				t.Fatalf("the shortened walk diverged from the full walk:\nfull  integrity=%q replay=%q\nshort integrity=%q replay=%q", fullIntegrity, fullReplay, shortIntegrity, shortReplay)
			}
			if test.statement == "" && (fullIntegrity != "" || fullReplay != "") {
				t.Fatalf("a clean tail was refused: integrity=%q replay=%q", fullIntegrity, fullReplay)
			}
			if test.statement != "" && fullIntegrity == "" && fullReplay == "" {
				t.Fatalf("the damaged tail %q was not refused by the full walk", test.name)
			}
		})
	}
}

// TestVerifiedPrefixTailRowReachingThePrefixWidensToFull proves a tail row
// claimed by a prefix operation is not skipped.
func TestVerifiedPrefixTailRowReachingThePrefixWidensToFull(t *testing.T) {
	fixture := buildStaleTailFixture(t)
	scope := takePoolScope(t, fixture.db)
	defer scope.release()
	for _, statement := range []string{"PRAGMA foreign_keys=OFF", "PRAGMA ignore_check_constraints=ON"} {
		if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE journal SET produced_by_operation_journal_id=?1 WHERE journal_id=(SELECT MAX(journal_id) FROM journal)", fixture.anchor); err != nil {
		t.Fatal(err)
	}
	release := scope.trustVerifiedPrefix()
	defer release()
	if err := scope.validateCanonicalOperations(); err == nil {
		t.Fatal("a tail row claimed by a prefix operation was accepted")
	}
	if scope.canonicalOperationsVisited <= 1 {
		t.Fatalf("visited %d operations; a tail reaching the prefix must widen to the full walk", scope.canonicalOperationsVisited)
	}
}

// TestVerifiedPrefixLedgerObjectsAreLedgerOwned pins the wiring: the prefix
// table is ledger machinery, outside the data contract and the shape digest.
func TestVerifiedPrefixLedgerObjectsAreLedgerOwned(t *testing.T) {
	var listed bool
	for _, object := range verifiedLedgerObjects {
		if object.kind == "table" && object.name == verifiedPrefixTable {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("%s is not in verifiedLedgerObjects", verifiedPrefixTable)
	}
	for _, object := range expectedActivationObjects {
		if object.name == verifiedPrefixTable {
			t.Fatalf("%s is in expectedActivationObjects; it would change the ledger shape", verifiedPrefixTable)
		}
	}
	if !strings.Contains(verifiedPrefixDDL, "CHECK (id = 1)") {
		t.Fatal("the prefix table is not a singleton")
	}
}
