package provenance_test

import (
	"sort"
	"testing"

	"github.com/dayvidpham/provenance"
)

// bulk_reads_test.go exercises the whole-graph read surface added for the PROV-O
// exporter: Tracker.AllActors, Tracker.AllEdges, and the Edge.CreatedAt field now
// surfaced from the edges.created_at column.

func TestAllActors(t *testing.T) {
	tt := openMemorySession(t)
	defer tt.Close()

	human, err := tt.RegisterHumanAgent("bulk", "Grace", "grace@example.org")
	if err != nil {
		t.Fatalf("RegisterHumanAgent: %v", err)
	}
	sw, err := tt.RegisterSoftwareAgent("bulk", "linter", "1.0", "src")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	ml, err := tt.RegisterMLAgent("bulk", provenance.RoleWorker, provenance.ProviderAnthropic, provenance.ModelID("claude-opus-4-6"))
	if err != nil {
		t.Fatalf("RegisterMLAgent: %v", err)
	}

	actors, err := tt.AllActors()
	if err != nil {
		t.Fatalf("AllActors: %v", err)
	}
	// openMemorySession also registers one system software agent, so expect 4.
	if len(actors) != 4 {
		t.Fatalf("AllActors returned %d actors, want 4", len(actors))
	}

	// Sorted ascending by ID string.
	if !sort.SliceIsSorted(actors, func(i, j int) bool { return actors[i].ID.String() < actors[j].ID.String() }) {
		t.Errorf("AllActors is not sorted by ID")
	}

	// Kinds are correctly discriminated on the base rows.
	kinds := map[string]provenance.AgentKind{}
	for _, a := range actors {
		kinds[a.ID.String()] = a.Kind
	}
	if kinds[human.ID.String()] != provenance.AgentKindHuman {
		t.Errorf("human agent kind = %v, want human", kinds[human.ID.String()])
	}
	if kinds[sw.ID.String()] != provenance.AgentKindSoftware {
		t.Errorf("software agent kind = %v, want software", kinds[sw.ID.String()])
	}
	if kinds[ml.ID.String()] != provenance.AgentKindMachineLearning {
		t.Errorf("ml agent kind = %v, want machine_learning", kinds[ml.ID.String()])
	}
}

func TestAllEdgesCarriesCreatedAtAndBlockedBy(t *testing.T) {
	tt := openMemorySession(t)
	defer tt.Close()

	a, err := tt.Create("bulk", "A", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create(A): %v", err)
	}
	b, err := tt.Create("bulk", "B", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create(B): %v", err)
	}
	// A derived_from B and A blocked_by B.
	if err := tt.AddEdge(a.ID, b.ID.String(), provenance.EdgeDerivedFrom); err != nil {
		t.Fatalf("AddEdge(derived_from): %v", err)
	}
	if err := tt.AddEdge(a.ID, b.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge(blocked_by): %v", err)
	}

	edges, err := tt.AllEdges()
	if err != nil {
		t.Fatalf("AllEdges: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("AllEdges returned %d edges, want 2 (incl. blocked_by)", len(edges))
	}
	var sawBlockedBy, sawDerived bool
	for _, e := range edges {
		if e.CreatedAt.IsZero() {
			t.Errorf("edge %v -> %v has zero CreatedAt (created_at not surfaced)", e.SourceID, e.TargetID)
		}
		switch e.Kind {
		case provenance.EdgeBlockedBy:
			sawBlockedBy = true
		case provenance.EdgeDerivedFrom:
			sawDerived = true
		}
	}
	if !sawBlockedBy {
		t.Errorf("AllEdges omitted the blocked_by edge; it must return ALL kinds")
	}
	if !sawDerived {
		t.Errorf("AllEdges omitted the derived_from edge")
	}
}

func TestEdgesPopulatesCreatedAt(t *testing.T) {
	tt := openMemorySession(t)
	defer tt.Close()

	a, err := tt.Create("bulk", "A", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create(A): %v", err)
	}
	b, err := tt.Create("bulk", "B", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create(B): %v", err)
	}
	if err := tt.AddEdge(a.ID, b.ID.String(), provenance.EdgeDerivedFrom); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	edges, err := tt.Edges(a.ID, nil)
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("Edges returned %d, want 1", len(edges))
	}
	if edges[0].CreatedAt.IsZero() {
		t.Errorf("per-task Edges did not surface CreatedAt")
	}
}
