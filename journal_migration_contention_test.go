package provenance

// journal_migration_contention_test.go covers legacy-baseline migration against a
// FILE-BACKED pool. The memory families in journal_concurrent_writers_test.go run on
// OpenMemory, whose pool is pinned to one connection, so no two scopes can ever hold
// SQLite file locks at the same time: a transaction that takes the read lock first and
// then promotes to a write is indistinguishable there from one that takes the write
// lock at BEGIN. Only a file-backed pool (four connections) can observe the difference,
// and the difference matters: SQLite never invokes the busy handler for a read-to-write
// promotion, so a promoting migration fails instantly with SQLITE_BUSY and busy_timeout
// is never honoured — the whole all-or-nothing baseline batch aborts.

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// migrationHoldWindow is how long the deterministic lock holder keeps the file's
// write lock while the contended migration is expected to wait. It is a hold
// duration, never a synchronization device: every wait below is a bounded wait on
// a channel condition, and the window is far shorter than SQLite's 5s
// busy_timeout, so a correctly waiting migration still commits inside its first
// busy window.
const migrationHoldWindow = 250 * time.Millisecond

// newFileRaceTracker opens the same race fixture as newRaceTracker on a real file,
// which is the only way to get the bounded multi-connection runtime pool. It
// returns the database path so a test can contend on that exact file.
func newFileRaceTracker(t *testing.T, name string) (*raceTracker, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".db")
	tr, err := OpenSQLite(path, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatalf("OpenSQLite(%q): %v", path, err)
	}
	return newRaceTrackerOn(t, tr), path
}

// holdFileWriteLock opens an independent connection to path and takes SQLite's
// write lock with an explicit BEGIN IMMEDIATE, producing exactly the file-level
// contention a second writer process produces. The returned release rolls the
// holder back; it is idempotent and also runs on cleanup.
func holdFileWriteLock(t *testing.T, path string) (release func()) {
	t.Helper()
	holder := openRawSQLiteTestConn(t, path, "rw")
	if err := rawExecute(holder, "BEGIN IMMEDIATE", nil); err != nil {
		if closeErr := holder.Close(); closeErr != nil {
			t.Errorf("close write-lock holder for %q: %v", path, closeErr)
		}
		t.Fatalf("acquire deterministic write lock on %q: %v", path, err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		if err := rawExecute(holder, "ROLLBACK", nil); err != nil {
			t.Errorf("release deterministic write lock on %q: %v", path, err)
		}
		if err := holder.Close(); err != nil {
			t.Errorf("close write-lock holder for %q: %v", path, err)
		}
	}
	t.Cleanup(release)
	return release
}

// TestMigrationBaselineWaitsForConcurrentFileWriter is the contention regression for
// anchorLegacyBaselines. With a deferred BEGIN the migration transaction established a
// read snapshot (the fold path reads before it writes) and the first baseline INSERT
// needed a read-to-write promotion, on which SQLite never calls the busy handler: a
// migration contending with any other writer failed instantly with SQLITE_BUSY and
// rolled back every baseline in the batch. The migration must instead take the write
// lock at BEGIN, wait out the holder inside busy_timeout, and then commit.
func TestMigrationBaselineWaitsForConcurrentFileWriter(t *testing.T) {
	t.Parallel()
	r, path := newFileRaceTracker(t, "migration-contention")

	legacy := LegacyTaskRow{ID: newCorpusTaskID(), Status: TaskStatusOpen}
	st := r.tr.(*sqliteTracker)
	if err := st.db.SeedLegacyTask(legacy); err != nil {
		t.Fatalf("seed legacy task: %v", err)
	}

	release := holdFileWriteLock(t, path)

	type migration struct {
		result  MigrationResult
		err     error
		elapsed time.Duration
	}
	done := make(chan migration, 1)
	start := time.Now()
	go func() {
		result, err := r.tr.Journal().MigrateLegacyBaseline(MigrationInput{
			System: r.actorA, BootstrapAuthority: r.boot, Legacy: []LegacyTaskRow{legacy},
		})
		done <- migration{result: result, err: err, elapsed: time.Since(start)}
	}()

	// The defect signature is an immediate return, so the first assertion is that the
	// migration is still waiting for the lock while the holder owns it.
	select {
	case got := <-done:
		t.Fatalf("contended MigrateLegacyBaseline returned after %s while another writer held the file write lock (err=%v); "+
			"want it to wait for the lock inside busy_timeout instead of failing instantly on a read-to-write promotion", got.elapsed, got.err)
	case <-time.After(migrationHoldWindow):
	}

	release()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("migration after write-lock release: %v", got.err)
		}
		if got.result.TasksMigrated != 1 {
			t.Fatalf("migration processed %d tasks, want 1", got.result.TasksMigrated)
		}
		if got.elapsed < migrationHoldWindow {
			t.Fatalf("migration elapsed %s, want at least the %s lock hold", got.elapsed, migrationHoldWindow)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("migration did not finish within 30s of the write lock being released")
	}

	if _, err := r.tr.Show(legacy.ID); err != nil {
		t.Fatalf("migrated task not readable after contention: %v", err)
	}
	r.assertConverged(t)
}

// TestConcurrentSessionVsMigrationRaceFileBacked is the file-backed twin of
// TestConcurrentSessionVsMigrationRace: on a four-connection pool the live Session and
// the migration hold real, separate SQLite connections, so the interleaving is a true
// two-writer file race rather than two scopes multiplexed onto one connection. Both
// arms must succeed and the database must converge.
func TestConcurrentSessionVsMigrationRaceFileBacked(t *testing.T) {
	t.Parallel()
	r, _ := newFileRaceTracker(t, "session-vs-migration")

	legacy := LegacyTaskRow{ID: newCorpusTaskID(), Status: TaskStatusOpen}
	st := r.tr.(*sqliteTracker)
	if err := st.db.SeedLegacyTask(legacy); err != nil {
		t.Fatalf("seed legacy task: %v", err)
	}

	session := r.tr.As(r.actorA, r.boot)
	var (
		wg         sync.WaitGroup
		nativeTask Task
		nativeErr  error
		migrateErr error
		migrateRes MigrationResult
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		nativeTask, nativeErr = session.Create("provenance-test", "native-during-file-migration", "", TaskTypeTask, PriorityMedium, PhaseUnscoped)
	}()
	go func() {
		defer wg.Done()
		migrateRes, migrateErr = r.tr.Journal().MigrateLegacyBaseline(MigrationInput{
			System: r.actorA, BootstrapAuthority: r.boot, Legacy: []LegacyTaskRow{legacy},
		})
	}()
	wg.Wait()

	if nativeErr != nil {
		t.Fatalf("native Session.Create during file-backed migration failed: %v", nativeErr)
	}
	if migrateErr != nil {
		t.Fatalf("file-backed migration during a live Session.Create failed: %v", migrateErr)
	}
	if migrateRes.TasksMigrated != 1 {
		t.Fatalf("migration processed %d tasks, want 1", migrateRes.TasksMigrated)
	}
	if _, err := r.tr.Show(nativeTask.ID); err != nil {
		t.Fatalf("native task not readable after race: %v", err)
	}
	if _, err := r.tr.Show(legacy.ID); err != nil {
		t.Fatalf("migrated task not readable after race: %v", err)
	}
	r.assertConverged(t)
}
