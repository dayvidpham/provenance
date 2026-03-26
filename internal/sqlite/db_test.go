package sqlite_test

import (
	"testing"
	"time"

	"github.com/dayvidpham/providence/internal/sqlite"
	"github.com/dayvidpham/providence/internal/testutil"
	"github.com/dayvidpham/providence/pkg/ptypes"
	"github.com/google/uuid"
)

// openTestDB delegates to shared testutil.OpenTestDB.
func openTestDB(t *testing.T) *sqlite.DB { return testutil.OpenTestDB(t) }

// makeTask delegates to shared testutil.MakeTask.
func makeTask(ns, title string) ptypes.Task { return testutil.MakeTask(ns, title) }

// ---------------------------------------------------------------------------
// Schema verification
// ---------------------------------------------------------------------------

func TestOpenAndClose(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open(:memory:) returned error: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close() returned error: %v", err)
	}
	// Second close should be safe.
	if err := db.Close(); err != nil {
		t.Fatalf("second db.Close() returned error: %v", err)
	}
}

func TestSchemaTablesExist(t *testing.T) {
	db := openTestDB(t)

	// Insert and retrieve a task to verify the schema is properly applied.
	task := makeTask("test-ns", "Schema check")
	if err := db.InsertTask(task); err != nil {
		t.Fatalf("InsertTask failed (schema may be incomplete): %v", err)
	}

	got, found, err := db.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if !found {
		t.Fatal("GetTask returned not found for just-inserted task")
	}
	if got.Title != "Schema check" {
		t.Errorf("Title = %q, want %q", got.Title, "Schema check")
	}
}

// ---------------------------------------------------------------------------
// Task CRUD
// ---------------------------------------------------------------------------

func TestInsertAndGetTask(t *testing.T) {
	db := openTestDB(t)

	task := makeTask("ns", "Test Task")
	task.Description = "A test task"
	task.Notes = "some notes"

	if err := db.InsertTask(task); err != nil {
		t.Fatalf("InsertTask error: %v", err)
	}

	got, found, err := db.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask error: %v", err)
	}
	if !found {
		t.Fatal("GetTask: task not found")
	}
	if got.Title != "Test Task" {
		t.Errorf("Title = %q, want %q", got.Title, "Test Task")
	}
	if got.Description != "A test task" {
		t.Errorf("Description = %q, want %q", got.Description, "A test task")
	}
	if got.Status != ptypes.StatusOpen {
		t.Errorf("Status = %v, want StatusOpen", got.Status)
	}
	if got.Phase != ptypes.PhaseUnscoped {
		t.Errorf("Phase = %v, want PhaseUnscoped", got.Phase)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	db := openTestDB(t)

	fakeID := ptypes.TaskID{Namespace: "ns", UUID: uuid.Must(uuid.NewV7())}
	_, found, err := db.GetTask(fakeID)
	if err != nil {
		t.Fatalf("GetTask error: %v", err)
	}
	if found {
		t.Error("GetTask should return not found for non-existent task")
	}
}

func TestUpdateTask(t *testing.T) {
	db := openTestDB(t)

	task := makeTask("ns", "Original Title")
	if err := db.InsertTask(task); err != nil {
		t.Fatalf("InsertTask error: %v", err)
	}

	newTitle := "Updated Title"
	newStatus := ptypes.StatusInProgress
	updated, err := db.UpdateTask(task.ID, ptypes.UpdateFields{
		Title:  &newTitle,
		Status: &newStatus,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("UpdateTask error: %v", err)
	}
	if updated.Title != "Updated Title" {
		t.Errorf("Title = %q, want %q", updated.Title, "Updated Title")
	}
	if updated.Status != ptypes.StatusInProgress {
		t.Errorf("Status = %v, want StatusInProgress", updated.Status)
	}
}

func TestCloseTask(t *testing.T) {
	db := openTestDB(t)

	task := makeTask("ns", "Task to close")
	if err := db.InsertTask(task); err != nil {
		t.Fatalf("InsertTask error: %v", err)
	}

	closed, err := db.CloseTask(task.ID, "done", time.Now().UTC())
	if err != nil {
		t.Fatalf("CloseTask error: %v", err)
	}
	if closed.Status != ptypes.StatusClosed {
		t.Errorf("Status = %v, want StatusClosed", closed.Status)
	}
	if closed.CloseReason != "done" {
		t.Errorf("CloseReason = %q, want %q", closed.CloseReason, "done")
	}
	if closed.ClosedAt == nil {
		t.Error("ClosedAt should not be nil after closing")
	}
}

func TestListTasks(t *testing.T) {
	db := openTestDB(t)

	task1 := makeTask("ns", "Task 1")
	task2 := makeTask("ns", "Task 2")
	for _, task := range []ptypes.Task{task1, task2} {
		if err := db.InsertTask(task); err != nil {
			t.Fatalf("InsertTask error: %v", err)
		}
	}

	tasks, err := db.ListTasks(ptypes.ListFilter{})
	if err != nil {
		t.Fatalf("ListTasks error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("ListTasks returned %d tasks, want 2", len(tasks))
	}
}

func TestListTasksWithFilter(t *testing.T) {
	db := openTestDB(t)

	task1 := makeTask("ns", "Bug task")
	task1.Type = ptypes.TaskTypeBug
	task2 := makeTask("ns", "Feature task")
	task2.Type = ptypes.TaskTypeFeature
	for _, task := range []ptypes.Task{task1, task2} {
		if err := db.InsertTask(task); err != nil {
			t.Fatalf("InsertTask error: %v", err)
		}
	}

	bugType := ptypes.TaskTypeBug
	tasks, err := db.ListTasks(ptypes.ListFilter{Type: &bugType})
	if err != nil {
		t.Fatalf("ListTasks error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("ListTasks(bug) returned %d tasks, want 1", len(tasks))
	}
	if tasks[0].Title != "Bug task" {
		t.Errorf("Title = %q, want %q", tasks[0].Title, "Bug task")
	}
}

func TestReadyAndBlockedTasks(t *testing.T) {
	db := openTestDB(t)

	parent := makeTask("ns", "Parent")
	child := makeTask("ns", "Child")
	if err := db.InsertTask(parent); err != nil {
		t.Fatalf("InsertTask parent: %v", err)
	}
	if err := db.InsertTask(child); err != nil {
		t.Fatalf("InsertTask child: %v", err)
	}

	// Before edge: both should be ready.
	ready, err := db.ReadyTasks()
	if err != nil {
		t.Fatalf("ReadyTasks error: %v", err)
	}
	if len(ready) != 2 {
		t.Fatalf("ReadyTasks before edge: got %d, want 2", len(ready))
	}

	// Add blocked-by edge: parent blocked by child.
	if err := db.InsertEdge(parent.ID, child.ID.String(), ptypes.EdgeBlockedBy, time.Now().UTC()); err != nil {
		t.Fatalf("InsertEdge error: %v", err)
	}

	ready, err = db.ReadyTasks()
	if err != nil {
		t.Fatalf("ReadyTasks error: %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("ReadyTasks after edge: got %d, want 1", len(ready))
	}
	if ready[0].Title != "Child" {
		t.Errorf("Ready task = %q, want %q", ready[0].Title, "Child")
	}

	blocked, err := db.BlockedTasks()
	if err != nil {
		t.Fatalf("BlockedTasks error: %v", err)
	}
	if len(blocked) != 1 {
		t.Fatalf("BlockedTasks: got %d, want 1", len(blocked))
	}
	if blocked[0].Title != "Parent" {
		t.Errorf("Blocked task = %q, want %q", blocked[0].Title, "Parent")
	}
}

// ---------------------------------------------------------------------------
// Edge CRUD
// ---------------------------------------------------------------------------

func TestInsertAndGetEdges(t *testing.T) {
	db := openTestDB(t)

	task1 := makeTask("ns", "Task 1")
	task2 := makeTask("ns", "Task 2")
	for _, task := range []ptypes.Task{task1, task2} {
		if err := db.InsertTask(task); err != nil {
			t.Fatalf("InsertTask error: %v", err)
		}
	}

	now := time.Now().UTC()
	if err := db.InsertEdge(task1.ID, task2.ID.String(), ptypes.EdgeBlockedBy, now); err != nil {
		t.Fatalf("InsertEdge error: %v", err)
	}

	edges, err := db.GetEdges(task1.ID, nil)
	if err != nil {
		t.Fatalf("GetEdges error: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("GetEdges returned %d edges, want 1", len(edges))
	}
	if edges[0].Kind != ptypes.EdgeBlockedBy {
		t.Errorf("EdgeKind = %v, want EdgeBlockedBy", edges[0].Kind)
	}
}

func TestDeleteEdge(t *testing.T) {
	db := openTestDB(t)

	task1 := makeTask("ns", "Task 1")
	task2 := makeTask("ns", "Task 2")
	for _, task := range []ptypes.Task{task1, task2} {
		if err := db.InsertTask(task); err != nil {
			t.Fatalf("InsertTask error: %v", err)
		}
	}

	now := time.Now().UTC()
	if err := db.InsertEdge(task1.ID, task2.ID.String(), ptypes.EdgeBlockedBy, now); err != nil {
		t.Fatalf("InsertEdge error: %v", err)
	}

	if err := db.DeleteEdge(task1.ID, task2.ID.String(), ptypes.EdgeBlockedBy); err != nil {
		t.Fatalf("DeleteEdge error: %v", err)
	}

	edges, err := db.GetEdges(task1.ID, nil)
	if err != nil {
		t.Fatalf("GetEdges error: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("GetEdges after delete: got %d edges, want 0", len(edges))
	}
}

func TestGetBlockedByEdges(t *testing.T) {
	db := openTestDB(t)

	task1 := makeTask("ns", "Task 1")
	task2 := makeTask("ns", "Task 2")
	for _, task := range []ptypes.Task{task1, task2} {
		if err := db.InsertTask(task); err != nil {
			t.Fatalf("InsertTask error: %v", err)
		}
	}

	now := time.Now().UTC()
	// blocked_by edge
	if err := db.InsertEdge(task1.ID, task2.ID.String(), ptypes.EdgeBlockedBy, now); err != nil {
		t.Fatalf("InsertEdge blocked_by error: %v", err)
	}
	// derived_from edge (should NOT appear in GetBlockedByEdges)
	if err := db.InsertEdge(task1.ID, task2.ID.String(), ptypes.EdgeDerivedFrom, now); err != nil {
		t.Fatalf("InsertEdge derived_from error: %v", err)
	}

	edges, err := db.GetBlockedByEdges()
	if err != nil {
		t.Fatalf("GetBlockedByEdges error: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("GetBlockedByEdges returned %d edges, want 1", len(edges))
	}
	if edges[0].Kind != ptypes.EdgeBlockedBy {
		t.Errorf("EdgeKind = %v, want EdgeBlockedBy", edges[0].Kind)
	}
}

func TestGetDepTree(t *testing.T) {
	db := openTestDB(t)

	// Create a chain: A -> B -> C (A blocked by B, B blocked by C)
	taskA := makeTask("ns", "A")
	taskB := makeTask("ns", "B")
	taskC := makeTask("ns", "C")
	for _, task := range []ptypes.Task{taskA, taskB, taskC} {
		if err := db.InsertTask(task); err != nil {
			t.Fatalf("InsertTask error: %v", err)
		}
	}

	now := time.Now().UTC()
	if err := db.InsertEdge(taskA.ID, taskB.ID.String(), ptypes.EdgeBlockedBy, now); err != nil {
		t.Fatalf("InsertEdge A->B error: %v", err)
	}
	if err := db.InsertEdge(taskB.ID, taskC.ID.String(), ptypes.EdgeBlockedBy, now); err != nil {
		t.Fatalf("InsertEdge B->C error: %v", err)
	}

	edges, err := db.GetDepTree(taskA.ID)
	if err != nil {
		t.Fatalf("GetDepTree error: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("GetDepTree returned %d edges, want 2", len(edges))
	}
}

// ---------------------------------------------------------------------------
// Label CRUD
// ---------------------------------------------------------------------------

func TestAddAndGetLabels(t *testing.T) {
	db := openTestDB(t)

	task := makeTask("ns", "Labeled task")
	if err := db.InsertTask(task); err != nil {
		t.Fatalf("InsertTask error: %v", err)
	}

	if err := db.AddLabel(task.ID, "priority:high"); err != nil {
		t.Fatalf("AddLabel error: %v", err)
	}
	if err := db.AddLabel(task.ID, "area:backend"); err != nil {
		t.Fatalf("AddLabel error: %v", err)
	}

	labels, err := db.GetLabels(task.ID)
	if err != nil {
		t.Fatalf("GetLabels error: %v", err)
	}
	if len(labels) != 2 {
		t.Fatalf("GetLabels returned %d labels, want 2", len(labels))
	}
	// Labels are sorted alphabetically.
	if labels[0] != "area:backend" {
		t.Errorf("labels[0] = %q, want %q", labels[0], "area:backend")
	}
	if labels[1] != "priority:high" {
		t.Errorf("labels[1] = %q, want %q", labels[1], "priority:high")
	}
}

func TestAddLabelIdempotent(t *testing.T) {
	db := openTestDB(t)

	task := makeTask("ns", "Labeled task")
	if err := db.InsertTask(task); err != nil {
		t.Fatalf("InsertTask error: %v", err)
	}

	// Adding the same label twice should not error.
	if err := db.AddLabel(task.ID, "dup"); err != nil {
		t.Fatalf("AddLabel first error: %v", err)
	}
	if err := db.AddLabel(task.ID, "dup"); err != nil {
		t.Fatalf("AddLabel second error: %v", err)
	}

	labels, err := db.GetLabels(task.ID)
	if err != nil {
		t.Fatalf("GetLabels error: %v", err)
	}
	if len(labels) != 1 {
		t.Errorf("GetLabels returned %d labels, want 1 (idempotent)", len(labels))
	}
}

func TestRemoveLabel(t *testing.T) {
	db := openTestDB(t)

	task := makeTask("ns", "Task")
	if err := db.InsertTask(task); err != nil {
		t.Fatalf("InsertTask error: %v", err)
	}
	if err := db.AddLabel(task.ID, "remove-me"); err != nil {
		t.Fatalf("AddLabel error: %v", err)
	}
	if err := db.RemoveLabel(task.ID, "remove-me"); err != nil {
		t.Fatalf("RemoveLabel error: %v", err)
	}

	labels, err := db.GetLabels(task.ID)
	if err != nil {
		t.Fatalf("GetLabels error: %v", err)
	}
	if len(labels) != 0 {
		t.Errorf("GetLabels after remove: got %d, want 0", len(labels))
	}
}

// ---------------------------------------------------------------------------
// Comment CRUD
// ---------------------------------------------------------------------------

func TestAddAndGetComments(t *testing.T) {
	db := openTestDB(t)

	task := makeTask("ns", "Commented task")
	if err := db.InsertTask(task); err != nil {
		t.Fatalf("InsertTask error: %v", err)
	}

	agent, err := db.RegisterHumanAgent("ns", "Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("RegisterHumanAgent error: %v", err)
	}

	comment, err := db.AddComment(task.ID, agent.ID, "First comment")
	if err != nil {
		t.Fatalf("AddComment error: %v", err)
	}
	if comment.Body != "First comment" {
		t.Errorf("Body = %q, want %q", comment.Body, "First comment")
	}

	comments, err := db.GetComments(task.ID)
	if err != nil {
		t.Fatalf("GetComments error: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("GetComments returned %d comments, want 1", len(comments))
	}
	if comments[0].Body != "First comment" {
		t.Errorf("comments[0].Body = %q, want %q", comments[0].Body, "First comment")
	}
}

// ---------------------------------------------------------------------------
// Agent TPT CRUD
// ---------------------------------------------------------------------------

func TestRegisterAndGetHumanAgent(t *testing.T) {
	db := openTestDB(t)

	ha, err := db.RegisterHumanAgent("ns", "Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("RegisterHumanAgent error: %v", err)
	}
	if ha.Name != "Alice" {
		t.Errorf("Name = %q, want %q", ha.Name, "Alice")
	}
	if ha.Kind != ptypes.AgentKindHuman {
		t.Errorf("Kind = %v, want AgentKindHuman", ha.Kind)
	}

	got, err := db.GetHumanAgent(ha.ID)
	if err != nil {
		t.Fatalf("GetHumanAgent error: %v", err)
	}
	if got.Name != "Alice" {
		t.Errorf("GetHumanAgent Name = %q, want %q", got.Name, "Alice")
	}
	if got.Contact != "alice@example.com" {
		t.Errorf("GetHumanAgent Contact = %q, want %q", got.Contact, "alice@example.com")
	}
}

func TestRegisterAndGetMLAgent(t *testing.T) {
	db := openTestDB(t)

	mla, err := db.RegisterMLAgent("ns", ptypes.RoleWorker, ptypes.ProviderAnthropic, "claude_opus_4")
	if err != nil {
		t.Fatalf("RegisterMLAgent error: %v", err)
	}
	if mla.Kind != ptypes.AgentKindMachineLearning {
		t.Errorf("Kind = %v, want AgentKindMachineLearning", mla.Kind)
	}
	if mla.Role != ptypes.RoleWorker {
		t.Errorf("Role = %v, want RoleWorker", mla.Role)
	}

	got, err := db.GetMLAgent(mla.ID)
	if err != nil {
		t.Fatalf("GetMLAgent error: %v", err)
	}
	if got.Model.Name != "claude_opus_4" {
		t.Errorf("Model.Name = %q, want %q", got.Model.Name, "claude_opus_4")
	}
}

func TestRegisterMLAgentUnknownModel(t *testing.T) {
	db := openTestDB(t)

	_, err := db.RegisterMLAgent("ns", ptypes.RoleWorker, ptypes.ProviderAnthropic, "nonexistent_model")
	if err == nil {
		t.Fatal("RegisterMLAgent should fail for unknown model")
	}
}

func TestRegisterAndGetSoftwareAgent(t *testing.T) {
	db := openTestDB(t)

	sa, err := db.RegisterSoftwareAgent("ns", "beads-cli", "1.0.0", "https://github.com/example/beads")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent error: %v", err)
	}
	if sa.Kind != ptypes.AgentKindSoftware {
		t.Errorf("Kind = %v, want AgentKindSoftware", sa.Kind)
	}

	got, err := db.GetSoftwareAgent(sa.ID)
	if err != nil {
		t.Fatalf("GetSoftwareAgent error: %v", err)
	}
	if got.Name != "beads-cli" {
		t.Errorf("Name = %q, want %q", got.Name, "beads-cli")
	}
	if got.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", got.Version, "1.0.0")
	}
}

func TestGetAgent(t *testing.T) {
	db := openTestDB(t)

	ha, err := db.RegisterHumanAgent("ns", "Bob", "bob@example.com")
	if err != nil {
		t.Fatalf("RegisterHumanAgent error: %v", err)
	}

	agent, err := db.GetAgent(ha.ID)
	if err != nil {
		t.Fatalf("GetAgent error: %v", err)
	}
	if agent.Kind != ptypes.AgentKindHuman {
		t.Errorf("Kind = %v, want AgentKindHuman", agent.Kind)
	}
}

func TestGetAgentNotFound(t *testing.T) {
	db := openTestDB(t)

	fakeID := ptypes.AgentID{Namespace: "ns", UUID: uuid.Must(uuid.NewV7())}
	_, err := db.GetAgent(fakeID)
	if err == nil {
		t.Fatal("GetAgent should fail for non-existent agent")
	}
}

// ---------------------------------------------------------------------------
// Activity CRUD
// ---------------------------------------------------------------------------

func TestStartAndEndActivity(t *testing.T) {
	db := openTestDB(t)

	agent, err := db.RegisterHumanAgent("ns", "Charlie", "")
	if err != nil {
		t.Fatalf("RegisterHumanAgent error: %v", err)
	}

	act, err := db.StartActivity(agent.ID, ptypes.PhaseWorkerSlices, ptypes.StageInProgress, "working on slice")
	if err != nil {
		t.Fatalf("StartActivity error: %v", err)
	}
	if act.Phase != ptypes.PhaseWorkerSlices {
		t.Errorf("Phase = %v, want PhaseWorkerSlices", act.Phase)
	}
	if act.EndedAt != nil {
		t.Error("EndedAt should be nil before ending")
	}

	ended, err := db.EndActivity(act.ID)
	if err != nil {
		t.Fatalf("EndActivity error: %v", err)
	}
	if ended.EndedAt == nil {
		t.Error("EndedAt should not be nil after ending")
	}
}

func TestGetActivities(t *testing.T) {
	db := openTestDB(t)

	agent, err := db.RegisterHumanAgent("ns", "Dave", "")
	if err != nil {
		t.Fatalf("RegisterHumanAgent error: %v", err)
	}

	if _, err := db.StartActivity(agent.ID, ptypes.PhaseRequest, ptypes.StageNotStarted, ""); err != nil {
		t.Fatalf("StartActivity error: %v", err)
	}
	if _, err := db.StartActivity(agent.ID, ptypes.PhaseElicit, ptypes.StageInProgress, ""); err != nil {
		t.Fatalf("StartActivity error: %v", err)
	}

	// Get all activities for this agent.
	activities, err := db.GetActivities(&agent.ID)
	if err != nil {
		t.Fatalf("GetActivities error: %v", err)
	}
	if len(activities) != 2 {
		t.Fatalf("GetActivities returned %d activities, want 2", len(activities))
	}

	// Get all activities (no filter).
	all, err := db.GetActivities(nil)
	if err != nil {
		t.Fatalf("GetActivities(nil) error: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("GetActivities(nil) returned %d activities, want 2", len(all))
	}
}

// ---------------------------------------------------------------------------
// List with label filter
// ---------------------------------------------------------------------------

func TestListTasksWithLabelFilter(t *testing.T) {
	db := openTestDB(t)

	task1 := makeTask("ns", "Labeled")
	task2 := makeTask("ns", "Unlabeled")
	for _, task := range []ptypes.Task{task1, task2} {
		if err := db.InsertTask(task); err != nil {
			t.Fatalf("InsertTask error: %v", err)
		}
	}
	if err := db.AddLabel(task1.ID, "epic:x"); err != nil {
		t.Fatalf("AddLabel error: %v", err)
	}

	tasks, err := db.ListTasks(ptypes.ListFilter{Label: "epic:x"})
	if err != nil {
		t.Fatalf("ListTasks error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("ListTasks(label=epic:x) returned %d tasks, want 1", len(tasks))
	}
	if tasks[0].Title != "Labeled" {
		t.Errorf("Title = %q, want %q", tasks[0].Title, "Labeled")
	}
}
