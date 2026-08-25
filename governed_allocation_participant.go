package provenance

import "context"

// GovernedAllocationSQLResult reports the effect of a participant SQL write.
// It is intentionally independent of database/sql and DBOS driver types.
type GovernedAllocationSQLResult interface {
	RowsAffected() (int64, error)
}

// GovernedAllocationSQLRow is one row returned by a participant query.
type GovernedAllocationSQLRow interface {
	Scan(dest ...any) error
}

// GovernedAllocationSQLRows is an iterated participant query result. A
// participant must close it after use.
type GovernedAllocationSQLRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// GovernedAllocationTransaction is the narrow SQL capability supplied to a
// GovernedAllocationParticipant. It is the exact DBOS transaction that also
// contains the governed reducer and its operation_outputs checkpoint.
//
// It deliberately omits database handles, transaction lifecycle operations,
// and DBOS APIs. DBOS exclusively owns transaction begin, commit, rollback,
// retry, and durable checkpointing.
type GovernedAllocationTransaction interface {
	Exec(context.Context, string, ...any) (GovernedAllocationSQLResult, error)
	Query(context.Context, string, ...any) (GovernedAllocationSQLRows, error)
	QueryRow(context.Context, string, ...any) GovernedAllocationSQLRow
}

// GovernedAllocationParticipant writes one integration-owned projection or
// audit record after a governed allocation has reduced to a successful closure.
// A nil participant is disabled. Standalone Session.AllocateGoverned does not
// invoke participants.
//
// Request and closure are defensive copies. A participant can safely inspect
// them but cannot alter the caller's request, the reducer's persisted result, or
// a later replayed closure.
//
// DBOS exact workflow replay (the same workflow ID and canonical input) returns
// its durable output without re-entering this callback. A distinct workflow ID
// for an already committed OperationID does enter the reducer's reconstruction
// path and can invoke this callback again. DBOS does not expose a reliable
// newly-committed versus reconstructed signal at this boundary. Participants
// must therefore deduplicate by OperationID and validate immutable audit data
// such as the closure anchor and child row identities before treating a repeat
// as successful.
//
// A non-nil error is an infrastructure failure, not a typed domain rejection:
// DBOS rolls back the reducer's domain writes, participant writes, and a
// successful operation_outputs checkpoint together. DBOS may retain a failed
// checkpoint for its workflow error outcome.
type GovernedAllocationParticipant func(context.Context, GovernedAllocationTransaction, GovernedAllocationRequest, OperationClosure) error
