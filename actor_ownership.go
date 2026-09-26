package provenance

import "github.com/dayvidpham/provenance/internal/journal"

// Actor-ownership queries are optional on Journal. SQLite and borrowed SQLite
// support this interface; see docs/actor-ownership-queries.md.
type ActorOwnershipQueryAPI = journal.ActorOwnershipQueryAPI
type ActorOwnershipQuery = journal.ActorOwnershipQuery
type ActorOwnershipSnapshot = journal.ActorOwnershipSnapshot
type OwnedTaskRow = journal.OwnedTaskRow
type OwnedMaterialRow = journal.OwnedMaterialRow
type OwnedEvidenceRow = journal.OwnedEvidenceRow
type ActorOwnershipWork = journal.ActorOwnershipWork
type ActorOwnershipStage = journal.ActorOwnershipStage
type ActorOwnershipLimitError = journal.ActorOwnershipLimitError
type ActorOwnershipIntegrityError = journal.ActorOwnershipIntegrityError
type OwnerProjectionMismatchError = journal.OwnerProjectionMismatchError

const (
	ActorOwnershipStageOwnedTasks = journal.ActorOwnershipStageOwnedTasks
	ActorOwnershipStageMaterials  = journal.ActorOwnershipStageMaterials
	ActorOwnershipStageEvidence   = journal.ActorOwnershipStageEvidence

	MaxActorOwnershipResultBytes = journal.MaxActorOwnershipResultBytes
)

// ErrActorOwnershipLimit marks unsupported stored result size, not capacity.
var ErrActorOwnershipLimit = journal.ErrActorOwnershipLimit
