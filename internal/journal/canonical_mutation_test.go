package journal

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

type invalidCanonicalV1Ref struct{}

func (invalidCanonicalV1Ref) canonicalV1FieldRef() {}

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
	wantDigest, err := hex.DecodeString("2a044cf580d36ae527513e4a520febd935ac34cb514ff91636b17984b555b727")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.DerivedDigest(), wantDigest) {
		t.Fatalf("canonical digest drifted: got %x want %x", got.DerivedDigest(), wantDigest)
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
		{Sort: EffectTaskCreateAllocated, ResultSlot: "allocated", RecordedAtOverride: &recordedAt, TaskID: task, Title: "title", Description: "description", Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped, Payload: []byte(`{"x":1}`), Contexts: []EventContext{ctx}},
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
				if bytes.Equal(prepared.DerivedDigest(), other.DerivedDigest()) {
					t.Fatalf("meaningful operand %s did not change digest", field.Name)
				}
				alternateDecoded, err := DecodeCanonicalMutation(other.CanonicalBytes())
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(alternateDecoded.NormalizedEffects(), []Effect{changed}) {
					t.Fatalf("alternate %s semantics=%#v want independently changed %#v", field.Name, alternateDecoded.NormalizedEffects(), []Effect{changed})
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
	if bytes.Equal(left.DerivedDigest(), right.DerivedDigest()) {
		t.Fatal("valid event-kind alternative did not change digest")
	}
	decoded, err := DecodeCanonicalMutation(right.CanonicalBytes())
	if err != nil {
		t.Fatal(err)
	}
	want := []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "fixture.two"}}
	if !reflect.DeepEqual(decoded.NormalizedEffects(), want) {
		t.Fatalf("alternate event semantics=%#v want %#v", decoded.NormalizedEffects(), want)
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
		append(append(common("task_create_allocated", "allocated", independentField{"task", task.String()}, independentField{"payload", `{"x":1}`}), ctx...), independentField{"title", "title"}, independentField{"description", "description"}, independentField{"type", "task"}, independentField{"priority", "medium"}, independentField{"phase", "unscoped"}),
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
		"allocated-no-slot":   {Sort: EffectTaskCreateAllocated, TaskID: task, Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped},
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
			} else {
				assertActionableCanonicalError(t, err)
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

func assertActionableCanonicalError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("error does not wrap ErrCanonicalMutation: %v", err)
	}
	var typed *CanonicalMutationError
	if !errors.As(err, &typed) {
		t.Fatalf("error is not CanonicalMutationError: %T %v", err, err)
	}
	if typed.Field == "" || typed.Reason == "" || typed.Fix == "" {
		t.Fatalf("incomplete typed error: %+v", typed)
	}
	message := err.Error()
	for _, part := range []string{"where:", "when:", "impact:", "fix:"} {
		if !strings.Contains(message, part) {
			t.Fatalf("error lacks %s: %v", part, err)
		}
	}
}

func TestCanonicalMalformedInventoryIsUniformlyTyped(t *testing.T) {
	fixtures := independentFamilyFields(t)
	task, _, _ := fixtureIDs(t)
	fieldWire := func(index int, change func([]independentField)) []byte {
		fields := append([]independentField(nil), fixtures[index]...)
		change(fields)
		return independentFixtureWire(fields)
	}
	cases := map[string][]byte{
		"optional-marker": fieldWire(0, func(fields []independentField) {
			for i := range fields {
				if fields[i].name == "update-title" {
					fields[i].value = "xnew"
				}
			}
		}),
		"invalid-task-id": fieldWire(7, func(fields []independentField) {
			for i := range fields {
				if fields[i].name == "task" {
					fields[i].value = task.String()[:len(task.String())-1] + "z"
				}
			}
		}),
		"invalid-context": fieldWire(7, func(fields []independentField) {
			for i := range fields {
				if fields[i].name == "context.0.kind" {
					fields[i].value = "unknown"
				}
			}
		}),
		"invalid-json": fieldWire(5, func(fields []independentField) {
			for i := range fields {
				if fields[i].name == "payload" {
					fields[i].value = "{]"
				}
			}
		}),
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeCanonicalMutation(wire)
			if err == nil {
				t.Fatal("malformed wire accepted")
			}
			assertActionableCanonicalError(t, err)
		})
	}
}

func TestCanonicalRawBoundsAndNamespacedKinds(t *testing.T) {
	task, _, _ := fixtureIDs(t)
	context, _ := TaskContext(task)
	contexts := make([]EventContext, MaxCanonicalContextsPerEffect*64)
	for i := range contexts {
		contexts[i] = context
	}
	_, err := PrepareMutationV1([]Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "fixture.event", Contexts: contexts}})
	if err == nil {
		t.Fatal("duplicate-heavy raw context input accepted")
	}
	assertActionableCanonicalError(t, err)
	var bounded *CanonicalMutationError
	_ = errors.As(err, &bounded)
	if !strings.Contains(bounded.Field, "context-count") {
		t.Fatalf("raw bound ran after context canonicalization: %+v", bounded)
	}
	for _, test := range []struct {
		name   string
		effect Effect
	}{{"decision-empty", Effect{Sort: EffectDecision}}, {"decision-unnamespaced", Effect{Sort: EffectDecision, DecisionKind: "unnamespaced"}}, {"decision-malformed", Effect{Sort: EffectDecision, DecisionKind: "Bad.kind"}}, {"decision-oversized", Effect{Sort: EffectDecision, DecisionKind: DecisionKind(strings.Repeat("x", MaxCanonicalFieldBytes+1))}}, {"evidence-empty", Effect{Sort: EffectEvidence}}, {"evidence-unnamespaced", Effect{Sort: EffectEvidence, EvidenceKind: "unnamespaced"}}, {"evidence-malformed", Effect{Sort: EffectEvidence, EvidenceKind: "bad..kind"}}, {"evidence-oversized", Effect{Sort: EffectEvidence, EvidenceKind: EvidenceKind(strings.Repeat("x", MaxCanonicalFieldBytes+1))}}} {
		t.Run(test.name, func(t *testing.T) {
			_, err := PrepareMutationV1([]Effect{test.effect})
			if err == nil {
				t.Fatal("invalid kind accepted")
			}
			assertActionableCanonicalError(t, err)
		})
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

func TestCanonicalMutationRejectsAggregateFramedSizeBeforeCanonicalAllocation(t *testing.T) {
	task, _, _ := fixtureIDs(t)
	payload := []byte(`"` + strings.Repeat("x", MaxCanonicalFieldBytes-2) + `"`)
	effects := make([]Effect, 9)
	for i := range effects {
		effects[i] = Effect{
			Sort:      EffectTaskEvent,
			TaskID:    task,
			EventKind: "fixture.event",
			Payload:   payload,
		}
	}

	_, err := PrepareMutationV1(effects)
	if err == nil {
		t.Fatal("individually bounded effects with oversized aggregate framing were accepted")
	}
	assertActionableCanonicalError(t, err)
	var bounded *CanonicalMutationError
	if !errors.As(err, &bounded) || bounded.Field != "mutation" {
		t.Fatalf("aggregate error = %#v, want mutation size error", err)
	}
	if !strings.Contains(bounded.Reason, "exact framed size") || !strings.Contains(bounded.Reason, "before canonical byte allocation") {
		t.Fatalf("aggregate error does not identify exact pre-allocation framing bound: %+v", bounded)
	}
}

func TestCanonicalSizeCounterHasExactHardCapAndNoBackingBuffer(t *testing.T) {
	counter := &canonicalSizeCounter{limit: MaxCanonicalMutationBytes}
	if n, err := counter.Write(make([]byte, MaxCanonicalMutationBytes)); err != nil || n != MaxCanonicalMutationBytes {
		t.Fatalf("exact size boundary rejected: n=%d err=%v", n, err)
	}
	if n, err := counter.Write([]byte{'x'}); n != 0 || !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("byte beyond exact boundary: n=%d err=%v, want typed rejection", n, err)
	}
	if counter.size != MaxCanonicalMutationBytes {
		t.Fatalf("counter advanced after overflow: %d", counter.size)
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

func TestCanonicalCodecRegistryIsExactUniqueAndComplete(t *testing.T) {
	if len(canonicalCodecRegistry) != 1 {
		t.Fatalf("codec registry cardinality=%d, want 1", len(canonicalCodecRegistry))
	}
	versions, tags := map[MutationEncodingVersion]bool{}, map[string]bool{}
	for _, descriptor := range canonicalCodecRegistry {
		if descriptor.version == 0 || descriptor.wireTag == "" || descriptor.decoder == nil {
			t.Fatalf("incomplete codec descriptor: %+v", descriptor)
		}
		if versions[descriptor.version] || tags[descriptor.wireTag] {
			t.Fatalf("duplicate codec descriptor: %+v", descriptor)
		}
		versions[descriptor.version], tags[descriptor.wireTag] = true, true
		resolved, ok := inspectMutationEncodingTag(descriptor.wireTag).version()
		if !ok || resolved != descriptor.version || !IsSupportedMutationEncoding(resolved) {
			t.Fatalf("codec descriptor does not round trip: %+v", descriptor)
		}
	}
}

func TestCanonicalV1FamilyRegistryIsExactUniqueCompleteAndRoundTrips(t *testing.T) {
	wantCount := int(EffectTaskCreateAllocated) + 1
	if len(canonicalV1Families) != wantCount {
		t.Fatalf("V1 family cardinality=%d, want %d", len(canonicalV1Families), wantCount)
	}
	sorts, tags := map[EffectSort]bool{}, map[string]bool{}
	for sort := EffectSort(0); int(sort) < wantCount; sort++ {
		tag, ok := mutationV1Codec.familyTag(sort)
		if !ok || tag == "" || sorts[sort] || tags[tag] {
			t.Fatalf("invalid V1 family mapping sort=%d tag=%q ok=%v", sort, tag, ok)
		}
		sorts[sort], tags[tag] = true, true
		decoded, err := mutationV1Codec.parseFamilyTag(tag)
		if err != nil || decoded != sort {
			t.Fatalf("V1 family round trip sort=%d tag=%q decoded=%d err=%v", sort, tag, decoded, err)
		}
	}
}

func TestCanonicalV1FieldRegistriesAreExactUniqueCompleteAndBounded(t *testing.T) {
	assertUnique := func(scope string, refs []canonicalV1FieldRef, want int) {
		t.Helper()
		if len(refs) != want {
			t.Fatalf("%s field cardinality=%d want=%d", scope, len(refs), want)
		}
		names := map[string]bool{}
		for _, ref := range refs {
			name, err := mutationV1Codec.renderFieldName(ref)
			if err != nil || name == "" || names[name] {
				t.Fatalf("%s invalid field name=%q err=%v", scope, name, err)
			}
			names[name] = true
		}
	}
	var envelopeRefs []canonicalV1FieldRef
	for field := envelopeVersion; field <= envelopeEffectCount; field++ {
		envelopeRefs = append(envelopeRefs, envelopeField(field))
	}
	assertUnique("envelope", envelopeRefs, 2)
	var effectRefs []canonicalV1FieldRef
	for field := effectFamily; field <= effectCommentBody; field++ {
		effectRefs = append(effectRefs, effectField(0, field))
	}
	assertUnique("effect", effectRefs, int(effectCommentBody))
	var contextRefs []canonicalV1FieldRef
	for field := contextKind; field <= contextIdentity; field++ {
		contextRefs = append(contextRefs, contextField(0, 0, field))
	}
	assertUnique("context", contextRefs, 2)
	invalid := []canonicalV1FieldRef{
		envelopeField(canonicalEnvelopeField(99)), effectField(-1, effectTask), effectField(MaxCanonicalEffects, effectTask),
		effectField(0, canonicalEffectField(99)), contextField(-1, 0, contextKind), contextField(0, -1, contextKind),
		contextField(0, MaxCanonicalContextsPerEffect, contextKind), contextField(0, 0, canonicalContextField(99)),
		invalidCanonicalV1Ref{},
	}
	for _, ref := range invalid {
		name, err := mutationV1Codec.renderFieldName(ref)
		var typed *CanonicalMutationError
		if err == nil || name != "" || !errors.Is(err, ErrCanonicalMutation) || !errors.As(err, &typed) {
			t.Fatalf("invalid V1 reference rendered name=%q err=%v", name, err)
		}
		if typed.Field == "" || typed.Reason == "" || typed.Fix == "" {
			t.Fatalf("invalid V1 reference returned incomplete actionable error: %+v", typed)
		}
	}
}

func TestCanonicalV2DescriptorExtensionCannotChangeV1(t *testing.T) {
	snapshotV1 := func() [][]byte {
		t.Helper()
		var snapshot [][]byte
		for _, effect := range validFamilyEffects(t) {
			prepared, err := PrepareMutationV1([]Effect{effect})
			if err != nil {
				t.Fatal(err)
			}
			snapshot = append(snapshot, prepared.CanonicalBytes(), prepared.DerivedDigest())
		}
		indexed, err := PrepareMutationV1(validFamilyEffects(t))
		if err != nil {
			t.Fatal(err)
		}
		return append(snapshot, indexed.CanonicalBytes(), indexed.DerivedDigest())
	}
	original := append(canonicalCodecDescriptors(nil), canonicalCodecRegistry...)
	defer func() { canonicalCodecRegistry = original }()
	before := snapshotV1()
	extended := append(canonicalCodecDescriptors(nil), original...)
	v2Called := false
	extended = append(extended, canonicalCodecDescriptor{version: MutationEncodingVersion(2), wireTag: "provenance.mutation.v2", prepare: func(effects []Effect, _ canonicalCodecDescriptor) (CanonicalMutation, error) {
		return CanonicalMutation{version: MutationEncodingVersion(2), effects: append([]Effect(nil), effects...)}, nil
	}, decoder: func(data []byte, version MutationEncodingVersion, _ string) (CanonicalMutation, error) {
		v2Called = true
		return CanonicalMutation{version: version, bytes: append([]byte(nil), data...)}, nil
	}})
	if err := extended.validate(); err != nil {
		t.Fatalf("valid V2 extension rejected: %v", err)
	}
	canonicalCodecRegistry = extended
	if MutationEncodingVersion(2).String() != "provenance.mutation.v2" || !IsSupportedMutationEncoding(MutationEncodingVersion(2)) {
		t.Fatal("public production lookups cannot resolve installed V2 codec")
	}
	v2Prepared, err := prepareCanonicalMutation(MutationEncodingVersion(2), []Effect{{Sort: EffectBootstrapAuthority}})
	if err != nil || v2Prepared.EncodingVersion() != MutationEncodingVersion(2) {
		t.Fatalf("V2 prepare dispatch failed: %+v err=%v", v2Prepared, err)
	}
	v2Bytes := bytes.Replace(before[0], []byte("provenance.mutation.v1"), []byte("provenance.mutation.v2"), 1)
	inspected, err := InspectCanonicalMutationEncodingVersion(v2Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if version, ok := inspected.RegisteredVersion(); !ok || version != MutationEncodingVersion(2) {
		t.Fatalf("V2 inspection lookup failed: %v %v", version, ok)
	}
	v2, err := DecodeCanonicalMutation(v2Bytes)
	if err != nil || !v2Called || v2.EncodingVersion() != MutationEncodingVersion(2) || !bytes.Equal(v2.CanonicalBytes(), v2Bytes) {
		t.Fatalf("production dispatch did not route V2 descriptor: mutation=%+v called=%v err=%v", v2, v2Called, err)
	}
	if during := snapshotV1(); !reflect.DeepEqual(before, during) {
		t.Fatal("installed V2 descriptor changed exhaustive V1 fixtures or digests")
	}
	assertInvalidRegistry := func(registry canonicalCodecDescriptors) {
		t.Helper()
		canonicalCodecRegistry = registry
		err := canonicalCodecRegistry.validate()
		var typed *CanonicalMutationError
		if !errors.Is(err, ErrCanonicalMutation) || !errors.As(err, &typed) || typed.Field == "" || typed.Reason == "" || typed.Fix == "" {
			t.Fatalf("shadow registry returned non-actionable error: %v", err)
		}
		if IsSupportedMutationEncoding(MutationEncodingV1) {
			t.Fatal("support lookup accepted invalid production registry")
		}
		if _, err := PrepareMutationV1(validFamilyEffects(t)); !errors.Is(err, ErrCanonicalMutation) {
			t.Fatalf("prepare accepted invalid registry: %v", err)
		}
		if _, err := DecodeCanonicalMutation(before[0]); !errors.Is(err, ErrCanonicalMutation) {
			t.Fatalf("decode accepted invalid registry: %v", err)
		}
		if _, err := InspectCanonicalMutationEncodingVersion(before[0]); !errors.Is(err, ErrCanonicalMutation) {
			t.Fatalf("inspect accepted invalid registry: %v", err)
		}
		canonicalCodecRegistry = extended
	}
	assertInvalidRegistry(append(append(canonicalCodecDescriptors(nil), extended...), canonicalCodecDescriptor{
		version: MutationEncodingV1, wireTag: "fixture.shadow.v1", prepare: extended[1].prepare, decoder: extended[1].decoder,
	}))
	assertInvalidRegistry(append(append(canonicalCodecDescriptors(nil), extended...), canonicalCodecDescriptor{
		version: MutationEncodingVersion(2), wireTag: MutationEncodingV1.String(), prepare: extended[1].prepare, decoder: extended[1].decoder,
	}))
	canonicalCodecRegistry = original
	if after := snapshotV1(); !reflect.DeepEqual(before, after) {
		t.Fatal("restoring production registry changed exhaustive V1 fixtures or digests")
	}
}
