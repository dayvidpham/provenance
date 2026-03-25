package providence_test

// reexport_test.go verifies that every exported symbol from pkg/ptypes is
// re-exported by the root providence package. If a new type, constant, or
// function is added to pkg/ptypes without a corresponding re-export, one or
// more of these compile-time assertions or runtime checks will fail.
//
// Strategy:
//   - Compile-time: type identity assertions (type alias = same type)
//   - Compile-time: constant identity assertions (same value, same type)
//   - Compile-time: sentinel error identity assertions
//   - Compile-time: parse function signature assertions
//   - Runtime: errors.Is checks to confirm sentinel errors are the same objects

import (
	"errors"
	"testing"

	"github.com/dayvidpham/providence"
	"github.com/dayvidpham/providence/pkg/ptypes"
)

// ---------------------------------------------------------------------------
// Compile-time type identity assertions
// ---------------------------------------------------------------------------
// These assignments verify that each providence.X is the same type as ptypes.X.
// Type aliases are transparent, so this compiles if and only if the alias exists
// and refers to the correct ptypes type.

var (
	// Enum types
	_ providence.Status    = ptypes.Status(0)
	_ providence.Priority  = ptypes.Priority(0)
	_ providence.TaskType  = ptypes.TaskType(0)
	_ providence.EdgeKind  = ptypes.EdgeKind(0)
	_ providence.AgentKind = ptypes.AgentKind(0)
	_ providence.Provider  = ptypes.Provider(0)
	_ providence.Role      = ptypes.Role(0)
	_ providence.Phase     = ptypes.Phase(0)
	_ providence.Stage     = ptypes.Stage(0)

	// ID types
	_ providence.TaskID     = ptypes.TaskID{}
	_ providence.AgentID    = ptypes.AgentID{}
	_ providence.ActivityID = ptypes.ActivityID{}
	_ providence.CommentID  = ptypes.CommentID{}

	// Entity types
	_ providence.Task          = ptypes.Task{}
	_ providence.Agent         = ptypes.Agent{}
	_ providence.HumanAgent    = ptypes.HumanAgent{}
	_ providence.MLAgent       = ptypes.MLAgent{}
	_ providence.SoftwareAgent = ptypes.SoftwareAgent{}
	_ providence.MLModel       = ptypes.MLModel{}
	_ providence.Activity      = ptypes.Activity{}
	_ providence.Edge          = ptypes.Edge{}
	_ providence.Label         = ptypes.Label{}
	_ providence.Comment       = ptypes.Comment{}

	// Supporting types
	_ providence.UpdateFields = ptypes.UpdateFields{}
	_ providence.ListFilter   = ptypes.ListFilter{}
)

// ---------------------------------------------------------------------------
// Compile-time constant identity assertions
// ---------------------------------------------------------------------------
// Assigning ptypes constants to typed providence variables guarantees both
// type compatibility and value identity.

var (
	// Status constants
	_ providence.Status = providence.StatusOpen
	_ providence.Status = providence.StatusInProgress
	_ providence.Status = providence.StatusClosed

	// Priority constants
	_ providence.Priority = providence.PriorityCritical
	_ providence.Priority = providence.PriorityHigh
	_ providence.Priority = providence.PriorityMedium
	_ providence.Priority = providence.PriorityLow
	_ providence.Priority = providence.PriorityBacklog

	// TaskType constants
	_ providence.TaskType = providence.TaskTypeBug
	_ providence.TaskType = providence.TaskTypeFeature
	_ providence.TaskType = providence.TaskTypeTask
	_ providence.TaskType = providence.TaskTypeEpic
	_ providence.TaskType = providence.TaskTypeChore

	// EdgeKind constants
	_ providence.EdgeKind = providence.EdgeBlockedBy
	_ providence.EdgeKind = providence.EdgeDerivedFrom
	_ providence.EdgeKind = providence.EdgeSupersedes
	_ providence.EdgeKind = providence.EdgeDiscoveredFrom
	_ providence.EdgeKind = providence.EdgeGeneratedBy
	_ providence.EdgeKind = providence.EdgeAttributedTo

	// AgentKind constants
	_ providence.AgentKind = providence.AgentKindHuman
	_ providence.AgentKind = providence.AgentKindMachineLearning
	_ providence.AgentKind = providence.AgentKindSoftware

	// Provider constants
	_ providence.Provider = providence.ProviderAnthropic
	_ providence.Provider = providence.ProviderGoogle
	_ providence.Provider = providence.ProviderOpenAI
	_ providence.Provider = providence.ProviderLocal

	// Role constants
	_ providence.Role = providence.RoleHuman
	_ providence.Role = providence.RoleArchitect
	_ providence.Role = providence.RoleSupervisor
	_ providence.Role = providence.RoleWorker
	_ providence.Role = providence.RoleReviewer

	// Phase constants
	_ providence.Phase = providence.PhaseRequest
	_ providence.Phase = providence.PhaseElicit
	_ providence.Phase = providence.PhasePropose
	_ providence.Phase = providence.PhaseReview
	_ providence.Phase = providence.PhasePlanUAT
	_ providence.Phase = providence.PhaseRatify
	_ providence.Phase = providence.PhaseHandoff
	_ providence.Phase = providence.PhaseImplPlan
	_ providence.Phase = providence.PhaseWorkerSlices
	_ providence.Phase = providence.PhaseCodeReview
	_ providence.Phase = providence.PhaseImplUAT
	_ providence.Phase = providence.PhaseLanding
	_ providence.Phase = providence.PhaseUnscoped

	// Stage constants
	_ providence.Stage = providence.StageNotStarted
	_ providence.Stage = providence.StageInProgress
	_ providence.Stage = providence.StageBlocked
	_ providence.Stage = providence.StageComplete
)

// ---------------------------------------------------------------------------
// Compile-time parse function signature assertions
// ---------------------------------------------------------------------------

var (
	_ func(string) (providence.TaskID, error)     = providence.ParseTaskID
	_ func(string) (providence.AgentID, error)    = providence.ParseAgentID
	_ func(string) (providence.ActivityID, error) = providence.ParseActivityID
	_ func(string) (providence.CommentID, error)  = providence.ParseCommentID
)

// ---------------------------------------------------------------------------
// Runtime: sentinel error identity
// ---------------------------------------------------------------------------
// errors.Is confirms the re-exported vars point to the same error values.

func TestReexportSentinelErrorIdentity(t *testing.T) {
	cases := []struct {
		name     string
		rootErr  error
		ptypeErr error
	}{
		{"ErrNotFound", providence.ErrNotFound, ptypes.ErrNotFound},
		{"ErrCycleDetected", providence.ErrCycleDetected, ptypes.ErrCycleDetected},
		{"ErrAlreadyClosed", providence.ErrAlreadyClosed, ptypes.ErrAlreadyClosed},
		{"ErrInvalidID", providence.ErrInvalidID, ptypes.ErrInvalidID},
		{"ErrAgentKindMismatch", providence.ErrAgentKindMismatch, ptypes.ErrAgentKindMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !errors.Is(c.rootErr, c.ptypeErr) {
				t.Errorf("providence.%s is not errors.Is-identical to ptypes.%s", c.name, c.name)
			}
			if !errors.Is(c.ptypeErr, c.rootErr) {
				t.Errorf("ptypes.%s is not errors.Is-identical to providence.%s", c.name, c.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Runtime: constant value identity
// ---------------------------------------------------------------------------
// Verifies that re-exported constants have the exact same integer values.

func TestReexportConstantValues(t *testing.T) {
	// Status
	if providence.StatusOpen != ptypes.StatusOpen {
		t.Errorf("StatusOpen: providence=%d, ptypes=%d", providence.StatusOpen, ptypes.StatusOpen)
	}
	if providence.StatusInProgress != ptypes.StatusInProgress {
		t.Errorf("StatusInProgress: providence=%d, ptypes=%d", providence.StatusInProgress, ptypes.StatusInProgress)
	}
	if providence.StatusClosed != ptypes.StatusClosed {
		t.Errorf("StatusClosed: providence=%d, ptypes=%d", providence.StatusClosed, ptypes.StatusClosed)
	}

	// Priority
	if providence.PriorityCritical != ptypes.PriorityCritical {
		t.Errorf("PriorityCritical mismatch")
	}
	if providence.PriorityHigh != ptypes.PriorityHigh {
		t.Errorf("PriorityHigh mismatch")
	}
	if providence.PriorityMedium != ptypes.PriorityMedium {
		t.Errorf("PriorityMedium mismatch")
	}
	if providence.PriorityLow != ptypes.PriorityLow {
		t.Errorf("PriorityLow mismatch")
	}
	if providence.PriorityBacklog != ptypes.PriorityBacklog {
		t.Errorf("PriorityBacklog mismatch")
	}

	// TaskType
	if providence.TaskTypeBug != ptypes.TaskTypeBug {
		t.Errorf("TaskTypeBug mismatch")
	}
	if providence.TaskTypeFeature != ptypes.TaskTypeFeature {
		t.Errorf("TaskTypeFeature mismatch")
	}
	if providence.TaskTypeTask != ptypes.TaskTypeTask {
		t.Errorf("TaskTypeTask mismatch")
	}
	if providence.TaskTypeEpic != ptypes.TaskTypeEpic {
		t.Errorf("TaskTypeEpic mismatch")
	}
	if providence.TaskTypeChore != ptypes.TaskTypeChore {
		t.Errorf("TaskTypeChore mismatch")
	}

	// EdgeKind
	if providence.EdgeBlockedBy != ptypes.EdgeBlockedBy {
		t.Errorf("EdgeBlockedBy mismatch")
	}
	if providence.EdgeDerivedFrom != ptypes.EdgeDerivedFrom {
		t.Errorf("EdgeDerivedFrom mismatch")
	}
	if providence.EdgeSupersedes != ptypes.EdgeSupersedes {
		t.Errorf("EdgeSupersedes mismatch")
	}
	if providence.EdgeDiscoveredFrom != ptypes.EdgeDiscoveredFrom {
		t.Errorf("EdgeDiscoveredFrom mismatch")
	}
	if providence.EdgeGeneratedBy != ptypes.EdgeGeneratedBy {
		t.Errorf("EdgeGeneratedBy mismatch")
	}
	if providence.EdgeAttributedTo != ptypes.EdgeAttributedTo {
		t.Errorf("EdgeAttributedTo mismatch")
	}

	// AgentKind
	if providence.AgentKindHuman != ptypes.AgentKindHuman {
		t.Errorf("AgentKindHuman mismatch")
	}
	if providence.AgentKindMachineLearning != ptypes.AgentKindMachineLearning {
		t.Errorf("AgentKindMachineLearning mismatch")
	}
	if providence.AgentKindSoftware != ptypes.AgentKindSoftware {
		t.Errorf("AgentKindSoftware mismatch")
	}

	// Provider
	if providence.ProviderAnthropic != ptypes.ProviderAnthropic {
		t.Errorf("ProviderAnthropic mismatch")
	}
	if providence.ProviderGoogle != ptypes.ProviderGoogle {
		t.Errorf("ProviderGoogle mismatch")
	}
	if providence.ProviderOpenAI != ptypes.ProviderOpenAI {
		t.Errorf("ProviderOpenAI mismatch")
	}
	if providence.ProviderLocal != ptypes.ProviderLocal {
		t.Errorf("ProviderLocal mismatch")
	}

	// Role
	if providence.RoleHuman != ptypes.RoleHuman {
		t.Errorf("RoleHuman mismatch")
	}
	if providence.RoleArchitect != ptypes.RoleArchitect {
		t.Errorf("RoleArchitect mismatch")
	}
	if providence.RoleSupervisor != ptypes.RoleSupervisor {
		t.Errorf("RoleSupervisor mismatch")
	}
	if providence.RoleWorker != ptypes.RoleWorker {
		t.Errorf("RoleWorker mismatch")
	}
	if providence.RoleReviewer != ptypes.RoleReviewer {
		t.Errorf("RoleReviewer mismatch")
	}

	// Phase
	if providence.PhaseRequest != ptypes.PhaseRequest {
		t.Errorf("PhaseRequest mismatch")
	}
	if providence.PhaseElicit != ptypes.PhaseElicit {
		t.Errorf("PhaseElicit mismatch")
	}
	if providence.PhasePropose != ptypes.PhasePropose {
		t.Errorf("PhasePropose mismatch")
	}
	if providence.PhaseReview != ptypes.PhaseReview {
		t.Errorf("PhaseReview mismatch")
	}
	if providence.PhasePlanUAT != ptypes.PhasePlanUAT {
		t.Errorf("PhasePlanUAT mismatch")
	}
	if providence.PhaseRatify != ptypes.PhaseRatify {
		t.Errorf("PhaseRatify mismatch")
	}
	if providence.PhaseHandoff != ptypes.PhaseHandoff {
		t.Errorf("PhaseHandoff mismatch")
	}
	if providence.PhaseImplPlan != ptypes.PhaseImplPlan {
		t.Errorf("PhaseImplPlan mismatch")
	}
	if providence.PhaseWorkerSlices != ptypes.PhaseWorkerSlices {
		t.Errorf("PhaseWorkerSlices mismatch")
	}
	if providence.PhaseCodeReview != ptypes.PhaseCodeReview {
		t.Errorf("PhaseCodeReview mismatch")
	}
	if providence.PhaseImplUAT != ptypes.PhaseImplUAT {
		t.Errorf("PhaseImplUAT mismatch")
	}
	if providence.PhaseLanding != ptypes.PhaseLanding {
		t.Errorf("PhaseLanding mismatch")
	}
	if providence.PhaseUnscoped != ptypes.PhaseUnscoped {
		t.Errorf("PhaseUnscoped mismatch")
	}

	// Stage
	if providence.StageNotStarted != ptypes.StageNotStarted {
		t.Errorf("StageNotStarted mismatch")
	}
	if providence.StageInProgress != ptypes.StageInProgress {
		t.Errorf("StageInProgress mismatch")
	}
	if providence.StageBlocked != ptypes.StageBlocked {
		t.Errorf("StageBlocked mismatch")
	}
	if providence.StageComplete != ptypes.StageComplete {
		t.Errorf("StageComplete mismatch")
	}
}

// ---------------------------------------------------------------------------
// Runtime: parse function identity
// ---------------------------------------------------------------------------
// Verifies that the root parse functions produce the same results.

func TestReexportParseFunctionIdentity(t *testing.T) {
	validID := "ns--018f4b12-3456-7890-abcd-ef0123456789"

	// ParseTaskID
	rootTask, rootErr := providence.ParseTaskID(validID)
	ptypeTask, ptypeErr := ptypes.ParseTaskID(validID)
	if rootErr != nil || ptypeErr != nil {
		t.Fatalf("ParseTaskID errors: root=%v, ptypes=%v", rootErr, ptypeErr)
	}
	if rootTask != ptypeTask {
		t.Errorf("ParseTaskID result mismatch: root=%+v, ptypes=%+v", rootTask, ptypeTask)
	}

	// ParseAgentID
	rootAgent, rootErr := providence.ParseAgentID(validID)
	ptypeAgent, ptypeErr := ptypes.ParseAgentID(validID)
	if rootErr != nil || ptypeErr != nil {
		t.Fatalf("ParseAgentID errors: root=%v, ptypes=%v", rootErr, ptypeErr)
	}
	if rootAgent != ptypeAgent {
		t.Errorf("ParseAgentID result mismatch: root=%+v, ptypes=%+v", rootAgent, ptypeAgent)
	}

	// ParseActivityID
	rootAct, rootErr := providence.ParseActivityID(validID)
	ptypeAct, ptypeErr := ptypes.ParseActivityID(validID)
	if rootErr != nil || ptypeErr != nil {
		t.Fatalf("ParseActivityID errors: root=%v, ptypes=%v", rootErr, ptypeErr)
	}
	if rootAct != ptypeAct {
		t.Errorf("ParseActivityID result mismatch: root=%+v, ptypes=%+v", rootAct, ptypeAct)
	}

	// ParseCommentID
	rootComment, rootErr := providence.ParseCommentID(validID)
	ptypeComment, ptypeErr := ptypes.ParseCommentID(validID)
	if rootErr != nil || ptypeErr != nil {
		t.Fatalf("ParseCommentID errors: root=%v, ptypes=%v", rootErr, ptypeErr)
	}
	if rootComment != ptypeComment {
		t.Errorf("ParseCommentID result mismatch: root=%+v, ptypes=%+v", rootComment, ptypeComment)
	}
}
