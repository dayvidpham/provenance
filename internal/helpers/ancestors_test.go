package helpers_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dayvidpham/providence"
	pvgraph "github.com/dayvidpham/providence/internal/graph"
	"github.com/dayvidpham/providence/internal/helpers"
	"github.com/dayvidpham/providence/internal/sqlite"
	dgraph "github.com/dominikbraun/graph"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func openTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.OpenMemory()
	if err != nil {
		t.Fatalf("sqlite.OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newTaskID(ns string) providence.TaskID {
	return providence.TaskID{Namespace: ns, UUID: uuid.Must(uuid.NewV7())}
}

func makeTask(id providence.TaskID, title string) providence.Task {
	n := time.Now().UTC()
	return providence.Task{
		ID:        id,
		Title:     title,
		Status:    providence.StatusOpen,
		Priority:  providence.PriorityMedium,
		Type:      providence.TaskTypeTask,
		Phase:     providence.PhaseUnscoped,
		CreatedAt: n,
		UpdatedAt: n,
	}
}

func containsID(ids []providence.TaskID, target providence.TaskID) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// buildGraph inserts the given vertices and edges (source→target, meaning
// source is blocked by target) into a new in-memory graph.
func buildGraph(t *testing.T, pairs [][2]providence.TaskID, allTasks []providence.Task) (dgraph.Graph[string, providence.Task], *sqlite.DB) {
	t.Helper()
	db := openTestDB(t)
	g := pvgraph.NewBlockedByGraph(db)

	for _, task := range allTasks {
		if err := g.AddVertex(task); err != nil {
			t.Fatalf("AddVertex %v: %v", task.ID, err)
		}
	}
	for _, pair := range pairs {
		if err := g.AddEdge(pair[0].String(), pair[1].String()); err != nil {
			t.Fatalf("AddEdge %v->%v: %v", pair[0], pair[1], err)
		}
	}
	return g, db
}

// ---------------------------------------------------------------------------
// TestAncestors — linear chain A→B→C: ancestors of A are B and C
//
// "A is blocked by B" means A→B. "B is blocked by C" means B→C.
// So for task A to proceed, B must finish. For B to proceed, C must finish.
// Ancestors(A) = {B, C} — tasks that A transitively depends on.
// ---------------------------------------------------------------------------

func TestAncestors(t *testing.T) {
	a := newTaskID("ns")
	b := newTaskID("ns")
	c := newTaskID("ns")

	// A→B (A blocked by B), B→C (B blocked by C)
	g, _ := buildGraph(t,
		[][2]providence.TaskID{{a, b}, {b, c}},
		[]providence.Task{makeTask(a, "A"), makeTask(b, "B"), makeTask(c, "C")},
	)

	ancestors, err := helpers.Ancestors(g, a)
	if err != nil {
		t.Fatalf("Ancestors: %v", err)
	}
	if len(ancestors) != 2 {
		t.Fatalf("expected 2 ancestors of A, got %d: %v", len(ancestors), ancestors)
	}
	if !containsID(ancestors, b) {
		t.Errorf("ancestors of A should contain B")
	}
	if !containsID(ancestors, c) {
		t.Errorf("ancestors of A should contain C")
	}
}

// ---------------------------------------------------------------------------
// TestAncestorsNoBlockers — task with no outgoing edges has no ancestors
// ---------------------------------------------------------------------------

func TestAncestorsNoBlockers(t *testing.T) {
	a := newTaskID("ns")
	g, _ := buildGraph(t, nil, []providence.Task{makeTask(a, "A")})

	ancestors, err := helpers.Ancestors(g, a)
	if err != nil {
		t.Fatalf("Ancestors: %v", err)
	}
	if len(ancestors) != 0 {
		t.Errorf("expected 0 ancestors for standalone task, got %d: %v", len(ancestors), ancestors)
	}
}

// ---------------------------------------------------------------------------
// TestAncestorsBranching — A→B, A→C: ancestors of A are both B and C
// ---------------------------------------------------------------------------

func TestAncestorsBranching(t *testing.T) {
	a := newTaskID("ns")
	b := newTaskID("ns")
	c := newTaskID("ns")

	g, _ := buildGraph(t,
		[][2]providence.TaskID{{a, b}, {a, c}},
		[]providence.Task{makeTask(a, "A"), makeTask(b, "B"), makeTask(c, "C")},
	)

	ancestors, err := helpers.Ancestors(g, a)
	if err != nil {
		t.Fatalf("Ancestors: %v", err)
	}
	if len(ancestors) != 2 {
		t.Fatalf("expected 2 ancestors, got %d: %v", len(ancestors), ancestors)
	}
	if !containsID(ancestors, b) {
		t.Errorf("expected B in ancestors")
	}
	if !containsID(ancestors, c) {
		t.Errorf("expected C in ancestors")
	}
}

// ---------------------------------------------------------------------------
// TestDescendants — linear chain A→B→C: descendants of C are B and A
//
// "A is blocked by B" means A→B. So C blocks B (B→C), and B blocks A (A→B).
// Descendants(C) = {B, A} — tasks that cannot start until C finishes.
//
// In graph terms: an edge A→B means A is "adjacent to" B.
// AdjacencyMap[C] = {} (no outgoing edges from C).
// We want "who depends on C?" — i.e., who has an incoming edge from C?
// That's the PredecessorMap perspective: C's in-edges come from B (B→C?).
//
// Wait — let's reclarify. The edge direction is: parent→child means parent
// is blocked by child. So A→B means A (parent) is blocked by B (child).
// Descendants of C are: tasks that (directly or transitively) have C as a
// blocker — i.e. tasks where C appears in their ancestor chain.
// This is NOT the adjacency of C, but the "reverse graph" of C.
//
// Descendants uses AdjacencyMap. But the AdjacencyMap of a directed graph
// stores outgoing edges. For edge A→B: AdjacencyMap[A] contains B.
// So AdjacencyMap[C] would be empty in the A→B→C chain.
//
// For Descendants(C) to return {B, A}, the Descendants function should follow
// the PredecessorMap (incoming edges) from C backward through the edge direction:
// "who points TO C?" = B (because B→C). "Who points TO B?" = A (because A→B).
//
// But wait — the Descendants doc says it uses AdjacencyMap. Let me reconsider.
// The semantics should be:
//   Ancestors(X): "what must X wait for?" — follows outgoing edges (X→...) deeply.
//   Descendants(X): "who is waiting for X?" — follows incoming edges (...→X) deeply.
//
// The code in ancestors.go uses PredecessorMap for Ancestors and AdjacencyMap
// for Descendants. But in a directed graph A→B→C:
//   - PredecessorMap[A] = {} (nothing points to A)
//   - PredecessorMap[B] = {A} (A points to B)
//   - AdjacencyMap[A] = {B}, AdjacencyMap[B] = {C}, AdjacencyMap[C] = {}
//
// Ancestors(A) via PredecessorMap: predecessors[A.String()] = {} → empty ✗
//
// This is inverted! The helpers.Ancestors function follows predecessors of the
// given ID. A's predecessors are nodes that point TO A. But in the edge A→B,
// B is A's successor, not predecessor. So following predecessors of A gives us
// nothing (correct: A has no blockers itself in a chain where A blocks others).
//
// For Ancestors(A) to return {B, C} in a chain A→B→C:
// Ancestors should follow the ADJACENCY (outgoing edges) of A.
// Because A→B means A follows B in the chain, and Ancestors means "what
// does A wait for?"
//
// Let me re-read ancestors.go carefully.
// helpers.Ancestors uses PredecessorMap and iterates predecessors[current].
// Predecessor of X = nodes Y where edge Y→X exists.
// In chain A→B→C: predecessor(B) = {A}, predecessor(C) = {B}.
// Ancestors(A) starting from A: predecessors[A] = {} → empty.
// That gives ancestors of A = [] — which is wrong for our use case.
//
// This suggests the Ancestors/Descendants semantic in ancestors.go may be
// inverted from what the test description expects. The tests define the
// expected behavior; the implementation must match. Let's define clearly:
//
//   Ancestors(id) = "tasks that id must wait for (its blockers, transitively)"
//     → In edge A→B (A blocked by B), call from A should return B and C.
//     → Must follow outgoing edges (adjacency): A→B→C.
//     → Should use AdjacencyMap.
//
//   Descendants(id) = "tasks that are waiting for id (blocked by id, transitively)"
//     → In edge A→B (A blocked by B), call from C should return B and A.
//     → Must follow incoming edges (predecessors): C←B←A.
//     → Should use PredecessorMap.
//
// The current ancestors.go has them SWAPPED. The tests here define the
// correct contract. The implementation will need to be corrected.
// ---------------------------------------------------------------------------

func TestDescendants(t *testing.T) {
	a := newTaskID("ns")
	b := newTaskID("ns")
	c := newTaskID("ns")

	// A→B (A blocked by B), B→C (B blocked by C)
	g, _ := buildGraph(t,
		[][2]providence.TaskID{{a, b}, {b, c}},
		[]providence.Task{makeTask(a, "A"), makeTask(b, "B"), makeTask(c, "C")},
	)

	// Descendants of C = tasks waiting for C = {B, A}
	descendants, err := helpers.Descendants(g, c)
	if err != nil {
		t.Fatalf("Descendants: %v", err)
	}
	if len(descendants) != 2 {
		t.Fatalf("expected 2 descendants of C, got %d: %v", len(descendants), descendants)
	}
	if !containsID(descendants, a) {
		t.Errorf("descendants of C should contain A")
	}
	if !containsID(descendants, b) {
		t.Errorf("descendants of C should contain B")
	}
}

// ---------------------------------------------------------------------------
// TestDescendantsNoWaiters — leaf task (no one waiting for it) has no descendants
// ---------------------------------------------------------------------------

func TestDescendantsNoWaiters(t *testing.T) {
	a := newTaskID("ns")
	g, _ := buildGraph(t, nil, []providence.Task{makeTask(a, "A")})

	descendants, err := helpers.Descendants(g, a)
	if err != nil {
		t.Fatalf("Descendants: %v", err)
	}
	if len(descendants) != 0 {
		t.Errorf("expected 0 descendants for standalone task, got %d: %v", len(descendants), descendants)
	}
}
