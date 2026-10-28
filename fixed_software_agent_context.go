package provenance

import "context"

// FixedSoftwareAgentRegistrarContext is an optional capability for atomic fixed
// software-agent registration using a caller's context. Tracker remains unchanged;
// trackers from OpenMemory, OpenSQLite and OpenBorrowedSQLite (including its
// context variant) implement this interface. The legacy registration method uses
// the same algorithm with context.Background.
//
// A nil or already-canceled context is refused before registration. The caller's
// context governs borrowed-handle preflight, connection leasing and operation SQL;
// registration makes one BEGIN attempt rather than retrying. Cancellation does
// not establish the result of a commit already in flight: the call waits for the
// transaction and connection cleanup to settle and returns their actual outcome.
// A successful commit can survive cancellation; a cleanup failure can leave the
// durable outcome uncertain and retires the connection. No registration continues
// after the method returns. Cleanup may extend the caller's cancellation deadline.
//
// In borrowed mode, connection creation and initialization belong to the pool's
// owner. Callers requiring bounded first-use setup must supply a connector that
// honors its Connect context; this capability does not change pool setup policy.
type FixedSoftwareAgentRegistrarContext interface {
	RegisterFixedSoftwareAgentContext(ctx context.Context, reg FixedSoftwareAgentRegistration) (SoftwareAgent, error)
}

var (
	_ FixedSoftwareAgentRegistrarContext = (*sqliteTracker)(nil)
	_ FixedSoftwareAgentRegistrarContext = (*borrowedTracker)(nil)
)
