// Package allocation defines the closed model and codec for governed task
// allocation. It deliberately depends only on the stable public identity types
// and the journal identity aliases, so both the standalone Modernc path and the
// DBOS fused path execute the same reducer.
package allocation

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

const (
	// MaxChildren is the largest governed batch accepted by the MVP contract.
	MaxChildren = 128
	// MaxAuthorityDepth includes the root assignment.
	MaxAuthorityDepth = 64

	maxCommandBytes      = 1024
	maxAssignmentIDBytes = 256
	maxTitleBytes        = 4096
	maxDescriptionBytes  = 64 << 10
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
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("decode governed operation closure: %w", err)
	}
	if wire.Version != 1 || !wire.Kind.valid() || wire.OperationID == "" || wire.Anchor == 0 || len(wire.Children) == 0 {
		return errors.New("decode governed operation closure: unsupported or structurally incomplete closure; restore a valid version-1 operation output")
	}
	for ordinal, child := range wire.Children {
		if child.Ordinal != ordinal || child.TaskRow.OperationID != journal.OperationID(wire.OperationID) || child.TaskRow.EffectOrdinal != ordinal || child.TaskRow.Subordinal != 0 || child.TaskRow.JournalID == 0 || child.AssignmentRow.OperationID != journal.OperationID(wire.OperationID) || child.AssignmentRow.EffectOrdinal != ordinal || child.AssignmentRow.Subordinal != 1 || child.AssignmentRow.JournalID == 0 {
			return errors.New("decode governed operation closure: child binding structural positions do not match the operation; restore a valid version-1 operation output")
		}
	}
	*c = NewClosure(journal.OperationID(wire.OperationID), wire.Kind, wire.Anchor, wire.Children)
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
	if err := journal.ValidateOperationID(operationID); err != nil {
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
