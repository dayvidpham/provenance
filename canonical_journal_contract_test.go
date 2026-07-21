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

	"zombiezen.com/go/sqlite"
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
	if !second.ShortCircuited {
		t.Fatal("retry did not short circuit")
	}
	second.ShortCircuited = false
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("retry did not return complete original result: first=%+v retry=%+v", first, second)
	}
	other, err := env.tr.RegisterSoftwareAgent("canonical", "other", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	differentAuthority := boot + 1000
	cases := map[string]OperationInput{"actor": retry, "nil-authority": retry, "authority": retry, "command": retry, "effect": retry}
	v := cases["actor"]
	v.ActorID = other.ID
	cases["actor"] = v
	v = cases["nil-authority"]
	v.AuthorityJournalID = nil
	cases["nil-authority"] = v
	v = cases["authority"]
	v.AuthorityJournalID = &differentAuthority
	cases["authority"] = v
	v = cases["command"]
	v.CommandDigest = []byte("different")
	cases["command"] = v
	v = cases["effect"]
	v.Effects = []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKindTaskUpdated, Payload: []byte(`{"a":1,"b":3}`)}}
	cases["effect"] = v
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			before := journalRowCount(t, env.tr)
			if _, err := env.tr.Journal().Apply(candidate); !errors.Is(err, ErrOperationConflict) {
				t.Fatalf("mismatch=%v, want conflict", err)
			}
			if after := journalRowCount(t, env.tr); after != before {
				t.Fatalf("conflicting retry wrote journal rows: before=%d after=%d", before, after)
			}
		})
	}
}

func journalRowCount(t *testing.T, tr Tracker) int64 {
	t.Helper()
	db := tr.(*sqliteTracker).db
	db.Lock()
	defer db.Unlock()
	var count int64
	if err := sqlitex.Execute(db.Conn(), `SELECT count(*) FROM journal`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		count = stmt.ColumnInt64(0)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return count
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
	results[0].ShortCircuited = false
	results[1].ShortCircuited = false
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatalf("independent retries returned different complete results: %+v %+v", results[0], results[1])
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

func TestCanonicalExactRetryAfterReopenReturnsCompleteResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retry-reopen.sqlite")
	tr, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := tr.RegisterSoftwareAgent("canonical", "reopen", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := tr.Journal().Apply(OperationInput{OperationID: "retry-reopen-genesis", ActorID: actor.ID, CommandDigest: []byte("genesis"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}})
	if err != nil {
		t.Fatal(err)
	}
	boot, _ := slotJournalID(genesis, "authority")
	task := newCorpusTaskID()
	op := OperationInput{OperationID: "retry-reopen-create", ActorID: actor.ID, AuthorityJournalID: &boot, CommandDigest: []byte("create"), Effects: []Effect{{Sort: EffectTaskCreate, ResultSlot: "task", TaskID: task, Title: "title", Description: "description", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}}}
	first, err := tr.Journal().Apply(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	before := journalRowCount(t, reopened)
	retry, err := reopened.Journal().Apply(op)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.ShortCircuited {
		t.Fatal("reopened exact retry did not short circuit")
	}
	retry.ShortCircuited = false
	if !reflect.DeepEqual(retry, first) {
		t.Fatalf("reopened retry result = %+v, want %+v", retry, first)
	}
	if after := journalRowCount(t, reopened); after != before {
		t.Fatalf("reopened exact retry wrote rows: before=%d after=%d", before, after)
	}
}

func TestPinnedSessionCreateAcrossIndependentHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pinned-create.sqlite")
	first, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	actor, err := first.RegisterSoftwareAgent("canonical", "creator", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := first.Journal().Apply(OperationInput{OperationID: "pinned-create-genesis", ActorID: actor.ID, CommandDigest: []byte("c"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}})
	if err != nil {
		t.Fatal(err)
	}
	boot, _ := slotJournalID(genesis, "authority")
	second, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var tasks [2]Task
	var errs [2]error
	var wg sync.WaitGroup
	wg.Add(2)
	call := func(i int, tr Tracker) {
		defer wg.Done()
		tasks[i], errs[i] = tr.As(actor.ID, boot).Create("canonical", "same", "same", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID("pinned-create-request"))
	}
	go call(0, first)
	go call(1, second)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(tasks[0], tasks[1]) {
		t.Fatalf("concurrent pinned creates differ: %#v %#v", tasks[0], tasks[1])
	}
	listed, err := first.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("created %d tasks, want one", len(listed))
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
	actor, err := tracker.RegisterSoftwareAgent("canonical", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	legacyInput := OperationInput{OperationID: "mixed-genesis", ActorID: actor.ID, CommandDigest: []byte("c"), MutationDigest: []byte("legacy-digest"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}}
	genesis, err := tracker.Journal().Apply(legacyInput)
	if err != nil {
		t.Fatal(err)
	}
	boot, _ := slotJournalID(genesis, "authority")
	makeOperationsSchemaLegacy(t, tracker)
	if err = tracker.Close(); err != nil {
		t.Fatal(err)
	}
	tracker, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("genuine pre-canonical migration: %v", err)
	}
	retried, err := tracker.Journal().Apply(legacyInput)
	if err != nil || !retried.ShortCircuited {
		t.Fatalf("legacy identity/result not preserved: %+v %v", retried, err)
	}
	retried.ShortCircuited = false
	if !reflect.DeepEqual(retried, genesis) {
		t.Fatalf("complete legacy result changed across migration: got=%+v want=%+v", retried, genesis)
	}
	task := newCorpusTaskID()
	if _, err = tracker.Journal().Apply(OperationInput{OperationID: "mixed-new", ActorID: actor.ID, AuthorityJournalID: &boot, CommandDigest: []byte("c"), Effects: []Effect{{Sort: EffectTaskCreate, TaskID: task, Title: "new", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}}}); err != nil {
		t.Fatal(err)
	}
	store := tracker.(*sqliteTracker)
	store.db.Lock()
	legacyOK, canonicalOK := false, false
	err = sqlitex.Execute(store.db.Conn(), `SELECT operation_id,mutation_encoding_version IS NULL,canonical_mutation IS NULL,hex(mutation_digest),length(canonical_mutation) FROM journal_operations ORDER BY journal_id`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		switch stmt.ColumnText(0) {
		case "mixed-genesis":
			legacyOK = stmt.ColumnInt(1) == 1 && stmt.ColumnInt(2) == 1 && stmt.ColumnText(3) == "6C65676163792D646967657374"
		case "mixed-new":
			canonicalOK = stmt.ColumnInt(1) == 0 && stmt.ColumnInt(2) == 0 && stmt.ColumnInt(4) > 0
		}
		return nil
	}})
	store.db.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !legacyOK || !canonicalOK {
		t.Fatalf("mixed rows not preserved: legacy=%v canonical=%v", legacyOK, canonicalOK)
	}
	if err = tracker.Close(); err != nil {
		t.Fatal(err)
	}
	stable, err := os.ReadFile(path)
	if err != nil {
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
		after, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(stable, after) {
			t.Fatalf("idempotent mixed reopen %d changed database bytes", i)
		}
	}
}

func makeOperationsSchemaLegacy(t *testing.T, tracker Tracker) {
	t.Helper()
	store := tracker.(*sqliteTracker)
	store.db.Lock()
	defer store.db.Unlock()
	for _, statement := range []string{
		`DROP TRIGGER journal_operations_canonical_insert`, `DROP TRIGGER journal_operations_canonical_update`,
		`PRAGMA foreign_keys=OFF`, `PRAGMA legacy_alter_table=ON`,
		`ALTER TABLE journal_operations RENAME TO journal_operations_canonical`,
		`CREATE TABLE journal_operations (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id),operation_id TEXT NOT NULL UNIQUE,authority_journal_id INTEGER REFERENCES journal_authorities(journal_id),command_digest BLOB NOT NULL,mutation_digest BLOB NOT NULL) STRICT`,
		`INSERT INTO journal_operations SELECT journal_id,operation_id,authority_journal_id,command_digest,X'6c65676163792d646967657374' FROM journal_operations_canonical`,
		`DROP TABLE journal_operations_canonical`, `PRAGMA foreign_keys=ON`,
	} {
		if err := sqlitex.ExecuteTransient(store.db.Conn(), statement, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCorruptLegacySchemaStartupRollsBackWithoutByteDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-corrupt.sqlite")
	tr, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := tr.RegisterSoftwareAgent("canonical", "legacy", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := tr.Journal().Apply(OperationInput{OperationID: "legacy-corrupt-genesis", ActorID: actor.ID, CommandDigest: []byte("c"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}})
	if err != nil {
		t.Fatal(err)
	}
	authority, ok := slotJournalID(result, "authority")
	if !ok {
		t.Fatal("genesis result missing authority slot")
	}
	makeOperationsSchemaLegacy(t, tr)
	corruptSQL(t, tr, `DELETE FROM journal_authority_bootstraps WHERE journal_id=?1`, int64(authority))
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opened, openErr := OpenSQLite(path)
	if opened != nil {
		_ = opened.Close()
	}
	if openErr == nil {
		t.Fatal("corrupt pre-canonical schema opened")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed legacy-schema startup changed database bytes")
	}
}

func TestMixedLegacyCanonicalMalformedPairsFailWithoutByteDrift(t *testing.T) {
	for name, statement := range map[string]string{
		"version-only":    `UPDATE journal_operations SET canonical_mutation=NULL WHERE operation_id='mixed-pair-new'`,
		"bytes-only":      `UPDATE journal_operations SET mutation_encoding_version=NULL WHERE operation_id='mixed-pair-new'`,
		"unknown-version": `UPDATE journal_operations SET mutation_encoding_version='unknown.v9' WHERE operation_id='mixed-pair-new'`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mixed-malformed.sqlite")
			tr, err := OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			actor, err := tr.RegisterSoftwareAgent("canonical", "mixed-pair", "1", "test")
			if err != nil {
				t.Fatal(err)
			}
			genesis, err := tr.Journal().Apply(OperationInput{OperationID: "mixed-pair-legacy", ActorID: actor.ID, CommandDigest: []byte("c"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}})
			if err != nil {
				t.Fatal(err)
			}
			boot, _ := slotJournalID(genesis, "authority")
			makeOperationsSchemaLegacy(t, tr)
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			tr, err = OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			task := newCorpusTaskID()
			if _, err = tr.Journal().Apply(OperationInput{OperationID: "mixed-pair-new", ActorID: actor.ID, AuthorityJournalID: &boot, CommandDigest: []byte("c"), Effects: []Effect{{Sort: EffectTaskCreate, TaskID: task, Title: "new", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}}}); err != nil {
				t.Fatal(err)
			}
			corruptDDL(t, tr, `DROP TRIGGER journal_operations_canonical_update`)
			corruptSQL(t, tr, statement)
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			opened, openErr := OpenSQLite(path)
			if opened != nil {
				_ = opened.Close()
			}
			if openErr == nil {
				t.Fatal("malformed mixed canonical pair opened")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed mixed startup changed database bytes")
			}
		})
	}
}

func TestDeleteModeCorruptionPreflightIsByteAndModeReadOnly(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "current"
		if legacy {
			name = "legacy-columns"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "delete-mode.sqlite")
			tr, err := OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			actor, err := tr.RegisterSoftwareAgent("canonical", "delete-mode", "1", "test")
			if err != nil {
				t.Fatal(err)
			}
			genesis, err := tr.Journal().Apply(OperationInput{OperationID: "delete-mode-genesis", ActorID: actor.ID, CommandDigest: []byte("c"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}})
			if err != nil {
				t.Fatal(err)
			}
			boot, _ := slotJournalID(genesis, "authority")
			task, err := tr.As(actor.ID, boot).Create("canonical", "title", "description", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID("delete-mode-task"))
			if err != nil {
				t.Fatal(err)
			}
			if legacy {
				makeOperationsSchemaLegacy(t, tr)
			}
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenURI)
			if err != nil {
				t.Fatal(err)
			}
			if err := sqlitex.ExecuteTransient(conn, `PRAGMA journal_mode=DELETE`, nil); err != nil {
				t.Fatal(err)
			}
			if legacy {
				if err := sqlitex.Execute(conn, `DELETE FROM journal_authority_bootstraps WHERE journal_id=?1`, &sqlitex.ExecOptions{Args: []any{int64(boot)}}); err != nil {
					t.Fatal(err)
				}
			} else if err := sqlitex.Execute(conn, `UPDATE tasks SET title='corrupt' WHERE id=?1`, &sqlitex.ExecOptions{Args: []any{task.ID.String()}}); err != nil {
				t.Fatal(err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotSQLiteFiles(t, path)
			opened, openErr := OpenSQLite(path)
			if opened != nil {
				_ = opened.Close()
			}
			if openErr == nil {
				t.Fatal("DELETE-mode corruption opened")
			}
			after := snapshotSQLiteFiles(t, path)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("failed Open changed DELETE-mode database files\nbefore=%v\nafter=%v", before, after)
			}
			if mode := sqliteJournalMode(t, path); mode != "delete" {
				t.Fatalf("journal mode=%q, want delete", mode)
			}
		})
	}
}

func TestDeleteModeActivationSchemaFailureDoesNotPersistWAL(t *testing.T) {
	for name, statements := range map[string][]string{
		"index-name-is-table": {`DROP INDEX idx_tasks_namespace`, `CREATE TABLE idx_tasks_namespace(value TEXT)`},
		"index-name-is-view":  {`DROP INDEX idx_edges_source`, `CREATE VIEW idx_edges_source AS SELECT 1 AS value`},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema-conflict.sqlite")
			tr, err := OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenURI)
			if err != nil {
				t.Fatal(err)
			}
			if err := sqlitex.ExecuteTransient(conn, `PRAGMA journal_mode=DELETE`, nil); err != nil {
				t.Fatal(err)
			}
			for _, statement := range statements {
				if err := sqlitex.ExecuteTransient(conn, statement, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotSQLiteFiles(t, path)
			opened, openErr := OpenSQLite(path)
			if opened != nil {
				_ = opened.Close()
			}
			if openErr == nil {
				t.Fatal("activation-relevant schema corruption opened")
			}
			if !strings.Contains(openErr.Error(), "isolated activation clone") {
				t.Fatalf("schema corruption was not rejected by read-only activation preflight: %v", openErr)
			}
			if after := snapshotSQLiteFiles(t, path); !reflect.DeepEqual(before, after) {
				t.Fatalf("failed Open changed DELETE database\nbefore=%v\nafter=%v", before, after)
			}
			if mode := sqliteJournalMode(t, path); mode != "delete" {
				t.Fatalf("journal mode=%q, want delete", mode)
			}
		})
	}
}

func snapshotSQLiteFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		data, err := os.ReadFile(path + suffix)
		if err == nil {
			result[suffix] = data
			continue
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return result
}

func sqliteJournalMode(t *testing.T, path string) string {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly|sqlite.OpenURI)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	mode := ""
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA journal_mode`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { mode = stmt.ColumnText(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	return mode
}

func TestMissingJournalOperationFKMigrationIsComposableAndIdempotent(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "valid"
		if corrupt {
			name = "corrupt-producer"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing-journal-fk.sqlite")
			tr, err := OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			actor, err := tr.RegisterSoftwareAgent("canonical", "missing-fk", "1", "test")
			if err != nil {
				t.Fatal(err)
			}
			genesis, err := tr.Journal().Apply(OperationInput{OperationID: "missing-fk-genesis", ActorID: actor.ID, CommandDigest: []byte("c"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}}})
			if err != nil {
				t.Fatal(err)
			}
			boot, _ := slotJournalID(genesis, "authority")
			if _, err := tr.As(actor.ID, boot).Create("canonical", "task", "task", TaskTypeTask, PriorityMedium, PhaseUnscoped, WithOperationID("missing-fk-task")); err != nil {
				t.Fatal(err)
			}
			store := tr.(*sqliteTracker)
			if err := store.db.AdversarialRemoveJournalOperationFK(); err != nil {
				t.Fatal(err)
			}
			if corrupt {
				corruptSQL(t, tr, `UPDATE journal SET produced_by_operation_journal_id=999999 WHERE produced_by_operation_journal_id IS NOT NULL AND journal_id=(SELECT max(journal_id) FROM journal)`)
			}
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotSQLiteFiles(t, path)
			tr, err = OpenSQLite(path)
			if corrupt {
				if err == nil {
					_ = tr.Close()
					t.Fatal("invalid producer topology opened")
				}
				if after := snapshotSQLiteFiles(t, path); !reflect.DeepEqual(before, after) {
					t.Fatal("failed pre-FK startup changed corrupt database")
				}
				return
			}
			if err != nil {
				t.Fatalf("supported pre-FK migration: %v", err)
			}
			if !journalOperationFKPresent(t, tr) {
				t.Fatal("migration did not add operation FK")
			}
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			tr, err = OpenSQLite(path)
			if err != nil {
				t.Fatalf("idempotent reopen: %v", err)
			}
			defer tr.Close()
			if !journalOperationFKPresent(t, tr) {
				t.Fatal("operation FK missing after idempotent reopen")
			}
		})
	}
}

func TestCanonicalSQLConstraintsAreVersionAgnostic(t *testing.T) {
	tr, err := OpenSQLite(filepath.Join(t.TempDir(), "generic-codec-schema.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	db := tr.(*sqliteTracker).db
	db.Lock()
	defer db.Unlock()
	if err := sqlitex.Execute(db.Conn(), `SELECT sql FROM sqlite_master WHERE name IN ('journal_operations','journal_operations_canonical_insert','journal_operations_canonical_update')`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		sql := stmt.ColumnText(0)
		if strings.Contains(sql, MutationEncodingV1) {
			t.Fatalf("SQLite schema embeds codec version: %s", sql)
		}
		if !strings.Contains(sql, "mutation_encoding_version") || !strings.Contains(sql, "canonical_mutation") {
			t.Fatalf("schema lost structural canonical pairing: %s", sql)
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestV1SpecificSQLAuthorityMigratesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1-schema.sqlite")
	tr, _ := buildStartupFixture(t, path)
	if err := tr.(*sqliteTracker).db.AdversarialInstallV1OperationConstraint(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	tr, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("migrate V1-specific schema: %v", err)
	}
	assertNoCodecVersionInSQLiteSchema(t, tr)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotSQLiteFiles(t, path)
	tr, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("idempotent reopen: %v", err)
	}
	assertNoCodecVersionInSQLiteSchema(t, tr)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	after := snapshotSQLiteFiles(t, path)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("idempotent generic-schema reopen changed database files")
	}
}

func assertNoCodecVersionInSQLiteSchema(t *testing.T, tr Tracker) {
	t.Helper()
	db := tr.(*sqliteTracker).db
	db.Lock()
	defer db.Unlock()
	if err := sqlitex.Execute(db.Conn(), `SELECT sql FROM sqlite_master WHERE name IN ('journal_operations','journal_operations_canonical_insert','journal_operations_canonical_update')`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		if sql := stmt.ColumnText(0); strings.Contains(sql, MutationEncodingV1) {
			t.Fatalf("SQLite schema embeds codec version: %s", sql)
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
}

func journalOperationFKPresent(t *testing.T, tr Tracker) bool {
	t.Helper()
	db := tr.(*sqliteTracker).db
	db.Lock()
	defer db.Unlock()
	present := false
	if err := sqlitex.ExecuteTransient(db.Conn(), `PRAGMA foreign_key_list(journal)`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		if stmt.ColumnText(3) == "produced_by_operation_journal_id" && stmt.ColumnText(2) == "journal_operations" {
			present = true
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return present
}
