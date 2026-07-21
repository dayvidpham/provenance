package journal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// MutationEncodingV1 is the stable wire-version tag for canonical mutations.
const MutationEncodingV1 = "provenance.mutation.v1"

// CanonicalMutation is the prepared mutation consumed by persistence and execution.
// Bytes are authoritative: Effects are decoded from Bytes rather than retained from
// the caller, and Digest is SHA-256(Bytes).
type CanonicalMutation struct {
	Version string
	Bytes   []byte
	Digest  []byte
	Effects []Effect
}

var canonicalEffectSorts = []EffectSort{
	EffectTaskEvent, EffectBootstrapAuthority, EffectAssignmentStart,
	EffectAssignmentEnd, EffectDecision, EffectEvidence, EffectTaskCreate,
	EffectEdgeAdd, EffectEdgeRemove, EffectLabelAdd, EffectLabelRemove,
	EffectCommentAdd,
}

// PrepareMutationV1 validates and normalizes effects, writes their canonical bytes,
// then decodes those bytes once. The returned decoded effects are the only effects a
// write path should execute.
func PrepareMutationV1(effects []Effect) (CanonicalMutation, error) {
	var out bytes.Buffer
	w := canonicalWriter{w: &out}
	w.field("version", []byte(MutationEncodingV1))
	w.field("effect-count", []byte(strconv.Itoa(len(effects))))
	for i := range effects {
		if err := encodeCanonicalEffect(&w, effects[i], i); err != nil {
			return CanonicalMutation{}, err
		}
	}
	if w.err != nil {
		return CanonicalMutation{}, fmt.Errorf("provenance: encode canonical mutation: %w", w.err)
	}
	return DecodeCanonicalMutation(out.Bytes())
}

// DecodeCanonicalMutation strictly decodes one complete canonical mutation. Field
// order is fixed, so unknown, missing, duplicate, and trailing fields all fail closed.
func DecodeCanonicalMutation(data []byte) (CanonicalMutation, error) {
	r := canonicalReader{r: bufio.NewReader(bytes.NewReader(data))}
	version, err := r.field("version")
	if err != nil {
		return CanonicalMutation{}, err
	}
	if string(version) != MutationEncodingV1 {
		return CanonicalMutation{}, fmt.Errorf("provenance: decode canonical mutation: unsupported version %q; fix: use %q", version, MutationEncodingV1)
	}
	rawCount, err := r.field("effect-count")
	if err != nil {
		return CanonicalMutation{}, err
	}
	count, err := strconv.Atoi(string(rawCount))
	if err != nil || count < 0 {
		return CanonicalMutation{}, fmt.Errorf("provenance: decode canonical mutation: invalid effect-count %q", rawCount)
	}
	effects := make([]Effect, count)
	for i := range effects {
		effects[i], err = decodeCanonicalEffect(&r, i)
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
		Version: MutationEncodingV1,
		Bytes:   append([]byte(nil), data...),
		Digest:  append([]byte(nil), digest[:]...),
		Effects: effects,
	}, nil
}

type canonicalWriter struct {
	w   io.Writer
	err error
}

func (w *canonicalWriter) field(name string, value []byte) {
	if w.err != nil {
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
	if err != nil || n < 0 {
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
	if !validEffectSort(e.Sort) {
		return fmt.Errorf("provenance: canonical mutation effect %d has unknown sort %d", index, int(e.Sort))
	}
	contexts, err := CanonicalEventContexts(e.Contexts)
	if err != nil {
		return fmt.Errorf("provenance: canonical mutation effect %d contexts: %w", index, err)
	}
	payload, err := canonicalJSON(e.Payload)
	if err != nil {
		return fmt.Errorf("provenance: canonical mutation effect %d payload: %w", index, err)
	}
	prefix := fmt.Sprintf("effect.%d.", index)
	w.field(prefix+"family", []byte(e.Sort.String()))
	w.field(prefix+"result-slot", []byte(e.ResultSlot))
	w.field(prefix+"actor", []byte(idString(e.ActorID)))
	w.field(prefix+"recorded-at-override", encodeOptionalInt64(e.RecordedAtOverride))
	w.field(prefix+"task", []byte(idString(e.TaskID)))
	w.field(prefix+"event-kind", []byte(e.EventKind))
	w.field(prefix+"payload", payload)
	w.field(prefix+"context-count", []byte(strconv.Itoa(len(contexts))))
	for j, context := range contexts {
		kind, identity, _ := EncodeStoredEventContext(context)
		w.field(fmt.Sprintf("%scontext.%d.kind", prefix, j), []byte(kind))
		w.field(fmt.Sprintf("%scontext.%d.identity", prefix, j), []byte(identity))
	}
	w.field(prefix+"title", []byte(e.Title))
	w.field(prefix+"description", []byte(e.Description))
	w.field(prefix+"type", []byte(e.Type.String()))
	w.field(prefix+"priority", []byte(e.Priority.String()))
	w.field(prefix+"phase", []byte(e.Phase.String()))
	w.field(prefix+"close-reason", []byte(e.CloseReason))
	w.field(prefix+"update-title", encodeOptionalString(e.UpdateTitle))
	w.field(prefix+"update-description", encodeOptionalString(e.UpdateDescription))
	w.field(prefix+"update-priority", encodeOptionalText(e.UpdatePriority))
	w.field(prefix+"update-phase", encodeOptionalText(e.UpdatePhase))
	w.field(prefix+"update-notes", encodeOptionalString(e.UpdateNotes))
	w.field(prefix+"forced", []byte(strconv.FormatBool(e.Forced)))
	w.field(prefix+"bootstrap-label", []byte(e.BootstrapLabel))
	w.field(prefix+"operation-authority", []byte(e.OperationAuthorityID))
	w.field(prefix+"assignment", []byte(e.AssignmentID))
	w.field(prefix+"slot", []byte(e.SlotID))
	w.field(prefix+"occupant", []byte(idString(e.Occupant)))
	w.field(prefix+"predecessor", []byte(e.Predecessor))
	w.field(prefix+"parent", []byte(e.Parent))
	w.field(prefix+"decision-kind", []byte(e.DecisionKind))
	w.field(prefix+"evidence-kind", []byte(e.EvidenceKind))
	w.field(prefix+"content-digest", e.ContentDigest)
	w.field(prefix+"edge-target", []byte(e.EdgeTargetID))
	w.field(prefix+"edge-kind", []byte(e.EdgeRelKind.String()))
	w.field(prefix+"label", []byte(e.Label))
	w.field(prefix+"comment", []byte(idString(e.CommentIdentity)))
	w.field(prefix+"comment-author", []byte(idString(e.CommentAuthor)))
	w.field(prefix+"comment-body", []byte(e.CommentBody))
	return nil
}

func decodeCanonicalEffect(r *canonicalReader, index int) (Effect, error) {
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
	var e Effect
	e.Sort = sort
	if b, x := read("result-slot"); x == nil {
		e.ResultSlot = ResultSlotID(b)
	} else {
		return e, x
	}
	if b, x := read("actor"); x == nil {
		e.ActorID, err = parseOptionalActor(string(b))
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("recorded-at-override"); x == nil {
		e.RecordedAtOverride, err = decodeOptionalInt64(b)
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("task"); x == nil {
		e.TaskID, err = parseOptionalTask(string(b))
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("event-kind"); x == nil {
		e.EventKind = EventKind(b)
	} else {
		return e, x
	}
	if b, x := read("payload"); x == nil {
		e.Payload = append(json.RawMessage(nil), b...)
	} else {
		return e, x
	}
	countRaw, err := read("context-count")
	if err != nil {
		return e, err
	}
	count, err := strconv.Atoi(string(countRaw))
	if err != nil || count < 0 {
		return e, fmt.Errorf("provenance: canonical mutation effect %d invalid context-count %q", index, countRaw)
	}
	for j := 0; j < count; j++ {
		kind, x := r.field(fmt.Sprintf("%scontext.%d.kind", p, j))
		if x != nil {
			return e, x
		}
		identity, x := r.field(fmt.Sprintf("%scontext.%d.identity", p, j))
		if x != nil {
			return e, x
		}
		context, x := DecodeStoredEventContext(EventContextKind(kind), string(identity))
		if x != nil {
			return e, x
		}
		e.Contexts = append(e.Contexts, context)
	}
	if b, x := read("title"); x == nil {
		e.Title = string(b)
	} else {
		return e, x
	}
	if b, x := read("description"); x == nil {
		e.Description = string(b)
	} else {
		return e, x
	}
	if b, x := read("type"); x == nil {
		err = e.Type.UnmarshalText(b)
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("priority"); x == nil {
		err = e.Priority.UnmarshalText(b)
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("phase"); x == nil {
		err = e.Phase.UnmarshalText(b)
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("close-reason"); x == nil {
		e.CloseReason = string(b)
	} else {
		return e, x
	}
	if b, x := read("update-title"); x == nil {
		e.UpdateTitle, err = decodeOptionalString(b)
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("update-description"); x == nil {
		e.UpdateDescription, err = decodeOptionalString(b)
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("update-priority"); x == nil {
		e.UpdatePriority, err = decodeOptionalPriority(b)
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("update-phase"); x == nil {
		e.UpdatePhase, err = decodeOptionalPhase(b)
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("update-notes"); x == nil {
		e.UpdateNotes, err = decodeOptionalString(b)
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("forced"); x == nil {
		e.Forced, err = strconv.ParseBool(string(b))
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("bootstrap-label"); x == nil {
		e.BootstrapLabel = string(b)
	} else {
		return e, x
	}
	if b, x := read("operation-authority"); x == nil {
		e.OperationAuthorityID = OperationAuthorityID(b)
	} else {
		return e, x
	}
	if b, x := read("assignment"); x == nil {
		e.AssignmentID = AssignmentID(b)
	} else {
		return e, x
	}
	if b, x := read("slot"); x == nil {
		e.SlotID = AssignmentSlotID(b)
	} else {
		return e, x
	}
	if b, x := read("occupant"); x == nil {
		e.Occupant, err = parseOptionalActor(string(b))
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("predecessor"); x == nil {
		e.Predecessor = AssignmentID(b)
	} else {
		return e, x
	}
	if b, x := read("parent"); x == nil {
		e.Parent = AssignmentID(b)
	} else {
		return e, x
	}
	if b, x := read("decision-kind"); x == nil {
		e.DecisionKind = DecisionKind(b)
	} else {
		return e, x
	}
	if b, x := read("evidence-kind"); x == nil {
		e.EvidenceKind = EvidenceKind(b)
	} else {
		return e, x
	}
	if b, x := read("content-digest"); x == nil {
		e.ContentDigest = append([]byte(nil), b...)
	} else {
		return e, x
	}
	if b, x := read("edge-target"); x == nil {
		e.EdgeTargetID = string(b)
	} else {
		return e, x
	}
	if b, x := read("edge-kind"); x == nil {
		err = e.EdgeRelKind.UnmarshalText(b)
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("label"); x == nil {
		e.Label = string(b)
	} else {
		return e, x
	}
	if b, x := read("comment"); x == nil {
		e.CommentIdentity, err = parseOptionalComment(string(b))
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("comment-author"); x == nil {
		e.CommentAuthor, err = parseOptionalActor(string(b))
	} else {
		return e, x
	}
	if err != nil {
		return e, err
	}
	if b, x := read("comment-body"); x == nil {
		e.CommentBody = string(b)
	} else {
		return e, x
	}
	return e, nil
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
