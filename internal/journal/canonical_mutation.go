package journal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// MutationEncodingV1 is the stable wire-version tag for canonical mutations.
const MutationEncodingV1 = "provenance.mutation.v1"

const (
	MaxCanonicalEffects           = 256
	MaxCanonicalContextsPerEffect = 64
	MaxCanonicalFieldBytes        = 1 << 20
	MaxCanonicalMutationBytes     = 8 << 20
)

var ErrCanonicalMutation = errors.New("provenance: invalid canonical mutation")

type CanonicalMutationError struct{ Field, Reason, Fix string }

func (e *CanonicalMutationError) Error() string {
	return fmt.Sprintf("%v: field %s: %s — where: canonical mutation boundary; when: before allocation or write; impact: mutation is rejected without side effects; fix: %s", ErrCanonicalMutation, e.Field, e.Reason, e.Fix)
}
func (e *CanonicalMutationError) Is(target error) bool { return target == ErrCanonicalMutation }
func canonicalMutationError(field, reason, fix string) error {
	return &CanonicalMutationError{Field: field, Reason: reason, Fix: fix}
}

// CanonicalMutation is the prepared mutation consumed by persistence and execution.
// Bytes are authoritative: Effects are decoded from Bytes rather than retained from
// the caller, and Digest is SHA-256(Bytes).
type CanonicalMutation struct {
	version string
	bytes   []byte
	digest  []byte
	effects []Effect
}

func (m CanonicalMutation) EncodingVersion() string { return m.version }
func (m CanonicalMutation) CanonicalBytes() []byte  { return append([]byte(nil), m.bytes...) }
func (m CanonicalMutation) DerivedDigest() []byte   { return append([]byte(nil), m.digest...) }
func (m CanonicalMutation) NormalizedEffects() []Effect {
	out := make([]Effect, len(m.effects))
	for i := range m.effects {
		out[i] = cloneCanonicalEffect(m.effects[i])
	}
	return out
}
func cloneCanonicalEffect(e Effect) Effect {
	e.Payload = append(json.RawMessage(nil), e.Payload...)
	e.Contexts = append([]EventContext(nil), e.Contexts...)
	e.ContentDigest = append([]byte(nil), e.ContentDigest...)
	if e.RecordedAtOverride != nil {
		v := *e.RecordedAtOverride
		e.RecordedAtOverride = &v
	}
	if e.UpdateTitle != nil {
		v := *e.UpdateTitle
		e.UpdateTitle = &v
	}
	if e.UpdateDescription != nil {
		v := *e.UpdateDescription
		e.UpdateDescription = &v
	}
	if e.UpdatePriority != nil {
		v := *e.UpdatePriority
		e.UpdatePriority = &v
	}
	if e.UpdatePhase != nil {
		v := *e.UpdatePhase
		e.UpdatePhase = &v
	}
	if e.UpdateNotes != nil {
		v := *e.UpdateNotes
		e.UpdateNotes = &v
	}
	return e
}

var canonicalEffectSorts = []EffectSort{
	EffectTaskEvent, EffectBootstrapAuthority, EffectAssignmentStart,
	EffectAssignmentEnd, EffectDecision, EffectEvidence, EffectTaskCreate,
	EffectEdgeAdd, EffectEdgeRemove, EffectLabelAdd, EffectLabelRemove,
	EffectCommentAdd, EffectTaskCreateAllocated,
}

// PrepareMutationV1 validates and normalizes effects, writes their canonical bytes,
// then decodes those bytes once. The returned decoded effects are the only effects a
// write path should execute.
func PrepareMutationV1(effects []Effect) (CanonicalMutation, error) {
	if len(effects) > MaxCanonicalEffects {
		return CanonicalMutation{}, canonicalMutationError("effect-count", fmt.Sprintf("%d exceeds maximum %d", len(effects), MaxCanonicalEffects), "split the operation into bounded mutations")
	}
	normalized := make([]Effect, len(effects))
	counter := &canonicalSizeCounter{limit: MaxCanonicalMutationBytes}
	w := canonicalWriter{w: counter}
	writeCanonicalEnvelopeHeader(&w, len(effects))
	for i := range effects {
		if err := validateRawCanonicalEffectBounds(effects[i], i); err != nil {
			return CanonicalMutation{}, err
		}
		var err error
		normalized[i], err = normalizeCanonicalEffect(effects[i], i)
		if err != nil {
			return CanonicalMutation{}, err
		}
		if err := encodeCanonicalEffect(&w, normalized[i], i); err != nil {
			return CanonicalMutation{}, err
		}
	}
	if w.err != nil {
		return CanonicalMutation{}, w.err
	}
	var out bytes.Buffer
	out.Grow(counter.size)
	w = canonicalWriter{w: &out}
	writeCanonicalEnvelopeHeader(&w, len(normalized))
	for i := range normalized {
		if err := encodeCanonicalEffect(&w, normalized[i], i); err != nil {
			return CanonicalMutation{}, err
		}
	}
	if w.err != nil {
		return CanonicalMutation{}, fmt.Errorf("provenance: encode bounded canonical mutation: %w", w.err)
	}
	return DecodeCanonicalMutation(out.Bytes())
}

func writeCanonicalEnvelopeHeader(w *canonicalWriter, effectCount int) {
	w.field("version", []byte(MutationEncodingV1))
	w.field("effect-count", []byte(strconv.Itoa(effectCount)))
}

type canonicalSizeCounter struct{ size, limit int }

func (w *canonicalSizeCounter) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.size {
		return 0, canonicalMutationError("mutation", fmt.Sprintf("exact framed size exceeds maximum %d at byte %d before canonical byte allocation", w.limit, w.size+len(p)), "reduce operands or split the operation")
	}
	w.size += len(p)
	return len(p), nil
}

// DecodeCanonicalMutation strictly decodes one complete canonical mutation. Field
// order is fixed, so unknown, missing, duplicate, and trailing fields all fail closed.
func DecodeCanonicalMutation(data []byte) (CanonicalMutation, error) {
	mutation, err := decodeCanonicalMutation(data)
	if err == nil {
		return mutation, nil
	}
	var typed *CanonicalMutationError
	if errors.As(err, &typed) {
		return CanonicalMutation{}, err
	}
	return CanonicalMutation{}, canonicalMutationError("wire", err.Error(), "restore bytes produced by a registered canonical codec with complete ordered fields")
}

func decodeCanonicalMutation(data []byte) (CanonicalMutation, error) {
	if len(data) > MaxCanonicalMutationBytes {
		return CanonicalMutation{}, canonicalMutationError("mutation", fmt.Sprintf("%d bytes exceeds maximum %d", len(data), MaxCanonicalMutationBytes), "restore bounded canonical bytes")
	}
	r := canonicalReader{r: bufio.NewReader(bytes.NewReader(data))}
	version, err := r.field("version")
	if err != nil {
		return CanonicalMutation{}, err
	}
	decoder, ok := canonicalMutationDecoderFor(string(version))
	if !ok {
		return CanonicalMutation{}, fmt.Errorf("unsupported encoding version %q", version)
	}
	return decoder(&r, data)
}

type canonicalMutationDecoder func(*canonicalReader, []byte) (CanonicalMutation, error)

func canonicalMutationDecoderFor(version string) (canonicalMutationDecoder, bool) {
	switch version {
	case MutationEncodingV1:
		return decodeCanonicalMutationV1, true
	default:
		return nil, false
	}
}

func decodeCanonicalMutationV1(r *canonicalReader, data []byte) (CanonicalMutation, error) {
	rawCount, err := r.field("effect-count")
	if err != nil {
		return CanonicalMutation{}, err
	}
	count, err := strconv.Atoi(string(rawCount))
	if err != nil || count < 0 || count > MaxCanonicalEffects {
		return CanonicalMutation{}, fmt.Errorf("provenance: decode canonical mutation: invalid effect-count %q", rawCount)
	}
	effects := make([]Effect, count)
	for i := range effects {
		effects[i], err = decodeCanonicalEffect(r, i)
		if err != nil {
			return CanonicalMutation{}, err
		}
	}
	if b, err := r.r.ReadByte(); err != io.EOF {
		if err == nil {
			return CanonicalMutation{}, fmt.Errorf("provenance: decode canonical mutation: trailing data begins with byte %q", b)
		}
		return CanonicalMutation{}, fmt.Errorf("provenance: decode canonical mutation trailing data: %w", err)
	}
	digest := sha256.Sum256(data)
	return CanonicalMutation{
		version: MutationEncodingV1,
		bytes:   append([]byte(nil), data...),
		digest:  append([]byte(nil), digest[:]...),
		effects: effects,
	}, nil
}

// IsSupportedMutationEncoding is the single codec-version registry used by
// persistence and wire decoding. SQL enforces only structural NULL/nonempty facts.
func IsSupportedMutationEncoding(version string) bool {
	_, ok := canonicalMutationDecoderFor(version)
	return ok
}

type canonicalWriter struct {
	w   io.Writer
	err error
}

func (w *canonicalWriter) field(name string, value []byte) {
	if w.err != nil {
		return
	}
	if len(value) > MaxCanonicalFieldBytes {
		w.err = canonicalMutationError(name, fmt.Sprintf("%d bytes exceeds maximum %d", len(value), MaxCanonicalFieldBytes), "reduce this operand")
		return
	}
	_, w.err = fmt.Fprintf(w.w, "%s:%d:", name, len(value))
	if w.err == nil {
		_, w.err = w.w.Write(value)
	}
	if w.err == nil {
		_, w.err = w.w.Write([]byte{'\n'})
	}
}

type canonicalReader struct{ r *bufio.Reader }

func (r *canonicalReader) field(want string) ([]byte, error) {
	name, err := r.r.ReadString(':')
	if err != nil {
		return nil, fmt.Errorf("provenance: decode canonical mutation: missing field %q: %w", want, err)
	}
	name = strings.TrimSuffix(name, ":")
	if name != want {
		return nil, fmt.Errorf("provenance: decode canonical mutation: expected field %q, found %q (unknown, duplicate, or out of order)", want, name)
	}
	rawLen, err := r.r.ReadString(':')
	if err != nil {
		return nil, fmt.Errorf("provenance: decode canonical mutation field %q length: %w", want, err)
	}
	n, err := strconv.Atoi(strings.TrimSuffix(rawLen, ":"))
	if err != nil || n < 0 || n > MaxCanonicalFieldBytes {
		return nil, fmt.Errorf("provenance: decode canonical mutation field %q has invalid length %q", want, rawLen)
	}
	value := make([]byte, n)
	if _, err := io.ReadFull(r.r, value); err != nil {
		return nil, fmt.Errorf("provenance: decode canonical mutation field %q is truncated: %w", want, err)
	}
	if terminator, err := r.r.ReadByte(); err != nil || terminator != '\n' {
		return nil, fmt.Errorf("provenance: decode canonical mutation field %q has invalid framing terminator", want)
	}
	return value, nil
}

func encodeCanonicalEffect(w *canonicalWriter, e Effect, index int) error {
	{
		prefix := fmt.Sprintf("effect.%d.", index)
		w.field(prefix+"family", []byte(e.Sort.String()))
		w.field(prefix+"result-slot", []byte(e.ResultSlot))
		w.field(prefix+"recorded-at-override", encodeOptionalInt64(e.RecordedAtOverride))
		contexts := func() {
			w.field(prefix+"context-count", []byte(strconv.Itoa(len(e.Contexts))))
			for j, c := range e.Contexts {
				k, id, _ := EncodeStoredEventContext(c)
				w.field(fmt.Sprintf("%scontext.%d.kind", prefix, j), []byte(k))
				w.field(fmt.Sprintf("%scontext.%d.identity", prefix, j), []byte(id))
			}
		}
		payload := func() { p, _ := canonicalJSON(e.Payload); w.field(prefix+"payload", p) }
		switch e.Sort {
		case EffectTaskCreate, EffectTaskCreateAllocated:
			w.field(prefix+"task", []byte(e.TaskID.String()))
			payload()
			contexts()
			w.field(prefix+"title", []byte(e.Title))
			w.field(prefix+"description", []byte(e.Description))
			w.field(prefix+"type", []byte(e.Type.String()))
			w.field(prefix+"priority", []byte(e.Priority.String()))
			w.field(prefix+"phase", []byte(e.Phase.String()))
		case EffectTaskEvent:
			w.field(prefix+"task", []byte(e.TaskID.String()))
			w.field(prefix+"event-kind", []byte(e.EventKind))
			payload()
			contexts()
			if e.EventKind == EventKindTaskUpdated {
				w.field(prefix+"update-title", encodeOptionalString(e.UpdateTitle))
				w.field(prefix+"update-description", encodeOptionalString(e.UpdateDescription))
				w.field(prefix+"update-priority", encodeOptionalText(e.UpdatePriority))
				w.field(prefix+"update-phase", encodeOptionalText(e.UpdatePhase))
				w.field(prefix+"update-notes", encodeOptionalString(e.UpdateNotes))
			}
			if IsTransitionLifecycleKind(e.EventKind) {
				w.field(prefix+"forced", []byte(strconv.FormatBool(e.Forced)))
				if e.EventKind == EventKindTaskClosed {
					w.field(prefix+"close-reason", []byte(e.CloseReason))
				}
			}
		case EffectBootstrapAuthority:
			w.field(prefix+"bootstrap-label", []byte(e.BootstrapLabel))
			w.field(prefix+"operation-authority", []byte(e.OperationAuthorityID))
		case EffectAssignmentStart:
			w.field(prefix+"task", []byte(e.TaskID.String()))
			w.field(prefix+"assignment", []byte(e.AssignmentID))
			w.field(prefix+"slot", []byte(e.SlotID))
			w.field(prefix+"occupant", []byte(idString(e.Occupant)))
			w.field(prefix+"predecessor", []byte(e.Predecessor))
			w.field(prefix+"parent", []byte(e.Parent))
		case EffectAssignmentEnd:
			w.field(prefix+"task", []byte(idString(e.TaskID)))
			w.field(prefix+"assignment", []byte(e.AssignmentID))
			w.field(prefix+"slot", []byte(e.SlotID))
		case EffectDecision:
			w.field(prefix+"task", []byte(idString(e.TaskID)))
			w.field(prefix+"decision-kind", []byte(e.DecisionKind))
			payload()
		case EffectEvidence:
			w.field(prefix+"task", []byte(idString(e.TaskID)))
			w.field(prefix+"evidence-kind", []byte(e.EvidenceKind))
			w.field(prefix+"content-digest", e.ContentDigest)
			payload()
		case EffectEdgeAdd, EffectEdgeRemove:
			w.field(prefix+"task", []byte(e.TaskID.String()))
			w.field(prefix+"edge-target", []byte(e.EdgeTargetID))
			w.field(prefix+"edge-kind", []byte(e.EdgeRelKind.String()))
			contexts()
		case EffectLabelAdd, EffectLabelRemove:
			w.field(prefix+"task", []byte(e.TaskID.String()))
			w.field(prefix+"label", []byte(e.Label))
			contexts()
		case EffectCommentAdd:
			w.field(prefix+"task", []byte(e.TaskID.String()))
			w.field(prefix+"comment", []byte(e.CommentIdentity.String()))
			w.field(prefix+"comment-author", []byte(e.CommentAuthor.String()))
			w.field(prefix+"comment-body", []byte(e.CommentBody))
			contexts()
		}
		return w.err
	}
}

func decodeCanonicalEffect(r *canonicalReader, index int) (Effect, error) {
	{
		p := fmt.Sprintf("effect.%d.", index)
		read := func(name string) ([]byte, error) { return r.field(p + name) }
		family, err := read("family")
		if err != nil {
			return Effect{}, err
		}
		sort, err := parseEffectSort(string(family))
		if err != nil {
			return Effect{}, err
		}
		e := Effect{Sort: sort}
		b, err := read("result-slot")
		if err != nil {
			return e, err
		}
		e.ResultSlot = ResultSlotID(b)
		b, err = read("recorded-at-override")
		if err != nil {
			return e, err
		}
		e.RecordedAtOverride, err = decodeOptionalInt64(b)
		if err != nil {
			return e, err
		}
		payload := func() error {
			raw, x := read("payload")
			if x == nil {
				e.Payload = append(json.RawMessage(nil), raw...)
			}
			return x
		}
		contexts := func() error {
			raw, x := read("context-count")
			if x != nil {
				return x
			}
			n, x := strconv.Atoi(string(raw))
			if x != nil || n < 0 || n > MaxCanonicalContextsPerEffect {
				return canonicalMutationError(p+"context-count", fmt.Sprintf("invalid bounded count %q", raw), "use a non-negative bounded count")
			}
			for j := 0; j < n; j++ {
				k, x := r.field(fmt.Sprintf("%scontext.%d.kind", p, j))
				if x != nil {
					return x
				}
				id, x := r.field(fmt.Sprintf("%scontext.%d.identity", p, j))
				if x != nil {
					return x
				}
				c, x := DecodeStoredEventContext(EventContextKind(k), string(id))
				if x != nil {
					return x
				}
				e.Contexts = append(e.Contexts, c)
			}
			return nil
		}
		task := func() error {
			raw, x := read("task")
			if x != nil {
				return x
			}
			e.TaskID, x = parseOptionalTask(string(raw))
			return x
		}
		switch sort {
		case EffectTaskCreate, EffectTaskCreateAllocated:
			if err = task(); err != nil {
				return e, err
			}
			if err = payload(); err != nil {
				return e, err
			}
			if err = contexts(); err != nil {
				return e, err
			}
			if b, err = read("title"); err == nil {
				e.Title = string(b)
			} else {
				return e, err
			}
			if b, err = read("description"); err == nil {
				e.Description = string(b)
			} else {
				return e, err
			}
			if b, err = read("type"); err == nil {
				err = e.Type.UnmarshalText(b)
			}
			if err != nil {
				return e, err
			}
			if b, err = read("priority"); err == nil {
				err = e.Priority.UnmarshalText(b)
			}
			if err != nil {
				return e, err
			}
			if b, err = read("phase"); err == nil {
				err = e.Phase.UnmarshalText(b)
			}
		case EffectTaskEvent:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read("event-kind"); err == nil {
				e.EventKind = EventKind(b)
			} else {
				return e, err
			}
			if err = payload(); err != nil {
				return e, err
			}
			if err = contexts(); err != nil {
				return e, err
			}
			if e.EventKind == EventKindTaskUpdated {
				if b, err = read("update-title"); err == nil {
					e.UpdateTitle, err = decodeOptionalString(b)
				}
				if err != nil {
					return e, err
				}
				if b, err = read("update-description"); err == nil {
					e.UpdateDescription, err = decodeOptionalString(b)
				}
				if err != nil {
					return e, err
				}
				if b, err = read("update-priority"); err == nil {
					e.UpdatePriority, err = decodeOptionalPriority(b)
				}
				if err != nil {
					return e, err
				}
				if b, err = read("update-phase"); err == nil {
					e.UpdatePhase, err = decodeOptionalPhase(b)
				}
				if err != nil {
					return e, err
				}
				if b, err = read("update-notes"); err == nil {
					e.UpdateNotes, err = decodeOptionalString(b)
				}
			}
			if IsTransitionLifecycleKind(e.EventKind) {
				if b, err = read("forced"); err == nil {
					e.Forced, err = strconv.ParseBool(string(b))
				}
				if err != nil {
					return e, err
				}
				if e.EventKind == EventKindTaskClosed {
					if b, err = read("close-reason"); err == nil {
						e.CloseReason = string(b)
					}
				}
			}
		case EffectBootstrapAuthority:
			if b, err = read("bootstrap-label"); err == nil {
				e.BootstrapLabel = string(b)
			} else {
				return e, err
			}
			if b, err = read("operation-authority"); err == nil {
				e.OperationAuthorityID = OperationAuthorityID(b)
			}
		case EffectAssignmentStart:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read("assignment"); err == nil {
				e.AssignmentID = AssignmentID(b)
			} else {
				return e, err
			}
			if b, err = read("slot"); err == nil {
				e.SlotID = AssignmentSlotID(b)
			} else {
				return e, err
			}
			if b, err = read("occupant"); err == nil {
				e.Occupant, err = parseOptionalActor(string(b))
			}
			if err != nil {
				return e, err
			}
			if b, err = read("predecessor"); err == nil {
				e.Predecessor = AssignmentID(b)
			} else {
				return e, err
			}
			if b, err = read("parent"); err == nil {
				e.Parent = AssignmentID(b)
			}
		case EffectAssignmentEnd:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read("assignment"); err == nil {
				e.AssignmentID = AssignmentID(b)
			} else {
				return e, err
			}
			if b, err = read("slot"); err == nil {
				e.SlotID = AssignmentSlotID(b)
			}
		case EffectDecision:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read("decision-kind"); err == nil {
				e.DecisionKind = DecisionKind(b)
			} else {
				return e, err
			}
			err = payload()
		case EffectEvidence:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read("evidence-kind"); err == nil {
				e.EvidenceKind = EvidenceKind(b)
			} else {
				return e, err
			}
			if b, err = read("content-digest"); err == nil {
				e.ContentDigest = append([]byte(nil), b...)
			} else {
				return e, err
			}
			err = payload()
		case EffectEdgeAdd, EffectEdgeRemove:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read("edge-target"); err == nil {
				e.EdgeTargetID = string(b)
			} else {
				return e, err
			}
			if b, err = read("edge-kind"); err == nil {
				err = e.EdgeRelKind.UnmarshalText(b)
			}
			if err == nil {
				err = contexts()
			}
		case EffectLabelAdd, EffectLabelRemove:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read("label"); err == nil {
				e.Label = string(b)
			} else {
				return e, err
			}
			err = contexts()
		case EffectCommentAdd:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read("comment"); err == nil {
				e.CommentIdentity, err = parseOptionalComment(string(b))
			}
			if err != nil {
				return e, err
			}
			if b, err = read("comment-author"); err == nil {
				e.CommentAuthor, err = parseOptionalActor(string(b))
			}
			if err != nil {
				return e, err
			}
			if b, err = read("comment-body"); err == nil {
				e.CommentBody = string(b)
			} else {
				return e, err
			}
			err = contexts()
		}
		if err != nil {
			return e, err
		}
		return normalizeCanonicalEffect(e, index)
	}
}

func normalizeCanonicalEffect(e Effect, index int) (Effect, error) {
	// Treat representational empty values as the zero value before shape checking.
	if len(e.Payload) > 0 {
		if payload, err := canonicalJSON(e.Payload); err == nil && string(payload) == "{}" {
			e.Payload = nil
		}
	}
	if len(e.Contexts) == 0 {
		e.Contexts = nil
	}
	if len(e.ContentDigest) == 0 {
		e.ContentDigest = nil
	}
	if !validEffectSort(e.Sort) {
		return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.family", index), fmt.Sprintf("unknown family %d", e.Sort), "use a declared EffectSort")
	}
	if e.ActorID.Namespace != "" {
		return Effect{}, fmt.Errorf("%w: %w", ErrActorPlacement, canonicalMutationError(fmt.Sprintf("effect.%d.actor", index), "per-effect ActorID is not behaviorally valid; actor comes from the operation anchor", "leave Effect.ActorID zero"))
	}
	n := Effect{Sort: e.Sort, ResultSlot: e.ResultSlot, RecordedAtOverride: e.RecordedAtOverride}
	switch e.Sort {
	case EffectTaskCreate, EffectTaskCreateAllocated:
		if e.TaskID.Namespace == "" || !e.Type.IsValid() || !e.Priority.IsValid() || !e.Phase.IsValid() {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.task-create", index), "invalid task identity or classification enum", "supply a namespaced task and valid type/priority/phase")
		}
		if e.Sort == EffectTaskCreateAllocated && e.ResultSlot == "" {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.result-slot", index), "allocated task create requires a result slot for committed identity reconciliation", "supply a stable non-empty result slot")
		}
		n.TaskID = e.TaskID
		n.Payload = e.Payload
		n.Contexts = e.Contexts
		n.Title = e.Title
		n.Description = e.Description
		n.Type = e.Type
		n.Priority = e.Priority
		n.Phase = e.Phase
	case EffectTaskEvent:
		if e.TaskID.Namespace == "" {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.task", index), "task event requires a task", "supply a namespaced TaskID")
		}
		if err := ValidateEventKind(e.EventKind); err != nil {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.event-kind", index), err.Error(), "use a valid namespaced event kind")
		}
		n.TaskID = e.TaskID
		n.EventKind = e.EventKind
		n.Payload = e.Payload
		n.Contexts = e.Contexts
		if e.EventKind == EventKindTaskUpdated {
			n.UpdateTitle = e.UpdateTitle
			n.UpdateDescription = e.UpdateDescription
			n.UpdatePriority = e.UpdatePriority
			n.UpdatePhase = e.UpdatePhase
			n.UpdateNotes = e.UpdateNotes
		}
		if IsTransitionLifecycleKind(e.EventKind) {
			n.Forced = e.Forced
			if e.EventKind == EventKindTaskClosed {
				n.CloseReason = e.CloseReason
			}
			if e.Forced && len(e.Payload) > 0 {
				return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.payload", index), "forced lifecycle payload is reducer-generated", "leave payload empty when Forced is true")
			}
		}
	case EffectBootstrapAuthority:
		n.BootstrapLabel = e.BootstrapLabel
		n.OperationAuthorityID = e.OperationAuthorityID
	case EffectAssignmentStart:
		if e.TaskID.Namespace == "" || e.AssignmentID == "" {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.assignment-start", index), "task and assignment are required", "supply both identities")
		}
		n.TaskID = e.TaskID
		n.AssignmentID = e.AssignmentID
		n.SlotID = e.SlotID
		n.Occupant = e.Occupant
		n.Predecessor = e.Predecessor
		n.Parent = e.Parent
	case EffectAssignmentEnd:
		if e.AssignmentID == "" {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.assignment", index), "assignment is required", "supply the episode identity")
		}
		n.AssignmentID = e.AssignmentID
		n.TaskID = e.TaskID
		n.SlotID = e.SlotID
	case EffectDecision:
		if err := ValidateEventKind(EventKind(e.DecisionKind)); err != nil {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.decision-kind", index), err.Error(), "use a lower-case namespaced decision kind such as caller.decision")
		}
		n.TaskID = e.TaskID
		n.DecisionKind = e.DecisionKind
		n.Payload = e.Payload
	case EffectEvidence:
		if err := ValidateEventKind(EventKind(e.EvidenceKind)); err != nil {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.evidence-kind", index), err.Error(), "use a lower-case namespaced evidence kind such as caller.evidence")
		}
		n.TaskID = e.TaskID
		n.EvidenceKind = e.EvidenceKind
		n.ContentDigest = e.ContentDigest
		n.Payload = e.Payload
	case EffectEdgeAdd, EffectEdgeRemove:
		if e.TaskID.Namespace == "" || e.EdgeTargetID == "" || !e.EdgeRelKind.IsValid() {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.edge", index), "invalid edge operands", "supply source, target, and valid edge kind")
		}
		n.TaskID = e.TaskID
		n.EdgeTargetID = e.EdgeTargetID
		n.EdgeRelKind = e.EdgeRelKind
		n.Contexts = e.Contexts
	case EffectLabelAdd, EffectLabelRemove:
		if e.TaskID.Namespace == "" || e.Label == "" {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.label", index), "task and label are required", "supply both operands")
		}
		n.TaskID = e.TaskID
		n.Label = e.Label
		n.Contexts = e.Contexts
	case EffectCommentAdd:
		if e.TaskID.Namespace == "" || e.CommentIdentity.Namespace == "" || e.CommentAuthor.Namespace == "" {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.comment", index), "task, comment identity, and author are required", "supply all comment operands")
		}
		n.TaskID = e.TaskID
		n.CommentIdentity = e.CommentIdentity
		n.CommentAuthor = e.CommentAuthor
		n.CommentBody = e.CommentBody
		n.Contexts = e.Contexts
	}
	if !reflect.DeepEqual(e, n) {
		v1, v2 := reflect.ValueOf(e), reflect.ValueOf(n)
		for i := 0; i < v1.NumField(); i++ {
			if !reflect.DeepEqual(v1.Field(i).Interface(), v2.Field(i).Interface()) {
				return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.%s", index, reflect.TypeOf(e).Field(i).Name), "field is populated but not consumed by this effect family", "clear the irrelevant field")
			}
		}
	}
	contexts, err := CanonicalEventContexts(n.Contexts)
	if err != nil {
		return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.contexts", index), err.Error(), "supply valid bounded event contexts")
	}
	if len(contexts) > MaxCanonicalContextsPerEffect {
		return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.context-count", index), fmt.Sprintf("%d exceeds maximum %d", len(contexts), MaxCanonicalContextsPerEffect), "reduce contexts")
	}
	if len(contexts) > 0 {
		n.Contexts = contexts
	} else {
		n.Contexts = nil
	}
	if len(n.Payload) > 0 {
		if _, err := canonicalJSON(n.Payload); err != nil {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect.%d.payload", index), err.Error(), "supply one strict JSON value without duplicate fields")
		}
	}
	return n, nil
}

func validateRawCanonicalEffectBounds(effect Effect, index int) error {
	if len(effect.Contexts) > MaxCanonicalContextsPerEffect {
		return canonicalMutationError(fmt.Sprintf("effect.%d.context-count", index), fmt.Sprintf("raw count %d exceeds maximum %d before canonicalization", len(effect.Contexts), MaxCanonicalContextsPerEffect), "reduce contexts before retrying")
	}
	value, typ := reflect.ValueOf(effect), reflect.TypeOf(effect)
	for i := 0; i < value.NumField(); i++ {
		field, name := value.Field(i), typ.Field(i).Name
		size := -1
		switch field.Kind() {
		case reflect.String:
			size = field.Len()
		case reflect.Slice:
			if field.Type().Elem().Kind() == reflect.Uint8 {
				size = field.Len()
			}
		case reflect.Pointer:
			if !field.IsNil() && field.Elem().Kind() == reflect.String {
				size = field.Elem().Len()
			}
		}
		if size > MaxCanonicalFieldBytes {
			return canonicalMutationError(fmt.Sprintf("effect.%d.%s", index, name), fmt.Sprintf("raw length %d exceeds maximum %d before normalization", size, MaxCanonicalFieldBytes), "reduce the operand before retrying")
		}
	}
	for contextIndex, context := range effect.Contexts {
		kind, identity, err := EncodeStoredEventContext(context)
		if err != nil {
			return canonicalMutationError(fmt.Sprintf("effect.%d.context.%d", index, contextIndex), err.Error(), "supply a valid context identity")
		}
		if len(kind) > MaxCanonicalFieldBytes || len(identity) > MaxCanonicalFieldBytes {
			return canonicalMutationError(fmt.Sprintf("effect.%d.context.%d", index, contextIndex), "raw context field exceeds maximum length", "reduce the context identity")
		}
	}
	return nil
}

func validEffectSort(sort EffectSort) bool {
	for _, candidate := range canonicalEffectSorts {
		if sort == candidate {
			return true
		}
	}
	return false
}

func parseEffectSort(s string) (EffectSort, error) {
	for _, sort := range canonicalEffectSorts {
		if sort.String() == s {
			return sort, nil
		}
	}
	return 0, fmt.Errorf("provenance: decode canonical mutation: unknown effect family %q", s)
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	value, err := decodeUniqueJSON(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON value or token")
	}
	return json.Marshal(value)
}

func decodeUniqueJSON(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch token := tok.(type) {
	case json.Delim:
		switch token {
		case '{':
			object := map[string]any{}
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				if _, duplicate := object[key]; duplicate {
					return nil, fmt.Errorf("duplicate JSON field %q", key)
				}
				value, err := decodeUniqueJSON(dec)
				if err != nil {
					return nil, err
				}
				object[key] = value
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim('}') {
				return nil, fmt.Errorf("unterminated JSON object")
			}
			return object, nil
		case '[':
			var array []any
			for dec.More() {
				value, err := decodeUniqueJSON(dec)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim(']') {
				return nil, fmt.Errorf("unterminated JSON array")
			}
			return array, nil
		}
	}
	return tok, nil
}

func idString[T interface{ String() string }](id T) string { return id.String() }

func encodeOptionalString(value *string) []byte {
	if value == nil {
		return []byte("0")
	}
	return append([]byte("1"), []byte(*value)...)
}
func decodeOptionalString(raw []byte) (*string, error) {
	if string(raw) == "0" {
		return nil, nil
	}
	if len(raw) < 1 || raw[0] != '1' {
		return nil, fmt.Errorf("invalid optional string marker")
	}
	value := string(raw[1:])
	return &value, nil
}
func encodeOptionalText[T interface{ String() string }](value *T) []byte {
	if value == nil {
		return []byte("0")
	}
	return append([]byte("1"), []byte((*value).String())...)
}
func encodeOptionalInt64(value *RecordedTime) []byte {
	if value == nil {
		return []byte("0")
	}
	return []byte("1" + strconv.FormatInt(*value, 10))
}
func decodeOptionalInt64(raw []byte) (*RecordedTime, error) {
	if string(raw) == "0" {
		return nil, nil
	}
	if len(raw) < 2 || raw[0] != '1' {
		return nil, fmt.Errorf("invalid optional timestamp marker")
	}
	value, err := strconv.ParseInt(string(raw[1:]), 10, 64)
	if err != nil {
		return nil, err
	}
	return &value, nil
}
func decodeOptionalPriority(raw []byte) (*Priority, error) {
	if string(raw) == "0" {
		return nil, nil
	}
	if len(raw) < 2 || raw[0] != '1' {
		return nil, fmt.Errorf("invalid optional priority marker")
	}
	var value Priority
	if err := value.UnmarshalText(raw[1:]); err != nil {
		return nil, err
	}
	return &value, nil
}
func decodeOptionalPhase(raw []byte) (*Phase, error) {
	if string(raw) == "0" {
		return nil, nil
	}
	if len(raw) < 2 || raw[0] != '1' {
		return nil, fmt.Errorf("invalid optional phase marker")
	}
	var value Phase
	if err := value.UnmarshalText(raw[1:]); err != nil {
		return nil, err
	}
	return &value, nil
}
func parseOptionalTask(raw string) (TaskID, error) {
	if raw == "--00000000-0000-0000-0000-000000000000" {
		return TaskID{}, nil
	}
	return ptypes.ParseTaskID(raw)
}
func parseOptionalActor(raw string) (ActorID, error) {
	if raw == "--00000000-0000-0000-0000-000000000000" {
		return ActorID{}, nil
	}
	return ptypes.ParseActorID(raw)
}
func parseOptionalComment(raw string) (CommentID, error) {
	if raw == "--00000000-0000-0000-0000-000000000000" {
		return CommentID{}, nil
	}
	return ptypes.ParseCommentID(raw)
}
