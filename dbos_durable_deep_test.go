package provenance

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
)

type duplicateLookupInput struct{}

func (duplicateLookupInput) MarshalJSON() ([]byte, error) {
	return []byte(`{"schema":"provenance.dbos.apply-input","context":"Yw==","mutation":{"nested":1,"nested":2}}`), nil
}

func (*duplicateLookupInput) UnmarshalJSON([]byte) error { return nil }

func duplicateLookupWorkflow(dbos.Context, duplicateLookupInput) (string, error) {
	return "seeded", nil
}

type internalDBOSStack struct {
	root            dbos.Context
	dbPath          string
	tracker         Tracker
	adapter         *DBOSAdapter
	actor           ActorID
	authority       JournalID
	workflowEntries atomic.Int64
}

func newInternalDBOSStack(t *testing.T, name string) *internalDBOSStack {
	t.Helper()
	path := t.TempDir() + "/durable.db"
	db, err := openSharedSQL(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := dbos.NewContext(context.Background(), dbos.Config{AppName: name, SQLiteSystemDB: db, ApplicationVersion: "durable-current"})
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := tracker.RegisterSoftwareAgent("durable", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := tracker.Journal().Apply(OperationInput{OperationID: OperationID(name + "-genesis"), ActorID: agent.ID, CommandDigest: []byte("genesis"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority", BootstrapLabel: "root"}}})
	if err != nil {
		t.Fatal(err)
	}
	authority, _ := slotJournalID(genesis, "authority")
	adapter, err := NewDBOSAdapter(root, tracker, DBOSAdapterConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := &internalDBOSStack{root: root, dbPath: path, tracker: tracker, adapter: adapter, actor: agent.ID, authority: authority}
	adapter.testHooks.onWorkflowEntry = func() { s.workflowEntries.Add(1) }
	if err := dbos.Launch(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownDBOSRoot(t, root, 5*time.Second); _ = tracker.Close(); _ = db.Close() })
	return s
}

func (s *internalDBOSStack) operation(id string) OperationInput {
	authority := s.authority
	return OperationInput{OperationID: OperationID(id), ActorID: s.actor, AuthorityJournalID: &authority, CommandDigest: []byte("command"), MutationDigest: []byte("fixed-caller"), RecordedAt: 100, Effects: []Effect{{Sort: EffectTaskCreate, ResultSlot: "task", TaskID: ptypes.TaskID{Namespace: "aura", UUID: uuid.Must(uuid.NewV7())}, Title: "durable", Description: "complete tuple", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseWorkerSlices}}}
}

func TestDBOSExplicitResponseLossRetrievesCompleteResult(t *testing.T) {
	t.Parallel()
	s := newInternalDBOSStack(t, "response-loss")
	op := s.operation("response-loss-operation")
	input, normalized, err := encodeApplyInput(s.adapter.contract, op)
	if err != nil {
		t.Fatal(err)
	}
	workflowID := s.adapter.contract.workflowPrefix + workflowIdentity(s.adapter.contract, s.root.GetApplicationVersion(), normalized.OperationID)
	// Start durable work and deliberately discard the returned handle without ever
	// observing its result, modelling transport/response loss after acceptance.
	if _, err := dbos.RunWorkflow(s.root, s.adapter.applyWorkflow, input, dbos.WithWorkflowID(workflowID), dbos.WithApplicationVersion(s.root.GetApplicationVersion())); err != nil {
		t.Fatalf("start discarded-response workflow: %v", err)
	}
	got, err := s.adapter.Apply(context.Background(), op)
	if err != nil {
		t.Fatalf("retry after response loss: %v", err)
	}
	looked, err := s.tracker.Journal().LookupCommitted(normalized.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, looked) || s.workflowEntries.Load() != 1 {
		t.Fatalf("response-loss recovery drift: got=%#v looked=%#v workflowEntries=%d", got, looked, s.workflowEntries.Load())
	}
	task, err := s.tracker.Show(normalized.Effects[0].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != normalized.Effects[0].TaskID || task.Title != "durable" || task.Description != "complete tuple" || task.Status != StatusOpen || task.Type != TaskTypeTask || task.Priority != PriorityMedium || task.Phase != PhaseWorkerSlices || task.Owner != nil || task.Notes != "" || task.CreatedAt.UnixNano() != normalized.RecordedAt || !task.UpdatedAt.Equal(task.CreatedAt) || task.ClosedAt != nil || task.CloseReason != "" {
		t.Fatalf("response-loss complete task tuple drifted: %#v", task)
	}
}

func TestDBOSApplyRejectsDuplicateStoredInputBeforeCallbacksOrWrites(t *testing.T) {
	path := t.TempDir() + "/lookup-boundary.db"
	db, err := openSharedSQL(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := dbos.NewContext(context.Background(), dbos.Config{AppName: "lookup-boundary", SQLiteSystemDB: db, ApplicationVersion: "lookup-boundary"})
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := tracker.RegisterSoftwareAgent("lookup", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := tracker.Journal().Apply(OperationInput{OperationID: "lookup-boundary-genesis", ActorID: agent.ID, CommandDigest: []byte("genesis"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority", BootstrapLabel: "root"}}})
	if err != nil {
		t.Fatal(err)
	}
	authority, _ := slotJournalID(genesis, "authority")
	adapter, err := NewDBOSAdapter(root, tracker, DBOSAdapterConfig{})
	if err != nil {
		t.Fatal(err)
	}
	dbos.RegisterWorkflow(root, duplicateLookupWorkflow)
	var workflowEntries atomic.Int64
	adapter.testHooks.onWorkflowEntry = func() { workflowEntries.Add(1) }
	if err := dbos.Launch(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownDBOSRoot(t, root, 5*time.Second); _ = tracker.Close(); _ = db.Close() })

	op := OperationInput{OperationID: "lookup-boundary-operation", ActorID: agent.ID, AuthorityJournalID: &authority, CommandDigest: []byte("command"), Effects: []Effect{{Sort: EffectTaskCreate, ResultSlot: "task", TaskID: ptypes.TaskID{Namespace: "lookup", UUID: uuid.Must(uuid.NewV7())}, Title: "must-not-write", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseWorkerSlices}}}
	_, normalized, err := encodeApplyInput(adapter.contract, op)
	if err != nil {
		t.Fatal(err)
	}
	workflowID := adapter.contract.workflowPrefix + workflowIdentity(adapter.contract, root.GetApplicationVersion(), normalized.OperationID)
	handle, err := dbos.RunWorkflow(root, duplicateLookupWorkflow, duplicateLookupInput{}, dbos.WithWorkflowID(workflowID), dbos.WithApplicationVersion(root.GetApplicationVersion()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.GetResult(); err != nil {
		t.Fatal(err)
	}

	tables := auditedSnapshotTableNames(t, db)
	before := snapshotSQLTables(t, db, tables...)
	_, applyErr := adapter.Apply(context.Background(), op)
	var diagnostic *DBOSDiagnosticError
	if !errors.As(applyErr, &diagnostic) || diagnostic.Class != DBOSDiagClassTerminalRetrieval || diagnostic.Stage != DBOSDiagStageWorkflowTerminalLookup || !strings.Contains(applyErr.Error(), "duplicate JSON object key") {
		t.Fatalf("Apply duplicate-input rejection=%#v err=%v", diagnostic, applyErr)
	}
	if workflowEntries.Load() != 0 {
		t.Fatalf("malformed lookup entered adapter workflow %d times", workflowEntries.Load())
	}
	looked, err := tracker.Journal().LookupCommitted(op.OperationID)
	if err != nil || looked.Kind != CommittedAbsent {
		t.Fatalf("malformed lookup wrote journal operation: result=%#v err=%v", looked, err)
	}
	after := snapshotSQLTables(t, db, tables...)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("malformed lookup changed durable tuples\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestDBOSSimultaneousExactAndChangedMutationRaces(t *testing.T) {
	t.Parallel()
	s := newInternalDBOSStack(t, "adapter-race")
	op := s.operation("adapter-race-operation")
	const observers = 16
	results := make([]CommittedResult, observers)
	errs := make([]error, observers)
	var wg sync.WaitGroup
	for i := range observers {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = s.adapter.Apply(context.Background(), op) }(i)
	}
	wg.Wait()
	for i := range observers {
		if errs[i] != nil {
			t.Fatalf("initial exact racer %d: %v", i, errs[i])
		}
		if !reflect.DeepEqual(results[i], results[0]) {
			t.Fatalf("initial racer %d result=%#v want=%#v", i, results[i], results[0])
		}
	}
	if s.workflowEntries.Load() != 1 {
		t.Fatalf("initial exact race workflow entries=%d want 1", s.workflowEntries.Load())
	}

	for i := range observers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			candidate := op
			candidate.Effects = append([]Effect(nil), op.Effects...)
			if i%2 == 1 {
				candidate.Effects[0].Title = "forbidden-change"
			}
			results[i], errs[i] = s.adapter.Apply(context.Background(), candidate)
		}(i)
	}
	wg.Wait()
	for i := range observers {
		if i%2 == 0 {
			if errs[i] != nil || !reflect.DeepEqual(results[i], results[0]) {
				t.Fatalf("exact completed racer %d result=%#v err=%v", i, results[i], errs[i])
			}
		} else if !errors.Is(errs[i], journal.ErrOperationConflict) {
			t.Fatalf("changed completed racer %d err=%v want conflict", i, errs[i])
		}
	}
	if s.workflowEntries.Load() != 1 {
		t.Fatalf("completed exact/changed race workflow entries=%d want still 1", s.workflowEntries.Load())
	}
	looked, err := s.tracker.Journal().LookupCommitted(op.OperationID)
	if err != nil || !reflect.DeepEqual(looked, results[0]) {
		t.Fatalf("race committed result=%#v err=%v want=%#v", looked, err, results[0])
	}
	tasks, err := s.tracker.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	matching := 0
	for _, task := range tasks {
		if task.ID == op.Effects[0].TaskID {
			matching++
			if task.Title != op.Effects[0].Title || task.Description != op.Effects[0].Description || task.Status != StatusOpen || task.Type != op.Effects[0].Type || task.Priority != op.Effects[0].Priority || task.Phase != op.Effects[0].Phase || task.Owner != nil || task.Notes != "" || task.CreatedAt.UnixNano() != op.RecordedAt || !task.UpdatedAt.Equal(task.CreatedAt) || task.ClosedAt != nil || task.CloseReason != "" {
				t.Fatalf("race complete task tuple drifted: %#v", task)
			}
		}
	}
	if matching != 1 {
		t.Fatalf("race task copies=%d want 1", matching)
	}
}
