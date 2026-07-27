package provenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dayvidpham/provenance/internal/allocation"
	"github.com/dayvidpham/provenance/internal/fusedtx"
	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
)

// FusedGovernedAllocatorConfig is the closed configuration for the one public
// governed DBOS capability. It deliberately accepts a DSN, not a DBOS context
// or *sql.DB: the capability opens one owned system handle, gives that exact
// handle to DBOS, and constructs the fused data source before exposing work.
type FusedGovernedAllocatorConfig struct {
	SQLiteDSN          string
	AppName            string
	ApplicationVersion string
	Logger             *slog.Logger
}

// FusedGovernedAllocator owns the DBOS root, exact SQLite system handle, and
// matching Provenance tracker as one capability. Close shuts down the owned DBOS
// root; it never closes caller-owned resources because callers supply neither a
// root nor a database handle.
type FusedGovernedAllocator struct {
	system  *fusedtx.System
	tracker Tracker

	closeOnce sync.Once
	closeErr  error
}

// OpenFusedGovernedAllocator creates the only public fused governed-allocation
// construction path. The exact DBOS system handle remains opaque, preventing a
// caller from pairing an existing root with a distinct same-file SQLite handle.
func OpenFusedGovernedAllocator(ctx context.Context, config FusedGovernedAllocatorConfig) (*FusedGovernedAllocator, error) {
	system, err := fusedtx.OpenSystem(ctx, fusedtx.SystemConfig{
		SQLiteDSN:          config.SQLiteDSN,
		AppName:            config.AppName,
		ApplicationVersion: config.ApplicationVersion,
		Logger:             config.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("provenance.OpenFusedGovernedAllocator: %w", err)
	}
	tracker, err := OpenBorrowedSQLite(system.DB())
	if err != nil {
		system.Close(30 * time.Second)
		return nil, fmt.Errorf("provenance.OpenFusedGovernedAllocator: activate Provenance schema on the owned DBOS system handle: %w", err)
	}
	allocator := &FusedGovernedAllocator{system: system, tracker: tracker}
	// Registration occurs before Launch and uses the allocator instance as the
	// DBOS configured instance. No raw root/handle pair crosses the public API.
	dbos.RegisterWorkflow(system.Root(), allocator.initializeRootWorkflow, dbos.WithInstance(allocator))
	dbos.RegisterWorkflow(system.Root(), allocator.allocateWorkflow, dbos.WithInstance(allocator))
	return allocator, nil
}

// ConfigName makes the allocator's two registered DBOS method workflows stable
// within its owned root. A root owns exactly one allocator capability.
func (a *FusedGovernedAllocator) ConfigName() string { return "governed-allocation" }

// Tracker exposes normal Provenance reads and setup registration over the same
// owned system database. Tracker has no AllocateGoverned method: standalone
// governed allocation is available only through Session.AllocateGoverned.
func (a *FusedGovernedAllocator) Tracker() Tracker {
	if a == nil {
		return nil
	}
	return a.tracker
}

// Launch starts the DBOS root created by OpenFusedGovernedAllocator.
func (a *FusedGovernedAllocator) Launch() error {
	if a == nil || a.system == nil || a.system.Root() == nil {
		return fmt.Errorf("provenance.FusedGovernedAllocator.Launch: allocator is nil or uninitialized; impact: no governed workflow can run; fix: construct it with OpenFusedGovernedAllocator")
	}
	return dbos.Launch(a.system.Root())
}

// Close releases the factory-owned DBOS root and then invalidates only the
// local borrowed tracker wrapper. It is safe to call repeatedly.
func (a *FusedGovernedAllocator) Close(timeout time.Duration) error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		if a.tracker != nil {
			a.closeErr = a.tracker.Close()
		}
		if a.system != nil {
			a.system.Close(timeout)
		}
	})
	return a.closeErr
}

// RunInitializeRoot executes genesis through the capability's registered DBOS
// workflow. A caller-supplied workflow ID is additionally bound to the exact
// canonical fused input before an existing workflow result can be attached.
func (a *FusedGovernedAllocator) RunInitializeRoot(ctx context.Context, workflowID string, request RootGenesisRequest) (OperationClosure, error) {
	if err := a.requireReady(ctx, request.OperationID, "RunInitializeRoot"); err != nil {
		return OperationClosure{}, err
	}
	input, err := newFusedGenesisInput(request)
	if err != nil {
		return OperationClosure{}, err
	}
	return a.runFusedWorkflow(workflowID, request.OperationID, "RunInitializeRoot", input, a.initializeRootWorkflow)
}

// RunAllocate executes an allocation under its explicit exact parent start
// authority. It has no zero-authority overload; fused and standalone paths both
// reach ReduceAllocation with a mandatory authority value.
func (a *FusedGovernedAllocator) RunAllocate(ctx context.Context, workflowID string, authority JournalID, request GovernedAllocationRequest) (OperationClosure, error) {
	if err := a.requireReady(ctx, request.OperationID, "RunAllocate"); err != nil {
		return OperationClosure{}, err
	}
	input, err := newFusedAllocationInput(authority, request)
	if err != nil {
		return OperationClosure{}, err
	}
	return a.runFusedWorkflow(workflowID, request.OperationID, "RunAllocate", input, a.allocateWorkflow)
}

func (a *FusedGovernedAllocator) requireReady(ctx context.Context, operation OperationID, method string) error {
	if a == nil || a.system == nil || a.system.Root() == nil {
		return allocation.NewError(allocation.ErrorValidation, operation, "FusedGovernedAllocator."+method, "the fused allocator is nil or uninitialized", "the reducer was not called", "construct it with OpenFusedGovernedAllocator", nil)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return allocation.NewError(allocation.ErrorValidation, operation, "FusedGovernedAllocator."+method, "the caller context is already cancelled", "no DBOS workflow was started", "retry with an active context", context.Cause(ctx))
		}
	}
	return nil
}

const fusedWorkflowInputVersion = 1

// fusedWorkflowInput is the only DBOS workflow input format for governed
// operations. CanonicalRequest is the complete request receipt generated by
// allocation.Canonicalize{Genesis,Allocation}; Authority deliberately remains
// separate because it is the Session/fused caller capability, not a request
// field. The struct has only fixed fields, so json.Marshal gives one stable
// encoding to compare with DBOS's persisted workflow input.
type fusedWorkflowInput struct {
	Version          int       `json:"version"`
	Authority        JournalID `json:"authority"`
	CanonicalRequest []byte    `json:"canonicalRequest"`
}

func newFusedGenesisInput(request RootGenesisRequest) (fusedWorkflowInput, error) {
	canonical, _, err := allocation.CanonicalizeGenesis(request)
	if err != nil {
		return fusedWorkflowInput{}, err
	}
	return newFusedWorkflowInput(0, canonical), nil
}

func newFusedAllocationInput(authority JournalID, request GovernedAllocationRequest) (fusedWorkflowInput, error) {
	canonical, _, err := allocation.CanonicalizeAllocation(request)
	if err != nil {
		return fusedWorkflowInput{}, err
	}
	return newFusedWorkflowInput(authority, canonical), nil
}

func newFusedWorkflowInput(authority JournalID, canonical []byte) fusedWorkflowInput {
	return fusedWorkflowInput{
		Version:          fusedWorkflowInputVersion,
		Authority:        authority,
		CanonicalRequest: append([]byte(nil), canonical...),
	}
}

// encoded is the exact JSON payload DBOS persists when using its default
// v0.20 serializer. ListWorkflows returns that decoded JSON string through its
// public API, allowing byte-level replay identity verification without touching
// DBOS implementation tables.
func (input fusedWorkflowInput) encoded() ([]byte, error) {
	if input.Version != fusedWorkflowInputVersion || len(input.CanonicalRequest) == 0 {
		return nil, fmt.Errorf("invalid fused workflow input version or canonical request")
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode canonical fused workflow input: %w", err)
	}
	return encoded, nil
}

func (input fusedWorkflowInput) genesisRequest() (RootGenesisRequest, error) {
	if input.Version != fusedWorkflowInputVersion {
		return RootGenesisRequest{}, allocation.NewError(allocation.ErrorCorruption, "", "fused genesis workflow input", "the persisted fused input version is unsupported", "the workflow input cannot be safely replayed", "start a new workflow with a current canonical input", nil)
	}
	request, err := allocation.DecodeGenesisRequest(input.CanonicalRequest)
	if err != nil {
		return RootGenesisRequest{}, allocation.NewError(allocation.ErrorCorruption, "", "fused genesis workflow input", "the persisted canonical request cannot be reconstructed", "the workflow input cannot be safely replayed", "restore the matching DBOS workflow input or start a new genesis workflow", err)
	}
	return request, nil
}

func (input fusedWorkflowInput) allocationRequest() (GovernedAllocationRequest, error) {
	if input.Version != fusedWorkflowInputVersion {
		return GovernedAllocationRequest{}, allocation.NewError(allocation.ErrorCorruption, "", "fused allocation workflow input", "the persisted fused input version is unsupported", "the workflow input cannot be safely replayed", "start a new workflow with a current canonical input", nil)
	}
	request, err := allocation.DecodeAllocationRequest(input.CanonicalRequest)
	if err != nil {
		return GovernedAllocationRequest{}, allocation.NewError(allocation.ErrorCorruption, "", "fused allocation workflow input", "the persisted canonical request cannot be reconstructed", "the workflow input cannot be safely replayed", "restore the matching DBOS workflow input or start a new allocation workflow", err)
	}
	return request, nil
}

// runFusedWorkflow verifies durable workflow identity on both sides of DBOS
// creation. The preflight prevents attaching to an already-known caller ID;
// the postflight check closes the race where another caller creates that ID
// between preflight and RunWorkflow. In either case a byte mismatch is a typed
// governed conflict and no reducer closure is returned.
func (a *FusedGovernedAllocator) runFusedWorkflow(workflowID string, operation OperationID, method string, input fusedWorkflowInput, workflow dbos.Workflow[fusedWorkflowInput, fusedOperationResult]) (OperationClosure, error) {
	want, err := input.encoded()
	if err != nil {
		return OperationClosure{}, allocation.NewError(allocation.ErrorValidation, operation, "FusedGovernedAllocator."+method, "the canonical fused workflow input could not be encoded", "no DBOS workflow or governed allocation was started", "supply a supported governed request and authority", err)
	}
	if workflowID != "" {
		found, err := a.verifyWorkflowInput(workflowID, operation, want)
		if err != nil {
			return OperationClosure{}, err
		}
		if found {
			return a.retrieveFusedWorkflow(workflowID, method)
		}
	}

	handle, err := dbos.RunWorkflow(a.system.Root(), workflow, input, dbos.WithWorkflowID(workflowID), dbos.WithRunInstance(a))
	if err != nil {
		// An enqueue/existence race may surface as an insert error rather than a
		// polling handle. Re-load through the public API before reporting it.
		found, verifyErr := a.verifyWorkflowInput(workflowID, operation, want)
		if verifyErr != nil {
			return OperationClosure{}, verifyErr
		}
		if !found {
			return OperationClosure{}, fmt.Errorf("provenance.FusedGovernedAllocator.%s: start DBOS workflow: %w", method, err)
		}
		return a.retrieveFusedWorkflow(workflowID, method)
	}

	found, err := a.verifyWorkflowInput(handle.GetWorkflowID(), operation, want)
	if err != nil {
		return OperationClosure{}, err
	}
	if !found {
		return OperationClosure{}, allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator workflow replay identity", fmt.Sprintf("DBOS returned workflow handle %q but its persisted input was not found", handle.GetWorkflowID()), "no closure was returned because the workflow identity cannot be verified", "retry with a new workflow ID after repairing DBOS workflow durability", nil)
	}
	result, err := handle.GetResult()
	if err != nil {
		return OperationClosure{}, fmt.Errorf("provenance.FusedGovernedAllocator.%s: retrieve DBOS workflow result: %w", method, err)
	}
	return result.unwrap()
}

func (a *FusedGovernedAllocator) retrieveFusedWorkflow(workflowID, method string) (OperationClosure, error) {
	handle, err := dbos.RetrieveWorkflow[fusedOperationResult](a.system.Root(), workflowID)
	if err != nil {
		return OperationClosure{}, fmt.Errorf("provenance.FusedGovernedAllocator.%s: retrieve matched DBOS workflow: %w", method, err)
	}
	result, err := handle.GetResult()
	if err != nil {
		return OperationClosure{}, fmt.Errorf("provenance.FusedGovernedAllocator.%s: retrieve DBOS workflow result: %w", method, err)
	}
	return result.unwrap()
}

// verifyWorkflowInput loads input through DBOS v0.20's public ListWorkflows
// API. The owned fused capability uses DBOS's default JSON serializer, whose
// public status input is the original JSON payload string. Comparing those
// bytes (rather than decoded maps or semantic fields) prevents a caller ID
// from silently attaching to a different request or authority.
func (a *FusedGovernedAllocator) verifyWorkflowInput(workflowID string, operation OperationID, want []byte) (bool, error) {
	if workflowID == "" {
		return false, nil
	}
	workflows, err := dbos.ListWorkflows(a.system.Root(),
		dbos.WithWorkflowIDs([]string{workflowID}),
		dbos.WithLimit(2),
		dbos.WithLoadInput(true),
		dbos.WithLoadOutput(false),
	)
	if err != nil {
		return false, fmt.Errorf("provenance.FusedGovernedAllocator: load persisted DBOS workflow input %q: %w", workflowID, err)
	}
	if len(workflows) == 0 {
		return false, nil
	}
	if len(workflows) != 1 {
		return false, allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator workflow replay identity", fmt.Sprintf("DBOS returned %d workflows for caller workflow ID %q", len(workflows), workflowID), "no workflow result was attached", "repair the duplicate DBOS workflow records before retrying", nil)
	}
	persisted, ok := workflows[0].Input.(string)
	if !ok {
		return false, allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator workflow replay identity", fmt.Sprintf("DBOS returned persisted workflow input as %T instead of canonical JSON bytes", workflows[0].Input), "no workflow result was attached", "restore the workflow with the default DBOS JSON serializer or use a new workflow ID", nil)
	}
	if !bytes.Equal([]byte(persisted), want) {
		return false, allocation.NewError(allocation.ErrorConflict, operation, "FusedGovernedAllocator workflow replay identity", fmt.Sprintf("caller workflow ID %q is already bound to different canonical request or authority bytes", workflowID), "no closure was returned and no governed rows were written", "retry the original exact fused input or choose a new workflow ID for changed request or authority", nil)
	}
	return true, nil
}

// fusedOperationResult transports typed domain rejections as DBOS-successful
// deterministic outputs. Returning a typed Error directly through DBOS would
// persist only an error string and lose errors.As semantics on retry.
type fusedOperationResult struct {
	Closure *OperationClosure
	Failure *fusedAllocationFailure
}

type fusedAllocationFailure struct {
	Kind      GovernedAllocationErrorKind
	Operation OperationID
	Where     string
	Why       string
	Impact    string
	Fix       string
}

func (r fusedOperationResult) unwrap() (OperationClosure, error) {
	if r.Failure != nil {
		return OperationClosure{}, allocation.NewError(r.Failure.Kind, r.Failure.Operation, r.Failure.Where, r.Failure.Why, r.Failure.Impact, r.Failure.Fix, nil)
	}
	if r.Closure == nil {
		return OperationClosure{}, allocation.NewError(allocation.ErrorCorruption, "", "FusedGovernedAllocator result", "DBOS returned neither a closure nor a typed failure", "the fused outcome is not trusted", "restore the matching DBOS workflow output or retry a new operation", nil)
	}
	return *r.Closure, nil
}

func resultFrom(closure OperationClosure, err error) (fusedOperationResult, error) {
	if err == nil {
		copy := closure
		return fusedOperationResult{Closure: &copy}, nil
	}
	var governed *allocation.Error
	if !errors.As(err, &governed) {
		return fusedOperationResult{}, err
	}
	return fusedOperationResult{Failure: &fusedAllocationFailure{
		Kind: governed.Kind, Operation: governed.Operation, Where: governed.Where,
		Why: governed.Why, Impact: governed.Impact, Fix: governed.Fix,
	}}, nil
}

func (a *FusedGovernedAllocator) initializeRootWorkflow(ctx dbos.DBOSContext, input fusedWorkflowInput) (fusedOperationResult, error) {
	request, err := input.genesisRequest()
	if err != nil {
		return resultFrom(OperationClosure{}, err)
	}
	closure, err := fusedtx.Run(ctx, a.system, func(txCtx context.Context, tx fusedtx.SQLTx) (OperationClosure, error) {
		return allocation.ReduceGenesis(txCtx, tx, request)
	})
	return resultFrom(closure, err)
}

func (a *FusedGovernedAllocator) allocateWorkflow(ctx dbos.DBOSContext, input fusedWorkflowInput) (fusedOperationResult, error) {
	request, err := input.allocationRequest()
	if err != nil {
		return resultFrom(OperationClosure{}, err)
	}
	closure, err := fusedtx.Run(ctx, a.system, func(txCtx context.Context, tx fusedtx.SQLTx) (OperationClosure, error) {
		return allocation.ReduceAllocation(txCtx, tx, request, input.Authority)
	})
	return resultFrom(closure, err)
}
