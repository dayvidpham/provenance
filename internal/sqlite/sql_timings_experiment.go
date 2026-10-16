//go:build pasture_experiment

package sqlite

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// The scalar counter gives concurrent callers distinct artifact invocation
// numbers before pool acquisition. Connection/transaction ownership cannot
// cover that pre-lease identity; no persistent invariant is synchronized here.
var sqlTimingCounter atomic.Uint64

// This mutex protects JSONL append framing across concurrent operations in one
// process, not database state. Pool ownership/SQLite transactions cannot protect
// a shared artifact after scope release; an atomic cannot serialize file writes.
// Separate processes must use separate paths (PID is also recorded).
var sqlTimingFileMu sync.Mutex

func init() {
	sqlTimingNew = sqlTimingExperimentOperation
	sqlTimingWrite = sqlTimingExperimentFlush
}

func sqlTimingExperimentOperation(ctx context.Context, method string) (context.Context, *sqlTimingRecorder) {
	path := os.Getenv("PASTURE_EXPERIMENT_SQL_TIMINGS")
	if path == "" || !filepath.IsAbs(path) {
		return ctx, nil
	}
	r := &sqlTimingRecorder{path: path, pid: os.Getpid(), op: sqlTimingCounter.Add(1), method: method, events: make([]sqlTimingEvent, 0, 64)}
	return context.WithValue(ctx, sqlTimingKey{}, r), r
}

func sqlTimingExperimentFlush(r *sqlTimingRecorder) {
	sqlTimingFileMu.Lock()
	defer sqlTimingFileMu.Unlock()
	// Artifact failures never change transaction semantics. The experiment
	// driver must reject absent/truncated records. Refuse non-private files.
	if info, err := os.Lstat(r.path); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	for _, event := range r.events {
		if encoder.Encode(event) != nil {
			return
		}
	}
}
