package bdimport

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance"
	"github.com/dayvidpham/provenance/pkg/provo"
)

// ---------------------------------------------------------------------------
// Mapping (pure) unit tests
// ---------------------------------------------------------------------------

func TestStatusForBD(t *testing.T) {
	cases := []struct {
		in   string
		want provenance.Status
		ok   bool
	}{
		{"open", provenance.StatusOpen, true},
		{"in_progress", provenance.StatusInProgress, true},
		{"closed", provenance.StatusClosed, true},
		{"frozen", provenance.StatusOpen, false},
	}
	for _, c := range cases {
		got, ok := statusForBD(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("statusForBD(%q) = (%v,%v), want (%v,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestTaskTypeForBD(t *testing.T) {
	cases := []struct {
		in   string
		want provenance.TaskType
		ok   bool
	}{
		{"bug", provenance.TaskTypeBug, true},
		{"feature", provenance.TaskTypeFeature, true},
		{"task", provenance.TaskTypeTask, true},
		{"epic", provenance.TaskTypeEpic, true},
		{"chore", provenance.TaskTypeChore, true},
		{"saga", provenance.TaskTypeTask, false},
	}
	for _, c := range cases {
		got, ok := taskTypeForBD(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("taskTypeForBD(%q) = (%v,%v), want (%v,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestPriorityForBD(t *testing.T) {
	cases := []struct {
		in   int
		want provenance.Priority
		ok   bool
	}{
		{0, provenance.PriorityCritical, true},
		{4, provenance.PriorityBacklog, true},
		{-1, provenance.PriorityCritical, false},
		{9, provenance.PriorityBacklog, false},
	}
	for _, c := range cases {
		got, ok := priorityForBD(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("priorityForBD(%d) = (%v,%v), want (%v,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestEdgeKindForDep(t *testing.T) {
	cases := []struct {
		in   string
		want provenance.EdgeKind
		ok   bool
	}{
		{"blocks", provenance.EdgeBlockedBy, true},
		{"discovered-from", provenance.EdgeDiscoveredFrom, true},
		{"parent-child", provenance.EdgeBlockedBy, false},
		{"related", provenance.EdgeBlockedBy, false},
	}
	for _, c := range cases {
		got, ok := edgeKindForDep(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("edgeKindForDep(%q) = (%v,%v), want (%v,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestDeterministicIDs(t *testing.T) {
	a := TaskID("ns", "bd-1")
	b := TaskID("ns", "bd-1")
	if a != b {
		t.Fatalf("TaskID not deterministic: %v vs %v", a, b)
	}
	if TaskID("ns", "bd-1") == TaskID("ns", "bd-2") {
		t.Fatal("distinct bd ids collided")
	}
	if TaskID("ns1", "bd-1") == TaskID("ns2", "bd-1") {
		t.Fatal("distinct namespaces collided")
	}
	if a.Namespace != "ns" {
		t.Fatalf("namespace not carried: %q", a.Namespace)
	}
	// Activity ids are a distinct derivation from task ids.
	if TaskID("ns", "bd-1").UUID == ActivityID("ns", "bd-1").UUID {
		t.Fatal("task and activity uuid derivations collided")
	}
}

// ---------------------------------------------------------------------------
// Source
// ---------------------------------------------------------------------------

func TestFileSource_Load(t *testing.T) {
	d, err := FileSource{Path: "testdata/fixture-basic.json"}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Issues) != 4 {
		t.Fatalf("issues = %d, want 4", len(d.Issues))
	}
	if len(d.Comments) != 2 {
		t.Fatalf("comments = %d, want 2", len(d.Comments))
	}
}

// ---------------------------------------------------------------------------
// Import — dry-run mapping summary
// ---------------------------------------------------------------------------

func TestImport_DryRun(t *testing.T) {
	res, err := Import(nil, FileSource{Path: "testdata/fixture-basic.json"},
		Options{Namespace: "t", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun {
		t.Fatal("DryRun flag not set on result")
	}
	// 4 tasks, 4 activities, 2 human agents + importer = 3.
	assertEq(t, "tasks", res.Tasks, 4)
	assertEq(t, "activities", res.Activities, 4)
	assertEq(t, "agents", res.Agents, 3)
	// deps: fix-b blocks+discovered-from fix-a (2, both mapped+present); fix-c both
	// skipped (dangling + unmapped). attributed: 4 (all have created_by). generated: 4.
	assertEq(t, "edges", res.Edges, 2+4+4)
	assertEq(t, "comments", res.Comments, 2)
	// fix-a: 2 labels + assignee(1); fix-c: 1 label => 4.
	assertEq(t, "labels", res.Labels, 4)
	// Warnings: unknown status, unknown type, out-of-range priority, unmapped dep,
	// dangling dep = 5.
	if len(res.Warnings) != 5 {
		t.Fatalf("warnings = %d, want 5:\n%v", len(res.Warnings), res.Warnings)
	}
}

// TestImport_WarningParity proves the primary write path surfaces exactly the same
// coercion/clamp/skip warnings as the dry-run plan for a degenerate fixture — no silent
// coercion asymmetry between them.
func TestImport_WarningParity(t *testing.T) {
	src := FileSource{Path: "testdata/fixture-basic.json"}

	dry, err := Import(nil, src, Options{Namespace: "t", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := openMem(t)
	real, err := Import(tr, src, Options{Namespace: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if d, r := sortedCopy(dry.Warnings), sortedCopy(real.Warnings); !equalStrings(d, r) {
		t.Fatalf("warning parity broken:\n dry-run (%d): %v\n import  (%d): %v", len(d), d, len(r), r)
	}
	if len(real.Warnings) == 0 {
		t.Fatal("expected the degenerate fixture to produce warnings")
	}
}

// TestImport_CycleWarnAndSkip proves a cycle-inducing dependency (2-cycle + self-dep)
// does not abort the import: the offending edges are warned and skipped, and every other
// task, label, comment, and edge still lands. Also covers the orphan-comment warning.
func TestImport_CycleWarnAndSkip(t *testing.T) {
	tr := openMem(t)
	res, err := Import(tr, FileSource{Path: "testdata/fixture-cycle.json"}, Options{Namespace: "t"})
	if err != nil {
		t.Fatalf("import must complete despite cycles, got: %v", err)
	}

	// Two cycle rejections (b→a closes the 2-cycle; x→x self-loop) + one orphan comment.
	var rejected, orphans int
	for _, w := range res.Warnings {
		if strings.Contains(w, "rejected") {
			rejected++
		}
		if strings.Contains(w, "not in dump (skipped)") {
			orphans++
		}
	}
	if rejected != 2 {
		t.Fatalf("expected 2 rejected cycle edges, got %d: %v", rejected, res.Warnings)
	}
	if orphans != 1 {
		t.Fatalf("expected 1 orphan-comment warning, got %d: %v", orphans, res.Warnings)
	}

	// The first edge of the 2-cycle landed; the closing edge did not.
	a, b := TaskID("t", "cyc-a"), TaskID("t", "cyc-b")
	if !hasEdge(t, tr, a, b, provenance.EdgeBlockedBy) {
		t.Error("cyc-a → cyc-b (first edge) should have landed")
	}
	if hasEdge(t, tr, b, a, provenance.EdgeBlockedBy) {
		t.Error("cyc-b → cyc-a (cycle-closing edge) must have been skipped")
	}
	// The self-dep produced no self edge.
	if hasEdge(t, tr, TaskID("t", "cyc-x"), TaskID("t", "cyc-x"), provenance.EdgeBlockedBy) {
		t.Error("cyc-x self-edge must have been skipped")
	}

	// All other content intact: cyc-c task, its label, and its comment.
	c := TaskID("t", "cyc-c")
	if _, err := tr.Show(c); err != nil {
		t.Fatalf("clean task cyc-c missing: %v", err)
	}
	if labels, _ := tr.Labels(c); !contains(labels, "clean") {
		t.Errorf("cyc-c label missing: %v", labels)
	}
	if cs, _ := tr.Comments(c); len(cs) != 1 {
		t.Errorf("cyc-c comment count = %d, want 1", len(cs))
	}
	// All four tasks present.
	if tasks, _ := tr.List(provenance.ListFilter{}); len(tasks) != 4 {
		t.Errorf("task count = %d, want 4", len(tasks))
	}
}

// ---------------------------------------------------------------------------
// Import — correctness
// ---------------------------------------------------------------------------

func TestImport_Correctness(t *testing.T) {
	tr := openMem(t)
	if _, err := Import(tr, FileSource{Path: "testdata/fixture-basic.json"}, Options{Namespace: "t"}); err != nil {
		t.Fatal(err)
	}

	// fix-a: closed bug, critical priority, 2 labels + assignee label, 2 comments,
	// activity ended.
	a := TaskID("t", "fix-a")
	ta, err := tr.Show(a)
	if err != nil {
		t.Fatal(err)
	}
	assertEq(t, "fix-a status", int(ta.Status), int(provenance.StatusClosed))
	assertEq(t, "fix-a type", int(ta.Type), int(provenance.TaskTypeBug))
	assertEq(t, "fix-a priority", int(ta.Priority), int(provenance.PriorityCritical))

	labels, _ := tr.Labels(a)
	if !contains(labels, "bd:assignee:worker-slot-1") || !contains(labels, "area:core") || !contains(labels, "kind:defect") {
		t.Fatalf("fix-a labels missing expected values: %v", labels)
	}

	comments, _ := tr.Comments(a)
	if len(comments) != 2 {
		t.Fatalf("fix-a comments = %d, want 2", len(comments))
	}

	// fix-c: unknown status/type default to open/task; priority 9 clamps to backlog.
	c := TaskID("t", "fix-c")
	tc, err := tr.Show(c)
	if err != nil {
		t.Fatal(err)
	}
	assertEq(t, "fix-c status(default)", int(tc.Status), int(provenance.StatusOpen))
	assertEq(t, "fix-c type(default)", int(tc.Type), int(provenance.TaskTypeTask))
	assertEq(t, "fix-c priority(clamped)", int(tc.Priority), int(provenance.PriorityBacklog))

	// fix-d: in_progress feature.
	td, _ := tr.Show(TaskID("t", "fix-d"))
	assertEq(t, "fix-d status", int(td.Status), int(provenance.StatusInProgress))

	// fix-b: blocked_by AND discovered_from fix-a.
	b := TaskID("t", "fix-b")
	if !hasEdge(t, tr, b, a, provenance.EdgeBlockedBy) {
		t.Error("fix-b missing blocked_by fix-a")
	}
	if !hasEdge(t, tr, b, a, provenance.EdgeDiscoveredFrom) {
		t.Error("fix-b missing discovered_from fix-a")
	}
	// fix-c dangling + unmapped deps produced no dep edges.
	if hasEdge(t, tr, c, TaskID("t", "ghost-99"), provenance.EdgeBlockedBy) {
		t.Error("fix-c should not have an edge to a non-imported target")
	}

	// Attribution + generated-by exist for fix-a.
	attr := provenance.EdgeAttributedTo
	if es, _ := tr.Edges(a, &attr); len(es) != 1 {
		t.Errorf("fix-a attributed_to edges = %d, want 1", len(es))
	}
	gen := provenance.EdgeGeneratedBy
	if es, _ := tr.Edges(a, &gen); len(es) != 1 || es[0].TargetID != ActivityID("t", "fix-a").String() {
		t.Errorf("fix-a generated_by edge wrong: %v", es)
	}

	// fix-a activity is ended (closed issue).
	acts, _ := tr.Activities(nil)
	var found bool
	for _, act := range acts {
		if act.ID == ActivityID("t", "fix-a") {
			found = true
			if act.EndedAt == nil {
				t.Error("fix-a activity should be ended (closed issue)")
			}
		}
	}
	if !found {
		t.Error("fix-a activity not found")
	}
}

// ---------------------------------------------------------------------------
// Import — idempotency (re-import must be a no-op)
// ---------------------------------------------------------------------------

func TestImport_Idempotent(t *testing.T) {
	tr := openMem(t)
	src := FileSource{Path: "testdata/dogfood/bd-dump.json"}

	r1, err := Import(tr, src, Options{Namespace: "t"})
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, tr)

	r2, err := Import(tr, src, Options{Namespace: "t"})
	if err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, tr)

	// Second run creates nothing.
	if r2.Tasks+r2.Edges+r2.Agents+r2.Activities+r2.Comments+r2.Labels != 0 {
		t.Fatalf("re-import created entities: %+v", r2)
	}
	// Store totals unchanged.
	before.assertEqual(t, after)
	// And the first run actually did the work.
	if r1.Tasks == 0 || r1.Edges == 0 || r1.Comments == 0 {
		t.Fatalf("first import produced too little: %+v", r1)
	}
	t.Logf("idempotent: first=%+v store=%+v", r1, after)
}

// TestImport_ActivityIdempotent re-validates the StartActivityWithID idempotency claim
// against the journal model: a deterministic activity id is inserted once and the second
// import does not duplicate it (see doc.go header re-survey).
func TestImport_ActivityIdempotent(t *testing.T) {
	tr := openMem(t)
	src := FileSource{Path: "testdata/fixture-basic.json"}
	if _, err := Import(tr, src, Options{Namespace: "t"}); err != nil {
		t.Fatal(err)
	}
	acts1, _ := tr.Activities(nil)
	if _, err := Import(tr, src, Options{Namespace: "t"}); err != nil {
		t.Fatal(err)
	}
	acts2, _ := tr.Activities(nil)
	if len(acts1) != len(acts2) || len(acts1) != 4 {
		t.Fatalf("activity count changed on re-import: %d -> %d (want 4)", len(acts1), len(acts2))
	}
}

// ---------------------------------------------------------------------------
// Dog-food: import the committed real subset, export Turtle, conform.
// ---------------------------------------------------------------------------

func TestDogfood_ImportCounts(t *testing.T) {
	tr := openMem(t)
	res, err := Import(tr, FileSource{Path: "testdata/dogfood/bd-dump.json"}, Options{Namespace: "taxonomy-of-benchmarks"})
	if err != nil {
		t.Fatal(err)
	}
	assertEq(t, "dogfood tasks", res.Tasks, 11)
	assertEq(t, "dogfood activities", res.Activities, 11)
	// 8 blocks + 7 discovered-from = 15 deps, +11 attributed +11 generated = 37.
	assertEq(t, "dogfood edges", res.Edges, 37)
	assertEq(t, "dogfood comments", res.Comments, 30)
	if len(res.Warnings) != 0 {
		t.Fatalf("dogfood subset should be self-contained, warnings: %v", res.Warnings)
	}
}

// TestDogfood_Conformance imports the committed dump, exports data-labour-prov Turtle,
// and validates it with Apache Jena (riot --validate + shacl validate) against the
// vendored shapes. Skips cleanly when the Jena binaries are not on PATH.
func TestDogfood_Conformance(t *testing.T) {
	riot, riotErr := exec.LookPath("riot")
	shacl, shaclErr := exec.LookPath("shacl")
	if riotErr != nil || shaclErr != nil {
		t.Skip("riot and/or shacl not on PATH (Apache Jena); run inside `nix develop`")
	}
	tr := openMem(t)
	if _, err := Import(tr, FileSource{Path: "testdata/dogfood/bd-dump.json"}, Options{Namespace: "taxonomy-of-benchmarks"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := provo.ExportTurtle(&buf, tr, provo.Options{
		BaseIRI:  "urn:provenance:taxonomy-of-benchmarks:",
		Registry: provenance.DefaultModelRegistry(),
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	graph := filepath.Join(dir, "graph.ttl")
	if err := os.WriteFile(graph, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(riot, "--validate", graph).CombinedOutput(); err != nil {
		t.Fatalf("riot --validate failed: %v\n%s", err, out)
	}
	shapes := filepath.Join("..", "provo", "testdata", "ontology", "shapes.ttl")
	out, err := exec.Command(shacl, "validate", "--shapes", shapes, "--data", graph).CombinedOutput()
	if err != nil {
		t.Fatalf("shacl validate failed to run: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("sh:conforms  true")) {
		t.Fatalf("dog-food graph did not conform:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func openMem(t *testing.T) provenance.Tracker {
	t.Helper()
	tr, err := provenance.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	return tr
}

func assertEq(t *testing.T, name string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", name, got, want)
	}
}

func sortedCopy(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func hasEdge(t *testing.T, tr provenance.Tracker, src, target provenance.TaskID, kind provenance.EdgeKind) bool {
	t.Helper()
	es, err := tr.Edges(src, &kind)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.TargetID == target.String() {
			return true
		}
	}
	return false
}

type storeSnapshot struct {
	tasks, edges, activities, comments, agents int
	statuses                                   map[string]provenance.Status
}

func snapshot(t *testing.T, tr provenance.Tracker) storeSnapshot {
	t.Helper()
	tasks, err := tr.List(provenance.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	edges, _ := tr.AllEdges()
	acts, _ := tr.Activities(nil)
	actors, _ := tr.AllActors()
	s := storeSnapshot{
		tasks: len(tasks), edges: len(edges), activities: len(acts), agents: len(actors),
		statuses: map[string]provenance.Status{},
	}
	for _, tk := range tasks {
		s.statuses[tk.ID.String()] = tk.Status
		cs, _ := tr.Comments(tk.ID)
		s.comments += len(cs)
	}
	return s
}

func (s storeSnapshot) assertEqual(t *testing.T, o storeSnapshot) {
	t.Helper()
	assertEq(t, "snapshot tasks", o.tasks, s.tasks)
	assertEq(t, "snapshot edges", o.edges, s.edges)
	assertEq(t, "snapshot activities", o.activities, s.activities)
	assertEq(t, "snapshot comments", o.comments, s.comments)
	assertEq(t, "snapshot agents", o.agents, s.agents)
	// Deep sample: per-task status must be identical.
	for id, st := range s.statuses {
		if o.statuses[id] != st {
			t.Errorf("status of %s changed: %v -> %v", id, st, o.statuses[id])
		}
	}
}
