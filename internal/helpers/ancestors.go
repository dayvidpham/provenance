// Package helpers provides graph traversal utilities for the Providence
// blocked-by dependency graph. These functions perform DFS over the
// dominikbraun/graph structure and resolve traversed IDs to full Task values.
package helpers

import (
	"fmt"

	"github.com/dayvidpham/providence/pkg/ptypes"
	dgraph "github.com/dominikbraun/graph"
)

// TaskGetter is the minimal interface needed by graph traversal helpers.
// It allows looking up a single task by ID without coupling to the concrete
// sqlite.DB type, making helpers independently testable with mocks.
type TaskGetter interface {
	GetTask(id ptypes.TaskID) (ptypes.Task, bool, error)
}

// Ancestors returns all tasks that transitively block the given task.
// In the blocked-by graph, A->B means "A is blocked by B". Ancestors of A
// are B and everything B transitively waits for (outgoing adjacency DFS).
// The given task itself is never included. Returns an empty slice if none.
func Ancestors(g dgraph.Graph[string, ptypes.Task], db TaskGetter, id ptypes.TaskID) ([]ptypes.Task, error) {
	adjacency, err := g.AdjacencyMap()
	if err != nil {
		return nil, fmt.Errorf(
			"helpers.Ancestors: failed to compute adjacency map for task %q: %w",
			id.String(), err,
		)
	}

	var ids []ptypes.TaskID
	visited := make(map[string]bool)
	var dfs func(cur string)
	dfs = func(cur string) {
		for adj := range adjacency[cur] {
			if !visited[adj] {
				visited[adj] = true
				if tid, err := ptypes.ParseTaskID(adj); err == nil {
					ids = append(ids, tid)
				}
				dfs(adj)
			}
		}
	}
	dfs(id.String())

	tasks := make([]ptypes.Task, 0, len(ids))
	for _, tid := range ids {
		task, found, err := db.GetTask(tid)
		if err != nil {
			return nil, fmt.Errorf("helpers.Ancestors: failed to resolve task %q: %w", tid.String(), err)
		}
		if found {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

// Descendants returns all tasks that are transitively waiting for the given
// task to complete. In the blocked-by graph, A->B means "A is blocked by B".
// Descendants of B are A and everything that transitively depends on A
// (predecessor DFS). The given task itself is never included. Returns an
// empty slice if none.
func Descendants(g dgraph.Graph[string, ptypes.Task], db TaskGetter, id ptypes.TaskID) ([]ptypes.Task, error) {
	predecessors, err := g.PredecessorMap()
	if err != nil {
		return nil, fmt.Errorf(
			"helpers.Descendants: failed to compute predecessor map for task %q: %w",
			id.String(), err,
		)
	}

	var ids []ptypes.TaskID
	visited := make(map[string]bool)
	var dfs func(cur string)
	dfs = func(cur string) {
		for pred := range predecessors[cur] {
			if !visited[pred] {
				visited[pred] = true
				if tid, err := ptypes.ParseTaskID(pred); err == nil {
					ids = append(ids, tid)
				}
				dfs(pred)
			}
		}
	}
	dfs(id.String())

	tasks := make([]ptypes.Task, 0, len(ids))
	for _, tid := range ids {
		task, found, err := db.GetTask(tid)
		if err != nil {
			return nil, fmt.Errorf("helpers.Descendants: failed to resolve task %q: %w", tid.String(), err)
		}
		if found {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}
