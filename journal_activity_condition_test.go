package provenance

// journal_activity_condition_test.go tests EffectActivityCreate fold through
// the real Apply production path: journaled Activity birth, ActivityID
// collision detection, exact replay, and rollback.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// newActivityID mints a fresh namespaced ActivityID for a create effect.
func newActivityID() ActivityID {
	return ActivityID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}
}

type activityStorageCounts struct {
	journal     int64
	operations  int64
	resultSlots int64
	activities  int64
	births      int64
}

func activityCounts(t *testing.T, path string) activityStorageCounts {
	t.Helper()
	var counts activityStorageCounts
	queries := []struct {
		sql   string
		count *int64
	}{
		{`SELECT count(*) FROM main.journal`, &counts.journal},
		{`SELECT count(*) FROM main.journal_operations`, &counts.operations},
		{`SELECT count(*) FROM main.journal_operation_result_slots`, &counts.resultSlots},
		{`SELECT count(*) FROM main.activities`, &counts.activities},
		{`SELECT count(*) FROM main.journal_activity_creations`, &counts.births},
	}
	withRawSQLiteTestConn(t, path, func(conn *sqlite.Conn) {
		for _, query := range queries {
			if err := sqlitex.Execute(conn, query.sql, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
				*query.count = stmt.ColumnInt64(0)
				return nil
			}}); err != nil {
				t.Fatalf("count ActivityCreate storage for %q: %v", query.sql, err)
			}
		}
	})
	return counts
}

func requireActivityCountsUnchanged(t *testing.T, before activityStorageCounts, path string) {
	t.Helper()
	if after := activityCounts(t, path); after != before {
		t.Fatalf("ActivityCreate failure changed durable rows: before=%+v after=%+v", before, after)
	}
}

func newFileActivityOpsEnv(t *testing.T) (*opsEnv, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "activity-fault.sqlite")
	tr, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("open file-backed ActivityCreate test database: %v", err)
	}
	// Fault-fixture cleanup is registered by the caller after this cleanup, so
	// LIFO test cleanup restores the main schema before checking Close.
	t.Cleanup(func() {
		if err := tr.Close(); err != nil {
			t.Errorf("close file-backed ActivityCreate test database after fixture cleanup: %v", err)
		}
	})
	agent, err := tr.RegisterSoftwareAgent("provenance-test", "activity-fault", "0", "test")
	if err != nil {
		t.Fatalf("register file-backed ActivityCreate test actor: %v", err)
	}
	return &opsEnv{
		journalEnv: &journalEnv{tr: tr, actor: agent.ID},
		actors:     map[string]ActorID{},
		tasks:      map[string]TaskID{},
	}, path
}

// TestActivityCreate_JournalsBirth verifies that EffectActivityCreate commits
// a journal_activity_creations row and the ActivityID is returned in the result slot.
func TestActivityCreate_JournalsBirth(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis-act")

	actID := newActivityID()
	agentID := env.actor

	opID := OperationID("activity-create-birth")
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        opID,
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("birth"),
		Effects: []Effect{{
			Sort:            EffectActivityCreate,
			ResultSlot:      "activity",
			ActivityID:      actID,
			ActivityAgentID: AgentID(agentID),
			ActivityPhase:   PhaseWorkerSlices,
			ActivityStage:   StageInProgress,
			ActivityNotes:   "test birth",
		}},
	})
	if err != nil {
		t.Fatalf("Apply ActivityCreate: %v", err)
	}
	if res.Kind != CommittedExact {
		t.Fatalf("Apply ActivityCreate: kind=%v", res.Kind)
	}

	// Result slot must carry the ActivityID.
	var actSlot *ResultSlotBinding
	for i := range res.ResultSlots {
		if res.ResultSlots[i].Slot == "activity" {
			actSlot = &res.ResultSlots[i]
		}
	}
	if actSlot == nil {
		t.Fatalf("Apply ActivityCreate: no 'activity' result slot in %+v", res.ResultSlots)
	}
	if actSlot.Kind != JournalKindActivity {
		t.Fatalf("Apply ActivityCreate: slot kind=%v, want JournalKindActivity", actSlot.Kind)
	}
	if actSlot.ActivityID == nil || *actSlot.ActivityID != actID {
		t.Fatalf("Apply ActivityCreate: slot ActivityID=%v, want %v", actSlot.ActivityID, actID)
	}
}

// TestActivityCreate_ExactReplay verifies that replaying an ActivityCreate
// operation returns the original result short-circuited.
func TestActivityCreate_ExactReplay(t *testing.T) {
	t.Parallel()
	env, path := newFileActivityOpsEnv(t)
	boot := env.genesis(t, "op-genesis-replay")

	actID := newActivityID()
	opID := OperationID("activity-create-replay")
	in := OperationInput{
		OperationID:        opID,
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("replay"),
		Effects: []Effect{{
			Sort:            EffectActivityCreate,
			ResultSlot:      "activity",
			ActivityID:      actID,
			ActivityAgentID: AgentID(env.actor),
			ActivityPhase:   PhaseWorkerSlices,
			ActivityStage:   StageInProgress,
		}},
	}
	res1, err := env.tr.Journal().Apply(in)
	if err != nil || res1.Kind != CommittedExact {
		t.Fatalf("first Apply: err=%v kind=%v", err, res1.Kind)
	}

	// Exact replay must return the same result.
	before := activityCounts(t, path)
	res2, err2 := env.tr.Journal().Apply(in)
	if err2 != nil || res2.Kind != CommittedExact {
		t.Fatalf("replay Apply: err=%v kind=%v", err2, res2.Kind)
	}
	if !res2.ShortCircuited {
		t.Fatal("replay Apply: ShortCircuited must be true")
	}
	if res2.AnchorJournalID != res1.AnchorJournalID {
		t.Fatalf("replay Apply: AnchorJournalID mismatch: first=%d replay=%d", res1.AnchorJournalID, res2.AnchorJournalID)
	}
	// ActivityID in result slot must match.
	if len(res2.ResultSlots) == 0 || res2.ResultSlots[0].ActivityID == nil || *res2.ResultSlots[0].ActivityID != actID {
		t.Fatalf("replay Apply: result slot mismatch: %+v", res2.ResultSlots)
	}
	requireActivityCountsUnchanged(t, before, path)
}

// TestActivityCreate_ForeignOperationCollisionRollsBack verifies that an
// ActivityCreate with an ActivityID already committed by a different operation
// returns typed ActivityConflict and rolls back the whole operation.
func TestActivityCreate_ForeignOperationCollisionRollsBack(t *testing.T) {
	t.Parallel()
	env, path := newFileActivityOpsEnv(t)
	boot := env.genesis(t, "op-genesis-collision")

	actID := newActivityID()

	// First operation commits the ActivityID.
	opA := OperationID("activity-collision-A")
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        opA,
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("opA"),
		Effects: []Effect{{
			Sort: EffectActivityCreate, ResultSlot: "activity",
			ActivityID: actID, ActivityAgentID: AgentID(env.actor),
			ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress,
		}},
	}); err != nil {
		t.Fatalf("first Apply (opA): %v", err)
	}

	// Second operation uses the SAME ActivityID (foreign collision).
	opB := OperationID("activity-collision-B")
	before := activityCounts(t, path)
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        opB,
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("opB"),
		Effects: []Effect{{
			Sort: EffectActivityCreate, ResultSlot: "activity",
			ActivityID: actID, ActivityAgentID: AgentID(env.actor),
			ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress,
		}},
	})
	if err == nil {
		t.Fatal("ActivityID collision: Apply should fail")
	}
	if !errors.Is(err, ErrActivityConflict) {
		t.Fatalf("ActivityID collision: expected ErrActivityConflict, got: %v", err)
	}
	var ac *ActivityConflict
	if !errors.As(err, &ac) {
		t.Fatalf("ActivityID collision: error does not wrap *ActivityConflict: %v", err)
	}
	if ac.ActivityID != actID {
		t.Fatalf("ActivityID collision: ActivityConflict.ActivityID=%v, want %v", ac.ActivityID, actID)
	}
	if ac.ExistingJournalID <= 0 {
		t.Fatalf("ActivityID collision: ActivityConflict.ExistingJournalID=%d, want >0", ac.ExistingJournalID)
	}
	requireActivityCountsUnchanged(t, before, path)

	// opB must not be persisted.
	result, _ := env.tr.Journal().LookupCommitted(opB)
	if result.Kind != CommittedAbsent {
		t.Fatalf("ActivityID collision: opB was persisted: %+v", result)
	}
}

func TestActivityCreate_NonJournaledCollisionRejectsMatchingAndDifferingAttribution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seed func(*testing.T, *opsEnv, ActivityID)
	}{
		{
			name: "matching",
			seed: func(t *testing.T, env *opsEnv, id ActivityID) {
				if _, err := env.tr.StartActivityWithID(id, env.actor, PhaseWorkerSlices, StageInProgress, "incoming notes"); err != nil {
					t.Fatalf("seed matching activity: %v", err)
				}
			},
		},
		{
			name: "differing agent phase stage and notes",
			seed: func(t *testing.T, env *opsEnv, id ActivityID) {
				other := env.actorFor(t, "activity-collision-other")
				if _, err := env.tr.StartActivityWithID(id, other, PhaseCodeReview, StageComplete, "different notes"); err != nil {
					t.Fatalf("seed differing activity: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			env, path := newFileActivityOpsEnv(t)
			boot := env.genesis(t, "op-genesis-non-journaled-collision")
			actID := newActivityID()
			test.seed(t, env, actID)
			before := activityCounts(t, path)

			_, err := env.tr.Journal().Apply(OperationInput{
				OperationID:        OperationID("activity-non-journaled-collision-" + test.name),
				ActorID:            env.actor,
				AuthorityJournalID: &boot,
				CommandDigest:      env.digest(test.name),
				Effects: []Effect{{
					Sort: EffectActivityCreate, ResultSlot: "activity",
					ActivityID: actID, ActivityAgentID: AgentID(env.actor),
					ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress,
					ActivityNotes: "incoming notes",
				}},
			})
			var conflict *ActivityConflict
			if !errors.Is(err, ErrActivityConflict) || !errors.As(err, &conflict) {
				t.Fatalf("non-journaled collision: err=%v, want *ActivityConflict", err)
			}
			if conflict.ActivityID != actID || conflict.ExistingJournalID != 0 {
				t.Fatalf("non-journaled collision: conflict=%+v, want ActivityID=%v ExistingJournalID=0", conflict, actID)
			}
			requireActivityCountsUnchanged(t, before, path)
		})
	}
}

func TestActivityCreate_ChangedPhaseOrStageConflictsWithoutWrites(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Effect)
	}{
		{"phase", func(effect *Effect) { effect.ActivityPhase = PhaseCodeReview }},
		{"stage", func(effect *Effect) { effect.ActivityStage = StageComplete }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			env, path := newFileActivityOpsEnv(t)
			boot := env.genesis(t, "op-genesis-activity-field-conflict")
			in := OperationInput{
				OperationID:        OperationID("activity-field-conflict-" + test.name),
				ActorID:            env.actor,
				AuthorityJournalID: &boot,
				CommandDigest:      env.digest(test.name),
				Effects: []Effect{{
					Sort: EffectActivityCreate, ResultSlot: "activity",
					ActivityID: newActivityID(), ActivityAgentID: AgentID(env.actor),
					ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress,
				}},
			}
			if result, err := env.tr.Journal().Apply(in); err != nil || result.Kind != CommittedExact {
				t.Fatalf("initial Apply: result=%+v err=%v", result, err)
			}
			before := activityCounts(t, path)
			changed := in
			changed.Effects = append([]Effect(nil), in.Effects...)
			test.mutate(&changed.Effects[0])

			result, err := env.tr.Journal().Apply(changed)
			var conflict *OperationConflict
			if !errors.Is(err, ErrOperationConflict) || !errors.As(err, &conflict) {
				t.Fatalf("changed %s: err=%v, want *OperationConflict", test.name, err)
			}
			if result.Kind != CommittedConflict || conflict.Axis != ConflictEffect || conflict.Index != 0 {
				t.Fatalf("changed %s: result=%+v conflict=%+v, want ConflictEffect index 0", test.name, result, conflict)
			}
			requireActivityCountsUnchanged(t, before, path)
		})
	}
}

func TestActivityCreate_BirthMappingFailureRollsBackActivity(t *testing.T) {
	t.Parallel()
	env, path := newFileActivityOpsEnv(t)
	boot := env.genesis(t, "op-genesis-birth-mapping-rollback")
	withRawSQLiteTestConn(t, path, func(conn *sqlite.Conn) {
		if err := sqlitex.ExecuteTransient(conn, `CREATE TRIGGER main.reject_activity_birth BEFORE INSERT ON journal_activity_creations BEGIN SELECT RAISE(ABORT, 'forced birth mapping failure'); END`, nil); err != nil {
			t.Fatalf("create birth-mapping failure trigger: %v", err)
		}
	})
	t.Cleanup(func() {
		withRawSQLiteTestConn(t, path, func(conn *sqlite.Conn) {
			if err := sqlitex.ExecuteTransient(conn, `DROP TRIGGER main.reject_activity_birth`, nil); err != nil {
				t.Errorf("drop birth-mapping failure trigger before tracker Close: %v", err)
				return
			}
			var remaining int64
			if err := sqlitex.Execute(conn, `SELECT count(*) FROM main.sqlite_schema WHERE type = 'trigger' AND name = 'reject_activity_birth'`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
				remaining = stmt.ColumnInt64(0)
				return nil
			}}); err != nil {
				t.Errorf("verify birth-mapping failure trigger cleanup before tracker Close: %v", err)
			} else if remaining != 0 {
				t.Errorf("verify birth-mapping failure trigger cleanup before tracker Close: remaining=%d, want 0", remaining)
			}
		})
	})
	before := activityCounts(t, path)

	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        "activity-birth-mapping-rollback",
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("birth-mapping-rollback"),
		Effects: []Effect{{
			Sort: EffectActivityCreate, ResultSlot: "activity",
			ActivityID: newActivityID(), ActivityAgentID: AgentID(env.actor),
			ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress,
		}},
	})
	if err == nil || errors.Is(err, ErrActivityConflict) {
		t.Fatalf("forced birth-mapping failure: err=%v, want non-conflict error", err)
	}
	requireActivityCountsUnchanged(t, before, path)
}

func TestActivityCreate_CollisionAttributionLookupErrorPropagates(t *testing.T) {
	t.Parallel()
	env, path := newFileActivityOpsEnv(t)
	boot := env.genesis(t, "op-genesis-collision-attribution-error")
	actID := newActivityID()
	if _, err := env.tr.StartActivityWithID(actID, env.actor, PhaseWorkerSlices, StageInProgress, "non-journaled"); err != nil {
		t.Fatalf("seed non-journaled activity: %v", err)
	}
	withRawSQLiteTestConn(t, path, func(conn *sqlite.Conn) {
		if err := sqlitex.ExecuteTransient(conn, `ALTER TABLE main.journal_activity_creations RENAME COLUMN journal_id TO invalid_test_journal_id`, nil); err != nil {
			t.Fatalf("rename attribution column for lookup failure: %v", err)
		}
	})
	t.Cleanup(func() {
		withRawSQLiteTestConn(t, path, func(conn *sqlite.Conn) {
			if err := sqlitex.ExecuteTransient(conn, `ALTER TABLE main.journal_activity_creations RENAME COLUMN invalid_test_journal_id TO journal_id`, nil); err != nil {
				t.Errorf("restore attribution column before tracker Close: %v", err)
				return
			}
			if err := sqlitex.ExecuteTransient(conn, `SELECT journal_id FROM main.journal_activity_creations LIMIT 0`, nil); err != nil {
				t.Errorf("verify attribution column restoration before tracker Close: %v", err)
			}
		})
	})
	before := activityCounts(t, path)

	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        "activity-collision-attribution-error",
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("collision-attribution-error"),
		Effects: []Effect{{
			Sort: EffectActivityCreate, ResultSlot: "activity",
			ActivityID: actID, ActivityAgentID: AgentID(env.actor),
			ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress,
		}},
	})
	if err == nil || errors.Is(err, ErrActivityConflict) || !strings.Contains(err.Error(), "attribute ActivityID collision") {
		t.Fatalf("collision attribution lookup: err=%v, want propagated lookup error", err)
	}
	requireActivityCountsUnchanged(t, before, path)
}

// TestActivityCreate_ReopenReconstructsActivitySlot verifies that after
// reopening a file-backed database, LookupCommitted reconstructs the
// ActivityID from the result slot correctly.
func TestActivityCreate_ReopenReconstructsActivitySlot(t *testing.T) {
	t.Parallel()
	dbPath := t.TempDir() + "/activity_reopen.db"

	openTracker := func() Tracker {
		tr, err := OpenSQLite(dbPath)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close() })
		return tr
	}

	tr := openTracker()
	actID := newActivityID()
	actor, err := tr.RegisterSoftwareAgent("provenance-test", "agent", "0", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	bootRes, err := tr.Journal().Apply(OperationInput{
		OperationID:   "reopen-genesis",
		ActorID:       actor.Agent.ID,
		CommandDigest: []byte("genesis"),
		Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "auth",
			OperationAuthorityID: OperationAuthorityID(actor.Agent.ID.String())}},
	})
	if err != nil || bootRes.Kind != CommittedExact {
		t.Fatalf("genesis: %v %v", err, bootRes.Kind)
	}
	// boot must be the AUTHORITY row JID (produced by EffectBootstrapAuthority),
	// not the operation anchor JID. The authority slot holds the authority's JID.
	var boot JournalID
	for _, s := range bootRes.ResultSlots {
		if s.Slot == "auth" {
			boot = s.ProducedJournalID
		}
	}
	if boot == 0 {
		t.Fatalf("genesis: no 'auth' result slot")
	}

	opID := OperationID("reopen-activity-create")
	in := OperationInput{
		OperationID:        opID,
		ActorID:            actor.Agent.ID,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("act-create"),
		Effects: []Effect{{
			Sort: EffectActivityCreate, ResultSlot: "activity",
			ActivityID: actID, ActivityAgentID: actor.Agent.ID,
			ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress,
		}},
	}
	res1, err := tr.Journal().Apply(in)
	if err != nil || res1.Kind != CommittedExact {
		t.Fatalf("Apply before close: err=%v kind=%v", err, res1.Kind)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen and verify LookupCommitted reconstructs the ActivityID.
	tr2 := openTracker()
	res2, err2 := tr2.Journal().LookupCommitted(opID)
	if err2 != nil || res2.Kind != CommittedExact {
		t.Fatalf("LookupCommitted after reopen: err=%v kind=%v", err2, res2.Kind)
	}
	var found bool
	for _, slot := range res2.ResultSlots {
		if slot.Slot == "activity" && slot.Kind == JournalKindActivity && slot.ActivityID != nil && *slot.ActivityID == actID {
			found = true
		}
	}
	if !found {
		t.Fatalf("LookupCommitted after reopen: activity slot not found or ActivityID wrong: %+v", res2.ResultSlots)
	}
}
