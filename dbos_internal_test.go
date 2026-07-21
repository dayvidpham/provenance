package provenance

// dbos_internal_test.go is a white-box suite over the adapter's unexported
// canonicalization, fingerprint, and step-outcome encoding (issue #6). It lives in
// package provenance because those seams are deliberately unexported (no dual
// export): the deterministic identity and closed encoding are internal contracts.

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
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

func mustFingerprintV2(t *testing.T, version string, input DBOSApplyInputV2) string {
	t.Helper()
	fingerprint, err := fingerprintV2(version, input)
	if err != nil {
		t.Fatalf("fingerprintV2: %v", err)
	}
	return fingerprint
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
				TaskID: taskID, Occupant: testActorID(t),
			},
			{
				Sort: EffectDecision, TaskID: taskID, DecisionKind: "pasture.decision.ratify", Payload: json.RawMessage(`{"accepted":true}`),
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

func TestWireV2_TransportsCanonicalBytesAndRejectsMalformedFrames(t *testing.T) {
	in := richOperationInput(t)
	prepared, err := journal.PrepareMutationV1(in.Effects)
	if err != nil {
		t.Fatalf("PrepareMutationV1: %v", err)
	}
	encoded, normalized, err := encodeApplyInputV2(in)
	if err != nil {
		t.Fatalf("encodeApplyInputV2: %v", err)
	}
	if encoded.Schema != DBOSApplyInputSchemaV2 || !bytes.Equal(encoded.Mutation, prepared.CanonicalBytes()) {
		t.Fatalf("V2 did not carry canonical bytes directly: schema=%q mutation-equal=%v", encoded.Schema, bytes.Equal(encoded.Mutation, prepared.CanonicalBytes()))
	}
	decoded, err := decodeApplyInputV2(encoded)
	if err != nil {
		t.Fatalf("decodeApplyInputV2: %v", err)
	}
	reencoded, _, err := encodeApplyInputV2(decoded)
	if err != nil {
		t.Fatalf("re-encode V2: %v", err)
	}
	if !bytes.Equal(encoded.Context, reencoded.Context) || !bytes.Equal(encoded.Mutation, reencoded.Mutation) || !bytes.Equal(decoded.MutationDigest, normalized.MutationDigest) {
		t.Fatal("V2 encode/decode/encode is not byte-stable")
	}

	tests := map[string]func(DBOSApplyInputV2) DBOSApplyInputV2{
		"unknown input version":  func(x DBOSApplyInputV2) DBOSApplyInputV2 { x.Schema = "provenance.dbos-apply-input/v3"; return x },
		"missing context field":  func(x DBOSApplyInputV2) DBOSApplyInputV2 { x.Context = x.Context[:len(x.Context)-12]; return x },
		"trailing context field": func(x DBOSApplyInputV2) DBOSApplyInputV2 { x.Context = append(x.Context, 0, 0, 0, 0); return x },
		"unknown context version": func(x DBOSApplyInputV2) DBOSApplyInputV2 {
			x.Context = append([]byte(nil), x.Context...)
			x.Context[4] ^= 0x01
			return x
		},
		"malformed canonical mutation": func(x DBOSApplyInputV2) DBOSApplyInputV2 { x.Mutation = append(x.Mutation, 0); return x },
		"oversized canonical mutation": func(x DBOSApplyInputV2) DBOSApplyInputV2 {
			x.Mutation = make([]byte, MaxCanonicalMutationBytes+1)
			return x
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decodeApplyInputV2(mutate(encoded))
			if err == nil {
				t.Fatal("malformed V2 input decoded successfully")
			}
			if name != "malformed canonical mutation" && name != "oversized canonical mutation" {
				if !errors.Is(err, ErrDBOSContextFrame) {
					t.Fatalf("context error %v does not wrap ErrDBOSContextFrame", err)
				}
				var frame *DBOSContextFrameError
				if !errors.As(err, &frame) || frame.Field == "" || frame.Reason == "" || frame.Fix == "" {
					t.Fatalf("context error is not complete typed diagnostic: %#v", err)
				}
				for _, token := range []string{"what:", "why:", "where:", "when:", "impact:", "fix:"} {
					if !strings.Contains(err.Error(), token) {
						t.Fatalf("context error lacks %s: %v", token, err)
					}
				}
			}
		})
	}

	legacy, err := encodeApplyInput(in)
	if err != nil {
		t.Fatalf("encode historical V1: %v", err)
	}
	if _, err := decodeApplyInput(legacy); err != nil {
		t.Fatalf("historical V1 no longer decodes: %v", err)
	}
	if applyWorkflowIDPrefix+fingerprintV1("v1", in) == applyWorkflowIDPrefixV2+mustFingerprintV2(t, "v1", encoded) {
		t.Fatal("V1 and V2 workflow identities collided")
	}
}

func TestFingerprint_StableAndSensitive(t *testing.T) {
	in := richOperationInput(t)
	legacyBase := fingerprintV1("v1", in)
	if legacyBase != fingerprintV1("v1", in) {
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
			if fingerprintV1("v2", in) == legacyBase {
				t.Errorf("fingerprint insensitive to application version")
			}
			continue
		}
		if fingerprintV1("v1", m(in)) == legacyBase {
			t.Errorf("fingerprint insensitive to %s change", name)
		}
	}

	// A genesis (nil authority) operation never collides with an authority-0 op.
	genesis := in
	genesis.AuthorityJournalID = nil
	zero := journal.JournalID(0)
	authZero := in
	authZero.AuthorityJournalID = &zero
	if fingerprintV1("v1", genesis) == fingerprintV1("v1", authZero) {
		t.Error("genesis and authority-0 fingerprints collide")
	}

	input, _, err := encodeApplyInputV2(in)
	if err != nil {
		t.Fatalf("encode V2: %v", err)
	}
	base := mustFingerprintV2(t, "v1", input)
	changedDigest := in
	changedDigest.MutationDigest = []byte("caller-controlled")
	inputChangedDigest, _, err := encodeApplyInputV2(changedDigest)
	if err != nil {
		t.Fatalf("encode V2 changed caller digest: %v", err)
	}
	if mustFingerprintV2(t, "v1", inputChangedDigest) != base {
		t.Error("V2 fingerprint depends on caller MutationDigest")
	}
	changedEffect := in
	changedEffect.Effects = append([]journal.Effect(nil), in.Effects...)
	changedEffect.Effects[0].Title = "different canonical operand"
	inputChangedEffect, _, err := encodeApplyInputV2(changedEffect)
	if err != nil {
		t.Fatalf("encode V2 changed effect: %v", err)
	}
	if mustFingerprintV2(t, "v1", inputChangedEffect) == base {
		t.Error("V2 fingerprint is insensitive to canonical effect bytes")
	}
	changedRecordedAt := in
	changedRecordedAt.RecordedAt++
	inputChangedRecordedAt, _, err := encodeApplyInputV2(changedRecordedAt)
	if err != nil {
		t.Fatalf("encode V2 changed RecordedAt: %v", err)
	}
	if bytes.Equal(inputChangedRecordedAt.Context, input.Context) {
		t.Error("V2 transport omitted changed audit RecordedAt")
	}
	if mustFingerprintV2(t, "v1", inputChangedRecordedAt) != base {
		t.Error("V2 logical identity includes audit-only RecordedAt")
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
