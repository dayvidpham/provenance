package provenance_test

import "github.com/dayvidpham/provenance"

var _ func(
	*provenance.Session,
	provenance.AssignmentTransferRequest,
	...provenance.ApplyOption,
) (provenance.AssignmentTransferResult, error) = (*provenance.Session).TransferAssignment
