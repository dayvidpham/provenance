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

	"github.com/google/uuid"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type allocationCounts struct {
	journal, operations, events, slots, tasks int64
}

func readAllocationCounts(t *testing.T, tr Tracker) allocationCounts {
	t.Helper()
	db := tr.(*sqliteTracker).db
	db.Lock()
	defer db.Unlock()
	counts := allocationCounts{}
	for _, query := range []struct {
		table string
		out   *int64
	}{
		{"journal", &counts.journal},
		{"journal_operations", &counts.operations},
		{"journal_task_events", &counts.events},
		{"journal_operation_result_slots", &counts.slots},
		{"tasks", &counts.tasks},
	} {
		if err := sqlitex.Execute(db.Conn(), "SELECT count(*) FROM "+query.table, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			*query.out = stmt.ColumnInt64(0)
			return nil
		}}); err != nil {
			t.Fatalf("count %s: %v", query.table, err)
		}
	}
	return counts
}

func newAllocationApplyTracker(t *testing.T, path string) (Tracker, ActorID, JournalID) {
	t.Helper()
	tr, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := tr.RegisterSoftwareAgent("allocation", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := tr.Journal().Apply(OperationInput{
		OperationID:   "allocation-proof-genesis",
		ActorID:       actor.ID,
		CommandDigest: []byte("genesis"),
		Effects:       []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	boot, ok := slotJournalID(genesis, "authority")
	if !ok {
		t.Fatal("genesis returned no authority slot")
	}
	return tr, actor.ID, boot
}

func allocatedCreateInput(opID OperationID, actor ActorID, boot JournalID, task TaskID) OperationInput {
	return OperationInput{
		OperationID:        opID,
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("allocated-create-command"),
		Effects: []Effect{{
			Sort: EffectTaskCreateAllocated, ResultSlot: "task", TaskID: task,
			Title: "allocated", Description: "proof", Type: TaskTypeTask,
			Priority: PriorityMedium, Phase: PhaseUnscoped,
		}},
	}
}

func withFreshAllocatedUUID(input OperationInput) OperationInput {
	retry := input
	retry.Effects = append([]Effect(nil), input.Effects...)
	retry.Effects[0].TaskID.UUID = uuid.Must(uuid.NewV7())
	return retry
}

func assertCompleteAllocatedResult(t *testing.T, result CommittedResult, task TaskID) {
	t.Helper()
	if result.Kind != CommittedExact || result.AnchorJournalID == 0 || len(result.EmittedEvents) != 1 || len(result.ResultSlots) != 1 {
		t.Fatalf("incomplete allocated result: %+v", result)
	}
	slot := result.ResultSlots[0]
	if slot.Slot != "task" || slot.ProducedJournalID != result.EmittedEvents[0] || slot.Kind != JournalKindTaskEvent || slot.TaskID == nil || *slot.TaskID != task {
		t.Fatalf("allocated result slot does not preserve name/id/kind/task binding: %+v", slot)
	}
}

func assertSameCompleteResult(t *testing.T, first, retry CommittedResult) {
	t.Helper()
	if first.ShortCircuited {
		t.Fatal("first execution was unexpectedly marked ShortCircuited")
	}
	if !retry.ShortCircuited {
		t.Fatal("retry result was not marked ShortCircuited")
	}
	retry.ShortCircuited = false
	if !reflect.DeepEqual(first, retry) {
		t.Fatalf("complete retry result changed:\nfirst=%+v\nretry=%+v", first, retry)
	}
}

func TestAllocatedCreateApplyReturnsCompleteResultAcrossRetryModes(t *testing.T) {
	t.Parallel()
	t.Run("same-process-arbitrary-operation-id", func(t *testing.T) {
		tr, actor, boot := newAllocationApplyTracker(t, filepath.Join(t.TempDir(), "same.sqlite"))
		defer tr.Close()
		input := allocatedCreateInput("allocated-arbitrary-key", actor, boot, TaskID{Namespace: "allocation", UUID: uuid.Must(uuid.NewV7())})
		first, err := tr.Journal().Apply(input)
		if err != nil {
			t.Fatal(err)
		}
		assertCompleteAllocatedResult(t, first, input.Effects[0].TaskID)
		before := readAllocationCounts(t, tr)
		retry, err := tr.Journal().Apply(withFreshAllocatedUUID(input))
		if err != nil {
			t.Fatal(err)
		}
		assertSameCompleteResult(t, first, retry)
		if after := readAllocationCounts(t, tr); after != before {
			t.Fatalf("same-process retry changed counts: before=%+v after=%+v", before, after)
		}
	})

	t.Run("close-reopen-uuidv7-like-operation-id", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "reopen.sqlite")
		tr, actor, boot := newAllocationApplyTracker(t, path)
		input := allocatedCreateInput(pinnedCreateOperationID(40), actor, boot, TaskID{Namespace: "allocation", UUID: uuid.Must(uuid.NewV7())})
		first, err := tr.Journal().Apply(input)
		if err != nil {
			t.Fatal(err)
		}
		assertCompleteAllocatedResult(t, first, input.Effects[0].TaskID)
		before := readAllocationCounts(t, tr)
		if err := tr.Close(); err != nil {
			t.Fatal(err)
		}
		tr, err = OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		defer tr.Close()
		retry, err := tr.Journal().Apply(withFreshAllocatedUUID(input))
		if err != nil {
			t.Fatal(err)
		}
		assertSameCompleteResult(t, first, retry)
		if after := readAllocationCounts(t, tr); after != before {
			t.Fatalf("reopen retry changed counts: before=%+v after=%+v", before, after)
		}
	})

	t.Run("simultaneous-independent-handles", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "simultaneous.sqlite")
		firstTracker, actor, boot := newAllocationApplyTracker(t, path)
		defer firstTracker.Close()
		secondTracker, err := OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		defer secondTracker.Close()
		before := readAllocationCounts(t, firstTracker)
		inputs := [2]OperationInput{
			allocatedCreateInput("allocated-overlap-key", actor, boot, TaskID{Namespace: "allocation", UUID: uuid.Must(uuid.NewV7())}),
			allocatedCreateInput("allocated-overlap-key", actor, boot, TaskID{Namespace: "allocation", UUID: uuid.Must(uuid.NewV7())}),
		}
		var results [2]CommittedResult
		var errs [2]error
		var wg sync.WaitGroup
		wg.Add(2)
		for i, tr := range []Tracker{firstTracker, secondTracker} {
			go func(i int, tr Tracker) {
				defer wg.Done()
				results[i], errs[i] = tr.Journal().Apply(inputs[i])
			}(i, tr)
		}
		wg.Wait()
		successes := 0
		for _, err := range errs {
			if err == nil {
				successes++
			} else if !isSQLiteContentionError(err) {
				t.Fatalf("simultaneous allocated create returned non-contention error: %v", err)
			}
		}
		if successes == 0 {
			t.Fatalf("simultaneous allocated create made no progress: %v", errs)
		}
		for i, tracker := range []Tracker{firstTracker, secondTracker} {
			results[i], errs[i] = tracker.Journal().LookupCommitted(inputs[i].OperationID)
			if errs[i] != nil || results[i].Kind != CommittedExact {
				t.Fatalf("independent allocation lookup %d: result=%+v err=%v", i, results[i], errs[i])
			}
		}
		if !reflect.DeepEqual(results[0], results[1]) {
			t.Fatalf("independent allocation lookups differ: %+v %+v", results[0], results[1])
		}
		committedTask, ok := taskSlotID(results[0], "task")
		if !ok {
			t.Fatal("winner returned no task slot")
		}
		assertCompleteAllocatedResult(t, results[0], committedTask)
		after := readAllocationCounts(t, firstTracker)
		want := before
		want.journal += 2
		want.operations++
		want.events++
		want.slots++
		want.tasks++
		if after != want {
			t.Fatalf("simultaneous retry left extra/orphan facts: before=%+v after=%+v want=%+v", before, after, want)
		}
	})
}

type allocationCorruptionFixture struct {
	input                             OperationInput
	anchor, sameTask, decision, other JournalID
}

func buildAllocationCorruptionFixture(t *testing.T, path string) (Tracker, allocationCorruptionFixture) {
	t.Helper()
	tr, actor, boot := newAllocationApplyTracker(t, path)
	task := TaskID{Namespace: "allocation", UUID: uuid.Must(uuid.NewV7())}
	input := allocatedCreateInput("allocation-corruption", actor, boot, task)
	input.Effects = append(input.Effects,
		Effect{Sort: EffectTaskEvent, ResultSlot: "same-task", TaskID: task, EventKind: "allocation.followup"},
		Effect{Sort: EffectDecision, ResultSlot: "decision", TaskID: task, DecisionKind: "allocation.decision"},
	)
	result, err := tr.Journal().Apply(input)
	if err != nil {
		t.Fatal(err)
	}
	sameTask, _ := slotJournalID(result, "same-task")
	decision, _ := slotJournalID(result, "decision")
	otherTask := TaskID{Namespace: "allocation", UUID: uuid.Must(uuid.NewV7())}
	other := OperationInput{OperationID: "allocation-other", ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("other"), Effects: []Effect{{Sort: EffectTaskCreate, ResultSlot: "other", TaskID: otherTask, Title: "other", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}}}
	otherResult, err := tr.Journal().Apply(other)
	if err != nil {
		t.Fatal(err)
	}
	otherEvent, _ := slotJournalID(otherResult, "other")
	return tr, allocationCorruptionFixture{input: input, anchor: result.AnchorJournalID, sameTask: sameTask, decision: decision, other: otherEvent}
}

func corruptAllocationFamily(t *testing.T, tr Tracker, anchor JournalID, replacement []byte) {
	t.Helper()
	db := tr.(*sqliteTracker).db
	db.Lock()
	var wire []byte
	err := sqlitex.Execute(db.Conn(), `SELECT canonical_mutation FROM journal_operations WHERE journal_id=?1`, &sqlitex.ExecOptions{Args: []any{int64(anchor)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		wire = make([]byte, stmt.ColumnLen(0))
		stmt.ColumnBytes(0, wire)
		return nil
	}})
	db.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	from := []byte("effect.0.family:21:task_create_allocated\n")
	if bytes.Count(wire, from) != 1 {
		t.Fatalf("allocation family marker count=%d, want one", bytes.Count(wire, from))
	}
	changed := bytes.Replace(wire, from, replacement, 1)
	corruptSQL(t, tr, `UPDATE journal_operations SET canonical_mutation=?1 WHERE journal_id=?2`, changed, int64(anchor))
}

func TestAllocatedCreateCorruptionFailsLiveAndOnOpenWithoutDrift(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate  func(*testing.T, Tracker, allocationCorruptionFixture)
		liveErr error
		openErr error
		token   string
	}{
		"missing-slot": {
			mutate: func(t *testing.T, tr Tracker, f allocationCorruptionFixture) {
				corruptSQL(t, tr, `DELETE FROM journal_operation_result_slots WHERE journal_id=?1 AND result_slot_id='task'`, int64(f.anchor))
			}, liveErr: ErrResultSlotIntegrity, openErr: ErrProjectionDivergence, token: "result slot",
		},
		"renamed-slot": {
			mutate: func(t *testing.T, tr Tracker, f allocationCorruptionFixture) {
				corruptSQL(t, tr, `UPDATE journal_operation_result_slots SET result_slot_id='renamed' WHERE journal_id=?1 AND result_slot_id='task'`, int64(f.anchor))
			}, liveErr: ErrResultSlotIntegrity, openErr: ErrProjectionDivergence, token: "result slot",
		},
		"redirected-foreign-slot": {
			mutate: func(t *testing.T, tr Tracker, f allocationCorruptionFixture) {
				corruptSQL(t, tr, `UPDATE journal_operation_result_slots SET produced_journal_id=?1 WHERE journal_id=?2 AND result_slot_id='task'`, int64(f.other), int64(f.anchor))
			}, liveErr: ErrResultSlotIntegrity, openErr: ErrProjectionDivergence, token: "result slot",
		},
		"non-task-slot": {
			mutate: func(t *testing.T, tr Tracker, f allocationCorruptionFixture) {
				corruptSQL(t, tr, `UPDATE journal_operation_result_slots SET produced_journal_id=?1 WHERE journal_id=?2 AND result_slot_id='task'`, int64(f.decision), int64(f.anchor))
			}, liveErr: ErrResultSlotIntegrity, openErr: ErrProjectionDivergence, token: "result slot",
		},
		"mismatched-produced-row": {
			mutate: func(t *testing.T, tr Tracker, f allocationCorruptionFixture) {
				corruptSQL(t, tr, `UPDATE journal_operation_result_slots SET produced_journal_id=?1 WHERE journal_id=?2 AND result_slot_id='task'`, int64(f.sameTask), int64(f.anchor))
			}, liveErr: ErrResultSlotIntegrity, openErr: ErrProjectionDivergence, token: "result slot",
		},
		"canonical-allocation-marker": {
			mutate: func(t *testing.T, tr Tracker, f allocationCorruptionFixture) {
				corruptAllocationFamily(t, tr, f.anchor, []byte("effect.0.family:21:task_create_allocatiox\n"))
			}, liveErr: ErrCanonicalMutation, openErr: ErrCanonicalMutation, token: "family",
		},
		"canonical-family": {
			mutate: func(t *testing.T, tr Tracker, f allocationCorruptionFixture) {
				corruptAllocationFamily(t, tr, f.anchor, []byte("effect.0.family:11:task_create\n"))
			}, liveErr: ErrOperationConflict, openErr: ErrProjectionDivergence, token: "mutation digest",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "allocation-corrupt.sqlite")
			tr, fixture := buildAllocationCorruptionFixture(t, path)
			test.mutate(t, tr, fixture)
			beforeRetry := readAllocationCounts(t, tr)
			result, err := tr.Journal().Apply(withFreshAllocatedUUID(fixture.input))
			if !errors.Is(err, test.liveErr) {
				t.Fatalf("live reconciliation result=%+v error=%v, want %v", result, err, test.liveErr)
			}
			if afterRetry := readAllocationCounts(t, tr); afterRetry != beforeRetry {
				t.Fatalf("failed live reconciliation wrote facts: before=%+v after=%+v", beforeRetry, afterRetry)
			}
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			beforeOpen, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			opened, openErr := OpenSQLite(path)
			if opened != nil {
				_ = opened.Close()
			}
			if !errors.Is(openErr, test.openErr) {
				t.Fatalf("corrupt allocation opened with %T %v, want %v", openErr, openErr, test.openErr)
			}
			if !strings.Contains(strings.ToLower(openErr.Error()), test.token) {
				t.Fatalf("open error does not identify %q: %v", test.token, openErr)
			}
			afterOpen, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeOpen, afterOpen) {
				t.Fatal("failed Open changed allocation-corrupt database bytes")
			}
		})
	}
}

func TestAllocatedCreateMarkerIsStableCanonicalRelation(t *testing.T) {
	prepared, err := PrepareMutationV1([]Effect{{
		Sort: EffectTaskCreateAllocated, ResultSlot: "task",
		TaskID: TaskID{Namespace: "allocation", UUID: uuid.Must(uuid.NewV7())},
		Title:  "task", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(prepared.CanonicalBytes(), []byte("task_create_allocated")) {
		t.Fatalf("allocated canonical family missing: %q", prepared.CanonicalBytes())
	}
}
