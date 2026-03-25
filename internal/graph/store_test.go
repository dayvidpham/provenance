package graph_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dayvidpham/providence"
	pvgraph "github.com/dayvidpham/providence/internal/graph"
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

// ---------------------------------------------------------------------------
// TestNewBlockedByGraph — smoke test: graph can be created
// ---------------------------------------------------------------------------

func TestNewBlockedByGraph(t *testing.T) {
	db := openTestDB(t)
	g := pvgraph.NewBlockedByGraph(db)
	if g == nil {
		t.Fatal("NewBlockedByGraph returned nil")
	}
}

// ---------------------------------------------------------------------------
// TestAddVertex — task added via graph is persisted and retrievable
// ---------------------------------------------------------------------------

func TestAddVertex(t *testing.T) {
	db := openTestDB(t)
	g := pvgraph.NewBlockedByGraph(db)

	id := newTaskID("test")
	task := makeTask(id, "first task")

	if err := g.AddVertex(task); err != nil {
		t.Fatalf("AddVertex: %v", err)
	}

	got, err := g.Vertex(id.String())
	if err != nil {
		t.Fatalf("Vertex: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID mismatch: got %v, want %v", got.ID, id)
	}
	if got.Title != task.Title {
		t.Errorf("Title: got %q, want %q", got.Title, task.Title)
	}
}

// ---------------------------------------------------------------------------
// TestVertexNotFound — querying non-existent vertex returns ErrVertexNotFound
// ---------------------------------------------------------------------------

func TestVertexNotFound(t *testing.T) {
	db := openTestDB(t)
	g := pvgraph.NewBlockedByGraph(db)

	missing := newTaskID("test")
	_, err := g.Vertex(missing.String())
	if !errors.Is(err, dgraph.ErrVertexNotFound) {
		t.Errorf("expected ErrVertexNotFound, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestAddEdge — blocked-by edge between two vertices is persisted
// ---------------------------------------------------------------------------

func TestAddEdge(t *testing.T) {
	db := openTestDB(t)
	g := pvgraph.NewBlockedByGraph(db)

	parentID := newTaskID("test")
	childID := newTaskID("test")

	if err := g.AddVertex(makeTask(parentID, "parent")); err != nil {
		t.Fatalf("AddVertex parent: %v", err)
	}
	if err := g.AddVertex(makeTask(childID, "child")); err != nil {
		t.Fatalf("AddVertex child: %v", err)
	}

	// parent is blocked-by child: edge from parent → child.
	if err := g.AddEdge(parentID.String(), childID.String()); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	edge, err := g.Edge(parentID.String(), childID.String())
	if err != nil {
		t.Fatalf("Edge: %v", err)
	}
	if edge.Source.ID.String() != parentID.String() {
		t.Errorf("edge.Source: got %v, want %v", edge.Source.ID, parentID)
	}
	if edge.Target.ID.String() != childID.String() {
		t.Errorf("edge.Target: got %v, want %v", edge.Target.ID, childID)
	}
}

// ---------------------------------------------------------------------------
// TestCyclePrevention — A→B→C→A must fail with ErrEdgeCreatesCycle
// ---------------------------------------------------------------------------

func TestCyclePrevention(t *testing.T) {
	db := openTestDB(t)
	g := pvgraph.NewBlockedByGraph(db)

	a := newTaskID("ns")
	b := newTaskID("ns")
	c := newTaskID("ns")

	for _, task := range []providence.Task{
		makeTask(a, "A"),
		makeTask(b, "B"),
		makeTask(c, "C"),
	} {
		if err := g.AddVertex(task); err != nil {
			t.Fatalf("AddVertex %v: %v", task.ID, err)
		}
	}

	// A is blocked by B.
	if err := g.AddEdge(a.String(), b.String()); err != nil {
		t.Fatalf("AddEdge A->B: %v", err)
	}
	// B is blocked by C.
	if err := g.AddEdge(b.String(), c.String()); err != nil {
		t.Fatalf("AddEdge B->C: %v", err)
	}
	// C is blocked by A — would close the cycle. Must fail.
	err := g.AddEdge(c.String(), a.String())
	if !errors.Is(err, dgraph.ErrEdgeCreatesCycle) {
		t.Errorf("expected ErrEdgeCreatesCycle for C->A, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestPredecessorMap — predecessors of a vertex are correct
// ---------------------------------------------------------------------------

// In the blocked-by graph an edge A→B means "A is blocked by B".
// The predecessor of A is B (B flows into A in the edge direction A→B).
// PredecessorMap returns for each vertex the set of vertices that have an
// outgoing edge pointing TO it — i.e. for vertex B: {A} (because A→B).
// Wait — actually in a directed graph where A→B:
//   - AdjacencyMap: A → {B}
//   - PredecessorMap: B → {A}   (A is a predecessor of B)
//
// So "A is blocked by B" (A→B) means B appears in A's adjacency but A appears
// as a predecessor of B. The helpers.Ancestors uses PredecessorMap to traverse
// backwards from a given node. Let's just verify the maps are internally
// consistent.
func TestPredecessorMap(t *testing.T) {
	db := openTestDB(t)
	g := pvgraph.NewBlockedByGraph(db)

	parent := newTaskID("ns") // blocked by child
	child := newTaskID("ns")

	if err := g.AddVertex(makeTask(parent, "parent")); err != nil {
		t.Fatalf("AddVertex parent: %v", err)
	}
	if err := g.AddVertex(makeTask(child, "child")); err != nil {
		t.Fatalf("AddVertex child: %v", err)
	}
	// parent is blocked by child: edge parent→child.
	if err := g.AddEdge(parent.String(), child.String()); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	predMap, err := g.PredecessorMap()
	if err != nil {
		t.Fatalf("PredecessorMap: %v", err)
	}

	// child's predecessors should include parent (parent→child means parent is a predecessor of child).
	childPreds, ok := predMap[child.String()]
	if !ok {
		t.Fatalf("child not found in PredecessorMap, map keys: %v", mapKeys(predMap))
	}
	if _, hasPar := childPreds[parent.String()]; !hasPar {
		t.Errorf("parent not in child's predecessors; got: %v", mapKeys(childPreds))
	}

	// parent's predecessors should be empty (no one is blocked by parent).
	parentPreds := predMap[parent.String()]
	if len(parentPreds) != 0 {
		t.Errorf("parent should have no predecessors, got: %v", mapKeys(parentPreds))
	}
}

// ---------------------------------------------------------------------------
// TestAdjacencyMap — adjacency of a vertex is correct
// ---------------------------------------------------------------------------

func TestAdjacencyMap(t *testing.T) {
	db := openTestDB(t)
	g := pvgraph.NewBlockedByGraph(db)

	parent := newTaskID("ns")
	child := newTaskID("ns")

	if err := g.AddVertex(makeTask(parent, "parent")); err != nil {
		t.Fatalf("AddVertex parent: %v", err)
	}
	if err := g.AddVertex(makeTask(child, "child")); err != nil {
		t.Fatalf("AddVertex child: %v", err)
	}
	if err := g.AddEdge(parent.String(), child.String()); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	adjMap, err := g.AdjacencyMap()
	if err != nil {
		t.Fatalf("AdjacencyMap: %v", err)
	}

	// parent's adjacency should include child.
	parentAdj, ok := adjMap[parent.String()]
	if !ok {
		t.Fatalf("parent not found in AdjacencyMap, keys: %v", mapKeys(adjMap))
	}
	if _, hasChild := parentAdj[child.String()]; !hasChild {
		t.Errorf("child not in parent's adjacency; got: %v", mapKeys(parentAdj))
	}

	// child's adjacency should be empty.
	childAdj := adjMap[child.String()]
	if len(childAdj) != 0 {
		t.Errorf("child should have no adjacency, got: %v", mapKeys(childAdj))
	}
}

// ---------------------------------------------------------------------------
// TestRemoveEdge — edge can be removed and Edge returns ErrEdgeNotFound
// ---------------------------------------------------------------------------

func TestRemoveEdge(t *testing.T) {
	db := openTestDB(t)
	g := pvgraph.NewBlockedByGraph(db)

	a := newTaskID("ns")
	b := newTaskID("ns")

	if err := g.AddVertex(makeTask(a, "A")); err != nil {
		t.Fatalf("AddVertex A: %v", err)
	}
	if err := g.AddVertex(makeTask(b, "B")); err != nil {
		t.Fatalf("AddVertex B: %v", err)
	}
	if err := g.AddEdge(a.String(), b.String()); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.RemoveEdge(a.String(), b.String()); err != nil {
		t.Fatalf("RemoveEdge: %v", err)
	}

	_, err := g.Edge(a.String(), b.String())
	if !errors.Is(err, dgraph.ErrEdgeNotFound) {
		t.Errorf("expected ErrEdgeNotFound after remove, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestVertexCount — count reflects actual insertions
// ---------------------------------------------------------------------------

func TestVertexCount(t *testing.T) {
	db := openTestDB(t)
	g := pvgraph.NewBlockedByGraph(db)

	for i := 0; i < 3; i++ {
		id := newTaskID("ns")
		if err := g.AddVertex(makeTask(id, "task")); err != nil {
			t.Fatalf("AddVertex %d: %v", i, err)
		}
	}

	// Access vertex count via listing.
	edges, err := g.Edges()
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	_ = edges // just ensure it doesn't error

	// Use PredecessorMap (which calls ListVertices) to confirm count.
	pm, err := g.PredecessorMap()
	if err != nil {
		t.Fatalf("PredecessorMap: %v", err)
	}
	if len(pm) != 3 {
		t.Errorf("expected 3 vertices in PredecessorMap, got %d", len(pm))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mapKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
