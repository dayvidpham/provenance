// Package allocation defines the closed model and codec for governed task
// allocation. It deliberately depends only on the stable public identity types
// and the journal identity aliases, so both the standalone Modernc path and the
// DBOS fused path execute the same reducer.
package allocation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

const (
	// MaxChildren is the largest governed batch accepted by the MVP contract.
	MaxChildren = 128

	maxCommandBytes      = 1024
	maxAssignmentIDBytes = 256
	maxTitleBytes        = 4096
	maxDescriptionBytes  = 64 << 10
	// A result contains at most MaxChildren child bindings and one emitted-row
	// and slot binding per accepted canonical effect. Six is JSON's worst-case
	// expansion for each input byte (a \uXXXX escape). Derive the private decoder
	// ceiling from the already accepted aggregate input bounds rather than an
	// unrelated one-MiB limit that could reject a valid committed result.
	maxAcceptedChildInputBytes = maxAssignmentIDBytes + maxTitleBytes + maxDescriptionBytes + maxCommandBytes
	maxResultWireBytes         = 6*(MaxChildren*maxAcceptedChildInputBytes+journal.MaxCanonicalEffects*maxAssignmentIDBytes) + 64<<10
	maxResultWireTokens        = 64 + MaxChildren*48 + journal.MaxCanonicalEffects*16
	maxResultWireDepth         = 16
)

// ErrorKind is the closed classification for governed-allocation failures.
// Callers should use errors.As to inspect an Error and branch on Kind rather
// than parsing Error text.
type ErrorKind uint8

const (
	ErrorValidation ErrorKind = iota + 1
	ErrorConflict
	ErrorAuthority
	ErrorRevoked
	ErrorDepth
	ErrorCollision
	ErrorGenesis
	ErrorCorruption
)

func (kind ErrorKind) String() string {
	switch kind {
	case ErrorValidation:
		return "validation"
	case ErrorConflict:
		return "conflict"
	case ErrorAuthority:
		return "authority"
	case ErrorRevoked:
		return "revoked"
	case ErrorDepth:
		return "depth"
	case ErrorCollision:
		return "collision"
	case ErrorGenesis:
		return "genesis"
	case ErrorCorruption:
		return "corruption"
	default:
		return fmt.Sprintf("ErrorKind(%d)", kind)
	}
}

// Error is an actionable, typed governed-allocation error. Operation identifies
// the rejected request where available; Where names the reducer boundary that
// detected it, and Fix tells a caller how to make forward progress.
type Error struct {
	Kind      ErrorKind
	Operation journal.OperationID
	Where     string
	Why       string
	Impact    string
	Fix       string
	Cause     error
}

func (e *Error) Error() string {
	return fmt.Sprintf(
		"provenance: governed allocation %s for operation %q -- where: %s; why: %s; impact: %s; fix: %s; cause: %v",
		e.Kind, e.Operation, e.Where, e.Why, e.Impact, e.Fix, e.Cause)
}

func (e *Error) Unwrap() error { return e.Cause }

// NewError makes a non-nil, actionable failure at a public API boundary.
func NewError(kind ErrorKind, operation journal.OperationID, where, why, impact, fix string, cause error) *Error {
	return &Error{Kind: kind, Operation: operation, Where: where, Why: why, Impact: impact, Fix: fix, Cause: cause}
}

// ChildSpec is one caller-identified task plus its initial assignment. The
// submitted slice order is semantically significant and is part of the
// canonical operation identity.
type ChildSpec struct {
	TaskID       ptypes.TaskID
	AssignmentID journal.AssignmentID
	Occupant     ptypes.ActorID
	Title        string
	Description  string
	Type         ptypes.TaskType
	Priority     ptypes.Priority
	Phase        ptypes.Phase
}

// GovernedAllocationRequest creates ordered children under one exact active
// parent assignment. Command is a caller-stable command identity; it is
// persisted in the canonical request so an OperationID cannot be reused for a
// different command or reordered/modified child list.
type GovernedAllocationRequest struct {
	OperationID        journal.OperationID
	ActorID            ptypes.ActorID
	Command            string
	ParentAssignmentID journal.AssignmentID
	Children           []ChildSpec
}

// CompositionVersion identifies the closed governed-allocation composition
// receipt. It is deliberately separate from the canonical journal mutation
// version: one governs the allocation-plus-supplements envelope, the other
// governs the canonical representation of each ordered supplemental effect.
type CompositionVersion uint8

const (
	// CompositionV1 is the only supported allocation/supplement envelope.
	CompositionV1 CompositionVersion = iota + 1
)

// SupplementPolicy identifies the statically closed supplemental effect set.
// It is a named policy rather than a caller-configured allow-list so a caller
// cannot widen allocation authority at runtime.
type SupplementPolicy uint8

const (
	// SupplementPolicyV1 permits only evidence, edge-add, task-event, and
	// activity-create effects. Allocation and assignment effects remain owned by
	// the governed allocator itself.
	SupplementPolicyV1 SupplementPolicy = iota + 1
)

func (policy SupplementPolicy) String() string {
	switch policy {
	case SupplementPolicyV1:
		return "governed-allocation-supplement-policy.v1"
	default:
		return fmt.Sprintf("SupplementPolicy(%d)", policy)
	}
}

// ReferenceScopeKind is the closed set of additional task-reference domains a
// composed request may cite. Zero means no additional references (legacy).
type ReferenceScopeKind uint8

const (
	// ReferenceScopeDescendants admits only explicitly named tasks whose stored
	// assignment lineage is proven to descend from Allocation.ParentAssignmentID.
	ReferenceScopeDescendants ReferenceScopeKind = iota + 1
)

// ReferenceScope explicitly names existing descendant subjects used by generic
// supplemental effects. It grants no allocation authority; every task is
// transaction-locally proven inside the allocation parent's assignment domain.
type ReferenceScope struct {
	Kind     ReferenceScopeKind
	Subjects []ptypes.TaskID
}

// ComposedRequest atomically allocates Children and then reduces the ordered
// SupplementalEffects through the canonical journal reducer in the same
// transaction. Version must be CompositionV1. SupplementalEffects are a
// strongly typed journal.Effect list; there is no stringly-typed side channel.
//
// OperationID remains Allocation.OperationID. The reducer derives a
// domain-separated, transaction-local journal operation identity only for the
// supplemental reducer rows; that internal identity is never an external API
// operation key.
type ComposedRequest struct {
	Version    CompositionVersion
	Allocation GovernedAllocationRequest
	// Conditions are ordered canonical journal fact assertions evaluated by the
	// fused transaction before the composition commits. The exported root package
	// aliases journal.Condition, so callers never need to import this internal
	// package to construct them.
	Conditions          []journal.Condition
	ReferenceScope      ReferenceScope
	SupplementalEffects []journal.Effect
}

// GovernedAllocationSupplementOperationID returns the stable persistence
// identity for a composed allocation's reducer-owned supplemental operation.
// It intentionally returns only the correlation identity, not the internal
// capability token required to execute a reserved operation.
func GovernedAllocationSupplementOperationID(external journal.OperationID) journal.OperationID {
	return journal.NewGovernedAllocationSupplementOperationID(external).OperationID()
}

// RootGenesisRequest creates the one initial root task and its initial root
// assignment. It has no authority field: NULL authority is permitted only for
// this single root initialization operation.
type RootGenesisRequest struct {
	OperationID journal.OperationID
	ActorID     ptypes.ActorID
	Command     string
	Root        ChildSpec
}

// RequestKind distinguishes the two immutable operation shapes persisted by
// the reducer.
type RequestKind uint8

const (
	RequestKindGenesis RequestKind = iota + 1
	RequestKindAllocation
)

func (kind RequestKind) String() string {
	switch kind {
	case RequestKindGenesis:
		return "genesis"
	case RequestKindAllocation:
		return "allocation"
	default:
		return fmt.Sprintf("RequestKind(%d)", kind)
	}
}

// ProducedRow is the immutable structural position of one row created by a
// governed operation. EffectOrdinal is the submitted child index and
// Subordinal is 0 for its task event or 1 for its assignment authority.
type ProducedRow struct {
	OperationID   journal.OperationID
	EffectOrdinal int
	Subordinal    int
	JournalID     journal.JournalID
}

// ChildBinding binds one caller child to its committed task and assignment
// identities and to the two persisted structural row positions.
type ChildBinding struct {
	Ordinal       int
	TaskID        ptypes.TaskID
	AssignmentID  journal.AssignmentID
	Occupant      ptypes.ActorID
	TaskRow       ProducedRow
	AssignmentRow ProducedRow
}

// OperationClosure is a copy-safe reconstructed governed result. Its mutable
// state is private; accessors return values or fresh slices so a caller cannot
// alter the closure cached by a retry or later recovered from SQLite.
type OperationClosure struct {
	operationID journal.OperationID
	kind        RequestKind
	anchor      journal.JournalID
	children    []ChildBinding
}

// NewClosure constructs a copy-safe closure after reducer structural checks.
func NewClosure(operationID journal.OperationID, kind RequestKind, anchor journal.JournalID, children []ChildBinding) OperationClosure {
	return OperationClosure{
		operationID: operationID,
		kind:        kind,
		anchor:      anchor,
		children:    append([]ChildBinding(nil), children...),
	}
}

func (c OperationClosure) OperationID() journal.OperationID   { return c.operationID }
func (c OperationClosure) Kind() RequestKind                  { return c.kind }
func (c OperationClosure) AnchorJournalID() journal.JournalID { return c.anchor }

// Children returns bindings in submitted order as a fresh slice.
func (c OperationClosure) Children() []ChildBinding {
	return append([]ChildBinding(nil), c.children...)
}

// Root returns the sole root binding for a genesis closure.
func (c OperationClosure) Root() (ChildBinding, bool) {
	if c.kind != RequestKindGenesis || len(c.children) != 1 {
		return ChildBinding{}, false
	}
	return c.children[0], true
}

// Equal compares the persisted semantic identity and all stable row positions.
func (c OperationClosure) Equal(other OperationClosure) bool {
	if c.operationID != other.operationID || c.kind != other.kind || c.anchor != other.anchor || len(c.children) != len(other.children) {
		return false
	}
	for i := range c.children {
		if c.children[i] != other.children[i] {
			return false
		}
	}
	return true
}

// ComposedResult is the copy-safe receipt returned by a composed allocation.
// Closure is retained through an accessor so retry/recovery cannot expose a
// mutable slice. The supplemental journal result is intentionally represented
// as its useful stable bindings rather than as a writable CommittedResult.
type ComposedResult struct {
	closure       OperationClosure
	emittedEvents []journal.JournalID
	resultSlots   []journal.ResultSlotBinding
	replayed      bool
}

// WithComposedReplay returns a defensive copy carrying invocation-local replay
// metadata. The flag is deliberately omitted from composedResultWire: it
// describes this call, not the canonical durable receipt.
func WithComposedReplay(result ComposedResult, replayed bool) ComposedResult {
	copy := NewComposedResult(result.Closure(), journal.CommittedResult{
		EmittedEvents: result.SupplementalEmittedEvents(),
		ResultSlots:   result.SupplementalResultSlots(),
	})
	copy.replayed = replayed
	return copy
}

// Replayed reports whether this invocation reconstructed an existing composed
// operation or retrieved an already-completed workflow.
func (r ComposedResult) Replayed() bool { return r.replayed }

// NewComposedResult constructs a receipt from a governed closure and the
// canonical journal result. It defensively copies all result slices and pointed
// identities before storing them.
func NewComposedResult(closure OperationClosure, result journal.CommittedResult) ComposedResult {
	return ComposedResult{
		closure:       NewClosure(closure.OperationID(), closure.Kind(), closure.AnchorJournalID(), closure.Children()),
		emittedEvents: append([]journal.JournalID(nil), result.EmittedEvents...),
		resultSlots:   copyResultSlots(result.ResultSlots),
	}
}

// Closure returns a fresh immutable governed-allocation closure value.
func (r ComposedResult) Closure() OperationClosure {
	return NewClosure(r.closure.OperationID(), r.closure.Kind(), r.closure.AnchorJournalID(), r.closure.Children())
}

// SupplementalEmittedEvents returns the canonical journal task-event closure
// in JournalID order as a fresh slice.
func (r ComposedResult) SupplementalEmittedEvents() []journal.JournalID {
	return append([]journal.JournalID(nil), r.emittedEvents...)
}

// SupplementalResultSlots returns resolved canonical journal result-slot
// bindings as a fresh slice, including fresh pointed TaskID/ActivityID values.
func (r ComposedResult) SupplementalResultSlots() []journal.ResultSlotBinding {
	return copyResultSlots(r.resultSlots)
}

func copyResultSlots(in []journal.ResultSlotBinding) []journal.ResultSlotBinding {
	out := make([]journal.ResultSlotBinding, len(in))
	for i := range in {
		out[i] = in[i]
		if in[i].TaskID != nil {
			value := *in[i].TaskID
			out[i].TaskID = &value
		}
		if in[i].ActivityID != nil {
			value := *in[i].ActivityID
			out[i].ActivityID = &value
		}
	}
	return out
}

type closureWire struct {
	Version     int               `json:"version"`
	OperationID string            `json:"operationID"`
	Kind        RequestKind       `json:"kind"`
	Anchor      journal.JournalID `json:"anchor"`
	Children    []ChildBinding    `json:"children"`
}

// MarshalJSON preserves private closure fields when DBOS checkpoints a fused
// callback result. It does not expose a mutable internal slice.
func (c OperationClosure) MarshalJSON() ([]byte, error) {
	return json.Marshal(closureWire{
		Version:     1,
		OperationID: string(c.operationID),
		Kind:        c.kind,
		Anchor:      c.anchor,
		Children:    c.Children(),
	})
}

// UnmarshalJSON rejects unknown closure versions and invalid structural
// positions before replacing the receiver's immutable state.
func (c *OperationClosure) UnmarshalJSON(data []byte) error {
	var wire closureWire
	if err := decodeStrictResultWire(data, &wire); err != nil {
		return fmt.Errorf("decode governed operation closure: %w", err)
	}
	operationID := journal.OperationID(wire.OperationID)
	if wire.Version != 1 || !wire.Kind.valid() || journal.ValidateExternalOperationID(operationID) != nil || wire.Anchor <= 0 || (wire.Kind == RequestKindGenesis && len(wire.Children) != 1) || (wire.Kind == RequestKindAllocation && (len(wire.Children) < 1 || len(wire.Children) > MaxChildren)) {
		return errors.New("decode governed operation closure: unsupported or structurally incomplete closure; restore a valid version-1 operation output")
	}
	seenRows := make(map[journal.JournalID]struct{}, len(wire.Children)*2)
	for ordinal, child := range wire.Children {
		if child.Ordinal != ordinal || !validTaskID(child.TaskID) || !validAssignmentID(child.AssignmentID) || !validActorID(child.Occupant) || child.TaskRow.OperationID != operationID || child.TaskRow.EffectOrdinal != ordinal || child.TaskRow.Subordinal != 0 || child.TaskRow.JournalID <= 0 || child.AssignmentRow.OperationID != operationID || child.AssignmentRow.EffectOrdinal != ordinal || child.AssignmentRow.Subordinal != 1 || child.AssignmentRow.JournalID <= 0 || child.TaskRow.JournalID == child.AssignmentRow.JournalID {
			return errors.New("decode governed operation closure: child binding structural positions do not match the operation; restore a valid version-1 operation output")
		}
		for _, row := range []journal.JournalID{child.TaskRow.JournalID, child.AssignmentRow.JournalID} {
			if _, duplicate := seenRows[row]; duplicate {
				return errors.New("decode governed operation closure: duplicate produced journal row; restore a valid version-1 operation output")
			}
			seenRows[row] = struct{}{}
		}
	}
	*c = NewClosure(journal.OperationID(wire.OperationID), wire.Kind, wire.Anchor, wire.Children)
	return nil
}

type composedResultWire struct {
	Version       int                         `json:"version"`
	Closure       OperationClosure            `json:"closure"`
	EmittedEvents []journal.JournalID         `json:"emittedEvents"`
	ResultSlots   []journal.ResultSlotBinding `json:"resultSlots"`
}

// MarshalJSON preserves the private copy-safe state when DBOS checkpoints a
// fused composed result. The wire contains only copied values.
func (r ComposedResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(composedResultWire{
		Version:       1,
		Closure:       r.Closure(),
		EmittedEvents: r.SupplementalEmittedEvents(),
		ResultSlots:   r.SupplementalResultSlots(),
	})
}

// UnmarshalJSON validates each externally serialized slot before restoring
// private receipt state, so a corrupt DBOS output cannot become a replay result.
func (r *ComposedResult) UnmarshalJSON(data []byte) error {
	var wire composedResultWire
	if err := decodeStrictResultWire(data, &wire); err != nil {
		return fmt.Errorf("decode composed governed allocation result: %w", err)
	}
	if wire.Version != 1 || wire.Closure.OperationID() == "" {
		return errors.New("decode composed governed allocation result: unsupported or structurally incomplete version-1 result; restore a valid DBOS operation output")
	}
	for i, event := range wire.EmittedEvents {
		if event <= 0 || (i > 0 && wire.EmittedEvents[i-1] >= event) {
			return fmt.Errorf("decode composed governed allocation result: emitted event %d has non-positive JournalID", i)
		}
	}
	for i, binding := range wire.ResultSlots {
		if err := journal.ValidateResultSlotBinding(binding); err != nil {
			return fmt.Errorf("decode composed governed allocation result: result slot %d: %w", i, err)
		}
		if binding.ProducedJournalID <= 0 || (i > 0 && wire.ResultSlots[i-1].Slot >= binding.Slot) {
			return fmt.Errorf("decode composed governed allocation result: result slots are duplicated, unsorted, or have an invalid produced row at index %d", i)
		}
	}
	*r = ComposedResult{
		closure:       NewClosure(wire.Closure.OperationID(), wire.Closure.Kind(), wire.Closure.AnchorJournalID(), wire.Closure.Children()),
		emittedEvents: append([]journal.JournalID(nil), wire.EmittedEvents...),
		resultSlots:   copyResultSlots(wire.ResultSlots),
	}
	return nil
}

// decodeStrictResultWire is the private, bounded DBOS-result decoder. The
// standard decoder accepts duplicate keys, so first walk the token stream and
// reject ambiguous objects before decoding into the closed wire struct.
func decodeStrictResultWire(data []byte, out any) error {
	if len(data) == 0 || len(data) > maxResultWireBytes {
		return fmt.Errorf("result wire length %d is outside 1..%d bytes", len(data), maxResultWireBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	tokens := 0
	if err := inspectResultWireValue(decoder, 0, &tokens); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("result wire contains trailing JSON values")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("result wire contains trailing JSON")
	}
	return nil
}

func inspectResultWireValue(decoder *json.Decoder, depth int, tokens *int) error {
	if depth > maxResultWireDepth {
		return errors.New("result wire exceeds maximum nesting depth")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	*tokens++
	if *tokens > maxResultWireTokens {
		return errors.New("result wire exceeds maximum token count")
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("result wire object key is not a string")
			}
			if _, duplicate := keys[key]; duplicate {
				return fmt.Errorf("result wire contains duplicate field %q", key)
			}
			keys[key] = struct{}{}
			if err := inspectResultWireValue(decoder, depth+1, tokens); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := inspectResultWireValue(decoder, depth+1, tokens); err != nil {
				return err
			}
		}
	default:
		return errors.New("result wire has an unexpected closing delimiter")
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if closing != json.Delim(map[json.Delim]json.Delim{'{': '}', '[': ']'}[delim]) {
		return errors.New("result wire has mismatched delimiters")
	}
	return nil
}

func (kind RequestKind) valid() bool {
	return kind == RequestKindGenesis || kind == RequestKindAllocation
}

type canonicalWire struct {
	Version            int         `json:"version"`
	Kind               RequestKind `json:"kind"`
	OperationID        string      `json:"operationID"`
	ActorID            string      `json:"actorID"`
	Command            string      `json:"command"`
	ParentAssignmentID string      `json:"parentAssignmentID,omitempty"`
	Children           []childWire `json:"children"`
}

type composedCanonicalWire struct {
	Version               CompositionVersion  `json:"version"`
	Policy                SupplementPolicy    `json:"policy"`
	AllocationCanonical   []byte              `json:"allocationCanonical"`
	SupplementalCanonical []byte              `json:"supplementalCanonical"`
	ReferenceScope        *referenceScopeWire `json:"referenceScope,omitempty"`
}

type referenceScopeWire struct {
	Kind     ReferenceScopeKind `json:"kind,omitempty"`
	Subjects []string           `json:"subjects,omitempty"`
}

type childWire struct {
	TaskID       string `json:"taskID"`
	AssignmentID string `json:"assignmentID"`
	Occupant     string `json:"occupant"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Type         int    `json:"type"`
	Priority     int    `json:"priority"`
	Phase        int    `json:"phase"`
}

// CanonicalizeAllocation validates and encodes a request with the submitted
// child order preserved exactly. The returned SHA-256 is persisted with the
// bytes and checked again during retry reconstruction.
func CanonicalizeAllocation(request GovernedAllocationRequest) ([]byte, [sha256.Size]byte, error) {
	if err := validateCommon(request.OperationID, request.ActorID, request.Command); err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	if !validAssignmentID(request.ParentAssignmentID) {
		return nil, [sha256.Size]byte{}, validationError(request.OperationID, "ParentAssignmentID", "an exact non-empty parent assignment ID is required", "the reducer cannot establish delegated authority", "supply the currently active parent AssignmentID")
	}
	if len(request.Children) < 1 || len(request.Children) > MaxChildren {
		return nil, [sha256.Size]byte{}, validationError(request.OperationID, "Children", fmt.Sprintf("the batch contains %d children, outside the allowed 1..%d range", len(request.Children), MaxChildren), "nothing was written", "submit between one and 128 ordered child specifications")
	}
	if err := validateChildren(request.OperationID, request.Children); err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	return marshalCanonical(canonicalWire{
		Version:            1,
		Kind:               RequestKindAllocation,
		OperationID:        string(request.OperationID),
		ActorID:            request.ActorID.String(),
		Command:            request.Command,
		ParentAssignmentID: string(request.ParentAssignmentID),
		Children:           toChildWire(request.Children),
	})
}

// CanonicalizeComposed validates and encodes the full allocation-plus-effects
// request identity. The allocation child order and supplemental effect order are
// both represented verbatim in their canonical sub-receipts. A retry therefore
// cannot reuse an OperationID with a changed payload, order, or result slot.
func CanonicalizeComposed(request ComposedRequest) ([]byte, [sha256.Size]byte, error) {
	if request.Version != CompositionV1 {
		return nil, [sha256.Size]byte{}, validationError(request.Allocation.OperationID, "Version", "the composed allocation request has an unsupported version", "nothing was written", "use CompositionV1")
	}
	if len(request.SupplementalEffects) == 0 {
		return nil, [sha256.Size]byte{}, validationError(request.Allocation.OperationID, "SupplementalEffects", "CompositionV1 requires at least one supplemental effect", "the DBOS workflow and reducer were not entered", "use the simple governed allocation API for allocation-only work, or supply at least one supported supplemental effect")
	}
	scopeWire, err := canonicalReferenceScope(request.Allocation.OperationID, request.ReferenceScope)
	if err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	allocationCanonical, _, err := CanonicalizeAllocation(request.Allocation)
	if err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	for index, effect := range request.SupplementalEffects {
		if !allowedSupplementSort(effect.Sort) {
			return nil, [sha256.Size]byte{}, validationError(request.Allocation.OperationID, fmt.Sprintf("SupplementalEffects[%d].Sort", index), fmt.Sprintf("effect sort %s is not permitted by static %s", effect.Sort, SupplementPolicyV1), "the DBOS workflow and reducer were not entered", "use only EffectEvidence, EffectEdgeAdd, EffectTaskEvent, or EffectActivityCreate")
		}
		if effect.Sort == journal.EffectTaskEvent && journal.IsReducerDerivedTaskEventKind(effect.EventKind) {
			return nil, [sha256.Size]byte{}, validationError(request.Allocation.OperationID, fmt.Sprintf("SupplementalEffects[%d].EventKind", index), fmt.Sprintf("task event kind %q is reducer-derived and cannot be supplied as a generic supplemental task event", effect.EventKind), "the DBOS workflow and reducer were not entered", "use EffectEdgeAdd for relationship mutations, or a caller-domain task event kind for supplemental events")
		}
	}
	// Canonicalize is the existing semantic and result-slot validation boundary.
	// A synthetic non-zero authority is sufficient here because canonicalization
	// validates shape, while the exact parent authority is checked in the owned
	// transaction before any rows are inserted.
	authority := journal.JournalID(1)
	prepared, err := journal.Canonicalize(journal.OperationInput{
		OperationID:        GovernedAllocationSupplementOperationID(request.Allocation.OperationID),
		ActorID:            request.Allocation.ActorID,
		AuthorityJournalID: &authority,
		CommandDigest:      supplementalCommandDigest(allocationCanonical),
		Effects:            copyEffects(request.SupplementalEffects),
		Conditions:         copyConditions(request.Conditions),
	})
	if err != nil {
		return nil, [sha256.Size]byte{}, validationError(request.Allocation.OperationID, "SupplementalEffects", err.Error(), "the DBOS workflow and reducer were not entered", "supply canonical allowed effects with unique valid result slots")
	}
	return marshalComposedCanonical(composedCanonicalWire{
		Version:               CompositionV1,
		Policy:                SupplementPolicyV1,
		AllocationCanonical:   allocationCanonical,
		SupplementalCanonical: prepared.CanonicalBytes(),
		ReferenceScope:        scopeWire,
	})
}

func canonicalReferenceScope(operation journal.OperationID, scope ReferenceScope) (*referenceScopeWire, error) {
	if scope.Kind == 0 && len(scope.Subjects) == 0 {
		return nil, nil
	}
	if scope.Kind != ReferenceScopeDescendants || len(scope.Subjects) == 0 || len(scope.Subjects) > MaxChildren {
		return nil, validationError(operation, "ReferenceScope", "the reference scope must use ReferenceScopeDescendants with 1..128 explicit subjects", "the composed request was not admitted", "name only existing descendant tasks required by supplemental effects")
	}
	out := referenceScopeWire{Kind: scope.Kind, Subjects: make([]string, len(scope.Subjects))}
	seen := make(map[string]struct{}, len(scope.Subjects))
	for i, subject := range scope.Subjects {
		if !validTaskID(subject) {
			return nil, validationError(operation, fmt.Sprintf("ReferenceScope.Subjects[%d]", i), "the subject TaskID is invalid", "the composed request was not admitted", "supply a valid namespaced TaskID")
		}
		text := subject.String()
		if _, duplicate := seen[text]; duplicate {
			return nil, validationError(operation, fmt.Sprintf("ReferenceScope.Subjects[%d]", i), "the subject is duplicated", "the composed request was not admitted", "list each descendant subject exactly once")
		}
		seen[text] = struct{}{}
		out.Subjects[i] = text
	}
	return &out, nil
}

func allowedSupplementSort(sort journal.EffectSort) bool {
	switch sort {
	case journal.EffectEvidence, journal.EffectEdgeAdd, journal.EffectTaskEvent, journal.EffectActivityCreate:
		return true
	default:
		return false
	}
}

func marshalComposedCanonical(wire composedCanonicalWire) ([]byte, [sha256.Size]byte, error) {
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("encode canonical composed governed allocation: %w", err)
	}
	return encoded, sha256.Sum256(encoded), nil
}

// DecodeComposedRequest reconstructs a fully validated request from its exact
// canonical receipt. It rejects a reserialized or policy-widened representation
// before a recovered fused workflow can enter a transaction.
func DecodeComposedRequest(encoded []byte) (ComposedRequest, error) {
	var wire composedCanonicalWire
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return ComposedRequest{}, fmt.Errorf("decode canonical composed governed allocation: %w", err)
	}
	if wire.Version != CompositionV1 || wire.Policy != SupplementPolicyV1 || len(wire.AllocationCanonical) == 0 {
		return ComposedRequest{}, errors.New("decode canonical composed governed allocation: unsupported version, policy, or missing allocation receipt")
	}
	allocationRequest, err := DecodeAllocationRequest(wire.AllocationCanonical)
	if err != nil {
		return ComposedRequest{}, fmt.Errorf("decode canonical composed governed allocation allocation: %w", err)
	}
	prepared, err := journal.DecodeCanonicalMutation(wire.SupplementalCanonical)
	if err != nil {
		return ComposedRequest{}, fmt.Errorf("decode canonical composed governed allocation supplements: %w", err)
	}
	request := ComposedRequest{
		Version:             wire.Version,
		Allocation:          allocationRequest,
		SupplementalEffects: prepared.NormalizedEffects(),
		Conditions:          prepared.NormalizedConditions(),
	}
	if wire.ReferenceScope != nil {
		request.ReferenceScope.Kind = wire.ReferenceScope.Kind
		request.ReferenceScope.Subjects = make([]ptypes.TaskID, len(wire.ReferenceScope.Subjects))
		for i, raw := range wire.ReferenceScope.Subjects {
			task, parseErr := ptypes.ParseTaskID(raw)
			if parseErr != nil {
				return ComposedRequest{}, fmt.Errorf("decode canonical composed governed allocation reference subject %d: %w", i, parseErr)
			}
			request.ReferenceScope.Subjects[i] = task
		}
	}
	reserialized, _, err := CanonicalizeComposed(request)
	if err != nil || !bytes.Equal(reserialized, encoded) {
		return ComposedRequest{}, errors.New("decode canonical composed governed allocation: stored bytes are not canonical; restore the matching allocation and supplemental receipt")
	}
	return request, nil
}

// SupplementalOperation prepares the one internal canonical journal operation
// that reduces a composition's supplemental effects. Its OperationID is a
// deterministic SHA-256 derivation with a fixed domain separator; it cannot be
// supplied by callers and is not a second external operation identity.
func SupplementalOperation(request ComposedRequest, authority journal.JournalID) (journal.OperationInput, journal.CanonicalMutation, error) {
	allocationCanonical, _, err := CanonicalizeAllocation(request.Allocation)
	if err != nil {
		return journal.OperationInput{}, journal.CanonicalMutation{}, err
	}
	input := journal.OperationInput{
		OperationID:        GovernedAllocationSupplementOperationID(request.Allocation.OperationID),
		ActorID:            request.Allocation.ActorID,
		AuthorityJournalID: &authority,
		CommandDigest:      supplementalCommandDigest(allocationCanonical),
		Effects:            copyEffects(request.SupplementalEffects),
		Conditions:         copyConditions(request.Conditions),
	}
	prepared, err := journal.Canonicalize(input)
	if err != nil {
		return journal.OperationInput{}, journal.CanonicalMutation{}, err
	}
	input.Effects = prepared.NormalizedEffects()
	input.Conditions = prepared.NormalizedConditions()
	input.MutationDigest = prepared.DerivedDigest()
	return input, prepared, nil
}

func copyConditions(in []journal.Condition) []journal.Condition {
	if len(in) == 0 {
		return nil
	}
	prepared, err := journal.Canonicalize(journal.OperationInput{Conditions: in})
	if err != nil {
		return append([]journal.Condition(nil), in...)
	}
	return prepared.NormalizedConditions()
}

func supplementalCommandDigest(allocationCanonical []byte) []byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("provenance.governed-allocation.supplement.command.v1\x00"))
	_, _ = hash.Write(allocationCanonical)
	return hash.Sum(nil)
}

func copyEffects(in []journal.Effect) []journal.Effect {
	// Canonicalize returns independently allocated normalized effects. This first
	// pass prevents a caller changing a payload slice while the request is being
	// prepared from altering the persisted identity.
	prepared, err := journal.Canonicalize(journal.OperationInput{Effects: in})
	if err == nil {
		return prepared.NormalizedEffects()
	}
	// The caller will receive the canonical validation error in the primary path;
	// use a minimal deep copy here solely to avoid retaining its mutable bytes.
	out := make([]journal.Effect, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Payload = append([]byte(nil), in[i].Payload...)
		out[i].ContentDigest = append([]byte(nil), in[i].ContentDigest...)
		out[i].Contexts = append([]journal.EventContext(nil), in[i].Contexts...)
		if in[i].RecordedAtOverride != nil {
			value := *in[i].RecordedAtOverride
			out[i].RecordedAtOverride = &value
		}
	}
	return out
}

// CanonicalizeGenesis validates and encodes the exact one-root request.
func CanonicalizeGenesis(request RootGenesisRequest) ([]byte, [sha256.Size]byte, error) {
	if err := validateCommon(request.OperationID, request.ActorID, request.Command); err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	if err := validateChildren(request.OperationID, []ChildSpec{request.Root}); err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	return marshalCanonical(canonicalWire{
		Version:     1,
		Kind:        RequestKindGenesis,
		OperationID: string(request.OperationID),
		ActorID:     request.ActorID.String(),
		Command:     request.Command,
		Children:    toChildWire([]ChildSpec{request.Root}),
	})
}

func marshalCanonical(wire canonicalWire) ([]byte, [sha256.Size]byte, error) {
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("encode canonical governed allocation: %w", err)
	}
	return encoded, sha256.Sum256(encoded), nil
}

// decodedCanonical is the validated, complete governed request encoded in one
// canonical receipt. Keeping Command here is important for DBOS recovery: a
// recovered workflow must replay the same complete request, not a projection
// that has silently lost its caller-stable command identity.
type decodedCanonical struct {
	kind        RequestKind
	operationID journal.OperationID
	actorID     ptypes.ActorID
	command     string
	parentID    journal.AssignmentID
	children    []ChildSpec
}

// DecodeCanonical validates the stored canonical request's version, digest
// shape, and canonical byte representation. It returns the original ordered
// child specifications without accepting unknown or reserialized forms.
func DecodeCanonical(encoded []byte) (RequestKind, journal.OperationID, ptypes.ActorID, journal.AssignmentID, []ChildSpec, error) {
	decoded, err := decodeCanonical(encoded)
	if err != nil {
		return 0, "", ptypes.ActorID{}, "", nil, err
	}
	return decoded.kind, decoded.operationID, decoded.actorID, decoded.parentID, decoded.children, nil
}

// DecodeAllocationRequest reconstructs one complete allocation request from
// its exact canonical bytes. It is used by the fused workflow recovery path so
// the bytes compared to DBOS durability are the same bytes given to the
// transaction-scoped reducer.
func DecodeAllocationRequest(encoded []byte) (GovernedAllocationRequest, error) {
	decoded, err := decodeCanonical(encoded)
	if err != nil {
		return GovernedAllocationRequest{}, err
	}
	if decoded.kind != RequestKindAllocation {
		return GovernedAllocationRequest{}, errors.New("decode canonical governed allocation: request kind is not allocation")
	}
	return GovernedAllocationRequest{
		OperationID:        decoded.operationID,
		ActorID:            decoded.actorID,
		Command:            decoded.command,
		ParentAssignmentID: decoded.parentID,
		Children:           decoded.children,
	}, nil
}

// DecodeGenesisRequest reconstructs one complete root request from its exact
// canonical bytes for the fused workflow recovery path.
func DecodeGenesisRequest(encoded []byte) (RootGenesisRequest, error) {
	decoded, err := decodeCanonical(encoded)
	if err != nil {
		return RootGenesisRequest{}, err
	}
	if decoded.kind != RequestKindGenesis || len(decoded.children) != 1 {
		return RootGenesisRequest{}, errors.New("decode canonical governed genesis: request is not one root")
	}
	return RootGenesisRequest{
		OperationID: decoded.operationID,
		ActorID:     decoded.actorID,
		Command:     decoded.command,
		Root:        decoded.children[0],
	}, nil
}

func decodeCanonical(encoded []byte) (decodedCanonical, error) {
	var wire canonicalWire
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return decodedCanonical{}, fmt.Errorf("decode canonical governed allocation: %w", err)
	}
	if wire.Version != 1 || !wire.Kind.valid() {
		return decodedCanonical{}, errors.New("decode canonical governed allocation: unsupported version or request kind")
	}
	if (wire.Kind == RequestKindGenesis && len(wire.Children) != 1) || (wire.Kind == RequestKindAllocation && (len(wire.Children) < 1 || len(wire.Children) > MaxChildren)) {
		return decodedCanonical{}, errors.New("decode canonical governed allocation: request kind has an invalid child count")
	}
	actor, err := ptypes.ParseActorID(wire.ActorID)
	if err != nil {
		return decodedCanonical{}, fmt.Errorf("decode canonical governed allocation actor: %w", err)
	}
	children := make([]ChildSpec, len(wire.Children))
	for i, child := range wire.Children {
		task, err := ptypes.ParseTaskID(child.TaskID)
		if err != nil {
			return decodedCanonical{}, fmt.Errorf("decode canonical governed allocation child %d task: %w", i, err)
		}
		occupant, err := ptypes.ParseActorID(child.Occupant)
		if err != nil {
			return decodedCanonical{}, fmt.Errorf("decode canonical governed allocation child %d occupant: %w", i, err)
		}
		children[i] = ChildSpec{TaskID: task, AssignmentID: journal.AssignmentID(child.AssignmentID), Occupant: occupant, Title: child.Title, Description: child.Description, Type: ptypes.TaskType(child.Type), Priority: ptypes.Priority(child.Priority), Phase: ptypes.Phase(child.Phase)}
	}
	if wire.Kind == RequestKindGenesis {
		_, _, err = CanonicalizeGenesis(RootGenesisRequest{OperationID: journal.OperationID(wire.OperationID), ActorID: actor, Command: wire.Command, Root: children[0]})
	} else {
		_, _, err = CanonicalizeAllocation(GovernedAllocationRequest{OperationID: journal.OperationID(wire.OperationID), ActorID: actor, Command: wire.Command, ParentAssignmentID: journal.AssignmentID(wire.ParentAssignmentID), Children: children})
	}
	if err != nil {
		return decodedCanonical{}, fmt.Errorf("validate decoded canonical governed allocation: %w", err)
	}
	reserialized, _, err := marshalCanonical(wire)
	if err != nil || string(reserialized) != string(encoded) {
		return decodedCanonical{}, errors.New("decode canonical governed allocation: stored bytes are not canonical; restore the operation from a matching backup")
	}
	return decodedCanonical{
		kind:        wire.Kind,
		operationID: journal.OperationID(wire.OperationID),
		actorID:     actor,
		command:     wire.Command,
		parentID:    journal.AssignmentID(wire.ParentAssignmentID),
		children:    children,
	}, nil
}

func validateCommon(operationID journal.OperationID, actor ptypes.ActorID, command string) error {
	if err := journal.ValidateExternalOperationID(operationID); err != nil {
		return validationError(operationID, "OperationID", err.Error(), "nothing was written", "supply a valid stable OperationID")
	}
	if !validActorID(actor) {
		return validationError(operationID, "ActorID", "a non-zero namespaced actor identity is required", "the allocation cannot be attributed", "supply a valid registered ActorID")
	}
	if command == "" || len(command) > maxCommandBytes {
		return validationError(operationID, "Command", fmt.Sprintf("command identity must contain 1..%d bytes", maxCommandBytes), "nothing was written", "supply a stable bounded command identity")
	}
	return nil
}

func validateChildren(operationID journal.OperationID, children []ChildSpec) error {
	tasks := make(map[ptypes.TaskID]struct{}, len(children))
	assignments := make(map[journal.AssignmentID]struct{}, len(children))
	for i, child := range children {
		if !validTaskID(child.TaskID) {
			return validationError(operationID, fmt.Sprintf("Children[%d].TaskID", i), "a non-zero namespaced task identity is required", "nothing was written", "supply a new valid caller TaskID")
		}
		if !validAssignmentID(child.AssignmentID) {
			return validationError(operationID, fmt.Sprintf("Children[%d].AssignmentID", i), "an assignment ID must contain 1..256 bytes", "nothing was written", "supply a stable new child AssignmentID")
		}
		if !validActorID(child.Occupant) {
			return validationError(operationID, fmt.Sprintf("Children[%d].Occupant", i), "a non-zero namespaced occupant identity is required", "nothing was written", "supply a registered occupant ActorID")
		}
		if strings.TrimSpace(child.Title) == "" || len(child.Title) > maxTitleBytes || len(child.Description) > maxDescriptionBytes || !child.Type.IsValid() || !child.Priority.IsValid() || !child.Phase.IsValid() {
			return validationError(operationID, fmt.Sprintf("Children[%d] metadata", i), "title, description, type, priority, or phase is invalid", "nothing was written", "supply a non-blank bounded title, bounded description, and valid task metadata enums")
		}
		if _, duplicate := tasks[child.TaskID]; duplicate {
			return validationError(operationID, fmt.Sprintf("Children[%d].TaskID", i), "the request repeats a child TaskID", "nothing was written", "give every child a unique TaskID")
		}
		if _, duplicate := assignments[child.AssignmentID]; duplicate {
			return validationError(operationID, fmt.Sprintf("Children[%d].AssignmentID", i), "the request repeats a child AssignmentID", "nothing was written", "give every child a unique AssignmentID")
		}
		tasks[child.TaskID] = struct{}{}
		assignments[child.AssignmentID] = struct{}{}
	}
	return nil
}

func validationError(operationID journal.OperationID, field, why, impact, fix string) error {
	return NewError(ErrorValidation, operationID, "canonical request validation: "+field, why, impact, fix, nil)
}

func validTaskID(id ptypes.TaskID) bool {
	return id.Namespace != "" && id.UUID != uuid.Nil
}

func validActorID(id ptypes.ActorID) bool {
	return id.Namespace != "" && id.UUID != uuid.Nil
}

func validAssignmentID(id journal.AssignmentID) bool {
	return len(id) > 0 && len(id) <= maxAssignmentIDBytes
}

func toChildWire(children []ChildSpec) []childWire {
	encoded := make([]childWire, len(children))
	for i, child := range children {
		encoded[i] = childWire{TaskID: child.TaskID.String(), AssignmentID: string(child.AssignmentID), Occupant: child.Occupant.String(), Title: child.Title, Description: child.Description, Type: int(child.Type), Priority: int(child.Priority), Phase: int(child.Phase)}
	}
	return encoded
}
