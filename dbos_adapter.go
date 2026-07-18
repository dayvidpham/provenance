package provenance

// dbos_adapter.go is the durable-execution adapter that runs one Provenance
// operation as a deterministic DBOS workflow whose single step is the atomic
// journal fold (issue dayvidpham/provenance#6). It introduces no parallel commit
// ledger: the workflow/step guards and restart semantics are bound to the SAME
// OperationID alternate key and journal contract the reducer already enforces, and
// post-checkpoint validation compares the journal-anchored committed operation.
//
// The adapter targets github.com/dbos-inc/dbos-transact-golang v0.16.0 (pinned),
// authorized by the user at Impl-UAT C7a.

import (
	"context"
	"fmt"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
)

// DBOSAdapterConfig configures a DBOSAdapter.
type DBOSAdapterConfig struct {
	// ExpectedApplicationVersion, when non-empty, is asserted equal to the DBOS
	// root's actual application version at construction; a mismatch rejects before
	// the workflow is registered or anything is written.
	ExpectedApplicationVersion string
}

// applyTestHooks are unexported, no-op-by-default seams the crash-gap subprocess
// tests replace to os.Exit at the two durable boundaries: afterDomainCommit fires
// after the reducer commits the domain mutation but before the step outcome is
// checkpointed; afterStepCheckpoint fires after the step outcome is durably
// recorded but before the workflow completes. Production leaves both as no-ops.
type applyTestHooks struct {
	afterDomainCommit   func()
	afterStepCheckpoint func()
}

func defaultApplyTestHooks() applyTestHooks {
	return applyTestHooks{
		afterDomainCommit:   func() {},
		afterStepCheckpoint: func() {},
	}
}

// DBOSAdapter runs Provenance operations as durable DBOS workflows over a shared
// journal contract. It owns exactly one registered provenance.apply/v1 workflow on
// the adapter root.
type DBOSAdapter struct {
	root               dbos.DBOSContext
	tracker            Tracker
	applicationVersion string
	testHooks          applyTestHooks
}

// NewDBOSAdapter validates the root and tracker, derives the actual application
// version from the DBOS root, rejects a non-empty mismatched expectation, and
// registers the single stable provenance.apply/v1 workflow on the not-yet-launched
// root (so it is recovery-visible after Launch).
func NewDBOSAdapter(root dbos.DBOSContext, tracker Tracker, config DBOSAdapterConfig) (*DBOSAdapter, error) {
	if root == nil {
		return nil, fmt.Errorf(
			"provenance.NewDBOSAdapter: the DBOS root context is nil — where: adapter construction; " +
				"impact: no durable workflow can be registered or run; fix: pass the DBOS root created with " +
				"NewDBOSContext(SqliteSystemDB=...) before calling Launch")
	}
	if tracker == nil {
		return nil, fmt.Errorf(
			"provenance.NewDBOSAdapter: the Tracker is nil — where: adapter construction; impact: no domain " +
				"fold target; fix: pass the borrowed tracker from OpenBorrowedSQLite over the same *sql.DB the " +
				"DBOS root uses")
	}
	actual := root.GetApplicationVersion()
	if config.ExpectedApplicationVersion != "" && config.ExpectedApplicationVersion != actual {
		return nil, fmt.Errorf(
			"provenance.NewDBOSAdapter: expected application version %q but the DBOS root reports %q — "+
				"where: adapter construction, before workflow registration; impact: nothing is registered or "+
				"written; fix: pass the matching version or leave ExpectedApplicationVersion empty to accept the "+
				"root's version",
			config.ExpectedApplicationVersion, actual)
	}

	a := &DBOSAdapter{
		root:               root,
		tracker:            tracker,
		applicationVersion: actual,
		testHooks:          defaultApplyTestHooks(),
	}
	// One stable workflow, registered before Launch so recovery can re-run it.
	dbos.RegisterWorkflow(root, a.applyWorkflow)
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
			Stage:     "pre-start",
			Impact:    "no workflow was started; nothing was committed",
			Fix:       "retry with an un-cancelled context to start the operation",
			Cause:     context.Cause(ctx),
		}
	}

	input, err := encodeApplyInput(in)
	if err != nil {
		return CommittedResult{}, fmt.Errorf("provenance.DBOSAdapter.Apply: canonicalize operation %q: %w", in.OperationID, err)
	}
	fp := fingerprint(a.applicationVersion, in)
	workflowID := applyWorkflowIDPrefix + fp

	// Start (or attach to) the durable workflow on the UN-CANCELLED adapter root, so
	// caller cancellation never cancels durable work.
	if _, err := dbos.RunWorkflow(
		a.root,
		a.applyWorkflow,
		input,
		dbos.WithWorkflowID(workflowID),
		dbos.WithApplicationVersion(a.applicationVersion),
	); err != nil {
		return CommittedResult{}, fmt.Errorf(
			"provenance.DBOSAdapter.Apply: start durable workflow %q for operation %q: %w — "+
				"where: RunWorkflow; impact: the operation may not have started; fix: ensure the adapter root "+
				"is launched and retry the same operation to attach to any in-flight run",
			workflowID, in.OperationID, err)
	}

	outcome, err := awaitWorkflowResult[DBOSStepOutcomeV1](ctx, a.root, workflowID, in.OperationID)
	if err != nil {
		return CommittedResult{}, err
	}
	return a.postValidate(in, outcome)
}

// applyWorkflow is the single registered durable workflow: decode/validate/
// fingerprint the input, then run ONE step wrapping the atomic journal fold. A
// domain failure is checkpointed as a closed outcome (nil Go error); only DBOS
// infrastructure failures use the Go-error channel.
func (a *DBOSAdapter) applyWorkflow(wfCtx dbos.DBOSContext, input DBOSApplyInputV1) (DBOSStepOutcomeV1, error) {
	in, err := decodeApplyInput(input)
	if err != nil {
		return DBOSStepOutcomeV1{}, fmt.Errorf("provenance.applyWorkflow: %w", err)
	}
	fp := fingerprint(a.applicationVersion, in)

	outcome, err := dbos.RunAsStep(
		wfCtx,
		func(stepCtx context.Context) (DBOSStepOutcomeV1, error) {
			result, applyErr := a.foldDomainMutation(in)
			if applyErr != nil {
				// INFRASTRUCTURE failures (a shut-down store, or a transient WAL lock
				// that survived the bounded retry — the borrowed bridge shares one WAL
				// with the DBOS system connection) are propagated on the Go-error
				// channel, NEVER checkpointed: checkpointing a transient lock as a
				// domain failure would make a recoverable condition falsely permanent.
				// Everything else is a §5/§9 DOMAIN failure, checkpointed as a closed
				// outcome with a nil Go error.
				if isInfrastructureError(applyErr) {
					return DBOSStepOutcomeV1{}, applyErr
				}
				return encodeDBOSApplyFailure(in.OperationID, in.MutationDigest, applyErr)
			}
			a.testHooks.afterDomainCommit()
			return encodeDBOSApplySuccess(in.OperationID, in.MutationDigest, result)
		},
		dbos.WithStepName(applyStepNamePrefix+fp),
	)
	if err != nil {
		return DBOSStepOutcomeV1{}, err
	}
	a.testHooks.afterStepCheckpoint()
	return outcome, nil
}

// foldDomainMutation runs the atomic journal fold through the tracker's journal. The
// shared-WAL transient-lock bounded retry lives in the borrowed-store layer
// (retryOnTransientLock, applied by borrowedJournal.Apply and the gated Session), so
// EVERY public write surface on the borrowed tracker absorbs contention with the DBOS
// system connection — not just this adapter step. A transient lock that survives the
// bounded retry returns unchanged (it still unwraps to the SQLite result code), so the
// workflow's isInfrastructureError classifier propagates it on the Go-error channel
// rather than checkpointing a recoverable condition as a permanent domain failure.
func (a *DBOSAdapter) foldDomainMutation(in journal.OperationInput) (CommittedResult, error) {
	return a.tracker.Journal().Apply(in)
}

// isTransientLock reports whether err is a retryable SQLite BUSY/LOCKED condition
// from the shared-WAL bridge (masking extended result codes to their primary).
func isTransientLock(err error) bool {
	primary := zs.ErrCode(err) & 0xFF
	return primary == zs.ResultBusy || primary == zs.ResultLocked
}

// isInfrastructureError reports whether err is an infrastructure condition (a
// shut-down borrowed store, or an unresolved transient lock) that must NOT be
// checkpointed as a durable domain failure.
func isInfrastructureError(err error) bool {
	if _, ok := AsStoreUnavailable(err); ok {
		return true
	}
	return isTransientLock(err)
}

// postValidate re-reads the committed operation read-only after a successful
// checkpoint and confirms the journal-anchored committed operation matches the
// checkpointed outcome. A failure outcome decodes to its typed journal error and
// writes nothing; a divergence returns CheckpointDivergenceError and writes
// nothing; an unknown lookup variant fails closed.
func (a *DBOSAdapter) postValidate(in journal.OperationInput, outcome DBOSStepOutcomeV1) (CommittedResult, error) {
	// Guard outcome identity BEFORE trusting either arm (defense-in-depth parity): a
	// mis-keyed/forged outcome — success OR failure — must not be attributed to this
	// operation. Applying it symmetrically means a decoded FAILURE outcome cannot
	// surface its typed error under the wrong operation either.
	if outcome.OperationID != string(in.OperationID) {
		return CommittedResult{}, &CheckpointDivergenceError{
			Operation: in.OperationID,
			Stage:     "post-validation outcome identity",
			Impact:    "the checkpointed outcome names a different operation; nothing is trusted",
			Fix:       "this indicates a corrupted or mis-keyed checkpoint; investigate the workflow ID derivation",
			Cause: fmt.Errorf("outcome operation %q != requested %q",
				outcome.OperationID, in.OperationID),
		}
	}
	success, decodeErr := outcome.Decode()
	if decodeErr != nil {
		// A failure outcome surfaces its typed journal error here (matrix
		// present-failure-outcome row); a malformed outcome fails closed.
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
		got, encErr := encodeDBOSApplySuccess(in.OperationID, in.MutationDigest, looked)
		if encErr != nil {
			return CommittedResult{}, fmt.Errorf(
				"provenance.DBOSAdapter.Apply: re-encode looked-up result for operation %q: %w", in.OperationID, encErr)
		}
		if !canonicalResultsEqual(*got.Success, success) {
			return CommittedResult{}, &CheckpointDivergenceError{
				Operation: in.OperationID,
				Stage:     "post-validation canonical-result comparison",
				Impact:    "the checkpointed result differs from the journal-anchored committed operation; nothing is trusted",
				Fix: "the DBOS checkpoint and the Provenance journal disagree; do not repair from the " +
					"checkpoint — inspect the operation's journal anchor and effects",
				Cause: fmt.Errorf("checkpoint result %+v != journal-anchored result %+v", success, *got.Success),
			}
		}
		return looked, nil
	case journal.CommittedAbsent:
		return CommittedResult{}, &CheckpointDivergenceError{
			Operation: in.OperationID,
			Stage:     "post-validation lookup",
			Impact:    "DBOS reports a completed step but the Provenance journal has no committed operation (orphaned checkpoint); nothing is trusted",
			Fix:       "do not trust the orphaned checkpoint; the domain commit is absent — investigate the crash/recovery path",
			Cause:     fmt.Errorf("LookupCommitted returned CommittedAbsent for a checkpointed success"),
		}
	case journal.CommittedConflict:
		return CommittedResult{}, &CheckpointDivergenceError{
			Operation: in.OperationID,
			Stage:     "post-validation lookup",
			Impact:    "the committed operation conflicts with the checkpointed identity; nothing is trusted",
			Fix:       "the OperationID resolves to a different committed identity than the checkpoint; investigate the conflicting operation",
			Cause:     fmt.Errorf("LookupCommitted returned CommittedConflict for a checkpointed success"),
		}
	default:
		return CommittedResult{}, &CheckpointDivergenceError{
			Operation: in.OperationID,
			Stage:     "post-validation lookup",
			Impact:    "LookupCommitted returned an unknown result variant; failing closed",
			Fix:       "this indicates an unhandled CommittedResultKind; nothing is trusted",
			Cause:     fmt.Errorf("unknown CommittedResultKind %d", int(looked.Kind)),
		}
	}
}

// awaitWorkflowResult retrieves the workflow result on a cancellable child of the
// adapter root, bridging caller cancellation with context.AfterFunc. It calls no
// CancelWorkflow: caller cancellation ends only the local await and returns a typed
// ApplyWaitCanceledError while durable work continues. It leaks no waiter.
func awaitWorkflowResult[T any](
	caller context.Context,
	root dbos.DBOSContext,
	workflowID string,
	operation OperationID,
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
				Stage:     "retrieve-workflow",
				Impact:    "durable work continues",
				Fix:       "retry the same operation to retrieve its outcome",
				Cause:     context.Cause(caller),
			}
		}
		return *new(T), err
	}
	result, err := handle.GetResult()
	if err != nil && caller.Err() != nil {
		return *new(T), &ApplyWaitCanceledError{
			Operation: operation,
			Stage:     "await-workflow",
			Impact:    "durable work continues",
			Fix:       "retry the same operation to retrieve its outcome",
			Cause:     context.Cause(caller),
		}
	}
	return result, err
}

// canonicalResultsEqual reports whether two canonical results name the same
// journal-anchored committed operation: identical anchor, ordered emitted-event
// closure, and slot-sorted bindings. CanonicalMutationResultV1 holds only
// journal-anchored fields (the per-call §9.4 ShortCircuited flag now lives on
// DBOSStepOutcomeV1, not inside the canonical result), so this comparison — like the
// struct's own == — never spuriously diverges a legitimate replay against its own
// committed record.
func canonicalResultsEqual(a, b CanonicalMutationResultV1) bool {
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
	Stage     string
	Impact    string
	Fix       string
	Cause     error
}

func (e *ApplyWaitCanceledError) Error() string {
	return fmt.Sprintf(
		"provenance: apply wait canceled for operation %q — stage: %s; impact: %s; fix: %s; cause: %v",
		e.Operation, e.Stage, e.Impact, e.Fix, e.Cause)
}

func (e *ApplyWaitCanceledError) Unwrap() error { return e.Cause }

// CheckpointDivergenceError is returned when a checkpointed success does not match
// the journal-anchored committed operation (missing, conflicting, or result
// mismatch). The adapter writes nothing and never repairs state from a checkpoint.
type CheckpointDivergenceError struct {
	Operation OperationID
	Stage     string
	Impact    string
	Fix       string
	Cause     error
}

func (e *CheckpointDivergenceError) Error() string {
	return fmt.Sprintf(
		"provenance: checkpoint divergence for operation %q — stage: %s; impact: %s; fix: %s; cause: %v",
		e.Operation, e.Stage, e.Impact, e.Fix, e.Cause)
}

func (e *CheckpointDivergenceError) Unwrap() error { return e.Cause }
