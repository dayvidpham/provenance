package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// This lifecycle regression proves that activation leaves a portable file
// database in a state a fresh database/sql pool can reopen. The reducer and
// projection suites own their domain replay assertions separately.
func TestFileActivationReopensWithWALAndRuntimePragmas(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/reopen.db"
	first, err := Open(path, nil)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	second, err := Open(path, nil)
	if err != nil {
		t.Fatalf("ExistingReady reopen: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	scope, err := second.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("lease reopened pool: %v", err)
	}
	defer scope.release()
	var journalMode string
	var foreignKeys, busyTimeout, synchronous int
	if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("reopened journal mode: %v", err)
	}
	if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("reopened foreign keys: %v", err)
	}
	if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("reopened busy timeout: %v", err)
	}
	if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatalf("reopened synchronous mode: %v", err)
	}
	if journalMode != "wal" || foreignKeys != 1 || busyTimeout != busyTimeoutMS || synchronous != 1 {
		t.Fatalf("reopened runtime state = (journal=%q fk=%d busy=%d sync=%d), want (wal,1,%d,1)", journalMode, foreignKeys, busyTimeout, synchronous, busyTimeoutMS)
	}
}

func TestMigrationFaultPreservesLegacyTasksAndJournal(t *testing.T) {
	t.Parallel()
	db := openPoolFileDB(t)
	system := registerSoftwareActor(t, db, "migration-system")
	bootstrap := genesisBoot(t, db, system)
	base := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	closedAt := base.Add(2 * time.Hour)
	legacyTasks := []ptypes.Task{
		{
			ID:          ptypes.TaskID{Namespace: "migration-fault", UUID: uuid.Must(uuid.NewV7())},
			Title:       "first legacy task",
			Description: "must survive an all-or-nothing migration fault",
			Status:      ptypes.StatusOpen,
			Priority:    ptypes.PriorityHigh,
			Type:        ptypes.TaskTypeBug,
			Phase:       ptypes.PhaseUnscoped,
			Notes:       "first legacy notes",
			CreatedAt:   base,
			UpdatedAt:   base.Add(time.Hour),
		},
		{
			ID:          ptypes.TaskID{Namespace: "migration-fault", UUID: uuid.Must(uuid.NewV7())},
			Title:       "second legacy task",
			Description: "must remain byte-for-byte logical task state",
			Status:      ptypes.StatusClosed,
			Priority:    ptypes.PriorityLow,
			Type:        ptypes.TaskTypeFeature,
			Phase:       ptypes.PhaseReview,
			Notes:       "second legacy notes",
			CreatedAt:   base.Add(3 * time.Hour),
			UpdatedAt:   base.Add(4 * time.Hour),
			ClosedAt:    &closedAt,
			CloseReason: "legacy completed",
		},
	}
	legacyRows := make([]journal.LegacyTaskRow, 0, len(legacyTasks))
	beforeTasks := make(map[ptypes.TaskID]ptypes.Task, len(legacyTasks))
	for _, task := range legacyTasks {
		if err := db.SeedLegacyTaskRow(task); err != nil {
			t.Fatalf("seed legacy task %q: %v", task.ID, err)
		}
		got, found, err := db.GetTask(task.ID)
		if err != nil || !found {
			t.Fatalf("capture legacy task %q = (%+v, %v, %v), want found", task.ID, got, found, err)
		}
		beforeTasks[task.ID] = got
		legacyRows = append(legacyRows, journal.LegacyTaskRow{
			ID: task.ID, Status: journal.TaskStatus(task.Status), CreatedAt: task.CreatedAt,
			UpdatedAt: task.UpdatedAt, ClosedAt: task.ClosedAt,
		})
	}
	beforeJournalRows := journalRowCount(t, db)

	_, err := db.AdversarialMigrateWithFault(journal.MigrationInput{
		System: system, BootstrapAuthority: bootstrap, Legacy: legacyRows,
	}, 1)
	if !errors.Is(err, journal.ErrMigrationFault) {
		t.Fatalf("AdversarialMigrateWithFault = %v, want ErrMigrationFault", err)
	}
	if anchors, err := db.CountBaselineAnchors(); err != nil || anchors != 0 {
		t.Fatalf("baseline anchors after rollback = (%d, %v), want (0, nil)", anchors, err)
	}
	if afterJournalRows := journalRowCount(t, db); afterJournalRows != beforeJournalRows {
		t.Fatalf("journal rows after migration fault = %d, want unchanged %d", afterJournalRows, beforeJournalRows)
	}
	for id, want := range beforeTasks {
		got, found, err := db.GetTask(id)
		if err != nil || !found {
			t.Fatalf("legacy task %q after rollback = (%+v, %v, %v), want found", id, got, found, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("legacy task %q changed during failed migration:\n got: %#v\nwant: %#v", id, got, want)
		}
	}
}

func TestReplayProjectionsUsesOneSnapshotAcrossConcurrentApply(t *testing.T) {
	t.Parallel()
	db := openPoolFileDB(t)
	actor := registerSoftwareActor(t, db, "replay-snapshot-system")
	bootstrap := genesisBoot(t, db, actor)
	taskID := journal.TaskID{Namespace: "replay-snapshot", UUID: uuid.Must(uuid.NewV7())}
	if _, err := db.Apply(journal.OperationInput{
		OperationID:        "replay-snapshot-create",
		ActorID:            actor,
		AuthorityJournalID: &bootstrap,
		CommandDigest:      []byte("replay-snapshot-create-command"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort: journal.EffectTaskCreate, TaskID: taskID, Title: "before concurrent apply",
			Description: "reader snapshot fixture", Type: ptypes.TaskTypeTask,
			Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped,
		}},
	}); err != nil {
		t.Fatalf("create replay fixture: %v", err)
	}

	updatedTitle := "after concurrent apply"
	writer := journal.OperationInput{
		OperationID:        "replay-snapshot-update",
		ActorID:            actor,
		AuthorityJournalID: &bootstrap,
		CommandDigest:      []byte("replay-snapshot-update-command"),
		RecordedAt:         time.Now().UTC().Add(time.Second).UnixNano(),
		Effects: []journal.Effect{{
			Sort: journal.EffectTaskEvent, TaskID: taskID, EventKind: journal.EventKindTaskUpdated,
			UpdateTitle: &updatedTitle,
		}},
	}
	reader := takePoolScope(t, db)
	defer reader.release()
	storedSnapshot := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		<-storedSnapshot
		_, err := db.Apply(writer)
		writerDone <- err
	}()

	_, err := reader.replayProjectionsModeWithStoredSnapshotBarrier(false, func() {
		close(storedSnapshot)
		select {
		case err := <-writerDone:
			if err != nil {
				t.Fatalf("concurrent Apply: %v", err)
			}
		case <-time.After(poolTestTimeout):
			t.Fatal("concurrent Apply did not finish while replay held its read snapshot")
		}
	})
	if err != nil {
		t.Fatalf("ReplayProjections mixed stored and derived snapshots: %v", err)
	}
	got, found, err := db.GetTask(taskID)
	if err != nil || !found || got.Title != updatedTitle {
		t.Fatalf("concurrent Apply task = (%+v, %v, %v), want updated title %q", got, found, err, updatedTitle)
	}
}
