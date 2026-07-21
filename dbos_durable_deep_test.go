package provenance

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
)

type internalDBOSStack struct {
	root            dbos.DBOSContext
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
	root, err := dbos.NewDBOSContext(context.Background(), dbos.Config{AppName: name, SqliteSystemDB: db, ApplicationVersion: "durable-v2"})
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
	t.Cleanup(func() { root.Shutdown(5 * time.Second); _ = tracker.Close(); _ = db.Close() })
	return s
}

func (s *internalDBOSStack) operation(id string) OperationInput {
	authority := s.authority
	return OperationInput{OperationID: OperationID(id), ActorID: s.actor, AuthorityJournalID: &authority, CommandDigest: []byte("command"), MutationDigest: []byte("fixed-caller"), RecordedAt: 100, Effects: []Effect{{Sort: EffectTaskCreate, ResultSlot: "task", TaskID: ptypes.TaskID{Namespace: "aura", UUID: uuid.Must(uuid.NewV7())}, Title: "durable", Description: "complete tuple", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseWorkerSlices}}}
}

func TestDBOSExplicitResponseLossRetrievesCompleteResult(t *testing.T) {
	s := newInternalDBOSStack(t, "response-loss")
	op := s.operation("response-loss-operation")
	input, normalized, err := encodeApplyInputV2(op)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := fingerprintV2(s.root.GetApplicationVersion(), input)
	if err != nil {
		t.Fatal(err)
	}
	workflowID := applyWorkflowIDPrefixV2 + fp
	// Start durable work and deliberately discard the returned handle without ever
	// observing its result, modelling transport/response loss after acceptance.
	if _, err := dbos.RunWorkflow(s.root, s.adapter.applyWorkflowV2, input, dbos.WithWorkflowID(workflowID), dbos.WithApplicationVersion(s.root.GetApplicationVersion())); err != nil {
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

func TestDBOSSimultaneousExactAndChangedMutationRaces(t *testing.T) {
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
