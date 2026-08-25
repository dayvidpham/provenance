package provenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
)

var errScriptedJournalFault = errors.New("scripted borrowed-journal dependency fault")

type scriptedRetryJournal struct {
	Journal
	mu          sync.Mutex
	failures    int64
	fault       error
	lookupFault error
	attempts    atomic.Int64
	writes      atomic.Int64
	lookups     atomic.Int64
}

func (j *scriptedRetryJournal) reset(failures int, fault error) {
	j.mu.Lock()
	j.failures = int64(failures)
	j.fault = fault
	j.lookupFault = nil
	j.mu.Unlock()
	j.attempts.Store(0)
	j.writes.Store(0)
	j.lookups.Store(0)
}

func (j *scriptedRetryJournal) setLookupFault(err error) {
	j.mu.Lock()
	j.lookupFault = err
	j.mu.Unlock()
}

func (j *scriptedRetryJournal) Apply(input OperationInput) (CommittedResult, error) {
	attempt := j.attempts.Add(1)
	j.mu.Lock()
	failures, fault := j.failures, j.fault
	j.mu.Unlock()
	if attempt <= failures {
		if fault == nil {
			fault = errScriptedJournalFault
		}
		return CommittedResult{}, fmt.Errorf("%w on attempt %d", fault, attempt)
	}
	result, err := j.Journal.Apply(input)
	if err == nil {
		j.writes.Add(1)
	}
	return result, err
}

func (j *scriptedRetryJournal) LookupCommitted(operation OperationID) (CommittedResult, error) {
	j.lookups.Add(1)
	j.mu.Lock()
	fault := j.lookupFault
	j.mu.Unlock()
	if fault != nil {
		return CommittedResult{}, fault
	}
	return j.Journal.LookupCommitted(operation)
}

type scriptedRetryTracker struct {
	Tracker
	journal *scriptedRetryJournal
}

func (t *scriptedRetryTracker) Journal() Journal { return t.journal }

type retryTerminalStack struct {
	root      dbos.DBOSContext
	db        *sql.DB
	borrowed  Tracker
	adapter   *DBOSAdapter
	journal   *scriptedRetryJournal
	actor     ActorID
	authority JournalID
	callbacks atomic.Int64
}

func newRetryTerminalStack(t *testing.T, name string, options DBOSStepOptions) *retryTerminalStack {
	t.Helper()
	path := filepath.Join(t.TempDir(), "retry-terminal.db")
	db, err := openSharedSQL(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := dbos.NewDBOSContext(context.Background(), dbos.Config{AppName: name, SqliteSystemDB: db, ApplicationVersion: "retry-terminal"})
	if err != nil {
		t.Fatal(err)
	}
	borrowed, err := OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := borrowed.RegisterSoftwareAgent("retry-terminal", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := borrowed.Journal().Apply(OperationInput{OperationID: OperationID(name + "-genesis"), ActorID: agent.ID, CommandDigest: []byte("genesis"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority", BootstrapLabel: "root"}}})
	if err != nil {
		t.Fatal(err)
	}
	authority, _ := slotJournalID(genesis, "authority")
	journal := &scriptedRetryJournal{Journal: borrowed.Journal()}
	tracker := &scriptedRetryTracker{Tracker: borrowed, journal: journal}
	adapter, err := NewDBOSAdapter(root, tracker, DBOSAdapterConfig{StepOptions: options})
	if err != nil {
		t.Fatal(err)
	}
	stack := &retryTerminalStack{root: root, db: db, borrowed: borrowed, adapter: adapter, journal: journal, actor: agent.ID, authority: authority}
	adapter.testHooks.beforeDomainCommit = func() { stack.callbacks.Add(1) }
	if err := dbos.Launch(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Shutdown(5 * time.Second); _ = borrowed.Close(); _ = db.Close() })
	return stack
}

func (s *retryTerminalStack) reset(failures int, fault error) {
	s.journal.reset(failures, fault)
	s.callbacks.Store(0)
	s.borrowed.(*borrowedTracker).journalApplyFault = nil
}

func (s *retryTerminalStack) operation(id string) OperationInput {
	authority := s.authority
	return OperationInput{OperationID: OperationID(id), ActorID: s.actor, AuthorityJournalID: &authority,
		CommandDigest: []byte("command"), Effects: []Effect{{Sort: EffectTaskCreate, ResultSlot: "task",
			TaskID: TaskID{Namespace: "retry", UUID: uuid.Must(uuid.NewV7())}, Title: "retry", Description: "terminal",
			Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseWorkerSlices}}}
}

func TestDBOSRegisteredBorrowedJournalRetryAndTerminalSemantics(t *testing.T) {
	t.Parallel()
	defaultRetry := newRetryTerminalStack(t, "retry-default-shared", DBOSStepOptions{})
	maxOneRetry := newRetryTerminalStack(t, "retry-max-one-shared", DBOSStepOptions{MaxRetries: 1, BaseInterval: time.Millisecond, BackoffFactor: 1})

	t.Run("succeeds on every default allowed attempt", func(t *testing.T) {
		for failures := 0; failures <= dbosDefaultMaxRetries; failures++ {
			defaultRetry.reset(failures, nil)
			op := defaultRetry.operation(fmt.Sprintf("retry-success-operation-%d", failures))
			result, err := defaultRetry.adapter.Apply(context.Background(), op)
			if err != nil {
				t.Fatalf("failure count %d Apply: %v", failures, err)
			}
			if result.Kind != CommittedExact || defaultRetry.journal.attempts.Load() != int64(failures+1) || defaultRetry.callbacks.Load() != int64(failures+1) || defaultRetry.journal.writes.Load() != 1 {
				t.Fatalf("failure count %d result=%#v attempts=%d callbacks=%d writes=%d", failures, result, defaultRetry.journal.attempts.Load(), defaultRetry.callbacks.Load(), defaultRetry.journal.writes.Load())
			}
		}
	})

	t.Run("domain failure folds once and replay folds zero", func(t *testing.T) {
		defaultRetry.reset(0, nil)
		op := OperationInput{OperationID: "domain-failure-once-operation", ActorID: defaultRetry.actor, CommandDigest: []byte("second-genesis"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority", BootstrapLabel: "second"}}}
		_, firstErr := defaultRetry.adapter.Apply(context.Background(), op)
		if !errors.Is(firstErr, ErrGenesis) || defaultRetry.callbacks.Load() != 1 || defaultRetry.journal.attempts.Load() != 1 || defaultRetry.journal.writes.Load() != 0 {
			t.Fatalf("first domain failure err=%v callbacks=%d attempts=%d writes=%d, want ErrGenesis/1/1/0", firstErr, defaultRetry.callbacks.Load(), defaultRetry.journal.attempts.Load(), defaultRetry.journal.writes.Load())
		}
		beforeReplay := snapshotSQLTables(t, defaultRetry.db, auditedSnapshotTableNames(t, defaultRetry.db)...)
		_, replayErr := defaultRetry.adapter.Apply(context.Background(), op)
		if !errors.Is(replayErr, ErrGenesis) || defaultRetry.callbacks.Load() != 1 || defaultRetry.journal.attempts.Load() != 1 {
			t.Fatalf("domain replay err=%v callbacks=%d attempts=%d, want ErrGenesis/1/1", replayErr, defaultRetry.callbacks.Load(), defaultRetry.journal.attempts.Load())
		}
		afterReplay := snapshotSQLTables(t, defaultRetry.db, auditedSnapshotTableNames(t, defaultRetry.db)...)
		if !reflect.DeepEqual(afterReplay, beforeReplay) {
			t.Fatal("domain failure replay changed durable snapshots")
		}
	})

	for _, faultCase := range []struct {
		name  string
		fault error
	}{{"busy", errors.New("injected SQLite busy")}, {"locked", errors.New("injected SQLite locked")}} {
		faultCase := faultCase
		t.Run("direct borrowed "+faultCase.name+" escapes unchanged after one attempt", func(t *testing.T) {
			maxOneRetry.reset(0, nil)
			borrowed := maxOneRetry.borrowed.(*borrowedTracker)
			fault := faultCase.fault
			var calls atomic.Int64
			borrowed.journalApplyFault = func() error {
				calls.Add(1)
				return fault
			}
			op := maxOneRetry.operation("retry-direct-operation-" + faultCase.name)
			before := snapshotSQLTables(t, maxOneRetry.db, auditedSnapshotTableNames(t, maxOneRetry.db)...)
			_, err := maxOneRetry.borrowed.Journal().Apply(op)
			if err != fault || calls.Load() != 1 {
				t.Fatalf("direct borrowed Apply err=%#v calls=%d, want unchanged fault %#v and one call", err, calls.Load(), fault)
			}
			after := snapshotSQLTables(t, maxOneRetry.db, auditedSnapshotTableNames(t, maxOneRetry.db)...)
			if !reflect.DeepEqual(after, before) {
				t.Fatal("direct borrowed contention failure changed durable snapshots")
			}
			borrowed.journalApplyFault = nil
		})

		t.Run("DBOS "+faultCase.name+" retry recovers", func(t *testing.T) {
			maxOneRetry.reset(0, nil)
			borrowed := maxOneRetry.borrowed.(*borrowedTracker)
			var borrowedCalls atomic.Int64
			borrowed.journalApplyFault = func() error {
				if borrowedCalls.Add(1) == 1 {
					return faultCase.fault
				}
				return nil
			}
			op := maxOneRetry.operation("retry-dbos-operation-" + faultCase.name)
			result, err := maxOneRetry.adapter.Apply(context.Background(), op)
			if err != nil || result.Kind != CommittedExact {
				t.Fatalf("Apply result=%#v err=%v", result, err)
			}
			if maxOneRetry.callbacks.Load() != 2 || borrowedCalls.Load() != 2 || maxOneRetry.journal.attempts.Load() != 2 || maxOneRetry.journal.writes.Load() != 1 {
				t.Fatalf("callbacks=%d borrowedCalls=%d attempts=%d successfulApplies=%d, want 2/2/2/1", maxOneRetry.callbacks.Load(), borrowedCalls.Load(), maxOneRetry.journal.attempts.Load(), maxOneRetry.journal.writes.Load())
			}
			looked, lookupErr := maxOneRetry.borrowed.Journal().LookupCommitted(op.OperationID)
			if lookupErr != nil || looked.Kind != CommittedExact {
				t.Fatalf("recovery did not commit exactly once: result=%#v err=%v", looked, lookupErr)
			}
			beforeReplay := snapshotSQLTables(t, maxOneRetry.db, auditedSnapshotTableNames(t, maxOneRetry.db)...)
			borrowed.journalApplyFault = nil
			_, replayErr := maxOneRetry.adapter.Apply(context.Background(), op)
			if replayErr != nil || maxOneRetry.callbacks.Load() != 2 || borrowedCalls.Load() != 2 {
				t.Fatalf("replay err=%v callbacks=%d borrowedCalls=%d", replayErr, maxOneRetry.callbacks.Load(), borrowedCalls.Load())
			}
			afterReplay := snapshotSQLTables(t, maxOneRetry.db, auditedSnapshotTableNames(t, maxOneRetry.db)...)
			if !reflect.DeepEqual(afterReplay, beforeReplay) {
				t.Fatal("successful retry replay changed durable snapshots")
			}
		})

		t.Run("DBOS "+faultCase.name+" exhaustion is terminal", func(t *testing.T) {
			maxOneRetry.reset(0, nil)
			borrowed := maxOneRetry.borrowed.(*borrowedTracker)
			var borrowedCalls atomic.Int64
			borrowed.journalApplyFault = func() error { borrowedCalls.Add(1); return faultCase.fault }
			op := maxOneRetry.operation("retry-dbos-exhaust-operation-" + faultCase.name)
			_, err := maxOneRetry.adapter.Apply(context.Background(), op)
			assertTerminalDiagnostic(t, err, op.OperationID)
			if maxOneRetry.callbacks.Load() != 2 || borrowedCalls.Load() != 2 || maxOneRetry.journal.attempts.Load() != 2 || maxOneRetry.journal.writes.Load() != 0 {
				t.Fatalf("callbacks=%d borrowedCalls=%d attempts=%d successfulApplies=%d, want 2/2/2/0", maxOneRetry.callbacks.Load(), borrowedCalls.Load(), maxOneRetry.journal.attempts.Load(), maxOneRetry.journal.writes.Load())
			}
			looked, lookupErr := maxOneRetry.borrowed.Journal().LookupCommitted(op.OperationID)
			if lookupErr != nil || looked.Kind != CommittedAbsent {
				t.Fatalf("DBOS exhaustion wrote domain state: result=%#v err=%v", looked, lookupErr)
			}
			beforeReplay := snapshotSQLTables(t, maxOneRetry.db, auditedSnapshotTableNames(t, maxOneRetry.db)...)
			attempts, callbacks, calls := maxOneRetry.journal.attempts.Load(), maxOneRetry.callbacks.Load(), borrowedCalls.Load()
			_, replayErr := maxOneRetry.adapter.Apply(context.Background(), op)
			assertTerminalDiagnostic(t, replayErr, op.OperationID)
			if maxOneRetry.journal.attempts.Load() != attempts || maxOneRetry.callbacks.Load() != callbacks || borrowedCalls.Load() != calls {
				t.Fatalf("terminal replay executed work: attempts=%d/%d callbacks=%d/%d borrowedCalls=%d/%d", attempts, maxOneRetry.journal.attempts.Load(), callbacks, maxOneRetry.callbacks.Load(), calls, borrowedCalls.Load())
			}
			afterReplay := snapshotSQLTables(t, maxOneRetry.db, auditedSnapshotTableNames(t, maxOneRetry.db)...)
			if !reflect.DeepEqual(afterReplay, beforeReplay) {
				t.Fatal("DBOS contention terminal replay changed durable snapshots")
			}
		})
	}

	variants := map[string]func(OperationInput) OperationInput{
		"identical": func(op OperationInput) OperationInput { return op },
		"changed-command": func(op OperationInput) OperationInput {
			op.CommandDigest = []byte("changed-command")
			return op
		},
		"changed-effect": func(op OperationInput) OperationInput {
			op.Effects = append([]Effect(nil), op.Effects...)
			op.Effects[0].Title = "changed canonical effect"
			return op
		},
		"changed-actor": func(op OperationInput) OperationInput {
			op.ActorID = ActorID{Namespace: "retry-other", UUID: uuid.Must(uuid.NewV7())}
			return op
		},
	}
	for name, change := range variants {
		t.Run("terminal replay "+name, func(t *testing.T) {
			maxOneRetry.reset(2, nil)
			op := maxOneRetry.operation("retry-terminal-operation-" + name)
			_, firstErr := maxOneRetry.adapter.Apply(context.Background(), op)
			assertTerminalDiagnostic(t, firstErr, op.OperationID)
			if maxOneRetry.journal.attempts.Load() != 2 || maxOneRetry.callbacks.Load() != 2 || maxOneRetry.journal.writes.Load() != 0 {
				t.Fatalf("exhaustion attempts=%d callbacks=%d writes=%d want 2/2/0", maxOneRetry.journal.attempts.Load(), maxOneRetry.callbacks.Load(), maxOneRetry.journal.writes.Load())
			}
			looked, err := maxOneRetry.borrowed.Journal().LookupCommitted(op.OperationID)
			if err != nil || looked.Kind != CommittedAbsent {
				t.Fatalf("exhaustion wrote a domain operation: result=%#v err=%v", looked, err)
			}
			var firstDiagnostic *DBOSDiagnosticError
			if !errors.As(firstErr, &firstDiagnostic) {
				t.Fatalf("terminal diagnostic missing: %v", firstErr)
			}
			workflows, err := dbos.ListWorkflows(maxOneRetry.root)
			if err != nil {
				t.Fatalf("list terminal workflows: %v", err)
			}
			foundError := false
			for _, workflow := range workflows {
				if workflow.ID == firstDiagnostic.Workflow && workflow.Status == dbos.WorkflowStatusError {
					foundError = true
				}
			}
			if !foundError {
				t.Fatalf("workflow %q is not durably ERROR: %#v", firstDiagnostic.Workflow, workflows)
			}
			workflowID := firstDiagnostic.Workflow
			tables := auditedSnapshotTableNames(t, maxOneRetry.db)
			beforeReplay := snapshotSQLTables(t, maxOneRetry.db, tables...)
			attempts, callbacks, writes := maxOneRetry.journal.attempts.Load(), maxOneRetry.callbacks.Load(), maxOneRetry.journal.writes.Load()
			_, replayErr := maxOneRetry.adapter.Apply(context.Background(), change(op))
			assertTerminalDiagnostic(t, replayErr, op.OperationID)
			var terminal *DBOSDiagnosticError
			if !errors.As(replayErr, &terminal) || terminal.Workflow != workflowID {
				t.Fatalf("replay workflow identity=%q want original %q", terminal.Workflow, workflowID)
			}
			if maxOneRetry.journal.attempts.Load() != attempts || maxOneRetry.callbacks.Load() != callbacks || maxOneRetry.journal.writes.Load() != writes {
				t.Fatalf("terminal replay executed dependency: attempts %d/%d callbacks %d/%d writes %d/%d", attempts, maxOneRetry.journal.attempts.Load(), callbacks, maxOneRetry.callbacks.Load(), writes, maxOneRetry.journal.writes.Load())
			}
			afterReplay := snapshotSQLTables(t, maxOneRetry.db, tables...)
			if !reflect.DeepEqual(afterReplay, beforeReplay) {
				t.Fatalf("terminal same-ID replay changed durable tuples\nbefore=%#v\nafter=%#v", beforeReplay, afterReplay)
			}
		})
	}

	t.Run("terminal error precedes conflicting standalone journal state", func(t *testing.T) {
		maxOneRetry.reset(2, nil)
		op := maxOneRetry.operation("terminal-before-conflict-operation")
		_, firstErr := maxOneRetry.adapter.Apply(context.Background(), op)
		assertTerminalDiagnostic(t, firstErr, op.OperationID)
		standalone := maxOneRetry.operation(string(op.OperationID))
		standalone.CommandDigest = []byte("standalone-conflict")
		standalone.Effects[0].TaskID = TaskID{Namespace: "standalone", UUID: uuid.Must(uuid.NewV7())}
		if _, err := maxOneRetry.borrowed.Journal().Apply(standalone); err != nil {
			t.Fatalf("establish standalone conflict: %v", err)
		}
		assertTerminalPrecedenceReplay(t, maxOneRetry, op, firstErr)
	})

	t.Run("terminal error precedes unavailable journal lookup", func(t *testing.T) {
		maxOneRetry.reset(2, nil)
		op := maxOneRetry.operation("terminal-before-unavailable-operation")
		_, firstErr := maxOneRetry.adapter.Apply(context.Background(), op)
		assertTerminalDiagnostic(t, firstErr, op.OperationID)
		lookups := maxOneRetry.journal.lookups.Load()
		maxOneRetry.journal.setLookupFault(errors.New("journal lookup must not run after terminal DBOS ERROR"))
		assertTerminalPrecedenceReplay(t, maxOneRetry, op, firstErr)
		if maxOneRetry.journal.lookups.Load() != lookups {
			t.Fatalf("terminal replay performed journal lookup: %d -> %d", lookups, maxOneRetry.journal.lookups.Load())
		}
	})

	t.Run("ambiguous domain failure uses Go error path", func(t *testing.T) {
		maxOneRetry.reset(2, errors.Join(ErrGenesis, ErrAuthorityScope))
		op := maxOneRetry.operation("ambiguous-domain-operation")
		_, err := maxOneRetry.adapter.Apply(context.Background(), op)
		assertTerminalDiagnostic(t, err, op.OperationID)
		if !strings.Contains(err.Error(), "ambiguous apply failure") || maxOneRetry.callbacks.Load() != 2 || maxOneRetry.journal.writes.Load() != 0 {
			t.Fatalf("registered ambiguity err=%v callbacks=%d writes=%d", err, maxOneRetry.callbacks.Load(), maxOneRetry.journal.writes.Load())
		}
		looked, lookupErr := maxOneRetry.borrowed.Journal().LookupCommitted(op.OperationID)
		if lookupErr != nil || looked.Kind != CommittedAbsent {
			t.Fatalf("registered ambiguity checkpointed domain state: result=%#v err=%v", looked, lookupErr)
		}
	})
}

func assertTerminalPrecedenceReplay(t *testing.T, s *retryTerminalStack, op OperationInput, firstErr error) {
	t.Helper()
	before := snapshotSQLTables(t, s.db, auditedSnapshotTableNames(t, s.db)...)
	attempts, callbacks, writes := s.journal.attempts.Load(), s.callbacks.Load(), s.journal.writes.Load()
	_, replayErr := s.adapter.Apply(context.Background(), op)
	assertTerminalDiagnostic(t, replayErr, op.OperationID)
	var firstDBOS, replayDBOS *dbos.DBOSError
	if !errors.As(firstErr, &firstDBOS) || !errors.As(replayErr, &replayDBOS) || firstDBOS.Code != replayDBOS.Code || firstDBOS.Message != replayDBOS.Message || firstDBOS.WorkflowID != replayDBOS.WorkflowID {
		t.Fatalf("terminal DBOSError drift: first=%#v replay=%#v", firstDBOS, replayDBOS)
	}
	if s.journal.attempts.Load() != attempts || s.callbacks.Load() != callbacks || s.journal.writes.Load() != writes {
		t.Fatalf("terminal precedence executed dependency: attempts=%d/%d callbacks=%d/%d writes=%d/%d", attempts, s.journal.attempts.Load(), callbacks, s.callbacks.Load(), writes, s.journal.writes.Load())
	}
	after := snapshotSQLTables(t, s.db, auditedSnapshotTableNames(t, s.db)...)
	if !reflect.DeepEqual(after, before) {
		t.Fatal("terminal precedence replay changed durable snapshots")
	}
}

func assertTerminalDiagnostic(t *testing.T, err error, operation OperationID) {
	t.Helper()
	var diagnostic *DBOSDiagnosticError
	var dbosErr *dbos.DBOSError
	if !errors.As(err, &diagnostic) || diagnostic.Class != DBOSDiagClassTerminalRetrieval || diagnostic.Field != DBOSDiagFieldWorkflow || diagnostic.Stage != DBOSDiagStageWorkflowTerminalLookup || diagnostic.Operation != operation || diagnostic.Workflow == "" || diagnostic.Impact == "" || diagnostic.Fix == "" || diagnostic.Cause == nil {
		t.Fatalf("terminal error lacks typed actionable diagnostic: %#v (%v)", diagnostic, err)
	}
	if !errors.As(err, &dbosErr) {
		t.Fatalf("terminal diagnostic does not preserve DBOS errors.As: %v", err)
	}
}
