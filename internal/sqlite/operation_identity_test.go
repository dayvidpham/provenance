package sqlite

import (
	"crypto/sha256"
	"errors"
	"os"
	"strings"
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
		name  string
		change func(*journal.OperationInput)
		axis  journal.ConflictAxis
		index int
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

func TestApplyRejectsConditionsBeforeAnyWrite(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	authority := genesisBoot(t, db, actor)
	before := journalRowCount(t, db)
	input := journal.OperationInput{
		OperationID: "condition-fail-closed", ActorID: actor, AuthorityJournalID: &authority, CommandDigest: []byte("command"),
		Conditions: []journal.Condition{{Kind: journal.ConditionCurrentFact, Selector: journal.FactSelector{Kind: journal.FactEvidence, Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskUnscoped}}, EvidenceKind: "fixture.evidence"}}},
		Effects:    []journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: task, EventKind: "fixture.event"}},
	}
	if _, err := db.Apply(input); err == nil || !strings.Contains(err.Error(), "transaction-local condition evaluation") || !strings.Contains(err.Error(), "nothing was committed") {
		t.Fatalf("condition Apply error is not actionable: %v", err)
	}
	if after := journalRowCount(t, db); after != before {
		t.Fatalf("condition Apply changed journal rows: before=%d after=%d", before, after)
	}
	result, err := db.LookupCommitted(input.OperationID)
	if err != nil || result.Kind != journal.CommittedAbsent {
		t.Fatalf("condition Apply persisted operation: result=%+v err=%v", result, err)
	}
}

func journalRowCount(t *testing.T, db *DB) int {
	t.Helper()
	db.Lock()
	defer db.Unlock()
	count := -1
	if err := sqlitex.Execute(db.Conn(), "SELECT COUNT(*) FROM journal", &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error { count = stmt.ColumnInt(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	return count
}

func persistAndLoadStoredIdentity(t *testing.T, db *DB, input storedOperationReplayIdentity) storedOperationReplayIdentity {
	t.Helper()
	db.Lock()
	defer db.Unlock()
	if err := sqlitex.Execute(db.Conn(), `CREATE TEMP TABLE operation_identity_fixture (
		operation_id TEXT PRIMARY KEY, actor_id TEXT NOT NULL, authority_journal_id INTEGER,
		command_digest BLOB NOT NULL, mutation_digest BLOB NOT NULL,
		encoding_version TEXT NOT NULL, canonical_mutation BLOB NOT NULL
	)`, nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.Execute(db.Conn(), `INSERT INTO operation_identity_fixture VALUES (?1,?2,?3,?4,?5,?6,?7)`, &sqlitex.ExecOptions{Args: []any{
		string(input.operationID), input.actorID.String(), int64(*input.authorityJournalID), input.commandDigest, input.mutationDigest, input.encodingVersion, input.canonicalMutation,
	}}); err != nil {
		t.Fatal(err)
	}
	var output storedOperationReplayIdentity
	if err := sqlitex.Execute(db.Conn(), `SELECT operation_id,actor_id,authority_journal_id,command_digest,mutation_digest,encoding_version,canonical_mutation FROM operation_identity_fixture`, &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
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
	return clone
}
