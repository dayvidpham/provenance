package sqlite

import (
	"context"
	"fmt"

	"github.com/dayvidpham/provenance/internal/allocation"
	"github.com/dayvidpham/provenance/internal/journal"
)

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
// is the exact start authority of the request parent before allocation work.
// It is the only standalone governed-allocation entry point.
func (db *DB) AllocateGovernedForAuthority(ctx context.Context, request allocation.GovernedAllocationRequest, authority journal.JournalID) (closure allocation.OperationClosure, err error) {
	return db.allocateGoverned(ctx, request, authority)
}

func (db *DB) allocateGoverned(ctx context.Context, request allocation.GovernedAllocationRequest, authority journal.JournalID) (closure allocation.OperationClosure, err error) {
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
