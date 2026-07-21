package provenance

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
)

func snapshotSQLTables(t *testing.T, db *sql.DB, tables ...string) map[string][][]string {
	t.Helper()
	out := make(map[string][][]string, len(tables))
	for _, table := range tables {
		rows, err := db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			record := make([]string, len(values))
			for i, value := range values {
				switch value := value.(type) {
				case nil:
					record[i] = "<null>"
				case []byte:
					record[i] = "blob:" + hex.EncodeToString(value)
				default:
					record[i] = fmt.Sprintf("%T:%v", value, value)
				}
			}
			out[table] = append(out[table], record)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		sort.Slice(out[table], func(i, j int) bool { return fmt.Sprint(out[table][i]) < fmt.Sprint(out[table][j]) })
	}
	return out
}

func TestDBOSCompletedRetryEveryCanonicalOperandHasZeroCallbackAndWrites(t *testing.T) {
	dbPath := t.TempDir() + "/exhaustive.db"
	db, err := openSharedSQL(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	root, err := dbos.NewDBOSContext(context.Background(), dbos.Config{AppName: "dbos-exhaustive", SqliteSystemDB: db, ApplicationVersion: "exhaustive-v2"})
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := tracker.RegisterSoftwareAgent("retry", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := tracker.RegisterSoftwareAgent("retry", "other", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesisInput := OperationInput{OperationID: "dbos-exhaustive-genesis", ActorID: actor.ID, CommandDigest: []byte("genesis"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority", BootstrapLabel: "root", OperationAuthorityID: "root"}}}
	genesisResult, err := tracker.Journal().Apply(genesisInput)
	if err != nil {
		t.Fatal(err)
	}
	authority, _ := slotJournalID(genesisResult, "authority")
	target, err := tracker.As(actor.ID, authority).Create("retry", "target", "target", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID("dbos-exhaustive-target"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.Journal().Apply(OperationInput{OperationID: "dbos-exhaustive-assignment-evidence", ActorID: actor.ID, AuthorityJournalID: &authority, CommandDigest: []byte("setup"), Effects: []Effect{
		{Sort: EffectAssignmentStart, TaskID: target.ID, AssignmentID: "previous", SlotID: SlotOwnerResponsibility, Occupant: actor.ID},
		{Sort: EffectAssignmentEnd, TaskID: target.ID, AssignmentID: "previous", SlotID: SlotOwnerResponsibility},
		{Sort: EffectAssignmentStart, TaskID: target.ID, AssignmentID: "parent", SlotID: SlotOwnerResponsibility, Occupant: actor.ID},
	}}); err != nil {
		t.Fatal(err)
	}
	fixture := retryMatrixFixture{actor: actor.ID, other: other.ID, authority: authority, genesisInput: genesisInput, genesisResult: genesisResult}
	fixture.input, fixture.result = dbosAllOperandOperation(t, fixture, target.ID)
	fixture.input.MutationDigest = []byte("fixed-caller-digest")
	adapter, err := NewDBOSAdapter(root, tracker, DBOSAdapterConfig{})
	if err != nil {
		t.Fatal(err)
	}
	callbacks := 0
	adapter.testHooks.afterDomainCommit = func() { callbacks++ }
	if err := dbos.Launch(root); err != nil {
		t.Fatal(err)
	}
	defer func() { root.Shutdown(5 * time.Second); _ = tracker.Close() }()
	want, err := adapter.Apply(context.Background(), fixture.input)
	if err != nil {
		t.Fatalf("initial exhaustive Apply: %v", err)
	}
	fixture.result = want
	if callbacks != 1 {
		t.Fatalf("initial callbacks=%d want 1", callbacks)
	}
	tables := []string{"workflow_status", "operation_outputs", "journal", "journal_operations", "journal_operation_result_slots", "journal_task_events", "journal_task_event_contexts", "journal_authorities", "journal_authority_bootstraps", "journal_authority_assignment_episodes", "journal_authority_assignment_transitions", "journal_decisions", "journal_evidence", "tasks", "edges", "labels", "comments"}
	before := snapshotSQLTables(t, db, tables...)
	candidates := retryMismatchCandidates(t, fixture)
	for name, candidate := range candidates {
		t.Run(name, func(t *testing.T) {
			candidate.MutationDigest = append([]byte(nil), fixture.input.MutationDigest...)
			callbacksBefore := callbacks
			_, err := adapter.Apply(context.Background(), candidate)
			if !errors.Is(err, ErrOperationConflict) {
				t.Fatalf("error=%v want typed ErrOperationConflict", err)
			}
			var conflict *OperationConflict
			if !errors.As(err, &conflict) {
				t.Fatalf("error lacks *OperationConflict: %v", err)
			}
			if callbacks != callbacksBefore {
				t.Fatalf("forbidden retry executed %d workflow callbacks", callbacks-callbacksBefore)
			}
			after := snapshotSQLTables(t, db, tables...)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("forbidden retry changed complete DBOS/domain tuples\nbefore=%#v\nafter=%#v", before, after)
			}
			looked, err := tracker.Journal().LookupCommitted(fixture.input.OperationID)
			if err != nil || !reflect.DeepEqual(looked, want) {
				t.Fatalf("complete committed result drifted: got=%#v err=%v want=%#v", looked, err, want)
			}
		})
	}
}

func dbosAllOperandOperation(t *testing.T, fixture retryMatrixFixture, target TaskID) (OperationInput, CommittedResult) {
	t.Helper()
	task := newCorpusTaskID()
	comment, _ := ParseCommentID("retry--018f0000-0000-7000-8000-000000000011")
	ctx, _ := TaskContext(target)
	title, description, notes := "updated", "updated description", "notes"
	priority, phase, at := PriorityHigh, PhaseCodeReview, int64(700)
	effects := []Effect{
		{Sort: EffectTaskCreate, ResultSlot: "create", RecordedAtOverride: &at, TaskID: task, Payload: []byte(`{"birth":1}`), Contexts: []EventContext{ctx}, Title: "title", Description: "description", Type: TaskTypeFeature, Priority: PriorityMedium, Phase: PhaseUnscoped},
		{Sort: EffectTaskEvent, ResultSlot: "update", RecordedAtOverride: &at, TaskID: task, EventKind: EventKindTaskUpdated, Payload: []byte(`{"update":1}`), Contexts: []EventContext{ctx}, UpdateTitle: &title, UpdateDescription: &description, UpdatePriority: &priority, UpdatePhase: &phase, UpdateNotes: &notes},
		{Sort: EffectTaskEvent, ResultSlot: "generic", RecordedAtOverride: &at, TaskID: task, EventKind: "retry.generic.one", Payload: []byte(`{"generic":1}`), Contexts: []EventContext{ctx}},
		{Sort: EffectAssignmentStart, ResultSlot: "assignment-start", RecordedAtOverride: &at, TaskID: task, AssignmentID: "retry-assignment", SlotID: SlotOwnerResponsibility, Occupant: fixture.actor, Predecessor: "previous", Parent: "parent"},
		{Sort: EffectAssignmentEnd, ResultSlot: "assignment-end", RecordedAtOverride: &at, TaskID: task, AssignmentID: "retry-assignment", SlotID: SlotOwnerResponsibility},
		{Sort: EffectDecision, ResultSlot: "decision", RecordedAtOverride: &at, TaskID: task, DecisionKind: "retry.decision", Payload: []byte(`{"decision":1}`)},
		{Sort: EffectEvidence, ResultSlot: "evidence", RecordedAtOverride: &at, TaskID: task, EvidenceKind: "retry.evidence", ContentDigest: []byte{1, 2, 3}, Payload: []byte(`{"evidence":1}`)},
		{Sort: EffectEdgeAdd, ResultSlot: "edge-add", RecordedAtOverride: &at, TaskID: task, EdgeTargetID: target.String(), EdgeRelKind: EdgeDerivedFrom, Contexts: []EventContext{ctx}},
		{Sort: EffectEdgeRemove, ResultSlot: "edge-remove", RecordedAtOverride: &at, TaskID: task, EdgeTargetID: target.String(), EdgeRelKind: EdgeDerivedFrom, Contexts: []EventContext{ctx}},
		{Sort: EffectLabelAdd, ResultSlot: "label-add", RecordedAtOverride: &at, TaskID: task, Label: "label", Contexts: []EventContext{ctx}},
		{Sort: EffectLabelRemove, ResultSlot: "label-remove", RecordedAtOverride: &at, TaskID: task, Label: "label", Contexts: []EventContext{ctx}},
		{Sort: EffectCommentAdd, ResultSlot: "comment", RecordedAtOverride: &at, TaskID: task, CommentIdentity: comment, CommentAuthor: fixture.actor, CommentBody: "body", Contexts: []EventContext{ctx}},
		{Sort: EffectTaskEvent, ResultSlot: "close", RecordedAtOverride: &at, TaskID: task, EventKind: EventKindTaskClosed, CloseReason: "done", Forced: true},
	}
	return OperationInput{OperationID: "dbos-exhaustive-operation", ActorID: fixture.actor, AuthorityJournalID: &fixture.authority, CommandDigest: []byte("command"), RecordedAt: 600, Effects: effects}, CommittedResult{}
}
