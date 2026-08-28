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
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
)

// shutdownDBOSRoot stops a DBOS root and reports a shutdown that did not
// finish. The runtime returns an error when the timeout expires with resources
// still running; a test that ignored it could then close a shared SQLite handle
// the runtime is still writing to, and the failure would surface later as an
// unrelated corruption.
func shutdownDBOSRoot(t *testing.T, root dbos.Context, timeout time.Duration) {
	t.Helper()
	if root == nil {
		return
	}
	if err := dbos.Shutdown(root, timeout); err != nil {
		t.Errorf("DBOS shutdown did not finish within %s: %v", timeout, err)
	}
}

func testTaskID(t *testing.T) ptypes.TaskID {
	t.Helper()
	return ptypes.TaskID{Namespace: "aura", UUID: uuid.Must(uuid.NewV7())}
}

func testActorID(t *testing.T) ptypes.ActorID {
	t.Helper()
	return ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}
}

func mustFingerprint(t *testing.T, version string, input DBOSApplyInput) string {
	t.Helper()
	fingerprint, err := fingerprint(newDBOSContractSnapshot(), version, input)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
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
	contract := newDBOSContractSnapshot()
	enc1, normalized, err := encodeApplyInput(contract, in)
	if err != nil {
		t.Fatalf("encodeApplyInput: %v", err)
	}
	if enc1.Schema != contract.applyInputSchema {
		t.Errorf("schema = %q, want %q", enc1.Schema, contract.applyInputSchema)
	}
	dec, err := decodeApplyInput(contract, enc1)
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
	enc2, _, err := encodeApplyInput(contract, dec)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(enc1.Context, enc2.Context) || !bytes.Equal(enc1.Mutation, enc2.Mutation) {
		t.Error("encode/decode/encode is not byte-stable")
	}
	if !bytes.Equal(dec.MutationDigest, normalized.MutationDigest) {
		t.Error("derived mutation digest drifted")
	}
}

func TestWire_WrongSchemaFailsClosed(t *testing.T) {
	in := richOperationInput(t)
	contract := newDBOSContractSnapshot()
	enc, _, _ := encodeApplyInput(contract, in)
	enc.Schema = "provenance.dbos-apply-input/v0"
	if _, err := decodeApplyInput(contract, enc); err == nil {
		t.Fatal("expected schema rejection")
	}
}

func TestWire_TransportsCanonicalBytesAndRejectsMalformedFrames(t *testing.T) {
	in := richOperationInput(t)
	prepared, err := journal.Canonicalize(journal.OperationInput{Effects: in.Effects})
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	contract := newDBOSContractSnapshot()
	encoded, normalized, err := encodeApplyInput(contract, in)
	if err != nil {
		t.Fatalf("encodeApplyInput: %v", err)
	}
	if encoded.Schema != contract.applyInputSchema || !bytes.Equal(encoded.Mutation, prepared.CanonicalBytes()) {
		t.Fatalf("DBOS input did not carry canonical bytes directly: schema=%q mutation-equal=%v", encoded.Schema, bytes.Equal(encoded.Mutation, prepared.CanonicalBytes()))
	}
	decoded, err := decodeApplyInput(contract, encoded)
	if err != nil {
		t.Fatalf("decodeApplyInput: %v", err)
	}
	reencoded, _, err := encodeApplyInput(contract, decoded)
	if err != nil {
		t.Fatalf("re-encode DBOS input: %v", err)
	}
	if !bytes.Equal(encoded.Context, reencoded.Context) || !bytes.Equal(encoded.Mutation, reencoded.Mutation) || !bytes.Equal(decoded.MutationDigest, normalized.MutationDigest) {
		t.Fatal("DBOS encode/decode/encode is not byte-stable")
	}

	tests := map[string]func(DBOSApplyInput) DBOSApplyInput{
		"unknown input version":  func(x DBOSApplyInput) DBOSApplyInput { x.Schema = "provenance.dbos-apply-input/v3"; return x },
		"missing context field":  func(x DBOSApplyInput) DBOSApplyInput { x.Context = x.Context[:len(x.Context)-12]; return x },
		"trailing context field": func(x DBOSApplyInput) DBOSApplyInput { x.Context = append(x.Context, 0, 0, 0, 0); return x },
		"unknown context version": func(x DBOSApplyInput) DBOSApplyInput {
			x.Context = append([]byte(nil), x.Context...)
			x.Context[4] ^= 0x01
			return x
		},
		"malformed canonical mutation": func(x DBOSApplyInput) DBOSApplyInput { x.Mutation = append(x.Mutation, 0); return x },
		"oversized canonical mutation": func(x DBOSApplyInput) DBOSApplyInput {
			x.Mutation = make([]byte, MaxCanonicalMutationBytes+1)
			return x
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decodeApplyInput(contract, mutate(encoded))
			if err == nil {
				t.Fatal("malformed DBOS input decoded successfully")
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

}

func TestFingerprint_StableAndSensitive(t *testing.T) {
	in := richOperationInput(t)
	contract := newDBOSContractSnapshot()
	input, _, err := encodeApplyInput(contract, in)
	if err != nil {
		t.Fatalf("encode DBOS input: %v", err)
	}
	base := mustFingerprint(t, "v1", input)
	if mustFingerprint(t, "v1", input) != base {
		t.Fatal("fingerprint not stable for identical input")
	}
	if mustFingerprint(t, "app-other", input) == base {
		t.Fatal("fingerprint insensitive to application version")
	}
	changedDigest := in
	changedDigest.MutationDigest = []byte("caller-controlled")
	inputChangedDigest, _, err := encodeApplyInput(contract, changedDigest)
	if err != nil {
		t.Fatalf("encode DBOS input with changed caller digest: %v", err)
	}
	if mustFingerprint(t, "v1", inputChangedDigest) != base {
		t.Error("DBOS fingerprint depends on caller MutationDigest")
	}
	changedEffect := in
	changedEffect.Effects = append([]journal.Effect(nil), in.Effects...)
	changedEffect.Effects[0].Title = "different canonical operand"
	inputChangedEffect, _, err := encodeApplyInput(contract, changedEffect)
	if err != nil {
		t.Fatalf("encode DBOS input with changed effect: %v", err)
	}
	if mustFingerprint(t, "v1", inputChangedEffect) == base {
		t.Error("DBOS fingerprint is insensitive to canonical effect bytes")
	}
	workflow := workflowIdentity(contract, "v1", in.OperationID)
	if workflowIdentity(contract, "v1", changedEffect.OperationID) != workflow {
		t.Error("workflow identity depends on canonical input beyond OperationID")
	}
	if workflowIdentity(contract, "app-other", in.OperationID) == workflow ||
		workflowIdentity(contract, "v1", in.OperationID+"-other") == workflow {
		t.Error("workflow identity is insensitive to application version or OperationID")
	}
	changedContract := contract
	changedContract.contextSchema += "-other"
	if workflowIdentity(changedContract, "v1", in.OperationID) == workflow {
		t.Error("workflow identity is insensitive to the captured contract")
	}
	// The pinned-library string is a fingerprint salt: it keys the durable
	// workflow namespace. This assertion is why dbosPinnedLibraryConst may not be
	// updated alongside a dependency bump -- changing it re-keys every workflow
	// ID rather than describing the new library.
	saltedContract := contract
	saltedContract.pinnedLibrary += "-other"
	if workflowIdentity(saltedContract, "v1", in.OperationID) == workflow {
		t.Error("workflow identity is insensitive to the pinned-library salt; changing that constant would silently NOT re-key durable workflows, contradicting its documented contract")
	}
	changedRecordedAt := in
	changedRecordedAt.RecordedAt++
	inputChangedRecordedAt, _, err := encodeApplyInput(contract, changedRecordedAt)
	if err != nil {
		t.Fatalf("encode DBOS input with changed RecordedAt: %v", err)
	}
	if bytes.Equal(inputChangedRecordedAt.Context, input.Context) {
		t.Error("DBOS transport omitted changed audit RecordedAt")
	}
	if mustFingerprint(t, "v1", inputChangedRecordedAt) != base {
		t.Error("DBOS logical identity includes audit-only RecordedAt")
	}
}

func TestDecodeListedWorkflowInputRejectsNonUniqueJSON(t *testing.T) {
	tests := map[string]string{
		"trailing JSON":        `{"schema":"s","context":"Yw==","mutation":"bQ=="} {}`,
		"duplicate top-level":  `{"schema":"s","schema":"s","context":"Yw==","mutation":"bQ=="}`,
		"duplicate nested key": `{"schema":"s","context":"Yw==","mutation":"bQ==","extra":{"key":1,"key":2}}`,
		"unknown field":        `{"schema":"s","context":"Yw==","mutation":"bQ==","unknown":true}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeListedWorkflowInput(raw); err == nil {
				t.Fatalf("decodeListedWorkflowInput accepted %s", name)
			}
		})
	}
}

func TestListedWorkflowDiagnosticRejectsAmbiguousIdentityAndStatus(t *testing.T) {
	contract := newDBOSContractSnapshot()
	const workflowID = "workflow-exact"
	operation := OperationID("operation-exact")
	tests := map[string][]dbos.WorkflowStatus{
		"mismatched ID": {{ID: "workflow-other", Status: dbos.WorkflowStatusSuccess}},
		"multiple rows": {
			{ID: workflowID, Status: dbos.WorkflowStatusSuccess},
			{ID: workflowID, Status: dbos.WorkflowStatusError},
		},
		"unknown status": {{ID: workflowID, Status: dbos.WorkflowStatusType("CORRUPT")}},
	}
	for name, workflows := range tests {
		t.Run(name, func(t *testing.T) {
			exists, err := listedWorkflowDiagnostic(workflows, contract, "v1", workflowID, "fingerprint", operation)
			var diagnostic *DBOSDiagnosticError
			if exists || !errors.As(err, &diagnostic) || diagnostic.Class != DBOSDiagClassTerminalRetrieval || diagnostic.Field != DBOSDiagFieldWorkflow || diagnostic.Stage != DBOSDiagStageWorkflowTerminalLookup || diagnostic.Operation != operation || diagnostic.Workflow != workflowID || diagnostic.Impact == "" || diagnostic.Fix == "" || diagnostic.Cause == nil {
				t.Fatalf("lookup rejection is not a closed actionable diagnostic: exists=%v diagnostic=%#v err=%v", exists, diagnostic, err)
			}
		})
	}

	terminalCause := errors.New("terminal dependency failure")
	exists, err := listedWorkflowDiagnostic([]dbos.WorkflowStatus{{
		ID: workflowID, Status: dbos.WorkflowStatusError, Input: `{"schema":"first","schema":"duplicate"}`, Error: terminalCause,
	}}, contract, "v1", workflowID, "fingerprint", operation)
	var diagnostic *DBOSDiagnosticError
	if !exists || !errors.As(err, &diagnostic) || !errors.Is(err, terminalCause) || strings.Contains(err.Error(), "duplicate JSON object key") {
		t.Fatalf("terminal ERROR did not take precedence over malformed input: exists=%v diagnostic=%#v err=%v", exists, diagnostic, err)
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
	contract := newDBOSContractSnapshot()
	outcome, err := encodeDBOSApplySuccess(contract, "op-g", []byte("mut"), result)
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
	var back DBOSStepOutcome
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := decodeDBOSStepOutcome(contract, back)
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			contract := newDBOSContractSnapshot()
			outcome, err := encodeDBOSApplyFailure(contract, "op-f", []byte("m"), c.err)
			if err != nil {
				t.Fatalf("encode failure returned Go error: %v", err)
			}
			if outcome.Failure == nil || outcome.Success != nil {
				t.Fatal("failure outcome is not a closed one-of")
			}
			// Survives a JSON serialize round trip.
			raw, _ := json.Marshal(outcome)
			var back DBOSStepOutcome
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			_, decErr := decodeDBOSStepOutcome(contract, back)
			if decErr == nil {
				t.Fatal("failure outcome Decode returned no error")
			}
			if c.sentinel != nil && !errors.Is(decErr, c.sentinel) {
				t.Errorf("decoded error %v is not errors.Is %v", decErr, c.sentinel)
			}
		})
	}
}

func TestOutcome_RejectsUnknownKindAndMismatchedNestedOperation(t *testing.T) {
	for name, outcome := range map[string]DBOSStepOutcome{
		"unknown-kind":     {Schema: newDBOSContractSnapshot().outcomeSchema, OperationID: "outer", Failure: &CanonicalApplyFailure{Kind: "future_kind", Message: "x", OperationID: "outer"}},
		"nested-operation": {Schema: newDBOSContractSnapshot().outcomeSchema, OperationID: "outer", Failure: &CanonicalApplyFailure{Kind: FailureGenesis, Message: "x", OperationID: "inner"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeDBOSStepOutcome(newDBOSContractSnapshot(), outcome); err == nil {
				t.Fatal("malformed durable failure decoded")
			}
		})
	}
}

func TestOutcome_ConflictReExposesTypedConflict(t *testing.T) {
	conflict := &journal.OperationConflict{OperationID: "op-c", Axis: journal.ConflictCommand, Index: -1}
	wrapped := errors.Join(journal.ErrOperationConflict, conflict)
	contract := newDBOSContractSnapshot()
	outcome, _ := encodeDBOSApplyFailure(contract, "op-c", []byte("m"), wrapped)
	raw, _ := json.Marshal(outcome)
	var back DBOSStepOutcome
	_ = json.Unmarshal(raw, &back)
	_, decErr := decodeDBOSStepOutcome(contract, back)
	var oc *journal.OperationConflict
	if !errors.As(decErr, &oc) {
		t.Fatalf("decoded conflict not errors.As-discoverable: %v", decErr)
	}
	if oc.Axis != journal.ConflictCommand || oc.Index != -1 {
		t.Errorf("conflict axis/index drifted: axis=%s index=%d", oc.Axis, oc.Index)
	}
}
