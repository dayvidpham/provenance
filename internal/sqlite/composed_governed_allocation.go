package sqlite

// This file is the transaction-scoped composition seam between governed
// allocation and the canonical journal reducer. It deliberately accepts only
// fusedtx.SQLTx: callers cannot obtain a raw handle, start a second transaction,
// or bypass DBOS's transaction/checkpoint ownership.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/internal/allocation"
	"github.com/dayvidpham/provenance/internal/fusedtx"
	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

var errComposedFailureProofRollback = errors.New("rollback composed failure proof")

// ComposedGovernedAllocationOutcome keeps the replay decision internal to the
// transaction-owned composition path. The fused capability uses it to avoid
// rerunning a composed participant on an exact external retry.
type ComposedGovernedAllocationOutcome struct {
	Result   allocation.ComposedResult
	Replayed bool
}

// composedSupplementReferenceReader is the smallest read-only contract shared
// by the DBOS transaction and the fused admission preflight. The preflight can
// reject already-resolvable bad task references before DBOS creates a checkpoint;
// it never owns authorization or writes, so the transaction path repeats this
// validation against its authoritative snapshot.
type composedSupplementReferenceReader interface{ fusedtx.SQLReader }

type composedSupplementConnReader struct {
	conn *sql.Conn
}

func (reader composedSupplementConnReader) QueryRow(ctx context.Context, query string, args ...any) fusedtx.Row {
	return reader.conn.QueryRowContext(ctx, query, args...)
}

func (reader composedSupplementConnReader) Query(ctx context.Context, query string, args ...any) (fusedtx.Rows, error) {
	return reader.conn.QueryContext(ctx, query, args...)
}

// PreflightComposedGovernedAllocation performs the read-only portion of the
// composed reference fence before a fused caller enqueues DBOS work. It copies
// through the canonical request boundary first, then resolves only the current
// parent ancestry and caller-declared children. It intentionally makes no
// authority decision: the transaction-local reducer repeats the same reference
// fence and remains authoritative against concurrent revocation or lineage
// changes.
func (db *DB) PreflightComposedGovernedAllocation(ctx context.Context, request allocation.ComposedRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	operationID := request.Allocation.OperationID
	canonical, _, err := allocation.CanonicalizeComposed(request)
	if err != nil {
		return err
	}
	request, err = allocation.DecodeComposedRequest(canonical)
	if err != nil {
		return allocation.NewError(allocation.ErrorCorruption, operationID, "composed governed allocation read-only preflight", "the canonical composed request could not be copied into its immutable preflight form", "no DBOS workflow, allocation, journal, or participant write was started", "retry with a valid canonical composed request", err)
	}
	scope, err := db.bindScope(ctx, projectionTargetLive)
	if err != nil {
		return fmt.Errorf("PreflightComposedGovernedAllocation: lease SQLite connection for read-only reference validation: %w", err)
	}
	defer scope.release()
	return validateComposedSupplementReferences(scope.ctx, composedSupplementConnReader{conn: scope.conn}, request)
}

// ProveAbsentComposedGovernedAllocationFailure re-executes the composed reducer
// under pinned BEGIN IMMEDIATE ownership only after proving that the external
// operation has no governed receipt. The transaction is always rolled back: it
// is a failure-authentication proof, never a second execution or commit path.
func (db *DB) ProveAbsentComposedGovernedAllocationFailure(ctx context.Context, request allocation.ComposedRequest, authority journal.JournalID) (*allocation.Error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	scope, err := db.bindScope(ctx, projectionTargetLive)
	if err != nil {
		return nil, fmt.Errorf("prove absent composed governed failure: lease pinned SQLite connection: %w", err)
	}
	safeRelease := false
	defer func() {
		if safeRelease {
			scope.release()
		} else {
			scope.discard()
		}
	}()

	var (
		authoritative *allocation.Error
		nonDomain     error
	)
	err = runImmediateTransaction(scope.ctx, scope.conn, func() error {
		var present int
		lookupErr := scope.conn.QueryRowContext(scope.ctx, `SELECT 1 FROM governed_allocation_operations WHERE operation_id=?1`, string(request.Allocation.OperationID)).Scan(&present)
		if lookupErr == nil {
			return fmt.Errorf("external governed operation %q is present", request.Allocation.OperationID)
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return fmt.Errorf("prove external governed operation %q absent: %w", request.Allocation.OperationID, lookupErr)
		}

		_, reduceErr := ReduceComposedGovernedAllocationOutcome(scope.ctx, allocationSQLTx{conn: scope.conn}, request, authority)
		if reduceErr != nil {
			var governed *allocation.Error
			if errors.As(reduceErr, &governed) {
				authoritative = governed
			} else {
				nonDomain = reduceErr
			}
		}
		return errComposedFailureProofRollback
	})
	// The sentinel authenticates the proof only when it is the complete terminal
	// transaction result. errors.Is would also accept errors.Join(sentinel,
	// rollback/restore failure), which means SQLite did not cleanly discard the
	// proof transaction and the leased connection is not safe to authenticate.
	if err != errComposedFailureProofRollback {
		return nil, fmt.Errorf("prove absent composed governed failure transaction: %w", err)
	}
	safeRelease = true
	if nonDomain != nil {
		return nil, fmt.Errorf("prove absent composed governed failure reducer returned a non-domain error: %w", nonDomain)
	}
	return authoritative, nil
}

// ReduceComposedGovernedAllocation is the one transaction-owned production
// composition path. It validates references before allocation writes, inserts
// the governed children, then reduces the approved supplemental effects through
// the canonical journal representation in the exact same transaction. An exact
// replay reconstructs both receipts without folding effects again.
func ReduceComposedGovernedAllocation(ctx context.Context, tx fusedtx.SQLTx, request allocation.ComposedRequest, authority journal.JournalID) (allocation.ComposedResult, error) {
	outcome, err := ReduceComposedGovernedAllocationOutcome(ctx, tx, request, authority)
	if err != nil {
		return allocation.ComposedResult{}, err
	}
	return allocation.WithComposedReplay(outcome.Result, outcome.Replayed), nil
}

// ReduceComposedGovernedAllocationOutcome is the internal result form used by
// the fused workflow. Replayed is true only after both allocation and
// supplemental receipts have been reconstructed without folding either reducer.
func ReduceComposedGovernedAllocationOutcome(ctx context.Context, tx fusedtx.SQLTx, request allocation.ComposedRequest, authority journal.JournalID) (ComposedGovernedAllocationOutcome, error) {
	canonical, _, err := allocation.CanonicalizeComposed(request)
	if err != nil {
		return ComposedGovernedAllocationOutcome{}, err
	}
	request, err = allocation.DecodeComposedRequest(canonical)
	if err != nil {
		return ComposedGovernedAllocationOutcome{}, allocation.NewError(allocation.ErrorCorruption, request.Allocation.OperationID, "composed governed allocation request boundary", "the canonical composed request could not be copied into its immutable reducer form", "the transaction was not committed", "retry with a valid canonical composed request", err)
	}
	if err := validateComposedSupplementReferences(ctx, tx, request); err != nil {
		return ComposedGovernedAllocationOutcome{}, err
	}

	allocationOutcome, err := allocation.ReduceComposedAllocation(ctx, tx, request, authority)
	if err != nil {
		return ComposedGovernedAllocationOutcome{}, err
	}
	in, prepared, err := allocation.SupplementalOperation(request, authority)
	if err != nil {
		return ComposedGovernedAllocationOutcome{}, allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "composed governed allocation supplemental canonicalization", "the supplemental effect list cannot be represented by the canonical journal reducer", "the enclosing transaction was not committed", "supply only valid supported supplemental effects", err)
	}
	var journalResult journal.CommittedResult
	if allocationOutcome.Replayed {
		journalResult, err = reconstructComposedSupplement(ctx, tx, in, prepared)
	} else {
		// Conditions are intentionally evaluated only for fresh work. Although the
		// allocation reducer has staged rows by this point, DBOS owns this same SQL
		// transaction, so a failure rolls those rows back together with every
		// supplement, participant record, and operation output. Exact replay above
		// reconstructs its durable receipt without consulting current fact state.
		if err = checkConditionsInTransaction(ctx, tx, in.Conditions, func(relation factContextRelation, id journal.JournalID) error {
			_, verifyErr := verifySelectedFactContextInTransaction(ctx, tx, relation, int64(id))
			return verifyErr
		}); err != nil {
			return ComposedGovernedAllocationOutcome{}, err
		}
		journalResult, err = reduceCanonicalComposedSupplement(ctx, tx, request.Allocation.OperationID, in, prepared)
	}
	if err != nil {
		return ComposedGovernedAllocationOutcome{}, err
	}
	return ComposedGovernedAllocationOutcome{Result: allocation.NewComposedResult(allocationOutcome.Closure, journalResult), Replayed: allocationOutcome.Replayed}, nil
}

// validateComposedSupplementReferences fences supplemental task-bearing effects
// to the resolved request parent ancestry plus newly allocated children. The
// canonical reducer later enforces the exact active parent authority at each
// produced JournalID; this pre-allocation check prevents an unrelated task from
// being used as an authority-escalation side channel.
func validateComposedSupplementReferences(ctx context.Context, reader composedSupplementReferenceReader, request allocation.ComposedRequest) error {
	allowed := make(map[string]struct{}, len(request.Allocation.Children)+4)
	assignment := request.Allocation.ParentAssignmentID
	visited := map[journal.AssignmentID]struct{}{}
	for assignment != "" {
		if _, seen := visited[assignment]; seen {
			return allocation.NewError(allocation.ErrorCorruption, request.Allocation.OperationID, "composed supplemental reference validation", fmt.Sprintf("the stored parent assignment ancestry revisits %q", assignment), "supplemental effects were rejected before allocation writes", "repair the cyclic parent_assignment_id rows from a consistent backup", nil)
		}
		visited[assignment] = struct{}{}
		var (
			taskRaw string
			parent  *string
		)
		err := reader.QueryRow(ctx, `SELECT task_id,parent_assignment_id
			FROM journal_authority_assignment_episodes WHERE assignment_id=?1`, string(assignment)).Scan(&taskRaw, &parent)
		if fusedtx.IsNoRows(err) {
			// The allocation reducer returns the more precise authority failure. Do
			// not turn an absent parent into an unrelated-reference error here.
			break
		}
		if err != nil {
			return fmt.Errorf("resolve composed parent ancestry at assignment %q: %w", assignment, err)
		}
		task, parseErr := journalParseTask(taskRaw)
		if parseErr != nil {
			return allocation.NewError(allocation.ErrorCorruption, request.Allocation.OperationID, "composed supplemental authority ancestry", fmt.Sprintf("stored assignment %q has malformed task identity %q", assignment, taskRaw), "supplemental effects were rejected before allocation writes", "repair the malformed task_id in journal_authority_assignment_episodes and retry", parseErr)
		}
		allowed[task.String()] = struct{}{}
		if parent == nil {
			break
		}
		assignment = journal.AssignmentID(*parent)
	}
	for _, child := range request.Allocation.Children {
		allowed[child.TaskID.String()] = struct{}{}
	}
	for index, subject := range request.ReferenceScope.Subjects {
		proven, err := referenceScopeDescendsFrom(ctx, reader, request.Allocation.OperationID, subject, request.Allocation.ParentAssignmentID)
		if err != nil {
			return err
		}
		if !proven {
			return allocation.NewError(allocation.ErrorAuthority, request.Allocation.OperationID, "composed reference scope validation", fmt.Sprintf("reference scope subject %d task %q is not a descendant of allocation parent assignment %q", index, subject, request.Allocation.ParentAssignmentID), "the request was rejected and the fused transaction wrote nothing", "cite a task whose assignment lineage descends from the exact allocation parent, or remove it from ReferenceScope", nil)
		}
		allowed[subject.String()] = struct{}{}
	}

	requireAllowed := func(index int, field string, task journal.TaskID) error {
		if task.Namespace == "" {
			return allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "composed supplemental reference validation", fmt.Sprintf("supplemental effect %d has no %s task identity", index, field), "supplemental effects were rejected before allocation writes", "cite the resolved parent/ancestor task or a child allocated by this request", nil)
		}
		if _, ok := allowed[task.String()]; !ok {
			return allocation.NewError(allocation.ErrorAuthority, request.Allocation.OperationID, "composed supplemental reference validation", fmt.Sprintf("supplemental effect %d references unrelated task %q", index, task.String()), "supplemental effects cannot escalate the request parent authority", "cite only a resolved parent/ancestor task or a child allocated by this request", nil)
		}
		return nil
	}

	for index, effect := range request.SupplementalEffects {
		canonicalContexts, err := journal.CanonicalEventContexts(effect.Contexts)
		if err != nil {
			return allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "composed supplemental reference validation", fmt.Sprintf("supplemental effect %d has invalid contexts", index), "supplemental effects were rejected before allocation writes", "supply only valid canonical contexts", err)
		}
		for _, eventContext := range canonicalContexts {
			if eventContext.Kind() != journal.EventContextKindTask {
				continue
			}
			_, identity, err := journal.EncodeStoredEventContext(eventContext)
			if err != nil {
				return allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "composed supplemental reference validation", fmt.Sprintf("supplemental effect %d has an invalid task context", index), "supplemental effects were rejected before allocation writes", "supply a valid parent/ancestor or allocated-child task context", err)
			}
			task, err := ptypes.ParseTaskID(identity)
			if err != nil {
				return allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "composed supplemental reference validation", fmt.Sprintf("supplemental effect %d has an invalid task context identity", index), "supplemental effects were rejected before allocation writes", "supply a valid parent/ancestor or allocated-child task context", err)
			}
			if err := requireAllowed(index, "task context", task); err != nil {
				return err
			}
		}
		switch effect.Sort {
		case journal.EffectEvidence:
			// Untasked evidence carries no authority-bearing task reference and is
			// permitted for operation-level evidence (for example, an epoch receipt).
			if effect.TaskID.Namespace != "" {
				if err := requireAllowed(index, "evidence", effect.TaskID); err != nil {
					return err
				}
			}
		case journal.EffectTaskEvent:
			if err := requireAllowed(index, "task-event", effect.TaskID); err != nil {
				return err
			}
		case journal.EffectEdgeAdd:
			if err := requireAllowed(index, "edge source", effect.TaskID); err != nil {
				return err
			}
			target, err := ptypes.ParseTaskID(effect.EdgeTargetID)
			if err != nil {
				return allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "composed supplemental reference validation", fmt.Sprintf("supplemental edge %d has an invalid target task identity", index), "supplemental effects were rejected before allocation writes", "supply a valid parent/ancestor or allocated-child TaskID as EdgeTargetID", err)
			}
			if err := requireAllowed(index, "edge target", target); err != nil {
				return err
			}
		case journal.EffectActivityCreate:
			// ActivityCreate has no task operand. Canonical validation and the
			// activities foreign key validate its typed identity and agent.
		default:
			return allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "composed supplemental reference validation", fmt.Sprintf("supplemental effect %d has unsupported sort %s", index, effect.Sort), "the DBOS workflow and reducer were not entered", "use the static SupplementPolicyV1 sorts", nil)
		}
	}
	return nil
}

func referenceScopeDescendsFrom(ctx context.Context, reader composedSupplementReferenceReader, operationID journal.OperationID, subject journal.TaskID, ancestor journal.AssignmentID) (bool, error) {
	rows, err := reader.Query(ctx, `SELECT parent_assignment_id FROM journal_authority_assignment_episodes WHERE task_id=?1`, subject.String())
	if err != nil {
		return false, fmt.Errorf("list composed reference scope subject %q assignments: %w", subject, err)
	}
	parents := make([]journal.AssignmentID, 0, 1)
	for rows.Next() {
		var parent *string
		if err := rows.Scan(&parent); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("read composed reference scope subject %q assignment parent: %w", subject, err)
		}
		if parent != nil {
			parents = append(parents, journal.AssignmentID(*parent))
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, fmt.Errorf("iterate composed reference scope subject %q assignments: %w", subject, err)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close composed reference scope subject %q assignments: %w", subject, err)
	}

	for _, current := range parents {
		visited := map[journal.AssignmentID]struct{}{}
		for current != "" {
			if current == ancestor {
				return true, nil
			}
			if _, seen := visited[current]; seen {
				return false, allocation.NewError(allocation.ErrorCorruption, operationID, "composed reference scope validation", fmt.Sprintf("reference scope subject %q has cyclic assignment ancestry at %q", subject, current), "supplemental effects were rejected before allocation writes", "repair the cyclic parent_assignment_id rows from a consistent backup", nil)
			}
			visited[current] = struct{}{}

			var parent *string
			err := reader.QueryRow(ctx, `SELECT parent_assignment_id FROM journal_authority_assignment_episodes WHERE assignment_id=?1`, string(current)).Scan(&parent)
			if fusedtx.IsNoRows(err) {
				break
			}
			if err != nil {
				return false, fmt.Errorf("resolve composed reference scope subject %q ancestry at assignment %q: %w", subject, current, err)
			}
			if parent == nil {
				break
			}
			current = journal.AssignmentID(*parent)
		}
	}
	return false, nil
}

// reduceCanonicalComposedSupplement is the restricted transaction-scoped
// invocation of the normal canonical journal semantics. It uses journal's
// CanonicalMutation as the executable source of effect/result-slot identity and
// implements only the statically admitted V1 effect families below. No caller
// can inject SQL or a raw transaction handle at this boundary.
func reduceCanonicalComposedSupplement(ctx context.Context, tx fusedtx.SQLTx, externalOperationID journal.OperationID, in journal.OperationInput, prepared journal.CanonicalMutation) (journal.CommittedResult, error) {
	if _, found, err := lookupComposedSupplement(ctx, tx, in.OperationID); err != nil {
		return journal.CommittedResult{}, err
	} else if found {
		return journal.CommittedResult{}, allocation.NewError(allocation.ErrorConflict, journal.OperationID(in.OperationID), "composed supplemental internal identity", "the domain-separated internal supplemental operation identity is already occupied", "the complete allocation transaction was rolled back", "retry the original external request or use a new external OperationID", nil)
	}
	if err := v1RequireAuthority(ctx, tx, *in.AuthorityJournalID, 1); err != nil {
		return journal.CommittedResult{}, err
	}

	now := time.Now().UTC().UnixNano()
	anchor, err := composedInsertJournalRow(ctx, tx, journal.JournalKindOperation, in.ActorID, now, nil)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_operations
		(journal_id,operation_id,authority_journal_id,command_digest,mutation_digest,mutation_encoding_version,canonical_mutation)
		VALUES (?1,?2,?3,?4,?5,?6,?7)`, anchor, string(in.OperationID), int64(*in.AuthorityJournalID), in.CommandDigest, prepared.DerivedDigest(), prepared.EncodingVersion().String(), prepared.CanonicalBytes()); err != nil {
		return journal.CommittedResult{}, fmt.Errorf("insert composed supplemental canonical operation: %w", err)
	}

	for index, effect := range in.Effects {
		kind, err := effect.Sort.JournalKind()
		if err != nil {
			return journal.CommittedResult{}, err
		}
		recordedAt := now
		if effect.RecordedAtOverride != nil {
			recordedAt = *effect.RecordedAtOverride
		}
		produced, err := composedInsertJournalRow(ctx, tx, kind, in.ActorID, recordedAt, &anchor)
		if err != nil {
			return journal.CommittedResult{}, err
		}
		switch effect.Sort {
		case journal.EffectTaskEvent:
			err = foldV1TaskEvent(ctx, tx, in, produced, recordedAt, effect)
		case journal.EffectEvidence:
			err = foldV1Evidence(ctx, tx, in, produced, effect)
		case journal.EffectEdgeAdd:
			err = foldV1EdgeAdd(ctx, tx, in, produced, recordedAt, effect)
		case journal.EffectActivityCreate:
			err = foldV1ActivityCreate(ctx, tx, in, produced, recordedAt, effect)
			if isForeignKeyViolation(err) {
				err = allocation.NewError(allocation.ErrorValidation, externalOperationID, "composed supplemental EffectActivityCreate", "the activity cites an agent that is not registered", "the complete governed allocation and all supplemental effects were rolled back", "register the activity agent before retrying the same request", err)
			}
		default:
			err = allocation.NewError(allocation.ErrorValidation, journal.OperationID(in.OperationID), "composed supplemental journal reducer", fmt.Sprintf("effect %d has unsupported sort %s", index, effect.Sort), "the transaction was rolled back", "use the static SupplementPolicyV1 effect set", nil)
		}
		if err != nil {
			return journal.CommittedResult{}, normalizeComposedV1DomainFailure(externalOperationID, effect.Sort, err)
		}
		switch effect.Sort {
		case journal.EffectTaskEvent:
			err = composedProjectTaskEvent(ctx, tx, in.ActorID, produced, recordedAt, effect)
		case journal.EffectEvidence:
			if effect.TaskID.Namespace != "" {
				err = composedAttributeAndWatermark(ctx, tx, effect.TaskID, in.ActorID, produced)
			}
		case journal.EffectEdgeAdd:
			if err = v1ProjectEdgeAdd(ctx, tx, effect.TaskID, effect.EdgeTargetID, effect.EdgeRelKind, recordedAt); err == nil {
				err = composedAttributeAndWatermark(ctx, tx, effect.TaskID, in.ActorID, produced)
			}
		}
		if err != nil {
			return journal.CommittedResult{}, normalizeComposedV1DomainFailure(externalOperationID, effect.Sort, err)
		}
		if effect.ResultSlot != "" {
			if err := requireResultSlotOwnOperation(ctx, tx, anchor, produced); err != nil {
				return journal.CommittedResult{}, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO journal_operation_result_slots (journal_id,result_slot_id,produced_journal_id) VALUES (?1,?2,?3)`, anchor, string(effect.ResultSlot), produced); err != nil {
				return journal.CommittedResult{}, fmt.Errorf("insert composed supplemental result slot %q: %w", effect.ResultSlot, err)
			}
		}
	}
	if err := v1ValidateClosedEvents(ctx, tx, in.Effects); err != nil {
		return journal.CommittedResult{}, normalizeComposedV1DomainFailure(externalOperationID, journal.EffectTaskEvent, err)
	}
	return reconstructComposedSupplement(ctx, tx, in, prepared)
}

// normalizeComposedV1DomainFailure admits only the three deterministic V1
// reducer rejections whose concrete sentinels/types are part of the static
// composed policy. Operational SQLite errors and arbitrary errors with similar
// text remain infrastructure failures and therefore never enter DBOS's durable
// governed-failure envelope.
func normalizeComposedV1DomainFailure(operation journal.OperationID, sort journal.EffectSort, cause error) error {
	var activityConflict *journal.ActivityConflict
	switch {
	case sort == journal.EffectActivityCreate && errors.As(cause, &activityConflict):
		return allocation.NewError(allocation.ErrorCollision, operation, "composed supplemental activity creation", "the requested ActivityID already belongs to a committed activity", "the complete governed allocation and all supplemental effects were rolled back", "retry the original request or choose a fresh ActivityID", activityConflict)
	case sort == journal.EffectEdgeAdd && errors.Is(cause, ptypes.ErrCycleDetected):
		return allocation.NewError(allocation.ErrorValidation, operation, "composed supplemental edge addition", "the requested blocked-by edge would create a dependency cycle", "the complete governed allocation and all supplemental effects were rolled back", "remove or redirect the cycle-forming edge before retrying", ptypes.ErrCycleDetected)
	case sort == journal.EffectTaskEvent && errors.Is(cause, journal.ErrStatusTransition):
		return allocation.NewError(allocation.ErrorValidation, operation, "composed supplemental task lifecycle", "the requested task lifecycle event is illegal from the task's current status", "the complete governed allocation and all supplemental effects were rolled back", "refresh the task status and submit a legal transition, or explicitly use the supported forced transition", cause)
	case errors.Is(cause, journal.ErrCloseWithoutEnding):
		return allocation.NewError(allocation.ErrorValidation, operation, "composed supplemental task lifecycle closure", "the supplemental lifecycle closes a task without first ending its active lifecycle", "the complete governed allocation and all supplemental effects were rolled back", "add the required ending event before closing the task and retry the exact request", cause)
	default:
		return cause
	}
}

func composedInsertJournalRow(ctx context.Context, tx fusedtx.SQLTx, kind journal.JournalKind, actor journal.ActorID, recordedAt int64, producing *int64) (int64, error) {
	var actorArg, producingArg any
	if producing == nil {
		actorArg = actor.String()
	} else {
		producingArg = *producing
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO journal (kind_id,actor_id,recorded_at,produced_by_operation_journal_id)
		VALUES (?1,?2,?3,?4) RETURNING journal_id`, int(kind), actorArg, recordedAt, producingArg).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert composed supplemental journal row (%s): %w", kind, err)
	}
	return id, nil
}

func composedProjectTaskEvent(ctx context.Context, tx fusedtx.SQLTx, actor journal.ActorID, jid, recordedAt int64, effect journal.Effect) error {
	if journal.IsReducerDerivedTaskEventKind(effect.EventKind) {
		// CanonicalizeComposed rejects these before DBOS starts. Keep this reducer
		// defense so a malformed recovered receipt cannot make a generic TaskEvent
		// impersonate a typed relationship, task birth, or migration projection.
		return fmt.Errorf("composed supplemental task event kind %q is reducer-derived; use the matching typed effect or a caller-domain task event", effect.EventKind)
	}
	return projectV1TaskEvent(ctx, tx, actor, jid, recordedAt, effect)
}

// projectV1TaskEvent is the one live post-fold task-event projection primitive
// shared by ordinary Apply and governed composition.
func projectV1TaskEvent(ctx context.Context, tx fusedtx.SQLTx, actor journal.ActorID, jid, recordedAt int64, effect journal.Effect) error {
	if status, lifecycle := journal.StatusForEventKind(effect.EventKind); lifecycle {
		if journal.IsTransitionLifecycleKind(effect.EventKind) && !effect.Forced {
			var current journal.TaskStatus
			if err := tx.QueryRow(ctx, `SELECT status_id FROM tasks WHERE id=?1`, effect.TaskID.String()).Scan(&current); err != nil {
				return fmt.Errorf("read current task status during shared V1 task-event projection: %w", err)
			}
			if err := journal.ValidateStatusTransition(current, effect.EventKind); err != nil {
				return err
			}
		}
		var closedAt any
		if status == journal.TaskStatusClosed {
			closedAt = recordedAt
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET status_id=?1,closed_at=?2,last_journal_id=?3 WHERE id=?4`, int(status), closedAt, jid, effect.TaskID.String()); err != nil {
			return fmt.Errorf("project shared V1 task-event status: %w", err)
		}
		return v1InsertAttribution(ctx, tx, effect.TaskID, actor, jid)
	}
	return composedAttributeAndWatermark(ctx, tx, effect.TaskID, actor, jid)
}

func composedAttributeAndWatermark(ctx context.Context, tx fusedtx.SQLTx, task journal.TaskID, actor journal.ActorID, jid int64) error {
	if err := v1InsertAttribution(ctx, tx, task, actor, jid); err != nil {
		return err
	}
	return v1AdvanceWatermark(ctx, tx, task, jid)
}

func v1RequireAuthority(ctx context.Context, tx fusedtx.SQLTx, authority journal.JournalID, before int64) error {
	if authority <= 0 {
		return fmt.Errorf("%w: composed supplemental operation has no positive parent authority", journal.ErrAuthorityScope)
	}
	var kind int
	if err := tx.QueryRow(ctx, `SELECT authority_kind_id FROM journal_authorities WHERE journal_id=?1`, int64(authority)).Scan(&kind); err != nil {
		return fmt.Errorf("%w: resolve composed supplemental authority %d: %v", journal.ErrAuthorityScope, authority, err)
	}
	return nil
}

type composedSupplementStored struct {
	anchor     int64
	recordedAt int64
	authority  journal.JournalID
	command    []byte
	digest     []byte
	version    string
	canonical  []byte
}

func lookupComposedSupplement(ctx context.Context, tx fusedtx.SQLTx, operation journal.OperationID) (composedSupplementStored, bool, error) {
	var stored composedSupplementStored
	var authority int64
	var actor sql.NullString
	var producer sql.NullInt64
	var kind journal.JournalKind
	err := tx.QueryRow(ctx, `SELECT o.journal_id,o.authority_journal_id,o.command_digest,o.mutation_digest,o.mutation_encoding_version,o.canonical_mutation,
		j.kind_id,j.actor_id,j.produced_by_operation_journal_id,j.recorded_at
		FROM journal_operations o JOIN journal j ON j.journal_id=o.journal_id WHERE o.operation_id=?1`, string(operation)).Scan(&stored.anchor, &authority, &stored.command, &stored.digest, &stored.version, &stored.canonical, &kind, &actor, &producer, &stored.recordedAt)
	if fusedtx.IsNoRows(err) {
		return composedSupplementStored{}, false, nil
	}
	if err != nil {
		return composedSupplementStored{}, false, fmt.Errorf("lookup composed supplemental operation %q: %w", operation, err)
	}
	if kind != journal.JournalKindOperation || !actor.Valid || producer.Valid {
		return composedSupplementStored{}, false, composedCorruption(operation, "the supplemental operation anchor is not a canonical actor-owned operation row")
	}
	stored.authority = journal.JournalID(authority)
	stored.command = append([]byte(nil), stored.command...)
	stored.digest = append([]byte(nil), stored.digest...)
	stored.canonical = append([]byte(nil), stored.canonical...)
	return stored, true, nil
}

func reconstructComposedSupplement(ctx context.Context, tx fusedtx.SQLTx, in journal.OperationInput, prepared journal.CanonicalMutation) (journal.CommittedResult, error) {
	stored, found, err := lookupComposedSupplement(ctx, tx, in.OperationID)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	if !found {
		return journal.CommittedResult{}, allocation.NewError(allocation.ErrorCorruption, journal.OperationID(in.OperationID), "composed supplemental replay", "the governed allocation receipt exists but its domain-separated canonical supplemental receipt is absent", "the composed result cannot be reconstructed", "restore the allocation and supplemental journal receipts from one consistent backup", nil)
	}
	if stored.authority != *in.AuthorityJournalID || !bytes.Equal(stored.command, in.CommandDigest) || !bytes.Equal(stored.digest, prepared.DerivedDigest()) || stored.version != prepared.EncodingVersion().String() || !bytes.Equal(stored.canonical, prepared.CanonicalBytes()) {
		return journal.CommittedResult{}, allocation.NewError(allocation.ErrorCorruption, journal.OperationID(in.OperationID), "composed supplemental replay", "the stored internal canonical journal receipt disagrees with the composed allocation receipt", "the composed result cannot be trusted", "restore the allocation and supplemental journal receipts from one consistent backup", nil)
	}
	var anchorActor string
	if err := tx.QueryRow(ctx, `SELECT actor_id FROM journal WHERE journal_id=?1`, stored.anchor).Scan(&anchorActor); err != nil || anchorActor != in.ActorID.String() {
		return journal.CommittedResult{}, composedCorruption(in.OperationID, "the supplemental operation anchor actor disagrees with the canonical operation actor")
	}
	produced := make([]journal.JournalID, 0, len(in.Effects))
	rows, err := tx.Query(ctx, `SELECT journal_id,kind_id,actor_id,produced_by_operation_journal_id,recorded_at FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id`, stored.anchor)
	if err != nil {
		return journal.CommittedResult{}, fmt.Errorf("load composed supplemental effect rows: %w", err)
	}
	for rows.Next() {
		var id int64
		var kind journal.JournalKind
		var actor sql.NullString
		var producer int64
		var recordedAt int64
		if err := rows.Scan(&id, &kind, &actor, &producer, &recordedAt); err != nil {
			_ = rows.Close()
			return journal.CommittedResult{}, err
		}
		index := len(produced)
		if index >= len(in.Effects) {
			_ = rows.Close()
			return journal.CommittedResult{}, composedCorruption(in.OperationID, "the supplemental operation has extra produced rows")
		}
		expectedKind, kindErr := in.Effects[index].Sort.JournalKind()
		expectedRecordedAt := stored.recordedAt
		if in.Effects[index].RecordedAtOverride != nil {
			expectedRecordedAt = *in.Effects[index].RecordedAtOverride
		}
		if kindErr != nil || kind != expectedKind || actor.Valid || producer != stored.anchor || recordedAt != expectedRecordedAt {
			_ = rows.Close()
			return journal.CommittedResult{}, composedCorruption(in.OperationID, fmt.Sprintf("supplemental produced row %d has the wrong kind or subtype", index))
		}
		if id <= stored.anchor {
			_ = rows.Close()
			return journal.CommittedResult{}, composedCorruption(in.OperationID, fmt.Sprintf("supplemental produced row %d does not strictly follow its operation anchor", index))
		}
		produced = append(produced, journal.JournalID(id))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return journal.CommittedResult{}, err
	}
	if err := rows.Close(); err != nil {
		return journal.CommittedResult{}, err
	}
	if len(produced) != len(in.Effects) {
		return journal.CommittedResult{}, composedCorruption(in.OperationID, "the supplemental operation is missing produced rows")
	}
	for index, effect := range in.Effects {
		if err := validateCanonicalComposedEffect(ctx, tx, in.OperationID, int64(produced[index]), effect); err != nil {
			return journal.CommittedResult{}, err
		}
	}

	result, err := reconstructCommitted(ctx, tx, stored.anchor)
	if err != nil {
		return journal.CommittedResult{}, composedCorruption(in.OperationID, err.Error())
	}
	expectedEvents := make([]journal.JournalID, 0, len(produced))
	expectedSlots := make(map[journal.ResultSlotID]journal.JournalID)
	for i, effect := range in.Effects {
		effectKind, _ := effect.Sort.JournalKind()
		if effectKind == journal.JournalKindTaskEvent {
			expectedEvents = append(expectedEvents, produced[i])
		}
		if effect.ResultSlot != "" {
			expectedSlots[effect.ResultSlot] = produced[i]
		}
	}
	if len(result.EmittedEvents) != len(expectedEvents) {
		return journal.CommittedResult{}, composedCorruption(in.OperationID, "the emitted task-event closure is incomplete or contains foreign rows")
	}
	for i := range expectedEvents {
		if result.EmittedEvents[i] != expectedEvents[i] {
			return journal.CommittedResult{}, composedCorruption(in.OperationID, "the emitted task-event closure order disagrees with canonical effects")
		}
	}
	if len(result.ResultSlots) != len(expectedSlots) {
		return journal.CommittedResult{}, composedCorruption(in.OperationID, "the exact canonical result-slot set is missing or has extra bindings")
	}
	for _, slot := range result.ResultSlots {
		expected, exists := expectedSlots[slot.Slot]
		if !exists || expected != slot.ProducedJournalID {
			return journal.CommittedResult{}, composedCorruption(in.OperationID, fmt.Sprintf("result slot %q is not anchored to its canonical operation-owned row", slot.Slot))
		}
		effect := in.Effects[indexOfProduced(produced, expected)]
		expectedKind, _ := effect.Sort.JournalKind()
		if slot.Kind != expectedKind || (effect.Sort == journal.EffectTaskEvent && (slot.TaskID == nil || *slot.TaskID != effect.TaskID)) || (effect.Sort == journal.EffectActivityCreate && (slot.ActivityID == nil || *slot.ActivityID != effect.ActivityID)) {
			return journal.CommittedResult{}, composedCorruption(in.OperationID, fmt.Sprintf("result slot %q has a forged kind or typed identity", slot.Slot))
		}
	}
	return result, nil
}

func indexOfProduced(rows []journal.JournalID, wanted journal.JournalID) int {
	for index, row := range rows {
		if row == wanted {
			return index
		}
	}
	return -1
}

func validateCanonicalComposedEffect(ctx context.Context, tx fusedtx.SQLTx, operation journal.OperationID, jid int64, effect journal.Effect) error {
	corrupt := func(why string) error { return composedCorruption(operation, why) }
	switch effect.Sort {
	case journal.EffectTaskEvent, journal.EffectEdgeAdd:
		var task, kind, payload string
		if err := tx.QueryRow(ctx, `SELECT task_id,event_kind,payload FROM journal_task_events WHERE journal_id=?1`, jid).Scan(&task, &kind, &payload); err != nil {
			return corrupt(fmt.Sprintf("supplemental effect row %d has no exact task-event subtype: %v", jid, err))
		}
		expectedKind := effect.EventKind
		expectedPayload := effect.Payload
		if effect.Sort == journal.EffectEdgeAdd {
			expectedKind = journal.EventKindEdgeAdded
			var err error
			expectedPayload, err = journal.EncodeEdgeMutationPayload(effect.EdgeTargetID, effect.EdgeRelKind)
			if err != nil {
				return corrupt(fmt.Sprintf("canonical edge operands at row %d cannot be encoded", jid))
			}
		} else if effect.Forced && journal.IsTransitionLifecycleKind(effect.EventKind) {
			expectedPayload = journal.EncodeForcedTransitionPayload()
		}
		if len(expectedPayload) == 0 {
			expectedPayload = []byte(`{}`)
		}
		if task != effect.TaskID.String() || kind != string(expectedKind) || payload != string(expectedPayload) {
			return corrupt(fmt.Sprintf("supplemental task-event row %d disagrees with canonical task, kind, operands, or payload", jid))
		}
		if err := validateCanonicalContexts(ctx, tx, operation, jid, "journal_task_event_contexts", "event_journal_id", effect.Contexts); err != nil {
			return err
		}
	case journal.EffectEvidence:
		var kind, payload string
		var task sql.NullString
		var digest []byte
		if err := tx.QueryRow(ctx, `SELECT evidence_kind,task_id,content_digest,payload FROM journal_evidence WHERE journal_id=?1`, jid).Scan(&kind, &task, &digest, &payload); err != nil {
			return corrupt(fmt.Sprintf("supplemental effect row %d has no exact evidence subtype: %v", jid, err))
		}
		expectedTask, expectsTask := "", effect.TaskID.Namespace != ""
		if expectsTask {
			expectedTask = effect.TaskID.String()
		}
		if kind != string(effect.EvidenceKind) || task.Valid != expectsTask || (expectsTask && task.String != expectedTask) || !bytes.Equal(digest, effect.ContentDigest) || payload != string(defaultJSONPayload(effect.Payload)) {
			return corrupt(fmt.Sprintf("supplemental evidence row %d disagrees with canonical identity, digest, or payload", jid))
		}
		if err := validateCanonicalContexts(ctx, tx, operation, jid, "journal_evidence_contexts", "evidence_journal_id", effect.Contexts); err != nil {
			return err
		}
	case journal.EffectActivityCreate:
		var activity, agent, notes string
		var phase, stage int
		var startedAt int64
		if err := tx.QueryRow(ctx, `SELECT ac.activity_id,a.agent_id,a.phase_id,a.stage_id,a.started_at,a.notes FROM journal_activity_creations ac JOIN activities a ON a.id=ac.activity_id WHERE ac.journal_id=?1`, jid).Scan(&activity, &agent, &phase, &stage, &startedAt, &notes); err != nil {
			return corrupt(fmt.Sprintf("supplemental effect row %d has no exact activity subtype: %v", jid, err))
		}
		var recordedAt int64
		if err := tx.QueryRow(ctx, `SELECT recorded_at FROM journal WHERE journal_id=?1`, jid).Scan(&recordedAt); err != nil {
			return corrupt(fmt.Sprintf("supplemental activity row %d has no recorded chronology: %v", jid, err))
		}
		if activity != effect.ActivityID.String() || agent != effect.ActivityAgentID.String() || phase != int(effect.ActivityPhase) || stage != int(effect.ActivityStage) || startedAt != recordedAt || notes != effect.ActivityNotes {
			return corrupt(fmt.Sprintf("supplemental activity row %d disagrees with canonical identity or operands", jid))
		}
	default:
		return corrupt(fmt.Sprintf("supplemental row %d has unsupported canonical subtype %s", jid, effect.Sort))
	}
	return nil
}

func defaultJSONPayload(payload []byte) []byte {
	if len(payload) == 0 {
		return []byte(`{}`)
	}
	return payload
}

func validateCanonicalContexts(ctx context.Context, tx fusedtx.SQLTx, operation journal.OperationID, jid int64, table, key string, expected []journal.EventContext) error {
	canonical, err := journal.CanonicalEventContexts(expected)
	if err != nil {
		return composedCorruption(operation, fmt.Sprintf("canonical contexts for row %d are invalid", jid))
	}
	want := make([][2]string, 0, len(canonical))
	for _, value := range canonical {
		kind, identity, err := journal.EncodeStoredEventContext(value)
		if err != nil {
			return composedCorruption(operation, fmt.Sprintf("canonical context for row %d cannot be encoded", jid))
		}
		want = append(want, [2]string{string(kind), identity})
	}
	query := fmt.Sprintf(`SELECT context_kind,context_identity FROM %s WHERE %s=?1 ORDER BY context_kind,context_identity`, table, key)
	if table == "journal_task_event_contexts" {
		query = fmt.Sprintf(`SELECT context_kind,context_identity FROM %s WHERE %s=?1 AND attached_by_journal_id=?1 ORDER BY context_kind,context_identity`, table, key)
	}
	rows, err := tx.Query(ctx, query, jid)
	if err != nil {
		return composedCorruption(operation, fmt.Sprintf("contexts for row %d cannot be read: %v", jid, err))
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var kind, identity string
		if err := rows.Scan(&kind, &identity); err != nil {
			return composedCorruption(operation, fmt.Sprintf("context for row %d cannot be decoded", jid))
		}
		if index >= len(want) || want[index] != [2]string{kind, identity} {
			return composedCorruption(operation, fmt.Sprintf("contexts for row %d are foreign, duplicated, or unsorted", jid))
		}
		index++
	}
	if rows.Err() != nil || index != len(want) {
		return composedCorruption(operation, fmt.Sprintf("contexts for row %d are incomplete", jid))
	}
	return nil
}

func composedCorruption(operation journal.OperationID, why string) error {
	return allocation.NewError(allocation.ErrorCorruption, operation, "composed supplemental replay reconstruction", why, "the composed result is not trusted and no participant was invoked", "restore the canonical operation, produced rows, and result slots from one consistent backup", nil)
}
