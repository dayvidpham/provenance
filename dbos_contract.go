package provenance

// dbos_contract.go is the SOLE production identity authority for the unversioned
// DBOS durable contract used by the Provenance adapter. All registration paths,
// input/context codecs, fingerprint derivation, workflow/step identity, and outcome
// codec consume the dbosContractSnapshot value captured once by NewDBOSAdapter.
//
// Proposal 54 (ratified 2026-07-22): DBOS input/context/workflow/outcome/step
// tokens are UNVERSIONED because no persisted DBOS records or consumers existed at
// the migration boundary. The independent journal codec provenance.mutation.v1 and
// MutationEncodingV1 bytes are preserved exactly and are out of scope here.
//
// There is no mutable package descriptor, duplicate production literal authority,
// V1/V2 active symbol family, compatibility decoder, dual registration, or migration
// path. Resume, fork, recovery-attempt workflow, private SQL, or second
// infrastructure ledger are deliberately absent.

import (
	"fmt"
	"math"
	"time"
)

// ---------------------------------------------------------------------------
// DBOS step retry options (Proposal 54 §MinimalPublicAPI)
// ---------------------------------------------------------------------------

// DBOSStepOptions configures the retry behaviour of the DBOS step that wraps each
// atomic domain fold. Zero fields are replaced by package defaults at adapter
// construction; valid nonzero overrides are copied and validated before registration.
//
// These options map internally and exclusively to the pinned v0.16 options:
//
//   - MaxRetries   → dbos.WithStepMaxRetries
//   - BaseInterval → dbos.WithBaseInterval
//   - BackoffFactor → dbos.WithBackoffFactor
//
// StepName is contract-owned and cannot be overridden by callers.
type DBOSStepOptions struct {
	// MaxRetries is the number of retries after the initial attempt.
	// 0 means default (3). Valid nonzero range: 1–12.
	MaxRetries int
	// BaseInterval is the base exponential back-off interval.
	// 0 means default (50 ms). Valid nonzero range: >0 and ≤5 s.
	BaseInterval time.Duration
	// BackoffFactor is the exponential multiplier applied between retries.
	// 0 means default (2). Valid nonzero constraint: finite and ≥1.
	BackoffFactor float64
}

// Package-level defaults per Proposal 54 UAT verbatim:
// "should be 3 retries by default, base interval would be 50ms."
const (
	dbosDefaultMaxRetries            = 3
	dbosDefaultBaseInterval          = 50 * time.Millisecond
	dbosDefaultResultPollingInterval = 50 * time.Millisecond
)

const dbosDefaultBackoffFactor = float64(2)

// resolvedDBOSStepOptions is the fully-resolved, validated copy of DBOSStepOptions
// that the adapter stores and uses to construct v0.16 step options.
type resolvedDBOSStepOptions struct {
	maxRetries    int
	baseInterval  time.Duration
	backoffFactor float64
}

// resolveDBOSStepOptions substitutes defaults for zero fields and validates nonzero
// overrides before adapter registration. Each field is validated independently.
// Errors are actionable per [C-actionable-errors]: field, value, rule, stage, impact, fix.
func resolveDBOSStepOptions(opts DBOSStepOptions) (resolvedDBOSStepOptions, error) {
	maxRetries := opts.MaxRetries
	if maxRetries == 0 {
		maxRetries = dbosDefaultMaxRetries
	} else if maxRetries < 1 || maxRetries > 12 {
		return resolvedDBOSStepOptions{}, &DBOSDiagnosticError{
			Class: DBOSDiagClassConfig, Field: DBOSDiagFieldMaxRetries,
			Value:  fmt.Sprintf("%d", maxRetries),
			Reason: "must be 0 (use default 3) or 1–12",
			Stage:  DBOSDiagStageAdapterConstruction,
			Impact: "nothing is registered or written",
			Fix:    "set MaxRetries to a value between 1 and 12, or leave it 0 to accept the default of 3",
		}
	}

	baseInterval := opts.BaseInterval
	if baseInterval == 0 {
		baseInterval = dbosDefaultBaseInterval
	} else if baseInterval < 0 || baseInterval > 5*time.Second {
		return resolvedDBOSStepOptions{}, &DBOSDiagnosticError{
			Class: DBOSDiagClassConfig, Field: DBOSDiagFieldBaseInterval,
			Value:  baseInterval.String(),
			Reason: "must be 0 (use default 50 ms) or >0 and ≤5 s",
			Stage:  DBOSDiagStageAdapterConstruction,
			Impact: "nothing is registered or written",
			Fix:    "set BaseInterval to a positive duration ≤5 s, or leave it 0 to accept the default of 50 ms",
		}
	}

	backoffFactor := opts.BackoffFactor
	if backoffFactor == 0 {
		backoffFactor = dbosDefaultBackoffFactor
	} else if backoffFactor < 1 || math.IsNaN(backoffFactor) || math.IsInf(backoffFactor, 0) {
		return resolvedDBOSStepOptions{}, &DBOSDiagnosticError{
			Class: DBOSDiagClassConfig, Field: DBOSDiagFieldBackoffFactor,
			Value:  fmt.Sprintf("%g", backoffFactor),
			Reason: "must be 0 (use default 2) or a finite value ≥1",
			Stage:  DBOSDiagStageAdapterConstruction,
			Impact: "nothing is registered or written",
			Fix:    "set BackoffFactor to a finite value ≥1, or leave it 0 to accept the default of 2",
		}
	}

	return resolvedDBOSStepOptions{
		maxRetries:    maxRetries,
		baseInterval:  baseInterval,
		backoffFactor: backoffFactor,
	}, nil
}

func resolveDBOSResultPollingInterval(interval time.Duration) (time.Duration, error) {
	if interval == 0 {
		return dbosDefaultResultPollingInterval, nil
	}
	if interval < 10*time.Millisecond || interval > 5*time.Second {
		return 0, &DBOSDiagnosticError{
			Class: DBOSDiagClassConfig, Field: DBOSDiagFieldResultPollingInterval,
			Value:  interval.String(),
			Reason: "must be 0 (use default 50 ms) or between 10 ms and 5 s inclusive",
			Stage:  DBOSDiagStageAdapterConstruction,
			Impact: "nothing is registered or written",
			Fix:    "set ResultPollingInterval between 10 ms and 5 s, or leave it 0 to accept the default of 50 ms",
		}
	}
	return interval, nil
}

// ---------------------------------------------------------------------------
// Captured const-backed unversioned DBOS contract (Proposal 54 §CapturedContract)
// ---------------------------------------------------------------------------

// The adjacent unexported const block is the SOLE production literal authority.
// A pure constructor returns a snapshot captured ONCE before adapter registration;
// all adapter paths (registration, input/context codecs, fingerprint, workflow/step
// identity, and outcome codec) consume that same adapter-captured value.
// There is no mutable package variable, duplicate literal authority, or rebuild-per-call path.
const (
	// dbosApplyInputSchemaConst is the schema tag for the DBOS apply-input envelope.
	dbosApplyInputSchemaConst = "provenance.dbos-apply-input"
	// dbosContextSchemaConst is the schema tag for the DBOS operation context frame.
	dbosContextSchemaConst = "provenance.dbos-context"
	// dbosOutcomeSchemaConst is the schema tag for the DBOSStepOutcome JSON blob.
	dbosOutcomeSchemaConst = "provenance.dbos-step-outcome"
	// dbosWorkflowSchemaConst is the unversioned workflow-schema fingerprint tag.
	dbosWorkflowSchemaConst = "provenance.apply"
	// dbosWorkflowPrefixConst is the prefix for durable workflow IDs.
	dbosWorkflowPrefixConst = "provenance.apply/"
	// dbosStepPrefixConst is the prefix for durable step names within one workflow.
	dbosStepPrefixConst = "provenance.apply-step/"
	// dbosPinnedLibraryConst is the exact library/version string included in fingerprint derivation.
	dbosPinnedLibraryConst = "github.com/dbos-inc/dbos-transact-golang v0.16.0"
)

// dbosContractSnapshot is the captured unversioned DBOS contract value.
// All production paths share one adapter-captured instance; nothing is rebuilt per call.
type dbosContractSnapshot struct {
	applyInputSchema string
	contextSchema    string
	outcomeSchema    string
	workflowSchema   string
	workflowPrefix   string
	stepPrefix       string
	pinnedLibrary    string
}

// newDBOSContractSnapshot returns a fresh snapshot of the adjacent const block.
// This function is called exactly once per adapter construction to capture the
// sole identity authority before registration.
func newDBOSContractSnapshot() dbosContractSnapshot {
	return dbosContractSnapshot{
		applyInputSchema: dbosApplyInputSchemaConst,
		contextSchema:    dbosContextSchemaConst,
		outcomeSchema:    dbosOutcomeSchemaConst,
		workflowSchema:   dbosWorkflowSchemaConst,
		workflowPrefix:   dbosWorkflowPrefixConst,
		stepPrefix:       dbosStepPrefixConst,
		pinnedLibrary:    dbosPinnedLibraryConst,
	}
}

// ---------------------------------------------------------------------------
// Typed diagnostic constants (Proposal 54 §TypedDiagnostics)
// ---------------------------------------------------------------------------

// DBOSDiagnosticClass identifies which subsystem produced a diagnostic.
type DBOSDiagnosticClass string

const (
	// DBOSDiagClassConfig identifies adapter construction/config validation failures.
	DBOSDiagClassConfig DBOSDiagnosticClass = "config"
	// DBOSDiagClassContextFrame identifies DBOS context codec failures.
	DBOSDiagClassContextFrame DBOSDiagnosticClass = "context-frame"
	// DBOSDiagClassOutcomeDecode identifies DBOS step-outcome decode failures.
	DBOSDiagClassOutcomeDecode DBOSDiagnosticClass = "outcome-decode"
	// DBOSDiagClassClassify identifies domain-failure classification failures.
	DBOSDiagClassClassify DBOSDiagnosticClass = "classify"
	// DBOSDiagClassStepRetry identifies DBOS step retry exhaustion.
	DBOSDiagClassStepRetry DBOSDiagnosticClass = "step-retry"
	// DBOSDiagClassTerminalRetrieval identifies terminal workflow retrieval failures.
	DBOSDiagClassTerminalRetrieval DBOSDiagnosticClass = "terminal-retrieval"
	// DBOSDiagClassCanonicalMutation identifies the nested journal mutation codec
	// when a DBOS input reaches that independently versioned boundary.
	DBOSDiagClassCanonicalMutation DBOSDiagnosticClass = "canonical-mutation"
)

// DBOSDiagnosticField identifies the specific field that caused a rejection.
type DBOSDiagnosticField string

const (
	DBOSDiagFieldMaxRetries            DBOSDiagnosticField = "MaxRetries"
	DBOSDiagFieldBaseInterval          DBOSDiagnosticField = "BaseInterval"
	DBOSDiagFieldBackoffFactor         DBOSDiagnosticField = "BackoffFactor"
	DBOSDiagFieldResultPollingInterval DBOSDiagnosticField = "ResultPollingInterval"
	DBOSDiagFieldOperation             DBOSDiagnosticField = "operation"
	DBOSDiagFieldContextVersion        DBOSDiagnosticField = "context_version"
	DBOSDiagFieldActor                 DBOSDiagnosticField = "actor"
	DBOSDiagFieldCommand               DBOSDiagnosticField = "command"
	DBOSDiagFieldAuthority             DBOSDiagnosticField = "authority"
	DBOSDiagFieldSchema                DBOSDiagnosticField = "schema"
	DBOSDiagFieldTrailing              DBOSDiagnosticField = "trailing"
	DBOSDiagFieldKind                  DBOSDiagnosticField = "kind"
	DBOSDiagFieldNestedOpID            DBOSDiagnosticField = "nested_operation_id"
	DBOSDiagFieldSuccessFailure        DBOSDiagnosticField = "success_failure"
	DBOSDiagFieldMessage               DBOSDiagnosticField = "message"
	DBOSDiagFieldConflictField         DBOSDiagnosticField = "conflict_field"
	DBOSDiagFieldDescriptorMatch       DBOSDiagnosticField = "descriptor_match"
	DBOSDiagFieldWorkflow              DBOSDiagnosticField = "workflow"
	DBOSDiagFieldContext               DBOSDiagnosticField = "context"
	DBOSDiagFieldRecordedAt            DBOSDiagnosticField = "recorded_at"
)

// DBOSDiagnosticStage identifies the step/operation during which a failure occurred.
type DBOSDiagnosticStage string

const (
	DBOSDiagStageAdapterConstruction    DBOSDiagnosticStage = "adapter-construction"
	DBOSDiagStageContextEncode          DBOSDiagnosticStage = "context-encode"
	DBOSDiagStageContextDecode          DBOSDiagnosticStage = "context-decode"
	DBOSDiagStageOutcomeEncode          DBOSDiagnosticStage = "outcome-encode"
	DBOSDiagStageOutcomeDecode          DBOSDiagnosticStage = "outcome-decode"
	DBOSDiagStageDomainFoldClassify     DBOSDiagnosticStage = "domain-fold-classify"
	DBOSDiagStageStepCheckpoint         DBOSDiagnosticStage = "step-checkpoint"
	DBOSDiagStageWorkflowTerminalLookup DBOSDiagnosticStage = "workflow-terminal-lookup"
	DBOSDiagStageApplyPreStart          DBOSDiagnosticStage = "apply-pre-start"
	DBOSDiagStageWorkflowRetrieve       DBOSDiagnosticStage = "workflow-retrieve"
	DBOSDiagStageWorkflowAwait          DBOSDiagnosticStage = "workflow-await"
	DBOSDiagStageCheckpointValidation   DBOSDiagnosticStage = "checkpoint-validation"
)

// ---------------------------------------------------------------------------
// DBOSConfigError: actionable pre-registration error [C-actionable-errors]
// ---------------------------------------------------------------------------

// DBOSDiagnosticError is the shared closed diagnostic returned by all DBOS adapter
// boundaries. It satisfies the [C-actionable-errors] constraint:
// (1) what went wrong, (2) why it happened, (3) where (field), (4) when (stage),
// (5) impact for the caller, (6) how to fix it.
type DBOSDiagnosticError struct {
	Class     DBOSDiagnosticClass
	Field     DBOSDiagnosticField
	Stage     DBOSDiagnosticStage
	Operation OperationID
	Workflow  string
	Value     string
	Position  *int
	Reason    string
	Impact    string
	Fix       string
	Cause     error
}

func (e *DBOSDiagnosticError) Error() string {
	return fmt.Sprintf(
		"provenance DBOS diagnostic: what: class=%s field=%s position=%v operation=%q workflow=%q value=%q; why: %s; where: DBOS adapter; when: %s; impact: %s; fix: %s; cause: %v",
		e.Class, e.Field, e.Position, e.Operation, e.Workflow, e.Value, e.Reason, e.Stage, e.Impact, e.Fix, e.Cause)
}

func (e *DBOSDiagnosticError) Unwrap() error { return e.Cause }

// DBOSConfigError remains a source-compatible name for construction diagnostics;
// it is the same consumed runtime type, not a second error authority.
type DBOSConfigError = DBOSDiagnosticError
