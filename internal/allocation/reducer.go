package allocation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/internal/fusedtx"
	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// EnsureSchema installs only the normalized operation, explicit produced-row,
// and singleton-genesis relations required to reconstruct a governed closure.
// Tasks, assignment episodes, authorities, and journal rows remain in the
// established Provenance schema; this is not a shadow ledger.
func EnsureSchema(ctx context.Context, tx fusedtx.SQLTx) error {
	// The initial MVP revision persisted a second result-binding relation. The
	// canonical request plus the authoritative structural positions already own
	// that information, so retaining it risks contradictory closure ownership.
	// Dropping the obsolete relation is safe because reconstruct validates and
	// derives every binding from the remaining canonical records in this same
	// transaction.
	if _, err := tx.Exec(ctx, `DROP TABLE IF EXISTS governed_operation_result_bindings`); err != nil {
		return fmt.Errorf("remove obsolete governed result bindings: %w", err)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS governed_allocation_operations (
			operation_id TEXT PRIMARY KEY REFERENCES journal_operations(operation_id),
			anchor_journal_id INTEGER NOT NULL UNIQUE REFERENCES journal_operations(journal_id),
			request_kind INTEGER NOT NULL,
			actor_id TEXT NOT NULL REFERENCES agents(id),
			canonical_request BLOB NOT NULL,
			canonical_digest BLOB NOT NULL,
			child_count INTEGER NOT NULL,
			CHECK (request_kind IN (1,2)),
			CHECK (child_count >= 1 AND child_count <= 128),
			CHECK (length(canonical_digest) = 32)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS governed_operation_effect_rows (
			anchor_journal_id INTEGER NOT NULL REFERENCES governed_allocation_operations(anchor_journal_id),
			effect_ordinal INTEGER NOT NULL,
			subordinal INTEGER NOT NULL,
			produced_journal_id INTEGER NOT NULL UNIQUE REFERENCES journal(journal_id),
			PRIMARY KEY (anchor_journal_id, effect_ordinal, subordinal),
			CHECK (effect_ordinal >= 0),
			CHECK (subordinal IN (0,1))
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS governed_allocation_genesis (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			operation_id TEXT NOT NULL UNIQUE REFERENCES governed_allocation_operations(operation_id)
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS governed_composed_supplement_owners (
			supplement_operation_id TEXT PRIMARY KEY,
			governed_operation_id TEXT NOT NULL UNIQUE REFERENCES governed_allocation_operations(operation_id)
		) STRICT, WITHOUT ROWID`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("ensure governed-allocation schema: %w", err)
		}
	}
	return nil
}

// ReduceGenesis applies or reconstructs the singleton root allocation. It owns
// no transaction lifecycle; callers must run it in a BEGIN IMMEDIATE scope or a
// DBOS RunAsTransaction callback.
func ReduceGenesis(ctx context.Context, tx fusedtx.SQLTx, request RootGenesisRequest) (OperationClosure, error) {
	canonical, digest, err := CanonicalizeGenesis(request)
	if err != nil {
		return OperationClosure{}, err
	}
	if existing, found, err := lookupOperation(ctx, tx, request.OperationID); err != nil {
		return OperationClosure{}, err
	} else if found {
		return existingOutcome(ctx, tx, existing, RequestKindGenesis, canonical, digest, request.OperationID)
	}
	if exists, err := operationIDAlreadyUsed(ctx, tx, request.OperationID); err != nil {
		return OperationClosure{}, err
	} else if exists {
		return OperationClosure{}, conflictError(request.OperationID, "operation identity is already used by a non-governed operation")
	}
	if exists, err := genesisAlreadyExists(ctx, tx); err != nil {
		return OperationClosure{}, err
	} else if exists {
		return OperationClosure{}, NewError(ErrorGenesis, request.OperationID, "governed genesis reducer", "a governed root already exists", "a second root would create a second authority origin", "retry the original genesis OperationID and canonical input", nil)
	}
	if occupied, err := journalAlreadyInitialized(ctx, tx); err != nil {
		return OperationClosure{}, err
	} else if occupied {
		return OperationClosure{}, NewError(ErrorGenesis, request.OperationID, "governed genesis reducer", "the existing V2 journal already contains an operation", "a root can be created only for a fresh governed V2 journal", "migrate or use the established authority path; do not create a second genesis", nil)
	}
	if err := validateActorsAndCollisions(ctx, tx, request.OperationID, request.ActorID, []ChildSpec{request.Root}); err != nil {
		return OperationClosure{}, err
	}

	anchor, err := insertOperation(ctx, tx, RequestKindGenesis, request.OperationID, request.ActorID, request.Command, 0, canonical, digest, 1)
	if err != nil {
		return OperationClosure{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO governed_allocation_genesis (singleton, operation_id) VALUES (1, ?1)`, string(request.OperationID)); err != nil {
		return OperationClosure{}, classifyWriteError(request.OperationID, "insert singleton governed genesis", err)
	}
	if err := insertChild(ctx, tx, anchor, 0, request.ActorID, request.Root, ""); err != nil {
		return OperationClosure{}, err
	}
	return reconstruct(ctx, tx, request.OperationID)
}

// ComposedOutcome distinguishes a newly inserted allocation from an exact
// external retry. The SQLite composition coordinator uses Replayed to reconstruct
// supplemental result bindings without re-entering the canonical journal reducer.
type ComposedOutcome struct {
	Closure  OperationClosure
	Replayed bool
}

// ReduceAllocation applies or reconstructs the baseline governed-allocation
// receipt. Its canonical bytes intentionally remain distinct from CompositionV1
// so existing simple allocations replay through the exact same identity and
// fused workflow input shape they used before composition was introduced.
func ReduceAllocation(ctx context.Context, tx fusedtx.SQLTx, request GovernedAllocationRequest, expectedAuthority journal.JournalID) (OperationClosure, error) {
	canonical, digest, err := CanonicalizeAllocation(request)
	if err != nil {
		return OperationClosure{}, err
	}
	outcome, err := reduceAllocationCanonical(ctx, tx, request, expectedAuthority, canonical, digest, allocationReceiptSimple)
	return outcome.Closure, err
}

// ReduceComposedAllocation applies or reconstructs the governed allocation
// receipt whose identity contains the full ordered supplemental effect list.
// It deliberately stops after inserting/reconstructing allocation rows; the
// caller owns reducing the supplemental canonical journal operation through the
// same enclosing transaction, then invokes any fused participant last.
func ReduceComposedAllocation(ctx context.Context, tx fusedtx.SQLTx, request ComposedRequest, expectedAuthority journal.JournalID) (ComposedOutcome, error) {
	canonical, digest, err := CanonicalizeComposed(request)
	if err != nil {
		return ComposedOutcome{}, err
	}
	return reduceAllocationCanonical(ctx, tx, request.Allocation, expectedAuthority, canonical, digest, allocationReceiptComposed)
}

// VerifyGenesisReceipt reconstructs a genesis result from an existing receipt
// without admitting or writing a missing operation. It is used after DBOS
// success decoding so the durable workflow output is never authoritative over
// SQLite.
func VerifyGenesisReceipt(ctx context.Context, tx fusedtx.SQLTx, request RootGenesisRequest) (OperationClosure, error) {
	canonical, digest, err := CanonicalizeGenesis(request)
	if err != nil {
		return OperationClosure{}, err
	}
	stored, found, err := lookupOperation(ctx, tx, request.OperationID)
	if err != nil {
		return OperationClosure{}, err
	}
	if !found {
		return OperationClosure{}, corruption(request.OperationID, "the authoritative genesis receipt is absent")
	}
	if err := validateExistingInput(stored, RequestKindGenesis, canonical, digest, request.OperationID); err != nil {
		return OperationClosure{}, err
	}
	return reconstruct(ctx, tx, request.OperationID)
}

// VerifyAllocationReceipt is the read-only counterpart of allocation replay.
func VerifyAllocationReceipt(ctx context.Context, tx fusedtx.SQLTx, request GovernedAllocationRequest, expectedAuthority journal.JournalID, composed bool) (OperationClosure, error) {
	var canonical []byte
	var digest [sha256.Size]byte
	var err error
	if composed {
		return OperationClosure{}, NewError(ErrorCorruption, request.OperationID, "authoritative allocation reconstruction", "a composed request must be verified with VerifyComposedAllocationReceipt", "no result was returned", "use the composed receipt verifier", nil)
	}
	canonical, digest, err = CanonicalizeAllocation(request)
	if err != nil {
		return OperationClosure{}, err
	}
	stored, found, err := lookupOperation(ctx, tx, request.OperationID)
	if err != nil {
		return OperationClosure{}, err
	}
	if !found {
		return OperationClosure{}, corruption(request.OperationID, "the authoritative allocation receipt is absent")
	}
	if err := validateExistingInput(stored, RequestKindAllocation, canonical, digest, request.OperationID); err != nil {
		return OperationClosure{}, err
	}
	if err := validateReplayAuthority(ctx, tx, stored, request.OperationID, request.ParentAssignmentID, expectedAuthority); err != nil {
		return OperationClosure{}, err
	}
	return reconstruct(ctx, tx, request.OperationID)
}

// VerifyComposedAllocationReceipt validates the composed owner marker and
// immutable request/authority before reconstructing the allocation closure.
func VerifyComposedAllocationReceipt(ctx context.Context, tx fusedtx.SQLTx, request ComposedRequest, expectedAuthority journal.JournalID) (OperationClosure, error) {
	canonical, digest, err := CanonicalizeComposed(request)
	if err != nil {
		return OperationClosure{}, err
	}
	stored, found, err := lookupOperation(ctx, tx, request.Allocation.OperationID)
	if err != nil {
		return OperationClosure{}, err
	}
	if !found {
		return OperationClosure{}, corruption(request.Allocation.OperationID, "the authoritative composed allocation receipt is absent")
	}
	if err := validateExistingInput(stored, RequestKindAllocation, canonical, digest, request.Allocation.OperationID); err != nil {
		return OperationClosure{}, err
	}
	if err := validateComposedSupplementOwner(ctx, tx, request.Allocation.OperationID); err != nil {
		return OperationClosure{}, err
	}
	if err := validateReplayAuthority(ctx, tx, stored, request.Allocation.OperationID, request.Allocation.ParentAssignmentID, expectedAuthority); err != nil {
		return OperationClosure{}, err
	}
	return reconstruct(ctx, tx, request.Allocation.OperationID)
}

// ProveCanonicalConflict authenticates a conflict solely from the governed
// SQLite receipt. submitted must already be the canonical request bytes for the
// statically selected workflow path. A malformed, generic, or exact receipt is
// corruption rather than evidence of conflict.
func ProveCanonicalConflict(ctx context.Context, tx fusedtx.SQLTx, operationID journal.OperationID, submitted []byte, verifyComposed func(ComposedRequest, journal.JournalID) error) error {
	stored, found, err := lookupOperation(ctx, tx, operationID)
	if err != nil {
		return corruption(operationID, "the authoritative governed receipt could not be loaded: "+err.Error())
	}
	if !found {
		return corruption(operationID, "no authoritative governed receipt exists for the reported conflict")
	}
	calculated := sha256.Sum256(stored.canonical)
	if !bytes.Equal(calculated[:], stored.digest) {
		return corruption(operationID, "canonical request digest does not match its persisted bytes")
	}
	var roundTrip []byte
	var composedReceipt *ComposedRequest
	var allocationParent journal.AssignmentID
	switch stored.kind {
	case RequestKindGenesis:
		request, decodeErr := DecodeGenesisRequest(stored.canonical)
		if decodeErr != nil || request.OperationID != operationID {
			return corruption(operationID, "the stored genesis receipt is malformed or embeds a different operation identity")
		}
		roundTrip, _, err = CanonicalizeGenesis(request)
	case RequestKindAllocation:
		request, decodeErr := DecodeAllocationRequest(stored.canonical)
		if decodeErr == nil {
			if request.OperationID != operationID {
				return corruption(operationID, "the stored allocation receipt embeds a different operation identity")
			}
			roundTrip, _, err = CanonicalizeAllocation(request)
			allocationParent = request.ParentAssignmentID
		} else {
			composed, composedErr := DecodeComposedRequest(stored.canonical)
			if composedErr != nil || composed.Allocation.OperationID != operationID {
				return corruption(operationID, "the stored allocation receipt is neither a valid simple nor composed canonical request")
			}
			roundTrip, _, err = CanonicalizeComposed(composed)
			composedReceipt = &composed
			allocationParent = composed.Allocation.ParentAssignmentID
		}
	default:
		return corruption(operationID, "the stored governed receipt has an unknown request kind")
	}
	if err != nil || !bytes.Equal(roundTrip, stored.canonical) {
		return corruption(operationID, "the stored governed receipt does not survive canonical round-trip validation")
	}
	if _, err := reconstruct(ctx, tx, operationID); err != nil {
		return corruption(operationID, "the stored governed receipt cannot be authoritatively reconstructed: "+err.Error())
	}
	if stored.kind == RequestKindAllocation {
		if !stored.hasAuthority {
			return corruption(operationID, "the allocation operation has no exact authority anchor")
		}
		if err := validateReplayAuthority(ctx, tx, stored, operationID, allocationParent, stored.authority); err != nil {
			return corruption(operationID, "the allocation authority receipt is not owned by its canonical parent: "+err.Error())
		}
	}
	if composedReceipt != nil {
		if err := validateComposedSupplementOwner(ctx, tx, operationID); err != nil {
			return err
		}
		if verifyComposed == nil {
			return corruption(operationID, "the composed supplemental receipt verifier is unavailable")
		}
		if err := verifyComposed(*composedReceipt, stored.authority); err != nil {
			return corruption(operationID, "the composed supplemental receipt is incomplete or corrupt: "+err.Error())
		}
	}
	submittedDigest := sha256.Sum256(submitted)
	if bytes.Equal(submitted, stored.canonical) || bytes.Equal(submittedDigest[:], stored.digest) {
		return corruption(operationID, "the submitted request exactly matches the committed governed receipt")
	}
	return conflictError(operationID, "the OperationID is committed to a different authoritative canonical governed request")
}

type allocationReceiptMode uint8

const (
	allocationReceiptSimple allocationReceiptMode = iota
	allocationReceiptComposed
)

// reduceAllocationCanonical is the single admission, replay, and write pipeline
// for both receipt encodings. Callers own only their encoding-specific
// canonicalization; mode controls the composed receipt's durable owner marker.
func reduceAllocationCanonical(ctx context.Context, tx fusedtx.SQLTx, request GovernedAllocationRequest, expectedAuthority journal.JournalID, canonical []byte, digest [sha256.Size]byte, mode allocationReceiptMode) (ComposedOutcome, error) {
	// Authority is part of replay identity, not merely a write-time permission.
	// Reject its zero value before looking up an existing operation so a forged
	// zero-authority Session cannot attach to a prior closure.
	if expectedAuthority == 0 {
		return ComposedOutcome{}, NewError(ErrorAuthority, request.OperationID, "governed allocation authority validation", "the allocation has no bound parent start authority", "nothing was written; governed allocation cannot proceed without an exact parent authority", "allocate through Session.AllocateGoverned or pass the parent assignment start JournalID through the fused capability", nil)
	}
	if existing, found, err := lookupOperation(ctx, tx, request.OperationID); err != nil {
		return ComposedOutcome{}, err
	} else if found {
		if err := validateExistingInput(existing, RequestKindAllocation, canonical, digest, request.OperationID); err != nil {
			return ComposedOutcome{}, err
		}
		// Only an exact composed canonical receipt is required to have the owner
		// marker. A simple allocation or genesis reusing this operation identity
		// is an ordinary immutable-input conflict, not evidence of corruption.
		if mode == allocationReceiptComposed {
			if err := validateComposedSupplementOwner(ctx, tx, request.OperationID); err != nil {
				return ComposedOutcome{}, err
			}
		}
		// An exact replay is allowed after revocation, but only after its persisted
		// request bytes and exact parent-start authority have been proved. Do not
		// run the current active-chain check here: it would make an immutable
		// previously committed closure depend on later revocation state.
		if err := validateReplayAuthority(ctx, tx, existing, request.OperationID, request.ParentAssignmentID, expectedAuthority); err != nil {
			return ComposedOutcome{}, err
		}
		closure, err := reconstruct(ctx, tx, request.OperationID)
		if err != nil {
			return ComposedOutcome{}, err
		}
		return ComposedOutcome{Closure: closure, Replayed: true}, nil
	}
	if exists, err := operationIDAlreadyUsed(ctx, tx, request.OperationID); err != nil {
		return ComposedOutcome{}, err
	} else if exists {
		return ComposedOutcome{}, conflictError(request.OperationID, "operation identity is already used by a non-governed operation")
	}
	if err := validateActiveParentChain(ctx, tx, request.OperationID, request.ParentAssignmentID, expectedAuthority); err != nil {
		return ComposedOutcome{}, err
	}
	if err := validateActorsAndCollisions(ctx, tx, request.OperationID, request.ActorID, request.Children); err != nil {
		return ComposedOutcome{}, err
	}

	anchor, err := insertOperation(ctx, tx, RequestKindAllocation, request.OperationID, request.ActorID, request.Command, expectedAuthority, canonical, digest, len(request.Children))
	if err != nil {
		return ComposedOutcome{}, err
	}
	if mode == allocationReceiptComposed {
		internalOperation := journal.NewGovernedAllocationSupplementOperationID(request.OperationID).OperationID()
		if _, err := tx.Exec(ctx, `INSERT INTO governed_composed_supplement_owners (supplement_operation_id,governed_operation_id) VALUES (?1,?2)`, string(internalOperation), string(request.OperationID)); err != nil {
			return ComposedOutcome{}, fmt.Errorf("persist composed supplemental operation ownership for %q: %w", request.OperationID, err)
		}
	}
	for ordinal, child := range request.Children {
		if err := insertChild(ctx, tx, anchor, ordinal, request.ActorID, child, request.ParentAssignmentID); err != nil {
			return ComposedOutcome{}, err
		}
	}
	closure, err := reconstruct(ctx, tx, request.OperationID)
	if err != nil {
		return ComposedOutcome{}, err
	}
	return ComposedOutcome{Closure: closure}, nil
}

func validateComposedSupplementOwner(ctx context.Context, tx fusedtx.SQLTx, operationID journal.OperationID) error {
	wantSupplement := journal.NewGovernedAllocationSupplementOperationID(operationID).OperationID()
	var storedOwner string
	err := tx.QueryRow(ctx, `SELECT governed_operation_id FROM governed_composed_supplement_owners WHERE supplement_operation_id=?1`, string(wantSupplement)).Scan(&storedOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return NewError(ErrorCorruption, operationID, "composed supplemental ownership replay", "the composed allocation receipt has no durable supplemental-operation owner linkage", "the composed result cannot be trusted", "restore the governed allocation, owner linkage, and supplemental operation from one consistent backup; do not backfill the marker", nil)
	}
	if err != nil {
		return fmt.Errorf("load composed supplemental operation ownership for %q: %w", operationID, err)
	}
	if storedOwner != string(operationID) {
		return NewError(ErrorCorruption, operationID, "composed supplemental ownership replay", fmt.Sprintf("the supplemental operation is linked to governed operation %q", storedOwner), "the composed result cannot be trusted", "restore the governed allocation, owner linkage, and supplemental operation from one consistent backup; do not infer ownership from the reserved prefix", nil)
	}
	return nil
}

type storedOperation struct {
	anchor       journal.JournalID
	kind         RequestKind
	canonical    []byte
	digest       []byte
	childCount   int
	authority    journal.JournalID
	hasAuthority bool
}

func lookupOperation(ctx context.Context, tx fusedtx.SQLTx, operationID journal.OperationID) (storedOperation, bool, error) {
	var stored storedOperation
	var anchor int64
	var authority sql.NullInt64
	err := tx.QueryRow(ctx, `SELECT governed.anchor_journal_id, governed.request_kind, governed.canonical_request, governed.canonical_digest, governed.child_count, operation.authority_journal_id
		FROM governed_allocation_operations governed
		JOIN journal_operations operation ON operation.journal_id = governed.anchor_journal_id
		WHERE governed.operation_id = ?1`, string(operationID)).Scan(&anchor, &stored.kind, &stored.canonical, &stored.digest, &stored.childCount, &authority)
	if errors.Is(err, sql.ErrNoRows) {
		return storedOperation{}, false, nil
	}
	if err != nil {
		return storedOperation{}, false, fmt.Errorf("load governed operation %q: %w", operationID, err)
	}
	stored.anchor = journal.JournalID(anchor)
	stored.canonical = append([]byte(nil), stored.canonical...)
	stored.digest = append([]byte(nil), stored.digest...)
	if authority.Valid {
		stored.authority = journal.JournalID(authority.Int64)
		stored.hasAuthority = true
	}
	return stored, true, nil
}

func existingOutcome(ctx context.Context, tx fusedtx.SQLTx, stored storedOperation, wantKind RequestKind, canonical []byte, digest [sha256.Size]byte, operationID journal.OperationID) (OperationClosure, error) {
	if err := validateExistingInput(stored, wantKind, canonical, digest, operationID); err != nil {
		return OperationClosure{}, err
	}
	return reconstruct(ctx, tx, operationID)
}

func validateExistingInput(stored storedOperation, wantKind RequestKind, canonical []byte, digest [sha256.Size]byte, operationID journal.OperationID) error {
	if stored.kind != wantKind || !bytes.Equal(stored.canonical, canonical) || !bytes.Equal(stored.digest, digest[:]) {
		return conflictError(operationID, "the submitted kind or canonical request bytes differ from the original governed operation")
	}
	return nil
}

// validateReplayAuthority proves a retry's supplied Session authority is the
// parent assignment's persisted start authority. Unlike validateActiveParentChain
// it deliberately does not inspect current end transitions: a closure that was
// committed under an active parent remains replayable after a later revocation.
func validateReplayAuthority(ctx context.Context, tx fusedtx.SQLTx, stored storedOperation, operationID journal.OperationID, parent journal.AssignmentID, expectedAuthority journal.JournalID) error {
	var startJournalID int64
	err := tx.QueryRow(ctx, `SELECT started.journal_id
		FROM journal_authority_assignment_episodes episode
		JOIN journal_authority_assignment_transitions started
		  ON started.assignment_id = episode.assignment_id AND started.transition_id = 0
		JOIN journal_authorities start_authority ON start_authority.journal_id = started.journal_id
		JOIN journal start_journal ON start_journal.journal_id = started.journal_id AND start_journal.kind_id = 2
		WHERE episode.assignment_id = ?1 AND episode.slot_id = 0`, string(parent)).Scan(&startJournalID)
	if errors.Is(err, sql.ErrNoRows) {
		return NewError(ErrorAuthority, operationID, "Session.AllocateGoverned replay authority validation", fmt.Sprintf("parent assignment %q does not exist or has no linked start authority", parent), "the prior closure was not returned and nothing was written", "retry through a Session bound to the original parent assignment start authority", nil)
	}
	if err != nil {
		return fmt.Errorf("read persisted parent start authority for replay %q: %w", parent, err)
	}
	if journal.JournalID(startJournalID) != expectedAuthority {
		return NewError(ErrorAuthority, operationID, "Session.AllocateGoverned replay authority validation", "the supplied Session authority does not identify the persisted start authority of the request parent", "the prior closure was not returned and nothing was written", "retry through a Session bound to the exact request parent assignment", nil)
	}
	if !stored.hasAuthority || stored.authority != expectedAuthority {
		return corruption(operationID, "the operation receipt authority does not match the persisted request parent start authority")
	}
	return nil
}

func operationIDAlreadyUsed(ctx context.Context, tx fusedtx.SQLTx, operationID journal.OperationID) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM journal_operations WHERE operation_id = ?1)`, string(operationID)).Scan(&exists); err != nil {
		return false, fmt.Errorf("check existing journal operation %q: %w", operationID, err)
	}
	return exists, nil
}

func genesisAlreadyExists(ctx context.Context, tx fusedtx.SQLTx) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM governed_allocation_genesis WHERE singleton = 1)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("check governed genesis state: %w", err)
	}
	return exists, nil
}

func journalAlreadyInitialized(ctx context.Context, tx fusedtx.SQLTx) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM journal_operations LIMIT 1)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("check fresh V2 journal before governed genesis: %w", err)
	}
	return exists, nil
}

func validateActorsAndCollisions(ctx context.Context, tx fusedtx.SQLTx, operationID journal.OperationID, actor ptypes.ActorID, children []ChildSpec) error {
	if err := requireActor(ctx, tx, operationID, actor, "request actor"); err != nil {
		return err
	}
	for ordinal, child := range children {
		if err := requireActor(ctx, tx, operationID, child.Occupant, fmt.Sprintf("child %d occupant", ordinal)); err != nil {
			return err
		}
		var taskExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id = ?1)`, child.TaskID.String()).Scan(&taskExists); err != nil {
			return fmt.Errorf("check child %d task collision: %w", ordinal, err)
		}
		if taskExists {
			return NewError(ErrorCollision, operationID, fmt.Sprintf("child %d task allocation", ordinal), "the caller TaskID already names an existing task", "the complete allocation is rejected without adopting or changing that task", "supply a fresh caller TaskID", nil)
		}
		var assignmentExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM journal_authority_assignment_episodes WHERE assignment_id = ?1)`, string(child.AssignmentID)).Scan(&assignmentExists); err != nil {
			return fmt.Errorf("check child %d assignment collision: %w", ordinal, err)
		}
		if assignmentExists {
			return NewError(ErrorCollision, operationID, fmt.Sprintf("child %d assignment allocation", ordinal), "the caller AssignmentID already names an existing assignment", "the complete allocation is rejected without changing that assignment", "supply a fresh caller AssignmentID", nil)
		}
	}
	return nil
}

func requireActor(ctx context.Context, tx fusedtx.SQLTx, operationID journal.OperationID, actor ptypes.ActorID, field string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE id = ?1)`, actor.String()).Scan(&exists); err != nil {
		return fmt.Errorf("check %s registration: %w", field, err)
	}
	if !exists {
		return NewError(ErrorValidation, operationID, "governed allocation actor validation", fmt.Sprintf("%s %q is not registered", field, actor.String()), "nothing was written", "register the actor before using it as an operation actor or assignment occupant", nil)
	}
	return nil
}

// validateActiveParentChain authorizes the requested parent and every ancestor
// in the allocation's write transaction. A valid direct parent is insufficient:
// a revoked middle ancestor also revokes all of its descendants. The chain walk
// detects cycles and requires each assignment start to be backed by a journal
// authority row before any governed row is inserted.
func validateActiveParentChain(ctx context.Context, tx fusedtx.SQLTx, operationID journal.OperationID, start journal.AssignmentID, expectedAuthority journal.JournalID) error {
	visited := map[journal.AssignmentID]struct{}{}
	current := start
	direct := true
	for {
		if _, found := visited[current]; found {
			return NewError(ErrorCorruption, operationID, "governed parent-chain validation", fmt.Sprintf("assignment parent chain revisits %q", current), "authorization fails closed and nothing was written", "repair the cyclic parent_assignment_id rows from a consistent backup", nil)
		}
		visited[current] = struct{}{}

		var (
			parent         sql.NullString
			startJournalID int64
			ended          bool
		)
		err := tx.QueryRow(ctx, `SELECT episode.parent_assignment_id, started.journal_id,
			EXISTS(SELECT 1 FROM journal_authority_assignment_transitions ended
			       WHERE ended.assignment_id = episode.assignment_id AND ended.transition_id = 1)
			FROM journal_authority_assignment_episodes episode
			JOIN journal_authority_assignment_transitions started
			  ON started.assignment_id = episode.assignment_id AND started.transition_id = 0
			JOIN journal_authorities start_authority ON start_authority.journal_id = started.journal_id
			JOIN journal start_journal ON start_journal.journal_id = started.journal_id AND start_journal.kind_id = 2
			WHERE episode.assignment_id = ?1 AND episode.slot_id = 0`, string(current)).Scan(&parent, &startJournalID, &ended)
		if errors.Is(err, sql.ErrNoRows) {
			if direct {
				return NewError(ErrorAuthority, operationID, "Session.AllocateGoverned", fmt.Sprintf("parent assignment %q does not exist or has no linked start authority", current), "nothing was written", "bind the Session to the exact active parent assignment authority", nil)
			}
			return NewError(ErrorCorruption, operationID, "governed parent-chain validation", fmt.Sprintf("ancestor assignment %q is absent or lacks a linked start authority", current), "authorization fails closed and nothing was written", "restore the assignment lineage from a consistent backup", err)
		}
		if err != nil {
			return fmt.Errorf("read parent chain at assignment %q: %w", current, err)
		}
		if ended {
			return NewError(ErrorRevoked, operationID, "governed parent-chain validation", fmt.Sprintf("assignment %q is ended or revoked", current), "nothing was written", "choose a descendant of an active root-to-parent chain", nil)
		}
		if direct && journal.JournalID(startJournalID) != expectedAuthority {
			return NewError(ErrorAuthority, operationID, "Session.AllocateGoverned", "the Session authority does not identify the request's exact parent assignment", "nothing was written; a Session cannot use one assignment to allocate under another", "bind the Session to the active parent assignment authority", nil)
		}
		if !parent.Valid {
			return nil
		}
		current = journal.AssignmentID(parent.String)
		direct = false
	}
}

func insertOperation(ctx context.Context, tx fusedtx.SQLTx, kind RequestKind, operationID journal.OperationID, actor ptypes.ActorID, command string, authority journal.JournalID, canonical []byte, digest [sha256.Size]byte, childCount int) (journal.JournalID, error) {
	now := time.Now().UTC().UnixNano()
	var anchor int64
	if err := tx.QueryRow(ctx, `INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id)
		VALUES (0, ?1, ?2, NULL) RETURNING journal_id`, actor.String(), now).Scan(&anchor); err != nil {
		return 0, fmt.Errorf("insert governed operation journal anchor: %w", err)
	}
	var authorityArg any
	if kind == RequestKindAllocation {
		authorityArg = int64(authority)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_operations
		(journal_id, operation_id, authority_journal_id, command_digest, mutation_digest, mutation_encoding_version, canonical_mutation)
		VALUES (?1, ?2, ?3, ?4, ?5, NULL, NULL)`, anchor, string(operationID), authorityArg, []byte(command), digest[:]); err != nil {
		return 0, classifyWriteError(operationID, "insert journal operation identity", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO governed_allocation_operations
		(operation_id, anchor_journal_id, request_kind, actor_id, canonical_request, canonical_digest, child_count)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)`, string(operationID), anchor, int(kind), actor.String(), canonical, digest[:], childCount); err != nil {
		return 0, classifyWriteError(operationID, "insert governed operation canonical receipt", err)
	}
	return journal.JournalID(anchor), nil
}

func insertChild(ctx context.Context, tx fusedtx.SQLTx, anchor journal.JournalID, ordinal int, actor ptypes.ActorID, child ChildSpec, parent journal.AssignmentID) error {
	now := time.Now().UTC().UnixNano()
	var taskRow int64
	if err := tx.QueryRow(ctx, `INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id)
		VALUES (1, NULL, ?1, ?2) RETURNING journal_id`, now, int64(anchor)).Scan(&taskRow); err != nil {
		return fmt.Errorf("insert governed child %d task journal row: %w", ordinal, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tasks
		(id, namespace, title, description, status_id, priority_id, type_id, phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason, last_journal_id)
		VALUES (?1, ?2, ?3, ?4, 0, ?5, ?6, ?7, NULL, '', ?8, ?8, NULL, '', ?9)`,
		child.TaskID.String(), child.TaskID.Namespace, child.Title, child.Description, int(child.Priority), int(child.Type), int(child.Phase), now, taskRow); err != nil {
		return fmt.Errorf("insert governed child %d task: %w", ordinal, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_task_events (journal_id, task_id, event_kind, payload)
		VALUES (?1, ?2, 'provenance.task.created', '{}')`, taskRow, child.TaskID.String()); err != nil {
		return fmt.Errorf("insert governed child %d created event: %w", ordinal, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)`, child.TaskID.String(), actor.String(), taskRow); err != nil {
		return fmt.Errorf("attribute governed child %d creator: %w", ordinal, err)
	}

	var assignmentRow int64
	if err := tx.QueryRow(ctx, `INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id)
		VALUES (2, NULL, ?1, ?2) RETURNING journal_id`, now, int64(anchor)).Scan(&assignmentRow); err != nil {
		return fmt.Errorf("insert governed child %d assignment journal row: %w", ordinal, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_authorities (journal_id, authority_kind_id, operation_authority_id)
		VALUES (?1, 1, ?2)`, assignmentRow, "governed-assignment/"+string(child.AssignmentID)); err != nil {
		return fmt.Errorf("insert governed child %d assignment authority: %w", ordinal, err)
	}
	var parentArg any
	if parent != "" {
		parentArg = string(parent)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_authority_assignment_episodes
		(assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id, parent_assignment_id)
		VALUES (?1, ?2, 0, ?3, NULL, ?4)`, string(child.AssignmentID), child.TaskID.String(), child.Occupant.String(), parentArg); err != nil {
		return fmt.Errorf("insert governed child %d assignment episode: %w", ordinal, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_authority_assignment_transitions (journal_id, assignment_id, transition_id)
		VALUES (?1, ?2, 0)`, assignmentRow, string(child.AssignmentID)); err != nil {
		return fmt.Errorf("insert governed child %d assignment transition: %w", ordinal, err)
	}
	if _, err := tx.Exec(ctx, `INSERT OR IGNORE INTO task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)`, child.TaskID.String(), child.Occupant.String(), assignmentRow); err != nil {
		return fmt.Errorf("attribute governed child %d occupant: %w", ordinal, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET owner_id = ?1, last_journal_id = ?2 WHERE id = ?3`, child.Occupant.String(), assignmentRow, child.TaskID.String()); err != nil {
		return fmt.Errorf("project governed child %d owner: %w", ordinal, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO governed_operation_effect_rows (anchor_journal_id, effect_ordinal, subordinal, produced_journal_id)
		VALUES (?1, ?2, 0, ?3), (?1, ?2, 1, ?4)`, int64(anchor), ordinal, taskRow, assignmentRow); err != nil {
		return fmt.Errorf("persist governed child %d row ownership: %w", ordinal, err)
	}
	return nil
}

func reconstruct(ctx context.Context, tx fusedtx.SQLTx, operationID journal.OperationID) (OperationClosure, error) {
	stored, found, err := lookupOperation(ctx, tx, operationID)
	if err != nil {
		return OperationClosure{}, err
	}
	if !found {
		return OperationClosure{}, NewError(ErrorCorruption, operationID, "governed closure reconstruction", "the operation receipt is absent", "no closure can be safely reconstructed", "restore governed operation rows from a consistent backup", nil)
	}
	calculated := sha256.Sum256(stored.canonical)
	if !bytes.Equal(calculated[:], stored.digest) {
		return OperationClosure{}, corruption(operationID, "canonical request digest does not match its persisted bytes")
	}
	var (
		kind       RequestKind
		actor      ptypes.ActorID
		parent     journal.AssignmentID
		children   []ChildSpec
		isComposed bool
	)
	if stored.kind == RequestKindAllocation {
		// The baseline allocation receipt predates composition and remains the
		// simple API's identity. Try it first so a persisted simple allocation is
		// never reinterpreted as a composed request during reopen/replay.
		decodedKind, decodedOperation, decodedActor, decodedParent, decodedChildren, decodeErr := DecodeCanonical(stored.canonical)
		if decodeErr == nil {
			if decodedOperation != operationID || decodedKind != stored.kind {
				return OperationClosure{}, corruption(operationID, "canonical request identity or kind disagrees with its receipt")
			}
			kind, actor, parent, children = decodedKind, decodedActor, decodedParent, decodedChildren
		} else {
			composed, composedErr := DecodeComposedRequest(stored.canonical)
			if composedErr != nil {
				return OperationClosure{}, corruption(operationID, "allocation canonical request cannot be decoded as a simple or composed receipt: "+decodeErr.Error()+"; "+composedErr.Error())
			}
			if composed.Allocation.OperationID != operationID {
				return OperationClosure{}, corruption(operationID, "composed canonical request operation identity disagrees with its receipt")
			}
			kind = RequestKindAllocation
			actor = composed.Allocation.ActorID
			parent = composed.Allocation.ParentAssignmentID
			children = composed.Allocation.Children
			isComposed = true
		}
	} else {
		decodedKind, decodedOperation, decodedActor, decodedParent, decodedChildren, decodeErr := DecodeCanonical(stored.canonical)
		if decodeErr != nil {
			return OperationClosure{}, corruption(operationID, "canonical request cannot be decoded: "+decodeErr.Error())
		}
		if decodedOperation != operationID || decodedKind != stored.kind {
			return OperationClosure{}, corruption(operationID, "canonical request identity or kind disagrees with its receipt")
		}
		kind, actor, parent, children = decodedKind, decodedActor, decodedParent, decodedChildren
	}
	if len(children) == 0 || stored.childCount != len(children) {
		return OperationClosure{}, corruption(operationID, "canonical request child count disagrees with its receipt")
	}
	var anchorOperation, receiptActor, anchorActor string
	var anchorKind int
	var anchorProducer sql.NullInt64
	if err := tx.QueryRow(ctx, `SELECT operation.operation_id,governed.actor_id,journal.kind_id,COALESCE(journal.actor_id,''),journal.produced_by_operation_journal_id FROM journal_operations operation JOIN governed_allocation_operations governed ON governed.anchor_journal_id=operation.journal_id JOIN journal ON journal.journal_id=operation.journal_id WHERE operation.journal_id=?1`, int64(stored.anchor)).Scan(&anchorOperation, &receiptActor, &anchorKind, &anchorActor, &anchorProducer); err != nil || anchorOperation != string(operationID) || receiptActor != actor.String() || anchorKind != int(journal.JournalKindOperation) || anchorActor != actor.String() || anchorProducer.Valid {
		return OperationClosure{}, corruption(operationID, "operation receipt anchor does not identify the governed operation")
	}

	positions, err := loadProducedRows(ctx, tx, operationID, stored.anchor, len(children))
	if err != nil {
		return OperationClosure{}, err
	}
	bindings := make([]ChildBinding, len(children))
	for ordinal, child := range children {
		taskRow := positions[[2]int{ordinal, 0}]
		assignmentRow := positions[[2]int{ordinal, 1}]
		if err := validateProducedRows(ctx, tx, operationID, stored.anchor, ordinal, child, parent, taskRow, assignmentRow, isComposed); err != nil {
			return OperationClosure{}, err
		}
		bindings[ordinal] = ChildBinding{
			Ordinal:      ordinal,
			TaskID:       child.TaskID,
			AssignmentID: child.AssignmentID,
			Occupant:     child.Occupant,
			TaskRow: ProducedRow{
				OperationID: operationID, EffectOrdinal: ordinal, Subordinal: 0, JournalID: taskRow,
			},
			AssignmentRow: ProducedRow{
				OperationID: operationID, EffectOrdinal: ordinal, Subordinal: 1, JournalID: assignmentRow,
			},
		}
	}
	return NewClosure(operationID, kind, stored.anchor, bindings), nil
}

// loadProducedRows loads exactly the two structurally owned rows for each
// canonical child. It is the sole source of structural positions used during
// closure reconstruction; no parallel result-binding relation is trusted.
func loadProducedRows(ctx context.Context, tx fusedtx.SQLTx, operationID journal.OperationID, anchor journal.JournalID, childCount int) (map[[2]int]journal.JournalID, error) {
	positions := make(map[[2]int]journal.JournalID, childCount*2)
	rows, err := tx.Query(ctx, `SELECT effect_ordinal, subordinal, produced_journal_id
		FROM governed_operation_effect_rows WHERE anchor_journal_id = ?1 ORDER BY effect_ordinal, subordinal`, int64(anchor))
	if err != nil {
		return nil, fmt.Errorf("read governed closure row positions: %w", err)
	}
	for rows.Next() {
		var ordinal, subordinal int
		var produced int64
		if err := rows.Scan(&ordinal, &subordinal, &produced); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan governed closure row position: %w", err)
		}
		key := [2]int{ordinal, subordinal}
		if ordinal < 0 || ordinal >= childCount || (subordinal != 0 && subordinal != 1) || produced <= 0 || positions[key] != 0 {
			_ = rows.Close()
			return nil, corruption(operationID, "effect-row ownership contains an invalid, zero, or duplicate structural position")
		}
		positions[key] = journal.JournalID(produced)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate governed closure row positions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close governed closure row positions: %w", err)
	}
	if len(positions) != childCount*2 {
		return nil, corruption(operationID, "effect-row ownership count does not match the canonical child count")
	}
	for ordinal := 0; ordinal < childCount; ordinal++ {
		if positions[[2]int{ordinal, 0}] == 0 || positions[[2]int{ordinal, 1}] == 0 {
			return nil, corruption(operationID, fmt.Sprintf("effect-row ownership is missing task or assignment position for child %d", ordinal))
		}
	}
	return positions, nil
}

// validateProducedRows is the single closure-integrity path. It validates that
// the position rows name the canonical child and that both journal rows were
// produced by this operation anchor rather than merely looking structurally
// similar in another operation.
func validateProducedRows(ctx context.Context, tx fusedtx.SQLTx, operationID journal.OperationID, anchor journal.JournalID, ordinal int, child ChildSpec, parent journal.AssignmentID, taskRow, assignmentRow journal.JournalID, composed bool) error {
	var kind int
	var taskID, eventKind, payload string
	var taskActor sql.NullString
	var taskProducer int64
	var taskRecorded int64
	err := tx.QueryRow(ctx, `SELECT j.kind_id, j.actor_id, j.produced_by_operation_journal_id, j.recorded_at, event.task_id, event.event_kind, event.payload
		FROM journal j JOIN journal_task_events event ON event.journal_id = j.journal_id
		WHERE j.journal_id = ?1`, int64(taskRow)).Scan(&kind, &taskActor, &taskProducer, &taskRecorded, &taskID, &eventKind, &payload)
	if err != nil || kind != int(journal.JournalKindTaskEvent) || taskActor.Valid || taskProducer != int64(anchor) || taskID != child.TaskID.String() || eventKind != "provenance.task.created" || payload != "{}" {
		return corruption(operationID, fmt.Sprintf("task produced row at child %d does not name the expected operation-owned task event", ordinal))
	}
	var namespace, title, description, owner, notes, closeReason string
	var status, priority, taskType, phase int
	var createdAt, updatedAt, watermark int64
	var closedAt sql.NullInt64
	if err := tx.QueryRow(ctx, `SELECT namespace,title,description,status_id,priority_id,type_id,phase_id,COALESCE(owner_id,''),notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM tasks WHERE id=?1`, child.TaskID.String()).Scan(&namespace, &title, &description, &status, &priority, &taskType, &phase, &owner, &notes, &createdAt, &updatedAt, &closedAt, &closeReason, &watermark); err != nil || namespace != child.TaskID.Namespace || title != child.Title || description != child.Description || priority != int(child.Priority) || taskType != int(child.Type) || phase != int(child.Phase) || notes != "" || createdAt != taskRecorded || (!composed && (status != 0 || owner != child.Occupant.String() || updatedAt != taskRecorded || closedAt.Valid || closeReason != "" || watermark != int64(assignmentRow))) {
		return corruption(operationID, fmt.Sprintf("task metadata or projection at child %d disagrees with the canonical allocation receipt", ordinal))
	}
	var assignment, episodeTask, occupant string
	var storedParent sql.NullString
	var assignmentActor sql.NullString
	var assignmentProducer int64
	var authorityKind int
	var authorityOwner string
	var slot, transition int
	var predecessor sql.NullString
	err = tx.QueryRow(ctx, `SELECT episode.assignment_id, episode.task_id, episode.actor_id, episode.parent_assignment_id,
		j.actor_id,j.produced_by_operation_journal_id,authority.authority_kind_id,authority.operation_authority_id,
		episode.slot_id,episode.predecessor_assignment_id,transition.transition_id
		FROM journal j
		JOIN journal_authorities authority ON authority.journal_id = j.journal_id
		JOIN journal_authority_assignment_transitions transition ON transition.journal_id = j.journal_id AND transition.transition_id = 0
		JOIN journal_authority_assignment_episodes episode ON episode.assignment_id = transition.assignment_id
		WHERE j.journal_id = ?1 AND j.kind_id = 2`, int64(assignmentRow)).Scan(&assignment, &episodeTask, &occupant, &storedParent, &assignmentActor, &assignmentProducer, &authorityKind, &authorityOwner, &slot, &predecessor, &transition)
	if err != nil || assignmentActor.Valid || assignmentProducer != int64(anchor) || authorityKind != 1 || authorityOwner != "governed-assignment/"+string(child.AssignmentID) || slot != 0 || predecessor.Valid || transition != 0 || assignment != string(child.AssignmentID) || episodeTask != child.TaskID.String() || occupant != child.Occupant.String() || (parent == "") != !storedParent.Valid || (parent != "" && storedParent.String != string(parent)) {
		return corruption(operationID, fmt.Sprintf("assignment produced row at child %d does not name the expected operation-owned child episode", ordinal))
	}
	rows, err := tx.Query(ctx, `SELECT actor_id,first_journal_id FROM task_attributions WHERE task_id=?1 ORDER BY actor_id`, child.TaskID.String())
	if err != nil {
		return corruption(operationID, fmt.Sprintf("task attribution receipt at child %d cannot be loaded", ordinal))
	}
	want := map[string]int64{child.Occupant.String(): int64(assignmentRow)}
	var requestActor string
	if err := tx.QueryRow(ctx, `SELECT actor_id FROM governed_allocation_operations WHERE anchor_journal_id=?1`, int64(anchor)).Scan(&requestActor); err != nil {
		_ = rows.Close()
		return corruption(operationID, "the allocation request actor is absent")
	}
	want[requestActor] = int64(taskRow)
	seen := 0
	for rows.Next() {
		var actor string
		var first int64
		if rows.Scan(&actor, &first) != nil || want[actor] != first {
			_ = rows.Close()
			return corruption(operationID, fmt.Sprintf("task attribution receipt at child %d is foreign or has a forged watermark", ordinal))
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return corruption(operationID, fmt.Sprintf("task attribution receipt at child %d cannot be read", ordinal))
	}
	_ = rows.Close()
	if !composed && seen != len(want) {
		return corruption(operationID, fmt.Sprintf("task attribution receipt at child %d is incomplete", ordinal))
	}
	return nil
}

func conflictError(operationID journal.OperationID, why string) error {
	return NewError(ErrorConflict, operationID, "governed operation identity check", why, "nothing was written; exact retry is the only safe replay", "retry the original canonical request or choose a new OperationID for changed work", nil)
}

func corruption(operationID journal.OperationID, why string) error {
	return NewError(ErrorCorruption, operationID, "governed closure reconstruction", why, "the persisted closure is not trusted and no write was attempted", "restore governed operation, produced-row position, and journal rows from one consistent backup", nil)
}

func classifyWriteError(operationID journal.OperationID, where string, err error) error {
	return NewError(ErrorCollision, operationID, where, "a uniqueness or structural database constraint rejected the allocation", "the enclosing transaction rolls back all governed rows", "use fresh caller IDs and a new operation identity, or repair conflicting persistent state", err)
}
