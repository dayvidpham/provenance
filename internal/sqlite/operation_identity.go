package sqlite

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
)

// storedOperationReplayIdentity is the complete persisted canonical identity needed to
// decide whether an OperationID reuse is exact. Only canonical (V1) operations are
// supported; opaque legacy rows are rejected at read time with a corruption error.
type storedOperationReplayIdentity struct {
	operationID        journal.OperationID
	actorID            journal.ActorID
	authorityJournalID *journal.JournalID
	commandDigest      []byte
	mutationDigest     []byte
	encodingVersion    string
	canonicalMutation  []byte
}

// allocatedTaskReconciler resolves only provisional UUIDs from already
// committed TaskCreateAllocated result slots. The SQLite transaction layer
// supplies the real implementation; canonical identity invokes it before
// preparing the candidate and never applies it to opaque legacy rows.
type allocatedTaskReconciler func(journal.OperationInput) (journal.OperationInput, error)

func newStoredOperationReplayIdentity(
	operationID journal.OperationID,
	actorID journal.ActorID,
	authorityJournalID *journal.JournalID,
	commandDigest, mutationDigest []byte,
	encodingVersion string,
	canonicalMutation []byte,
) storedOperationReplayIdentity {
	return storedOperationReplayIdentity{
		operationID:        operationID,
		actorID:            actorID,
		authorityJournalID: cloneJournalID(authorityJournalID),
		commandDigest:      append([]byte(nil), commandDigest...),
		mutationDigest:     append([]byte(nil), mutationDigest...),
		encodingVersion:    encodingVersion,
		canonicalMutation:  append([]byte(nil), canonicalMutation...),
	}
}

func decodeStoredOperationMutation(stored storedOperationReplayIdentity) (journal.CanonicalMutation, error) {
	mutation, err := journal.DecodeCanonicalMutation(stored.canonicalMutation)
	if err != nil {
		return journal.CanonicalMutation{}, fmt.Errorf("decode stored operation %q canonical mutation: %w — where: decodeStoredOperationMutation; when: reading a committed operation's canonical bytes; impact: the caller fails closed and nothing is written; fix: %s", stored.operationID, err, unsupportedPreV004DatabaseFix)
	}
	return mutation, nil
}

// compareStoredOperationIdentity compares the stored canonical identity with the
// candidate OperationInput. Only canonical (V1) operations with an encoding_version
// and canonical_mutation are supported. A missing encoding version is a corruption
// error, not a legacy-compatibility path: pre-v0.0.4 databases are unsupported and
// are recreated, never migrated or repaired.
func compareStoredOperationIdentity(stored storedOperationReplayIdentity, candidate journal.OperationInput, reconcile allocatedTaskReconciler) error {
	if stored.encodingVersion == "" {
		// No opaque legacy compatibility: a missing encoding version in a journal row
		// that also has or lacks canonical bytes is a corruption signal, not a pre-v0.0.4 row.
		return storedIdentityIntegrityError(stored.operationID, "encoding_version",
			"operation has no encoding_version — the row is either corrupt or was committed before v0.0.4",
			unsupportedPreV004DatabaseFix)
	}
	if len(stored.canonicalMutation) == 0 {
		return storedIdentityIntegrityError(stored.operationID, "canonical_mutation", "canonical operation has an empty byte stream", "restore the evolved V1 bytes committed for this operation")
	}

	tag, err := journal.InspectCanonicalMutationEncodingVersion(stored.canonicalMutation)
	if err != nil {
		return fmt.Errorf("inspect stored operation %q canonical identity: %w", stored.operationID, err)
	}
	if !tag.MatchesStoredText(stored.encodingVersion) {
		return storedIdentityIntegrityError(stored.operationID, "encoding_version", "stored version column does not match the canonical byte tag", "restore the redundant version column from the same committed operation")
	}
	decoded, err := journal.DecodeCanonicalMutation(stored.canonicalMutation)
	if err != nil {
		return fmt.Errorf("decode stored operation %q canonical identity: %w — where: compareStoredOperationIdentity; when: replaying a stored operation against a candidate input; impact: the operation fails closed and nothing is written; fix: %s", stored.operationID, err, unsupportedPreV004DatabaseFix)
	}
	if decoded.EncodingVersion().String() != stored.encodingVersion {
		return storedIdentityIntegrityError(stored.operationID, "encoding_version", "decoded mutation version does not match the stored version column", "restore the version and canonical bytes from the same committed operation")
	}
	digest := sha256.Sum256(stored.canonicalMutation)
	if !bytes.Equal(digest[:], stored.mutationDigest) {
		return storedIdentityIntegrityError(stored.operationID, "mutation_digest", "stored digest is not SHA-256 of the canonical bytes", "restore the canonical bytes and digest from the same committed operation")
	}

	if hasAllocatedTaskCreate(candidate.Effects) {
		if reconcile == nil {
			return fmt.Errorf("reconcile allocated task identities for operation %q before canonical comparison: allocated TaskCreate requires the committed result-slot reconciler", stored.operationID)
		}
		candidate, err = reconcile(candidate)
		if err != nil {
			return fmt.Errorf("reconcile allocated task identities for operation %q before canonical comparison: %w", stored.operationID, err)
		}
	}
	prepared, err := journal.Canonicalize(candidate)
	if err != nil {
		return fmt.Errorf("prepare candidate operation %q for canonical comparison: %w", stored.operationID, err)
	}
	storedInput := journal.OperationInput{
		OperationID:        stored.operationID,
		ActorID:            stored.actorID,
		AuthorityJournalID: cloneJournalID(stored.authorityJournalID),
		CommandDigest:      append([]byte(nil), stored.commandDigest...),
		MutationDigest:     decoded.DerivedDigest(),
		Conditions:         decoded.NormalizedConditions(),
		Effects:            decoded.NormalizedEffects(),
	}
	candidateInput := candidate
	candidateInput.OperationID = stored.operationID
	candidateInput.Conditions = prepared.NormalizedConditions()
	candidateInput.Effects = prepared.NormalizedEffects()
	candidateInput.MutationDigest = prepared.DerivedDigest()
	if conflict := journal.CompareOperationIdentity(stored.operationID, storedInput, candidateInput); conflict != nil {
		return fmt.Errorf("canonical operation identity differs: %w", conflict)
	}
	return nil
}

func hasAllocatedTaskCreate(effects []journal.Effect) bool {
	for _, effect := range effects {
		if effect.Sort == journal.EffectTaskCreateAllocated {
			return true
		}
	}
	return false
}

func storedIdentityIntegrityError(operationID journal.OperationID, field, reason, fix string) error {
	return &journal.CanonicalMutationError{
		Field:  field,
		Reason: fmt.Sprintf("stored operation %q: %s", operationID, reason),
		Fix:    fix,
	}
}

func cloneJournalID(value *journal.JournalID) *journal.JournalID {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
