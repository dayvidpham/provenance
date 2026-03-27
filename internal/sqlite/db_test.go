package sqlite_test

import (
	_ "embed"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dayvidpham/providence/internal/sqlite"
	"github.com/dayvidpham/providence/internal/testutil"
	"github.com/dayvidpham/providence/pkg/ptypes"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
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

// ---------------------------------------------------------------------------
// YAML fixture types
// ---------------------------------------------------------------------------

//go:embed testdata/fixtures.yaml
var sqliteFixtureData []byte

// sqliteFixtures mirrors the top-level structure of testdata/fixtures.yaml.
type sqliteFixtures struct {
	UpdateTask        updateTaskFixtures        `yaml:"update_task"`
	ListTasks         listTasksFixtures         `yaml:"list_tasks"`
	RegisterMLAgent   registerMLAgentFixtures   `yaml:"register_ml_agent"`
	StartActivity     startActivityFixtures     `yaml:"start_activity"`
	AgentKindMismatch agentKindMismatchFixtures `yaml:"agent_kind_mismatch"`
}

// --- UpdateTask ---

type updateTaskFixtures struct {
	UpdateFieldSets []updateFieldSet  `yaml:"update_field_sets"`
	FieldValues     updateFieldValues `yaml:"field_values"`
}

type updateFieldSet struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Fields      []string `yaml:"fields"`
}

type updateFieldValues struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	Status      int    `yaml:"status"`
	Priority    int    `yaml:"priority"`
	Phase       int    `yaml:"phase"`
	Notes       string `yaml:"notes"`
}

// --- ListTasks ---

type listTasksFixtures struct {
	SeedTasks  []seedTask      `yaml:"seed_tasks"`
	FilterSets []listFilterSet `yaml:"filter_sets"`
}

type seedTask struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
	Title     string `yaml:"title"`
	Status    int    `yaml:"status"`
	Priority  int    `yaml:"priority"`
	Type      int    `yaml:"type"`
	Phase     int    `yaml:"phase"`
	Label     string `yaml:"label"`
}

type listFilterSet struct {
	Name          string     `yaml:"name"`
	Description   string     `yaml:"description"`
	Filter        yamlFilter `yaml:"filter"`
	ExpectedTasks []string   `yaml:"expected_tasks"`
}

// yamlFilter mirrors ptypes.ListFilter with nullable int pointers.
type yamlFilter struct {
	Status    *int   `yaml:"status"`
	Priority  *int   `yaml:"priority"`
	Type      *int   `yaml:"type"`
	Phase     *int   `yaml:"phase"`
	Label     string `yaml:"label"`
	Namespace string `yaml:"namespace"`
}

// toListFilter converts the YAML filter to a ptypes.ListFilter.
func (yf yamlFilter) toListFilter() ptypes.ListFilter {
	f := ptypes.ListFilter{
		Label:     yf.Label,
		Namespace: yf.Namespace,
	}
	if yf.Status != nil {
		s := ptypes.Status(*yf.Status)
		f.Status = &s
	}
	if yf.Priority != nil {
		p := ptypes.Priority(*yf.Priority)
		f.Priority = &p
	}
	if yf.Type != nil {
		tp := ptypes.TaskType(*yf.Type)
		f.Type = &tp
	}
	if yf.Phase != nil {
		ph := ptypes.Phase(*yf.Phase)
		f.Phase = &ph
	}
	return f
}

// --- RegisterMLAgent ---

type registerMLAgentFixtures struct {
	Roles         []int         `yaml:"roles"`
	KnownModels   []mlModelSpec `yaml:"known_models"`
	UnknownModels []mlModelSpec `yaml:"unknown_models"`
}

type mlModelSpec struct {
	Provider    int    `yaml:"provider"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// --- StartActivity ---

type startActivityFixtures struct {
	Phases       []int         `yaml:"phases"`
	Stages       []int         `yaml:"stages"`
	NoteVariants []noteVariant `yaml:"note_variants"`
}

type noteVariant struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// --- AgentKindMismatch ---

type agentKindMismatchFixtures struct {
	AgentKinds      []agentKindSpec  `yaml:"agent_kinds"`
	GetterFunctions []getterFuncSpec `yaml:"getter_functions"`
}

type agentKindSpec struct {
	Name string `yaml:"name"`
	Kind int    `yaml:"kind"`
}

type getterFuncSpec struct {
	Name string `yaml:"name"`
	Kind int    `yaml:"kind"`
}

// ---------------------------------------------------------------------------
// Fixture loading
// ---------------------------------------------------------------------------

func loadSQLiteFixtures(t *testing.T) sqliteFixtures {
	t.Helper()
	var f sqliteFixtures
	if err := yaml.Unmarshal(sqliteFixtureData, &f); err != nil {
		t.Fatalf("failed to parse testdata/fixtures.yaml: %v", err)
	}
	return f
}

// ---------------------------------------------------------------------------
// Target 1: UpdateTask — Dynamic SET clause permutations
// ---------------------------------------------------------------------------

func TestUpdateTask_YAMLPermutations(t *testing.T) {
	fix := loadSQLiteFixtures(t)

	for _, fs := range fix.UpdateTask.UpdateFieldSets {
		fs := fs // capture
		t.Run(fs.Name, func(t *testing.T) {
			db := openTestDB(t)

			// Register an owner agent to use when "owner" field is in the set.
			agent, err := db.RegisterHumanAgent("test-ns", "OwnerAgent", "owner@example.com")
			if err != nil {
				t.Fatalf("RegisterHumanAgent error: %v", err)
			}

			task := makeTask("test-ns", "Original Title")
			task.Description = "original description"
			task.Notes = "original notes"
			if err := db.InsertTask(task); err != nil {
				t.Fatalf("InsertTask error: %v", err)
			}

			// Build UpdateFields from fixture field list.
			vals := fix.UpdateTask.FieldValues
			fields := ptypes.UpdateFields{}
			fieldSet := make(map[string]bool, len(fs.Fields))
			for _, f := range fs.Fields {
				fieldSet[f] = true
			}
			if fieldSet["title"] {
				v := vals.Title
				fields.Title = &v
			}
			if fieldSet["description"] {
				v := vals.Description
				fields.Description = &v
			}
			if fieldSet["status"] {
				v := ptypes.Status(vals.Status)
				fields.Status = &v
			}
			if fieldSet["priority"] {
				v := ptypes.Priority(vals.Priority)
				fields.Priority = &v
			}
			if fieldSet["phase"] {
				v := ptypes.Phase(vals.Phase)
				fields.Phase = &v
			}
			if fieldSet["notes"] {
				v := vals.Notes
				fields.Notes = &v
			}
			if fieldSet["owner"] {
				fields.Owner = &agent.ID
			}

			updated, err := db.UpdateTask(task.ID, fields, time.Now().UTC())
			if err != nil {
				t.Fatalf("UpdateTask error: %v", err)
			}

			// Verify each field that was set.
			if fieldSet["title"] && updated.Title != vals.Title {
				t.Errorf("Title = %q, want %q", updated.Title, vals.Title)
			}
			if fieldSet["description"] && updated.Description != vals.Description {
				t.Errorf("Description = %q, want %q", updated.Description, vals.Description)
			}
			if fieldSet["status"] && updated.Status != ptypes.Status(vals.Status) {
				t.Errorf("Status = %v, want %v", updated.Status, ptypes.Status(vals.Status))
			}
			if fieldSet["priority"] && updated.Priority != ptypes.Priority(vals.Priority) {
				t.Errorf("Priority = %v, want %v", updated.Priority, ptypes.Priority(vals.Priority))
			}
			if fieldSet["phase"] && updated.Phase != ptypes.Phase(vals.Phase) {
				t.Errorf("Phase = %v, want %v", updated.Phase, ptypes.Phase(vals.Phase))
			}
			if fieldSet["notes"] && updated.Notes != vals.Notes {
				t.Errorf("Notes = %q, want %q", updated.Notes, vals.Notes)
			}
			if fieldSet["owner"] {
				if updated.Owner == nil {
					t.Error("Owner = nil, want non-nil")
				} else if *updated.Owner != agent.ID {
					t.Errorf("Owner = %v, want %v", updated.Owner, agent.ID)
				}
			}

			// Fields NOT in the set must remain at their original values.
			if !fieldSet["title"] && updated.Title != "Original Title" {
				t.Errorf("Title changed unexpectedly: got %q, want %q", updated.Title, "Original Title")
			}
			if !fieldSet["description"] && updated.Description != "original description" {
				t.Errorf("Description changed unexpectedly: got %q", updated.Description)
			}
			if !fieldSet["status"] && updated.Status != ptypes.StatusOpen {
				t.Errorf("Status changed unexpectedly: got %v", updated.Status)
			}
			if !fieldSet["priority"] && updated.Priority != ptypes.PriorityMedium {
				t.Errorf("Priority changed unexpectedly: got %v", updated.Priority)
			}
			if !fieldSet["phase"] && updated.Phase != ptypes.PhaseUnscoped {
				t.Errorf("Phase changed unexpectedly: got %v", updated.Phase)
			}
			if !fieldSet["notes"] && updated.Notes != "original notes" {
				t.Errorf("Notes changed unexpectedly: got %q", updated.Notes)
			}
			if !fieldSet["owner"] && updated.Owner != nil {
				t.Errorf("Owner changed unexpectedly: got %v", updated.Owner)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Target 2: ListTasks — Dynamic WHERE clause permutations
// ---------------------------------------------------------------------------

func TestListTasks_YAMLPermutations(t *testing.T) {
	fix := loadSQLiteFixtures(t)

	for _, fs := range fix.ListTasks.FilterSets {
		fs := fs // capture
		t.Run(fs.Name, func(t *testing.T) {
			db := openTestDB(t)

			// Seed all tasks. Map name → TaskID for expected-task lookup.
			taskIDs := make(map[string]ptypes.TaskID, len(fix.ListTasks.SeedTasks))
			for _, st := range fix.ListTasks.SeedTasks {
				task := makeTask(st.Namespace, st.Title)
				task.Status = ptypes.Status(st.Status)
				task.Priority = ptypes.Priority(st.Priority)
				task.Type = ptypes.TaskType(st.Type)
				task.Phase = ptypes.Phase(st.Phase)
				if err := db.InsertTask(task); err != nil {
					t.Fatalf("InsertTask(%q) error: %v", st.Name, err)
				}
				if st.Label != "" {
					if err := db.AddLabel(task.ID, st.Label); err != nil {
						t.Fatalf("AddLabel(%q, %q) error: %v", st.Name, st.Label, err)
					}
				}
				taskIDs[st.Name] = task.ID
			}

			filter := fs.Filter.toListFilter()
			tasks, err := db.ListTasks(filter)
			if err != nil {
				t.Fatalf("ListTasks error: %v", err)
			}

			// Build a set of returned IDs for O(1) lookup.
			returnedIDs := make(map[ptypes.TaskID]bool, len(tasks))
			for _, tsk := range tasks {
				returnedIDs[tsk.ID] = true
			}

			// Every expected task must appear in results.
			for _, expectedName := range fs.ExpectedTasks {
				id, ok := taskIDs[expectedName]
				if !ok {
					t.Fatalf("expected task name %q not found in seed map", expectedName)
				}
				if !returnedIDs[id] {
					t.Errorf("task %q (id=%v) expected in results but not found", expectedName, id)
				}
			}

			// Result count must match expected count exactly.
			if len(tasks) != len(fs.ExpectedTasks) {
				returnedNames := make([]string, 0, len(tasks))
				for _, tsk := range tasks {
					for n, id := range taskIDs {
						if id == tsk.ID {
							returnedNames = append(returnedNames, n)
						}
					}
				}
				t.Errorf("ListTasks returned %d tasks, want %d; got: %v",
					len(tasks), len(fs.ExpectedTasks), returnedNames)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Target 3: RegisterMLAgent — Role × Provider × model_state cross-product
// ---------------------------------------------------------------------------

func TestRegisterMLAgent_YAMLPermutations(t *testing.T) {
	fix := loadSQLiteFixtures(t)

	// Known models: each (role, model) combination must succeed.
	for _, roleVal := range fix.RegisterMLAgent.Roles {
		for _, model := range fix.RegisterMLAgent.KnownModels {
			roleVal, model := roleVal, model // capture
			testName := fmt.Sprintf("role_%d/provider_%d/%s", roleVal, model.Provider, model.Name)
			t.Run(testName, func(t *testing.T) {
				db := openTestDB(t)

				role := ptypes.Role(roleVal)
				provider := ptypes.Provider(model.Provider)

				mla, err := db.RegisterMLAgent("test-ns", role, provider, model.Name)
				if err != nil {
					t.Fatalf("RegisterMLAgent(%v, %v, %q) unexpected error: %v", role, provider, model.Name, err)
				}
				if mla.Role != role {
					t.Errorf("Role = %v, want %v", mla.Role, role)
				}
				if mla.Model.Name != model.Name {
					t.Errorf("Model.Name = %q, want %q", mla.Model.Name, model.Name)
				}
				if mla.Model.Provider != provider {
					t.Errorf("Model.Provider = %v, want %v", mla.Model.Provider, provider)
				}
				if mla.Kind != ptypes.AgentKindMachineLearning {
					t.Errorf("Kind = %v, want AgentKindMachineLearning", mla.Kind)
				}

				// Round-trip via GetMLAgent.
				got, err := db.GetMLAgent(mla.ID)
				if err != nil {
					t.Fatalf("GetMLAgent error: %v", err)
				}
				if got.Role != role {
					t.Errorf("GetMLAgent Role = %v, want %v", got.Role, role)
				}
				if got.Model.Name != model.Name {
					t.Errorf("GetMLAgent Model.Name = %q, want %q", got.Model.Name, model.Name)
				}
			})
		}
	}

	// Unknown models: must return ErrNotFound.
	for _, model := range fix.RegisterMLAgent.UnknownModels {
		model := model // capture
		testName := fmt.Sprintf("unknown/provider_%d/%s", model.Provider, model.Name)
		t.Run(testName, func(t *testing.T) {
			db := openTestDB(t)

			provider := ptypes.Provider(model.Provider)
			_, err := db.RegisterMLAgent("test-ns", ptypes.RoleWorker, provider, model.Name)
			if err == nil {
				t.Fatalf("RegisterMLAgent(%v, %q) expected ErrNotFound, got nil", provider, model.Name)
			}
			if !errors.Is(err, ptypes.ErrNotFound) {
				t.Errorf("RegisterMLAgent(%v, %q) error = %v, want ErrNotFound", provider, model.Name, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Target 4: StartActivity — Phase × Stage × notes cross-product
// ---------------------------------------------------------------------------

func TestStartActivity_YAMLPermutations(t *testing.T) {
	fix := loadSQLiteFixtures(t)

	count := 0
	for _, phaseVal := range fix.StartActivity.Phases {
		for _, stageVal := range fix.StartActivity.Stages {
			for _, noteVar := range fix.StartActivity.NoteVariants {
				phaseVal, stageVal, noteVar := phaseVal, stageVal, noteVar // capture
				testName := fmt.Sprintf("phase_%d/stage_%d/%s", phaseVal, stageVal, noteVar.Name)
				t.Run(testName, func(t *testing.T) {
					db := openTestDB(t)

					agent, err := db.RegisterHumanAgent("test-ns", "ActivityAgent", "")
					if err != nil {
						t.Fatalf("RegisterHumanAgent error: %v", err)
					}

					phase := ptypes.Phase(phaseVal)
					stage := ptypes.Stage(stageVal)
					notes := noteVar.Value

					act, err := db.StartActivity(agent.ID, phase, stage, notes)
					if err != nil {
						t.Fatalf("StartActivity(phase=%v, stage=%v, notes=%q) error: %v",
							phase, stage, notes, err)
					}

					// Verify fields returned by StartActivity.
					if act.Phase != phase {
						t.Errorf("Phase = %v, want %v", act.Phase, phase)
					}
					if act.Stage != stage {
						t.Errorf("Stage = %v, want %v", act.Stage, stage)
					}
					if act.Notes != notes {
						t.Errorf("Notes = %q, want %q", act.Notes, notes)
					}
					if act.AgentID != agent.ID {
						t.Errorf("AgentID = %v, want %v", act.AgentID, agent.ID)
					}
					if act.EndedAt != nil {
						t.Error("EndedAt should be nil before ending")
					}
					if act.StartedAt.IsZero() {
						t.Error("StartedAt should not be zero")
					}

					// Verify persistence via GetActivities.
					activities, err := db.GetActivities(&agent.ID)
					if err != nil {
						t.Fatalf("GetActivities error: %v", err)
					}
					if len(activities) != 1 {
						t.Fatalf("GetActivities returned %d, want 1", len(activities))
					}
					got := activities[0]
					if got.Phase != phase {
						t.Errorf("Persisted Phase = %v, want %v", got.Phase, phase)
					}
					if got.Stage != stage {
						t.Errorf("Persisted Stage = %v, want %v", got.Stage, stage)
					}
					if got.Notes != notes {
						t.Errorf("Persisted Notes = %q, want %q", got.Notes, notes)
					}
				})
				count++
			}
		}
	}
	_ = count // 13 × 4 × 2 = 104
}

// ---------------------------------------------------------------------------
// Target 5: AgentKindMismatch — 3 getters × 3 agent kinds
// ---------------------------------------------------------------------------

func TestAgentKindMismatch_YAMLPermutations(t *testing.T) {
	fix := loadSQLiteFixtures(t)

	for _, ak := range fix.AgentKindMismatch.AgentKinds {
		ak := ak // capture
		t.Run("kind_"+ak.Name, func(t *testing.T) {
			db := openTestDB(t)

			var agentID ptypes.AgentID
			switch ak.Kind {
			case 0: // human
				a, err := db.RegisterHumanAgent("test-ns", "Human", "h@example.com")
				if err != nil {
					t.Fatalf("RegisterHumanAgent error: %v", err)
				}
				agentID = a.ID
			case 1: // ml
				a, err := db.RegisterMLAgent("test-ns", ptypes.RoleWorker, ptypes.ProviderAnthropic, "claude_opus_4")
				if err != nil {
					t.Fatalf("RegisterMLAgent error: %v", err)
				}
				agentID = a.ID
			case 2: // software
				a, err := db.RegisterSoftwareAgent("test-ns", "tool", "1.0", "")
				if err != nil {
					t.Fatalf("RegisterSoftwareAgent error: %v", err)
				}
				agentID = a.ID
			default:
				t.Fatalf("unknown agent kind %d in fixtures", ak.Kind)
			}

			// For each getter, check success or ErrNotFound.
			for _, gf := range fix.AgentKindMismatch.GetterFunctions {
				gf := gf // capture
				t.Run("getter_"+gf.Name, func(t *testing.T) {
					expectMatch := ak.Kind == gf.Kind

					switch gf.Name {
					case "GetHumanAgent":
						_, err := db.GetHumanAgent(agentID)
						if expectMatch {
							if err != nil {
								t.Errorf("GetHumanAgent on human agent returned unexpected error: %v", err)
							}
						} else {
							if err == nil {
								t.Errorf("GetHumanAgent on %s agent expected ErrNotFound, got nil", ak.Name)
							} else if !errors.Is(err, ptypes.ErrNotFound) {
								t.Errorf("GetHumanAgent on %s agent error = %v, want ErrNotFound", ak.Name, err)
							}
						}

					case "GetMLAgent":
						_, err := db.GetMLAgent(agentID)
						if expectMatch {
							if err != nil {
								t.Errorf("GetMLAgent on ml agent returned unexpected error: %v", err)
							}
						} else {
							if err == nil {
								t.Errorf("GetMLAgent on %s agent expected ErrNotFound, got nil", ak.Name)
							} else if !errors.Is(err, ptypes.ErrNotFound) {
								t.Errorf("GetMLAgent on %s agent error = %v, want ErrNotFound", ak.Name, err)
							}
						}

					case "GetSoftwareAgent":
						_, err := db.GetSoftwareAgent(agentID)
						if expectMatch {
							if err != nil {
								t.Errorf("GetSoftwareAgent on software agent returned unexpected error: %v", err)
							}
						} else {
							if err == nil {
								t.Errorf("GetSoftwareAgent on %s agent expected ErrNotFound, got nil", ak.Name)
							} else if !errors.Is(err, ptypes.ErrNotFound) {
								t.Errorf("GetSoftwareAgent on %s agent error = %v, want ErrNotFound", ak.Name, err)
							}
						}

					default:
						t.Fatalf("unknown getter function %q in fixtures", gf.Name)
					}
				})
			}
		})
	}
}
