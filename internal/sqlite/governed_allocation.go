package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/internal/allocation"
	"github.com/dayvidpham/provenance/internal/journal"
)

// ReconstructGovernedGenesisSnapshot validates and reconstructs an existing
// receipt in a read-only SQLite snapshot. An absent receipt is corruption and
// this path never calls a reducer admission/write branch.
func (db *DB) ReconstructGovernedGenesisSnapshot(ctx context.Context, request allocation.RootGenesisRequest) (result allocation.OperationClosure, err error) {
	err = db.withGovernedReadSnapshot(ctx, func(ctx context.Context, tx allocationSQLTx) error {
		result, err = allocation.VerifyGenesisReceipt(ctx, tx, request)
		return err
	})
	return result, err
}

func (db *DB) ReconstructGovernedAllocationSnapshot(ctx context.Context, request allocation.GovernedAllocationRequest, authority journal.JournalID) (result allocation.OperationClosure, err error) {
	err = db.withGovernedReadSnapshot(ctx, func(ctx context.Context, tx allocationSQLTx) error {
		result, err = allocation.VerifyAllocationReceipt(ctx, tx, request, authority, false)
		return err
	})
	return result, err
}

func (db *DB) ReconstructGovernedComposedSnapshot(ctx context.Context, request allocation.ComposedRequest, authority journal.JournalID) (result allocation.ComposedResult, err error) {
	var closure allocation.OperationClosure
	err = db.withGovernedReadSnapshot(ctx, func(ctx context.Context, tx allocationSQLTx) error {
		closure, err = allocation.VerifyComposedAllocationReceipt(ctx, tx, request, authority)
		if err != nil {
			return err
		}
		in, prepared, prepareErr := allocation.SupplementalOperation(request, authority)
		if prepareErr != nil {
			return prepareErr
		}
		committed, reconstructErr := reconstructComposedSupplement(ctx, tx, in, prepared)
		if reconstructErr != nil {
			return reconstructErr
		}
		if projectionErr := verifyComposedFinalProjections(ctx, tx, request, closure); projectionErr != nil {
			return projectionErr
		}
		result = allocation.NewComposedResult(closure, committed)
		return nil
	})
	return result, err
}

type expectedComposedTask struct {
	namespace, title, description, owner, notes, closeReason string
	status, priority, taskType, phase                        int
	createdAt, updatedAt, watermark                          int64
	closedAt                                                 sql.NullInt64
	attributions                                             map[string]int64
}

// verifyComposedFinalProjections derives only newly allocated child state from
// the immutable allocation receipt and its ordered canonical supplemental
// effects. It never writes live projections and intentionally does not treat the
// birth projection as final, so admitted metadata/lifecycle effects replay.
func verifyComposedFinalProjections(ctx context.Context, tx allocationSQLTx, request allocation.ComposedRequest, closure allocation.OperationClosure) error {
	children := closure.Children()
	states := make(map[string]*expectedComposedTask, len(children))
	requestActor := request.Allocation.ActorID.String()
	for i, binding := range children {
		child := request.Allocation.Children[i]
		var born int64
		if err := tx.QueryRow(ctx, `SELECT recorded_at FROM journal WHERE journal_id=?1`, int64(binding.TaskRow.JournalID)).Scan(&born); err != nil {
			return composedCorruption(request.Allocation.OperationID, fmt.Sprintf("child %d birth chronology is absent", i))
		}
		attrs := map[string]int64{requestActor: int64(binding.TaskRow.JournalID)}
		if _, exists := attrs[child.Occupant.String()]; !exists {
			attrs[child.Occupant.String()] = int64(binding.AssignmentRow.JournalID)
		}
		states[child.TaskID.String()] = &expectedComposedTask{
			namespace: child.TaskID.Namespace, title: child.Title, description: child.Description,
			owner: child.Occupant.String(), status: 0, priority: int(child.Priority), taskType: int(child.Type), phase: int(child.Phase),
			createdAt: born, updatedAt: born, watermark: int64(binding.AssignmentRow.JournalID), attributions: attrs,
		}
	}
	var supplementalAnchor int64
	if err := tx.QueryRow(ctx, `SELECT journal_id FROM journal_operations WHERE operation_id=?1`, string(journal.NewGovernedAllocationSupplementOperationID(request.Allocation.OperationID).OperationID())).Scan(&supplementalAnchor); err != nil {
		return composedCorruption(request.Allocation.OperationID, "the supplemental operation anchor is absent during final projection proof")
	}
	rows, err := tx.Query(ctx, `SELECT journal_id,recorded_at FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id`, supplementalAnchor)
	if err != nil {
		return err
	}
	defer rows.Close()
	type producedRow struct{ id, at int64 }
	produced := make([]producedRow, 0, len(request.SupplementalEffects))
	for rows.Next() {
		var row producedRow
		if err := rows.Scan(&row.id, &row.at); err != nil {
			return err
		}
		produced = append(produced, row)
	}
	if rows.Err() != nil || len(produced) != len(request.SupplementalEffects) {
		return composedCorruption(request.Allocation.OperationID, "the ordered supplemental effect rows are incomplete during final projection proof")
	}
	expectedEdges := map[string]struct{}{}
	for i, effect := range request.SupplementalEffects {
		state := states[effect.TaskID.String()]
		if state != nil {
			attribute := effect.Sort == journal.EffectTaskEvent || effect.Sort == journal.EffectEdgeAdd || (effect.Sort == journal.EffectEvidence && effect.TaskID.Namespace != "")
			if attribute {
				if _, ok := state.attributions[requestActor]; !ok {
					state.attributions[requestActor] = produced[i].id
				}
				state.watermark = produced[i].id
			}
			if effect.Sort == journal.EffectTaskEvent {
				changed := false
				if effect.UpdateTitle != nil {
					state.title = *effect.UpdateTitle
					changed = true
				}
				if effect.UpdateDescription != nil {
					state.description = *effect.UpdateDescription
					changed = true
				}
				if effect.UpdatePriority != nil {
					state.priority = int(*effect.UpdatePriority)
					changed = true
				}
				if effect.UpdatePhase != nil {
					state.phase = int(*effect.UpdatePhase)
					changed = true
				}
				if effect.UpdateNotes != nil {
					state.notes = *effect.UpdateNotes
					changed = true
				}
				if changed {
					state.updatedAt = produced[i].at
				}
				if status, lifecycle := journal.StatusForEventKind(effect.EventKind); lifecycle {
					state.status = int(status)
					state.closedAt = sql.NullInt64{}
					if status == journal.TaskStatusClosed {
						state.closedAt = sql.NullInt64{Int64: produced[i].at, Valid: true}
						state.closeReason = effect.CloseReason
					}
				}
			}
		}
		if effect.Sort == journal.EffectEdgeAdd {
			if _, sourceChild := states[effect.TaskID.String()]; sourceChild {
				expectedEdges[fmt.Sprintf("%s\x00%s\x00%d", effect.TaskID.String(), effect.EdgeTargetID, effect.EdgeRelKind)] = struct{}{}
			}
			if _, targetChild := states[effect.EdgeTargetID]; targetChild {
				expectedEdges[fmt.Sprintf("%s\x00%s\x00%d", effect.TaskID.String(), effect.EdgeTargetID, effect.EdgeRelKind)] = struct{}{}
			}
		}
	}
	for taskID, want := range states {
		var got expectedComposedTask
		if err := tx.QueryRow(ctx, `SELECT namespace,title,description,COALESCE(owner_id,''),notes,close_reason,status_id,priority_id,type_id,phase_id,created_at,updated_at,last_journal_id,closed_at FROM tasks WHERE id=?1`, taskID).Scan(&got.namespace, &got.title, &got.description, &got.owner, &got.notes, &got.closeReason, &got.status, &got.priority, &got.taskType, &got.phase, &got.createdAt, &got.updatedAt, &got.watermark, &got.closedAt); err != nil || got.namespace != want.namespace || got.title != want.title || got.description != want.description || got.owner != want.owner || got.notes != want.notes || got.closeReason != want.closeReason || got.status != want.status || got.priority != want.priority || got.taskType != want.taskType || got.phase != want.phase || got.createdAt != want.createdAt || got.updatedAt != want.updatedAt || got.watermark != want.watermark || got.closedAt != want.closedAt {
			return composedCorruption(request.Allocation.OperationID, fmt.Sprintf("final task projection for allocated child %q disagrees with canonical receipt plus effects", taskID))
		}
		attrs := map[string]int64{}
		attrRows, qerr := tx.Query(ctx, `SELECT actor_id,first_journal_id FROM task_attributions WHERE task_id=?1`, taskID)
		if qerr != nil {
			return qerr
		}
		for attrRows.Next() {
			var actor string
			var first int64
			if err := attrRows.Scan(&actor, &first); err != nil {
				_ = attrRows.Close()
				return err
			}
			attrs[actor] = first
		}
		_ = attrRows.Close()
		if len(attrs) != len(want.attributions) {
			return composedCorruption(request.Allocation.OperationID, fmt.Sprintf("final attributions for allocated child %q are incomplete or foreign", taskID))
		}
		for actor, first := range want.attributions {
			if attrs[actor] != first {
				return composedCorruption(request.Allocation.OperationID, fmt.Sprintf("final attribution for allocated child %q is forged", taskID))
			}
		}
	}
	actualEdges := map[string]struct{}{}
	edgeRows, err := tx.Query(ctx, `SELECT source_id,target_id,kind_id FROM edges`)
	if err != nil {
		return err
	}
	for edgeRows.Next() {
		var s, t string
		var k int
		if err := edgeRows.Scan(&s, &t, &k); err != nil {
			_ = edgeRows.Close()
			return err
		}
		if states[s] != nil || states[t] != nil {
			actualEdges[fmt.Sprintf("%s\x00%s\x00%d", s, t, k)] = struct{}{}
		}
	}
	_ = edgeRows.Close()
	if len(actualEdges) != len(expectedEdges) {
		return composedCorruption(request.Allocation.OperationID, "final canonical edge projection has missing or foreign edges")
	}
	for edge := range expectedEdges {
		if _, ok := actualEdges[edge]; !ok {
			return composedCorruption(request.Allocation.OperationID, "final canonical edge projection disagrees with ordered effects")
		}
	}
	return nil
}

// ProveGovernedCanonicalConflictSnapshot performs the conflict proof in one
// read-only snapshot; DBOS failure diagnostics are deliberately not inputs.
func (db *DB) ProveGovernedCanonicalConflictSnapshot(ctx context.Context, operation journal.OperationID, submitted []byte) (err error) {
	err = db.withGovernedReadSnapshot(ctx, func(ctx context.Context, tx allocationSQLTx) error {
		return allocation.ProveCanonicalConflict(ctx, tx, operation, submitted, func(request allocation.ComposedRequest, authority journal.JournalID) error {
			in, prepared, prepareErr := allocation.SupplementalOperation(request, authority)
			if prepareErr != nil {
				return prepareErr
			}
			_, verifyErr := reconstructComposedSupplement(ctx, tx, in, prepared)
			return verifyErr
		})
	})
	return err
}

// ClassifyComposedGovernedAllocationSnapshot is the private, read-only durable
// identity gate used before the optional fresh-reference preflight.  A true
// result means the exact composed request and authority already have a durable
// receipt; false,nil means the identity is absent.  Any occupied-but-different
// identity is returned as the canonical typed governed conflict.
func (db *DB) ClassifyComposedGovernedAllocationSnapshot(ctx context.Context, request allocation.ComposedRequest, authority journal.JournalID) (exact bool, err error) {
	canonical, digest, err := allocation.CanonicalizeComposed(request)
	if err != nil {
		return false, err
	}
	err = db.withGovernedReadSnapshot(ctx, func(ctx context.Context, tx allocationSQLTx) error {
		var storedCanonical, storedDigest []byte
		var storedAuthority sql.NullInt64
		var kind allocation.RequestKind
		scanErr := tx.QueryRow(ctx, `SELECT g.request_kind,g.canonical_request,g.canonical_digest,o.authority_journal_id
			FROM governed_allocation_operations g JOIN journal_operations o ON o.journal_id=g.anchor_journal_id
			WHERE g.operation_id=?1`, string(request.Allocation.OperationID)).Scan(&kind, &storedCanonical, &storedDigest, &storedAuthority)
		if errors.Is(scanErr, sql.ErrNoRows) {
			// The governed lookup is deliberately followed by the global journal
			// lookup in this same snapshot.  This preserves identity-first
			// admission even when the submitted composed request contains a stale
			// or unauthorized reference.
			var governedAnchor int64
			governedErr := tx.QueryRow(ctx, `SELECT anchor_journal_id FROM governed_allocation_operations WHERE operation_id=?1`, string(request.Allocation.OperationID)).Scan(&governedAnchor)
			if governedErr == nil {
				return allocation.NewError(allocation.ErrorCorruption, request.Allocation.OperationID, "composed durable identity classification", "the governed receipt is orphaned from its global journal operation anchor", "the occupied identity is not trusted and fresh reference admission was not attempted", "restore the governed and journal operation rows from one consistent backup", nil)
			}
			if !errors.Is(governedErr, sql.ErrNoRows) {
				return allocation.NewError(allocation.ErrorCorruption, request.Allocation.OperationID, "composed durable identity classification", "the governed receipt occupancy could not be inspected", "identity classification stopped before reference admission", "repair the governed receipt table, then retry", governedErr)
			}
			var genericAnchor int64
			genericErr := tx.QueryRow(ctx, `SELECT journal_id FROM journal_operations WHERE operation_id=?1`, string(request.Allocation.OperationID)).Scan(&genericAnchor)
			if errors.Is(genericErr, sql.ErrNoRows) {
				return nil
			}
			if genericErr != nil {
				return allocation.NewError(allocation.ErrorCorruption, request.Allocation.OperationID, "composed durable identity classification", "global OperationID occupancy could not be inspected", "identity classification stopped before reference admission", "repair journal_operations, then retry", genericErr)
			}
			return proveCommittedOperationIDCollisionInTransaction(ctx, tx, request.Allocation.OperationID)
		}
		if scanErr != nil {
			return allocation.NewError(allocation.ErrorCorruption, request.Allocation.OperationID, "composed durable identity classification", "the occupied OperationID receipt could not be read", "fresh reference admission was not attempted and no workflow was started", "repair the governed operation receipt from a consistent backup, then retry", scanErr)
		}
		if kind != allocation.RequestKindAllocation || !bytes.Equal(storedCanonical, canonical) || !bytes.Equal(storedDigest, digest[:]) {
			return allocation.ProveCanonicalConflict(ctx, tx, request.Allocation.OperationID, canonical, func(storedRequest allocation.ComposedRequest, storedReceiptAuthority journal.JournalID) error {
				in, prepared, prepareErr := allocation.SupplementalOperation(storedRequest, storedReceiptAuthority)
				if prepareErr != nil {
					return prepareErr
				}
				_, verifyErr := reconstructComposedSupplement(ctx, tx, in, prepared)
				return verifyErr
			})
		}
		if !storedAuthority.Valid {
			return allocation.NewError(allocation.ErrorCorruption, request.Allocation.OperationID, "composed durable identity classification", "the exact composed receipt has no stored authority", "the receipt is not trusted and no workflow was started", "restore the governed operation and authority from one consistent backup", nil)
		}
		storedReceiptAuthority := journal.JournalID(storedAuthority.Int64)
		if _, verifyErr := allocation.VerifyComposedAllocationReceipt(ctx, tx, request, storedReceiptAuthority); verifyErr != nil {
			return verifyErr
		}
		in, prepared, prepareErr := allocation.SupplementalOperation(request, storedReceiptAuthority)
		if prepareErr != nil {
			return prepareErr
		}
		if _, verifyErr := reconstructComposedSupplement(ctx, tx, in, prepared); verifyErr != nil {
			return verifyErr
		}
		if storedReceiptAuthority != authority {
			return allocation.NewError(allocation.ErrorConflict, request.Allocation.OperationID, "composed durable identity classification", "the OperationID already belongs to different canonical request or authority bytes", "fresh reference admission was bypassed and the changed request wrote nothing", "retry the original exact composed request and authority, or choose a fresh OperationID", nil)
		}
		exact = true
		return nil
	})
	return exact, err
}

// ProveCommittedOperationIDCollisionSnapshot distinguishes a structurally valid
// non-governed journal operation occupying a caller's OperationID from malformed
// durable state. The complete proof is pinned to one read snapshot.
func (db *DB) ProveCommittedOperationIDCollisionSnapshot(ctx context.Context, operation journal.OperationID) (err error) {
	err = db.withGovernedReadSnapshot(ctx, func(ctx context.Context, tx allocationSQLTx) error {
		return proveCommittedOperationIDCollisionInTransaction(ctx, tx, operation)
	})
	return err
}

// proveCommittedOperationIDCollisionInTransaction authenticates a generic
// journal receipt without opening another snapshot.  Callers that have already
// observed occupancy use it to distinguish a global conflict from corruption.
func proveCommittedOperationIDCollisionInTransaction(ctx context.Context, tx allocationSQLTx, operation journal.OperationID) error {
	var anchor int64
	var kind int
	var actor sql.NullString
	var producer sql.NullInt64
	var recordedAt int64
	var authority sql.NullInt64
	var digest []byte
	var version string
	var canonical []byte
	if scanErr := tx.QueryRow(ctx, `SELECT o.journal_id,j.kind_id,j.actor_id,j.produced_by_operation_journal_id,j.recorded_at,o.authority_journal_id,o.mutation_digest,o.mutation_encoding_version,o.canonical_mutation
			FROM journal_operations o JOIN journal j ON j.journal_id=o.journal_id WHERE o.operation_id=?1`, string(operation)).Scan(&anchor, &kind, &actor, &producer, &recordedAt, &authority, &digest, &version, &canonical); scanErr != nil {
		return allocation.NewError(allocation.ErrorCorruption, operation, "global OperationID collision proof", "the reportedly committed operation cannot be loaded", "the collision is not trusted", "restore the journal operation from a consistent backup", scanErr)
	}
	prepared, decodeErr := journal.DecodeCanonicalMutation(canonical)
	if decodeErr != nil || kind != int(journal.JournalKindOperation) || !actor.Valid || producer.Valid || prepared.EncodingVersion().String() != version || !bytes.Equal(prepared.DerivedDigest(), digest) {
		return allocation.NewError(allocation.ErrorCorruption, operation, "global OperationID collision proof", "the occupied operation is not a structurally valid canonical operation receipt", "the collision is not trusted", "repair the operation anchor and canonical mutation from one backup", decodeErr)
	}
	parsedActor, parseErr := journalParseActor(actor.String)
	if parseErr != nil {
		return allocation.NewError(allocation.ErrorCorruption, operation, "global OperationID collision proof", "the occupied operation anchor has a malformed actor identity", "the collision is not trusted", "restore the operation anchor from the same backup as its canonical mutation", parseErr)
	}
	if verifyErr := authenticateGenericCanonicalReceipt(ctx, tx, operation, canonicalStoredOperation{
		anchor: anchor, actor: parsedActor, recordedAt: recordedAt, authority: nullableJournalID(authority),
	}, prepared.NormalizedEffects()); verifyErr != nil {
		return allocation.NewError(allocation.ErrorCorruption, operation, "global OperationID collision proof", "the occupied operation's ordered canonical produced-row closure is incomplete or forged", "the collision is not trusted", "restore the operation anchor, produced rows, subtype/context rows, and result slots from one consistent backup", verifyErr)
	}
	if _, reconstructErr := reconstructCommitted(ctx, tx, anchor); reconstructErr != nil {
		return allocation.NewError(allocation.ErrorCorruption, operation, "global OperationID collision proof", "the occupied canonical operation result cannot be reconstructed", "the collision is not trusted", "repair the operation-produced rows and result slots", reconstructErr)
	}
	return allocation.NewError(allocation.ErrorConflict, operation, "global OperationID collision proof", "the OperationID already belongs to a valid committed non-governed journal operation", "the governed request was not executed", "retry with a fresh OperationID", nil)
}

func nullableJournalID(value sql.NullInt64) *journal.JournalID {
	if !value.Valid {
		return nil
	}
	id := journal.JournalID(value.Int64)
	return &id
}

// authenticateGenericCanonicalReceipt reuses the startup canonical row and
// slot validators against the caller's already-pinned read transaction. It is
// intentionally receipt-local: projections and lifecycle are outside this
// identity classification boundary.
func authenticateGenericCanonicalReceipt(ctx context.Context, tx allocationSQLTx, operationID journal.OperationID, operation canonicalStoredOperation, effects []journal.Effect) error {
	scope := &connScope{ctx: ctx, conn: tx.conn, projectionTarget: projectionTargetLive}
	contextSchema, err := scope.classifyFactContextSchema()
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT journal_id,kind_id,actor_id,produced_by_operation_journal_id,recorded_at FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id`, operation.anchor)
	if err != nil {
		return err
	}
	defer rows.Close()
	produced := make([]int64, 0, len(effects))
	for rows.Next() {
		var id, kind, producer, recordedAt int64
		var actor sql.NullString
		if err := rows.Scan(&id, &kind, &actor, &producer, &recordedAt); err != nil {
			return err
		}
		index := len(produced)
		if index >= len(effects) {
			return fmt.Errorf("canonical operation %d has extra produced row %d", operation.anchor, id)
		}
		expectedKind, kindErr := effects[index].Sort.JournalKind()
		expectedRecordedAt := operation.recordedAt
		if effects[index].RecordedAtOverride != nil {
			expectedRecordedAt = *effects[index].RecordedAtOverride
		}
		if kindErr != nil || journal.JournalKind(kind) != expectedKind || actor.Valid || producer != operation.anchor || recordedAt != expectedRecordedAt || id <= operation.anchor {
			return fmt.Errorf("canonical operation %d produced row %d disagrees with effect %d kind, producer, actor placement, or chronology", operation.anchor, id, index)
		}
		produced = append(produced, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(produced) != len(effects) {
		return fmt.Errorf("canonical operation %d produced-row count is %d, want %d", operation.anchor, len(produced), len(effects))
	}
	if err := scope.validateCanonicalResultSlots(operation.anchor, produced, effects); err != nil {
		return err
	}
	for index, effect := range effects {
		var err error
		if effect.Sort == journal.EffectActivityCreate {
			err = validateCanonicalComposedEffect(ctx, tx, operationID, produced[index], effect)
		} else {
			err = scope.validateCanonicalEffectRow(operation, produced[index], effect, contextSchema == factContextSchemaCanonical)
		}
		if err != nil {
			return fmt.Errorf("canonical operation %d effect %d: %w", operation.anchor, index, err)
		}
	}
	return nil
}

func (db *DB) withGovernedReadSnapshot(ctx context.Context, read func(context.Context, allocationSQLTx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	scope, err := db.bindScope(ctx, projectionTargetLive)
	if err != nil {
		return fmt.Errorf("lease SQLite connection for governed receipt reconstruction: %w", err)
	}
	defer scope.release()
	return runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error { return read(scope.ctx, allocationSQLTx{conn: scope.conn}) })
}

// InitializeGovernedRoot owns the standalone BEGIN IMMEDIATE transaction for
// the one root initialization operation, then delegates every mutation and
// reconstruction step to the transaction-scoped allocation reducer.
func (db *DB) InitializeGovernedRoot(ctx context.Context, request allocation.RootGenesisRequest) (closure allocation.OperationClosure, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	scope, err := db.bindScope(ctx, projectionTargetLive)
	if err != nil {
		return allocation.OperationClosure{}, fmt.Errorf("InitializeGovernedRoot: lease SQLite connection: %w", err)
	}
	defer scope.release()
	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		closure, err = allocation.ReduceGenesis(scope.ctx, allocationSQLTx{conn: scope.conn}, request)
		return err
	}); err != nil {
		return allocation.OperationClosure{}, err
	}
	return closure, nil
}

// AllocateGovernedForAuthority additionally proves a Session's bound authority
// is the exact start authority of the request parent before allocation work. It
// retains the baseline simple allocation receipt and is the standalone entry
// point used by Session.AllocateGoverned.
func (db *DB) AllocateGovernedForAuthority(ctx context.Context, request allocation.GovernedAllocationRequest, authority journal.JournalID) (closure allocation.OperationClosure, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	scope, err := db.bindScope(ctx, projectionTargetLive)
	if err != nil {
		return allocation.OperationClosure{}, fmt.Errorf("AllocateGoverned: lease SQLite connection: %w", err)
	}
	defer scope.release()
	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		closure, err = allocation.ReduceAllocation(scope.ctx, allocationSQLTx{conn: scope.conn}, request, authority)
		return err
	}); err != nil {
		return allocation.OperationClosure{}, err
	}
	return closure, nil
}

// AllocateGovernedComposedForAuthority is the standalone transaction-owned
// counterpart of the fused composition path. It retains BEGIN IMMEDIATE for
// standalone callers and delegates all allocation plus supplemental reduction
// to the same transaction-scoped reducer used by DBOS.
func (db *DB) AllocateGovernedComposedForAuthority(ctx context.Context, request allocation.ComposedRequest, authority journal.JournalID) (result allocation.ComposedResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	scope, err := db.bindScope(ctx, projectionTargetLive)
	if err != nil {
		return allocation.ComposedResult{}, fmt.Errorf("AllocateGovernedComposed: lease SQLite connection: %w", err)
	}
	defer scope.release()
	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		result, err = ReduceComposedGovernedAllocation(scope.ctx, allocationSQLTx{conn: scope.conn}, request, authority)
		return err
	}); err != nil {
		return allocation.ComposedResult{}, err
	}
	return result, nil
}
