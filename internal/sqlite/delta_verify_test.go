package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// gateTally counts store-wide gates observed through the caller's context.
// Each test owns its tally, so parallel tests cannot share a counter.
type gateTally struct {
	mu sync.Mutex
	n  map[string]int
}

func (g *gateTally) record(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n == nil {
		g.n = make(map[string]int)
	}
	g.n[name]++
}

func (g *gateTally) count(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n[name]
}

func (g *gateTally) context() context.Context {
	return withJournalGateProbe(context.Background(), g.record)
}

func assertGateCounts(t *testing.T, g *gateTally, delta, subtype, actor int) {
	t.Helper()
	if gotD, gotS, gotA := g.count(journalGateDelta), g.count(journalGateSubtype), g.count(journalGateActor); gotD != delta || gotS != subtype || gotA != actor {
		t.Fatalf("gates delta=%d subtype=%d actor=%d, want delta=%d subtype=%d actor=%d", gotD, gotS, gotA, delta, subtype, actor)
	}
}

func openCurrentLedger(t *testing.T) (*DB, journal.ActorID, journal.JournalID, int64) {
	t.Helper()
	path := t.TempDir() + "/delta-current.db"
	actor, boot, _, seeded := advanceSeed(t, path)
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen a current ledger: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close current-ledger store: %v", err)
		}
	})
	return db, actor, boot, seeded
}

func deltaTaskInput(actor journal.ActorID, boot journal.JournalID, opID string) journal.OperationInput {
	return journal.OperationInput{
		OperationID:        journal.OperationID(opID),
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte(opID + "-c"),
		MutationDigest:     []byte(opID + "-m"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort:     journal.EffectTaskCreate,
			TaskID:   ptypes.TaskID{Namespace: "verified", UUID: uuid.Must(uuid.NewV7())},
			Title:    "delta " + opID,
			Type:     ptypes.TaskTypeTask,
			Priority: ptypes.PriorityMedium,
			Phase:    ptypes.PhaseWorkerSlices,
		}},
	}
}

func ledgerSnapshot(t *testing.T, db *DB) (verifiedLedger, verifiedStoreIdentity, bool) {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	ledger, ok := scope.readVerifiedLedger()
	identity, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		t.Fatalf("read store identity: %v", err)
	}
	return ledger, identity, ok
}

// TestVerifyJournalDeltaChecksOnlyTheNewRows pins the induction step used when
// the verified-as-of ledger matches: a valid appended operation passes, while a
// bare new journal row and a subordinate row carrying an actor are rejected.
func TestVerifyJournalDeltaChecksOnlyTheNewRows(t *testing.T) {
	db := openPoolFileDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)

	scope := takePoolScope(t, db)
	before, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("before max journal id: %v", err)
	}
	scope.release()

	appendEventsOp(t, db, boot, actor, task, "op-delta-valid", []opEvent{{
		kind:    journal.EventKind("provenance.task.created"),
		payload: []byte(`{"valid":true}`),
	}})

	scope = takePoolScope(t, db)
	after, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("after max journal id: %v", err)
	}
	if err := scope.verifyJournalDelta(before, after); err != nil {
		t.Fatalf("valid new rows were rejected: %v", err)
	}
	scope.release()

	scope = takePoolScope(t, db)
	beforeBare, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("before bare max journal id: %v", err)
	}
	scope.release()
	if _, err := db.AppendBareJournalRow(journal.JournalKindDecision, actor, time.Now()); err != nil {
		t.Fatalf("append bare journal row: %v", err)
	}
	scope = takePoolScope(t, db)
	afterBare, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("after bare max journal id: %v", err)
	}
	if err := scope.verifyJournalDelta(beforeBare, afterBare); !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("bare new row error = %v, want ErrSubtypeIntegrity", err)
	}
	scope.release()

	scope = takePoolScope(t, db)
	beforeActor, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("before actor max journal id: %v", err)
	}
	scope.release()
	if _, err := db.AdversarialSubordinateRowCarryingActor(actor, task); err != nil {
		t.Fatalf("seed subordinate actor violation: %v", err)
	}
	scope = takePoolScope(t, db)
	afterActor, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("after actor max journal id: %v", err)
	}
	if err := scope.verifyJournalDelta(beforeActor, afterActor); !errors.Is(err, journal.ErrActorPlacement) {
		t.Fatalf("subordinate actor error = %v, want ErrActorPlacement", err)
	}
	scope.release()
}

// TestVerifyJournalDeltaRejectsCrossSubtypeAndAuthorityDetail pins the two
// remaining induction failures: a new journal id in two subtype tables, and an
// authority whose detail row disagrees with its kind.
func TestVerifyJournalDeltaRejectsCrossSubtypeAndAuthorityDetail(t *testing.T) {
	db := openPoolFileDB(t)
	actor, task := seedActorAndTask(t, db)

	scope := takePoolScope(t, db)
	beforeCross, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("before cross-subtype tail: %v", err)
	}
	scope.release()
	if _, err := db.AdversarialJournalRowTwoSubtypes(actor); err != nil {
		t.Fatalf("seed cross-subtype row: %v", err)
	}
	scope = takePoolScope(t, db)
	afterCross, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("after cross-subtype tail: %v", err)
	}
	if err := scope.verifyJournalDelta(beforeCross, afterCross); !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("cross-subtype error = %v, want ErrSubtypeIntegrity", err)
	}
	scope.release()

	scope = takePoolScope(t, db)
	beforeAuth, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("before authority tail: %v", err)
	}
	scope.release()
	if _, err := db.AdversarialAuthorityDetailMismatch(actor, task); err != nil {
		t.Fatalf("seed authority detail mismatch: %v", err)
	}
	scope = takePoolScope(t, db)
	afterAuth, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("after authority tail: %v", err)
	}
	if err := scope.verifyJournalDelta(beforeAuth, afterAuth); !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("authority detail error = %v, want ErrSubtypeIntegrity", err)
	}
	scope.release()
}

// TestVerifyJournalDeltaRejectsAnchorMissingActor pins the other half of the
// actor-placement comparison: an anchor with no stored actor.
func TestVerifyJournalDeltaRejectsAnchorMissingActor(t *testing.T) {
	db := openPoolFileDB(t)
	scope := takePoolScope(t, db)
	defer scope.release()
	before, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("before missing-actor tail: %v", err)
	}
	restore, err := scope.suppressCheckConstraints("delta missing actor")
	if err != nil {
		t.Fatalf("suppress CHECK: %v", err)
	}
	defer func() {
		if err := restore(nil); err != nil {
			t.Errorf("restore CHECK: %v", err)
		}
	}()
	if _, err := scope.conn.ExecContext(scope.ctx,
		"INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id) VALUES (?1, NULL, ?2, NULL)",
		int(journal.JournalKindDecision), time.Now().UnixNano()); err != nil {
		t.Fatalf("insert anchor missing actor: %v", err)
	}
	after, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("after missing-actor tail: %v", err)
	}
	if err := scope.verifyJournalDelta(before, after); !errors.Is(err, journal.ErrActorPlacement) {
		t.Fatalf("missing-actor error = %v, want ErrActorPlacement", err)
	}
}

// TestVerifyJournalDeltaIgnoresRowsOutsideTheRange proves the induction step
// does not scan the journal prefix. A cross-subtype row at or below `after` is
// invisible to the delta and still rejected by the full subtype gate.
func TestVerifyJournalDeltaIgnoresRowsOutsideTheRange(t *testing.T) {
	db := openPoolFileDB(t)
	actor, _ := seedActorAndTask(t, db)
	if _, err := db.AdversarialJournalRowTwoSubtypes(actor); err != nil {
		t.Fatalf("seed cross-subtype prefix: %v", err)
	}
	scope := takePoolScope(t, db)
	defer scope.release()
	badTail, err := scope.maxJournalID()
	if err != nil {
		t.Fatalf("bad tail: %v", err)
	}
	if err := scope.verifyJournalDelta(badTail, badTail); err != nil {
		t.Fatalf("empty range scanned the journal: %v", err)
	}
	var goodTail int64
	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		jid, err := scope.insertJournalRow(journal.JournalKindDecision, actor, time.Now().UnixNano(), nil)
		if err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx,
			"INSERT INTO journal_decisions (journal_id, decision_kind, task_id, payload) VALUES (?1, ?2, NULL, ?3)",
			jid, "pasture.review.vote", "{}"); err != nil {
			return err
		}
		goodTail = jid
		return nil
	}); err != nil {
		t.Fatalf("append a valid decision after the bad prefix: %v", err)
	}
	if err := scope.verifyJournalDelta(badTail, goodTail); err != nil {
		t.Fatalf("delta rejected a valid suffix, so it scanned the prefix: %v", err)
	}
	if err := scope.verifyJournalDelta(badTail-1, goodTail); !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("delta including the bad row = %v, want ErrSubtypeIntegrity", err)
	}
	if err := scope.verifySubtypeIntegrity(); !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("full subtype gate = %v, want ErrSubtypeIntegrity; the full path must still see the prefix", err)
	}
}

// TestEligibleApplyUsesOneDeltaAndAdvances proves a current ledger makes one
// Apply run the delta gate only, then advance in the same commit. A replay
// writes nothing, so it runs no gate and does not move the ledger.
func TestEligibleApplyUsesOneDeltaAndAdvances(t *testing.T) {
	db, actor, boot, seeded := openCurrentLedger(t)
	gates := &gateTally{}
	input := deltaTaskInput(actor, boot, "op-delta-eligible")
	if _, err := db.ApplyContext(gates.context(), input); err != nil {
		t.Fatalf("eligible Apply: %v", err)
	}
	assertGateCounts(t, gates, 1, 0, 0)
	ledger, identity, present := ledgerSnapshot(t, db)
	if !present || ledger.identity != identity || ledger.shapeID != verifiedShapeID() {
		t.Fatalf("eligible Apply did not advance the ledger to the post-write identity (present=%v ledger=%+v store=%+v shape=%q)", present, ledger.identity, identity, ledger.shapeID)
	}
	if ledger.count != seeded {
		t.Fatalf("verified_count = %d, want %d; a delta advance must not count as a whole-store verification", ledger.count, seeded)
	}

	replay := &gateTally{}
	res, err := db.ApplyContext(replay.context(), input)
	if err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	if !res.ShortCircuited {
		t.Fatal("exact replay was not short-circuited")
	}
	assertGateCounts(t, replay, 0, 0, 0)
	after, _, _ := ledgerSnapshot(t, db)
	if after.identity != ledger.identity || after.count != seeded {
		t.Fatalf("short-circuit moved the ledger from %+v/%d to %+v/%d", ledger.identity, seeded, after.identity, after.count)
	}
}

// TestEligibleBatchUsesOneDeltaOverTheAppendedRange proves a batch of several
// records still runs one delta check, not one full scan and not one check per
// record.
func TestEligibleBatchUsesOneDeltaOverTheAppendedRange(t *testing.T) {
	db, actor, boot, seeded := openCurrentLedger(t)
	gates := &gateTally{}
	inputs := []journal.OperationInput{
		deltaTaskInput(actor, boot, "op-delta-batch-a"),
		deltaTaskInput(actor, boot, "op-delta-batch-b"),
		batchDecisionInput("op-delta-batch-decision", actor, boot, `{}`),
	}
	result, err := db.ApplyBatchContext(gates.context(), inputs)
	if err != nil || !result.Committed {
		t.Fatalf("eligible batch: committed=%v err=%v", result.Committed, err)
	}
	assertGateCounts(t, gates, 1, 0, 0)
	ledger, identity, present := ledgerSnapshot(t, db)
	if !present || ledger.identity != identity || ledger.count != seeded {
		t.Fatalf("batch ledger present=%v count=%d want %d; ledger=%+v store=%+v", present, ledger.count, seeded, ledger.identity, identity)
	}
}

// TestEligibleApplyRollsBackADeltaViolation proves the delta gate's error is
// what rejects the write: a trigger adds a second subtype row during the fold,
// the delta rejects it, and the transaction leaves no new row behind.
func TestEligibleApplyRollsBackADeltaViolation(t *testing.T) {
	db, actor, boot, seeded := openCurrentLedger(t)
	scope := takePoolScope(t, db)
	if _, err := scope.conn.ExecContext(scope.ctx, `
		CREATE TRIGGER delta_test_cross_subtype AFTER INSERT ON journal_decisions
		BEGIN
			INSERT INTO journal_evidence (journal_id, evidence_kind, task_id, content_digest, payload)
			VALUES (NEW.journal_id, 'pasture.git.commit', NULL, x'78', '{}');
		END`); err != nil {
		scope.release()
		t.Fatalf("install cross-subtype trigger: %v", err)
	}
	scope.release()

	before, _, _ := ledgerSnapshot(t, db)
	gates := &gateTally{}
	_, err := db.ApplyContext(gates.context(), batchDecisionInput("op-delta-cross", actor, boot, `{}`))
	if !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("cross-subtype Apply error = %v, want ErrSubtypeIntegrity", err)
	}
	assertGateCounts(t, gates, 1, 0, 0)
	after, identity, present := ledgerSnapshot(t, db)
	if !present || after.identity != before.identity || after.count != seeded {
		t.Fatalf("rejected delta moved the ledger from %+v to %+v (store %+v, count %d)", before.identity, after.identity, identity, after.count)
	}
	if after.identity != identity {
		t.Fatalf("rejected delta committed store identity %+v ahead of ledger %+v", identity, after.identity)
	}
	if err := db.VerifyIntegrity(); err != nil {
		t.Fatalf("VerifyIntegrity after rollback: %v", err)
	}
}

// TestIneligibleWriteUsesFullPathOnce proves an absent, foreign, or
// out-of-band-stale ledger runs the full subtype and actor gates once and does
// not advance. A pre-existing bare row is rejected by that full path; a delta
// check would have ignored it and committed.
func TestIneligibleWriteUsesFullPathOnce(t *testing.T) {
	t.Run("out-of-band edit", func(t *testing.T) {
		db, actor, boot, seeded := openCurrentLedger(t)
		scope := takePoolScope(t, db)
		if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE tasks SET title = title"); err != nil {
			scope.release()
			t.Fatalf("out-of-band UPDATE: %v", err)
		}
		scope.release()
		before, _, _ := ledgerSnapshot(t, db)
		gates := &gateTally{}
		if _, err := db.ApplyContext(gates.context(), deltaTaskInput(actor, boot, "op-delta-stale")); err != nil {
			t.Fatalf("stale-ledger Apply: %v", err)
		}
		assertGateCounts(t, gates, 0, 1, 1)
		after, identity, present := ledgerSnapshot(t, db)
		if !present || after.identity != before.identity || after.count != seeded {
			t.Fatalf("stale write advanced the ledger from %+v/%d to %+v/%d", before.identity, seeded, after.identity, after.count)
		}
		if after.identity == identity {
			t.Fatalf("stale write left the ledger matching the store %+v; the full path must not advance", identity)
		}
	})

	t.Run("absent ledger", func(t *testing.T) {
		db, actor, boot, _ := openCurrentLedger(t)
		scope := takePoolScope(t, db)
		if _, err := scope.conn.ExecContext(scope.ctx, "DELETE FROM "+verifiedStoreTable); err != nil {
			scope.release()
			t.Fatalf("delete ledger: %v", err)
		}
		scope.release()
		gates := &gateTally{}
		if _, err := db.ApplyContext(gates.context(), deltaTaskInput(actor, boot, "op-delta-absent")); err != nil {
			t.Fatalf("absent-ledger Apply: %v", err)
		}
		assertGateCounts(t, gates, 0, 1, 1)
		if _, _, present := ledgerSnapshot(t, db); present {
			t.Fatal("absent ledger was inserted by the write; only the full open/audit path may create it")
		}
	})

	t.Run("foreign shape", func(t *testing.T) {
		db, actor, boot, seeded := openCurrentLedger(t)
		scope := takePoolScope(t, db)
		if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE "+verifiedStoreTable+" SET shape_id=?1 WHERE id=1", "foreign-shape"); err != nil {
			scope.release()
			t.Fatalf("forge shape: %v", err)
		}
		scope.release()
		gates := &gateTally{}
		if _, err := db.ApplyContext(gates.context(), deltaTaskInput(actor, boot, "op-delta-foreign")); err != nil {
			t.Fatalf("foreign-shape Apply: %v", err)
		}
		assertGateCounts(t, gates, 0, 1, 1)
		ledger, _, present := ledgerSnapshot(t, db)
		if !present || ledger.shapeID != "foreign-shape" || ledger.count != seeded {
			t.Fatalf("foreign shape after write = present %v shape %q count %d, want foreign-shape/%d", present, ledger.shapeID, ledger.count, seeded)
		}
	})

	t.Run("bare prefix is rejected by the full path", func(t *testing.T) {
		db, actor, boot, _ := openCurrentLedger(t)
		if _, err := db.AppendBareJournalRow(journal.JournalKindDecision, actor, time.Now()); err != nil {
			t.Fatalf("append bare prefix: %v", err)
		}
		gates := &gateTally{}
		_, err := db.ApplyContext(gates.context(), deltaTaskInput(actor, boot, "op-delta-bare-prefix"))
		if !errors.Is(err, journal.ErrSubtypeIntegrity) {
			t.Fatalf("Apply over a bare prefix = %v, want ErrSubtypeIntegrity from the full path", err)
		}
		assertGateCounts(t, gates, 0, 1, 0)
		if err := db.VerifyIntegrity(); !errors.Is(err, journal.ErrSubtypeIntegrity) {
			t.Fatalf("VerifyIntegrity after the rejected write = %v, want the bare prefix still present", err)
		}
	})

	t.Run("ephemeral store", func(t *testing.T) {
		db := newJournalDB(t)
		actor, task := seedActorAndTask(t, db)
		boot := genesisBoot(t, db, actor)
		gates := &gateTally{}
		if _, err := db.ApplyContext(gates.context(), journal.OperationInput{
			OperationID:        "op-delta-ephemeral",
			ActorID:            actor,
			AuthorityJournalID: &boot,
			CommandDigest:      []byte("ephemeral-c"),
			MutationDigest:     []byte("ephemeral-m"),
			RecordedAt:         time.Now().UTC().UnixNano(),
			Effects: []journal.Effect{{
				Sort: journal.EffectTaskEvent, TaskID: task,
				EventKind: journal.EventKind("provenance.task.created"),
				Payload:   []byte(`{"ephemeral":true}`),
			}},
		}); err != nil {
			t.Fatalf("ephemeral Apply: %v", err)
		}
		assertGateCounts(t, gates, 0, 1, 1)
	})

	t.Run("stale batch runs the full gates once", func(t *testing.T) {
		db, actor, boot, _ := openCurrentLedger(t)
		scope := takePoolScope(t, db)
		if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE tasks SET title = title"); err != nil {
			scope.release()
			t.Fatalf("out-of-band UPDATE: %v", err)
		}
		scope.release()
		gates := &gateTally{}
		result, err := db.ApplyBatchContext(gates.context(), []journal.OperationInput{
			deltaTaskInput(actor, boot, "op-delta-stale-batch-a"),
			deltaTaskInput(actor, boot, "op-delta-stale-batch-b"),
		})
		if err != nil || !result.Committed {
			t.Fatalf("stale batch: committed=%v err=%v", result.Committed, err)
		}
		assertGateCounts(t, gates, 0, 1, 1)
	})
}
