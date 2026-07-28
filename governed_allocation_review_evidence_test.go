package provenance_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	provenance "github.com/dayvidpham/provenance"
)

func TestReviewEvidenceCommittedSuccessCannotBeReplacedByValidFailure(t *testing.T) {
	for _, composed := range []bool{false, true} {
		name := "simple"
		if composed {
			name = "composed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			calls := 0
			path := filepath.Join(t.TempDir(), "review-failure-arm-"+name+".db")
			participant := func(context.Context, provenance.GovernedAllocationTransaction, provenance.GovernedAllocationRequest, provenance.OperationClosure) error {
				calls++
				return nil
			}
			fused, db := openFusedReceiptProof(t, path, "review-failure-arm-"+name, participant)
			actor := registerGovernedActor(t, fused.Tracker(), "review-failure-arm-"+name)
			root := initializeFusedRoot(t, fused, actor, "review-failure-arm-root-"+name)
			workflow := "review-success-" + name
			request := governedRequest("review-failure-arm-"+name, actor, root.AssignmentID, 1)
			if composed {
				_, err := fused.RunAllocateComposed(ctx, workflow, root.AssignmentRow.JournalID, composedGovernedRequest("review-failure-arm-"+name, actor, root, 1))
				if err != nil {
					t.Fatal(err)
				}
			} else if _, err := fused.RunAllocate(ctx, workflow, root.AssignmentRow.JournalID, request); err != nil {
				t.Fatal(err)
			}

			// Obtain a structurally valid Failure arm from the same production workflow,
			// rather than manufacturing malformed JSON.
			badAuthority := provenance.JournalID(0)
			failureWorkflow := "review-valid-failure-" + name
			if composed {
				// A committed OperationID is now classified before fresh authority
				// admission, so obtain the independent valid failure arm under a
				// fresh identity rather than trying to overwrite the success identity.
				_, _ = fused.RunAllocateComposed(ctx, failureWorkflow, badAuthority, composedGovernedRequest("review-valid-failure-arm-"+name, actor, root, 1))
			} else {
				_, _ = fused.RunAllocate(ctx, failureWorkflow, badAuthority, request)
			}
			var failureOutput string
			if err := db.QueryRow(`SELECT output FROM workflow_status WHERE workflow_uuid=?1`, failureWorkflow).Scan(&failureOutput); err != nil {
				t.Fatal(err)
			}
			if _, err := base64.StdEncoding.DecodeString(failureOutput); err != nil {
				t.Fatalf("failure fixture is not encoded DBOS output: %v", err)
			}
			if _, err := db.Exec(`UPDATE workflow_status SET output=?1 WHERE workflow_uuid=?2`, failureOutput, workflow); err != nil {
				t.Fatal(err)
			}
			before := snapshotGovernedTables(t, db)
			outputs := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1`, workflow)
			if err := fused.Close(30 * time.Second); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			// Reopen through the same real DBOS/SQLite constructor.
			fused, db = openFusedReceiptProof(t, path, "review-failure-arm-"+name, participant)
			var err error
			if composed {
				var got provenance.GovernedAllocationComposedResult
				got, err = fused.RunAllocateComposed(ctx, workflow, root.AssignmentRow.JournalID, composedGovernedRequest("review-failure-arm-"+name, actor, root, 1))
				if !reflect.DeepEqual(got, provenance.GovernedAllocationComposedResult{}) {
					t.Fatalf("forged failure returned receipt: %+v", got)
				}
			} else {
				var got provenance.OperationClosure
				got, err = fused.RunAllocate(ctx, workflow, root.AssignmentRow.JournalID, request)
				assertEmptyClosure(t, got)
			}
			mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
			if calls != 1 {
				t.Fatalf("participant calls=%d, want 1", calls)
			}
			assertNoGovernedWrites(t, before, db)
			if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1`, workflow); got != outputs {
				t.Fatalf("checkpoint count changed: %d -> %d", outputs, got)
			}
		})
	}
}

func TestReviewEvidenceComposedParticipantFailureUsesCompleteRollbackOracle(t *testing.T) {
	ctx := context.Background()
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "review-complete-rollback", func(ctx context.Context, tx provenance.GovernedAllocationTransaction, request provenance.GovernedAllocationRequest, closure provenance.OperationClosure) error {
		_, err := tx.Exec(ctx, `INSERT INTO fused_governed_participant_audit(operation_id,anchor_journal_id,child_task_id) VALUES(?1,?2,?3)`, request.OperationID, closure.AnchorJournalID(), closure.Children()[0].TaskID.String())
		if err != nil {
			return err
		}
		return errors.New("participant rejected commit")
	})
	createFusedGovernedParticipantAuditTable(t, db)
	actor := registerGovernedActor(t, fused.Tracker(), "review-complete-rollback")
	if err := fused.Launch(); err != nil {
		t.Fatal(err)
	}
	root := initializeFusedRoot(t, fused, actor, "review-complete-rollback-root")
	request := composedGovernedRequest("review-complete-rollback", actor, root, 1)
	workflow := "review-complete-rollback-workflow"
	before := reviewRollbackCounts(t, db)
	if _, err := fused.RunAllocateComposed(ctx, workflow, root.AssignmentRow.JournalID, request); err == nil {
		t.Fatal("participant failure succeeded")
	}
	after := reviewRollbackCounts(t, db)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("operation-scoped rollback oracle changed:\nbefore=%v\nafter=%v", before, after)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1 AND error IS NULL`, workflow); got != 0 {
		t.Fatalf("success checkpoints=%d", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1 AND error IS NOT NULL`, workflow); got != 1 {
		t.Fatalf("error checkpoints=%d, want 1", got)
	}
}

func reviewRollbackCounts(t *testing.T, db *sql.DB) []int {
	t.Helper()
	queries := []string{
		`SELECT COUNT(*) FROM governed_allocation_operations`, `SELECT COUNT(*) FROM governed_operation_effect_rows`, `SELECT COUNT(*) FROM governed_composed_supplement_owners`,
		`SELECT COUNT(*) FROM journal_operations`, `SELECT COUNT(*) FROM journal`, `SELECT COUNT(*) FROM journal_operation_result_slots`,
		`SELECT COUNT(*) FROM journal_task_event_contexts`, `SELECT COUNT(*) FROM journal_evidence_contexts`, `SELECT COUNT(*) FROM tasks`,
		`SELECT COUNT(*) FROM task_attributions`, `SELECT COUNT(*) FROM journal_authorities`, `SELECT COUNT(*) FROM journal_authority_assignment_episodes`,
		`SELECT COUNT(*) FROM journal_authority_assignment_transitions`, `SELECT COUNT(*) FROM journal_evidence`, `SELECT COUNT(*) FROM journal_task_events`,
		`SELECT COUNT(*) FROM edges`, `SELECT COUNT(*) FROM activities`, `SELECT COUNT(*) FROM journal_activity_creations`, `SELECT COUNT(*) FROM fused_governed_participant_audit`,
	}
	result := make([]int, len(queries))
	for i, query := range queries {
		result[i] = countFusedGovernedRows(t, db, query)
	}
	return result
}

func TestReviewEvidenceForeignProducerMutationTargetsCanonicalRowAndExtraRowSeparately(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(map[bool]string{false: "canonical", true: "extra"}[extra], func(t *testing.T) {
			ctx := context.Background()
			fused, db := openFusedAllocatorWithDatabase(t, "review-foreign-producer")
			actor := registerGovernedActor(t, fused.Tracker(), "review-foreign-producer")
			if err := fused.Launch(); err != nil {
				t.Fatal(err)
			}
			root := initializeFusedRoot(t, fused, actor, "review-foreign-producer-root")
			request := composedGovernedRequest("review-foreign-producer", actor, root, 1)
			if _, err := fused.RunAllocateComposed(ctx, "review-foreign-producer-original", root.AssignmentRow.JournalID, request); err != nil {
				t.Fatal(err)
			}
			if extra {
				result, err := db.Exec(`INSERT INTO journal(kind_id,recorded_at,produced_by_operation_journal_id) SELECT kind_id,recorded_at,(SELECT journal_id FROM journal_operations WHERE operation_id=?1) FROM journal WHERE journal_id=(SELECT produced_journal_id FROM journal_operation_result_slots WHERE result_slot_id='slice-event')`, composedSupplementOperationIDForTest(request.Allocation.OperationID))
				if err != nil {
					t.Fatal(err)
				}
				extraRow, err := result.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
				mustMutateComposedReceipt(t, db, `INSERT INTO journal_task_events(journal_id,task_id,event_kind,payload) VALUES(?1,?2,'provenance.extra','{}')`, extraRow, root.TaskID.String())
			} else {
				mustMutateComposedReceipt(t, db, `UPDATE journal SET produced_by_operation_journal_id=(SELECT MIN(journal_id) FROM journal_operations) WHERE journal_id=(SELECT produced_journal_id FROM journal_operation_result_slots WHERE result_slot_id='slice-event')`)
			}
			got, err := fused.RunAllocateComposed(ctx, "review-foreign-producer-retry", root.AssignmentRow.JournalID, request)
			mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
			if !reflect.DeepEqual(got, provenance.GovernedAllocationComposedResult{}) {
				t.Fatalf("tamper returned receipt: %+v", got)
			}
		})
	}
}

func TestReviewEvidenceFreshWorkflowRejectsWrongParentAuthorityBeforeWrites(t *testing.T) {
	ctx := context.Background()
	calls := 0
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "review-wrong-parent", func(context.Context, provenance.GovernedAllocationTransaction, provenance.GovernedAllocationRequest, provenance.OperationClosure) error {
		calls++
		return nil
	})
	actor := registerGovernedActor(t, fused.Tracker(), "review-wrong-parent")
	if err := fused.Launch(); err != nil {
		t.Fatal(err)
	}
	root := initializeFusedRoot(t, fused, actor, "review-wrong-parent-root")
	foreign := governedRequest("review-foreign-authority", actor, root.AssignmentID, 1)
	foreignResult, err := fused.RunAllocate(ctx, "review-foreign-authority-workflow", root.AssignmentRow.JournalID, foreign)
	if err != nil {
		t.Fatal(err)
	}
	wrong := foreignResult.Children()[0].AssignmentRow.JournalID
	request := governedRequest("review-wrong-parent", actor, root.AssignmentID, 1)
	before := snapshotGovernedTables(t, db)
	beforeCalls := calls
	result, err := fused.RunAllocate(ctx, "review-wrong-parent-fresh-workflow", wrong, request)
	mustGovernedError(t, err, provenance.GovernedAllocationAuthority)
	assertEmptyClosure(t, result)
	if calls != beforeCalls {
		t.Fatalf("participant ran for wrong authority: %d -> %d", beforeCalls, calls)
	}
	assertNoGovernedWrites(t, before, db)
}

func TestReviewEvidenceJoinedParticipantAndCleanupFailureCannotAuthenticateDomainRejection(t *testing.T) {
	ctx := context.Background()
	domainLike := &provenance.GovernedAllocationError{Kind: provenance.GovernedAllocationAuthority, Operation: "forged-domain", Why: "participant supplied", Impact: "none", Fix: "none"}
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "review-joined-failure", func(context.Context, provenance.GovernedAllocationTransaction, provenance.GovernedAllocationRequest, provenance.OperationClosure) error {
		return errors.Join(domainLike, errors.New("injected rollback cleanup failure"))
	})
	createFusedGovernedParticipantAuditTable(t, db)
	actor := registerGovernedActor(t, fused.Tracker(), "review-joined-failure")
	if err := fused.Launch(); err != nil {
		t.Fatal(err)
	}
	root := initializeFusedRoot(t, fused, actor, "review-joined-failure-root")
	request := composedGovernedRequest("review-joined-failure", actor, root, 1)
	before := reviewRollbackCounts(t, db)
	result, err := fused.RunAllocateComposed(ctx, "review-joined-failure-workflow", root.AssignmentRow.JournalID, request)
	if err == nil {
		t.Fatal("joined participant/cleanup failure succeeded")
	}
	var governed *provenance.GovernedAllocationError
	if errors.As(err, &governed) {
		t.Fatalf("participant-controlled joined failure authenticated as domain rejection: %+v", governed)
	}
	if !reflect.DeepEqual(result, provenance.GovernedAllocationComposedResult{}) {
		t.Fatalf("joined failure returned receipt: %+v", result)
	}
	if after := reviewRollbackCounts(t, db); !reflect.DeepEqual(after, before) {
		t.Fatalf("joined failure left domain writes: before=%v after=%v", before, after)
	}
}
