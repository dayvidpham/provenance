package provenance

// dbos_outcome.go defines the closed, JSON-stable step-outcome encoding the
// adapter checkpoints through DBOS, and the deterministic operation-scoped
// workflow identity plus full input fingerprint (issue dayvidpham/provenance#6).
//
// Pinned DBOS v0.16.0 serializes an ordinary Go error returned from a step as a
// plain string, erasing its type. A Provenance DOMAIN failure (a §5/§9 typed
// journal error) must survive recovery as a typed, errors.As/errors.Is-matchable
// value, so a domain failure is NEVER returned as the step's Go error: it is
// encoded INTO a DBOSStepOutcome (returned with a nil Go error) as a closed
// CanonicalApplyFailure variant. Only genuine DBOS INFRASTRUCTURE failures use
// the step/workflow Go-error channel.
//
// Re-anchoring (Proposal 50 amendment): the canonical mutation result is the
// journal-anchored committed operation — its anchor JournalID, the task_event
// ProducedByOperationJournalID closure (EmittedEvents in JournalID order), and the
// slot->produced-row bindings — reconstructed from journal.CommittedResult, not a
// second decision/result ledger. A zero-task-event operation still has an anchor,
// so post-validation never depends on any task-event being present.
//
// Schema tags and prefix constants are owned exclusively by dbos_contract.go.

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"sort"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// CanonicalResultSlot is one slot->produced-row binding in the canonical result.
// TaskEvent carries only TaskID, Activity carries only ActivityID, and the
// Authority/Decision/Evidence kinds carry neither entity arm.
type CanonicalResultSlot struct {
	Slot              string `json:"slot"`
	ProducedJournalID int64  `json:"produced_journal_id"`
	Kind              int    `json:"kind"`
	TaskID            string `json:"task_id,omitempty"`
	ActivityID        string `json:"activity_id,omitempty"`
}

// CanonicalMutationResult is the journal-anchored canonical result of one
// committed operation. EmittedEvents is the task_event closure in ascending
// JournalID order; ResultSlots is slot-sorted and slot-unique. Every field here is
// canonical, journal-anchored state, so its natural equality (== / reflect.DeepEqual,
// and canonicalResultsEqual) is honest -- the per-call §9.4 replay flag deliberately
// does NOT live here (see DBOSStepOutcome.ShortCircuited).
type CanonicalMutationResult struct {
	AnchorJournalID int64                 `json:"anchor_journal_id"`
	EmittedEvents   []int64               `json:"emitted_events"`
	ResultSlots     []CanonicalResultSlot `json:"result_slots"`
}

// ApplyFailureKind is the closed, string-backed wire discriminator over every
// public journal failure the reducer can return. It is what lets a checkpointed
// domain failure reconstruct an errors.Is-matchable typed error after DBOS has
// erased the original Go error's type. String backing provides stable, self-
// describing JSON wire values (Proposal 54).
//
// There is no "unexpected" catch-all: an error matching zero or multiple
// descriptors is NOT a durable domain failure and remains on the Go-error channel.
type ApplyFailureKind string

const (
	FailureOperationConflict   ApplyFailureKind = "operation_conflict"
	FailureConditionFailed     ApplyFailureKind = "condition_failed"
	FailureActivityConflict    ApplyFailureKind = "activity_conflict"
	FailureGenesis             ApplyFailureKind = "genesis"
	FailureAuthorityScope      ApplyFailureKind = "authority_scope"
	FailureAssignmentLifecycle ApplyFailureKind = "assignment_lifecycle"
	FailureOrphanedEvidence    ApplyFailureKind = "orphaned_evidence"
	FailureStaleEpisode        ApplyFailureKind = "stale_episode"
	FailureResultSlotIntegrity ApplyFailureKind = "result_slot_integrity"
	FailureCloseWithoutEnding  ApplyFailureKind = "close_without_ending"
	FailureParentCitation      ApplyFailureKind = "parent_citation"
	FailureCorruptParentChain  ApplyFailureKind = "corrupt_parent_chain"
	FailureInvalidID           ApplyFailureKind = "invalid_id"
	FailureNotFound            ApplyFailureKind = "not_found"
	FailureAlreadyClosed       ApplyFailureKind = "already_closed"
	FailureGenesisRequired     ApplyFailureKind = "genesis_required"
)

// applyFailureDescriptor pairs one closed domain-failure kind with the sentinel
// error it matches and reconstructs. The slice is the sole authority for the
// domain-failure contract; no parallel map or iota enum exists.
type applyFailureDescriptor struct {
	kind        ApplyFailureKind
	sentinel    error
	extract     func(error, *CanonicalApplyFailure) error
	validate    func(*CanonicalApplyFailure) error
	reconstruct func(*CanonicalApplyFailure) error
}

func extractOperationConflict(err error, failure *CanonicalApplyFailure) error {
	var conflict *journal.OperationConflict
	if !errors.As(err, &conflict) {
		return fmt.Errorf("operation conflict does not expose a typed *journal.OperationConflict")
	}
	axis, index := conflict.Axis, conflict.Index
	failure.ConflictAxis, failure.ConflictIndex = &axis, &index
	return nil
}

func validateOperationConflict(failure *CanonicalApplyFailure) error {
	if failure.ConflictAxis == nil {
		return &conflictMetadataError{field: DBOSDiagFieldConflictAxis, reason: "operation conflict requires conflict_axis"}
	}
	if failure.ConflictIndex == nil {
		return &conflictMetadataError{field: DBOSDiagFieldConflictIndex, reason: "operation conflict requires conflict_index"}
	}
	axis := *failure.ConflictAxis
	// All five axes are nonzero; zero is invalid. ConflictEffect is the maximum.
	if axis == 0 || axis > journal.ConflictEffect {
		return &conflictMetadataError{field: DBOSDiagFieldConflictAxis, reason: fmt.Sprintf("invalid conflict axis %d (must be 1=%s..5=%s)", axis, journal.ConflictActor, journal.ConflictEffect)}
	}
	index := *failure.ConflictIndex
	if axis <= journal.ConflictCommand && index != -1 {
		return &conflictMetadataError{field: DBOSDiagFieldConflictIndex, reason: fmt.Sprintf("scalar conflict axis %s requires conflict_index -1, got %d", axis, index)}
	}
	if axis >= journal.ConflictCondition && index < -1 {
		return &conflictMetadataError{field: DBOSDiagFieldConflictIndex, reason: fmt.Sprintf("collection conflict axis %s requires conflict_index >= -1, got %d", axis, index)}
	}
	if axis == journal.ConflictCondition && index >= MaxCanonicalConditions {
		return &conflictMetadataError{field: DBOSDiagFieldConflictIndex, reason: fmt.Sprintf("condition conflict index %d exceeds maximum %d", index, MaxCanonicalConditions-1)}
	}
	if axis == journal.ConflictEffect && index >= MaxCanonicalEffects {
		return &conflictMetadataError{field: DBOSDiagFieldConflictIndex, reason: fmt.Sprintf("effect conflict index %d exceeds maximum %d", index, MaxCanonicalEffects-1)}
	}
	return nil
}

type conflictMetadataError struct {
	field  DBOSDiagnosticField
	reason string
}

func (e *conflictMetadataError) Error() string { return e.reason }

func reconstructOperationConflict(failure *CanonicalApplyFailure) error {
	return fmt.Errorf("%s (recovered from checkpointed outcome): %w: %w", failure.Message,
		journal.ErrOperationConflict,
		&journal.OperationConflict{OperationID: journal.OperationID(failure.OperationID), Axis: *failure.ConflictAxis, Index: *failure.ConflictIndex})
}

func extractConditionFailure(err error, failure *CanonicalApplyFailure) error {
	var condition *journal.ConditionFailure
	if !errors.As(err, &condition) {
		return fmt.Errorf("condition failure does not expose a typed *journal.ConditionFailure")
	}
	index, kind, reason := condition.Index, condition.Kind, condition.Reason
	asserted, actual := int64(condition.AssertedJournalID), int64(condition.ActualJournalID)
	failure.ConditionIndex, failure.ConditionKind, failure.ConditionReason = &index, &kind, &reason
	failure.AssertedJournalID, failure.ActualJournalID = &asserted, &actual
	return nil
}

func validateConditionFailure(failure *CanonicalApplyFailure) error {
	if failure.ConditionIndex == nil {
		return &conflictMetadataError{field: DBOSDiagFieldConditionIndex, reason: "condition failure requires condition_index"}
	}
	if failure.ConditionKind == nil {
		return &conflictMetadataError{field: DBOSDiagFieldConditionKind, reason: "condition failure requires condition_kind"}
	}
	if failure.ConditionReason == nil {
		return &conflictMetadataError{field: DBOSDiagFieldConditionReason, reason: "condition failure requires condition_reason"}
	}
	if failure.AssertedJournalID == nil {
		return &conflictMetadataError{field: DBOSDiagFieldAssertedJournalID, reason: "condition failure requires asserted_journal_id"}
	}
	if failure.ActualJournalID == nil {
		return &conflictMetadataError{field: DBOSDiagFieldActualJournalID, reason: "condition failure requires actual_journal_id"}
	}
	if *failure.ConditionIndex < 0 || *failure.ConditionIndex >= MaxCanonicalConditions {
		return &conflictMetadataError{field: DBOSDiagFieldConditionIndex, reason: fmt.Sprintf("condition_index %d is outside 0..%d", *failure.ConditionIndex, MaxCanonicalConditions-1)}
	}
	if *failure.ConditionKind != journal.ConditionExactFact && *failure.ConditionKind != journal.ConditionCurrentFact {
		return &conflictMetadataError{field: DBOSDiagFieldConditionKind, reason: fmt.Sprintf("invalid condition_kind %d", *failure.ConditionKind)}
	}
	if *failure.ConditionReason < journal.ConditionFactMissing || *failure.ConditionReason > journal.ConditionCurrentMismatch {
		return &conflictMetadataError{field: DBOSDiagFieldConditionReason, reason: fmt.Sprintf("invalid condition_reason %d", *failure.ConditionReason)}
	}
	if *failure.AssertedJournalID < 0 || *failure.ActualJournalID < 0 {
		return &conflictMetadataError{field: DBOSDiagFieldAssertedJournalID, reason: "condition journal IDs must be non-negative"}
	}
	validCombination := (*failure.ConditionKind == journal.ConditionExactFact && (*failure.ConditionReason == journal.ConditionFactMissing || *failure.ConditionReason == journal.ConditionFactMismatch)) ||
		(*failure.ConditionKind == journal.ConditionCurrentFact && (*failure.ConditionReason == journal.ConditionFactMissing || *failure.ConditionReason == journal.ConditionCurrentMismatch))
	if !validCombination {
		return &conflictMetadataError{field: DBOSDiagFieldConditionReason, reason: fmt.Sprintf("condition kind %s cannot report reason %s", *failure.ConditionKind, *failure.ConditionReason)}
	}
	if *failure.ConditionReason == journal.ConditionFactMissing && *failure.ActualJournalID != 0 {
		return &conflictMetadataError{field: DBOSDiagFieldActualJournalID, reason: "FactMissing requires actual_journal_id 0"}
	}
	if *failure.ConditionKind == journal.ConditionCurrentFact && *failure.ConditionReason == journal.ConditionFactMissing && *failure.AssertedJournalID == 0 {
		return &conflictMetadataError{field: DBOSDiagFieldAssertedJournalID, reason: "CurrentFact FactMissing requires a positive asserted_journal_id"}
	}
	if *failure.ConditionReason != journal.ConditionFactMissing && *failure.ActualJournalID <= 0 {
		return &conflictMetadataError{field: DBOSDiagFieldActualJournalID, reason: "a mismatch reason requires a positive actual_journal_id"}
	}
	if *failure.ConditionReason == journal.ConditionFactMismatch && *failure.ActualJournalID == *failure.AssertedJournalID {
		return &conflictMetadataError{field: DBOSDiagFieldActualJournalID, reason: "FactMismatch requires actual_journal_id different from asserted_journal_id"}
	}
	if *failure.ConditionReason == journal.ConditionCurrentMismatch && *failure.ActualJournalID == *failure.AssertedJournalID {
		return &conflictMetadataError{field: DBOSDiagFieldActualJournalID, reason: "CurrentMismatch requires actual_journal_id different from asserted_journal_id"}
	}
	return nil
}

func reconstructConditionFailure(failure *CanonicalApplyFailure) error {
	return fmt.Errorf("%s (recovered from checkpointed outcome): %w", failure.Message, &journal.ConditionFailure{
		Index: *failure.ConditionIndex, Kind: *failure.ConditionKind, Reason: *failure.ConditionReason,
		AssertedJournalID: journal.JournalID(*failure.AssertedJournalID), ActualJournalID: journal.JournalID(*failure.ActualJournalID),
	})
}

func extractActivityConflict(err error, failure *CanonicalApplyFailure) error {
	var activity *journal.ActivityConflict
	if !errors.As(err, &activity) {
		return fmt.Errorf("activity conflict does not expose a typed *journal.ActivityConflict")
	}
	failure.ActivityID = activity.ActivityID.String()
	existing := int64(activity.ExistingJournalID)
	failure.ExistingJournalID = &existing
	return nil
}

func validateActivityConflict(failure *CanonicalApplyFailure) error {
	if failure.ActivityID == "" {
		return &conflictMetadataError{field: DBOSDiagFieldActivityID, reason: "activity conflict requires activity_id"}
	}
	if failure.ExistingJournalID == nil {
		return &conflictMetadataError{field: DBOSDiagFieldExistingJournalID, reason: "activity conflict requires existing_journal_id"}
	}
	if _, err := ptypes.ParseActivityID(failure.ActivityID); err != nil {
		return &conflictMetadataError{field: DBOSDiagFieldActivityID, reason: fmt.Sprintf("invalid activity_id: %v", err)}
	}
	if *failure.ExistingJournalID <= 0 {
		return &conflictMetadataError{field: DBOSDiagFieldExistingJournalID, reason: "existing_journal_id must be positive"}
	}
	return nil
}

func reconstructActivityConflict(failure *CanonicalApplyFailure) error {
	activity, err := ptypes.ParseActivityID(failure.ActivityID)
	if err != nil {
		return fmt.Errorf("reconstruct activity conflict: %w", err)
	}
	return fmt.Errorf("%s (recovered from checkpointed outcome): %w", failure.Message, &journal.ActivityConflict{
		ActivityID: activity, ExistingJournalID: journal.JournalID(*failure.ExistingJournalID),
	})
}

// canonicalApplyFailureDescriptors is the sole ordered descriptor literal. Each
// call returns fresh values, so callers cannot mutate shared classification or
// reconstruction authority.
func canonicalApplyFailureDescriptors() []applyFailureDescriptor {
	return []applyFailureDescriptor{
		{kind: FailureOperationConflict, sentinel: journal.ErrOperationConflict, extract: extractOperationConflict, validate: validateOperationConflict, reconstruct: reconstructOperationConflict},
		{kind: FailureConditionFailed, sentinel: journal.ErrConditionFailed, extract: extractConditionFailure, validate: validateConditionFailure, reconstruct: reconstructConditionFailure},
		{kind: FailureActivityConflict, sentinel: journal.ErrActivityConflict, extract: extractActivityConflict, validate: validateActivityConflict, reconstruct: reconstructActivityConflict},
		{kind: FailureGenesis, sentinel: journal.ErrGenesis},
		{kind: FailureAuthorityScope, sentinel: journal.ErrAuthorityScope},
		{kind: FailureAssignmentLifecycle, sentinel: journal.ErrAssignmentLifecycle},
		{kind: FailureOrphanedEvidence, sentinel: journal.ErrOrphanedEvidence},
		{kind: FailureStaleEpisode, sentinel: journal.ErrStaleEpisode},
		{kind: FailureResultSlotIntegrity, sentinel: journal.ErrResultSlotIntegrity},
		{kind: FailureCloseWithoutEnding, sentinel: journal.ErrCloseWithoutEnding},
		{kind: FailureParentCitation, sentinel: journal.ErrParentCitation},
		{kind: FailureCorruptParentChain, sentinel: journal.ErrCorruptParentChain},
		{kind: FailureInvalidID, sentinel: ptypes.ErrInvalidID},
		{kind: FailureNotFound, sentinel: ErrNotFound},
		{kind: FailureAlreadyClosed, sentinel: ErrAlreadyClosed},
		{kind: FailureGenesisRequired, sentinel: ErrGenesisRequired},
	}
}

// AmbiguousApplyFailureError reports an invalid error graph that matches more
// than one closed domain descriptor. It remains on DBOS's Go-error channel.
type AmbiguousApplyFailureError struct {
	Class  DBOSDiagnosticClass
	Field  DBOSDiagnosticField
	Stage  DBOSDiagnosticStage
	Reason string
	Impact string
	Fix    string
	cause  error
	kinds  []ApplyFailureKind
}

func (e *AmbiguousApplyFailureError) Error() string {
	return fmt.Sprintf("provenance: ambiguous apply failure -- class=%s field=%s stage=%s matched=%v; reason: %s; impact: %s; fix: %s; cause: %v",
		e.Class, e.Field, e.Stage, e.kinds, e.Reason, e.Impact, e.Fix, e.cause)
}

func (e *AmbiguousApplyFailureError) Unwrap() error { return e.cause }

// MatchedKinds returns stable contract-order evidence without exposing mutable
// internal storage.
func (e *AmbiguousApplyFailureError) MatchedKinds() []ApplyFailureKind {
	return append([]ApplyFailureKind(nil), e.kinds...)
}

// classifyDomainFailure returns a descriptor only for exactly one match. Zero
// matches return the original error; multiple matches return typed ambiguity.
func classifyDomainFailure(err error) (applyFailureDescriptor, error) {
	descriptors := canonicalApplyFailureDescriptors()
	matched := make([]applyFailureDescriptor, 0, 1)
	kinds := make([]ApplyFailureKind, 0, 1)
	for _, descriptor := range descriptors {
		if errors.Is(err, descriptor.sentinel) {
			matched = append(matched, descriptor)
			kinds = append(kinds, descriptor.kind)
		}
	}
	switch len(matched) {
	case 0:
		return applyFailureDescriptor{}, err
	case 1:
		return matched[0], nil
	default:
		return applyFailureDescriptor{}, &AmbiguousApplyFailureError{
			Class: DBOSDiagClassClassify, Field: DBOSDiagFieldDescriptorMatch, Stage: DBOSDiagStageDomainFoldClassify,
			Reason: "the error graph matches multiple closed domain failure descriptors",
			Impact: "nothing is checkpointed; DBOS treats the fold as a retryable Go error",
			Fix:    "make the returned domain sentinel graph disjoint so exactly one ApplyFailureKind matches",
			cause:  err, kinds: kinds,
		}
	}
}

// failureSentinel returns the sentinel error for the given kind from the
// closed contract table. Returns false if the kind is not in the contract.
func failureDescriptor(kind ApplyFailureKind) (applyFailureDescriptor, bool) {
	for _, descriptor := range canonicalApplyFailureDescriptors() {
		if descriptor.kind == kind {
			return descriptor, true
		}
	}
	return applyFailureDescriptor{}, false
}

// CanonicalApplyFailure is the closed encoding of one checkpointed domain
// failure. Message is the original actionable text; Kind selects the sentinel a
// decoded failure wraps so callers recover the typed error with errors.Is.
// OperationID must equal the outer DBOSStepOutcome.OperationID (validated on decode).
type CanonicalApplyFailure struct {
	Kind    ApplyFailureKind `json:"kind"`
	Message string           `json:"message"`
	// ConflictAxis and ConflictIndex are set on FailureOperationConflict.
	// Index is -1 for scalar axes (Actor/Authority/Command) or collection-length mismatch.
	ConflictAxis  *journal.ConflictAxis `json:"conflict_axis,omitempty"`
	ConflictIndex *int                  `json:"conflict_index,omitempty"`
	// Condition fields are set only on FailureConditionFailed. Pointers make
	// omitted metadata distinguishable from its valid zero values on decode.
	ConditionIndex    *int                            `json:"condition_index,omitempty"`
	ConditionKind     *journal.ConditionKind          `json:"condition_kind,omitempty"`
	ConditionReason   *journal.ConditionFailureReason `json:"condition_reason,omitempty"`
	AssertedJournalID *int64                          `json:"asserted_journal_id,omitempty"`
	ActualJournalID   *int64                          `json:"actual_journal_id,omitempty"`
	// Activity fields are set only on FailureActivityConflict.
	ActivityID        string `json:"activity_id,omitempty"`
	ExistingJournalID *int64 `json:"existing_journal_id,omitempty"`
	OperationID       string `json:"operation_id,omitempty"`
}

// DBOSStepOutcome is the closed step checkpoint: exactly one of Success or
// Failure is non-nil. OperationID and MutationDigest bind the outcome to its
// operation so post-validation can reject an outcome that drifted from its input.
type DBOSStepOutcome struct {
	Schema         string                   `json:"schema"`
	OperationID    string                   `json:"operation_id"`
	MutationDigest []byte                   `json:"mutation_digest"`
	Success        *CanonicalMutationResult `json:"success,omitempty"`
	Failure        *CanonicalApplyFailure   `json:"failure,omitempty"`
	// ShortCircuited is a PER-CALL §9.4 replay flag, not journal-anchored canonical
	// state (LookupCommitted, a pure read, never short-circuits anything), so it lives
	// BESIDE the canonical result rather than inside CanonicalMutationResult -- keeping
	// that struct's equality honest for any future == / reflect.DeepEqual user. It is
	// informational (audit) only; post-validation compares canonical results, never it.
	ShortCircuited bool `json:"short_circuited,omitempty"`
}

// UnmarshalJSON rejects unknown and duplicate fields before a recovered outcome
// can reach post-validation. The semantic one-of and typed metadata checks stay
// in decodeDBOSStepOutcome so direct and recovered delivery share one authority.
func (o *DBOSStepOutcome) UnmarshalJSON(raw []byte) error {
	type wire DBOSStepOutcome
	var decoded wire
	if err := decodeStrictDBOSJSON(raw, &decoded); err != nil {
		return fmt.Errorf("decode DBOS step outcome JSON: %w", err)
	}
	*o = DBOSStepOutcome(decoded)
	return nil
}

// encodeDBOSApplySuccess encodes a committed reducer result as a closed success
// outcome. It validates the result is a CommittedExact, slot-sorts and
// deduplicates the bindings, revalidates every typed ID, and stamps the exact
// OperationID/MutationDigest.
func encodeDBOSApplySuccess(contract dbosContractSnapshot, operation journal.OperationID, mutation []byte, result journal.CommittedResult) (DBOSStepOutcome, error) {
	if result.Kind != journal.CommittedExact {
		return DBOSStepOutcome{}, fmt.Errorf(
			"provenance: encode success outcome for operation %q -- reducer returned a non-Exact result (%s) -- "+
				"where: step success encode; impact: nothing is checkpointed as success; fix: a committed "+
				"operation must reconstruct as CommittedExact",
			operation, result.Kind)
	}
	if len(result.ResultSlots) > journal.MaxCanonicalResultSlots {
		return DBOSStepOutcome{}, fmt.Errorf("%w: provenance: encode success outcome for operation %q -- result-slot count %d exceeds maximum %d -- where: step success encode; when: before allocating transport collections; impact: nothing is checkpointed; fix: split the operation into bounded mutations", journal.ErrResultSlotIntegrity, operation, len(result.ResultSlots), journal.MaxCanonicalResultSlots)
	}
	slots := make([]CanonicalResultSlot, 0, len(result.ResultSlots))
	seen := make(map[string]struct{}, len(result.ResultSlots))
	for _, b := range result.ResultSlots {
		if _, dup := seen[string(b.Slot)]; dup {
			return DBOSStepOutcome{}, fmt.Errorf(
				"%w: provenance: encode success outcome for operation %q -- duplicate result slot %q -- "+
					"where: step success encode; impact: the binding map is ambiguous; fix: each "+
					"ResultSlotID must be unique within one operation (§3.2)",
				journal.ErrResultSlotIntegrity, operation, b.Slot)
		}
		seen[string(b.Slot)] = struct{}{}
		slot, err := canonicalResultSlotFromBinding(b)
		if err != nil {
			return DBOSStepOutcome{}, fmt.Errorf(
				"provenance: encode success outcome for operation %q slot %q: %w", operation, b.Slot, err)
		}
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Slot < slots[j].Slot })
	if len(result.EmittedEvents) > MaxCanonicalEffects {
		return DBOSStepOutcome{}, fmt.Errorf("%w: provenance: encode success outcome for operation %q -- emitted-event count %d exceeds maximum %d before allocation -- where: step success encode; impact: the checkpoint is rejected; fix: split the operation into bounded effects", journal.ErrResultSlotIntegrity, operation, len(result.EmittedEvents), MaxCanonicalEffects)
	}
	emitted := make([]int64, len(result.EmittedEvents))
	for i, event := range result.EmittedEvents {
		emitted[i] = int64(event)
	}
	if err := validateCanonicalMutationResult(int64(result.AnchorJournalID), emitted, slots); err != nil {
		return DBOSStepOutcome{}, fmt.Errorf("provenance: encode success outcome for operation %q: %w", operation, err)
	}
	return DBOSStepOutcome{
		Schema:         contract.outcomeSchema,
		OperationID:    string(operation),
		MutationDigest: append([]byte(nil), mutation...),
		Success: &CanonicalMutationResult{
			AnchorJournalID: int64(result.AnchorJournalID),
			EmittedEvents:   emitted,
			ResultSlots:     slots,
		},
		ShortCircuited: result.ShortCircuited,
	}, nil
}

// encodeDBOSApplyFailure encodes a uniquely classified reducer error as a closed
// failure outcome. The returned Go error is ALWAYS nil: a domain failure is a
// legitimate, durable checkpoint, not a step infrastructure error.
//
// Callers MUST have verified via classifyDomainFailure that err has exactly one
// matching descriptor before calling this function.
func encodeDBOSApplyFailure(contract dbosContractSnapshot, operation journal.OperationID, mutation []byte, err error) (DBOSStepOutcome, error) {
	descriptor, classifyErr := classifyDomainFailure(err)
	if classifyErr != nil {
		return DBOSStepOutcome{}, classifyErr
	}
	fail := &CanonicalApplyFailure{
		Kind:        descriptor.kind,
		Message:     err.Error(),
		OperationID: string(operation),
	}
	if descriptor.extract != nil {
		if extractErr := descriptor.extract(err, fail); extractErr != nil {
			return DBOSStepOutcome{}, diagnostic(DBOSDiagClassClassify, DBOSDiagFieldConflictAxis,
				DBOSDiagStageOutcomeEncode, operation, "", extractErr.Error(), "nothing is checkpointed",
				"return the descriptor's complete typed domain error", err)
		}
	}
	if validateErr := validateApplyFailureEnvelope(string(operation), fail, descriptor); validateErr != nil {
		return DBOSStepOutcome{}, validateErr
	}
	return DBOSStepOutcome{
		Schema:         contract.outcomeSchema,
		OperationID:    string(operation),
		MutationDigest: append([]byte(nil), mutation...),
		Failure:        fail,
	}, nil
}

// Decode returns the canonical success result, or the reconstructed typed failure
// error for a failure outcome. A malformed outcome (wrong schema, neither or both
// arms set, unknown failure kind, or mismatched nested OperationID) fails closed.
func decodeDBOSStepOutcome(contract dbosContractSnapshot, o DBOSStepOutcome) (CanonicalMutationResult, error) {
	operation := journal.OperationID(o.OperationID)
	if o.Schema != contract.outcomeSchema {
		return CanonicalMutationResult{}, diagnostic(DBOSDiagClassOutcomeDecode, DBOSDiagFieldSchema,
			DBOSDiagStageOutcomeDecode, operation, "", fmt.Sprintf("schema %q is not %q", o.Schema, contract.outcomeSchema),
			"the checkpoint is not trusted", "restore an outcome produced by this adapter contract", nil)
	}
	if (o.Success == nil) == (o.Failure == nil) {
		return CanonicalMutationResult{}, diagnostic(DBOSDiagClassOutcomeDecode, DBOSDiagFieldSuccessFailure,
			DBOSDiagStageOutcomeDecode, operation, "", "exactly one of success or failure must be set",
			"the ambiguous checkpoint is rejected", "restore a closed one-of outcome", nil)
	}
	if o.Failure != nil {
		descriptor, known := failureDescriptor(o.Failure.Kind)
		if !known {
			return CanonicalMutationResult{}, diagnostic(DBOSDiagClassOutcomeDecode, DBOSDiagFieldKind,
				DBOSDiagStageOutcomeDecode, operation, "", fmt.Sprintf("unknown durable failure kind %q", o.Failure.Kind),
				"the checkpoint is rejected", "restore an outcome produced by the closed failure contract", nil)
		}
		if err := validateApplyFailureEnvelope(o.OperationID, o.Failure, descriptor); err != nil {
			return CanonicalMutationResult{}, err
		}
		return CanonicalMutationResult{}, descriptor.asError(o.Failure)
	}
	if err := validateCanonicalMutationResult(o.Success.AnchorJournalID, o.Success.EmittedEvents, o.Success.ResultSlots); err != nil {
		return CanonicalMutationResult{}, diagnostic(DBOSDiagClassOutcomeDecode, DBOSDiagFieldKind,
			DBOSDiagStageOutcomeDecode, operation, "", err.Error(), "the malformed success checkpoint is rejected",
			"restore slot metadata produced by the same committed operation", err)
	}
	return *o.Success, nil
}

func canonicalResultSlotFromBinding(binding journal.ResultSlotBinding) (CanonicalResultSlot, error) {
	if err := journal.ValidateResultSlotBinding(binding); err != nil {
		return CanonicalResultSlot{}, err
	}
	slot := CanonicalResultSlot{
		Slot:              string(binding.Slot),
		ProducedJournalID: int64(binding.ProducedJournalID),
		Kind:              int(binding.Kind),
	}
	if binding.TaskID != nil {
		slot.TaskID = binding.TaskID.String()
	}
	if binding.ActivityID != nil {
		slot.ActivityID = binding.ActivityID.String()
	}
	return slot, nil
}

func resultSlotBindingFromCanonical(slot CanonicalResultSlot) (journal.ResultSlotBinding, error) {
	binding := journal.ResultSlotBinding{
		Slot:              journal.ResultSlotID(slot.Slot),
		ProducedJournalID: journal.JournalID(slot.ProducedJournalID),
		Kind:              journal.JournalKind(slot.Kind),
	}
	if slot.TaskID != "" {
		taskID, err := ptypes.ParseTaskID(slot.TaskID)
		if err != nil {
			return journal.ResultSlotBinding{}, fmt.Errorf("%w: parse TaskID %q: %v", journal.ErrResultSlotIntegrity, slot.TaskID, err)
		}
		binding.TaskID = &taskID
	}
	if slot.ActivityID != "" {
		activityID, err := ptypes.ParseActivityID(slot.ActivityID)
		if err != nil {
			return journal.ResultSlotBinding{}, fmt.Errorf("%w: parse ActivityID %q: %v", journal.ErrResultSlotIntegrity, slot.ActivityID, err)
		}
		binding.ActivityID = &activityID
	}
	if err := journal.ValidateResultSlotBinding(binding); err != nil {
		return journal.ResultSlotBinding{}, err
	}
	return binding, nil
}

func validateCanonicalResultSlots(slots []CanonicalResultSlot) error {
	if len(slots) > journal.MaxCanonicalResultSlots {
		return fmt.Errorf("%w: result-slot count %d exceeds maximum %d before validation allocation", journal.ErrResultSlotIntegrity, len(slots), journal.MaxCanonicalResultSlots)
	}
	for i, slot := range slots {
		if i > 0 && slots[i-1].Slot >= slot.Slot {
			return fmt.Errorf("%w: result slots must be unique and sorted: slot %q follows %q", journal.ErrResultSlotIntegrity, slot.Slot, slots[i-1].Slot)
		}
		if _, err := resultSlotBindingFromCanonical(slot); err != nil {
			return fmt.Errorf("result slot %d (%q): %w", i, slot.Slot, err)
		}
	}
	return nil
}

// validateCanonicalMutationResult validates the complete journal-anchored
// result, not only its slot arms. This is shared by success encode/decode and
// equality so a malformed anchor/event closure cannot cross one boundary and
// be accepted at another.
func validateCanonicalMutationResult(anchor int64, emitted []int64, slots []CanonicalResultSlot) error {
	if anchor <= 0 {
		return fmt.Errorf("%w: anchor_journal_id must be positive", journal.ErrResultSlotIntegrity)
	}
	if len(emitted) > MaxCanonicalEffects {
		return fmt.Errorf("%w: emitted-event count %d exceeds maximum %d before validation iteration", journal.ErrResultSlotIntegrity, len(emitted), MaxCanonicalEffects)
	}
	previous := anchor
	for i, event := range emitted {
		if event <= previous {
			return fmt.Errorf("%w: emitted_events[%d]=%d is not strictly after %d", journal.ErrResultSlotIntegrity, i, event, previous)
		}
		previous = event
	}
	if err := validateCanonicalResultSlots(slots); err != nil {
		return err
	}
	for i, slot := range slots {
		if slot.ProducedJournalID <= anchor {
			return fmt.Errorf("%w: result_slots[%d] produced journal %d is not after anchor %d", journal.ErrResultSlotIntegrity, i, slot.ProducedJournalID, anchor)
		}
	}
	return nil
}

func validateApplyFailureEnvelope(outerOperation string, failure *CanonicalApplyFailure, descriptor applyFailureDescriptor) error {
	op := journal.OperationID(outerOperation)
	reject := func(field DBOSDiagnosticField, reason, fix string) error {
		return diagnostic(DBOSDiagClassOutcomeDecode, field, DBOSDiagStageOutcomeDecode, op, "", reason,
			"the malformed durable checkpoint is rejected", fix, nil)
	}
	if err := journal.ValidateOperationID(op); err != nil {
		return reject(DBOSDiagFieldOperation, "outer operation identity is invalid: "+err.Error(), "restore the original valid operation identity")
	}
	nested := journal.OperationID(failure.OperationID)
	if err := journal.ValidateOperationID(nested); err != nil {
		return reject(DBOSDiagFieldNestedOpID, "nested operation identity is invalid: "+err.Error(), "restore the original valid nested operation identity")
	}
	if failure.OperationID != outerOperation {
		return reject(DBOSDiagFieldNestedOpID, fmt.Sprintf("nested operation %q does not match outer operation %q", failure.OperationID, outerOperation), "restore both identities from the same execution")
	}
	if failure.Message == "" {
		return reject(DBOSDiagFieldMessage, "failure message is empty", "restore the original actionable domain failure message")
	}
	metadataReject := func(field DBOSDiagnosticField, reason, fix string) error {
		return reject(field, reason, fix)
	}
	if descriptor.kind != FailureOperationConflict && (failure.ConflictAxis != nil || failure.ConflictIndex != nil) {
		return metadataReject(DBOSDiagFieldConflictAxis, "operation conflict metadata is forbidden for this failure kind", failureMetadataRemovalFix(DBOSDiagFieldConflictAxis))
	}
	if descriptor.kind != FailureConditionFailed && (failure.ConditionIndex != nil || failure.ConditionKind != nil || failure.ConditionReason != nil || failure.AssertedJournalID != nil || failure.ActualJournalID != nil) {
		return metadataReject(DBOSDiagFieldConditionIndex, "condition failure metadata is forbidden for this failure kind", failureMetadataRemovalFix(DBOSDiagFieldConditionIndex))
	}
	if descriptor.kind != FailureActivityConflict && (failure.ActivityID != "" || failure.ExistingJournalID != nil) {
		return metadataReject(DBOSDiagFieldActivityID, "activity conflict metadata is forbidden for this failure kind", failureMetadataRemovalFix(DBOSDiagFieldActivityID))
	}
	if descriptor.validate != nil {
		if err := descriptor.validate(failure); err != nil {
			field := DBOSDiagFieldConflictAxis
			var metadata *conflictMetadataError
			if errors.As(err, &metadata) {
				field = metadata.field
			}
			return reject(field, err.Error(), failureRepairFix(descriptor.kind, field))
		}
	}
	return nil
}

func failureRepairFix(kind ApplyFailureKind, field DBOSDiagnosticField) string {
	switch kind {
	case FailureOperationConflict:
		return "restore conflict_axis and conflict_index from the canonical OperationConflict descriptor, using -1 for scalar or length-mismatch conflicts"
	case FailureConditionFailed:
		return "restore condition_index, kind, reason, and observed journal IDs from the canonical ConditionFailure descriptor"
	case FailureActivityConflict:
		return "restore activity_id and existing_journal_id from the canonical ActivityConflict descriptor"
	default:
		switch field {
		case DBOSDiagFieldConflictAxis, DBOSDiagFieldConflictIndex:
			return "remove operation-conflict metadata from this failure descriptor"
		case DBOSDiagFieldConditionIndex, DBOSDiagFieldConditionKind, DBOSDiagFieldConditionReason, DBOSDiagFieldAssertedJournalID, DBOSDiagFieldActualJournalID:
			return "remove condition-failure metadata from this failure descriptor"
		case DBOSDiagFieldActivityID, DBOSDiagFieldExistingJournalID:
			return "remove activity-conflict metadata from this failure descriptor"
		default:
			return "restore the complete metadata for the closed failure descriptor"
		}
	}
}

func failureMetadataRemovalFix(field DBOSDiagnosticField) string {
	switch field {
	case DBOSDiagFieldConflictAxis, DBOSDiagFieldConflictIndex:
		return "remove operation-conflict metadata from this failure descriptor"
	case DBOSDiagFieldConditionIndex, DBOSDiagFieldConditionKind, DBOSDiagFieldConditionReason, DBOSDiagFieldAssertedJournalID, DBOSDiagFieldActualJournalID:
		return "remove condition-failure metadata from this failure descriptor"
	case DBOSDiagFieldActivityID, DBOSDiagFieldExistingJournalID:
		return "remove activity-conflict metadata from this failure descriptor"
	default:
		return "remove metadata that belongs to a different closed failure descriptor"
	}
}

func (d applyFailureDescriptor) asError(failure *CanonicalApplyFailure) error {
	if d.reconstruct != nil {
		return d.reconstruct(failure)
	}
	return fmt.Errorf("%s (recovered from checkpointed outcome): %w", failure.Message, d.sentinel)
}

func diagnostic(class DBOSDiagnosticClass, field DBOSDiagnosticField, stage DBOSDiagnosticStage, operation OperationID, workflow, reason, impact, fix string, cause error) error {
	return &DBOSDiagnosticError{Class: class, Field: field, Stage: stage, Operation: operation, Workflow: workflow,
		Reason: reason, Impact: impact, Fix: fix, Cause: cause}
}

// ---------------------------------------------------------------------------
// Deterministic workflow identity and operation fingerprint
// ---------------------------------------------------------------------------

// workflowIdentity computes the operation-scoped portion of the durable workflow
// ID. Every canonical input variant for one OperationID must address one DBOS
// workflow; fingerprint remains the stricter input/step collision guard below.
func workflowIdentity(contract dbosContractSnapshot, applicationVersion string, operation OperationID) string {
	h := sha256.New()
	for _, value := range [][]byte{
		[]byte(contract.applyInputSchema), []byte(contract.contextSchema),
		[]byte(contract.outcomeSchema), []byte(contract.workflowSchema),
		[]byte(contract.workflowPrefix), []byte(contract.stepPrefix),
		[]byte(contract.pinnedLibrary),
		[]byte(applicationVersion), []byte(operation),
	} {
		writeFingerprintValue(h, value)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// fingerprint computes the SHA-256 hex digest that keys the durable workflow and
// step, over the length-delimited pinned schema/contract/version identity plus the
// operation's four-field replay identity and structural mutation. It is stable for
// an exact input and changes for any version/actor/authority/OperationID/command/
// mutation change. Mutation is the reviewed canonical byte stream, never the caller's digest;
// audit-only RecordedAt remains transported but deliberately is not hashed.
//
// All schema/contract strings are drawn from the adapter-captured dbosContractSnapshot
// via the adapter field, NOT from the package-level const aliases — the snapshot is
// the sole identity authority per Proposal 54.
func fingerprint(contract dbosContractSnapshot, applicationVersion string, input DBOSApplyInput) (string, error) {
	identity, err := decodeDBOSContext(contract, input.Context)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	values := [][]byte{
		[]byte(contract.applyInputSchema), []byte(contract.workflowSchema),
		[]byte(contract.outcomeSchema), []byte(contract.pinnedLibrary),
		[]byte(applicationVersion), []byte(identity.OperationID), []byte(actorToWire(identity.ActorID)),
	}
	if identity.AuthorityJournalID == nil {
		values = append(values, []byte("authority:genesis"))
	} else {
		var authority [8]byte
		binary.BigEndian.PutUint64(authority[:], uint64(*identity.AuthorityJournalID))
		values = append(values, authority[:])
	}
	values = append(values, identity.CommandDigest, input.Mutation)
	for _, value := range values {
		writeFingerprintValue(h, value)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func writeFingerprintValue(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}
