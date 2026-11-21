package provenance

import (
	"context"
	"fmt"

	"github.com/dayvidpham/provenance/internal/sqlite"
)

// AdmittedMLAgentRegistrarContext registers an exact model identity that the
// caller has already admitted. It does not change Tracker's registry policy.
type AdmittedMLAgentRegistrarContext interface {
	RegisterAdmittedMLAgentContext(ctx context.Context, namespace string, role Role, provider Provider, model ModelID) (MLAgent, error)
}

// DuplicateMLAgentError reports the existing identity for an exact registration.
type DuplicateMLAgentError = sqlite.DuplicateMLAgentError

var (
	_ AdmittedMLAgentRegistrarContext = (*sqliteTracker)(nil)
	_ AdmittedMLAgentRegistrarContext = (*borrowedTracker)(nil)
)

func (t *sqliteTracker) RegisterAdmittedMLAgentContext(ctx context.Context, namespace string, role Role, provider Provider, model ModelID) (MLAgent, error) {
	return t.db.RegisterAdmittedMLAgentContext(ctx, namespace, role, provider, model)
}

func (b *borrowedTracker) RegisterAdmittedMLAgentContext(ctx context.Context, namespace string, role Role, provider Provider, model ModelID) (MLAgent, error) {
	const op = "RegisterAdmittedMLAgentContext"
	if ctx == nil {
		return MLAgent{}, fmt.Errorf("%s: nil context; no agent created; supply a live context", op)
	}
	if err := b.availableContext(ctx, op); err != nil {
		return MLAgent{}, registrationContextError(ctx, err)
	}
	registrar, ok := b.inner.(AdmittedMLAgentRegistrarContext)
	if !ok {
		return MLAgent{}, fmt.Errorf("%s: borrowed tracker lacks admitted ML registration; no agent created; use a backend with this capability", op)
	}
	return registrar.RegisterAdmittedMLAgentContext(ctx, namespace, role, provider, model)
}
