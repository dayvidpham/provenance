package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// prefixFixture is a file-backed store whose ledger and verified prefix are
// current: genesis, one task, a decision+evidence operation with contexts, a
// second task whose creation carries a context, and one assignment start.
type prefixFixture struct {
	path         string
	actor        journal.ActorID
	task         journal.TaskID
	boot         journal.JournalID
	anchor       int64
	decision     int64
	evidence     int64
	assignment   int64
	assignmentID string
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
	actorContext, err := journal.ActorContext(actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Apply(journal.OperationInput{
		OperationID: "prefix-contexted-task", ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("prefix-contexted-task"),
		Effects: []journal.Effect{{
			Sort: journal.EffectTaskCreate, TaskID: ptypes.TaskID{Namespace: "provenance-test", UUID: uuid.New()}, Title: "contexted",
			Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped,
			Contexts: []journal.EventContext{actorContext},
		}},
	}); err != nil {
		t.Fatalf("create the contexted task: %v", err)
	}
	const assignmentID = "prefix-assignment"
	started, err := db.Apply(journal.OperationInput{
		OperationID: "prefix-assignment-start", ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("prefix-assignment-start"),
		Effects: []journal.Effect{{Sort: journal.EffectAssignmentStart, ResultSlot: "start", AssignmentID: assignmentID, TaskID: task, SlotID: journal.SlotOwnerResponsibility, Occupant: actor}},
	})
	if err != nil {
		t.Fatalf("start the assignment: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the prefix fixture: %v", err)
	}
	// The direct agent insert above made the ledger stale; the reopen is the full
	// path that records the ledger and the prefix for the state the store is in.
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("reopen to record the prefix: %v", err)
	}
	fixture := prefixFixture{
		path: path, actor: actor, task: task, boot: boot, anchor: anchor, decision: decision, evidence: evidence,
		assignment: int64(factContextResultSlot(t, started, "start")), assignmentID: assignmentID,
	}
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

// mustReadPrefixRow is readPrefixRow for a row the test requires to exist.
func mustReadPrefixRow(t *testing.T, path string) (int64, verifiedPrefixDigests) {
	t.Helper()
	through, digests, ok := readPrefixRow(t, path)
	if !ok {
		t.Fatal("the verified prefix row is missing")
	}
	return through, digests
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

func countRows(t *testing.T, scope *connScope, query string) int {
	t.Helper()
	var n int
	if err := scope.conn.QueryRowContext(scope.ctx, query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
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

// relaxConstraints lets a test write rows the schema would refuse, on the
// scope's own connection.
func relaxConstraints(t *testing.T, scope *connScope) {
	t.Helper()
	for _, statement := range []string{"PRAGMA foreign_keys=OFF", "PRAGMA ignore_check_constraints=ON"} {
		if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// rawExecPinned runs statements on one pinned connection with foreign keys off,
// outside any open DB, the way an out-of-band editor would.
func rawExecPinned(t *testing.T, path string, statements []string, args ...[]any) {
	t.Helper()
	raw := openVerifiedTablesTestDB(t, path)
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, pragma := range []string{"PRAGMA foreign_keys=OFF", "PRAGMA ignore_check_constraints=ON"} {
		if _, err := conn.ExecContext(context.Background(), pragma); err != nil {
			t.Fatal(err)
		}
	}
	for i, statement := range statements {
		var a []any
		if i < len(args) {
			a = args[i]
		}
		if _, err := conn.ExecContext(context.Background(), statement, a...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// staleTailFixture is a store whose ledger is stale and whose journal has two or
// more operations above the verified prefix.
type staleTailFixture struct {
	prefixFixture
	db        *DB
	prefixTop int64
	tails     []int64
	tail      int64 // the first tail anchor
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
	var tails []int64
	for _, id := range []string{"prefix-tail-1", "prefix-tail-2"} {
		anchor, _, _ := applyPrefixFacts(t, db, fixture.actor, fixture.task, fixture.boot, id)
		tails = append(tails, anchor)
	}
	// The boundary case: the tail begins exactly at through+1.
	if tails[0] != top+1 {
		t.Fatalf("first tail anchor %d, want exactly through+1 = %d", tails[0], top+1)
	}
	return staleTailFixture{prefixFixture: fixture, db: db, prefixTop: top, tails: tails, tail: tails[0]}
}

// ---------------------------------------------------------------------------
// Digest scope
// ---------------------------------------------------------------------------

// prefixMutation is one out-of-band edit and the digest family it must move.
// table and column name the digested input it exercises; column "(row)" is a
// whole-row insert, delete, or move.
type prefixMutation struct {
	table, column string
	suffix        string
	statement     string
	args          []any
	contexts      bool // the contexts digest must move; otherwise the journal digest
}

func (m prefixMutation) name() string { return m.table + "." + m.column + m.suffix }

func prefixMutations(f prefixFixture) []prefixMutation {
	const firstEvent = "(SELECT MIN(journal_id) FROM journal_task_events)"
	const firstEventContext = "(SELECT MIN(event_journal_id) FROM journal_task_event_contexts)"
	decision, evidence, anchor, boot := []any{f.decision}, []any{f.evidence}, []any{f.anchor}, []any{int64(f.boot)}
	assignmentID := []any{f.assignmentID}
	return []prefixMutation{
		{"journal", "journal_id", "", "UPDATE journal SET journal_id=journal_id+1000000 WHERE journal_id=?1", decision, false},
		{"journal", "kind_id", "", "UPDATE journal SET kind_id=kind_id+1 WHERE journal_id=?1", decision, false},
		{"journal", "actor_id", "", "UPDATE journal SET actor_id='someone-else' WHERE journal_id=?1", anchor, false},
		{"journal", "actor_id", " null", "UPDATE journal SET actor_id=NULL WHERE journal_id=?1", anchor, false},
		{"journal", "recorded_at", "", "UPDATE journal SET recorded_at=recorded_at+1 WHERE journal_id=?1", decision, false},
		{"journal", "produced_by_operation_journal_id", "", "UPDATE journal SET produced_by_operation_journal_id=produced_by_operation_journal_id+1 WHERE journal_id=?1", decision, false},
		{"journal", "produced_by_operation_journal_id", " null", "UPDATE journal SET produced_by_operation_journal_id=NULL WHERE journal_id=?1", decision, false},

		{"journal_operations", "journal_id", "", "UPDATE journal_operations SET journal_id=journal_id+1000000 WHERE journal_id=?1", anchor, false},
		{"journal_operations", "operation_id", "", "UPDATE journal_operations SET operation_id='renamed' WHERE journal_id=?1", anchor, false},
		{"journal_operations", "authority_journal_id", "", "UPDATE journal_operations SET authority_journal_id=NULL WHERE journal_id=?1", anchor, false},
		{"journal_operations", "command_digest", "", "UPDATE journal_operations SET command_digest=x'00' WHERE journal_id=?1", anchor, false},
		{"journal_operations", "mutation_digest", "", "UPDATE journal_operations SET mutation_digest=x'00' WHERE journal_id=?1", anchor, false},
		{"journal_operations", "mutation_encoding_version", "", "UPDATE journal_operations SET mutation_encoding_version=mutation_encoding_version||'x' WHERE journal_id=?1", anchor, false},
		{"journal_operations", "canonical_mutation", "", "UPDATE journal_operations SET canonical_mutation=CAST(canonical_mutation||x'00' AS BLOB) WHERE journal_id=?1", anchor, false},

		{"journal_task_events", "journal_id", "", "UPDATE journal_task_events SET journal_id=journal_id+1000000 WHERE journal_id=" + firstEvent, nil, false},
		{"journal_task_events", "task_id", "", "UPDATE journal_task_events SET task_id=task_id||'x' WHERE journal_id=" + firstEvent, nil, false},
		{"journal_task_events", "event_kind", "", "UPDATE journal_task_events SET event_kind=event_kind||'x' WHERE journal_id=" + firstEvent, nil, false},
		{"journal_task_events", "payload", "", "UPDATE journal_task_events SET payload='{\"changed\":true}' WHERE journal_id=" + firstEvent, nil, false},

		{"journal_task_event_contexts", "event_journal_id", "", "UPDATE journal_task_event_contexts SET event_journal_id=event_journal_id+1000000 WHERE event_journal_id=" + firstEventContext, nil, false},
		{"journal_task_event_contexts", "context_kind", "", "UPDATE journal_task_event_contexts SET context_kind=context_kind||'x' WHERE event_journal_id=" + firstEventContext, nil, false},
		{"journal_task_event_contexts", "context_identity", "", "UPDATE journal_task_event_contexts SET context_identity=context_identity||'x' WHERE event_journal_id=" + firstEventContext, nil, false},
		{"journal_task_event_contexts", "attached_by_journal_id", "", "UPDATE journal_task_event_contexts SET attached_by_journal_id=attached_by_journal_id+1 WHERE event_journal_id=" + firstEventContext, nil, false},

		{"journal_decisions", "journal_id", "", "UPDATE journal_decisions SET journal_id=journal_id+1000000 WHERE journal_id=?1", decision, false},
		{"journal_decisions", "decision_kind", "", "UPDATE journal_decisions SET decision_kind=decision_kind||'x' WHERE journal_id=?1", decision, false},
		{"journal_decisions", "task_id", "", "UPDATE journal_decisions SET task_id=NULL WHERE journal_id=?1", decision, false},
		{"journal_decisions", "payload", "", "UPDATE journal_decisions SET payload='{\"changed\":true}' WHERE journal_id=?1", decision, false},

		{"journal_evidence", "journal_id", "", "UPDATE journal_evidence SET journal_id=journal_id+1000000 WHERE journal_id=?1", evidence, false},
		{"journal_evidence", "evidence_kind", "", "UPDATE journal_evidence SET evidence_kind=evidence_kind||'x' WHERE journal_id=?1", evidence, false},
		{"journal_evidence", "task_id", "", "UPDATE journal_evidence SET task_id=NULL WHERE journal_id=?1", evidence, false},
		{"journal_evidence", "content_digest", "", "UPDATE journal_evidence SET content_digest=x'09' WHERE journal_id=?1", evidence, false},
		{"journal_evidence", "payload", "", "UPDATE journal_evidence SET payload='{\"changed\":true}' WHERE journal_id=?1", evidence, false},

		{"journal_operation_result_slots", "journal_id", "", "UPDATE journal_operation_result_slots SET journal_id=journal_id+1000000 WHERE journal_id=?1 AND result_slot_id='decision'", anchor, false},
		{"journal_operation_result_slots", "result_slot_id", "", "UPDATE journal_operation_result_slots SET result_slot_id=result_slot_id||'x' WHERE journal_id=?1 AND result_slot_id='decision'", anchor, false},
		{"journal_operation_result_slots", "produced_journal_id", "", "UPDATE journal_operation_result_slots SET produced_journal_id=produced_journal_id+1000000 WHERE journal_id=?1 AND result_slot_id='decision'", anchor, false},

		{"journal_authorities", "journal_id", "", "UPDATE journal_authorities SET journal_id=journal_id+1000000 WHERE journal_id=?1", boot, false},
		{"journal_authorities", "authority_kind_id", "", "UPDATE journal_authorities SET authority_kind_id=authority_kind_id+1 WHERE journal_id=?1", boot, false},
		{"journal_authorities", "operation_authority_id", "", "UPDATE journal_authorities SET operation_authority_id=operation_authority_id||'x' WHERE journal_id=?1", boot, false},

		{"journal_authority_bootstraps", "journal_id", "", "UPDATE journal_authority_bootstraps SET journal_id=journal_id+1000000 WHERE journal_id=?1", boot, false},
		{"journal_authority_bootstraps", "label", "", "UPDATE journal_authority_bootstraps SET label=label||'x' WHERE journal_id=?1", boot, false},

		{"journal_authority_assignment_transitions", "journal_id", "", "UPDATE journal_authority_assignment_transitions SET journal_id=journal_id+1000000 WHERE assignment_id=?1", assignmentID, false},
		{"journal_authority_assignment_transitions", "assignment_id", "", "UPDATE journal_authority_assignment_transitions SET assignment_id=assignment_id||'x' WHERE assignment_id=?1", assignmentID, false},
		{"journal_authority_assignment_transitions", "transition_id", "", "UPDATE journal_authority_assignment_transitions SET transition_id=transition_id+1 WHERE assignment_id=?1", assignmentID, false},

		{"journal_authority_assignment_episodes", "assignment_id", "", "UPDATE journal_authority_assignment_episodes SET assignment_id=assignment_id||'x' WHERE assignment_id=?1", assignmentID, false},
		{"journal_authority_assignment_episodes", "task_id", "", "UPDATE journal_authority_assignment_episodes SET task_id=task_id||'x' WHERE assignment_id=?1", assignmentID, false},
		{"journal_authority_assignment_episodes", "slot_id", "", "UPDATE journal_authority_assignment_episodes SET slot_id=slot_id+1 WHERE assignment_id=?1", assignmentID, false},
		{"journal_authority_assignment_episodes", "actor_id", "", "UPDATE journal_authority_assignment_episodes SET actor_id=actor_id||'x' WHERE assignment_id=?1", assignmentID, false},
		{"journal_authority_assignment_episodes", "predecessor_assignment_id", "", "UPDATE journal_authority_assignment_episodes SET predecessor_assignment_id='p' WHERE assignment_id=?1", assignmentID, false},
		{"journal_authority_assignment_episodes", "parent_assignment_id", "", "UPDATE journal_authority_assignment_episodes SET parent_assignment_id='p' WHERE assignment_id=?1", assignmentID, false},

		{"journal_decision_contexts", "decision_journal_id", "", "UPDATE journal_decision_contexts SET decision_journal_id=decision_journal_id+1000000 WHERE decision_journal_id=?1", decision, true},
		{"journal_decision_contexts", "context_kind", "", "UPDATE journal_decision_contexts SET context_kind=context_kind||'x' WHERE decision_journal_id=?1", decision, true},
		{"journal_decision_contexts", "context_identity", "", "UPDATE journal_decision_contexts SET context_identity=context_identity||'x' WHERE decision_journal_id=?1", decision, true},
		{"journal_decision_contexts", "(row)", " delete", "DELETE FROM journal_decision_contexts WHERE decision_journal_id=?1", decision, true},
		{"journal_decision_contexts", "(row)", " insert", "INSERT INTO journal_decision_contexts (decision_journal_id,context_kind,context_identity) VALUES (?1,'zz','zz')", decision, true},
		{"journal_evidence_contexts", "evidence_journal_id", "", "UPDATE journal_evidence_contexts SET evidence_journal_id=evidence_journal_id+1000000 WHERE evidence_journal_id=?1", evidence, true},
		{"journal_evidence_contexts", "context_kind", "", "UPDATE journal_evidence_contexts SET context_kind=context_kind||'x' WHERE evidence_journal_id=?1", evidence, true},
		{"journal_evidence_contexts", "context_identity", "", "UPDATE journal_evidence_contexts SET context_identity=context_identity||'x' WHERE evidence_journal_id=?1", evidence, true},
		{"journal_evidence_contexts", "(row)", " delete", "DELETE FROM journal_evidence_contexts WHERE evidence_journal_id=?1", evidence, true},
		{"journal_evidence_contexts", "(row)", " insert", "INSERT INTO journal_evidence_contexts (evidence_journal_id,context_kind,context_identity) VALUES (?1,'zz','zz')", evidence, true},
	}
}

// TestVerifiedPrefixDigestCoversEveryColumn mutates every digested column and
// requires exactly the matching digest to change.
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
	recordedThrough, recorded := mustReadPrefixRow(t, fixture.path)
	if recordedThrough != through || recorded != base {
		t.Fatalf("recorded prefix %d %+v differs from a fresh computation %d %+v", recordedThrough, recorded, through, base)
	}

	for _, test := range prefixMutations(fixture) {
		t.Run(test.name(), func(t *testing.T) {
			relaxConstraints(t, scope)
			if _, err := scope.conn.ExecContext(scope.ctx, "SAVEPOINT prefix_mutation"); err != nil {
				t.Fatal(err)
			}
			defer func() {
				for _, statement := range []string{"ROLLBACK TO prefix_mutation", "RELEASE prefix_mutation", "PRAGMA ignore_check_constraints=OFF", "PRAGMA foreign_keys=ON"} {
					if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
						t.Fatalf("%s: %v", statement, err)
					}
				}
			}()
			result, err := scope.conn.ExecContext(scope.ctx, test.statement, test.args...)
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
			if journalMoved == contextsMoved || contextsMoved != test.contexts {
				t.Fatalf("journal digest moved=%v contexts digest moved=%v, want only the %s family", journalMoved, contextsMoved, map[bool]string{false: "journal", true: "contexts"}[test.contexts])
			}
		})
	}
}

// selectedColumns parses the column names of a digest statement's select list.
func selectedColumns(query string) []string {
	list := query[len("SELECT "):strings.Index(query, " FROM ")]
	var columns []string
	for _, column := range strings.Split(list, ",") {
		if dot := strings.LastIndex(column, "."); dot >= 0 {
			column = column[dot+1:]
		}
		columns = append(columns, strings.TrimSpace(column))
	}
	return columns
}

// prefixUnhashedColumns lists a column of a hashed table the digest does not
// read, with the reason it is safe. It is empty today: every column of every
// hashed table is digested. A new column must be hashed or listed here.
var prefixUnhashedColumns = map[string]string{}

// prefixTableDisposition classifies every covered table. "digest" means the
// prefix digests hash it; anything else is the reason the skipped canonical
// validators do not read it, or the check that covers it instead.
var prefixTableDisposition = map[string]string{
	"statuses":                            "closed seed lookup; activationReferenceSeedsCurrent and the whole-store foreign-key check",
	"priorities":                          "closed seed lookup; activationReferenceSeedsCurrent",
	"task_types":                          "closed seed lookup; activationReferenceSeedsCurrent",
	"edge_kinds":                          "closed seed lookup; activationReferenceSeedsCurrent",
	"agent_kinds":                         "closed seed lookup; activationReferenceSeedsCurrent",
	"providers":                           "closed seed lookup; activationReferenceSeedsCurrent",
	"roles":                               "closed seed lookup; activationReferenceSeedsCurrent",
	"phases":                              "closed seed lookup; activationReferenceSeedsCurrent",
	"stages":                              "closed seed lookup; activationReferenceSeedsCurrent",
	"journal_kinds":                       "closed seed lookup; activationReferenceSeedsCurrent",
	"authority_kinds":                     "closed seed lookup; activationReferenceSeedsCurrent",
	"assignment_slots":                    "closed seed lookup; activationReferenceSeedsCurrent",
	"assignment_transitions":              "closed seed lookup; activationReferenceSeedsCurrent",
	"ml_models":                           "model registry; verifyModelRegistryIntegrity on the partial path",
	"agents":                              "agent registry; verifyAgentRegistryIntegrity and the journal.actor_id orphan probe",
	"agents_human":                        "agent registry subtype; verifyAgentRegistryIntegrity",
	"agents_ml":                           "agent registry subtype; verifyAgentRegistryIntegrity",
	"agents_software":                     "agent registry subtype; verifyAgentRegistryIntegrity",
	"tasks":                               "projection; diffed in full by replayProjections",
	"edges":                               "projection; diffed in full by replayProjections",
	"activities":                          "projection; diffed in full by replayProjections",
	"labels":                              "projection; diffed in full by replayProjections",
	"comments":                            "projection; diffed in full by replayProjections",
	"task_attributions":                   "projection; diffed in full by replayProjections",
	"journal_activity_creations":          "activity projection input; read by the replay fold, not by the canonical validators",
	"actor_namespace_claims":              "actor registry; verifyActorPlacement and the claim foreign keys in the full verifyIntegrity",
	"fixed_actor_manifest_entries":        "actor registry; verifyActorPlacement and the claim foreign keys in the full verifyIntegrity",
	"governed_allocation_operations":      "governed allocation relations; not read by the canonical validators",
	"governed_operation_effect_rows":      "governed allocation relations; not read by the canonical validators",
	"governed_allocation_genesis":         "governed allocation relations; not read by the canonical validators",
	"governed_composed_supplement_owners": "governed allocation relations; not read by the canonical validators",
	"provenance_id_format":                "identity format marker; activationIDFormatCurrent",
}

// TestVerifiedPrefixDigestScopeIsPinned fails when a column or table that the
// skipped validators could read is added without updating the digest: every
// column of every hashed table must be in the digest's select list (or on an
// explicit allowlist) and must have a mutation case, and every covered table
// must be classified.
func TestVerifiedPrefixDigestScopeIsPinned(t *testing.T) {
	fixture := buildPrefixFixture(t)
	raw := openVerifiedTablesTestDB(t, fixture.path)

	hashed := map[string][]string{}
	for _, relation := range prefixJournalRelations {
		hashed[relation.table()] = selectedColumns(relation.query())
	}
	for _, relation := range []factContextRelation{factContextDecision, factContextEvidence} {
		hashed[relation.tableName()] = selectedColumns(relation.contextPrefixSQL())
	}

	mutated := map[string]bool{}
	for _, mutation := range prefixMutations(fixture) {
		mutated[mutation.table+"."+mutation.column] = true
	}

	for table, digested := range hashed {
		rows, err := raw.Query("SELECT name FROM pragma_table_info(?1)", table)
		if err != nil {
			t.Fatal(err)
		}
		var columns []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			columns = append(columns, name)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if len(columns) == 0 {
			t.Fatalf("table %s has no columns", table)
		}
		for _, column := range columns {
			key := table + "." + column
			if reason, ok := prefixUnhashedColumns[key]; ok {
				if reason == "" {
					t.Errorf("%s is allowlisted without a reason", key)
				}
				continue
			}
			if !slices.Contains(digested, column) {
				t.Errorf("column %s is not hashed by the verified-prefix digest and is not allowlisted: hash it or list it in prefixUnhashedColumns with a reason", key)
			}
			if !mutated[key] {
				t.Errorf("column %s has no mutation case in prefixMutations", key)
			}
		}
		for _, column := range digested {
			if !slices.Contains(columns, column) {
				t.Errorf("the digest selects %s.%s, which the table does not have", table, column)
			}
		}
	}

	for _, table := range verifiedCoveredTableNames {
		reason, ok := prefixTableDisposition[table]
		if !ok || reason == "" {
			t.Errorf("covered table %s is not classified: digest it or add a reason to prefixTableDisposition", table)
			continue
		}
		if _, isHashed := hashed[table]; isHashed != (reason == "digest") {
			t.Errorf("covered table %s: hashed=%v but disposition is %q", table, isHashed, reason)
		}
	}
	for table, reason := range prefixTableDisposition {
		if verifiedTableID(table) == 0 && table != "journal_decision_contexts" && table != "journal_evidence_contexts" {
			t.Errorf("prefixTableDisposition names %s, which is not a covered table", table)
		}
		_ = reason
	}
}

func init() {
	// The hashed tables classify themselves as "digest" so the table check above
	// is exhaustive over the covered list.
	for _, relation := range prefixJournalRelations {
		prefixTableDisposition[relation.table()] = "digest"
	}
	prefixTableDisposition["journal_decision_contexts"] = "digest"
	prefixTableDisposition["journal_evidence_contexts"] = "digest"
}

// ---------------------------------------------------------------------------
// Validator and decoder version
// ---------------------------------------------------------------------------

func TestPrefixDigestCurrentEncoding(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		current bool
	}{
		{"valid", prefixDigestRecorded(strings.Repeat("ab", 32)), true},
		{"empty", prefixDigestRecorded(""), false},
		{"short", prefixDigestRecorded(strings.Repeat("a", 63)), false},
		{"long", prefixDigestRecorded(strings.Repeat("a", 65)), false},
		{"non-hex", prefixDigestRecorded(strings.Repeat("z", 64)), false},
		{"old-version", "old:" + strings.Repeat("a", 64), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := prefixDigestCurrent(test.value); got != test.current {
				t.Fatalf("prefixDigestCurrent(%q) = %v, want %v", test.value, got, test.current)
			}
		})
	}
}

func TestVerifiedPrefixVersionIsPinnedAndForcesFullPath(t *testing.T) {
	// Pinned on purpose: change this together with verifiedPrefixValidatorVersion
	// when the canonical decoder or validators change.
	const pinned = "provenance.verified-prefix.validator.1"
	if verifiedPrefixValidatorVersion != pinned {
		t.Fatalf("verifiedPrefixValidatorVersion = %q, want the pinned %q", verifiedPrefixValidatorVersion, pinned)
	}
	fixture := buildPrefixFixture(t)
	_, row := mustReadPrefixRow(t, fixture.path)
	for _, digest := range []string{row.journal, row.contexts} {
		if !strings.HasPrefix(digest, pinned+":") {
			t.Fatalf("recorded digest %q does not carry the validator version", digest)
		}
	}

	// A prefix recorded by another validator version must not be trusted, even on
	// the generation shortcut, and must be rewritten by the next full path.
	other := strings.Replace(row.journal, pinned, "provenance.verified-prefix.validator.0", 1)
	if _, err := openVerifiedTablesTestDB(t, fixture.path).Exec("UPDATE "+verifiedPrefixTable+" SET journal_digest=?1", other); err != nil {
		t.Fatal(err)
	}
	raw := openVerifiedTablesTestDB(t, fixture.path)
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	scope := borrowConnScope(conn, projectionTargetLive)
	markLedgerStale(t, scope)
	if trust := scope.resolveVerifiedPrefix(); trust != (verifiedPrefixTrust{}) {
		t.Fatalf("trust = %+v for a prefix recorded under another validator version, want none", trust)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	_, countBefore := readLedgerGeneration(t, fixture.path)
	if err := reopenFileBacked(t, fixture.path); err != nil {
		t.Fatal(err)
	}
	if _, countAfter := readLedgerGeneration(t, fixture.path); countAfter != countBefore+1 {
		t.Fatalf("verified_count = %d, want %d: a version change must run the full path", countAfter, countBefore+1)
	}
	_, rewritten := mustReadPrefixRow(t, fixture.path)
	if !prefixDigestCurrent(rewritten.journal) || !prefixDigestCurrent(rewritten.contexts) {
		t.Fatalf("the full path did not rewrite the digests under the current version: %+v", rewritten)
	}
}

// ---------------------------------------------------------------------------
// Tail validation, counts, and boundary
// ---------------------------------------------------------------------------

func TestVerifiedPrefixBehindLedgerFastPathButNotFullTrust(t *testing.T) {
	fixture := buildPrefixFixture(t)
	db, err := Open(fixture.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	applyPrefixFacts(t, db, fixture.actor, fixture.task, fixture.boot, "sanctioned-behind-prefix")
	scope := takePoolScope(t, db)
	through, _, ok := scope.readVerifiedPrefix()
	ledger, ledgerOK := scope.readVerifiedLedger()
	if !ok || !ledgerOK || through >= ledger.identity.journalID || !scope.verifiedLedgerMatches() {
		t.Fatalf("sanctioned write did not leave a usable behind prefix: through=%d ledger=%+v", through, ledger)
	}
	if trust := scope.resolveVerifiedPrefix(); trust != (verifiedPrefixTrust{}) {
		t.Fatalf("behind prefix armed full-path trust: %+v", trust)
	}
	markLedgerStale(t, scope)
	scope.release()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, before := readLedgerGeneration(t, fixture.path)
	if err := reopenFileBacked(t, fixture.path); err != nil {
		t.Fatal(err)
	}
	if _, after := readLedgerGeneration(t, fixture.path); after != before+1 {
		t.Fatalf("stale full path did not verify the store: count %d -> %d", before, after)
	}
	through, _ = mustReadPrefixRow(t, fixture.path)
	if want := ledgerJournalID(t, fixture.path); through != want {
		t.Fatalf("full validation did not renew prefix: through %d, ledger %d", through, want)
	}
}

func TestVerifiedPrefixTailValidatesOnlyTheTail(t *testing.T) {
	fixture := buildStaleTailFixture(t)
	scope := takePoolScope(t, fixture.db)
	defer scope.release()

	trust := scope.resolveVerifiedPrefix()
	if !trust.journal || !trust.contexts || trust.through != fixture.prefixTop {
		t.Fatalf("trust = %+v, want both families trusted through %d (the digest still matches)", trust, fixture.prefixTop)
	}
	totalOps := countRows(t, scope, "SELECT COUNT(*) FROM journal_operations")
	canonicalOps := countRows(t, scope, "SELECT COUNT(*) FROM journal_operations WHERE canonical_mutation IS NOT NULL")
	tailOps := len(fixture.tails)

	release := scope.trustVerifiedPrefix()
	if err := scope.verifyFactContextIntegrity(); err != nil {
		t.Fatalf("tail fact-context validation: %v", err)
	}
	if err := scope.validateCanonicalOperations(); err != nil {
		t.Fatalf("tail canonical validation: %v", err)
	}
	release()
	if scope.canonicalOperationsVisited != tailOps || scope.factContextOperationsVisited != tailOps {
		t.Fatalf("trusted walk visited %d canonical and %d fact-context operations, want exactly the %d tail operations starting at through+1", scope.canonicalOperationsVisited, scope.factContextOperationsVisited, tailOps)
	}

	scope.canonicalOperationsVisited, scope.factContextOperationsVisited = 0, 0
	if err := scope.verifyFactContextIntegrity(); err != nil {
		t.Fatalf("full fact-context validation: %v", err)
	}
	if err := scope.validateCanonicalOperations(); err != nil {
		t.Fatalf("full canonical validation: %v", err)
	}
	if scope.canonicalOperationsVisited != totalOps || scope.factContextOperationsVisited != canonicalOps {
		t.Fatalf("an unarmed scope visited %d canonical (want %d) and %d fact-context (want %d) operations, want the whole store", scope.canonicalOperationsVisited, totalOps, scope.factContextOperationsVisited, canonicalOps)
	}
}

// TestVerifiedPrefixOpenRecordsTheNewTail runs the production open over a stale
// ledger with an appended tail.
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
	through, recorded := mustReadPrefixRow(t, fixture.path)
	top := maxJournalID(t, fixture.path)
	if through != top || ledgerJournalID(t, fixture.path) != top {
		t.Fatalf("prefix through %d, ledger %d, want both at the tail %d", through, ledgerJournalID(t, fixture.path), top)
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

// ---------------------------------------------------------------------------
// Open-path refusals with exact text
// ---------------------------------------------------------------------------

const startupCanonicalSuffix = `; where: startup canonical validation; when: before accepting the database; impact: Open fails closed and no projection row is mutated; fix: restore the operation, canonical bytes, and subtype rows from the same committed backup`

func openRefusalPrefix(path string) string {
	return fmt.Sprintf("sqlite.Open: read-only startup preflight failed on %q: ", path)
}

// TestVerifiedPrefixOldRowMutationRefusesTheOpen mutates one old row of each
// family out of band. The open must fail with the exact message that names that
// row's anchor, and must not advance the ledger or disturb the prefix row.
func TestVerifiedPrefixOldRowMutationRefusesTheOpen(t *testing.T) {
	q := regexp.QuoteMeta
	for _, test := range []struct {
		name      string
		statement string
		arg       func(prefixFixture) int64
		want      func(prefixFixture) string
	}{
		{
			"journal_operations", "UPDATE journal_operations SET mutation_digest=x'00' WHERE journal_id=?1",
			func(f prefixFixture) int64 { return f.anchor },
			func(f prefixFixture) string {
				return "^" + q(fmt.Sprintf("%sprovenance: replay projection diverged from stored projection: provenance: canonical operation %d field mutation digest diverged — stored=\"00\" canonical=\"", openRefusalPrefix(f.path), f.anchor)) +
					"[0-9a-f]{64}" + q("\""+startupCanonicalSuffix) + "$"
			},
		},
		{
			"journal", "UPDATE journal SET recorded_at=recorded_at+1 WHERE journal_id=?1",
			func(f prefixFixture) int64 { return f.decision },
			func(f prefixFixture) string {
				return "^" + q(fmt.Sprintf("%soperation %d effect 0: provenance: replay projection diverged from stored projection: provenance: canonical operation %d field row %d recorded_at diverged — stored=\"", openRefusalPrefix(f.path), f.anchor, f.anchor, f.decision)) +
					`\d+` + q("\" canonical=\"") + `\d+` + q("\""+startupCanonicalSuffix) + "$"
			},
		},
		{
			"journal_decisions", "UPDATE journal_decisions SET payload='{\"c\":1}' WHERE journal_id=?1",
			func(f prefixFixture) int64 { return f.decision },
			func(f prefixFixture) string {
				return "^" + q(fmt.Sprintf("%soperation %d effect 0: provenance: replay projection diverged from stored projection: provenance: canonical operation %d field row %d subtype column 2 diverged — stored=%q canonical=%q%s",
					openRefusalPrefix(f.path), f.anchor, f.anchor, f.decision, `{"c":1}`, "{}", startupCanonicalSuffix)) + "$"
			},
		},
		{
			"journal_decision_contexts", "DELETE FROM journal_decision_contexts WHERE decision_journal_id=?1",
			func(f prefixFixture) int64 { return f.decision },
			func(f prefixFixture) string {
				return "^" + q(fmt.Sprintf("%sprovenance: fact context integrity violated: context set mismatch for journal %d (stored=0, canonical=2) — where: canonical fact-context comparison for operation %d; when: fact-context validation; impact: the database is rejected rather than returning or replaying an incomplete fact; fix: restore exactly the canonical context rows for this fact",
					openRefusalPrefix(f.path), f.decision, f.anchor)) + "$"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := buildPrefixFixture(t)
			_, countBefore := readLedgerGeneration(t, fixture.path)
			throughBefore, digestsBefore := mustReadPrefixRow(t, fixture.path)
			rawExecPinned(t, fixture.path, []string{test.statement}, []any{test.arg(fixture)})

			db, err := Open(fixture.path, nil)
			if err == nil {
				_ = db.Close()
				t.Fatalf("opening a store with a mutated old %s row succeeded", test.name)
			}
			if !regexp.MustCompile(test.want(fixture)).MatchString(err.Error()) {
				t.Fatalf("open error does not match the expected exact message for the anchor:\n got: %s\nwant: %s", err.Error(), test.want(fixture))
			}
			if _, countAfter := readLedgerGeneration(t, fixture.path); countAfter != countBefore {
				t.Fatalf("a refused open advanced verified_count %d -> %d", countBefore, countAfter)
			}
			if through, digests := mustReadPrefixRow(t, fixture.path); through != throughBefore || digests != digestsBefore {
				t.Fatalf("a refused open disturbed the prefix row: %d %+v", through, digests)
			}
		})
	}
}

// TestVerifiedPrefixOpenRefusesADamagedTail runs Open over a stale ledger whose
// tail carries a forged operation. The refusal must be the full walk's text, and
// the ledger must not advance.
func TestVerifiedPrefixOpenRefusesADamagedTail(t *testing.T) {
	fixture := buildStaleTailFixture(t)
	forge := "UPDATE journal_operations SET mutation_digest=x'00' WHERE journal_id=?1"

	// The full walk's message, computed on the damaged state and rolled back.
	scope := takePoolScope(t, fixture.db)
	relaxConstraints(t, scope)
	if _, err := scope.conn.ExecContext(scope.ctx, "SAVEPOINT forged_tail"); err != nil {
		t.Fatal(err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, forge, fixture.tail); err != nil {
		t.Fatal(err)
	}
	if err := scope.verifyIntegrity(); err != nil {
		t.Fatalf("the forged tail should pass verifyIntegrity and fail the canonical walk: %v", err)
	}
	_, fullErr := scope.replayProjections()
	if fullErr == nil {
		t.Fatal("the full walk accepted the forged tail")
	}
	for _, statement := range []string{"ROLLBACK TO forged_tail", "RELEASE forged_tail", "PRAGMA ignore_check_constraints=OFF", "PRAGMA foreign_keys=ON"} {
		if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	scope.release()
	if err := fixture.db.Close(); err != nil {
		t.Fatal(err)
	}

	_, countBefore := readLedgerGeneration(t, fixture.path)
	throughBefore, digestsBefore := mustReadPrefixRow(t, fixture.path)
	rawExecPinned(t, fixture.path, []string{forge}, []any{fixture.tail})
	db, err := Open(fixture.path, nil)
	if err == nil {
		_ = db.Close()
		t.Fatal("opening a store with a forged tail operation succeeded")
	}
	want := openRefusalPrefix(fixture.path) + fullErr.Error()
	if err.Error() != want {
		t.Fatalf("open error differs from the full walk's message:\n got: %s\nwant: %s", err.Error(), want)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("canonical operation %d field mutation digest", fixture.tail)) {
		t.Fatalf("open error does not name the forged tail anchor %d: %s", fixture.tail, err.Error())
	}
	if _, countAfter := readLedgerGeneration(t, fixture.path); countAfter != countBefore {
		t.Fatalf("a refused open advanced verified_count %d -> %d", countBefore, countAfter)
	}
	if through, digests := mustReadPrefixRow(t, fixture.path); through != throughBefore || digests != digestsBefore {
		t.Fatal("a refused open disturbed the prefix row")
	}
}

// TestVerifiedPrefixTrustDoesNotBypassRegistryChecks changes a row the skipped
// validators do not read — an agent that journal.actor_id points at, and a
// closed seed lookup — while the prefix stays trusted. The open must still
// refuse or run the whole-store verification.
func TestVerifiedPrefixTrustDoesNotBypassRegistryChecks(t *testing.T) {
	for _, test := range []struct {
		name       string
		statements func(prefixFixture) ([]string, [][]any)
	}{
		{"agent row", func(f prefixFixture) ([]string, [][]any) {
			return []string{"DELETE FROM agents_software WHERE agent_id=?1", "DELETE FROM agents WHERE id=?1"},
				[][]any{{f.actor.String()}, {f.actor.String()}}
		}},
		{"seed lookup", func(prefixFixture) ([]string, [][]any) {
			return []string{"UPDATE statuses SET name=name||'-tampered' WHERE id=(SELECT MIN(id) FROM statuses)"}, [][]any{nil}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := buildPrefixFixture(t)
			_, countBefore := readLedgerGeneration(t, fixture.path)
			statements, args := test.statements(fixture)
			rawExecPinned(t, fixture.path, statements, args...)

			// The prefix digests only the journal families, so the prefix itself
			// stays trusted: the protection comes from the separate checks.
			conn, err := openVerifiedTablesTestDB(t, fixture.path).Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			scope := borrowConnScope(conn, projectionTargetLive)
			if trust := scope.resolveVerifiedPrefix(); !trust.journal || !trust.contexts {
				t.Fatalf("trust = %+v, want the journal prefix still trusted", trust)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := Open(fixture.path, nil)
			if err == nil {
				_ = db.Close()
				if _, countAfter := readLedgerGeneration(t, fixture.path); countAfter == countBefore {
					t.Fatalf("a changed %s was accepted without a refusal or a whole-store verification", test.name)
				}
				return
			}
			if test.name == "agent row" && !errors.Is(err, journal.ErrSubtypeIntegrity) {
				t.Fatalf("a removed journal actor returned %v, want ErrSubtypeIntegrity", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Trust resolution
// ---------------------------------------------------------------------------

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
			relaxConstraints(t, scope)
			if _, err := scope.conn.ExecContext(scope.ctx, test.statement, test.arg(fixture)); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			trust := scope.resolveVerifiedPrefix()
			if trust.journal != test.wantJournal || trust.contexts != test.wantContexts {
				t.Fatalf("trust = %+v, want journal=%v contexts=%v", trust, test.wantJournal, test.wantContexts)
			}
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

// TestVerifiedPrefixGenerationShortcutSkipsWithoutAHash proves the "stale ledger,
// unchanged generations" rule directly: with the prefix tables' counters equal
// to the ledger's, the prefix is trusted even though the recorded digest is
// syntactically valid but mismatched; malformed digests are never trusted.
func TestVerifiedPrefixGenerationShortcutSkipsWithoutAHash(t *testing.T) {
	fixture := buildPrefixFixture(t)
	db, err := Open(fixture.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	scope := takePoolScope(t, db)
	defer scope.release()

	markLedgerStale(t, scope) // a non-prefix table moved: the ledger is stale
	if scope.verifiedLedgerMatches() {
		t.Fatal("the ledger should be stale")
	}
	bogus := prefixDigestRecorded(strings.Repeat("0", 64))
	if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE "+verifiedPrefixTable+" SET journal_digest=?1, contexts_digest=?1", bogus); err != nil {
		t.Fatal(err)
	}
	if trust := scope.resolveVerifiedPrefix(); !trust.journal || !trust.contexts {
		t.Fatalf("trust = %+v, want both families skipped on unchanged generations without comparing the digest", trust)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE "+verifiedPrefixTable+" SET journal_digest=?1", prefixDigestRecorded("not-a-digest")); err != nil {
		t.Fatal(err)
	}
	if trust := scope.resolveVerifiedPrefix(); trust != (verifiedPrefixTrust{}) {
		t.Fatalf("trust = %+v for malformed digest, want none", trust)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE "+verifiedPrefixTable+" SET journal_digest=?1", bogus); err != nil {
		t.Fatal(err)
	}

	// A prefix table moves: the shortcut no longer applies and the bogus digest
	// cannot match.
	if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE journal SET recorded_at=recorded_at WHERE journal_id=?1", fixture.anchor); err != nil {
		t.Fatal(err)
	}
	if trust := scope.resolveVerifiedPrefix(); trust.journal {
		t.Fatalf("trust = %+v after a journal counter moved with a mismatching digest, want the journal untrusted", trust)
	}
}

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
			fixture := buildStaleTailFixture(t)
			scope := takePoolScope(t, fixture.db)
			defer scope.release()
			if _, err := scope.conn.ExecContext(scope.ctx, test.statement); err != nil {
				t.Fatalf("damage the prefix row: %v", err)
			}
			if trust := scope.resolveVerifiedPrefix(); trust != (verifiedPrefixTrust{}) {
				t.Fatalf("trust = %+v, want none", trust)
			}
			totalOps := countRows(t, scope, "SELECT COUNT(*) FROM journal_operations")
			canonicalOps := countRows(t, scope, "SELECT COUNT(*) FROM journal_operations WHERE canonical_mutation IS NOT NULL")
			release := scope.trustVerifiedPrefix()
			if err := scope.validateCanonicalOperations(); err != nil {
				t.Fatalf("full canonical validation: %v", err)
			}
			if err := scope.verifyFactContextIntegrity(); err != nil {
				t.Fatalf("full fact-context validation: %v", err)
			}
			release()
			if scope.canonicalOperationsVisited != totalOps || scope.factContextOperationsVisited != canonicalOps {
				t.Fatalf("visited %d canonical (want %d) and %d fact-context (want %d) operations, want the whole store", scope.canonicalOperationsVisited, totalOps, scope.factContextOperationsVisited, canonicalOps)
			}
		})
	}
}

func TestVerifiedPrefixMissingRowOnStaleLedgerIsRewrittenByTheOpen(t *testing.T) {
	fixture := buildPrefixFixture(t)
	if _, err := openVerifiedTablesTestDB(t, fixture.path).Exec("DELETE FROM " + verifiedPrefixTable); err != nil {
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
	if through, _ := mustReadPrefixRow(t, fixture.path); through != ledgerJournalID(t, fixture.path) {
		t.Fatalf("the full verification recorded prefix through %d, want %d", through, ledgerJournalID(t, fixture.path))
	}
}

func TestVerifiedPrefixPartialPathCarriesThePrefix(t *testing.T) {
	fixture := buildPrefixFixture(t)
	throughBefore, digestsBefore := mustReadPrefixRow(t, fixture.path)
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
	if throughAfter, digestsAfter := mustReadPrefixRow(t, fixture.path); throughAfter != throughBefore || digestsAfter != digestsBefore {
		t.Fatalf("the partial path changed the prefix: %d %+v -> %d %+v", throughBefore, digestsBefore, throughAfter, digestsAfter)
	}
}

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
	through, recorded := mustReadPrefixRow(t, fixture.path)
	if through != ledgerJournalID(t, fixture.path) {
		t.Fatalf("adoption seeded through %d, want the ledger journal id %d", through, ledgerJournalID(t, fixture.path))
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

func TestVerifiedPrefixActivationSeedsOnlyAbsentLegacyTable(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "existing-table-missing-row"
		if legacy {
			name = "absent-legacy-table"
		}
		t.Run(name, func(t *testing.T) {
			fixture := buildPrefixFixture(t)
			raw := openVerifiedTablesTestDB(t, fixture.path)
			if legacy {
				generation, _ := readLedgerGeneration(t, fixture.path)
				if err := simulateV3Ledger(t, fixture.path, generation); err != nil {
					t.Fatal(err)
				}
			} else if _, err := raw.Exec("DELETE FROM " + verifiedPrefixTable); err != nil {
				t.Fatal(err)
			}
			conn, err := raw.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			scope := borrowConnScope(conn, projectionTargetLive)
			if err := activateSchema(scope, nil, defaultActivationRetryPolicy()); err != nil {
				t.Fatalf("activate: %v", err)
			}
			if legacy && scope.canonicalOperationsVisited != 0 {
				t.Fatalf("legacy adoption decoded %d operations", scope.canonicalOperationsVisited)
			}
			if !legacy && (scope.canonicalOperationsVisited == 0 || scope.factContextOperationsVisited == 0) {
				t.Fatalf("missing row bypassed full validation: canonical=%d contexts=%d", scope.canonicalOperationsVisited, scope.factContextOperationsVisited)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			_, digests := mustReadPrefixRow(t, fixture.path)
			if !prefixDigestCurrent(digests.journal) || !prefixDigestCurrent(digests.contexts) {
				t.Fatalf("activation did not record a usable prefix: %+v", digests)
			}
		})
	}
}

// A forged old proof must not hide an invalid canonical operation or be
// renewed on a failed open, even when the other ledger identities still match.
func TestVerifiedPrefixVersionEnforcedByOpen(t *testing.T) {
	for _, route := range []string{"current", "adoption", "partial", "malformed", "missing", "newer", "partial-newer"} {
		t.Run(route, func(t *testing.T) {
			fixture := buildPrefixFixture(t)
			through, original := mustReadPrefixRow(t, fixture.path)
			raw := openVerifiedTablesTestDB(t, fixture.path)
			if route == "adoption" {
				generation, _ := readLedgerGeneration(t, fixture.path)
				if err := simulateV3Ledger(t, fixture.path, generation); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(verifiedPrefixDDL); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec("INSERT INTO "+verifiedPrefixTable+" VALUES (1, ?1, ?2, ?3)", through, original.journal, original.contexts); err != nil {
					t.Fatal(err)
				}
			}
			old := strings.Replace(original.journal, verifiedPrefixValidatorVersion, "old-validator", 1)
			if route == "malformed" {
				old = prefixDigestRecorded(strings.Repeat("z", 64))
			}
			if route == "missing" {
				if _, err := raw.Exec("DELETE FROM " + verifiedPrefixTable); err != nil {
					t.Fatal(err)
				}
			} else if strings.Contains(route, "newer") {
				if _, err := raw.Exec("UPDATE " + verifiedPrefixTable + " SET through_journal_id=through_journal_id+1"); err != nil {
					t.Fatal(err)
				}
			} else if _, err := raw.Exec("UPDATE "+verifiedPrefixTable+" SET journal_digest=?1", old); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(route, "partial") {
				if _, err := raw.Exec("UPDATE "+verifiedTableGenerationTable+" SET generation=generation+1 WHERE table_id=?1", verifiedTableID("agents")); err != nil {
					t.Fatal(err)
				}
				conn, err := raw.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				scope := borrowConnScope(conn, projectionTargetLive)
				before, err := scope.readVerifiedStoreIdentity()
				if err != nil {
					t.Fatal(err)
				}
				if _, eligible := scope.partialLedgerEligible(before, nil); !eligible {
					t.Fatal("fixture is not otherwise eligible for partial verification")
				}
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var digest []byte
			if err := raw.QueryRow("SELECT mutation_digest FROM journal_operations WHERE journal_id=?1", fixture.anchor).Scan(&digest); err != nil {
				t.Fatal(err)
			}
			adjust := "UPDATE " + verifiedTableGenerationTable + " SET generation=generation-1 WHERE table_id=?1"
			args := []any{verifiedTableID("journal_operations")}
			if route == "adoption" {
				adjust = "UPDATE " + storeGenerationTable + " SET generation=generation-1 WHERE id=1"
				args = nil
			}
			mutate := func(value []byte) {
				t.Helper()
				if _, err := raw.Exec("UPDATE journal_operations SET mutation_digest=?1 WHERE journal_id=?2", value, fixture.anchor); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(adjust, args...); err != nil {
					t.Fatal(err)
				}
			}
			mutate([]byte{0})
			if route == "adoption" {
				conn, err := raw.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				scope := borrowConnScope(conn, projectionTargetLive)
				err = activateSchema(scope, nil, defaultActivationRetryPolicy())
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("canonical operation %d field mutation digest diverged", fixture.anchor)) {
					t.Fatalf("activation did not reject the canonical anchor: %v", err)
				}
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			}
			db, err := Open(fixture.path, nil)
			if err == nil {
				_ = db.Close()
				t.Fatal("open accepted an invalid canonical operation under an incompatible prefix")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("canonical operation %d field mutation digest diverged", fixture.anchor)) {
				t.Fatalf("open returned the wrong error for the canonical anchor: %v", err)
			}
			_, after, exists := readPrefixRow(t, fixture.path)
			if route == "missing" {
				if exists {
					t.Fatal("failed open seeded a missing prefix")
				}
			} else if !exists || strings.Contains(route, "newer") && after.journal != original.journal ||
				!strings.Contains(route, "newer") && after.journal != old {
				t.Fatalf("failed open changed the old proof: %+v", after)
			}
			mutate(digest)
			_, countBefore := readLedgerGeneration(t, fixture.path)
			if err := reopenFileBacked(t, fixture.path); err != nil {
				t.Fatalf("open after restoring valid operation: %v", err)
			}
			_, renewed := mustReadPrefixRow(t, fixture.path)
			if !prefixDigestCurrent(renewed.journal) || !prefixDigestCurrent(renewed.contexts) {
				t.Fatalf("prefix not renewed after validation: %+v", renewed)
			}
			if strings.HasPrefix(route, "partial") {
				if _, countAfter := readLedgerGeneration(t, fixture.path); countAfter != countBefore+1 {
					t.Fatalf("partial route did not run whole-store validation: count %d -> %d", countBefore, countAfter)
				}
			}
		})
	}
}

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
	throughBefore, digestsBefore := mustReadPrefixRow(t, fixture.path)
	markLedgerStale(t, scope)
	if scope.persistVerifiedLedgerIfUnchanged(before) {
		t.Fatal("the ledger was recorded for a store that moved during the verification")
	}
	if throughAfter, digestsAfter := mustReadPrefixRow(t, fixture.path); throughAfter != throughBefore || digestsAfter != digestsBefore {
		t.Fatal("a refused ledger write rewrote the prefix row")
	}
}

// TestVerifiedPrefixDifferentialMatchesFullDecode compares the shortened walk
// with the full walk over tails that are clean and tails that are damaged: the
// outcome, error text, and first-error anchor must be identical. The case whose
// tail row is claimed by a prefix operation must widen to the full walk, which
// the visit counts show.
func TestVerifiedPrefixDifferentialMatchesFullDecode(t *testing.T) {
	for _, test := range []struct {
		name      string
		statement string
		args      func(staleTailFixture) []any
		widens    bool
	}{
		{"clean tail", "", nil, false},
		{"tail context removed", "DELETE FROM journal_decision_contexts WHERE decision_journal_id>?1", func(f staleTailFixture) []any { return []any{f.prefixTop} }, false},
		{"tail context added", "INSERT INTO journal_evidence_contexts (evidence_journal_id,context_kind,context_identity) SELECT journal_id,'zz','zz' FROM journal_evidence WHERE journal_id>?1", func(f staleTailFixture) []any { return []any{f.prefixTop} }, false},
		{"tail digest forged", "UPDATE journal_operations SET mutation_digest=x'00' WHERE journal_id=?1", func(f staleTailFixture) []any { return []any{f.tail} }, false},
		{"second tail digest forged", "UPDATE journal_operations SET mutation_digest=x'00' WHERE journal_id=?1", func(f staleTailFixture) []any { return []any{f.tails[1]} }, false},
		{"tail bytes forged", "UPDATE journal_operations SET canonical_mutation=x'00' WHERE journal_id=?1", func(f staleTailFixture) []any { return []any{f.tail} }, false},
		{"tail row claimed by a prefix operation", "UPDATE journal SET produced_by_operation_journal_id=?2 WHERE journal_id=(SELECT MAX(journal_id) FROM journal)", func(f staleTailFixture) []any { return []any{f.tail, f.anchor} }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := buildStaleTailFixture(t)
			scope := takePoolScope(t, fixture.db)
			defer scope.release()
			if test.statement != "" {
				relaxConstraints(t, scope)
				if _, err := scope.conn.ExecContext(scope.ctx, test.statement, test.args(fixture)...); err != nil {
					t.Fatalf("damage the tail: %v", err)
				}
			}
			trust := scope.resolveVerifiedPrefix()
			if !trust.journal || !trust.contexts {
				t.Fatalf("trust = %+v; the prefix is untouched so it must be trusted", trust)
			}

			run := func(armed bool) (integrity, replay string, visited int) {
				scope.canonicalOperationsVisited = 0
				if armed {
					release := scope.trustVerifiedPrefix()
					defer release()
				}
				if err := scope.verifyIntegrity(); err != nil {
					integrity = err.Error()
				}
				if _, err := scope.replayProjections(); err != nil {
					replay = err.Error()
				}
				return integrity, replay, scope.canonicalOperationsVisited
			}
			fullIntegrity, fullReplay, fullVisited := run(false)
			shortIntegrity, shortReplay, shortVisited := run(true)
			if fullIntegrity != shortIntegrity || fullReplay != shortReplay {
				t.Fatalf("the shortened walk diverged from the full walk:\nfull  integrity=%q replay=%q\nshort integrity=%q replay=%q", fullIntegrity, fullReplay, shortIntegrity, shortReplay)
			}
			switch {
			case test.statement == "":
				if fullIntegrity != "" || fullReplay != "" {
					t.Fatalf("a clean tail was refused: integrity=%q replay=%q", fullIntegrity, fullReplay)
				}
				if shortVisited != len(fixture.tails) {
					t.Fatalf("the shortened walk visited %d operations, want the %d tail operations", shortVisited, len(fixture.tails))
				}
			case fullIntegrity == "" && fullReplay == "":
				t.Fatalf("the damaged tail %q was not refused by the full walk", test.name)
			}
			if test.widens && shortVisited != fullVisited {
				t.Fatalf("a tail reaching the prefix visited %d operations armed vs %d full; it must widen to the full walk", shortVisited, fullVisited)
			}
		})
	}
}

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
