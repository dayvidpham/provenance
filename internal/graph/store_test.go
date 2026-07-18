package graph_test

import (
	"errors"
	"testing"

	intgraph "github.com/dayvidpham/provenance/internal/graph"
	dbsqlite "github.com/dayvidpham/provenance/internal/sqlite"
	"github.com/dayvidpham/provenance/internal/testutil"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	dgraph "github.com/dominikbraun/graph"
)

// openTestDB delegates to shared testutil.OpenTestDB.
func openTestDB(t *testing.T) *dbsqlite.DB { return testutil.OpenTestDB(t) }

// makeTask delegates to shared testutil.MakeTask with a default title.
func makeTask(ns string) ptypes.Task { return testutil.MakeTask(ns, "test task") }

// seedTask puts a task row on disk via the OLD-schema legacy seeding seam. Task
// creation is journaled, so the graph store no longer creates rows (AddVertex is
// retired as a creation path); a base-layer graph test that needs a pre-existing
// vertex seeds the underlying tasks row directly, exactly as a legacy database on disk
// would carry it, and the store then sees it as a vertex (Vertex reads tasks(id)).
func seedTask(t *testing.T, db *dbsqlite.DB, task ptypes.Task) {
	t.Helper()
	if err := db.SeedLegacyTaskRow(task); err != nil {
		t.Fatalf("seedTask(%s): %v", task.ID.String(), err)
	}
}

func TestNewStoreImplementsInterface(t *testing.T) {
	db := openTestDB(t)
	store := intgraph.NewStore(db)

	// Compile-time check is in store.go via var _ dgraph.Store = (*Store)(nil).
	// Runtime check: the store should be non-nil.
	if store == nil {
		t.Fatal("NewStore returned nil")
	}
}

// TestAddVertexRejectsDirectCreate pins the retired creation semantics: AddVertex for a
// task with no row is a direct-write creation attempt and fails with
// ErrDirectVertexCreate; AddVertex for an already-seeded task is the dominikbraun
// already-exists condition; neither writes a row.
func TestAddVertexRejectsDirectCreate(t *testing.T) {
	db := openTestDB(t)
	store := intgraph.NewStore(db)

	task := makeTask("test-ns")
	hash := task.ID.String()

	// No row yet: a creation attempt is rejected, structurally, naming Session.Create.
	err := store.AddVertex(hash, task, dgraph.VertexProperties{})
	if !errors.Is(err, intgraph.ErrDirectVertexCreate) {
		t.Fatalf("AddVertex for uncreated task: got %v, want ErrDirectVertexCreate", err)
	}
	// It wrote nothing — the task is still absent.
	if _, _, verr := store.Vertex(hash); verr != dgraph.ErrVertexNotFound {
		t.Fatalf("Vertex after rejected AddVertex: got %v, want ErrVertexNotFound", verr)
	}

	// Once the task exists (journaled birth is modelled here by the legacy seam), a
	// repeat AddVertex is the already-exists condition and Vertex returns the task.
	seedTask(t, db, task)
	if err := store.AddVertex(hash, task, dgraph.VertexProperties{}); err != dgraph.ErrVertexAlreadyExists {
		t.Fatalf("AddVertex for existing task: got %v, want ErrVertexAlreadyExists", err)
	}
	got, _, err := store.Vertex(hash)
	if err != nil {
		t.Fatalf("Vertex() error: %v", err)
	}
	if got.ID != task.ID {
		t.Errorf("Vertex() ID = %v, want %v", got.ID, task.ID)
	}
	if got.Title != task.Title {
		t.Errorf("Vertex() Title = %q, want %q", got.Title, task.Title)
	}
}

func TestAddVertexHashMismatch(t *testing.T) {
	db := openTestDB(t)
	store := intgraph.NewStore(db)

	task := makeTask("test-ns")

	// The hash/id consistency check runs before any existence lookup, so a mismatched
	// hash is rejected regardless of whether the task row exists.
	err := store.AddVertex("wrong-hash", task, dgraph.VertexProperties{})
	if err == nil {
		t.Fatal("AddVertex with mismatched hash should return error")
	}
	if errors.Is(err, intgraph.ErrDirectVertexCreate) {
		t.Errorf("hash-mismatch error should not be ErrDirectVertexCreate: %v", err)
	}
}

func TestVertexNotFound(t *testing.T) {
	db := openTestDB(t)
	store := intgraph.NewStore(db)

	fakeID := testutil.MakeTaskID("ns")
	_, _, err := store.Vertex(fakeID.String())
	if err != dgraph.ErrVertexNotFound {
		t.Errorf("Vertex() for non-existent task: got %v, want ErrVertexNotFound", err)
	}
}

func TestRemoveVertexNotImplemented(t *testing.T) {
	db := openTestDB(t)
	store := intgraph.NewStore(db)

	err := store.RemoveVertex("anything")
	if err == nil {
		t.Fatal("RemoveVertex should return an error (not implemented)")
	}
}

func TestListVerticesAndVertexCount(t *testing.T) {
	db := openTestDB(t)
	store := intgraph.NewStore(db)

	// Initially empty.
	hashes, err := store.ListVertices()
	if err != nil {
		t.Fatalf("ListVertices() error: %v", err)
	}
	if len(hashes) != 0 {
		t.Errorf("ListVertices() initial = %d, want 0", len(hashes))
	}

	count, err := store.VertexCount()
	if err != nil {
		t.Fatalf("VertexCount() error: %v", err)
	}
	if count != 0 {
		t.Errorf("VertexCount() initial = %d, want 0", count)
	}

	// Seed two tasks.
	t1 := makeTask("ns")
	t2 := makeTask("ns")
	seedTask(t, db, t1)
	seedTask(t, db, t2)

	hashes, err = store.ListVertices()
	if err != nil {
		t.Fatalf("ListVertices() error: %v", err)
	}
	if len(hashes) != 2 {
		t.Errorf("ListVertices() = %d, want 2", len(hashes))
	}

	count, err = store.VertexCount()
	if err != nil {
		t.Fatalf("VertexCount() error: %v", err)
	}
	if count != 2 {
		t.Errorf("VertexCount() = %d, want 2", count)
	}
}

func TestAddEdgeAndEdge(t *testing.T) {
	db := openTestDB(t)
	store := intgraph.NewStore(db)

	t1 := makeTask("ns")
	t2 := makeTask("ns")
	seedTask(t, db, t1)
	seedTask(t, db, t2)

	edge := dgraph.Edge[string]{Source: t1.ID.String(), Target: t2.ID.String()}
	if err := store.AddEdge(t1.ID.String(), t2.ID.String(), edge); err != nil {
		t.Fatalf("AddEdge() error: %v", err)
	}

	got, err := store.Edge(t1.ID.String(), t2.ID.String())
	if err != nil {
		t.Fatalf("Edge() error: %v", err)
	}
	if got.Source != t1.ID.String() {
		t.Errorf("Edge source = %q, want %q", got.Source, t1.ID.String())
	}
	if got.Target != t2.ID.String() {
		t.Errorf("Edge target = %q, want %q", got.Target, t2.ID.String())
	}
}

func TestEdgeNotFound(t *testing.T) {
	db := openTestDB(t)
	store := intgraph.NewStore(db)

	t1 := makeTask("ns")
	t2 := makeTask("ns")
	seedTask(t, db, t1)
	seedTask(t, db, t2)

	_, err := store.Edge(t1.ID.String(), t2.ID.String())
	if err != dgraph.ErrEdgeNotFound {
		t.Errorf("Edge() for non-existent edge: got %v, want ErrEdgeNotFound", err)
	}
}

func TestRemoveEdge(t *testing.T) {
	db := openTestDB(t)
	store := intgraph.NewStore(db)

	t1 := makeTask("ns")
	t2 := makeTask("ns")
	seedTask(t, db, t1)
	seedTask(t, db, t2)

	edge := dgraph.Edge[string]{Source: t1.ID.String(), Target: t2.ID.String()}
	if err := store.AddEdge(t1.ID.String(), t2.ID.String(), edge); err != nil {
		t.Fatalf("AddEdge() error: %v", err)
	}

	if err := store.RemoveEdge(t1.ID.String(), t2.ID.String()); err != nil {
		t.Fatalf("RemoveEdge() error: %v", err)
	}

	_, err := store.Edge(t1.ID.String(), t2.ID.String())
	if err != dgraph.ErrEdgeNotFound {
		t.Errorf("Edge() after RemoveEdge: got %v, want ErrEdgeNotFound", err)
	}
}

func TestListEdges(t *testing.T) {
	db := openTestDB(t)
	store := intgraph.NewStore(db)

	t1 := makeTask("ns")
	t2 := makeTask("ns")
	t3 := makeTask("ns")
	for _, task := range []ptypes.Task{t1, t2, t3} {
		seedTask(t, db, task)
	}

	// Add two edges.
	if err := store.AddEdge(t1.ID.String(), t2.ID.String(), dgraph.Edge[string]{}); err != nil {
		t.Fatalf("AddEdge t1->t2 error: %v", err)
	}
	if err := store.AddEdge(t1.ID.String(), t3.ID.String(), dgraph.Edge[string]{}); err != nil {
		t.Fatalf("AddEdge t1->t3 error: %v", err)
	}

	edges, err := store.ListEdges()
	if err != nil {
		t.Fatalf("ListEdges() error: %v", err)
	}
	if len(edges) != 2 {
		t.Errorf("ListEdges() = %d edges, want 2", len(edges))
	}
}

func TestNewGraphCycleDetection(t *testing.T) {
	db := openTestDB(t)
	g := intgraph.NewGraph(db)

	t1 := makeTask("ns")
	t2 := makeTask("ns")
	seedTask(t, db, t1)
	seedTask(t, db, t2)

	// t1 -> t2 (t1 blocked by t2).
	if err := g.AddEdge(t1.ID.String(), t2.ID.String()); err != nil {
		t.Fatalf("AddEdge t1->t2: %v", err)
	}

	// t2 -> t1 should be rejected (cycle).
	err := g.AddEdge(t2.ID.String(), t1.ID.String())
	if err == nil {
		t.Fatal("AddEdge t2->t1 should have been rejected (cycle)")
	}
	if err != dgraph.ErrEdgeCreatesCycle {
		t.Errorf("AddEdge t2->t1: got %v, want ErrEdgeCreatesCycle", err)
	}
}
