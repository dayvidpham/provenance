package provenance

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/testcorpus"
	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
)

//go:embed testdata/contract/dbos_retry_baseline.yaml
var dbosRetryYAML []byte

type dbosRetryInput struct {
	OperationID       string            `yaml:"operationID"`
	Command           string            `yaml:"command"`
	RecordedAt        int64             `yaml:"recordedAt"`
	EffectRecordedAt  int64             `yaml:"effectRecordedAt"`
	CommentID         string            `yaml:"commentID"`
	UpdateTitle       string            `yaml:"updateTitle"`
	UpdateDescription string            `yaml:"updateDescription"`
	UpdateNotes       string            `yaml:"updateNotes"`
	Effects           []dbosRetryEffect `yaml:"effects"`
}

type dbosRetryEffect struct {
	Family           dbosRetryFamily `yaml:"family"`
	ResultSlot       string          `yaml:"resultSlot"`
	TaskRef          string          `yaml:"taskRef"`
	Payload          string          `yaml:"payload"`
	ContextRef       string          `yaml:"contextRef"`
	Title            string          `yaml:"title"`
	Description      string          `yaml:"description"`
	EventKind        string          `yaml:"eventKind"`
	AssignmentID     string          `yaml:"assignmentID"`
	AssignmentSlot   string          `yaml:"assignmentSlot"`
	OccupantRef      string          `yaml:"occupantRef"`
	Predecessor      string          `yaml:"predecessor"`
	Parent           string          `yaml:"parent"`
	DecisionKind     string          `yaml:"decisionKind"`
	EvidenceKind     string          `yaml:"evidenceKind"`
	ContentDigestHex string          `yaml:"contentDigestHex"`
	EdgeTargetRef    string          `yaml:"edgeTargetRef"`
	EdgeKind         string          `yaml:"edgeKind"`
	Label            string          `yaml:"label"`
	CommentRef       string          `yaml:"commentRef"`
	CommentAuthorRef string          `yaml:"commentAuthorRef"`
	CommentBody      string          `yaml:"commentBody"`
	CloseReason      string          `yaml:"closeReason"`
	Forced           bool            `yaml:"forced"`
}

type dbosRetryExpected struct {
	EffectCount           int               `yaml:"effectCount"`
	MismatchOperatorCount int               `yaml:"mismatchOperatorCount"`
	Targets               []dbosRetryTarget `yaml:"targets"`
}

type dbosRetryMutationClass string

type dbosRetryFamily string

const (
	dbosRetryReplace dbosRetryMutationClass = "replace"
	dbosRetryRemove  dbosRetryMutationClass = "remove"
	dbosRetryReorder dbosRetryMutationClass = "reorder"

	dbosRetryFamilyOperation        dbosRetryFamily = "operation"
	dbosRetryFamilyTaskCreate       dbosRetryFamily = "task_create"
	dbosRetryFamilyTaskEventUpdate  dbosRetryFamily = "task_event_update"
	dbosRetryFamilyTaskEventGeneric dbosRetryFamily = "task_event_generic"
	dbosRetryFamilyAssignmentStart  dbosRetryFamily = "assignment_start"
	dbosRetryFamilyAssignmentEnd    dbosRetryFamily = "assignment_end"
	dbosRetryFamilyDecision         dbosRetryFamily = "decision"
	dbosRetryFamilyEvidence         dbosRetryFamily = "evidence"
	dbosRetryFamilyEdgeAdd          dbosRetryFamily = "edge_add"
	dbosRetryFamilyEdgeRemove       dbosRetryFamily = "edge_remove"
	dbosRetryFamilyLabelAdd         dbosRetryFamily = "label_add"
	dbosRetryFamilyLabelRemove      dbosRetryFamily = "label_remove"
	dbosRetryFamilyCommentAdd       dbosRetryFamily = "comment_add"
	dbosRetryFamilyTaskEventClose   dbosRetryFamily = "task_event_close"
)

type dbosRetryTarget struct {
	Operator      string                 `yaml:"operator"`
	Family        dbosRetryFamily        `yaml:"family"`
	EffectIndex   int                    `yaml:"effectIndex"`
	Path          string                 `yaml:"path"`
	MutationClass dbosRetryMutationClass `yaml:"mutationClass"`
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
	if err := validateDBOSRetryInput(c.Input); err != nil {
		t.Fatalf("DBOS retry input: %v", err)
	}
	wantFamilies := []dbosRetryFamily{dbosRetryFamilyTaskCreate, dbosRetryFamilyTaskEventUpdate, dbosRetryFamilyTaskEventGeneric, dbosRetryFamilyAssignmentStart, dbosRetryFamilyAssignmentEnd, dbosRetryFamilyDecision, dbosRetryFamilyEvidence, dbosRetryFamilyEdgeAdd, dbosRetryFamilyEdgeRemove, dbosRetryFamilyLabelAdd, dbosRetryFamilyLabelRemove, dbosRetryFamilyCommentAdd, dbosRetryFamilyTaskEventClose}
	gotFamilies := make([]dbosRetryFamily, len(c.Input.Effects))
	for i, effect := range c.Input.Effects {
		gotFamilies[i] = effect.Family
		if err := validateDBOSRetryEffect(effect); err != nil {
			t.Fatalf("DBOS retry effect %d: %v", i, err)
		}
	}
	if c.Name != "all-canonical-operands" || c.Mutation.Operator != "all-canonical-operands" || c.Classification != testcorpus.MustPass || c.Input.OperationID == "" || c.Input.Command == "" || c.Input.RecordedAt == 0 || c.Input.EffectRecordedAt == 0 || c.Input.CommentID == "" || c.Input.UpdateTitle == "" || c.Input.UpdateDescription == "" || c.Input.UpdateNotes == "" || !slices.Equal(gotFamilies, wantFamilies) || c.Expected.EffectCount != len(wantFamilies) || c.Expected.MismatchOperatorCount != 88 || len(c.Expected.Targets) != 88 {
		t.Fatalf("DBOS retry YAML is outside closed baseline membership: %#v", c)
	}
	seenTargets := make(map[dbosRetryTarget]struct{}, len(c.Expected.Targets))
	seenOperators := make(map[string]struct{}, len(c.Expected.Targets))
	for _, target := range c.Expected.Targets {
		if target.Operator == "" || target.Family == "" || target.Path == "" || (target.MutationClass != dbosRetryReplace && target.MutationClass != dbosRetryRemove && target.MutationClass != dbosRetryReorder) {
			t.Fatalf("retry target metadata is incomplete or untyped: %#v", target)
		}
		if _, duplicate := seenOperators[target.Operator]; duplicate {
			t.Fatalf("duplicate retry target operator %q", target.Operator)
		}
		if _, duplicate := seenTargets[target]; duplicate {
			t.Fatalf("duplicate retry target tuple %#v", target)
		}
		if target.EffectIndex == -1 {
			if target.Family != dbosRetryFamilyOperation {
				t.Fatalf("operation target has effect family %#v", target)
			}
		} else if target.EffectIndex < 0 || target.EffectIndex >= len(c.Input.Effects) || target.Family != c.Input.Effects[target.EffectIndex].Family {
			t.Fatalf("target family/index does not identify its baseline effect: %#v", target)
		}
		seenOperators[target.Operator] = struct{}{}
		seenTargets[target] = struct{}{}
	}
	return c
}

func validateDBOSRetryEffect(effect dbosRetryEffect) error {
	populated := map[string]bool{
		"resultSlot": effect.ResultSlot != "", "taskRef": effect.TaskRef != "", "payload": effect.Payload != "", "contextRef": effect.ContextRef != "",
		"title": effect.Title != "", "description": effect.Description != "", "eventKind": effect.EventKind != "", "assignmentID": effect.AssignmentID != "",
		"assignmentSlot": effect.AssignmentSlot != "", "occupantRef": effect.OccupantRef != "", "predecessor": effect.Predecessor != "", "parent": effect.Parent != "",
		"decisionKind": effect.DecisionKind != "", "evidenceKind": effect.EvidenceKind != "", "contentDigestHex": effect.ContentDigestHex != "",
		"edgeTargetRef": effect.EdgeTargetRef != "", "edgeKind": effect.EdgeKind != "", "label": effect.Label != "", "commentRef": effect.CommentRef != "",
		"commentAuthorRef": effect.CommentAuthorRef != "", "commentBody": effect.CommentBody != "", "closeReason": effect.CloseReason != "", "forced": effect.Forced,
	}
	common := []string{"resultSlot", "taskRef"}
	allowed := map[dbosRetryFamily][]string{
		"task_create":        {"payload", "contextRef", "title", "description"},
		"task_event_update":  {"payload", "contextRef", "eventKind"},
		"task_event_generic": {"payload", "contextRef", "eventKind"},
		"assignment_start":   {"assignmentID", "assignmentSlot", "occupantRef", "predecessor", "parent"},
		"assignment_end":     {"assignmentID", "assignmentSlot"},
		"decision":           {"payload", "decisionKind"}, "evidence": {"payload", "evidenceKind", "contentDigestHex"},
		"edge_add": {"contextRef", "edgeTargetRef", "edgeKind"}, "edge_remove": {"contextRef", "edgeTargetRef", "edgeKind"},
		"label_add": {"contextRef", "label"}, "label_remove": {"contextRef", "label"},
		"comment_add":      {"contextRef", "commentRef", "commentAuthorRef", "commentBody"},
		"task_event_close": {"eventKind", "closeReason", "forced"},
	}
	familyAllowed, ok := allowed[effect.Family]
	if !ok {
		return fmt.Errorf("unknown family %q", effect.Family)
	}
	accepted := map[string]bool{}
	for _, field := range append(common, familyAllowed...) {
		accepted[field] = true
	}
	for field, set := range populated {
		if set && !accepted[field] {
			return fmt.Errorf("family %q populates forbidden field %q", effect.Family, field)
		}
	}
	for field := range accepted {
		if !populated[field] {
			return fmt.Errorf("family %q omits required field %q", effect.Family, field)
		}
	}
	refs := map[string]map[string]bool{
		"taskRef": {"task": true}, "contextRef": {"task-context": true}, "occupantRef": {"actor": true},
		"edgeTargetRef": {"target": true}, "commentRef": {"comment": true}, "commentAuthorRef": {"actor": true},
	}
	for field, values := range refs {
		value := map[string]string{"taskRef": effect.TaskRef, "contextRef": effect.ContextRef, "occupantRef": effect.OccupantRef, "edgeTargetRef": effect.EdgeTargetRef, "commentRef": effect.CommentRef, "commentAuthorRef": effect.CommentAuthorRef}[field]
		if value != "" && !values[value] {
			return fmt.Errorf("family %q has unknown symbolic reference %s=%q", effect.Family, field, value)
		}
	}
	if effect.ContentDigestHex != "" {
		if _, err := hex.DecodeString(effect.ContentDigestHex); err != nil {
			return fmt.Errorf("family %q has invalid contentDigestHex: %w", effect.Family, err)
		}
	}
	return nil
}

func validateDBOSRetryInput(input dbosRetryInput) error {
	if input.OperationID == "" || input.Command == "" || input.RecordedAt == 0 || input.EffectRecordedAt == 0 || input.UpdateTitle == "" || input.UpdateDescription == "" || input.UpdateNotes == "" {
		return errors.New("required retry input scalar is empty")
	}
	if _, err := ParseCommentID(input.CommentID); err != nil {
		return fmt.Errorf("commentID: %w", err)
	}
	wantFamilies := []dbosRetryFamily{dbosRetryFamilyTaskCreate, dbosRetryFamilyTaskEventUpdate, dbosRetryFamilyTaskEventGeneric, dbosRetryFamilyAssignmentStart, dbosRetryFamilyAssignmentEnd, dbosRetryFamilyDecision, dbosRetryFamilyEvidence, dbosRetryFamilyEdgeAdd, dbosRetryFamilyEdgeRemove, dbosRetryFamilyLabelAdd, dbosRetryFamilyLabelRemove, dbosRetryFamilyCommentAdd, dbosRetryFamilyTaskEventClose}
	if len(input.Effects) != len(wantFamilies) {
		return fmt.Errorf("effect count=%d want %d", len(input.Effects), len(wantFamilies))
	}
	for i, effect := range input.Effects {
		if effect.Family != wantFamilies[i] {
			return fmt.Errorf("effect %d family=%q want %q", i, effect.Family, wantFamilies[i])
		}
		if err := validateDBOSRetryEffect(effect); err != nil {
			return fmt.Errorf("effect %d: %w", i, err)
		}
	}
	return nil
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
		slices.SortFunc(out[table], slices.Compare)
	}
	return out
}

var auditedDurableTables = []string{
	"activities", "actor_namespace_claims", "agent_kinds", "agents", "agents_human", "agents_ml", "agents_software",
	"application_versions", "assignment_slots", "assignment_transitions", "authority_kinds", "comments", "dbos_migrations",
	"edge_kinds", "edges", "event_dispatch_kv", "fixed_actor_manifest_entries", "journal", "journal_authorities",
	"journal_authority_assignment_episodes", "journal_authority_assignment_transitions", "journal_authority_bootstraps",
	"journal_activity_creations", "journal_decision_contexts", "journal_decisions", "journal_evidence", "journal_evidence_contexts", "journal_kinds", "journal_operation_result_slots", "journal_operations",
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
	t.Parallel()
	dbPath := t.TempDir() + "/exhaustive.db"
	db, err := openSharedSQL(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	root, err := dbos.NewDBOSContext(context.Background(), dbos.Config{AppName: "dbos-exhaustive", SqliteSystemDB: db, ApplicationVersion: "exhaustive-current"})
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
	return dbosAllOperandOperationFromFixture(t, fixture, target, loadDBOSRetryFixture(t))
}

func dbosAllOperandOperationFromFixture(t *testing.T, fixture retryMatrixFixture, target TaskID, baseline testcorpus.Case[dbosRetryInput, dbosRetryExpected]) (OperationInput, CommittedResult) {
	t.Helper()
	task, err := ParseTaskID("retry--018f0000-0000-7000-8000-000000000010")
	if err != nil {
		t.Fatal(err)
	}
	comment, err := ParseCommentID(baseline.Input.CommentID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := TaskContext(target)
	title, description, notes := baseline.Input.UpdateTitle, baseline.Input.UpdateDescription, baseline.Input.UpdateNotes
	priority, phase, at := PriorityHigh, PhaseCodeReview, baseline.Input.EffectRecordedAt
	resolveTask := func(ref string) TaskID {
		if ref == "task" {
			return task
		}
		t.Fatalf("unresolved task reference %q", ref)
		return TaskID{}
	}
	resolveActor := func(ref string) ActorID {
		if ref == "actor" {
			return fixture.actor
		}
		t.Fatalf("unresolved actor reference %q", ref)
		return ActorID{}
	}
	effects := make([]Effect, len(baseline.Input.Effects))
	for i, source := range baseline.Input.Effects {
		effect := Effect{ResultSlot: ResultSlotID(source.ResultSlot), RecordedAtOverride: &at, TaskID: resolveTask(source.TaskRef)}
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
			effect.Sort, effect.AssignmentID, effect.SlotID, effect.Occupant, effect.Predecessor, effect.Parent = EffectAssignmentStart, AssignmentID(source.AssignmentID), map[string]AssignmentSlotID{"owner-responsibility": SlotOwnerResponsibility}[source.AssignmentSlot], resolveActor(source.OccupantRef), AssignmentID(source.Predecessor), AssignmentID(source.Parent)
		case "assignment_end":
			effect.Sort, effect.AssignmentID, effect.SlotID = EffectAssignmentEnd, AssignmentID(source.AssignmentID), map[string]AssignmentSlotID{"owner-responsibility": SlotOwnerResponsibility}[source.AssignmentSlot]
		case "decision":
			effect.Sort, effect.DecisionKind = EffectDecision, DecisionKind(source.DecisionKind)
		case "evidence":
			digest, err := hex.DecodeString(source.ContentDigestHex)
			if err != nil {
				t.Fatalf("retry evidence digest: %v", err)
			}
			effect.Sort, effect.EvidenceKind, effect.ContentDigest = EffectEvidence, EvidenceKind(source.EvidenceKind), digest
		case "edge_add", "edge_remove":
			if source.EdgeTargetRef != "target" {
				t.Fatalf("unresolved edge target reference %q", source.EdgeTargetRef)
			}
			effect.EdgeTargetID, effect.EdgeRelKind = target.String(), map[string]EdgeKind{"derived_from": EdgeDerivedFrom}[source.EdgeKind]
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
			if source.CommentRef != "comment" {
				t.Fatalf("unresolved comment reference %q", source.CommentRef)
			}
			effect.Sort, effect.CommentIdentity, effect.CommentAuthor, effect.CommentBody = EffectCommentAdd, comment, resolveActor(source.CommentAuthorRef), source.CommentBody
		case "task_event_close":
			effect.Sort, effect.EventKind, effect.CloseReason, effect.Forced, effect.Contexts = EffectTaskEvent, EventKind(source.EventKind), source.CloseReason, source.Forced, nil
		default:
			t.Fatalf("unknown DBOS retry effect family %q", source.Family)
		}
		effects[i] = effect
	}
	return OperationInput{OperationID: OperationID(baseline.Input.OperationID), ActorID: fixture.actor, AuthorityJournalID: &fixture.authority, CommandDigest: []byte(baseline.Input.Command), RecordedAt: baseline.Input.RecordedAt, Effects: effects}, CommittedResult{}
}

func TestDBOSRetryYAMLValuesAreAuthoritative(t *testing.T) {
	t.Parallel()
	baseline := loadDBOSRetryFixture(t)
	fixture := retryMatrixFixture{actor: testActorID(t), authority: 7}
	target := newCorpusTaskID()
	before, _ := dbosAllOperandOperationFromFixture(t, fixture, target, baseline)
	candidates := retryMismatchCandidates(t, retryMatrixFixture{input: before, actor: fixture.actor, other: testActorID(t), authority: fixture.authority})

	for _, mutation := range retryTypedMutations(baseline.Input) {
		mutation := mutation
		t.Run("input/"+mutation.name, func(t *testing.T) {
			if err := validateDBOSRetryInput(mutation.value); err != nil {
				return
			}
			changed := baseline
			changed.Input = mutation.value
			after, _ := dbosAllOperandOperationFromFixture(t, fixture, target, changed)
			if reflect.DeepEqual(before, after) {
				t.Fatalf("accepted YAML input mutation %s did not alter the built operation", mutation.name)
			}
		})
	}
	for _, mutation := range retryTypedMutations(baseline.Expected) {
		mutation := mutation
		t.Run("expected/"+mutation.name, func(t *testing.T) {
			if err := validateRetryTargetBijection(before, candidates, mutation.value, baseline.Input.Effects); err == nil {
				t.Fatalf("YAML expected mutation %s remained accepted", mutation.name)
			}
		})
	}
}

func TestDBOSRetryOperatorsBijectIndependentExactTargets(t *testing.T) {
	baseline := loadDBOSRetryFixture(t)
	fixture := retryMatrixFixture{actor: testActorID(t), other: testActorID(t), authority: 7}
	base, _ := dbosAllOperandOperationFromFixture(t, fixture, newCorpusTaskID(), baseline)
	fixture.input = base
	candidates := retryMismatchCandidates(t, fixture)
	if err := validateRetryTargetBijection(base, candidates, baseline.Expected, baseline.Input.Effects); err != nil {
		t.Fatal(err)
	}
}

func validateRetryTargetBijection(base OperationInput, candidates map[string]OperationInput, expected dbosRetryExpected, effects []dbosRetryEffect) error {
	if expected.EffectCount != len(effects) || expected.MismatchOperatorCount != len(candidates) || len(expected.Targets) != len(candidates) {
		return fmt.Errorf("expected counts effects/operators/targets=%d/%d/%d, actual=%d/%d/%d", expected.EffectCount, expected.MismatchOperatorCount, len(expected.Targets), len(effects), len(candidates), len(candidates))
	}
	seenOperators := make(map[string]struct{}, len(candidates))
	type semanticTuple struct {
		family        dbosRetryFamily
		effectIndex   int
		path          string
		mutationClass dbosRetryMutationClass
	}
	seenTuples := make(map[semanticTuple]struct{}, len(candidates))
	for _, target := range expected.Targets {
		candidate, ok := candidates[target.Operator]
		if !ok {
			return fmt.Errorf("independent target %q has no operator", target.Operator)
		}
		if _, duplicate := seenOperators[target.Operator]; duplicate {
			return fmt.Errorf("operator %q maps more than once", target.Operator)
		}
		actual, err := retrySemanticTarget(target.Operator, base, candidate)
		if err != nil {
			return err
		}
		if actual != target {
			return fmt.Errorf("operator %q target=%#v want exact YAML tuple %#v", target.Operator, actual, target)
		}
		if candidate.OperationID != base.OperationID || !bytes.Equal(candidate.MutationDigest, base.MutationDigest) {
			return fmt.Errorf("operator %q changed protected operation/caller-digest identity", target.Operator)
		}
		tuple := semanticTuple{actual.Family, actual.EffectIndex, actual.Path, actual.MutationClass}
		if _, duplicate := seenTuples[tuple]; duplicate {
			return fmt.Errorf("semantic target tuple is not unique: %#v", actual)
		}
		seenOperators[target.Operator] = struct{}{}
		seenTuples[tuple] = struct{}{}
	}
	if len(seenOperators) != len(candidates) || len(seenTuples) != len(candidates) {
		return fmt.Errorf("operators are not fully bijected: operators=%d tuples=%d candidates=%d", len(seenOperators), len(seenTuples), len(candidates))
	}
	return nil
}

func retrySemanticTarget(operator string, base, candidate OperationInput) (dbosRetryTarget, error) {
	diffs := retrySemanticDiff(base, candidate)
	if len(diffs) != 1 {
		return dbosRetryTarget{}, fmt.Errorf("operator %q has %d semantic changes %v, want exactly one", operator, len(diffs), diffs)
	}
	target := dbosRetryTarget{Operator: operator, Family: dbosRetryFamilyOperation, EffectIndex: -1, Path: diffs[0], MutationClass: dbosRetryReplace}
	if target.Path == "Effects.order" {
		target.MutationClass = dbosRetryReorder
		return target, nil
	}
	if target.Path == "AuthorityJournalID" && candidate.AuthorityJournalID == nil {
		target.MutationClass = dbosRetryRemove
	}
	if strings.HasPrefix(target.Path, "Effects[") {
		close := strings.IndexByte(target.Path, ']')
		if close < 8 {
			return dbosRetryTarget{}, fmt.Errorf("operator %q produced malformed effect path %q", operator, target.Path)
		}
		index, err := strconv.Atoi(target.Path[len("Effects["):close])
		if err != nil || index < 0 || index >= len(base.Effects) {
			return dbosRetryTarget{}, fmt.Errorf("operator %q produced out-of-range effect path %q", operator, target.Path)
		}
		target.EffectIndex = index
		target.Family, err = retryFamilyFromEffect(base.Effects[index])
		if err != nil {
			return dbosRetryTarget{}, fmt.Errorf("operator %q: %w", operator, err)
		}
	}
	return target, nil
}

func retryFamilyFromEffect(effect Effect) (dbosRetryFamily, error) {
	switch effect.Sort {
	case EffectTaskCreate:
		return dbosRetryFamilyTaskCreate, nil
	case EffectTaskEvent:
		switch {
		case effect.EventKind == EventKindTaskClosed:
			return dbosRetryFamilyTaskEventClose, nil
		case effect.UpdateTitle != nil || effect.UpdateDescription != nil || effect.UpdatePriority != nil || effect.UpdatePhase != nil || effect.UpdateNotes != nil:
			return dbosRetryFamilyTaskEventUpdate, nil
		default:
			return dbosRetryFamilyTaskEventGeneric, nil
		}
	case EffectAssignmentStart:
		return dbosRetryFamilyAssignmentStart, nil
	case EffectAssignmentEnd:
		return dbosRetryFamilyAssignmentEnd, nil
	case EffectDecision:
		return dbosRetryFamilyDecision, nil
	case EffectEvidence:
		return dbosRetryFamilyEvidence, nil
	case EffectEdgeAdd:
		return dbosRetryFamilyEdgeAdd, nil
	case EffectEdgeRemove:
		return dbosRetryFamilyEdgeRemove, nil
	case EffectLabelAdd:
		return dbosRetryFamilyLabelAdd, nil
	case EffectLabelRemove:
		return dbosRetryFamilyLabelRemove, nil
	case EffectCommentAdd:
		return dbosRetryFamilyCommentAdd, nil
	default:
		return "", fmt.Errorf("effect sort %q has no retry-family mapping", effect.Sort)
	}
}

type retryTypedMutation[T any] struct {
	name  string
	value T
}

type retryValueStep struct {
	field int
	index int
	slice bool
}

func retryTypedMutations[T any](source T) []retryTypedMutation[T] {
	var out []retryTypedMutation[T]
	var walk func(reflect.Value, []retryValueStep, string)
	walk = func(value reflect.Value, path []retryValueStep, name string) {
		switch value.Kind() {
		case reflect.Struct:
			typ := value.Type()
			for i := 0; i < value.NumField(); i++ {
				walk(value.Field(i), append(path, retryValueStep{field: i}), joinRetryMutationPath(name, typ.Field(i).Name))
			}
		case reflect.Slice:
			for i := 0; i < value.Len(); i++ {
				copyValue := cloneRetryValue(reflect.ValueOf(source))
				slot := locateRetryValue(copyValue, path)
				slot.Set(reflect.AppendSlice(slot.Slice(0, i), slot.Slice(i+1, slot.Len())))
				out = append(out, retryTypedMutation[T]{name: fmt.Sprintf("%s.remove[%d]", name, i), value: copyValue.Interface().(T)})
				walk(value.Index(i), append(path, retryValueStep{index: i, slice: true}), fmt.Sprintf("%s[%d]", name, i))
			}
		case reflect.String, reflect.Int, reflect.Int64, reflect.Bool:
			copyValue := cloneRetryValue(reflect.ValueOf(source))
			slot := locateRetryValue(copyValue, path)
			switch slot.Kind() {
			case reflect.String:
				slot.SetString(slot.String() + "-mutated")
			case reflect.Int, reflect.Int64:
				slot.SetInt(slot.Int() + 1)
			case reflect.Bool:
				slot.SetBool(!slot.Bool())
			}
			out = append(out, retryTypedMutation[T]{name: name, value: copyValue.Interface().(T)})
		}
	}
	walk(reflect.ValueOf(source), nil, "")
	return out
}

func joinRetryMutationPath(prefix, field string) string {
	if prefix == "" {
		return field
	}
	return prefix + "." + field
}

func locateRetryValue(root reflect.Value, path []retryValueStep) reflect.Value {
	for _, step := range path {
		if step.slice {
			root = root.Index(step.index)
		} else {
			root = root.Field(step.field)
		}
	}
	return root
}

func cloneRetryValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Struct:
		copyValue := reflect.New(value.Type()).Elem()
		for i := 0; i < value.NumField(); i++ {
			copyValue.Field(i).Set(cloneRetryValue(value.Field(i)))
		}
		return copyValue
	case reflect.Slice:
		copyValue := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			copyValue.Index(i).Set(cloneRetryValue(value.Index(i)))
		}
		return copyValue
	default:
		return value
	}
}

func retrySemanticDiff(base, candidate OperationInput) []string {
	var diffs []string
	if base.ActorID != candidate.ActorID {
		diffs = append(diffs, "ActorID")
	}
	if !reflect.DeepEqual(base.AuthorityJournalID, candidate.AuthorityJournalID) {
		diffs = append(diffs, "AuthorityJournalID")
	}
	if !bytes.Equal(base.CommandDigest, candidate.CommandDigest) {
		diffs = append(diffs, "CommandDigest")
	}
	if base.RecordedAt != candidate.RecordedAt {
		diffs = append(diffs, "RecordedAt")
	}
	if len(base.Effects) != len(candidate.Effects) {
		return append(diffs, "Effects.length")
	}
	if len(base.Effects) > 6 && reflect.DeepEqual(base.Effects[5], candidate.Effects[6]) && reflect.DeepEqual(base.Effects[6], candidate.Effects[5]) {
		ordered := true
		for i := range base.Effects {
			if i != 5 && i != 6 && !reflect.DeepEqual(base.Effects[i], candidate.Effects[i]) {
				ordered = false
			}
		}
		if ordered {
			return append(diffs, "Effects.order")
		}
	}
	effectType := reflect.TypeOf(Effect{})
	for i := range base.Effects {
		left, right := reflect.ValueOf(base.Effects[i]), reflect.ValueOf(candidate.Effects[i])
		for field := 0; field < effectType.NumField(); field++ {
			if !reflect.DeepEqual(left.Field(field).Interface(), right.Field(field).Interface()) {
				diffs = append(diffs, fmt.Sprintf("Effects[%d].%s", i, effectType.Field(field).Name))
			}
		}
	}
	return diffs
}
