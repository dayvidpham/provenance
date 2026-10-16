//go:build pasture_experiment

package sqlite

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

func timingEvents(t *testing.T, path string) []sqlTimingEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []sqlTimingEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event sqlTimingEvent
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.PID <= 0 || event.Operation == 0 || event.StartNS <= 0 || event.EndNS < event.StartNS || event.Outcome == "" {
			t.Fatalf("invalid event: %+v", event)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func newSQLTimingTestRecorder(t *testing.T, path string) *sqlTimingRecorder {
	t.Helper()
	a := &sqlTimingArtifact{}
	t.Cleanup(func() {
		if a.file != nil {
			_ = a.file.Close()
		}
	})
	return &sqlTimingRecorder{path: path, sandbox: filepath.Dir(path), artifact: a, pid: os.Getpid(), op: sqlTimingCounter.Add(1), method: "Apply"}
}

func sqlTimingTempDir(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("/tmp/opencode", "provenance-sql-timing-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})
	return path
}

// Subprocesses avoid process-global environment mutation while exercising the
// exact opt-in entry point and public store methods used by the consumer lane.
func TestSQLTimingsOptIn(t *testing.T) {
	for _, kind := range []string{"disabled", "memory", "borrowed", "standalone"} {
		t.Run(kind, func(t *testing.T) {
			enabled := kind != "disabled"
			path := filepath.Join(sqlTimingTempDir(t), "timings.jsonl")
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "-test.run=^TestSQLTimingsChild$", "-test.shuffle=on", "-test.fullpath=true", "-test.timeout=20m")
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "PASTURE_EXPERIMENT_") && !strings.HasPrefix(value, "PROVENANCE_TIMING_") {
					cmd.Env = append(cmd.Env, value)
				}
			}
			cmd.Env = append(cmd.Env, "PROVENANCE_TIMING_CHILD=1")
			if enabled {
				cmd.Env = append(cmd.Env, "PASTURE_EXPERIMENT_SQL_TIMINGS="+path, "PASTURE_EXPERIMENT_SANDBOX="+filepath.Dir(path))
			}
			if kind == "borrowed" {
				cmd.Env = append(cmd.Env, "PROVENANCE_TIMING_BORROWED="+filepath.Join(filepath.Dir(path), "borrowed.sqlite"))
			}
			if kind == "standalone" {
				cmd.Env = append(cmd.Env, "PROVENANCE_TIMING_STANDALONE="+filepath.Join(filepath.Dir(path), "standalone.sqlite"))
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("child: %v\n%s", err, output)
			}
			if !enabled {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("disabled artifact: %v", err)
				}
				return
			}
			events := timingEvents(t, path)
			groups := map[uint64][]sqlTimingEvent{}
			for _, e := range events {
				groups[e.Operation] = append(groups[e.Operation], e)
			}
			var batch, rolledBack bool
			for _, group := range groups {
				phases := map[string][]sqlTimingEvent{}
				for _, e := range group {
					phases[e.Phase] = append(phases[e.Phase], e)
				}
				for _, phase := range []string{"sql_conn", "begin_wait", "begin_attempt", "transaction_scope", "body", "fold"} {
					if len(phases[phase]) == 0 {
						t.Fatalf("missing %s in %+v", phase, group)
					}
				}
				txn := phases["transaction_scope"][0]
				body := phases["body"][0]
				attempts := phases["begin_attempt"]
				if txn.StartNS != attempts[len(attempts)-1].EndNS || txn.StartNS != phases["begin_wait"][0].EndNS {
					t.Fatal("transaction scope did not start at successful BEGIN return")
				}
				if phases["sql_conn"][0].EndNS > phases["begin_attempt"][0].StartNS || txn.StartNS > body.StartNS || txn.EndNS < body.EndNS {
					t.Fatalf("owner ordering: %+v", group)
				}
				for _, fold := range phases["fold"] {
					if fold.StartNS < body.StartNS || fold.EndNS > body.EndNS {
						t.Fatalf("fold outside body: %+v", fold)
					}
				}
				if txn.Outcome == "committed" {
					if len(phases["commit"]) != 1 || len(phases["rollback"]) != 0 {
						t.Fatal("incorrect commit accounting")
					}
					if body.EndNS > phases["commit"][0].StartNS {
						t.Fatal("body includes commit")
					}
					if txn.EndNS != phases["commit"][0].EndNS {
						t.Fatal("transaction scope did not stop at COMMIT return")
					}
				} else {
					rolledBack = true
					if txn.Outcome != "rolled_back" || len(phases["rollback"]) != 1 || len(phases["commit"]) != 0 {
						t.Fatal("incorrect rollback accounting")
					}
					if txn.EndNS != phases["rollback"][0].EndNS {
						t.Fatal("transaction scope did not stop at rollback return")
					}
				}
				if group[0].Method == "ApplyBatchContext" && txn.Outcome == "committed" {
					batch = true
					if len(phases["fold"]) != 2 || len(phases["gate_actor_placement"]) != 1 || len(phases["gate_subtype_integrity"]) != 1 {
						t.Fatalf("batch accounting: %+v", phases)
					}
				}
			}
			if !batch || !rolledBack {
				t.Fatalf("missing positive/negative controls: batch=%v rollback=%v", batch, rolledBack)
			}
		})
	}
}

func TestSQLTimingsChild(t *testing.T) {
	if os.Getenv("PROVENANCE_TIMING_CHILD") != "1" {
		t.Skip("subprocess only")
	}
	var db *DB
	if path := os.Getenv("PROVENANCE_TIMING_STANDALONE"); path != "" {
		var err error
		db, err = Open(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if !db.timingDatabaseKnown || db.timingDatabasePath == "" {
			t.Fatal("standalone database safety identity missing")
		}
	} else if path := os.Getenv("PROVENANCE_TIMING_BORROWED"); path != "" {
		pool, err := sql.Open(sqliteDriverName, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pool.Close() })
		db, err = OpenBorrowed(pool, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if !db.timingDatabaseKnown || db.timingDatabasePath == "" {
			t.Fatal("borrowed database safety identity missing")
		}
	} else {
		db = newJournalDB(t)
	}
	actor := ptypes.ActorID{Namespace: "timing-test", UUID: uuid.New()}
	seedActor(t, db, actor)
	authority := genesisBoot(t, db, actor)
	// This subprocess runs only this serial test, so observing the existing
	// writer hook cannot race another test or mutate the parent's globals.
	// Keep the actual public entrypoints, recorder factory, and artifact writer:
	// manually releasing a scope before r.flush would not pin their defer order.
	write := sqlTimingWrite
	defer func() { sqlTimingWrite = write }()
	boundary, method, outcome := "Apply/success", "Apply", "committed"
	flushes := 0
	sqlTimingWrite = func(r *sqlTimingRecorder) {
		db.lifecycleMu.Lock()
		registered := len(db.scopeCancels)
		db.lifecycleMu.Unlock()
		if inUse := db.db.Stats().InUse; registered != 0 || inUse != 0 {
			t.Fatalf("%s: artifact flush preceded scope release: registered=%d in_use=%d", boundary, registered, inUse)
		}
		if r.method != method {
			t.Fatalf("%s: flushed method=%s, want %s", boundary, r.method, method)
		}
		transactions := 0
		for _, event := range r.events {
			if event.Phase == "transaction_scope" {
				transactions++
				if event.Outcome != outcome {
					t.Fatalf("%s: transaction outcome=%s, want %s", boundary, event.Outcome, outcome)
				}
			}
		}
		if transactions != 1 {
			t.Fatalf("%s: transaction events=%d, want 1", boundary, transactions)
		}
		flushes++
		write(r)
	}
	assertFlush := func(want int) {
		t.Helper()
		if os.Getenv("PASTURE_EXPERIMENT_SQL_TIMINGS") == "" {
			want = 0
		}
		if flushes != want {
			t.Fatalf("%s: public entrypoint flushes=%d, want %d", boundary, flushes, want)
		}
	}
	in := batchDecisionInput("timing-one", actor, authority, `{}`)
	if _, err := db.Apply(in); err != nil {
		t.Fatal(err)
	}
	assertFlush(1)
	boundary, method = "ApplyBatchContext/success", "ApplyBatchContext"
	inputs := []journal.OperationInput{batchDecisionInput("timing-two", actor, authority, `{}`), batchDecisionInput("timing-three", actor, authority, `{}`)}
	if _, err := db.ApplyBatchContext(t.Context(), inputs); err != nil {
		t.Fatal(err)
	}
	assertFlush(2)
	boundary, method, outcome = "Apply/rollback", "Apply", "rolled_back"
	in.CommandDigest = []byte("conflict")
	if _, err := db.Apply(in); err == nil {
		t.Fatal("conflict control unexpectedly committed")
	}
	assertFlush(3)
	boundary, method = "ApplyBatchContext/rollback", "ApplyBatchContext"
	inputs[0].CommandDigest = []byte("batch-conflict")
	if result, err := db.ApplyBatchContext(t.Context(), inputs); err == nil || result.Committed {
		t.Fatalf("batch conflict control unexpectedly committed: %+v, %v", result, err)
	}
	assertFlush(4)
	sqlTimingWrite = write
	if os.Getenv("PASTURE_EXPERIMENT_SQL_TIMINGS") == "" {
		ctx := context.Background()
		if allocations := testing.AllocsPerRun(10, func() {
			got, r := sqlTimingOperation(ctx, "Apply", nil)
			if got != ctx || r != nil {
				t.Fatal("no-env enabled")
			}
			sqlTimingStart(ctx, "body").finish(nil)
			r.flush()
		}); allocations != 0 {
			t.Fatalf("no-env allocations: %v", allocations)
		}
	}
	if os.Getenv("PROVENANCE_TIMING_EXPECT_ERROR") == "1" {
		if sqlTimingOutput.err == nil {
			t.Fatal("artifact rejection was not sticky")
		}
		first := sqlTimingOutput.err
		if _, err := db.Apply(batchDecisionInput("after-output-error", actor, authority, `{}`)); err != nil {
			t.Fatalf("artifact failure changed ACK: %v", err)
		}
		if sqlTimingOutput.err != first {
			t.Fatal("sticky failure changed")
		}
	}
}

type timingBoundaryQueryer struct {
	sqlQueryer
	t    *testing.T
	path string
	fail string
}

func (q timingBoundaryQueryer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if _, err := os.Stat(q.path); !os.IsNotExist(err) {
		q.t.Fatalf("I/O before scope completion at %s: %v", query, err)
	}
	if query == q.fail {
		return nil, errors.New("injected boundary fault")
	}
	return q.sqlQueryer.ExecContext(ctx, query, args...)
}

func TestSQLTimingsActualOwnerErrors(t *testing.T) {
	for _, fail := range []string{"BEGIN IMMEDIATE", "body", "COMMIT", "ROLLBACK"} {
		t.Run(fail, func(t *testing.T) {
			db := newJournalDB(t)
			path := filepath.Join(sqlTimingTempDir(t), "owner.jsonl")
			r := newSQLTimingTestRecorder(t, path)
			ctx := context.WithValue(t.Context(), sqlTimingKey{}, r)
			scope, err := db.bindScope(ctx, projectionTargetLive)
			if err != nil {
				t.Fatal(err)
			}
			q := timingBoundaryQueryer{sqlQueryer: scope.conn, t: t, path: path, fail: fail}
			err = runImmediateTransaction(scope.ctx, q, func() error {
				if len(r.events) < 3 {
					t.Fatal("BEGIN/pool boundaries not recorded before body")
				}
				if fail == "body" || fail == "ROLLBACK" {
					return errors.New("body control")
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault did not propagate")
			}
			// Failed rollback deliberately leaves the real transaction active;
			// clean it through the actual connection before returning the lease.
			if fail == "ROLLBACK" {
				_, _ = scope.conn.ExecContext(context.Background(), "ROLLBACK")
			}
			scope.release()
			r.flush()
			phases := map[string]sqlTimingEvent{}
			for _, e := range timingEvents(t, path) {
				phases[e.Phase] = e
			}
			if fail == "BEGIN IMMEDIATE" {
				if _, ok := phases["transaction_scope"]; ok {
					t.Fatal("failed BEGIN acquired a transaction")
				}
				if phases["begin_attempt"].Outcome != "error" || phases["begin_wait"].Outcome != "error" {
					t.Fatal("failed attempt lost")
				}
			} else {
				want := "rolled_back"
				if fail == "COMMIT" {
					want = "commit_error_rolled_back"
				}
				if fail == "ROLLBACK" {
					want = "rollback_error"
				}
				if phases["transaction_scope"].Outcome != want {
					t.Fatalf("outcome=%s want %s", phases["transaction_scope"].Outcome, want)
				}
			}
		})
	}
}

func TestSQLTimingsConcurrentPrivateArtifact(t *testing.T) {
	path := filepath.Join(sqlTimingTempDir(t), "concurrent.jsonl")
	template := newSQLTimingTestRecorder(t, path)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			r := *template
			r.op = sqlTimingCounter.Add(1)
			ctx := context.WithValue(context.Background(), sqlTimingKey{}, &r)
			for range 10 {
				sqlTimingStart(ctx, "body").finish(nil)
			}
			r.flush()
		})
	}
	wg.Wait()
	if got := len(timingEvents(t, path)); got != 320 {
		t.Fatalf("framing/count: %d", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("artifact mode %v", info.Mode())
	}
}

type timingRetryQueryer struct {
	sqlQueryer
	busy     error
	attempts int
}

func (q *timingRetryQueryer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if query == "BEGIN IMMEDIATE" {
		q.attempts++
		if q.attempts == 1 {
			return nil, q.busy
		}
	}
	return q.sqlQueryer.ExecContext(ctx, query, args...)
}

func TestSQLTimingsBusyRetryAccounting(t *testing.T) {
	// Harvest the real driver's BUSY, then release the holder. A scripted first
	// attempt reuses that error; the next attempt executes the actual BEGIN.
	// This proves retry accounting without depending on holder release timing.
	db, err := Open(filepath.Join(t.TempDir(), "retry.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	holder := takePoolScope(t, db)
	defer holder.release()
	contender := takePoolScope(t, db)
	defer contender.release()
	if _, err := holder.conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := contender.conn.ExecContext(t.Context(), "PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	_, busy := contender.conn.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	if !isBusyError(busy) {
		t.Fatalf("harvest BUSY: %v", busy)
	}
	if _, err := holder.conn.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sqlTimingTempDir(t), "retry.jsonl")
	r := newSQLTimingTestRecorder(t, path)
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), sqlTimingKey{}, r), 5*time.Second)
	defer cancel()
	q := &timingRetryQueryer{sqlQueryer: contender.conn, busy: busy}
	if err := runImmediateTransaction(ctx, q, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	contender.release()
	r.flush()
	var attempts, cleanups, scopes int
	var wait sqlTimingEvent
	for _, e := range timingEvents(t, path) {
		if e.Phase == "begin_wait" {
			wait = e
		}
	}
	for _, e := range timingEvents(t, path) {
		switch e.Phase {
		case "begin_attempt":
			attempts++
			want := "busy"
			if attempts == 2 {
				want = "ok"
			}
			if e.Outcome != want || e.StartNS < wait.StartNS || e.EndNS > wait.EndNS {
				t.Fatalf("retry interval/outcome: %+v wait=%+v", e, wait)
			}
		case "rollback_begin_cleanup":
			cleanups++
		case "transaction_scope":
			scopes++
			if e.Outcome != "committed" || e.StartNS != wait.EndNS {
				t.Fatalf("acquired scope: %+v wait=%+v", e, wait)
			}
		}
	}
	if q.attempts != 2 || attempts != 2 || cleanups != 1 || scopes != 1 {
		t.Fatalf("accounting: driver=%d attempts=%d cleanup=%d scopes=%d", q.attempts, attempts, cleanups, scopes)
	}
}

func TestSQLTimingsPoolAcquisitionCanceled(t *testing.T) {
	db := newJournalDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	path := filepath.Join(sqlTimingTempDir(t), "pool.jsonl")
	r := newSQLTimingTestRecorder(t, path)
	ctx = context.WithValue(ctx, sqlTimingKey{}, r)
	if scope, err := db.bindScope(ctx, projectionTargetLive); scope != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquisition: scope=%v err=%v", scope, err)
	}
	r.flush()
	events := timingEvents(t, path)
	if len(events) != 1 || events[0].Phase != "sql_conn" || events[0].Outcome != "canceled" {
		t.Fatalf("canceled acquisition accounting: %+v", events)
	}
}

func TestSQLTimingsRefusesExistingAndReservedArtifacts(t *testing.T) {
	for _, kind := range []string{"database", "hardlink", "symlink", "dangling", "wal", "shm", "journal", "directory-alias", "outside"} {
		t.Run(kind, func(t *testing.T) {
			sandbox := sqlTimingTempDir(t)
			main := filepath.Join(sandbox, "main.sqlite")
			db, err := Open(main, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(main, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(main)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(sandbox, "timings.jsonl")
			switch kind {
			case "database":
				path = main
			case "hardlink":
				if err := os.Link(main, path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(main, path); err != nil {
					t.Fatal(err)
				}
			case "dangling":
				if err := os.Symlink(filepath.Join(sandbox, "absent.sqlite"), path); err != nil {
					t.Fatal(err)
				}
			case "wal":
				path = main + "-wal"
			case "shm":
				path = main + "-shm"
			case "journal":
				path = main + "-journal"
			case "directory-alias":
				alias := filepath.Join(sandbox, "alias")
				if err := os.Symlink(sandbox, alias); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(alias, "main.sqlite-journal")
			case "outside":
				path = filepath.Join(t.TempDir(), "outside.jsonl")
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "-test.run=^TestSQLTimingsChild$", "-test.shuffle=on", "-test.fullpath=true", "-test.timeout=20m")
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "PASTURE_EXPERIMENT_") && !strings.HasPrefix(value, "PROVENANCE_TIMING_") {
					cmd.Env = append(cmd.Env, value)
				}
			}
			cmd.Env = append(cmd.Env, "PROVENANCE_TIMING_CHILD=1", "PROVENANCE_TIMING_EXPECT_ERROR=1", "PASTURE_EXPERIMENT_SANDBOX="+sandbox, "PASTURE_EXPERIMENT_SQL_TIMINGS="+path)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("ACK/error child: %v\n%s", err, output)
			}
			if strings.Count(string(output), "EXPERIMENT_SQL_TIMINGS_ERROR:") != 1 {
				t.Fatalf("missing/duplicated sticky failure signal: %s", output)
			}
			after, err := os.ReadFile(main)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("database bytes changed")
			}
			if kind != "database" && kind != "hardlink" && kind != "symlink" && kind != "dangling" {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("refused absent destination was created: %v", err)
				}
			}
			if _, err := os.Stat(filepath.Join(sandbox, "absent.sqlite")); !os.IsNotExist(err) {
				t.Fatalf("dangling target created: %v", err)
			}
		})
	}
}

func TestSQLTimingsCanonicalDatabaseProtection(t *testing.T) {
	sandbox := sqlTimingTempDir(t)
	main := filepath.Join(sandbox, "unusual-name.jsonl")
	if err := os.WriteFile(main, []byte("database control"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(sandbox, "alias")
	if err := os.Symlink(sandbox, alias); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		r := newSQLTimingTestRecorder(t, filepath.Join(alias, "unusual-name.jsonl"+suffix))
		r.databasePath = main
		if _, err := sqlTimingSafeDestination(r); err == nil {
			t.Fatalf("canonical database/sidecar accepted: %s", suffix)
		}
	}
}

func TestSQLTimingsRetainedHandleAndStickyWriteFailure(t *testing.T) {
	path := filepath.Join(sqlTimingTempDir(t), "retained.jsonl")
	r := newSQLTimingTestRecorder(t, path)
	ctx := context.WithValue(t.Context(), sqlTimingKey{}, r)
	sqlTimingStart(ctx, "body").finish(nil)
	r.flush()
	a := r.artifact.(*sqlTimingArtifact)
	first := a.file
	if first == nil || a.err != nil {
		t.Fatalf("exclusive creation: %v", a.err)
	}
	r.flush()
	if a.file != first || len(timingEvents(t, path)) != 2 {
		t.Fatal("artifact not reused through retained handle")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a.file = readOnly // Same owned inode, but Encode must now fail to write.
	r.flush()
	if a.err == nil {
		t.Fatal("write failure not sticky")
	}
	firstError := a.err
	r.path = filepath.Join(filepath.Dir(path), "must-not-recover.jsonl")
	r.flush()
	if a.err != firstError {
		t.Fatal("error changed after failure")
	}
	if _, err := os.Stat(r.path); !os.IsNotExist(err) {
		t.Fatalf("sticky failure created another artifact: %v", err)
	}
}

func TestSQLTimingsOwnedArtifactCannotBeReboundOrHardlinked(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "hardlink", true: "replaced"}[replace], func(t *testing.T) {
			sandbox := sqlTimingTempDir(t)
			path := filepath.Join(sandbox, "owned.jsonl")
			r := newSQLTimingTestRecorder(t, path)
			ctx := context.WithValue(t.Context(), sqlTimingKey{}, r)
			sqlTimingStart(ctx, "body").finish(nil)
			r.flush()
			if r.artifact.(*sqlTimingArtifact).err != nil {
				t.Fatal("positive control failed")
			}
			victim := filepath.Join(sandbox, "victim.sqlite")
			if replace {
				if err := os.WriteFile(victim, []byte("database control"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, filepath.Join(sandbox, "original.jsonl")); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(victim, path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Link(path, victim); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(victim)
			if err != nil {
				t.Fatal(err)
			}
			r.flush()
			if r.artifact.(*sqlTimingArtifact).err == nil {
				t.Fatal("replacement/hardlink was not rejected")
			}
			after, err := os.ReadFile(victim)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("rebound/hardlinked victim changed")
			}
		})
	}
}
