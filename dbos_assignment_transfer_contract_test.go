package provenance_test

import (
	"context"

	"github.com/dayvidpham/provenance"
)

var _ func(
	*provenance.DBOSAdapter,
	context.Context,
	*provenance.Session,
	provenance.AssignmentTransferRequest,
	...provenance.ApplyOption,
) (provenance.AssignmentTransferResult, error) = (*provenance.DBOSAdapter).TransferAssignment
