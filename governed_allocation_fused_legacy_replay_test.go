package provenance

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/allocation"
	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
)

// This test is parallel: it owns a private t.TempDir database and DBOS application
// name, its participant counter is local to its own closure, and its frozen baseline
// expectations are immutable literals rather than shared fixture state.

func TestFusedLegacyRunAllocateReopensBaselineWorkflowWithoutParticipant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "legacy-fused-replay.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	var participantCalls int
	participant := GovernedAllocationParticipant(func(context.Context, GovernedAllocationTransaction, GovernedAllocationRequest, OperationClosure) error {
		participantCalls++
		return nil
	})
	open := func(t *testing.T) *FusedGovernedAllocator {
		t.Helper()
		allocator, err := OpenFusedGovernedAllocator(ctx, FusedGovernedAllocatorConfig{
			SQLiteDSN: dsn, AppName: "provenance-legacy-fused-replay", ApplicationVersion: "test-v1", Logger: slog.Default(), Participant: participant,
		})
		if err != nil {
			t.Fatalf("open fused allocator: %v", err)
		}
		if err := allocator.Launch(); err != nil {
			_ = allocator.Close(30 * time.Second)
			t.Fatalf("launch fused allocator: %v", err)
		}
		return allocator
	}

	firstAllocator := open(t)
	actorID := ActorID{Namespace: "legacy-replay", UUID: uuid.UUID(BigEndianUUID(1))}
	actor, err := firstAllocator.Tracker().RegisterFixedSoftwareAgent(FixedSoftwareAgentRegistration{
		Claim:     ActorNamespaceClaim{Namespace: "legacy-replay", ClaimantID: "baseline-2b923b9", Codec: OrdinalV1CodecName, Range: UUIDRange{Min: BigEndianUUID(0), Max: BigEndianUUID(1023)}},
		Entry:     FixedActorEntry{ActorID: actorID, Namespace: "legacy-replay", ActorKind: AgentKindSoftware, Name: "legacy-replay/baseline", Metadata: `{"baseline":"2b923b9"}`},
		AgentName: "legacy-replay", Version: "1", Source: "baseline-2b923b9",
	})
	if err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("register actor: %v", err)
	}
	rootRequest := RootGenesisRequest{
		OperationID: "legacy-replay-genesis",
		ActorID:     actor.ID,
		Command:     "test.genesis",
		Root:        legacyReplayChild("root", actor.ID),
	}
	rootClosure, err := firstAllocator.RunInitializeRoot(ctx, "legacy-replay-genesis-workflow", rootRequest)
	if err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("initialize legacy replay root: %v", err)
	}
	root, ok := rootClosure.Root()
	if !ok {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatal("legacy replay root closure has no root binding")
	}
	request := GovernedAllocationRequest{
		OperationID:        "legacy-replay-allocation",
		ActorID:            actor.ID,
		Command:            "test.allocate",
		ParentAssignmentID: root.AssignmentID,
		Children:           []GovernedChildSpec{legacyReplayChild("child", actor.ID)},
	}
	canonical, _, err := allocation.CanonicalizeAllocation(request)
	if err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("canonicalize baseline allocation request: %v", err)
	}
	// These are literal bytes emitted by baseline 2b923b9. Do not regenerate
	// either expectation with current constructors: this test is the compatibility
	// fixture proving the simple receipt and DBOS workflow input remain frozen.
	wantCanonical := []byte(`{"version":1,"kind":2,"operationID":"legacy-replay-allocation","actorID":"legacy-replay--00000000-0000-0000-0000-000000000001","command":"test.allocate","parentAssignmentID":"assignment/legacy-replay/root","children":[{"taskID":"legacy-replay--9bdc1fda-ccaa-581c-a1d7-489bbfad3658","assignmentID":"assignment/legacy-replay/child","occupant":"legacy-replay--00000000-0000-0000-0000-000000000001","title":"legacy replay child","description":"baseline fused workflow replay test","type":2,"priority":2,"phase":8}]}`)
	wantWorkflowInput := []byte(`{"version":1,"authority":3,"canonicalRequest":"eyJ2ZXJzaW9uIjoxLCJraW5kIjoyLCJvcGVyYXRpb25JRCI6ImxlZ2FjeS1yZXBsYXktYWxsb2NhdGlvbiIsImFjdG9ySUQiOiJsZWdhY3ktcmVwbGF5LS0wMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDEiLCJjb21tYW5kIjoidGVzdC5hbGxvY2F0ZSIsInBhcmVudEFzc2lnbm1lbnRJRCI6ImFzc2lnbm1lbnQvbGVnYWN5LXJlcGxheS9yb290IiwiY2hpbGRyZW4iOlt7InRhc2tJRCI6ImxlZ2FjeS1yZXBsYXktLTliZGMxZmRhLWNjYWEtNTgxYy1hMWQ3LTQ4OWJiZmFkMzY1OCIsImFzc2lnbm1lbnRJRCI6ImFzc2lnbm1lbnQvbGVnYWN5LXJlcGxheS9jaGlsZCIsIm9jY3VwYW50IjoibGVnYWN5LXJlcGxheS0tMDAwMDAwMDAtMDAwMC0wMDAwLTAwMDAtMDAwMDAwMDAwMDAxIiwidGl0bGUiOiJsZWdhY3kgcmVwbGF5IGNoaWxkIiwiZGVzY3JpcHRpb24iOiJiYXNlbGluZSBmdXNlZCB3b3JrZmxvdyByZXBsYXkgdGVzdCIsInR5cGUiOjIsInByaW9yaXR5IjoyLCJwaGFzZSI6OH1dfQ=="}`)
	if !bytes.Equal(canonical, wantCanonical) {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("current simple canonical bytes drifted from baseline 2b923b9: got=%q want=%q", canonical, wantCanonical)
	}

	first, err := firstAllocator.RunAllocate(ctx, "legacy-replay-allocation-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("run baseline allocation: %v", err)
	}
	if participantCalls != 1 {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("participant calls after baseline allocation=%d, want 1", participantCalls)
	}
	workflows, err := dbos.ListWorkflows(firstAllocator.system.Root(), dbos.WithFilterWorkflowIDs("legacy-replay-allocation-workflow"), dbos.WithFilterLimit(2), dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(false))
	if err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("load persisted legacy workflow input: %v", err)
	}
	if len(workflows) != 1 {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("persisted legacy workflow count=%d, want 1", len(workflows))
	}
	persistedWorkflowInput, ok := workflows[0].Input.(string)
	if !ok || !bytes.Equal([]byte(persistedWorkflowInput), wantWorkflowInput) {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("persisted legacy workflow input=%q, want baseline simple input %q", persistedWorkflowInput, wantWorkflowInput)
	}
	var persistedReceipt []byte
	if err := firstAllocator.system.DB().QueryRowContext(ctx, `SELECT canonical_request FROM governed_allocation_operations WHERE operation_id=?1`, request.OperationID).Scan(&persistedReceipt); err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("load baseline governed allocation receipt: %v", err)
	}
	if !bytes.Equal(persistedReceipt, wantCanonical) {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("persisted legacy allocation receipt differs from baseline canonical request")
	}
	var receipt struct {
		Version     int                    `json:"version"`
		Kind        allocation.RequestKind `json:"kind"`
		OperationID string                 `json:"operationID"`
	}
	if err := json.Unmarshal(persistedReceipt, &receipt); err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("decode baseline governed allocation receipt: %v", err)
	}
	if receipt.Version != 1 || receipt.Kind != allocation.RequestKindAllocation || receipt.OperationID != string(request.OperationID) {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("baseline allocation receipt=%+v, want simple allocation shape", receipt)
	}
	// This is the literal DBOS_JSON payload persisted by baseline 2b923b9. Seed
	// its normal base64 wire representation directly rather than invoking the
	// current result writer, so reopen exercises compatibility with history.
	wantHistoricalOutput := []byte(`{"Closure":{"version":1,"operationID":"legacy-replay-allocation","kind":2,"anchor":4,"children":[{"Ordinal":0,"TaskID":{"Namespace":"legacy-replay","UUID":"9bdc1fda-ccaa-581c-a1d7-489bbfad3658"},"AssignmentID":"assignment/legacy-replay/child","Occupant":{"Namespace":"legacy-replay","UUID":"00000000-0000-0000-0000-000000000001"},"TaskRow":{"OperationID":"legacy-replay-allocation","EffectOrdinal":0,"Subordinal":0,"JournalID":5},"AssignmentRow":{"OperationID":"legacy-replay-allocation","EffectOrdinal":0,"Subordinal":1,"JournalID":6}}]},"Failure":null}`)
	if _, err := firstAllocator.system.DB().ExecContext(ctx, `UPDATE workflow_status SET output=?1 WHERE workflow_uuid=?2`, base64.StdEncoding.EncodeToString(wantHistoricalOutput), "legacy-replay-allocation-workflow"); err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("seed literal historical DBOS allocation output: %v", err)
	}
	var storedOutput string
	if err := firstAllocator.system.DB().QueryRowContext(ctx, `SELECT output FROM workflow_status WHERE workflow_uuid=?1`, "legacy-replay-allocation-workflow").Scan(&storedOutput); err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("load seeded historical DBOS allocation output: %v", err)
	}
	decodedOutput, err := base64.StdEncoding.DecodeString(storedOutput)
	if err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("decode seeded historical DBOS allocation output: %v", err)
	}
	if !bytes.Equal(decodedOutput, wantHistoricalOutput) {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("seeded historical DBOS allocation output drifted: got=%q want=%q", decodedOutput, wantHistoricalOutput)
	}
	if err := firstAllocator.Close(30 * time.Second); err != nil {
		t.Fatalf("close first fused allocator: %v", err)
	}

	reopened := open(t)
	t.Cleanup(func() { _ = reopened.Close(30 * time.Second) })
	replayedRoot, err := reopened.RunInitializeRoot(ctx, "legacy-replay-genesis-workflow", rootRequest)
	if err != nil {
		t.Fatalf("reopen and retry baseline genesis workflow: %v", err)
	}
	if !rootClosure.Equal(replayedRoot) {
		t.Fatalf("reopened baseline genesis closure differs: first=%+v replayed=%+v", rootClosure, replayedRoot)
	}
	replayed, err := reopened.RunAllocate(ctx, "legacy-replay-allocation-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("reopen and retry baseline allocation workflow: %v", err)
	}
	if !first.Equal(replayed) {
		t.Fatalf("reopened baseline allocation closure differs: first=%+v replayed=%+v", first, replayed)
	}
	if participantCalls != 1 {
		t.Fatalf("participant reran during reopened same-workflow replay: calls=%d, want 1", participantCalls)
	}
}

func legacyReplayChild(name string, actor ActorID) GovernedChildSpec {
	return GovernedChildSpec{
		TaskID:       TaskID{Namespace: "legacy-replay", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("task/"+name))},
		AssignmentID: AssignmentID("assignment/legacy-replay/" + name),
		Occupant:     actor,
		Title:        "legacy replay " + name,
		Description:  "baseline fused workflow replay test",
		Type:         TaskTypeTask,
		Priority:     PriorityMedium,
		Phase:        PhaseWorkerSlices,
	}
}
