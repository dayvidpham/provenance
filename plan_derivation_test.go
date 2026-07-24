package provenance_test

import (
	"errors"
	"testing"

	"github.com/dayvidpham/provenance"
)

// plan_derivation_test.go covers the additive model extensions: the built-in
// plan seed + StartActivity plan association, and the derivation
// qualifier verb/reads (§3.3).

// ---------------------------------------------------------------------------
// Plan layer (§3.1)
// ---------------------------------------------------------------------------

func TestBuiltinPlanSeeded(t *testing.T) {
	tt := openMemorySession(t)
	defer tt.Close()

	plans, err := tt.Plans()
	if err != nil {
		t.Fatalf("Plans: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected exactly the built-in plan, got %d plans", len(plans))
	}
	pl := plans[0]
	if pl.ID != provenance.BuiltinPlanID() {
		t.Errorf("built-in plan ID = %v, want %v", pl.ID, provenance.BuiltinPlanID())
	}
	if pl.Title != provenance.BuiltinPlanTitle {
		t.Errorf("built-in plan title = %q, want %q", pl.Title, provenance.BuiltinPlanTitle)
	}
	if pl.Version != provenance.BuiltinPlanVersion {
		t.Errorf("built-in plan version = %q, want %q", pl.Version, provenance.BuiltinPlanVersion)
	}

	steps, err := tt.PlanSteps(pl.ID)
	if err != nil {
		t.Fatalf("PlanSteps: %v", err)
	}
	// 12 protocol phases + unscoped catch-all = 13 steps, in Phase enum order.
	wantPhases := []provenance.Phase{
		provenance.PhaseRequest, provenance.PhaseElicit, provenance.PhasePropose, provenance.PhaseReview,
		provenance.PhasePlanUAT, provenance.PhaseRatify, provenance.PhaseHandoff, provenance.PhaseImplPlan,
		provenance.PhaseWorkerSlices, provenance.PhaseCodeReview, provenance.PhaseImplUAT, provenance.PhaseLanding,
		provenance.PhaseUnscoped,
	}
	if len(steps) != len(wantPhases) {
		t.Fatalf("built-in plan has %d steps, want %d", len(steps), len(wantPhases))
	}
	for i, st := range steps {
		if st.Ordinal != i {
			t.Errorf("step %d ordinal = %d, want %d", i, st.Ordinal, i)
		}
		if st.Phase != wantPhases[i] {
			t.Errorf("step %d phase = %v, want %v", i, st.Phase, wantPhases[i])
		}
		if st.PlanID != pl.ID {
			t.Errorf("step %d planID = %v, want %v", i, st.PlanID, pl.ID)
		}
	}
}

func TestBuiltinPlanSeedIdempotentAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/plan.db"
	tt := openSQLiteSession(t, path)
	plans1, err := tt.Plans()
	if err != nil {
		t.Fatalf("Plans #1: %v", err)
	}
	tt.Close()

	// Re-open the same database: the seed must not duplicate the plan or its steps.
	tr2, err := provenance.OpenSQLite(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer tr2.Close()
	plans2, err := tr2.Plans()
	if err != nil {
		t.Fatalf("Plans #2: %v", err)
	}
	if len(plans1) != 1 || len(plans2) != 1 {
		t.Fatalf("plan count drifted across reopen: %d then %d", len(plans1), len(plans2))
	}
	steps, err := tr2.PlanSteps(provenance.BuiltinPlanID())
	if err != nil {
		t.Fatalf("PlanSteps after reopen: %v", err)
	}
	if len(steps) != 13 {
		t.Fatalf("built-in plan steps drifted across reopen: %d, want 13", len(steps))
	}
}

func TestStartActivityPlanAssociation(t *testing.T) {
	tt := openMemorySession(t)
	defer tt.Close()
	agent, err := tt.RegisterSoftwareAgent("provenance-test", "tool", "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}

	// Default: recorded under the built-in plan.
	def, err := tt.StartActivity(agent.ID, provenance.PhasePropose, provenance.StageInProgress, "default")
	if err != nil {
		t.Fatalf("StartActivity(default): %v", err)
	}
	if def.PlanID == nil || *def.PlanID != provenance.BuiltinPlanID() {
		t.Errorf("default activity PlanID = %v, want built-in plan", def.PlanID)
	}

	// InPlan: pinned explicitly (to the built-in plan, the only seeded one).
	pinned, err := tt.StartActivity(agent.ID, provenance.PhaseReview, provenance.StageComplete, "pinned",
		provenance.InPlan(provenance.BuiltinPlanID()))
	if err != nil {
		t.Fatalf("StartActivity(InPlan): %v", err)
	}
	if pinned.PlanID == nil || *pinned.PlanID != provenance.BuiltinPlanID() {
		t.Errorf("pinned activity PlanID = %v, want built-in plan", pinned.PlanID)
	}

	// Unplanned: nil plan (legacy/unplanned).
	unplanned, err := tt.StartActivity(agent.ID, provenance.PhaseLanding, provenance.StageComplete, "unplanned",
		provenance.Unplanned())
	if err != nil {
		t.Fatalf("StartActivity(Unplanned): %v", err)
	}
	if unplanned.PlanID != nil {
		t.Errorf("unplanned activity PlanID = %v, want nil", unplanned.PlanID)
	}

	// The association round-trips through the Activities read.
	acts, err := tt.Activities(&agent.ID)
	if err != nil {
		t.Fatalf("Activities: %v", err)
	}
	planned := 0
	for _, a := range acts {
		if a.PlanID != nil {
			planned++
		}
	}
	if planned != 2 {
		t.Errorf("expected 2 planned activities, got %d", planned)
	}
}

func TestStartActivityWithIDPlanReplaySafe(t *testing.T) {
	tt := openMemorySession(t)
	defer tt.Close()
	agent, err := tt.RegisterSoftwareAgent("provenance-test", "tool", "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	id := provenance.ActivityID{Namespace: "provenance-test", UUID: agent.ID.UUID}

	first, err := tt.StartActivityWithID(id, agent.ID, provenance.PhasePropose, provenance.StageInProgress, "v1")
	if err != nil {
		t.Fatalf("StartActivityWithID #1: %v", err)
	}
	if first.PlanID == nil || *first.PlanID != provenance.BuiltinPlanID() {
		t.Fatalf("first activity PlanID = %v, want built-in plan", first.PlanID)
	}
	// Idempotent replay returns the original row with its plan preserved.
	replay, err := tt.StartActivityWithID(id, agent.ID, provenance.PhasePropose, provenance.StageInProgress, "v2")
	if err != nil {
		t.Fatalf("StartActivityWithID #2: %v", err)
	}
	if replay.PlanID == nil || *replay.PlanID != provenance.BuiltinPlanID() {
		t.Errorf("replay activity PlanID = %v, want built-in plan", replay.PlanID)
	}
}

// ---------------------------------------------------------------------------
// Derivation qualifier (§3.3)
// ---------------------------------------------------------------------------

// newDerivationFixture creates source/target tasks with a derived_from edge and
// returns the session tracker plus the two task IDs.
func newDerivationFixture(t *testing.T) (*testTracker, provenance.Task, provenance.Task) {
	t.Helper()
	tt := openMemorySession(t)
	src, err := tt.Create("provenance-test", "PROPOSAL-2", "derived", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhasePropose)
	if err != nil {
		t.Fatalf("Create(src): %v", err)
	}
	tgt, err := tt.Create("provenance-test", "PROPOSAL-1", "origin", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhasePropose)
	if err != nil {
		t.Fatalf("Create(tgt): %v", err)
	}
	if err := tt.s.AddEdge(src.ID, tgt.ID.String(), provenance.EdgeDerivedFrom); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	return tt, src, tgt
}

func TestQualifyDerivationHappyPath(t *testing.T) {
	tt, src, tgt := newDerivationFixture(t)
	defer tt.Close()
	agent, err := tt.RegisterSoftwareAgent("provenance-test", "dedup-tool", "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	act, err := tt.StartActivity(agent.ID, provenance.PhasePropose, provenance.StageComplete, "dedup pass")
	if err != nil {
		t.Fatalf("StartActivity: %v", err)
	}

	if err := tt.s.QualifyDerivation(src.ID, tgt.ID, provenance.DerivationDeduplication, &act.ID); err != nil {
		t.Fatalf("QualifyDerivation: %v", err)
	}

	quals, err := tt.DerivationQualifiers(src.ID)
	if err != nil {
		t.Fatalf("DerivationQualifiers: %v", err)
	}
	if len(quals) != 1 {
		t.Fatalf("expected 1 qualifier, got %d", len(quals))
	}
	q := quals[0]
	if q.SourceID != src.ID || q.TargetID != tgt.ID {
		t.Errorf("qualifier endpoints = %v->%v, want %v->%v", q.SourceID, q.TargetID, src.ID, tgt.ID)
	}
	if q.Kind != provenance.DerivationDeduplication {
		t.Errorf("qualifier kind = %v, want %v", q.Kind, provenance.DerivationDeduplication)
	}
	if q.ActivityID == nil || *q.ActivityID != act.ID {
		t.Errorf("qualifier activity = %v, want %v", q.ActivityID, act.ID)
	}
}

func TestQualifyDerivationWithoutActivity(t *testing.T) {
	tt, src, tgt := newDerivationFixture(t)
	defer tt.Close()
	if err := tt.s.QualifyDerivation(src.ID, tgt.ID, provenance.DerivationTranslation, nil); err != nil {
		t.Fatalf("QualifyDerivation: %v", err)
	}
	quals, err := tt.DerivationQualifiers(src.ID)
	if err != nil {
		t.Fatalf("DerivationQualifiers: %v", err)
	}
	if len(quals) != 1 || quals[0].Kind != provenance.DerivationTranslation || quals[0].ActivityID != nil {
		t.Fatalf("unexpected qualifier: %+v", quals)
	}
}

func TestQualifyDerivationRejectsNonDerivationEdge(t *testing.T) {
	tt := openMemorySession(t)
	defer tt.Close()
	src, err := tt.Create("provenance-test", "A", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhasePropose)
	if err != nil {
		t.Fatalf("Create(src): %v", err)
	}
	tgt, err := tt.Create("provenance-test", "B", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhasePropose)
	if err != nil {
		t.Fatalf("Create(tgt): %v", err)
	}
	// A discovered_from edge is NOT a derivation edge: qualifying must be rejected,
	// and nothing may be written.
	if err := tt.s.AddEdge(src.ID, tgt.ID.String(), provenance.EdgeDiscoveredFrom); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	err = tt.s.QualifyDerivation(src.ID, tgt.ID, provenance.DerivationDeduplication, nil)
	if !errors.Is(err, provenance.ErrNoDerivationEdge) {
		t.Fatalf("QualifyDerivation on non-derivation edge = %v, want ErrNoDerivationEdge", err)
	}
	quals, err := tt.DerivationQualifiers(src.ID)
	if err != nil {
		t.Fatalf("DerivationQualifiers: %v", err)
	}
	if len(quals) != 0 {
		t.Fatalf("expected no qualifier written, got %d", len(quals))
	}
}

func TestQualifyDerivationRejectsMissingEdge(t *testing.T) {
	tt := openMemorySession(t)
	defer tt.Close()
	src, err := tt.Create("provenance-test", "A", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhasePropose)
	if err != nil {
		t.Fatalf("Create(src): %v", err)
	}
	tgt, err := tt.Create("provenance-test", "B", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhasePropose)
	if err != nil {
		t.Fatalf("Create(tgt): %v", err)
	}
	err = tt.s.QualifyDerivation(src.ID, tgt.ID, provenance.DerivationDeduplication, nil)
	if !errors.Is(err, provenance.ErrNoDerivationEdge) {
		t.Fatalf("QualifyDerivation with no edge = %v, want ErrNoDerivationEdge", err)
	}
}

func TestQualifyDerivationSupersedes(t *testing.T) {
	tt := openMemorySession(t)
	defer tt.Close()
	src, err := tt.Create("provenance-test", "P3", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhasePropose)
	if err != nil {
		t.Fatalf("Create(src): %v", err)
	}
	tgt, err := tt.Create("provenance-test", "P2", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhasePropose)
	if err != nil {
		t.Fatalf("Create(tgt): %v", err)
	}
	if err := tt.s.AddEdge(src.ID, tgt.ID.String(), provenance.EdgeSupersedes); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tt.s.QualifyDerivation(src.ID, tgt.ID, provenance.DerivationVerificationSubset, nil); err != nil {
		t.Fatalf("QualifyDerivation(supersedes): %v", err)
	}
	quals, err := tt.DerivationQualifiers(src.ID)
	if err != nil {
		t.Fatalf("DerivationQualifiers: %v", err)
	}
	if len(quals) != 1 || quals[0].Kind != provenance.DerivationVerificationSubset {
		t.Fatalf("unexpected qualifier on supersedes edge: %+v", quals)
	}
}

func TestQualifyDerivationReQualifyReplaces(t *testing.T) {
	tt, src, tgt := newDerivationFixture(t)
	defer tt.Close()
	if err := tt.s.QualifyDerivation(src.ID, tgt.ID, provenance.DerivationDeduplication, nil); err != nil {
		t.Fatalf("QualifyDerivation #1: %v", err)
	}
	// Re-qualifying the same relationship replaces the (single-valued) kind.
	if err := tt.s.QualifyDerivation(src.ID, tgt.ID, provenance.DerivationLabelCorrection, nil); err != nil {
		t.Fatalf("QualifyDerivation #2: %v", err)
	}
	quals, err := tt.DerivationQualifiers(src.ID)
	if err != nil {
		t.Fatalf("DerivationQualifiers: %v", err)
	}
	if len(quals) != 1 {
		t.Fatalf("re-qualify should not add a second qualifier, got %d", len(quals))
	}
	if quals[0].Kind != provenance.DerivationLabelCorrection {
		t.Errorf("re-qualify kind = %v, want %v", quals[0].Kind, provenance.DerivationLabelCorrection)
	}
}

func TestQualifyDerivationInvalidKind(t *testing.T) {
	tt, src, tgt := newDerivationFixture(t)
	defer tt.Close()
	if err := tt.s.QualifyDerivation(src.ID, tgt.ID, provenance.DerivationKind(99), nil); err == nil {
		t.Fatal("QualifyDerivation with invalid kind expected error, got nil")
	}
}

func TestQualifyDerivationCascadesOnEdgeRemoval(t *testing.T) {
	tt, src, tgt := newDerivationFixture(t)
	defer tt.Close()
	if err := tt.s.QualifyDerivation(src.ID, tgt.ID, provenance.DerivationDeduplication, nil); err != nil {
		t.Fatalf("QualifyDerivation: %v", err)
	}
	// Removing the derivation edge must cascade-delete its qualifier (no orphans).
	if err := tt.s.RemoveEdge(src.ID, tgt.ID.String(), provenance.EdgeDerivedFrom); err != nil {
		t.Fatalf("RemoveEdge: %v", err)
	}
	quals, err := tt.DerivationQualifiers(src.ID)
	if err != nil {
		t.Fatalf("DerivationQualifiers: %v", err)
	}
	if len(quals) != 0 {
		t.Fatalf("qualifier orphaned after edge removal: %d remain", len(quals))
	}
}
