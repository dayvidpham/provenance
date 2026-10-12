package provenance

import (
	"context"
	"database/sql"
	"testing"
)

// An existing external implementation can implement Tracker without the optional
// registration capability. The borrowed decorator must not panic on it.
type legacyRegistrationTracker struct{ Tracker }

func TestSoftwareAgentContextMissingCapability(t *testing.T) {
	t.Parallel()
	pool, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	b := &borrowedTracker{inner: legacyRegistrationTracker{}, sentinel: pool}
	_, err = b.RegisterSoftwareAgentContext(context.Background(), "test", "agent", "1", "source")
	missing, ok := AsStoreUnavailable(err)
	if !ok || missing.Operation != "RegisterSoftwareAgentContext" || missing.Stage != "capability check before registration" {
		t.Fatalf("missing capability: %v", err)
	}
}
