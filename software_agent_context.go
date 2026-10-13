package provenance

import (
	"context"
	"errors"
)

// SoftwareAgentRegistrarContext is an additive capability implemented by trackers
// returned by OpenSQLite, OpenMemory, and OpenBorrowedSQLite. Tracker is unchanged
// so existing implementations remain source-compatible. Callers can assert this
// interface when registration must honor cancellation.
type SoftwareAgentRegistrarContext interface {
	// RegisterSoftwareAgentContext registers a fresh UUIDv7 software agent. ctx
	// controls pool acquisition and the single registration transaction. A canceled
	// waiter stops within the connection's busy tier, without retrying registration.
	// The tracker does not retain ctx. Connection setup remains pool-owner policy.
	RegisterSoftwareAgentContext(ctx context.Context, namespace, name, version, source string) (SoftwareAgent, error)
}

var (
	_ SoftwareAgentRegistrarContext = (*sqliteTracker)(nil)
	_ SoftwareAgentRegistrarContext = (*borrowedTracker)(nil)
)

// Preserve both cancellation identity and a caller-supplied Cause without
// repeating an error already present in the chain.
func registrationContextError(ctx context.Context, err error) error {
	if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
		err = errors.Join(err, ctx.Err())
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(err, cause) {
		err = errors.Join(err, cause)
	}
	return err
}
