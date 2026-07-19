//go:build compilefail

package compilefail

// This file MUST NOT compile. Each function demonstrates a caller attempt the
// adapter's public surface deliberately makes a type error: passing a raw DBOS
// workflow/step option, an explicit workflow ID/application version/step name, or
// overriding the adapter's private durable identity. The fixture test asserts these
// diagnostics appear.

import (
	"context"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/dayvidpham/provenance"
)

// callerCannotPassWorkflowOption: Apply takes only (ctx, OperationInput); a raw
// DBOS WorkflowOption is too many arguments.
func callerCannotPassWorkflowOption(a *provenance.DBOSAdapter, op provenance.OperationInput) {
	_, _ = a.Apply(context.Background(), op, dbos.WithWorkflowID("caller-chosen-id"))
}

// callerCannotPassStepOption: likewise for a raw StepOption / step name.
func callerCannotPassStepOption(a *provenance.DBOSAdapter, op provenance.OperationInput) {
	_, _ = a.Apply(context.Background(), op, dbos.WithStepName("caller-step"))
}

// callerCannotPassApplicationVersion: no application-version override argument.
func callerCannotPassApplicationVersion(a *provenance.DBOSAdapter, op provenance.OperationInput) {
	_, _ = a.Apply(context.Background(), op, dbos.WithApplicationVersion("v9"))
}

// callerCannotOverrideDurableIdentity: the adapter's identity fields are unexported
// and unreachable from another package.
func callerCannotOverrideDurableIdentity(a *provenance.DBOSAdapter) {
	a.applicationVersion = "forged"
	a.testHooks = nil
}
