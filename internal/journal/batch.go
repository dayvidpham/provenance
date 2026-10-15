package journal

import (
	"errors"
	"fmt"
)

// batch.go defines the public batch-apply surface: one call that commits a
// bounded batch of canonical operations in ONE SQLite transaction, reporting a
// per-record outcome for every input. It is the batched counterpart of Apply
// (§9): the reducer folds each record through the SAME per-operation steps
// Apply uses, so the §9.4 idempotent-replay short-circuit, the §11 conflict
// classification, the per-effect authorization, and the post-fold integrity
// gates are all preserved per record, and a record is never more lenient in a
// batch than it is through Apply alone.
//
// DURABILITY IS ALL-OR-NOTHING. The batch is one BEGIN IMMEDIATE transaction: if
// every record commits or short-circuits the transaction commits and every newly
// committed record is durable; if any record fails the transaction rolls back
// and NO record this batch newly folded is durable. A short-circuited record is
// the one exception, and it is not a write of this batch: it was already durably
// committed by an earlier transaction and remains so whatever this batch does.

// MaxBatchRecords bounds one batch. It exists so a single call cannot stage an
// unbounded number of operations and hold SQLite write ownership for an
// unbounded time: the whole batch runs under one BEGIN IMMEDIATE, so the batch
// size is also the caller's latency budget, and a batch that outruns the busy
// window of every other writer on the store would starve them. The bound is
// generous (an order of magnitude above the daemon lifecycle lane's own batch
// budget of 20) while still finite; a caller that needs more records should make
// more than one call.
const MaxBatchRecords = 256

// BatchRecordOutcome is the closed disposition of ONE record within a batch
// apply. It reports DURABILITY, not the fold's internal state: an outcome is
// BatchRecordCommitted or BatchRecordShortCircuited exactly when the record is
// durably recorded after the call returns.
type BatchRecordOutcome int

const (
	// BatchRecordCommitted: this batch folded the record as a new operation and
	// the batch's single transaction committed, so the record is durably
	// recorded. It never accompanies a rolled-back batch.
	BatchRecordCommitted BatchRecordOutcome = iota
	// BatchRecordShortCircuited: the OperationID was already durably committed
	// with an identical identity, so §9.4's short-circuit returned the original
	// result and this batch wrote nothing for it. The record is durably recorded
	// from its earlier transaction whether or not this batch commits.
	BatchRecordShortCircuited
	// BatchRecordConflict: the OperationID is already committed with a differing
	// identity (§11). Nothing was written for this record and the batch rolled
	// back. Err carries the typed *OperationConflict.
	BatchRecordConflict
	// BatchRecordFailed: the record failed for another typed reason — a
	// pre-condition, authority, integrity, or input failure — so nothing was
	// written for it and the batch rolled back. Err carries that record's error,
	// the SAME error Apply returns for the same input.
	BatchRecordFailed
	// BatchRecordNotRecorded: the record is not durably recorded and did not
	// itself fail. This is the outcome of a record that was folded as a new
	// operation in a batch that then rolled back because a LATER record failed,
	// and of a record that was never folded because an EARLIER record failed. A
	// caller that needs to tell those two apart can read BatchApplyError.Index:
	// records before it were folded and rolled back, records after it were never
	// attempted.
	BatchRecordNotRecorded
)

var batchRecordOutcomeNames = [...]string{
	BatchRecordCommitted:      "committed",
	BatchRecordShortCircuited: "short-circuited",
	BatchRecordConflict:       "conflict",
	BatchRecordFailed:         "failed",
	BatchRecordNotRecorded:    "not-recorded",
}

func (o BatchRecordOutcome) String() string {
	if int(o) >= 0 && int(o) < len(batchRecordOutcomeNames) {
		return batchRecordOutcomeNames[o]
	}
	return fmt.Sprintf("BatchRecordOutcome(%d)", int(o))
}

// BatchRecordResult is one record's per-record outcome in a batch apply, aligned
// by index with the batch inputs. It is populated for every input, including the
// inputs after a failure, so a caller can map outcomes back to its own records
// positionally without guessing.
type BatchRecordResult struct {
	// Outcome is the record's closed durability disposition (see
	// BatchRecordOutcome).
	Outcome BatchRecordOutcome
	// Result is the record's reconstructed §3.2/§9.4 result: for
	// BatchRecordCommitted and BatchRecordShortCircuited the anchor, emitted-event
	// closure, and result slots (with ShortCircuited set on the short-circuit);
	// for BatchRecordConflict the CommittedConflict variant carrying the typed
	// *OperationConflict. It is the zero value for a record that never reached a
	// fold (BatchRecordFailed on input validation, or BatchRecordNotRecorded).
	Result CommittedResult
	// Err is the record's typed error, nil when the record committed or
	// short-circuited. It is the SAME error Apply returns for the same input, so
	// errors.Is(err, ErrOperationConflict), errors.As(err, &*OperationConflict),
	// and the condition/authority sentinels all hold.
	Err error
}

// BatchResult is the closed result of a batch apply. Records is always aligned
// by index with the inputs, one entry per input.
type BatchResult struct {
	// Committed is true iff the batch's single transaction committed. It is true
	// exactly when no record has an outcome other than BatchRecordCommitted or
	// BatchRecordShortCircuited. When it is false, every BatchRecordCommitted
	// outcome was reclassified to BatchRecordNotRecorded, because the rollback
	// undid the fold.
	Committed bool
	// Records is one BatchRecordResult per input, in input order.
	Records []BatchRecordResult
}

// BatchApplyError is the typed, actionable error a batch apply returns when the
// batch did not commit. It names the record that failed (Index, -1 when the
// failure was the transaction itself rather than one record's fold) and wraps
// that record's own error, so errors.Is and errors.As recover the identical
// taxonomy a per-record Apply would surface. A caller may switch on
// BatchResult.Records[i].Outcome instead of inspecting the error.
type BatchApplyError struct {
	// Index is the position of the failing record in the batch inputs, or -1
	// when the transaction failed for a reason not attributable to one record
	// (for example a COMMIT failure or a lost writer lock).
	Index int
	// OperationID is the failing record's OperationID, empty when Index is -1.
	OperationID OperationID
	// Cause is the failing record's typed error (or the transaction error when
	// Index is -1).
	Cause error
}

func (e *BatchApplyError) Error() string {
	where := "record " + fmt.Sprint(e.Index)
	if e.Index < 0 {
		where = "the batch transaction"
	}
	return fmt.Sprintf(
		"%v: batch apply did not commit — the whole batch rolled back and no record it "+
			"newly folded is durably recorded; the failure was in %s (OperationID %q) — "+
			"where: internal/sqlite batch apply, one BEGIN IMMEDIATE transaction; "+
			"when: while folding the batch; impact: zero new records committed (any "+
			"short-circuited record remains durable from its earlier transaction); "+
			"fix: retry the failing record, correct its input, or split the batch: %v",
		ErrBatchApply, where, e.OperationID, e.Cause)
}

func (e *BatchApplyError) Unwrap() error { return e.Cause }

// Is lets errors.Is recover the ErrBatchApply sentinel.
func (e *BatchApplyError) Is(target error) bool { return target == ErrBatchApply }

var (
	// ErrBatchApply classifies a batch apply that did not commit; the typed
	// *BatchApplyError carries the failing record's index and error.
	ErrBatchApply = errors.New("provenance: batch apply did not commit")
	// ErrBatchEmpty is returned for an empty batch. An empty batch is refused
	// rather than treated as a no-op transaction, because a caller that submits
	// nothing has almost certainly lost its records and should not read a
	// successful empty result as "they committed".
	ErrBatchEmpty = errors.New("provenance: batch apply requires at least one record")
	// ErrBatchTooLarge is returned when a batch exceeds MaxBatchRecords.
	ErrBatchTooLarge = errors.New("provenance: batch apply exceeds MaxBatchRecords")
)
