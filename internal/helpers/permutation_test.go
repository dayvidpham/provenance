package helpers_test

import (
	_ "embed"
	"fmt"
	"testing"

	intgraph "github.com/dayvidpham/provenance/internal/graph"
	"github.com/dayvidpham/provenance/internal/helpers"
	"github.com/dayvidpham/provenance/internal/testutil"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	dgraph "github.com/dominikbraun/graph"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Fixture types — mirrors testdata/fixtures.yaml structure
// ---------------------------------------------------------------------------

// GraphFixtures is the top-level fixture container.
type GraphFixtures struct {
	GraphTopologies []TopologyFixture `yaml:"graph_topologies"`
}

// TopologyFixture describes one named graph topology and its expected traversal
// results for each queried node.
type TopologyFixture struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Nodes       []string       `yaml:"nodes"`
	Edges       []EdgeFixture  `yaml:"edges"`
	Queries     []QueryFixture `yaml:"queries"`
}

// EdgeFixture describes a single directed edge: source is blocked by target.
type EdgeFixture struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

// QueryFixture describes expected Ancestors and Descendants for one node label.
type QueryFixture struct {
	Node        string   `yaml:"node"`
	Ancestors   []string `yaml:"ancestors"`
	Descendants []string `yaml:"descendants"`
}

// ---------------------------------------------------------------------------
// Fixture loading via go:embed
// ---------------------------------------------------------------------------

//go:embed testdata/fixtures.yaml
var graphFixtureData []byte

func loadGraphFixtures(t *testing.T) GraphFixtures {
	t.Helper()
	var f GraphFixtures
	if err := yaml.Unmarshal(graphFixtureData, &f); err != nil {
		t.Fatalf(
			"permutation_test: failed to parse testdata/fixtures.yaml: %v — "+
				"check that the YAML is well-formed and matches the GraphFixtures struct",
			err,
		)
	}
	return f
}

// ---------------------------------------------------------------------------
// Graph setup helper
// ---------------------------------------------------------------------------

// setupTopology creates a fresh in-memory DB and graph for the given topology.
// It returns the graph, the DB, and a map from node label (e.g. "A") to the
// concrete ptypes.Task so that queries can look up expected IDs by label.
func setupTopology(t *testing.T, topo TopologyFixture) (
	dgraph.Graph[string, ptypes.Task],
	helpers.TaskGetter,
	map[string]ptypes.Task,
) {
	t.Helper()

	db := testutil.OpenTestDB(t)
	g := intgraph.NewGraph(db)

	// Create one task per node label.
	nodeMap := make(map[string]ptypes.Task, len(topo.Nodes))
	for _, label := range topo.Nodes {
		task := testutil.MakeTask("fixture", fmt.Sprintf("node-%s", label))
		if err := db.SeedLegacyTaskRow(task); err != nil {
			t.Fatalf(
				"permutation_test.setupTopology: topology %q: SeedLegacyTaskRow for node %q failed: %v — "+
					"check that the task ID is unique and the DB schema is correct",
				topo.Name, label, err,
			)
		}
		nodeMap[label] = task
	}

	// Add edges according to the fixture definition.
	for _, edge := range topo.Edges {
		src, ok := nodeMap[edge.Source]
		if !ok {
			t.Fatalf(
				"permutation_test.setupTopology: topology %q: edge source label %q not found in nodes list — "+
					"every edge source must appear in the nodes list",
				topo.Name, edge.Source,
			)
		}
		tgt, ok := nodeMap[edge.Target]
		if !ok {
			t.Fatalf(
				"permutation_test.setupTopology: topology %q: edge target label %q not found in nodes list — "+
					"every edge target must appear in the nodes list",
				topo.Name, edge.Target,
			)
		}
		if err := g.AddEdge(src.ID.String(), tgt.ID.String()); err != nil {
			t.Fatalf(
				"permutation_test.setupTopology: topology %q: AddEdge %q -> %q failed: %v — "+
					"verify that the edge does not create a cycle",
				topo.Name, edge.Source, edge.Target, err,
			)
		}
	}

	return g, db, nodeMap
}

// taskIDSet builds a set of TaskID strings from a slice of Tasks for O(1) lookup.
func taskIDSet(tasks []ptypes.Task) map[string]bool {
	s := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		s[task.ID.String()] = true
	}
	return s
}

// assertTaskSet verifies that got (as a set) exactly matches the expected labels.
// It reports missing and extra entries, and also checks the queried node is absent.
func assertTaskSet(
	t *testing.T,
	testName string,
	topologyName string,
	queriedLabel string,
	queriedTask ptypes.Task,
	got []ptypes.Task,
	expectedLabels []string,
	nodeMap map[string]ptypes.Task,
) {
	t.Helper()

	gotSet := taskIDSet(got)

	// The queried node must never appear in its own traversal result.
	if gotSet[queriedTask.ID.String()] {
		t.Errorf(
			"%s: topology %q: %s(%q) must not include the queried node itself",
			testName, topologyName, testName, queriedLabel,
		)
	}

	// Build expected set from labels and verify each label resolves.
	wantLabels := make([]string, 0, len(expectedLabels))
	for _, label := range expectedLabels {
		if _, ok := nodeMap[label]; !ok {
			t.Errorf(
				"%s: topology %q: expected label %q not found in nodes list — "+
					"every expected label must appear in the nodes list",
				testName, topologyName, label,
			)
			continue
		}
		wantLabels = append(wantLabels, label)
	}

	// Count mismatch check.
	if len(got) != len(wantLabels) {
		t.Errorf(
			"%s: topology %q node %q: returned %d tasks, want %d",
			testName, topologyName, queriedLabel, len(got), len(wantLabels),
		)
	}

	// Check each expected label is present in result.
	for _, label := range wantLabels {
		task := nodeMap[label]
		if !gotSet[task.ID.String()] {
			t.Errorf(
				"%s: topology %q node %q: expected %q missing from result",
				testName, topologyName, queriedLabel, label,
			)
		}
	}
}

// ---------------------------------------------------------------------------
// Permutation tests: Ancestors
// ---------------------------------------------------------------------------

func TestAncestors_GraphTopologies(t *testing.T) {
	fixtures := loadGraphFixtures(t)

	for _, topo := range fixtures.GraphTopologies {
		topo := topo // capture for parallel subtests
		t.Run(topo.Name, func(t *testing.T) {
			g, db, nodeMap := setupTopology(t, topo)

			for _, query := range topo.Queries {
				query := query // capture for parallel subtests
				t.Run(fmt.Sprintf("node=%s", query.Node), func(t *testing.T) {
					queried, ok := nodeMap[query.Node]
					if !ok {
						t.Fatalf(
							"TestAncestors_GraphTopologies: topology %q query node %q not in nodeMap — "+
								"every query node must appear in the nodes list",
							topo.Name, query.Node,
						)
					}

					got, err := helpers.Ancestors(g, db, queried.ID)
					if err != nil {
						t.Fatalf(
							"TestAncestors_GraphTopologies: topology %q node %q: Ancestors returned error: %v",
							topo.Name, query.Node, err,
						)
					}

					assertTaskSet(
						t,
						"TestAncestors_GraphTopologies",
						topo.Name,
						query.Node,
						queried,
						got,
						query.Ancestors,
						nodeMap,
					)
				})
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Permutation tests: Descendants
// ---------------------------------------------------------------------------

func TestDescendants_GraphTopologies(t *testing.T) {
	fixtures := loadGraphFixtures(t)

	for _, topo := range fixtures.GraphTopologies {
		topo := topo // capture for parallel subtests
		t.Run(topo.Name, func(t *testing.T) {
			g, db, nodeMap := setupTopology(t, topo)

			for _, query := range topo.Queries {
				query := query // capture for parallel subtests
				t.Run(fmt.Sprintf("node=%s", query.Node), func(t *testing.T) {
					queried, ok := nodeMap[query.Node]
					if !ok {
						t.Fatalf(
							"TestDescendants_GraphTopologies: topology %q query node %q not in nodeMap — "+
								"every query node must appear in the nodes list",
							topo.Name, query.Node,
						)
					}

					got, err := helpers.Descendants(g, db, queried.ID)
					if err != nil {
						t.Fatalf(
							"TestDescendants_GraphTopologies: topology %q node %q: Descendants returned error: %v",
							topo.Name, query.Node, err,
						)
					}

					assertTaskSet(
						t,
						"TestDescendants_GraphTopologies",
						topo.Name,
						query.Node,
						queried,
						got,
						query.Descendants,
						nodeMap,
					)
				})
			}
		})
	}
}
