package journal

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// Shared type aliases into the public ptypes domain. Kept so journal code and
// its callers refer to one identity domain.
type (
	Task       = ptypes.Task
	Status     = ptypes.Status
	Phase      = ptypes.Phase
	Stage      = ptypes.Stage
	TaskID     = ptypes.TaskID
	EdgeKind   = ptypes.EdgeKind
	CommentID  = ptypes.CommentID
	ActorID    = ptypes.ActorID
	ActivityID = ptypes.ActivityID
)

// EventContextKind identifies one context identity domain. Built-in kinds
// (task/activity/actor/git) are interpreted by the reducer for identity
// validation; caller-extension namespaces are recorded opaquely
// (docs/journal-relational-contract.md §5.2).
type EventContextKind string

// GitOID is a distinct Git object-identity domain accepting canonical
// lower-case SHA-1 (40 hex) and SHA-256 (64 hex) object IDs.
type GitOID string

const (
	EventContextKindTask     EventContextKind = "task"
	EventContextKindActivity EventContextKind = "activity"
	EventContextKindActor    EventContextKind = "actor"
	EventContextKindGit      EventContextKind = "git"
)

// EventContext is an opaque, immutable kind plus canonical encoded identity.
// Values can only be constructed through the typed built-in constructors or a
// validated ExtensionContextDescriptor, so a raw kind/identity pair can never
// bypass validation.
type EventContext struct {
	kind     EventContextKind
	identity string
}

// Kind returns the context identity domain.
func (c EventContext) Kind() EventContextKind { return c.kind }

// MarshalJSON provides the stable read-only query representation.
func (c EventContext) MarshalJSON() ([]byte, error) {
	if err := validateEventContext(c); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Kind     EventContextKind `json:"kind"`
		Identity string           `json:"identity"`
	}{Kind: c.kind, Identity: c.identity})
}

// TaskContext constructs a validated task-domain context.
func TaskContext(id TaskID) (EventContext, error) {
	if err := validateTaskID(id); err != nil {
		return EventContext{}, fmt.Errorf("TaskContext: %w", err)
	}
	return EventContext{kind: EventContextKindTask, identity: id.String()}, nil
}

// ActivityContext constructs a validated activity-domain context.
func ActivityContext(id ActivityID) (EventContext, error) {
	if err := validateActivityID(id); err != nil {
		return EventContext{}, fmt.Errorf("ActivityContext: %w", err)
	}
	return EventContext{kind: EventContextKindActivity, identity: id.String()}, nil
}

// ActorContext constructs a validated actor-domain context.
func ActorContext(id ActorID) (EventContext, error) {
	if err := validateActorID(id); err != nil {
		return EventContext{}, fmt.Errorf("ActorContext: %w", err)
	}
	return EventContext{kind: EventContextKindActor, identity: id.String()}, nil
}

// GitContext constructs a validated Git object-domain context.
func GitContext(id GitOID) (EventContext, error) {
	if err := validateGitOID(id); err != nil {
		return EventContext{}, fmt.Errorf("GitContext: %w", err)
	}
	return EventContext{kind: EventContextKindGit, identity: string(id)}, nil
}

// ExtensionContextID is implemented by a distinct caller-owned string-backed
// identity. The marker declares its domain, so constructors accept no redundant
// raw kind argument. The built-in string type intentionally does not satisfy it.
type ExtensionContextID interface {
	~string
	EventContextDomain() EventContextKind
}

// ExtensionContextDescriptor is an opaque, typed validator for one caller-owned
// context identity domain.
type ExtensionContextDescriptor[ID ExtensionContextID] struct {
	kind     EventContextKind
	validate func(ID) error
}

// DefineExtensionContext creates a descriptor whose kind is derived solely from
// ID's marker method.
func DefineExtensionContext[ID ExtensionContextID](validate func(ID) error) (ExtensionContextDescriptor[ID], error) {
	var zero ID
	kind := zero.EventContextDomain()
	if err := validateExtensionContextKind(kind); err != nil {
		return ExtensionContextDescriptor[ID]{}, fmt.Errorf("DefineExtensionContext: %w", err)
	}
	if validate == nil {
		return ExtensionContextDescriptor[ID]{}, fmt.Errorf("DefineExtensionContext: validator is required")
	}
	return ExtensionContextDescriptor[ID]{kind: kind, validate: validate}, nil
}

// ExtensionContext validates and constructs one caller-owned context value.
func ExtensionContext[ID ExtensionContextID](descriptor ExtensionContextDescriptor[ID], id ID) (EventContext, error) {
	if descriptor.validate == nil {
		return EventContext{}, fmt.Errorf("ExtensionContext: zero or invalid descriptor")
	}
	if got := id.EventContextDomain(); got != descriptor.kind {
		return EventContext{}, fmt.Errorf("ExtensionContext: ID reports context kind %q, descriptor requires %q", got, descriptor.kind)
	}
	if err := validateExtensionContextKind(descriptor.kind); err != nil {
		return EventContext{}, fmt.Errorf("ExtensionContext: %w", err)
	}
	if err := descriptor.validate(id); err != nil {
		return EventContext{}, fmt.Errorf("ExtensionContext: invalid %q identity: %w", descriptor.kind, err)
	}
	if string(id) == "" {
		return EventContext{}, fmt.Errorf("ExtensionContext: %q identity is empty", descriptor.kind)
	}
	return EventContext{kind: descriptor.kind, identity: string(id)}, nil
}

// CanonicalEventContexts validates, deduplicates, and sorts a context set by
// (EventContextKind, encoded identity) lexical order. The returned slice never
// aliases input. Behavior is carried forward unchanged from the salvage
// CanonicalEventContexts (docs/journal-relational-contract.md §5.2).
func CanonicalEventContexts(contexts []EventContext) ([]EventContext, error) {
	if len(contexts) == 0 {
		return []EventContext{}, nil
	}
	canonical := append([]EventContext(nil), contexts...)
	for _, context := range canonical {
		if err := validateEventContext(context); err != nil {
			return nil, err
		}
	}
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].kind != canonical[j].kind {
			return canonical[i].kind < canonical[j].kind
		}
		return canonical[i].identity < canonical[j].identity
	})
	out := canonical[:0]
	for _, context := range canonical {
		if len(out) == 0 || out[len(out)-1] != context {
			out = append(out, context)
		}
	}
	return out, nil
}

// DecodeStoredEventContext is the persistence-only inverse of the typed public
// constructors. This package is internal so callers cannot use it as a raw
// EventContext construction bypass.
func DecodeStoredEventContext(kind EventContextKind, identity string) (EventContext, error) {
	context := EventContext{kind: kind, identity: identity}
	if err := validateEventContext(context); err != nil {
		return EventContext{}, err
	}
	return context, nil
}

// EncodeStoredEventContext is the internal persistence codec. EventContext's
// encoded identity is intentionally not exposed through the public package.
func EncodeStoredEventContext(context EventContext) (EventContextKind, string, error) {
	if err := validateEventContext(context); err != nil {
		return "", "", err
	}
	return context.kind, context.identity, nil
}

func validateEventContext(context EventContext) error {
	if context.identity == "" {
		return fmt.Errorf("invalid %q event context: identity is empty", context.kind)
	}
	switch context.kind {
	case EventContextKindTask:
		id, err := ptypes.ParseTaskID(context.identity)
		if err != nil {
			return fmt.Errorf("invalid task event context: %w", err)
		}
		return validateTaskID(id)
	case EventContextKindActivity:
		id, err := ptypes.ParseActivityID(context.identity)
		if err != nil {
			return fmt.Errorf("invalid activity event context: %w", err)
		}
		return validateActivityID(id)
	case EventContextKindActor:
		id, err := ptypes.ParseActorID(context.identity)
		if err != nil {
			return fmt.Errorf("invalid actor event context: %w", err)
		}
		return validateActorID(id)
	case EventContextKindGit:
		return validateGitOID(GitOID(context.identity))
	default:
		return validateExtensionContextKind(context.kind)
	}
}

func validateExtensionContextKind(kind EventContextKind) error {
	if kind == EventContextKindTask || kind == EventContextKindActivity || kind == EventContextKindActor || kind == EventContextKindGit {
		return fmt.Errorf("context kind %q is reserved by Provenance", kind)
	}
	if err := validateNamespacedName(string(kind)); err != nil {
		return fmt.Errorf("invalid extension context kind %q: %w", kind, err)
	}
	if strings.Split(string(kind), ".")[0] == "provenance" {
		return fmt.Errorf("context kind %q is in the reserved Provenance namespace", kind)
	}
	return nil
}

func validateNamespacedName(name string) error {
	parts := strings.Split(name, ".")
	if len(parts) < 2 {
		return fmt.Errorf("must contain a caller or Provenance namespace")
	}
	for _, part := range parts {
		if part == "" || part[0] < 'a' || part[0] > 'z' {
			return fmt.Errorf("components must start with a lower-case ASCII letter")
		}
		for i := 1; i < len(part); i++ {
			c := part[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return fmt.Errorf("component %q contains invalid character %q", part, c)
			}
		}
	}
	return nil
}

func validateTaskID(id TaskID) error {
	if id.Namespace == "" || id.UUID == uuid.Nil {
		return fmt.Errorf("%w: task ID must have a namespace and non-zero UUID", ptypes.ErrInvalidID)
	}
	parsed, err := ptypes.ParseTaskID(id.String())
	if err != nil || parsed != id {
		return fmt.Errorf("%w: invalid task ID", ptypes.ErrInvalidID)
	}
	return nil
}

func validateActorID(id ActorID) error {
	if id.Namespace == "" || id.UUID == uuid.Nil {
		return fmt.Errorf("%w: actor ID must have a namespace and non-zero UUID", ptypes.ErrInvalidID)
	}
	parsed, err := ptypes.ParseActorID(id.String())
	if err != nil || parsed != id {
		return fmt.Errorf("%w: invalid actor ID", ptypes.ErrInvalidID)
	}
	return nil
}

func validateActivityID(id ActivityID) error {
	if id.Namespace == "" || id.UUID == uuid.Nil {
		return fmt.Errorf("%w: activity ID must have a namespace and non-zero UUID", ptypes.ErrInvalidID)
	}
	parsed, err := ptypes.ParseActivityID(id.String())
	if err != nil || parsed != id {
		return fmt.Errorf("%w: invalid activity ID", ptypes.ErrInvalidID)
	}
	return nil
}

func validateGitOID(id GitOID) error {
	s := string(id)
	if len(s) != 40 && len(s) != 64 {
		return fmt.Errorf("invalid Git OID %q: want 40 or 64 lower-case hexadecimal characters", id)
	}
	decoded, err := hex.DecodeString(s)
	if err != nil || strings.ToLower(s) != s || bytes.Equal(decoded, make([]byte, len(decoded))) {
		return fmt.Errorf("invalid Git OID %q: want non-zero lower-case hexadecimal", id)
	}
	return nil
}
