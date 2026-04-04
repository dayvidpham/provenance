package provenance_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/dayvidpham/provenance"
)

// ---------------------------------------------------------------------------
// Shared test fixtures (M5)
// ---------------------------------------------------------------------------

// openTestTracker returns a fresh in-memory Tracker for testing.
// The tracker is closed automatically when the test ends.
func openTestTracker(t *testing.T) provenance.Tracker {
	t.Helper()
	tr, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory() failed: %v", err)
	}
	t.Cleanup(func() {
		if err := tr.Close(); err != nil {
			t.Errorf("tracker.Close() failed: %v", err)
		}
	})
	return tr
}

// mustCreateTask creates a task with sensible defaults and fatals on error.
// Uses TaskTypeTask, PriorityMedium, and PhaseUnscoped as defaults.
func mustCreateTask(t *testing.T, tr provenance.Tracker, namespace string) provenance.Task {
	t.Helper()
	task, err := tr.Create(namespace, "Test Task", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("mustCreateTask(%q): Create() failed: %v", namespace, err)
	}
	return task
}

func TestOpenMemory(t *testing.T) {
	tr, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory() returned error: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}
}

func TestCreateAndShow(t *testing.T) {
	tr := openTestTracker(t)

	task, err := tr.Create("test-ns", "My Task", "A description", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if task.Title != "My Task" {
		t.Errorf("Title = %q, want %q", task.Title, "My Task")
	}
	if task.Description != "A description" {
		t.Errorf("Description = %q, want %q", task.Description, "A description")
	}
	if task.Status != provenance.StatusOpen {
		t.Errorf("Status = %v, want StatusOpen", task.Status)
	}
	if task.Priority != provenance.PriorityMedium {
		t.Errorf("Priority = %v, want PriorityMedium", task.Priority)
	}
	if task.Type != provenance.TaskTypeTask {
		t.Errorf("Type = %v, want TaskTypeTask", task.Type)
	}
	if task.Phase != provenance.PhaseUnscoped {
		t.Errorf("Phase = %v, want PhaseUnscoped", task.Phase)
	}
	if task.ID.Namespace != "test-ns" {
		t.Errorf("Namespace = %q, want %q", task.ID.Namespace, "test-ns")
	}

	// Show returns the same task.
	got, err := tr.Show(task.ID)
	if err != nil {
		t.Fatalf("Show() error: %v", err)
	}
	if got.ID != task.ID {
		t.Errorf("Show ID = %v, want %v", got.ID, task.ID)
	}
	if got.Title != task.Title {
		t.Errorf("Show Title = %q, want %q", got.Title, task.Title)
	}
}

func TestCreateGeneratesUUIDv7(t *testing.T) {
	tr := openTestTracker(t)

	a, err := tr.Create("ns", "Task A", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create A error: %v", err)
	}
	b, err := tr.Create("ns", "Task B", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create B error: %v", err)
	}

	if a.ID == b.ID {
		t.Errorf("Create produced duplicate IDs: %v", a.ID)
	}
}

func TestShowNotFound(t *testing.T) {
	tr := openTestTracker(t)

	fakeID, err := provenance.ParseTaskID("ns--00000000-0000-7000-8000-000000000000")
	if err != nil {
		t.Fatalf("ParseTaskID error: %v", err)
	}

	_, err = tr.Show(fakeID)
	if !errors.Is(err, provenance.ErrNotFound) {
		t.Errorf("Show non-existent task: got %v, want ErrNotFound", err)
	}
}

func TestUpdateTask(t *testing.T) {
	tr := openTestTracker(t)

	task, err := tr.Create("ns", "Old Title", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	newTitle := "New Title"
	updated, err := tr.Update(task.ID, provenance.UpdateFields{Title: &newTitle})
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	if updated.Title != "New Title" {
		t.Errorf("Updated title = %q, want %q", updated.Title, "New Title")
	}
	if !updated.UpdatedAt.After(task.UpdatedAt) && updated.UpdatedAt != task.UpdatedAt {
		// UpdatedAt should be >= original. In rapid tests they may be equal nanoseconds.
		// Just check it didn't go backwards.
		if updated.UpdatedAt.Before(task.UpdatedAt) {
			t.Errorf("UpdatedAt went backwards: %v < %v", updated.UpdatedAt, task.UpdatedAt)
		}
	}
}

func TestCloseTask(t *testing.T) {
	tr := openTestTracker(t)

	task, err := tr.Create("ns", "Close Me", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	closed, err := tr.CloseTask(task.ID, "done")
	if err != nil {
		t.Fatalf("CloseTask() error: %v", err)
	}
	if closed.Status != provenance.StatusClosed {
		t.Errorf("Status = %v, want StatusClosed", closed.Status)
	}
	if closed.ClosedAt == nil {
		t.Error("ClosedAt is nil after close")
	}
	if closed.CloseReason != "done" {
		t.Errorf("CloseReason = %q, want %q", closed.CloseReason, "done")
	}
}

func TestCloseTaskAlreadyClosed(t *testing.T) {
	tr := openTestTracker(t)

	task, err := tr.Create("ns", "Double Close", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if _, err := tr.CloseTask(task.ID, "first close"); err != nil {
		t.Fatalf("First CloseTask() error: %v", err)
	}

	_, err = tr.CloseTask(task.ID, "second close")
	if !errors.Is(err, provenance.ErrAlreadyClosed) {
		t.Errorf("Second CloseTask: got %v, want ErrAlreadyClosed", err)
	}
}

func TestAddEdgeBlockedBy(t *testing.T) {
	tr := openTestTracker(t)

	parent, err := tr.Create("ns", "Parent", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create parent error: %v", err)
	}
	child, err := tr.Create("ns", "Child", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create child error: %v", err)
	}

	// parent is blocked by child.
	if err := tr.AddEdge(parent.ID, child.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge() error: %v", err)
	}

	kind := provenance.EdgeBlockedBy
	edges, err := tr.Edges(parent.ID, &kind)
	if err != nil {
		t.Fatalf("Edges() error: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("Edges() returned %d edges, want 1", len(edges))
	}
	if edges[0].TargetID != child.ID.String() {
		t.Errorf("Edge target = %q, want %q", edges[0].TargetID, child.ID.String())
	}
}

func TestAddEdgeCycleDetected(t *testing.T) {
	tr := openTestTracker(t)

	a, err := tr.Create("ns", "A", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create A error: %v", err)
	}
	b, err := tr.Create("ns", "B", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create B error: %v", err)
	}

	// A blocked by B.
	if err := tr.AddEdge(a.ID, b.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge A->B error: %v", err)
	}

	// B blocked by A — would form a cycle.
	err = tr.AddEdge(b.ID, a.ID.String(), provenance.EdgeBlockedBy)
	if !errors.Is(err, provenance.ErrCycleDetected) {
		t.Errorf("AddEdge B->A: got %v, want ErrCycleDetected", err)
	}
}

func TestReadyAndBlocked(t *testing.T) {
	tr := openTestTracker(t)

	parent, err := tr.Create("ns", "Parent", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create parent error: %v", err)
	}
	child, err := tr.Create("ns", "Child", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create child error: %v", err)
	}

	// parent blocked by child.
	if err := tr.AddEdge(parent.ID, child.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge error: %v", err)
	}

	ready, err := tr.Ready()
	if err != nil {
		t.Fatalf("Ready() error: %v", err)
	}
	blocked, err := tr.Blocked()
	if err != nil {
		t.Fatalf("Blocked() error: %v", err)
	}

	// child should be ready; parent should be blocked.
	findID := func(tasks []provenance.Task, id provenance.TaskID) bool {
		for _, t := range tasks {
			if t.ID == id {
				return true
			}
		}
		return false
	}

	if !findID(ready, child.ID) {
		t.Errorf("child not in Ready() list")
	}
	if findID(ready, parent.ID) {
		t.Errorf("parent unexpectedly in Ready() list (should be blocked)")
	}
	if !findID(blocked, parent.ID) {
		t.Errorf("parent not in Blocked() list")
	}
	if findID(blocked, child.ID) {
		t.Errorf("child unexpectedly in Blocked() list (should be ready)")
	}

	// Close child — parent should become ready.
	if _, err := tr.CloseTask(child.ID, "done"); err != nil {
		t.Fatalf("CloseTask(child) error: %v", err)
	}

	ready2, err := tr.Ready()
	if err != nil {
		t.Fatalf("Ready() after close error: %v", err)
	}
	if !findID(ready2, parent.ID) {
		t.Errorf("parent not ready after child is closed")
	}
}

func TestAddLabel(t *testing.T) {
	tr := openTestTracker(t)

	task, err := tr.Create("ns", "Task", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if err := tr.AddLabel(task.ID, "urgent"); err != nil {
		t.Fatalf("AddLabel() error: %v", err)
	}
	if err := tr.AddLabel(task.ID, "backend"); err != nil {
		t.Fatalf("AddLabel() error: %v", err)
	}

	labels, err := tr.Labels(task.ID)
	if err != nil {
		t.Fatalf("Labels() error: %v", err)
	}

	hasLabel := func(ls []string, want string) bool {
		for _, l := range ls {
			if l == want {
				return true
			}
		}
		return false
	}

	if !hasLabel(labels, "urgent") {
		t.Errorf("label 'urgent' not found in %v", labels)
	}
	if !hasLabel(labels, "backend") {
		t.Errorf("label 'backend' not found in %v", labels)
	}
}

func TestAddComment(t *testing.T) {
	tr := openTestTracker(t)

	agent, err := tr.RegisterHumanAgent("ns", "Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("RegisterHumanAgent() error: %v", err)
	}

	task, err := tr.Create("ns", "Task", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	comment, err := tr.AddComment(task.ID, agent.ID, "first comment")
	if err != nil {
		t.Fatalf("AddComment() error: %v", err)
	}
	if comment.Body != "first comment" {
		t.Errorf("Body = %q, want %q", comment.Body, "first comment")
	}
	if comment.AuthorID != agent.ID {
		t.Errorf("AuthorID = %v, want %v", comment.AuthorID, agent.ID)
	}
	if comment.TaskID != task.ID {
		t.Errorf("TaskID = %v, want %v", comment.TaskID, task.ID)
	}

	comments, err := tr.Comments(task.ID)
	if err != nil {
		t.Fatalf("Comments() error: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("Comments() returned %d, want 1", len(comments))
	}
	if comments[0].Body != "first comment" {
		t.Errorf("Comment body = %q, want %q", comments[0].Body, "first comment")
	}
}

func TestRegisterHumanAgent(t *testing.T) {
	tr := openTestTracker(t)

	agent, err := tr.RegisterHumanAgent("ns", "Bob", "bob@example.com")
	if err != nil {
		t.Fatalf("RegisterHumanAgent() error: %v", err)
	}
	if agent.Name != "Bob" {
		t.Errorf("Name = %q, want %q", agent.Name, "Bob")
	}
	if agent.Contact != "bob@example.com" {
		t.Errorf("Contact = %q, want %q", agent.Contact, "bob@example.com")
	}
	if agent.Kind != provenance.AgentKindHuman {
		t.Errorf("Kind = %v, want AgentKindHuman", agent.Kind)
	}
	if agent.ID.Namespace != "ns" {
		t.Errorf("Namespace = %q, want %q", agent.ID.Namespace, "ns")
	}

	// Retrieve via HumanAgent.
	retrieved, err := tr.HumanAgent(agent.ID)
	if err != nil {
		t.Fatalf("HumanAgent() error: %v", err)
	}
	if retrieved.Name != "Bob" {
		t.Errorf("Retrieved name = %q, want %q", retrieved.Name, "Bob")
	}
}

func TestRegisterMLAgent(t *testing.T) {
	tr := openTestTracker(t)

	// "claude-sonnet-4-6" is a model seeded in the schema at database creation time.
	agent, err := tr.RegisterMLAgent("ns", provenance.RoleWorker, provenance.ProviderAnthropic, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("RegisterMLAgent() error: %v", err)
	}
	if agent.Role != provenance.RoleWorker {
		t.Errorf("Role = %v, want RoleWorker", agent.Role)
	}
	if agent.Model.Provider != provenance.ProviderAnthropic {
		t.Errorf("Provider = %v, want ProviderAnthropic", agent.Model.Provider)
	}
	if agent.Model.Name != "claude-sonnet-4-6" {
		t.Errorf("ModelName = %q, want %q", agent.Model.Name, "claude-sonnet-4-6")
	}
	if agent.Kind != provenance.AgentKindMachineLearning {
		t.Errorf("Kind = %v, want AgentKindMachineLearning", agent.Kind)
	}

	// Retrieve via MLAgent.
	retrieved, err := tr.MLAgent(agent.ID)
	if err != nil {
		t.Fatalf("MLAgent() error: %v", err)
	}
	if retrieved.Model.Name != "claude-sonnet-4-6" {
		t.Errorf("Retrieved model name = %q, want %q", retrieved.Model.Name, "claude-sonnet-4-6")
	}
}

func TestStartAndEndActivity(t *testing.T) {
	tr := openTestTracker(t)

	agent, err := tr.RegisterHumanAgent("ns", "Worker", "")
	if err != nil {
		t.Fatalf("RegisterHumanAgent() error: %v", err)
	}

	act, err := tr.StartActivity(agent.ID, provenance.PhaseWorkerSlices, provenance.StageInProgress, "implementing slice 4")
	if err != nil {
		t.Fatalf("StartActivity() error: %v", err)
	}
	if act.AgentID != agent.ID {
		t.Errorf("AgentID = %v, want %v", act.AgentID, agent.ID)
	}
	if act.Phase != provenance.PhaseWorkerSlices {
		t.Errorf("Phase = %v, want PhaseWorkerSlices", act.Phase)
	}
	if act.Stage != provenance.StageInProgress {
		t.Errorf("Stage = %v, want StageInProgress", act.Stage)
	}
	if act.EndedAt != nil {
		t.Errorf("EndedAt should be nil at start, got %v", act.EndedAt)
	}

	ended, err := tr.EndActivity(act.ID)
	if err != nil {
		t.Fatalf("EndActivity() error: %v", err)
	}
	if ended.EndedAt == nil {
		t.Error("EndedAt is nil after EndActivity")
	}
	if ended.ID != act.ID {
		t.Errorf("EndActivity ID = %v, want %v", ended.ID, act.ID)
	}
}

func TestAncestorsAndDescendants(t *testing.T) {
	tr := openTestTracker(t)

	// Chain: A blocked by B blocked by C.
	// Ancestors of A = {B, C}, Descendants of C = {A, B}.
	a, err := tr.Create("ns", "A", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	b, err := tr.Create("ns", "B", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}
	c, err := tr.Create("ns", "C", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create C: %v", err)
	}

	if err := tr.AddEdge(a.ID, b.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge A->B: %v", err)
	}
	if err := tr.AddEdge(b.ID, c.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge B->C: %v", err)
	}

	ancestors, err := tr.Ancestors(a.ID)
	if err != nil {
		t.Fatalf("Ancestors(A) error: %v", err)
	}

	containsTask := func(tasks []provenance.Task, id provenance.TaskID) bool {
		for _, t := range tasks {
			if t.ID == id {
				return true
			}
		}
		return false
	}

	if !containsTask(ancestors, b.ID) {
		t.Errorf("B not in Ancestors(A): %v", ancestors)
	}
	if !containsTask(ancestors, c.ID) {
		t.Errorf("C not in Ancestors(A): %v", ancestors)
	}
	if containsTask(ancestors, a.ID) {
		t.Errorf("A should not be in its own ancestors")
	}

	descendants, err := tr.Descendants(c.ID)
	if err != nil {
		t.Fatalf("Descendants(C) error: %v", err)
	}

	if !containsTask(descendants, a.ID) {
		t.Errorf("A not in Descendants(C): %v", descendants)
	}
	if !containsTask(descendants, b.ID) {
		t.Errorf("B not in Descendants(C): %v", descendants)
	}
	if containsTask(descendants, c.ID) {
		t.Errorf("C should not be in its own descendants")
	}
}

func TestDepTree(t *testing.T) {
	tr := openTestTracker(t)

	root, err := tr.Create("ns", "Root", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	dep1, err := tr.Create("ns", "Dep1", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create dep1: %v", err)
	}
	dep2, err := tr.Create("ns", "Dep2", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create dep2: %v", err)
	}
	dep3, err := tr.Create("ns", "Dep3", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create dep3: %v", err)
	}

	// root blocked by dep1, dep1 blocked by dep2, root blocked by dep3.
	if err := tr.AddEdge(root.ID, dep1.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge root->dep1: %v", err)
	}
	if err := tr.AddEdge(dep1.ID, dep2.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge dep1->dep2: %v", err)
	}
	if err := tr.AddEdge(root.ID, dep3.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge root->dep3: %v", err)
	}

	tree, err := tr.DepTree(root.ID)
	if err != nil {
		t.Fatalf("DepTree() error: %v", err)
	}
	if len(tree) != 3 {
		t.Errorf("DepTree() returned %d edges, want 3", len(tree))
	}
}

func TestList(t *testing.T) {
	tr := openTestTracker(t)

	_, err := tr.Create("ns", "Open Task", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create open: %v", err)
	}
	closedTask, err := tr.Create("ns", "Closed Task", "", provenance.TaskTypeBug, provenance.PriorityHigh, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create closed: %v", err)
	}
	if _, err := tr.CloseTask(closedTask.ID, "fixed"); err != nil {
		t.Fatalf("CloseTask: %v", err)
	}
	_, err = tr.Create("ns", "Feature Task", "", provenance.TaskTypeFeature, provenance.PriorityLow, provenance.PhaseWorkerSlices)
	if err != nil {
		t.Fatalf("Create feature: %v", err)
	}

	// No filter: should return all 3.
	all, err := tr.List(provenance.ListFilter{})
	if err != nil {
		t.Fatalf("List (no filter) error: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("List() returned %d tasks, want 3", len(all))
	}

	// Filter by status = open.
	openStatus := provenance.StatusOpen
	open, err := tr.List(provenance.ListFilter{Status: &openStatus})
	if err != nil {
		t.Fatalf("List (open) error: %v", err)
	}
	if len(open) != 2 {
		t.Errorf("List(open) returned %d tasks, want 2", len(open))
	}

	// Filter by phase = worker_slices.
	phase := provenance.PhaseWorkerSlices
	byPhase, err := tr.List(provenance.ListFilter{Phase: &phase})
	if err != nil {
		t.Fatalf("List (phase) error: %v", err)
	}
	if len(byPhase) != 1 {
		t.Errorf("List(phase=worker_slices) returned %d tasks, want 1", len(byPhase))
	}
}

// ===========================================================================
// Part A: 6 Integration Tests
// ===========================================================================

// TestRegisterSoftwareAgent registers a software agent, retrieves it via
// SoftwareAgent(), and verifies all fields match.
func TestRegisterSoftwareAgent(t *testing.T) {
	tr := openTestTracker(t)

	sa, err := tr.RegisterSoftwareAgent("ns", "provenance-cli", "v0.1.0", "https://github.com/dayvidpham/provenance")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent() error: %v", err)
	}

	// Verify returned fields.
	if sa.Kind != provenance.AgentKindSoftware {
		t.Errorf("Kind = %v, want AgentKindSoftware", sa.Kind)
	}
	if sa.Name != "provenance-cli" {
		t.Errorf("Name = %q, want %q", sa.Name, "provenance-cli")
	}
	if sa.Version != "v0.1.0" {
		t.Errorf("Version = %q, want %q", sa.Version, "v0.1.0")
	}
	if sa.Source != "https://github.com/dayvidpham/provenance" {
		t.Errorf("Source = %q, want %q", sa.Source, "https://github.com/dayvidpham/provenance")
	}
	if sa.ID.Namespace != "ns" {
		t.Errorf("Namespace = %q, want %q", sa.ID.Namespace, "ns")
	}

	// Retrieve via SoftwareAgent() and verify round-trip.
	retrieved, err := tr.SoftwareAgent(sa.ID)
	if err != nil {
		t.Fatalf("SoftwareAgent() error: %v", err)
	}
	if retrieved.Name != sa.Name {
		t.Errorf("Retrieved Name = %q, want %q", retrieved.Name, sa.Name)
	}
	if retrieved.Version != sa.Version {
		t.Errorf("Retrieved Version = %q, want %q", retrieved.Version, sa.Version)
	}
	if retrieved.Source != sa.Source {
		t.Errorf("Retrieved Source = %q, want %q", retrieved.Source, sa.Source)
	}
	if retrieved.Kind != provenance.AgentKindSoftware {
		t.Errorf("Retrieved Kind = %v, want AgentKindSoftware", retrieved.Kind)
	}
}

// TestAgent registers agents of each kind and verifies the base Agent()
// method returns the correct AgentKind.
func TestAgent(t *testing.T) {
	tr := openTestTracker(t)

	// Register one of each kind.
	human, err := tr.RegisterHumanAgent("ns", "Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("RegisterHumanAgent() error: %v", err)
	}
	ml, err := tr.RegisterMLAgent("ns", provenance.RoleReviewer, provenance.ProviderAnthropic, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("RegisterMLAgent() error: %v", err)
	}
	sw, err := tr.RegisterSoftwareAgent("ns", "lint-tool", "v2.0.0", "/usr/local/bin/lint")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent() error: %v", err)
	}

	// Verify Agent() for each returns the correct Kind.
	cases := []struct {
		name     string
		id       provenance.AgentID
		wantKind provenance.AgentKind
	}{
		{"human", human.ID, provenance.AgentKindHuman},
		{"ml", ml.ID, provenance.AgentKindMachineLearning},
		{"software", sw.ID, provenance.AgentKindSoftware},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, err := tr.Agent(tc.id)
			if err != nil {
				t.Fatalf("Agent(%v) error: %v", tc.id, err)
			}
			if agent.Kind != tc.wantKind {
				t.Errorf("Agent(%v).Kind = %v, want %v", tc.id, agent.Kind, tc.wantKind)
			}
			if agent.ID != tc.id {
				t.Errorf("Agent(%v).ID = %v, want %v", tc.id, agent.ID, tc.id)
			}
		})
	}
}

// TestActivities starts and ends an activity, then calls Activities() to list
// activities and verifies the returned list contains the activity.
func TestActivities(t *testing.T) {
	tr := openTestTracker(t)

	agent, err := tr.RegisterHumanAgent("ns", "Worker", "")
	if err != nil {
		t.Fatalf("RegisterHumanAgent() error: %v", err)
	}

	// Start an activity.
	act, err := tr.StartActivity(agent.ID, provenance.PhaseCodeReview, provenance.StageInProgress, "reviewing PR")
	if err != nil {
		t.Fatalf("StartActivity() error: %v", err)
	}

	// End the activity.
	ended, err := tr.EndActivity(act.ID)
	if err != nil {
		t.Fatalf("EndActivity() error: %v", err)
	}
	if ended.EndedAt == nil {
		t.Fatal("EndedAt should be non-nil after EndActivity")
	}

	// List activities for this agent.
	activities, err := tr.Activities(&agent.ID)
	if err != nil {
		t.Fatalf("Activities() error: %v", err)
	}
	if len(activities) != 1 {
		t.Fatalf("Activities() returned %d, want 1", len(activities))
	}
	if activities[0].ID != act.ID {
		t.Errorf("Activities()[0].ID = %v, want %v", activities[0].ID, act.ID)
	}
	if activities[0].Phase != provenance.PhaseCodeReview {
		t.Errorf("Activities()[0].Phase = %v, want PhaseCodeReview", activities[0].Phase)
	}
	if activities[0].Notes != "reviewing PR" {
		t.Errorf("Activities()[0].Notes = %q, want %q", activities[0].Notes, "reviewing PR")
	}
	if activities[0].EndedAt == nil {
		t.Error("Activities()[0].EndedAt should be non-nil")
	}

	// List all activities (nil filter).
	all, err := tr.Activities(nil)
	if err != nil {
		t.Fatalf("Activities(nil) error: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("Activities(nil) returned %d, want 1", len(all))
	}
}

// TestRemoveEdge creates two tasks, adds a blocked-by edge, verifies it exists,
// removes it, and verifies it is gone.
func TestRemoveEdge(t *testing.T) {
	tr := openTestTracker(t)

	parent := mustCreateTask(t, tr, "ns")
	child := mustCreateTask(t, tr, "ns")

	// Add edge.
	if err := tr.AddEdge(parent.ID, child.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge() error: %v", err)
	}

	// Verify edge exists.
	kind := provenance.EdgeBlockedBy
	edges, err := tr.Edges(parent.ID, &kind)
	if err != nil {
		t.Fatalf("Edges() error: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("Edges() returned %d, want 1", len(edges))
	}

	// Remove edge.
	if err := tr.RemoveEdge(parent.ID, child.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("RemoveEdge() error: %v", err)
	}

	// Verify edge is gone.
	edges2, err := tr.Edges(parent.ID, &kind)
	if err != nil {
		t.Fatalf("Edges() after remove error: %v", err)
	}
	if len(edges2) != 0 {
		t.Errorf("Edges() after RemoveEdge returned %d, want 0", len(edges2))
	}

	// RemoveEdge on non-existent edge is idempotent (no error).
	if err := tr.RemoveEdge(parent.ID, child.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Errorf("RemoveEdge() on non-existent edge: got error %v, want nil", err)
	}
}

// TestNonBlockedByEdges verifies that non-BlockedBy edge kinds (EdgeDerivedFrom,
// EdgeSupersedes) do NOT affect task readiness queries (Blocked/Ready).
func TestNonBlockedByEdges(t *testing.T) {
	tr := openTestTracker(t)

	taskA := mustCreateTask(t, tr, "ns")
	taskB := mustCreateTask(t, tr, "ns")

	// Add a DerivedFrom edge: A derived from B.
	if err := tr.AddEdge(taskA.ID, taskB.ID.String(), provenance.EdgeDerivedFrom); err != nil {
		t.Fatalf("AddEdge(DerivedFrom) error: %v", err)
	}
	// Add a Supersedes edge: A supersedes B.
	if err := tr.AddEdge(taskA.ID, taskB.ID.String(), provenance.EdgeSupersedes); err != nil {
		t.Fatalf("AddEdge(Supersedes) error: %v", err)
	}

	// Verify the edges were created.
	derivedKind := provenance.EdgeDerivedFrom
	derivedEdges, err := tr.Edges(taskA.ID, &derivedKind)
	if err != nil {
		t.Fatalf("Edges(DerivedFrom) error: %v", err)
	}
	if len(derivedEdges) != 1 {
		t.Fatalf("Edges(DerivedFrom) returned %d, want 1", len(derivedEdges))
	}
	supersedesKind := provenance.EdgeSupersedes
	supersedesEdges, err := tr.Edges(taskA.ID, &supersedesKind)
	if err != nil {
		t.Fatalf("Edges(Supersedes) error: %v", err)
	}
	if len(supersedesEdges) != 1 {
		t.Fatalf("Edges(Supersedes) returned %d, want 1", len(supersedesEdges))
	}

	// Both tasks should still be ready (non-BlockedBy edges don't affect readiness).
	ready, err := tr.Ready()
	if err != nil {
		t.Fatalf("Ready() error: %v", err)
	}
	blocked, err := tr.Blocked()
	if err != nil {
		t.Fatalf("Blocked() error: %v", err)
	}

	containsTask := func(tasks []provenance.Task, id provenance.TaskID) bool {
		for _, tk := range tasks {
			if tk.ID == id {
				return true
			}
		}
		return false
	}

	if !containsTask(ready, taskA.ID) {
		t.Errorf("taskA should be in Ready() — non-BlockedBy edges must not affect readiness")
	}
	if !containsTask(ready, taskB.ID) {
		t.Errorf("taskB should be in Ready() — non-BlockedBy edges must not affect readiness")
	}
	if containsTask(blocked, taskA.ID) {
		t.Errorf("taskA should NOT be in Blocked() — non-BlockedBy edges must not affect readiness")
	}
	if containsTask(blocked, taskB.ID) {
		t.Errorf("taskB should NOT be in Blocked() — non-BlockedBy edges must not affect readiness")
	}
}

// TestRemoveLabel verifies AddLabel → Labels → RemoveLabel → Labels round-trip,
// and that RemoveLabel is idempotent (no error on double-remove).
func TestRemoveLabel(t *testing.T) {
	tr := openTestTracker(t)
	task := mustCreateTask(t, tr, "ns")

	// Add labels.
	if err := tr.AddLabel(task.ID, "priority:high"); err != nil {
		t.Fatalf("AddLabel() error: %v", err)
	}
	if err := tr.AddLabel(task.ID, "team:backend"); err != nil {
		t.Fatalf("AddLabel() error: %v", err)
	}

	// Verify both exist.
	labels, err := tr.Labels(task.ID)
	if err != nil {
		t.Fatalf("Labels() error: %v", err)
	}
	if len(labels) != 2 {
		t.Fatalf("Labels() returned %d, want 2", len(labels))
	}

	// Remove one label.
	if err := tr.RemoveLabel(task.ID, "priority:high"); err != nil {
		t.Fatalf("RemoveLabel() error: %v", err)
	}

	// Verify only one remains.
	labels2, err := tr.Labels(task.ID)
	if err != nil {
		t.Fatalf("Labels() after remove error: %v", err)
	}
	if len(labels2) != 1 {
		t.Fatalf("Labels() after RemoveLabel returned %d, want 1", len(labels2))
	}
	if labels2[0] != "team:backend" {
		t.Errorf("Remaining label = %q, want %q", labels2[0], "team:backend")
	}

	// Remove the same label again — should be idempotent (no error).
	if err := tr.RemoveLabel(task.ID, "priority:high"); err != nil {
		t.Errorf("RemoveLabel() on non-existent label: got error %v, want nil", err)
	}

	// Verify labels unchanged after idempotent remove.
	labels3, err := tr.Labels(task.ID)
	if err != nil {
		t.Fatalf("Labels() after idempotent remove error: %v", err)
	}
	if len(labels3) != 1 {
		t.Errorf("Labels() after idempotent RemoveLabel returned %d, want 1", len(labels3))
	}
}

// ===========================================================================
// Part B: Minor Cleanups — Tests
// ===========================================================================

// TestCreateEmptyNamespace verifies that Create rejects an empty namespace (M3).
func TestCreateEmptyNamespace(t *testing.T) {
	tr := openTestTracker(t)

	_, err := tr.Create("", "Title", "Desc", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err == nil {
		t.Fatal("Create() with empty namespace: expected error, got nil")
	}
	if !errors.Is(err, provenance.ErrInvalidID) {
		t.Errorf("Create() with empty namespace: got %v, want ErrInvalidID", err)
	}
}

// TestConcurrentCreate verifies that 10 goroutines each doing 20 Create
// operations do not race and all 200 tasks are created successfully (M4).
func TestConcurrentCreate(t *testing.T) {
	tr := openTestTracker(t)

	const goroutines = 10
	const opsPerGoroutine = 20
	const totalOps = goroutines * opsPerGoroutine

	var wg sync.WaitGroup
	errs := make(chan error, totalOps)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gID int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				title := fmt.Sprintf("task-g%d-i%d", gID, i)
				_, err := tr.Create("concurrent-ns", title, "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
				if err != nil {
					errs <- fmt.Errorf("goroutine %d, op %d: %w", gID, i, err)
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Create error: %v", err)
	}

	// Verify all 200 tasks were created.
	tasks, err := tr.List(provenance.ListFilter{Namespace: "concurrent-ns"})
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(tasks) != totalOps {
		t.Errorf("List() returned %d tasks, want %d", len(tasks), totalOps)
	}
}

// TestListFilterByLabel verifies that ListFilter.Label filters correctly (M6).
func TestListFilterByLabel(t *testing.T) {
	tr := openTestTracker(t)

	taskA := mustCreateTask(t, tr, "label-ns")
	taskB := mustCreateTask(t, tr, "label-ns")
	_ = mustCreateTask(t, tr, "label-ns") // taskC — no label

	if err := tr.AddLabel(taskA.ID, "urgent"); err != nil {
		t.Fatalf("AddLabel(A, urgent) error: %v", err)
	}
	if err := tr.AddLabel(taskB.ID, "urgent"); err != nil {
		t.Fatalf("AddLabel(B, urgent) error: %v", err)
	}
	if err := tr.AddLabel(taskA.ID, "backend"); err != nil {
		t.Fatalf("AddLabel(A, backend) error: %v", err)
	}

	// Filter by label "urgent" — should return A and B.
	urgentTasks, err := tr.List(provenance.ListFilter{Label: "urgent"})
	if err != nil {
		t.Fatalf("List(label=urgent) error: %v", err)
	}
	if len(urgentTasks) != 2 {
		t.Errorf("List(label=urgent) returned %d tasks, want 2", len(urgentTasks))
	}

	// Filter by label "backend" — should return only A.
	backendTasks, err := tr.List(provenance.ListFilter{Label: "backend"})
	if err != nil {
		t.Fatalf("List(label=backend) error: %v", err)
	}
	if len(backendTasks) != 1 {
		t.Errorf("List(label=backend) returned %d tasks, want 1", len(backendTasks))
	}

	// Filter by non-existent label — should return zero.
	noneTasks, err := tr.List(provenance.ListFilter{Label: "nonexistent"})
	if err != nil {
		t.Fatalf("List(label=nonexistent) error: %v", err)
	}
	if len(noneTasks) != 0 {
		t.Errorf("List(label=nonexistent) returned %d tasks, want 0", len(noneTasks))
	}
}

// TestListFilterByNamespace verifies that ListFilter.Namespace filters correctly (M6).
func TestListFilterByNamespace(t *testing.T) {
	tr := openTestTracker(t)

	_ = mustCreateTask(t, tr, "alpha")
	_ = mustCreateTask(t, tr, "alpha")
	_ = mustCreateTask(t, tr, "beta")

	// Filter by namespace "alpha" — should return 2.
	alphaTasks, err := tr.List(provenance.ListFilter{Namespace: "alpha"})
	if err != nil {
		t.Fatalf("List(namespace=alpha) error: %v", err)
	}
	if len(alphaTasks) != 2 {
		t.Errorf("List(namespace=alpha) returned %d tasks, want 2", len(alphaTasks))
	}

	// Filter by namespace "beta" — should return 1.
	betaTasks, err := tr.List(provenance.ListFilter{Namespace: "beta"})
	if err != nil {
		t.Fatalf("List(namespace=beta) error: %v", err)
	}
	if len(betaTasks) != 1 {
		t.Errorf("List(namespace=beta) returned %d tasks, want 1", len(betaTasks))
	}

	// Filter by non-existent namespace — should return 0.
	noneTasks, err := tr.List(provenance.ListFilter{Namespace: "gamma"})
	if err != nil {
		t.Fatalf("List(namespace=gamma) error: %v", err)
	}
	if len(noneTasks) != 0 {
		t.Errorf("List(namespace=gamma) returned %d tasks, want 0", len(noneTasks))
	}

	// No namespace filter — should return all 3.
	allTasks, err := tr.List(provenance.ListFilter{})
	if err != nil {
		t.Fatalf("List(no filter) error: %v", err)
	}
	if len(allTasks) != 3 {
		t.Errorf("List(no filter) returned %d tasks, want 3", len(allTasks))
	}
}
