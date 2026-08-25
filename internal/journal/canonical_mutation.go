package journal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// MutationEncodingVersion identifies the one canonical mutation representation.
type MutationEncodingVersion uint8

const (
	MutationEncodingV1 MutationEncodingVersion = iota + 1
)

type inspectedMutationEncodingTag struct{ text string }

func (v MutationEncodingVersion) String() string {
	if v == MutationEncodingV1 {
		return canonicalMutationV1WireTag
	}
	return fmt.Sprintf("MutationEncodingVersion(%d)", v)
}

type canonicalCodecDescriptor struct {
	version MutationEncodingVersion
	wireTag string
}

const canonicalMutationV1WireTag = "provenance.mutation.v1"

var canonicalV1Descriptor = canonicalCodecDescriptor{version: MutationEncodingV1, wireTag: canonicalMutationV1WireTag}

func inspectMutationEncodingTag(text string) inspectedMutationEncodingTag {
	return inspectedMutationEncodingTag{text: text}
}

func (tag inspectedMutationEncodingTag) version() (MutationEncodingVersion, bool) {
	return MutationEncodingV1, tag.text == canonicalMutationV1WireTag
}

// MatchesStoredText compares an opaque inspected wire tag with the redundant
// SQLite text column without exposing the tag as a protocol string.
func (tag inspectedMutationEncodingTag) MatchesStoredText(stored string) bool {
	return tag.text == stored
}

// RegisteredVersion reports the sole supported evolved V1 tag.
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
	version    MutationEncodingVersion
	bytes      []byte
	digest     []byte
	effects    []Effect
	conditions []Condition
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
func (m CanonicalMutation) NormalizedConditions() []Condition {
	out := make([]Condition, len(m.conditions))
	for i := range out {
		out[i] = cloneCanonicalCondition(m.conditions[i], i)
	}
	return out
}

type canonicalV1Codec struct{}

var mutationV1Codec canonicalV1Codec

type canonicalEnvelopeField uint8

const (
	envelopeVersion canonicalEnvelopeField = iota + 1
	envelopeEffectCount
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
		if typed.field != effectFamily {
			return "", canonicalMutationError("field-reference", fmt.Sprintf("unknown V1 effect field %d", typed.field), "use a declared V1 effect field")
		}
		return effectWireName(typed.index, "family"), nil
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

// Canonicalize is the sole public preparation boundary. It validates and normalizes
// the OperationInput, writes the canonical V1 bytes, and strictly decodes them
// once to produce the CanonicalMutation the write path must execute. No caller
// outside this package should call prepareMutationV1 or prepareMutationV1Operation directly.
func Canonicalize(in OperationInput) (CanonicalMutation, error) {
	return prepareMutationV1Operation(in, canonicalV1Descriptor)
}

func prepareMutationV1(effects []Effect, descriptor canonicalCodecDescriptor) (CanonicalMutation, error) {
	return prepareMutationV1Operation(OperationInput{Effects: effects}, descriptor)
}

func prepareMutationV1Operation(in OperationInput, descriptor canonicalCodecDescriptor) (CanonicalMutation, error) {
	effects := in.Effects
	if len(effects) > MaxCanonicalEffects {
		return CanonicalMutation{}, canonicalMutationError("effect-count", fmt.Sprintf("%d exceeds maximum %d", len(effects), MaxCanonicalEffects), "split the operation into bounded mutations")
	}
	normalized := make([]Effect, len(effects))
	if err := validateSemanticResultSlots(effects); err != nil {
		return CanonicalMutation{}, err
	}
	counter := &canonicalSizeCounter{limit: MaxCanonicalMutationBytes}
	w := canonicalWriter{codec: mutationV1Codec, w: counter}
	conditions, err := normalizeConditions(in.Conditions)
	if err != nil {
		return CanonicalMutation{}, err
	}
	writeCanonicalEnvelopeHeader(&w, len(conditions), len(effects), descriptor.wireTag)
	for i := range conditions {
		encodeSemanticCondition(&w, conditions[i], i)
	}
	w.field(envelopeField(envelopeEffectCount), []byte(strconv.Itoa(len(effects))))
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
	writeCanonicalEnvelopeHeader(&w, len(conditions), len(normalized), descriptor.wireTag)
	for i := range conditions {
		encodeSemanticCondition(&w, conditions[i], i)
	}
	w.field(envelopeField(envelopeEffectCount), []byte(strconv.Itoa(len(normalized))))
	for i := range normalized {
		if err := mutationV1Codec.encodeEffect(&w, normalized[i], i); err != nil {
			return CanonicalMutation{}, err
		}
	}
	if w.err != nil {
		return CanonicalMutation{}, fmt.Errorf("provenance: encode bounded canonical mutation: %w", w.err)
	}
	return decodeCanonicalMutationV1(out.Bytes(), descriptor.version, descriptor.wireTag)
}

func validateResultSlotID(slot ResultSlotID) error {
	for _, r := range string(slot) {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("contains a control character")
		}
	}
	return nil
}

func writeCanonicalEnvelopeHeader(w *canonicalWriter, conditionCount, effectCount int, wireTag string) {
	w.field(envelopeField(envelopeVersion), []byte(wireTag))
	w.rawField("condition-count", []byte(strconv.Itoa(conditionCount)))
	_ = effectCount
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
	if len(data) > MaxCanonicalMutationBytes {
		return CanonicalMutation{}, canonicalMutationError("mutation", fmt.Sprintf("%d bytes exceeds maximum %d", len(data), MaxCanonicalMutationBytes), "restore bounded evolved V1 bytes")
	}
	mutation, err := decodeCanonicalMutationV1(data, MutationEncodingV1, canonicalMutationV1WireTag)
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

func decodeCanonicalMutationV1(data []byte, versionID MutationEncodingVersion, wireTag string) (CanonicalMutation, error) {
	r := canonicalReader{codec: mutationV1Codec, r: bufio.NewReader(bytes.NewReader(data))}
	version, err := r.field(envelopeField(envelopeVersion))
	if err != nil || versionID != MutationEncodingV1 || string(version) != wireTag {
		return CanonicalMutation{}, fmt.Errorf("provenance: decode V1 canonical mutation: invalid version frame %q: %w", version, err)
	}
	rawConditionCount, err := r.rawField("condition-count")
	if err != nil {
		return CanonicalMutation{}, err
	}
	conditionCount, err := parseBoundedCount(rawConditionCount, MaxCanonicalConditions, "condition-count")
	if err != nil {
		return CanonicalMutation{}, err
	}
	conditions := make([]Condition, conditionCount)
	for i := range conditions {
		conditions[i], err = decodeSemanticCondition(&r, i)
		if err != nil {
			return CanonicalMutation{}, err
		}
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
	reencoded, err := encodeNormalizedMutation(conditions, effects)
	if err != nil {
		return CanonicalMutation{}, err
	}
	if !bytes.Equal(reencoded, data) {
		return CanonicalMutation{}, canonicalMutationError("wire", "decoded semantics do not re-encode to the identical canonical bytes", "use sorted unique collections, canonical JSON/scalars, and the exact evolved V1 field representation")
	}
	digest := sha256.Sum256(data)
	return CanonicalMutation{
		version:    versionID,
		bytes:      append([]byte(nil), data...),
		digest:     append([]byte(nil), digest[:]...),
		effects:    effects,
		conditions: conditions,
	}, nil
}

func encodeNormalizedMutation(conditions []Condition, effects []Effect) ([]byte, error) {
	counter := &canonicalSizeCounter{limit: MaxCanonicalMutationBytes}
	w := canonicalWriter{codec: mutationV1Codec, w: counter}
	writeCanonicalEnvelopeHeader(&w, len(conditions), len(effects), canonicalMutationV1WireTag)
	for i := range conditions {
		encodeSemanticCondition(&w, conditions[i], i)
	}
	w.field(envelopeField(envelopeEffectCount), []byte(strconv.Itoa(len(effects))))
	for i := range effects {
		if err := mutationV1Codec.encodeEffect(&w, effects[i], i); err != nil {
			return nil, err
		}
	}
	if w.err != nil {
		return nil, w.err
	}
	var out bytes.Buffer
	out.Grow(counter.size)
	w = canonicalWriter{codec: mutationV1Codec, w: &out}
	writeCanonicalEnvelopeHeader(&w, len(conditions), len(effects), canonicalMutationV1WireTag)
	for i := range conditions {
		encodeSemanticCondition(&w, conditions[i], i)
	}
	w.field(envelopeField(envelopeEffectCount), []byte(strconv.Itoa(len(effects))))
	for i := range effects {
		if err := mutationV1Codec.encodeEffect(&w, effects[i], i); err != nil {
			return nil, err
		}
	}
	if w.err != nil {
		return nil, w.err
	}
	return out.Bytes(), nil
}

// IsSupportedMutationEncoding reports only the intentionally evolved V1.
func IsSupportedMutationEncoding(version MutationEncodingVersion) bool {
	return version == MutationEncodingV1
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

func (w *canonicalWriter) rawField(name string, value []byte) {
	if w.err != nil {
		return
	}
	if len(value) > MaxCanonicalFieldBytes {
		w.err = canonicalMutationError(name, "field exceeds maximum size", "reduce this operand")
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

func (r *canonicalReader) rawField(want string) ([]byte, error) {
	name, err := r.r.ReadString(':')
	if err != nil {
		return nil, fmt.Errorf("missing field %q: %w", want, err)
	}
	if strings.TrimSuffix(name, ":") != want {
		return nil, fmt.Errorf("expected field %q, found %q", want, strings.TrimSuffix(name, ":"))
	}
	rawLen, err := r.r.ReadString(':')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSuffix(rawLen, ":"))
	if err != nil || n < 0 || n > MaxCanonicalFieldBytes {
		return nil, fmt.Errorf("field %q has invalid bounded length", want)
	}
	value := make([]byte, n)
	if _, err := io.ReadFull(r.r, value); err != nil {
		return nil, err
	}
	if term, err := r.r.ReadByte(); err != nil || term != '\n' {
		return nil, fmt.Errorf("field %q has invalid terminator", want)
	}
	return value, nil
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

func normalizeConditions(in []Condition) ([]Condition, error) {
	if len(in) > MaxCanonicalConditions {
		return nil, canonicalMutationError("condition-count", fmt.Sprintf("%d exceeds maximum %d", len(in), MaxCanonicalConditions), "split the operation or reduce its conditions")
	}
	out := make([]Condition, len(in))
	for i := range in {
		var err error
		out[i], err = normalizeCondition(in[i], i)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func conditionName(index int, field string) string {
	return fmt.Sprintf("condition.%d.%s", index, field)
}

func parseBoundedCount(raw []byte, maximum int, field string) (int, error) {
	value, err := strconv.Atoi(string(raw))
	if err != nil || value < 0 || value > maximum {
		return 0, canonicalMutationError(field, fmt.Sprintf("invalid bounded count %q", raw), fmt.Sprintf("use a count from 0 through %d", maximum))
	}
	return value, nil
}

func equalContexts(a, b []EventContext) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
