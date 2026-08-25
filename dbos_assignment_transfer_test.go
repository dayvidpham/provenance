package provenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
)

type dbosAssignmentTransferStack struct {
	db      *sql.DB
	root    dbos.DBOSContext
	tracker Tracker
	adapter *DBOSAdapter
	entries atomic.Int64
}

func openDBOSAssignmentTransferStack(t *testing.T, path, appName, version string) *dbosAssignmentTransferStack {
	t.Helper()
	db, err := openSharedSQL(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := dbos.NewDBOSContext(context.Background(), dbos.Config{
		AppName: appName, SqliteSystemDB: db, ApplicationVersion: version,
	})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	tracker, err := OpenBorrowedSQLite(db)
	if err != nil {
		root.Shutdown(5 * time.Second)
		_ = db.Close()
		t.Fatal(err)
	}
	adapter, err := NewDBOSAdapter(root, tracker, DBOSAdapterConfig{})
	if err != nil {
		_ = tracker.Close()
		root.Shutdown(5 * time.Second)
		_ = db.Close()
		t.Fatal(err)
	}
	stack := &dbosAssignmentTransferStack{db: db, root: root, tracker: tracker, adapter: adapter}
	adapter.testHooks.onWorkflowEntry = func() { stack.entries.Add(1) }
	if err := dbos.Launch(root); err != nil {
		stack.close()
		t.Fatal(err)
	}
	return stack
}

func (s *dbosAssignmentTransferStack) close() {
	if s.root != nil {
		s.root.Shutdown(5 * time.Second)
		s.root = nil
	}
	if s.tracker != nil {
		_ = s.tracker.Close()
		s.tracker = nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

func establishDBOSAssignmentTransferFixture(t *testing.T, stack *dbosAssignmentTransferStack, label string) (ActorID, ActorID, JournalID, TaskID, JournalID) {
	t.Helper()
	actorA, err := stack.tracker.RegisterSoftwareAgent("dbos-transfer", "actor-a-"+label, "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent(actor-a): %v", err)
	}
	actorB, err := stack.tracker.RegisterSoftwareAgent("dbos-transfer", "actor-b-"+label, "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent(actor-b): %v", err)
	}
	genesis, err := stack.tracker.Journal().Apply(OperationInput{
		OperationID:   OperationID("dbos-transfer-genesis-" + label),
		ActorID:       actorA.ID,
		CommandDigest: []byte("dbos-transfer-genesis-" + label),
		Effects: []Effect{{
			Sort: EffectBootstrapAuthority, ResultSlot: "authority", BootstrapLabel: "dbos-transfer-root",
		}},
	})
	if err != nil {
		t.Fatalf("transfer genesis: %v", err)
	}
	boot, ok := slotJournalID(genesis, "authority")
	if !ok {
		t.Fatal("transfer genesis produced no authority")
	}
	task := TaskID{Namespace: "dbos-transfer", UUID: uuid.Must(uuid.NewV7())}
	if _, err := stack.tracker.Journal().Apply(OperationInput{
		OperationID: OperationID("dbos-transfer-task-" + label), ActorID: actorA.ID, AuthorityJournalID: &boot,
		CommandDigest: []byte("dbos-transfer-task-" + label),
		Effects: []Effect{{
			Sort: EffectTaskCreate, TaskID: task, Title: "dbos transfer " + label,
			Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseWorkerSlices,
		}},
	}); err != nil {
		t.Fatalf("create transfer task: %v", err)
	}
	previous := AssignmentID("DBOS-TRANSFER-PREVIOUS-" + label)
	started, err := stack.tracker.Journal().Apply(OperationInput{
		OperationID: OperationID("dbos-transfer-start-" + label), ActorID: actorA.ID, AuthorityJournalID: &boot,
		CommandDigest: []byte("dbos-transfer-start-" + label),
		Effects: []Effect{{
			Sort: EffectAssignmentStart, ResultSlot: "authority", TaskID: task,
			AssignmentID: previous, SlotID: SlotOwnerResponsibility, Occupant: actorA.ID,
		}},
	})
	if err != nil {
		t.Fatalf("start transfer predecessor: %v", err)
	}
	authority, ok := slotJournalID(started, "authority")
	if !ok {
		t.Fatal("predecessor start produced no authority")
	}
	return actorA.ID, actorB.ID, boot, task, authority
}

func assertDBOSAssignmentTransferOwner(t *testing.T, tracker Tracker, taskID TaskID, want *ActorID) {
	t.Helper()
	task, err := tracker.Show(taskID)
	if err != nil {
		t.Fatalf("Show(%q): %v", taskID, err)
	}
	if (task.Owner == nil) != (want == nil) || task.Owner != nil && *task.Owner != *want {
		t.Fatalf("task owner = %v, want %v", task.Owner, want)
	}
}

func TestDBOSAdapterTransferAssignmentSuccessAndReplayAcrossWorkflows(t *testing.T) {
	path := t.TempDir() + "/transfer.db"
	const appName = "dbos-assignment-transfer"
	firstStack := openDBOSAssignmentTransferStack(t, path, appName, "transfer-v1")
	actorA, actorB, _, task, authority := establishDBOSAssignmentTransferFixture(t, firstStack, "success")
	request := transferRequest(task, "DBOS-TRANSFER-PREVIOUS-success", "DBOS-TRANSFER-NEXT-success", actorB)
	operation := OperationID("dbos-transfer-success")

	first, err := firstStack.adapter.TransferAssignment(context.Background(), firstStack.tracker.As(actorA, authority), request, WithOperationID(operation))
	if err != nil {
		t.Fatalf("first durable transfer: %v", err)
	}
	want := assignmentTransferResult(request, false)
	if first != want {
		t.Fatalf("first durable transfer = %+v, want %+v", first, want)
	}
	if firstStack.entries.Load() != 1 {
		t.Fatalf("first workflow entries = %d, want 1", firstStack.entries.Load())
	}

	sameWorkflow, err := firstStack.adapter.TransferAssignment(context.Background(), firstStack.tracker.As(actorA, authority), request, WithOperationID(operation))
	if err != nil {
		t.Fatalf("same-workflow replay: %v", err)
	}
	want.Replayed = true
	if sameWorkflow != want || firstStack.entries.Load() != 1 {
		t.Fatalf("same-workflow replay = %+v entries=%d, want %+v and one entry", sameWorkflow, firstStack.entries.Load(), want)
	}
	assertDBOSAssignmentTransferOwner(t, firstStack.tracker, task, &actorB)
	firstStack.close()

	// Same application version recovers and attaches to the original workflow
	// without executing another callback.
	sameVersion := openDBOSAssignmentTransferStack(t, path, appName, "transfer-v1")
	reopened, err := sameVersion.adapter.TransferAssignment(context.Background(), sameVersion.tracker.As(actorA, authority), request, WithOperationID(operation))
	if err != nil {
		t.Fatalf("reopened same-workflow replay: %v", err)
	}
	if reopened != want || sameVersion.entries.Load() != 0 {
		t.Fatalf("reopened same-workflow replay = %+v entries=%d, want %+v and zero entries", reopened, sameVersion.entries.Load(), want)
	}
	sameVersion.close()

	// A different application version has a distinct DBOS workflow identity, but
	// the existing core operation still wins admission and returns its semantic
	// result as a replay.
	distinctVersion := openDBOSAssignmentTransferStack(t, path, appName, "transfer-v2")
	distinct, err := distinctVersion.adapter.TransferAssignment(context.Background(), distinctVersion.tracker.As(actorA, authority), request, WithOperationID(operation))
	if err != nil {
		t.Fatalf("distinct-workflow replay: %v", err)
	}
	if distinct != want || distinctVersion.entries.Load() != 1 {
		t.Fatalf("distinct-workflow replay = %+v entries=%d, want %+v and one core replay callback", distinct, distinctVersion.entries.Load(), want)
	}
	assertDBOSAssignmentTransferOwner(t, distinctVersion.tracker, task, &actorB)
	distinctVersion.close()
}

func TestDBOSAdapterTransferAssignmentChangedInputConflicts(t *testing.T) {
	stack := openDBOSAssignmentTransferStack(t, t.TempDir()+"/transfer.db", "dbos-transfer-conflict", "transfer-v1")
	defer stack.close()
	actorA, actorB, _, task, authority := establishDBOSAssignmentTransferFixture(t, stack, "conflict")
	operation := OperationID("dbos-transfer-conflict")
	if _, err := stack.adapter.TransferAssignment(context.Background(), stack.tracker.As(actorA, authority), transferRequest(task, "DBOS-TRANSFER-PREVIOUS-conflict", "DBOS-TRANSFER-NEXT-B-conflict", actorB), WithOperationID(operation)); err != nil {
		t.Fatalf("first durable transfer: %v", err)
	}
	entries := stack.entries.Load()
	_, err := stack.adapter.TransferAssignment(context.Background(), stack.tracker.As(actorA, authority), transferRequest(task, "DBOS-TRANSFER-PREVIOUS-conflict", "DBOS-TRANSFER-NEXT-C-conflict", actorA), WithOperationID(operation))
	if !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("changed-input durable retry error = %v, want ErrOperationConflict", err)
	}
	if stack.entries.Load() != entries {
		t.Fatalf("changed input entered a workflow: entries %d -> %d", entries, stack.entries.Load())
	}
	assertDBOSAssignmentTransferOwner(t, stack.tracker, task, &actorB)
}

func TestDBOSAdapterTransferAssignmentRevocationRaceParity(t *testing.T) {
	stack := openDBOSAssignmentTransferStack(t, t.TempDir()+"/transfer.db", "dbos-transfer-race", "transfer-v1")
	defer stack.close()
	actorA, actorB, boot, _, _ := establishDBOSAssignmentTransferFixture(t, stack, "race-seed")

	const iterations = 12
	for index := 0; index < iterations; index++ {
		suffix := fmt.Sprintf("race-%d", index)
		task := TaskID{Namespace: "dbos-transfer", UUID: uuid.Must(uuid.NewV7())}
		if _, err := stack.tracker.Journal().Apply(OperationInput{
			OperationID: OperationID("dbos-transfer-race-task-" + suffix), ActorID: actorA, AuthorityJournalID: &boot,
			CommandDigest: []byte("dbos-transfer-race-task-" + suffix),
			Effects:       []Effect{{Sort: EffectTaskCreate, TaskID: task, Title: suffix, Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseWorkerSlices}},
		}); err != nil {
			t.Fatalf("iteration %d create task: %v", index, err)
		}
		previous := AssignmentID("DBOS-TRANSFER-RACE-PREVIOUS-" + suffix)
		started, err := stack.tracker.Journal().Apply(OperationInput{
			OperationID: OperationID("dbos-transfer-race-start-" + suffix), ActorID: actorA, AuthorityJournalID: &boot,
			CommandDigest: []byte("dbos-transfer-race-start-" + suffix),
			Effects:       []Effect{{Sort: EffectAssignmentStart, ResultSlot: "authority", TaskID: task, AssignmentID: previous, SlotID: SlotOwnerResponsibility, Occupant: actorA}},
		})
		if err != nil {
			t.Fatalf("iteration %d start predecessor: %v", index, err)
		}
		authority, ok := slotJournalID(started, "authority")
		if !ok {
			t.Fatalf("iteration %d predecessor start produced no authority", index)
		}

		transferOperation := OperationID("dbos-transfer-race-transfer-" + suffix)
		revokeOperation := OperationID("dbos-transfer-race-revoke-" + suffix)
		request := transferRequest(task, previous, AssignmentID("DBOS-TRANSFER-RACE-NEXT-"+suffix), actorB)
		revoke := OperationInput{
			OperationID: revokeOperation, ActorID: actorA, AuthorityJournalID: &boot,
			CommandDigest: []byte("dbos-transfer-race-revoke-" + suffix),
			Effects:       []Effect{{Sort: EffectAssignmentEnd, AssignmentID: previous, TaskID: task, SlotID: SlotOwnerResponsibility}},
		}
		var (
			wait                       sync.WaitGroup
			transferErr, revocationErr error
		)
		start := make(chan struct{})
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			_, transferErr = stack.adapter.TransferAssignment(context.Background(), stack.tracker.As(actorA, authority), request, WithOperationID(transferOperation))
		}()
		go func() {
			defer wait.Done()
			<-start
			_, revocationErr = stack.tracker.Journal().Apply(revoke)
		}()
		close(start)
		wait.Wait()

		switch {
		case transferErr == nil && errors.Is(revocationErr, ErrStaleEpisode):
			assertDBOSAssignmentTransferOwner(t, stack.tracker, task, &actorB)
		case revocationErr == nil && errors.Is(transferErr, ErrStaleEpisode):
			assertDBOSAssignmentTransferOwner(t, stack.tracker, task, nil)
		default:
			t.Fatalf("iteration %d transfer=%v revocation=%v, want one winner and one ErrStaleEpisode loser", index, transferErr, revocationErr)
		}
	}
	if err := stack.tracker.Journal().VerifyIntegrity(); err != nil {
		t.Fatalf("VerifyIntegrity after durable transfer/revocation races: %v", err)
	}
	if _, err := stack.tracker.Journal().ReplayProjections(); err != nil {
		t.Fatalf("ReplayProjections after durable transfer/revocation races: %v", err)
	}
}
