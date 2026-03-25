package providence_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/dayvidpham/providence"
)

// ---------------------------------------------------------------------------
// Generic helpers
// ---------------------------------------------------------------------------

// marshalUnmarshalText verifies a round-trip through MarshalText/UnmarshalText.
// unmarshal must be a pointer to a zero-valued enum.
type textMarshaler interface {
	MarshalText() ([]byte, error)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

func TestStatusString(t *testing.T) {
	cases := []struct {
		s    providence.Status
		want string
	}{
		{providence.StatusOpen, "open"},
		{providence.StatusInProgress, "in_progress"},
		{providence.StatusClosed, "closed"},
		{providence.Status(99), "Status(99)"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("Status(%d).String() = %q, want %q", int(c.s), got, c.want)
		}
	}
}

func TestStatusMarshalText(t *testing.T) {
	valid := []providence.Status{
		providence.StatusOpen,
		providence.StatusInProgress,
		providence.StatusClosed,
	}
	for _, s := range valid {
		b, err := s.MarshalText()
		if err != nil {
			t.Errorf("Status(%d).MarshalText() unexpected error: %v", int(s), err)
		}
		if string(b) != s.String() {
			t.Errorf("Status(%d).MarshalText() = %q, want %q", int(s), string(b), s.String())
		}
	}

	// Invalid status should error.
	_, err := providence.Status(99).MarshalText()
	if err == nil {
		t.Error("Status(99).MarshalText() expected error, got nil")
	}
}

func TestStatusUnmarshalText(t *testing.T) {
	cases := []struct {
		input string
		want  providence.Status
	}{
		{"open", providence.StatusOpen},
		{"in_progress", providence.StatusInProgress},
		{"closed", providence.StatusClosed},
	}
	for _, c := range cases {
		var s providence.Status
		if err := s.UnmarshalText([]byte(c.input)); err != nil {
			t.Errorf("UnmarshalText(%q) unexpected error: %v", c.input, err)
		}
		if s != c.want {
			t.Errorf("UnmarshalText(%q) = %v, want %v", c.input, s, c.want)
		}
	}

	// Unknown text should error.
	var s providence.Status
	if err := s.UnmarshalText([]byte("unknown")); err == nil {
		t.Error("UnmarshalText(\"unknown\") expected error, got nil")
	}
}

func TestStatusRoundTrip(t *testing.T) {
	for _, s := range []providence.Status{
		providence.StatusOpen, providence.StatusInProgress, providence.StatusClosed,
	} {
		b, err := s.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText: %v", err)
		}
		var got providence.Status
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("UnmarshalText: %v", err)
		}
		if got != s {
			t.Errorf("round-trip: got %v, want %v", got, s)
		}
	}
}

func TestStatusIsValid(t *testing.T) {
	for _, s := range []providence.Status{
		providence.StatusOpen, providence.StatusInProgress, providence.StatusClosed,
	} {
		if !s.IsValid() {
			t.Errorf("Status(%d).IsValid() = false, want true", int(s))
		}
	}
	if providence.Status(99).IsValid() {
		t.Error("Status(99).IsValid() = true, want false")
	}
}

func TestStatusJSONRoundTrip(t *testing.T) {
	original := providence.StatusInProgress
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	// json.Marshal on an int produces "1", not "in_progress".
	// Status uses MarshalText, so it encodes as a JSON string.
	var got providence.Status
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got != original {
		t.Errorf("json round-trip: got %v, want %v", got, original)
	}
}

// ---------------------------------------------------------------------------
// Priority
// ---------------------------------------------------------------------------

func TestPriorityRoundTrip(t *testing.T) {
	values := []providence.Priority{
		providence.PriorityCritical,
		providence.PriorityHigh,
		providence.PriorityMedium,
		providence.PriorityLow,
		providence.PriorityBacklog,
	}
	for _, p := range values {
		b, err := p.MarshalText()
		if err != nil {
			t.Fatalf("Priority(%d).MarshalText(): %v", int(p), err)
		}
		var got providence.Priority
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("Priority.UnmarshalText(%q): %v", string(b), err)
		}
		if got != p {
			t.Errorf("Priority round-trip: got %v, want %v", got, p)
		}
	}
}

func TestPriorityIsValid(t *testing.T) {
	valid := []providence.Priority{
		providence.PriorityCritical,
		providence.PriorityHigh,
		providence.PriorityMedium,
		providence.PriorityLow,
		providence.PriorityBacklog,
	}
	for _, p := range valid {
		if !p.IsValid() {
			t.Errorf("Priority(%d).IsValid() = false", int(p))
		}
	}
	if providence.Priority(99).IsValid() {
		t.Error("Priority(99).IsValid() = true, want false")
	}
}

func TestPriorityStringValues(t *testing.T) {
	cases := []struct {
		p    providence.Priority
		want string
	}{
		{providence.PriorityCritical, "critical"},
		{providence.PriorityHigh, "high"},
		{providence.PriorityMedium, "medium"},
		{providence.PriorityLow, "low"},
		{providence.PriorityBacklog, "backlog"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Errorf("Priority(%d).String() = %q, want %q", int(c.p), got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// TaskType
// ---------------------------------------------------------------------------

func TestTaskTypeRoundTrip(t *testing.T) {
	values := []providence.TaskType{
		providence.TaskTypeBug,
		providence.TaskTypeFeature,
		providence.TaskTypeTask,
		providence.TaskTypeEpic,
		providence.TaskTypeChore,
	}
	for _, tt := range values {
		b, err := tt.MarshalText()
		if err != nil {
			t.Fatalf("TaskType(%d).MarshalText(): %v", int(tt), err)
		}
		var got providence.TaskType
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("TaskType.UnmarshalText(%q): %v", string(b), err)
		}
		if got != tt {
			t.Errorf("TaskType round-trip: got %v, want %v", got, tt)
		}
	}
}

func TestTaskTypeStringValues(t *testing.T) {
	cases := []struct {
		tt   providence.TaskType
		want string
	}{
		{providence.TaskTypeBug, "bug"},
		{providence.TaskTypeFeature, "feature"},
		{providence.TaskTypeTask, "task"},
		{providence.TaskTypeEpic, "epic"},
		{providence.TaskTypeChore, "chore"},
	}
	for _, c := range cases {
		if got := c.tt.String(); got != c.want {
			t.Errorf("TaskType(%d).String() = %q, want %q", int(c.tt), got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// EdgeKind
// ---------------------------------------------------------------------------

func TestEdgeKindRoundTrip(t *testing.T) {
	values := []providence.EdgeKind{
		providence.EdgeBlockedBy,
		providence.EdgeDerivedFrom,
		providence.EdgeSupersedes,
		providence.EdgeDiscoveredFrom,
		providence.EdgeGeneratedBy,
		providence.EdgeAttributedTo,
	}
	for _, ek := range values {
		b, err := ek.MarshalText()
		if err != nil {
			t.Fatalf("EdgeKind(%d).MarshalText(): %v", int(ek), err)
		}
		var got providence.EdgeKind
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("EdgeKind.UnmarshalText(%q): %v", string(b), err)
		}
		if got != ek {
			t.Errorf("EdgeKind round-trip: got %v, want %v", got, ek)
		}
	}
}

func TestEdgeKindStringValues(t *testing.T) {
	cases := []struct {
		ek   providence.EdgeKind
		want string
	}{
		{providence.EdgeBlockedBy, "blocked_by"},
		{providence.EdgeDerivedFrom, "derived_from"},
		{providence.EdgeSupersedes, "supersedes"},
		{providence.EdgeDiscoveredFrom, "discovered_from"},
		{providence.EdgeGeneratedBy, "generated_by"},
		{providence.EdgeAttributedTo, "attributed_to"},
	}
	for _, c := range cases {
		if got := c.ek.String(); got != c.want {
			t.Errorf("EdgeKind(%d).String() = %q, want %q", int(c.ek), got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// AgentKind
// ---------------------------------------------------------------------------

func TestAgentKindRoundTrip(t *testing.T) {
	values := []providence.AgentKind{
		providence.AgentKindHuman,
		providence.AgentKindMachineLearning,
		providence.AgentKindSoftware,
	}
	for _, ak := range values {
		b, err := ak.MarshalText()
		if err != nil {
			t.Fatalf("AgentKind(%d).MarshalText(): %v", int(ak), err)
		}
		var got providence.AgentKind
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("AgentKind.UnmarshalText(%q): %v", string(b), err)
		}
		if got != ak {
			t.Errorf("AgentKind round-trip: got %v, want %v", got, ak)
		}
	}
}

func TestAgentKindStringValues(t *testing.T) {
	cases := []struct {
		ak   providence.AgentKind
		want string
	}{
		{providence.AgentKindHuman, "human"},
		{providence.AgentKindMachineLearning, "machine_learning"},
		{providence.AgentKindSoftware, "software"},
	}
	for _, c := range cases {
		if got := c.ak.String(); got != c.want {
			t.Errorf("AgentKind(%d).String() = %q, want %q", int(c.ak), got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

func TestProviderRoundTrip(t *testing.T) {
	values := []providence.Provider{
		providence.ProviderAnthropic,
		providence.ProviderGoogle,
		providence.ProviderOpenAI,
		providence.ProviderLocal,
	}
	for _, p := range values {
		b, err := p.MarshalText()
		if err != nil {
			t.Fatalf("Provider(%d).MarshalText(): %v", int(p), err)
		}
		var got providence.Provider
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("Provider.UnmarshalText(%q): %v", string(b), err)
		}
		if got != p {
			t.Errorf("Provider round-trip: got %v, want %v", got, p)
		}
	}
}

func TestProviderStringValues(t *testing.T) {
	cases := []struct {
		p    providence.Provider
		want string
	}{
		{providence.ProviderAnthropic, "anthropic"},
		{providence.ProviderGoogle, "google"},
		{providence.ProviderOpenAI, "openai"},
		{providence.ProviderLocal, "local"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Errorf("Provider(%d).String() = %q, want %q", int(c.p), got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Role
// ---------------------------------------------------------------------------

func TestRoleRoundTrip(t *testing.T) {
	values := []providence.Role{
		providence.RoleHuman,
		providence.RoleArchitect,
		providence.RoleSupervisor,
		providence.RoleWorker,
		providence.RoleReviewer,
	}
	for _, r := range values {
		b, err := r.MarshalText()
		if err != nil {
			t.Fatalf("Role(%d).MarshalText(): %v", int(r), err)
		}
		var got providence.Role
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("Role.UnmarshalText(%q): %v", string(b), err)
		}
		if got != r {
			t.Errorf("Role round-trip: got %v, want %v", got, r)
		}
	}
}

func TestRoleStringValues(t *testing.T) {
	cases := []struct {
		r    providence.Role
		want string
	}{
		{providence.RoleHuman, "human"},
		{providence.RoleArchitect, "architect"},
		{providence.RoleSupervisor, "supervisor"},
		{providence.RoleWorker, "worker"},
		{providence.RoleReviewer, "reviewer"},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Errorf("Role(%d).String() = %q, want %q", int(c.r), got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Phase
// ---------------------------------------------------------------------------

func TestPhaseRoundTrip(t *testing.T) {
	values := []providence.Phase{
		providence.PhaseRequest,
		providence.PhaseElicit,
		providence.PhasePropose,
		providence.PhaseReview,
		providence.PhasePlanUAT,
		providence.PhaseRatify,
		providence.PhaseHandoff,
		providence.PhaseImplPlan,
		providence.PhaseWorkerSlices,
		providence.PhaseCodeReview,
		providence.PhaseImplUAT,
		providence.PhaseLanding,
		providence.PhaseUnscoped,
	}
	for _, p := range values {
		b, err := p.MarshalText()
		if err != nil {
			t.Fatalf("Phase(%d).MarshalText(): %v", int(p), err)
		}
		var got providence.Phase
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("Phase.UnmarshalText(%q): %v", string(b), err)
		}
		if got != p {
			t.Errorf("Phase round-trip: got %v, want %v", got, p)
		}
	}
}

func TestPhaseStringValues(t *testing.T) {
	cases := []struct {
		p    providence.Phase
		want string
	}{
		{providence.PhaseRequest, "request"},
		{providence.PhaseElicit, "elicit"},
		{providence.PhasePropose, "propose"},
		{providence.PhaseReview, "review"},
		{providence.PhasePlanUAT, "plan_uat"},
		{providence.PhaseRatify, "ratify"},
		{providence.PhaseHandoff, "handoff"},
		{providence.PhaseImplPlan, "impl_plan"},
		{providence.PhaseWorkerSlices, "worker_slices"},
		{providence.PhaseCodeReview, "code_review"},
		{providence.PhaseImplUAT, "impl_uat"},
		{providence.PhaseLanding, "landing"},
		{providence.PhaseUnscoped, "unscoped"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Errorf("Phase(%d).String() = %q, want %q", int(c.p), got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Stage
// ---------------------------------------------------------------------------

func TestStageRoundTrip(t *testing.T) {
	values := []providence.Stage{
		providence.StageNotStarted,
		providence.StageInProgress,
		providence.StageBlocked,
		providence.StageComplete,
	}
	for _, s := range values {
		b, err := s.MarshalText()
		if err != nil {
			t.Fatalf("Stage(%d).MarshalText(): %v", int(s), err)
		}
		var got providence.Stage
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("Stage.UnmarshalText(%q): %v", string(b), err)
		}
		if got != s {
			t.Errorf("Stage round-trip: got %v, want %v", got, s)
		}
	}
}

func TestStageStringValues(t *testing.T) {
	cases := []struct {
		s    providence.Stage
		want string
	}{
		{providence.StageNotStarted, "not_started"},
		{providence.StageInProgress, "in_progress"},
		{providence.StageBlocked, "blocked"},
		{providence.StageComplete, "complete"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("Stage(%d).String() = %q, want %q", int(c.s), got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Verify all enums have Sprintf fallback for out-of-range values
// ---------------------------------------------------------------------------

func TestEnumOutOfRangeFallback(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"Status(99)", providence.Status(99).String(), "Status(99)"},
		{"Priority(99)", providence.Priority(99).String(), "Priority(99)"},
		{"TaskType(99)", providence.TaskType(99).String(), "TaskType(99)"},
		{"EdgeKind(99)", providence.EdgeKind(99).String(), "EdgeKind(99)"},
		{"AgentKind(99)", providence.AgentKind(99).String(), "AgentKind(99)"},
		{"Provider(99)", providence.Provider(99).String(), "Provider(99)"},
		{"Role(99)", providence.Role(99).String(), "Role(99)"},
		{"Phase(99)", providence.Phase(99).String(), fmt.Sprintf("Phase(%d)", 99)},
		{"Stage(99)", providence.Stage(99).String(), "Stage(99)"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}
