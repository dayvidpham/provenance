package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

func assignmentAssertionFixture(t *testing.T) AssignmentActiveAssertion {
	t.Helper()
	task, err := ptypes.ParseTaskID("fixture/018f0000-0000-7000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := ptypes.ParseActorID("fixture/018f0000-0000-7000-8000-000000000002")
	if err != nil {
		t.Fatal(err)
	}
	parent := AssignmentID("parent")
	return AssignmentActiveAssertion{
		AssignmentID: "action", TaskID: task, SlotID: SlotOwnerResponsibility,
		Occupant: actor, AuthorityJournalID: 42, ParentAssignmentID: &parent,
	}
}

func TestAssignmentActiveCanonicalClosedArms(t *testing.T) {
	valid := assignmentAssertionFixture(t)
	for _, test := range []struct {
		name   string
		change func(*Condition)
	}{
		{"missing-arm", func(c *Condition) { c.AssignmentActive = nil }},
		{"fact-kind", func(c *Condition) { c.Selector.Kind = FactEvidence }},
		{"fact-decision", func(c *Condition) { c.Selector.DecisionKind = "test.fact" }},
		{"fact-evidence", func(c *Condition) { c.Selector.EvidenceKind = "test.fact" }},
		{"fact-task", func(c *Condition) {
			c.Selector.Filter.TaskScope = FactTaskScope{Kind: FactTaskExact, TaskID: valid.TaskID}
		}},
		{"fact-actor", func(c *Condition) { c.Selector.Filter.EffectiveActorIDs = []ActorID{valid.Occupant} }},
		{"fact-operation", func(c *Condition) { c.Selector.Filter.OperationIDs = []OperationID{"other"} }},
		{"fact-journal", func(c *Condition) { c.AssertedJournalID = 42 }},
		{"exact-fact-mixed", func(c *Condition) {
			c.Kind = ConditionExactFact
			c.Selector = FactSelector{Kind: FactDecision, DecisionKind: "test.fact"}
			c.AssertedJournalID = 42
		}},
		{"current-fact-mixed", func(c *Condition) {
			c.Kind = ConditionCurrentFact
			c.Selector = FactSelector{Kind: FactEvidence, EvidenceKind: "test.fact"}
		}},
		{"unknown-kind", func(c *Condition) { c.Kind = 99 }},
		{"empty-assignment", func(c *Condition) { c.AssignmentActive.AssignmentID = "" }},
		{"control-assignment", func(c *Condition) { c.AssignmentActive.AssignmentID = "bad\n" }},
		{"empty-task", func(c *Condition) { c.AssignmentActive.TaskID = TaskID{} }},
		{"empty-occupant", func(c *Condition) { c.AssignmentActive.Occupant = ActorID{} }},
		{"empty-slot", func(c *Condition) { c.AssignmentActive.SlotID = "" }},
		{"unknown-slot", func(c *Condition) { c.AssignmentActive.SlotID = "axis-reviewer" }},
		{"zero-authority", func(c *Condition) { c.AssignmentActive.AuthorityJournalID = 0 }},
		{"negative-authority", func(c *Condition) { c.AssignmentActive.AuthorityJournalID = -1 }},
		{"empty-parent", func(c *Condition) { *c.AssignmentActive.ParentAssignmentID = "" }},
		{"self-parent", func(c *Condition) { *c.AssignmentActive.ParentAssignmentID = c.AssignmentActive.AssignmentID }},
	} {
		t.Run(test.name, func(t *testing.T) {
			condition := AssignmentActiveCondition(valid)
			test.change(&condition)
			if _, err := Canonicalize(OperationInput{Conditions: []Condition{condition}}); !errors.Is(err, ErrCanonicalMutation) {
				t.Fatalf("malformed condition was not refused: %v", err)
			}
		})
	}
}

func TestAssignmentActiveCanonicalStrictWire(t *testing.T) {
	a := assignmentAssertionFixture(t)
	prepared, err := Canonicalize(OperationInput{Conditions: []Condition{AssignmentActiveCondition(a)}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := encodeAssignmentAssertion(a)
	if err != nil {
		t.Fatal(err)
	}
	frame := func(value string) string {
		return fmt.Sprintf("condition.0.assignment-active:%d:%s\n", len(value), value)
	}
	original := prepared.CanonicalBytes()
	for _, field := range []string{"assignment_id", "task_id", "slot_id", "occupant", "authority_journal_id", "parent_assignment_id"} {
		for _, shape := range []string{"missing", "null"} {
			t.Run(field+"-"+shape, func(t *testing.T) {
				var members map[string]json.RawMessage
				if err := json.Unmarshal(payload, &members); err != nil {
					t.Fatal(err)
				}
				if shape == "missing" {
					delete(members, field)
				} else {
					members[field] = json.RawMessage("null")
				}
				changed, err := json.Marshal(members)
				if err != nil {
					t.Fatal(err)
				}
				wire := bytes.Replace(original, []byte(frame(string(payload))), []byte(frame(string(changed))), 1)
				decoded, err := DecodeCanonicalMutation(wire)
				if field == "parent_assignment_id" && shape == "null" {
					if err != nil || decoded.NormalizedConditions()[0].AssignmentActive.ParentAssignmentID != nil {
						t.Fatalf("explicit null parent is the exact no-parent arm: %v", err)
					}
				} else if !errors.Is(err, ErrCanonicalMutation) {
					t.Fatalf("missing/null required member accepted: %v", err)
				}
			})
		}
	}
	for _, test := range []struct {
		name    string
		payload string
	}{
		{"missing-parent", strings.Replace(string(payload), `"parent_assignment_id":"parent",`, "", 1)},
		{"null-identity", strings.Replace(string(payload), `"assignment_id":"action"`, `"assignment_id":null`, 1)},
		{"unknown-member", strings.Replace(string(payload), `{`, `{"unknown":true,`, 1)},
		{"duplicate-member", strings.Replace(string(payload), `{`, `{"assignment_id":"action",`, 1)},
		{"mixed-fact", strings.Replace(string(payload), `{`, `{"fact_kind":1,`, 1)},
		{"trailing-value", string(payload) + `{}`},
		{"null-arm", `null`},
		{"array-arm", `[]`},
		{"invalid-parent", strings.Replace(string(payload), `"parent_assignment_id":"parent"`, `"parent_assignment_id":{}`, 1)},
		{"fraction-authority", strings.Replace(string(payload), `"authority_journal_id":42`, `"authority_journal_id":42.0`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.payload == string(payload) {
				t.Fatal("wire mutation missed its exact member")
			}
			wire := bytes.Replace(original, []byte(frame(string(payload))), []byte(frame(test.payload)), 1)
			if bytes.Equal(wire, original) {
				t.Fatal("wire mutation missed its frame")
			}
			if _, err := DecodeCanonicalMutation(wire); !errors.Is(err, ErrCanonicalMutation) {
				t.Fatalf("invalid assignment wire accepted: %v", err)
			}
		})
	}
	for _, test := range []struct {
		name string
		wire []byte
	}{
		{"v2-arm-in-v1", bytes.Replace(original, []byte("provenance.mutation.v2"), []byte("provenance.mutation.v1"), 1)},
		{"unknown-version", bytes.Replace(original, []byte("provenance.mutation.v2"), []byte("provenance.mutation.v9"), 1)},
		{"duplicate-frame", append(append([]byte(nil), original...), []byte(frame(string(payload)))...)},
		{"missing-frame", bytes.Replace(original, []byte(frame(string(payload))), nil, 1)},
		{"unknown-frame", bytes.Replace(original, []byte("assignment-active:"), []byte("fact-kind:"), 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeCanonicalMutation(test.wire); !errors.Is(err, ErrCanonicalMutation) {
				t.Fatalf("closed wire admitted malformed frame/version: %v", err)
			}
		})
	}
	legacy, err := Canonicalize(v004FixtureInput(t))
	if err != nil {
		t.Fatal(err)
	}
	promoted := bytes.Replace(legacy.CanonicalBytes(), []byte("provenance.mutation.v1"), []byte("provenance.mutation.v2"), 1)
	if _, err := DecodeCanonicalMutation(promoted); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("fact-only operation must not be silently promoted: %v", err)
	}
	// The actual V1-only decoder remains a useful old-reader refusal witness.
	if _, err := decodeCanonicalMutationV1(original, MutationEncodingV1, canonicalMutationV1WireTag); err == nil {
		t.Fatal("V1-only reader admitted V2 history")
	}
}

func TestAssignmentActiveMixedConditionsAndBound(t *testing.T) {
	input := v004FixtureInput(t)
	input.Conditions = append(input.Conditions, AssignmentActiveCondition(assignmentAssertionFixture(t)))
	prepared, err := Canonicalize(input)
	if err != nil || prepared.EncodingVersion() != MutationEncodingV2 {
		t.Fatalf("mixed condition set must use V2: %v", err)
	}
	if !reflect.DeepEqual(prepared.NormalizedConditions(), input.Conditions) {
		t.Fatal("mixed conditions lost their order or arm")
	}
	input.Conditions = repeatConditions(input.Conditions[1], MaxCanonicalConditions+1)
	if _, err := Canonicalize(input); !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("assignment conditions bypassed the condition count bound: %v", err)
	}
}

func TestAssignmentActiveCanonicalRoundTrip(t *testing.T) {
	a := assignmentAssertionFixture(t)
	condition := AssignmentActiveCondition(a)
	prepared, err := Canonicalize(OperationInput{Conditions: []Condition{condition}})
	if err != nil {
		t.Fatalf("prepare exact assignment assertion: %v", err)
	}
	if prepared.EncodingVersion().String() != "provenance.mutation.v2" {
		t.Fatalf("assignment condition must select its own wire version: %s", prepared.EncodingVersion())
	}
	golden, err := os.ReadFile("../../testdata/contract/mutation_v2_assignment.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(golden, prepared.CanonicalBytes()) || fmt.Sprintf("%x", sha256.Sum256(golden)) != "43f7574db457ad916ad670b9c50a9c1b695e29fe0881c18dab42a182fab5ed20" {
		t.Fatal("independent V2 assignment bytes or digest drifted")
	}
	decoded, err := DecodeCanonicalMutation(prepared.CanonicalBytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.NormalizedConditions(); !reflect.DeepEqual(got, []Condition{condition}) {
		t.Fatalf("assignment assertion lost identity: %+v", got)
	}
	copy := decoded.NormalizedConditions()
	*copy[0].AssignmentActive.ParentAssignmentID = "changed"
	if !reflect.DeepEqual(decoded.NormalizedConditions(), []Condition{condition}) {
		t.Fatal("normalized assertion aliases retained parent identity")
	}
}
