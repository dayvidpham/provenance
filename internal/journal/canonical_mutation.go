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

// MutationEncodingVersion identifies a registered canonical mutation codec.
// It is intentionally not string-backed: persisted wire text is owned by the
// codec registry below and cannot become an accidental protocol authority.
type MutationEncodingVersion uint8

const (
	MutationEncodingV1 MutationEncodingVersion = iota + 1
)

type inspectedMutationEncodingTag struct{ text string }

func (v MutationEncodingVersion) String() string {
	descriptor, ok := canonicalCodecForVersion(v)
	if !ok {
		return fmt.Sprintf("MutationEncodingVersion(%d)", v)
	}
	return descriptor.wireTag
}

type canonicalCodecDescriptor struct {
	version MutationEncodingVersion
	wireTag string
	prepare canonicalMutationPreparer
	decoder canonicalMutationDecoder
}

type canonicalCodecDescriptors []canonicalCodecDescriptor

var canonicalCodecRegistry = canonicalCodecDescriptors{
	canonicalCodecDescriptor{version: MutationEncodingV1, wireTag: "provenance.mutation.v1", prepare: prepareMutationV1, decoder: decodeCanonicalMutationV1},
}

func canonicalCodecForVersion(version MutationEncodingVersion) (canonicalCodecDescriptor, bool) {
	if canonicalCodecRegistry.validate() != nil {
		var zero canonicalCodecDescriptor
		return zero, false
	}
	return canonicalCodecRegistry.codecForVersion(version)
}

func (registry canonicalCodecDescriptors) validate() error {
	versions := make(map[MutationEncodingVersion]struct{}, len(registry))
	tags := make(map[string]struct{}, len(registry))
	for _, descriptor := range registry {
		if descriptor.version == 0 || descriptor.wireTag == "" || descriptor.prepare == nil || descriptor.decoder == nil {
			return canonicalMutationError("codec-registry", "codec descriptor has an empty version, wire tag, preparer, or decoder", "register a complete codec descriptor")
		}
		if _, exists := versions[descriptor.version]; exists {
			return canonicalMutationError("codec-registry", fmt.Sprintf("duplicate mutation encoding version %d", descriptor.version), "register each encoding version exactly once")
		}
		if _, exists := tags[descriptor.wireTag]; exists {
			return canonicalMutationError("codec-registry", fmt.Sprintf("duplicate mutation encoding wire tag %q", descriptor.wireTag), "register each wire tag exactly once")
		}
		versions[descriptor.version] = struct{}{}
		tags[descriptor.wireTag] = struct{}{}
	}
	return nil
}

func (registry canonicalCodecDescriptors) codecForVersion(version MutationEncodingVersion) (canonicalCodecDescriptor, bool) {
	for _, descriptor := range registry {
		if descriptor.version == version {
			return descriptor, true
		}
	}
	var zero canonicalCodecDescriptor
	return zero, false
}

func (registry canonicalCodecDescriptors) versionForTag(tag string) (MutationEncodingVersion, bool) {
	for _, descriptor := range registry {
		if descriptor.wireTag == tag {
			return descriptor.version, true
		}
	}
	return 0, false
}

func inspectMutationEncodingTag(text string) inspectedMutationEncodingTag {
	return inspectedMutationEncodingTag{text: text}
}

func (tag inspectedMutationEncodingTag) version() (MutationEncodingVersion, bool) {
	if canonicalCodecRegistry.validate() != nil {
		return 0, false
	}
	return canonicalCodecRegistry.versionForTag(tag.text)
}

// MatchesStoredText compares an opaque inspected wire tag with the redundant
// SQLite text column without exposing the tag as a protocol string.
func (tag inspectedMutationEncodingTag) MatchesStoredText(stored string) bool {
	return tag.text == stored
}

// RegisteredVersion resolves a matching inspected tag through the codec
// registry. Unknown tags remain opaque and distinguishable from mismatches.
func (tag inspectedMutationEncodingTag) RegisteredVersion() (MutationEncodingVersion, bool) {
	return tag.version()
}

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
	version MutationEncodingVersion
	bytes   []byte
	digest  []byte
	effects []Effect
}

func (m CanonicalMutation) EncodingVersion() MutationEncodingVersion { return m.version }
func (m CanonicalMutation) CanonicalBytes() []byte                   { return append([]byte(nil), m.bytes...) }
func (m CanonicalMutation) DerivedDigest() []byte                    { return append([]byte(nil), m.digest...) }
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

type canonicalV1FamilyDescriptor struct {
	sort EffectSort
	tag  string
}

type canonicalV1FamilyRegistry []canonicalV1FamilyDescriptor

var canonicalV1Families = canonicalV1FamilyRegistry{
	canonicalV1FamilyDescriptor{sort: EffectTaskEvent, tag: "task_event"},
	canonicalV1FamilyDescriptor{sort: EffectBootstrapAuthority, tag: "bootstrap_authority"},
	canonicalV1FamilyDescriptor{sort: EffectAssignmentStart, tag: "assignment_start"},
	canonicalV1FamilyDescriptor{sort: EffectAssignmentEnd, tag: "assignment_end"},
	canonicalV1FamilyDescriptor{sort: EffectDecision, tag: "decision"},
	canonicalV1FamilyDescriptor{sort: EffectEvidence, tag: "evidence"},
	canonicalV1FamilyDescriptor{sort: EffectTaskCreate, tag: "task_create"},
	canonicalV1FamilyDescriptor{sort: EffectEdgeAdd, tag: "edge_add"},
	canonicalV1FamilyDescriptor{sort: EffectEdgeRemove, tag: "edge_remove"},
	canonicalV1FamilyDescriptor{sort: EffectLabelAdd, tag: "label_add"},
	canonicalV1FamilyDescriptor{sort: EffectLabelRemove, tag: "label_remove"},
	canonicalV1FamilyDescriptor{sort: EffectCommentAdd, tag: "comment_add"},
	canonicalV1FamilyDescriptor{sort: EffectTaskCreateAllocated, tag: "task_create_allocated"},
}

type canonicalV1Codec struct{}

var mutationV1Codec canonicalV1Codec

type canonicalEnvelopeField uint8

const (
	envelopeVersion canonicalEnvelopeField = iota + 1
	envelopeEffectCount
)

type canonicalEffectField uint8

const (
	effectFamily canonicalEffectField = iota + 1
	effectResultSlot
	effectRecordedAtOverride
	effectContextCount
	effectPayload
	effectTask
	effectTitle
	effectDescription
	effectType
	effectPriority
	effectPhase
	effectEventKind
	effectUpdateTitle
	effectUpdateDescription
	effectUpdatePriority
	effectUpdatePhase
	effectUpdateNotes
	effectForced
	effectCloseReason
	effectBootstrapLabel
	effectOperationAuthority
	effectAssignment
	effectSlot
	effectOccupant
	effectPredecessor
	effectParent
	effectDecisionKind
	effectEvidenceKind
	effectContentDigest
	effectEdgeTarget
	effectEdgeKind
	effectLabel
	effectComment
	effectCommentAuthor
	effectCommentBody
)

type canonicalContextField uint8

const (
	contextKind canonicalContextField = iota + 1
	contextIdentity
)

type canonicalV1FieldRef interface{ canonicalV1FieldRef() }
type canonicalV1EnvelopeRef struct{ field canonicalEnvelopeField }
type canonicalV1EffectRef struct {
	index int
	field canonicalEffectField
}
type canonicalV1ContextRef struct {
	effectIndex, contextIndex int
	field                     canonicalContextField
}

func (canonicalV1EnvelopeRef) canonicalV1FieldRef() {}
func (canonicalV1EffectRef) canonicalV1FieldRef()   {}
func (canonicalV1ContextRef) canonicalV1FieldRef()  {}

func envelopeField(field canonicalEnvelopeField) canonicalV1FieldRef {
	return canonicalV1EnvelopeRef{field: field}
}
func effectField(index int, field canonicalEffectField) canonicalV1FieldRef {
	return canonicalV1EffectRef{index: index, field: field}
}
func contextField(effectIndex, contextIndex int, field canonicalContextField) canonicalV1FieldRef {
	return canonicalV1ContextRef{effectIndex: effectIndex, contextIndex: contextIndex, field: field}
}

func (canonicalV1Codec) renderFieldName(ref canonicalV1FieldRef) (string, error) {
	switch typed := ref.(type) {
	case canonicalV1EnvelopeRef:
		switch typed.field {
		case envelopeVersion:
			return "version", nil
		case envelopeEffectCount:
			return "effect-count", nil
		default:
			return "", canonicalMutationError("field-reference", fmt.Sprintf("unknown V1 envelope field %d", typed.field), "use a declared V1 envelope field")
		}
	case canonicalV1ContextRef:
		if typed.effectIndex < 0 || typed.effectIndex >= MaxCanonicalEffects || typed.contextIndex < 0 || typed.contextIndex >= MaxCanonicalContextsPerEffect {
			return "", canonicalMutationError("field-reference", "V1 context index is negative or outside the canonical bounds", "use bounded non-negative effect and context indices")
		}
		var name string
		switch typed.field {
		case contextKind:
			name = "kind"
		case contextIdentity:
			name = "identity"
		default:
			return "", canonicalMutationError("field-reference", fmt.Sprintf("unknown V1 context field %d", typed.field), "use a declared V1 context field")
		}
		return fmt.Sprintf("effect.%d.context.%d.%s", typed.effectIndex, typed.contextIndex, name), nil
	case canonicalV1EffectRef:
		if typed.index < 0 || typed.index >= MaxCanonicalEffects {
			return "", canonicalMutationError("field-reference", "V1 effect index is negative or outside the canonical bound", "use a bounded non-negative effect index")
		}
		var name string
		switch typed.field {
		case effectFamily:
			name = "family"
		case effectResultSlot:
			name = "result-slot"
		case effectRecordedAtOverride:
			name = "recorded-at-override"
		case effectContextCount:
			name = "context-count"
		case effectPayload:
			name = "payload"
		case effectTask:
			name = "task"
		case effectTitle:
			name = "title"
		case effectDescription:
			name = "description"
		case effectType:
			name = "type"
		case effectPriority:
			name = "priority"
		case effectPhase:
			name = "phase"
		case effectEventKind:
			name = "event-kind"
		case effectUpdateTitle:
			name = "update-title"
		case effectUpdateDescription:
			name = "update-description"
		case effectUpdatePriority:
			name = "update-priority"
		case effectUpdatePhase:
			name = "update-phase"
		case effectUpdateNotes:
			name = "update-notes"
		case effectForced:
			name = "forced"
		case effectCloseReason:
			name = "close-reason"
		case effectBootstrapLabel:
			name = "bootstrap-label"
		case effectOperationAuthority:
			name = "operation-authority"
		case effectAssignment:
			name = "assignment"
		case effectSlot:
			name = "slot"
		case effectOccupant:
			name = "occupant"
		case effectPredecessor:
			name = "predecessor"
		case effectParent:
			name = "parent"
		case effectDecisionKind:
			name = "decision-kind"
		case effectEvidenceKind:
			name = "evidence-kind"
		case effectContentDigest:
			name = "content-digest"
		case effectEdgeTarget:
			name = "edge-target"
		case effectEdgeKind:
			name = "edge-kind"
		case effectLabel:
			name = "label"
		case effectComment:
			name = "comment"
		case effectCommentAuthor:
			name = "comment-author"
		case effectCommentBody:
			name = "comment-body"
		default:
			return "", canonicalMutationError("field-reference", fmt.Sprintf("unknown V1 effect field %d", typed.field), "use a declared V1 effect field")
		}
		return fmt.Sprintf("effect.%d.%s", typed.index, name), nil
	default:
		return "", canonicalMutationError("field-reference", "unknown V1 field reference variant", "use a V1 scope-specific field constructor")
	}
}

func (codec canonicalV1Codec) diagnosticField(ref canonicalV1FieldRef) string {
	name, err := codec.renderFieldName(ref)
	if err != nil {
		return "field-reference"
	}
	return name
}

// PrepareMutationV1 validates and normalizes effects, writes their canonical bytes,
// then decodes those bytes once. The returned decoded effects are the only effects a
// write path should execute.
func PrepareMutationV1(effects []Effect) (CanonicalMutation, error) {
	return prepareCanonicalMutation(MutationEncodingV1, effects)
}

func prepareCanonicalMutation(version MutationEncodingVersion, effects []Effect) (CanonicalMutation, error) {
	descriptor, ok := canonicalCodecForVersion(version)
	if !ok {
		return CanonicalMutation{}, canonicalMutationError("codec-registry", fmt.Sprintf("mutation encoding version %d has no complete registered codec", version), "register exactly one complete codec descriptor before preparing mutations")
	}
	return descriptor.prepare(effects, descriptor)
}

func prepareMutationV1(effects []Effect, descriptor canonicalCodecDescriptor) (CanonicalMutation, error) {
	if len(effects) > MaxCanonicalEffects {
		return CanonicalMutation{}, canonicalMutationError("effect-count", fmt.Sprintf("%d exceeds maximum %d", len(effects), MaxCanonicalEffects), "split the operation into bounded mutations")
	}
	normalized := make([]Effect, len(effects))
	counter := &canonicalSizeCounter{limit: MaxCanonicalMutationBytes}
	w := canonicalWriter{codec: mutationV1Codec, w: counter}
	writeCanonicalEnvelopeHeader(&w, len(effects), descriptor.wireTag)
	for i := range effects {
		if err := validateRawCanonicalEffectBounds(effects[i], i); err != nil {
			return CanonicalMutation{}, err
		}
		var err error
		normalized[i], err = mutationV1Codec.normalizeEffect(effects[i], i)
		if err != nil {
			return CanonicalMutation{}, err
		}
		if err := mutationV1Codec.encodeEffect(&w, normalized[i], i); err != nil {
			return CanonicalMutation{}, err
		}
	}
	if w.err != nil {
		return CanonicalMutation{}, w.err
	}
	var out bytes.Buffer
	out.Grow(counter.size)
	w = canonicalWriter{codec: mutationV1Codec, w: &out}
	writeCanonicalEnvelopeHeader(&w, len(normalized), descriptor.wireTag)
	for i := range normalized {
		if err := mutationV1Codec.encodeEffect(&w, normalized[i], i); err != nil {
			return CanonicalMutation{}, err
		}
	}
	if w.err != nil {
		return CanonicalMutation{}, fmt.Errorf("provenance: encode bounded canonical mutation: %w", w.err)
	}
	return descriptor.decoder(out.Bytes(), descriptor.version, descriptor.wireTag)
}

func writeCanonicalEnvelopeHeader(w *canonicalWriter, effectCount int, wireTag string) {
	w.field(envelopeField(envelopeVersion), []byte(wireTag))
	w.field(envelopeField(envelopeEffectCount), []byte(strconv.Itoa(effectCount)))
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
	mutation, err := decodeCanonicalMutationWithRegistry(data, canonicalCodecRegistry)
	if err == nil {
		return mutation, nil
	}
	var typed *CanonicalMutationError
	if errors.As(err, &typed) {
		return CanonicalMutation{}, err
	}
	return CanonicalMutation{}, canonicalMutationError("wire", err.Error(), "restore bytes produced by a registered canonical codec with complete ordered fields")
}

// InspectCanonicalMutationEncodingVersion reads only the framed wire-version
// field. It does not require that this build support the version, allowing
// startup to distinguish a column/wire mismatch from a matching unknown codec.
func InspectCanonicalMutationEncodingVersion(data []byte) (inspectedMutationEncodingTag, error) {
	return inspectCanonicalMutationEncodingVersionWithRegistry(data, canonicalCodecRegistry)
}

func inspectCanonicalMutationEncodingVersionWithRegistry(data []byte, registry canonicalCodecDescriptors) (inspectedMutationEncodingTag, error) {
	if err := registry.validate(); err != nil {
		return inspectedMutationEncodingTag{}, err
	}
	if len(data) > MaxCanonicalMutationBytes {
		return inspectedMutationEncodingTag{}, canonicalMutationError("mutation", fmt.Sprintf("%d bytes exceeds maximum %d", len(data), MaxCanonicalMutationBytes), "restore bounded canonical bytes")
	}
	r := canonicalReader{codec: mutationV1Codec, r: bufio.NewReader(bytes.NewReader(data))}
	version, err := r.field(envelopeField(envelopeVersion))
	if err == nil {
		return inspectMutationEncodingTag(string(version)), nil
	}
	var typed *CanonicalMutationError
	if errors.As(err, &typed) {
		return inspectedMutationEncodingTag{}, err
	}
	return inspectedMutationEncodingTag{}, canonicalMutationError("wire version", err.Error(), "restore the leading framed version field from a committed canonical mutation")
}

func decodeCanonicalMutationWithRegistry(data []byte, registry canonicalCodecDescriptors) (CanonicalMutation, error) {
	if err := registry.validate(); err != nil {
		return CanonicalMutation{}, err
	}
	if len(data) > MaxCanonicalMutationBytes {
		return CanonicalMutation{}, canonicalMutationError("mutation", fmt.Sprintf("%d bytes exceeds maximum %d", len(data), MaxCanonicalMutationBytes), "restore bounded canonical bytes")
	}
	r := canonicalReader{codec: mutationV1Codec, r: bufio.NewReader(bytes.NewReader(data))}
	version, err := r.field(envelopeField(envelopeVersion))
	if err != nil {
		return CanonicalMutation{}, err
	}
	inspected := inspectMutationEncodingTag(string(version))
	registered, ok := registry.versionForTag(inspected.text)
	if !ok {
		return CanonicalMutation{}, fmt.Errorf("unsupported encoding version %q", version)
	}
	descriptor, ok := registry.codecForVersion(registered)
	if !ok {
		return CanonicalMutation{}, fmt.Errorf("unsupported encoding version %q", version)
	}
	return descriptor.decoder(data, descriptor.version, descriptor.wireTag)
}

type canonicalMutationDecoder func([]byte, MutationEncodingVersion, string) (CanonicalMutation, error)
type canonicalMutationPreparer func([]Effect, canonicalCodecDescriptor) (CanonicalMutation, error)

func decodeCanonicalMutationV1(data []byte, versionID MutationEncodingVersion, wireTag string) (CanonicalMutation, error) {
	r := canonicalReader{codec: mutationV1Codec, r: bufio.NewReader(bytes.NewReader(data))}
	version, err := r.field(envelopeField(envelopeVersion))
	if err != nil || versionID != MutationEncodingV1 || string(version) != wireTag {
		return CanonicalMutation{}, fmt.Errorf("provenance: decode V1 canonical mutation: invalid version frame %q: %w", version, err)
	}
	rawCount, err := r.field(envelopeField(envelopeEffectCount))
	if err != nil {
		return CanonicalMutation{}, err
	}
	count, err := strconv.Atoi(string(rawCount))
	if err != nil || count < 0 || count > MaxCanonicalEffects {
		return CanonicalMutation{}, fmt.Errorf("provenance: decode canonical mutation: invalid effect-count %q", rawCount)
	}
	effects := make([]Effect, count)
	for i := range effects {
		effects[i], err = mutationV1Codec.decodeEffect(&r, i)
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
		version: versionID,
		bytes:   append([]byte(nil), data...),
		digest:  append([]byte(nil), digest[:]...),
		effects: effects,
	}, nil
}

// IsSupportedMutationEncoding is the single codec-version registry used by
// persistence and wire decoding. SQL enforces only structural NULL/nonempty facts.
func IsSupportedMutationEncoding(version MutationEncodingVersion) bool {
	descriptor, ok := canonicalCodecForVersion(version)
	return ok && descriptor.decoder != nil
}

type canonicalWriter struct {
	codec canonicalV1Codec
	w     io.Writer
	err   error
}

func (w *canonicalWriter) field(ref canonicalV1FieldRef, value []byte) {
	if w.err != nil {
		return
	}
	name, err := w.codec.renderFieldName(ref)
	if err != nil {
		w.err = err
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

type canonicalReader struct {
	codec canonicalV1Codec
	r     *bufio.Reader
}

func (r *canonicalReader) field(ref canonicalV1FieldRef) ([]byte, error) {
	want, renderErr := r.codec.renderFieldName(ref)
	if renderErr != nil {
		return nil, renderErr
	}
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

func (codec canonicalV1Codec) encodeEffect(w *canonicalWriter, e Effect, index int) error {
	{
		family, _ := mutationV1Codec.familyTag(e.Sort)
		w.field(effectField(index, effectFamily), []byte(family))
		w.field(effectField(index, effectResultSlot), []byte(e.ResultSlot))
		w.field(effectField(index, effectRecordedAtOverride), codec.encodeOptionalInt64(e.RecordedAtOverride))
		contexts := func() {
			w.field(effectField(index, effectContextCount), []byte(strconv.Itoa(len(e.Contexts))))
			for j, c := range e.Contexts {
				k, id, _ := EncodeStoredEventContext(c)
				w.field(contextField(index, j, contextKind), []byte(k))
				w.field(contextField(index, j, contextIdentity), []byte(id))
			}
		}
		payload := func() { p, _ := codec.canonicalJSON(e.Payload); w.field(effectField(index, effectPayload), p) }
		switch e.Sort {
		case EffectTaskCreate, EffectTaskCreateAllocated:
			w.field(effectField(index, effectTask), []byte(e.TaskID.String()))
			payload()
			contexts()
			w.field(effectField(index, effectTitle), []byte(e.Title))
			w.field(effectField(index, effectDescription), []byte(e.Description))
			w.field(effectField(index, effectType), []byte(e.Type.String()))
			w.field(effectField(index, effectPriority), []byte(e.Priority.String()))
			w.field(effectField(index, effectPhase), []byte(e.Phase.String()))
		case EffectTaskEvent:
			w.field(effectField(index, effectTask), []byte(e.TaskID.String()))
			w.field(effectField(index, effectEventKind), []byte(e.EventKind))
			payload()
			contexts()
			if e.EventKind == EventKindTaskUpdated {
				w.field(effectField(index, effectUpdateTitle), codec.encodeOptionalString(e.UpdateTitle))
				w.field(effectField(index, effectUpdateDescription), codec.encodeOptionalString(e.UpdateDescription))
				w.field(effectField(index, effectUpdatePriority), codec.encodeOptionalPriority(e.UpdatePriority))
				w.field(effectField(index, effectUpdatePhase), codec.encodeOptionalPhase(e.UpdatePhase))
				w.field(effectField(index, effectUpdateNotes), codec.encodeOptionalString(e.UpdateNotes))
			}
			if IsTransitionLifecycleKind(e.EventKind) {
				w.field(effectField(index, effectForced), []byte(strconv.FormatBool(e.Forced)))
				if e.EventKind == EventKindTaskClosed {
					w.field(effectField(index, effectCloseReason), []byte(e.CloseReason))
				}
			}
		case EffectBootstrapAuthority:
			w.field(effectField(index, effectBootstrapLabel), []byte(e.BootstrapLabel))
			w.field(effectField(index, effectOperationAuthority), []byte(e.OperationAuthorityID))
		case EffectAssignmentStart:
			w.field(effectField(index, effectTask), []byte(e.TaskID.String()))
			w.field(effectField(index, effectAssignment), []byte(e.AssignmentID))
			w.field(effectField(index, effectSlot), []byte(e.SlotID))
			w.field(effectField(index, effectOccupant), []byte(idString(e.Occupant)))
			w.field(effectField(index, effectPredecessor), []byte(e.Predecessor))
			w.field(effectField(index, effectParent), []byte(e.Parent))
		case EffectAssignmentEnd:
			w.field(effectField(index, effectTask), []byte(idString(e.TaskID)))
			w.field(effectField(index, effectAssignment), []byte(e.AssignmentID))
			w.field(effectField(index, effectSlot), []byte(e.SlotID))
		case EffectDecision:
			w.field(effectField(index, effectTask), []byte(idString(e.TaskID)))
			w.field(effectField(index, effectDecisionKind), []byte(e.DecisionKind))
			payload()
		case EffectEvidence:
			w.field(effectField(index, effectTask), []byte(idString(e.TaskID)))
			w.field(effectField(index, effectEvidenceKind), []byte(e.EvidenceKind))
			w.field(effectField(index, effectContentDigest), e.ContentDigest)
			payload()
		case EffectEdgeAdd, EffectEdgeRemove:
			w.field(effectField(index, effectTask), []byte(e.TaskID.String()))
			w.field(effectField(index, effectEdgeTarget), []byte(e.EdgeTargetID))
			w.field(effectField(index, effectEdgeKind), []byte(e.EdgeRelKind.String()))
			contexts()
		case EffectLabelAdd, EffectLabelRemove:
			w.field(effectField(index, effectTask), []byte(e.TaskID.String()))
			w.field(effectField(index, effectLabel), []byte(e.Label))
			contexts()
		case EffectCommentAdd:
			w.field(effectField(index, effectTask), []byte(e.TaskID.String()))
			w.field(effectField(index, effectComment), []byte(e.CommentIdentity.String()))
			w.field(effectField(index, effectCommentAuthor), []byte(e.CommentAuthor.String()))
			w.field(effectField(index, effectCommentBody), []byte(e.CommentBody))
			contexts()
		}
		return w.err
	}
}

func (codec canonicalV1Codec) decodeEffect(r *canonicalReader, index int) (Effect, error) {
	{
		fieldName := func(field canonicalEffectField) string {
			name, _ := codec.renderFieldName(effectField(index, field))
			return name
		}
		read := func(field canonicalEffectField) ([]byte, error) { return r.field(effectField(index, field)) }
		family, err := read(effectFamily)
		if err != nil {
			return Effect{}, err
		}
		sort, err := mutationV1Codec.parseFamilyTag(string(family))
		if err != nil {
			return Effect{}, err
		}
		e := Effect{Sort: sort}
		b, err := read(effectResultSlot)
		if err != nil {
			return e, err
		}
		e.ResultSlot = ResultSlotID(b)
		b, err = read(effectRecordedAtOverride)
		if err != nil {
			return e, err
		}
		e.RecordedAtOverride, err = codec.decodeOptionalInt64(b)
		if err != nil {
			return e, err
		}
		payload := func() error {
			raw, x := read(effectPayload)
			if x == nil {
				e.Payload = append(json.RawMessage(nil), raw...)
			}
			return x
		}
		contexts := func() error {
			raw, x := read(effectContextCount)
			if x != nil {
				return x
			}
			n, x := strconv.Atoi(string(raw))
			if x != nil || n < 0 || n > MaxCanonicalContextsPerEffect {
				return canonicalMutationError(fieldName(effectContextCount), fmt.Sprintf("invalid bounded count %q", raw), "use a non-negative bounded count")
			}
			for j := 0; j < n; j++ {
				k, x := r.field(contextField(index, j, contextKind))
				if x != nil {
					return x
				}
				id, x := r.field(contextField(index, j, contextIdentity))
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
			raw, x := read(effectTask)
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
			if b, err = read(effectTitle); err == nil {
				e.Title = string(b)
			} else {
				return e, err
			}
			if b, err = read(effectDescription); err == nil {
				e.Description = string(b)
			} else {
				return e, err
			}
			if b, err = read(effectType); err == nil {
				err = e.Type.UnmarshalText(b)
			}
			if err != nil {
				return e, err
			}
			if b, err = read(effectPriority); err == nil {
				err = e.Priority.UnmarshalText(b)
			}
			if err != nil {
				return e, err
			}
			if b, err = read(effectPhase); err == nil {
				err = e.Phase.UnmarshalText(b)
			}
		case EffectTaskEvent:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read(effectEventKind); err == nil {
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
				if b, err = read(effectUpdateTitle); err == nil {
					e.UpdateTitle, err = codec.decodeOptionalString(b)
				}
				if err != nil {
					return e, err
				}
				if b, err = read(effectUpdateDescription); err == nil {
					e.UpdateDescription, err = codec.decodeOptionalString(b)
				}
				if err != nil {
					return e, err
				}
				if b, err = read(effectUpdatePriority); err == nil {
					e.UpdatePriority, err = codec.decodeOptionalPriority(b)
				}
				if err != nil {
					return e, err
				}
				if b, err = read(effectUpdatePhase); err == nil {
					e.UpdatePhase, err = codec.decodeOptionalPhase(b)
				}
				if err != nil {
					return e, err
				}
				if b, err = read(effectUpdateNotes); err == nil {
					e.UpdateNotes, err = codec.decodeOptionalString(b)
				}
			}
			if IsTransitionLifecycleKind(e.EventKind) {
				if b, err = read(effectForced); err == nil {
					e.Forced, err = strconv.ParseBool(string(b))
				}
				if err != nil {
					return e, err
				}
				if e.EventKind == EventKindTaskClosed {
					if b, err = read(effectCloseReason); err == nil {
						e.CloseReason = string(b)
					}
				}
			}
		case EffectBootstrapAuthority:
			if b, err = read(effectBootstrapLabel); err == nil {
				e.BootstrapLabel = string(b)
			} else {
				return e, err
			}
			if b, err = read(effectOperationAuthority); err == nil {
				e.OperationAuthorityID = OperationAuthorityID(b)
			}
		case EffectAssignmentStart:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read(effectAssignment); err == nil {
				e.AssignmentID = AssignmentID(b)
			} else {
				return e, err
			}
			if b, err = read(effectSlot); err == nil {
				e.SlotID = AssignmentSlotID(b)
			} else {
				return e, err
			}
			if b, err = read(effectOccupant); err == nil {
				e.Occupant, err = parseOptionalActor(string(b))
			}
			if err != nil {
				return e, err
			}
			if b, err = read(effectPredecessor); err == nil {
				e.Predecessor = AssignmentID(b)
			} else {
				return e, err
			}
			if b, err = read(effectParent); err == nil {
				e.Parent = AssignmentID(b)
			}
		case EffectAssignmentEnd:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read(effectAssignment); err == nil {
				e.AssignmentID = AssignmentID(b)
			} else {
				return e, err
			}
			if b, err = read(effectSlot); err == nil {
				e.SlotID = AssignmentSlotID(b)
			}
		case EffectDecision:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read(effectDecisionKind); err == nil {
				e.DecisionKind = DecisionKind(b)
			} else {
				return e, err
			}
			err = payload()
		case EffectEvidence:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read(effectEvidenceKind); err == nil {
				e.EvidenceKind = EvidenceKind(b)
			} else {
				return e, err
			}
			if b, err = read(effectContentDigest); err == nil {
				e.ContentDigest = append([]byte(nil), b...)
			} else {
				return e, err
			}
			err = payload()
		case EffectEdgeAdd, EffectEdgeRemove:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read(effectEdgeTarget); err == nil {
				e.EdgeTargetID = string(b)
			} else {
				return e, err
			}
			if b, err = read(effectEdgeKind); err == nil {
				err = e.EdgeRelKind.UnmarshalText(b)
			}
			if err == nil {
				err = contexts()
			}
		case EffectLabelAdd, EffectLabelRemove:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read(effectLabel); err == nil {
				e.Label = string(b)
			} else {
				return e, err
			}
			err = contexts()
		case EffectCommentAdd:
			if err = task(); err != nil {
				return e, err
			}
			if b, err = read(effectComment); err == nil {
				e.CommentIdentity, err = parseOptionalComment(string(b))
			}
			if err != nil {
				return e, err
			}
			if b, err = read(effectCommentAuthor); err == nil {
				e.CommentAuthor, err = parseOptionalActor(string(b))
			}
			if err != nil {
				return e, err
			}
			if b, err = read(effectCommentBody); err == nil {
				e.CommentBody = string(b)
			} else {
				return e, err
			}
			err = contexts()
		}
		if err != nil {
			return e, err
		}
		return codec.normalizeEffect(e, index)
	}
}

func (codec canonicalV1Codec) normalizeEffect(e Effect, index int) (Effect, error) {
	// Treat representational empty values as the zero value before shape checking.
	if len(e.Payload) > 0 {
		if payload, err := codec.canonicalJSON(e.Payload); err == nil && string(payload) == "{}" {
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
		return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectFamily)), fmt.Sprintf("unknown family %d", e.Sort), "use a declared EffectSort")
	}
	if e.ActorID.Namespace != "" {
		return Effect{}, fmt.Errorf("%w: %w", ErrActorPlacement, canonicalMutationError(fmt.Sprintf("effect[%d] actor input", index), "per-effect ActorID is not behaviorally valid; actor comes from the operation anchor", "leave Effect.ActorID zero"))
	}
	n := Effect{Sort: e.Sort, ResultSlot: e.ResultSlot, RecordedAtOverride: e.RecordedAtOverride}
	switch e.Sort {
	case EffectTaskCreate, EffectTaskCreateAllocated:
		if e.TaskID.Namespace == "" || !e.Type.IsValid() || !e.Priority.IsValid() || !e.Phase.IsValid() {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect[%d] task-create input", index), "invalid task identity or classification enum", "supply a namespaced task and valid type/priority/phase")
		}
		if e.Sort == EffectTaskCreateAllocated && e.ResultSlot == "" {
			return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectResultSlot)), "allocated task create requires a result slot for committed identity reconciliation", "supply a stable non-empty result slot")
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
			return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectTask)), "task event requires a task", "supply a namespaced TaskID")
		}
		if err := ValidateEventKind(e.EventKind); err != nil {
			return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectEventKind)), err.Error(), "use a valid namespaced event kind")
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
				return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectPayload)), "forced lifecycle payload is reducer-generated", "leave payload empty when Forced is true")
			}
		}
	case EffectBootstrapAuthority:
		n.BootstrapLabel = e.BootstrapLabel
		n.OperationAuthorityID = e.OperationAuthorityID
	case EffectAssignmentStart:
		if e.TaskID.Namespace == "" || e.AssignmentID == "" {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect[%d] assignment-start input", index), "task and assignment are required", "supply both identities")
		}
		n.TaskID = e.TaskID
		n.AssignmentID = e.AssignmentID
		n.SlotID = e.SlotID
		n.Occupant = e.Occupant
		n.Predecessor = e.Predecessor
		n.Parent = e.Parent
	case EffectAssignmentEnd:
		if e.AssignmentID == "" {
			return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectAssignment)), "assignment is required", "supply the episode identity")
		}
		n.AssignmentID = e.AssignmentID
		n.TaskID = e.TaskID
		n.SlotID = e.SlotID
	case EffectDecision:
		if err := ValidateEventKind(EventKind(e.DecisionKind)); err != nil {
			return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectDecisionKind)), err.Error(), "use a lower-case namespaced decision kind such as caller.decision")
		}
		n.TaskID = e.TaskID
		n.DecisionKind = e.DecisionKind
		n.Payload = e.Payload
	case EffectEvidence:
		if err := ValidateEventKind(EventKind(e.EvidenceKind)); err != nil {
			return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectEvidenceKind)), err.Error(), "use a lower-case namespaced evidence kind such as caller.evidence")
		}
		n.TaskID = e.TaskID
		n.EvidenceKind = e.EvidenceKind
		n.ContentDigest = e.ContentDigest
		n.Payload = e.Payload
	case EffectEdgeAdd, EffectEdgeRemove:
		if e.TaskID.Namespace == "" || e.EdgeTargetID == "" || !e.EdgeRelKind.IsValid() {
			return Effect{}, canonicalMutationError(fmt.Sprintf("effect[%d] edge input", index), "invalid edge operands", "supply source, target, and valid edge kind")
		}
		n.TaskID = e.TaskID
		n.EdgeTargetID = e.EdgeTargetID
		n.EdgeRelKind = e.EdgeRelKind
		n.Contexts = e.Contexts
	case EffectLabelAdd, EffectLabelRemove:
		if e.TaskID.Namespace == "" || e.Label == "" {
			return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectLabel)), "task and label are required", "supply both operands")
		}
		n.TaskID = e.TaskID
		n.Label = e.Label
		n.Contexts = e.Contexts
	case EffectCommentAdd:
		if e.TaskID.Namespace == "" || e.CommentIdentity.Namespace == "" || e.CommentAuthor.Namespace == "" {
			return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectComment)), "task, comment identity, and author are required", "supply all comment operands")
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
				return Effect{}, canonicalMutationError(fmt.Sprintf("effect[%d] input %s", index, reflect.TypeOf(e).Field(i).Name), "field is populated but not consumed by this effect family", "clear the irrelevant field")
			}
		}
	}
	contexts, err := CanonicalEventContexts(n.Contexts)
	if err != nil {
		return Effect{}, canonicalMutationError(fmt.Sprintf("effect[%d] contexts input", index), err.Error(), "supply valid bounded event contexts")
	}
	if len(contexts) > MaxCanonicalContextsPerEffect {
		return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectContextCount)), fmt.Sprintf("%d exceeds maximum %d", len(contexts), MaxCanonicalContextsPerEffect), "reduce contexts")
	}
	if len(contexts) > 0 {
		n.Contexts = contexts
	} else {
		n.Contexts = nil
	}
	if len(n.Payload) > 0 {
		if _, err := codec.canonicalJSON(n.Payload); err != nil {
			return Effect{}, canonicalMutationError(codec.diagnosticField(effectField(index, effectPayload)), err.Error(), "supply one strict JSON value without duplicate fields")
		}
	}
	return n, nil
}

func validateRawCanonicalEffectBounds(effect Effect, index int) error {
	if len(effect.Contexts) > MaxCanonicalContextsPerEffect {
		return canonicalMutationError(mutationV1Codec.diagnosticField(effectField(index, effectContextCount)), fmt.Sprintf("raw count %d exceeds maximum %d before canonicalization", len(effect.Contexts), MaxCanonicalContextsPerEffect), "reduce contexts before retrying")
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
			return canonicalMutationError(fmt.Sprintf("effect[%d] input %s", index, name), fmt.Sprintf("raw length %d exceeds maximum %d before normalization", size, MaxCanonicalFieldBytes), "reduce the operand before retrying")
		}
	}
	for contextIndex, context := range effect.Contexts {
		kind, identity, err := EncodeStoredEventContext(context)
		if err != nil {
			return canonicalMutationError(fmt.Sprintf("effect[%d] context[%d] input", index, contextIndex), err.Error(), "supply a valid context identity")
		}
		if len(kind) > MaxCanonicalFieldBytes || len(identity) > MaxCanonicalFieldBytes {
			return canonicalMutationError(fmt.Sprintf("effect[%d] context[%d] input", index, contextIndex), "raw context field exceeds maximum length", "reduce the context identity")
		}
	}
	return nil
}

func validEffectSort(sort EffectSort) bool {
	_, ok := mutationV1Codec.familyTag(sort)
	return ok
}

func (canonicalV1Codec) familyTag(sort EffectSort) (string, bool) {
	for _, descriptor := range canonicalV1Families {
		if descriptor.sort == sort {
			return descriptor.tag, true
		}
	}
	return "", false
}

func (canonicalV1Codec) parseFamilyTag(tag string) (EffectSort, error) {
	for _, descriptor := range canonicalV1Families {
		if descriptor.tag == tag {
			return descriptor.sort, nil
		}
	}
	return 0, fmt.Errorf("provenance: decode canonical mutation: unknown effect family %q", tag)
}

func (canonicalV1Codec) canonicalJSON(raw json.RawMessage) ([]byte, error) {
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

type canonicalOptionalMarker byte

const (
	canonicalOptionalAbsent  canonicalOptionalMarker = '0'
	canonicalOptionalPresent canonicalOptionalMarker = '1'
	canonicalV1ZeroIdentity                          = "--00000000-0000-0000-0000-000000000000"
)

func optionalMarker(marker canonicalOptionalMarker) []byte { return []byte{byte(marker)} }

func (canonicalV1Codec) encodeOptionalString(value *string) []byte {
	if value == nil {
		return optionalMarker(canonicalOptionalAbsent)
	}
	return append(optionalMarker(canonicalOptionalPresent), []byte(*value)...)
}
func (canonicalV1Codec) decodeOptionalString(raw []byte) (*string, error) {
	if bytes.Equal(raw, optionalMarker(canonicalOptionalAbsent)) {
		return nil, nil
	}
	if len(raw) < 1 || canonicalOptionalMarker(raw[0]) != canonicalOptionalPresent {
		return nil, fmt.Errorf("invalid optional string marker")
	}
	value := string(raw[1:])
	return &value, nil
}
func encodeOptionalText[T interface{ String() string }](value *T) []byte {
	if value == nil {
		return optionalMarker(canonicalOptionalAbsent)
	}
	return append(optionalMarker(canonicalOptionalPresent), []byte((*value).String())...)
}
func (canonicalV1Codec) encodeOptionalPriority(value *Priority) []byte {
	return encodeOptionalText(value)
}
func (canonicalV1Codec) encodeOptionalPhase(value *Phase) []byte { return encodeOptionalText(value) }

func (canonicalV1Codec) encodeOptionalInt64(value *RecordedTime) []byte {
	if value == nil {
		return optionalMarker(canonicalOptionalAbsent)
	}
	return append(optionalMarker(canonicalOptionalPresent), strconv.AppendInt(nil, *value, 10)...)
}
func (canonicalV1Codec) decodeOptionalInt64(raw []byte) (*RecordedTime, error) {
	if bytes.Equal(raw, optionalMarker(canonicalOptionalAbsent)) {
		return nil, nil
	}
	if len(raw) < 2 || canonicalOptionalMarker(raw[0]) != canonicalOptionalPresent {
		return nil, fmt.Errorf("invalid optional timestamp marker")
	}
	value, err := strconv.ParseInt(string(raw[1:]), 10, 64)
	if err != nil {
		return nil, err
	}
	return &value, nil
}
func (canonicalV1Codec) decodeOptionalPriority(raw []byte) (*Priority, error) {
	if bytes.Equal(raw, optionalMarker(canonicalOptionalAbsent)) {
		return nil, nil
	}
	if len(raw) < 2 || canonicalOptionalMarker(raw[0]) != canonicalOptionalPresent {
		return nil, fmt.Errorf("invalid optional priority marker")
	}
	var value Priority
	if err := value.UnmarshalText(raw[1:]); err != nil {
		return nil, err
	}
	return &value, nil
}
func (canonicalV1Codec) decodeOptionalPhase(raw []byte) (*Phase, error) {
	if bytes.Equal(raw, optionalMarker(canonicalOptionalAbsent)) {
		return nil, nil
	}
	if len(raw) < 2 || canonicalOptionalMarker(raw[0]) != canonicalOptionalPresent {
		return nil, fmt.Errorf("invalid optional phase marker")
	}
	var value Phase
	if err := value.UnmarshalText(raw[1:]); err != nil {
		return nil, err
	}
	return &value, nil
}
func parseOptionalTask(raw string) (TaskID, error) {
	if raw == canonicalV1ZeroIdentity {
		return TaskID{}, nil
	}
	return ptypes.ParseTaskID(raw)
}
func parseOptionalActor(raw string) (ActorID, error) {
	if raw == canonicalV1ZeroIdentity {
		return ActorID{}, nil
	}
	return ptypes.ParseActorID(raw)
}
func parseOptionalComment(raw string) (CommentID, error) {
	if raw == canonicalV1ZeroIdentity {
		return CommentID{}, nil
	}
	return ptypes.ParseCommentID(raw)
}
