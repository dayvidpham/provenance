package provenance

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"

	_ "modernc.org/sqlite"
)

type recoveryParityStack struct {
	db      *sql.DB
	root    dbos.DBOSContext
	tracker Tracker
	adapter *DBOSAdapter
	entries atomic.Int64
}

func openRecoveryParityStack(t *testing.T, path string, register bool, actor ActorID, appName string) *recoveryParityStack {
	t.Helper()
	db, err := openSharedSQL(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := dbos.NewDBOSContext(context.Background(), dbos.Config{AppName: appName, SqliteSystemDB: db, ApplicationVersion: "recovery-parity"})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	tracker, err := OpenBorrowedSQLite(db)
	if err != nil {
		root.Shutdown(5 * time.Second)
		_ = db.Close()
		t.Fatal(err)
	}
	if register {
		agent, err := tracker.RegisterSoftwareAgent("recovery", "actor", "1", "test")
		if err != nil {
			_ = tracker.Close()
			_ = db.Close()
			t.Fatal(err)
		}
		actor = agent.ID
	}
	adapter, err := NewDBOSAdapter(root, tracker, DBOSAdapterConfig{})
	if err != nil {
		_ = tracker.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	stack := &recoveryParityStack{db: db, root: root, tracker: tracker, adapter: adapter}
	adapter.testHooks.onWorkflowEntry = func() { stack.entries.Add(1) }
	if err := dbos.Launch(root); err != nil {
		stack.close()
		t.Fatal(err)
	}
	return stack
}

func (s *recoveryParityStack) close() {
	if s.root != nil {
		s.root.Shutdown(5 * time.Second)
		s.root = nil
	}
	if s.tracker != nil {
		_ = s.tracker.Close()
		s.tracker = nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

func recoveryParityActivity(id string) ActivityID {
	return ActivityID{Namespace: "recovery", UUID: uuid.MustParse(id)}
}

func TestDBOSRecoveredConditionAndActivityParity(t *testing.T) {
	path := t.TempDir() + "/recovery.db"
	const appName = "dbos-recovery-parity"

	// Seed the shared journal before DBOS starts. These rows provide one decision
	// fact for the successful condition and one ActivityID for the conflict case.
	seedDB, err := openSharedSQL(path)
	if err != nil {
		t.Fatal(err)
	}
	seedTracker, err := OpenBorrowedSQLite(seedDB)
	if err != nil {
		_ = seedDB.Close()
		t.Fatal(err)
	}
	seedAgent, err := seedTracker.RegisterSoftwareAgent("recovery", "actor", "1", "test")
	if err != nil {
		_ = seedTracker.Close()
		_ = seedDB.Close()
		t.Fatal(err)
	}
	actor := seedAgent.ID
	genesis, err := seedTracker.Journal().Apply(OperationInput{OperationID: "recovery-parity-genesis", ActorID: actor, CommandDigest: []byte("genesis"), Effects: []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority", BootstrapLabel: "root"}}})
	if err != nil {
		_ = seedTracker.Close()
		_ = seedDB.Close()
		t.Fatal(err)
	}
	authority, _ := slotJournalID(genesis, "authority")
	_ = seedTracker.Close()
	_ = seedDB.Close()

	first := openRecoveryParityStack(t, path, false, actor, appName)
	defer first.close()
	auth := authority
	taskID := TaskID{Namespace: "recovery", UUID: uuid.Must(uuid.NewV7())}
	task, err := first.tracker.Journal().Apply(OperationInput{OperationID: "recovery-parity-task", ActorID: actor, AuthorityJournalID: &auth, CommandDigest: []byte("task"), Effects: []Effect{{Sort: EffectTaskCreate, ResultSlot: "task", TaskID: taskID, Title: "recovery", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseWorkerSlices}}})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := first.tracker.Journal().Apply(OperationInput{OperationID: "recovery-parity-decision", ActorID: actor, AuthorityJournalID: &auth, CommandDigest: []byte("decision"), Effects: []Effect{{Sort: EffectDecision, ResultSlot: "decision", TaskID: taskID, DecisionKind: "fixture.condition", Payload: []byte(`{"ok":true}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	decisionID, _ := slotJournalID(decision, "decision")
	collisionActivity := recoveryParityActivity("018f0000-0000-7000-8000-000000000011")
	if _, err := first.tracker.Journal().Apply(OperationInput{OperationID: "recovery-parity-activity-seed", ActorID: actor, AuthorityJournalID: &auth, CommandDigest: []byte("activity-seed"), Effects: []Effect{{Sort: EffectActivityCreate, ResultSlot: "seed-activity", ActivityID: collisionActivity, ActivityAgentID: AgentID(actor), ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress}}}); err != nil {
		t.Fatal(err)
	}

	conditionSuccessActivity := recoveryParityActivity("018f0000-0000-7000-8000-000000000012")
	conditionSuccess := OperationInput{
		OperationID: "recovery-parity-activity-success", ActorID: actor, AuthorityJournalID: &auth, CommandDigest: []byte("activity-success"),
		Conditions: []Condition{{Kind: ConditionExactFact, Selector: FactSelector{Kind: FactDecision, Filter: FactFilter{TaskScope: FactTaskScope{Kind: FactTaskAny}}, DecisionKind: "fixture.condition"}, AssertedJournalID: decisionID}},
		Effects:    []Effect{{Sort: EffectActivityCreate, ResultSlot: "activity", ActivityID: conditionSuccessActivity, ActivityAgentID: AgentID(actor), ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress, ActivityNotes: "recovered"}},
	}
	conditionFailure := OperationInput{
		OperationID: "recovery-parity-condition-failure", ActorID: actor, AuthorityJournalID: &auth, CommandDigest: []byte("condition-failure"),
		Conditions: []Condition{{Kind: ConditionExactFact, Selector: FactSelector{Kind: FactDecision, Filter: FactFilter{TaskScope: FactTaskScope{Kind: FactTaskAny}}, DecisionKind: "fixture.missing"}, AssertedJournalID: 1}},
		Effects:    []Effect{{Sort: EffectActivityCreate, ResultSlot: "never", ActivityID: recoveryParityActivity("018f0000-0000-7000-8000-000000000013"), ActivityAgentID: AgentID(actor), ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress}},
	}
	activityConflict := OperationInput{
		OperationID: "recovery-parity-activity-conflict", ActorID: actor, AuthorityJournalID: &auth, CommandDigest: []byte("activity-conflict"),
		Effects: []Effect{{Sort: EffectActivityCreate, ResultSlot: "activity", ActivityID: collisionActivity, ActivityAgentID: AgentID(actor), ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress}},
	}

	firstSuccess, err := first.adapter.Apply(context.Background(), conditionSuccess)
	if err != nil || len(firstSuccess.ResultSlots) != 1 || firstSuccess.ResultSlots[0].ActivityID == nil || *firstSuccess.ResultSlots[0].ActivityID != conditionSuccessActivity {
		t.Fatalf("first conditioned ActivityCreate result=%#v err=%v", firstSuccess, err)
	}
	_, firstConditionErr := first.adapter.Apply(context.Background(), conditionFailure)
	_, firstActivityErr := first.adapter.Apply(context.Background(), activityConflict)
	var firstCondition *ConditionFailure
	var firstActivity *ActivityConflict
	if !errors.As(firstConditionErr, &firstCondition) || !errors.As(firstActivityErr, &firstActivity) || firstCondition.Reason != ConditionFactMissing || firstCondition.ActualJournalID != 0 || firstActivity.ActivityID != collisionActivity || firstActivity.ExistingJournalID <= 0 {
		t.Fatalf("first typed failures condition=%v activity=%v", firstConditionErr, firstActivityErr)
	}
	if first.entries.Load() != 3 {
		t.Fatalf("first callback entries=%d, want one per first delivery", first.entries.Load())
	}
	first.close()

	reopened := openRecoveryParityStack(t, path, false, actor, appName)
	defer reopened.close()
	recoveredSuccess, err := reopened.adapter.Apply(context.Background(), conditionSuccess)
	if err != nil || !reflect.DeepEqual(recoveredSuccess, firstSuccess) {
		t.Fatalf("recovered conditioned ActivityCreate=%#v err=%v want %#v", recoveredSuccess, err, firstSuccess)
	}
	_, recoveredConditionErr := reopened.adapter.Apply(context.Background(), conditionFailure)
	_, recoveredActivityErr := reopened.adapter.Apply(context.Background(), activityConflict)
	var recoveredCondition *ConditionFailure
	var recoveredActivity *ActivityConflict
	if !errors.As(recoveredConditionErr, &recoveredCondition) || !errors.As(recoveredActivityErr, &recoveredActivity) || !reflect.DeepEqual(recoveredCondition, firstCondition) || !reflect.DeepEqual(recoveredActivity, firstActivity) {
		t.Fatalf("recovered typed failures condition=%v activity=%v want condition=%#v activity=%#v", recoveredConditionErr, recoveredActivityErr, firstCondition, firstActivity)
	}
	if reopened.entries.Load() != 0 {
		t.Fatalf("recovered callback entries=%d, want zero re-entry", reopened.entries.Load())
	}
	looked, err := reopened.tracker.Journal().LookupCommitted(conditionSuccess.OperationID)
	if err != nil || looked.Kind != CommittedExact || !reflect.DeepEqual(looked, recoveredSuccess) {
		t.Fatalf("recovered journal success=%#v err=%v result=%#v", looked, err, recoveredSuccess)
	}
	for _, op := range []OperationID{conditionFailure.OperationID, activityConflict.OperationID} {
		looked, err := reopened.tracker.Journal().LookupCommitted(op)
		if err != nil || looked.Kind != CommittedAbsent {
			t.Fatalf("typed failure operation %q committed: %#v err=%v", op, looked, err)
		}
	}
	_ = task
}
