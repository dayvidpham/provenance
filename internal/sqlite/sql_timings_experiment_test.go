//go:build pasture_experiment

package sqlite

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// Subprocesses avoid process-global environment mutation while exercising the
// exact opt-in entry point and public store methods used by the consumer lane.
func TestSQLTimingsOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "timings.jsonl")
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "-test.run=^TestSQLTimingsChild$", "-test.shuffle=on", "-test.fullpath=true", "-test.timeout=20m")
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "PASTURE_EXPERIMENT_SQL_TIMINGS=") {
					cmd.Env = append(cmd.Env, value)
				}
			}
			cmd.Env = append(cmd.Env, "PROVENANCE_TIMING_CHILD=1")
			if enabled {
				cmd.Env = append(cmd.Env, "PASTURE_EXPERIMENT_SQL_TIMINGS="+path)
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
	db := newJournalDB(t)
	actor := ptypes.ActorID{Namespace: "timing-test", UUID: uuid.New()}
	seedActor(t, db, actor)
	authority := genesisBoot(t, db, actor)
	in := batchDecisionInput("timing-one", actor, authority, `{}`)
	if _, err := db.Apply(in); err != nil {
		t.Fatal(err)
	}
	inputs := []journal.OperationInput{batchDecisionInput("timing-two", actor, authority, `{}`), batchDecisionInput("timing-three", actor, authority, `{}`)}
	if _, err := db.ApplyBatchContext(t.Context(), inputs); err != nil {
		t.Fatal(err)
	}
	in.CommandDigest = []byte("conflict")
	if _, err := db.Apply(in); err == nil {
		t.Fatal("conflict control unexpectedly committed")
	}
	if os.Getenv("PASTURE_EXPERIMENT_SQL_TIMINGS") == "" {
		ctx := context.Background()
		if allocations := testing.AllocsPerRun(10, func() {
			got, r := sqlTimingOperation(ctx, "Apply")
			if got != ctx || r != nil {
				t.Fatal("no-env enabled")
			}
			sqlTimingStart(ctx, "body").finish(nil)
			r.flush()
		}); allocations != 0 {
			t.Fatalf("no-env allocations: %v", allocations)
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
			path := filepath.Join(t.TempDir(), "owner.jsonl")
			r := &sqlTimingRecorder{path: path, pid: os.Getpid(), op: sqlTimingCounter.Add(1), method: "Apply"}
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
	path := filepath.Join(t.TempDir(), "concurrent.jsonl")
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			r := &sqlTimingRecorder{path: path, pid: os.Getpid(), op: sqlTimingCounter.Add(1), method: "Apply"}
			ctx := context.WithValue(context.Background(), sqlTimingKey{}, r)
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
	path := filepath.Join(t.TempDir(), "retry.jsonl")
	r := &sqlTimingRecorder{path: path, pid: os.Getpid(), op: sqlTimingCounter.Add(1), method: "Apply"}
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
	path := filepath.Join(t.TempDir(), "pool.jsonl")
	r := &sqlTimingRecorder{path: path, pid: os.Getpid(), op: sqlTimingCounter.Add(1), method: "Apply"}
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
