package provenance

// dbos_crashgap_test.go proves the two crash-gap recovery guarantees (issue #6)
// with real subprocess crashes at the unexported afterDomainCommit /
// afterStepCheckpoint boundaries:
//
//   - Gap 1 (crash AFTER domain commit, BEFORE step checkpoint): recovery re-runs
//     the step, which folds onto the already-committed operation via the §9.4
//     idempotent short-circuit — zero duplicate rows — then checkpoints.
//   - Gap 2 (crash AFTER step checkpoint, BEFORE workflow completion): recovery
//     reuses the durable closed step outcome without re-running the step.
//
// Both recover to exactly one domain mutation and one eventual DBOS result.
//
// The child branch runs in a re-exec of the test binary gated by PROV_CRASH_GAP; it
// sets the hook to os.Exit and drives one adapter.Apply that crashes mid-flight. The
// parent then reopens the SAME shared database, relaunches DBOS (triggering
// recovery), and drives the same operation to completion.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	_ "modernc.org/sqlite"
)

const (
	dbosInternalTestPoolSize = 16
	crashAppName             = "provenance-crashgap"
	crashAppVersion          = "crash-v1"
	crashExitBefore          = 41
	crashExitDomain          = 42
	crashExitStep            = 43
	crashExitFinished        = 7 // child returned without crashing (a failure)
)

// TestCrashGapChild is the re-exec child entry point. It is a no-op unless
// PROV_CRASH_GAP is set, so it does not run as a normal unit test.
func TestCrashGapChild(t *testing.T) {
	gap := os.Getenv("PROV_CRASH_GAP")
	if gap == "" {
		t.Skip("child entry point; runs only under PROV_CRASH_GAP")
	}
	runCrashChild(gap, os.Getenv("PROV_DBPATH"), os.Getenv("PROV_ACTOR"), os.Getenv("PROV_AUTH"), os.Getenv("PROV_TASK"), os.Getenv("PROV_MARKER"))
	// If we reach here the crash hook did not fire.
	os.Exit(crashExitFinished)
}

func openSharedSQL(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(dbosInternalTestPoolSize)
	db.SetMaxIdleConns(dbosInternalTestPoolSize / 2)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return db, nil
}

func crashOp(actor ptypes.ActorID, auth journal.JournalID, taskID ptypes.TaskID) journal.OperationInput {
	a := auth
	return journal.OperationInput{
		OperationID:        "op-crash",
		ActorID:            actor,
		AuthorityJournalID: &a,
		CommandDigest:      []byte("cmd-crash"),
		MutationDigest:     []byte("mut-crash"),
		RecordedAt:         1,
		Effects: []journal.Effect{{
			Sort:       EffectTaskCreate,
			ResultSlot: "task",
			TaskID:     taskID,
			Title:      "crash-task",
			Type:       TaskTypeTask,
			Priority:   PriorityMedium,
			Phase:      PhaseWorkerSlices,
		}},
	}
}

// runCrashChild builds the stack, arms the crash hook for gap, and drives one Apply
// that must crash via os.Exit inside the step.
func appendCrashMarker(path, marker string) {
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(file, marker)
	_ = file.Close()
}

func runCrashChild(gap, dbpath, actorStr, authStr, taskStr, marker string) {
	db, err := openSharedSQL(dbpath)
	if err != nil {
		os.Exit(20)
	}
	root, err := dbos.NewContext(context.Background(), dbos.Config{
		AppName: crashAppName, SQLiteSystemDB: db, ApplicationVersion: crashAppVersion,
	})
	if err != nil {
		os.Exit(21)
	}
	tracker, err := OpenBorrowedSQLite(db)
	if err != nil {
		os.Exit(22)
	}
	adapter, err := NewDBOSAdapter(root, tracker, DBOSAdapterConfig{})
	if err != nil {
		os.Exit(23)
	}
	adapter.testHooks.onWorkflowEntry = func() { appendCrashMarker(marker, "workflow-entry") }
	adapter.testHooks.beforeDomainCommit = func() { appendCrashMarker(marker, "domain-attempt") }
	adapter.testHooks.afterDomainCommit = func() { appendCrashMarker(marker, "domain-commit") }
	adapter.testHooks.afterStepCheckpoint = func() { appendCrashMarker(marker, "step-checkpoint") }
	switch gap {
	case "before":
		adapter.testHooks.beforeDomainCommit = func() { appendCrashMarker(marker, "domain-attempt"); os.Exit(crashExitBefore) }
	case "domain":
		adapter.testHooks.afterDomainCommit = func() { appendCrashMarker(marker, "domain-commit"); os.Exit(crashExitDomain) }
	case "step":
		adapter.testHooks.afterStepCheckpoint = func() { appendCrashMarker(marker, "step-checkpoint"); os.Exit(crashExitStep) }
	default:
		os.Exit(24)
	}
	if err := dbos.Launch(root); err != nil {
		os.Exit(25)
	}
	actor, err := ptypes.ParseActorID(actorStr)
	if err != nil {
		os.Exit(26)
	}
	auth, err := strconv.ParseInt(authStr, 10, 64)
	if err != nil {
		os.Exit(27)
	}
	taskID, err := ptypes.ParseTaskID(taskStr)
	if err != nil {
		os.Exit(28)
	}
	_, _ = adapter.Apply(context.Background(), crashOp(actor, journal.JournalID(auth), taskID))
}

func runCrashGap(t *testing.T, gap string, wantExit int) {
	t.Helper()
	dir := t.TempDir()
	dbpath := dir + "/crash.db"
	markerPath := dir + "/crash.markers"

	// Parent establishes the genesis authority + committing actor (no DBOS needed),
	// then releases the file before spawning the child.
	actor, auth := seedCrashGenesis(t, dbpath)
	taskID := ptypes.TaskID{Namespace: "aura", UUID: uuid.Must(uuid.NewV7())}

	// Spawn the child, which crashes mid-Apply at the gap boundary.
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashGapChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		"PROV_CRASH_GAP="+gap,
		"PROV_DBPATH="+dbpath,
		"PROV_ACTOR="+actor.String(),
		"PROV_AUTH="+strconv.FormatInt(int64(auth), 10),
		"PROV_TASK="+taskID.String(),
		"PROV_MARKER="+markerPath,
	)
	out, err := cmd.CombinedOutput()
	code := exitCode(err)
	if code != wantExit {
		t.Fatalf("child exit code = %d, want %d (crash at %s gap)\n%s", code, wantExit, gap, out)
	}

	// Parent reopens the SAME shared database and relaunches DBOS, triggering
	// recovery of the crashed workflow.
	db, err := openSharedSQL(dbpath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()
	root, err := dbos.NewContext(context.Background(), dbos.Config{
		AppName: crashAppName, SQLiteSystemDB: db, ApplicationVersion: crashAppVersion,
	})
	if err != nil {
		t.Fatalf("reopen dbos.NewContext: %v", err)
	}
	tracker, err := OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("reopen OpenBorrowedSQLite: %v", err)
	}
	adapter, err := NewDBOSAdapter(root, tracker, DBOSAdapterConfig{})
	if err != nil {
		t.Fatalf("reopen NewDBOSAdapter: %v", err)
	}
	adapter.testHooks.onWorkflowEntry = func() { appendCrashMarker(markerPath, "workflow-entry") }
	adapter.testHooks.beforeDomainCommit = func() { appendCrashMarker(markerPath, "domain-attempt") }
	adapter.testHooks.afterDomainCommit = func() { appendCrashMarker(markerPath, "domain-commit") }
	adapter.testHooks.afterStepCheckpoint = func() { appendCrashMarker(markerPath, "step-checkpoint") }
	if err := dbos.Launch(root); err != nil {
		t.Fatalf("reopen Launch: %v", err)
	}
	defer func() { shutdownDBOSRoot(t, root, 5*time.Second); _ = tracker.Close() }()

	// Drive the same operation to completion: it attaches to the recovered workflow.
	res, err := adapter.Apply(context.Background(), crashOp(actor, auth, taskID))
	if err != nil {
		t.Fatalf("post-recovery Apply: %v", err)
	}
	if res.Kind != journal.CommittedExact {
		t.Fatalf("recovered result Kind = %v, want CommittedExact", res.Kind)
	}

	// Exactly one committed operation, one task, zero duplicates.
	looked, err := tracker.Journal().LookupCommitted("op-crash")
	if err != nil {
		t.Fatalf("LookupCommitted: %v", err)
	}
	if looked.Kind != journal.CommittedExact {
		t.Fatalf("LookupCommitted Kind = %v, want CommittedExact", looked.Kind)
	}
	if !reflect.DeepEqual(res, looked) {
		t.Fatalf("recovered complete result=%#v want journal result=%#v", res, looked)
	}
	task, err := tracker.Show(taskID)
	if err != nil {
		t.Fatalf("Show recovered task: %v", err)
	}
	if task.ID != taskID || task.Title != "crash-task" || task.Description != "" || task.Type != TaskTypeTask || task.Priority != PriorityMedium || task.Phase != PhaseWorkerSlices || task.Status != StatusOpen || task.Owner != nil || task.Notes != "" || task.CreatedAt.UnixNano() != 1 || !task.UpdatedAt.Equal(task.CreatedAt) || task.ClosedAt != nil || task.CloseReason != "" {
		t.Fatalf("recovered complete task tuple drifted: %#v", task)
	}
	tasks, err := tracker.List(ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	crashTasks := 0
	for _, task := range tasks {
		if task.Title == "crash-task" {
			crashTasks++
		}
	}
	if crashTasks != 1 {
		t.Errorf("crash-task row count = %d, want exactly 1 (zero duplicates)", crashTasks)
	}
	// Recovery leaves the journal convergent (no double-fold, no orphan).
	if _, err := tracker.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections after recovery: %v", err)
	}
	markerBytes, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read crash marker: %v", err)
	}
	var domainAttempts []string
	for _, marker := range strings.Split(strings.TrimSpace(string(markerBytes)), "\n") {
		if marker == "domain-attempt" {
			domainAttempts = append(domainAttempts, marker)
		}
	}
	wantAttempts := 1
	if gap != "step" {
		wantAttempts = 2
	}
	if len(domainAttempts) != wantAttempts {
		t.Fatalf("crash seam %s domain-attempt vector=%v, want %d attempts", gap, domainAttempts, wantAttempts)
	}
	var workflows, outputs, operations int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_status`).Scan(&workflows); err != nil {
		t.Fatalf("count workflow_status: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM operation_outputs`).Scan(&outputs); err != nil {
		t.Fatalf("count operation_outputs: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM journal_operations WHERE operation_id = ?`, "op-crash").Scan(&operations); err != nil {
		t.Fatalf("count committed operation: %v", err)
	}
	if workflows != 1 || outputs != 1 || operations != 1 {
		t.Fatalf("crash seam %s durable counts workflows=%d outputs=%d operations=%d, want 1/1/1", gap, workflows, outputs, operations)
	}
}

// seedCrashGenesis registers the committing actor and establishes the genesis
// bootstrap authority on a fresh shared file, then closes the borrowed tracker so
// the child can own the DBOS lifecycle.
func seedCrashGenesis(t *testing.T, dbpath string) (ptypes.ActorID, journal.JournalID) {
	t.Helper()
	db, err := openSharedSQL(dbpath)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer func() { _ = db.Close() }()
	tracker, err := OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("seed OpenBorrowedSQLite: %v", err)
	}
	sys, err := tracker.RegisterSoftwareAgent("provenance-test", "pasture-system", "0", "t")
	if err != nil {
		t.Fatalf("seed register: %v", err)
	}
	res, err := tracker.Journal().Apply(journal.OperationInput{
		OperationID:    "op-genesis",
		ActorID:        sys.ID,
		CommandDigest:  []byte("genesis-c"),
		MutationDigest: []byte("genesis-m"),
		Effects:        []journal.Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("seed genesis: %v", err)
	}
	_ = tracker.Close()
	for i := range res.ResultSlots {
		if string(res.ResultSlots[i].Slot) == "auth" {
			return sys.ID, res.ResultSlots[i].ProducedJournalID
		}
	}
	t.Fatal("seed genesis: no bootstrap authority slot")
	return ptypes.ActorID{}, 0
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		return ee.ExitCode()
	}
	return -1
}

func asExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			*target = ee
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// recoveryQueueName is the reserved queue the runtime re-enqueues recovered
// workflows onto. It is an internal constant of the library, so this test names
// it as durable data read back out of workflow_status rather than importing it.
// Source: dbos/internal/models/queue.go:9, InternalQueueName.
const recoveryQueueName = "_dbos_internal_queue"

// The supported runtime no longer recovers a crashed workflow on the recovering
// caller's goroutine: Launch re-enqueues every PENDING workflow it finds onto
// the reserved internal queue, and that queue's worker runs it. Prove it with no
// second caller at all -- the reopening process only builds the stack and
// launches -- and prove where it ran by reading the queue the row was placed on.
// Source: dbos/dbos.go:825 and dbos/recovery.go:9-21.
func TestRecoveredWorkflowFinishesOnTheInternalQueueWithoutASecondCaller(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("subprocess crash-recovery test")
	}
	dir := t.TempDir()
	dbpath := dir + "/recovery-on-launch.db"
	markerPath := dir + "/recovery-on-launch.markers"

	actor, auth := seedCrashGenesis(t, dbpath)
	taskID := ptypes.TaskID{Namespace: "aura", UUID: uuid.Must(uuid.NewV7())}

	// The child crashes before the domain commit, so nothing of the operation is
	// durable yet except the PENDING workflow row.
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashGapChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		"PROV_CRASH_GAP=before",
		"PROV_DBPATH="+dbpath,
		"PROV_ACTOR="+actor.String(),
		"PROV_AUTH="+strconv.FormatInt(int64(auth), 10),
		"PROV_TASK="+taskID.String(),
		"PROV_MARKER="+markerPath,
	)
	out, err := cmd.CombinedOutput()
	if code := exitCode(err); code != crashExitBefore {
		t.Fatalf("child exit code = %d, want %d (crash before the domain commit)\n%s", code, crashExitBefore, out)
	}

	db, err := openSharedSQL(dbpath)
	if err != nil {
		t.Fatalf("reopen the crashed database: %v", err)
	}
	defer func() { _ = db.Close() }()
	root, err := dbos.NewContext(context.Background(), dbos.Config{
		AppName: crashAppName, SQLiteSystemDB: db, ApplicationVersion: crashAppVersion,
	})
	if err != nil {
		t.Fatalf("reopen dbos.NewContext: %v", err)
	}
	tracker, err := OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("reopen OpenBorrowedSQLite: %v", err)
	}
	adapter, err := NewDBOSAdapter(root, tracker, DBOSAdapterConfig{})
	if err != nil {
		t.Fatalf("reopen NewDBOSAdapter: %v", err)
	}
	workflowID := adapter.contract.workflowPrefix + workflowIdentity(adapter.contract, adapter.applicationVersion, "op-crash")
	if err := dbos.Launch(root); err != nil {
		t.Fatalf("reopen Launch: %v", err)
	}
	defer func() { shutdownDBOSRoot(t, root, 10*time.Second); _ = tracker.Close() }()

	// Wait on the workflow's own terminal condition, with a bounded ceiling. The
	// handle blocks until the recovered workflow reaches a terminal state; the
	// timeout is a failure ceiling, never a scheduling assumption.
	type completion struct {
		outcome DBOSStepOutcome
		err     error
	}
	finished := make(chan completion, 1)
	go func() {
		handle, retrieveErr := dbos.RetrieveWorkflow[DBOSStepOutcome](root, workflowID)
		if retrieveErr != nil {
			finished <- completion{err: retrieveErr}
			return
		}
		outcome, resultErr := handle.GetResult()
		finished <- completion{outcome: outcome, err: resultErr}
	}()
	select {
	case done := <-finished:
		if done.err != nil {
			t.Fatalf("the recovered workflow did not complete on its own: %v", done.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the recovered workflow never reached a terminal state, with no second caller driving it: " +
			"recovery no longer completes through the runtime's internal queue")
	}

	// The domain effect is durable exactly once, and the journal agrees.
	looked, err := tracker.Journal().LookupCommitted("op-crash")
	if err != nil {
		t.Fatalf("LookupCommitted after unattended recovery: %v", err)
	}
	if looked.Kind != journal.CommittedExact {
		t.Fatalf("LookupCommitted Kind = %v, want CommittedExact", looked.Kind)
	}
	if _, err := tracker.Show(taskID); err != nil {
		t.Fatalf("Show the recovered task: %v", err)
	}

	// The row records WHERE it ran: recovery placed it on the reserved internal
	// queue, which is the behaviour this test exists to pin.
	var queueName string
	var status string
	if err := db.QueryRow(`SELECT COALESCE(queue_name, ''), status FROM workflow_status WHERE workflow_uuid = ?`, workflowID).
		Scan(&queueName, &status); err != nil {
		t.Fatalf("read the recovered workflow row: %v", err)
	}
	if queueName != recoveryQueueName {
		t.Errorf("recovered workflow queue_name = %q, want %q: recovery did not run through the internal queue",
			queueName, recoveryQueueName)
	}
	if status != string(dbos.WorkflowStatusSuccess) {
		t.Errorf("recovered workflow status = %q, want %q", status, dbos.WorkflowStatusSuccess)
	}

	var workflows, outputs, operations int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_status`).Scan(&workflows); err != nil {
		t.Fatalf("count workflow_status: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM operation_outputs`).Scan(&outputs); err != nil {
		t.Fatalf("count operation_outputs: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM journal_operations WHERE operation_id = ?`, "op-crash").Scan(&operations); err != nil {
		t.Fatalf("count the committed operation: %v", err)
	}
	if workflows != 1 || outputs != 1 || operations != 1 {
		t.Fatalf("unattended recovery durable counts workflows=%d outputs=%d operations=%d, want 1/1/1",
			workflows, outputs, operations)
	}
}

func TestCrashGap1_DomainCommitBeforeCheckpoint(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("subprocess crash-recovery test")
	}
	runCrashGap(t, "domain", crashExitDomain)
}

func TestCrashGap0_BeforeDomainCommit(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("subprocess crash-recovery test")
	}
	runCrashGap(t, "before", crashExitBefore)
}

func TestCrashGap2_StepCheckpointBeforeCompletion(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("subprocess crash-recovery test")
	}
	runCrashGap(t, "step", crashExitStep)
}
