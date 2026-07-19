package provenance

// dbos_internal_test.go is a white-box suite over the adapter's unexported
// canonicalization, fingerprint, and step-outcome encoding (issue #6). It lives in
// package provenance because those seams are deliberately unexported (no dual
// export): the deterministic identity and closed encoding are internal contracts.

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

func testTaskID(t *testing.T) ptypes.TaskID {
	t.Helper()
	return ptypes.TaskID{Namespace: "aura", UUID: uuid.Must(uuid.NewV7())}
}

func testActorID(t *testing.T) ptypes.ActorID {
	t.Helper()
	return ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}
}

// richOperationInput builds an input touching every wire field family: a task
// create, a task_event carrying typed contexts and materialized updates, an
// assignment start, and a decision.
func richOperationInput(t *testing.T) journal.OperationInput {
	t.Helper()
	auth := journal.JournalID(7)
	taskID := testTaskID(t)
	tctx, err := TaskContext(taskID)
	if err != nil {
		t.Fatalf("TaskContext: %v", err)
	}
	gctx, err := GitContext("0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatalf("GitContext: %v", err)
	}
	newTitle := "renamed"
	return journal.OperationInput{
		OperationID:        "op-rich",
		ActorID:            testActorID(t),
		AuthorityJournalID: &auth,
		CommandDigest:      []byte("cmd-rich"),
		MutationDigest:     []byte("mut-rich"),
		RecordedAt:         1234567890,
		Effects: []journal.Effect{
			{
				Sort: EffectTaskCreate, ResultSlot: "task", TaskID: taskID,
				Title: "orig", Description: "d", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseWorkerSlices,
			},
			{
				Sort: EffectTaskEvent, TaskID: taskID, EventKind: EventKindTaskUpdated,
				Payload: json.RawMessage(`{"k":"v"}`), Contexts: []EventContext{tctx, gctx},
				UpdateTitle: &newTitle,
			},
			{
				Sort: EffectAssignmentStart, AssignmentID: "asg-1", SlotID: SlotOwnerResponsibility,
				Occupant: testActorID(t),
			},
			{
				Sort: EffectDecision, DecisionKind: "pasture.decision.ratify", ContentDigest: []byte{0x01, 0x02},
			},
		},
	}
}

func TestWire_RoundTripIsStable(t *testing.T) {
	in := richOperationInput(t)
	enc1, err := encodeApplyInput(in)
	if err != nil {
		t.Fatalf("encodeApplyInput: %v", err)
	}
	if enc1.Schema != DBOSApplyInputSchemaV1 {
		t.Errorf("schema = %q, want %q", enc1.Schema, DBOSApplyInputSchemaV1)
	}
	dec, err := decodeApplyInput(enc1)
	if err != nil {
		t.Fatalf("decodeApplyInput: %v", err)
	}
	// Identity survived.
	if dec.OperationID != in.OperationID || dec.ActorID != in.ActorID {
		t.Errorf("identity drifted: %q/%v", dec.OperationID, dec.ActorID)
	}
	if dec.AuthorityJournalID == nil || *dec.AuthorityJournalID != *in.AuthorityJournalID {
		t.Errorf("authority drifted: %v", dec.AuthorityJournalID)
	}
	if len(dec.Effects) != len(in.Effects) {
		t.Fatalf("effect count %d, want %d", len(dec.Effects), len(in.Effects))
	}
	if dec.Effects[1].EventKind != EventKindTaskUpdated || len(dec.Effects[1].Contexts) != 2 {
		t.Errorf("task_event effect drifted: %+v", dec.Effects[1])
	}
	// Re-encoding the decoded input yields identical bytes (deterministic codec).
	enc2, err := encodeApplyInput(dec)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(enc1.Context, enc2.Context) || !bytes.Equal(enc1.Mutation, enc2.Mutation) {
		t.Error("encode/decode/encode is not byte-stable")
	}
}

func TestWire_WrongSchemaFailsClosed(t *testing.T) {
	in := richOperationInput(t)
	enc, _ := encodeApplyInput(in)
	enc.Schema = "provenance.dbos-apply-input/v0"
	if _, err := decodeApplyInput(enc); err == nil {
		t.Fatal("expected schema rejection")
	}
}

func TestFingerprint_StableAndSensitive(t *testing.T) {
	in := richOperationInput(t)
	base := fingerprint("v1", in)
	if base != fingerprint("v1", in) {
		t.Fatal("fingerprint not stable for identical input")
	}

	mutators := map[string]func(journal.OperationInput) journal.OperationInput{
		"version":   func(x journal.OperationInput) journal.OperationInput { return x }, // handled below
		"actor":     func(x journal.OperationInput) journal.OperationInput { x.ActorID = testActorID(t); return x },
		"operation": func(x journal.OperationInput) journal.OperationInput { x.OperationID = "op-other"; return x },
		"command":   func(x journal.OperationInput) journal.OperationInput { x.CommandDigest = []byte("other"); return x },
		"mutation":  func(x journal.OperationInput) journal.OperationInput { x.MutationDigest = []byte("other"); return x },
		"authority": func(x journal.OperationInput) journal.OperationInput {
			a := journal.JournalID(99)
			x.AuthorityJournalID = &a
			return x
		},
	}
	for name, m := range mutators {
		if name == "version" {
			if fingerprint("v2", in) == base {
				t.Errorf("fingerprint insensitive to application version")
			}
			continue
		}
		if fingerprint("v1", m(in)) == base {
			t.Errorf("fingerprint insensitive to %s change", name)
		}
	}

	// A genesis (nil authority) operation never collides with an authority-0 op.
	genesis := in
	genesis.AuthorityJournalID = nil
	zero := journal.JournalID(0)
	authZero := in
	authZero.AuthorityJournalID = &zero
	if fingerprint("v1", genesis) == fingerprint("v1", authZero) {
		t.Error("genesis and authority-0 fingerprints collide")
	}
}

func TestOutcome_SuccessGoldenAndDecode(t *testing.T) {
	taskID := testTaskID(t)
	result := journal.CommittedResult{
		Kind:            journal.CommittedExact,
		AnchorJournalID: 10,
		EmittedEvents:   []journal.JournalID{11, 12},
		ResultSlots: []journal.ResultSlotBinding{
			{Slot: "b", ProducedJournalID: 12, Kind: journal.JournalKindTaskEvent, TaskID: &taskID},
			{Slot: "a", ProducedJournalID: 11, Kind: journal.JournalKindTaskEvent, TaskID: &taskID},
		},
	}
	outcome, err := encodeDBOSApplySuccess("op-g", []byte("mut"), result)
	if err != nil {
		t.Fatalf("encode success: %v", err)
	}
	// Slots are slot-sorted deterministically.
	if outcome.Success.ResultSlots[0].Slot != "a" || outcome.Success.ResultSlots[1].Slot != "b" {
		t.Errorf("slots not slot-sorted: %+v", outcome.Success.ResultSlots)
	}
	// Stable JSON golden (round-trips DBOS's JSON serializer).
	raw, err := json.Marshal(outcome)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back DBOSStepOutcomeV1
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := back.Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.AnchorJournalID != 10 || len(got.EmittedEvents) != 2 {
		t.Errorf("decoded success drifted: %+v", got)
	}
}

func TestOutcome_FailureRoundTripsTypedError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		sentinel error
	}{
		{"genesis", journal.ErrGenesis, journal.ErrGenesis},
		{"authority", journal.ErrAuthorityScope, journal.ErrAuthorityScope},
		{"lifecycle", journal.ErrAssignmentLifecycle, journal.ErrAssignmentLifecycle},
		{"notfound", ErrNotFound, ErrNotFound},
		{"unexpected", errors.New("boom"), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outcome, err := encodeDBOSApplyFailure("op-f", []byte("m"), c.err)
			if err != nil {
				t.Fatalf("encode failure returned Go error: %v", err)
			}
			if outcome.Failure == nil || outcome.Success != nil {
				t.Fatal("failure outcome is not a closed one-of")
			}
			// Survives a JSON serialize round trip.
			raw, _ := json.Marshal(outcome)
			var back DBOSStepOutcomeV1
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			_, decErr := back.Decode()
			if decErr == nil {
				t.Fatal("failure outcome Decode returned no error")
			}
			if c.sentinel != nil && !errors.Is(decErr, c.sentinel) {
				t.Errorf("decoded error %v is not errors.Is %v", decErr, c.sentinel)
			}
		})
	}
}

func TestOutcome_ConflictReExposesTypedConflict(t *testing.T) {
	conflict := &journal.OperationConflict{OperationID: "op-c", Field: "mutation digest"}
	wrapped := errors.Join(journal.ErrOperationConflict, conflict)
	outcome, _ := encodeDBOSApplyFailure("op-c", []byte("m"), wrapped)
	raw, _ := json.Marshal(outcome)
	var back DBOSStepOutcomeV1
	_ = json.Unmarshal(raw, &back)
	_, decErr := back.Decode()
	var oc *journal.OperationConflict
	if !errors.As(decErr, &oc) {
		t.Fatalf("decoded conflict not errors.As-discoverable: %v", decErr)
	}
	if oc.Field != "mutation digest" {
		t.Errorf("conflict field drifted: %q", oc.Field)
	}
}
