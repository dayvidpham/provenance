//go:build !pasture_experiment

package sqlite

import (
	"context"
	"testing"
)

func TestSQLTimingsDefaultNoop(t *testing.T) {
	ctx := context.Background()
	if allocations := testing.AllocsPerRun(10, func() {
		got, recorder := sqlTimingOperation(ctx, "Apply")
		if got != ctx || recorder != nil {
			t.Fatal("default build enabled timing")
		}
		sqlTimingStart(got, "body").finish(nil)
		recorder.flush()
	}); allocations != 0 {
		t.Fatalf("disabled allocations = %v", allocations)
	}
}
