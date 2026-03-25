package providence_test

import (
	"errors"
	"testing"

	"github.com/dayvidpham/providence"
)

// openTestTracker returns a fresh in-memory Tracker for testing.
func openTestTracker(t *testing.T) providence.Tracker {
	t.Helper()
	tr, err := providence.OpenMemory()
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

func TestOpenMemory(t *testing.T) {
	tr, err := providence.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory() returned error: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}
}

func TestCreateAndShow(t *testing.T) {
	tr := openTestTracker(t)

	task, err := tr.Create("test-ns", "My Task", "A description", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if task.Title != "My Task" {
		t.Errorf("Title = %q, want %q", task.Title, "My Task")
	}
	if task.Description != "A description" {
		t.Errorf("Description = %q, want %q", task.Description, "A description")
	}
	if task.Status != providence.StatusOpen {
		t.Errorf("Status = %v, want StatusOpen", task.Status)
	}
	if task.Priority != providence.PriorityMedium {
		t.Errorf("Priority = %v, want PriorityMedium", task.Priority)
	}
	if task.Type != providence.TaskTypeTask {
		t.Errorf("Type = %v, want TaskTypeTask", task.Type)
	}
	if task.Phase != providence.PhaseUnscoped {
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

	a, err := tr.Create("ns", "Task A", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create A error: %v", err)
	}
	b, err := tr.Create("ns", "Task B", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create B error: %v", err)
	}

	if a.ID == b.ID {
		t.Errorf("Create produced duplicate IDs: %v", a.ID)
	}
}

func TestShowNotFound(t *testing.T) {
	tr := openTestTracker(t)

	fakeID, err := providence.ParseTaskID("ns--00000000-0000-7000-8000-000000000000")
	if err != nil {
		t.Fatalf("ParseTaskID error: %v", err)
	}

	_, err = tr.Show(fakeID)
	if !errors.Is(err, providence.ErrNotFound) {
		t.Errorf("Show non-existent task: got %v, want ErrNotFound", err)
	}
}

func TestUpdateTask(t *testing.T) {
	tr := openTestTracker(t)

	task, err := tr.Create("ns", "Old Title", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	newTitle := "New Title"
	updated, err := tr.Update(task.ID, providence.UpdateFields{Title: &newTitle})
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

	task, err := tr.Create("ns", "Close Me", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	closed, err := tr.CloseTask(task.ID, "done")
	if err != nil {
		t.Fatalf("CloseTask() error: %v", err)
	}
	if closed.Status != providence.StatusClosed {
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

	task, err := tr.Create("ns", "Double Close", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if _, err := tr.CloseTask(task.ID, "first close"); err != nil {
		t.Fatalf("First CloseTask() error: %v", err)
	}

	_, err = tr.CloseTask(task.ID, "second close")
	if !errors.Is(err, providence.ErrAlreadyClosed) {
		t.Errorf("Second CloseTask: got %v, want ErrAlreadyClosed", err)
	}
}

func TestAddEdgeBlockedBy(t *testing.T) {
	tr := openTestTracker(t)

	parent, err := tr.Create("ns", "Parent", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create parent error: %v", err)
	}
	child, err := tr.Create("ns", "Child", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create child error: %v", err)
	}

	// parent is blocked by child.
	if err := tr.AddEdge(parent.ID, child.ID.String(), providence.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge() error: %v", err)
	}

	kind := providence.EdgeBlockedBy
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

	a, err := tr.Create("ns", "A", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create A error: %v", err)
	}
	b, err := tr.Create("ns", "B", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create B error: %v", err)
	}

	// A blocked by B.
	if err := tr.AddEdge(a.ID, b.ID.String(), providence.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge A->B error: %v", err)
	}

	// B blocked by A — would form a cycle.
	err = tr.AddEdge(b.ID, a.ID.String(), providence.EdgeBlockedBy)
	if !errors.Is(err, providence.ErrCycleDetected) {
		t.Errorf("AddEdge B->A: got %v, want ErrCycleDetected", err)
	}
}

func TestReadyAndBlocked(t *testing.T) {
	tr := openTestTracker(t)

	parent, err := tr.Create("ns", "Parent", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create parent error: %v", err)
	}
	child, err := tr.Create("ns", "Child", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create child error: %v", err)
	}

	// parent blocked by child.
	if err := tr.AddEdge(parent.ID, child.ID.String(), providence.EdgeBlockedBy); err != nil {
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
	findID := func(tasks []providence.Task, id providence.TaskID) bool {
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

	task, err := tr.Create("ns", "Task", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
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

	task, err := tr.Create("ns", "Task", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
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
	if agent.Kind != providence.AgentKindHuman {
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

	// "claude_sonnet_4" is a model seeded in the schema at database creation time.
	agent, err := tr.RegisterMLAgent("ns", providence.RoleWorker, providence.ProviderAnthropic, "claude_sonnet_4")
	if err != nil {
		t.Fatalf("RegisterMLAgent() error: %v", err)
	}
	if agent.Role != providence.RoleWorker {
		t.Errorf("Role = %v, want RoleWorker", agent.Role)
	}
	if agent.Model.Provider != providence.ProviderAnthropic {
		t.Errorf("Provider = %v, want ProviderAnthropic", agent.Model.Provider)
	}
	if agent.Model.Name != "claude_sonnet_4" {
		t.Errorf("ModelName = %q, want %q", agent.Model.Name, "claude_sonnet_4")
	}
	if agent.Kind != providence.AgentKindMachineLearning {
		t.Errorf("Kind = %v, want AgentKindMachineLearning", agent.Kind)
	}

	// Retrieve via MLAgent.
	retrieved, err := tr.MLAgent(agent.ID)
	if err != nil {
		t.Fatalf("MLAgent() error: %v", err)
	}
	if retrieved.Model.Name != "claude_sonnet_4" {
		t.Errorf("Retrieved model name = %q, want %q", retrieved.Model.Name, "claude_sonnet_4")
	}
}

func TestStartAndEndActivity(t *testing.T) {
	tr := openTestTracker(t)

	agent, err := tr.RegisterHumanAgent("ns", "Worker", "")
	if err != nil {
		t.Fatalf("RegisterHumanAgent() error: %v", err)
	}

	act, err := tr.StartActivity(agent.ID, providence.PhaseWorkerSlices, providence.StageInProgress, "implementing slice 4")
	if err != nil {
		t.Fatalf("StartActivity() error: %v", err)
	}
	if act.AgentID != agent.ID {
		t.Errorf("AgentID = %v, want %v", act.AgentID, agent.ID)
	}
	if act.Phase != providence.PhaseWorkerSlices {
		t.Errorf("Phase = %v, want PhaseWorkerSlices", act.Phase)
	}
	if act.Stage != providence.StageInProgress {
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
	a, err := tr.Create("ns", "A", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	b, err := tr.Create("ns", "B", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}
	c, err := tr.Create("ns", "C", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create C: %v", err)
	}

	if err := tr.AddEdge(a.ID, b.ID.String(), providence.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge A->B: %v", err)
	}
	if err := tr.AddEdge(b.ID, c.ID.String(), providence.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge B->C: %v", err)
	}

	ancestors, err := tr.Ancestors(a.ID)
	if err != nil {
		t.Fatalf("Ancestors(A) error: %v", err)
	}

	containsTask := func(tasks []providence.Task, id providence.TaskID) bool {
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

	root, err := tr.Create("ns", "Root", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	dep1, err := tr.Create("ns", "Dep1", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create dep1: %v", err)
	}
	dep2, err := tr.Create("ns", "Dep2", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create dep2: %v", err)
	}
	dep3, err := tr.Create("ns", "Dep3", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create dep3: %v", err)
	}

	// root blocked by dep1, dep1 blocked by dep2, root blocked by dep3.
	if err := tr.AddEdge(root.ID, dep1.ID.String(), providence.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge root->dep1: %v", err)
	}
	if err := tr.AddEdge(dep1.ID, dep2.ID.String(), providence.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge dep1->dep2: %v", err)
	}
	if err := tr.AddEdge(root.ID, dep3.ID.String(), providence.EdgeBlockedBy); err != nil {
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

	_, err := tr.Create("ns", "Open Task", "", providence.TaskTypeTask, providence.PriorityMedium, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create open: %v", err)
	}
	closedTask, err := tr.Create("ns", "Closed Task", "", providence.TaskTypeBug, providence.PriorityHigh, providence.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create closed: %v", err)
	}
	if _, err := tr.CloseTask(closedTask.ID, "fixed"); err != nil {
		t.Fatalf("CloseTask: %v", err)
	}
	_, err = tr.Create("ns", "Feature Task", "", providence.TaskTypeFeature, providence.PriorityLow, providence.PhaseWorkerSlices)
	if err != nil {
		t.Fatalf("Create feature: %v", err)
	}

	// No filter: should return all 3.
	all, err := tr.List(providence.ListFilter{})
	if err != nil {
		t.Fatalf("List (no filter) error: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("List() returned %d tasks, want 3", len(all))
	}

	// Filter by status = open.
	openStatus := providence.StatusOpen
	open, err := tr.List(providence.ListFilter{Status: &openStatus})
	if err != nil {
		t.Fatalf("List (open) error: %v", err)
	}
	if len(open) != 2 {
		t.Errorf("List(open) returned %d tasks, want 2", len(open))
	}

	// Filter by phase = worker_slices.
	phase := providence.PhaseWorkerSlices
	byPhase, err := tr.List(providence.ListFilter{Phase: &phase})
	if err != nil {
		t.Fatalf("List (phase) error: %v", err)
	}
	if len(byPhase) != 1 {
		t.Errorf("List(phase=worker_slices) returned %d tasks, want 1", len(byPhase))
	}
}
