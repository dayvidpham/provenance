// Package graph provides a dominikbraun/graph Store backed by internal/sqlite.
// It exposes a single constructor, NewBlockedByGraph, which returns a directed,
// cycle-preventing graph whose vertices are providence.Task values and whose edges
// represent blocked-by relationships.
package graph

import (
	"fmt"
	"time"

	"github.com/dayvidpham/providence"
	"github.com/dayvidpham/providence/internal/sqlite"
	dgraph "github.com/dominikbraun/graph"
)

// sqliteStore implements dgraph.Store[string, providence.Task] backed by a
// *sqlite.DB. The hash key K is the task ID wire format ("namespace--uuid").
// Each sqlite function acquires the DB mutex internally; this Store must NOT
// acquire it separately.
type sqliteStore struct {
	db *sqlite.DB
}

// Verify at compile time that *sqliteStore satisfies Store[string, Task].
var _ dgraph.Store[string, providence.Task] = (*sqliteStore)(nil)

// AddVertex inserts a task into the database. The hash must equal task.ID.String().
// If the vertex already exists the insert is silently ignored (idempotent).
func (s *sqliteStore) AddVertex(hash string, value providence.Task, _ dgraph.VertexProperties) error {
	// Validate the hash matches the task ID.
	if hash != value.ID.String() {
		return fmt.Errorf(
			"graph.sqliteStore.AddVertex: hash %q does not match task ID %q — "+
				"the hash function must be func(t Task) string { return t.ID.String() }; "+
				"ensure NewBlockedByGraph's hash function is used when calling AddVertex",
			hash, value.ID.String(),
		)
	}
	if err := sqlite.InsertTask(s.db, value); err != nil {
		return fmt.Errorf(
			"graph.sqliteStore.AddVertex: failed to insert task %q into sqlite: %w — "+
				"check that the task ID is valid and the database is writable",
			hash, err,
		)
	}
	return nil
}

// Vertex retrieves the task with the given hash (task ID wire format).
// Returns dgraph.ErrVertexNotFound if no such task exists.
func (s *sqliteStore) Vertex(hash string) (providence.Task, dgraph.VertexProperties, error) {
	id, err := providence.ParseTaskID(hash)
	if err != nil {
		return providence.Task{}, dgraph.VertexProperties{}, fmt.Errorf(
			"graph.sqliteStore.Vertex: cannot parse hash %q as TaskID: %w — "+
				"hash values in this store are task ID wire strings (namespace--uuidv7)",
			hash, err,
		)
	}

	task, found, err := sqlite.GetTask(s.db, id)
	if err != nil {
		return providence.Task{}, dgraph.VertexProperties{}, fmt.Errorf(
			"graph.sqliteStore.Vertex: failed to get task %q: %w",
			hash, err,
		)
	}
	if !found {
		return providence.Task{}, dgraph.VertexProperties{}, dgraph.ErrVertexNotFound
	}
	return task, dgraph.VertexProperties{}, nil
}

// RemoveVertex is not needed for the MVP. It returns an error unconditionally.
func (s *sqliteStore) RemoveVertex(hash string) error {
	return fmt.Errorf(
		"graph.sqliteStore.RemoveVertex: not implemented — " +
			"vertex removal is not supported in this release; " +
			"close (status=closed) the task via sqlite.CloseTask instead of deleting it",
	)
}

// ListVertices returns the string hash of every task in the database.
func (s *sqliteStore) ListVertices() ([]string, error) {
	tasks, err := sqlite.ListTasks(s.db, providence.ListFilter{})
	if err != nil {
		return nil, fmt.Errorf(
			"graph.sqliteStore.ListVertices: failed to list tasks: %w",
			err,
		)
	}
	hashes := make([]string, len(tasks))
	for i, t := range tasks {
		hashes[i] = t.ID.String()
	}
	return hashes, nil
}

// VertexCount returns the total number of tasks in the database.
func (s *sqliteStore) VertexCount() (int, error) {
	hashes, err := s.ListVertices()
	if err != nil {
		return 0, fmt.Errorf("graph.sqliteStore.VertexCount: %w", err)
	}
	return len(hashes), nil
}

// AddEdge inserts a blocked-by edge from sourceHash to targetHash.
// The edge kind is always EdgeBlockedBy; the Edge metadata is otherwise ignored.
func (s *sqliteStore) AddEdge(sourceHash, targetHash string, _ dgraph.Edge[string]) error {
	srcID, err := providence.ParseTaskID(sourceHash)
	if err != nil {
		return fmt.Errorf(
			"graph.sqliteStore.AddEdge: invalid source hash %q: %w — "+
				"edge source and target must be task ID wire strings",
			sourceHash, err,
		)
	}
	if err := sqlite.InsertEdge(s.db, srcID, targetHash, providence.EdgeBlockedBy, time.Now()); err != nil {
		return fmt.Errorf(
			"graph.sqliteStore.AddEdge: failed to insert edge %q->%q: %w",
			sourceHash, targetHash, err,
		)
	}
	return nil
}

// UpdateEdge is a no-op: blocked-by edges carry no mutable properties.
// It returns nil so the graph library can update edge metadata without error.
func (s *sqliteStore) UpdateEdge(sourceHash, targetHash string, _ dgraph.Edge[string]) error {
	// Verify the edge exists.
	_, err := s.Edge(sourceHash, targetHash)
	return err
}

// RemoveEdge deletes the blocked-by edge from sourceHash to targetHash.
func (s *sqliteStore) RemoveEdge(sourceHash, targetHash string) error {
	srcID, err := providence.ParseTaskID(sourceHash)
	if err != nil {
		return fmt.Errorf(
			"graph.sqliteStore.RemoveEdge: invalid source hash %q: %w",
			sourceHash, err,
		)
	}
	if err := sqlite.DeleteEdge(s.db, srcID, targetHash, providence.EdgeBlockedBy); err != nil {
		return fmt.Errorf(
			"graph.sqliteStore.RemoveEdge: failed to delete edge %q->%q: %w",
			sourceHash, targetHash, err,
		)
	}
	return nil
}

// Edge returns the dgraph.Edge[string] for the given source→target pair.
// Returns dgraph.ErrEdgeNotFound if no such blocked-by edge exists.
func (s *sqliteStore) Edge(sourceHash, targetHash string) (dgraph.Edge[string], error) {
	srcID, err := providence.ParseTaskID(sourceHash)
	if err != nil {
		return dgraph.Edge[string]{}, fmt.Errorf(
			"graph.sqliteStore.Edge: invalid source hash %q: %w",
			sourceHash, err,
		)
	}

	kind := providence.EdgeBlockedBy
	edges, err := sqlite.GetEdges(s.db, srcID, &kind)
	if err != nil {
		return dgraph.Edge[string]{}, fmt.Errorf(
			"graph.sqliteStore.Edge: failed to query edges for %q: %w",
			sourceHash, err,
		)
	}

	for _, e := range edges {
		if e.TargetID == targetHash {
			return dgraph.Edge[string]{
				Source: sourceHash,
				Target: targetHash,
				Properties: dgraph.EdgeProperties{
					Attributes: map[string]string{},
				},
			}, nil
		}
	}
	return dgraph.Edge[string]{}, dgraph.ErrEdgeNotFound
}

// ListEdges returns all blocked-by edges in the database as dgraph.Edge[string].
func (s *sqliteStore) ListEdges() ([]dgraph.Edge[string], error) {
	edges, err := sqlite.GetBlockedByEdges(s.db)
	if err != nil {
		return nil, fmt.Errorf(
			"graph.sqliteStore.ListEdges: failed to query blocked-by edges: %w",
			err,
		)
	}
	result := make([]dgraph.Edge[string], len(edges))
	for i, e := range edges {
		result[i] = dgraph.Edge[string]{
			Source: e.SourceID,
			Target: e.TargetID,
			Properties: dgraph.EdgeProperties{
				Attributes: map[string]string{},
			},
		}
	}
	return result, nil
}

// NewBlockedByGraph creates a directed, cycle-preventing graph of providence.Task
// vertices backed by the provided SQLite database. Edges represent blocked-by
// relationships: an edge from A to B means "A is blocked by B" (B must finish first).
//
// The hash function is func(t Task) string { return t.ID.String() }, so graph
// operations use the task ID wire format ("namespace--uuid") as the hash key.
//
// Use graph.AddVertex to register tasks and graph.AddEdge to add dependencies.
// Cycle detection is enforced by the dominikbraun/graph library: if adding an
// edge would form a cycle, AddEdge returns dgraph.ErrEdgeCreatesCycle, which the
// Tracker layer maps to providence.ErrCycleDetected.
func NewBlockedByGraph(db *sqlite.DB) dgraph.Graph[string, providence.Task] {
	store := &sqliteStore{db: db}
	return dgraph.NewWithStore(
		func(t providence.Task) string { return t.ID.String() },
		store,
		dgraph.Directed(),
		dgraph.PreventCycles(),
	)
}
