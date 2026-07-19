// Package graph provides the dominikbraun/graph.Store adapter backed by
// the Provenance SQLite database. It bridges the in-memory graph library
// with persistent storage so that cycle detection and traversal work over
// the full blocked-by subgraph.
package graph

import (
	"errors"
	"fmt"
	"time"

	dbsqlite "github.com/dayvidpham/provenance/internal/sqlite"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	dgraph "github.com/dominikbraun/graph"
)

// ErrDirectVertexCreate is returned by Store.AddVertex when it is asked to create a
// graph vertex for a task that has no row in the tasks table. Task creation is
// journaled: a task is born only through the reducer fold (Session.Create / an Atomic
// EffectTaskCreate), which INSERTs the tasks row with a non-NULL journal watermark. The
// graph is a live, read-only view over that table (Store.Vertex reads tasks(id)), so
// there is no direct-write vertex-creation path — this rejection is the structural
// analogue of the journal's direct-write-rejection invariant. errors.Is recovers it.
var ErrDirectVertexCreate = errors.New("provenance: direct graph-vertex creation is not permitted")

// Store implements dgraph.Store[string, ptypes.Task] for the blocked-by
// subgraph. All persistence is delegated to the internal/sqlite.DB.
type Store struct {
	db *dbsqlite.DB
}

var _ dgraph.Store[string, ptypes.Task] = (*Store)(nil)

// NewStore returns a Store backed by the given sqlite.DB.
func NewStore(db *dbsqlite.DB) *Store {
	return &Store{db: db}
}

// NewGraph constructs a directed, cycle-preventing dgraph.Graph using this
// Store for persistence. This is the canonical way to obtain the blocked-by
// graph.
func NewGraph(db *dbsqlite.DB) dgraph.Graph[string, ptypes.Task] {
	store := NewStore(db)
	return dgraph.NewWithStore(
		func(task ptypes.Task) string { return task.ID.String() },
		store,
		dgraph.Directed(),
		dgraph.PreventCycles(),
	)
}

// AddVertex satisfies the dgraph.Store interface but no longer creates rows: task
// creation is journaled (Session.Create / an Atomic EffectTaskCreate), and the graph is
// a live view over the tasks table. A call for a task that already has a row is the
// dominikbraun already-exists condition (ErrVertexAlreadyExists); a call for a task with
// no row is a retired direct-write creation attempt and fails with ErrDirectVertexCreate
// naming Session.Create as the path. It writes nothing.
func (s *Store) AddVertex(hash string, value ptypes.Task, _ dgraph.VertexProperties) error {
	if hash != value.ID.String() {
		return fmt.Errorf(
			"graph.Store.AddVertex: hash %q does not match task ID %q — "+
				"the hash function must return task.ID.String()",
			hash, value.ID.String(),
		)
	}
	_, found, err := s.db.GetTask(value.ID)
	if err != nil {
		return fmt.Errorf("graph.Store.AddVertex: check task %q existence: %w", hash, err)
	}
	if found {
		return dgraph.ErrVertexAlreadyExists
	}
	return fmt.Errorf(
		"%w: graph.Store.AddVertex for task %q — where: blocked-by graph store adapter; "+
			"why: task creation is journaled, so a task row is born only through the reducer "+
			"fold (a non-NULL journal watermark), never a direct graph-vertex write; "+
			"impact: nothing was written; fix: create the task via Session.Create (or an "+
			"Atomic EffectTaskCreate) first — the graph then sees it as a vertex automatically",
		ErrDirectVertexCreate, hash)
}

func (s *Store) Vertex(hash string) (ptypes.Task, dgraph.VertexProperties, error) {
	id, err := ptypes.ParseTaskID(hash)
	if err != nil {
		return ptypes.Task{}, dgraph.VertexProperties{}, fmt.Errorf(
			"graph.Store.Vertex: cannot parse hash %q as TaskID: %w", hash, err,
		)
	}
	task, found, err := s.db.GetTask(id)
	if err != nil {
		return ptypes.Task{}, dgraph.VertexProperties{}, fmt.Errorf(
			"graph.Store.Vertex: failed to get task %q: %w", hash, err,
		)
	}
	if !found {
		return ptypes.Task{}, dgraph.VertexProperties{}, dgraph.ErrVertexNotFound
	}
	return task, dgraph.VertexProperties{}, nil
}

func (s *Store) RemoveVertex(_ string) error {
	return fmt.Errorf(
		"graph.Store.RemoveVertex: not implemented — " +
			"close the task via CloseTask instead of deleting it",
	)
}

func (s *Store) ListVertices() ([]string, error) {
	tasks, err := s.db.ListTasks(ptypes.ListFilter{})
	if err != nil {
		return nil, fmt.Errorf("graph.Store.ListVertices: %w", err)
	}
	hashes := make([]string, len(tasks))
	for i, task := range tasks {
		hashes[i] = task.ID.String()
	}
	return hashes, nil
}

func (s *Store) VertexCount() (int, error) {
	count, err := s.db.TaskCount()
	if err != nil {
		return 0, fmt.Errorf("graph.Store.VertexCount: %w", err)
	}
	return count, nil
}

func (s *Store) AddEdge(sourceHash, targetHash string, _ dgraph.Edge[string]) error {
	srcID, err := ptypes.ParseTaskID(sourceHash)
	if err != nil {
		return fmt.Errorf("graph.Store.AddEdge: invalid source hash %q: %w", sourceHash, err)
	}
	return s.db.InsertEdge(srcID, targetHash, ptypes.EdgeBlockedBy, time.Now().UTC())
}

func (s *Store) UpdateEdge(sourceHash, targetHash string, _ dgraph.Edge[string]) error {
	_, err := s.Edge(sourceHash, targetHash)
	return err
}

func (s *Store) RemoveEdge(sourceHash, targetHash string) error {
	srcID, err := ptypes.ParseTaskID(sourceHash)
	if err != nil {
		return fmt.Errorf("graph.Store.RemoveEdge: invalid source hash %q: %w", sourceHash, err)
	}
	return s.db.DeleteEdge(srcID, targetHash, ptypes.EdgeBlockedBy)
}

func (s *Store) Edge(sourceHash, targetHash string) (dgraph.Edge[string], error) {
	srcID, err := ptypes.ParseTaskID(sourceHash)
	if err != nil {
		return dgraph.Edge[string]{}, fmt.Errorf("graph.Store.Edge: invalid source hash %q: %w", sourceHash, err)
	}
	kind := ptypes.EdgeBlockedBy
	edges, err := s.db.GetEdges(srcID, &kind)
	if err != nil {
		return dgraph.Edge[string]{}, fmt.Errorf("graph.Store.Edge: %w", err)
	}
	for _, e := range edges {
		if e.TargetID == targetHash {
			return dgraph.Edge[string]{
				Source:     sourceHash,
				Target:     targetHash,
				Properties: dgraph.EdgeProperties{Attributes: map[string]string{}},
			}, nil
		}
	}
	return dgraph.Edge[string]{}, dgraph.ErrEdgeNotFound
}

func (s *Store) ListEdges() ([]dgraph.Edge[string], error) {
	edges, err := s.db.GetBlockedByEdges()
	if err != nil {
		return nil, fmt.Errorf("graph.Store.ListEdges: %w", err)
	}
	result := make([]dgraph.Edge[string], len(edges))
	for i, e := range edges {
		result[i] = dgraph.Edge[string]{
			Source:     e.SourceID,
			Target:     e.TargetID,
			Properties: dgraph.EdgeProperties{Attributes: map[string]string{}},
		}
	}
	return result, nil
}
