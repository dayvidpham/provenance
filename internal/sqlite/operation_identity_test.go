package sqlite

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestStoredOperationIdentityUsesStructuralCanonicalComparison(t *testing.T) {
	db := newJournalDB(t)
	actorA, _ := ptypes.ParseActorID("fixture--018f0000-0000-7000-8000-000000000002")
	actorB, _ := ptypes.ParseActorID("fixture--018f0000-0000-7000-8000-000000000003")
	canonicalBytes, err := os.ReadFile("../../testdata/contract/mutation_v1_v004.bin")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonicalBytes)
	authority := journal.JournalID(7)
	stored := persistAndLoadStoredIdentity(t, db, storedOperationReplayIdentity{
		operationID: "fixture-operation", actorID: actorA, authorityJournalID: &authority,
		commandDigest: []byte("command"), mutationDigest: digest[:],
		encodingVersion: journal.MutationEncodingV1.String(), canonicalMutation: canonicalBytes,
	})
	exact := journal.OperationInput{
		OperationID: "fixture-operation", ActorID: actorA, AuthorityJournalID: &authority, CommandDigest: []byte("command"),
		Conditions: []journal.Condition{{Kind: journal.ConditionCurrentFact, Selector: journal.FactSelector{Kind: journal.FactEvidence, Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskUnscoped}}, EvidenceKind: "fixture.evidence"}, AssertedJournalID: 0}},
		Effects:    []journal.Effect{{Sort: journal.EffectBootstrapAuthority, ResultSlot: "root", BootstrapLabel: "root", OperationAuthorityID: journal.OperationAuthorityID(actorA.String())}},
	}
	reconciled := 0
	if err := compareStoredOperationIdentity(stored, exact, func(input journal.OperationInput) (journal.OperationInput, error) {
		reconciled++
		return input, nil
	}); err != nil || reconciled != 0 {
		t.Fatalf("exact canonical comparison err=%v reconciliations=%d", err, reconciled)
	}

	// One changed case per axis with correct broad conflict detection.
	cases := []struct {
		name   string
		change func(*journal.OperationInput)
		axis   journal.ConflictAxis
		index  int
	}{
		{"actor", func(in *journal.OperationInput) { in.ActorID = actorB }, journal.ConflictActor, -1},
		{"authority", func(in *journal.OperationInput) { value := journal.JournalID(8); in.AuthorityJournalID = &value }, journal.ConflictAuthority, -1},
		{"command", func(in *journal.OperationInput) { in.CommandDigest = []byte("changed") }, journal.ConflictCommand, -1},
		{"condition", func(in *journal.OperationInput) { in.Conditions[0].AssertedJournalID = 2 }, journal.ConflictCondition, 0},
		{"effect", func(in *journal.OperationInput) { in.Effects[0].BootstrapLabel = "changed" }, journal.ConflictEffect, 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneIdentityInput(exact)
			test.change(&candidate)
			err := compareStoredOperationIdentity(stored, candidate, func(input journal.OperationInput) (journal.OperationInput, error) { return input, nil })
			var conflict *journal.OperationConflict
			if !errors.Is(err, journal.ErrOperationConflict) || !errors.As(err, &conflict) || conflict.Axis != test.axis || conflict.Index != test.index {
				t.Fatalf("error=%v conflict=%+v want axis=%s index=%d", err, conflict, test.axis, test.index)
			}
			// All axes are nonzero; zero would indicate a bug in conflict construction.
		})
	}

	task, _ := ptypes.ParseTaskID("fixture--018f0000-0000-7000-8000-000000000020")
	allocatedInput := journal.OperationInput{OperationID: "allocated", ActorID: actorA, CommandDigest: []byte("allocated"), Effects: []journal.Effect{{Sort: journal.EffectTaskCreateAllocated, ResultSlot: "task", TaskID: task, Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped}}}
	allocatedMutation, err := journal.Canonicalize(allocatedInput)
	if err != nil {
		t.Fatal(err)
	}
	allocatedStored := storedOperationReplayIdentity{operationID: allocatedInput.OperationID, actorID: actorA, commandDigest: []byte("allocated"), mutationDigest: allocatedMutation.DerivedDigest(), encodingVersion: allocatedMutation.EncodingVersion().String(), canonicalMutation: allocatedMutation.CanonicalBytes()}
	reconciled = 0
	if err := compareStoredOperationIdentity(allocatedStored, allocatedInput, func(input journal.OperationInput) (journal.OperationInput, error) { reconciled++; return input, nil }); err != nil || reconciled != 1 {
		t.Fatalf("allocated identity err=%v reconciliations=%d", err, reconciled)
	}
}

func TestStoredOperationIdentitySeparatesCorruptionFromOpaqueConflict(t *testing.T) {
	actor, _ := ptypes.ParseActorID("fixture--018f0000-0000-7000-8000-000000000002")
	canonicalBytes, err := os.ReadFile("../../testdata/contract/mutation_v1_v004.bin")
	if err != nil {
		t.Fatal(err)
	}
	badDigest := sha256.Sum256([]byte("not-the-canonical-bytes"))
	canonical := storedOperationReplayIdentity{operationID: "canonical", actorID: actor, commandDigest: []byte("command"), mutationDigest: badDigest[:], encodingVersion: journal.MutationEncodingV1.String(), canonicalMutation: canonicalBytes}
	err = compareStoredOperationIdentity(canonical, journal.OperationInput{ActorID: actor, CommandDigest: []byte("command")}, nil)
	var conflict *journal.OperationConflict
	if !errors.Is(err, journal.ErrCanonicalMutation) || errors.As(err, &conflict) {
		t.Fatalf("stored canonical corruption err=%v conflict=%+v", err, conflict)
	}

	// A row with no encoding_version is a corruption signal (not a legacy-compat path).
	// compareStoredOperationIdentity must return a CanonicalMutationError, not a conflict.
	opaque := storedOperationReplayIdentity{operationID: "opaque", actorID: actor, commandDigest: []byte("command"), mutationDigest: []byte("stored")}
	err = compareStoredOperationIdentity(opaque, journal.OperationInput{ActorID: actor, CommandDigest: []byte("command")}, nil)
	var corruptionErr *journal.CanonicalMutationError
	if !errors.As(err, &corruptionErr) || errors.As(err, &conflict) {
		t.Fatalf("opaque row (no encoding_version) returned non-corruption error: err=%v conflict=%+v", err, conflict)
	}
}

func TestApplyUsesStructuralStoredOperationIdentity(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	authority := genesisBoot(t, db, actor)
	input := journal.OperationInput{
		OperationID: "identity-apply", ActorID: actor, AuthorityJournalID: &authority,
		CommandDigest: []byte("command"), RecordedAt: time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "identity.event", ResultSlot: "event"}},
	}
	first, err := db.Apply(input)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := db.Apply(input)
	if err != nil || !replayed.ShortCircuited || replayed.AnchorJournalID != first.AnchorJournalID {
		t.Fatalf("exact replay result=%+v err=%v", replayed, err)
	}
	for _, test := range []struct {
		name   string
		change func(*journal.OperationInput)
		axis   journal.ConflictAxis
		index  int
	}{
		{"command", func(candidate *journal.OperationInput) { candidate.CommandDigest = []byte("changed") }, journal.ConflictCommand, -1},
		{"effect", func(candidate *journal.OperationInput) { candidate.Effects[0].EventKind = "identity.changed" }, journal.ConflictEffect, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneIdentityInput(input)
			test.change(&candidate)
			result, err := db.Apply(candidate)
			var conflict *journal.OperationConflict
			if !errors.Is(err, journal.ErrOperationConflict) || !errors.As(err, &conflict) || result.Kind != journal.CommittedConflict || result.Conflict != conflict || conflict.Axis != test.axis || conflict.Index != test.index {
				t.Fatalf("result=%+v err=%v conflict=%+v want axis=%s index=%d", result, err, conflict, test.axis, test.index)
			}
		})
	}
}

func TestApplyReplayAxisAndIndexMatrixHasZeroDurableDelta(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	authority := genesisBoot(t, db, actor)
	other, _ := ptypes.ParseActorID("fixture--018f0000-0000-7000-8000-000000000099")
	scope, err := db.bindConn(context.Background())
	if err != nil {
		t.Fatalf("bind alternate actor seed scope: %v", err)
	}
	if err := sqlitex.Execute(scope.conn, `INSERT INTO agents (id, kind_id) VALUES (?1, ?2)`, &sqlitex.ExecOptions{Args: []any{other.String(), int(ptypes.AgentKindSoftware)}}); err != nil {
		scope.release()
		t.Fatalf("seed alternate actor: %v", err)
	}
	if err := sqlitex.Execute(scope.conn, `INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1,?2,?3,?4)`, &sqlitex.ExecOptions{Args: []any{other.String(), "alternate", "0", "test"}}); err != nil {
		scope.release()
		t.Fatalf("seed alternate software actor: %v", err)
	}
	scope.release()

	setup, err := db.Apply(journal.OperationInput{
		OperationID: "identity-matrix-setup", ActorID: actor, AuthorityJournalID: &authority,
		CommandDigest: []byte("identity-matrix-setup"),
		Effects: []journal.Effect{
			{Sort: journal.EffectDecision, ResultSlot: "decision", TaskID: task, DecisionKind: "identity.matrix.decision", Payload: []byte(`{"decision":true}`)},
			{Sort: journal.EffectEvidence, ResultSlot: "evidence", TaskID: task, EvidenceKind: "identity.matrix.evidence", ContentDigest: []byte("evidence"), Payload: []byte(`{"evidence":true}`)},
		},
	})
	if err != nil {
		t.Fatalf("Apply matrix setup: %v", err)
	}
	slotJID := func(slot journal.ResultSlotID) journal.JournalID {
		for _, binding := range setup.ResultSlots {
			if binding.Slot == slot {
				return binding.ProducedJournalID
			}
		}
		t.Fatalf("setup result has no slot %q", slot)
		return 0
	}
	input := journal.OperationInput{
		OperationID: "identity-matrix", ActorID: actor, AuthorityJournalID: &authority,
		CommandDigest: []byte("identity-matrix"),
		Conditions: []journal.Condition{
			{Kind: journal.ConditionExactFact, Selector: journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "identity.matrix.decision", Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: task}}}, AssertedJournalID: slotJID("decision")},
			{Kind: journal.ConditionCurrentFact, Selector: journal.FactSelector{Kind: journal.FactEvidence, EvidenceKind: "identity.matrix.evidence", Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: task}}}, AssertedJournalID: slotJID("evidence")},
		},
		Effects: []journal.Effect{
			{Sort: journal.EffectDecision, DecisionKind: "identity.matrix.output", Payload: []byte(`{"index":0}`)},
			{Sort: journal.EffectEvidence, EvidenceKind: "identity.matrix.output", ContentDigest: []byte("output"), Payload: []byte(`{"index":1}`)},
		},
	}
	first, err := db.Apply(input)
	if err != nil {
		t.Fatalf("Apply matrix baseline: %v", err)
	}
	beforeRows := journalRowCount(t, db)
	replayed, err := db.Apply(input)
	if err != nil || !replayed.ShortCircuited || replayed.AnchorJournalID != first.AnchorJournalID {
		t.Fatalf("exact replay result=%+v err=%v", replayed, err)
	}
	assertApplyZeroDelta(t, db, beforeRows)
	taskContext, err := journal.TaskContext(task)
	if err != nil {
		t.Fatal(err)
	}
	actorContext, err := journal.ActorContext(actor)
	if err != nil {
		t.Fatal(err)
	}
	alternateTask, _ := ptypes.ParseTaskID("fixture--018f0000-0000-7000-8000-000000000098")

	cases := []struct {
		name   string
		axis   journal.ConflictAxis
		index  int
		change func(*journal.OperationInput)
	}{
		{"actor", journal.ConflictActor, -1, func(in *journal.OperationInput) { in.ActorID = other }},
		{"authority", journal.ConflictAuthority, -1, func(in *journal.OperationInput) { in.AuthorityJournalID = nil }},
		{"command", journal.ConflictCommand, -1, func(in *journal.OperationInput) { in.CommandDigest = []byte("changed") }},
		{"condition-length", journal.ConflictCondition, -1, func(in *journal.OperationInput) { in.Conditions = in.Conditions[:1] }},
		{"condition-exact-kind-at-zero", journal.ConflictCondition, 0, func(in *journal.OperationInput) { in.Conditions[0].Kind = journal.ConditionCurrentFact }},
		{"condition-decision-kind-at-zero", journal.ConflictCondition, 0, func(in *journal.OperationInput) { in.Conditions[0].Selector.DecisionKind = "identity.matrix.changed" }},
		{"condition-current-selector-at-one", journal.ConflictCondition, 1, func(in *journal.OperationInput) { in.Conditions[1].Selector.EvidenceKind = "identity.matrix.changed" }},
		{"condition-selector-variant-at-one", journal.ConflictCondition, 1, func(in *journal.OperationInput) {
			in.Conditions[1].Selector.Kind = journal.FactDecision
			in.Conditions[1].Selector.DecisionKind = "identity.matrix.decision"
			in.Conditions[1].Selector.EvidenceKind = ""
		}},
		{"condition-task-scope-kind-at-one", journal.ConflictCondition, 1, func(in *journal.OperationInput) {
			in.Conditions[1].Selector.Filter.TaskScope = journal.FactTaskScope{Kind: journal.FactTaskAny}
		}},
		{"condition-task-scope-id-at-one", journal.ConflictCondition, 1, func(in *journal.OperationInput) { in.Conditions[1].Selector.Filter.TaskScope.TaskID = alternateTask }},
		{"condition-context-kind-at-one", journal.ConflictCondition, 1, func(in *journal.OperationInput) {
			in.Conditions[1].Selector.Filter.RequiredContexts = []journal.EventContext{taskContext}
		}},
		{"condition-context-identity-at-one", journal.ConflictCondition, 1, func(in *journal.OperationInput) {
			in.Conditions[1].Selector.Filter.RequiredContexts = []journal.EventContext{actorContext}
		}},
		{"condition-effective-actor-at-one", journal.ConflictCondition, 1, func(in *journal.OperationInput) {
			in.Conditions[1].Selector.Filter.EffectiveActorIDs = []journal.ActorID{actor}
		}},
		{"condition-operation-at-one", journal.ConflictCondition, 1, func(in *journal.OperationInput) {
			in.Conditions[1].Selector.Filter.OperationIDs = []journal.OperationID{"identity-matrix-setup"}
		}},
		{"condition-current-journal-at-one", journal.ConflictCondition, 1, func(in *journal.OperationInput) { in.Conditions[1].AssertedJournalID++ }},
		{"effect-length", journal.ConflictEffect, -1, func(in *journal.OperationInput) { in.Effects = in.Effects[:1] }},
		{"effect-decision-at-zero", journal.ConflictEffect, 0, func(in *journal.OperationInput) { in.Effects[0].DecisionKind = "identity.matrix.changed" }},
		{"effect-evidence-at-one", journal.ConflictEffect, 1, func(in *journal.OperationInput) { in.Effects[1].ContentDigest = []byte("changed") }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneIdentityInput(input)
			test.change(&candidate)
			result, err := db.Apply(candidate)
			var conflict *journal.OperationConflict
			if !errors.Is(err, journal.ErrOperationConflict) || !errors.As(err, &conflict) || result.Kind != journal.CommittedConflict || result.Conflict != conflict || conflict.Axis != test.axis || conflict.Index != test.index {
				t.Fatalf("result=%+v err=%v conflict=%+v want axis=%s index=%d", result, err, conflict, test.axis, test.index)
			}
			assertApplyZeroDelta(t, db, beforeRows)
		})
	}
}

// TestApplyConditionFailClosedWithZeroWrites verifies that a failing condition
// (CurrentFact: assertion of a non-existent evidence row being current) returns
// typed ConditionFailure and produces zero persisted journal rows.
func TestApplyConditionFailClosedWithZeroWrites(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	authority := genesisBoot(t, db, actor)
	before := journalRowCount(t, db)
	// Asserted JournalID 999 does not exist; CurrentFact must fail.
	nonExistentJID := journal.JournalID(999)
	input := journal.OperationInput{
		OperationID: "condition-fail-closed", ActorID: actor, AuthorityJournalID: &authority, CommandDigest: []byte("command"),
		Conditions: []journal.Condition{{
			Kind:              journal.ConditionCurrentFact,
			Selector:          journal.FactSelector{Kind: journal.FactEvidence, Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskUnscoped}}, EvidenceKind: "fixture.evidence"},
			AssertedJournalID: nonExistentJID,
		}},
		Effects: []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "fixture.event"}},
	}
	_, err := db.Apply(input)
	if err == nil {
		t.Fatal("condition Apply succeeded but should have failed (no matching fact row)")
	}
	if !errors.Is(err, journal.ErrConditionFailed) {
		t.Fatalf("condition Apply error is not ErrConditionFailed: %v", err)
	}
	var cf *journal.ConditionFailure
	if !errors.As(err, &cf) {
		t.Fatalf("condition Apply error does not wrap *ConditionFailure: %v", err)
	}
	if cf.Index != 0 || cf.Kind != journal.ConditionCurrentFact {
		t.Fatalf("ConditionFailure: got index=%d kind=%s, want index=0 kind=CurrentFact", cf.Index, cf.Kind)
	}
	if cf.Reason != journal.ConditionFactMissing {
		t.Fatalf("ConditionFailure reason: got %s, want FactMissing", cf.Reason)
	}
	if after := journalRowCount(t, db); after != before {
		t.Fatalf("condition Apply changed journal rows (want zero writes): before=%d after=%d", before, after)
	}
	result, lookupErr := db.LookupCommitted(input.OperationID)
	if lookupErr != nil || result.Kind != journal.CommittedAbsent {
		t.Fatalf("condition Apply persisted operation: result=%+v err=%v", result, lookupErr)
	}
}

// TestApplyConditionCurrentFactAbsenceSucceeds verifies that CurrentFact with
// AssertedJournalID=0 (absence assertion) succeeds when no matching fact exists.
func TestApplyConditionCurrentFactAbsenceSucceeds(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	authority := genesisBoot(t, db, actor)
	input := journal.OperationInput{
		OperationID: "condition-absence-ok", ActorID: actor, AuthorityJournalID: &authority, CommandDigest: []byte("command"),
		Conditions: []journal.Condition{{
			Kind:              journal.ConditionCurrentFact,
			Selector:          journal.FactSelector{Kind: journal.FactEvidence, Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskUnscoped}}, EvidenceKind: "fixture.evidence"},
			AssertedJournalID: 0, // assert absence
		}},
		Effects: []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "fixture.event"}},
	}
	res, err := db.Apply(input)
	if err != nil {
		t.Fatalf("absence-assertion Apply failed: %v", err)
	}
	if res.Kind != journal.CommittedExact {
		t.Fatalf("absence-assertion Apply result kind = %v, want CommittedExact", res.Kind)
	}
	// Exact retry must return the same result.
	res2, err2 := db.Apply(input)
	if err2 != nil || res2.Kind != journal.CommittedExact || !res2.ShortCircuited {
		t.Fatalf("condition Apply retry: result=%+v err=%v", res2, err2)
	}
}

func journalRowCount(t *testing.T, db *DB) int {
	t.Helper()
	scope, err := db.bindConn(context.Background())
	if err != nil {
		t.Fatalf("bind journal row count scope: %v", err)
	}
	defer scope.release()
	count := -1
	if err := sqlitex.Execute(scope.conn, "SELECT COUNT(*) FROM journal", &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error { count = stmt.ColumnInt(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertApplyZeroDelta(t *testing.T, db *DB, rows int) {
	t.Helper()
	if got := journalRowCount(t, db); got != rows {
		t.Fatalf("replay changed journal row count: got %d, want %d", got, rows)
	}
}

func persistAndLoadStoredIdentity(t *testing.T, db *DB, input storedOperationReplayIdentity) storedOperationReplayIdentity {
	t.Helper()
	scope, err := db.bindConn(context.Background())
	if err != nil {
		t.Fatalf("bind operation identity fixture scope: %v", err)
	}
	defer scope.release()
	if err := sqlitex.Execute(scope.conn, `CREATE TEMP TABLE operation_identity_fixture (
		operation_id TEXT PRIMARY KEY, actor_id TEXT NOT NULL, authority_journal_id INTEGER,
		command_digest BLOB NOT NULL, mutation_digest BLOB NOT NULL,
		encoding_version TEXT NOT NULL, canonical_mutation BLOB NOT NULL
	)`, nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.Execute(scope.conn, `INSERT INTO operation_identity_fixture VALUES (?1,?2,?3,?4,?5,?6,?7)`, &sqlitex.ExecOptions{Args: []any{
		string(input.operationID), input.actorID.String(), int64(*input.authorityJournalID), input.commandDigest, input.mutationDigest, input.encodingVersion, input.canonicalMutation,
	}}); err != nil {
		t.Fatal(err)
	}
	var output storedOperationReplayIdentity
	if err := sqlitex.Execute(scope.conn, `SELECT operation_id,actor_id,authority_journal_id,command_digest,mutation_digest,encoding_version,canonical_mutation FROM operation_identity_fixture`, &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
		output.operationID = journal.OperationID(stmt.ColumnText(0))
		actor, err := ptypes.ParseActorID(stmt.ColumnText(1))
		if err != nil {
			return err
		}
		output.actorID = actor
		authority := journal.JournalID(stmt.ColumnInt64(2))
		output.authorityJournalID = &authority
		output.commandDigest = readIdentityBlob(stmt, 3)
		output.mutationDigest = readIdentityBlob(stmt, 4)
		output.encodingVersion = stmt.ColumnText(5)
		output.canonicalMutation = readIdentityBlob(stmt, 6)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return output
}

func readIdentityBlob(stmt *zs.Stmt, column int) []byte {
	value := make([]byte, stmt.ColumnLen(column))
	stmt.ColumnBytes(column, value)
	return value
}

func cloneIdentityInput(input journal.OperationInput) journal.OperationInput {
	clone := input
	clone.AuthorityJournalID = cloneJournalID(input.AuthorityJournalID)
	clone.CommandDigest = append([]byte(nil), input.CommandDigest...)
	clone.Conditions = append([]journal.Condition(nil), input.Conditions...)
	clone.Effects = append([]journal.Effect(nil), input.Effects...)
	for i := range clone.Conditions {
		filter := &clone.Conditions[i].Selector.Filter
		filter.RequiredContexts = append([]journal.EventContext(nil), filter.RequiredContexts...)
		filter.EffectiveActorIDs = append([]journal.ActorID(nil), filter.EffectiveActorIDs...)
		filter.OperationIDs = append([]journal.OperationID(nil), filter.OperationIDs...)
	}
	for i := range clone.Effects {
		clone.Effects[i].Payload = append([]byte(nil), input.Effects[i].Payload...)
		clone.Effects[i].Contexts = append([]journal.EventContext(nil), input.Effects[i].Contexts...)
		clone.Effects[i].ContentDigest = append([]byte(nil), input.Effects[i].ContentDigest...)
	}
	return clone
}
