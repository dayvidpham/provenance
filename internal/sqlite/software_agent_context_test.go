package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSoftwareAgentContextLeaseCancellation(t *testing.T) {
	t.Parallel()
	db := openPoolMemoryDB(t)
	db.db.SetMaxOpenConns(1)
	lease, err := db.bindScope(t.Context(), projectionTargetLive)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	before := db.db.Stats().WaitCount
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = db.RegisterSoftwareAgentContext(ctx, "test", "leased", "1", "source")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lease cancellation: %v", err)
	}
	if db.db.Stats().WaitCount <= before {
		t.Fatal("registration never waited for its lease")
	}
}
