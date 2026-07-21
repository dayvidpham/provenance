package journal

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

func fixtureIDs(t *testing.T) (TaskID, ActorID, CommentID) {
	t.Helper()
	task, _ := ptypes.ParseTaskID("fixture--018f0000-0000-7000-8000-000000000001")
	actor, _ := ptypes.ParseActorID("fixture--018f0000-0000-7000-8000-000000000002")
	comment, _ := ptypes.ParseCommentID("fixture--018f0000-0000-7000-8000-000000000003")
	return task, actor, comment
}

func TestCanonicalMutationV1IndependentGoldenBytes(t *testing.T) {
	effect := Effect{Sort: EffectBootstrapAuthority, ResultSlot: "root", BootstrapLabel: "root", OperationAuthorityID: "auth-1"}
	got, err := PrepareMutationV1([]Effect{effect})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("version:22:provenance.mutation.v1\neffect-count:1:1\neffect.0.family:19:bootstrap_authority\neffect.0.result-slot:4:root\neffect.0.recorded-at-override:1:0\neffect.0.bootstrap-label:4:root\neffect.0.operation-authority:6:auth-1\n")
	if !bytes.Equal(got.CanonicalBytes(), want) {
		t.Fatalf("canonical bytes drifted\n got %q\nwant %q", got.CanonicalBytes(), want)
	}
	decoded, err := DecodeCanonicalMutation(want)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.NormalizedEffects(), []Effect{effect}) {
		t.Fatalf("decoded independent fixture = %#v, want %#v", decoded.NormalizedEffects(), []Effect{effect})
	}
}

func validFamilyEffects(t *testing.T) []Effect {
	task, actor, comment := fixtureIDs(t)
	title, description, notes := "new", "new description", "notes"
	priority, phase := ptypes.PriorityHigh, ptypes.PhaseCodeReview
	recordedAt := int64(42)
	ctx, _ := TaskContext(task)
	return []Effect{
		{Sort: EffectTaskEvent, ResultSlot: "event", RecordedAtOverride: &recordedAt, TaskID: task, EventKind: EventKindTaskUpdated, Payload: []byte(`{"a":1,"b":2}`), Contexts: []EventContext{ctx}, UpdateTitle: &title, UpdateDescription: &description, UpdatePriority: &priority, UpdatePhase: &phase, UpdateNotes: &notes},
		{Sort: EffectTaskEvent, ResultSlot: "close", RecordedAtOverride: &recordedAt, TaskID: task, EventKind: EventKindTaskClosed, CloseReason: "done", Forced: true},
		{Sort: EffectBootstrapAuthority, ResultSlot: "bootstrap", RecordedAtOverride: &recordedAt, BootstrapLabel: "root", OperationAuthorityID: "auth"},
		{Sort: EffectAssignmentStart, ResultSlot: "start", RecordedAtOverride: &recordedAt, TaskID: task, AssignmentID: "a", SlotID: SlotOwnerResponsibility, Occupant: actor, Predecessor: "p", Parent: "parent"},
		{Sort: EffectAssignmentEnd, ResultSlot: "end", RecordedAtOverride: &recordedAt, TaskID: task, AssignmentID: "a", SlotID: SlotOwnerResponsibility},
		{Sort: EffectDecision, ResultSlot: "decision", RecordedAtOverride: &recordedAt, TaskID: task, DecisionKind: "fixture.decision", Payload: []byte(`{"x":1}`)},
		{Sort: EffectEvidence, ResultSlot: "evidence", RecordedAtOverride: &recordedAt, TaskID: task, EvidenceKind: "fixture.evidence", ContentDigest: []byte{1, 2}, Payload: []byte(`{"x":1}`)},
		{Sort: EffectTaskCreate, ResultSlot: "create", RecordedAtOverride: &recordedAt, TaskID: task, Title: "title", Description: "description", Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped, Payload: []byte(`{"x":1}`), Contexts: []EventContext{ctx}},
		{Sort: EffectEdgeAdd, ResultSlot: "edge-add", RecordedAtOverride: &recordedAt, TaskID: task, EdgeTargetID: task.String(), EdgeRelKind: ptypes.EdgeDerivedFrom, Contexts: []EventContext{ctx}},
		{Sort: EffectEdgeRemove, ResultSlot: "edge-remove", RecordedAtOverride: &recordedAt, TaskID: task, EdgeTargetID: task.String(), EdgeRelKind: ptypes.EdgeDerivedFrom, Contexts: []EventContext{ctx}},
		{Sort: EffectLabelAdd, ResultSlot: "label-add", RecordedAtOverride: &recordedAt, TaskID: task, Label: "label", Contexts: []EventContext{ctx}},
		{Sort: EffectLabelRemove, ResultSlot: "label-remove", RecordedAtOverride: &recordedAt, TaskID: task, Label: "label", Contexts: []EventContext{ctx}},
		{Sort: EffectCommentAdd, ResultSlot: "comment", RecordedAtOverride: &recordedAt, TaskID: task, CommentIdentity: comment, CommentAuthor: actor, CommentBody: "body", Contexts: []EventContext{ctx}},
	}
}

func TestCanonicalMutationV1EveryFamilyRoundTripsMeaningfulSemantics(t *testing.T) {
	covered := map[string]bool{}
	for fixtureIndex, effect := range validFamilyEffects(t) {
		t.Run(effect.Sort.String(), func(t *testing.T) {
			prepared, err := PrepareMutationV1([]Effect{effect})
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeCanonicalMutation(prepared.CanonicalBytes())
			if err != nil {
				t.Fatal(err)
			}
			expected := effect
			literalWire := independentFixtureWire(independentFamilyFields(t)[fixtureIndex])
			if !bytes.Equal(prepared.CanonicalBytes(), literalWire) {
				t.Fatalf("family %s wire drifted\n got %q\nwant %q", effect.Sort, prepared.CanonicalBytes(), literalWire)
			}
			decoded, err = DecodeCanonicalMutation(literalWire)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.NormalizedEffects(), []Effect{expected}) {
				t.Fatalf("family round trip = %#v, want %#v", decoded.NormalizedEffects(), []Effect{expected})
			}
			typ := reflect.TypeOf(expected)
			value := reflect.ValueOf(expected)
			for i := 0; i < value.NumField(); i++ {
				field := typ.Field(i)
				if field.Name == "Sort" || field.Name == "ActorID" || value.Field(i).IsZero() {
					continue
				}
				covered[field.Name] = true
				changed := expected
				changedValue := reflect.ValueOf(&changed).Elem().Field(i)
				skipAlternative := false
				switch field.Name {
				case "TaskID":
					changed.TaskID, _ = ptypes.ParseTaskID("fixture--018f0000-0000-7000-8000-000000000004")
				case "Occupant", "CommentAuthor":
					alternate, _ := ptypes.ParseActorID("fixture--018f0000-0000-7000-8000-000000000005")
					if field.Name == "Occupant" {
						changed.Occupant = alternate
					} else {
						changed.CommentAuthor = alternate
					}
				case "CommentIdentity":
					changed.CommentIdentity, _ = ptypes.ParseCommentID("fixture--018f0000-0000-7000-8000-000000000006")
				case "Type":
					changed.Type = ptypes.TaskTypeFeature
				case "Priority":
					changed.Priority = ptypes.PriorityCritical
				case "Phase":
					changed.Phase = ptypes.PhaseCodeReview
				case "EdgeRelKind":
					changed.EdgeRelKind = ptypes.EdgeBlockedBy
				case "ContentDigest":
					changed.ContentDigest = []byte{9, 8}
				case "EventKind", "SlotID":
					skipAlternative = true
				default:
					switch changedValue.Kind() {
					case reflect.String:
						changedValue.SetString(changedValue.String() + "-changed")
					case reflect.Bool:
						changedValue.SetBool(!changedValue.Bool())
					case reflect.Int:
						changedValue.SetInt(changedValue.Int() + 1)
					case reflect.Slice:
						changedValue.Set(reflect.Zero(changedValue.Type()))
					case reflect.Struct:
						changedValue.Set(reflect.Zero(changedValue.Type()))
					case reflect.Ptr:
						replacement := reflect.New(changedValue.Type().Elem())
						switch replacement.Elem().Kind() {
						case reflect.String:
							replacement.Elem().SetString(changedValue.Elem().String() + "-changed")
						case reflect.Int, reflect.Int64:
							replacement.Elem().SetInt(changedValue.Elem().Int() + 1)
						default:
							t.Fatalf("unhandled pointer operand %s", field.Name)
						}
						changedValue.Set(replacement)
					default:
						t.Fatalf("unhandled operand %s (%s)", field.Name, changedValue.Kind())
					}
				}
				if skipAlternative {
					continue
				}
				other, changeErr := PrepareMutationV1([]Effect{changed})
				if changeErr != nil {
					t.Fatalf("valid alternative for %s rejected: %v", field.Name, changeErr)
				}
				if bytes.Equal(prepared.CanonicalBytes(), other.CanonicalBytes()) {
					t.Fatalf("meaningful operand %s did not change identity", field.Name)
				}
			}
		})
	}
	for _, field := range []string{"ResultSlot", "RecordedAtOverride", "TaskID", "EventKind", "Payload", "Contexts", "Title", "Description", "Type", "Priority", "Phase", "CloseReason", "UpdateTitle", "UpdateDescription", "UpdatePriority", "UpdatePhase", "UpdateNotes", "Forced", "BootstrapLabel", "OperationAuthorityID", "AssignmentID", "SlotID", "Occupant", "Predecessor", "Parent", "DecisionKind", "EvidenceKind", "ContentDigest", "EdgeTargetID", "EdgeRelKind", "Label", "CommentIdentity", "CommentAuthor", "CommentBody"} {
		if !covered[field] {
			t.Errorf("meaningful Effect operand %s has no round-trip/identity guard", field)
		}
	}
}

func TestCanonicalEventKindValidAlternativeAndActorRejection(t *testing.T) {
	task, actor, _ := fixtureIDs(t)
	left, err := PrepareMutationV1([]Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "fixture.one"}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := PrepareMutationV1([]Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "fixture.two"}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(left.CanonicalBytes(), right.CanonicalBytes()) {
		t.Fatal("valid event-kind alternative did not change canonical identity")
	}
	if _, err := PrepareMutationV1([]Effect{{Sort: EffectTaskEvent, ActorID: actor, TaskID: task, EventKind: "fixture.one"}}); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("per-effect ActorID error=%v", err)
	}
}

type independentField struct{ name, value string }

func independentFixtureWire(fields []independentField) []byte {
	var out bytes.Buffer
	write := func(name, value string) { fmt.Fprintf(&out, "%s:%d:%s\n", name, len(value), value) }
	write("version", "provenance.mutation.v1")
	write("effect-count", "1")
	for _, field := range fields {
		write("effect.0."+field.name, field.value)
	}
	return out.Bytes()
}

func independentFamilyFields(t *testing.T) [][]independentField {
	t.Helper()
	task, actor, comment := fixtureIDs(t)
	common := func(family, slot string, rest ...independentField) []independentField {
		return append([]independentField{{"family", family}, {"result-slot", slot}, {"recorded-at-override", "142"}}, rest...)
	}
	ctx := []independentField{{"context-count", "1"}, {"context.0.kind", "task"}, {"context.0.identity", task.String()}}
	withCtx := func(fields []independentField) []independentField { return append(fields, ctx...) }
	return [][]independentField{
		append(append(common("task_event", "event", independentField{"task", task.String()}, independentField{"event-kind", string(EventKindTaskUpdated)}, independentField{"payload", `{"a":1,"b":2}`}), ctx...), independentField{"update-title", "1new"}, independentField{"update-description", "1new description"}, independentField{"update-priority", "1high"}, independentField{"update-phase", "1code_review"}, independentField{"update-notes", "1notes"}),
		common("task_event", "close", independentField{"task", task.String()}, independentField{"event-kind", string(EventKindTaskClosed)}, independentField{"payload", "{}"}, independentField{"context-count", "0"}, independentField{"forced", "true"}, independentField{"close-reason", "done"}),
		common("bootstrap_authority", "bootstrap", independentField{"bootstrap-label", "root"}, independentField{"operation-authority", "auth"}),
		common("assignment_start", "start", independentField{"task", task.String()}, independentField{"assignment", "a"}, independentField{"slot", "owner-responsibility"}, independentField{"occupant", actor.String()}, independentField{"predecessor", "p"}, independentField{"parent", "parent"}),
		common("assignment_end", "end", independentField{"task", task.String()}, independentField{"assignment", "a"}, independentField{"slot", "owner-responsibility"}),
		common("decision", "decision", independentField{"task", task.String()}, independentField{"decision-kind", "fixture.decision"}, independentField{"payload", `{"x":1}`}),
		common("evidence", "evidence", independentField{"task", task.String()}, independentField{"evidence-kind", "fixture.evidence"}, independentField{"content-digest", string([]byte{1, 2})}, independentField{"payload", `{"x":1}`}),
		append(append(common("task_create", "create", independentField{"task", task.String()}, independentField{"payload", `{"x":1}`}), ctx...), independentField{"title", "title"}, independentField{"description", "description"}, independentField{"type", "task"}, independentField{"priority", "medium"}, independentField{"phase", "unscoped"}),
		withCtx(common("edge_add", "edge-add", independentField{"task", task.String()}, independentField{"edge-target", task.String()}, independentField{"edge-kind", "derived_from"})),
		withCtx(common("edge_remove", "edge-remove", independentField{"task", task.String()}, independentField{"edge-target", task.String()}, independentField{"edge-kind", "derived_from"})),
		withCtx(common("label_add", "label-add", independentField{"task", task.String()}, independentField{"label", "label"})),
		withCtx(common("label_remove", "label-remove", independentField{"task", task.String()}, independentField{"label", "label"})),
		withCtx(common("comment_add", "comment", independentField{"task", task.String()}, independentField{"comment", comment.String()}, independentField{"comment-author", actor.String()}, independentField{"comment-body", "body"})),
	}
}

func TestIndependentCanonicalOracleNegativeControls(t *testing.T) {
	fixture := independentFixtureWire(independentFamilyFields(t)[7])
	prepared, err := PrepareMutationV1([]Effect{validFamilyEffects(t)[7]})
	if err != nil {
		t.Fatal(err)
	}
	for name, changed := range map[string][]byte{
		"family-tag":  bytes.Replace(prepared.CanonicalBytes(), []byte("task_create"), []byte("task_create_x"), 1),
		"field-name":  bytes.Replace(prepared.CanonicalBytes(), []byte("effect.0.title"), []byte("effect.0.titel"), 1),
		"field-order": bytes.Replace(prepared.CanonicalBytes(), []byte("effect.0.title:5:title\neffect.0.description:11:description"), []byte("effect.0.description:11:description\neffect.0.title:5:title"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(changed, fixture) {
				t.Fatal("mutated production wire matched independent oracle")
			}
			if _, err := DecodeCanonicalMutation(changed); err == nil {
				t.Fatal("mutated production wire decoded")
			}
		})
	}
}

func TestCanonicalMutationRejectsIrrelevantFieldsAndInvalidEnums(t *testing.T) {
	task, _, _ := fixtureIDs(t)
	cases := map[string]Effect{
		"bootstrap-task":      {Sort: EffectBootstrapAuthority, TaskID: task},
		"decision-label":      {Sort: EffectDecision, DecisionKind: "fixture.decision", Label: "ignored"},
		"invalid-edge-enum":   {Sort: EffectEdgeAdd, TaskID: task, EdgeTargetID: "x", EdgeRelKind: ptypes.EdgeKind(99)},
		"invalid-create-enum": {Sort: EffectTaskCreate, TaskID: task, Type: ptypes.TaskType(99), Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped},
	}
	for name, effect := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := PrepareMutationV1([]Effect{effect})
			if !errors.Is(err, ErrCanonicalMutation) {
				t.Fatalf("error=%v, want typed canonical error", err)
			}
		})
	}
}

func TestDecodeCanonicalMutationStrictPopulatedMatrix(t *testing.T) {
	base, err := PrepareMutationV1([]Effect{validFamilyEffects(t)[0]})
	if err != nil {
		t.Fatal(err)
	}
	replace := func(old, new string) []byte {
		return bytes.Replace(base.CanonicalBytes(), []byte(old), []byte(new), 1)
	}
	cases := map[string][]byte{
		"unsupported-version":   replace("provenance.mutation.v1", "provenance.mutation.v2"),
		"unknown-family":        replace("task_event", "unknown_fx"),
		"unknown-enum":          replace("code_review", "badbadbadbad"),
		"unknown-context-tag":   replace("task:", "xxxx:"),
		"missing-field":         bytes.Replace(base.CanonicalBytes(), []byte("effect.0.result-slot"), []byte("missing-slot-field"), 1),
		"duplicate-field":       bytes.Replace(base.CanonicalBytes(), []byte("effect.0.recorded-at-override"), []byte("effect.0.result-slot          "), 1),
		"trailing":              append(base.CanonicalBytes(), 'x'),
		"overflow-effect-count": []byte("version:22:provenance.mutation.v1\neffect-count:9:999999999\n"),
		"overflow-field-length": []byte("version:999999999:"),
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCanonicalMutation(wire); err == nil {
				t.Fatalf("malformed populated wire accepted: %q", wire)
			}
		})
	}
	if _, err := PrepareMutationV1(make([]Effect, MaxCanonicalEffects+1)); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("unbounded encode=%v", err)
	}
	if _, err := DecodeCanonicalMutation(bytes.Repeat([]byte{'x'}, MaxCanonicalMutationBytes+1)); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("unbounded decode=%v", err)
	}
}

func TestCanonicalMutationStaticLimitsAtBoundary(t *testing.T) {
	effects := make([]Effect, MaxCanonicalEffects)
	for i := range effects {
		effects[i] = Effect{Sort: EffectBootstrapAuthority}
	}
	if _, err := PrepareMutationV1(effects); err != nil {
		t.Fatalf("exact effect bound rejected: %v", err)
	}
	contexts := make([]EventContext, MaxCanonicalContextsPerEffect)
	for i := range contexts {
		id := TaskID{Namespace: "fixture", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(strconv.Itoa(i)))}
		contexts[i], _ = TaskContext(id)
	}
	task, _, _ := fixtureIDs(t)
	if _, err := PrepareMutationV1([]Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "fixture.event", Contexts: contexts}}); err != nil {
		t.Fatalf("exact context bound rejected: %v", err)
	}
	if _, err := PrepareMutationV1([]Effect{{Sort: EffectTaskCreate, TaskID: task, Title: strings.Repeat("x", MaxCanonicalFieldBytes), Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped}}); err != nil {
		t.Fatalf("exact field bound rejected: %v", err)
	}
	if _, err := DecodeCanonicalMutation(bytes.Repeat([]byte{'x'}, MaxCanonicalMutationBytes)); err == nil {
		t.Fatal("malformed mutation at total-size bound was accepted")
	}
}

func TestCanonicalJSONRejectsDuplicateAndTrailingFields(t *testing.T) {
	task, _, _ := fixtureIDs(t)
	for _, payload := range []string{`{"x":1,"x":2}`, `{"x":1} {"y":2}`} {
		if _, err := PrepareMutationV1([]Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "fixture.event", Payload: []byte(payload)}}); err == nil {
			t.Fatalf("accepted %s", payload)
		}
	}
}

func TestCanonicalWireHasNoUnconsumedFieldNames(t *testing.T) {
	for _, effect := range validFamilyEffects(t) {
		prepared, err := PrepareMutationV1([]Effect{effect})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(prepared.CanonicalBytes()), "effect.0.actor:") {
			t.Fatalf("family %s encoded rejected actor field", effect.Sort)
		}
	}
}
