package provenance

import (
	"context"
	"fmt"
	"reflect"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/dayvidpham/provenance/internal/journal"
)

// TransferAssignment runs Session.TransferAssignment through the adapter's
// durable DBOS workflow path. The Session remains the authority capability and
// the request remains the public transfer contract; DBOS supplies retry and
// recovery only, never an alternate transfer rule or result ledger.
func (a *DBOSAdapter) TransferAssignment(ctx context.Context, session *Session, request AssignmentTransferRequest, opts ...ApplyOption) (AssignmentTransferResult, error) {
	bound, err := a.transferSession(session)
	if err != nil {
		return AssignmentTransferResult{}, err
	}
	cfg := bound.resolveAssignmentTransfer(request, opts)
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return AssignmentTransferResult{}, &ApplyWaitCanceledError{
			Operation: cfg.opID,
			Stage:     DBOSDiagStageApplyPreStart,
			Impact:    "no workflow was started; nothing was committed",
			Fix:       "retry with an un-cancelled context to start the transfer",
			Cause:     context.Cause(ctx),
		}
	}
	if err := bound.checkGate("TransferAssignment"); err != nil {
		return AssignmentTransferResult{}, err
	}
	if err := bound.requireInitialized("TransferAssignment"); err != nil {
		return AssignmentTransferResult{}, err
	}

	input, normalized, err := encodeAssignmentTransferInput(a.contract, bound.assignmentTransferOperationInput(request, cfg))
	if err != nil {
		return AssignmentTransferResult{}, fmt.Errorf("provenance.DBOSAdapter.TransferAssignment: canonicalize transfer operation %q: %w", cfg.opID, err)
	}
	workflowID := a.contract.workflowPrefix + workflowIdentityForKind(a.contract, a.applicationVersion, normalized.OperationID, input.Kind)
	workflows, err := lookupExistingWorkflows(a.root, workflowID, normalized.OperationID)
	if err != nil {
		return AssignmentTransferResult{}, err
	}
	if err := listedTerminalWorkflowDiagnostic(workflows, workflowID, normalized.OperationID); err != nil {
		return AssignmentTransferResult{}, err
	}

	// Admission must consult the core transfer path for a committed operation:
	// exact replay wins before predecessor liveness, while changed input remains a
	// typed conflict without entering another DBOS workflow.
	existing, err := a.tracker.Journal().LookupCommitted(normalized.OperationID)
	if err != nil {
		return AssignmentTransferResult{}, fmt.Errorf("provenance.DBOSAdapter.TransferAssignment: preflight LookupCommitted for operation %q: %w", normalized.OperationID, err)
	}
	if existing.Kind == journal.CommittedExact {
		if _, err := bound.TransferAssignment(request, assignmentTransferReplayOptions(normalized)...); err != nil {
			return AssignmentTransferResult{}, err
		}
	}

	fp, err := fingerprint(a.contract, a.applicationVersion, input)
	if err != nil {
		return AssignmentTransferResult{}, fmt.Errorf("provenance.DBOSAdapter.TransferAssignment: derive workflow identity for operation %q: %w", normalized.OperationID, err)
	}
	exists, identityErr := listedWorkflowDiagnostic(workflows, a.contract, a.applicationVersion, workflowID, fp, normalized.OperationID)
	if identityErr != nil {
		return AssignmentTransferResult{}, identityErr
	}
	if exists {
		outcome, err := awaitWorkflowResult[DBOSStepOutcome](ctx, a.root, workflowID, normalized.OperationID, a.resultPollingInterval)
		if err != nil {
			return AssignmentTransferResult{}, err
		}
		if _, err := a.postValidate(normalized, outcome); err != nil {
			return AssignmentTransferResult{}, err
		}
		return assignmentTransferResult(request, true), nil
	}

	if _, err := dbos.RunWorkflow(
		a.root,
		a.transferWorkflow,
		input,
		dbos.WithWorkflowID(workflowID),
		dbos.WithApplicationVersion(a.applicationVersion),
	); err != nil {
		return AssignmentTransferResult{}, diagnostic(DBOSDiagClassStepRetry, DBOSDiagFieldWorkflow,
			DBOSDiagStageStepCheckpoint, normalized.OperationID, workflowID,
			"DBOS rejected durable transfer workflow start", "the transfer may not have started",
			"ensure the adapter root is launched and retry the same OperationID to attach", err)
	}
	if _, identityErr := existingWorkflowDiagnostic(a.root, a.contract, a.applicationVersion, workflowID, fp, normalized.OperationID); identityErr != nil {
		return AssignmentTransferResult{}, identityErr
	}

	outcome, err := awaitWorkflowResult[DBOSStepOutcome](ctx, a.root, workflowID, normalized.OperationID, a.resultPollingInterval)
	if err != nil {
		return AssignmentTransferResult{}, err
	}
	if _, err := a.postValidate(normalized, outcome); err != nil {
		return AssignmentTransferResult{}, err
	}
	return assignmentTransferResult(request, outcome.ShortCircuited), nil
}

func (a *DBOSAdapter) transferSession(session *Session) (*Session, error) {
	if a == nil || a.root == nil || a.tracker == nil {
		return nil, fmt.Errorf("provenance.DBOSAdapter.TransferAssignment: adapter is nil or uninitialized -- where: durable transfer entry; impact: no workflow or transfer was started; fix: construct the adapter with NewDBOSAdapter before use")
	}
	if session == nil || session.tr == nil {
		return nil, fmt.Errorf("provenance.DBOSAdapter.TransferAssignment: Session is nil or uninitialized -- where: durable transfer entry; impact: no workflow or transfer was started; fix: pass the Session returned by the adapter tracker As method")
	}
	bound := a.tracker.As(session.actor, session.authority)
	if bound == nil || bound.tr == nil || bound.tr != session.tr {
		return nil, fmt.Errorf("provenance.DBOSAdapter.TransferAssignment: Session is not bound to the adapter tracker -- where: durable transfer entry; impact: no workflow or transfer was started, preventing a split DBOS/domain store; fix: create the Session with the same borrowed tracker passed to NewDBOSAdapter")
	}
	return bound, nil
}

func assignmentTransferReplayOptions(in journal.OperationInput) []ApplyOption {
	return []ApplyOption{
		WithOperationID(in.OperationID),
		WithCommandDigest(in.CommandDigest),
		WithMutationDigest(in.MutationDigest),
	}
}

func assignmentTransferResult(request AssignmentTransferRequest, replayed bool) AssignmentTransferResult {
	return AssignmentTransferResult{
		TaskID:               request.TaskID,
		SlotID:               request.SlotID,
		PreviousAssignmentID: request.PreviousAssignmentID,
		NextAssignmentID:     request.NextAssignmentID,
		NextOccupant:         request.NextOccupant,
		Replayed:             replayed,
	}
}

func (a *DBOSAdapter) transferWorkflow(wfCtx dbos.Context, input DBOSApplyInput) (DBOSStepOutcome, error) {
	return a.runDomainWorkflow(wfCtx, input, "transferWorkflow", decodeAssignmentTransferInput, a.foldDomainAssignmentTransfer)
}

func (a *DBOSAdapter) foldDomainAssignmentTransfer(in journal.OperationInput) (CommittedResult, error) {
	request, err := assignmentTransferRequestFromOperation(in)
	if err != nil {
		return CommittedResult{}, err
	}
	authority := JournalID(0)
	if in.AuthorityJournalID != nil {
		authority = *in.AuthorityJournalID
	}
	session := a.tracker.As(in.ActorID, authority)
	if session == nil {
		return CommittedResult{}, fmt.Errorf("provenance.DBOSAdapter.TransferAssignment: adapter tracker returned a nil Session -- where: durable transfer workflow; impact: no core transfer was attempted; fix: recreate the adapter with a functional borrowed tracker")
	}
	result, err := session.TransferAssignment(request, assignmentTransferReplayOptions(in)...)
	if err != nil {
		return CommittedResult{}, err
	}
	committed, err := a.tracker.Journal().LookupCommitted(in.OperationID)
	if err != nil {
		return CommittedResult{}, fmt.Errorf("provenance.DBOSAdapter.TransferAssignment: read committed transfer %q after core fold: %w", in.OperationID, err)
	}
	if committed.Kind != journal.CommittedExact {
		return CommittedResult{}, fmt.Errorf("provenance.DBOSAdapter.TransferAssignment: core transfer %q returned success but LookupCommitted is %s -- where: durable transfer workflow; impact: no success checkpoint is trusted; fix: inspect the shared journal operation record before retrying", in.OperationID, committed.Kind)
	}
	committed.ShortCircuited = result.Replayed
	return committed, nil
}

// assignmentTransferRequestFromOperation binds the transfer workflow's existing
// canonical operation envelope back to the sole public request shape before it
// calls Session.TransferAssignment. It is a transport check, not a second
// canonicalizer: canonical mutation validation remains journal.Canonicalize.
func assignmentTransferRequestFromOperation(in journal.OperationInput) (AssignmentTransferRequest, error) {
	if len(in.Conditions) != 0 || len(in.Effects) != 2 {
		return AssignmentTransferRequest{}, fmt.Errorf("%w: DBOS transfer operation must contain exactly the canonical two effects and no conditions", ErrCanonicalMutation)
	}
	end, start := in.Effects[0], in.Effects[1]
	request := AssignmentTransferRequest{
		TaskID:               end.TaskID,
		SlotID:               end.SlotID,
		PreviousAssignmentID: end.AssignmentID,
		NextAssignmentID:     start.AssignmentID,
		NextOccupant:         start.Occupant,
	}
	if !reflect.DeepEqual(in.Effects, assignmentTransferEffects(request)) {
		return AssignmentTransferRequest{}, fmt.Errorf("%w: DBOS transfer operation does not exactly encode AssignmentTransferRequest", ErrCanonicalMutation)
	}
	return request, nil
}
