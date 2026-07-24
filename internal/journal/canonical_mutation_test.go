package journal

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

func v004FixtureInput(t *testing.T) OperationInput {
	t.Helper()
	actor, err := ptypes.ParseActorID("fixture--018f0000-0000-7000-8000-000000000002")
	if err != nil {
		t.Fatal(err)
	}
	return OperationInput{
		Conditions: []Condition{{Kind: ConditionCurrentFact, Selector: FactSelector{Kind: FactEvidence, Filter: FactFilter{TaskScope: FactTaskScope{Kind: FactTaskUnscoped}}, EvidenceKind: "fixture.evidence"}, AssertedJournalID: 0}},
		Effects:    []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "root", BootstrapLabel: "root", OperationAuthorityID: OperationAuthorityID(actor.String())}},
	}
}

func TestMutationV1V004IndependentFixture(t *testing.T) {
	want, err := os.ReadFile("../../testdata/contract/mutation_v1_v004.bin")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Canonicalize(v004FixtureInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prepared.CanonicalBytes(), want) {
		t.Fatalf("evolved V1 bytes drifted\n got %q\nwant %q", prepared.CanonicalBytes(), want)
	}
	if got := sha256.Sum256(want); got != [32]byte{0xe3, 0xea, 0x1b, 0xa0, 0x27, 0xe0, 0x96, 0x04, 0xe3, 0xc7, 0x86, 0x38, 0xc5, 0xbd, 0x54, 0xee, 0xf1, 0xa4, 0xcb, 0x07, 0x71, 0x9b, 0xf8, 0x71, 0x05, 0x4b, 0x65, 0xd3, 0x9a, 0xe1, 0x29, 0xb8} {
		t.Fatalf("fixture digest = %x", got)
	}
	decoded, err := DecodeCanonicalMutation(want)
	if err != nil {
		t.Fatal(err)
	}
	wantInput := v004FixtureInput(t)
	wantDigest := sha256.Sum256(want)
	wantInput.MutationDigest = append([]byte(nil), wantDigest[:]...)
	gotInput := OperationInput{Conditions: decoded.NormalizedConditions(), Effects: decoded.NormalizedEffects(), MutationDigest: decoded.DerivedDigest()}
	if decoded.EncodingVersion() != MutationEncodingV1 || !reflect.DeepEqual(gotInput, wantInput) {
		t.Fatalf("decoded fixture lost complete normalized semantics: got=%#v want=%#v", gotInput, wantInput)
	}
	lines := bytes.Split(bytes.TrimSuffix(want, []byte{'\n'}), []byte{'\n'})
	for i := range lines {
		mutated := append([][]byte(nil), lines...)
		mutated[i] = append([]byte(nil), mutated[i]...)
		mutated[i][0] ^= 1
		wire := bytes.Join(mutated, []byte{'\n'})
		wire = append(wire, '\n')
		if _, err := DecodeCanonicalMutation(wire); !errors.Is(err, ErrCanonicalMutation) {
			t.Fatalf("fixture field %d is not load-bearing: %v", i, err)
		}
	}
}

// TestConditionExactFactCanonicalRoundTrip independently verifies that
// ConditionExactFact (the nonzero kind=1 value) encodes, decodes, and normalizes
// correctly. The v004 fixture uses ConditionCurrentFact (kind=2) only.
func TestConditionExactFactCanonicalRoundTrip(t *testing.T) {
	// Verify ConditionExactFact is nonzero and distinct from ConditionCurrentFact.
	if ConditionExactFact == 0 {
		t.Fatalf("ConditionExactFact must be nonzero; got 0")
	}
	if ConditionExactFact == ConditionCurrentFact {
		t.Fatalf("ConditionExactFact == ConditionCurrentFact; iota shifted incorrectly")
	}
	// Independently author a ConditionExactFact with a known JournalID.
	cond := Condition{
		Kind: ConditionExactFact,
		Selector: FactSelector{
			Kind:         FactDecision,
			Filter:       FactFilter{TaskScope: FactTaskScope{Kind: FactTaskAny}},
			DecisionKind: "fixture.approval",
		},
		AssertedJournalID: 42,
	}
	prepared, err := Canonicalize(OperationInput{
		Conditions: []Condition{cond},
		Effects:    []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "check"}},
	})
	if err != nil {
		t.Fatalf("Canonicalize ExactFact: %v", err)
	}
	decoded, err := DecodeCanonicalMutation(prepared.CanonicalBytes())
	if err != nil {
		t.Fatalf("decode ExactFact round-trip: %v", err)
	}
	got := decoded.NormalizedConditions()
	if len(got) != 1 || got[0].Kind != ConditionExactFact || got[0].AssertedJournalID != 42 {
		t.Fatalf("ExactFact round-trip = %+v; want Kind=ExactFact AssertedJournalID=42", got)
	}
	if got[0].Selector.DecisionKind != "fixture.approval" {
		t.Fatalf("ExactFact DecisionKind = %q; want fixture.approval", got[0].Selector.DecisionKind)
	}
	// Zero ConditionKind must be rejected.
	_, err = Canonicalize(OperationInput{Conditions: []Condition{{Kind: 0, Selector: FactSelector{Kind: FactDecision, Filter: FactFilter{TaskScope: FactTaskScope{Kind: FactTaskAny}}, DecisionKind: "fixture.decision"}}}})
	if !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("zero ConditionKind accepted: %v", err)
	}
}

// TestActivityCreateCanonicalRoundTrip independently verifies that EffectActivityCreate
// encodes, decodes, and normalizes with all required fields (ActivityID, AgentID, Phase,
// Stage, Notes, ResultSlot) and that the canonical bytes round-trip strictly.
// Malformed and irrelevant inputs must fail normalization.
func TestActivityCreateCanonicalRoundTrip(t *testing.T) {
	actID, err := ptypes.ParseActivityID("fixture--018f0000-0000-7000-8000-000000000010")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := ptypes.ParseAgentID("fixture--018f0000-0000-7000-8000-000000000011")
	if err != nil {
		t.Fatal(err)
	}

	completeEffect := Effect{
		Sort:            EffectActivityCreate,
		ResultSlot:      "born",
		ActivityID:      actID,
		ActivityAgentID: agent,
		ActivityPhase:   ptypes.PhaseWorkerSlices,
		ActivityStage:   ptypes.StageInProgress,
		ActivityNotes:   "slice 2",
	}

	// Round-trip: Canonicalize → DecodeCanonicalMutation → NormalizedEffects
	prepared, err := Canonicalize(OperationInput{Effects: []Effect{completeEffect}})
	if err != nil {
		t.Fatalf("Canonicalize ActivityCreate: %v", err)
	}
	decoded, err := DecodeCanonicalMutation(prepared.CanonicalBytes())
	if err != nil {
		t.Fatalf("DecodeCanonicalMutation ActivityCreate: %v", err)
	}
	got := decoded.NormalizedEffects()
	if len(got) != 1 {
		t.Fatalf("NormalizedEffects len = %d; want 1", len(got))
	}
	e := got[0]
	if e.Sort != EffectActivityCreate {
		t.Fatalf("Sort = %v; want EffectActivityCreate", e.Sort)
	}
	if e.ResultSlot != "born" {
		t.Fatalf("ResultSlot = %q; want born", e.ResultSlot)
	}
	if e.ActivityID != actID {
		t.Fatalf("ActivityID = %v; want %v", e.ActivityID, actID)
	}
	if e.ActivityAgentID != agent {
		t.Fatalf("ActivityAgentID = %v; want %v", e.ActivityAgentID, agent)
	}
	if e.ActivityPhase != ptypes.PhaseWorkerSlices {
		t.Fatalf("ActivityPhase = %v; want PhaseWorkerSlices", e.ActivityPhase)
	}
	if e.ActivityStage != ptypes.StageInProgress {
		t.Fatalf("ActivityStage = %v; want StageInProgress", e.ActivityStage)
	}
	if e.ActivityNotes != "slice 2" {
		t.Fatalf("ActivityNotes = %q; want slice 2", e.ActivityNotes)
	}

	// Changing any semantic field changes the canonical bytes.
	for _, change := range []struct {
		name string
		f    func() Effect
	}{
		{"activity-id", func() Effect {
			e2 := completeEffect
			e2.ActivityID, _ = ptypes.ParseActivityID("fixture--018f0000-0000-7000-8000-000000000099")
			return e2
		}},
		{"agent-id", func() Effect {
			e2 := completeEffect
			e2.ActivityAgentID, _ = ptypes.ParseAgentID("fixture--018f0000-0000-7000-8000-000000000099")
			return e2
		}},
		{"phase", func() Effect {
			e2 := completeEffect
			e2.ActivityPhase = ptypes.PhasePropose
			return e2
		}},
		{"stage", func() Effect {
			e2 := completeEffect
			e2.ActivityStage = ptypes.StageComplete
			return e2
		}},
		{"notes", func() Effect {
			e2 := completeEffect
			e2.ActivityNotes = "changed"
			return e2
		}},
		{"result-slot", func() Effect {
			e2 := completeEffect
			e2.ResultSlot = "other"
			return e2
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			other, err := Canonicalize(OperationInput{Effects: []Effect{change.f()}})
			if err != nil {
				t.Fatalf("Canonicalize changed %s: %v", change.name, err)
			}
			if bytes.Equal(prepared.CanonicalBytes(), other.CanonicalBytes()) {
				t.Fatalf("changed %s did not change canonical bytes", change.name)
			}
		})
	}

	// Malformed: missing ResultSlot
	noSlot := completeEffect
	noSlot.ResultSlot = ""
	if _, err := Canonicalize(OperationInput{Effects: []Effect{noSlot}}); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("missing ResultSlot accepted: %v", err)
	}

	// Malformed: zero ActivityID
	zeroID := completeEffect
	zeroID.ActivityID = ActivityID{}
	if _, err := Canonicalize(OperationInput{Effects: []Effect{zeroID}}); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("zero ActivityID accepted: %v", err)
	}

	// Malformed: zero AgentID
	zeroAgent := completeEffect
	zeroAgent.ActivityAgentID = AgentID{}
	if _, err := Canonicalize(OperationInput{Effects: []Effect{zeroAgent}}); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("zero AgentID accepted: %v", err)
	}

	// Malformed: zero Phase (invalid)
	zeroPhase := completeEffect
	zeroPhase.ActivityPhase = Phase(-1) // -1 is out of range, IsValid() returns false
	if _, err := Canonicalize(OperationInput{Effects: []Effect{zeroPhase}}); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("zero Phase accepted: %v", err)
	}

	// Malformed: zero Stage (invalid)
	zeroStage := completeEffect
	zeroStage.ActivityStage = Stage(-1) // -1 is out of range, IsValid() returns false
	if _, err := Canonicalize(OperationInput{Effects: []Effect{zeroStage}}); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("zero Stage accepted: %v", err)
	}

	// Irrelevant field: TaskID must not be set on ActivityCreate
	irrelevant := completeEffect
	task, _ := ptypes.ParseTaskID("fixture--018f0000-0000-7000-8000-000000000020")
	irrelevant.TaskID = task
	if _, err := Canonicalize(OperationInput{Effects: []Effect{irrelevant}}); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("irrelevant TaskID accepted: %v", err)
	}
}

func TestMutationV1RejectsOldShapeAndMalformedFrames(t *testing.T) {
	old := []byte("version:22:provenance.mutation.v1\neffect-count:1:0\n")
	valid, err := os.ReadFile("../../testdata/contract/mutation_v1_v004.bin")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"old-layout":   old,
		"missing":      bytes.Replace(valid, []byte("condition.0.kind:1:2\n"), nil, 1),
		"duplicate":    bytes.Replace(valid, []byte("condition.0.kind:1:2\n"), []byte("condition.0.kind:1:1\ncondition.0.kind:1:1\n"), 1),
		"out-of-order": bytes.Replace(valid, []byte("condition-count:1:1\ncondition.0.kind:1:2\n"), []byte("condition.0.kind:1:1\ncondition-count:1:1\n"), 1),
		"trailing":     append(append([]byte(nil), valid...), 'x'),
		"oversized":    bytes.Repeat([]byte{'x'}, MaxCanonicalMutationBytes+1),
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeCanonicalMutation(wire)
			var typed *CanonicalMutationError
			if !errors.Is(err, ErrCanonicalMutation) || !errors.As(err, &typed) {
				t.Fatalf("error=%v, want actionable canonical error", err)
			}
		})
	}
}

func TestMutationV1BoundsAndIrrelevantFields(t *testing.T) {
	validCondition := v004FixtureInput(t).Conditions[0]
	taskID, _ := ptypes.ParseTaskID("fixture--018f0000-0000-7000-8000-000000000021")
	boundary := v004FixtureInput(t)
	boundary.Conditions = make([]Condition, MaxCanonicalConditions)
	for i := range boundary.Conditions {
		boundary.Conditions[i] = validCondition
	}
	boundary.Effects = make([]Effect, MaxCanonicalEffects)
	for i := range boundary.Effects {
		boundary.Effects[i] = Effect{Sort: EffectBootstrapAuthority}
	}
	if _, err := Canonicalize(boundary); err != nil {
		t.Fatalf("exact condition/effect count boundaries rejected: %v", err)
	}
	in := v004FixtureInput(t)
	in.Conditions = make([]Condition, MaxCanonicalConditions+1)
	if _, err := Canonicalize(in); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("condition bound error=%v", err)
	}
	in = v004FixtureInput(t)
	in.Effects[0].Label = "irrelevant"
	if _, err := Canonicalize(in); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("irrelevant field error=%v", err)
	}
	// EffectTaskCreateAllocated requires a non-empty result slot.
	in = OperationInput{Effects: []Effect{{Sort: EffectTaskCreateAllocated, TaskID: taskID, Title: "t", Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped}}}
	if _, err := Canonicalize(in); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("allocated-create without proof slot error=%v", err)
	}
	in = v004FixtureInput(t)
	in.Effects = append(in.Effects, Effect{Sort: EffectBootstrapAuthority, ResultSlot: "root"}) // duplicate of fixture slot "root"
	if _, err := Canonicalize(in); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("duplicate slot error=%v", err)
	}
	in = v004FixtureInput(t)
	in.Effects[0].ResultSlot = "bad\nslot"
	if _, err := Canonicalize(in); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("control-bearing slot error=%v", err)
	}
	in = OperationInput{Effects: []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: strings.Repeat("x", MaxCanonicalFieldBytes)}}}
	if _, err := Canonicalize(in); err != nil {
		t.Fatalf("exact field boundary rejected: %v", err)
	}
	in.Effects[0].BootstrapLabel += "x"
	if _, err := Canonicalize(in); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("field boundary+1 error=%v", err)
	}
	in = OperationInput{Effects: make([]Effect, 9)}
	for i := range in.Effects {
		in.Effects[i] = Effect{Sort: EffectBootstrapAuthority, BootstrapLabel: strings.Repeat("x", MaxCanonicalFieldBytes)}
	}
	if _, err := Canonicalize(in); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("aggregate boundary+1 error=%v", err)
	}
	in = v004FixtureInput(t)
	in.Conditions[0].Selector.Filter.RequiredContexts = make([]EventContext, MaxCanonicalContextsPerEffect+1)
	if _, err := Canonicalize(in); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("condition context boundary+1 error=%v", err)
	}
	in = v004FixtureInput(t)
	in.Conditions[0].Selector.Filter.OperationIDs = make([]OperationID, MaxFactFilterValues+1)
	if _, err := Canonicalize(in); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("filter value boundary+1 error=%v", err)
	}
}

func TestMutationV1EveryResourceBoundaryIsExact(t *testing.T) {
	validCondition := v004FixtureInput(t).Conditions[0]
	for _, test := range []struct {
		name     string
		at, over OperationInput
	}{
		{"conditions", OperationInput{Conditions: repeatConditions(validCondition, MaxCanonicalConditions)}, OperationInput{Conditions: repeatConditions(validCondition, MaxCanonicalConditions+1)}},
		{"effects", OperationInput{Effects: repeatEffects(Effect{Sort: EffectBootstrapAuthority}, MaxCanonicalEffects)}, OperationInput{Effects: repeatEffects(Effect{Sort: EffectBootstrapAuthority}, MaxCanonicalEffects+1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Canonicalize(test.at); err != nil {
				t.Fatalf("at limit: %v", err)
			}
			if _, err := Canonicalize(test.over); !errors.Is(err, ErrCanonicalMutation) {
				t.Fatalf("limit+1: %v", err)
			}
		})
	}

	task, _ := ptypes.ParseTaskID("fixture--018f0000-0000-7000-8000-000000000021")
	actor, _ := ptypes.ParseActorID("fixture--018f0000-0000-7000-8000-000000000022")
	context, _ := TaskContext(task)
	dimensions := []struct {
		name string
		set  func(*Condition, int)
	}{
		{"contexts", func(c *Condition, n int) {
			c.Selector.Filter.RequiredContexts = make([]EventContext, n)
			for i := range c.Selector.Filter.RequiredContexts {
				c.Selector.Filter.RequiredContexts[i] = context
			}
		}},
		{"actors", func(c *Condition, n int) {
			c.Selector.Filter.EffectiveActorIDs = make([]ActorID, n)
			for i := range c.Selector.Filter.EffectiveActorIDs {
				c.Selector.Filter.EffectiveActorIDs[i] = actor
			}
		}},
		{"operations", func(c *Condition, n int) {
			c.Selector.Filter.OperationIDs = make([]OperationID, n)
			for i := range c.Selector.Filter.OperationIDs {
				c.Selector.Filter.OperationIDs[i] = "fixture-operation"
			}
		}},
	}
	for _, test := range dimensions {
		t.Run(test.name, func(t *testing.T) {
			at := validCondition
			test.set(&at, MaxFactFilterValues)
			if test.name == "contexts" {
				test.set(&at, MaxCanonicalContextsPerEffect)
			}
			if _, err := Canonicalize(OperationInput{Conditions: []Condition{at}}); err != nil {
				t.Fatalf("at limit: %v", err)
			}
			over := validCondition
			limit := MaxFactFilterValues
			if test.name == "contexts" {
				limit = MaxCanonicalContextsPerEffect
			}
			test.set(&over, limit+1)
			if _, err := Canonicalize(OperationInput{Conditions: []Condition{over}}); !errors.Is(err, ErrCanonicalMutation) {
				t.Fatalf("limit+1: %v", err)
			}
		})
	}

	effects := repeatEffects(Effect{Sort: EffectBootstrapAuthority, BootstrapLabel: strings.Repeat("x", MaxCanonicalFieldBytes)}, 7)
	effects = append(effects, Effect{Sort: EffectBootstrapAuthority})
	var prepared CanonicalMutation
	low, high := 0, MaxCanonicalFieldBytes
	for low <= high {
		middle := low + (high-low)/2
		effects[7].BootstrapLabel = strings.Repeat("x", middle)
		candidate, candidateErr := Canonicalize(OperationInput{Effects: effects})
		if candidateErr != nil {
			high = middle - 1
			continue
		}
		prepared = candidate
		low = middle + 1
	}
	if delta := MaxCanonicalMutationBytes - len(prepared.CanonicalBytes()); delta > 0 {
		effects[7].BootstrapLabel = strings.Repeat("x", high)
		effects[7].OperationAuthorityID = OperationAuthorityID(strings.Repeat("y", delta))
		exact, exactErr := Canonicalize(OperationInput{Effects: effects})
		if exactErr != nil {
			t.Fatalf("construct exact aggregate boundary: %v", exactErr)
		}
		prepared = exact
	}
	if len(prepared.CanonicalBytes()) != MaxCanonicalMutationBytes {
		t.Fatalf("aggregate exact bytes=%d want=%d", len(prepared.CanonicalBytes()), MaxCanonicalMutationBytes)
	}
	if _, err := DecodeCanonicalMutation(prepared.CanonicalBytes()); err != nil {
		t.Fatalf("decode exact aggregate boundary: %v", err)
	}
	if _, err := DecodeCanonicalMutation(append(prepared.CanonicalBytes(), 'x')); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("decode aggregate limit+1: %v", err)
	}
	effects[7].OperationAuthorityID += "x"
	if _, err := Canonicalize(OperationInput{Effects: effects}); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("aggregate limit+1: %v", err)
	}
}

func repeatConditions(value Condition, count int) []Condition {
	out := make([]Condition, count)
	for i := range out {
		out[i] = value
	}
	return out
}
func repeatEffects(value Effect, count int) []Effect {
	out := make([]Effect, count)
	for i := range out {
		out[i] = value
	}
	return out
}

func TestMutationV1RejectsMalformedUnionOperands(t *testing.T) {
	task, _ := ptypes.ParseTaskID("fixture--018f0000-0000-7000-8000-000000000001")
	context, _ := TaskContext(task)
	cases := map[string]OperationInput{
		"unknown-condition":    {Conditions: []Condition{{Kind: ConditionKind(99), Selector: FactSelector{Kind: FactEvidence, Filter: FactFilter{TaskScope: FactTaskScope{Kind: FactTaskUnscoped}}, EvidenceKind: "fixture.evidence"}}}},
		"invalid-scope-arm":    {Conditions: []Condition{{Kind: ConditionCurrentFact, Selector: FactSelector{Kind: FactEvidence, Filter: FactFilter{TaskScope: FactTaskScope{Kind: FactTaskAny, TaskID: task}}, EvidenceKind: "fixture.evidence"}}}},
		"invalid-selector-arm": {Conditions: []Condition{{Kind: ConditionCurrentFact, Selector: FactSelector{Kind: FactDecision, Filter: FactFilter{TaskScope: FactTaskScope{Kind: FactTaskAny}}, DecisionKind: "fixture.decision", EvidenceKind: "fixture.evidence"}}}},
		"invalid-json":         {Effects: []Effect{{Sort: EffectDecision, DecisionKind: "fixture.decision", Payload: []byte(`{"a":`)}}},
		"duplicate-json":       {Effects: []Effect{{Sort: EffectDecision, DecisionKind: "fixture.decision", Payload: []byte(`{"a":1,"a":2}`)}}},
		"invalid-context":      {Effects: []Effect{{Sort: EffectDecision, DecisionKind: "fixture.decision", Contexts: []EventContext{{}}}}},
		"irrelevant-context":   {Effects: []Effect{{Sort: EffectBootstrapAuthority, Contexts: []EventContext{context}}}},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Canonicalize(input); !errors.Is(err, ErrCanonicalMutation) {
				t.Fatalf("error=%v, want ErrCanonicalMutation", err)
			}
		})
	}
	valid, err := os.ReadFile("../../testdata/contract/mutation_v1_v004.bin")
	if err != nil {
		t.Fatal(err)
	}
	malformedOptional := bytes.Replace(valid, []byte("effect.0.recorded-at-override:1:0\n"), []byte("effect.0.recorded-at-override:1:x\n"), 1)
	if _, err := DecodeCanonicalMutation(malformedOptional); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("invalid optional marker error=%v", err)
	}
}

func TestDecisionEvidenceContextsAreSemantic(t *testing.T) {
	task, _ := ptypes.ParseTaskID("fixture--018f0000-0000-7000-8000-000000000001")
	ctx, _ := TaskContext(task)
	for _, effect := range []Effect{{Sort: EffectDecision, DecisionKind: "fixture.decision", Contexts: []EventContext{ctx}}, {Sort: EffectEvidence, EvidenceKind: "fixture.evidence", Contexts: []EventContext{ctx}}} {
		with, err := Canonicalize(OperationInput{Effects: []Effect{effect}})
		if err != nil {
			t.Fatal(err)
		}
		effect.Contexts = nil
		without, err := Canonicalize(OperationInput{Effects: []Effect{effect}})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(with.CanonicalBytes(), without.CanonicalBytes()) || !strings.Contains(string(with.CanonicalBytes()), "context.0.identity") {
			t.Fatalf("contexts absent from %s identity", effect.Sort)
		}
	}
}
