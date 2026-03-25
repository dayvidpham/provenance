// Package helpers provides graph traversal helpers over a dominikbraun/graph
// backed by providence tasks. These functions operate on the blocked-by subgraph
// produced by graph.NewBlockedByGraph.
package helpers

import (
	"fmt"

	"github.com/dayvidpham/providence"
	dgraph "github.com/dominikbraun/graph"
)

// Ancestors returns all TaskIDs that transitively block the given task by
// following blocked-by edges forward (via AdjacencyMap).
//
// In the blocked-by graph an edge A→B means "A is blocked by B". Ancestors of
// A are therefore the set of tasks that A must wait for — B and everything that
// B transitively waits for. The traversal follows outgoing (adjacency) edges
// because those represent the "blocked by" relationships.
//
// The result set is unordered. The given task's own ID is never included.
// Returns an empty slice (not an error) when no blockers exist.
func Ancestors(g dgraph.Graph[string, providence.Task], id providence.TaskID) ([]providence.TaskID, error) {
	adjacency, err := g.AdjacencyMap()
	if err != nil {
		return nil, fmt.Errorf(
			"helpers.Ancestors: failed to compute adjacency map for task %q: %w — "+
				"check that the graph store (sqlite.DB) is accessible",
			id.String(), err,
		)
	}

	var result []providence.TaskID
	visited := make(map[string]bool)

	var dfs func(current string)
	dfs = func(current string) {
		for adj := range adjacency[current] {
			if !visited[adj] {
				visited[adj] = true
				tid, err := providence.ParseTaskID(adj)
				if err == nil {
					result = append(result, tid)
				}
				dfs(adj)
			}
		}
	}

	dfs(id.String())
	return result, nil
}

// Descendants returns all TaskIDs that are transitively waiting for the given
// task to complete, by following blocked-by edges backward (via PredecessorMap).
//
// In the blocked-by graph an edge A→B means "A is blocked by B". Descendants of
// B are therefore the set of tasks that cannot proceed until B finishes — A and
// everything that transitively depends on A. The traversal follows incoming
// (predecessor) edges because those represent the "is being waited on by"
// relationships.
//
// The result set is unordered. The given task's own ID is never included.
// Returns an empty slice (not an error) when no waiters exist.
func Descendants(g dgraph.Graph[string, providence.Task], id providence.TaskID) ([]providence.TaskID, error) {
	predecessors, err := g.PredecessorMap()
	if err != nil {
		return nil, fmt.Errorf(
			"helpers.Descendants: failed to compute predecessor map for task %q: %w — "+
				"check that the graph store (sqlite.DB) is accessible",
			id.String(), err,
		)
	}

	var result []providence.TaskID
	visited := make(map[string]bool)

	var dfs func(current string)
	dfs = func(current string) {
		for pred := range predecessors[current] {
			if !visited[pred] {
				visited[pred] = true
				tid, err := providence.ParseTaskID(pred)
				if err == nil {
					result = append(result, tid)
				}
				dfs(pred)
			}
		}
	}

	dfs(id.String())
	return result, nil
}
