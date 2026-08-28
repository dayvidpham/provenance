package provenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
)

type dbosFieldMutation struct {
	field   string
	goField string
	change  func(*OperationInput)
}

type dbosFamilyBaseline struct {
	name       string
	sort       EffectSort
	input      OperationInput
	mutations  []dbosFieldMutation
	allocation bool
}

type dbosFamilyEnv struct {
	db              *sql.DB
	root            dbos.Context
	tracker         Tracker
	adapter         *DBOSAdapter
	actor           ActorID
	other           ActorID
	authority       JournalID
	workflowEntries int
}

var dbosCanonicalFieldInventory = map[EffectSort][]string{
	EffectTaskEvent:           {"ResultSlot", "RecordedAtOverride", "TaskID", "EventKind", "Payload", "Contexts.count", "Contexts.kind", "Contexts.identity", "CloseReason", "UpdateTitle", "UpdateDescription", "UpdatePriority", "UpdatePhase", "UpdateNotes", "Forced"},
	EffectBootstrapAuthority:  {"ResultSlot", "RecordedAtOverride", "BootstrapLabel", "OperationAuthorityID"},
	EffectAssignmentStart:     {"ResultSlot", "RecordedAtOverride", "TaskID", "AssignmentID", "SlotID", "Occupant", "Predecessor", "Parent"},
	EffectAssignmentEnd:       {"ResultSlot", "RecordedAtOverride", "TaskID", "AssignmentID", "SlotID"},
	EffectDecision:            {"ResultSlot", "RecordedAtOverride", "TaskID", "DecisionKind", "Payload"},
	EffectEvidence:            {"ResultSlot", "RecordedAtOverride", "TaskID", "EvidenceKind", "ContentDigest", "Payload"},
	EffectTaskCreate:          {"ResultSlot", "RecordedAtOverride", "TaskID", "Payload", "Contexts.count", "Contexts.kind", "Contexts.identity", "Title", "Description", "Type", "Priority", "Phase"},
	EffectEdgeAdd:             {"ResultSlot", "RecordedAtOverride", "TaskID", "EdgeTargetID", "EdgeRelKind", "Contexts.count", "Contexts.kind", "Contexts.identity"},
	EffectEdgeRemove:          {"ResultSlot", "RecordedAtOverride", "TaskID", "EdgeTargetID", "EdgeRelKind", "Contexts.count", "Contexts.kind", "Contexts.identity"},
	EffectLabelAdd:            {"ResultSlot", "RecordedAtOverride", "TaskID", "Label", "Contexts.count", "Contexts.kind", "Contexts.identity"},
	EffectLabelRemove:         {"ResultSlot", "RecordedAtOverride", "TaskID", "Label", "Contexts.count", "Contexts.kind", "Contexts.identity"},
	EffectCommentAdd:          {"ResultSlot", "RecordedAtOverride", "TaskID", "CommentIdentity", "CommentAuthor", "CommentBody", "Contexts.count", "Contexts.kind", "Contexts.identity"},
	EffectTaskCreateAllocated: {"ResultSlot", "RecordedAtOverride", "TaskID", "Payload", "Contexts.count", "Contexts.kind", "Contexts.identity", "Title", "Description", "Type", "Priority", "Phase"},
	// ActivityCreate: ResultSlot is required (proof-of-allocation). SQLite fold in .1.2.
	EffectActivityCreate: {"ResultSlot", "RecordedAtOverride", "ActivityID", "ActivityAgentID", "ActivityPhase", "ActivityStage", "ActivityNotes"},
}

func TestDBOSCanonicalFieldInventoryIsClosed(t *testing.T) {
	if len(dbosCanonicalFieldInventory) != 14 {
		t.Fatalf("family inventory has %d sorts, want closed set of 14", len(dbosCanonicalFieldInventory))
	}
	covered := map[string]bool{}
	for _, fields := range dbosCanonicalFieldInventory {
		for _, field := range fields {
			if dot := strings.IndexByte(field, '.'); dot >= 0 {
				field = field[:dot]
			}
			covered[field] = true
		}
	}
	typ := reflect.TypeOf(Effect{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i).Name
		if field == "Sort" || field == "ActorID" {
			continue
		}
		if !covered[field] {
			t.Fatalf("canonical Effect field %s has no DBOS completed-retry inventory entry", field)
		}
	}
}

func TestDBOSResultSlotTransportInventoryIsClosed(t *testing.T) {
	bindings := []ResultSlotBinding{
		{Slot: "task", ProducedJournalID: 1, Kind: JournalKindTaskEvent, TaskID: &TaskID{Namespace: "inventory", UUID: uuid.Must(uuid.NewV7())}},
		// Activity slot (JournalKindActivity) reserved for later vertical
		{Slot: "authority", ProducedJournalID: 3, Kind: JournalKindAuthority},
		{Slot: "decision", ProducedJournalID: 4, Kind: JournalKindDecision},
		{Slot: "evidence", ProducedJournalID: 5, Kind: JournalKindEvidence},
	}
	for _, binding := range bindings {
		wire, err := canonicalResultSlotFromBinding(binding)
		if err != nil {
			t.Fatalf("encode %s slot: %v", binding.Kind, err)
		}
		got, err := resultSlotBindingFromCanonical(wire)
		if err != nil || !reflect.DeepEqual(got, binding) {
			t.Fatalf("round-trip %s slot = %#v, %v; want %#v", binding.Kind, got, err, binding)
		}
	}
}

func runDBOSFamilyBaseline(t *testing.T, env *dbosFamilyEnv, baseline dbosFamilyBaseline, tables []string) {
	t.Helper()
	entriesBeforeBaseline := env.workflowEntries
	want, err := env.adapter.Apply(context.Background(), baseline.input)
	if err != nil {
		t.Fatalf("apply valid %s baseline: %v", baseline.name, err)
	}
	if env.workflowEntries-entriesBeforeBaseline != 1 {
		t.Fatalf("valid %s baseline workflow entries=%d, want exactly 1", baseline.name, env.workflowEntries-entriesBeforeBaseline)
	}
	beforeEntries := env.workflowEntries
	before := snapshotSQLTables(t, env.db, tables...)
	wantFields := append([]string(nil), dbosCanonicalFieldInventory[baseline.sort]...)
	gotFields := make([]string, 0, len(baseline.mutations))
	for _, mutation := range baseline.mutations {
		gotFields = append(gotFields, mutation.field)
	}
	sort.Strings(wantFields)
	sort.Strings(gotFields)
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Fatalf("%s mutator inventory=%v want closed fields=%v", baseline.name, gotFields, wantFields)
	}
	for _, mutation := range baseline.mutations {
		t.Run(baseline.name+"/"+mutation.field, func(t *testing.T) {
			candidate := baseline.input
			candidate.Effects = cloneRetryEffects(baseline.input.Effects)
			mutation.change(&candidate)
			if candidate.OperationID != baseline.input.OperationID || !reflect.DeepEqual(candidate.MutationDigest, baseline.input.MutationDigest) {
				t.Fatal("mutator changed OperationID or caller MutationDigest")
			}
			diffs := changedEffectFields(baseline.input.Effects, candidate.Effects)
			wantDiff := fmt.Sprintf("effect.%d.%s", changedEffectIndex(baseline.input.Effects, candidate.Effects), mutation.goField)
			if len(diffs) != 1 || diffs[0] != wantDiff {
				t.Fatalf("mutator changed fields %v, want exactly %s", diffs, wantDiff)
			}
			if _, err := Canonicalize(OperationInput{Effects: candidate.Effects}); err != nil {
				t.Fatalf("single-field candidate is not canonical: %v", err)
			}
			_, err := env.adapter.Apply(context.Background(), candidate)
			if !errors.Is(err, ErrOperationConflict) {
				t.Fatalf("error=%v want ErrOperationConflict", err)
			}
			var conflict *OperationConflict
			if !errors.As(err, &conflict) {
				t.Fatalf("error lacks *OperationConflict: %v", err)
			}
			if env.workflowEntries != beforeEntries {
				t.Fatalf("forbidden field retry entered workflow callback: %d -> %d", beforeEntries, env.workflowEntries)
			}
			if after := snapshotSQLTables(t, env.db, tables...); !reflect.DeepEqual(after, before) {
				t.Fatalf("forbidden field retry changed DBOS/journal/domain snapshot")
			}
			looked, lookupErr := env.tracker.Journal().LookupCommitted(baseline.input.OperationID)
			if lookupErr != nil || !reflect.DeepEqual(looked, want) {
				t.Fatalf("committed result drifted: got=%#v err=%v want=%#v", looked, lookupErr, want)
			}
		})
	}
	if baseline.allocation {
		candidate := baseline.input
		candidate.Effects = cloneRetryEffects(baseline.input.Effects)
		candidate.Effects[0].TaskID.UUID = uuid.Must(uuid.NewV7())
		got, err := env.adapter.Apply(context.Background(), candidate)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("allocation UUID-only retry got=%#v err=%v want=%#v", got, err, want)
		}
		if env.workflowEntries != beforeEntries {
			t.Fatalf("allocation UUID-only retry entered workflow callback: %d -> %d", beforeEntries, env.workflowEntries)
		}
		if after := snapshotSQLTables(t, env.db, tables...); !reflect.DeepEqual(after, before) {
			for table := range before {
				if !reflect.DeepEqual(before[table], after[table]) {
					t.Errorf("allocation UUID-only retry changed %s\nbefore=%#v\nafter=%#v", table, before[table], after[table])
				}
			}
			t.FailNow()
		}
	}
}

func changedEffectIndex(before, after []Effect) int {
	for i := range before {
		if !reflect.DeepEqual(before[i], after[i]) {
			return i
		}
	}
	return -1
}

func changedEffectFields(before, after []Effect) []string {
	var out []string
	typ := reflect.TypeOf(Effect{})
	for i := range before {
		left, right := reflect.ValueOf(before[i]), reflect.ValueOf(after[i])
		for field := 0; field < typ.NumField(); field++ {
			if !reflect.DeepEqual(left.Field(field).Interface(), right.Field(field).Interface()) {
				out = append(out, fmt.Sprintf("effect.%d.%s", i, typ.Field(field).Name))
			}
		}
	}
	return out
}

func newDBOSFamilyEnv(t *testing.T, name string, withGenesis bool) *dbosFamilyEnv {
	t.Helper()
	db, err := openSharedSQL(t.TempDir() + "/family.db")
	if err != nil {
		t.Fatal(err)
	}
	root, err := dbos.NewContext(context.Background(), dbos.Config{AppName: name, SQLiteSystemDB: db, ApplicationVersion: "family-current"})
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := tracker.RegisterSoftwareAgent("family", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := tracker.RegisterSoftwareAgent("family", "other", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	var authority JournalID
	if withGenesis {
		result, err := tracker.Journal().Apply(OperationInput{OperationID: OperationID(name + "-genesis"), ActorID: actor.ID, CommandDigest: []byte("genesis"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority", BootstrapLabel: "root", OperationAuthorityID: "root"}}})
		if err != nil {
			t.Fatal(err)
		}
		authority, _ = slotJournalID(result, "authority")
	}
	adapter, err := NewDBOSAdapter(root, tracker, DBOSAdapterConfig{})
	if err != nil {
		t.Fatal(err)
	}
	env := &dbosFamilyEnv{db: db, root: root, tracker: tracker, adapter: adapter, actor: actor.ID, other: other.ID, authority: authority}
	adapter.testHooks.onWorkflowEntry = func() { env.workflowEntries++ }
	t.Cleanup(func() { shutdownDBOSRoot(t, root, 5*time.Second); _ = tracker.Close(); _ = db.Close() })
	return env
}

func launchDBOSFamilyEnv(t *testing.T, env *dbosFamilyEnv) {
	t.Helper()
	if err := dbos.Launch(env.root); err != nil {
		t.Fatal(err)
	}
}

func fieldChange(field string, effect int, change func(*Effect)) dbosFieldMutation {
	goField := field
	if dot := strings.IndexByte(goField, '.'); dot >= 0 {
		goField = goField[:dot]
	}
	return dbosFieldMutation{field: field, goField: goField, change: func(in *OperationInput) { change(&in.Effects[effect]) }}
}

func commonFieldChanges(effect int) []dbosFieldMutation {
	return []dbosFieldMutation{
		fieldChange("ResultSlot", effect, func(e *Effect) { e.ResultSlot += "-changed" }),
		fieldChange("RecordedAtOverride", effect, func(e *Effect) { value := *e.RecordedAtOverride + 1; e.RecordedAtOverride = &value }),
	}
}

func contextFieldChanges(effect int, sameIdentityKind, alternateIdentity EventContext) []dbosFieldMutation {
	return []dbosFieldMutation{
		fieldChange("Contexts.count", effect, func(e *Effect) { e.Contexts = append([]EventContext(nil), e.Contexts[:1]...) }),
		fieldChange("Contexts.kind", effect, func(e *Effect) {
			e.Contexts = append([]EventContext(nil), e.Contexts...)
			e.Contexts[0] = sameIdentityKind
		}),
		fieldChange("Contexts.identity", effect, func(e *Effect) {
			e.Contexts = append([]EventContext(nil), e.Contexts...)
			e.Contexts[0] = alternateIdentity
		}),
	}
}

func familyOperation(env *dbosFamilyEnv, name string, effects ...Effect) OperationInput {
	authority := env.authority
	return OperationInput{OperationID: OperationID("family-" + name), ActorID: env.actor, AuthorityJournalID: &authority, CommandDigest: []byte("command"), MutationDigest: []byte("fixed-caller-digest"), RecordedAt: 600, Effects: effects}
}

func TestDBOSCompletedRetryUsesOneValidBaselineAndOneFieldChangePerFamily(t *testing.T) {
	t.Parallel()
	bootstrapEnv := newDBOSFamilyEnv(t, "family-bootstrap", false)
	launchDBOSFamilyEnv(t, bootstrapEnv)
	at := int64(700)
	bootstrap := dbosFamilyBaseline{name: "bootstrap_authority", sort: EffectBootstrapAuthority, input: OperationInput{OperationID: "family-bootstrap", ActorID: bootstrapEnv.actor, CommandDigest: []byte("command"), MutationDigest: []byte("fixed-caller-digest"), RecordedAt: 600, Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority", RecordedAtOverride: &at, BootstrapLabel: "root", OperationAuthorityID: "authority-id"}}}}
	bootstrap.mutations = append(commonFieldChanges(0),
		fieldChange("BootstrapLabel", 0, func(e *Effect) { e.BootstrapLabel = "changed-root" }),
		fieldChange("OperationAuthorityID", 0, func(e *Effect) { e.OperationAuthorityID = "changed-authority" }),
	)
	runDBOSFamilyBaseline(t, bootstrapEnv, bootstrap, auditedSnapshotTableNames(t, bootstrapEnv.db))

	env := newDBOSFamilyEnv(t, "family-main", true)
	session := env.tracker.As(env.actor, env.authority)
	target, err := session.Create("family", "target", "", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID("family-target"))
	if err != nil {
		t.Fatal(err)
	}
	alternate, err := session.Create("family", "alternate", "", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID("family-alternate"))
	if err != nil {
		t.Fatal(err)
	}
	eventTask, err := session.Create("family", "event", "", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID("family-event-target"))
	if err != nil {
		t.Fatal(err)
	}
	assignmentTask, err := session.Create("family", "assignment", "", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID("family-assignment-target"))
	if err != nil {
		t.Fatal(err)
	}
	assignmentEndTask, err := session.Create("family", "assignment-end", "", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID("family-assignment-end-target"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tracker.Journal().Apply(familyOperation(env, "assignment-setup",
		Effect{Sort: EffectAssignmentStart, TaskID: assignmentTask.ID, AssignmentID: "previous", SlotID: SlotOwnerResponsibility, Occupant: env.actor},
		Effect{Sort: EffectAssignmentEnd, TaskID: assignmentTask.ID, AssignmentID: "previous", SlotID: SlotOwnerResponsibility},
		Effect{Sort: EffectAssignmentStart, TaskID: target.ID, AssignmentID: "parent", SlotID: SlotOwnerResponsibility, Occupant: env.actor},
		Effect{Sort: EffectAssignmentStart, TaskID: assignmentEndTask.ID, AssignmentID: "ending", SlotID: SlotOwnerResponsibility, Occupant: env.actor},
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.tracker.Journal().Apply(familyOperation(env, "remove-setup",
		Effect{Sort: EffectEdgeAdd, TaskID: target.ID, EdgeTargetID: alternate.ID.String(), EdgeRelKind: EdgeDerivedFrom},
		Effect{Sort: EffectLabelAdd, TaskID: target.ID, Label: "remove-me"},
	)); err != nil {
		t.Fatal(err)
	}
	launchDBOSFamilyEnv(t, env)
	taskContext, _ := TaskContext(target.ID)
	actorContext, _ := ActorContext(env.actor)
	contexts := []EventContext{taskContext, actorContext}
	sameIdentityActor, _ := ActorContext(ActorID{Namespace: target.ID.Namespace, UUID: target.ID.UUID})
	alternateContext, _ := TaskContext(alternate.ID)
	contextChanges := func(effect int) []dbosFieldMutation {
		return contextFieldChanges(effect, sameIdentityActor, alternateContext)
	}
	taskChange := func(effect int) dbosFieldMutation {
		return fieldChange("TaskID", effect, func(e *Effect) { e.TaskID = alternate.ID })
	}

	makeCreate := func(sort EffectSort, name string) dbosFamilyBaseline {
		taskID := newCorpusTaskID()
		base := dbosFamilyBaseline{name: name, sort: sort, allocation: sort == EffectTaskCreateAllocated, input: familyOperation(env, name, Effect{Sort: sort, ResultSlot: "task", RecordedAtOverride: &at, TaskID: taskID, Payload: []byte(`{"created":true}`), Contexts: contexts, Title: "title", Description: "description", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped})}
		base.mutations = append(commonFieldChanges(0), taskChange(0),
			fieldChange("Payload", 0, func(e *Effect) { e.Payload = []byte(`{"created":false}`) }),
		)
		base.mutations = append(base.mutations, contextChanges(0)...)
		base.mutations = append(base.mutations,
			fieldChange("Title", 0, func(e *Effect) { e.Title = "changed" }),
			fieldChange("Description", 0, func(e *Effect) { e.Description = "changed" }),
			fieldChange("Type", 0, func(e *Effect) { e.Type = TaskTypeFeature }),
			fieldChange("Priority", 0, func(e *Effect) { e.Priority = PriorityHigh }),
			fieldChange("Phase", 0, func(e *Effect) { e.Phase = PhaseCodeReview }),
		)
		if sort == EffectTaskCreateAllocated {
			// UUID-only is accepted separately; TaskID's forbidden mutator isolates namespace.
			base.mutations[2] = fieldChange("TaskID", 0, func(e *Effect) { e.TaskID.Namespace = "changed" })
		}
		return base
	}

	title, description, notes := "updated", "updated description", "notes"
	priority, phase := PriorityHigh, PhaseCodeReview
	event := dbosFamilyBaseline{name: "task_event", sort: EffectTaskEvent, input: familyOperation(env, "task-event",
		Effect{Sort: EffectTaskEvent, ResultSlot: "update", RecordedAtOverride: &at, TaskID: eventTask.ID, EventKind: EventKindTaskUpdated, Payload: []byte(`{"update":1}`), Contexts: contexts, UpdateTitle: &title, UpdateDescription: &description, UpdatePriority: &priority, UpdatePhase: &phase, UpdateNotes: &notes},
		Effect{Sort: EffectTaskEvent, TaskID: eventTask.ID, EventKind: "family.generic", Payload: []byte(`{"generic":1}`)},
		Effect{Sort: EffectTaskEvent, TaskID: eventTask.ID, EventKind: EventKindTaskClosed, Forced: true, CloseReason: "done"},
	)}
	event.mutations = append(commonFieldChanges(0), taskChange(0),
		fieldChange("EventKind", 1, func(e *Effect) { e.EventKind = "family.changed" }),
		fieldChange("Payload", 0, func(e *Effect) { e.Payload = []byte(`{"update":2}`) }),
	)
	event.mutations = append(event.mutations, contextChanges(0)...)
	event.mutations = append(event.mutations,
		fieldChange("CloseReason", 2, func(e *Effect) { e.CloseReason = "changed" }),
		fieldChange("UpdateTitle", 0, func(e *Effect) { value := "changed"; e.UpdateTitle = &value }),
		fieldChange("UpdateDescription", 0, func(e *Effect) { value := "changed"; e.UpdateDescription = &value }),
		fieldChange("UpdatePriority", 0, func(e *Effect) { value := PriorityCritical; e.UpdatePriority = &value }),
		fieldChange("UpdatePhase", 0, func(e *Effect) { value := PhaseImplPlan; e.UpdatePhase = &value }),
		fieldChange("UpdateNotes", 0, func(e *Effect) { value := "changed"; e.UpdateNotes = &value }),
		fieldChange("Forced", 2, func(e *Effect) { e.Forced = false }),
	)

	assignmentStart := dbosFamilyBaseline{name: "assignment_start", sort: EffectAssignmentStart, input: familyOperation(env, "assignment-start", Effect{Sort: EffectAssignmentStart, ResultSlot: "start", RecordedAtOverride: &at, TaskID: assignmentTask.ID, AssignmentID: "new-assignment", SlotID: SlotOwnerResponsibility, Occupant: env.actor, Predecessor: "previous", Parent: "parent"})}
	assignmentStart.mutations = append(commonFieldChanges(0), taskChange(0),
		fieldChange("AssignmentID", 0, func(e *Effect) { e.AssignmentID = "changed" }),
		fieldChange("SlotID", 0, func(e *Effect) { e.SlotID = "" }),
		fieldChange("Occupant", 0, func(e *Effect) { e.Occupant = env.other }),
		fieldChange("Predecessor", 0, func(e *Effect) { e.Predecessor = "changed" }),
		fieldChange("Parent", 0, func(e *Effect) { e.Parent = "changed" }),
	)
	assignmentEnd := dbosFamilyBaseline{name: "assignment_end", sort: EffectAssignmentEnd, input: familyOperation(env, "assignment-end", Effect{Sort: EffectAssignmentEnd, ResultSlot: "end", RecordedAtOverride: &at, TaskID: assignmentEndTask.ID, AssignmentID: "ending", SlotID: SlotOwnerResponsibility})}
	assignmentEnd.mutations = append(commonFieldChanges(0), taskChange(0),
		fieldChange("AssignmentID", 0, func(e *Effect) { e.AssignmentID = "changed" }),
		fieldChange("SlotID", 0, func(e *Effect) { e.SlotID = "" }),
	)

	simpleContexts := func(name string, sort EffectSort, effect Effect, extras ...dbosFieldMutation) dbosFamilyBaseline {
		base := dbosFamilyBaseline{name: name, sort: sort, input: familyOperation(env, name, effect)}
		base.mutations = append(commonFieldChanges(0), taskChange(0))
		base.mutations = append(base.mutations, extras...)
		if effect.Contexts != nil {
			base.mutations = append(base.mutations, contextChanges(0)...)
		}
		return base
	}
	comment, _ := ParseCommentID("family--018f0000-0000-7000-8000-000000000031")
	altComment, _ := ParseCommentID("family--018f0000-0000-7000-8000-000000000032")
	families := []dbosFamilyBaseline{
		makeCreate(EffectTaskCreate, "task_create"),
		makeCreate(EffectTaskCreateAllocated, "task_create_allocated"),
		event,
		assignmentStart,
		assignmentEnd,
		simpleContexts("decision", EffectDecision, Effect{Sort: EffectDecision, ResultSlot: "decision", RecordedAtOverride: &at, TaskID: target.ID, DecisionKind: "family.decision", Payload: []byte(`{"x":1}`)}, fieldChange("DecisionKind", 0, func(e *Effect) { e.DecisionKind = "family.changed" }), fieldChange("Payload", 0, func(e *Effect) { e.Payload = []byte(`{"x":2}`) })),
		simpleContexts("evidence", EffectEvidence, Effect{Sort: EffectEvidence, ResultSlot: "evidence", RecordedAtOverride: &at, TaskID: target.ID, EvidenceKind: "family.evidence", ContentDigest: []byte{1, 2}, Payload: []byte(`{"x":1}`)}, fieldChange("EvidenceKind", 0, func(e *Effect) { e.EvidenceKind = "family.changed" }), fieldChange("ContentDigest", 0, func(e *Effect) { e.ContentDigest = []byte{3, 4} }), fieldChange("Payload", 0, func(e *Effect) { e.Payload = []byte(`{"x":2}`) })),
		simpleContexts("edge_add", EffectEdgeAdd, Effect{Sort: EffectEdgeAdd, ResultSlot: "edge", RecordedAtOverride: &at, TaskID: target.ID, EdgeTargetID: "new-target", EdgeRelKind: EdgeDerivedFrom, Contexts: contexts}, fieldChange("EdgeTargetID", 0, func(e *Effect) { e.EdgeTargetID = "changed-target" }), fieldChange("EdgeRelKind", 0, func(e *Effect) { e.EdgeRelKind = EdgeBlockedBy })),
		simpleContexts("edge_remove", EffectEdgeRemove, Effect{Sort: EffectEdgeRemove, ResultSlot: "edge", RecordedAtOverride: &at, TaskID: target.ID, EdgeTargetID: alternate.ID.String(), EdgeRelKind: EdgeDerivedFrom, Contexts: contexts}, fieldChange("EdgeTargetID", 0, func(e *Effect) { e.EdgeTargetID = "changed-target" }), fieldChange("EdgeRelKind", 0, func(e *Effect) { e.EdgeRelKind = EdgeBlockedBy })),
		simpleContexts("label_add", EffectLabelAdd, Effect{Sort: EffectLabelAdd, ResultSlot: "label", RecordedAtOverride: &at, TaskID: target.ID, Label: "new-label", Contexts: contexts}, fieldChange("Label", 0, func(e *Effect) { e.Label = "changed" })),
		simpleContexts("label_remove", EffectLabelRemove, Effect{Sort: EffectLabelRemove, ResultSlot: "label", RecordedAtOverride: &at, TaskID: target.ID, Label: "remove-me", Contexts: contexts}, fieldChange("Label", 0, func(e *Effect) { e.Label = "changed" })),
		simpleContexts("comment_add", EffectCommentAdd, Effect{Sort: EffectCommentAdd, ResultSlot: "comment", RecordedAtOverride: &at, TaskID: target.ID, CommentIdentity: comment, CommentAuthor: env.actor, CommentBody: "body", Contexts: contexts}, fieldChange("CommentIdentity", 0, func(e *Effect) { e.CommentIdentity = altComment }), fieldChange("CommentAuthor", 0, func(e *Effect) { e.CommentAuthor = env.other }), fieldChange("CommentBody", 0, func(e *Effect) { e.CommentBody = "changed" })),
	}
	tables := auditedSnapshotTableNames(t, env.db)
	for _, family := range families {
		runDBOSFamilyBaseline(t, env, family, tables)
	}

	// Negative control: bypass Adapter.Apply's completed-operation preflight and
	// deliberately start a fresh workflow whose step reaches Journal.Apply and
	// conflicts. The entry oracle must increment even though no domain commit occurs.
	conflicting := families[0].input
	conflicting.Effects = cloneRetryEffects(conflicting.Effects)
	conflicting.Effects[0].Title = "negative-control-conflict"
	contract := newDBOSContractSnapshot()
	input, _, err := encodeApplyInput(contract, conflicting)
	if err != nil {
		t.Fatal(err)
	}
	entriesBefore := env.workflowEntries
	handle, err := dbos.RunWorkflow(env.root, env.adapter.applyWorkflow, input,
		dbos.WithWorkflowID("dbos-callback-entry-negative-control-"+uuid.NewString()),
		dbos.WithApplicationVersion(env.root.GetApplicationVersion()))
	if err != nil {
		t.Fatalf("start callback-entry negative control: %v", err)
	}
	outcome, err := handle.GetResult()
	if err != nil {
		t.Fatalf("await callback-entry negative control: %v", err)
	}
	if _, err := decodeDBOSStepOutcome(contract, outcome); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("negative control outcome error=%v, want ErrOperationConflict", err)
	}
	if env.workflowEntries-entriesBefore != 1 {
		t.Fatalf("entered-conflict negative control changed oracle by %d, want exactly 1", env.workflowEntries-entriesBefore)
	}
}

func TestDBOSDurableSnapshotDetectsTaskAttributionMutation(t *testing.T) {
	env := newDBOSFamilyEnv(t, "snapshot-attribution-negative-control", true)
	session := env.tracker.As(env.actor, env.authority)
	if _, err := session.Create("snapshot", "attributed", "", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID("snapshot-attribution-task")); err != nil {
		t.Fatal(err)
	}
	tables := auditedSnapshotTableNames(t, env.db)
	if !slices.Contains(tables, "task_attributions") {
		t.Fatal("audited durable snapshot omits task_attributions")
	}
	before := snapshotSQLTables(t, env.db, tables...)
	result, err := env.db.Exec(`DELETE FROM task_attributions WHERE (task_id, actor_id) IN (SELECT task_id, actor_id FROM task_attributions LIMIT 1)`)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		t.Fatalf("attribution negative control changed %d rows, err=%v", changed, err)
	}
	after := snapshotSQLTables(t, env.db, tables...)
	if reflect.DeepEqual(before, after) {
		t.Fatal("attribution mutation escaped complete durable snapshot")
	}
	if reflect.DeepEqual(before["task_attributions"], after["task_attributions"]) {
		t.Fatal("task_attributions relation did not participate in snapshot inequality")
	}
}
