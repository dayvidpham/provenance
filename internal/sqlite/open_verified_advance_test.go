package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// open_verified_advance_test.go pins PROPOSAL-20 (sanctioned-write ledger
// advance, the induction): a write through applyPreparedOperation or
// ApplyBatchContext that finds the ledger matching its pre-write snapshot,
// folds its delta through the shared reducer, and passes the store-wide gates
// advances the ledger to its post-write identity in its own commit — so the
// next open takes the fast path with verified_count untouched.
//
// Every pin below exercises its path through the production entrypoints
// (Open, Apply, ApplyBatchContext, AppendTaskEvent, AddComment,
// StartActivityWithID, ReplayProjections) and goes RED when the behaviour is
// removed: deleting the advance (or recording the wrong identity) turns the
// fast-path assertions into full-path counts, dropping the pre-check lets
// poisoned/foreign ledgers advance, moving the advance outside the transaction
// breaks atomicity, and generalising it to AppendTaskEvent or the direct
// mutators makes the closed-set pins advance.
//
// None of them calls t.Parallel(): each opens and closes file-backed stores on
// its own path, mirroring open_verified_test.go.

// advanceSeed returns an actor, its genesis authority, and a task on a
// file-backed store whose ledger is current, plus the ledger's verified_count.
// Unlike seedLedgerStore it returns the actor and authority so the test can
// issue further sanctioned writes through the production Apply path without an
// intervening direct-mutator write (which would poison the pre-check).
func advanceSeed(t *testing.T, path string) (journal.ActorID, journal.JournalID, ptypes.TaskID, int64) {
	t.Helper()
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("activate advance store on %q: %v", path, err)
	}
	actor, err := db.RegisterSoftwareAgent("verified", "advance-writer", "0", "open_verified_advance_test")
	if err != nil {
		_ = db.Close()
		t.Fatalf("register writer: %v", err)
	}
	boot := genesisBoot(t, db, actor.ID)
	task := ptypes.TaskID{Namespace: "verified", UUID: uuid.Must(uuid.NewV7())}
	if _, err := db.Apply(journal.OperationInput{
		OperationID:        "op-advance-seed-create",
		ActorID:            actor.ID,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("advance-seed-c"),
		MutationDigest:     []byte("advance-seed-m"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort: journal.EffectTaskCreate, TaskID: task, Title: "advance seed task",
			Description: "seed for the sanctioned-advance pins",
			Type:        ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseWorkerSlices,
		}},
	}); err != nil {
		_ = db.Close()
		t.Fatalf("journal-create the seed task: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close advance store after seeding: %v", err)
	}
	// The seeding writes leave the ledger stale, so this reopen runs the full
	// path once and re-persists the ledger for the state the store is left in.
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("reopen to establish a current ledger: %v", err)
	}
	count, present := ledgerRow(t, path)
	if !present {
		t.Fatal("the reopen did not persist a verified-as-of ledger")
	}
	return actor.ID, boot, task, count
}

// advanceLedgerState reads the persisted ledger identity and count through an
// independent read-only connection. The store must be closed.
func advanceLedgerState(t *testing.T, path string) (generation, journalID int64, digest, shape, tablesDigest string, count int64) {
	t.Helper()
	raw, err := sql.Open(sqliteDriverName, "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open read-only advance reader on %q: %v", path, err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close advance reader: %v", err)
		}
	}()
	if err := raw.QueryRow("SELECT generation, journal_id, journal_digest, shape_id, tables_digest, verified_count FROM "+verifiedStoreTable+" WHERE id = 1").Scan(&generation, &journalID, &digest, &shape, &tablesDigest, &count); err != nil {
		t.Fatalf("read advance ledger from %q: %v", path, err)
	}
	return generation, journalID, digest, shape, tablesDigest, count
}

// advanceStoreState reads the live store identity (generation and journal tail)
// through an independent read-only connection. The store must be closed.
func advanceStoreState(t *testing.T, path string) (generation, journalID int64) {
	t.Helper()
	raw, err := sql.Open(sqliteDriverName, "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open read-only store reader on %q: %v", path, err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close store reader: %v", err)
		}
	}()
	if err := raw.QueryRow("SELECT generation FROM " + storeGenerationTable + " WHERE id = 1").Scan(&generation); err != nil {
		t.Fatalf("read store generation from %q: %v", path, err)
	}
	if err := raw.QueryRow("SELECT COALESCE(MAX(journal_id), 0) FROM journal").Scan(&journalID); err != nil {
		t.Fatalf("read journal tail from %q: %v", path, err)
	}
	return generation, journalID
}

// advanceApplyTask commits one task-create through the production Apply path.
func advanceApplyTask(t *testing.T, db *DB, actor journal.ActorID, boot journal.JournalID, opID string) ptypes.TaskID {
	t.Helper()
	task := ptypes.TaskID{Namespace: "verified", UUID: uuid.Must(uuid.NewV7())}
	if _, err := db.Apply(journal.OperationInput{
		OperationID:        journal.OperationID(opID),
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte(opID + "-c"),
		MutationDigest:     []byte(opID + "-m"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort: journal.EffectTaskCreate, TaskID: task, Title: "advance " + opID,
			Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseWorkerSlices,
		}},
	}); err != nil {
		t.Fatalf("Apply %q: %v", opID, err)
	}
	return task
}

// TestSanctionedSingleWriteAdvancesLedger is P1: a verified store plus one
// Session/Apply write leaves the ledger recording the post-write identity with
// verified_count untouched, and the next open takes the fast path. RED if the
// advance is removed (the reopen runs the full path, count +1) or if the
// advance records the pre-write identity (same observable).
func TestSanctionedSingleWriteAdvancesLedger(t *testing.T) {
	path := t.TempDir() + "/advance-p1.db"
	actor, boot, _, seeded := advanceSeed(t, path)

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen before the sanctioned write: %v", err)
	}
	advanceApplyTask(t, db, actor, boot, "op-advance-p1")
	if err := db.Close(); err != nil {
		t.Fatalf("close after the sanctioned write: %v", err)
	}

	ledgerGen, ledgerJID, _, ledgerShape, _, ledgerCount := advanceLedgerState(t, path)
	storeGen, storeJID := advanceStoreState(t, path)
	if ledgerGen != storeGen || ledgerJID != storeJID {
		t.Fatalf("ledger identity (gen=%d tail=%d) does not match store (gen=%d tail=%d); the sanctioned write did not advance it", ledgerGen, ledgerJID, storeGen, storeJID)
	}
	if ledgerShape != verifiedShapeID() {
		t.Fatalf("ledger shape %q is not this build's v3 shape %q", ledgerShape, verifiedShapeID())
	}
	if ledgerCount != seeded {
		t.Fatalf("verified_count moved %d -> %d on a sanctioned write; only a whole-store verification may advance it", seeded, ledgerCount)
	}

	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after a sanctioned write: %v", err)
	}
	if after, _ := ledgerRow(t, path); after != seeded {
		t.Fatalf("verified_count moved %d -> %d on the next open; the sanctioned write did not earn the fast path", seeded, after)
	}
}

// TestSanctionedBatchWriteAdvancesLedgerOnce is P2: one ApplyBatchContext over
// 2+ records advances the ledger exactly to the post-batch identity, once.
// RED if only the single path advances (the batch leaves the ledger stale and
// the reopen runs the full path).
func TestSanctionedBatchWriteAdvancesLedgerOnce(t *testing.T) {
	path := t.TempDir() + "/advance-p2.db"
	actor, boot, _, seeded := advanceSeed(t, path)

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen before the batch: %v", err)
	}
	inputs := make([]journal.OperationInput, 0, 2)
	for _, opID := range []string{"op-advance-p2-a", "op-advance-p2-b"} {
		task := ptypes.TaskID{Namespace: "verified", UUID: uuid.Must(uuid.NewV7())}
		inputs = append(inputs, journal.OperationInput{
			OperationID:        journal.OperationID(opID),
			ActorID:            actor,
			AuthorityJournalID: &boot,
			CommandDigest:      []byte(opID + "-c"),
			MutationDigest:     []byte(opID + "-m"),
			RecordedAt:         time.Now().UTC().UnixNano(),
			Effects: []journal.Effect{{
				Sort: journal.EffectTaskCreate, TaskID: task, Title: "batch task",
				Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseWorkerSlices,
			}},
		})
	}
	result, err := db.ApplyBatchContext(context.Background(), inputs)
	if err != nil {
		_ = db.Close()
		t.Fatalf("ApplyBatchContext: %v", err)
	}
	if !result.Committed {
		_ = db.Close()
		t.Fatalf("batch did not commit: %+v", result)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close after the batch: %v", err)
	}

	ledgerGen, ledgerJID, _, _, _, ledgerCount := advanceLedgerState(t, path)
	storeGen, storeJID := advanceStoreState(t, path)
	if ledgerGen != storeGen || ledgerJID != storeJID {
		t.Fatalf("ledger identity (gen=%d tail=%d) does not match post-batch store (gen=%d tail=%d)", ledgerGen, ledgerJID, storeGen, storeJID)
	}
	if ledgerCount != seeded {
		t.Fatalf("verified_count moved %d -> %d on a batch; only a whole-store verification may advance it", seeded, ledgerCount)
	}

	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after a batch: %v", err)
	}
	if after, _ := ledgerRow(t, path); after != seeded {
		t.Fatalf("verified_count moved %d -> %d on the open after a batch; the batch did not earn the fast path", seeded, after)
	}
}

// TestOutOfBandWriteStillTakesFullPath is P3: a raw DML write outside the fold
// moves the generation without advancing the ledger, and the next open runs
// the full path — including when a sanctioned write follows the poison before
// any full path heals it. RED if the advance drops the pre-check (the
// sanctioned write would then heal the unverified store and the reopen would
// skip).
func TestOutOfBandWriteStillTakesFullPath(t *testing.T) {
	t.Run("raw DML alone", func(t *testing.T) {
		path := t.TempDir() + "/advance-p3a.db"
		_, _, _, seeded := advanceSeed(t, path)

		db, err := Open(path, nil)
		if err != nil {
			t.Fatalf("reopen before the out-of-band write: %v", err)
		}
		scope := takePoolScope(t, db)
		// Value-preserving UPDATE: the trigger still fires, so the generation
		// moves without the content changing.
		if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE tasks SET title = title"); err != nil {
			scope.release()
			_ = db.Close()
			t.Fatalf("out-of-band UPDATE: %v", err)
		}
		scope.release()
		if err := db.Close(); err != nil {
			t.Fatalf("close after the out-of-band write: %v", err)
		}

		if err := reopenFileBacked(t, path); err != nil {
			t.Fatalf("open after an out-of-band write: %v", err)
		}
		if after, _ := ledgerRow(t, path); after != seeded+1 {
			t.Fatalf("verified_count after an out-of-band write = %d, want %d; the full path must have run exactly once", after, seeded+1)
		}
	})

	t.Run("sanctioned write does not heal a poisoned store", func(t *testing.T) {
		path := t.TempDir() + "/advance-p3b.db"
		actor, boot, _, seeded := advanceSeed(t, path)

		db, err := Open(path, nil)
		if err != nil {
			t.Fatalf("reopen before the poison: %v", err)
		}
		scope := takePoolScope(t, db)
		if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE tasks SET title = title"); err != nil {
			scope.release()
			_ = db.Close()
			t.Fatalf("out-of-band UPDATE: %v", err)
		}
		scope.release()
		// A sanctioned write on the poisoned store must commit its own delta but
		// must NOT advance the ledger: the pre-state was never verified.
		advanceApplyTask(t, db, actor, boot, "op-advance-p3b")
		if err := db.Close(); err != nil {
			t.Fatalf("close after the sanctioned write: %v", err)
		}

		if err := reopenFileBacked(t, path); err != nil {
			t.Fatalf("open after poison plus sanctioned write: %v", err)
		}
		if after, _ := ledgerRow(t, path); after != seeded+1 {
			t.Fatalf("verified_count = %d, want %d; a sanctioned write must not heal an unverified store", after, seeded+1)
		}
		// Healed now: the next open is the fast path again.
		if err := reopenFileBacked(t, path); err != nil {
			t.Fatalf("open after the heal: %v", err)
		}
		if after, _ := ledgerRow(t, path); after != seeded+1 {
			t.Fatalf("verified_count moved after the heal; want the fast path to hold at %d", seeded+1)
		}
	})
}

// TestForeignShapeLedgerRefusesAdvance is P4: a ledger row carrying a foreign
// shape stops the next sanctioned write advancing, and the next open runs the
// full path. RED if the advance ignores shape_id.
func TestForeignShapeLedgerRefusesAdvance(t *testing.T) {
	path := t.TempDir() + "/advance-p4.db"
	actor, boot, _, seeded := advanceSeed(t, path)

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen before the shape forgery: %v", err)
	}
	scope := takePoolScope(t, db)
	if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE "+verifiedStoreTable+" SET shape_id = ?1 WHERE id = 1", "foreign-shape"); err != nil {
		scope.release()
		_ = db.Close()
		t.Fatalf("forge a foreign shape: %v", err)
	}
	scope.release()
	advanceApplyTask(t, db, actor, boot, "op-advance-p4")
	if err := db.Close(); err != nil {
		t.Fatalf("close after the write on a foreign-shaped ledger: %v", err)
	}

	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after a foreign-shaped ledger plus write: %v", err)
	}
	if after, _ := ledgerRow(t, path); after != seeded+1 {
		t.Fatalf("verified_count = %d, want %d; a foreign-shaped ledger must force the full path", after, seeded+1)
	}
	// The full path re-persisted this build's v3 shape: the store is current.
	_, _, _, shape, _, _ := advanceLedgerState(t, path)
	if shape != verifiedShapeID() {
		t.Fatalf("ledger shape after the heal %q is not this build's shape %q", shape, verifiedShapeID())
	}
}

// TestFailedWriteLeavesLedgerAndStateTogether is P5 (rollback atomicity): a
// sanctioned batch that fails inside its transaction rolls everything back —
// the ledger still describes the store, and the next open is the fast path —
// while a successful write moves both. RED if the advance is moved outside the
// transaction (the failure would leave the ledger ahead of the rolled-back
// store and the reopen would take the full path).
func TestFailedWriteLeavesLedgerAndStateTogether(t *testing.T) {
	path := t.TempDir() + "/advance-p5.db"
	actor, boot, _, seeded := advanceSeed(t, path)

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen before the failing batch: %v", err)
	}
	good := journal.OperationInput{
		OperationID:        "op-advance-p5-good",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("p5-good-c"),
		MutationDigest:     []byte("p5-good-m"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort: journal.EffectTaskCreate, TaskID: ptypes.TaskID{Namespace: "verified", UUID: uuid.Must(uuid.NewV7())},
			Title: "p5 good", Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseWorkerSlices,
		}},
	}
	// Cites an authority that was never committed: the fold fails inside the
	// transaction after the good record already folded, so the whole batch —
	// good record, generation bumps, and any ledger advance — rolls back.
	bogus := journal.JournalID(1 << 40)
	bad := journal.OperationInput{
		OperationID:        "op-advance-p5-bad",
		ActorID:            actor,
		AuthorityJournalID: &bogus,
		CommandDigest:      []byte("p5-bad-c"),
		MutationDigest:     []byte("p5-bad-m"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort: journal.EffectTaskCreate, TaskID: ptypes.TaskID{Namespace: "verified", UUID: uuid.Must(uuid.NewV7())},
			Title: "p5 bad", Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseWorkerSlices,
		}},
	}
	if _, err := db.ApplyBatchContext(context.Background(), []journal.OperationInput{good, bad}); err == nil {
		_ = db.Close()
		t.Fatal("batch with an uncommitted authority succeeded; want the typed authority failure and a rollback")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close after the rolled-back batch: %v", err)
	}

	// Nothing committed: the ledger still describes the store, so the next open
	// is the fast path and the count does not move.
	ledgerGen, ledgerJID, _, _, _, ledgerCount := advanceLedgerState(t, path)
	storeGen, storeJID := advanceStoreState(t, path)
	if ledgerGen != storeGen || ledgerJID != storeJID {
		t.Fatalf("after a rolled-back batch ledger (gen=%d tail=%d) != store (gen=%d tail=%d); a half-advance leaked", ledgerGen, ledgerJID, storeGen, storeJID)
	}
	if ledgerCount != seeded {
		t.Fatalf("verified_count moved %d -> %d on a rolled-back batch", seeded, ledgerCount)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after a rolled-back batch: %v", err)
	}
	if after, _ := ledgerRow(t, path); after != seeded {
		t.Fatalf("verified_count moved %d -> %d after a rolled-back batch; the ledger and the store did not stay together", seeded, after)
	}

	// And a successful write still moves both together.
	db, err = Open(path, nil)
	if err != nil {
		t.Fatalf("reopen for the successful write: %v", err)
	}
	advanceApplyTask(t, db, actor, boot, "op-advance-p5-ok")
	if err := db.Close(); err != nil {
		t.Fatalf("close after the successful write: %v", err)
	}
	ledgerGen, ledgerJID, _, _, _, _ = advanceLedgerState(t, path)
	storeGen, storeJID = advanceStoreState(t, path)
	if ledgerGen != storeGen || ledgerJID != storeJID {
		t.Fatalf("after a successful write ledger (gen=%d tail=%d) != store (gen=%d tail=%d)", ledgerGen, ledgerJID, storeGen, storeJID)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after the successful write: %v", err)
	}
	if after, _ := ledgerRow(t, path); after != seeded {
		t.Fatalf("verified_count moved %d -> %d after a successful write; want the fast path", seeded, after)
	}
}

// TestClosedSanctionedSetDoesNotAdvance is P6: writes outside the sanctioned
// fold prove less and must NOT advance — the next open runs the full path.
// AppendTaskEvent is the named dead path: it is not on the Journal interface
// and has no callers, and on the current schema its bare task-event insert is
// refused by the journal CHECK (a task_event must be operation-produced), so it
// fails without writing and advances nothing. AddComment and StartActivityWithID
// are live direct mutators: they commit to covered tables outside the fold, so
// the ledger stays stale and the next open runs the full path. RED if someone
// generalises the advance without proof.
func TestClosedSanctionedSetDoesNotAdvance(t *testing.T) {
	t.Run("AppendTaskEvent advances nothing", func(t *testing.T) {
		path := t.TempDir() + "/advance-p6-append.db"
		actor, _, task, seeded := advanceSeed(t, path)

		db, err := Open(path, nil)
		if err != nil {
			t.Fatalf("reopen before AppendTaskEvent: %v", err)
		}
		_, appendErr := db.AppendTaskEvent(journal.AppendTaskEventInput{
			ActorID:    actor,
			TaskID:     task,
			EventKind:  journal.EventKindTaskUpdated,
			RecordedAt: time.Now().UTC(),
			Payload:    []byte(`{}`),
		})
		if appendErr == nil {
			_ = db.Close()
			t.Fatal("AppendTaskEvent succeeded; it is the dead path and must stay refused by the journal CHECK")
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close after refused AppendTaskEvent: %v", err)
		}

		// Refused before any write: the ledger still describes the store, so the
		// next open is the fast path — and, crucially, no half-advance leaked.
		ledgerGen, ledgerJID, _, _, _, ledgerCount := advanceLedgerState(t, path)
		storeGen, storeJID := advanceStoreState(t, path)
		if ledgerGen != storeGen || ledgerJID != storeJID {
			t.Fatalf("after a refused AppendTaskEvent ledger (gen=%d tail=%d) != store (gen=%d tail=%d)", ledgerGen, ledgerJID, storeGen, storeJID)
		}
		if ledgerCount != seeded {
			t.Fatalf("verified_count moved %d -> %d on a refused write", seeded, ledgerCount)
		}
		if err := reopenFileBacked(t, path); err != nil {
			t.Fatalf("open after refused AppendTaskEvent: %v", err)
		}
		if after, _ := ledgerRow(t, path); after != seeded {
			t.Fatalf("verified_count moved %d -> %d after a refused write; want the fast path", seeded, after)
		}
	})

	t.Run("direct mutator AddComment diverges and is refused", func(t *testing.T) {
		path := t.TempDir() + "/advance-p6-comment.db"
		actor, _, task, seeded := advanceSeed(t, path)

		db, err := Open(path, nil)
		if err != nil {
			t.Fatalf("reopen before AddComment: %v", err)
		}
		if _, err := db.AddComment(task, actor, "p6 direct comment"); err != nil {
			_ = db.Close()
			t.Fatalf("AddComment: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close after AddComment: %v", err)
		}

		// Boundary: a direct un-journaled comment is outside the journaled
		// domain projection, so the full path refuses it exactly as before —
		// the fix changes nothing there. The direct write poisoned the
		// pre-check (had it been verifiable, the sanctioned set still would
		// not have healed it), and the refused open re-persists nothing.
		before, _ := ledgerRow(t, path)
		if before != seeded {
			t.Fatalf("the direct write itself moved verified_count to %d (want %d)", before, seeded)
		}
		err = reopenFileBacked(t, path)
		if !errors.Is(err, journal.ErrProjectionDivergence) {
			t.Fatalf("opening a store with an un-journaled comment = %v, want journal.ErrProjectionDivergence", err)
		}
		if after, _ := ledgerRow(t, path); after != before {
			t.Fatalf("a refused open re-persisted the ledger (count %d -> %d)", before, after)
		}
	})

	t.Run("direct mutator StartActivityWithID", func(t *testing.T) {
		path := t.TempDir() + "/advance-p6-activity.db"
		actor, _, _, seeded := advanceSeed(t, path)

		db, err := Open(path, nil)
		if err != nil {
			t.Fatalf("reopen before StartActivityWithID: %v", err)
		}
		id := ptypes.ActivityID{Namespace: "verified", UUID: uuid.Must(uuid.NewV7())}
		if _, err := db.StartActivityWithID(id, actor, ptypes.PhaseWorkerSlices, ptypes.StageInProgress, "p6"); err != nil {
			_ = db.Close()
			t.Fatalf("StartActivityWithID: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close after StartActivityWithID: %v", err)
		}

		if err := reopenFileBacked(t, path); err != nil {
			t.Fatalf("open after StartActivityWithID: %v", err)
		}
		if after, _ := ledgerRow(t, path); after != seeded+1 {
			t.Fatalf("verified_count = %d, want %d; a direct mutator must not advance the ledger", after, seeded+1)
		}
	})
}

// TestLiveAndShadowReducersAgree is P7 (reducer-twin parity): a canonical
// task-event operation folded live must pass a from-empty ReplayProjections —
// forced explicitly here, because after this fix the next open no longer runs
// it. A drift between the live projector (projectV1TaskEvent) and the shadow
// materializer would surface here as a divergence. RED if the twins drift
// (ReplayProjections fails); it also fails if the live fold itself is broken.
func TestLiveAndShadowReducersAgree(t *testing.T) {
	path := t.TempDir() + "/advance-p7.db"
	actor, boot, task, seeded := advanceSeed(t, path)

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen before the canonical task event: %v", err)
	}
	newTitle := "p7 updated title"
	if _, err := db.Apply(journal.OperationInput{
		OperationID:        "op-advance-p7-event",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("p7-c"),
		MutationDigest:     []byte("p7-m"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort: journal.EffectTaskEvent, TaskID: task,
			EventKind: journal.EventKindTaskUpdated, UpdateTitle: &newTitle,
		}},
	}); err != nil {
		_ = db.Close()
		t.Fatalf("Apply canonical task event: %v", err)
	}
	// Force the convergence the next open would now skip: the live fold above
	// must agree with a from-empty shadow re-derivation.
	if _, err := db.ReplayProjections(); err != nil {
		_ = db.Close()
		t.Fatalf("ReplayProjections after a live canonical task event: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close after the parity write: %v", err)
	}

	// The sanctioned write advanced the ledger (P1), so this open is fast; drop
	// the ledger to force the deferred full path explicitly and prove it still
	// converges rather than merely being deferred forever.
	dropVerifiedLedger(t, path)
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("forced full-path open after dropping the ledger: %v", err)
	}
	if count, present := ledgerRow(t, path); !present || count != 1 {
		t.Fatalf("ledger after a forced full path: present=%v count=%d, want a fresh row at 1 (seeded generation was %d)", present, count, seeded)
	}
	if err := reopenFileBacked(t, path); err != nil {
		t.Fatalf("open after the forced heal: %v", err)
	}
	if after, _ := ledgerRow(t, path); after != 1 {
		t.Fatalf("verified_count moved after the forced heal; want the fast path to hold at 1, got %d", after)
	}
}
