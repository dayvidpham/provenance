package provenance

// dbos_adapter.go is the durable-execution adapter that runs one Provenance
// operation as a deterministic DBOS workflow whose single step is the atomic
// journal fold (issue dayvidpham/provenance#6). It introduces no parallel commit
// ledger: the workflow/step guards and restart semantics are bound to the SAME
// OperationID alternate key and journal contract the reducer already enforces, and
// post-checkpoint validation compares the journal-anchored committed operation.
//
// The adapter targets the repository-pinned DBOS library version, authorized by
// the user at Impl-UAT C7a.
//
// Identity constants and retry-option types are defined in dbos_contract.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/dayvidpham/provenance/internal/journal"
)

// DBOSAdapterConfig configures a DBOSAdapter.
// It is the only public configuration surface; no functional options, raw DBOS
// options, second constructor, setters, exported resolver, recovery API, or
// mutable runtime config exists.
type DBOSAdapterConfig struct {
	// ExpectedApplicationVersion, when non-empty, is asserted equal to the DBOS
	// root's actual application version at construction; a mismatch rejects before
	// the workflow is registered or anything is written.
	ExpectedApplicationVersion string
	// StepOptions configures retry behaviour for the DBOS step wrapping each
	// atomic domain fold. Zero fields use package defaults (3 retries, 50 ms,
	// factor 2). Nonzero fields are validated and copied before registration.
	StepOptions DBOSStepOptions
	// ResultPollingInterval controls how often a waiting Apply checks DBOS for a
	// workflow result. Zero uses 50 ms. Valid nonzero range: 10 ms through 5 s.
	ResultPollingInterval time.Duration
}

// applyTestHooks are unexported, no-op-by-default seams the crash-gap subprocess
// tests replace around durable boundaries. onWorkflowEntry observes the first
// instruction of a registered callback and is never consulted for behavior;
// beforeDomainCommit fires before the fold, afterDomainCommit after commit but
// before checkpoint, and afterStepCheckpoint after checkpoint but before workflow
// completion. Production leaves all hooks as no-ops.
type applyTestHooks struct {
	onWorkflowEntry     func()
	beforeDomainCommit  func()
	afterDomainCommit   func()
	afterStepCheckpoint func()
}

func defaultApplyTestHooks() applyTestHooks {
	return applyTestHooks{
		onWorkflowEntry:     func() {},
		beforeDomainCommit:  func() {},
		afterDomainCommit:   func() {},
		afterStepCheckpoint: func() {},
	}
}

// DBOSAdapter runs Provenance operations as durable DBOS workflows over a shared
// journal contract. The adapter captures its contract snapshot once at
// construction; no paths rebuild or mutate the identity after registration.
type DBOSAdapter struct {
	root                  dbos.Context
	tracker               Tracker
	applicationVersion    string
	contract              dbosContractSnapshot
	stepOptions           resolvedDBOSStepOptions
	resultPollingInterval time.Duration
	testHooks             applyTestHooks
}

// NewDBOSAdapter validates the root and tracker, resolves step options (applying
// defaults for zero fields and validating nonzero overrides), derives the actual
// application version from the DBOS root, rejects a non-empty mismatched
// expectation, captures the unversioned DBOS contract snapshot, and registers
// the workflow on the not-yet-launched root.
//
// All validation occurs before registration so partial registration is impossible.
func NewDBOSAdapter(root dbos.Context, tracker Tracker, config DBOSAdapterConfig) (*DBOSAdapter, error) {
	if root == nil {
		return nil, fmt.Errorf(
			"provenance.NewDBOSAdapter: the DBOS root context is nil -- where: adapter construction; " +
				"impact: no durable workflow can be registered or run; fix: pass the DBOS root created with " +
				"NewContext(SQLiteSystemDB=...) before calling Launch")
	}
	if tracker == nil {
		return nil, fmt.Errorf(
			"provenance.NewDBOSAdapter: the Tracker is nil -- where: adapter construction; impact: no domain " +
				"fold target; fix: pass the borrowed tracker from OpenBorrowedSQLite over the same *sql.DB the " +
				"DBOS root uses")
	}

	// Resolve step options before touching the root or tracker, so any invalid
	// configuration is caught early and never partially applied.
	resolved, err := resolveDBOSStepOptions(config.StepOptions)
	if err != nil {
		return nil, err
	}
	resultPollingInterval, err := resolveDBOSResultPollingInterval(config.ResultPollingInterval)
	if err != nil {
		return nil, err
	}

	actual := root.GetApplicationVersion()
	if config.ExpectedApplicationVersion != "" && config.ExpectedApplicationVersion != actual {
		return nil, fmt.Errorf(
			"provenance.NewDBOSAdapter: expected application version %q but the DBOS root reports %q -- "+
				"where: adapter construction, before workflow registration; impact: nothing is registered or "+
				"written; fix: pass the matching version or leave ExpectedApplicationVersion empty to accept the "+
				"root's version",
			config.ExpectedApplicationVersion, actual)
	}

	// Capture the sole unversioned DBOS contract snapshot once before registration.
	contract := newDBOSContractSnapshot()

	a := &DBOSAdapter{
		root:                  root,
		tracker:               tracker,
		applicationVersion:    actual,
		contract:              contract,
		stepOptions:           resolved,
		resultPollingInterval: resultPollingInterval,
		testHooks:             defaultApplyTestHooks(),
	}
	dbos.RegisterWorkflow(root, a.applyWorkflow)
	dbos.RegisterWorkflow(root, a.transferWorkflow)
	return a, nil
}

// Apply runs one operation as a durable workflow and returns its journal-anchored
// committed result. It requires the adapter root to be launched but accepts an
// ordinary context.Context: the caller context controls pre/post work and
// cancellation reporting only, while the durable workflow runs on the adapter root.
// An already-cancelled context starts no workflow.
func (a *DBOSAdapter) Apply(ctx context.Context, in journal.OperationInput) (CommittedResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CommittedResult{}, &ApplyWaitCanceledError{
			Operation: in.OperationID,
			Stage:     DBOSDiagStageApplyPreStart,
			Impact:    "no workflow was started; nothing was committed",
			Fix:       "retry with an un-cancelled context to start the operation",
			Cause:     context.Cause(ctx),
		}
	}

	input, normalized, err := encodeApplyInput(a.contract, in)
	if err != nil {
		return CommittedResult{}, fmt.Errorf("provenance.DBOSAdapter.Apply: canonicalize operation %q: %w", in.OperationID, err)
	}
	// Reserved syntax was valid before composed allocation existed, so durable
	// workflow decoding remains syntactic. Classify it against durable ownership
	// before any DBOS lookup/start: only an exact unmarked historical replay may
	// continue; fresh or composition-owned identities fail without a checkpoint.
	if journal.IsReservedInternalOperationID(normalized.OperationID) {
		journalAPI := a.tracker.Journal()
		var applyErr error
		if contextual, ok := journalAPI.(ContextJournal); ok {
			_, applyErr = contextual.ApplyContext(ctx, normalized)
		} else {
			_, applyErr = journalAPI.Apply(normalized)
		}
		if applyErr != nil {
			return CommittedResult{}, fmt.Errorf("provenance.DBOSAdapter.Apply: classify reserved operation %q before workflow work: %w", normalized.OperationID, applyErr)
		}
	}

	// Workflow identity is operation-scoped and available immediately after
	// canonical validation. Inspect it before any journal access so terminal DBOS
	// ERROR remains authoritative even if journal state later changes or fails.
	workflowID := a.contract.workflowPrefix + workflowIdentity(a.contract, a.applicationVersion, normalized.OperationID)
	workflows, err := lookupExistingWorkflows(a.root, workflowID, normalized.OperationID)
	if err != nil {
		return CommittedResult{}, err
	}
	if err := listedTerminalWorkflowDiagnostic(workflows, workflowID, normalized.OperationID); err != nil {
		return CommittedResult{}, err
	}

	// DBOS executes zero callbacks when a workflow is already complete. Ask the
	// journal's reviewed replay path to validate the proposed canonical effects
	// before attaching. It performs no writes for an existing operation and is the
	// sole authority for the allocated-create UUID exception.
	existing, err := a.tracker.Journal().LookupCommitted(normalized.OperationID)
	if err != nil {
		return CommittedResult{}, fmt.Errorf("provenance.DBOSAdapter.Apply: preflight LookupCommitted for operation %q: %w", normalized.OperationID, err)
	}
	if existing.Kind == journal.CommittedExact {
		journalAPI := a.tracker.Journal()
		var validated CommittedResult
		var applyErr error
		if contextual, ok := journalAPI.(ContextJournal); ok {
			validated, applyErr = contextual.ApplyContext(ctx, normalized)
		} else {
			validated, applyErr = journalAPI.Apply(normalized)
		}
		if applyErr != nil {
			return CommittedResult{}, applyErr
		}
		normalized, err = reconcileDBOSAllocatedCreates(normalized, validated)
		if err != nil {
			return CommittedResult{}, err
		}
		input, normalized, err = encodeApplyInput(a.contract, normalized)
		if err != nil {
			return CommittedResult{}, fmt.Errorf("provenance.DBOSAdapter.Apply: canonicalize reconciled operation %q: %w", in.OperationID, err)
		}
	}
	fp, err := fingerprint(a.contract, a.applicationVersion, input)
	if err != nil {
		return CommittedResult{}, fmt.Errorf("provenance.DBOSAdapter.Apply: derive workflow identity for operation %q: %w", in.OperationID, err)
	}
	// Reuse the strict lookup result. For nonterminal rows, the full canonical
	// fingerprint remains the step/input collision guard after allocated-create
	// reconciliation has produced the final canonical input.
	exists, identityErr := listedWorkflowDiagnostic(workflows, a.contract, a.applicationVersion, workflowID, fp, normalized.OperationID)
	if identityErr != nil {
		return CommittedResult{}, identityErr
	}
	// Always retrieve before starting. Existing SUCCESS, ERROR, or in-flight state
	// attaches read-only; only an explicit NonExistentWorkflowError may create a row.
	// In particular, terminal ERROR replay must not call RunWorkflow because DBOS
	// updates workflow_status.updated_at even when it executes zero callbacks.
	var outcome DBOSStepOutcome
	if exists {
		outcome, err = awaitWorkflowResult[DBOSStepOutcome](ctx, a.root, workflowID, normalized.OperationID, a.resultPollingInterval)
		if err != nil {
			return CommittedResult{}, err
		}
		return a.postValidate(normalized, outcome)
	}

	// Start (or attach to) the durable workflow on the UN-CANCELLED adapter root, so
	// caller cancellation never cancels durable work.
	if _, err := dbos.RunWorkflow(
		a.root,
		a.applyWorkflow,
		input,
		dbos.WithWorkflowID(workflowID),
		dbos.WithApplicationVersion(a.applicationVersion),
	); err != nil {
		return CommittedResult{}, diagnostic(DBOSDiagClassStepRetry, DBOSDiagFieldWorkflow,
			DBOSDiagStageStepCheckpoint, normalized.OperationID, workflowID,
			"DBOS rejected durable workflow start", "the operation may not have started",
			"ensure the adapter root is launched and retry the same OperationID to attach", err)
	}
	// Another caller may have inserted this operation-scoped workflow between the
	// preflight and RunWorkflow. Re-read its stored input before awaiting it.
	if _, identityErr := existingWorkflowDiagnostic(a.root, a.contract, a.applicationVersion, workflowID, fp, normalized.OperationID); identityErr != nil {
		return CommittedResult{}, identityErr
	}

	outcome, err = awaitWorkflowResult[DBOSStepOutcome](ctx, a.root, workflowID, normalized.OperationID, a.resultPollingInterval)
	if err != nil {
		return CommittedResult{}, err
	}
	return a.postValidate(normalized, outcome)
}

func existingWorkflowDiagnostic(root dbos.Context, contract dbosContractSnapshot, applicationVersion, workflowID, requestedFingerprint string, operation OperationID) (bool, error) {
	workflows, err := lookupExistingWorkflows(root, workflowID, operation)
	if err != nil {
		return false, err
	}
	return listedWorkflowDiagnostic(workflows, contract, applicationVersion, workflowID, requestedFingerprint, operation)
}

func lookupExistingWorkflows(root dbos.Context, workflowID string, operation OperationID) ([]dbos.WorkflowStatus, error) {
	workflows, err := dbos.ListWorkflows(root, dbos.WithFilterWorkflowIDs(workflowID), dbos.WithFilterLimit(2), dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(true))
	if err != nil {
		return nil, diagnostic(DBOSDiagClassTerminalRetrieval, DBOSDiagFieldWorkflow,
			DBOSDiagStageWorkflowTerminalLookup, operation, workflowID,
			"DBOS could not inspect the durable workflow identity", "no callback or durable write is attempted without a collision check",
			"repair DBOS storage availability, then retry the same OperationID", err)
	}
	return workflows, nil
}

func listedTerminalWorkflowDiagnostic(workflows []dbos.WorkflowStatus, workflowID string, operation OperationID) error {
	if len(workflows) == 0 {
		return nil
	}
	if len(workflows) != 1 || workflows[0].ID != workflowID || !knownWorkflowStatus(workflows[0].Status) {
		_, err := listedWorkflowDiagnostic(workflows, newDBOSContractSnapshot(), "", workflowID, "", operation)
		return err
	}
	workflow := workflows[0]
	if workflow.Status != dbos.WorkflowStatusError {
		return nil
	}
	cause := workflow.Error
	if cause == nil {
		cause = errors.New("DBOS persisted terminal ERROR without structured error details")
	}
	return diagnostic(DBOSDiagClassTerminalRetrieval, DBOSDiagFieldWorkflow,
		DBOSDiagStageWorkflowTerminalLookup, operation, workflowID,
		"DBOS reports the workflow is already terminal ERROR", "no callback or durable write is attempted by same-ID replay",
		"repair the dependency and issue a new OperationID for new work; this OperationID remains terminal", terminalDBOSCause(cause, workflowID))
}

func listedWorkflowDiagnostic(workflows []dbos.WorkflowStatus, contract dbosContractSnapshot, applicationVersion, workflowID, requestedFingerprint string, operation OperationID) (bool, error) {
	if len(workflows) == 0 {
		return false, nil
	}
	if len(workflows) != 1 {
		return false, diagnostic(DBOSDiagClassTerminalRetrieval, DBOSDiagFieldWorkflow,
			DBOSDiagStageWorkflowTerminalLookup, operation, workflowID,
			fmt.Sprintf("DBOS returned %d rows for one exact workflow identity", len(workflows)), "the ambiguous workflow state is not executed or trusted",
			"repair the DBOS workflow index so the exact workflow ID resolves to one row, then retry", errors.New("multiple workflows matched one exact workflow ID"))
	}
	workflow := workflows[0]
	if workflow.ID != workflowID {
		return false, diagnostic(DBOSDiagClassTerminalRetrieval, DBOSDiagFieldWorkflow,
			DBOSDiagStageWorkflowTerminalLookup, operation, workflowID,
			fmt.Sprintf("DBOS returned workflow ID %q for exact requested ID %q", workflow.ID, workflowID), "the mismatched workflow state is not executed or trusted",
			"repair the DBOS workflow index and retry the same OperationID", errors.New("exact workflow lookup returned a mismatched ID"))
	}
	if !knownWorkflowStatus(workflow.Status) {
		return false, diagnostic(DBOSDiagClassTerminalRetrieval, DBOSDiagFieldWorkflow,
			DBOSDiagStageWorkflowTerminalLookup, operation, workflowID,
			fmt.Sprintf("DBOS returned unsupported workflow status %q", workflow.Status), "the malformed workflow state is not executed or trusted",
			"repair the DBOS workflow status from the same durable backup, then retry", errors.New("workflow lookup returned an unknown status"))
	}
	if workflow.Status != dbos.WorkflowStatusError {
		storedInput, decodeErr := decodeListedWorkflowInput(workflow.Input)
		if decodeErr != nil {
			return true, diagnostic(DBOSDiagClassTerminalRetrieval, DBOSDiagFieldWorkflow,
				DBOSDiagStageWorkflowTerminalLookup, operation, workflowID,
				"DBOS returned an unreadable stored workflow input", "the existing workflow is not executed or trusted",
				"restore the DBOS workflow input from the same durable backup", decodeErr)
		}
		storedFingerprint, fingerprintErr := fingerprint(contract, applicationVersion, storedInput)
		if fingerprintErr != nil {
			return true, diagnostic(DBOSDiagClassTerminalRetrieval, DBOSDiagFieldWorkflow,
				DBOSDiagStageWorkflowTerminalLookup, operation, workflowID,
				"DBOS stored workflow input violates the captured contract", "the existing workflow is not executed or trusted",
				"restore the DBOS workflow input and contract from the same durable backup", fingerprintErr)
		}
		if storedFingerprint != requestedFingerprint {
			return true, diagnostic(DBOSDiagClassTerminalRetrieval, DBOSDiagFieldWorkflow,
				DBOSDiagStageWorkflowTerminalLookup, operation, workflowID,
				"the OperationID already belongs to a different canonical workflow input", "no callback or durable write is attempted for the changed input",
				"retry the original canonical input, or issue a new OperationID for new work", journal.ErrOperationConflict)
		}
		return true, nil
	}
	cause := workflow.Error
	if cause == nil {
		cause = errors.New("DBOS persisted terminal ERROR without structured error details")
	}
	return true, diagnostic(DBOSDiagClassTerminalRetrieval, DBOSDiagFieldWorkflow,
		DBOSDiagStageWorkflowTerminalLookup, operation, workflowID,
		"DBOS reports the workflow is already terminal ERROR", "no callback or durable write is attempted by same-ID replay",
		"repair the dependency and issue a new OperationID for new work; this OperationID remains terminal", terminalDBOSCause(cause, workflowID))
}

func knownWorkflowStatus(status dbos.WorkflowStatusType) bool {
	switch status {
	case dbos.WorkflowStatusPending,
		dbos.WorkflowStatusEnqueued,
		dbos.WorkflowStatusDelayed,
		dbos.WorkflowStatusSuccess,
		dbos.WorkflowStatusError,
		dbos.WorkflowStatusCancelled,
		dbos.WorkflowStatusMaxRecoveryAttemptsExceeded:
		return true
	default:
		return false
	}
}

func decodeListedWorkflowInput(input any) (DBOSApplyInput, error) {
	var raw []byte
	if encoded, ok := input.(string); ok {
		raw = []byte(encoded)
	} else if encoded, ok := input.([]byte); ok {
		raw = append([]byte(nil), encoded...)
	} else {
		var err error
		raw, err = json.Marshal(input)
		if err != nil {
			return DBOSApplyInput{}, fmt.Errorf("encode DBOS listed workflow input: %w", err)
		}
	}
	var decoded DBOSApplyInput
	if err := decodeStrictDBOSJSON(raw, &decoded); err != nil {
		return DBOSApplyInput{}, fmt.Errorf("decode DBOS listed workflow input: %w", err)
	}
	return decoded, nil
}

func reconcileDBOSAllocatedCreates(in journal.OperationInput, result journal.CommittedResult) (journal.OperationInput, error) {
	slots := make(map[journal.ResultSlotID]journal.ResultSlotBinding, len(result.ResultSlots))
	for _, binding := range result.ResultSlots {
		slots[binding.Slot] = binding
	}
	in.Effects = append([]journal.Effect(nil), in.Effects...)
	for i := range in.Effects {
		effect := &in.Effects[i]
		if effect.Sort != journal.EffectTaskCreateAllocated {
			continue
		}
		binding, ok := slots[effect.ResultSlot]
		if !ok || binding.TaskID == nil || binding.Kind != journal.JournalKindTaskEvent || binding.TaskID.Namespace != effect.TaskID.Namespace {
			return journal.OperationInput{}, fmt.Errorf(
				"%w: allocated create operation %q slot %q does not resolve to a task in namespace %q -- "+
					"where: DBOS completed-workflow preflight; impact: no workflow starts and no writes occur; "+
					"fix: restore the operation result slot and canonical mutation from the same committed backup",
				journal.ErrResultSlotIntegrity, in.OperationID, effect.ResultSlot, effect.TaskID.Namespace)
		}
		effect.TaskID.UUID = binding.TaskID.UUID
	}
	return in, nil
}

// applyWorkflow decodes and runs one atomic fold step. A domain failure is
// checkpointed as a closed outcome (nil Go error); only DBOS infrastructure
// failures use the Go-error channel. The step name is derived from the adapter-
// captured contract prefix plus the fingerprint.
func (a *DBOSAdapter) applyWorkflow(wfCtx dbos.Context, input DBOSApplyInput) (DBOSStepOutcome, error) {
	return a.runDomainWorkflow(wfCtx, input, "applyWorkflow", decodeApplyInput, a.foldDomainMutation)
}

func (a *DBOSAdapter) runDomainWorkflow(
	wfCtx dbos.Context,
	input DBOSApplyInput,
	workflowName string,
	decode func(dbosContractSnapshot, DBOSApplyInput) (journal.OperationInput, error),
	fold func(journal.OperationInput) (CommittedResult, error),
) (DBOSStepOutcome, error) {
	a.testHooks.onWorkflowEntry()
	in, err := decode(a.contract, input)
	if err != nil {
		return DBOSStepOutcome{}, fmt.Errorf("provenance.%s: %w", workflowName, err)
	}
	fp, err := fingerprint(a.contract, a.applicationVersion, input)
	if err != nil {
		return DBOSStepOutcome{}, fmt.Errorf("provenance.%s: derive step identity: %w", workflowName, err)
	}
	// Step name is derived from the adapter-captured contract prefix.
	stepName := a.contract.stepPrefix + fp
	outcome, err := dbos.RunAsStep(wfCtx, func(stepCtx context.Context) (DBOSStepOutcome, error) {
		a.testHooks.beforeDomainCommit()
		result, applyErr := fold(in)
		outcome, checkpointErr := checkpointDomainApplyResult(a.contract, in, result, applyErr)
		if checkpointErr != nil || applyErr != nil {
			return outcome, checkpointErr
		}
		a.testHooks.afterDomainCommit()
		return outcome, nil
	}, dbosStepOptions(stepName, a.stepOptions)...)
	if err != nil {
		return DBOSStepOutcome{}, err
	}
	a.testHooks.afterStepCheckpoint()
	return outcome, nil
}

// checkpointDomainApplyResult classifies the fold result and routes it to the
// correct step-outcome encoding. It is the single routing point:
//   - nil applyErr -> success outcome (nil Go error)
//   - exactly-one domain match -> domain failure outcome (nil Go error, zero DBOS retries)
//   - zero/multiple matches or infrastructure error -> pass-through Go error (DBOS retries)
func checkpointDomainApplyResult(contract dbosContractSnapshot, in journal.OperationInput, result journal.CommittedResult, applyErr error) (DBOSStepOutcome, error) {
	if applyErr == nil {
		return encodeDBOSApplySuccess(contract, in.OperationID, in.MutationDigest, result)
	}
	if _, classifyErr := classifyDomainFailure(applyErr); classifyErr == nil {
		return encodeDBOSApplyFailure(contract, in.OperationID, in.MutationDigest, applyErr)
	} else {
		var ambiguous *AmbiguousApplyFailureError
		if errors.As(classifyErr, &ambiguous) {
			return DBOSStepOutcome{}, classifyErr
		}
	}
	// Infrastructure, unknown, or multi-match error: leave on DBOS's retryable
	// Go-error channel. No domain checkpoint, no CanonicalApplyFailure.
	return DBOSStepOutcome{}, applyErr
}

// dbosStepOptions is the sole translation from resolved values to the pinned
// opaque DBOS functional options.
func dbosStepOptions(stepName string, options resolvedDBOSStepOptions) []dbos.StepOption {
	return []dbos.StepOption{
		dbos.WithStepName(stepName),
		dbos.WithStepMaxRetries(options.maxRetries),
		dbos.WithStepBaseInterval(options.baseInterval),
		dbos.WithStepBackoffFactor(options.backoffFactor),
	}
}

// foldDomainMutation runs one atomic journal fold through the tracker's journal.
// SQLite's busy_timeout is the sole local contention wait. BUSY/LOCKED errors that
// escape that attempt remain on the Go-error channel so DBOS's configured durable
// step retries handle them instead of checkpointing an infrastructure failure.
func (a *DBOSAdapter) foldDomainMutation(in journal.OperationInput) (CommittedResult, error) {
	return a.tracker.Journal().Apply(in)
}

// postValidate re-reads the committed operation read-only after a successful
// checkpoint and confirms the journal-anchored committed operation matches the
// checkpointed outcome. A failure outcome decodes to its typed journal error and
// writes nothing; a divergence returns CheckpointDivergenceError and writes
// nothing; an unknown lookup variant fails closed.
func (a *DBOSAdapter) postValidate(in journal.OperationInput, outcome DBOSStepOutcome) (CommittedResult, error) {
	// Guard outcome identity BEFORE trusting either arm (defense-in-depth parity): a
	// mis-keyed/forged outcome -- success OR failure -- must not be attributed to this
	// operation. Applying it symmetrically means a decoded FAILURE outcome cannot
	// surface its typed error under the wrong operation either.
	if outcome.OperationID != string(in.OperationID) {
		return CommittedResult{}, &CheckpointDivergenceError{
			Operation: in.OperationID,
			Stage:     DBOSDiagStageCheckpointValidation,
			Impact:    "the checkpointed outcome names a different operation; nothing is trusted",
			Fix:       "this indicates a corrupted or mis-keyed checkpoint; investigate the workflow ID derivation",
			Cause: fmt.Errorf("outcome operation %q != requested %q",
				outcome.OperationID, in.OperationID),
		}
	}
	if !bytes.Equal(outcome.MutationDigest, in.MutationDigest) {
		return CommittedResult{}, &CheckpointDivergenceError{
			Operation: in.OperationID, Stage: DBOSDiagStageCheckpointValidation,
			Impact: "the checkpoint belongs to different canonical effects; nothing is trusted",
			Fix:    "restore the DBOS checkpoint and journal operation from the same committed execution",
			Cause:  fmt.Errorf("outcome canonical digest %x != requested %x", outcome.MutationDigest, in.MutationDigest),
		}
	}
	success, decodeErr := decodeDBOSStepOutcome(a.contract, outcome)
	if outcome.Failure != nil {
		// A structurally valid failure is only authoritative after the journal
		// confirms that no operation committed. Malformed failures still fail
		// immediately at the outcome boundary and never consult journal state.
		var diagnosticErr *DBOSDiagnosticError
		if errors.As(decodeErr, &diagnosticErr) {
			return CommittedResult{}, decodeErr
		}
		looked, lookupErr := a.tracker.Journal().LookupCommitted(in.OperationID)
		if lookupErr != nil {
			return CommittedResult{}, checkpointFailureDivergence(in.OperationID,
				"the journal could not confirm absence for a checkpointed domain failure",
				"repair journal availability and reconcile the checkpoint with the same durable operation before retrying", lookupErr)
		}
		if looked.Kind != journal.CommittedAbsent {
			return CommittedResult{}, checkpointFailureDivergence(in.OperationID,
				fmt.Sprintf("the journal reports %s for a checkpointed domain failure", looked.Kind),
				"do not trust the failure checkpoint; reconcile the journal operation and DBOS workflow from the same durable backup", fmt.Errorf("failure checkpoint requires CommittedAbsent, got %s", looked.Kind))
		}
		return CommittedResult{}, decodeErr
	}
	if decodeErr != nil {
		// A malformed success outcome fails closed before any journal result is
		// trusted. Valid success results continue through the journal authority
		// comparison below.
		return CommittedResult{}, decodeErr
	}

	looked, err := a.tracker.Journal().LookupCommitted(in.OperationID)
	if err != nil {
		return CommittedResult{}, fmt.Errorf(
			"provenance.DBOSAdapter.Apply: post-checkpoint LookupCommitted for operation %q: %w",
			in.OperationID, err)
	}
	switch looked.Kind {
	case journal.CommittedExact:
		got, encErr := encodeDBOSApplySuccess(a.contract, in.OperationID, in.MutationDigest, looked)
		if encErr != nil {
			return CommittedResult{}, &CheckpointDivergenceError{
				Operation: in.OperationID,
				Stage:     DBOSDiagStageCheckpointValidation,
				Impact:    "the journal returned malformed result-slot metadata; the checkpoint is not trusted",
				Fix:       "restore the operation and result-slot rows from the same committed backup",
				Cause:     encErr,
			}
		}
		if !canonicalResultsEqual(*got.Success, success) {
			return CommittedResult{}, &CheckpointDivergenceError{
				Operation: in.OperationID,
				Stage:     DBOSDiagStageCheckpointValidation,
				Impact:    "the checkpointed result differs from the journal-anchored committed operation; nothing is trusted",
				Fix: "the DBOS checkpoint and the Provenance journal disagree; do not repair from the " +
					"checkpoint -- inspect the operation's journal anchor and effects",
				Cause: fmt.Errorf("checkpoint result %+v != journal-anchored result %+v", success, *got.Success),
			}
		}
		return looked, nil
	case journal.CommittedAbsent:
		return CommittedResult{}, &CheckpointDivergenceError{
			Operation: in.OperationID,
			Stage:     DBOSDiagStageCheckpointValidation,
			Impact:    "DBOS reports a completed step but the Provenance journal has no committed operation (orphaned checkpoint); nothing is trusted",
			Fix:       "do not trust the orphaned checkpoint; the domain commit is absent -- investigate the crash/recovery path",
			Cause:     fmt.Errorf("LookupCommitted returned CommittedAbsent for a checkpointed success"),
		}
	case journal.CommittedConflict:
		return CommittedResult{}, &CheckpointDivergenceError{
			Operation: in.OperationID,
			Stage:     DBOSDiagStageCheckpointValidation,
			Impact:    "the committed operation conflicts with the checkpointed identity; nothing is trusted",
			Fix:       "the OperationID resolves to a different committed identity than the checkpoint; investigate the conflicting operation",
			Cause:     fmt.Errorf("LookupCommitted returned CommittedConflict for a checkpointed success"),
		}
	default:
		return CommittedResult{}, &CheckpointDivergenceError{
			Operation: in.OperationID,
			Stage:     DBOSDiagStageCheckpointValidation,
			Impact:    "LookupCommitted returned an unknown result variant; failing closed",
			Fix:       "this indicates an unhandled CommittedResultKind; nothing is trusted",
			Cause:     fmt.Errorf("unknown CommittedResultKind %d", int(looked.Kind)),
		}
	}
}

func checkpointFailureDivergence(operation OperationID, impact, fix string, cause error) error {
	return &CheckpointDivergenceError{
		Operation: operation,
		Stage:     DBOSDiagStageCheckpointValidation,
		Impact:    impact,
		Fix:       fix,
		Cause:     cause,
	}
}

// awaitWorkflowResult retrieves the workflow result on a cancellable child of the
// adapter root, bridging caller cancellation with context.AfterFunc. It calls no
// CancelWorkflow: caller cancellation ends only the local await and returns a typed
// ApplyWaitCanceledError while durable work continues. It leaks no waiter.
func awaitWorkflowResult[T any](
	caller context.Context,
	root dbos.Context,
	workflowID string,
	operation OperationID,
	pollingInterval time.Duration,
) (T, error) {
	waitBase, cancelWait := context.WithCancelCause(root)
	stop := context.AfterFunc(caller, func() {
		cancelWait(context.Cause(caller))
	})
	defer func() { stop(); cancelWait(nil) }()

	waitCtx := dbos.From(root, waitBase)
	handle, err := dbos.RetrieveWorkflow[T](waitCtx, workflowID)
	if err != nil {
		if caller.Err() != nil {
			return *new(T), &ApplyWaitCanceledError{
				Operation: operation,
				Stage:     DBOSDiagStageWorkflowRetrieve,
				Impact:    "durable work continues",
				Fix:       "retry the same operation to retrieve its outcome",
				Cause:     context.Cause(caller),
			}
		}
		return *new(T), diagnostic(DBOSDiagClassTerminalRetrieval, DBOSDiagFieldWorkflow,
			DBOSDiagStageWorkflowTerminalLookup, operation, workflowID,
			"DBOS could not retrieve the durable workflow handle", "no domain callback or write is started by retrieval",
			"repair DBOS storage availability, then retry the same OperationID", err)
	}
	result, err := handle.GetResult(dbos.WithHandlePollingInterval(pollingInterval))
	if err != nil && caller.Err() != nil {
		return *new(T), &ApplyWaitCanceledError{
			Operation: operation,
			Stage:     DBOSDiagStageWorkflowAwait,
			Impact:    "durable work continues",
			Fix:       "retry the same operation to retrieve its outcome",
			Cause:     context.Cause(caller),
		}
	}
	if err != nil {
		cause := terminalDBOSCause(err, workflowID)
		return *new(T), diagnostic(DBOSDiagClassTerminalRetrieval, DBOSDiagFieldWorkflow,
			DBOSDiagStageWorkflowTerminalLookup, operation, workflowID,
			"DBOS returned a terminal workflow error after step retry exhaustion", "the workflow remains terminal ERROR and no recovery workflow is created",
			"repair the dependency and issue a new OperationID for new work; replaying this OperationID only retrieves the terminal error", cause)
	}
	return result, nil
}

// terminalMaxStepRetriesMarker is the prefix the DBOS runtime writes when it
// formats a step-retry-exhaustion error. The runtime prints the code by NAME,
// not by number, so the marker is derived from the code constant instead of
// being spelled out: a rename in the library then moves this marker with it
// rather than leaving a branch that can never match.
// Source: dbos/internal/models/errors.go, (*Error).Error.
var terminalMaxStepRetriesMarker = "DBOS Error " + dbos.ErrorCodeMaxStepRetriesExceeded.String() + ":"

// DBOS persists workflow errors as text. Reconstruct its closed retry code
// on terminal retrieval so first delivery and later same-ID replay expose the same
// errors.As-visible runtime error contract instead of degrading to string matching.
func terminalDBOSCause(err error, workflowID string) error {
	var typed *dbos.Error
	if errors.As(err, &typed) {
		return err
	}
	if strings.Contains(err.Error(), terminalMaxStepRetriesMarker) || strings.Contains(err.Error(), "exceeded its maximum") {
		return &dbos.Error{Message: err.Error(), Code: dbos.ErrorCodeMaxStepRetriesExceeded, WorkflowID: workflowID}
	}
	return err
}

// canonicalResultsEqual reports whether two canonical results name the same
// journal-anchored committed operation: identical anchor, ordered emitted-event
// closure, and slot-sorted bindings. CanonicalMutationResult holds only
// journal-anchored fields (the per-call §9.4 ShortCircuited flag now lives on
// DBOSStepOutcome, not inside the canonical result), so this comparison -- like the
// struct's own == -- never spuriously diverges a legitimate replay against its own
// committed record.
func canonicalResultsEqual(a, b CanonicalMutationResult) bool {
	if validateCanonicalMutationResult(a.AnchorJournalID, a.EmittedEvents, a.ResultSlots) != nil || validateCanonicalMutationResult(b.AnchorJournalID, b.EmittedEvents, b.ResultSlots) != nil {
		return false
	}
	if a.AnchorJournalID != b.AnchorJournalID {
		return false
	}
	if len(a.EmittedEvents) != len(b.EmittedEvents) || len(a.ResultSlots) != len(b.ResultSlots) {
		return false
	}
	for i := range a.EmittedEvents {
		if a.EmittedEvents[i] != b.EmittedEvents[i] {
			return false
		}
	}
	for i := range a.ResultSlots {
		if a.ResultSlots[i] != b.ResultSlots[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Typed adapter errors
// ---------------------------------------------------------------------------

// ApplyWaitCanceledError is returned when the CALLER context is cancelled while the
// adapter awaits a durable workflow result. The durable work is never cancelled;
// re-issuing the same operation retrieves its eventual outcome.
type ApplyWaitCanceledError struct {
	Operation OperationID
	Stage     DBOSDiagnosticStage
	Impact    string
	Fix       string
	Cause     error
}

func (e *ApplyWaitCanceledError) Error() string {
	return fmt.Sprintf(
		"provenance: apply wait canceled for operation %q -- stage: %s; impact: %s; fix: %s%s",
		e.Operation, e.Stage, e.Impact, e.Fix, causeClause(e.Cause))
}

func (e *ApplyWaitCanceledError) Unwrap() error { return e.Cause }

// CheckpointDivergenceError is returned when a checkpointed success does not match
// the journal-anchored committed operation (missing, conflicting, or result
// mismatch). The adapter writes nothing and never repairs state from a checkpoint.
type CheckpointDivergenceError struct {
	Operation OperationID
	Stage     DBOSDiagnosticStage
	Impact    string
	Fix       string
	Cause     error
}

func (e *CheckpointDivergenceError) Error() string {
	return fmt.Sprintf(
		"provenance: checkpoint divergence for operation %q -- stage: %s; impact: %s; fix: %s%s",
		e.Operation, e.Stage, e.Impact, e.Fix, causeClause(e.Cause))
}

func (e *CheckpointDivergenceError) Unwrap() error { return e.Cause }
