package provenance_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	provenance "github.com/dayvidpham/provenance"
	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/google/uuid"
)

// Every top-level test in this file is parallel under the isolation proof
// documented above openGovernedTracker in governed_allocation_integration_test.go:
// each test owns a private t.TempDir database, a private DBOS application name,
// and a snapshot closure local to itself.

// TestGovernedAllocationLateActivityFailureRollsBackWholeV1Fold exercises the
// production DBOS transaction with ActivityCreate last, after the other three V1
// families have written. Every operation-owned row must disappear together while
// DBOS retains only its terminal error checkpoint.
func TestGovernedAllocationLateActivityFailureRollsBackWholeV1Fold(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "v1-late-activity-rollback")
	actor := registerGovernedActor(t, fused.Tracker(), "v1-late-activity-rollback")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "v1-late-activity-root")
	request := composedGovernedRequest("v1-late-activity-rollback", actor, root, 1)
	request.SupplementalEffects[3].ActivityAgentID = provenance.ActorID{Namespace: "governed", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("missing-v1-activity-agent"))}
	workflowID := "v1-late-activity-rollback-workflow"
	child := request.Allocation.Children[0]
	activity := request.SupplementalEffects[3].ActivityID
	internalOperation := journal.NewGovernedAllocationSupplementOperationID(journal.OperationID(request.Allocation.OperationID)).OperationID()

	// Snapshot every table touched by the allocation and supplemental reducers.
	// The fixture already owns durable root rows, so comparing before and after is
	// stronger than merely expecting operation-specific rows to be absent.
	snapshotQueries := []string{
		`SELECT COUNT(*) FROM governed_allocation_operations`,
		`SELECT COUNT(*) FROM governed_operation_effect_rows`,
		`SELECT COUNT(*) FROM governed_composed_supplement_owners`,
		`SELECT COUNT(*) FROM journal_operations`,
		`SELECT COUNT(*) FROM journal`,
		`SELECT COUNT(*) FROM journal_operation_result_slots`,
		`SELECT COUNT(*) FROM journal_authorities`,
		`SELECT COUNT(*) FROM journal_authority_assignment_episodes`,
		`SELECT COUNT(*) FROM journal_authority_assignment_transitions`,
		`SELECT COUNT(*) FROM journal_task_event_contexts`,
		`SELECT COUNT(*) FROM journal_evidence_contexts`,
		`SELECT COUNT(*) FROM journal_evidence`,
		`SELECT COUNT(*) FROM journal_task_events`,
		`SELECT COUNT(*) FROM edges`,
		`SELECT COUNT(*) FROM activities`,
		`SELECT COUNT(*) FROM journal_activity_creations`,
		`SELECT COUNT(*) FROM tasks`,
		`SELECT COUNT(*) FROM task_attributions`,
	}
	snapshot := func() []int {
		state := make([]int, len(snapshotQueries))
		for index, query := range snapshotQueries {
			state[index] = countFusedGovernedRows(t, db, query)
		}
		return state
	}
	before := snapshot()

	assertActivityFailure := func(label string, result provenance.GovernedAllocationComposedResult, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s late ActivityCreate reducer failure unexpectedly succeeded", label)
		}
		if !reflect.DeepEqual(result, provenance.GovernedAllocationComposedResult{}) {
			t.Fatalf("%s late ActivityCreate reducer failure returned a public receipt: %+v", label, result)
		}
		var governed *provenance.GovernedAllocationError
		if !errors.As(err, &governed) {
			t.Fatalf("%s late ActivityCreate reducer failure type=%T, want *provenance.GovernedAllocationError: %v", label, err, err)
		}
		if governed.Kind != provenance.GovernedAllocationValidation ||
			governed.Operation != request.Allocation.OperationID ||
			governed.Where != "composed supplemental EffectActivityCreate" ||
			governed.Why == "" || governed.Impact == "" || governed.Fix == "" {
			t.Fatalf("%s late ActivityCreate reducer error is not typed, stage-specific, and actionable: %+v", label, governed)
		}
		for _, phrase := range []string{"agent", "rolled back", "register"} {
			if !strings.Contains(strings.ToLower(governed.Why+" "+governed.Impact+" "+governed.Fix), phrase) {
				t.Fatalf("%s late ActivityCreate reducer error lacks actionable %q detail: %+v", label, phrase, governed)
			}
		}
	}

	result, err := fused.RunAllocateComposed(ctx, workflowID, root.AssignmentRow.JournalID, request)
	assertActivityFailure("first attempt", result, err)
	if after := snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("late ActivityCreate failure changed reducer-owned durable state:\nbefore=%v\nafter=%v", before, after)
	}

	// The same workflow ID is a DBOS replay of its terminal error checkpoint. It
	// must preserve the public typed contract without entering either reducer (or
	// reaching a composed participant) a second time.
	retried, retryErr := fused.RunAllocateComposed(ctx, workflowID, root.AssignmentRow.JournalID, request)
	assertActivityFailure("same-workflow retry", retried, retryErr)
	if afterRetry := snapshot(); !reflect.DeepEqual(afterRetry, before) {
		t.Fatalf("same-workflow ActivityCreate retry changed reducer-owned durable state:\nbefore=%v\nafter=%v", before, afterRetry)
	}
	checks := []struct {
		name  string
		query string
		args  []any
	}{
		{"allocation task/projection/watermark", `SELECT COUNT(*) FROM tasks WHERE id=?1`, []any{child.TaskID.String()}},
		{"allocation operation receipt", `SELECT COUNT(*) FROM governed_allocation_operations WHERE operation_id=?1`, []any{request.Allocation.OperationID}},
		{"allocation effect rows", `SELECT COUNT(*) FROM governed_operation_effect_rows e JOIN governed_allocation_operations o ON o.anchor_journal_id=e.anchor_journal_id WHERE o.operation_id=?1`, []any{request.Allocation.OperationID}},
		{"supplement marker", `SELECT COUNT(*) FROM governed_composed_supplement_owners WHERE governed_operation_id=?1`, []any{request.Allocation.OperationID}},
		{"supplement operation", `SELECT COUNT(*) FROM journal_operations WHERE operation_id=?1`, []any{internalOperation}},
		{"journal rows", `SELECT COUNT(*) FROM journal j JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE o.operation_id=?1`, []any{internalOperation}},
		{"result slots", `SELECT COUNT(*) FROM journal_operation_result_slots s JOIN journal_operations o ON o.journal_id=s.journal_id WHERE o.operation_id=?1`, []any{internalOperation}},
		{"task contexts", `SELECT COUNT(*) FROM journal_task_event_contexts c JOIN journal_task_events e ON e.journal_id=c.event_journal_id WHERE e.task_id=?1`, []any{child.TaskID.String()}},
		{"evidence contexts", `SELECT COUNT(*) FROM journal_evidence_contexts c JOIN journal_evidence e ON e.journal_id=c.evidence_journal_id WHERE e.task_id=?1`, []any{child.TaskID.String()}},
		{"evidence", `SELECT COUNT(*) FROM journal_evidence WHERE task_id=?1`, []any{child.TaskID.String()}},
		{"events", `SELECT COUNT(*) FROM journal_task_events WHERE task_id=?1`, []any{child.TaskID.String()}},
		{"edges", `SELECT COUNT(*) FROM edges WHERE source_id=?1 AND target_id=?2`, []any{root.TaskID.String(), child.TaskID.String()}},
		{"activity", `SELECT COUNT(*) FROM activities WHERE id=?1`, []any{activity.String()}},
		{"activity attribution", `SELECT COUNT(*) FROM journal_activity_creations WHERE activity_id=?1`, []any{activity.String()}},
		{"task attribution", `SELECT COUNT(*) FROM task_attributions WHERE task_id=?1`, []any{child.TaskID.String()}},
	}
	for _, check := range checks {
		if got := countFusedGovernedRows(t, db, check.query, check.args...); got != 0 {
			t.Errorf("%s survived rollback: got %d rows", check.name, got)
		}
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1 AND error IS NULL`, workflowID); got != 0 {
		t.Fatalf("successful operation output survived rollback: %d", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1 AND error IS NOT NULL`, workflowID); got != 1 {
		t.Fatalf("typed terminal error checkpoint count=%d, want 1", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM governed_allocation_operations WHERE operation_id=?1`, request.Allocation.OperationID); got != 0 {
		t.Fatalf("public governed allocation receipt survived rollback: %d", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM journal_operations WHERE operation_id=?1`, internalOperation); got != 0 {
		t.Fatalf("internal supplemental receipt survived rollback: %d", got)
	}

	var encoded string
	if err := db.QueryRow(`SELECT output FROM workflow_status WHERE workflow_uuid=?1`, workflowID).Scan(&encoded); err != nil {
		t.Fatalf("read durable composed failure output: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode durable composed failure output: %v", err)
	}
	var output any
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatalf("decode durable composed failure JSON: %v", err)
	}
	mutated := false
	var mutateWhy func(any)
	mutateWhy = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			for key, child := range value {
				if key == "Why" && !mutated {
					value[key], mutated = "forged nonempty reducer explanation", true
					continue
				}
				mutateWhy(child)
			}
		case []any:
			for _, child := range value {
				mutateWhy(child)
			}
		}
	}
	mutateWhy(output)
	if !mutated {
		t.Fatalf("durable composed failure output has no mutable Failure.Why: %s", raw)
	}
	forged, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("encode tampered composed failure output: %v", err)
	}
	if _, err := db.Exec(`UPDATE workflow_status SET output=?1 WHERE workflow_uuid=?2`, base64.StdEncoding.EncodeToString(forged), workflowID); err != nil {
		t.Fatalf("tamper durable composed Failure.Why: %v", err)
	}
	checkpointsBeforeForgedRetry := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1`, workflowID)
	forgedResult, forgedErr := fused.RunAllocateComposed(ctx, workflowID, root.AssignmentRow.JournalID, request)
	if !reflect.DeepEqual(forgedResult, provenance.GovernedAllocationComposedResult{}) {
		t.Fatalf("tampered failure retry returned a public receipt: %+v", forgedResult)
	}
	var forgedGoverned *provenance.GovernedAllocationError
	if !errors.As(forgedErr, &forgedGoverned) || forgedGoverned.Kind != provenance.GovernedAllocationCorruption {
		t.Fatalf("tampered Failure.Why retry error=%T %v, want governed corruption", forgedErr, forgedErr)
	}
	if afterForged := snapshot(); !reflect.DeepEqual(afterForged, before) {
		t.Fatalf("tampered Failure.Why retry changed reducer-owned durable state:\nbefore=%v\nafter=%v", before, afterForged)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1`, workflowID); got != checkpointsBeforeForgedRetry {
		t.Fatalf("tampered Failure.Why retry added a checkpoint: before=%d after=%d", checkpointsBeforeForgedRetry, got)
	}
}
