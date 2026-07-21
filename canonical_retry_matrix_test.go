package provenance

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type retryMatrixFixture struct {
	path          string
	actor, other  ActorID
	authority     JournalID
	input         OperationInput
	result        CommittedResult
	genesisInput  OperationInput
	genesisResult CommittedResult
}

func buildRetryMatrixFixture(t *testing.T) (Tracker, retryMatrixFixture) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "retry-matrix.sqlite")
	tr, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := tr.RegisterSoftwareAgent("retry", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := tr.RegisterSoftwareAgent("retry", "other", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesisInput := OperationInput{OperationID: "retry-matrix-genesis", ActorID: actor.ID, CommandDigest: []byte("genesis"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority", BootstrapLabel: "root", OperationAuthorityID: "root"}}}
	gen, err := tr.Journal().Apply(genesisInput)
	if err != nil {
		t.Fatal(err)
	}
	boot, _ := slotJournalID(gen, "authority")
	target, err := tr.As(actor.ID, boot).Create("retry", "target", "target", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID(pinnedCreateOperationID(1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Journal().Apply(OperationInput{OperationID: "retry-matrix-assignment-evidence", ActorID: actor.ID, AuthorityJournalID: &boot, CommandDigest: []byte("setup"), Effects: []Effect{
		{Sort: EffectAssignmentStart, TaskID: target.ID, AssignmentID: "previous", SlotID: SlotOwnerResponsibility, Occupant: actor.ID},
		{Sort: EffectAssignmentEnd, TaskID: target.ID, AssignmentID: "previous", SlotID: SlotOwnerResponsibility},
		{Sort: EffectAssignmentStart, TaskID: target.ID, AssignmentID: "parent", SlotID: SlotOwnerResponsibility, Occupant: actor.ID},
	}}); err != nil {
		t.Fatalf("assignment evidence setup: %v", err)
	}
	task, alt := newCorpusTaskID(), newCorpusTaskID()
	comment, _ := ParseCommentID("retry--018f0000-0000-7000-8000-000000000011")
	ctx, _ := TaskContext(target.ID)
	altCtx, _ := TaskContext(alt)
	_ = altCtx
	title, description, notes := "updated", "updated description", "notes"
	priority, phase := PriorityHigh, PhaseCodeReview
	at := int64(700)
	effects := []Effect{
		{Sort: EffectTaskCreate, ResultSlot: "create", RecordedAtOverride: &at, TaskID: task, Payload: []byte(`{"birth":1}`), Contexts: []EventContext{ctx}, Title: "title", Description: "description", Type: TaskTypeFeature, Priority: PriorityMedium, Phase: PhaseUnscoped},
		{Sort: EffectTaskEvent, ResultSlot: "update", RecordedAtOverride: &at, TaskID: task, EventKind: EventKindTaskUpdated, Payload: []byte(`{"update":1}`), Contexts: []EventContext{ctx}, UpdateTitle: &title, UpdateDescription: &description, UpdatePriority: &priority, UpdatePhase: &phase, UpdateNotes: &notes},
		{Sort: EffectTaskEvent, ResultSlot: "generic", RecordedAtOverride: &at, TaskID: task, EventKind: "retry.generic.one", Payload: []byte(`{"generic":1}`), Contexts: []EventContext{ctx}},
		{Sort: EffectAssignmentStart, ResultSlot: "assignment-start", RecordedAtOverride: &at, TaskID: task, AssignmentID: "retry-assignment", SlotID: SlotOwnerResponsibility, Occupant: actor.ID, Predecessor: "previous", Parent: "parent"},
		{Sort: EffectAssignmentEnd, ResultSlot: "assignment-end", RecordedAtOverride: &at, TaskID: task, AssignmentID: "retry-assignment", SlotID: SlotOwnerResponsibility},
		{Sort: EffectDecision, ResultSlot: "decision", RecordedAtOverride: &at, TaskID: task, DecisionKind: "retry.decision", Payload: []byte(`{"decision":1}`)},
		{Sort: EffectEvidence, ResultSlot: "evidence", RecordedAtOverride: &at, TaskID: task, EvidenceKind: "retry.evidence", ContentDigest: []byte{1, 2, 3}, Payload: []byte(`{"evidence":1}`)},
		{Sort: EffectEdgeAdd, ResultSlot: "edge-add", RecordedAtOverride: &at, TaskID: task, EdgeTargetID: target.ID.String(), EdgeRelKind: EdgeDerivedFrom, Contexts: []EventContext{ctx}},
		{Sort: EffectEdgeRemove, ResultSlot: "edge-remove", RecordedAtOverride: &at, TaskID: task, EdgeTargetID: target.ID.String(), EdgeRelKind: EdgeDerivedFrom, Contexts: []EventContext{ctx}},
		{Sort: EffectLabelAdd, ResultSlot: "label-add", RecordedAtOverride: &at, TaskID: task, Label: "label", Contexts: []EventContext{ctx}},
		{Sort: EffectLabelRemove, ResultSlot: "label-remove", RecordedAtOverride: &at, TaskID: task, Label: "label", Contexts: []EventContext{ctx}},
		{Sort: EffectCommentAdd, ResultSlot: "comment", RecordedAtOverride: &at, TaskID: task, CommentIdentity: comment, CommentAuthor: actor.ID, CommentBody: "body", Contexts: []EventContext{ctx}},
		{Sort: EffectTaskEvent, ResultSlot: "close", RecordedAtOverride: &at, TaskID: task, EventKind: EventKindTaskClosed, CloseReason: "done", Forced: true},
	}
	in := OperationInput{OperationID: "retry-matrix-operation", ActorID: actor.ID, AuthorityJournalID: &boot, CommandDigest: []byte("command"), RecordedAt: 600, Effects: effects}
	result, err := tr.Journal().Apply(in)
	if err != nil {
		t.Fatalf("commit all-family retry fixture: %v", err)
	}
	return tr, retryMatrixFixture{path: path, actor: actor.ID, other: other.ID, authority: boot, input: in, result: result, genesisInput: genesisInput, genesisResult: gen}
}

func cloneRetryEffects(in []Effect) []Effect {
	out := append([]Effect(nil), in...)
	for i := range out {
		out[i].Payload = append([]byte(nil), in[i].Payload...)
		out[i].ContentDigest = append([]byte(nil), in[i].ContentDigest...)
		out[i].Contexts = append([]EventContext(nil), in[i].Contexts...)
	}
	return out
}

func retryMismatchCandidates(t *testing.T, f retryMatrixFixture) map[string]OperationInput {
	t.Helper()
	out := map[string]OperationInput{}
	add := func(name string, change func(*OperationInput)) {
		v := f.input
		v.Effects = cloneRetryEffects(f.input.Effects)
		change(&v)
		if _, err := PrepareMutationV1(v.Effects); err != nil {
			t.Fatalf("candidate %s is not canonical: %v", name, err)
		}
		out[name] = v
	}
	add("actor", func(v *OperationInput) { v.ActorID = f.other })
	add("authority-null", func(v *OperationInput) { v.AuthorityJournalID = nil })
	add("authority-change", func(v *OperationInput) { x := f.authority + 999; v.AuthorityJournalID = &x })
	add("command", func(v *OperationInput) { v.CommandDigest = []byte("changed") })
	add("effect-order", func(v *OperationInput) { v.Effects[5], v.Effects[6] = v.Effects[6], v.Effects[5] })
	add("effect-family", func(v *OperationInput) {
		v.Effects[2] = Effect{Sort: EffectDecision, ResultSlot: "generic", RecordedAtOverride: v.Effects[2].RecordedAtOverride, TaskID: v.Effects[2].TaskID, DecisionKind: "retry.changed", Payload: []byte(`{"generic":1}`)}
	})
	altTask := newCorpusTaskID()
	altCtx, _ := TaskContext(altTask)
	altComment, _ := ParseCommentID("retry--018f0000-0000-7000-8000-000000000012")
	at := int64(701)
	title, description, notes := "changed", "changed description", "changed notes"
	priority, phase := PriorityCritical, PhaseImplPlan
	for i := range f.input.Effects {
		i := i
		add("result-slot-"+string(rune('a'+i)), func(v *OperationInput) { v.Effects[i].ResultSlot += "-changed" })
		add("recorded-at-"+string(rune('a'+i)), func(v *OperationInput) { v.Effects[i].RecordedAtOverride = &at })
		if f.input.Effects[i].TaskID.Namespace != "" {
			add("task-"+string(rune('a'+i)), func(v *OperationInput) { v.Effects[i].TaskID = altTask })
		}
	}
	add("create-payload", func(v *OperationInput) { v.Effects[0].Payload = []byte(`{"birth":2}`) })
	add("create-context", func(v *OperationInput) { v.Effects[0].Contexts = []EventContext{altCtx} })
	add("create-title", func(v *OperationInput) { v.Effects[0].Title = "changed" })
	add("create-description", func(v *OperationInput) { v.Effects[0].Description = "changed" })
	add("create-type", func(v *OperationInput) { v.Effects[0].Type = TaskTypeEpic })
	add("create-priority", func(v *OperationInput) { v.Effects[0].Priority = PriorityHigh })
	add("create-phase", func(v *OperationInput) { v.Effects[0].Phase = PhaseCodeReview })
	add("event-kind", func(v *OperationInput) { v.Effects[2].EventKind = "retry.generic.two" })
	add("event-payload", func(v *OperationInput) { v.Effects[1].Payload = []byte(`{"update":2}`) })
	add("event-context", func(v *OperationInput) { v.Effects[1].Contexts = []EventContext{altCtx} })
	add("update-title", func(v *OperationInput) { v.Effects[1].UpdateTitle = &title })
	add("update-description", func(v *OperationInput) { v.Effects[1].UpdateDescription = &description })
	add("update-priority", func(v *OperationInput) { v.Effects[1].UpdatePriority = &priority })
	add("update-phase", func(v *OperationInput) { v.Effects[1].UpdatePhase = &phase })
	add("update-notes", func(v *OperationInput) { v.Effects[1].UpdateNotes = &notes })
	add("close-forced", func(v *OperationInput) { v.Effects[12].Forced = false })
	add("close-reason", func(v *OperationInput) { v.Effects[12].CloseReason = "changed" })
	add("assignment-id", func(v *OperationInput) { v.Effects[3].AssignmentID = "changed" })
	add("assignment-occupant", func(v *OperationInput) { v.Effects[3].Occupant = f.other })
	add("assignment-predecessor", func(v *OperationInput) { v.Effects[3].Predecessor = "changed" })
	add("assignment-parent", func(v *OperationInput) { v.Effects[3].Parent = "changed" })
	add("assignment-end-id", func(v *OperationInput) { v.Effects[4].AssignmentID = "changed" })
	add("decision-kind", func(v *OperationInput) { v.Effects[5].DecisionKind = "retry.changed" })
	add("decision-payload", func(v *OperationInput) { v.Effects[5].Payload = []byte(`{"decision":2}`) })
	add("evidence-kind", func(v *OperationInput) { v.Effects[6].EvidenceKind = "retry.changed" })
	add("evidence-digest", func(v *OperationInput) { v.Effects[6].ContentDigest = []byte{9} })
	add("evidence-payload", func(v *OperationInput) { v.Effects[6].Payload = []byte(`{"evidence":2}`) })
	add("edge-target", func(v *OperationInput) { v.Effects[7].EdgeTargetID = altTask.String() })
	add("edge-kind", func(v *OperationInput) { v.Effects[7].EdgeRelKind = EdgeBlockedBy })
	add("edge-context", func(v *OperationInput) { v.Effects[7].Contexts = []EventContext{altCtx} })
	add("edge-remove-target", func(v *OperationInput) { v.Effects[8].EdgeTargetID = altTask.String() })
	add("edge-remove-kind", func(v *OperationInput) { v.Effects[8].EdgeRelKind = EdgeBlockedBy })
	add("edge-remove-context", func(v *OperationInput) { v.Effects[8].Contexts = []EventContext{altCtx} })
	add("label", func(v *OperationInput) { v.Effects[9].Label = "changed" })
	add("label-context", func(v *OperationInput) { v.Effects[9].Contexts = []EventContext{altCtx} })
	add("label-remove", func(v *OperationInput) { v.Effects[10].Label = "changed" })
	add("label-remove-context", func(v *OperationInput) { v.Effects[10].Contexts = []EventContext{altCtx} })
	add("comment-id", func(v *OperationInput) { v.Effects[11].CommentIdentity = altComment })
	add("comment-author", func(v *OperationInput) { v.Effects[11].CommentAuthor = f.other })
	add("comment-body", func(v *OperationInput) { v.Effects[11].CommentBody = "changed" })
	add("comment-context", func(v *OperationInput) { v.Effects[11].Contexts = []EventContext{altCtx} })
	return out
}

type operationCounts struct{ journal, operations, slots int64 }

func readOperationCounts(t *testing.T, tr Tracker) operationCounts {
	t.Helper()
	db := tr.(*sqliteTracker).db
	db.Lock()
	defer db.Unlock()
	c := operationCounts{}
	for _, q := range []struct {
		sql string
		dst *int64
	}{{`SELECT count(*) FROM journal`, &c.journal}, {`SELECT count(*) FROM journal_operations`, &c.operations}, {`SELECT count(*) FROM journal_operation_result_slots`, &c.slots}} {
		if err := sqlitex.Execute(db.Conn(), q.sql, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { *q.dst = stmt.ColumnInt64(0); return nil }}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func assertExactRetry(t *testing.T, tr Tracker, f retryMatrixFixture) {
	t.Helper()
	before := readOperationCounts(t, tr)
	got, err := tr.Journal().Apply(f.input)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ShortCircuited {
		t.Fatal("exact retry did not short circuit")
	}
	got.ShortCircuited = false
	if !reflect.DeepEqual(got, f.result) {
		t.Fatalf("complete exact result=%+v want=%+v", got, f.result)
	}
	if after := readOperationCounts(t, tr); after != before {
		t.Fatalf("exact retry wrote rows: before=%+v after=%+v", before, after)
	}
}
func assertMismatchMatrix(t *testing.T, tr Tracker, f retryMatrixFixture) {
	t.Helper()
	for name, candidate := range retryMismatchCandidates(t, f) {
		t.Run(name, func(t *testing.T) {
			before := readOperationCounts(t, tr)
			got, err := tr.Journal().Apply(candidate)
			if !errors.Is(err, ErrOperationConflict) || !completeConflictResult(got, candidate.OperationID) {
				t.Fatalf("result=%+v error=%v, want typed committed conflict", got, err)
			}
			if after := readOperationCounts(t, tr); after != before {
				t.Fatalf("conflict wrote rows: before=%+v after=%+v", before, after)
			}
		})
	}
}

func completeConflictResult(result CommittedResult, operationID OperationID) bool {
	return result.Kind == CommittedConflict && result.Conflict != nil && result.Conflict.OperationID == operationID && result.Conflict.Field != "" && result.AnchorJournalID == 0 && len(result.EmittedEvents) == 0 && len(result.ResultSlots) == 0 && !result.ShortCircuited
}

func bootstrapMismatchCandidates(f retryMatrixFixture) map[string]OperationInput {
	makeCandidate := func(change func(*Effect)) OperationInput {
		candidate := f.genesisInput
		candidate.Effects = cloneRetryEffects(candidate.Effects)
		change(&candidate.Effects[0])
		return candidate
	}
	return map[string]OperationInput{
		"bootstrap-label":        makeCandidate(func(effect *Effect) { effect.BootstrapLabel = "changed" }),
		"bootstrap-authority-id": makeCandidate(func(effect *Effect) { effect.OperationAuthorityID = "changed" }),
		"bootstrap-slot":         makeCandidate(func(effect *Effect) { effect.ResultSlot = "changed" }),
		"bootstrap-recorded-at":  makeCandidate(func(effect *Effect) { value := int64(9); effect.RecordedAtOverride = &value }),
	}
}

func assertBootstrapRetryMatrix(t *testing.T, tr Tracker, f retryMatrixFixture) {
	t.Helper()
	before := readOperationCounts(t, tr)
	got, err := tr.Journal().Apply(f.genesisInput)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ShortCircuited {
		t.Fatal("genesis exact retry did not short circuit")
	}
	got.ShortCircuited = false
	if !reflect.DeepEqual(got, f.genesisResult) {
		t.Fatalf("complete genesis retry=%+v want=%+v", got, f.genesisResult)
	}
	if after := readOperationCounts(t, tr); after != before {
		t.Fatalf("genesis retry wrote rows: before=%+v after=%+v", before, after)
	}
	for name, candidate := range bootstrapMismatchCandidates(f) {
		t.Run(name, func(t *testing.T) {
			before := readOperationCounts(t, tr)
			result, err := tr.Journal().Apply(candidate)
			if !errors.Is(err, ErrOperationConflict) || !completeConflictResult(result, candidate.OperationID) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if after := readOperationCounts(t, tr); after != before {
				t.Fatalf("bootstrap conflict wrote rows: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestCanonicalRetryMatrixSameProcessAndReopen(t *testing.T) {
	tr, f := buildRetryMatrixFixture(t)
	assertBootstrapRetryMatrix(t, tr, f)
	assertExactRetry(t, tr, f)
	assertMismatchMatrix(t, tr, f)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	tr, err := OpenSQLite(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	assertBootstrapRetryMatrix(t, tr, f)
	assertExactRetry(t, tr, f)
	assertMismatchMatrix(t, tr, f)
}

func TestCanonicalRetryMatrixSimultaneousIndependentHandles(t *testing.T) {
	first, f := buildRetryMatrixFixture(t)
	defer first.Close()
	second, err := OpenSQLite(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	run := func(name string, input OperationInput, exact bool, expected CommittedResult) {
		t.Run(name, func(t *testing.T) {
			before := readOperationCounts(t, first)
			var results [2]CommittedResult
			var errs [2]error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); results[0], errs[0] = first.Journal().Apply(input) }()
			go func() { defer wg.Done(); results[1], errs[1] = second.Journal().Apply(input) }()
			wg.Wait()
			for i := range errs {
				if exact {
					if errs[i] != nil {
						t.Fatal(errs[i])
					}
					results[i].ShortCircuited = false
					if !reflect.DeepEqual(results[i], expected) {
						t.Fatalf("handle %d result=%+v want=%+v", i, results[i], expected)
					}
				} else if !errors.Is(errs[i], ErrOperationConflict) || !completeConflictResult(results[i], input.OperationID) {
					t.Fatalf("handle %d result=%+v error=%v", i, results[i], errs[i])
				}
			}
			if after := readOperationCounts(t, first); after != before {
				t.Fatalf("independent retry wrote rows: before=%+v after=%+v", before, after)
			}
		})
	}
	run("exact", f.input, true, f.result)
	run("genesis-exact", f.genesisInput, true, f.genesisResult)
	for name, candidate := range bootstrapMismatchCandidates(f) {
		run(name, candidate, false, CommittedResult{})
	}
	for name, candidate := range retryMismatchCandidates(t, f) {
		run(name, candidate, false, CommittedResult{})
	}
}

func TestInvalidDecisionEvidenceKindsFailBeforeJournalWrites(t *testing.T) {
	tr, f := buildRetryMatrixFixture(t)
	defer tr.Close()
	before := readOperationCounts(t, tr)
	for _, test := range []struct {
		name   string
		effect Effect
	}{{"decision", Effect{Sort: EffectDecision, TaskID: f.input.Effects[0].TaskID, DecisionKind: "unnamespaced"}}, {"evidence", Effect{Sort: EffectEvidence, TaskID: f.input.Effects[0].TaskID, EvidenceKind: "bad..kind", ContentDigest: []byte{1}}}} {
		t.Run(test.name, func(t *testing.T) {
			_, err := tr.Journal().Apply(OperationInput{OperationID: OperationID("invalid-kind-" + test.name), ActorID: f.actor, AuthorityJournalID: &f.authority, CommandDigest: []byte("invalid"), Effects: []Effect{test.effect}})
			if !errors.Is(err, ErrCanonicalMutation) {
				t.Fatalf("error=%v, want canonical rejection", err)
			}
			if after := readOperationCounts(t, tr); after != before {
				t.Fatalf("invalid kind wrote rows: before=%+v after=%+v", before, after)
			}
		})
	}
}
