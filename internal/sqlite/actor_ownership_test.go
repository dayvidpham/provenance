package sqlite

import (
	"context"
	"database/sql"
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

// createActorOwnershipTaskWithID creates a task under an identity the caller
// chose, for fixtures whose stored order has to be known rather than derived
// from a label.
func createActorOwnershipTaskWithID(t *testing.T, db *DB, actor journal.ActorID, boot journal.JournalID, id journal.TaskID, phase ptypes.Phase) {
	t.Helper()
	if _, err := db.Apply(journal.OperationInput{
		OperationID: "create-" + journal.OperationID(id.UUID.String()), ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte(id.String()),
		Effects: []journal.Effect{{Sort: journal.EffectTaskCreate, TaskID: id, Title: id.String(), Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: phase}},
	}); err != nil {
		t.Fatalf("create task %s: %v", id, err)
	}
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

// TestActorOwnershipWireGuardSuppressesValuesThatDoNotFitTheBudget drives the
// three result statements themselves, with the arguments the read binds to them,
// and observes what SQLite hands the Go scanner.
//
// The result byte bound rests on one claim: a value that cannot fit the
// remaining budget never enters Go memory, because the byte length of every
// variable-width column is selected beside the value and the value itself only
// when that length fits. Nothing above the scanner can demonstrate that, because
// the read's outcome is identical either way — the running byte total refuses
// the crossing row whether the value arrived or the wire suppressed it. So the
// arrival is observed here, at the boundary the claim is about, and the
// remaining budget is driven across each guarded column in both directions: a
// budget that suppresses nothing, one that suppresses a subset, and one that
// suppresses everything.
//
// The exact boundary of each column — a value whose declared length is exactly
// the remaining budget — is covered per column by
// TestActorOwnershipWireGuardAdmitsAValueThatExactlyFitsTheBudget, because a
// budget chosen from this table only lands on a column's declared length by
// accident, and an accidental landing is a guard nobody has to keep.
func TestActorOwnershipWireGuardSuppressesValuesThatDoNotFitTheBudget(t *testing.T) {
	t.Run("owned-task-columns", func(t *testing.T) {
		fixture := actorOwnershipOwnedTaskWireFixture(t)
		for _, budget := range actorOwnershipWireOwnedTaskBudgets {
			t.Run(budget.name, func(t *testing.T) {
				rows := fixture.drive(t, budget.bound)
				audit := &actorOwnershipWireAudit{t: t, statement: fixture.statement, bound: budget.bound}
				for index, row := range rows {
					for _, column := range row.columns {
						audit.column(column)
					}
					if !row.phase.Valid || !row.started.Valid || !row.producer.Valid || !row.occupant.Valid {
						t.Fatalf("owned-task row %d lost an unguarded column: phase=%+v started=%+v producer=%+v occupant=%+v", index+1, row.phase, row.started, row.producer, row.occupant)
					}
				}
				audit.done()
				fixture.check(t, rows)
			})
		}
	})

	t.Run("material-columns", func(t *testing.T) {
		fixture := actorOwnershipMaterialWireFixture(t)
		for _, budget := range actorOwnershipWireBudgets {
			t.Run(budget.name, func(t *testing.T) {
				rows := fixture.drive(t, budget.bound)
				audit := &actorOwnershipWireAudit{t: t, statement: fixture.statement, bound: budget.bound}
				for _, row := range rows {
					for _, column := range row.columns {
						audit.column(column)
					}
				}
				audit.done()
				fixture.check(t, rows)
			})
		}
	})

	t.Run("evidence-columns", func(t *testing.T) {
		fixture := actorOwnershipEvidenceWireFixture(t)
		for _, budget := range actorOwnershipWireBudgets {
			t.Run(budget.name, func(t *testing.T) {
				rows := fixture.drive(t, budget.bound)
				audit := &actorOwnershipWireAudit{t: t, statement: fixture.statement, bound: budget.bound}
				for _, row := range rows {
					for _, column := range row.columns {
						audit.column(column)
					}
				}
				audit.done()
				fixture.check(t, rows)
			})
		}
	})
}

// TestActorOwnershipWireGuardAdmitsAValueThatExactlyFitsTheBudget covers the
// other direction of the same claim, and does it per column rather than for one
// representative column: a value whose declared length is EXACTLY the remaining
// budget must still arrive.
//
// That is the one case a strict guard cannot survive. A guard written `<` rather
// than `<=` suppresses precisely the value whose length equals the budget, so
// with only budgets that land beside a column's declared length, turning seven
// of the eight guards strict leaves the whole suite green. Here each guarded
// column gets its own case derived from the fixture's own stored lengths, so a
// guard that turns strict on any one of them turns exactly that column's
// subtest red and the other seven green, and the red names the column.
func TestActorOwnershipWireGuardAdmitsAValueThatExactlyFitsTheBudget(t *testing.T) {
	for _, statement := range []struct {
		name  string
		build func(*testing.T) actorOwnershipWireFixture
	}{
		{name: "owned-task-columns", build: actorOwnershipOwnedTaskWireFixture},
		{name: "material-columns", build: actorOwnershipMaterialWireFixture},
		{name: "evidence-columns", build: actorOwnershipEvidenceWireFixture},
	} {
		t.Run(statement.name, func(t *testing.T) {
			actorOwnershipWireBoundary(t, statement.build(t))
		})
	}
}

// actorOwnershipWireBoundary derives one exact-boundary case per guarded column
// and asserts that the value whose declared length is exactly the remaining
// budget arrives for each of them.
//
// The bound is the shortest value each column actually returns, measured under
// the production limit where this fixture suppresses nothing, so each case is a
// real arrival rather than an arithmetic claim. The columns are the ones the
// driver names, and a column added to a statement without being added to its
// driver fails the scan instead of going untested, so the list cannot drift away
// from the SQL silently. A column every row stores as NULL gets no case and is
// simply absent from the list, which is the one gap this shape cannot close; no
// such column exists in these three fixtures, and all eight are named in a
// subtest run. Each subtest also states the safety direction for every column it
// saw, so the arrival it proves is not bought by hydrating something larger.
func actorOwnershipWireBoundary(t *testing.T, fixture actorOwnershipWireFixture) {
	t.Helper()
	order := make([]string, 0, 8)
	shortest := make(map[string]int64, 8)
	for _, row := range fixture.drive(t, journal.MaxActorOwnershipResultBytes) {
		for _, column := range row.columns {
			if !column.declared.Valid {
				continue
			}
			if _, seen := shortest[column.name]; !seen {
				order = append(order, column.name)
				shortest[column.name] = column.declared.Int64
				continue
			}
			if column.declared.Int64 < shortest[column.name] {
				shortest[column.name] = column.declared.Int64
			}
		}
	}
	for _, name := range order {
		bound := shortest[name]
		t.Run(name, func(t *testing.T) {
			admitted := false
			for _, row := range fixture.drive(t, bound) {
				for _, column := range row.columns {
					if !column.declared.Valid {
						if column.arrived {
							t.Fatalf("%s %s: a stored NULL column arrived as a value", fixture.statement, column.name)
						}
						continue
					}
					if column.declared.Int64 > bound && column.arrived {
						t.Fatalf("%s %s: Go received %d bytes against a %d-byte remaining budget, so a value that does not fit was copied into memory", fixture.statement, column.name, column.size, bound)
					}
					if column.name != name || column.declared.Int64 != bound {
						continue
					}
					if !column.arrived {
						t.Fatalf("%s %s: a value of exactly the remaining %d bytes was suppressed; the budget is inclusive, so a strict < guard is indistinguishable from no guard at all", fixture.statement, name, bound)
					}
					admitted = true
				}
			}
			if !admitted {
				t.Fatalf("%s %s: no value of exactly %d bytes was returned, so this case cannot tell an inclusive guard from a strict one", fixture.statement, name, bound)
			}
		})
	}
}

// actorOwnershipWireOwnedTaskBudgets walks the remaining budget across the four
// guarded owned-task columns of that fixture, one case suppressing every guarded
// value, one covering the short identifiers, and one covering all of them. The
// exact boundary of each of the four is derived separately, per column, by
// actorOwnershipWireBoundary.
var actorOwnershipWireOwnedTaskBudgets = []struct {
	name  string
	bound int64
}{
	{name: "budget-suppresses-every-guarded-value", bound: 1},
	{name: "budget-covers-the-short-identifiers", bound: 32},
	{name: "budget-covers-every-guarded-value", bound: 4 << 10},
}

// actorOwnershipWireBudgets walks the remaining budget across the two stored
// values each material and evidence row carries: the kind and the payload, which
// is either 14 and 20 bytes or 5000. Every case therefore suppresses a
// different subset of the four guarded columns, and the two 5000-byte values
// stand in for the oversized payload the bound exists for. The exact boundary of
// each of the four is derived separately, per column, by
// actorOwnershipWireBoundary.
var actorOwnershipWireBudgets = []struct {
	name  string
	bound int64
}{
	{name: "budget-suppresses-every-guarded-value", bound: 8},
	{name: "budget-covers-a-payload-but-not-its-kind", bound: 20},
	{name: "budget-suppresses-one-oversized-payload", bound: 4 << 10},
	{name: "budget-covers-every-guarded-value", bound: 1 << 20},
}

// The two wire-guard task identities are named for the order their wire strings
// sort in, and both fixtures below create them the other way round: task two is
// created first, so a statement ordering by journal ID returns it first while the
// order its join plan produces is task one. That is what makes the declared
// length sequences the fixtures check a real check of the statements' ORDER BY
// rather than of a coincidence.
var (
	actorOwnershipWireTaskOne = journal.TaskID{Namespace: "actor-ownership", UUID: uuid.MustParse("00000000-0000-4000-8000-000000000001")}
	actorOwnershipWireTaskTwo = journal.TaskID{Namespace: "actor-ownership", UUID: uuid.MustParse("00000000-0000-4000-8000-000000000002")}
)

// actorOwnershipWireFixture is one result statement's fixture: a driver that
// runs that statement against any remaining byte budget, the name the subjects
// report under, and the stored-order facts the fixture was built to pin. Both
// wire-guard subjects build their fixtures here, so the two agree on the rows
// they observe and neither can drift from the other.
type actorOwnershipWireFixture struct {
	statement string
	drive     actorOwnershipWireDriver
	check     func(t *testing.T, rows []actorOwnershipWireRow)
}

// actorOwnershipWireDriver runs one of the three result statements with a given
// remaining byte budget — the value the read binds as its per-value limit — and
// reports every row it returned.
type actorOwnershipWireDriver func(t *testing.T, bound int64) []actorOwnershipWireRow

// actorOwnershipWireRow is one row as a result statement returned it. columns
// carries that row's guarded columns; the remaining fields are the columns the
// read needs that carry no guard, so a subject can still assert they arrived.
type actorOwnershipWireRow struct {
	columns  []actorOwnershipWireColumn
	phase    sql.NullInt64
	started  sql.NullInt64
	producer sql.NullInt64
	occupant sql.NullString
}

// declared returns the byte length SQLite declared for one guarded column of
// this row, and whether the statement selected that column at all.
func (r actorOwnershipWireRow) declared(name string) (sql.NullInt64, bool) {
	for _, column := range r.columns {
		if column.name == name {
			return column.declared, true
		}
	}
	return sql.NullInt64{}, false
}

// actorOwnershipWireColumn is one guarded column of one row: the byte length
// SQLite declared for it, and what the Go scanner was actually handed.
type actorOwnershipWireColumn struct {
	name     string
	declared sql.NullInt64
	arrived  bool
	size     int
}

// actorOwnershipWireColumnAt reads a guarded column's scan destination, which is
// the only thing that says whether the value crossed the wire.
func actorOwnershipWireColumnAt(t *testing.T, name string, declared sql.NullInt64, destination any) actorOwnershipWireColumn {
	t.Helper()
	arrived, size := actorOwnershipWireValue(t, destination)
	return actorOwnershipWireColumn{name: name, declared: declared, arrived: arrived, size: size}
}

// actorOwnershipOwnedTaskWireFixture is the owned-task statement's fixture: one
// plain owned task and one transferred task whose successor carries a stored
// predecessor, so all four guarded owned-task columns return a value and one of
// them returns NULL on the plain row. episodes.actor_id is selected on every row
// and carries no guard of its own; the read's accounting is what budgets it, which
// TestActorOwnershipEpisodeOccupantIsBudgetedButNeverSuppressed covers.
func actorOwnershipOwnedTaskWireFixture(t *testing.T) actorOwnershipWireFixture {
	t.Helper()
	db := openActorOwnershipDB(t, ":memory:")
	actor := insertActorOwnershipActor(t, db, "wire-actor")
	boot := genesisBoot(t, db, actor)
	plain := createActorOwnershipTask(t, db, actor, boot, "wire-plain", ptypes.PhaseWorkerSlices)
	plainStart := startActorOwnershipTask(t, db, actor, boot, plain, "wire-plain-start", actorOwnershipMaterialKind, json.RawMessage(`{"plain":true}`), false)
	transferred := createActorOwnershipTask(t, db, actor, boot, "wire-transferred", ptypes.PhaseWorkerSlices)
	startActorOwnershipTask(t, db, actor, boot, transferred, "wire-transferred-prior", actorOwnershipMaterialKind, json.RawMessage(`{"prior":true}`), false)
	successor := startActorOwnershipSuccessor(t, db, actor, boot, transferred, "wire-transferred-prior", "wire-transferred-successor")

	fixture := actorOwnershipWireFixture{statement: "owned tasks", drive: actorOwnershipOwnedTaskWireDriver(t, db, actor)}
	fixture.check = func(t *testing.T, rows []actorOwnershipWireRow) {
		t.Helper()
		starts := make([]int64, 0, len(rows))
		predecessors := 0
		for _, row := range rows {
			starts = append(starts, row.started.Int64)
			// The length column is selected whether or not the wire suppressed the
			// value, so a stored predecessor is still countable when its own value
			// was not sent.
			if declared, present := row.declared("episodes.predecessor_assignment_id"); present && declared.Valid {
				predecessors++
			}
		}
		if want := []int64{int64(plainStart), int64(successor)}; !reflect.DeepEqual(starts, want) {
			t.Fatalf("owned-task start order=%v, want %v", starts, want)
		}
		if predecessors != 1 {
			t.Fatalf("%d rows carry a stored predecessor, want exactly the transfer successor", predecessors)
		}
	}
	return fixture
}

// actorOwnershipOwnedTaskWireDriver runs the owned-task statement against a
// store and actor the caller supplied, so a subject with its own fixture can
// observe arrivals on the same statement the shared fixtures drive.
func actorOwnershipOwnedTaskWireDriver(t *testing.T, db *DB, actor journal.ActorID) actorOwnershipWireDriver {
	t.Helper()
	return func(t *testing.T, bound int64) []actorOwnershipWireRow {
		t.Helper()
		scope := takePoolScope(t, db)
		defer scope.release()
		rows, err := scope.conn.QueryContext(scope.ctx, actorOwnershipOwnedTasksSQL, actor.String(), bound, transitionStartedID, slotOwnerResponsibilityID, transitionEndedID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		observed := make([]actorOwnershipWireRow, 0, 2)
		for rows.Next() {
			var (
				taskLength, occupantLength, assignmentLength, operationLength, predecessorLength sql.NullInt64
				taskID, occupant, assignmentID, operationID, predecessorID                       sql.NullString
				phase, started, producer                                                         sql.NullInt64
			)
			if err := rows.Scan(&taskLength, &taskID, &phase, &assignmentLength, &assignmentID, &occupantLength, &occupant, &started, &producer, &operationLength, &operationID, &predecessorLength, &predecessorID); err != nil {
				t.Fatal(err)
			}
			observed = append(observed, actorOwnershipWireRow{
				columns: []actorOwnershipWireColumn{
					actorOwnershipWireColumnAt(t, "tasks.id", taskLength, &taskID),
					actorOwnershipWireColumnAt(t, "episodes.assignment_id", assignmentLength, &assignmentID),
					actorOwnershipWireColumnAt(t, "journal_operations.operation_id", operationLength, &operationID),
					actorOwnershipWireColumnAt(t, "episodes.predecessor_assignment_id", predecessorLength, &predecessorID),
				},
				phase: phase, started: started, producer: producer, occupant: occupant,
			})
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return observed
	}
}

func actorOwnershipMaterialWireFixture(t *testing.T) actorOwnershipWireFixture {
	t.Helper()
	db := openActorOwnershipDB(t, ":memory:")
	actor := insertActorOwnershipActor(t, db, "wire-material")
	boot := genesisBoot(t, db, actor)
	createActorOwnershipTaskWithID(t, db, actor, boot, actorOwnershipWireTaskTwo, ptypes.PhaseWorkerSlices)
	startActorOwnershipTask(t, db, actor, boot, actorOwnershipWireTaskTwo, "wire-material-first", actorOwnershipMaterialKind, json.RawMessage(`{"small":true}`), false)
	createActorOwnershipTaskWithID(t, db, actor, boot, actorOwnershipWireTaskOne, ptypes.PhaseWorkerSlices)
	startActorOwnershipTask(t, db, actor, boot, actorOwnershipWireTaskOne, "wire-material-second", actorOwnershipMaterialKind, actorOwnershipJSONSize(t, 5000), false)

	bindings := actorOwnershipWireBindings(t, db, actorOwnershipWireSource{task: actorOwnershipWireTaskTwo, operation: "wire-material-first"}, actorOwnershipWireSource{task: actorOwnershipWireTaskOne, operation: "wire-material-second"})
	kinds, err := json.Marshal([]journal.EventKind{actorOwnershipMaterialKind})
	if err != nil {
		t.Fatal(err)
	}
	fixture := actorOwnershipWireFixture{statement: "material"}
	fixture.drive = func(t *testing.T, bound int64) []actorOwnershipWireRow {
		t.Helper()
		scope := takePoolScope(t, db)
		defer scope.release()
		rows, err := scope.conn.QueryContext(scope.ctx, actorOwnershipMaterialsSQL, bindings, bound, string(kinds))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		observed := make([]actorOwnershipWireRow, 0, 2)
		for rows.Next() {
			var (
				journalID                 int64
				ownerTask                 string
				kindLength, payloadLength sql.NullInt64
				kind                      sql.NullString
				payload                   []byte
			)
			if err := rows.Scan(&journalID, &ownerTask, &kindLength, &kind, &payloadLength, &payload); err != nil {
				t.Fatal(err)
			}
			observed = append(observed, actorOwnershipWireRow{columns: []actorOwnershipWireColumn{
				actorOwnershipWireColumnAt(t, "journal_task_events.event_kind", kindLength, &kind),
				actorOwnershipWireColumnAt(t, "journal_task_events.payload", payloadLength, &payload),
			}})
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return observed
	}
	fixture.check = func(t *testing.T, rows []actorOwnershipWireRow) {
		t.Helper()
		got := actorOwnershipWirePayloadLengths(rows, "journal_task_events.payload")
		if want := []int64{int64(len(`{"small":true}`)), 5000}; !reflect.DeepEqual(got, want) {
			t.Fatalf("material declared payload lengths=%v, want %v in journal-id order", got, want)
		}
	}
	return fixture
}

func actorOwnershipEvidenceWireFixture(t *testing.T) actorOwnershipWireFixture {
	t.Helper()
	db := openActorOwnershipDB(t, ":memory:")
	actor := insertActorOwnershipActor(t, db, "wire-evidence")
	boot := genesisBoot(t, db, actor)
	createActorOwnershipTaskWithID(t, db, actor, boot, actorOwnershipWireTaskTwo, ptypes.PhaseWorkerSlices)
	startActorOwnershipTask(t, db, actor, boot, actorOwnershipWireTaskTwo, "wire-evidence-first", actorOwnershipMaterialKind, json.RawMessage(`{"small":true}`), true)
	createActorOwnershipTaskWithID(t, db, actor, boot, actorOwnershipWireTaskOne, ptypes.PhaseWorkerSlices)
	startActorOwnershipTask(t, db, actor, boot, actorOwnershipWireTaskOne, "wire-evidence-second", actorOwnershipMaterialKind, json.RawMessage(`{"large":true}`), true)
	enlargeActorOwnershipPayload(t, db, "UPDATE journal_evidence SET payload=?1 WHERE journal_id=?2", actorOwnershipJSONSize(t, 5000), actorOwnershipEvidenceJournalID(t, db, actorOwnershipWireTaskOne, actorOwnershipEvidenceKind))

	bindings := actorOwnershipWireBindings(t, db, actorOwnershipWireSource{task: actorOwnershipWireTaskTwo, operation: "wire-evidence-first"}, actorOwnershipWireSource{task: actorOwnershipWireTaskOne, operation: "wire-evidence-second"})
	kinds, err := json.Marshal([]journal.EvidenceKind{actorOwnershipEvidenceKind})
	if err != nil {
		t.Fatal(err)
	}
	fixture := actorOwnershipWireFixture{statement: "evidence"}
	fixture.drive = func(t *testing.T, bound int64) []actorOwnershipWireRow {
		t.Helper()
		scope := takePoolScope(t, db)
		defer scope.release()
		rows, err := scope.conn.QueryContext(scope.ctx, actorOwnershipEvidenceSQL, bindings, bound, string(kinds))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		observed := make([]actorOwnershipWireRow, 0, 2)
		for rows.Next() {
			var (
				journalID                 int64
				ownerTask                 string
				kindLength, payloadLength sql.NullInt64
				kind                      sql.NullString
				payload                   []byte
			)
			if err := rows.Scan(&journalID, &ownerTask, &kindLength, &kind, &payloadLength, &payload); err != nil {
				t.Fatal(err)
			}
			observed = append(observed, actorOwnershipWireRow{columns: []actorOwnershipWireColumn{
				actorOwnershipWireColumnAt(t, "journal_evidence.evidence_kind", kindLength, &kind),
				actorOwnershipWireColumnAt(t, "journal_evidence.payload", payloadLength, &payload),
			}})
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return observed
	}
	fixture.check = func(t *testing.T, rows []actorOwnershipWireRow) {
		t.Helper()
		got := actorOwnershipWirePayloadLengths(rows, "journal_evidence.payload")
		if want := []int64{int64(len(`{"evidence":"start"}`)), 5000}; !reflect.DeepEqual(got, want) {
			t.Fatalf("evidence declared payload lengths=%v, want %v in journal-id order", got, want)
		}
	}
	return fixture
}

// actorOwnershipWirePayloadLengths collects the byte lengths a statement
// declared for one payload column, in the order it returned its rows, so a
// subject can state the stored sequence rather than a count.
func actorOwnershipWirePayloadLengths(rows []actorOwnershipWireRow, name string) []int64 {
	lengths := make([]int64, 0, len(rows))
	for _, row := range rows {
		declared, _ := row.declared(name)
		lengths = append(lengths, declared.Int64)
	}
	return lengths
}

// actorOwnershipWireAudit answers the question the result byte bound rests on,
// for the values one statement handed the Go scanner: each guarded value arrived
// if and only if its own byte length fitted the remaining budget, and no single
// value larger than that budget was ever copied. The bound is per value on the
// wire; the cumulative bound is the read's own accounting, which stops at the
// crossing row instead of returning what it has copied so far.
type actorOwnershipWireAudit struct {
	t         *testing.T
	statement string
	bound     int64
	hydrated  int64
	largest   int64
	observed  int
}

func (a *actorOwnershipWireAudit) column(column actorOwnershipWireColumn) {
	a.t.Helper()
	if !column.declared.Valid {
		if column.arrived {
			a.t.Fatalf("%s %s: a stored NULL column arrived as a value", a.statement, column.name)
		}
		return
	}
	if fits := column.declared.Int64 <= a.bound; column.arrived != fits {
		a.t.Fatalf("%s %s: %d declared bytes against a %d-byte remaining budget arrived=%t, want arrived=%t", a.statement, column.name, column.declared.Int64, a.bound, column.arrived, fits)
	}
	if column.arrived && int64(column.size) != column.declared.Int64 {
		a.t.Fatalf("%s %s: %d bytes arrived for a declared length of %d", a.statement, column.name, column.size, column.declared.Int64)
	}
	a.hydrated += int64(column.size)
	if int64(column.size) > a.largest {
		a.largest = int64(column.size)
	}
	a.observed++
}

func (a *actorOwnershipWireAudit) done() {
	a.t.Helper()
	if a.observed == 0 {
		a.t.Fatalf("%s: no guarded column was observed", a.statement)
	}
	if a.largest > a.bound {
		a.t.Fatalf("%s: Go received a %d-byte value against a %d-byte remaining budget (%d bytes in total)", a.statement, a.largest, a.bound, a.hydrated)
	}
}

// actorOwnershipWireValue reports what the Go scanner was actually handed for
// one column: a suppressed value is NULL, which leaves a []byte destination nil
// and a sql.NullString destination invalid.
func actorOwnershipWireValue(t *testing.T, destination any) (arrived bool, size int) {
	t.Helper()
	switch typed := destination.(type) {
	case *sql.NullString:
		return typed.Valid, len(typed.String)
	case *[]byte:
		return *typed != nil, len(*typed)
	default:
		t.Fatalf("the wire audit cannot read a %T scan destination", destination)
		return false, 0
	}
}

type actorOwnershipWireSource struct {
	task      journal.TaskID
	operation string
}

// actorOwnershipWireBindings builds the producer bindings statements 4 and 5
// read. The read derives them from the owned-task row it has just scanned, so a
// subject that drives those statements directly has to state the same three
// fields; the length assertions above fail if the statement stops selecting the
// rows these bindings name.
func actorOwnershipWireBindings(t *testing.T, db *DB, sources ...actorOwnershipWireSource) string {
	t.Helper()
	bindings := make([]actorOwnershipOwnedBinding, 0, len(sources))
	for _, source := range sources {
		scope := takePoolScope(t, db)
		var producer int64
		err := scope.conn.QueryRowContext(scope.ctx, "SELECT journal_id FROM journal_operations WHERE operation_id=?1", source.operation).Scan(&producer)
		scope.release()
		if err != nil {
			t.Fatalf("producer journal for %s: %v", source.operation, err)
		}
		bindings = append(bindings, actorOwnershipOwnedBinding{
			TaskID:              source.task.String(),
			StartProducer:       producer,
			SupplementOperation: journal.NewGovernedAllocationSupplementOperationID(journal.OperationID(source.operation)).OperationID(),
		})
	}
	encoded, err := json.Marshal(bindings)
	if err != nil {
		t.Fatalf("encode actor-ownership producer bindings: %v", err)
	}
	return string(encoded)
}

// actorOwnershipMaterialJournalID is the one task-event row a kind selects for
// a task. The create operation writes a second task event for the same task, so
// the kind is part of the lookup: a row the material stage never reads would
// make a fixture for that stage pass for the wrong reason.
func actorOwnershipMaterialJournalID(t *testing.T, db *DB, task journal.TaskID, kind journal.EventKind) int64 {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	var id int64
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT journal_id FROM journal_task_events WHERE task_id=?1 AND event_kind=?2", task.String(), string(kind)).Scan(&id); err != nil {
		t.Fatalf("material row for %s/%s: %v", task, kind, err)
	}
	return id
}

// actorOwnershipEvidenceJournalID is the one evidence row a kind selects for a
// task, looked up the same way actorOwnershipMaterialJournalID is.
func actorOwnershipEvidenceJournalID(t *testing.T, db *DB, task journal.TaskID, kind journal.EvidenceKind) int64 {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	var id int64
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT journal_id FROM journal_evidence WHERE task_id=?1 AND evidence_kind=?2", task.String(), string(kind)).Scan(&id); err != nil {
		t.Fatalf("evidence row for %s/%s: %v", task, kind, err)
	}
	return id
} // startActorOwnershipSuccessor ends the named owner-slot episode and starts its
// successor carrying that episode as the transfer predecessor the read returns
// without following.
func startActorOwnershipSuccessor(t *testing.T, db *DB, actor journal.ActorID, boot journal.JournalID, task journal.TaskID, previous, operation string) journal.JournalID {
	t.Helper()
	bootID := boot
	result, err := db.Apply(journal.OperationInput{
		OperationID: journal.OperationID(operation), ActorID: actor, AuthorityJournalID: &bootID, CommandDigest: []byte(operation),
		Effects: []journal.Effect{
			{Sort: journal.EffectAssignmentEnd, AssignmentID: journal.AssignmentID(previous), TaskID: task, SlotID: journal.SlotOwnerResponsibility},
			{Sort: journal.EffectAssignmentStart, ResultSlot: "start", AssignmentID: journal.AssignmentID(operation), TaskID: task, SlotID: journal.SlotOwnerResponsibility, Occupant: actor, Predecessor: journal.AssignmentID(previous)},
		},
	})
	if err != nil {
		t.Fatalf("start owner-slot successor %s: %v", operation, err)
	}
	for _, binding := range result.ResultSlots {
		if binding.Slot == "start" {
			return binding.ProducedJournalID
		}
	}
	t.Fatalf("successor operation %s produced no start slot", operation)
	return 0
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

// TestActorOwnershipEpisodeOccupantIsBudgetedButNeverSuppressed covers the one
// variable-width result value the read cannot suppress, and pins the two halves
// of what it does instead.
//
// episodes.actor_id is the live occupant of the winning episode, and the
// owned-task stage reads a NULL occupant as *OwnerProjectionMismatchError. So
// the column carries no suppression clause: a merely large but valid occupant
// must keep arriving, or the bound would report a size refusal to the operator as
// an accusation that tasks.owner_id disagrees with the writer's winning-episode
// rule, which is a different fault with a different fix. What it must not do is
// escape the bound, so its declared length is selected beside the value and added
// to the row total.
//
// Each half is asserted on its own. Counted: the row's total is measured from the
// statement's own declared lengths, and the read is driven one byte under that
// total — a limit every guarded column clears and only the occupant's own bytes
// cross — and it must refuse with *ActorOwnershipLimitError, never with a
// projection mismatch. Never suppressed: at a one-byte budget, where every
// guarded column is suppressed, the occupant still arrives intact, and at exactly
// the row's own total the read succeeds and returns the task.
func TestActorOwnershipEpisodeOccupantIsBudgetedButNeverSuppressed(t *testing.T) {
	db := openActorOwnershipDB(t, ":memory:")
	// A namespace no supported writer produces and no length ceiling rejects, so
	// the stored occupant parses as a valid actor identity and the only thing
	// wrong with the row is that it is too large for the budget. Inserting the
	// agents row directly is what makes episodes.actor_id's reference satisfiable.
	wide := journal.ActorID{Namespace: strings.Repeat("oversized-occupant-", 256), UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("actor/oversized-occupant"))}
	insertActorOwnershipActorID(t, db, wide)
	boot := genesisBoot(t, db, wide)
	task := createActorOwnershipTask(t, db, wide, boot, "occupant-budget", ptypes.PhaseWorkerSlices)
	startActorOwnershipTask(t, db, wide, boot, task, "occupant-budget-start", actorOwnershipMaterialKind, json.RawMessage(`{"occupied":true}`), false)

	drive := actorOwnershipOwnedTaskWireDriver(t, db, wide)
	rows := drive(t, journal.MaxActorOwnershipResultBytes)
	if len(rows) != 1 {
		t.Fatalf("owned-task statement returned %d rows, want the one oversized-occupant task", len(rows))
	}
	if !rows[0].occupant.Valid || rows[0].occupant.String != wide.String() {
		t.Fatalf("stored occupant=%v, want the oversized actor identity intact", rows[0].occupant)
	}
	guarded := int64(0)
	for _, column := range rows[0].columns {
		if column.declared.Valid {
			guarded += column.declared.Int64
		}
	}
	occupant := int64(len(wide.String()))
	if occupant <= guarded {
		t.Fatalf("the oversized occupant is %d bytes against %d bytes of guarded columns, so a budget that clears the guarded columns is not crossed by the occupant alone", occupant, guarded)
	}
	// The statement declares the occupant's length beside the value, so the row
	// total the read has to reproduce is the guarded columns plus the occupant,
	// and the two cases below bracket it to the byte.
	total := guarded + occupant

	snapshot, err := db.queryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: wide}, total-1)
	var limitErr *journal.ActorOwnershipLimitError
	if !errors.As(err, &limitErr) || !errors.Is(err, journal.ErrActorOwnershipLimit) {
		t.Fatalf("query error=%T %v, want *ActorOwnershipLimitError: the occupant's bytes must be inside the bound", err, err)
	}
	var mismatch *journal.OwnerProjectionMismatchError
	if errors.As(err, &mismatch) || errors.Is(err, journal.ErrProjectionDivergence) {
		t.Fatalf("query error=%v, a size refusal must not be reported as a projection mismatch", err)
	}
	if limitErr.Stage != journal.ActorOwnershipStageOwnedTasks || limitErr.LimitBytes != total-1 || limitErr.ObservedBytes != total || !reflect.DeepEqual(snapshot, journal.ActorOwnershipSnapshot{}) {
		t.Fatalf("limit error=%+v snapshot=%+v, want the owned-tasks stage refusing at %d observed against a %d-byte limit", limitErr, snapshot, total, total-1)
	}

	// The same class has to survive a budget BELOW the occupant's own length,
	// which is the case a suppression clause would get wrong: a suppressed
	// occupant arrives as a NULL, and the NULL arm turns that into a projection
	// mismatch. So the refusal here is a size refusal too, not an accusation that
	// tasks.owner_id disagrees with the writer about a store that agrees.
	under, err := db.queryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: wide}, occupant-1)
	var underLimit *journal.ActorOwnershipLimitError
	if !errors.As(err, &underLimit) || !errors.Is(err, journal.ErrActorOwnershipLimit) {
		t.Fatalf("query error=%T %v, want *ActorOwnershipLimitError below the occupant's own %d bytes", err, err, occupant)
	}
	if errors.As(err, &mismatch) || errors.Is(err, journal.ErrProjectionDivergence) {
		t.Fatalf("query error=%v, a size refusal below the occupant's length must not be reported as a projection mismatch", err)
	}
	if underLimit.Stage != journal.ActorOwnershipStageOwnedTasks || underLimit.LimitBytes != occupant-1 || underLimit.ObservedBytes != total || !reflect.DeepEqual(under, journal.ActorOwnershipSnapshot{}) {
		t.Fatalf("limit error=%+v snapshot=%+v, want the owned-tasks stage refusing at %d observed against a %d-byte limit", underLimit, under, total, occupant-1)
	}

	atOne := drive(t, 1)
	if len(atOne) != 1 {
		t.Fatalf("owned-task statement returned %d rows at a one-byte budget, want the row whose occupant is never suppressed", len(atOne))
	}
	if !atOne[0].occupant.Valid || atOne[0].occupant.String != wide.String() {
		t.Fatalf("occupant=%v at a one-byte budget, want the oversized identity to cross the wire regardless of the budget", atOne[0].occupant)
	}
	suppressed, guardedColumns := 0, 0
	for _, column := range atOne[0].columns {
		if !column.declared.Valid {
			// A stored NULL is not a value the guard can suppress or admit.
			continue
		}
		guardedColumns++
		if column.arrived {
			t.Fatalf("guarded column %s arrived at a one-byte budget, so the fixture no longer exercises suppression and the occupant's arrival beside it proves nothing", column.name)
		}
		suppressed++
	}
	if suppressed == 0 || suppressed != guardedColumns {
		t.Fatalf("%d of %d guarded columns were suppressed at a one-byte budget, want every one of them suppressed", suppressed, guardedColumns)
	}

	snapshot, err = db.queryActorOwnership(context.Background(), journal.ActorOwnershipQuery{Actor: wide}, total)
	if err != nil {
		t.Fatalf("query at the row's own %d-byte total: %v, want the oversized occupant to be inside its own budget", total, err)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].TaskID != task || snapshot.Work.ResultBytes != total {
		t.Fatalf("snapshot tasks=%+v work=%+v, want the one task carried by a %d-byte result", snapshot.Tasks, snapshot.Work, total)
	}
}

// insertActorOwnershipActorID registers an actor identity the test chose, so a
// fixture can carry an identity shape the writer's own name-based path cannot
// produce.
func insertActorOwnershipActorID(t *testing.T, db *DB, actor journal.ActorID) {
	t.Helper()
	execActorOwnershipTestSQL(t, db, "INSERT INTO agents (id,kind_id) VALUES (?1,?2)", actor.String(), int(ptypes.AgentKindSoftware))
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

// enlargeActorOwnershipPayload stores a payload far larger than the one the
// writer produced, so a statement's own wire guard has something to suppress
// while the stored value stays valid JSON.
func enlargeActorOwnershipPayload(t *testing.T, db *DB, statement string, payload json.RawMessage, journalID int64) {
	t.Helper()
	execActorOwnershipTestSQL(t, db, statement, string(payload), journalID)
}

// corruptActorOwnershipPayload stores a payload the relation's own JSON check
// would refuse. No supported writer produces one, which is the whole reason the
// result stages treat it as an integrity fault, so the check is turned off for
// that one statement and put back on the same connection.
func corruptActorOwnershipPayload(t *testing.T, db *DB, statement, invalid string, journalID int64) {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA ignore_check_constraints=ON"); err != nil {
		t.Fatalf("relax the stored JSON check: %v", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, statement, invalid, journalID); err != nil {
		t.Fatalf("store an undecodable payload: %v", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA ignore_check_constraints=OFF"); err != nil {
		t.Fatalf("restore the stored JSON check: %v", err)
	}
}

// TestActorOwnershipIntegrityFaultCarriesItsStage covers each of the three
// result stages that can reject a stored row. Each subject drives the real
// store, so the stage in the error is the stage that read the row, and each
// asserts that the subtype sentinel still matches and that a stored decode cause
// is still reachable, which is what a consumer's error mapping depends on.
func TestActorOwnershipIntegrityFaultCarriesItsStage(t *testing.T) {
	for _, test := range []struct {
		name  string
		stage journal.ActorOwnershipStage
		query func(actorOwnershipFixture) journal.ActorOwnershipQuery
		arm   func(*testing.T, *DB, actorOwnershipFixture)
		cause string
	}{
		{
			name:  "owned-tasks-without-its-operation-producer",
			stage: journal.ActorOwnershipStageOwnedTasks,
			query: func(fixture actorOwnershipFixture) journal.ActorOwnershipQuery {
				return journal.ActorOwnershipQuery{Actor: fixture.actor}
			},
			arm: func(t *testing.T, db *DB, fixture actorOwnershipFixture) {
				removeActorOwnershipProducer(t, db, fixture.started)
			},
		},
		{
			name:  "owned-tasks-with-an-undecodable-operation-id",
			stage: journal.ActorOwnershipStageOwnedTasks,
			query: func(fixture actorOwnershipFixture) journal.ActorOwnershipQuery {
				return journal.ActorOwnershipQuery{Actor: fixture.actor}
			},
			arm: func(t *testing.T, db *DB, fixture actorOwnershipFixture) {
				execActorOwnershipTestSQL(t, db, "UPDATE journal_operations SET operation_id=?1 WHERE operation_id=?2", "", "ownership-start")
			},
			cause: "invalid operation ID",
		},
		{
			name:  "materials-with-an-undecodable-payload",
			stage: journal.ActorOwnershipStageMaterials,
			query: func(fixture actorOwnershipFixture) journal.ActorOwnershipQuery {
				return journal.ActorOwnershipQuery{Actor: fixture.actor, MaterialKinds: []journal.EventKind{actorOwnershipMaterialKind}}
			},
			arm: func(t *testing.T, db *DB, fixture actorOwnershipFixture) {
				corruptActorOwnershipPayload(t, db, "UPDATE journal_task_events SET payload=?1 WHERE journal_id=?2", "not json at all", actorOwnershipMaterialJournalID(t, db, fixture.task, actorOwnershipMaterialKind))
			},
		},
		{
			name:  "evidence-with-an-undecodable-payload",
			stage: journal.ActorOwnershipStageEvidence,
			query: func(fixture actorOwnershipFixture) journal.ActorOwnershipQuery {
				return journal.ActorOwnershipQuery{Actor: fixture.actor, EvidenceKinds: []journal.EvidenceKind{actorOwnershipEvidenceKind}}
			},
			arm: func(t *testing.T, db *DB, fixture actorOwnershipFixture) {
				corruptActorOwnershipPayload(t, db, "UPDATE journal_evidence SET payload=?1 WHERE journal_id=?2", "not json at all", actorOwnershipEvidenceJournalID(t, db, fixture.task, actorOwnershipEvidenceKind))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openActorOwnershipDB(t, ":memory:")
			fixture := newActorOwnershipFixture(t, db)
			test.arm(t, db, fixture)
			snapshot, err := db.QueryActorOwnership(context.Background(), test.query(fixture))
			var integrity *journal.ActorOwnershipIntegrityError
			if !errors.As(err, &integrity) {
				t.Fatalf("query error=%T %v, want *ActorOwnershipIntegrityError", err, err)
			}
			if !errors.Is(err, journal.ErrSubtypeIntegrity) {
				t.Fatalf("integrity error=%v, want ErrSubtypeIntegrity in the chain", err)
			}
			if integrity.Stage != test.stage {
				t.Fatalf("integrity stage=%q, want %q: the operator would be told the wrong stage holds the damage", integrity.Stage, test.stage)
			}
			if !strings.Contains(err.Error(), "stage "+string(test.stage)) || !strings.Contains(err.Error(), "fix:") {
				t.Fatalf("integrity text=%q, want the stage token and a fix", err.Error())
			}
			if !reflect.DeepEqual(snapshot, journal.ActorOwnershipSnapshot{}) {
				t.Fatalf("integrity fault returned a partial snapshot=%+v", snapshot)
			}
			if test.cause == "" {
				if integrity.Cause != nil {
					t.Fatalf("integrity cause=%v, want none for this arm", integrity.Cause)
				}
				return
			}
			if integrity.Cause == nil || !strings.Contains(integrity.Cause.Error(), test.cause) {
				t.Fatalf("integrity cause=%v, want the stored decode rejection naming %q", integrity.Cause, test.cause)
			}
			if !errors.Is(err, integrity.Cause) || !strings.Contains(err.Error(), integrity.Cause.Error()) {
				t.Fatalf("integrity error=%v, want the stored decode cause reachable through errors.Is and the message", err)
			}
		})
	}
}

// TestActorOwnershipIntegrityErrorKeepsItsCauseReachable pins the two halves of
// the typed fault's contract on their own: the subtype sentinel always matches,
// and a stored decode cause stays in the chain for errors.Is and errors.As, with
// no cause rendered as "<nil>" in the text.
func TestActorOwnershipIntegrityErrorKeepsItsCauseReachable(t *testing.T) {
	cause := errors.New("decode stored task id \"broken\": invalid ID")
	withoutCause := &journal.ActorOwnershipIntegrityError{Stage: journal.ActorOwnershipStageMaterials, Problem: "material row 42 has invalid JSON", Fix: "restore the canonical task-event payload from the same committed backup"}
	withCause := &journal.ActorOwnershipIntegrityError{Stage: journal.ActorOwnershipStageEvidence, Problem: "evidence owner task is malformed", Fix: "restore the owned task and producer binding from the same committed backup", Cause: cause}
	if !errors.Is(withoutCause, journal.ErrSubtypeIntegrity) || errors.Is(withoutCause, cause) {
		t.Fatalf("cause-free integrity error must match only the subtype sentinel: %v", withoutCause)
	}
	if !errors.Is(withCause, journal.ErrSubtypeIntegrity) || !errors.Is(withCause, cause) {
		t.Fatalf("integrity error with a cause must match the sentinel and the cause: %v", withCause)
	}
	if !strings.HasPrefix(withoutCause.Error(), journal.ErrSubtypeIntegrity.Error()+": ") || strings.Contains(withoutCause.Error(), "<nil>") {
		t.Fatalf("cause-free integrity text=%q", withoutCause.Error())
	}
	if want := journal.ErrSubtypeIntegrity.Error() + ": evidence owner task is malformed — why: the stored row cannot be decoded as a supported ownership fact; where: QueryActorOwnership result-row validation, stage evidence; when: inside the read transaction; impact: no result returned and nothing was written; fix: restore the owned task and producer binding from the same committed backup: " + cause.Error(); withCause.Error() != want {
		t.Fatalf("integrity text=%q, want %q", withCause.Error(), want)
	}
}

// TestActorOwnershipStageBarrierIsTestOnly scans the package's Go sources and
// refuses any non-test file that installs the stage barrier seam. The scan is
// deliberately narrowed to `*.go` and to non-test files: a scratch file or an
// editor backup that happens to mention the symbol is not a production install,
// so reading it would turn this subject red for a reason that has nothing to do
// with the invariant. The subject's own non-vacuity is checked rather than
// assumed — the file that legitimately declares the seam has to be among the
// files the glob returns, and at least one other non-test Go file has to have
// been read, or a glob that matches nothing would report success while testing
// nothing.
func TestActorOwnershipStageBarrierIsTestOnly(t *testing.T) {
	const declaring = "actor_ownership.go"
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned, found := 0, false
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == declaring {
			found = true
			continue
		}
		scanned++
		contents, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(contents), "installActorOwnershipStageBarrier") {
			t.Fatalf("non-test production file %s installs actor ownership stage barrier", name)
		}
	}
	if !found {
		t.Fatalf("%s was not among the scanned Go files, so the exclusion that lets it declare the seam is untested", declaring)
	}
	if scanned == 0 {
		t.Fatalf("no other non-test Go file was scanned, so the stage-barrier seam was never checked")
	}
}
