package provenance_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	provenance "github.com/dayvidpham/provenance"
)

// journal_batch_api_test.go proves the public batch-apply surface from an
// external package: BatchJournal is discoverable by type assertion on the store
// Tracker.Journal() returns (both standalone and borrowed), a batch commits
// records in one call, and the per-record outcome taxonomy, the §9.4
// short-circuit, and the §11 conflict are all recoverable through the public
// types.

func batchPublicInput(operationID string, actor provenance.ActorID, authority provenance.JournalID) provenance.OperationInput {
	return provenance.OperationInput{
		OperationID:        provenance.OperationID(operationID),
		ActorID:            actor,
		AuthorityJournalID: &authority,
		CommandDigest:      []byte(operationID),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []provenance.Effect{{
			Sort:         provenance.EffectDecision,
			DecisionKind: "batch.public.decision",
			Payload:      []byte(`{"public":true}`),
			ResultSlot:   "decision",
		}},
	}
}

func TestBatchJournalPublicSurface(t *testing.T) {
	t.Parallel()
	tracker, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = tracker.Close() })
	agent, err := tracker.RegisterSoftwareAgent("batch-public", "actor", "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	actor := agent.Agent.ID
	boot := publicFactGenesis(t, tracker, actor, "batch-public-genesis")

	batch, ok := tracker.Journal().(provenance.BatchJournal)
	if !ok {
		t.Fatal("Tracker.Journal() does not implement provenance.BatchJournal")
	}

	const n = 5
	inputs := make([]provenance.OperationInput, n)
	for i := range inputs {
		inputs[i] = batchPublicInput(fmt.Sprintf("batch-public-%02d", i), actor, boot)
	}
	result, err := batch.ApplyBatchContext(t.Context(), inputs)
	if err != nil {
		t.Fatalf("ApplyBatchContext: %v", err)
	}
	if !result.Committed || len(result.Records) != n {
		t.Fatalf("batch result = %+v, want committed with %d records", result, n)
	}
	for i, record := range result.Records {
		if record.Outcome != provenance.BatchRecordCommitted || record.Err != nil {
			t.Fatalf("record %d = %+v, want committed", i, record)
		}
	}

	// A second identical batch short-circuits every record; a changed payload
	// under the same OperationID is a typed conflict that rolls the batch back.
	replay, err := batch.ApplyBatchContext(t.Context(), inputs)
	if err != nil {
		t.Fatalf("ApplyBatch replay: %v", err)
	}
	for i, record := range replay.Records {
		if record.Outcome != provenance.BatchRecordShortCircuited || !record.Result.ShortCircuited {
			t.Fatalf("replay record %d = %+v, want short-circuited", i, record)
		}
	}

	conflicting := batchPublicInput("batch-public-00", actor, boot)
	conflicting.Effects[0].Payload = []byte(`{"public":false}`)
	conflictResult, err := batch.ApplyBatchContext(t.Context(), []provenance.OperationInput{conflicting})
	if err == nil {
		t.Fatal("conflicting batch returned no error")
	}
	if !errors.Is(err, provenance.ErrBatchApply) || !errors.Is(err, provenance.ErrOperationConflict) {
		t.Fatalf("conflict error = %v, want errors.Is both ErrBatchApply and ErrOperationConflict", err)
	}
	var batchErr *provenance.BatchApplyError
	if !errors.As(err, &batchErr) || batchErr.Index != 0 {
		t.Fatalf("conflict error = %v, want *BatchApplyError at index 0", err)
	}
	if conflictResult.Committed || conflictResult.Records[0].Outcome != provenance.BatchRecordConflict {
		t.Fatalf("conflict result = %+v, want a rolled-back conflict", conflictResult)
	}

	// Bounds are public and enforced.
	if _, err := batch.ApplyBatchContext(t.Context(), nil); !errors.Is(err, provenance.ErrBatchEmpty) {
		t.Fatalf("empty batch error = %v, want ErrBatchEmpty", err)
	}
	oversized := make([]provenance.OperationInput, provenance.MaxBatchRecords+1)
	if _, err := batch.ApplyBatchContext(t.Context(), oversized); !errors.Is(err, provenance.ErrBatchTooLarge) {
		t.Fatalf("oversized batch error = %v, want ErrBatchTooLarge", err)
	}
}

func TestBatchJournalPublicBorrowedStore(t *testing.T) {
	t.Parallel()
	db, _ := openFileDB(t)
	tracker, err := provenance.OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("OpenBorrowedSQLite: %v", err)
	}
	t.Cleanup(func() { _ = tracker.Close() })
	agent, err := tracker.RegisterSoftwareAgent("batch-borrowed", "actor", "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	actor := agent.Agent.ID
	boot := publicFactGenesis(t, tracker, actor, "batch-borrowed-genesis")

	batch, ok := tracker.Journal().(provenance.BatchJournal)
	if !ok {
		t.Fatal("a borrowed Tracker.Journal() does not implement provenance.BatchJournal")
	}
	inputs := []provenance.OperationInput{
		batchPublicInput("batch-borrowed-0", actor, boot),
		batchPublicInput("batch-borrowed-1", actor, boot),
	}
	result, err := batch.ApplyBatchContext(t.Context(), inputs)
	if err != nil {
		t.Fatalf("borrowed ApplyBatch: %v", err)
	}
	if !result.Committed || len(result.Records) != len(inputs) {
		t.Fatalf("borrowed batch result = %+v, want committed with %d records", result, len(inputs))
	}
	for i, record := range result.Records {
		if record.Outcome != provenance.BatchRecordCommitted {
			t.Fatalf("borrowed record %d = %+v, want committed", i, record)
		}
	}
}
