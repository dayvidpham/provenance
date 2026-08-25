package allocation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

func TestRecoveredResultWireRejectsAmbiguousAndMalformedJSON(t *testing.T) {
	operation := journal.OperationID("result-wire-fixture")
	task := ptypes.TaskID{Namespace: "fixture", UUID: uuid.MustParse("018f0000-0000-7000-8000-000000000020")}
	actor := ptypes.ActorID{Namespace: "fixture", UUID: uuid.MustParse("018f0000-0000-7000-8000-000000000021")}
	closure := NewClosure(operation, RequestKindAllocation, 10, []ChildBinding{{
		Ordinal: 0, TaskID: task, AssignmentID: "assignment-1", Occupant: actor,
		TaskRow:       ProducedRow{OperationID: operation, EffectOrdinal: 0, Subordinal: 0, JournalID: 11},
		AssignmentRow: ProducedRow{OperationID: operation, EffectOrdinal: 0, Subordinal: 1, JournalID: 12},
	}})
	valid, err := json.Marshal(closure)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := [][]byte{
		append(append([]byte(nil), valid...), []byte(` {}`)...),
		[]byte(`{"version":1,"version":1,"operationID":"result-wire-fixture","kind":2,"anchor":10,"children":[]}`),
		[]byte(`{"version":1,"operationID":"result-wire-fixture","kind":2,"anchor":10,"children":[],"unknown":true}`),
	}
	for _, fixture := range fixtures {
		var decoded OperationClosure
		if err := json.Unmarshal(fixture, &decoded); err == nil {
			t.Fatalf("accepted corrupt closure wire: %s", fixture)
		}
	}
}

func TestRecoveredResultWireBoundAdmitsLargestCanonicalResultAndRejectsCapPlusOne(t *testing.T) {
	operation := journal.OperationID("largest-canonical-result")
	actor := ptypes.ActorID{Namespace: "fixture", UUID: uuid.MustParse("018f0000-0000-7000-8000-000000000021")}
	children := make([]ChildBinding, MaxChildren)
	for index := range children {
		task := ptypes.TaskID{Namespace: "fixture", UUID: uuid.MustParse(fmt.Sprintf("018f0000-0000-7000-8000-%012x", index+1))}
		children[index] = ChildBinding{
			Ordinal: index, TaskID: task, AssignmentID: journal.AssignmentID(strings.Repeat("a", maxAssignmentIDBytes-4) + fmt.Sprintf("%04d", index)), Occupant: actor,
			TaskRow:       ProducedRow{OperationID: operation, EffectOrdinal: index, Subordinal: 0, JournalID: journal.JournalID(2*index + 2)},
			AssignmentRow: ProducedRow{OperationID: operation, EffectOrdinal: index, Subordinal: 1, JournalID: journal.JournalID(2*index + 3)},
		}
	}
	events := make([]journal.JournalID, journal.MaxCanonicalEffects)
	slots := make([]journal.ResultSlotBinding, journal.MaxCanonicalEffects)
	for index := range events {
		events[index] = journal.JournalID(1000 + index)
		slots[index] = journal.ResultSlotBinding{Slot: journal.ResultSlotID(fmt.Sprintf("slot-%03d", index)), ProducedJournalID: journal.JournalID(1000 + index), Kind: journal.JournalKindEvidence}
	}
	largest := NewComposedResult(NewClosure(operation, RequestKindAllocation, 1, children), journal.CommittedResult{EmittedEvents: events, ResultSlots: slots})
	wire, err := json.Marshal(largest)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ComposedResult
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("largest accepted canonical result (%d bytes) was rejected: %v", len(wire), err)
	}

	tooLarge := make([]byte, maxResultWireBytes+1)
	for index := range tooLarge {
		tooLarge[index] = ' '
	}
	if err := decodeStrictResultWire(tooLarge, &decoded); err == nil {
		t.Fatalf("accepted result wire at private cap+1 (%d bytes)", len(tooLarge))
	}
}

func TestRecoveredComposedResultWireRejectsUnsortedEventsAndSlots(t *testing.T) {
	for _, fixture := range [][]byte{
		[]byte(`{"version":1,"closure":{"version":1,"operationID":"result-wire-fixture","kind":2,"anchor":10,"children":[{"Ordinal":0,"TaskID":{"Namespace":"fixture","UUID":"018f0000-0000-7000-8000-000000000020"},"AssignmentID":"assignment-1","Occupant":{"Namespace":"fixture","UUID":"018f0000-0000-7000-8000-000000000021"},"TaskRow":{"OperationID":"result-wire-fixture","EffectOrdinal":0,"Subordinal":0,"JournalID":11},"AssignmentRow":{"OperationID":"result-wire-fixture","EffectOrdinal":0,"Subordinal":1,"JournalID":12}}]},"emittedEvents":[20,19],"resultSlots":[]}`),
	} {
		var decoded ComposedResult
		if err := json.Unmarshal(fixture, &decoded); err == nil {
			t.Fatal("accepted unsorted composed result wire")
		}
	}
}
