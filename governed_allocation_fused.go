package provenance

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dayvidpham/provenance/internal/allocation"
	"github.com/dayvidpham/provenance/internal/fusedtx"
	provenancesqlite "github.com/dayvidpham/provenance/internal/sqlite"
	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
)

// BoundGovernedAllocator is the factory-certified host-owned allocation
// capability. Its narrow API owns lifecycle and setup without exposing DBOS,
// SQL, or transaction handles.
type BoundGovernedAllocator struct {
	allocator *FusedGovernedAllocator
}

// HostBoundGovernedAllocator is a construction-time-only borrowed runner for a
// host that already owns the DBOS application root. It registers Provenance's
// composed workflows on that root and runs them through the caller-supplied SQL
// handle, but deliberately has no Launch, Close, Tracker, DBOS, or SQL accessor.
// The host remains the sole lifecycle owner.
type HostBoundGovernedAllocator struct {
	allocator *FusedGovernedAllocator
}

// NewHostBoundGovernedAllocator borrows the two exact local variables used by a
// host while constructing its engine root: root and the *sql.DB assigned to
// dbos.Config.SQLiteSystemDB. The DBOS runtime provides no supported pointer inspector,
// so this constructor does not claim to certify independently assembled pairs;
// callers must pass the same variables at the one engine construction site.
// Registration must occur before the host launches root.
func NewHostBoundGovernedAllocator(ctx context.Context, root dbos.Context, systemDB *sql.DB, participant GovernedAllocationParticipant) (*HostBoundGovernedAllocator, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("provenance.NewHostBoundGovernedAllocator: caller context is cancelled -- where: borrowed runner construction before workflow registration; impact: no workflow was registered and no lifecycle action occurred; fix: retry with an active construction context: %w", context.Cause(ctx))
	}
	system, err := fusedtx.BindSystem(root, systemDB)
	if err != nil {
		return nil, fmt.Errorf("provenance.NewHostBoundGovernedAllocator: borrow the engine DBOS/SQLite root pair: %w", err)
	}
	tracker, err := OpenBorrowedSQLite(systemDB)
	if err != nil {
		return nil, fmt.Errorf("provenance.NewHostBoundGovernedAllocator: activate Provenance on the caller-owned SQLiteSystemDB before workflow registration -- impact: no DBOS lifecycle action occurred; fix: pass a live pre-launch engine database with Provenance-compatible schema: %w", err)
	}
	allocator := &FusedGovernedAllocator{system: system, tracker: tracker, participant: participant}
	registerFusedGovernedAllocatorWorkflows(root, allocator)
	return &HostBoundGovernedAllocator{allocator: allocator}, nil
}

func (a *HostBoundGovernedAllocator) RunInitializeRoot(ctx context.Context, workflowID string, request RootGenesisRequest) (OperationClosure, error) {
	if a == nil || a.allocator == nil {
		return OperationClosure{}, allocation.NewError(allocation.ErrorValidation, request.OperationID, "HostBoundGovernedAllocator.RunInitializeRoot", "the borrowed runner is nil or uninitialized", "no DBOS workflow was started", "construct it once with NewHostBoundGovernedAllocator before the engine launches its root", nil)
	}
	return a.allocator.RunInitializeRoot(ctx, workflowID, request)
}

func (a *HostBoundGovernedAllocator) RunAllocateComposed(ctx context.Context, workflowID string, authority JournalID, request GovernedAllocationComposedRequest) (GovernedAllocationComposedResult, error) {
	if len(request.Allocation.Children) != 1 {
		return GovernedAllocationComposedResult{}, allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "HostBoundGovernedAllocator.RunAllocateComposed", "the one-child composed allocation entry point requires exactly one child", "no DBOS workflow was started", "use RunAllocateComposedBatch for 1..128 ordered children", nil)
	}
	return a.RunAllocateComposedBatch(ctx, workflowID, authority, request)
}

func (a *HostBoundGovernedAllocator) RunAllocateComposedBatch(ctx context.Context, workflowID string, authority JournalID, request GovernedAllocationComposedRequest) (GovernedAllocationComposedResult, error) {
	if a == nil || a.allocator == nil {
		return GovernedAllocationComposedResult{}, allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "HostBoundGovernedAllocator.RunAllocateComposedBatch", "the borrowed runner is nil or uninitialized", "no DBOS workflow was started", "construct it once with NewHostBoundGovernedAllocator before the engine launches its root", nil)
	}
	return a.allocator.RunAllocateComposedBatch(ctx, workflowID, authority, request)
}

// GovernedAllocationSupplementOperationID returns the stable producer identity
// used by composed supplemental effects. This pure correlation helper grants no
// capability to create or execute reserved operations, bind DBOS, or access SQL.
func GovernedAllocationSupplementOperationID(external OperationID) OperationID {
	return allocation.GovernedAllocationSupplementOperationID(external)
}

// OpenBoundGovernedAllocator constructs the host-facing, task-level capability
// without accepting or exposing DBOS or SQL handles. The returned capability
// owns its root lifecycle and certifies exact-handle fused durability by
// construction.
func OpenBoundGovernedAllocator(ctx context.Context, config FusedGovernedAllocatorConfig) (*BoundGovernedAllocator, error) {
	allocator, err := OpenFusedGovernedAllocator(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("provenance.OpenBoundGovernedAllocator: construct certified fused host binding: %w", err)
	}
	return &BoundGovernedAllocator{allocator: allocator}, nil
}

// Tracker exposes setup and read access without exposing the certified SQL or
// DBOS handles.
func (a *BoundGovernedAllocator) Tracker() Tracker {
	if a == nil || a.allocator == nil {
		return nil
	}
	return a.allocator.Tracker()
}

// Launch starts the factory-owned DBOS root after all Provenance workflows have
// been registered.
func (a *BoundGovernedAllocator) Launch() error {
	if a == nil || a.allocator == nil {
		return fmt.Errorf("provenance.BoundGovernedAllocator.Launch: allocator is nil or uninitialized -- where: certified host launch; impact: no workflow can run; fix: construct it with OpenBoundGovernedAllocator")
	}
	return a.allocator.Launch()
}

// Close releases the factory-owned certified binding.
func (a *BoundGovernedAllocator) Close(timeout time.Duration) error {
	if a == nil || a.allocator == nil {
		return nil
	}
	return a.allocator.Close(timeout)
}

// RunInitializeRoot initializes the certified binding's governed authority.
func (a *BoundGovernedAllocator) RunInitializeRoot(ctx context.Context, workflowID string, request RootGenesisRequest) (OperationClosure, error) {
	if a == nil || a.allocator == nil {
		return OperationClosure{}, allocation.NewError(allocation.ErrorValidation, request.OperationID, "BoundGovernedAllocator.RunInitializeRoot", "the bound allocator is nil or uninitialized", "no DBOS workflow was started", "construct it with OpenBoundGovernedAllocator", nil)
	}
	return a.allocator.RunInitializeRoot(ctx, workflowID, request)
}

// RunAllocateComposed executes a composed allocation that must carry exactly
// one child. It is the narrow entry point; RunAllocateComposedBatch accepts the
// same request type with 1..128 ordered children.
func (a *BoundGovernedAllocator) RunAllocateComposed(ctx context.Context, workflowID string, authority JournalID, request GovernedAllocationComposedRequest) (GovernedAllocationComposedResult, error) {
	if len(request.Allocation.Children) != 1 {
		return GovernedAllocationComposedResult{}, allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "BoundGovernedAllocator.RunAllocateComposed", "the one-child composed allocation entry point requires exactly one child", "no DBOS workflow was started", "use RunAllocateComposedBatch for 1..128 ordered children", nil)
	}
	return a.RunAllocateComposedBatch(ctx, workflowID, authority, request)
}

// RunAllocateComposedBatch executes an ordered multi-child composition through
// the allocator bound to the host's exact DBOS/SQLite system handle.
func (a *BoundGovernedAllocator) RunAllocateComposedBatch(ctx context.Context, workflowID string, authority JournalID, request GovernedAllocationComposedRequest) (GovernedAllocationComposedResult, error) {
	if a == nil || a.allocator == nil {
		return GovernedAllocationComposedResult{}, allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "BoundGovernedAllocator.RunAllocateComposedBatch", "the bound allocator is nil or uninitialized", "no DBOS workflow was started", "construct it with OpenBoundGovernedAllocator before launch", nil)
	}
	return a.allocator.RunAllocateComposedBatch(ctx, workflowID, authority, request)
}

// FusedGovernedAllocatorConfig is the closed configuration for the one public
// governed DBOS capability. It deliberately accepts a DSN, not a DBOS context
// or *sql.DB: the capability opens one owned system handle, gives that exact
// handle to DBOS, and constructs the fused data source before exposing work.
type FusedGovernedAllocatorConfig struct {
	SQLiteDSN          string
	AppName            string
	ApplicationVersion string
	Logger             *slog.Logger
	// Participant optionally writes an integration-owned audit or projection
	// record in the exact transaction that commits a fused allocation. It is
	// intentionally unavailable to the standalone Session path.
	Participant GovernedAllocationParticipant
}

// FusedGovernedAllocator owns the DBOS root, exact SQLite system handle, and
// matching Provenance tracker as one capability. Close shuts down the owned DBOS
// root; it never closes caller-owned resources because callers supply neither a
// root nor a database handle.
type FusedGovernedAllocator struct {
	system      *fusedtx.System
	tracker     Tracker
	participant GovernedAllocationParticipant
	ownsRoot    bool

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
		return nil, fmt.Errorf("provenance.OpenFusedGovernedAllocator: activate Provenance schema on the owned DBOS system handle: %w",
			errors.Join(err, system.Close(30*time.Second)))
	}
	allocator := &FusedGovernedAllocator{system: system, tracker: tracker, participant: config.Participant, ownsRoot: true}
	// Registration occurs before Launch and uses the allocator instance as the
	// DBOS configured instance. No raw root/handle pair crosses the public API.
	registerFusedGovernedAllocatorWorkflows(system.Root(), allocator)
	return allocator, nil
}

func registerFusedGovernedAllocatorWorkflows(root dbos.Context, allocator *FusedGovernedAllocator) {
	dbos.RegisterWorkflow(root, allocator.initializeRootWorkflow, dbos.WithInstance(allocator))
	dbos.RegisterWorkflow(root, allocator.allocateWorkflow, dbos.WithInstance(allocator))
	dbos.RegisterWorkflow(root, allocator.allocateComposedWorkflow, dbos.WithInstance(allocator))
}

// ConfigName makes the allocator's three registered DBOS method workflows
// (initializeRootWorkflow, allocateWorkflow, allocateComposedWorkflow) stable
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
	if !a.ownsRoot {
		return fmt.Errorf("provenance.FusedGovernedAllocator.Launch: borrowed host runner does not own the DBOS root -- where: lifecycle boundary; impact: the root was not launched; fix: launch the engine-owned root exactly once from the host")
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
		var trackerErr error
		if a.tracker != nil {
			trackerErr = a.tracker.Close()
		}
		var shutdownErr error
		if a.system != nil && a.ownsRoot {
			// A shutdown that times out leaves DBOS resources running on the
			// shared SQLite handle. Report it instead of dropping it: a caller
			// that treats an incomplete shutdown as complete may then close or
			// reuse a database that DBOS is still writing to.
			shutdownErr = a.system.Close(timeout)
		}
		a.closeErr = errors.Join(trackerErr, shutdownErr)
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

// RunAllocateComposed executes allocation plus the closed supplemental journal
// effect set in one DBOS-owned SQLite transaction. Its workflow identity is the
// complete canonical composition receipt, so a same workflow ID cannot attach
// to a changed effect order, payload, or result-slot binding.
func (a *FusedGovernedAllocator) RunAllocateComposed(ctx context.Context, workflowID string, authority JournalID, request GovernedAllocationComposedRequest) (GovernedAllocationComposedResult, error) {
	if len(request.Allocation.Children) != 1 {
		return GovernedAllocationComposedResult{}, allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "FusedGovernedAllocator.RunAllocateComposed", "the one-child composed allocation entry point requires exactly one child", "no DBOS workflow was started", "use RunAllocateComposedBatch for 1..128 ordered children", nil)
	}
	return a.RunAllocateComposedBatch(ctx, workflowID, authority, request)
}

// RunAllocateComposedBatch allocates the complete ordered child list and folds
// its shared supplements in one DBOS-owned SQLite transaction.
func (a *FusedGovernedAllocator) RunAllocateComposedBatch(ctx context.Context, workflowID string, authority JournalID, request GovernedAllocationComposedRequest) (GovernedAllocationComposedResult, error) {
	if err := a.requireReady(ctx, request.Allocation.OperationID, "RunAllocateComposedBatch"); err != nil {
		return GovernedAllocationComposedResult{}, err
	}
	input, err := newFusedComposedAllocationInput(authority, request)
	if err != nil {
		return GovernedAllocationComposedResult{}, err
	}
	// Authenticate an existing caller identity before reference admission. This
	// makes changed retries canonical conflicts even when their stale references
	// would independently fail the cheap new-work preflight.
	want, encodeErr := input.encoded()
	if encodeErr != nil {
		return GovernedAllocationComposedResult{}, allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "FusedGovernedAllocator.RunAllocateComposedBatch identity admission", "the canonical batch workflow input could not be encoded", "no DBOS workflow or governed rows were written", "supply a supported batch request and authority", encodeErr)
	}
	if workflowID != "" {
		found, verifyErr := a.verifyWorkflowInput(workflowID, request.Allocation.OperationID, "RunAllocateComposedBatch", want)
		if verifyErr != nil {
			return GovernedAllocationComposedResult{}, verifyErr
		}
		if found {
			return a.retrieveFusedComposedWorkflow(workflowID, request.Allocation.OperationID, "RunAllocateComposedBatch", input)
		}
	}
	// Operation identity precedes all fresh-work reference and condition
	// admission.  This is deliberately durable (not a DBOS workflow-ID lookup):
	// a distinct workflow ID for an exact receipt may replay, while changed input
	// must report the canonical governed conflict even if its references are now
	// stale or unauthorized.
	store, storeErr := a.sqliteStore(request.Allocation.OperationID)
	if storeErr != nil {
		return GovernedAllocationComposedResult{}, storeErr
	}
	exactReceipt, classifyErr := store.ClassifyComposedGovernedAllocationSnapshot(ctx, request, authority)
	if classifyErr != nil {
		return GovernedAllocationComposedResult{}, classifyErr
	}
	// This read-only preflight rejects task references that can already be
	// resolved as unrelated, avoiding a durable DBOS error checkpoint for an
	// invalid caller request. It is not an authorization decision: the fused
	// transaction repeats the fence against its own snapshot before any write,
	// so no check-then-act gap can admit a concurrent lineage/revocation change.
	if !exactReceipt {
		if err := a.preflightComposedSupplementReferences(ctx, request); err != nil {
			return GovernedAllocationComposedResult{}, err
		}
	}
	return a.runFusedComposedWorkflow(workflowID, request.Allocation.OperationID, "RunAllocateComposedBatch", input, a.allocateComposedWorkflow)
}

func (a *FusedGovernedAllocator) preflightComposedSupplementReferences(ctx context.Context, request GovernedAllocationComposedRequest) error {
	borrowed, ok := a.tracker.(*borrowedTracker)
	if !ok {
		return allocation.NewError(allocation.ErrorCorruption, request.Allocation.OperationID, "FusedGovernedAllocator.RunAllocateComposedBatch reference preflight", "the fused allocator does not retain its expected borrowed tracker", "no DBOS workflow or allocation was started", "recreate the fused allocator with OpenFusedGovernedAllocator", nil)
	}
	if err := borrowed.available("FusedGovernedAllocator.RunAllocateComposedBatch reference preflight"); err != nil {
		return err
	}
	tracker, ok := borrowed.inner.(*sqliteTracker)
	if !ok || tracker.db == nil {
		return allocation.NewError(allocation.ErrorCorruption, request.Allocation.OperationID, "FusedGovernedAllocator.RunAllocateComposedBatch reference preflight", "the borrowed tracker has no SQLite reference-validation store", "no DBOS workflow or allocation was started", "recreate the fused allocator with OpenFusedGovernedAllocator", nil)
	}
	if err := tracker.db.PreflightComposedGovernedAllocation(ctx, request); err != nil {
		return err
	}
	return nil
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
// allocation.CanonicalizeGenesis, allocation.CanonicalizeAllocation, or the
// explicit allocation.CanonicalizeComposed API; Authority deliberately remains
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

// newFusedAllocationInput preserves the baseline simple workflow identity. A
// persisted simple DBOS workflow therefore remains retrievable after the
// composed allocation API is added alongside it.
func newFusedAllocationInput(authority JournalID, request GovernedAllocationRequest) (fusedWorkflowInput, error) {
	canonical, _, err := allocation.CanonicalizeAllocation(request)
	if err != nil {
		return fusedWorkflowInput{}, err
	}
	return newFusedWorkflowInput(authority, canonical), nil
}

func newFusedComposedAllocationInput(authority JournalID, request GovernedAllocationComposedRequest) (fusedWorkflowInput, error) {
	canonical, _, err := allocation.CanonicalizeComposed(request)
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
// runtime serializer. ListWorkflows returns that decoded JSON string through its
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

func (input fusedWorkflowInput) composedAllocationRequest() (GovernedAllocationComposedRequest, error) {
	if input.Version != fusedWorkflowInputVersion {
		return GovernedAllocationComposedRequest{}, allocation.NewError(allocation.ErrorCorruption, "", "fused composed allocation workflow input", "the persisted fused input version is unsupported", "the workflow input cannot be safely replayed", "start a new workflow with a current canonical input", nil)
	}
	request, err := allocation.DecodeComposedRequest(input.CanonicalRequest)
	if err != nil {
		return GovernedAllocationComposedRequest{}, allocation.NewError(allocation.ErrorCorruption, "", "fused composed allocation workflow input", "the persisted canonical request cannot be reconstructed", "the workflow input cannot be safely replayed", "restore the matching DBOS workflow input or start a new allocation workflow", err)
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
		found, err := a.verifyWorkflowInput(workflowID, operation, method, want)
		if err != nil {
			return OperationClosure{}, err
		}
		if found {
			return a.retrieveFusedWorkflow(workflowID, operation, method, input)
		}
	}

	handle, err := dbos.RunWorkflow(a.system.Root(), workflow, input, dbos.WithWorkflowID(workflowID), dbos.WithRunInstance(a))
	if err != nil {
		// An enqueue/existence race may surface as an insert error rather than a
		// polling handle. Re-load through the public API before reporting it.
		found, verifyErr := a.verifyWorkflowInput(workflowID, operation, method, want)
		if verifyErr != nil {
			return OperationClosure{}, verifyErr
		}
		if !found {
			return OperationClosure{}, fmt.Errorf("provenance.FusedGovernedAllocator.%s: start DBOS workflow: %w", method, err)
		}
		return a.retrieveFusedWorkflow(workflowID, operation, method, input)
	}

	found, err := a.verifyWorkflowInput(handle.GetWorkflowID(), operation, method, want)
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
	return a.finalizeFusedResult(input, operation, result)
}

func (a *FusedGovernedAllocator) retrieveFusedWorkflow(workflowID string, operation OperationID, method string, input fusedWorkflowInput) (OperationClosure, error) {
	handle, err := dbos.RetrieveWorkflow[fusedOperationResult](a.system.Root(), workflowID)
	if err != nil {
		return OperationClosure{}, fmt.Errorf("provenance.FusedGovernedAllocator.%s: retrieve matched DBOS workflow: %w", method, err)
	}
	result, err := handle.GetResult()
	if err != nil {
		return OperationClosure{}, fmt.Errorf("provenance.FusedGovernedAllocator.%s: retrieve DBOS workflow result: %w", method, err)
	}
	return a.finalizeFusedResult(input, operation, result)
}

func (a *FusedGovernedAllocator) runFusedComposedWorkflow(workflowID string, operation OperationID, method string, input fusedWorkflowInput, workflow dbos.Workflow[fusedWorkflowInput, fusedComposedOperationResult]) (GovernedAllocationComposedResult, error) {
	want, err := input.encoded()
	if err != nil {
		return GovernedAllocationComposedResult{}, allocation.NewError(allocation.ErrorValidation, operation, "FusedGovernedAllocator."+method, "the canonical fused workflow input could not be encoded", "no DBOS workflow or governed allocation was started", "supply a supported composed governed request and authority", err)
	}
	if workflowID != "" {
		found, err := a.verifyWorkflowInput(workflowID, operation, method, want)
		if err != nil {
			return GovernedAllocationComposedResult{}, err
		}
		if found {
			return a.retrieveFusedComposedWorkflow(workflowID, operation, method, input)
		}
	}
	handle, err := dbos.RunWorkflow(a.system.Root(), workflow, input, dbos.WithWorkflowID(workflowID), dbos.WithRunInstance(a))
	if err != nil {
		found, verifyErr := a.verifyWorkflowInput(workflowID, operation, method, want)
		if verifyErr != nil {
			return GovernedAllocationComposedResult{}, verifyErr
		}
		if !found {
			return GovernedAllocationComposedResult{}, fmt.Errorf("provenance.FusedGovernedAllocator.%s: start DBOS workflow: %w", method, err)
		}
		return a.retrieveFusedComposedWorkflow(workflowID, operation, method, input)
	}
	found, err := a.verifyWorkflowInput(handle.GetWorkflowID(), operation, method, want)
	if err != nil {
		return GovernedAllocationComposedResult{}, err
	}
	if !found {
		return GovernedAllocationComposedResult{}, allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator workflow replay identity", fmt.Sprintf("DBOS returned workflow handle %q but its persisted input was not found", handle.GetWorkflowID()), "no composed result was returned because the workflow identity cannot be verified", "retry with a new workflow ID after repairing DBOS workflow durability", nil)
	}
	result, err := handle.GetResult()
	if err != nil {
		return GovernedAllocationComposedResult{}, fmt.Errorf("provenance.FusedGovernedAllocator.%s: retrieve DBOS workflow result: %w", method, err)
	}
	return a.finalizeFusedComposedResult(input, operation, result, false)
}

func (a *FusedGovernedAllocator) retrieveFusedComposedWorkflow(workflowID string, operation OperationID, method string, input fusedWorkflowInput) (GovernedAllocationComposedResult, error) {
	handle, err := dbos.RetrieveWorkflow[fusedComposedOperationResult](a.system.Root(), workflowID)
	if err != nil {
		return GovernedAllocationComposedResult{}, fmt.Errorf("provenance.FusedGovernedAllocator.%s: retrieve matched DBOS workflow: %w", method, err)
	}
	result, err := handle.GetResult()
	if err != nil {
		return GovernedAllocationComposedResult{}, fmt.Errorf("provenance.FusedGovernedAllocator.%s: retrieve DBOS workflow result: %w", method, err)
	}
	return a.finalizeFusedComposedResult(input, operation, result, true)
}

func validFusedFailure(f *fusedAllocationFailure, operation OperationID) bool {
	return f != nil && f.Operation == operation && f.Kind >= allocation.ErrorValidation && f.Kind <= allocation.ErrorCorruption &&
		f.Where != "" && f.Why != "" && f.Impact != "" && f.Fix != ""
}

func (a *FusedGovernedAllocator) authenticateFusedFailure(input fusedWorkflowInput, operation OperationID, failure *fusedAllocationFailure, hasSuccess bool) error {
	if hasSuccess || !validFusedFailure(failure, operation) {
		return allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator durable failure binding", "DBOS returned a mixed, malformed, unknown, or operation-mismatched failure output", "no failure or success result was trusted", "restore the DBOS output from the same durable backup as the authoritative journal", nil)
	}
	committed, err := a.tracker.Journal().LookupCommitted(operation)
	if failure.Kind == allocation.ErrorConflict && err == nil && committed.Kind != CommittedAbsent {
		store, storeErr := a.sqliteStore(operation)
		if storeErr != nil {
			return storeErr
		}
		proved := store.ProveGovernedCanonicalConflictSnapshot(context.Background(), operation, input.CanonicalRequest)
		if proved != nil {
			var governed *allocation.Error
			if errors.As(proved, &governed) && governed.Kind == allocation.ErrorConflict {
				return proved
			}
			global := store.ProveCommittedOperationIDCollisionSnapshot(context.Background(), operation)
			if errors.As(global, &governed) && governed.Kind == allocation.ErrorConflict {
				return global
			}
			return allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator durable conflict binding", "SQLite could not prove a canonically different, structurally valid governed receipt", "the DBOS conflict was not trusted and no result was returned", "repair the governed receipt from a consistent backup or retry with a new OperationID", proved)
		}
		return allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator durable conflict binding", "SQLite conflict proof returned no authoritative conflict", "the DBOS conflict was not trusted and no result was returned", "repair the governed receipt from a consistent backup", nil)
	}
	if err != nil || committed.Kind != CommittedAbsent {
		return allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator durable failure binding", "DBOS reported failure but the authoritative journal does not prove the requested operation absent", "the durable failure is not trusted and no result was returned", "reconcile the DBOS workflow and journal from the same durable backup", err)
	}
	return allocation.NewError(failure.Kind, operation, "FusedGovernedAllocator durable governed rejection", "the governed workflow rejected the request before committing its operation identity", "nothing was committed and no receipt was returned", "correct the governed request identified by the typed rejection or retry with a new OperationID", nil)
}

func (a *FusedGovernedAllocator) finalizeFusedResult(input fusedWorkflowInput, operation OperationID, result fusedOperationResult) (OperationClosure, error) {
	if result.Failure != nil {
		return OperationClosure{}, a.authenticateFusedFailure(input, operation, result.Failure, result.Closure != nil)
	}
	if result.Closure == nil {
		return OperationClosure{}, a.authenticateFusedFailure(input, operation, nil, false)
	}
	return a.verifyFusedResult(input, operation, *result.Closure)
}

func (a *FusedGovernedAllocator) finalizeFusedComposedResult(input fusedWorkflowInput, operation OperationID, result fusedComposedOperationResult, retrieved bool) (GovernedAllocationComposedResult, error) {
	if result.Failure != nil {
		return GovernedAllocationComposedResult{}, a.authenticateFusedComposedFailure(input, operation, result.Failure, result.Result != nil)
	}
	if result.Result == nil {
		return GovernedAllocationComposedResult{}, a.authenticateFusedComposedFailure(input, operation, nil, false)
	}
	decoded := allocation.WithComposedReplay(allocation.NewComposedResult(result.Result.Closure(), journalResultFromComposed(*result.Result)), result.Replayed || retrieved)
	return a.verifyFusedComposedResult(input, operation, decoded)
}

func (a *FusedGovernedAllocator) authenticateFusedComposedFailure(input fusedWorkflowInput, operation OperationID, failure *fusedAllocationFailure, hasSuccess bool) error {
	if hasSuccess || !validFusedFailure(failure, operation) {
		return allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator durable failure binding", "DBOS returned a mixed, malformed, unknown, or operation-mismatched failure output", "no failure or success result was trusted", "restore the DBOS output from the same durable backup as the authoritative journal", nil)
	}
	committed, err := a.tracker.Journal().LookupCommitted(operation)
	if failure.Kind == allocation.ErrorConflict && err == nil && committed.Kind != CommittedAbsent {
		return a.authenticateFusedFailure(input, operation, failure, hasSuccess)
	}
	if err != nil || committed.Kind != CommittedAbsent {
		return allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator durable composed failure binding", "DBOS reported failure but the authoritative journal does not prove the requested operation absent", "the durable failure is not trusted and no result was returned", "reconcile the DBOS workflow and journal from the same durable backup", err)
	}
	request, decodeErr := input.composedAllocationRequest()
	if decodeErr != nil {
		return decodeErr
	}
	store, storeErr := a.sqliteStore(operation)
	if storeErr != nil {
		return storeErr
	}
	authoritative, proofErr := store.ProveAbsentComposedGovernedAllocationFailure(context.Background(), request, input.Authority)
	if proofErr == nil && authoritative != nil && authoritative.Kind == failure.Kind && authoritative.Operation == failure.Operation &&
		authoritative.Where == failure.Where && authoritative.Why == failure.Why && authoritative.Impact == failure.Impact && authoritative.Fix == failure.Fix {
		return authoritative
	}
	return allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator durable composed failure proof", "the persisted composed failure does not exactly match an authoritative rolled-back reducer rejection", "the untrusted DBOS failure was not returned and no composed result was produced", "restore matching DBOS and SQLite durability from one backup or retry a new operation after repair", proofErr)
}

func (a *FusedGovernedAllocator) sqliteStore(operation OperationID) (*provenancesqlite.DB, error) {
	borrowed, ok := a.tracker.(*borrowedTracker)
	if !ok {
		return nil, allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator authoritative result reconstruction", "the allocator has no borrowed SQLite tracker", "the DBOS result was not returned", "reopen the fused allocator", nil)
	}
	tracker, ok := borrowed.inner.(*sqliteTracker)
	if !ok || tracker.db == nil {
		return nil, allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator authoritative result reconstruction", "the allocator has no authoritative SQLite store", "the DBOS result was not returned", "reopen the fused allocator", nil)
	}
	return tracker.db, nil
}

func (a *FusedGovernedAllocator) verifyFusedResult(input fusedWorkflowInput, operation OperationID, decoded OperationClosure) (OperationClosure, error) {
	store, err := a.sqliteStore(operation)
	if err != nil {
		return OperationClosure{}, err
	}
	var authoritative OperationClosure
	if input.Authority == 0 {
		request, decodeErr := input.genesisRequest()
		if decodeErr != nil {
			return OperationClosure{}, decodeErr
		}
		authoritative, err = store.ReconstructGovernedGenesisSnapshot(context.Background(), request)
	} else {
		request, decodeErr := input.allocationRequest()
		if decodeErr != nil {
			return OperationClosure{}, decodeErr
		}
		authoritative, err = store.ReconstructGovernedAllocationSnapshot(context.Background(), request, input.Authority)
	}
	if err != nil {
		return OperationClosure{}, err
	}
	if !decoded.Equal(authoritative) {
		return OperationClosure{}, allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator DBOS result binding", "the decoded DBOS closure differs from the authoritative SQLite receipt", "no usable closure was returned", "restore matching DBOS and SQLite durability from one backup", nil)
	}
	return authoritative, nil
}

func (a *FusedGovernedAllocator) verifyFusedComposedResult(input fusedWorkflowInput, operation OperationID, decoded GovernedAllocationComposedResult) (GovernedAllocationComposedResult, error) {
	store, err := a.sqliteStore(operation)
	if err != nil {
		return GovernedAllocationComposedResult{}, err
	}
	request, err := input.composedAllocationRequest()
	if err != nil {
		return GovernedAllocationComposedResult{}, err
	}
	authoritative, err := store.ReconstructGovernedComposedSnapshot(context.Background(), request, input.Authority)
	if err != nil {
		return GovernedAllocationComposedResult{}, err
	}
	if !decoded.Closure().Equal(authoritative.Closure()) || !equalComposedBindings(decoded, authoritative) {
		return GovernedAllocationComposedResult{}, allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator composed DBOS result binding", "the decoded DBOS result differs from the authoritative SQLite receipt", "no usable composed receipt was returned", "restore matching DBOS and SQLite durability from one backup", nil)
	}
	return allocation.WithComposedReplay(authoritative, decoded.Replayed()), nil
}

func equalComposedBindings(left, right GovernedAllocationComposedResult) bool {
	leftEvents, rightEvents := left.SupplementalEmittedEvents(), right.SupplementalEmittedEvents()
	if len(leftEvents) != len(rightEvents) {
		return false
	}
	for i := range leftEvents {
		if leftEvents[i] != rightEvents[i] {
			return false
		}
	}
	leftSlots, rightSlots := left.SupplementalResultSlots(), right.SupplementalResultSlots()
	if len(leftSlots) != len(rightSlots) {
		return false
	}
	for i := range leftSlots {
		if leftSlots[i].Slot != rightSlots[i].Slot || leftSlots[i].ProducedJournalID != rightSlots[i].ProducedJournalID || leftSlots[i].Kind != rightSlots[i].Kind {
			return false
		}
		if (leftSlots[i].TaskID == nil) != (rightSlots[i].TaskID == nil) || leftSlots[i].TaskID != nil && *leftSlots[i].TaskID != *rightSlots[i].TaskID {
			return false
		}
		if (leftSlots[i].ActivityID == nil) != (rightSlots[i].ActivityID == nil) || leftSlots[i].ActivityID != nil && *leftSlots[i].ActivityID != *rightSlots[i].ActivityID {
			return false
		}
	}
	return true
}

// verifyWorkflowInput loads input through the DBOS runtime's public ListWorkflows
// API. The owned fused capability uses DBOS's default JSON serializer, whose
// public status input is the original JSON payload string. Comparing those
// bytes (rather than decoded maps or semantic fields) prevents a caller ID
// from silently attaching to a different request or authority.
func (a *FusedGovernedAllocator) verifyWorkflowInput(workflowID string, operation OperationID, method string, want []byte) (bool, error) {
	if workflowID == "" {
		return false, nil
	}
	workflows, err := dbos.ListWorkflows(a.system.Root(),
		dbos.WithFilterWorkflowIDs(workflowID),
		dbos.WithFilterLimit(2),
		dbos.WithFilterLoadInput(true),
		dbos.WithFilterLoadOutput(false),
	)
	if err != nil {
		return false, fmt.Errorf("provenance.FusedGovernedAllocator: load persisted DBOS workflow input %q: %w", workflowID, err)
	}
	if len(workflows) == 0 {
		return false, nil
	}
	if len(workflows) != 1 {
		return false, allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator."+method+" workflow replay identity", fmt.Sprintf("DBOS returned %d workflows for caller workflow ID %q", len(workflows), workflowID), "no workflow result was attached", "repair the duplicate DBOS workflow records before retrying", nil)
	}
	persisted, ok := workflows[0].Input.(string)
	if !ok {
		return false, allocation.NewError(allocation.ErrorCorruption, operation, "FusedGovernedAllocator."+method+" workflow replay identity", fmt.Sprintf("DBOS returned persisted workflow input as %T instead of canonical JSON bytes", workflows[0].Input), "no workflow result was attached", "restore the workflow with the default DBOS JSON serializer or use a new workflow ID", nil)
	}
	if !bytes.Equal([]byte(persisted), want) {
		return false, allocation.NewError(allocation.ErrorConflict, operation, "FusedGovernedAllocator."+method+" workflow replay identity", fmt.Sprintf("caller workflow ID %q is already bound to different canonical request or authority bytes", workflowID), "no closure was returned and no governed rows were written", "retry the original exact fused input or choose a new workflow ID for changed request or authority", nil)
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

// fusedComposedOperationResult transports a copy-safe composed receipt through
// DBOS. It mirrors fusedOperationResult so typed governed rejections remain
// durable successful workflow outputs rather than lossy error strings.
type fusedComposedOperationResult struct {
	Result   *GovernedAllocationComposedResult
	Failure  *fusedAllocationFailure
	Replayed bool
}

func (r fusedComposedOperationResult) unwrap() (GovernedAllocationComposedResult, error) {
	if r.Failure != nil {
		return GovernedAllocationComposedResult{}, allocation.NewError(r.Failure.Kind, r.Failure.Operation, r.Failure.Where, r.Failure.Why, r.Failure.Impact, r.Failure.Fix, nil)
	}
	if r.Result == nil {
		return GovernedAllocationComposedResult{}, allocation.NewError(allocation.ErrorCorruption, "", "FusedGovernedAllocator composed result", "DBOS returned neither a composed result nor a typed failure", "the fused outcome is not trusted", "restore the matching DBOS workflow output or retry a new operation", nil)
	}
	return allocation.WithComposedReplay(allocation.NewComposedResult(r.Result.Closure(), journalResultFromComposed(*r.Result)), r.Replayed), nil
}

func journalResultFromComposed(result GovernedAllocationComposedResult) CommittedResult {
	return CommittedResult{EmittedEvents: result.SupplementalEmittedEvents(), ResultSlots: result.SupplementalResultSlots()}
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

func composedResultFrom(result GovernedAllocationComposedResult, err error) (fusedComposedOperationResult, error) {
	if err == nil {
		copy := allocation.NewComposedResult(result.Closure(), journalResultFromComposed(result))
		return fusedComposedOperationResult{Result: &copy, Replayed: result.Replayed()}, nil
	}
	var governed *allocation.Error
	if !errors.As(err, &governed) {
		return fusedComposedOperationResult{}, err
	}
	return fusedComposedOperationResult{Failure: &fusedAllocationFailure{
		Kind: governed.Kind, Operation: governed.Operation, Where: governed.Where,
		Why: governed.Why, Impact: governed.Impact, Fix: governed.Fix,
	}}, nil
}

func (a *FusedGovernedAllocator) initializeRootWorkflow(ctx dbos.Context, input fusedWorkflowInput) (fusedOperationResult, error) {
	request, err := input.genesisRequest()
	if err != nil {
		return resultFrom(OperationClosure{}, err)
	}
	closure, err := fusedtx.Run(ctx, a.system, func(txCtx context.Context, tx fusedtx.SQLTx) (OperationClosure, error) {
		return allocation.ReduceGenesis(txCtx, tx, request)
	})
	return resultFrom(closure, err)
}

func (a *FusedGovernedAllocator) allocateComposedWorkflow(ctx dbos.Context, input fusedWorkflowInput) (fusedComposedOperationResult, error) {
	request, err := input.composedAllocationRequest()
	if err != nil {
		return composedResultFrom(GovernedAllocationComposedResult{}, err)
	}
	result, err := fusedtx.Run(ctx, a.system, func(txCtx context.Context, tx fusedtx.SQLTx) (GovernedAllocationComposedResult, error) {
		outcome, err := provenancesqlite.ReduceComposedGovernedAllocationOutcome(txCtx, tx, request, input.Authority)
		if err != nil {
			// Typed reducer rejections remain durable DBOS outcomes and must not
			// trigger an integration side effect.
			return GovernedAllocationComposedResult{}, err
		}
		// An exact composed retry reconstructs both persisted receipts. It must
		// never repeat the transaction participant, including when the caller uses
		// a distinct DBOS workflow ID for the same external operation.
		if a.participant == nil || outcome.Replayed {
			return allocation.WithComposedReplay(outcome.Result, outcome.Replayed), nil
		}
		if err := a.participant(txCtx, governedAllocationTransaction{tx: tx}, copyGovernedAllocationRequest(request.Allocation), copyOperationClosure(outcome.Result.Closure())); err != nil {
			return GovernedAllocationComposedResult{}, newGovernedAllocationParticipantFailure(request.Allocation.OperationID, err, governedAllocationParticipantFailureAfterComposedAllocation)
		}
		return allocation.WithComposedReplay(outcome.Result, false), nil
	})
	return composedResultFrom(result, err)
}

// allocateWorkflow retains the baseline simple fused path, including its
// participant behavior on a distinct workflow replay. Composition has its own
// explicit workflow and receipt type because supplemental effects change the
// public operation identity.
func (a *FusedGovernedAllocator) allocateWorkflow(ctx dbos.Context, input fusedWorkflowInput) (fusedOperationResult, error) {
	request, err := input.allocationRequest()
	if err != nil {
		return resultFrom(OperationClosure{}, err)
	}
	closure, err := fusedtx.Run(ctx, a.system, func(txCtx context.Context, tx fusedtx.SQLTx) (OperationClosure, error) {
		closure, err := allocation.ReduceAllocation(txCtx, tx, request, input.Authority)
		if err != nil {
			// Typed reducer rejections remain durable DBOS outcomes and must not
			// trigger an integration side effect.
			return OperationClosure{}, err
		}
		if a.participant == nil {
			return closure, nil
		}
		if err := a.participant(txCtx, governedAllocationTransaction{tx: tx}, copyGovernedAllocationRequest(request), copyOperationClosure(closure)); err != nil {
			return OperationClosure{}, newGovernedAllocationParticipantFailure(request.OperationID, err, governedAllocationParticipantFailureAfterAllocation)
		}
		return closure, nil
	})
	return resultFrom(closure, err)
}
