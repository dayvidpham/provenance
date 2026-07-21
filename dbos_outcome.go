package provenance

// dbos_outcome.go defines the closed, JSON-stable step-outcome encoding the
// adapter checkpoints through DBOS, and the deterministic operation fingerprint
// that keys the durable workflow and step (issue dayvidpham/provenance#6).
//
// Pinned DBOS v0.16.0 serializes an ordinary Go error returned from a step as a
// plain string, erasing its type. A Provenance DOMAIN failure (a §5/§9 typed
// journal error) must survive recovery as a typed, errors.As/errors.Is-matchable
// value, so a domain failure is NEVER returned as the step's Go error: it is
// encoded INTO a DBOSStepOutcomeV1 (returned with a nil Go error) as a closed
// CanonicalApplyFailureV1 variant. Only genuine DBOS INFRASTRUCTURE failures use
// the step/workflow Go-error channel.
//
// Re-anchoring (Proposal 50 amendment): the canonical mutation result is the
// journal-anchored committed operation — its anchor JournalID, the task_event
// ProducedByOperationJournalID closure (EmittedEvents in JournalID order), and the
// slot→produced-row bindings — reconstructed from journal.CommittedResult, not a
// second decision/result ledger. A zero-task-event operation still has an anchor,
// so post-validation never depends on any task-event being present.

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// Closed schema tags and the pinned durable-identity constants. They are inputs to
// the fingerprint, so a change to any of them deliberately changes every workflow
// and step ID (a different code contract must not silently reuse a checkpoint).
const (
	ApplyWorkflowSchemaV1     = "provenance.apply/v1"
	ApplyWorkflowSchemaV2     = "provenance.apply/v2"
	DBOSStepOutcomeSchemaV1   = "provenance.dbos-step-outcome/v1"
	PinnedDBOSContractVersion = "github.com/dbos-inc/dbos-transact-golang v0.16.0"

	applyWorkflowIDPrefix   = "provenance.apply/v1/"
	applyStepNamePrefix     = "provenance.apply-step/v1/"
	applyWorkflowIDPrefixV2 = "provenance.apply/v2/"
	applyStepNamePrefixV2   = "provenance.apply-step/v2/"
)

// CanonicalResultSlotV1 is one slot→produced-row binding in the canonical result:
// the caller's local slot handle, the produced journal row, its JournalKind, and,
// for a task_event row, the resolved TaskID (empty otherwise).
type CanonicalResultSlotV1 struct {
	Slot              string `json:"slot"`
	ProducedJournalID int64  `json:"produced_journal_id"`
	Kind              int    `json:"kind"`
	TaskID            string `json:"task_id,omitempty"`
}

// CanonicalMutationResultV1 is the journal-anchored canonical result of one
// committed operation. EmittedEvents is the task_event closure in ascending
// JournalID order; ResultSlots is slot-sorted and slot-unique. Every field here is
// canonical, journal-anchored state, so its natural equality (== / reflect.DeepEqual,
// and canonicalResultsEqual) is honest — the per-call §9.4 replay flag deliberately
// does NOT live here (see DBOSStepOutcomeV1.ShortCircuited).
type CanonicalMutationResultV1 struct {
	AnchorJournalID int64                   `json:"anchor_journal_id"`
	EmittedEvents   []int64                 `json:"emitted_events"`
	ResultSlots     []CanonicalResultSlotV1 `json:"result_slots"`
}

// ApplyFailureKind is the closed discriminator over every public journal failure
// the reducer can return, plus an unexpected-failure catch-all. It is what lets a
// checkpointed domain failure reconstruct an errors.Is-matchable typed error after
// DBOS has erased the original Go error's type.
type ApplyFailureKind int

const (
	FailureUnexpected ApplyFailureKind = iota
	FailureOperationConflict
	FailureGenesis
	FailureAuthorityScope
	FailureAssignmentLifecycle
	FailureOrphanedEvidence
	FailureStaleEpisode
	FailureResultSlotIntegrity
	FailureCloseWithoutEnding
	FailureParentCitation
	FailureCorruptParentChain
	FailureInvalidID
	FailureNotFound
	FailureAlreadyClosed
	FailureGenesisRequired
)

// failureSentinels maps each closed kind to the sentinel a decoded failure wraps,
// so errors.Is(decoded, journal.ErrGenesis) (etc.) holds after recovery. The
// unexpected variant wraps no sentinel (nil) and carries only its message.
var failureSentinels = map[ApplyFailureKind]error{
	FailureOperationConflict:   journal.ErrOperationConflict,
	FailureGenesis:             journal.ErrGenesis,
	FailureAuthorityScope:      journal.ErrAuthorityScope,
	FailureAssignmentLifecycle: journal.ErrAssignmentLifecycle,
	FailureOrphanedEvidence:    journal.ErrOrphanedEvidence,
	FailureStaleEpisode:        journal.ErrStaleEpisode,
	FailureResultSlotIntegrity: journal.ErrResultSlotIntegrity,
	FailureCloseWithoutEnding:  journal.ErrCloseWithoutEnding,
	FailureParentCitation:      journal.ErrParentCitation,
	FailureCorruptParentChain:  journal.ErrCorruptParentChain,
	FailureInvalidID:           ptypes.ErrInvalidID,
	FailureNotFound:            ErrNotFound,
	FailureAlreadyClosed:       ErrAlreadyClosed,
	FailureGenesisRequired:     ErrGenesisRequired,
}

// classifyFailure maps a reducer error onto its closed kind by matching each
// public sentinel in a deterministic order (most-specific-first is unnecessary:
// the sentinels are disjoint). An error matching none is FailureUnexpected.
func classifyFailure(err error) ApplyFailureKind {
	// Ordered so the enum's own order is the scan order; disjoint sentinels.
	for _, k := range []ApplyFailureKind{
		FailureOperationConflict, FailureGenesis, FailureAuthorityScope,
		FailureAssignmentLifecycle, FailureOrphanedEvidence, FailureStaleEpisode,
		FailureResultSlotIntegrity, FailureCloseWithoutEnding, FailureParentCitation,
		FailureCorruptParentChain, FailureInvalidID, FailureNotFound,
		FailureAlreadyClosed, FailureGenesisRequired,
	} {
		if errors.Is(err, failureSentinels[k]) {
			return k
		}
	}
	return FailureUnexpected
}

// CanonicalApplyFailureV1 is the closed encoding of one checkpointed domain
// failure. Message is the original actionable text; Kind selects the sentinel a
// decoded failure wraps so callers recover the typed error with errors.Is.
type CanonicalApplyFailureV1 struct {
	Kind    ApplyFailureKind `json:"kind"`
	Message string           `json:"message"`
	// ConflictField, when Kind is FailureOperationConflict, carries the first
	// differing identity field so a decoded conflict re-exposes a typed
	// *journal.OperationConflict via errors.As.
	ConflictField string `json:"conflict_field,omitempty"`
	OperationID   string `json:"operation_id,omitempty"`
}

// DBOSStepOutcomeV1 is the closed step checkpoint: exactly one of Success or
// Failure is non-nil. OperationID and MutationDigest bind the outcome to its
// operation so post-validation can reject an outcome that drifted from its input.
type DBOSStepOutcomeV1 struct {
	Schema         string                     `json:"schema"`
	OperationID    string                     `json:"operation_id"`
	MutationDigest []byte                     `json:"mutation_digest"`
	Success        *CanonicalMutationResultV1 `json:"success,omitempty"`
	Failure        *CanonicalApplyFailureV1   `json:"failure,omitempty"`
	// ShortCircuited is a PER-CALL §9.4 replay flag, not journal-anchored canonical
	// state (LookupCommitted, a pure read, never short-circuits anything), so it lives
	// BESIDE the canonical result rather than inside CanonicalMutationResultV1 — keeping
	// that struct's equality honest for any future == / reflect.DeepEqual user. It is
	// informational (audit) only; post-validation compares canonical results, never it.
	ShortCircuited bool `json:"short_circuited,omitempty"`
}

// encodeDBOSApplySuccess encodes a committed reducer result as a closed success
// outcome. It validates the result is a CommittedExact, slot-sorts and
// deduplicates the bindings, revalidates every typed ID, and stamps the exact
// OperationID/MutationDigest.
func encodeDBOSApplySuccess(operation journal.OperationID, mutation []byte, result journal.CommittedResult) (DBOSStepOutcomeV1, error) {
	if result.Kind != journal.CommittedExact {
		return DBOSStepOutcomeV1{}, fmt.Errorf(
			"provenance: encode success outcome for operation %q — reducer returned a non-Exact result (%s) — "+
				"where: step success encode; impact: nothing is checkpointed as success; fix: a committed "+
				"operation must reconstruct as CommittedExact",
			operation, result.Kind)
	}
	slots := make([]CanonicalResultSlotV1, 0, len(result.ResultSlots))
	seen := make(map[string]struct{}, len(result.ResultSlots))
	for _, b := range result.ResultSlots {
		if _, dup := seen[string(b.Slot)]; dup {
			return DBOSStepOutcomeV1{}, fmt.Errorf(
				"provenance: encode success outcome for operation %q — duplicate result slot %q — "+
					"where: step success encode; impact: the binding map is ambiguous; fix: each "+
					"ResultSlotID must be unique within one operation (§3.2)",
				operation, b.Slot)
		}
		seen[string(b.Slot)] = struct{}{}
		taskID := ""
		if b.TaskID != nil {
			if err := journalValidateTaskID(*b.TaskID); err != nil {
				return DBOSStepOutcomeV1{}, fmt.Errorf(
					"provenance: encode success outcome for operation %q slot %q: %w", operation, b.Slot, err)
			}
			taskID = b.TaskID.String()
		}
		slots = append(slots, CanonicalResultSlotV1{
			Slot:              string(b.Slot),
			ProducedJournalID: int64(b.ProducedJournalID),
			Kind:              int(b.Kind),
			TaskID:            taskID,
		})
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Slot < slots[j].Slot })
	emitted := make([]int64, len(result.EmittedEvents))
	for i, e := range result.EmittedEvents {
		emitted[i] = int64(e)
	}
	return DBOSStepOutcomeV1{
		Schema:         DBOSStepOutcomeSchemaV1,
		OperationID:    string(operation),
		MutationDigest: append([]byte(nil), mutation...),
		Success: &CanonicalMutationResultV1{
			AnchorJournalID: int64(result.AnchorJournalID),
			EmittedEvents:   emitted,
			ResultSlots:     slots,
		},
		ShortCircuited: result.ShortCircuited,
	}, nil
}

// encodeDBOSApplyFailure encodes a reducer error as a closed failure outcome. The
// returned Go error is ALWAYS nil: a domain failure is a legitimate, durable
// checkpoint, not a step infrastructure error.
func encodeDBOSApplyFailure(operation journal.OperationID, mutation []byte, err error) (DBOSStepOutcomeV1, error) {
	kind := classifyFailure(err)
	fail := &CanonicalApplyFailureV1{
		Kind:        kind,
		Message:     err.Error(),
		OperationID: string(operation),
	}
	var conflict *journal.OperationConflict
	if errors.As(err, &conflict) {
		fail.ConflictField = conflict.Field
	}
	return DBOSStepOutcomeV1{
		Schema:         DBOSStepOutcomeSchemaV1,
		OperationID:    string(operation),
		MutationDigest: append([]byte(nil), mutation...),
		Failure:        fail,
	}, nil
}

// Decode returns the canonical success result, or the reconstructed typed failure
// error for a failure outcome. A malformed outcome (wrong schema, neither or both
// arms set) fails closed.
func (o DBOSStepOutcomeV1) Decode() (CanonicalMutationResultV1, error) {
	if o.Schema != DBOSStepOutcomeSchemaV1 {
		return CanonicalMutationResultV1{}, fmt.Errorf(
			"provenance: decode step outcome — schema tag %q is not %q — where: outcome decode; "+
				"impact: the checkpoint is not trusted; fix: outcomes are produced only by "+
				"encodeDBOSApplySuccess/Failure",
			o.Schema, DBOSStepOutcomeSchemaV1)
	}
	if (o.Success == nil) == (o.Failure == nil) {
		return CanonicalMutationResultV1{}, fmt.Errorf(
			"provenance: decode step outcome for operation %q — exactly one of Success or Failure must be "+
				"set — where: outcome decode; impact: the checkpoint is ambiguous and rejected; fix: a "+
				"well-formed outcome is a closed one-of",
			o.OperationID)
	}
	if o.Failure != nil {
		return CanonicalMutationResultV1{}, o.Failure.asError()
	}
	return *o.Success, nil
}

// asError reconstructs a typed, errors.Is/As-matchable error from a checkpointed
// failure. A FailureOperationConflict re-exposes a *journal.OperationConflict; any
// other known kind wraps its sentinel; an unexpected kind carries its message.
func (f *CanonicalApplyFailureV1) asError() error {
	if f.Kind == FailureOperationConflict {
		return fmt.Errorf("%w: %w", journal.ErrOperationConflict,
			&journal.OperationConflict{OperationID: journal.OperationID(f.OperationID), Field: f.ConflictField})
	}
	if sentinel, ok := failureSentinels[f.Kind]; ok {
		return fmt.Errorf("%s (recovered from checkpointed outcome): %w", f.Message, sentinel)
	}
	return fmt.Errorf(
		"provenance: checkpointed unexpected domain failure for operation %q: %s — where: recovered "+
			"step outcome; impact: the operation failed and nothing was committed; fix: inspect the "+
			"original message; this failure kind is outside the closed reducer set",
		f.OperationID, f.Message)
}

// ---------------------------------------------------------------------------
// Deterministic operation fingerprint (§9.4 alternate key over the pinned contract)
// ---------------------------------------------------------------------------

// fingerprint computes the SHA-256 hex digest that keys the durable workflow and
// step, over the length-delimited pinned schema/contract/version identity plus the
// operation's four-field replay identity and structural mutation. It is stable for
// an exact input and changes for any version/actor/authority/OperationID/command/
// mutation change.
func fingerprintV1(applicationVersion string, in journal.OperationInput) string {
	h := sha256.New()
	write := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		_, _ = h.Write(n[:])
		_, _ = h.Write(b)
	}
	writeStr := func(s string) { write([]byte(s)) }

	writeStr(DBOSApplyInputSchemaV1)
	writeStr(ApplyWorkflowSchemaV1)
	writeStr(DBOSStepOutcomeSchemaV1)
	writeStr(PinnedDBOSContractVersion)
	writeStr(applicationVersion)
	writeStr(in.ActorID.String())
	// Exact governing authority: the JournalID, or a distinct genesis sentinel when
	// nil, so a genesis operation and an authority-0 operation never collide.
	if in.AuthorityJournalID == nil {
		writeStr("authority:genesis")
	} else {
		var a [8]byte
		binary.BigEndian.PutUint64(a[:], uint64(int64(*in.AuthorityJournalID)))
		write(a[:])
	}
	writeStr(string(in.OperationID))
	write(in.CommandDigest)
	write(in.MutationDigest)

	sum := h.Sum(nil)
	return fmt.Sprintf("%x", sum)
}

// fingerprintV2 binds the runtime contract and the complete closed V2 input.
// Mutation is the reviewed canonical byte stream, never the caller's digest.
func fingerprintV2(applicationVersion string, input DBOSApplyInputV2) string {
	h := sha256.New()
	write := func(value []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(value)
	}
	for _, value := range [][]byte{
		[]byte(DBOSApplyInputSchemaV2), []byte(ApplyWorkflowSchemaV2),
		[]byte(DBOSStepOutcomeSchemaV1), []byte(PinnedDBOSContractVersion),
		[]byte(applicationVersion), input.Context, input.Mutation,
	} {
		write(value)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// journalValidateTaskID mirrors the journal package's task-ID validity check for a
// non-zero, parseable task identity, used when a result slot resolves a task_event
// produced row.
func journalValidateTaskID(id ptypes.TaskID) error {
	if id == (ptypes.TaskID{}) {
		return fmt.Errorf("%w: result-slot task ID is the zero value", ptypes.ErrInvalidID)
	}
	parsed, err := ptypes.ParseTaskID(id.String())
	if err != nil || parsed != id {
		return fmt.Errorf("%w: result-slot task ID does not round-trip", ptypes.ErrInvalidID)
	}
	return nil
}
