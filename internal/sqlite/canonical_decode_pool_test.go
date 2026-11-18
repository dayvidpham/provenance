package sqlite

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
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
	for _, workers := range []int{1, 2, 3, 4, 8, 16} {
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
// fails sooner in wall-clock time. The early index's producer is held until the
// late index has been produced (and has produced its error), so the late failure
// is certain to exist before the early one is even delivered.
func TestForEachOrderedReportsFirstErrorInIndexOrder(t *testing.T) {
	const n = canonicalDecodeChunk * 20
	const earlyIdx, lateIdx = canonicalDecodeChunk + 7, canonicalDecodeChunk*3 + 5
	errEarly, errLate := errors.New("early"), errors.New("late")
	for _, workers := range []int{2, 8, 16} {
		t.Run(fmt.Sprintf("workers-%d", workers), func(t *testing.T) {
			lateProduced := make(chan struct{})
			var gateTimedOut atomic.Bool
			produce := func(i int) error {
				switch i {
				case lateIdx:
					close(lateProduced)
					return errLate
				case earlyIdx:
					select {
					case <-lateProduced:
					case <-time.After(10 * time.Second):
						gateTimedOut.Store(true)
					}
					return errEarly
				}
				return nil
			}
			consumed := 0
			lateBeforeEarlyConsumed := false
			err := forEachOrdered(n, workers, produce, func(i int, produced error) error {
				consumed++
				if produced == nil {
					return nil
				}
				select {
				case <-lateProduced:
					lateBeforeEarlyConsumed = true
				default:
				}
				return produced
			})
			if gateTimedOut.Load() {
				t.Fatalf("the late chunk was never produced while the early chunk was held; the pool cannot run %d chunks ahead at %d workers", lateIdx/canonicalDecodeChunk-earlyIdx/canonicalDecodeChunk, workers)
			}
			if !lateBeforeEarlyConsumed {
				t.Fatal("the late failure was not produced before the early failure was consumed; the case no longer exercises out-of-order production")
			}
			if !errors.Is(err, errEarly) {
				t.Fatalf("error = %v; want the first in index order (%v)", err, errEarly)
			}
			if consumed != earlyIdx+1 {
				t.Fatalf("consumed %d items; want the walk to stop at index %d", consumed, earlyIdx)
			}
		})
	}
}

// TestForEachOrderedInFlightChunksNeverExceedWindow measures the real bound: a
// chunk is in flight from the moment its first index is produced until its last
// index is consumed, and at most workers*2 may be in flight. The first consume
// holds until the producers have claimed every chunk they may, so the maximum
// must reach the window (saturation) and never pass it. The window size is the
// literal 2 on purpose: raising the production constant must fail this test.
func TestForEachOrderedInFlightChunksNeverExceedWindow(t *testing.T) {
	for _, workers := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("workers-%d", workers), func(t *testing.T) {
			window := workers * 2
			n := canonicalDecodeChunk * (window + 12)
			var started, finished, maxInFlight atomic.Int64
			produce := func(i int) int {
				if i%canonicalDecodeChunk == 0 {
					inFlight := started.Add(1) - finished.Load()
					for {
						cur := maxInFlight.Load()
						if inFlight <= cur || maxInFlight.CompareAndSwap(cur, inFlight) {
							break
						}
					}
					if (i/canonicalDecodeChunk)%3 == 0 {
						time.Sleep(200 * time.Microsecond) // jitter: chunks finish out of order
					}
				}
				return i
			}
			next := 0
			err := forEachOrdered(n, workers, produce, func(i, r int) error {
				if i != next || r != i {
					t.Fatalf("consumed (%d,%d); want index %d in order", i, r, next)
				}
				next++
				if i == 0 {
					deadline := time.Now().Add(10 * time.Second)
					for started.Load() < int64(window) {
						if time.Now().After(deadline) {
							t.Fatalf("producers claimed %d chunks while the consumer was held; want the window of %d to fill", started.Load(), window)
						}
						time.Sleep(time.Millisecond)
					}
					time.Sleep(20 * time.Millisecond) // give an oversized window time to over-claim
				}
				if i%canonicalDecodeChunk == canonicalDecodeChunk-1 {
					finished.Add(1)
				}
				return nil
			})
			if err != nil || next != n {
				t.Fatalf("walk = (%d consumed, %v); want (%d, nil)", next, err, n)
			}
			if got := maxInFlight.Load(); got != int64(window) {
				t.Fatalf("max chunks in flight = %d; want exactly the window %d (saturated, never exceeded)", got, window)
			}
		})
	}
}

// producerLedger records producer activity without runtime.NumGoroutine.
type producerLedger struct {
	active   atomic.Int64
	started  atomic.Int64
	finished atomic.Int64
}

func (l *producerLedger) enter() func() {
	l.active.Add(1)
	l.started.Add(1)
	return func() {
		l.finished.Add(1)
		l.active.Add(-1)
	}
}

func (l *producerLedger) requireQuiescent(t *testing.T, context string) {
	t.Helper()
	if active := l.active.Load(); active != 0 {
		t.Fatalf("%s: %d producers still running after forEachOrdered returned", context, active)
	}
	if started, finished := l.started.Load(), l.finished.Load(); started != finished {
		t.Fatalf("%s: %d producers started but %d finished", context, started, finished)
	}
}

// Success path: every index is produced exactly once and no producer is running
// when the walk returns.
func TestForEachOrderedSuccessProducesEveryIndexOnceAndLeavesNoProducer(t *testing.T) {
	const n = canonicalDecodeChunk * 30
	for _, workers := range []int{2, 4, 8, 16} {
		t.Run(fmt.Sprintf("workers-%d", workers), func(t *testing.T) {
			var ledger producerLedger
			produced := make([]atomic.Int32, n)
			err := forEachOrdered(n, workers, func(i int) int {
				defer ledger.enter()()
				produced[i].Add(1)
				return i
			}, func(int, int) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			ledger.requireQuiescent(t, "success")
			for i := range produced {
				if got := produced[i].Load(); got != 1 {
					t.Fatalf("index %d produced %d times; want exactly once", i, got)
				}
			}
		})
	}
}

// Early stop: a worker is held inside produce on a later chunk. forEachOrdered
// must not return while that producer runs; it returns only after the producer
// is released. Dropping the join makes it return at once and fails this test.
func TestForEachOrderedEarlyStopWaitsForInFlightProducer(t *testing.T) {
	const workers = 4
	const n = canonicalDecodeChunk * 16
	const blockedIdx = canonicalDecodeChunk * 2
	stop := errors.New("stop")
	var ledger producerLedger
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseProducer := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseProducer)
	returned := make(chan error, 1)
	go func() {
		returned <- forEachOrdered(n, workers, func(i int) int {
			defer ledger.enter()()
			if i == blockedIdx {
				enteredOnce.Do(func() { close(entered) })
				<-release
			}
			return i
		}, func(i, _ int) error {
			if i == 0 {
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					return errors.New("no producer reached the blocked index")
				}
				return stop
			}
			return nil
		})
	}()
	select {
	case err := <-returned:
		t.Fatalf("forEachOrdered returned (%v) while a producer was still running; the worker join is missing", err)
	case <-time.After(200 * time.Millisecond):
	}
	if active := ledger.active.Load(); active < 1 {
		t.Fatalf("active producers = %d while one is held; the held producer is not being tracked", active)
	}
	releaseProducer()
	select {
	case err := <-returned:
		if !errors.Is(err, stop) {
			t.Fatalf("error = %v; want stop", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("forEachOrdered did not return after the producer was released")
	}
	ledger.requireQuiescent(t, "early stop")
}

// Early stop with idle workers: workers parked waiting for a window slot must
// also be released, not only the ones inside produce.
func TestForEachOrderedEarlyStopReleasesWorkersParkedOnWindow(t *testing.T) {
	const workers = 4
	const n = canonicalDecodeChunk * 64
	stop := errors.New("stop")
	var ledger producerLedger
	err := forEachOrdered(n, workers, func(i int) int {
		defer ledger.enter()()
		return i
	}, func(i, _ int) error {
		if i == 0 {
			// Let the producers fill the window and park on it before stopping.
			deadline := time.Now().Add(10 * time.Second)
			for ledger.finished.Load() < int64(workers*2*canonicalDecodeChunk) {
				if time.Now().After(deadline) {
					t.Fatal("producers never filled the window")
				}
				time.Sleep(time.Millisecond)
			}
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("error = %v; want stop", err)
	}
	ledger.requireQuiescent(t, "early stop, parked workers")
	if got := ledger.finished.Load(); got >= n {
		t.Fatalf("produced %d of %d items after an early stop; want the walk cancelled", got, n)
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

// TestParallelCanonicalValidationSaturatesWindowOnLargeStore walks a store with
// more chunks than the window holds at 2, 3, and 8 workers (18 chunks against
// windows of 4, 6, and 16), first clean and then with an early digest fault and
// a late decode fault, and requires every worker count to match the sequential
// walk.
func TestParallelCanonicalValidationSaturatesWindowOnLargeStore(t *testing.T) {
	const operations = canonicalDecodeChunk*18 - 40
	db, anchors := canonicalDecodeStore(t, operations)
	if chunks := (len(anchors) + 1 + canonicalDecodeChunk - 1) / canonicalDecodeChunk; chunks <= 8*canonicalDecodeWindowPerWorker {
		t.Fatalf("store spans %d chunks; want more than the 8-worker window of %d", chunks, 8*canonicalDecodeWindowPerWorker)
	}
	workerCounts := []int{2, 3, 8}
	requireMatchesSequential := func(wantOpContains string) {
		t.Helper()
		wantOp, wantFact := canonicalDecodeValidationErrors(t, db, 1)
		if wantOpContains == "" {
			if wantOp != nil || wantFact != nil {
				t.Fatalf("clean store sequential validation = (%v, %v); want nil", wantOp, wantFact)
			}
		} else if wantOp == nil || !strings.Contains(wantOp.Error(), wantOpContains) {
			t.Fatalf("sequential validateCanonicalOperations = %v; want it to contain %q", wantOp, wantOpContains)
		}
		for _, workers := range workerCounts {
			gotOp, gotFact := canonicalDecodeValidationErrors(t, db, workers)
			if fmt.Sprint(gotOp) != fmt.Sprint(wantOp) {
				t.Fatalf("workers=%d validateCanonicalOperations = %v; want sequential result %v", workers, gotOp, wantOp)
			}
			if fmt.Sprint(gotFact) != fmt.Sprint(wantFact) {
				t.Fatalf("workers=%d validateCanonicalFactContextSets = %v; want sequential result %v", workers, gotFact, wantFact)
			}
		}
	}
	requireMatchesSequential("")
	early, late := anchors[canonicalDecodeChunk*2+9], anchors[len(anchors)-canonicalDecodeChunk]
	execCanonicalDecodeDamage(t, db, "UPDATE journal_operations SET mutation_digest=x'00' WHERE journal_id=?1", early)
	execCanonicalDecodeDamage(t, db, "UPDATE journal_operations SET canonical_mutation=x'00' WHERE journal_id=?1", late)
	requireMatchesSequential(fmt.Sprintf("canonical operation %d field mutation digest diverged", early))
}
