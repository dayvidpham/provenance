// Package ptypes provides the public type definitions for the Providence task
// dependency tracker. It contains all enum types, ID types, entity structs,
// supporting types, and sentinel errors.
//
// This package has ZERO dependencies on the root providence package or any
// internal/ package. It is safe to import from anywhere within the module.
//
// Consumers of the library should continue to use the root
// "github.com/dayvidpham/providence" package, which re-exports everything
// from ptypes via transparent type aliases.
package ptypes

import "fmt"

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// Status represents the lifecycle state of a task.
type Status int

const (
	StatusOpen       Status = iota // 0: Task is created but not yet started
	StatusInProgress               // 1: Work is actively happening
	StatusClosed                   // 2: Work is complete
)

var statusStrings = [...]string{
	StatusOpen:       "open",
	StatusInProgress: "in_progress",
	StatusClosed:     "closed",
}

func (s Status) String() string {
	if int(s) >= 0 && int(s) < len(statusStrings) {
		return statusStrings[s]
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

func (s Status) MarshalText() ([]byte, error) {
	if !s.IsValid() {
		return nil, fmt.Errorf("providence: cannot marshal invalid Status(%d) — valid range is 0–%d", int(s), len(statusStrings)-1)
	}
	return []byte(s.String()), nil
}

func (s *Status) UnmarshalText(b []byte) error {
	text := string(b)
	for i, name := range statusStrings {
		if name == text {
			*s = Status(i)
			return nil
		}
	}
	return fmt.Errorf(
		"providence: unknown Status %q — valid values: %v — "+
			"fix by using one of the listed values",
		text, statusStrings[:],
	)
}

func (s Status) IsValid() bool {
	return s >= StatusOpen && s <= StatusClosed
}

// ---------------------------------------------------------------------------
// Priority
// ---------------------------------------------------------------------------

// Priority represents task urgency (0 = critical, 4 = backlog).
type Priority int

const (
	PriorityCritical Priority = iota // 0: security, data loss, broken builds
	PriorityHigh                     // 1: major features, important bugs
	PriorityMedium                   // 2: default
	PriorityLow                      // 3: polish, optimization
	PriorityBacklog                  // 4: future ideas
)

var priorityStrings = [...]string{
	PriorityCritical: "critical",
	PriorityHigh:     "high",
	PriorityMedium:   "medium",
	PriorityLow:      "low",
	PriorityBacklog:  "backlog",
}

func (p Priority) String() string {
	if int(p) >= 0 && int(p) < len(priorityStrings) {
		return priorityStrings[p]
	}
	return fmt.Sprintf("Priority(%d)", int(p))
}

func (p Priority) MarshalText() ([]byte, error) {
	if !p.IsValid() {
		return nil, fmt.Errorf("providence: cannot marshal invalid Priority(%d) — valid range is 0–%d", int(p), len(priorityStrings)-1)
	}
	return []byte(p.String()), nil
}

func (p *Priority) UnmarshalText(b []byte) error {
	text := string(b)
	for i, name := range priorityStrings {
		if name == text {
			*p = Priority(i)
			return nil
		}
	}
	return fmt.Errorf(
		"providence: unknown Priority %q — valid values: %v — "+
			"fix by using one of the listed values",
		text, priorityStrings[:],
	)
}

func (p Priority) IsValid() bool {
	return p >= PriorityCritical && p <= PriorityBacklog
}

// ---------------------------------------------------------------------------
// TaskType
// ---------------------------------------------------------------------------

// TaskType classifies the kind of work.
// Protocol artifacts are distinguished by Phase, not by TaskType.
type TaskType int

const (
	TaskTypeBug     TaskType = iota // 0: Something broken
	TaskTypeFeature                 // 1: New functionality
	TaskTypeTask                    // 2: Work item (tests, docs, refactoring)
	TaskTypeEpic                    // 3: Large feature with subtasks
	TaskTypeChore                   // 4: Maintenance (dependencies, tooling)
)

var taskTypeStrings = [...]string{
	TaskTypeBug:     "bug",
	TaskTypeFeature: "feature",
	TaskTypeTask:    "task",
	TaskTypeEpic:    "epic",
	TaskTypeChore:   "chore",
}

func (t TaskType) String() string {
	if int(t) >= 0 && int(t) < len(taskTypeStrings) {
		return taskTypeStrings[t]
	}
	return fmt.Sprintf("TaskType(%d)", int(t))
}

func (t TaskType) MarshalText() ([]byte, error) {
	if !t.IsValid() {
		return nil, fmt.Errorf("providence: cannot marshal invalid TaskType(%d) — valid range is 0–%d", int(t), len(taskTypeStrings)-1)
	}
	return []byte(t.String()), nil
}

func (t *TaskType) UnmarshalText(b []byte) error {
	text := string(b)
	for i, name := range taskTypeStrings {
		if name == text {
			*t = TaskType(i)
			return nil
		}
	}
	return fmt.Errorf(
		"providence: unknown TaskType %q — valid values: %v — "+
			"fix by using one of the listed values",
		text, taskTypeStrings[:],
	)
}

func (t TaskType) IsValid() bool {
	return t >= TaskTypeBug && t <= TaskTypeChore
}

// ---------------------------------------------------------------------------
// EdgeKind
// ---------------------------------------------------------------------------

// EdgeKind classifies the relationship between entities.
type EdgeKind int

const (
	EdgeBlockedBy      EdgeKind = iota // 0: Task → Task: affects task readiness
	EdgeDerivedFrom                    // 1: Task → Task: PROPOSAL-2 derived from PROPOSAL-1
	EdgeSupersedes                     // 2: Task → Task: PROPOSAL-3 supersedes PROPOSAL-2
	EdgeDiscoveredFrom                 // 3: Task → Task: found during work on parent
	EdgeGeneratedBy                    // 4: Task → Activity: which activity produced this
	EdgeAttributedTo                   // 5: Task → Agent: which agent owns this
)

var edgeKindStrings = [...]string{
	EdgeBlockedBy:      "blocked_by",
	EdgeDerivedFrom:    "derived_from",
	EdgeSupersedes:     "supersedes",
	EdgeDiscoveredFrom: "discovered_from",
	EdgeGeneratedBy:    "generated_by",
	EdgeAttributedTo:   "attributed_to",
}

func (e EdgeKind) String() string {
	if int(e) >= 0 && int(e) < len(edgeKindStrings) {
		return edgeKindStrings[e]
	}
	return fmt.Sprintf("EdgeKind(%d)", int(e))
}

func (e EdgeKind) MarshalText() ([]byte, error) {
	if !e.IsValid() {
		return nil, fmt.Errorf("providence: cannot marshal invalid EdgeKind(%d) — valid range is 0–%d", int(e), len(edgeKindStrings)-1)
	}
	return []byte(e.String()), nil
}

func (e *EdgeKind) UnmarshalText(b []byte) error {
	text := string(b)
	for i, name := range edgeKindStrings {
		if name == text {
			*e = EdgeKind(i)
			return nil
		}
	}
	return fmt.Errorf(
		"providence: unknown EdgeKind %q — valid values: %v — "+
			"fix by using one of the listed values",
		text, edgeKindStrings[:],
	)
}

func (e EdgeKind) IsValid() bool {
	return e >= EdgeBlockedBy && e <= EdgeAttributedTo
}

// ---------------------------------------------------------------------------
// AgentKind
// ---------------------------------------------------------------------------

// AgentKind discriminates the agent TPT hierarchy.
type AgentKind int

const (
	AgentKindHuman           AgentKind = iota // 0: Human user
	AgentKindMachineLearning                  // 1: AI/ML model agent
	AgentKindSoftware                         // 2: Software tool or script
)

var agentKindStrings = [...]string{
	AgentKindHuman:           "human",
	AgentKindMachineLearning: "machine_learning",
	AgentKindSoftware:        "software",
}

func (a AgentKind) String() string {
	if int(a) >= 0 && int(a) < len(agentKindStrings) {
		return agentKindStrings[a]
	}
	return fmt.Sprintf("AgentKind(%d)", int(a))
}

func (a AgentKind) MarshalText() ([]byte, error) {
	if !a.IsValid() {
		return nil, fmt.Errorf("providence: cannot marshal invalid AgentKind(%d) — valid range is 0–%d", int(a), len(agentKindStrings)-1)
	}
	return []byte(a.String()), nil
}

func (a *AgentKind) UnmarshalText(b []byte) error {
	text := string(b)
	for i, name := range agentKindStrings {
		if name == text {
			*a = AgentKind(i)
			return nil
		}
	}
	return fmt.Errorf(
		"providence: unknown AgentKind %q — valid values: %v — "+
			"fix by using one of the listed values",
		text, agentKindStrings[:],
	)
}

func (a AgentKind) IsValid() bool {
	return a >= AgentKindHuman && a <= AgentKindSoftware
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

// Provider identifies the organization behind an ML model.
type Provider int

const (
	ProviderAnthropic Provider = iota // 0
	ProviderGoogle                    // 1
	ProviderOpenAI                    // 2
	ProviderLocal                     // 3
)

var providerStrings = [...]string{
	ProviderAnthropic: "anthropic",
	ProviderGoogle:    "google",
	ProviderOpenAI:    "openai",
	ProviderLocal:     "local",
}

func (p Provider) String() string {
	if int(p) >= 0 && int(p) < len(providerStrings) {
		return providerStrings[p]
	}
	return fmt.Sprintf("Provider(%d)", int(p))
}

func (p Provider) MarshalText() ([]byte, error) {
	if !p.IsValid() {
		return nil, fmt.Errorf("providence: cannot marshal invalid Provider(%d) — valid range is 0–%d", int(p), len(providerStrings)-1)
	}
	return []byte(p.String()), nil
}

func (p *Provider) UnmarshalText(b []byte) error {
	text := string(b)
	for i, name := range providerStrings {
		if name == text {
			*p = Provider(i)
			return nil
		}
	}
	return fmt.Errorf(
		"providence: unknown Provider %q — valid values: %v — "+
			"fix by using one of the listed values",
		text, providerStrings[:],
	)
}

func (p Provider) IsValid() bool {
	return p >= ProviderAnthropic && p <= ProviderLocal
}

// ---------------------------------------------------------------------------
// Role
// ---------------------------------------------------------------------------

// Role identifies an agent's role in the protocol.
// Only used by ML agents (agents_ml.role_id).
type Role int

const (
	RoleHuman      Role = iota // 0: Human user (conceptual; not stored on agents_ml)
	RoleArchitect              // 1: Architect agent
	RoleSupervisor             // 2: Supervisor agent
	RoleWorker                 // 3: Worker agent
	RoleReviewer               // 4: Reviewer agent
)

var roleStrings = [...]string{
	RoleHuman:      "human",
	RoleArchitect:  "architect",
	RoleSupervisor: "supervisor",
	RoleWorker:     "worker",
	RoleReviewer:   "reviewer",
}

func (r Role) String() string {
	if int(r) >= 0 && int(r) < len(roleStrings) {
		return roleStrings[r]
	}
	return fmt.Sprintf("Role(%d)", int(r))
}

func (r Role) MarshalText() ([]byte, error) {
	if !r.IsValid() {
		return nil, fmt.Errorf("providence: cannot marshal invalid Role(%d) — valid range is 0–%d", int(r), len(roleStrings)-1)
	}
	return []byte(r.String()), nil
}

func (r *Role) UnmarshalText(b []byte) error {
	text := string(b)
	for i, name := range roleStrings {
		if name == text {
			*r = Role(i)
			return nil
		}
	}
	return fmt.Errorf(
		"providence: unknown Role %q — valid values: %v — "+
			"fix by using one of the listed values",
		text, roleStrings[:],
	)
}

func (r Role) IsValid() bool {
	return r >= RoleHuman && r <= RoleReviewer
}

// ---------------------------------------------------------------------------
// Phase
// ---------------------------------------------------------------------------

// Phase identifies a phase in the epoch lifecycle.
// Every task has a required phase. Use PhaseUnscoped for generic
// tasks that are not specific to a protocol phase.
type Phase int

const (
	PhaseRequest      Phase = iota // p1
	PhaseElicit                    // p2
	PhasePropose                   // p3
	PhaseReview                    // p4
	PhasePlanUAT                   // p5
	PhaseRatify                    // p6
	PhaseHandoff                   // p7
	PhaseImplPlan                  // p8
	PhaseWorkerSlices              // p9
	PhaseCodeReview                // p10
	PhaseImplUAT                   // p11
	PhaseLanding                   // p12
	PhaseUnscoped                  // Generic tasks not tied to a specific protocol phase
)

var phaseStrings = [...]string{
	PhaseRequest:      "request",
	PhaseElicit:       "elicit",
	PhasePropose:      "propose",
	PhaseReview:       "review",
	PhasePlanUAT:      "plan_uat",
	PhaseRatify:       "ratify",
	PhaseHandoff:      "handoff",
	PhaseImplPlan:     "impl_plan",
	PhaseWorkerSlices: "worker_slices",
	PhaseCodeReview:   "code_review",
	PhaseImplUAT:      "impl_uat",
	PhaseLanding:      "landing",
	PhaseUnscoped:     "unscoped",
}

func (p Phase) String() string {
	if int(p) >= 0 && int(p) < len(phaseStrings) {
		return phaseStrings[p]
	}
	return fmt.Sprintf("Phase(%d)", int(p))
}

func (p Phase) MarshalText() ([]byte, error) {
	if !p.IsValid() {
		return nil, fmt.Errorf("providence: cannot marshal invalid Phase(%d) — valid range is 0–%d", int(p), len(phaseStrings)-1)
	}
	return []byte(p.String()), nil
}

func (p *Phase) UnmarshalText(b []byte) error {
	text := string(b)
	for i, name := range phaseStrings {
		if name == text {
			*p = Phase(i)
			return nil
		}
	}
	return fmt.Errorf(
		"providence: unknown Phase %q — valid values: %v — "+
			"fix by using one of the listed values",
		text, phaseStrings[:],
	)
}

func (p Phase) IsValid() bool {
	return p >= PhaseRequest && p <= PhaseUnscoped
}

// ---------------------------------------------------------------------------
// Stage
// ---------------------------------------------------------------------------

// Stage captures fine-grained progress within a phase.
type Stage int

const (
	StageNotStarted Stage = iota // 0
	StageInProgress              // 1
	StageBlocked                 // 2
	StageComplete                // 3
)

var stageStrings = [...]string{
	StageNotStarted: "not_started",
	StageInProgress: "in_progress",
	StageBlocked:    "blocked",
	StageComplete:   "complete",
}

func (s Stage) String() string {
	if int(s) >= 0 && int(s) < len(stageStrings) {
		return stageStrings[s]
	}
	return fmt.Sprintf("Stage(%d)", int(s))
}

func (s Stage) MarshalText() ([]byte, error) {
	if !s.IsValid() {
		return nil, fmt.Errorf("providence: cannot marshal invalid Stage(%d) — valid range is 0–%d", int(s), len(stageStrings)-1)
	}
	return []byte(s.String()), nil
}

func (s *Stage) UnmarshalText(b []byte) error {
	text := string(b)
	for i, name := range stageStrings {
		if name == text {
			*s = Stage(i)
			return nil
		}
	}
	return fmt.Errorf(
		"providence: unknown Stage %q — valid values: %v — "+
			"fix by using one of the listed values",
		text, stageStrings[:],
	)
}

func (s Stage) IsValid() bool {
	return s >= StageNotStarted && s <= StageComplete
}
