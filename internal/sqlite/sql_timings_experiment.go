//go:build pasture_experiment

package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// Pre-lease artifact identities are not protected by connection ownership.
var sqlTimingCounter atomic.Uint64

// The mutex owns one exclusively created artifact and sticky failure state,
// including framing across concurrent daemon flushes after scope release.
// SQLite/pool ownership cannot protect this post-transaction file lifetime;
// an atomic cannot serialize its initialization and writes. No DB invariant
// depends on this process-local primitive. The handle lives until process exit.
type sqlTimingArtifact struct {
	mu   sync.Mutex
	file *os.File
	path string
	err  error
}

var sqlTimingOutput sqlTimingArtifact

func init() {
	sqlTimingNew = sqlTimingExperimentOperation
	sqlTimingWrite = sqlTimingExperimentFlush
	sqlTimingBorrowedPath = sqlTimingExperimentDatabase
}

func (a *sqlTimingArtifact) failLocked(err error) {
	if a.err == nil {
		a.err = err
		fmt.Fprintln(os.Stderr, "EXPERIMENT_SQL_TIMINGS_ERROR: private artifact refused or write failed; timings are invalid; ACK unchanged; fix: stop the lane and use a fresh private sandbox/path:", err)
	}
}

func sqlTimingExperimentDatabase(ctx context.Context, conn sqlQueryer) (string, bool) {
	if os.Getenv("PASTURE_EXPERIMENT_SQL_TIMINGS") == "" {
		return "", false
	}
	var path string
	err := conn.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path)
	if err != nil {
		sqlTimingOutput.mu.Lock()
		sqlTimingOutput.failLocked(fmt.Errorf("cannot identify borrowed main database: %w", err))
		sqlTimingOutput.mu.Unlock()
	}
	return path, err == nil
}

func sqlTimingExperimentOperation(ctx context.Context, method string, db *DB) (context.Context, *sqlTimingRecorder) {
	path := os.Getenv("PASTURE_EXPERIMENT_SQL_TIMINGS")
	if path == "" {
		return ctx, nil
	}
	if db == nil || !db.timingDatabaseKnown {
		sqlTimingOutput.mu.Lock()
		sqlTimingOutput.failLocked(errors.New("main database path unknown; configure timing before opening the borrowed store"))
		sqlTimingOutput.mu.Unlock()
		return ctx, nil
	}
	r := &sqlTimingRecorder{path: path, sandbox: os.Getenv("PASTURE_EXPERIMENT_SANDBOX"), databasePath: db.timingDatabasePath, artifact: &sqlTimingOutput, pid: os.Getpid(), op: sqlTimingCounter.Add(1), method: method, events: make([]sqlTimingEvent, 0, 64)}
	return context.WithValue(ctx, sqlTimingKey{}, r), r
}

// Canonicalize the existing parent, not the final component: the artifact may
// not exist, and a dangling final symlink must reach O_EXCL and be refused.
func sqlTimingCanonicalDestination(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("artifact/sandbox path must be absolute")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(path)))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(filepath.Clean(path))), nil
}

func sqlTimingSafeDestination(r *sqlTimingRecorder) (string, error) {
	if !filepath.IsAbs(r.sandbox) {
		return "", errors.New("PASTURE_EXPERIMENT_SANDBOX must name a private directory under /tmp/opencode")
	}
	sandbox, err := filepath.EvalSymlinks(filepath.Clean(r.sandbox))
	if err != nil {
		return "", err
	}
	approved, err := filepath.EvalSymlinks("/tmp/opencode")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(sandbox, approved+string(os.PathSeparator)) {
		return "", errors.New("sandbox must be a strict descendant of /tmp/opencode")
	}
	info, err := os.Stat(sandbox)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("sandbox must be a private directory (no group/other permissions)")
	}
	destination, err := sqlTimingCanonicalDestination(r.path)
	if err != nil {
		return "", err
	}
	if filepath.Dir(destination) != sandbox {
		return "", errors.New("artifact must be a direct child of the canonical sandbox")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if strings.HasSuffix(strings.ToLower(destination), suffix) {
			return "", errors.New("artifact basename is reserved for a SQLite sidecar")
		}
	}
	if r.databasePath != "" {
		absolute, err := filepath.Abs(r.databasePath)
		if err != nil {
			return "", err
		}
		main, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return "", fmt.Errorf("cannot resolve main database safely: %w", err)
		}
		alias, err := sqlTimingCanonicalDestination(absolute)
		if err != nil {
			return "", err
		}
		for _, base := range []string{main, alias} {
			for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
				if destination == base+suffix {
					return "", errors.New("artifact destination is the main database or a reserved SQLite sidecar")
				}
			}
		}
	}
	return destination, nil
}

func sqlTimingExperimentFlush(r *sqlTimingRecorder) {
	a, ok := r.artifact.(*sqlTimingArtifact)
	if !ok {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return
	}
	destination, err := sqlTimingSafeDestination(r)
	if err != nil {
		a.failLocked(err)
		return
	}
	if a.file == nil {
		root, err := os.OpenRoot(filepath.Dir(destination))
		if err != nil {
			a.failLocked(err)
			return
		}
		file, err := root.OpenFile(filepath.Base(destination), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		_ = root.Close()
		if err != nil {
			a.failLocked(err)
			return
		}
		a.file, a.path = file, destination
	} else if destination != a.path {
		a.failLocked(errors.New("artifact destination changed during the process; use a new process and fresh path"))
		return
	}
	// Retaining the FD prevents redirection, and this check also makes path
	// replacement, permission exposure, or newly added hardlinks invalidate the
	// artifact explicitly instead of letting the driver read a different inode.
	owned, err := a.file.Stat()
	if err != nil {
		a.failLocked(err)
		return
	}
	current, err := os.Lstat(destination)
	if err != nil {
		a.failLocked(err)
		return
	}
	stat, supported := owned.Sys().(*syscall.Stat_t)
	if !current.Mode().IsRegular() || current.Mode().Perm()&0077 != 0 || !os.SameFile(owned, current) || !supported || stat.Nlink != 1 {
		a.failLocked(errors.New("owned artifact was replaced, exposed, or hardlinked; stop this lane"))
		return
	}
	encoder := json.NewEncoder(a.file)
	for _, event := range r.events {
		if err := encoder.Encode(event); err != nil {
			a.failLocked(err)
			return
		}
	}
}
