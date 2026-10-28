package provenance

import "context"

// FixedSoftwareAgentRegistrarContext is an optional capability for atomic fixed
// software-agent registration using a caller's context. Tracker remains unchanged.
// Cancellation does not establish the result of a commit already in flight;
// the call waits for the transaction and connection cleanup to settle.
type FixedSoftwareAgentRegistrarContext interface {
	RegisterFixedSoftwareAgentContext(ctx context.Context, reg FixedSoftwareAgentRegistration) (SoftwareAgent, error)
}

var (
	_ FixedSoftwareAgentRegistrarContext = (*sqliteTracker)(nil)
	_ FixedSoftwareAgentRegistrarContext = (*borrowedTracker)(nil)
)
