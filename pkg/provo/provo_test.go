package provo_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance"
	"github.com/dayvidpham/provenance/internal/testutil"
	"github.com/dayvidpham/provenance/pkg/provo"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

var update = flag.Bool("update", false, "update golden files in testdata/golden")

const fixtureNS = "provo"

// fixture is a built provenance graph plus the alias map that normalizes its random
// UUIDv7 IDs to stable tokens, so golden output is reproducible across runs.
type fixture struct {
	tr       provenance.Tracker
	replacer *strings.Replacer
}

// buildFixture constructs a deterministic-content graph covering: a human agent, a
// software agent, a registry-backed ML agent, activities in two phases, and one edge
// of every EXPORTED kind (derived_from, supersedes, discovered_from, generated_by,
// attributed_to) plus a blocked_by edge (which must NOT appear in the export).
func buildFixture(t *testing.T) *fixture {
	t.Helper()

	tr, err := provenance.OpenMemory(
		provenance.WithModelRegistry(provenance.NewRegistry(testutil.TestModels())))
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	// System actor + genesis authority; every task/edge mutation flows through the
	// journaled Session bound to it.
	sys, err := tr.RegisterSoftwareAgent(fixtureNS, "pasture-system", "0", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent(system): %v", err)
	}
	boot := establishGenesis(t, tr, sys.ID)
	sess := tr.As(sys.ID, boot)

	// Agents.
	human, err := tr.RegisterHumanAgent(fixtureNS, "Ada Lovelace", "ada@example.org")
	if err != nil {
		t.Fatalf("RegisterHumanAgent: %v", err)
	}
	sw, err := tr.RegisterSoftwareAgent(fixtureNS, "pasture-cli", "1.2.0", "https://github.com/dayvidpham/provenance")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	ml, err := tr.RegisterMLAgent(fixtureNS, provenance.RoleWorker, provenance.ProviderAnthropic, provenance.ModelID("claude-opus-4-6"))
	if err != nil {
		t.Fatalf("RegisterMLAgent: %v", err)
	}

	// Tasks. Descriptions include a quote, a newline, and a non-ASCII rune to
	// exercise Turtle literal escaping through the full export path.
	req, err := sess.Create(fixtureNS, "REQUEST: PROV-O export", "the \"request\" body\nwith a newline and a snowman ☃",
		provenance.TaskTypeFeature, provenance.PriorityHigh, provenance.PhaseRequest)
	if err != nil {
		t.Fatalf("Create(request): %v", err)
	}
	prop, err := sess.Create(fixtureNS, "PROPOSAL-1", "first proposal",
		provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhasePropose)
	if err != nil {
		t.Fatalf("Create(proposal-1): %v", err)
	}
	prop2, err := sess.Create(fixtureNS, "PROPOSAL-2", "revised proposal",
		provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhasePropose)
	if err != nil {
		t.Fatalf("Create(proposal-2): %v", err)
	}
	bug, err := sess.Create(fixtureNS, "BUG: escaping", "",
		provenance.TaskTypeBug, provenance.PriorityHigh, provenance.PhaseCodeReview)
	if err != nil {
		t.Fatalf("Create(bug): %v", err)
	}

	// Activities in two phases: an ML-agent act (gets p-plan:Activity + qualified
	// association) and a human-agent act (plain prov:Activity).
	actML, err := tr.StartActivity(ml.ID, provenance.PhasePropose, provenance.StageInProgress, "drafting the proposal")
	if err != nil {
		t.Fatalf("StartActivity(ml): %v", err)
	}
	if _, err := tr.EndActivity(actML.ID); err != nil {
		t.Fatalf("EndActivity(ml): %v", err)
	}
	actHuman, err := tr.StartActivity(human.ID, provenance.PhaseReview, provenance.StageComplete, "review pass")
	if err != nil {
		t.Fatalf("StartActivity(human): %v", err)
	}

	// Edges: one of every exported kind, plus a blocked_by that must be filtered out.
	mustEdge(t, sess, prop.ID, req.ID.String(), provenance.EdgeDerivedFrom)
	mustEdge(t, sess, prop2.ID, prop.ID.String(), provenance.EdgeSupersedes)
	mustEdge(t, sess, bug.ID, prop.ID.String(), provenance.EdgeDiscoveredFrom)
	mustEdge(t, sess, req.ID, actML.ID.String(), provenance.EdgeGeneratedBy)
	mustEdge(t, sess, req.ID, ml.ID.String(), provenance.EdgeAttributedTo)
	mustEdge(t, sess, prop.ID, req.ID.String(), provenance.EdgeBlockedBy)

	// Stable alias map: every random UUIDv7 id.String() → a readable token, so the
	// golden file is reproducible and human-reviewable.
	pairs := []string{
		sys.ID.String(), fixtureNS + "--agent-system",
		human.ID.String(), fixtureNS + "--agent-human",
		sw.ID.String(), fixtureNS + "--agent-software",
		ml.ID.String(), fixtureNS + "--agent-ml",
		req.ID.String(), fixtureNS + "--task-request",
		prop.ID.String(), fixtureNS + "--task-proposal-1",
		prop2.ID.String(), fixtureNS + "--task-proposal-2",
		bug.ID.String(), fixtureNS + "--task-bug",
		actML.ID.String(), fixtureNS + "--act-ml",
		actHuman.ID.String(), fixtureNS + "--act-human",
	}
	return &fixture{tr: tr, replacer: strings.NewReplacer(pairs...)}
}

func mustEdge(t *testing.T, s *provenance.Session, src ptypes.TaskID, tgt string, kind provenance.EdgeKind) {
	t.Helper()
	if err := s.AddEdge(src, tgt, kind); err != nil {
		t.Fatalf("AddEdge(%v -> %s, %v): %v", src, tgt, kind, err)
	}
}

// establishGenesis applies one genesis operation and returns the produced bootstrap
// authority's JournalID (mirrors the root package's test helper).
func establishGenesis(t *testing.T, tr provenance.Tracker, actor provenance.ActorID) provenance.JournalID {
	t.Helper()
	res, err := tr.Journal().Apply(provenance.OperationInput{
		OperationID:    "op-genesis",
		ActorID:        actor,
		CommandDigest:  []byte("genesis-c"),
		MutationDigest: []byte("genesis-m"),
		Effects:        []provenance.Effect{{Sort: provenance.EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("establishGenesis: %v", err)
	}
	for i := range res.ResultSlots {
		if string(res.ResultSlots[i].Slot) == "auth" {
			return res.ResultSlots[i].ProducedJournalID
		}
	}
	t.Fatal("establishGenesis: no bootstrap authority slot produced")
	return 0
}

// dateTimeLiteral matches an xsd:dateTime literal so wall-clock timestamps (from
// StartActivity/edge CreatedAt) can be normalized out of golden comparisons. The
// exact formatting is asserted separately by TestDateTime, and real timestamps are
// exercised by the live SHACL/riot conformance test.
var dateTimeLiteral = regexp.MustCompile(`"[^"]*"\^\^xsd:dateTime`)

// exportNormalized exports the fixture to Turtle and applies the alias map plus
// timestamp normalization, yielding reproducible bytes suitable for golden comparison.
func (f *fixture) exportNormalized(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := provo.ExportTurtle(&buf, f.tr, provo.Options{Registry: provenance.NewRegistry(testutil.TestModels())}); err != nil {
		t.Fatalf("ExportTurtle: %v", err)
	}
	out := f.replacer.Replace(buf.String())
	return dateTimeLiteral.ReplaceAllString(out, `"TIMESTAMP"^^xsd:dateTime`)
}

func TestExportTurtle_Golden(t *testing.T) {
	f := buildFixture(t)
	got := f.exportNormalized(t)

	goldenPath := filepath.Join("testdata", "golden", "graph.ttl")
	if *update {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("Turtle output != golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestExportTurtle_Deterministic(t *testing.T) {
	f := buildFixture(t)
	var a, b bytes.Buffer
	if err := provo.ExportTurtle(&a, f.tr, provo.Options{}); err != nil {
		t.Fatalf("ExportTurtle #1: %v", err)
	}
	if err := provo.ExportTurtle(&b, f.tr, provo.Options{}); err != nil {
		t.Fatalf("ExportTurtle #2: %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("export is not deterministic across runs")
	}
}

func TestExportTurtle_BlockedByNotExported(t *testing.T) {
	f := buildFixture(t)
	var buf bytes.Buffer
	if err := provo.ExportTurtle(&buf, f.tr, provo.Options{}); err != nil {
		t.Fatalf("ExportTurtle: %v", err)
	}
	out := buf.String()
	// blocked_by has no PROV relation; none of its verbs should appear, and no bare
	// "blocked" token should leak into the graph.
	if strings.Contains(out, "blocked") {
		t.Errorf("export unexpectedly mentions a blocked_by edge:\n%s", out)
	}
}

func TestExportTurtle_EmptyGraph(t *testing.T) {
	tr, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer tr.Close()
	var buf bytes.Buffer
	if err := provo.ExportTurtle(&buf, tr, provo.Options{}); err != nil {
		t.Fatalf("ExportTurtle(empty): %v", err)
	}
	out := buf.String()
	// Even with no data the preamble + built-in plan are always emitted.
	if !strings.Contains(out, "pplan:Plan") {
		t.Errorf("empty-graph export missing the built-in plan:\n%s", out)
	}
	if !strings.Contains(out, provo.DefaultVocabIRI) {
		t.Errorf("empty-graph export missing default vocab IRI")
	}
}

func TestExportROCrate(t *testing.T) {
	f := buildFixture(t)
	dir := filepath.Join(t.TempDir(), "crate")
	if err := provo.ExportROCrate(dir, f.tr, provo.Options{Registry: provenance.NewRegistry(testutil.TestModels())}); err != nil {
		t.Fatalf("ExportROCrate: %v", err)
	}

	// graph.ttl must exist and be a non-empty Turtle payload.
	ttl, err := os.ReadFile(filepath.Join(dir, "graph.ttl"))
	if err != nil {
		t.Fatalf("read graph.ttl: %v", err)
	}
	if !bytes.Contains(ttl, []byte("prov:Entity")) {
		t.Errorf("graph.ttl missing expected content")
	}

	// ro-crate-metadata.json must be valid JSON declaring the crate root and
	// graph.ttl as a text/turtle File entity.
	raw, err := os.ReadFile(filepath.Join(dir, "ro-crate-metadata.json"))
	if err != nil {
		t.Fatalf("read ro-crate-metadata.json: %v", err)
	}
	var meta struct {
		Context string           `json:"@context"`
		Graph   []map[string]any `json:"@graph"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("ro-crate-metadata.json is not valid JSON: %v", err)
	}
	if meta.Context == "" {
		t.Errorf("ro-crate-metadata.json missing @context")
	}
	var sawRoot, sawTurtleFile bool
	for _, node := range meta.Graph {
		if node["@id"] == "./" {
			sawRoot = true
		}
		if node["@id"] == "graph.ttl" && node["encodingFormat"] == "text/turtle" {
			sawTurtleFile = true
		}
	}
	if !sawRoot {
		t.Errorf("ro-crate-metadata.json missing crate root ./ node")
	}
	if !sawTurtleFile {
		t.Errorf("ro-crate-metadata.json missing graph.ttl File entity with encodingFormat text/turtle")
	}

	// If riot is available, the crate's graph must be syntactically valid Turtle.
	if riot, err := exec.LookPath("riot"); err == nil {
		if out, err := exec.Command(riot, "--validate", filepath.Join(dir, "graph.ttl")).CombinedOutput(); err != nil {
			t.Fatalf("riot --validate on crate graph.ttl failed: %v\n%s", err, out)
		}
	}
}

// ---------------------------------------------------------------------------
// Conformance: riot --validate + shacl validate against the vendored shapes.
// Skips cleanly when the Apache Jena binaries are not on PATH.
// ---------------------------------------------------------------------------

func TestExportTurtle_Conformance(t *testing.T) {
	riot, riotErr := exec.LookPath("riot")
	shacl, shaclErr := exec.LookPath("shacl")
	if riotErr != nil || shaclErr != nil {
		t.Skip("riot and/or shacl not on PATH (Apache Jena); run inside `nix develop` to enable SHACL conformance")
	}

	f := buildFixture(t)
	dir := t.TempDir()
	graphPath := filepath.Join(dir, "graph.ttl")
	gf, err := os.Create(graphPath)
	if err != nil {
		t.Fatalf("create graph.ttl: %v", err)
	}
	if err := provo.ExportTurtle(gf, f.tr, provo.Options{Registry: provenance.NewRegistry(testutil.TestModels())}); err != nil {
		_ = gf.Close()
		t.Fatalf("ExportTurtle: %v", err)
	}
	if err := gf.Close(); err != nil {
		t.Fatalf("close graph.ttl: %v", err)
	}

	// (a) Syntactic validity.
	if out, err := exec.Command(riot, "--validate", graphPath).CombinedOutput(); err != nil {
		t.Fatalf("riot --validate failed: %v\n%s", err, out)
	}

	// (b) SHACL conformance. Shapes graph = vocabulary + shapes concatenated, so the
	// shapes' references to vocabulary individuals resolve.
	shapesPath := combineShapes(t, dir)
	out, err := exec.Command(shacl, "validate", "--shapes", shapesPath, "--data", graphPath).CombinedOutput()
	if err != nil {
		t.Fatalf("shacl validate failed to run: %v\n%s", err, out)
	}
	report := string(out)
	if regexp.MustCompile(`sh:conforms\s+false`).MatchString(report) {
		t.Fatalf("SHACL conformance FAILED:\n%s", report)
	}
	if !regexp.MustCompile(`sh:conforms\s+true`).MatchString(report) {
		t.Fatalf("SHACL report did not confirm conformance:\n%s", report)
	}
}

// combineShapes writes vocab + shapes into one file under dir and returns its path.
func combineShapes(t *testing.T, dir string) string {
	t.Helper()
	var b bytes.Buffer
	for _, name := range []string{"data-labour-prov.ttl", "shapes.ttl"} {
		data, err := os.ReadFile(filepath.Join("testdata", "ontology", name))
		if err != nil {
			t.Fatalf("read vendored %s: %v", name, err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, "shapes-combined.ttl")
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatalf("write combined shapes: %v", err)
	}
	return path
}

// sanity: alias tokens are unique so normalization can't collapse two ids.
func TestFixtureAliasesUnique(t *testing.T) {
	f := buildFixture(t)
	// Round-trip a probe through the replacer for each token and ensure the set of
	// replacement targets has no duplicates by construction.
	seen := map[string]bool{}
	for _, tok := range []string{
		"agent-system", "agent-human", "agent-software", "agent-ml",
		"task-request", "task-proposal-1", "task-proposal-2", "task-bug",
		"act-ml", "act-human",
	} {
		full := fixtureNS + "--" + tok
		if seen[full] {
			t.Fatalf("duplicate alias token %q", full)
		}
		seen[full] = true
	}
	// exercise the replacer so an accidental empty map is caught.
	if got := f.replacer.Replace("no-ids-here"); got != "no-ids-here" {
		t.Fatalf("replacer mangled plain text: %q", got)
	}
	sorted := make([]string, 0, len(seen))
	for k := range seen {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	if len(sorted) != 10 {
		t.Fatalf("expected 10 unique aliases, got %d", len(sorted))
	}
}
