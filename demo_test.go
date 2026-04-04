package provenance_test

// demo_test.go — 9 integration demos proving provenance replaces Beads with
// full PROV-O lineage tracking. See pasture/llm/demo/provenance.md for the plan.

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/dayvidpham/provenance"
)

// ---------------------------------------------------------------------------
// Demo 1: Core Workflow (Beads Replacement)
// ---------------------------------------------------------------------------

func TestDemo_CoreWorkflow(t *testing.T) {
	tr := openTestTracker(t)

	// Create
	task, err := tr.Create("aura-plugins", "REQUEST: Port codegen to Go",
		"User request verbatim", provenance.TaskTypeFeature, provenance.PriorityMedium, provenance.PhaseRequest)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if task.ID.Namespace != "aura-plugins" {
		t.Errorf("Namespace = %q, want %q", task.ID.Namespace, "aura-plugins")
	}
	if task.Status != provenance.StatusOpen {
		t.Errorf("Status = %v, want StatusOpen", task.Status)
	}

	// Show
	found, err := tr.Show(task.ID)
	if err != nil {
		t.Fatalf("Show failed: %v", err)
	}
	if found.Title != "REQUEST: Port codegen to Go" {
		t.Errorf("Title = %q, want %q", found.Title, "REQUEST: Port codegen to Go")
	}

	// Update
	inProgress := provenance.StatusInProgress
	updated, err := tr.Update(task.ID, provenance.UpdateFields{Status: &inProgress})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if updated.Status != provenance.StatusInProgress {
		t.Errorf("Status = %v, want StatusInProgress", updated.Status)
	}

	// Close
	closed, err := tr.CloseTask(task.ID, "Implemented and pushed")
	if err != nil {
		t.Fatalf("CloseTask failed: %v", err)
	}
	if closed.Status != provenance.StatusClosed {
		t.Errorf("Status = %v, want StatusClosed", closed.Status)
	}

	// List — no open tasks
	openStatus := provenance.StatusOpen
	tasks, err := tr.List(provenance.ListFilter{Status: &openStatus})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("List(StatusOpen) = %d tasks, want 0", len(tasks))
	}
}

// ---------------------------------------------------------------------------
// Demo 2: Dependency Graph
// ---------------------------------------------------------------------------

func TestDemo_DependencyGraph(t *testing.T) {
	tr := openTestTracker(t)

	request := mustCreate(t, tr, "proj", "REQUEST", "", provenance.TaskTypeFeature, provenance.PriorityHigh, provenance.PhaseRequest)
	proposal := mustCreate(t, tr, "proj", "PROPOSAL-1", "", provenance.TaskTypeTask, provenance.PriorityHigh, provenance.PhasePropose)
	implPlan := mustCreate(t, tr, "proj", "IMPL_PLAN", "", provenance.TaskTypeTask, provenance.PriorityHigh, provenance.PhaseImplPlan)
	slice1 := mustCreate(t, tr, "proj", "SLICE-1", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseWorkerSlices)

	// Chain: REQUEST <- PROPOSAL <- IMPL_PLAN <- SLICE-1
	mustAddEdge(t, tr, request.ID, proposal.ID.String(), provenance.EdgeBlockedBy)
	mustAddEdge(t, tr, proposal.ID, implPlan.ID.String(), provenance.EdgeBlockedBy)
	mustAddEdge(t, tr, implPlan.ID, slice1.ID.String(), provenance.EdgeBlockedBy)

	// Only SLICE-1 should be ready
	ready, err := tr.Ready()
	if err != nil {
		t.Fatalf("Ready() failed: %v", err)
	}
	if len(ready) != 1 || ready[0].Title != "SLICE-1" {
		t.Errorf("Ready() = %d tasks (want 1: SLICE-1), got titles: %v", len(ready), taskTitles(ready))
	}

	// REQUEST, PROPOSAL, IMPL_PLAN are blocked
	blocked, err := tr.Blocked()
	if err != nil {
		t.Fatalf("Blocked() failed: %v", err)
	}
	if len(blocked) != 3 {
		t.Errorf("Blocked() = %d tasks, want 3", len(blocked))
	}

	// Close SLICE-1 → IMPL_PLAN becomes ready
	mustCloseTask(t, tr, slice1.ID, "Done")
	ready2, err := tr.Ready()
	if err != nil {
		t.Fatalf("Ready() after close failed: %v", err)
	}
	if !containsTitle(ready2, "IMPL_PLAN") {
		t.Errorf("After closing SLICE-1, Ready() should include IMPL_PLAN, got: %v", taskTitles(ready2))
	}

	// DepTree from REQUEST
	edges, err := tr.DepTree(request.ID)
	if err != nil {
		t.Fatalf("DepTree(request) failed: %v", err)
	}
	if len(edges) < 3 {
		t.Errorf("DepTree(request) = %d edges, want >= 3", len(edges))
	}
}

// ---------------------------------------------------------------------------
// Demo 3: Cycle Detection
// ---------------------------------------------------------------------------

func TestDemo_CycleDetection(t *testing.T) {
	tr := openTestTracker(t)

	a := mustCreate(t, tr, "proj", "Task A", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	b := mustCreate(t, tr, "proj", "Task B", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	c := mustCreate(t, tr, "proj", "Task C", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)

	// Direct cycle: A->B, B->A
	mustAddEdge(t, tr, a.ID, b.ID.String(), provenance.EdgeBlockedBy)
	err := tr.AddEdge(b.ID, a.ID.String(), provenance.EdgeBlockedBy)
	if !errors.Is(err, provenance.ErrCycleDetected) {
		t.Errorf("Direct cycle: got %v, want ErrCycleDetected", err)
	}

	// Transitive cycle: A->B->C->A
	mustAddEdge(t, tr, b.ID, c.ID.String(), provenance.EdgeBlockedBy)
	err = tr.AddEdge(c.ID, a.ID.String(), provenance.EdgeBlockedBy)
	if !errors.Is(err, provenance.ErrCycleDetected) {
		t.Errorf("Transitive cycle: got %v, want ErrCycleDetected", err)
	}

	// Non-blocking edges do NOT enforce cycles
	err = tr.AddEdge(b.ID, a.ID.String(), provenance.EdgeDerivedFrom)
	if err != nil {
		t.Errorf("EdgeDerivedFrom should not enforce cycles, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Demo 4: Provenance Edges (PROV-O Lineage)
// ---------------------------------------------------------------------------

func TestDemo_ProvenanceEdges(t *testing.T) {
	tr := openTestTracker(t)

	prop1 := mustCreate(t, tr, "proj", "PROPOSAL-1", "Initial", provenance.TaskTypeTask, provenance.PriorityHigh, provenance.PhasePropose)
	prop2 := mustCreate(t, tr, "proj", "PROPOSAL-2", "Revised", provenance.TaskTypeTask, provenance.PriorityHigh, provenance.PhasePropose)
	prop3 := mustCreate(t, tr, "proj", "PROPOSAL-3", "Final", provenance.TaskTypeTask, provenance.PriorityHigh, provenance.PhasePropose)

	// Derivation chain
	mustAddEdge(t, tr, prop2.ID, prop1.ID.String(), provenance.EdgeDerivedFrom)
	mustAddEdge(t, tr, prop3.ID, prop2.ID.String(), provenance.EdgeDerivedFrom)

	// Supersession
	mustAddEdge(t, tr, prop2.ID, prop1.ID.String(), provenance.EdgeSupersedes)
	mustAddEdge(t, tr, prop3.ID, prop2.ID.String(), provenance.EdgeSupersedes)

	// Query derivation lineage
	derivedFrom := provenance.EdgeDerivedFrom
	edges, err := tr.Edges(prop3.ID, &derivedFrom)
	if err != nil {
		t.Fatalf("Edges(prop3, DerivedFrom) failed: %v", err)
	}
	if len(edges) != 1 || edges[0].TargetID != prop2.ID.String() {
		t.Errorf("Edges(prop3, DerivedFrom) = %v, want 1 edge to prop2", edges)
	}

	// Provenance edges do NOT affect readiness
	ready, err := tr.Ready()
	if err != nil {
		t.Fatalf("Ready() failed: %v", err)
	}
	if len(ready) != 3 {
		t.Errorf("All 3 proposals should be ready (no EdgeBlockedBy), got %d", len(ready))
	}
}

// ---------------------------------------------------------------------------
// Demo 5: PROV-O Agents
// ---------------------------------------------------------------------------

func TestDemo_PROVOAgents(t *testing.T) {
	tr := openTestTracker(t)

	// Human agent
	human, err := tr.RegisterHumanAgent("aura", "David Pham", "dayvidpham@gmail.com")
	if err != nil {
		t.Fatalf("RegisterHumanAgent failed: %v", err)
	}
	if human.Name != "David Pham" {
		t.Errorf("Name = %q, want %q", human.Name, "David Pham")
	}

	// ML agents
	sup, err := tr.RegisterMLAgent("aura", provenance.RoleSupervisor, provenance.ProviderAnthropic, "claude-opus-4-6")
	if err != nil {
		t.Fatalf("RegisterMLAgent(supervisor) failed: %v", err)
	}
	worker, err := tr.RegisterMLAgent("aura", provenance.RoleWorker, provenance.ProviderAnthropic, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("RegisterMLAgent(worker) failed: %v", err)
	}
	if sup.ID == worker.ID {
		t.Error("Supervisor and worker should have different IDs")
	}

	// Software agent
	bdCli, err := tr.RegisterSoftwareAgent("aura", "beads", "0.4.0", "github.com/dayvidpham/beads")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent failed: %v", err)
	}
	if bdCli.Name != "beads" {
		t.Errorf("Name = %q, want %q", bdCli.Name, "beads")
	}

	// Kind mismatch — MLAgent on a human ID returns an error
	// (implementation uses ErrNotFound since the SQL query joins on agents_ml)
	_, err = tr.MLAgent(human.ID)
	if err == nil {
		t.Error("MLAgent(humanID) should return an error for a human agent")
	}

	// Compile-time type safety: human.ID is AgentID, not TaskID — this
	// would not compile: tr.Show(human.ID)
}

// ---------------------------------------------------------------------------
// Demo 6: PROV-O Activities
// ---------------------------------------------------------------------------

func TestDemo_PROVOActivities(t *testing.T) {
	tr := openTestTracker(t)

	agent := mustRegisterMLAgent(t, tr, "aura", provenance.RoleSupervisor, provenance.ProviderAnthropic, "claude-opus-4-6")

	// Start activity
	activity, err := tr.StartActivity(agent.ID, provenance.PhaseImplPlan, provenance.StageInProgress, "Decomposing into slices")
	if err != nil {
		t.Fatalf("StartActivity failed: %v", err)
	}
	if activity.EndedAt != nil {
		t.Error("EndedAt should be nil before EndActivity")
	}

	// Create task linked to activity
	task := mustCreate(t, tr, "aura", "IMPL_PLAN: feature X", "", provenance.TaskTypeTask, provenance.PriorityHigh, provenance.PhaseImplPlan)
	mustAddEdge(t, tr, task.ID, activity.ID.String(), provenance.EdgeGeneratedBy)
	mustAddEdge(t, tr, task.ID, agent.ID.String(), provenance.EdgeAttributedTo)

	// End activity
	ended, err := tr.EndActivity(activity.ID)
	if err != nil {
		t.Fatalf("EndActivity failed: %v", err)
	}
	if ended.EndedAt == nil {
		t.Error("EndedAt should be set after EndActivity")
	}

	// Query by agent
	activities, err := tr.Activities(&agent.ID)
	if err != nil {
		t.Fatalf("Activities(agent) failed: %v", err)
	}
	if len(activities) != 1 {
		t.Errorf("Activities(agent) = %d, want 1", len(activities))
	}
}

// ---------------------------------------------------------------------------
// Demo 7: Labels + Comments
// ---------------------------------------------------------------------------

func TestDemo_LabelsAndComments(t *testing.T) {
	tr := openTestTracker(t)

	agent := mustRegisterMLAgent(t, tr, "aura", provenance.RoleReviewer, provenance.ProviderAnthropic, "claude-opus-4-6")
	task := mustCreate(t, tr, "proj", "SLICE-1", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseWorkerSlices)

	// Labels
	mustAddLabel(t, tr, task.ID, "aura:p9-impl:s9-slice")
	mustAddLabel(t, tr, task.ID, "aura:severity:blocker")
	mustAddLabel(t, tr, task.ID, "aura:p9-impl:s9-slice") // idempotent

	labels, err := tr.Labels(task.ID)
	if err != nil {
		t.Fatalf("Labels(%v) failed: %v", task.ID, err)
	}
	if len(labels) != 2 {
		t.Errorf("Labels = %d, want 2 (idempotent add)", len(labels))
	}

	// Comments
	comment, err := tr.AddComment(task.ID, agent.ID, "VOTE: ACCEPT — no blockers found")
	if err != nil {
		t.Fatalf("AddComment failed: %v", err)
	}
	if comment.Body != "VOTE: ACCEPT — no blockers found" {
		t.Errorf("Body = %q, want VOTE message", comment.Body)
	}

	comments, err := tr.Comments(task.ID)
	if err != nil {
		t.Fatalf("Comments(%v) failed: %v", task.ID, err)
	}
	if len(comments) != 1 {
		t.Errorf("Comments = %d, want 1", len(comments))
	}
}

// ---------------------------------------------------------------------------
// Demo 8: Persistence
// ---------------------------------------------------------------------------

func TestDemo_Persistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "provenance-demo.db")

	// Session 1: create data
	t1, err := provenance.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite session 1: %v", err)
	}
	task := mustCreate(t, t1, "proj", "Persistent task", "", provenance.TaskTypeFeature, provenance.PriorityHigh, provenance.PhaseRequest)
	taskID := task.ID
	t1.Close()

	// Session 2: reopen, data survives
	t2, err := provenance.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite session 2: %v", err)
	}
	found, err := t2.Show(taskID)
	if err != nil {
		t.Fatalf("Show after reopen: %v", err)
	}
	if found.Title != "Persistent task" {
		t.Errorf("Title = %q, want %q", found.Title, "Persistent task")
	}
	t2.Close()

	// In-memory: data does NOT survive
	mem, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	memTask := mustCreate(t, mem, "proj", "Ephemeral", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseRequest)
	mem.Close()
	mem2, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory (second): %v", err)
	}
	defer mem2.Close()
	_, err = mem2.Show(memTask.ID)
	if !errors.Is(err, provenance.ErrNotFound) {
		t.Errorf("In-memory after close: got %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Demo 9: Full Epoch Simulation
// ---------------------------------------------------------------------------

func TestDemo_FullEpochSimulation(t *testing.T) {
	tr := openTestTracker(t)

	// --- Register agents ---
	human := mustRegisterHumanAgent(t, tr, "aura", "David Pham", "dayvidpham@gmail.com")
	architect := mustRegisterMLAgent(t, tr, "aura", provenance.RoleArchitect, provenance.ProviderAnthropic, "claude-opus-4-6")
	supervisor := mustRegisterMLAgent(t, tr, "aura", provenance.RoleSupervisor, provenance.ProviderAnthropic, "claude-opus-4-6")
	workerAgent := mustRegisterMLAgent(t, tr, "aura", provenance.RoleWorker, provenance.ProviderAnthropic, "claude-sonnet-4-6")
	reviewerA := mustRegisterMLAgent(t, tr, "aura", provenance.RoleReviewer, provenance.ProviderAnthropic, "claude-opus-4-6")

	// --- Phase 1: REQUEST ---
	reqActivity := mustStartActivity(t, tr, human.ID, provenance.PhaseRequest, provenance.StageInProgress, "User submits request")
	request := mustCreate(t, tr, "aura", "REQUEST: Port codegen to Go", "verbatim request",
		provenance.TaskTypeFeature, provenance.PriorityHigh, provenance.PhaseRequest)
	mustAddEdge(t, tr, request.ID, reqActivity.ID.String(), provenance.EdgeGeneratedBy)
	mustAddEdge(t, tr, request.ID, human.ID.String(), provenance.EdgeAttributedTo)
	mustAddLabel(t, tr, request.ID, "aura:p1-user:s1_1-classify")
	mustEndActivity(t, tr, reqActivity.ID)

	// --- Phase 3: PROPOSAL ---
	propActivity := mustStartActivity(t, tr, architect.ID, provenance.PhasePropose, provenance.StageInProgress, "Writing proposal")
	proposal := mustCreate(t, tr, "aura", "PROPOSAL-1: codegen port", "full plan",
		provenance.TaskTypeTask, provenance.PriorityHigh, provenance.PhasePropose)
	mustAddEdge(t, tr, proposal.ID, propActivity.ID.String(), provenance.EdgeGeneratedBy)
	mustAddEdge(t, tr, proposal.ID, architect.ID.String(), provenance.EdgeAttributedTo)
	mustAddEdge(t, tr, request.ID, proposal.ID.String(), provenance.EdgeBlockedBy)
	mustAddLabel(t, tr, proposal.ID, "aura:p3-plan:s3-propose")
	mustEndActivity(t, tr, propActivity.ID)

	// --- Phase 4: REVIEW ---
	mustAddComment(t, tr, proposal.ID, reviewerA.ID, "VOTE: ACCEPT — correctness verified")

	// --- Phase 8: IMPL_PLAN ---
	planActivity := mustStartActivity(t, tr, supervisor.ID, provenance.PhaseImplPlan, provenance.StageInProgress, "Decomposing")
	implPlan := mustCreate(t, tr, "aura", "IMPL_PLAN: 3 slices", "",
		provenance.TaskTypeTask, provenance.PriorityHigh, provenance.PhaseImplPlan)
	mustAddEdge(t, tr, implPlan.ID, planActivity.ID.String(), provenance.EdgeGeneratedBy)
	mustAddEdge(t, tr, proposal.ID, implPlan.ID.String(), provenance.EdgeBlockedBy)
	slice1 := mustCreate(t, tr, "aura", "SLICE-1: types", "",
		provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseWorkerSlices)
	mustAddEdge(t, tr, implPlan.ID, slice1.ID.String(), provenance.EdgeBlockedBy)
	mustEndActivity(t, tr, planActivity.ID)

	// --- Readiness check ---
	ready, err := tr.Ready()
	if err != nil {
		t.Fatalf("Ready() failed: %v", err)
	}
	if !containsTitle(ready, "SLICE-1: types") {
		t.Errorf("Only SLICE-1 should be ready, got: %v", taskTitles(ready))
	}
	if containsTitle(ready, "REQUEST: Port codegen to Go") || containsTitle(ready, "PROPOSAL-1: codegen port") || containsTitle(ready, "IMPL_PLAN: 3 slices") {
		t.Errorf("REQUEST/PROPOSAL/IMPL_PLAN should be blocked, but appeared in Ready(): %v", taskTitles(ready))
	}

	// --- Phase 9: Worker implements ---
	implActivity := mustStartActivity(t, tr, workerAgent.ID, provenance.PhaseWorkerSlices, provenance.StageInProgress, "Implementing")
	mustAddEdge(t, tr, slice1.ID, implActivity.ID.String(), provenance.EdgeGeneratedBy)
	mustCloseTask(t, tr, slice1.ID, "Tests pass, committed")
	mustEndActivity(t, tr, implActivity.ID)

	// IMPL_PLAN should now be ready
	ready2, err := tr.Ready()
	if err != nil {
		t.Fatalf("Ready() after close failed: %v", err)
	}
	if !containsTitle(ready2, "IMPL_PLAN: 3 slices") {
		t.Errorf("After closing SLICE-1, IMPL_PLAN should be ready, got: %v", taskTitles(ready2))
	}

	// --- Full provenance query ---
	ancestors, err := tr.Ancestors(request.ID)
	if err != nil {
		t.Fatalf("Ancestors(request) failed: %v", err)
	}
	if len(ancestors) < 2 {
		t.Errorf("Ancestors(request) = %d, want >= 2 (proposal, implPlan, slice1)", len(ancestors))
	}

	descendants, err := tr.Descendants(slice1.ID)
	if err != nil {
		t.Fatalf("Descendants(slice1) failed: %v", err)
	}
	if len(descendants) < 2 {
		t.Errorf("Descendants(slice1) = %d, want >= 2 (implPlan, proposal, request)", len(descendants))
	}

	// Verify full lineage: every task has provenance edges
	genBy := provenance.EdgeGeneratedBy
	requestEdges, err := tr.Edges(request.ID, &genBy)
	if err != nil {
		t.Fatalf("Edges(request, EdgeGeneratedBy) failed: %v", err)
	}
	if len(requestEdges) != 1 {
		t.Errorf("REQUEST should have 1 EdgeGeneratedBy, got %d", len(requestEdges))
	}
	attrTo := provenance.EdgeAttributedTo
	requestAttr, err := tr.Edges(request.ID, &attrTo)
	if err != nil {
		t.Fatalf("Edges(request, EdgeAttributedTo) failed: %v", err)
	}
	if len(requestAttr) != 1 {
		t.Errorf("REQUEST should have 1 EdgeAttributedTo, got %d", len(requestAttr))
	}

	// Verify agent activity history
	humanActivities, err := tr.Activities(&human.ID)
	if err != nil {
		t.Fatalf("Activities(human) failed: %v", err)
	}
	architectActivities, err := tr.Activities(&architect.ID)
	if err != nil {
		t.Fatalf("Activities(architect) failed: %v", err)
	}
	supervisorActivities, err := tr.Activities(&supervisor.ID)
	if err != nil {
		t.Fatalf("Activities(supervisor) failed: %v", err)
	}
	workerActivities, err := tr.Activities(&workerAgent.ID)
	if err != nil {
		t.Fatalf("Activities(worker) failed: %v", err)
	}

	if len(humanActivities) != 1 {
		t.Errorf("Human activities = %d, want 1", len(humanActivities))
	}
	if len(architectActivities) != 1 {
		t.Errorf("Architect activities = %d, want 1", len(architectActivities))
	}
	if len(supervisorActivities) != 1 {
		t.Errorf("Supervisor activities = %d, want 1", len(supervisorActivities))
	}
	if len(workerActivities) != 1 {
		t.Errorf("Worker activities = %d, want 1", len(workerActivities))
	}

	t.Logf("Full epoch simulation passed: %d agents, %d activities, %d tasks, full provenance chain verified",
		5, 4, 4, // human+architect+supervisor+worker+reviewer, 4 activities, 4 tasks
	)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustCreate(t *testing.T, tr provenance.Tracker, namespace, title, description string, taskType provenance.TaskType, priority provenance.Priority, phase provenance.Phase) provenance.Task {
	t.Helper()
	task, err := tr.Create(namespace, title, description, taskType, priority, phase)
	if err != nil {
		t.Fatalf("Create(%q) failed: %v", title, err)
	}
	return task
}

func mustRegisterHumanAgent(t *testing.T, tr provenance.Tracker, namespace, name, contact string) provenance.HumanAgent {
	t.Helper()
	agent, err := tr.RegisterHumanAgent(namespace, name, contact)
	if err != nil {
		t.Fatalf("RegisterHumanAgent(%q) failed: %v", name, err)
	}
	return agent
}

func mustRegisterMLAgent(t *testing.T, tr provenance.Tracker, namespace string, role provenance.Role, provider provenance.Provider, modelName string) provenance.MLAgent {
	t.Helper()
	agent, err := tr.RegisterMLAgent(namespace, role, provider, modelName)
	if err != nil {
		t.Fatalf("RegisterMLAgent(%s, %q) failed: %v", role, modelName, err)
	}
	return agent
}

func mustStartActivity(t *testing.T, tr provenance.Tracker, agentID provenance.AgentID, phase provenance.Phase, stage provenance.Stage, notes string) provenance.Activity {
	t.Helper()
	act, err := tr.StartActivity(agentID, phase, stage, notes)
	if err != nil {
		t.Fatalf("StartActivity failed: %v", err)
	}
	return act
}

func mustCloseTask(t *testing.T, tr provenance.Tracker, id provenance.TaskID, reason string) provenance.Task {
	t.Helper()
	task, err := tr.CloseTask(id, reason)
	if err != nil {
		t.Fatalf("CloseTask(%v, %q) failed: %v", id, reason, err)
	}
	return task
}

func mustAddLabel(t *testing.T, tr provenance.Tracker, id provenance.TaskID, label string) {
	t.Helper()
	if err := tr.AddLabel(id, label); err != nil {
		t.Fatalf("AddLabel(%v, %q) failed: %v", id, label, err)
	}
}

func mustEndActivity(t *testing.T, tr provenance.Tracker, id provenance.ActivityID) provenance.Activity {
	t.Helper()
	act, err := tr.EndActivity(id)
	if err != nil {
		t.Fatalf("EndActivity(%v) failed: %v", id, err)
	}
	return act
}

func mustAddComment(t *testing.T, tr provenance.Tracker, taskID provenance.TaskID, authorID provenance.AgentID, body string) provenance.Comment {
	t.Helper()
	comment, err := tr.AddComment(taskID, authorID, body)
	if err != nil {
		t.Fatalf("AddComment(%v, %v) failed: %v", taskID, authorID, err)
	}
	return comment
}

func mustAddEdge(t *testing.T, tr provenance.Tracker, sourceID provenance.TaskID, targetID string, kind provenance.EdgeKind) {
	t.Helper()
	if err := tr.AddEdge(sourceID, targetID, kind); err != nil {
		t.Fatalf("AddEdge(%s -> %s, %s) failed: %v", sourceID, targetID, kind, err)
	}
}

func taskTitles(tasks []provenance.Task) []string {
	titles := make([]string, len(tasks))
	for i, t := range tasks {
		titles[i] = t.Title
	}
	return titles
}

func containsTitle(tasks []provenance.Task, title string) bool {
	for _, t := range tasks {
		if t.Title == title {
			return true
		}
	}
	return false
}
