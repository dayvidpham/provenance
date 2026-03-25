package ptypes_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/dayvidpham/providence/pkg/ptypes"
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
		s    ptypes.Status
		want string
	}{
		{ptypes.StatusOpen, "open"},
		{ptypes.StatusInProgress, "in_progress"},
		{ptypes.StatusClosed, "closed"},
		{ptypes.Status(99), "Status(99)"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("Status(%d).String() = %q, want %q", int(c.s), got, c.want)
		}
	}
}

func TestStatusMarshalText(t *testing.T) {
	valid := []ptypes.Status{
		ptypes.StatusOpen,
		ptypes.StatusInProgress,
		ptypes.StatusClosed,
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
	_, err := ptypes.Status(99).MarshalText()
	if err == nil {
		t.Error("Status(99).MarshalText() expected error, got nil")
	}
}

func TestStatusUnmarshalText(t *testing.T) {
	cases := []struct {
		input string
		want  ptypes.Status
	}{
		{"open", ptypes.StatusOpen},
		{"in_progress", ptypes.StatusInProgress},
		{"closed", ptypes.StatusClosed},
	}
	for _, c := range cases {
		var s ptypes.Status
		if err := s.UnmarshalText([]byte(c.input)); err != nil {
			t.Errorf("UnmarshalText(%q) unexpected error: %v", c.input, err)
		}
		if s != c.want {
			t.Errorf("UnmarshalText(%q) = %v, want %v", c.input, s, c.want)
		}
	}

	// Unknown text should error.
	var s ptypes.Status
	if err := s.UnmarshalText([]byte("unknown")); err == nil {
		t.Error("UnmarshalText(\"unknown\") expected error, got nil")
	}
}

func TestStatusRoundTrip(t *testing.T) {
	for _, s := range []ptypes.Status{
		ptypes.StatusOpen, ptypes.StatusInProgress, ptypes.StatusClosed,
	} {
		b, err := s.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText: %v", err)
		}
		var got ptypes.Status
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("UnmarshalText: %v", err)
		}
		if got != s {
			t.Errorf("round-trip: got %v, want %v", got, s)
		}
	}
}

func TestStatusIsValid(t *testing.T) {
	for _, s := range []ptypes.Status{
		ptypes.StatusOpen, ptypes.StatusInProgress, ptypes.StatusClosed,
	} {
		if !s.IsValid() {
			t.Errorf("Status(%d).IsValid() = false, want true", int(s))
		}
	}
	if ptypes.Status(99).IsValid() {
		t.Error("Status(99).IsValid() = true, want false")
	}
}

func TestStatusJSONRoundTrip(t *testing.T) {
	original := ptypes.StatusInProgress
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	// json.Marshal on an int produces "1", not "in_progress".
	// Status uses MarshalText, so it encodes as a JSON string.
	var got ptypes.Status
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
	values := []ptypes.Priority{
		ptypes.PriorityCritical,
		ptypes.PriorityHigh,
		ptypes.PriorityMedium,
		ptypes.PriorityLow,
		ptypes.PriorityBacklog,
	}
	for _, p := range values {
		b, err := p.MarshalText()
		if err != nil {
			t.Fatalf("Priority(%d).MarshalText(): %v", int(p), err)
		}
		var got ptypes.Priority
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("Priority.UnmarshalText(%q): %v", string(b), err)
		}
		if got != p {
			t.Errorf("Priority round-trip: got %v, want %v", got, p)
		}
	}
}

func TestPriorityIsValid(t *testing.T) {
	valid := []ptypes.Priority{
		ptypes.PriorityCritical,
		ptypes.PriorityHigh,
		ptypes.PriorityMedium,
		ptypes.PriorityLow,
		ptypes.PriorityBacklog,
	}
	for _, p := range valid {
		if !p.IsValid() {
			t.Errorf("Priority(%d).IsValid() = false", int(p))
		}
	}
	if ptypes.Priority(99).IsValid() {
		t.Error("Priority(99).IsValid() = true, want false")
	}
}

func TestPriorityStringValues(t *testing.T) {
	cases := []struct {
		p    ptypes.Priority
		want string
	}{
		{ptypes.PriorityCritical, "critical"},
		{ptypes.PriorityHigh, "high"},
		{ptypes.PriorityMedium, "medium"},
		{ptypes.PriorityLow, "low"},
		{ptypes.PriorityBacklog, "backlog"},
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
	values := []ptypes.TaskType{
		ptypes.TaskTypeBug,
		ptypes.TaskTypeFeature,
		ptypes.TaskTypeTask,
		ptypes.TaskTypeEpic,
		ptypes.TaskTypeChore,
	}
	for _, tt := range values {
		b, err := tt.MarshalText()
		if err != nil {
			t.Fatalf("TaskType(%d).MarshalText(): %v", int(tt), err)
		}
		var got ptypes.TaskType
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
		tt   ptypes.TaskType
		want string
	}{
		{ptypes.TaskTypeBug, "bug"},
		{ptypes.TaskTypeFeature, "feature"},
		{ptypes.TaskTypeTask, "task"},
		{ptypes.TaskTypeEpic, "epic"},
		{ptypes.TaskTypeChore, "chore"},
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
	values := []ptypes.EdgeKind{
		ptypes.EdgeBlockedBy,
		ptypes.EdgeDerivedFrom,
		ptypes.EdgeSupersedes,
		ptypes.EdgeDiscoveredFrom,
		ptypes.EdgeGeneratedBy,
		ptypes.EdgeAttributedTo,
	}
	for _, ek := range values {
		b, err := ek.MarshalText()
		if err != nil {
			t.Fatalf("EdgeKind(%d).MarshalText(): %v", int(ek), err)
		}
		var got ptypes.EdgeKind
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
		ek   ptypes.EdgeKind
		want string
	}{
		{ptypes.EdgeBlockedBy, "blocked_by"},
		{ptypes.EdgeDerivedFrom, "derived_from"},
		{ptypes.EdgeSupersedes, "supersedes"},
		{ptypes.EdgeDiscoveredFrom, "discovered_from"},
		{ptypes.EdgeGeneratedBy, "generated_by"},
		{ptypes.EdgeAttributedTo, "attributed_to"},
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
	values := []ptypes.AgentKind{
		ptypes.AgentKindHuman,
		ptypes.AgentKindMachineLearning,
		ptypes.AgentKindSoftware,
	}
	for _, ak := range values {
		b, err := ak.MarshalText()
		if err != nil {
			t.Fatalf("AgentKind(%d).MarshalText(): %v", int(ak), err)
		}
		var got ptypes.AgentKind
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
		ak   ptypes.AgentKind
		want string
	}{
		{ptypes.AgentKindHuman, "human"},
		{ptypes.AgentKindMachineLearning, "machine_learning"},
		{ptypes.AgentKindSoftware, "software"},
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
	values := []ptypes.Provider{
		ptypes.ProviderAnthropic,
		ptypes.ProviderGoogle,
		ptypes.ProviderOpenAI,
		ptypes.ProviderLocal,
	}
	for _, p := range values {
		b, err := p.MarshalText()
		if err != nil {
			t.Fatalf("Provider(%d).MarshalText(): %v", int(p), err)
		}
		var got ptypes.Provider
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
		p    ptypes.Provider
		want string
	}{
		{ptypes.ProviderAnthropic, "anthropic"},
		{ptypes.ProviderGoogle, "google"},
		{ptypes.ProviderOpenAI, "openai"},
		{ptypes.ProviderLocal, "local"},
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
	values := []ptypes.Role{
		ptypes.RoleHuman,
		ptypes.RoleArchitect,
		ptypes.RoleSupervisor,
		ptypes.RoleWorker,
		ptypes.RoleReviewer,
	}
	for _, r := range values {
		b, err := r.MarshalText()
		if err != nil {
			t.Fatalf("Role(%d).MarshalText(): %v", int(r), err)
		}
		var got ptypes.Role
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
		r    ptypes.Role
		want string
	}{
		{ptypes.RoleHuman, "human"},
		{ptypes.RoleArchitect, "architect"},
		{ptypes.RoleSupervisor, "supervisor"},
		{ptypes.RoleWorker, "worker"},
		{ptypes.RoleReviewer, "reviewer"},
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
	values := []ptypes.Phase{
		ptypes.PhaseRequest,
		ptypes.PhaseElicit,
		ptypes.PhasePropose,
		ptypes.PhaseReview,
		ptypes.PhasePlanUAT,
		ptypes.PhaseRatify,
		ptypes.PhaseHandoff,
		ptypes.PhaseImplPlan,
		ptypes.PhaseWorkerSlices,
		ptypes.PhaseCodeReview,
		ptypes.PhaseImplUAT,
		ptypes.PhaseLanding,
		ptypes.PhaseUnscoped,
	}
	for _, p := range values {
		b, err := p.MarshalText()
		if err != nil {
			t.Fatalf("Phase(%d).MarshalText(): %v", int(p), err)
		}
		var got ptypes.Phase
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
		p    ptypes.Phase
		want string
	}{
		{ptypes.PhaseRequest, "request"},
		{ptypes.PhaseElicit, "elicit"},
		{ptypes.PhasePropose, "propose"},
		{ptypes.PhaseReview, "review"},
		{ptypes.PhasePlanUAT, "plan_uat"},
		{ptypes.PhaseRatify, "ratify"},
		{ptypes.PhaseHandoff, "handoff"},
		{ptypes.PhaseImplPlan, "impl_plan"},
		{ptypes.PhaseWorkerSlices, "worker_slices"},
		{ptypes.PhaseCodeReview, "code_review"},
		{ptypes.PhaseImplUAT, "impl_uat"},
		{ptypes.PhaseLanding, "landing"},
		{ptypes.PhaseUnscoped, "unscoped"},
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
	values := []ptypes.Stage{
		ptypes.StageNotStarted,
		ptypes.StageInProgress,
		ptypes.StageBlocked,
		ptypes.StageComplete,
	}
	for _, s := range values {
		b, err := s.MarshalText()
		if err != nil {
			t.Fatalf("Stage(%d).MarshalText(): %v", int(s), err)
		}
		var got ptypes.Stage
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
		s    ptypes.Stage
		want string
	}{
		{ptypes.StageNotStarted, "not_started"},
		{ptypes.StageInProgress, "in_progress"},
		{ptypes.StageBlocked, "blocked"},
		{ptypes.StageComplete, "complete"},
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
		{"Status(99)", ptypes.Status(99).String(), "Status(99)"},
		{"Priority(99)", ptypes.Priority(99).String(), "Priority(99)"},
		{"TaskType(99)", ptypes.TaskType(99).String(), "TaskType(99)"},
		{"EdgeKind(99)", ptypes.EdgeKind(99).String(), "EdgeKind(99)"},
		{"AgentKind(99)", ptypes.AgentKind(99).String(), "AgentKind(99)"},
		{"Provider(99)", ptypes.Provider(99).String(), "Provider(99)"},
		{"Role(99)", ptypes.Role(99).String(), "Role(99)"},
		{"Phase(99)", ptypes.Phase(99).String(), fmt.Sprintf("Phase(%d)", 99)},
		{"Stage(99)", ptypes.Stage(99).String(), "Stage(99)"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}
