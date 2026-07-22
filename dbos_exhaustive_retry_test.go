package provenance

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/testcorpus"
	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
)

//go:embed testdata/contract/dbos_retry.yaml
var dbosRetryYAML []byte

type dbosRetryInput struct {
	OperationID       string            `yaml:"operationID"`
	Command           string            `yaml:"command"`
	RecordedAt        int64             `yaml:"recordedAt"`
	EffectRecordedAt  int64             `yaml:"effectRecordedAt"`
	TaskNamespace     string            `yaml:"taskNamespace"`
	CommentID         string            `yaml:"commentID"`
	UpdateTitle       string            `yaml:"updateTitle"`
	UpdateDescription string            `yaml:"updateDescription"`
	UpdateNotes       string            `yaml:"updateNotes"`
	Label             string            `yaml:"label"`
	CloseReason       string            `yaml:"closeReason"`
	Effects           []dbosRetryEffect `yaml:"effects"`
}

type dbosRetryEffect struct {
	Family           string `yaml:"family"`
	ResultSlot       string `yaml:"resultSlot"`
	TaskRef          string `yaml:"taskRef"`
	Payload          string `yaml:"payload"`
	ContextRef       string `yaml:"contextRef"`
	Title            string `yaml:"title"`
	Description      string `yaml:"description"`
	EventKind        string `yaml:"eventKind"`
	AssignmentID     string `yaml:"assignmentID"`
	AssignmentSlot   string `yaml:"assignmentSlot"`
	OccupantRef      string `yaml:"occupantRef"`
	Predecessor      string `yaml:"predecessor"`
	Parent           string `yaml:"parent"`
	DecisionKind     string `yaml:"decisionKind"`
	EvidenceKind     string `yaml:"evidenceKind"`
	ContentDigestHex string `yaml:"contentDigestHex"`
	EdgeTargetRef    string `yaml:"edgeTargetRef"`
	EdgeKind         string `yaml:"edgeKind"`
	Label            string `yaml:"label"`
	CommentRef       string `yaml:"commentRef"`
	CommentAuthorRef string `yaml:"commentAuthorRef"`
	CommentBody      string `yaml:"commentBody"`
	CloseReason      string `yaml:"closeReason"`
	Forced           bool   `yaml:"forced"`
}

type dbosRetryExpected struct {
	EffectCount           int `yaml:"effectCount"`
	MismatchOperatorCount int `yaml:"mismatchOperatorCount"`
}

func loadDBOSRetryFixture(t *testing.T) testcorpus.Case[dbosRetryInput, dbosRetryExpected] {
	t.Helper()
	corpus, err := testcorpus.LoadCorpus[dbosRetryInput, dbosRetryExpected](dbosRetryYAML)
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := corpus.CheckExact(1); err != nil {
		t.Fatal(err)
	}
	c := corpus.Cases[0]
	wantFamilies := []string{"task_create", "task_event_update", "task_event_generic", "assignment_start", "assignment_end", "decision", "evidence", "edge_add", "edge_remove", "label_add", "label_remove", "comment_add", "task_event_close"}
	gotFamilies := make([]string, len(c.Input.Effects))
	for i, effect := range c.Input.Effects {
		gotFamilies[i] = effect.Family
		for field, ref := range map[string]string{"taskRef": effect.TaskRef, "contextRef": effect.ContextRef, "occupantRef": effect.OccupantRef, "edgeTargetRef": effect.EdgeTargetRef, "commentRef": effect.CommentRef, "commentAuthorRef": effect.CommentAuthorRef} {
			if ref != "" && ref != "task" && ref != "target" && ref != "actor" && ref != "comment" && ref != "task-context" {
				t.Fatalf("DBOS retry effect %q has unknown symbolic runtime reference %s=%q", effect.Family, field, ref)
			}
		}
	}
	if c.Name != "all-canonical-operands" || c.Mutation.Operator != "all-canonical-operands" || c.Classification != testcorpus.MustPass || !slices.Equal(gotFamilies, wantFamilies) || c.Expected.EffectCount != len(wantFamilies) || c.Expected.MismatchOperatorCount != 88 {
		t.Fatalf("DBOS retry YAML is outside closed baseline membership: %#v", c)
	}
	return c
}

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

var auditedDurableTables = []string{
	"activities", "actor_namespace_claims", "agent_kinds", "agents", "agents_human", "agents_ml", "agents_software",
	"application_versions", "assignment_slots", "assignment_transitions", "authority_kinds", "comments", "dbos_migrations",
	"edge_kinds", "edges", "event_dispatch_kv", "fixed_actor_manifest_entries", "journal", "journal_authorities",
	"journal_authority_assignment_episodes", "journal_authority_assignment_transitions", "journal_authority_bootstraps",
	"journal_decisions", "journal_evidence", "journal_kinds", "journal_operation_result_slots", "journal_operations",
	"journal_task_event_contexts", "journal_task_events", "labels", "ml_models", "notifications", "operation_outputs", "phases",
	"priorities", "providers", "queues", "roles", "sqlite_sequence", "stages", "statuses", "streams", "task_attributions",
	"task_types", "tasks", "workflow_events", "workflow_events_history", "workflow_schedules", "workflow_status",
}

func auditedSnapshotTableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var actual []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		actual = append(actual, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := append([]string(nil), auditedDurableTables...)
	slices.Sort(want)
	if !slices.Equal(actual, want) {
		t.Fatalf("durable table inventory drifted; classify every new/removed mutable relation before updating the snapshot oracle\nactual=%v\nwant=%v", actual, want)
	}
	return actual
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
	workflowEntries := 0
	adapter.testHooks.onWorkflowEntry = func() { workflowEntries++ }
	if err := dbos.Launch(root); err != nil {
		t.Fatal(err)
	}
	defer func() { root.Shutdown(5 * time.Second); _ = tracker.Close() }()
	want, err := adapter.Apply(context.Background(), fixture.input)
	if err != nil {
		t.Fatalf("initial exhaustive Apply: %v", err)
	}
	fixture.result = want
	if workflowEntries != 1 {
		t.Fatalf("initial workflow entries=%d want 1", workflowEntries)
	}
	tables := auditedSnapshotTableNames(t, db)
	before := snapshotSQLTables(t, db, tables...)
	candidates := retryMismatchCandidates(t, fixture)
	for name, candidate := range candidates {
		t.Run(name, func(t *testing.T) {
			candidate.MutationDigest = append([]byte(nil), fixture.input.MutationDigest...)
			entriesBefore := workflowEntries
			_, err := adapter.Apply(context.Background(), candidate)
			if !errors.Is(err, ErrOperationConflict) {
				t.Fatalf("error=%v want typed ErrOperationConflict", err)
			}
			var conflict *OperationConflict
			if !errors.As(err, &conflict) {
				t.Fatalf("error lacks *OperationConflict: %v", err)
			}
			if workflowEntries != entriesBefore {
				t.Fatalf("forbidden retry entered %d workflow callbacks", workflowEntries-entriesBefore)
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
	baseline := loadDBOSRetryFixture(t)
	task := newCorpusTaskID()
	comment, _ := ParseCommentID(baseline.Input.CommentID)
	ctx, _ := TaskContext(target)
	title, description, notes := baseline.Input.UpdateTitle, baseline.Input.UpdateDescription, baseline.Input.UpdateNotes
	priority, phase, at := PriorityHigh, PhaseCodeReview, baseline.Input.EffectRecordedAt
	effects := make([]Effect, len(baseline.Input.Effects))
	for i, source := range baseline.Input.Effects {
		effect := Effect{ResultSlot: ResultSlotID(source.ResultSlot), RecordedAtOverride: &at, TaskID: task}
		if source.Payload != "" {
			effect.Payload = []byte(source.Payload)
		}
		if source.ContextRef != "" {
			effect.Contexts = []EventContext{ctx}
		}
		switch source.Family {
		case "task_create":
			effect.Sort, effect.Title, effect.Description, effect.Type, effect.Priority, effect.Phase = EffectTaskCreate, source.Title, source.Description, TaskTypeFeature, PriorityMedium, PhaseUnscoped
		case "task_event_update":
			effect.Sort, effect.EventKind, effect.UpdateTitle, effect.UpdateDescription, effect.UpdatePriority, effect.UpdatePhase, effect.UpdateNotes = EffectTaskEvent, EventKind(source.EventKind), &title, &description, &priority, &phase, &notes
		case "task_event_generic":
			effect.Sort, effect.EventKind = EffectTaskEvent, EventKind(source.EventKind)
		case "assignment_start":
			effect.Sort, effect.AssignmentID, effect.SlotID, effect.Occupant, effect.Predecessor, effect.Parent = EffectAssignmentStart, AssignmentID(source.AssignmentID), SlotOwnerResponsibility, fixture.actor, AssignmentID(source.Predecessor), AssignmentID(source.Parent)
		case "assignment_end":
			effect.Sort, effect.AssignmentID, effect.SlotID = EffectAssignmentEnd, AssignmentID(source.AssignmentID), SlotOwnerResponsibility
		case "decision":
			effect.Sort, effect.DecisionKind = EffectDecision, DecisionKind(source.DecisionKind)
		case "evidence":
			digest, err := hex.DecodeString(source.ContentDigestHex)
			if err != nil {
				t.Fatalf("retry evidence digest: %v", err)
			}
			effect.Sort, effect.EvidenceKind, effect.ContentDigest = EffectEvidence, EvidenceKind(source.EvidenceKind), digest
		case "edge_add", "edge_remove":
			effect.EdgeTargetID, effect.EdgeRelKind = target.String(), EdgeDerivedFrom
			if source.Family == "edge_add" {
				effect.Sort = EffectEdgeAdd
			} else {
				effect.Sort = EffectEdgeRemove
			}
		case "label_add", "label_remove":
			effect.Label = source.Label
			if source.Family == "label_add" {
				effect.Sort = EffectLabelAdd
			} else {
				effect.Sort = EffectLabelRemove
			}
		case "comment_add":
			effect.Sort, effect.CommentIdentity, effect.CommentAuthor, effect.CommentBody = EffectCommentAdd, comment, fixture.actor, source.CommentBody
		case "task_event_close":
			effect.Sort, effect.EventKind, effect.CloseReason, effect.Forced, effect.Contexts = EffectTaskEvent, EventKind(source.EventKind), source.CloseReason, source.Forced, nil
		default:
			t.Fatalf("unknown DBOS retry effect family %q", source.Family)
		}
		effects[i] = effect
	}
	return OperationInput{OperationID: OperationID(baseline.Input.OperationID), ActorID: fixture.actor, AuthorityJournalID: &fixture.authority, CommandDigest: []byte(baseline.Input.Command), RecordedAt: baseline.Input.RecordedAt, Effects: effects}, CommittedResult{}
}
