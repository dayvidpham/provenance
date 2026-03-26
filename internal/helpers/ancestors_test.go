package helpers_test

import (
	"testing"

	intgraph "github.com/dayvidpham/providence/internal/graph"
	"github.com/dayvidpham/providence/internal/helpers"
	dbsqlite "github.com/dayvidpham/providence/internal/sqlite"
	"github.com/dayvidpham/providence/internal/testutil"
	"github.com/dayvidpham/providence/pkg/ptypes"
	dgraph "github.com/dominikbraun/graph"
)

// openTestDB delegates to shared testutil.OpenTestDB.
func openTestDB(t *testing.T) *dbsqlite.DB { return testutil.OpenTestDB(t) }

// makeTask delegates to shared testutil.MakeTask.
func makeTask(ns, title string) ptypes.Task { return testutil.MakeTask(ns, title) }

// containsTask delegates to shared testutil.ContainsTask.
func containsTask(tasks []ptypes.Task, id ptypes.TaskID) bool {
	return testutil.ContainsTask(tasks, id)
}

// setupChain creates tasks A, B, C and edges A->B->C (A blocked by B blocked by C).
// Returns the graph, tasks, and DB.
func setupChain(t *testing.T) (dgraph.Graph[string, ptypes.Task], ptypes.Task, ptypes.Task, ptypes.Task, *dbsqlite.DB) {
	t.Helper()
	db := openTestDB(t)
	g := intgraph.NewGraph(db)

	a := makeTask("ns", "A")
	b := makeTask("ns", "B")
	c := makeTask("ns", "C")

	for _, task := range []ptypes.Task{a, b, c} {
		if err := g.AddVertex(task); err != nil {
			t.Fatalf("AddVertex(%s) error: %v", task.Title, err)
		}
	}

	// A blocked by B, B blocked by C.
	if err := g.AddEdge(a.ID.String(), b.ID.String()); err != nil {
		t.Fatalf("AddEdge A->B: %v", err)
	}
	if err := g.AddEdge(b.ID.String(), c.ID.String()); err != nil {
		t.Fatalf("AddEdge B->C: %v", err)
	}

	return g, a, b, c, db
}

func TestAncestorsChain(t *testing.T) {
	g, a, b, c, db := setupChain(t)

	// Ancestors of A (things A is blocked by) = {B, C}.
	ancestors, err := helpers.Ancestors(g, db, a.ID)
	if err != nil {
		t.Fatalf("Ancestors(A) error: %v", err)
	}

	if !containsTask(ancestors, b.ID) {
		t.Errorf("B not in Ancestors(A)")
	}
	if !containsTask(ancestors, c.ID) {
		t.Errorf("C not in Ancestors(A)")
	}
	if containsTask(ancestors, a.ID) {
		t.Errorf("A should not be in its own ancestors")
	}
	if len(ancestors) != 2 {
		t.Errorf("Ancestors(A) = %d tasks, want 2", len(ancestors))
	}
}

func TestAncestorsMiddleNode(t *testing.T) {
	g, _, b, c, db := setupChain(t)

	// Ancestors of B = {C} (only what B is blocked by).
	ancestors, err := helpers.Ancestors(g, db, b.ID)
	if err != nil {
		t.Fatalf("Ancestors(B) error: %v", err)
	}

	if !containsTask(ancestors, c.ID) {
		t.Errorf("C not in Ancestors(B)")
	}
	if len(ancestors) != 1 {
		t.Errorf("Ancestors(B) = %d tasks, want 1", len(ancestors))
	}
}

func TestAncestorsLeafNode(t *testing.T) {
	g, _, _, c, db := setupChain(t)

	// Ancestors of C = {} (C is not blocked by anything).
	ancestors, err := helpers.Ancestors(g, db, c.ID)
	if err != nil {
		t.Fatalf("Ancestors(C) error: %v", err)
	}

	if len(ancestors) != 0 {
		t.Errorf("Ancestors(C) = %d tasks, want 0", len(ancestors))
	}
}

func TestDescendantsChain(t *testing.T) {
	g, a, b, c, db := setupChain(t)

	// Descendants of C (things waiting for C) = {A, B}.
	descendants, err := helpers.Descendants(g, db, c.ID)
	if err != nil {
		t.Fatalf("Descendants(C) error: %v", err)
	}

	if !containsTask(descendants, a.ID) {
		t.Errorf("A not in Descendants(C)")
	}
	if !containsTask(descendants, b.ID) {
		t.Errorf("B not in Descendants(C)")
	}
	if containsTask(descendants, c.ID) {
		t.Errorf("C should not be in its own descendants")
	}
	if len(descendants) != 2 {
		t.Errorf("Descendants(C) = %d tasks, want 2", len(descendants))
	}
}

func TestDescendantsMiddleNode(t *testing.T) {
	g, a, b, _, db := setupChain(t)

	// Descendants of B = {A} (only A is waiting for B).
	descendants, err := helpers.Descendants(g, db, b.ID)
	if err != nil {
		t.Fatalf("Descendants(B) error: %v", err)
	}

	if !containsTask(descendants, a.ID) {
		t.Errorf("A not in Descendants(B)")
	}
	if len(descendants) != 1 {
		t.Errorf("Descendants(B) = %d tasks, want 1", len(descendants))
	}
}

func TestDescendantsRootNode(t *testing.T) {
	g, a, _, _, db := setupChain(t)

	// Descendants of A = {} (nothing is waiting for A).
	descendants, err := helpers.Descendants(g, db, a.ID)
	if err != nil {
		t.Fatalf("Descendants(A) error: %v", err)
	}

	if len(descendants) != 0 {
		t.Errorf("Descendants(A) = %d tasks, want 0", len(descendants))
	}
}

func TestAncestorsDiamond(t *testing.T) {
	// Diamond: A blocked by B and C, both B and C blocked by D.
	//   A
	//  / \
	// B   C
	//  \ /
	//   D
	db := openTestDB(t)
	g := intgraph.NewGraph(db)

	a := makeTask("ns", "A")
	b := makeTask("ns", "B")
	c := makeTask("ns", "C")
	d := makeTask("ns", "D")

	for _, task := range []ptypes.Task{a, b, c, d} {
		if err := g.AddVertex(task); err != nil {
			t.Fatalf("AddVertex(%s) error: %v", task.Title, err)
		}
	}

	if err := g.AddEdge(a.ID.String(), b.ID.String()); err != nil {
		t.Fatalf("AddEdge A->B: %v", err)
	}
	if err := g.AddEdge(a.ID.String(), c.ID.String()); err != nil {
		t.Fatalf("AddEdge A->C: %v", err)
	}
	if err := g.AddEdge(b.ID.String(), d.ID.String()); err != nil {
		t.Fatalf("AddEdge B->D: %v", err)
	}
	if err := g.AddEdge(c.ID.String(), d.ID.String()); err != nil {
		t.Fatalf("AddEdge C->D: %v", err)
	}

	// Ancestors of A = {B, C, D}.
	ancestors, err := helpers.Ancestors(g, db, a.ID)
	if err != nil {
		t.Fatalf("Ancestors(A) error: %v", err)
	}

	if !containsTask(ancestors, b.ID) {
		t.Errorf("B not in Ancestors(A)")
	}
	if !containsTask(ancestors, c.ID) {
		t.Errorf("C not in Ancestors(A)")
	}
	if !containsTask(ancestors, d.ID) {
		t.Errorf("D not in Ancestors(A)")
	}
	if len(ancestors) != 3 {
		t.Errorf("Ancestors(A) = %d tasks, want 3", len(ancestors))
	}

	// Descendants of D = {A, B, C}.
	descendants, err := helpers.Descendants(g, db, d.ID)
	if err != nil {
		t.Fatalf("Descendants(D) error: %v", err)
	}

	if !containsTask(descendants, a.ID) {
		t.Errorf("A not in Descendants(D)")
	}
	if !containsTask(descendants, b.ID) {
		t.Errorf("B not in Descendants(D)")
	}
	if !containsTask(descendants, c.ID) {
		t.Errorf("C not in Descendants(D)")
	}
	if len(descendants) != 3 {
		t.Errorf("Descendants(D) = %d tasks, want 3", len(descendants))
	}
}

func TestAncestorsEmptyGraph(t *testing.T) {
	db := openTestDB(t)
	g := intgraph.NewGraph(db)

	task := makeTask("ns", "lonely")
	if err := g.AddVertex(task); err != nil {
		t.Fatalf("AddVertex error: %v", err)
	}

	ancestors, err := helpers.Ancestors(g, db, task.ID)
	if err != nil {
		t.Fatalf("Ancestors error: %v", err)
	}
	if len(ancestors) != 0 {
		t.Errorf("Ancestors of isolated node = %d, want 0", len(ancestors))
	}

	descendants, err := helpers.Descendants(g, db, task.ID)
	if err != nil {
		t.Fatalf("Descendants error: %v", err)
	}
	if len(descendants) != 0 {
		t.Errorf("Descendants of isolated node = %d, want 0", len(descendants))
	}
}
