package provenance

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"zombiezen.com/go/sqlite/sqlitex"
)

func TestCanonicalRetryIgnoresCallerMutationDigestButRejectsEffectChange(t *testing.T) {
	env := newOpsEnv(t)
	boot := env.genesis(t, "canonical-retry-genesis")
	task := env.taskFor(t, "canonical-retry-task")
	base := OperationInput{OperationID: "canonical-retry", ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: []byte("command"), MutationDigest: []byte("untrusted-a"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKindTaskUpdated, Payload: []byte(`{"b":2,"a":1}`)}},
	}
	first, err := env.tr.Journal().Apply(base)
	if err != nil {
		t.Fatal(err)
	}
	retry := base
	retry.MutationDigest = []byte("untrusted-b")
	retry.Effects = []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKindTaskUpdated, Payload: []byte(`{ "a": 1, "b": 2 }`)}}
	second, err := env.tr.Journal().Apply(retry)
	if err != nil {
		t.Fatalf("semantically identical canonical retry: %v", err)
	}
	if !second.ShortCircuited || second.AnchorJournalID != first.AnchorJournalID || !bytes.Equal(base.CommandDigest, retry.CommandDigest) {
		t.Fatalf("retry did not return complete original result: first=%+v retry=%+v", first, second)
	}
	retry.Effects[0].Payload = []byte(`{"a":1,"b":3}`)
	if _, err := env.tr.Journal().Apply(retry); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("changed canonical effect = %v, want ErrOperationConflict", err)
	}
}

func TestCanonicalRetryAcrossIndependentHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "independent.sqlite")
	firstTracker, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer firstTracker.Close()
	actor, err := firstTracker.RegisterSoftwareAgent("canonical", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := firstTracker.Journal().Apply(OperationInput{OperationID: "independent-genesis", ActorID: actor.ID, CommandDigest: []byte("c"), MutationDigest: []byte("ignored"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}})
	if err != nil {
		t.Fatal(err)
	}
	boot, ok := slotJournalID(genesis, "authority")
	if !ok {
		t.Fatal("missing authority")
	}
	taskID := newCorpusTaskID()
	if _, err := firstTracker.Journal().Apply(OperationInput{OperationID: "independent-create", ActorID: actor.ID, AuthorityJournalID: &boot, CommandDigest: []byte("c"), MutationDigest: []byte("ignored"), Effects: []Effect{{Sort: EffectTaskCreate, TaskID: taskID, Title: "task", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}}}); err != nil {
		t.Fatal(err)
	}
	secondTracker, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondTracker.Close()
	op := OperationInput{OperationID: "independent-retry", ActorID: actor.ID, AuthorityJournalID: &boot, CommandDigest: []byte("c"), MutationDigest: []byte("ignored"), Effects: []Effect{{Sort: EffectTaskEvent, TaskID: taskID, EventKind: EventKindTaskUpdated, Payload: []byte(`{"x":1}`)}}}
	var results [2]CommittedResult
	var errs [2]error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); results[0], errs[0] = firstTracker.Journal().Apply(op) }()
	go func() { defer wg.Done(); results[1], errs[1] = secondTracker.Journal().Apply(op) }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("independent handle %d: %v", i, err)
		}
	}
	if results[0].AnchorJournalID != results[1].AnchorJournalID {
		t.Fatalf("independent retries returned different anchors: %+v %+v", results[0], results[1])
	}
	op.OperationID = "independent-conflict"
	left, right := op, op
	left.Effects = []Effect{{Sort: EffectTaskEvent, TaskID: taskID, EventKind: EventKindTaskUpdated, Payload: []byte(`{"winner":"left"}`)}}
	right.Effects = []Effect{{Sort: EffectTaskEvent, TaskID: taskID, EventKind: EventKindTaskUpdated, Payload: []byte(`{"winner":"right"}`)}}
	results = [2]CommittedResult{}
	errs = [2]error{}
	wg.Add(2)
	go func() { defer wg.Done(); results[0], errs[0] = firstTracker.Journal().Apply(left) }()
	go func() { defer wg.Done(); results[1], errs[1] = secondTracker.Journal().Apply(right) }()
	wg.Wait()
	successes, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrOperationConflict) {
			conflicts++
		} else {
			t.Fatalf("independent conflict race returned %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("independent conflict race = %d successes/%d conflicts", successes, conflicts)
	}
}

func TestStartupCanonicalValidationFailsClosedWithoutByteDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.sqlite")
	tracker, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := tracker.RegisterSoftwareAgent("canonical", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := tracker.Journal().Apply(OperationInput{OperationID: "corrupt-genesis", ActorID: actor.ID, CommandDigest: []byte("c"), MutationDigest: []byte("ignored"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}})
	if err != nil {
		t.Fatal(err)
	}
	boot, _ := slotJournalID(genesis, "authority")
	task := newCorpusTaskID()
	if _, err := tracker.Journal().Apply(OperationInput{OperationID: "corrupt-create", ActorID: actor.ID, AuthorityJournalID: &boot, CommandDigest: []byte("c"), MutationDigest: []byte("ignored"), Effects: []Effect{{Sort: EffectTaskCreate, TaskID: task, Title: "canonical title", Description: "description", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}}}); err != nil {
		t.Fatal(err)
	}
	store := tracker.(*sqliteTracker)
	store.db.Lock()
	err = sqlitex.Execute(store.db.Conn(), `UPDATE tasks SET title='corrupt title' WHERE id=?1`, &sqlitex.ExecOptions{Args: []any{task.String()}})
	store.db.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenSQLite(path)
	if opened != nil {
		_ = opened.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "field title") {
		t.Fatalf("startup accepted corruption or returned non-specific error: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed startup validation mutated corrupt database bytes")
	}
}

func TestCanonicalTaskStateSurvivesRestartAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.sqlite")
	tracker, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := tracker.RegisterSoftwareAgent("canonical", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := tracker.Journal().Apply(OperationInput{OperationID: "restart-genesis", ActorID: actor.ID, CommandDigest: []byte("c"), MutationDigest: []byte("ignored"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}})
	if err != nil {
		t.Fatal(err)
	}
	boot, _ := slotJournalID(genesis, "authority")
	session := tracker.As(actor.ID, boot)
	task, err := session.Create("canonical", "title", "description", TaskTypeFeature, PriorityMedium, PhaseWorkerSlices, WithOperationID("restart-create"))
	if err != nil {
		t.Fatal(err)
	}
	title, description, notes := "updated title", "updated description", "complete notes"
	priority, phase := PriorityHigh, PhaseCodeReview
	task, err = session.Update(task.ID, UpdateFields{Title: &title, Description: &description, Notes: &notes, Priority: &priority, Phase: &phase}, WithOperationID("restart-update"))
	if err != nil {
		t.Fatal(err)
	}
	task, err = session.Start(task.ID, WithOperationID("restart-start"))
	if err != nil {
		t.Fatal(err)
	}
	task, err = session.CloseTask(task.ID, "finished", WithOperationID("restart-close"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Show(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, task) {
		t.Fatalf("complete task state changed across restart/replay\n got: %#v\nwant: %#v", got, task)
	}
	if _, err := reopened.Journal().ReplayProjections(); err != nil {
		t.Fatalf("explicit replay after startup: %v", err)
	}
}

func TestCanonicalSchemaMigrationAndMixedLegacyRowsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.sqlite")
	tracker, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	store := tracker.(*sqliteTracker)
	store.db.Lock()
	for _, statement := range []string{`ALTER TABLE journal_operations DROP COLUMN canonical_mutation`, `ALTER TABLE journal_operations DROP COLUMN mutation_encoding_version`} {
		if err = sqlitex.ExecuteTransient(store.db.Conn(), statement, nil); err != nil {
			store.db.Unlock()
			t.Fatal(err)
		}
	}
	store.db.Unlock()
	if err = tracker.Close(); err != nil {
		t.Fatal(err)
	}
	tracker, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("column migration: %v", err)
	}
	actor, err := tracker.RegisterSoftwareAgent("canonical", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := tracker.Journal().Apply(OperationInput{OperationID: "mixed-genesis", ActorID: actor.ID, CommandDigest: []byte("c"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}})
	if err != nil {
		t.Fatal(err)
	}
	boot, _ := slotJournalID(genesis, "authority")
	store = tracker.(*sqliteTracker)
	store.db.Lock()
	err = sqlitex.Execute(store.db.Conn(), `UPDATE journal_operations SET mutation_encoding_version=NULL,canonical_mutation=NULL WHERE operation_id='mixed-genesis'`, nil)
	store.db.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	task := newCorpusTaskID()
	if _, err = tracker.Journal().Apply(OperationInput{OperationID: "mixed-new", ActorID: actor.ID, AuthorityJournalID: &boot, CommandDigest: []byte("c"), Effects: []Effect{{Sort: EffectTaskCreate, TaskID: task, Title: "new", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}}}); err != nil {
		t.Fatal(err)
	}
	if err = tracker.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		tracker, err = OpenSQLite(path)
		if err != nil {
			t.Fatalf("idempotent mixed reopen %d: %v", i, err)
		}
		if err = tracker.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
