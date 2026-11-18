package sqlite

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dayvidpham/provenance/internal/journal"
	ptypes "github.com/dayvidpham/provenance/pkg/ptypes"
)

func withCanonicalDecodeWorkers(t *testing.T, n int) {
	t.Helper()
	canonicalDecodeWorkersOverride.Store(int32(n))
	t.Cleanup(func() { canonicalDecodeWorkersOverride.Store(0) })
}

func TestCanonicalDecodeWorkersDefaultIsBounded(t *testing.T) {
	canonicalDecodeWorkersOverride.Store(0)
	got := canonicalDecodeWorkers()
	if got < 1 || got > canonicalDecodeMaxWorkers {
		t.Fatalf("canonicalDecodeWorkers() = %d; want within [1,%d]", got, canonicalDecodeMaxWorkers)
	}
	if want := runtime.GOMAXPROCS(0); want <= canonicalDecodeMaxWorkers && got != want {
		t.Fatalf("canonicalDecodeWorkers() = %d; want GOMAXPROCS %d", got, want)
	}
	withCanonicalDecodeWorkers(t, 3)
	if got := canonicalDecodeWorkers(); got != 3 {
		t.Fatalf("override = %d; want 3", got)
	}
}

func TestForEachOrderedConsumesInIndexOrder(t *testing.T) {
	for _, workers := range []int{1, 2, 4, 16} {
		for _, n := range []int{0, 1, canonicalDecodeChunk - 1, canonicalDecodeChunk, canonicalDecodeChunk + 1, canonicalDecodeChunk*7 + 13} {
			t.Run(fmt.Sprintf("workers-%d-n-%d", workers, n), func(t *testing.T) {
				next := 0
				err := forEachOrdered(n, workers, func(i int) int { return i * 3 }, func(i, r int) error {
					if i != next || r != i*3 {
						t.Fatalf("consumed (%d,%d); want (%d,%d)", i, r, next, next*3)
					}
					next++
					return nil
				})
				if err != nil || next != n {
					t.Fatalf("walk = (%d consumed, %v); want (%d, nil)", next, err, n)
				}
			})
		}
	}
}

// The reported error must be the first in index order even when a later index
// fails sooner in wall-clock time.
func TestForEachOrderedReportsFirstErrorInIndexOrder(t *testing.T) {
	const n = canonicalDecodeChunk * 20
	errEarly, errLate := errors.New("early"), errors.New("late")
	for _, workers := range []int{1, 2, 8, 16} {
		t.Run(fmt.Sprintf("workers-%d", workers), func(t *testing.T) {
			produce := func(i int) error {
				if i == n/4 {
					time.Sleep(20 * time.Millisecond) // finishes after the late failure is produced
				}
				return nil
			}
			consumed := 0
			err := forEachOrdered(n, workers, produce, func(i int, _ error) error {
				consumed++
				switch i {
				case n / 4:
					return errEarly
				case n - 1:
					return errLate
				}
				return nil
			})
			if !errors.Is(err, errEarly) {
				t.Fatalf("error = %v; want the first in index order (%v)", err, errEarly)
			}
			if consumed != n/4+1 {
				t.Fatalf("consumed %d items; want the walk to stop at index %d", consumed, n/4)
			}
		})
	}
}

// Producers may not run unboundedly ahead of a slow consumer, and every worker
// is gone when the walk returns, success or failure.
func TestForEachOrderedBoundsInFlightWorkAndJoinsWorkers(t *testing.T) {
	const workers = 4
	const n = canonicalDecodeChunk * 64
	var produced, consumedCount atomic.Int64
	var maxAhead atomic.Int64
	before := runtime.NumGoroutine()
	stop := errors.New("stop")
	err := forEachOrdered(n, workers, func(i int) int {
		ahead := produced.Add(1) - consumedCount.Load()
		for {
			cur := maxAhead.Load()
			if ahead <= cur || maxAhead.CompareAndSwap(cur, ahead) {
				break
			}
		}
		return i
	}, func(i, _ int) error {
		consumedCount.Add(1)
		if i%canonicalDecodeChunk == 0 {
			time.Sleep(time.Millisecond) // slow consumer
		}
		if i == n/2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("error = %v; want stop", err)
	}
	limit := int64(workers*canonicalDecodeWindowPerWorker+workers) * canonicalDecodeChunk
	if got := maxAhead.Load(); got > limit {
		t.Fatalf("producers ran %d items ahead of the consumer; bound is %d", got, limit)
	}
	if got := produced.Load(); got >= n {
		t.Fatalf("produced %d of %d items after an early stop; want the walk cancelled", got, n)
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines after walk = %d; want <= %d (workers must be joined)", after, before)
	}
}

// canonicalDecodeStore builds a store with enough canonical operations to span
// several decode chunks, and returns the anchors of the decision operations in
// anchor order.
func canonicalDecodeStore(t *testing.T, operations int) (*DB, []int64) {
	t.Helper()
	db := newJournalDB(t)
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	seedActor(t, db, actor)
	authority := genesisBoot(t, db, actor)
	var anchors []int64
	for start := 0; start < operations; start += 64 {
		end := start + 64
		if end > operations {
			end = operations
		}
		batch := make([]journal.OperationInput, 0, end-start)
		for i := start; i < end; i++ {
			batch = append(batch, batchDecisionInput(fmt.Sprintf("decode-pool-%05d", i), actor, authority, fmt.Sprintf(`{"i":%d}`, i)))
		}
		res, err := db.ApplyBatch(batch)
		if err != nil || !res.Committed {
			t.Fatalf("ApplyBatch [%d,%d) = %+v, %v", start, end, res, err)
		}
		for _, record := range res.Records {
			anchors = append(anchors, int64(record.Result.AnchorJournalID))
		}
	}
	return db, anchors
}

func execCanonicalDecodeDamage(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, query, args...); err != nil {
		t.Fatalf("damage %q: %v", query, err)
	}
}

func canonicalDecodeValidationErrors(t *testing.T, db *DB, workers int) (opErr, factErr error) {
	t.Helper()
	withCanonicalDecodeWorkers(t, workers)
	scope := takePoolScope(t, db)
	defer scope.release()
	opErr = scope.validateCanonicalOperations()
	factErr = scope.validateCanonicalFactContextSets()
	return opErr, factErr
}

type canonicalDecodeDamage struct {
	query string
	args  []any
}

// TestParallelCanonicalValidationMatchesSequential proves the pool is a pure
// optimisation: for a clean store and for stores with several damaged
// operations, every worker count reports exactly the error the one-worker
// (sequential) walk reports, which is the first in anchor order.
func TestParallelCanonicalValidationMatchesSequential(t *testing.T) {
	const operations = canonicalDecodeChunk*3 + 17
	for _, tc := range []struct {
		name   string
		damage func(anchors []int64) []canonicalDecodeDamage
		// wantOp is a substring the sequential validateCanonicalOperations error must contain.
		wantOp string
	}{
		{name: "clean"},
		{
			name: "late-decode-failure",
			damage: func(a []int64) []canonicalDecodeDamage {
				return []canonicalDecodeDamage{{"UPDATE journal_operations SET canonical_mutation=x'00' WHERE journal_id=?1", []any{a[len(a)-3]}}}
			},
			wantOp: "startup canonical validation operation",
		},
		{
			name: "digest-before-decode-failure",
			damage: func(a []int64) []canonicalDecodeDamage {
				return []canonicalDecodeDamage{
					{"UPDATE journal_operations SET mutation_digest=x'00' WHERE journal_id=?1", []any{a[canonicalDecodeChunk+5]}},
					{"UPDATE journal_operations SET canonical_mutation=x'00' WHERE journal_id=?1", []any{a[canonicalDecodeChunk*2+30]}},
				}
			},
			wantOp: "field mutation digest diverged",
		},
		{
			// A database-backed check on an early operation must beat a decode
			// failure on a later one, although the pool decodes the later one first.
			name: "database-check-before-decode-failure",
			damage: func(a []int64) []canonicalDecodeDamage {
				return []canonicalDecodeDamage{
					{"UPDATE journal_operations SET authority_journal_id=NULL WHERE journal_id=?1", []any{a[10]}},
					{"UPDATE journal_operations SET canonical_mutation=x'00' WHERE journal_id=?1", []any{a[canonicalDecodeChunk*2+30]}},
				}
			},
			wantOp: "field authority diverged",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, anchors := canonicalDecodeStore(t, operations)
			if tc.damage != nil {
				for _, d := range tc.damage(anchors) {
					execCanonicalDecodeDamage(t, db, d.query, d.args...)
				}
			}
			wantOp, wantFact := canonicalDecodeValidationErrors(t, db, 1)
			if tc.damage == nil {
				if wantOp != nil || wantFact != nil {
					t.Fatalf("clean store sequential validation = (%v, %v); want nil", wantOp, wantFact)
				}
			} else if wantOp == nil || !strings.Contains(wantOp.Error(), tc.wantOp) {
				t.Fatalf("sequential validateCanonicalOperations = %v; want it to contain %q", wantOp, tc.wantOp)
			}
			for _, workers := range []int{2, 3, 4, 8, 16} {
				gotOp, gotFact := canonicalDecodeValidationErrors(t, db, workers)
				if fmt.Sprint(gotOp) != fmt.Sprint(wantOp) {
					t.Fatalf("workers=%d validateCanonicalOperations = %v; want sequential result %v", workers, gotOp, wantOp)
				}
				if fmt.Sprint(gotFact) != fmt.Sprint(wantFact) {
					t.Fatalf("workers=%d validateCanonicalFactContextSets = %v; want sequential result %v", workers, gotFact, wantFact)
				}
			}
		})
	}
}

// TestParallelCanonicalValidationReportsFirstAnchor pins which error wins when
// two operations are damaged differently: the lower anchor, whatever the kind.
func TestParallelCanonicalValidationReportsFirstAnchor(t *testing.T) {
	const operations = canonicalDecodeChunk*3 + 17
	db, anchors := canonicalDecodeStore(t, operations)
	early, late := anchors[canonicalDecodeChunk+5], anchors[canonicalDecodeChunk*2+60]
	execCanonicalDecodeDamage(t, db, "UPDATE journal_operations SET mutation_digest=x'00' WHERE journal_id=?1", early)
	execCanonicalDecodeDamage(t, db, "UPDATE journal_operations SET canonical_mutation=x'00' WHERE journal_id=?1", late)
	wantOp := fmt.Sprintf("canonical operation %d field mutation digest diverged", early)
	wantFact := fmt.Sprintf("decode canonical operation %d for fact-context validation", late)
	for _, workers := range []int{1, 2, 4, 8, 16} {
		gotOp, gotFact := canonicalDecodeValidationErrors(t, db, workers)
		if gotOp == nil || !strings.Contains(gotOp.Error(), wantOp) {
			t.Fatalf("workers=%d validateCanonicalOperations = %v; want it to name the earlier anchor: %q", workers, gotOp, wantOp)
		}
		// The fact-context walk ignores the digest, so its first failure is the decode.
		if gotFact == nil || !strings.Contains(gotFact.Error(), wantFact) {
			t.Fatalf("workers=%d validateCanonicalFactContextSets = %v; want %q", workers, gotFact, wantFact)
		}
	}
}
