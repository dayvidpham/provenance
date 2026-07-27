package provenance

import (
	"context"
	"fmt"

	"github.com/dayvidpham/provenance/internal/allocation"
	"github.com/dayvidpham/provenance/internal/fusedtx"
)

// governedAllocationTransaction bridges the internal fused SQL contract to the
// public participant contract without letting DBOS or internal package types
// cross the public API boundary.
type governedAllocationTransaction struct {
	tx fusedtx.SQLTx
}

var _ GovernedAllocationTransaction = governedAllocationTransaction{}

func (tx governedAllocationTransaction) Exec(ctx context.Context, query string, args ...any) (GovernedAllocationSQLResult, error) {
	result, err := tx.tx.Exec(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (tx governedAllocationTransaction) Query(ctx context.Context, query string, args ...any) (GovernedAllocationSQLRows, error) {
	rows, err := tx.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (tx governedAllocationTransaction) QueryRow(ctx context.Context, query string, args ...any) GovernedAllocationSQLRow {
	return tx.tx.QueryRow(ctx, query, args...)
}

func copyGovernedAllocationRequest(request GovernedAllocationRequest) GovernedAllocationRequest {
	request.Children = append([]GovernedChildSpec(nil), request.Children...)
	return request
}

func copyOperationClosure(closure OperationClosure) OperationClosure {
	return allocation.NewClosure(closure.OperationID(), closure.Kind(), closure.AnchorJournalID(), closure.Children())
}

// governedAllocationParticipantFailure intentionally does not unwrap its
// source error. A participant can return an allocation.Error, but it must still
// fail the transaction as infrastructure rather than be converted into a
// durable typed domain rejection by resultFrom.
type governedAllocationParticipantFailure struct {
	operation OperationID
	cause     error
}

func newGovernedAllocationParticipantFailure(operation OperationID, cause error) error {
	return &governedAllocationParticipantFailure{operation: operation, cause: cause}
}

func (e *governedAllocationParticipantFailure) Error() string {
	return fmt.Sprintf(
		"provenance.FusedGovernedAllocator.allocateWorkflow: governed allocation participant failed for operation %q -- where: fused DBOS transaction after reducer success and before checkpoint; why: %v; impact: DBOS rolls back governed domain rows, participant writes, and the successful operation_outputs checkpoint; fix: make the participant's OperationID-bound audit write and immutable-data validation succeed before retrying",
		e.operation,
		e.cause,
	)
}
