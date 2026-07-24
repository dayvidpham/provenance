package provenance

// journal_activity_condition_test.go tests EffectActivityCreate fold through
// the real Apply production path: journaled Activity birth, ActivityID
// collision detection, exact replay, and rollback.

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// newActivityID mints a fresh namespaced ActivityID for a create effect.
func newActivityID() ActivityID {
	return ActivityID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}
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
	env := newOpsEnv(t)
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
}

// TestActivityCreate_ForeignOperationCollisionRollsBack verifies that an
// ActivityCreate with an ActivityID already committed by a different operation
// returns typed ActivityConflict and rolls back the whole operation.
func TestActivityCreate_ForeignOperationCollisionRollsBack(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
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

	// Count journal rows before the collision attempt.
	// (We can count via LookupCommitted to detect new rows.)

	// Second operation uses the SAME ActivityID (foreign collision).
	opB := OperationID("activity-collision-B")
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

	// opB must not be persisted.
	result, _ := env.tr.Journal().LookupCommitted(opB)
	if result.Kind != CommittedAbsent {
		t.Fatalf("ActivityID collision: opB was persisted: %+v", result)
	}
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
