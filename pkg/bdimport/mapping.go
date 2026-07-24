package bdimport

// mapping.go is the pure, side-effect-free contract between the bd value space and the
// provenance value space: deterministic ID derivation and strongly-typed enum mapping.
// Everything here is statically defined (per the project's preference for static over
// runtime definition) and returns typed enums, never stringly-typed values. Unmappable
// inputs are reported (ok=false) rather than coerced or fabricated.

import (
	"github.com/dayvidpham/provenance"
	"github.com/google/uuid"
)

// bdimportNamespaceUUID is the fixed UUIDv5 namespace root under which every minted
// provenance ID is derived. It is a stable name-based UUID so re-import reproduces the
// same TaskIDs and ActivityIDs. It is deliberately independent of the provenance
// namespace argument, which is folded into each derivation string below.
var bdimportNamespaceUUID = uuid.NewSHA1(uuid.NameSpaceURL,
	[]byte("github.com/dayvidpham/provenance/pkg/bdimport"))

// TaskID returns the deterministic provenance TaskID for a bd issue id under the given
// provenance namespace. Stable across runs and stores (UUIDv5), which is what makes
// re-import idempotent.
func TaskID(namespace, bdID string) provenance.TaskID {
	return provenance.TaskID{
		Namespace: namespace,
		UUID:      uuid.NewSHA1(bdimportNamespaceUUID, []byte(namespace+"|task|"+bdID)),
	}
}

// ActivityID returns the deterministic provenance ActivityID for a bd issue's lifecycle
// activity. Deterministic so StartActivityWithID's ON CONFLICT DO NOTHING makes activity
// emission a no-op on re-import.
func ActivityID(namespace, bdID string) provenance.ActivityID {
	return provenance.ActivityID{
		Namespace: namespace,
		UUID:      uuid.NewSHA1(bdimportNamespaceUUID, []byte(namespace+"|activity|"+bdID)),
	}
}

// ---------------------------------------------------------------------------
// Enum mapping — all total, all typed, unmappable inputs reported not coerced.
// ---------------------------------------------------------------------------

// statusForBD maps a bd status token to the provenance Status. ok=false for an
// unrecognized token (caller reports it; the task defaults to Open).
func statusForBD(s string) (provenance.Status, bool) {
	switch s {
	case "open":
		return provenance.StatusOpen, true
	case "in_progress":
		return provenance.StatusInProgress, true
	case "closed":
		return provenance.StatusClosed, true
	default:
		return provenance.StatusOpen, false
	}
}

// taskTypeForBD maps a bd issue_type token to the provenance TaskType. ok=false for an
// unrecognized token (caller reports it; the task defaults to TaskTypeTask).
func taskTypeForBD(t string) (provenance.TaskType, bool) {
	switch t {
	case "bug":
		return provenance.TaskTypeBug, true
	case "feature":
		return provenance.TaskTypeFeature, true
	case "task":
		return provenance.TaskTypeTask, true
	case "epic":
		return provenance.TaskTypeEpic, true
	case "chore":
		return provenance.TaskTypeChore, true
	default:
		return provenance.TaskTypeTask, false
	}
}

// priorityForBD maps a bd integer priority (0=critical … 4=backlog) to the provenance
// Priority. ok=false when the value is out of the valid range (caller reports it; the
// value is clamped into range).
func priorityForBD(p int) (provenance.Priority, bool) {
	if p < int(provenance.PriorityCritical) {
		return provenance.PriorityCritical, false
	}
	if p > int(provenance.PriorityBacklog) {
		return provenance.PriorityBacklog, false
	}
	return provenance.Priority(p), true
}

// edgeKindForDep maps a bd dependency type to the provenance EdgeKind. ok=false for a
// kind this importer does not map (caller skips + reports; never coerced). blocks and
// discovered-from are the kinds present in the surveyed corpus; parent-child and related
// are intentionally left unmapped (documented in the package doc).
func edgeKindForDep(depType string) (provenance.EdgeKind, bool) {
	switch depType {
	case "blocks":
		return provenance.EdgeBlockedBy, true
	case "discovered-from":
		return provenance.EdgeDiscoveredFrom, true
	default:
		return provenance.EdgeBlockedBy, false
	}
}

// stageForStatus derives the activity Stage that best represents a bd issue's current
// lifecycle status, for the single lifecycle Activity the importer emits per issue.
func stageForStatus(s provenance.Status) provenance.Stage {
	switch s {
	case provenance.StatusInProgress:
		return provenance.StageInProgress
	case provenance.StatusClosed:
		return provenance.StageComplete
	default:
		return provenance.StageNotStarted
	}
}
