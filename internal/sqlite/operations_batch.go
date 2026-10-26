package sqlite

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
)

// operations_batch.go implements the batched counterpart of Apply: one
// BEGIN IMMEDIATE transaction that folds a bounded batch of canonical operations
// through the SAME per-operation reducer step Apply uses, returning a per-record
// outcome for every input. It does not introduce a second fold path — every
// record runs scope.foldPreparedOperation, so the §9.4 replay short-circuit, the
// §11 conflict classification, the per-effect authorization, the close-ends
// gate, and the result-slot reconstruction are exactly those of a single Apply.
//
// The two STORE-WIDE integrity gates (verifySubtypeIntegrity and
// verifyActorPlacement) are deferred to the transaction's outer closure. When
// the verified-as-of ledger matches, the closure verifies only the journal rows
// this batch appended (verifyJournalDelta); when the ledger is stale or absent,
// it runs the full invariant over the final state. A failure still rolls the
// whole batch back. Running the full gates per record would make the batch
// O(records x journal) and erase the throughput the batch exists to deliver.
// Every PER-RECORD check is unchanged.

// preparedBatchOperation is one canonicalized batch record, ready to fold. The
// inputs are all prepared BEFORE write ownership is taken, mirroring
// ApplyContext's "canonicalize before acquiring write ownership" discipline, so
// a malformed record rejects the whole batch without ever opening a transaction.
type preparedBatchOperation struct {
	in                   journal.OperationInput
	prepared             journal.CanonicalMutation
	callerMutationDigest []byte
}

// ApplyBatch commits a bounded batch of operations in one transaction without a
// caller deadline; it is the no-deadline form of ApplyBatchContext.
func (db *DB) ApplyBatch(inputs []journal.OperationInput) (journal.BatchResult, error) {
	return db.ApplyBatchContext(context.Background(), inputs)
}

// ApplyBatchContext commits a bounded batch of operations atomically while
// bounding SQLite lock acquisition by ctx. A nil context has the same
// no-deadline behavior as ApplyBatch.
//
// SEMANTICS. All inputs are canonicalized and validated before any transaction
// opens; a failure there writes nothing and names the offending index. Then one
// BEGIN IMMEDIATE transaction folds each record in input order. Every record is
// folded through the same per-operation reducer step Apply uses, so each record
// keeps Apply's exact taxonomy: an identical committed OperationID short-circuits
// (§9.4), a differing one is a typed conflict (§11), and any other failure is the
// same typed error Apply would return. The batch commits only when every record
// either commits or short-circuits; the first failing record rolls the whole
// transaction back, so no record this batch newly folded is durably recorded and
// the batch can never leave a half-applied state. A busy writer lock is handled
// by the same bounded retry Apply uses (runImmediateTransaction).
//
// The returned BatchResult always carries one entry per input, aligned by index.
// On a rolled-back batch the newly folded records are reported
// BatchRecordNotRecorded (durability-honest), the failing record carries its
// typed error and outcome, and a short-circuited record stays
// BatchRecordShortCircuited because its earlier transaction is durable.
func (db *DB) ApplyBatchContext(ctx context.Context, inputs []journal.OperationInput) (res journal.BatchResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return journal.BatchResult{}, fmt.Errorf("ApplyBatch: caller deadline expired before preparing or leasing a SQLite connection; no transaction was opened and nothing was committed; retry with a live or longer context: %w", err)
	}
	if len(inputs) == 0 {
		return journal.BatchResult{}, fmt.Errorf(
			"%w: an empty batch was submitted — where: ApplyBatch input validation; when: before any "+
				"transaction; impact: nothing was committed; fix: submit at least one record, or do not "+
				"call ApplyBatch when there is nothing to commit",
			journal.ErrBatchEmpty)
	}
	if len(inputs) > journal.MaxBatchRecords {
		return journal.BatchResult{}, fmt.Errorf(
			"%w: %d records were submitted but MaxBatchRecords is %d — where: ApplyBatch input "+
				"validation; when: before any transaction; impact: nothing was committed; fix: split the "+
				"batch into calls of at most %d records so one transaction cannot hold write ownership "+
				"for an unbounded time",
			journal.ErrBatchTooLarge, len(inputs), journal.MaxBatchRecords, journal.MaxBatchRecords)
	}

	prepared := make([]preparedBatchOperation, len(inputs))
	for i := range inputs {
		in, canonical, callerMutationDigest, prepErr := prepareApplyInput(inputs[i])
		if prepErr != nil {
			// Pure input failure: no transaction has opened, so nothing was
			// written. Report every record as not recorded, name the offending
			// index, and preserve the per-record error taxonomy.
			records := notRecordedBatchRecords(len(inputs))
			records[i] = journal.BatchRecordResult{Outcome: journal.BatchRecordFailed, Err: prepErr}
			return journal.BatchResult{Committed: false, Records: records},
				&journal.BatchApplyError{Index: i, OperationID: inputs[i].OperationID, Cause: prepErr}
		}
		prepared[i] = preparedBatchOperation{in: in, prepared: canonical, callerMutationDigest: callerMutationDigest}
	}

	ctx, timings := sqlTimingOperation(ctx, "ApplyBatchContext", db)
	defer timings.flush()
	scope, leaseErr := db.bindScope(ctx, projectionTargetLive)
	if leaseErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return journal.BatchResult{}, fmt.Errorf("ApplyBatch: caller deadline expired while leasing a SQLite connection; no transaction was opened and nothing was committed; retry with a longer context or reduce pool contention: %w", ctxErr)
		}
		return journal.BatchResult{}, fmt.Errorf("ApplyBatch: lease pooled connection before write transaction: %w", leaseErr)
	}
	defer scope.release()

	// outcomes is pre-filled with BatchRecordNotRecorded for every input and
	// overwritten by the transaction closure as each record is folded, so a record
	// the closure never reached keeps the honest not-recorded outcome.
	outcomes := notRecordedBatchRecords(len(inputs))
	failIndex := -1
	transactionErr := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		// The sanctioned-write induction shares this one transaction: the
		// pre-check runs before the first fold over the pre-write snapshot, and
		// the ledger advance runs after the last fold and the once-per-batch
		// store-wide gates succeed over the post-batch state, so no writer can
		// interleave and a crash leaves both or neither. The advance is
		// best-effort — its failure never fails the batch.
		eligible := scope.verifiedLedgerEligibleForAdvance()
		before, err := scope.maxJournalID()
		if err != nil {
			return err
		}
		committedAny := false
		for i := range prepared {
			record, foldErr := scope.foldPreparedOperation(prepared[i].in, prepared[i].prepared, prepared[i].callerMutationDigest, foldOptions{deferStoreGates: true})
			if foldErr != nil {
				outcome := journal.BatchRecordFailed
				if record.Kind == journal.CommittedConflict {
					outcome = journal.BatchRecordConflict
				}
				outcomes[i] = journal.BatchRecordResult{Outcome: outcome, Result: record, Err: foldErr}
				failIndex = i
				return foldErr
			}
			if record.ShortCircuited {
				outcomes[i] = journal.BatchRecordResult{Outcome: journal.BatchRecordShortCircuited, Result: record}
				continue
			}
			outcomes[i] = journal.BatchRecordResult{Outcome: journal.BatchRecordCommitted, Result: record}
			committedAny = true
		}
		// When the verified ledger matches the pre-write snapshot, verifyJournalDelta
		// checks only the journal rows this batch appended. The full subtype and
		// actor gates run only in the else branch, once over the final state,
		// when the ledger is stale or absent. Either way a failure rolls the
		// whole batch back exactly as a per-record failure would.
		if committedAny {
			after, tailErr := scope.maxJournalID()
			if tailErr != nil {
				failIndex = -1
				return tailErr
			}
			if eligible {
				if gateErr := scope.verifyJournalDelta(before, after); gateErr != nil {
					failIndex = -1
					return gateErr
				}
			} else {
				if gateErr := scope.verifySubtypeIntegrity(); gateErr != nil {
					failIndex = -1
					return gateErr
				}
				if gateErr := scope.verifyActorPlacement(); gateErr != nil {
					failIndex = -1
					return gateErr
				}
			}
		}
		if eligible && committedAny {
			// Best-effort by contract: a failed advance leaves the ledger stale,
			// which is the fail-closed state the next open heals.
			_ = scope.advanceVerifiedLedger()
		}
		return nil
	})
	if transactionErr != nil {
		// The transaction rolled back. Reclassify every provisional committed
		// outcome as not recorded — durability-honest — and fill the records the
		// closure never reached. Short-circuits stay short-circuits: they are
		// durable from an earlier transaction, not from this rolled-back one.
		for i := range outcomes {
			if outcomes[i].Outcome == journal.BatchRecordCommitted {
				outcomes[i] = journal.BatchRecordResult{Outcome: journal.BatchRecordNotRecorded}
			}
		}
		// Deadline authority for the BEGIN leg lives in runScopedTransaction's
		// retry loop; this heuristic remains for the fold and COMMIT legs, where a
		// deadline can expire mid-transaction without a typed context error.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= 10*time.Millisecond {
			transactionErr = fmt.Errorf("caller deadline expired while acquiring SQLite write ownership; the batch transaction was rolled back and nothing was committed; retry with a longer context or reduce writer contention: %w", errors.Join(context.DeadlineExceeded, transactionErr))
		}
		operationID := journal.OperationID("")
		if failIndex >= 0 {
			operationID = prepared[failIndex].in.OperationID
		}
		return journal.BatchResult{Committed: false, Records: outcomes},
			&journal.BatchApplyError{Index: failIndex, OperationID: operationID, Cause: transactionErr}
	}
	return journal.BatchResult{Committed: true, Records: outcomes}, nil
}

// notRecordedBatchRecords returns one BatchRecordNotRecorded entry per record.
func notRecordedBatchRecords(n int) []journal.BatchRecordResult {
	records := make([]journal.BatchRecordResult, n)
	for i := range records {
		records[i] = journal.BatchRecordResult{Outcome: journal.BatchRecordNotRecorded}
	}
	return records
}
