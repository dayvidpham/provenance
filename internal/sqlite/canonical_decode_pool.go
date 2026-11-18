package sqlite

import (
	"runtime"
	"sync"
	"sync/atomic"
)

const (
	// canonicalDecodeMaxWorkers caps the decode pool. Decode is CPU-bound and the
	// consumer is a single SQLite connection, so more workers than this only add
	// scheduling cost.
	canonicalDecodeMaxWorkers = 16
	// canonicalDecodeChunk is the number of operations one worker claims at a time.
	// Decode costs tens of microseconds per operation, so a chunk amortizes the
	// channel hand-off to well under one percent of the work it carries.
	canonicalDecodeChunk = 128
	// canonicalDecodeWindowPerWorker bounds how many chunks may be decoded but not
	// yet consumed, per worker. It keeps decoded effects bounded no matter how
	// far the consumer lags the decoders.
	canonicalDecodeWindowPerWorker = 2
)

// canonicalDecodeWorkersOverride is a test seam. Zero means "use the default".
var canonicalDecodeWorkersOverride atomic.Int32

// canonicalDecodeWorkers returns the decode pool size: GOMAXPROCS, capped at
// canonicalDecodeMaxWorkers, unless a test set an override.
func canonicalDecodeWorkers() int {
	if n := int(canonicalDecodeWorkersOverride.Load()); n > 0 {
		return n
	}
	n := runtime.GOMAXPROCS(0)
	if n > canonicalDecodeMaxWorkers {
		n = canonicalDecodeMaxWorkers
	}
	if n < 1 {
		n = 1
	}
	return n
}

// forEachOrdered runs produce(i) for every i in [0,n) on a bounded worker pool
// and calls consume(i, result) on the calling goroutine in strictly ascending i.
//
// Error ordering: consume sees results in index order and the first non-nil
// consume error ends the walk, so the reported error is exactly the one a
// sequential `for i { consume(i, produce(i)) }` would return — the first in
// anchor order. Producers must be pure: they must not touch the database or any
// shared mutable state, because later indexes may be produced (and discarded)
// after an earlier index fails.
//
// Memory: indexes are claimed in contiguous chunks and at most
// workers*canonicalDecodeWindowPerWorker chunks are in flight, so only that
// many results exist at once. The input slice is indexed, never copied.
//
// With workers <= 1, or an input smaller than one chunk, the walk is the plain
// sequential loop on the calling goroutine.
func forEachOrdered[R any](n, workers int, produce func(i int) R, consume func(i int, r R) error) error {
	if n <= 0 {
		return nil
	}
	if workers <= 1 || n <= canonicalDecodeChunk {
		for i := 0; i < n; i++ {
			if err := consume(i, produce(i)); err != nil {
				return err
			}
		}
		return nil
	}
	chunks := (n + canonicalDecodeChunk - 1) / canonicalDecodeChunk
	if workers > chunks {
		workers = chunks
	}
	window := workers * canonicalDecodeWindowPerWorker
	slots := make([]chan []R, chunks)
	for c := range slots {
		slots[c] = make(chan []R, 1)
	}
	tokens := make(chan struct{}, window)
	for i := 0; i < window; i++ {
		tokens <- struct{}{}
	}
	done := make(chan struct{})
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-tokens:
				case <-done:
					return
				}
				c := int(next.Add(1) - 1)
				if c >= chunks {
					return
				}
				lo := c * canonicalDecodeChunk
				hi := lo + canonicalDecodeChunk
				if hi > n {
					hi = n
				}
				out := make([]R, hi-lo)
				for i := lo; i < hi; i++ {
					out[i-lo] = produce(i)
				}
				slots[c] <- out // capacity 1 and one send per chunk: never blocks
			}
		}()
	}
	defer func() {
		close(done)
		wg.Wait()
	}()
	for c := 0; c < chunks; c++ {
		out := <-slots[c]
		lo := c * canonicalDecodeChunk
		for j := range out {
			if err := consume(lo+j, out[j]); err != nil {
				return err
			}
		}
		tokens <- struct{}{}
	}
	return nil
}
