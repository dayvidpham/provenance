package journal

import (
	"fmt"
	"sort"
	"time"
)

const (
	MaxCanonicalConditions  = 64
	MaxFactFilterValues     = 64
	MaxFactQueryKinds       = 64
	MaxFactPageSize         = 256
	MaxCanonicalResultSlots = MaxCanonicalEffects
)

type FactTaskScopeKind uint8

const (
	FactTaskAny FactTaskScopeKind = iota
	FactTaskUnscoped
	FactTaskExact
)

func FactTaskScopeKinds() []FactTaskScopeKind {
	return []FactTaskScopeKind{FactTaskAny, FactTaskUnscoped, FactTaskExact}
}

type FactTaskScope struct {
	Kind   FactTaskScopeKind
	TaskID TaskID
}

type FactFilter struct {
	TaskScope         FactTaskScope
	RequiredContexts  []EventContext
	EffectiveActorIDs []ActorID
	OperationIDs      []OperationID
}

type FactKind uint8

const (
	FactDecision FactKind = iota
	FactEvidence
)

type FactSelector struct {
	Kind         FactKind
	Filter       FactFilter
	DecisionKind DecisionKind
	EvidenceKind EvidenceKind
}

// ConditionKind is the closed set of pre-condition semantics on an Apply.
// Zero is intentionally invalid; callers must use the named nonzero constants.
type ConditionKind uint8

const (
	// ConditionExactFact asserts that the selected fact exists and its JournalID
	// equals AssertedJournalID exactly. Returns ConditionFailure if absent or
	// if the stored JournalID differs (ConditionFactMissing or ConditionFactMismatch).
	ConditionExactFact ConditionKind = iota + 1 // nonzero: zero is invalid

	// ConditionCurrentFact asserts that the selected fact exists and is the
	// current (highest JournalID) instance. Returns ConditionFailure if absent
	// or if a newer instance exists (ConditionFactMissing or ConditionCurrentMismatch).
	ConditionCurrentFact
)

var conditionKindNames = [...]string{0: "<invalid>", 1: "ExactFact", 2: "CurrentFact"}

// String returns the diagnostic name for the condition kind.
func (k ConditionKind) String() string {
	if int(k) < len(conditionKindNames) && conditionKindNames[k] != "" {
		return conditionKindNames[k]
	}
	return fmt.Sprintf("ConditionKind(%d)", k)
}

func ConditionKinds() []ConditionKind {
	return semanticConditionKinds()
}

type Condition struct {
	Kind              ConditionKind
	Selector          FactSelector
	AssertedJournalID JournalID
}

// ConditionFailureReason is the diagnostic cause of a ConditionFailure.
type ConditionFailureReason uint8

const (
	ConditionFactMissing     ConditionFailureReason = iota // no matching fact row exists
	ConditionFactMismatch                                  // ExactFact: stored JournalID ≠ asserted
	ConditionCurrentMismatch                               // CurrentFact: a newer instance exists
)

var conditionFailureReasonNames = [...]string{"FactMissing", "FactMismatch", "CurrentMismatch"}

// String returns the diagnostic name for the failure reason.
func (r ConditionFailureReason) String() string {
	if int(r) < len(conditionFailureReasonNames) {
		return conditionFailureReasonNames[r]
	}
	return fmt.Sprintf("ConditionFailureReason(%d)", r)
}

func ConditionFailureReasons() []ConditionFailureReason {
	return []ConditionFailureReason{ConditionFactMissing, ConditionFactMismatch, ConditionCurrentMismatch}
}

// ConditionFailure and ActivityConflict are defined in conflict.go.

type FactPageRequest struct {
	Limit                int
	SnapshotMaxJournalID JournalID
	AfterJournalID       JournalID
}

type DecisionQuery struct {
	Filter FactFilter
	Kinds  []DecisionKind
	Page   FactPageRequest
}

type EvidenceQuery struct {
	Filter FactFilter
	Kinds  []EvidenceKind
	Page   FactPageRequest
}

type FactCursor struct {
	SnapshotMaxJournalID JournalID
	AfterJournalID       JournalID
}

type DecisionRow struct {
	JournalID                   JournalID
	RecordedAt                  time.Time
	TaskID                      *TaskID
	DecisionKind                DecisionKind
	Payload                     []byte
	Contexts                    []EventContext
	EffectiveActorID            ActorID
	ProducingOperationID        OperationID
	ProducingOperationJournalID JournalID
}

type EvidenceRow struct {
	JournalID                   JournalID
	RecordedAt                  time.Time
	TaskID                      *TaskID
	EvidenceKind                EvidenceKind
	ContentDigest               []byte
	Payload                     []byte
	Contexts                    []EventContext
	EffectiveActorID            ActorID
	ProducingOperationID        OperationID
	ProducingOperationJournalID JournalID
}

type DecisionPage struct {
	Rows                 []DecisionRow
	SnapshotMaxJournalID JournalID
	Next                 *FactCursor
}
type EvidencePage struct {
	Rows                 []EvidenceRow
	SnapshotMaxJournalID JournalID
	Next                 *FactCursor
}

// FactQueryAPI is implemented by the bounded read layer and exposed from the
// mutation Journal through its lifecycle-owned Facts accessor.
type FactQueryAPI interface {
	QueryDecisions(DecisionQuery) (DecisionPage, error)
	QueryEvidence(EvidenceQuery) (EvidencePage, error)
}

func (p FactPageRequest) Validate() error {
	if p.Limit < 1 || p.Limit > MaxFactPageSize || p.SnapshotMaxJournalID < 0 || p.AfterJournalID < 0 || (p.SnapshotMaxJournalID == 0 && p.AfterJournalID != 0) || (p.SnapshotMaxJournalID != 0 && p.AfterJournalID > p.SnapshotMaxJournalID) {
		return fmt.Errorf("%w: invalid fact page limit/cursor — where: fact query input; when: before SQL; impact: no query ran; fix: use Limit 1..256 and a non-negative cursor not beyond its non-zero snapshot watermark", ErrInvalidQuery)
	}
	return nil
}

func (q DecisionQuery) Validate() error {
	if err := q.Page.Validate(); err != nil {
		return err
	}
	if len(q.Kinds) == 0 || len(q.Kinds) > MaxFactQueryKinds {
		return fmt.Errorf("%w: decision kinds must contain 1..64 values", ErrInvalidQuery)
	}
	if _, err := normalizeFactFilter(q.Filter); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	for _, kind := range q.Kinds {
		if err := ValidateEventKind(EventKind(kind)); err != nil {
			return fmt.Errorf("%w: malformed decision kind: %v", ErrInvalidQuery, err)
		}
	}
	return nil
}

func (q EvidenceQuery) Validate() error {
	if err := q.Page.Validate(); err != nil {
		return err
	}
	if len(q.Kinds) == 0 || len(q.Kinds) > MaxFactQueryKinds {
		return fmt.Errorf("%w: evidence kinds must contain 1..64 values", ErrInvalidQuery)
	}
	if _, err := normalizeFactFilter(q.Filter); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	for _, kind := range q.Kinds {
		if err := ValidateEventKind(EventKind(kind)); err != nil {
			return fmt.Errorf("%w: malformed evidence kind: %v", ErrInvalidQuery, err)
		}
	}
	return nil
}

func normalizeFactFilter(in FactFilter) (FactFilter, error) {
	if len(in.RequiredContexts) > MaxCanonicalContextsPerEffect || len(in.EffectiveActorIDs) > MaxFactFilterValues || len(in.OperationIDs) > MaxFactFilterValues {
		return FactFilter{}, canonicalMutationError("fact-filter", "a collection exceeds its static bound", "reduce each filter dimension to at most 64 values")
	}
	switch in.TaskScope.Kind {
	case FactTaskAny, FactTaskUnscoped:
		if in.TaskScope.TaskID != (TaskID{}) {
			return FactFilter{}, canonicalMutationError("fact-filter.task-scope", "Any and Unscoped require a zero TaskID", "clear TaskID or select FactTaskExact")
		}
	case FactTaskExact:
		if err := validateTaskID(in.TaskScope.TaskID); err != nil {
			return FactFilter{}, canonicalMutationError("fact-filter.task-scope", err.Error(), "supply a valid exact TaskID")
		}
	default:
		return FactFilter{}, canonicalMutationError("fact-filter.task-scope", "unknown scope kind", "use a declared FactTaskScopeKind")
	}
	out := FactFilter{TaskScope: in.TaskScope}
	var err error
	out.RequiredContexts, err = CanonicalEventContexts(in.RequiredContexts)
	if err != nil {
		return FactFilter{}, canonicalMutationError("fact-filter.contexts", err.Error(), "supply valid contexts")
	}
	out.EffectiveActorIDs = append([]ActorID(nil), in.EffectiveActorIDs...)
	for _, id := range out.EffectiveActorIDs {
		if err := validateActorID(id); err != nil {
			return FactFilter{}, canonicalMutationError("fact-filter.actors", err.Error(), "supply valid ActorIDs")
		}
	}
	sort.Slice(out.EffectiveActorIDs, func(i, j int) bool { return out.EffectiveActorIDs[i].String() < out.EffectiveActorIDs[j].String() })
	out.EffectiveActorIDs = dedupActors(out.EffectiveActorIDs)
	out.OperationIDs = append([]OperationID(nil), in.OperationIDs...)
	for _, id := range out.OperationIDs {
		if err := ValidateOperationID(id); err != nil {
			return FactFilter{}, canonicalMutationError("fact-filter.operations", err.Error(), "supply valid OperationIDs")
		}
	}
	sort.Slice(out.OperationIDs, func(i, j int) bool { return out.OperationIDs[i] < out.OperationIDs[j] })
	out.OperationIDs = dedupOperations(out.OperationIDs)
	return out, nil
}

func dedupActors(v []ActorID) []ActorID {
	out := v[:0]
	for _, x := range v {
		if len(out) == 0 || out[len(out)-1] != x {
			out = append(out, x)
		}
	}
	return out
}

func dedupOperations(v []OperationID) []OperationID {
	out := v[:0]
	for _, x := range v {
		if len(out) == 0 || out[len(out)-1] != x {
			out = append(out, x)
		}
	}
	return out
}

func normalizeSelector(in FactSelector) (FactSelector, error) {
	filter, err := normalizeFactFilter(in.Filter)
	if err != nil {
		return FactSelector{}, err
	}
	out := FactSelector{Kind: in.Kind, Filter: filter}
	switch in.Kind {
	case FactDecision:
		if in.EvidenceKind != "" || ValidateEventKind(EventKind(in.DecisionKind)) != nil {
			return FactSelector{}, canonicalMutationError("selector", "Decision requires exactly one valid DecisionKind", "set DecisionKind and clear EvidenceKind")
		}
		out.DecisionKind = in.DecisionKind
	case FactEvidence:
		if in.DecisionKind != "" || ValidateEventKind(EventKind(in.EvidenceKind)) != nil {
			return FactSelector{}, canonicalMutationError("selector", "Evidence requires exactly one valid EvidenceKind", "set EvidenceKind and clear DecisionKind")
		}
		out.EvidenceKind = in.EvidenceKind
	default:
		return FactSelector{}, canonicalMutationError("selector", "unknown fact kind", "use FactDecision or FactEvidence")
	}
	return out, nil
}

// normalizeCondition validates and normalizes one Condition for canonical encoding.
// ConditionKind zero is invalid; callers must supply ConditionExactFact or ConditionCurrentFact.
func normalizeCondition(in Condition, index int) (Condition, error) {
	switch in.Kind {
	case ConditionExactFact, ConditionCurrentFact:
		// valid nonzero kinds
	default:
		return Condition{}, canonicalMutationError(conditionName(index, "kind"),
			fmt.Sprintf("invalid condition kind %s (%d) — zero is reserved; use ConditionExactFact or ConditionCurrentFact", in.Kind, in.Kind),
			"use ConditionExactFact (1) or ConditionCurrentFact (2)")
	}
	if in.AssertedJournalID < 0 {
		return Condition{}, canonicalMutationError(conditionName(index, "asserted-journal-id"), "journal id must be non-negative", "use a positive committed journal row id or 0 to assert absence")
	}
	selector, err := normalizeSelector(in.Selector)
	if err != nil {
		return Condition{}, err
	}
	return Condition{
		Kind:              in.Kind,
		Selector:          selector,
		AssertedJournalID: in.AssertedJournalID,
	}, nil
}
