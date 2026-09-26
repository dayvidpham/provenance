package provenance_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	p "github.com/dayvidpham/provenance"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const (
	actorOwnershipMaterialKind = p.EventKind("pasture.ownership.material")
	actorOwnershipEvidenceKind = p.EvidenceKind("pasture.ownership.evidence")
)

type actorOwnershipStore struct {
	tracker p.Tracker
	api     p.ActorOwnershipQueryAPI
}

func openActorOwnershipStore(t *testing.T, backing string) actorOwnershipStore {
	t.Helper()
	var (
		tracker p.Tracker
		err     error
	)
	switch backing {
	case "memory":
		tracker, err = p.OpenMemory(p.WithModelRegistry(p.NewRegistry(nil)))
	case "sqlite":
		tracker, err = p.OpenSQLite(filepath.Join(t.TempDir(), "actor-ownership.db"), p.WithModelRegistry(p.NewRegistry(nil)))
	case "borrowed":
		db, _ := openFileDB(t)
		tracker, err = p.OpenBorrowedSQLite(db, p.WithModelRegistry(p.NewRegistry(nil)))
	default:
		t.Fatalf("unknown actor-ownership backing %q", backing)
	}
	if err != nil {
		t.Fatalf("open %s actor-ownership store: %v", backing, err)
	}
	t.Cleanup(func() { _ = tracker.Close() })
	api, ok := tracker.Journal().(p.ActorOwnershipQueryAPI)
	if !ok {
		t.Fatalf("%s Journal lacks ActorOwnershipQueryAPI", backing)
	}
	return actorOwnershipStore{tracker: tracker, api: api}
}

func registerActorOwnershipActor(t *testing.T, tracker p.Tracker, namespace, name string) p.ActorID {
	t.Helper()
	agent, err := tracker.RegisterSoftwareAgent(namespace, name, "1", "actor-ownership-test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent(%s/%s): %v", namespace, name, err)
	}
	return agent.ID
}

func actorOwnershipGenesis(t *testing.T, tracker p.Tracker, actor p.ActorID, operation p.OperationID) p.JournalID {
	t.Helper()
	result, err := tracker.Journal().Apply(p.OperationInput{
		OperationID: operation, ActorID: actor, CommandDigest: []byte(operation + "-command"),
		Effects: []p.Effect{{Sort: p.EffectBootstrapAuthority, BootstrapLabel: "actor-ownership", ResultSlot: "bootstrap"}},
	})
	if err != nil {
		t.Fatalf("actor-ownership genesis: %v", err)
	}
	for _, slot := range result.ResultSlots {
		if slot.Slot == "bootstrap" {
			return slot.ProducedJournalID
		}
	}
	t.Fatal("actor-ownership genesis produced no bootstrap slot")
	return 0
}

func createActorOwnershipTask(t *testing.T, tracker p.Tracker, actor p.ActorID, boot p.JournalID, label string, phase p.Phase) p.TaskID {
	t.Helper()
	task, err := tracker.As(actor, boot).Create("actor-ownership", label, "", p.TaskTypeTask, p.PriorityMedium, phase)
	if err != nil {
		t.Fatalf("Create(%s): %v", label, err)
	}
	return task.ID
}

type actorOwnershipStart struct {
	started  p.JournalID
	material p.JournalID
	evidence p.JournalID
}

func startActorOwnershipTask(t *testing.T, tracker p.Tracker, actor, occupant p.ActorID, boot p.JournalID, task p.TaskID, operation p.OperationID, payload []byte, evidence bool) actorOwnershipStart {
	t.Helper()
	effects := []p.Effect{
		{Sort: p.EffectAssignmentStart, ResultSlot: "start", AssignmentID: p.AssignmentID(operation), TaskID: task, SlotID: p.SlotOwnerResponsibility, Occupant: occupant},
		{Sort: p.EffectTaskEvent, ResultSlot: "material", TaskID: task, EventKind: actorOwnershipMaterialKind, Payload: append([]byte(nil), payload...)},
	}
	if evidence {
		effects = append(effects, p.Effect{Sort: p.EffectEvidence, ResultSlot: "evidence", TaskID: task, EvidenceKind: actorOwnershipEvidenceKind, ContentDigest: []byte(operation + "-digest"), Payload: []byte(`{"evidence":"owned"}`)})
	}
	result, err := tracker.Journal().Apply(p.OperationInput{OperationID: operation, ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte(operation + "-command"), Effects: effects})
	if err != nil {
		t.Fatalf("start %s: %v", task, err)
	}
	var out actorOwnershipStart
	for _, slot := range result.ResultSlots {
		switch slot.Slot {
		case "start":
			out.started = slot.ProducedJournalID
		case "material":
			out.material = slot.ProducedJournalID
		case "evidence":
			out.evidence = slot.ProducedJournalID
		}
	}
	if out.started == 0 || out.material == 0 || evidence && out.evidence == 0 {
		t.Fatalf("start %s incomplete receipt: %+v", task, out)
	}
	return out
}

func TestActorOwnershipReturnsOwnedTasksWithPhaseAndMaterial(t *testing.T) {
	for _, backing := range []string{"memory", "sqlite", "borrowed"} {
		t.Run(backing, func(t *testing.T) {
			store := openActorOwnershipStore(t, backing)
			actor := registerActorOwnershipActor(t, store.tracker, "actor-ownership-"+backing, "owner")
			boot := actorOwnershipGenesis(t, store.tracker, actor, p.OperationID("genesis-"+backing))
			query := p.ActorOwnershipQuery{Actor: actor, MaterialKinds: []p.EventKind{actorOwnershipMaterialKind}}

			empty, err := store.api.QueryActorOwnership(context.Background(), query)
			if err != nil || !empty.ActorKnown || len(empty.Tasks) != 0 {
				t.Fatalf("zero-task snapshot=%+v err=%v", empty, err)
			}

			phases := []p.Phase{p.PhaseRequest, p.PhaseCodeReview, p.PhaseWorkerSlices}
			tasks := make([]p.TaskID, len(phases))
			starts := make([]actorOwnershipStart, len(phases))
			for i, phase := range phases {
				label := fmt.Sprintf("owned-%d", i)
				tasks[i] = createActorOwnershipTask(t, store.tracker, actor, boot, label, phase)
				starts[i] = startActorOwnershipTask(t, store.tracker, actor, actor, boot, tasks[i], p.OperationID("owned-start-"+label), []byte(fmt.Sprintf(`{"task":%d}`, i)), false)
				if i == 0 {
					partial, err := store.api.QueryActorOwnership(context.Background(), query)
					if err != nil || len(partial.Tasks) != 1 || partial.Tasks[0].TaskID != tasks[0] {
						t.Fatalf("one-task snapshot=%+v err=%v", partial, err)
					}
				}
			}
			one, err := store.api.QueryActorOwnership(context.Background(), query)
			if err != nil || len(one.Tasks) != 3 {
				t.Fatalf("three-task snapshot=%+v err=%v", one, err)
			}
			for i, row := range one.Tasks {
				if row.TaskID != tasks[i] || row.Phase != phases[i] || row.StartedJournalID != starts[i].started || row.AssignmentID != p.AssignmentID("owned-start-owned-"+fmt.Sprint(i)) || row.ProducingOperationID != p.OperationID("owned-start-owned-"+fmt.Sprint(i)) || len(row.Materials) != 1 || row.Materials[0].JournalID != starts[i].material || row.Materials[0].EventKind != actorOwnershipMaterialKind {
					t.Fatalf("owned row %d=%+v; starts=%+v tasks=%v", i, row, starts, tasks)
				}
			}
			if one.Work.TaskRows != 3 || one.Work.MaterialRows != 3 || one.Work.ResultBytes <= 0 {
				t.Fatalf("ownership work=%+v", one.Work)
			}
		})
	}
}

func TestActorOwnershipUnknownAndIdleActors(t *testing.T) {
	store := openActorOwnershipStore(t, "memory")
	actor := registerActorOwnershipActor(t, store.tracker, "actor-ownership-idle", "idle")
	actorOwnershipGenesis(t, store.tracker, actor, "idle-genesis")
	idle, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor})
	if err != nil || !idle.ActorKnown || len(idle.Tasks) != 0 {
		t.Fatalf("idle snapshot=%+v err=%v", idle, err)
	}
	unknown := p.ActorID{Namespace: "actor-ownership-idle", UUID: uuid.New()}
	missing, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: unknown})
	if err != nil || missing.ActorKnown || len(missing.Tasks) != 0 {
		t.Fatalf("unknown snapshot=%+v err=%v", missing, err)
	}
}

func TestActorOwnershipEndedEpisodeIsAbsent(t *testing.T) {
	store := openActorOwnershipStore(t, "memory")
	actor := registerActorOwnershipActor(t, store.tracker, "actor-ownership-ended", "owner")
	boot := actorOwnershipGenesis(t, store.tracker, actor, "ended-genesis")
	task := createActorOwnershipTask(t, store.tracker, actor, boot, "ended", p.PhaseWorkerSlices)
	startActorOwnershipTask(t, store.tracker, actor, actor, boot, task, "ended-start", []byte(`{"active":true}`), false)
	if _, err := store.tracker.Journal().Apply(p.OperationInput{OperationID: "ended-stop", ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("end"), Effects: []p.Effect{{Sort: p.EffectAssignmentEnd, AssignmentID: "ended-start", TaskID: task, SlotID: p.SlotOwnerResponsibility}}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor})
	if err != nil || len(snapshot.Tasks) != 0 || snapshot.Work.TaskRows != 0 {
		t.Fatalf("ended episode snapshot=%+v err=%v", snapshot, err)
	}
}

func TestActorOwnershipNewestActiveEpisodeWinsLikeTheWriter(t *testing.T) {
	store := openActorOwnershipStore(t, "memory")
	older := registerActorOwnershipActor(t, store.tracker, "actor-ownership-newest", "older")
	newer := registerActorOwnershipActor(t, store.tracker, "actor-ownership-newest", "newer")
	boot := actorOwnershipGenesis(t, store.tracker, older, "newest-genesis")
	task := createActorOwnershipTask(t, store.tracker, older, boot, "two-active", p.PhaseWorkerSlices)
	startActorOwnershipTask(t, store.tracker, older, older, boot, task, "newest-start-a", []byte(`{"episode":"a"}`), false)
	startActorOwnershipTask(t, store.tracker, older, newer, boot, task, "newest-start-b", []byte(`{"episode":"b"}`), false)

	oldSnapshot, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: older, MaterialKinds: []p.EventKind{actorOwnershipMaterialKind}})
	if err != nil || len(oldSnapshot.Tasks) != 0 {
		t.Fatalf("older active episode snapshot=%+v err=%v", oldSnapshot, err)
	}
	newSnapshot, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: newer, MaterialKinds: []p.EventKind{actorOwnershipMaterialKind}})
	if err != nil || len(newSnapshot.Tasks) != 1 || newSnapshot.Tasks[0].AssignmentID != "newest-start-b" || len(newSnapshot.Tasks[0].Materials) != 1 || newSnapshot.Tasks[0].Materials[0].JournalID == 0 {
		t.Fatalf("newest episode snapshot=%+v err=%v", newSnapshot, err)
	}
}

func TestActorOwnershipTransferSuccessorHasNoMaterialAndNamesItsPredecessor(t *testing.T) {
	store := openActorOwnershipStore(t, "memory")
	actorA := registerActorOwnershipActor(t, store.tracker, "actor-ownership-transfer", "a")
	actorB := registerActorOwnershipActor(t, store.tracker, "actor-ownership-transfer", "b")
	actorC := registerActorOwnershipActor(t, store.tracker, "actor-ownership-transfer", "c")
	boot := actorOwnershipGenesis(t, store.tracker, actorA, "transfer-genesis")
	task := createActorOwnershipTask(t, store.tracker, actorA, boot, "transfer", p.PhaseWorkerSlices)
	startA := startActorOwnershipTask(t, store.tracker, actorA, actorA, boot, task, "transfer-a", []byte(`{"episode":"a"}`), false)
	if _, err := store.tracker.As(actorA, startA.started).TransferAssignment(p.AssignmentTransferRequest{TaskID: task, SlotID: p.SlotOwnerResponsibility, PreviousAssignmentID: "transfer-a", NextAssignmentID: "transfer-b", NextOccupant: actorB}, p.WithOperationID("transfer-a-b")); err != nil {
		t.Fatal(err)
	}
	starts, err := store.tracker.Journal().(p.AssignmentStartQueryAPI).QueryAssignmentStarts(p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 16}, AssignmentIDs: []p.AssignmentID{"transfer-b"}})
	if err != nil || len(starts.Rows) != 1 {
		t.Fatalf("find B start: %+v err=%v", starts, err)
	}
	if _, err := store.tracker.As(actorB, starts.Rows[0].AuthorityJournalID).TransferAssignment(p.AssignmentTransferRequest{TaskID: task, SlotID: p.SlotOwnerResponsibility, PreviousAssignmentID: "transfer-b", NextAssignmentID: "transfer-c", NextOccupant: actorC}, p.WithOperationID("transfer-b-c")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actorC, MaterialKinds: []p.EventKind{actorOwnershipMaterialKind}})
	if err != nil || len(snapshot.Tasks) != 1 || snapshot.Tasks[0].PredecessorAssignmentID == nil || *snapshot.Tasks[0].PredecessorAssignmentID != "transfer-b" || len(snapshot.Tasks[0].Materials) != 0 || snapshot.Work.MaterialRows != 0 {
		t.Fatalf("transfer successor snapshot=%+v err=%v", snapshot, err)
	}
}

func TestActorOwnershipMaterialComesOnlyFromTheProducingOperation(t *testing.T) {
	// Every subtest owns a private in-memory tracker or fused allocator/database.
	// It joins the governed-allocation family's reviewed parallel schedule.
	t.Parallel()
	t.Run("composed-supplement", func(t *testing.T) {
		fused, _ := openFusedAllocatorWithDatabase(t, "ownership-composed-material")
		if err := fused.Launch(); err != nil {
			t.Fatal(err)
		}
		actor := registerGovernedActor(t, fused.Tracker(), "ownership-composed-material")
		root := initializeFusedRoot(t, fused, actor, "ownership-composed-material")
		request := composedGovernedRequest("ownership-composed-material", actor, root, 1)
		closure, err := fused.RunAllocateComposed(context.Background(), "ownership-composed-material-flow", root.AssignmentRow.JournalID, request)
		if err != nil {
			t.Fatal(err)
		}
		child := closure.Closure().Children()[0]
		api := fused.Tracker().Journal().(p.ActorOwnershipQueryAPI)
		snapshot, err := api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor, MaterialKinds: []p.EventKind{"provenance.slice.created"}, EvidenceKinds: []p.EvidenceKind{"provenance.assignment.command"}})
		if err != nil {
			t.Fatal(err)
		}
		var owned *p.OwnedTaskRow
		for i := range snapshot.Tasks {
			if snapshot.Tasks[i].TaskID == child.TaskID {
				owned = &snapshot.Tasks[i]
				break
			}
		}
		if owned == nil || len(owned.Materials) != 1 || len(owned.Evidence) != 1 {
			t.Fatalf("composed material/evidence=%+v err=%v child=%+v", snapshot, err, child)
		}
	})

	t.Run("non-composed-start", func(t *testing.T) {
		store := openActorOwnershipStore(t, "memory")
		actor := registerActorOwnershipActor(t, store.tracker, "ownership-normal-material", "owner")
		boot := actorOwnershipGenesis(t, store.tracker, actor, "normal-material-genesis")
		task := createActorOwnershipTask(t, store.tracker, actor, boot, "normal", p.PhaseWorkerSlices)
		start := startActorOwnershipTask(t, store.tracker, actor, actor, boot, task, "normal-start", []byte(`{"normal":true}`), true)
		snapshot, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor, MaterialKinds: []p.EventKind{actorOwnershipMaterialKind}, EvidenceKinds: []p.EvidenceKind{actorOwnershipEvidenceKind}})
		if err != nil || len(snapshot.Tasks) != 1 || len(snapshot.Tasks[0].Materials) != 1 || snapshot.Tasks[0].Materials[0].JournalID != start.material || len(snapshot.Tasks[0].Evidence) != 1 || snapshot.Tasks[0].Evidence[0].JournalID != start.evidence {
			t.Fatalf("normal material/evidence=%+v err=%v", snapshot, err)
		}
	})

	t.Run("unrelated-producer", func(t *testing.T) {
		store := openActorOwnershipStore(t, "memory")
		actor := registerActorOwnershipActor(t, store.tracker, "ownership-negative-material", "owner")
		boot := actorOwnershipGenesis(t, store.tracker, actor, "negative-material-genesis")
		task := createActorOwnershipTask(t, store.tracker, actor, boot, "negative", p.PhaseWorkerSlices)
		startActorOwnershipTask(t, store.tracker, actor, actor, boot, task, "negative-start", []byte(`{"wanted":true}`), true)
		if _, err := store.tracker.Journal().Apply(p.OperationInput{OperationID: "negative-unrelated", ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("negative"), Effects: []p.Effect{
			{Sort: p.EffectTaskEvent, TaskID: task, EventKind: actorOwnershipMaterialKind, Payload: []byte(`{"unrelated":true}`)},
			{Sort: p.EffectEvidence, TaskID: task, EvidenceKind: actorOwnershipEvidenceKind, ContentDigest: []byte("unrelated"), Payload: []byte(`{"unrelated":true}`)},
		}}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor, MaterialKinds: []p.EventKind{actorOwnershipMaterialKind}, EvidenceKinds: []p.EvidenceKind{actorOwnershipEvidenceKind}})
		if err != nil || snapshot.Work.MaterialRows != 1 || snapshot.Work.EvidenceRows != 1 || bytes.Contains(snapshot.Tasks[0].Materials[0].Payload, []byte("unrelated")) || bytes.Contains(snapshot.Tasks[0].Evidence[0].Payload, []byte("unrelated")) {
			t.Fatalf("unrelated producer leaked: %+v err=%v", snapshot, err)
		}
	})

	t.Run("prior-transfers", func(t *testing.T) {
		store := openActorOwnershipStore(t, "memory")
		actor := registerActorOwnershipActor(t, store.tracker, "ownership-history-material", "owner")
		boot := actorOwnershipGenesis(t, store.tracker, actor, "history-material-genesis")
		task := createActorOwnershipTask(t, store.tracker, actor, boot, "history", p.PhaseWorkerSlices)
		previous := p.AssignmentID("history-0")
		startActorOwnershipTask(t, store.tracker, actor, actor, boot, task, p.OperationID(previous), []byte(`{"history":0}`), true)
		for i := 1; i <= 3; i++ {
			next := p.AssignmentID(fmt.Sprintf("history-%d", i))
			if _, err := store.tracker.Journal().Apply(p.OperationInput{OperationID: p.OperationID(next), ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte(next), Effects: []p.Effect{
				{Sort: p.EffectAssignmentEnd, AssignmentID: previous, TaskID: task, SlotID: p.SlotOwnerResponsibility},
				{Sort: p.EffectAssignmentStart, AssignmentID: next, TaskID: task, SlotID: p.SlotOwnerResponsibility, Occupant: actor, Predecessor: previous},
				{Sort: p.EffectTaskEvent, TaskID: task, EventKind: actorOwnershipMaterialKind, Payload: []byte(fmt.Sprintf(`{"history":%d}`, i))},
				{Sort: p.EffectEvidence, TaskID: task, EvidenceKind: actorOwnershipEvidenceKind, ContentDigest: []byte(next), Payload: []byte(fmt.Sprintf(`{"history":%d}`, i))},
			}}); err != nil {
				t.Fatal(err)
			}
			previous = next
		}
		snapshot, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor, MaterialKinds: []p.EventKind{actorOwnershipMaterialKind}, EvidenceKinds: []p.EvidenceKind{actorOwnershipEvidenceKind}})
		if err != nil || len(snapshot.Tasks) != 1 || snapshot.Work.MaterialRows != 1 || snapshot.Work.EvidenceRows != 1 {
			t.Fatalf("prior transfer history snapshot=%+v err=%v", snapshot, err)
		}
	})
}

func TestActorOwnershipEvidenceComesOnlyFromTheProducingOperation(t *testing.T) {
	store := openActorOwnershipStore(t, "memory")
	actor := registerActorOwnershipActor(t, store.tracker, "ownership-shared-evidence", "owner")
	boot := actorOwnershipGenesis(t, store.tracker, actor, "shared-evidence-genesis")
	task1 := createActorOwnershipTask(t, store.tracker, actor, boot, "shared-1", p.PhaseRequest)
	task2 := createActorOwnershipTask(t, store.tracker, actor, boot, "shared-2", p.PhaseReview)
	if _, err := store.tracker.Journal().Apply(p.OperationInput{OperationID: "shared-evidence-start", ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("shared"), Effects: []p.Effect{
		{Sort: p.EffectAssignmentStart, AssignmentID: "shared-a", TaskID: task1, SlotID: p.SlotOwnerResponsibility, Occupant: actor},
		{Sort: p.EffectAssignmentStart, AssignmentID: "shared-b", TaskID: task2, SlotID: p.SlotOwnerResponsibility, Occupant: actor},
		{Sort: p.EffectEvidence, EvidenceKind: actorOwnershipEvidenceKind, ContentDigest: []byte("shared"), Payload: []byte(`{"shared":true}`)},
	}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor, EvidenceKinds: []p.EvidenceKind{actorOwnershipEvidenceKind}})
	if err != nil || len(snapshot.Tasks) != 2 || len(snapshot.Tasks[0].Evidence) != 1 || len(snapshot.Tasks[1].Evidence) != 1 || snapshot.Tasks[0].Evidence[0].JournalID != snapshot.Tasks[1].Evidence[0].JournalID || snapshot.Work.EvidenceRows != 2 {
		t.Fatalf("shared evidence snapshot=%+v err=%v", snapshot, err)
	}
}

func TestActorOwnershipSelectivityIgnoresOtherActorsAndHistory(t *testing.T) {
	db, _ := openFileDB(t)
	tracker, err := p.OpenBorrowedSQLite(db, p.WithModelRegistry(p.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tracker.Close() })
	api := tracker.Journal().(p.ActorOwnershipQueryAPI)
	actor := registerActorOwnershipActor(t, tracker, "ownership-selectivity", "owner")
	other := registerActorOwnershipActor(t, tracker, "ownership-selectivity", "other")
	boot := actorOwnershipGenesis(t, tracker, actor, "selectivity-genesis")
	owned := createActorOwnershipTask(t, tracker, actor, boot, "selectivity-owned", p.PhaseWorkerSlices)
	startActorOwnershipTask(t, tracker, actor, actor, boot, owned, "selectivity-owned-start", []byte(`{"owned":true}`), false)
	foreignTasks := bulkActorOwnershipTasks(t, tracker, actor, other, boot, "foreign", 200)
	seedActorOwnershipHistory(t, db, "selectivity-genesis", foreignTasks[0], 5000)
	snapshot, err := api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor})
	if err != nil || len(snapshot.Tasks) != 1 || snapshot.Tasks[0].TaskID != owned || snapshot.Work.TaskRows != 1 {
		t.Fatalf("selectivity snapshot=%+v err=%v", snapshot, err)
	}
}

func seedActorOwnershipHistory(t *testing.T, db *sql.DB, operation p.OperationID, task p.TaskID, count int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var producer int64
	if err := tx.QueryRow("SELECT journal_id FROM journal_operations WHERE operation_id=?", operation).Scan(&producer); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := tx.QueryRow("SELECT COALESCE(MAX(journal_id),0) FROM journal").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`WITH RECURSIVE seq(value) AS (
		VALUES(1) UNION ALL SELECT value+1 FROM seq WHERE value<?
	) INSERT INTO journal(kind_id,actor_id,recorded_at,produced_by_operation_journal_id)
	SELECT 1,NULL,value,? FROM seq`, count, producer); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := tx.QueryRow("SELECT COALESCE(MAX(journal_id),0) FROM journal").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO journal_task_events(journal_id,task_id,event_kind,payload) SELECT journal_id,?,'pasture.unrelated.event','{\"unrelated\":true}' FROM journal WHERE journal_id>? AND journal_id<=?", task.String(), before, after); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestActorOwnershipFiveHundredTasks(t *testing.T) {
	store := openActorOwnershipStore(t, "memory")
	actor := registerActorOwnershipActor(t, store.tracker, "ownership-500", "owner")
	boot := actorOwnershipGenesis(t, store.tracker, actor, "five-hundred-genesis")
	tasks := bulkActorOwnershipTasks(t, store.tracker, actor, actor, boot, "owned-500", 500)
	snapshot, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor})
	if err != nil || len(snapshot.Tasks) != 500 || snapshot.Work.TaskRows != 500 {
		t.Fatalf("500-task snapshot count=%d work=%+v err=%v", len(snapshot.Tasks), snapshot.Work, err)
	}
	for i, task := range tasks {
		if snapshot.Tasks[i].TaskID != task {
			t.Fatalf("task order[%d]=%s, want %s", i, snapshot.Tasks[i].TaskID, task)
		}
	}
}

func bulkActorOwnershipTasks(t *testing.T, tracker p.Tracker, committer, occupant p.ActorID, boot p.JournalID, prefix string, count int) []p.TaskID {
	t.Helper()
	tasks := make([]p.TaskID, 0, count)
	const batchSize = 64
	for start := 0; start < count; start += batchSize {
		end := start + batchSize
		if end > count {
			end = count
		}
		effects := make([]p.Effect, 0, (end-start)*2)
		batchTasks := make([]p.TaskID, 0, end-start)
		for i := start; i < end; i++ {
			label := fmt.Sprintf("%s-%04d", prefix, i)
			task := p.TaskID{Namespace: "actor-ownership", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(label))}
			batchTasks = append(batchTasks, task)
			effects = append(effects,
				p.Effect{Sort: p.EffectTaskCreate, TaskID: task, Title: label, Type: p.TaskTypeTask, Priority: p.PriorityMedium, Phase: p.PhaseWorkerSlices},
				p.Effect{Sort: p.EffectAssignmentStart, AssignmentID: p.AssignmentID(label + "-assignment"), TaskID: task, SlotID: p.SlotOwnerResponsibility, Occupant: occupant},
			)
		}
		if _, err := tracker.Journal().Apply(p.OperationInput{OperationID: p.OperationID(fmt.Sprintf("%s-batch-%d", prefix, start)), ActorID: committer, AuthorityJournalID: &boot, CommandDigest: []byte(prefix), Effects: effects}); err != nil {
			t.Fatalf("bulk ownership tasks %s[%d:%d]: %v", prefix, start, end, err)
		}
		tasks = append(tasks, batchTasks...)
	}
	return tasks
}

func TestActorOwnershipValidationBeforeLease(t *testing.T) {
	store := openActorOwnershipStore(t, "memory")
	actor := registerActorOwnershipActor(t, store.tracker, "ownership-validation", "owner")
	materials := []p.EventKind{actorOwnershipMaterialKind, actorOwnershipMaterialKind}
	evidence := []p.EvidenceKind{actorOwnershipEvidenceKind, actorOwnershipEvidenceKind}
	originalMaterials := append([]p.EventKind(nil), materials...)
	originalEvidence := append([]p.EvidenceKind(nil), evidence...)
	query := p.ActorOwnershipQuery{Actor: actor, MaterialKinds: materials, EvidenceKinds: evidence}
	snapshot, err := store.api.QueryActorOwnership(context.Background(), query)
	if err != nil || !reflect.DeepEqual(materials, originalMaterials) || !reflect.DeepEqual(evidence, originalEvidence) {
		t.Fatalf("valid duplicate query snapshot=%+v err=%v materials=%v evidence=%v", snapshot, err, materials, evidence)
	}
	for _, test := range []struct {
		name string
		ctx  context.Context
		q    p.ActorOwnershipQuery
	}{
		{name: "nil-context", q: query},
		{name: "malformed-actor", ctx: context.Background(), q: p.ActorOwnershipQuery{Actor: p.ActorID{}}},
		{name: "empty-material-kind", ctx: context.Background(), q: p.ActorOwnershipQuery{Actor: actor, MaterialKinds: []p.EventKind{""}}},
		{name: "empty-evidence-kind", ctx: context.Background(), q: p.ActorOwnershipQuery{Actor: actor, EvidenceKinds: []p.EvidenceKind{""}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.api.QueryActorOwnership(test.ctx, test.q)
			if !errors.Is(err, p.ErrInvalidQuery) || !strings.Contains(err.Error(), "fix:") {
				t.Fatalf("validation error=%v", err)
			}
		})
	}
	if err := store.tracker.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: p.ActorID{}})
	if !errors.Is(err, p.ErrInvalidQuery) {
		t.Fatalf("malformed query reached closed-store lease: %v", err)
	}
}

func TestActorOwnershipEmptyJournal(t *testing.T) {
	store := openActorOwnershipStore(t, "memory")
	actor := p.ActorID{Namespace: "ownership-empty", UUID: uuid.New()}
	snapshot, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor})
	if err != nil || snapshot.Through != 0 || snapshot.ActorKnown || len(snapshot.Tasks) != 0 {
		t.Fatalf("empty journal snapshot=%+v err=%v", snapshot, err)
	}
}

func TestActorOwnershipBorrowedStoreAfterRootClose(t *testing.T) {
	db, _ := openFileDB(t)
	tracker, err := p.OpenBorrowedSQLite(db, p.WithModelRegistry(p.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	actor := registerActorOwnershipActor(t, tracker, "ownership-borrowed-close", "owner")
	query := p.ActorOwnershipQuery{Actor: actor}
	api := tracker.Journal().(p.ActorOwnershipQueryAPI)
	if _, err := api.QueryActorOwnership(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = api.QueryActorOwnership(context.Background(), query)
	var unavailable *p.StoreUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "QueryActorOwnership") || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("closed borrowed query error=%T %v", err, err)
	}
}

func TestActorOwnershipRefusesOneOversizedValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.db")
	tracker, err := p.OpenSQLite(path, p.WithModelRegistry(p.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tracker.Close()
	actor := registerActorOwnershipActor(t, tracker, "ownership-oversized", "owner")
	boot := actorOwnershipGenesis(t, tracker, actor, "oversized-genesis")
	task := createActorOwnershipTask(t, tracker, actor, boot, "oversized", p.PhaseWorkerSlices)
	start := startActorOwnershipTask(t, tracker, actor, actor, boot, task, "oversized-start", []byte(`{"small":true}`), false)
	oversized := make([]byte, p.MaxActorOwnershipResultBytes+1)
	oversized[0] = '"'
	for i := 1; i < len(oversized)-1; i++ {
		oversized[i] = 'x'
	}
	oversized[len(oversized)-1] = '"'
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE journal_task_events SET payload=CAST(? AS TEXT) WHERE journal_id=?", string(oversized), start.material); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	api := tracker.Journal().(p.ActorOwnershipQueryAPI)
	snapshot, err := api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor, MaterialKinds: []p.EventKind{actorOwnershipMaterialKind}})
	var limitErr *p.ActorOwnershipLimitError
	if !errors.As(err, &limitErr) || limitErr.Stage != p.ActorOwnershipStageMaterials || limitErr.Work.MaterialRows != 0 || limitErr.ObservedBytes != limitErr.Work.ResultBytes+int64(len(oversized)+len(actorOwnershipMaterialKind)) || !reflect.DeepEqual(snapshot, p.ActorOwnershipSnapshot{}) {
		t.Fatalf("oversized snapshot=%+v error=%+v", snapshot, limitErr)
	}
}

func TestActorOwnershipHydratesValuesExactly(t *testing.T) {
	store := openActorOwnershipStore(t, "memory")
	actor := registerActorOwnershipActor(t, store.tracker, "ownership-bytes", "owner")
	boot := actorOwnershipGenesis(t, store.tracker, actor, "hydrate-genesis")
	payload := json.RawMessage(`{"text":"a\u0000b"}`)
	first := createActorOwnershipTask(t, store.tracker, actor, boot, "hydrate-1", p.PhaseWorkerSlices)
	startActorOwnershipTask(t, store.tracker, actor, actor, boot, first, "hydrate-a", payload, true)
	second := createActorOwnershipTask(t, store.tracker, actor, boot, "hydrate-2", p.PhaseWorkerSlices)
	startActorOwnershipTask(t, store.tracker, actor, actor, boot, second, "hydrate-initial", []byte(`{"initial":true}`), false)
	if _, err := store.tracker.Journal().Apply(p.OperationInput{OperationID: "hydrate-b", ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("hydrate"), Effects: []p.Effect{
		{Sort: p.EffectAssignmentEnd, AssignmentID: "hydrate-initial", TaskID: second, SlotID: p.SlotOwnerResponsibility},
		{Sort: p.EffectAssignmentStart, AssignmentID: "hydrate-b", TaskID: second, SlotID: p.SlotOwnerResponsibility, Occupant: actor, Predecessor: "hydrate-initial"},
	}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.api.QueryActorOwnership(context.Background(), p.ActorOwnershipQuery{Actor: actor, MaterialKinds: []p.EventKind{actorOwnershipMaterialKind}, EvidenceKinds: []p.EvidenceKind{actorOwnershipEvidenceKind}})
	if err != nil || len(snapshot.Tasks) != 2 || snapshot.Tasks[0].PredecessorAssignmentID != nil || snapshot.Tasks[1].PredecessorAssignmentID == nil || *snapshot.Tasks[1].PredecessorAssignmentID != "hydrate-initial" || !bytes.Equal(snapshot.Tasks[0].Materials[0].Payload, payload) {
		t.Fatalf("exact hydration snapshot=%+v err=%v want payload=%q", snapshot, err, payload)
	}
}

func TestActorOwnershipTypedErrorText(t *testing.T) {
	actor := p.ActorID{Namespace: "errors", UUID: uuid.New()}
	task := p.TaskID{Namespace: "errors", UUID: uuid.New()}
	limit := &p.ActorOwnershipLimitError{Actor: actor, Stage: p.ActorOwnershipStageMaterials, LimitBytes: 100, ObservedBytes: 101, Work: p.ActorOwnershipWork{ResultBytes: 1}}
	wantLimit := fmt.Sprintf("provenance: the ownership read for actor %s stopped at stage materials after 101 bytes, above its bound of 100 bytes. Why: a correct result is a few hundred bytes per owned task, so the store holds a value that no supported writer produces. Where: QueryActorOwnership, stage materials. When: inside the read transaction, before the row was added to the result. Impact: no result was returned; nothing was written. Fix: run Journal.VerifyIntegrity and Journal.ReplayProjections on this store to find the damaged rows; both only read; then restore the store from a known-good copy.", actor)
	if limit.Error() != wantLimit || !errors.Is(limit, p.ErrActorOwnershipLimit) {
		t.Fatalf("limit error=%q, want %q", limit, wantLimit)
	}
	mismatch := &p.OwnerProjectionMismatchError{Task: task, Owner: actor}
	wantMismatch := fmt.Sprintf("provenance: tasks.owner_id for task %s names actor %s, but the newest active owner-slot episode is held by nobody. Why: the owner column is written in the same transaction as the episode rows, so they must agree. Where: QueryActorOwnership, stage owned-tasks. When: inside the read transaction. Impact: no result was returned; nothing was written. Fix: run Journal.ReplayProjections and Journal.VerifyIntegrity to confirm the diverging field (both only read); then restore the store from a known-good copy.", task, actor)
	if mismatch.Error() != wantMismatch || !errors.Is(mismatch, p.ErrProjectionDivergence) {
		t.Fatalf("mismatch error=%q, want %q", mismatch, wantMismatch)
	}
	integrity := &p.ActorOwnershipIntegrityError{Stage: p.ActorOwnershipStageEvidence, Problem: fmt.Sprintf("evidence row %d names non-owned task %s", 42, task), Fix: "restore the task, owner episode, and evidence producer rows from the same committed backup"}
	wantIntegrity := fmt.Sprintf("provenance: journal subtype integrity violated: evidence row 42 names non-owned task %s — why: the stored row cannot be decoded as a supported ownership fact; where: QueryActorOwnership result-row validation, stage evidence; when: inside the read transaction; impact: no result returned and nothing was written; fix: restore the task, owner episode, and evidence producer rows from the same committed backup", task)
	if integrity.Error() != wantIntegrity || !errors.Is(integrity, p.ErrSubtypeIntegrity) {
		t.Fatalf("integrity error=%q, want %q", integrity, wantIntegrity)
	}
}
