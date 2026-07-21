package journal

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

func canonicalFixtureEffects(t *testing.T) []Effect {
	t.Helper()
	task, _ := ptypes.ParseTaskID("fixture--018f0000-0000-7000-8000-000000000001")
	actor, _ := ptypes.ParseActorID("fixture--018f0000-0000-7000-8000-000000000002")
	comment, _ := ptypes.ParseCommentID("fixture--018f0000-0000-7000-8000-000000000003")
	activity, _ := ptypes.ParseActivityID("fixture--018f0000-0000-7000-8000-000000000004")
	taskContext, _ := TaskContext(task)
	actorContext, _ := ActorContext(actor)
	activityContext, _ := ActivityContext(activity)
	gitContext, _ := GitContext("0123456789abcdef0123456789abcdef01234567")
	updatedTitle, updatedDescription, updatedNotes := "updated title", "updated description", "updated notes"
	updatedPriority, updatedPhase := ptypes.PriorityHigh, ptypes.PhaseCodeReview
	override := RecordedTime(123456789)
	base := Effect{
		ResultSlot: "slot", RecordedAtOverride: &override, TaskID: task,
		Payload: json.RawMessage(`{"z":2,"a":1}`), Contexts: []EventContext{gitContext, taskContext, actorContext, activityContext},
		Title: "title", Description: "description", Type: ptypes.TaskTypeFeature,
		Priority: ptypes.PriorityHigh, Phase: ptypes.PhaseWorkerSlices,
		CloseReason: "complete", UpdateTitle: &updatedTitle, UpdateDescription: &updatedDescription,
		UpdatePriority: &updatedPriority, UpdatePhase: &updatedPhase, UpdateNotes: &updatedNotes,
		Forced: true, BootstrapLabel: "root", OperationAuthorityID: "authority-1",
		AssignmentID: "assignment-1", SlotID: SlotOwnerResponsibility, Occupant: actor,
		Predecessor: "assignment-0", Parent: "assignment-parent", DecisionKind: "fixture.decision",
		EvidenceKind: "fixture.evidence", ContentDigest: []byte{0, 1, 2, 255},
		EdgeTargetID: task.String(), EdgeRelKind: ptypes.EdgeDerivedFrom, Label: "fixture-label",
		CommentIdentity: comment, CommentAuthor: actor, CommentBody: "body\x00with delimiter-like :12:data",
	}
	sorts := append([]EffectSort(nil), canonicalEffectSorts...)
	effects := make([]Effect, len(sorts))
	for i, sort := range sorts {
		effects[i] = base
		effects[i].Sort = sort
		effects[i].EventKind = "fixture.event"
		effects[i].ResultSlot = ResultSlotID(sort.String())
	}
	return effects
}

func TestCanonicalMutationV1GoldenAndRoundTripAllFamilies(t *testing.T) {
	prepared, err := PrepareMutationV1(canonicalFixtureEffects(t))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCanonicalMutation(prepared.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Effects, prepared.Effects) {
		t.Fatalf("decode round trip changed effects\n got: %#v\nwant: %#v", decoded.Effects, prepared.Effects)
	}
	if len(decoded.Effects) != len(canonicalEffectSorts) {
		t.Fatalf("consumed %d families, want %d", len(decoded.Effects), len(canonicalEffectSorts))
	}
	for i, sort := range canonicalEffectSorts {
		if decoded.Effects[i].Sort != sort {
			t.Fatalf("family %d = %s, want %s", i, decoded.Effects[i].Sort, sort)
		}
	}

	fixtureBytes, err := os.ReadFile("testdata/canonical_mutation_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct{ Version, SHA256 string }
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	got := hex.EncodeToString(prepared.Digest)
	if fixture.Version != MutationEncodingV1 || fixture.SHA256 != got {
		t.Fatalf("canonical golden drift: version=%q sha256=%q; want fixture version=%q sha256=%q", MutationEncodingV1, got, fixture.Version, fixture.SHA256)
	}
}

func TestCanonicalMutationV1EveryOperandIsConsumed(t *testing.T) {
	baseline := canonicalFixtureEffects(t)
	prepared, err := PrepareMutationV1(baseline)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name  string
		apply func(*Effect)
	}{
		{"result-slot", func(e *Effect) { e.ResultSlot = "changed" }}, {"recorded-at", func(e *Effect) { v := RecordedTime(9); e.RecordedAtOverride = &v }},
		{"task", func(e *Effect) { e.TaskID.Namespace = "other" }}, {"event-kind", func(e *Effect) { e.EventKind = "fixture.changed" }},
		{"payload", func(e *Effect) { e.Payload = json.RawMessage(`{"changed":true}`) }}, {"contexts", func(e *Effect) { e.Contexts = e.Contexts[:1] }},
		{"title", func(e *Effect) { e.Title += "x" }}, {"description", func(e *Effect) { e.Description += "x" }}, {"type", func(e *Effect) { e.Type = ptypes.TaskTypeBug }},
		{"priority", func(e *Effect) { e.Priority = ptypes.PriorityLow }}, {"phase", func(e *Effect) { e.Phase = ptypes.PhaseLanding }}, {"close-reason", func(e *Effect) { e.CloseReason += "x" }},
		{"update-title", func(e *Effect) { v := "x"; e.UpdateTitle = &v }}, {"update-description", func(e *Effect) { v := "x"; e.UpdateDescription = &v }},
		{"update-priority", func(e *Effect) { v := ptypes.PriorityLow; e.UpdatePriority = &v }}, {"update-phase", func(e *Effect) { v := ptypes.PhaseLanding; e.UpdatePhase = &v }},
		{"update-notes", func(e *Effect) { v := "x"; e.UpdateNotes = &v }}, {"forced", func(e *Effect) { e.Forced = !e.Forced }},
		{"bootstrap-label", func(e *Effect) { e.BootstrapLabel += "x" }}, {"operation-authority", func(e *Effect) { e.OperationAuthorityID += "x" }},
		{"assignment", func(e *Effect) { e.AssignmentID += "x" }}, {"slot", func(e *Effect) { e.SlotID = "other" }}, {"occupant", func(e *Effect) { e.Occupant.Namespace = "other" }},
		{"predecessor", func(e *Effect) { e.Predecessor += "x" }}, {"parent", func(e *Effect) { e.Parent += "x" }}, {"decision", func(e *Effect) { e.DecisionKind += "x" }},
		{"evidence", func(e *Effect) { e.EvidenceKind += "x" }}, {"content-digest", func(e *Effect) { e.ContentDigest = []byte("x") }},
		{"edge-target", func(e *Effect) { e.EdgeTargetID += "x" }}, {"edge-kind", func(e *Effect) { e.EdgeRelKind = ptypes.EdgeBlockedBy }}, {"label", func(e *Effect) { e.Label += "x" }},
		{"comment", func(e *Effect) { e.CommentIdentity.Namespace = "other" }}, {"comment-author", func(e *Effect) { e.CommentAuthor.Namespace = "other" }}, {"comment-body", func(e *Effect) { e.CommentBody += "x" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := append([]Effect(nil), baseline...)
			mutation.apply(&changed[0])
			candidate, err := PrepareMutationV1(changed)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(prepared.Bytes, candidate.Bytes) {
				t.Fatalf("operand %s was not consumed", mutation.name)
			}
		})
	}
}

func TestDecodeCanonicalMutationStrictFraming(t *testing.T) {
	prepared, err := PrepareMutationV1(nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"unknown-version": bytes.Replace(prepared.Bytes, []byte(MutationEncodingV1), []byte("provenance.mutation.v2"), 1),
		"missing":         prepared.Bytes[:len(prepared.Bytes)-1],
		"duplicate":       append(append([]byte(nil), prepared.Bytes...), prepared.Bytes...),
		"trailing":        append(append([]byte(nil), prepared.Bytes...), 'x'),
		"unknown-field":   []byte(strings.Replace(string(prepared.Bytes), "effect-count", "unknown-field", 1)),
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCanonicalMutation(wire); err == nil {
				t.Fatal("malformed canonical mutation was accepted")
			}
		})
	}
}

func TestCanonicalJSONRejectsDuplicateAndTrailingFields(t *testing.T) {
	for _, payload := range []string{`{"x":1,"x":2}`, `{"x":1} {"y":2}`} {
		if _, err := PrepareMutationV1([]Effect{{Sort: EffectTaskEvent, Payload: json.RawMessage(payload)}}); err == nil {
			t.Fatalf("accepted payload %s", payload)
		}
	}
}
