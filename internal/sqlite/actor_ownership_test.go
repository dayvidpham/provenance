package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

const (
	actorOwnershipMaterialKind = journal.EventKind("pasture.ownership.material")
	actorOwnershipEvidenceKind = journal.EvidenceKind("pasture.ownership.evidence")
)

type actorOwnershipFixture struct {
	actor        journal.ActorID
	other        journal.ActorID
	unregistered journal.ActorID
	boot         journal.JournalID
	task         journal.TaskID
	started      journal.JournalID
}

func openActorOwnershipDB(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newActorOwnershipFixture(t *testing.T, db *DB) actorOwnershipFixture {
	t.Helper()
	actor := insertActorOwnershipActor(t, db, "actor")
	other := insertActorOwnershipActor(t, db, "other")
	unregistered := journal.ActorID{Namespace: "actor-ownership", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("actor/unregistered"))}
	boot := genesisBoot(t, db, actor)
	task := journal.TaskID{Namespace: "actor-ownership", UUID: uuid.New()}
	if _, err := db.Apply(journal.OperationInput{
		OperationID: "actor-ownership-create-" + journal.OperationID(task.UUID.String()),
		ActorID:     actor, AuthorityJournalID: &boot, CommandDigest: []byte("create"),
		Effects: []journal.Effect{{Sort: journal.EffectTaskCreate, TaskID: task, Title: "ownership", Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseWorkerSlices}},
	}); err != nil {
		t.Fatalf("create ownership task: %v", err)
	}
	started := startActorOwnershipTask(t, db, actor, boot, task, "ownership-start", actorOwnershipMaterialKind, json.RawMessage(`{"value":"start"}`), true)
	return actorOwnershipFixture{actor: actor, other: other, unregistered: unregistered, boot: boot, task: task, started: started}
}

func insertActorOwnershipActor(t *testing.T, db *DB, name string) journal.ActorID {
	t.Helper()
	actor, parseErr := ptypes.ParseActorID(name)
	if parseErr != nil {
		actor = journal.ActorID{Namespace: "actor-ownership", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("actor/"+name))}
	}
	scope := takePoolScope(t, db)
	_, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO agents (id,kind_id) VALUES (?1,?2)", actor.String(), int(ptypes.AgentKindSoftware))
	if err == nil {
		_, err = scope.conn.ExecContext(scope.ctx, "INSERT INTO agents_software (agent_id,name,version,source) VALUES (?1,?2,?3,?4)", actor.String(), name, "1", "test")
	}
	scope.release()
	if err != nil {
		t.Fatalf("insert ownership actor %s: %v", name, err)
	}
	return actor
}

func createActorOwnershipTask(t *testing.T, db *DB, actor journal.ActorID, boot journal.JournalID, label string, phase ptypes.Phase) journal.TaskID {
	t.Helper()
	task := journal.TaskID{Namespace: "actor-ownership", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("task/"+label))}
	if _, err := db.Apply(journal.OperationInput{
		OperationID: "create-" + journal.OperationID(label), ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte(label),
		Effects: []journal.Effect{{Sort: journal.EffectTaskCreate, TaskID: task, Title: label, Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: phase}},
	}); err != nil {
		t.Fatalf("create task %s: %v", label, err)
	}
	return task
}

func startActorOwnershipTask(t *testing.T, db *DB, actor journal.ActorID, boot journal.JournalID, task journal.TaskID, operation string, kind journal.EventKind, payload json.RawMessage, evidence bool) journal.JournalID {
	t.Helper()
	bootID := boot
	effects := []journal.Effect{
		{Sort: journal.EffectAssignmentStart, ResultSlot: "start", AssignmentID: journal.AssignmentID(operation), TaskID: task, SlotID: journal.SlotOwnerResponsibility, Occupant: actor},
		{Sort: journal.EffectTaskEvent, ResultSlot: "material", TaskID: task, EventKind: kind, Payload: payload},
	}
	if evidence {
		effects = append(effects, journal.Effect{Sort: journal.EffectEvidence, ResultSlot: "evidence", TaskID: task, EvidenceKind: actorOwnershipEvidenceKind, ContentDigest: []byte("digest-" + operation), Payload: json.RawMessage(`{"evidence":"start"}`)})
	}
	result, err := db.Apply(journal.OperationInput{OperationID: journal.OperationID(operation), ActorID: actor, AuthorityJournalID: &bootID, CommandDigest: []byte(operation), Effects: effects})
	if err != nil {
		t.Fatalf("start task %s: %v", task, err)
	}
	for _, binding := range result.ResultSlots {
		if binding.Slot == "start" {
			return binding.ProducedJournalID
		}
	}
	t.Fatalf("start operation %s produced no start slot", operation)
	return 0
}

func actorOwnershipAPI(t *testing.T, db *DB) journal.ActorOwnershipQueryAPI {
	t.Helper()
	api, ok := any(db).(journal.ActorOwnershipQueryAPI)
	if !ok {
		t.Fatal("DB lacks ActorOwnershipQueryAPI")
	}
	return api
}

func waitActorOwnershipSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func waitActorOwnershipResult(t *testing.T, result <-chan actorOwnershipResult) actorOwnershipResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ownership query")
		return actorOwnershipResult{}
	}
}

type actorOwnershipResult struct {
	snapshot journal.ActorOwnershipSnapshot
	err      error
}

func TestActorOwnershipIsOneTransaction(t *testing.T) {
	for _, test := range []struct {
		name  string
		stage int
		check func(*testing.T, actorOwnershipFixture, *DB, journal.ActorOwnershipSnapshot)
	}{
		{
			name:  "after-through",
			stage: 1,
			check: func(t *testing.T, fixture actorOwnershipFixture, writer *DB, snapshot journal.ActorOwnershipSnapshot) {
				t.Helper()
				if snapshot.Through <= 0 {
					t.Fatalf("Through=%d, want positive pre-writer boundary", snapshot.Through)
				}
				for _, task := range snapshot.Tasks {
					if task.StartedJournalID > snapshot.Through {
						t.Fatalf("task %s start %d escaped Through=%d", task.TaskID, task.StartedJournalID, snapshot.Through)
					}
				}
				if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].TaskID != fixture.task {
					t.Fatalf("snapshot tasks=%+v, want only pre-writer task", snapshot.Tasks)
				}
				fresh, err := actorOwnershipAPI(t, writer).QueryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: fixture.actor})
				if err != nil || len(fresh.Tasks) != 2 {
					t.Fatalf("fresh writer read=%+v err=%v, want both tasks", fresh, err)
				}
			},
		},
		{
			name:  "after-actor-known",
			stage: 2,
			check: func(t *testing.T, _ actorOwnershipFixture, _ *DB, snapshot journal.ActorOwnershipSnapshot) {
				t.Helper()
				if snapshot.ActorKnown || len(snapshot.Tasks) != 0 {
					t.Fatalf("late registration/start leaked into one snapshot: known=%t tasks=%+v", snapshot.ActorKnown, snapshot.Tasks)
				}
			},
		},
		{
			name:  "after-owned-tasks",
			stage: 3,
			check: func(t *testing.T, fixture actorOwnershipFixture, writer *DB, snapshot journal.ActorOwnershipSnapshot) {
				t.Helper()
				if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].TaskID != fixture.task || snapshot.Tasks[0].Phase != ptypes.PhaseWorkerSlices || len(snapshot.Tasks[0].Materials) != 1 {
					t.Fatalf("held snapshot=%+v, want pre-commit phase and material", snapshot)
				}
				fresh, err := actorOwnershipAPI(t, writer).QueryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: fixture.actor})
				if err != nil || len(fresh.Tasks) != 0 {
					t.Fatalf("post-writer read=%+v err=%v, want ended task absent", fresh, err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "one-transaction.db")
			reader := openActorOwnershipDB(t, path)
			writer := openActorOwnershipDB(t, path)
			fixture := newActorOwnershipFixture(t, reader)

			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			reader.installActorOwnershipStageBarrier(func(stage int) {
				if stage != test.stage {
					return
				}
				once.Do(func() { close(entered) })
				<-release
			})
			result := make(chan actorOwnershipResult, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			target := fixture.actor
			if test.stage == 2 {
				target = fixture.unregistered
			}
			go func() {
				snapshot, err := actorOwnershipAPI(t, reader).QueryActorOwnership(ctx, journal.ActorOwnershipQuery{Actor: target, MaterialKinds: []journal.EventKind{actorOwnershipMaterialKind}})
				result <- actorOwnershipResult{snapshot: snapshot, err: err}
			}()
			waitActorOwnershipSignal(t, entered, "read barrier")

			if test.stage == 1 {
				task := createActorOwnershipTask(t, writer, fixture.actor, fixture.boot, "late-task", ptypes.PhaseCodeReview)
				startActorOwnershipTask(t, writer, fixture.actor, fixture.boot, task, "late-start", actorOwnershipMaterialKind, json.RawMessage(`{"late":true}`), false)
			} else if test.stage == 2 {
				insertActorOwnershipActor(t, writer, target.String())
				task := createActorOwnershipTask(t, writer, target, fixture.boot, "late-registered-task", ptypes.PhaseCodeReview)
				startActorOwnershipTask(t, writer, target, fixture.boot, task, "late-registered-start", actorOwnershipMaterialKind, json.RawMessage(`{"late":true}`), false)
			} else {
				nextPhase := ptypes.PhaseCodeReview
				if _, err := writer.Apply(journal.OperationInput{OperationID: "post-snapshot-change", ActorID: fixture.actor, AuthorityJournalID: &fixture.boot, CommandDigest: []byte("post"), Effects: []journal.Effect{
					{Sort: journal.EffectAssignmentEnd, AssignmentID: "ownership-start", TaskID: fixture.task, SlotID: journal.SlotOwnerResponsibility},
					{Sort: journal.EffectTaskEvent, TaskID: fixture.task, EventKind: journal.EventKindTaskUpdated, UpdatePhase: &nextPhase},
				}}); err != nil {
					t.Fatalf("post-snapshot writer: %v", err)
				}
			}
			close(release)
			got := waitActorOwnershipResult(t, result)
			if got.err != nil {
				t.Fatalf("held query: %v", got.err)
			}
			test.check(t, fixture, writer, got.snapshot)
		})
	}
}

func TestActorOwnershipCancellationAfterBeginRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancel.db")
	db := openActorOwnershipDB(t, path)
	fixture := newActorOwnershipFixture(t, db)
	db.db.SetMaxOpenConns(1)
	entered := make(chan struct{})
	release := make(chan struct{})
	db.installActorOwnershipStageBarrier(func(stage int) {
		if stage == 1 {
			close(entered)
			<-release
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := db.queryActorOwnership(ctx, journal.ActorOwnershipQuery{Actor: fixture.actor}, journal.MaxActorOwnershipResultBytes)
		result <- err
	}()
	waitActorOwnershipSignal(t, entered, "cancel barrier")
	cancel()
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled query error=%v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled query did not return")
	}
	conn, err := db.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("pool-of-one retained a lock after cancellation: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

func TestActorOwnershipByteGuardStopsAtTheCrossingRow(t *testing.T) {
	const limit = int64(4 << 10)
	for _, test := range []struct {
		name     string
		payloads []int
		wantRows int
	}{
		{name: "crossing-third", payloads: []int{1500, 1500, 1000, 3000}, wantRows: 2},
		{name: "single-value-over-limit", payloads: []int{5000}, wantRows: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "byte-guard.db")
			db := openActorOwnershipDB(t, path)
			actor := insertActorOwnershipActor(t, db, "guard-actor")
			boot := genesisBoot(t, db, actor)
			for i, size := range test.payloads {
				label := fmt.Sprintf("guard-%d", i)
				task := createActorOwnershipTask(t, db, actor, boot, label, ptypes.PhaseWorkerSlices)
				payload := actorOwnershipJSONSize(t, size)
				startActorOwnershipTask(t, db, actor, boot, task, label, actorOwnershipMaterialKind, payload, false)
			}
			snapshot, err := db.queryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: actor, MaterialKinds: []journal.EventKind{actorOwnershipMaterialKind}}, limit)
			if !errors.Is(err, journal.ErrActorOwnershipLimit) {
				t.Fatalf("query error=%v, want actor ownership limit", err)
			}
			var limitErr *journal.ActorOwnershipLimitError
			if !errors.As(err, &limitErr) {
				t.Fatalf("query error=%T, want *ActorOwnershipLimitError", err)
			}
			if limitErr.Stage != journal.ActorOwnershipStageMaterials || limitErr.LimitBytes != limit || limitErr.Work.MaterialRows != test.wantRows || !reflect.DeepEqual(snapshot, journal.ActorOwnershipSnapshot{}) {
				t.Fatalf("limit result snapshot=%+v error=%+v", snapshot, limitErr)
			}
			var crossing int64
			if test.wantRows == 2 {
				crossing = int64(len(actorOwnershipJSONSize(t, test.payloads[2])))
			} else {
				crossing = int64(len(actorOwnershipJSONSize(t, test.payloads[0])))
			}
			crossing += int64(len(actorOwnershipMaterialKind))
			if limitErr.ObservedBytes != limitErr.Work.ResultBytes+crossing {
				t.Fatalf("ObservedBytes=%d, want pre-crossing Work.ResultBytes=%d plus first crossing value=%d", limitErr.ObservedBytes, limitErr.Work.ResultBytes, crossing)
			}
			if test.wantRows == 2 && limitErr.ObservedBytes >= limitErr.Work.ResultBytes+crossing+int64(len(actorOwnershipJSONSize(t, test.payloads[3]))+len(actorOwnershipMaterialKind)) {
				t.Fatalf("later row %d contributed to first-crossing ObservedBytes=%d", test.payloads[3], limitErr.ObservedBytes)
			}
		})
	}
}

func actorOwnershipJSONSize(t *testing.T, size int) json.RawMessage {
	t.Helper()
	if size < 2 {
		t.Fatalf("payload size %d is too small for JSON", size)
	}
	payload := make([]byte, size)
	payload[0] = '"'
	for i := 1; i < len(payload)-1; i++ {
		payload[i] = 'x'
	}
	payload[len(payload)-1] = '"'
	if !json.Valid(payload) {
		t.Fatalf("constructed payload is not valid JSON")
	}
	return payload
}

func TestActorOwnershipSkipsEmptyKindStatements(t *testing.T) {
	db := openActorOwnershipDB(t, ":memory:")
	fixture := newActorOwnershipFixture(t, db)
	var stages []int
	db.installActorOwnershipStageBarrier(func(stage int) { stages = append(stages, stage) })
	snapshot, err := db.queryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: fixture.actor}, journal.MaxActorOwnershipResultBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stages, []int{1, 2, 3}) {
		t.Fatalf("barrier stages=%v, want only statements 1..3", stages)
	}
	if snapshot.Work.TaskRows != 1 || snapshot.Work.MaterialRows != 0 || snapshot.Work.EvidenceRows != 0 {
		t.Fatalf("empty-kind work=%+v", snapshot.Work)
	}
	source, err := os.ReadFile("actor_ownership.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if !strings.Contains(text, "if len(query.MaterialKinds) != 0 {\n\t\t\tif err := db.readActorOwnershipMaterials") || !strings.Contains(text, "if len(query.EvidenceKinds) != 0 {\n\t\t\tif err := db.readActorOwnershipEvidence") {
		t.Fatal("empty kind lists are not source-pinned to skip statement 4 and statement 5")
	}
}

func TestActorOwnershipProjectionMismatchIsTyped(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func(*testing.T, *DB, actorOwnershipFixture)
		occupant func(actorOwnershipFixture) *journal.ActorID
	}{
		{
			name: "no-active-episode",
			mutate: func(t *testing.T, db *DB, fixture actorOwnershipFixture) {
				t.Helper()
				if _, err := db.Apply(journal.OperationInput{OperationID: "end-for-mismatch", ActorID: fixture.actor, AuthorityJournalID: &fixture.boot, CommandDigest: []byte("end"), Effects: []journal.Effect{{Sort: journal.EffectAssignmentEnd, AssignmentID: "ownership-start", TaskID: fixture.task, SlotID: journal.SlotOwnerResponsibility}}}); err != nil {
					t.Fatal(err)
				}
				execActorOwnershipTestSQL(t, db, "UPDATE tasks SET owner_id=?1 WHERE id=?2", fixture.actor.String(), fixture.task.String())
			},
			occupant: func(actorOwnershipFixture) *journal.ActorID { return nil },
		},
		{
			name: "other-occupant",
			mutate: func(t *testing.T, db *DB, fixture actorOwnershipFixture) {
				t.Helper()
				startActorOwnershipTask(t, db, fixture.other, fixture.boot, fixture.task, "mismatching-start", actorOwnershipMaterialKind, json.RawMessage(`{"other":true}`), false)
				execActorOwnershipTestSQL(t, db, "UPDATE tasks SET owner_id=?1 WHERE id=?2", fixture.actor.String(), fixture.task.String())
			},
			occupant: func(fixture actorOwnershipFixture) *journal.ActorID { return &fixture.other },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openActorOwnershipDB(t, ":memory:")
			fixture := newActorOwnershipFixture(t, db)
			test.mutate(t, db, fixture)
			snapshot, err := db.QueryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: fixture.actor})
			var mismatch *journal.OwnerProjectionMismatchError
			if !errors.As(err, &mismatch) || !errors.Is(err, journal.ErrProjectionDivergence) {
				t.Fatalf("query error=%T %v, want typed projection mismatch", err, err)
			}
			if mismatch.Task != fixture.task || mismatch.Owner != fixture.actor || !reflect.DeepEqual(mismatch.ActiveOccupant, test.occupant(fixture)) || !reflect.DeepEqual(snapshot, journal.ActorOwnershipSnapshot{}) {
				t.Fatalf("mismatch=%+v snapshot=%+v", mismatch, snapshot)
			}
		})
	}
}

func TestActorOwnershipReadTakesNoWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "read-only-lock.db")
	reader := openActorOwnershipDB(t, path)
	fixture := newActorOwnershipFixture(t, reader)
	writer := openActorOwnershipDB(t, path)
	scope := takePoolScope(t, writer)
	if _, err := scope.conn.ExecContext(scope.ctx, "BEGIN IMMEDIATE"); err != nil {
		scope.release()
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := reader.QueryActorOwnership(ctx, journal.ActorOwnershipQuery{Actor: fixture.actor})
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("read under held writer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read did not complete under held writer")
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "ROLLBACK"); err != nil {
		scope.release()
		t.Fatal(err)
	}
	scope.release()
}

func TestActorOwnershipProducerIntegrityAndTwoProducerSelectivity(t *testing.T) {
	t.Run("missing-operation-producer", func(t *testing.T) {
		db := openActorOwnershipDB(t, ":memory:")
		fixture := newActorOwnershipFixture(t, db)
		removeActorOwnershipProducer(t, db, fixture.started)
		snapshot, err := db.QueryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: fixture.actor})
		if !errors.Is(err, journal.ErrSubtypeIntegrity) || !reflect.DeepEqual(snapshot, journal.ActorOwnershipSnapshot{}) || !strings.Contains(err.Error(), "fix:") {
			t.Fatalf("missing producer snapshot=%+v err=%v", snapshot, err)
		}
	})

	t.Run("normal-and-supplement-producers-negative", func(t *testing.T) {
		db := openActorOwnershipDB(t, ":memory:")
		fixture := newActorOwnershipFixture(t, db)
		insertActorOwnershipSupplement(t, db, fixture, journal.NewGovernedAllocationSupplementOperationID("ownership-start").OperationID())
		if _, err := db.Apply(journal.OperationInput{OperationID: "unrelated-owner-operation", ActorID: fixture.actor, AuthorityJournalID: &fixture.boot, CommandDigest: []byte("unrelated"), Effects: []journal.Effect{
			{Sort: journal.EffectTaskEvent, TaskID: fixture.task, EventKind: actorOwnershipMaterialKind, Payload: json.RawMessage(`{"value":"unrelated"}`)},
			{Sort: journal.EffectEvidence, TaskID: fixture.task, EvidenceKind: actorOwnershipEvidenceKind, ContentDigest: []byte("unrelated"), Payload: json.RawMessage(`{"evidence":"unrelated"}`)},
		}}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := db.QueryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: fixture.actor, MaterialKinds: []journal.EventKind{actorOwnershipMaterialKind}, EvidenceKinds: []journal.EvidenceKind{actorOwnershipEvidenceKind}})
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Tasks) != 1 || len(snapshot.Tasks[0].Materials) != 2 || len(snapshot.Tasks[0].Evidence) != 2 || snapshot.Work.MaterialRows != 2 || snapshot.Work.EvidenceRows != 2 {
			t.Fatalf("two-producer snapshot=%+v", snapshot)
		}
		for _, row := range snapshot.Tasks[0].Materials {
			if strings.Contains(string(row.Payload), "unrelated") {
				t.Fatalf("unrelated producer leaked: %+v", snapshot.Tasks[0].Materials)
			}
		}
	})
}

func TestActorOwnershipCountIgnoresOtherOwners(t *testing.T) {
	db := openActorOwnershipDB(t, ":memory:")
	actor := insertActorOwnershipActor(t, db, "count-actor")
	other := insertActorOwnershipActor(t, db, "count-other")
	boot := genesisBoot(t, db, actor)
	for i := range 40 {
		owned := createActorOwnershipTask(t, db, actor, boot, fmt.Sprintf("owned-%d", i), ptypes.PhaseWorkerSlices)
		startActorOwnershipTask(t, db, actor, boot, owned, fmt.Sprintf("owned-start-%d", i), actorOwnershipMaterialKind, json.RawMessage(`{"owned":true}`), false)
		foreign := createActorOwnershipTask(t, db, actor, boot, fmt.Sprintf("foreign-%d", i), ptypes.PhaseWorkerSlices)
		startActorOwnershipTask(t, db, other, boot, foreign, fmt.Sprintf("foreign-start-%d", i), actorOwnershipMaterialKind, json.RawMessage(`{"foreign":true}`), false)
	}
	snapshot, err := db.QueryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 40 || snapshot.Work.TaskRows != 40 {
		t.Fatalf("owned count=%d work=%+v, want exactly 40", len(snapshot.Tasks), snapshot.Work)
	}
}

func TestActorOwnershipReadDoesNotCommitProjectionWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "read-only-data-version.db")
	reader := openActorOwnershipDB(t, path)
	fixture := newActorOwnershipFixture(t, reader)
	observer := openActorOwnershipDB(t, path)
	scope := takePoolScope(t, observer)
	defer scope.release()
	const projectionFingerprint = `SELECT COALESCE(group_concat(id||':'||COALESCE(notes,''),'|'),'') FROM (SELECT id,notes FROM tasks ORDER BY id)`
	var before string
	if err := scope.conn.QueryRowContext(scope.ctx, projectionFingerprint).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.QueryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: fixture.actor, MaterialKinds: []journal.EventKind{actorOwnershipMaterialKind}, EvidenceKinds: []journal.EvidenceKind{actorOwnershipEvidenceKind}}); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := scope.conn.QueryRowContext(scope.ctx, projectionFingerprint).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("read-only query changed the task projection: before=%q after=%q", before, after)
	}
}

func removeActorOwnershipProducer(t *testing.T, db *DB, started journal.JournalID) {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	_, err := scope.conn.ExecContext(scope.ctx, "DELETE FROM journal_operations WHERE journal_id=(SELECT produced_by_operation_journal_id FROM journal WHERE journal_id=?)", started)
	if _, restoreErr := scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=ON"); restoreErr != nil && err == nil {
		err = restoreErr
	}
	if err != nil {
		t.Fatalf("remove actor-ownership producer: %v", err)
	}
}

func insertActorOwnershipSupplement(t *testing.T, db *DB, fixture actorOwnershipFixture, operation journal.OperationID) {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	var anchor int64
	if err := scope.conn.QueryRowContext(scope.ctx, "INSERT INTO journal(kind_id,actor_id,recorded_at,produced_by_operation_journal_id) VALUES(0,?1,?2,NULL) RETURNING journal_id", fixture.actor.String(), time.Now().UnixNano()).Scan(&anchor); err != nil {
		t.Fatalf("insert supplemental operation anchor: %v", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_operations(journal_id,operation_id,command_digest,mutation_digest) VALUES(?1,?2,?3,?4)", anchor, operation, []byte("supplement-command"), []byte("supplement-mutation")); err != nil {
		t.Fatalf("insert supplemental operation: %v", err)
	}
	var materialID int64
	if err := scope.conn.QueryRowContext(scope.ctx, "INSERT INTO journal(kind_id,actor_id,recorded_at,produced_by_operation_journal_id) VALUES(1,NULL,?1,?2) RETURNING journal_id", time.Now().UnixNano(), anchor).Scan(&materialID); err != nil {
		t.Fatalf("insert supplemental material journal: %v", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_task_events(journal_id,task_id,event_kind,payload) VALUES(?1,?2,?3,?4)", materialID, fixture.task.String(), actorOwnershipMaterialKind, `{"value":"supplement"}`); err != nil {
		t.Fatalf("insert supplemental material: %v", err)
	}
	var evidenceID int64
	if err := scope.conn.QueryRowContext(scope.ctx, "INSERT INTO journal(kind_id,actor_id,recorded_at,produced_by_operation_journal_id) VALUES(4,NULL,?1,?2) RETURNING journal_id", time.Now().UnixNano(), anchor).Scan(&evidenceID); err != nil {
		t.Fatalf("insert supplemental evidence journal: %v", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_evidence(journal_id,evidence_kind,task_id,content_digest,payload) VALUES(?1,?2,?3,?4,?5)", evidenceID, actorOwnershipEvidenceKind, fixture.task.String(), []byte("supplement"), `{"evidence":"supplement"}`); err != nil {
		t.Fatalf("insert supplemental evidence: %v", err)
	}
}

func execActorOwnershipTestSQL(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	scope := takePoolScope(t, db)
	_, err := scope.conn.ExecContext(scope.ctx, query, args...)
	scope.release()
	if err != nil {
		t.Fatalf("actor-ownership test SQL %q: %v", query, err)
	}
}

func TestActorOwnershipStageBarrierIsTestOnly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), "_test.go") || entry.Name() == "actor_ownership.go" {
			continue
		}
		contents, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(contents), "installActorOwnershipStageBarrier") {
			t.Fatalf("non-test production file %s installs actor ownership stage barrier", entry.Name())
		}
	}
}
