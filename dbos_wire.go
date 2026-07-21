package provenance

// dbos_wire.go is the canonical, deterministic serialization bridge between a
// validated in-memory journal.OperationInput and the closed DBOSApplyInputV1 the
// DBOS durable-workflow layer persists and re-runs on recovery (issue
// dayvidpham/provenance#6). DBOS re-executes a registered workflow from its
// PERSISTED input on recovery, so the whole OperationInput — identity plus the
// ordered effect list — must survive an encode/decode round trip byte-for-byte.
//
// A journal.EventContext is an opaque value with unexported fields and no public
// JSON decoder (internal/journal/context.go), so an Effect cannot be marshaled
// through the public API. This file lives in package provenance, which imports
// internal/journal, and therefore round-trips each context through the internal
// EncodeStoredEventContext / DecodeStoredEventContext persistence codec — the one
// validated inverse pair — reconstructing an identical, revalidated EventContext.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// DBOSApplyInputSchemaV1 is the closed schema tag stamped on every
// DBOSApplyInputV1; a decoded input carrying any other tag fails closed.
const DBOSApplyInputSchemaV1 = "provenance.dbos-apply-input/v1"

const (
	DBOSApplyInputSchemaV2 = "provenance.dbos-apply-input/v2"
	dbosContextSchemaV2    = "provenance.dbos-context/v2"
	maxDBOSContextBytes    = 3*MaxCanonicalFieldBytes + 64
)

var ErrDBOSContextFrame = errors.New("provenance: invalid DBOS V2 context frame")

// DBOSContextFrameError is the uniform typed recovery diagnostic for malformed
// V2 context bytes. No workflow fold has run when this error is returned.
type DBOSContextFrameError struct {
	Field  string
	Reason string
	Fix    string
}

func (e *DBOSContextFrameError) Error() string {
	return fmt.Sprintf("%v: what: V2 context field %s is invalid; why: %s; where: DBOS V2 context codec; when: before workflow identity or domain fold; impact: the persisted input is rejected and no domain write runs; fix: %s", ErrDBOSContextFrame, e.Field, e.Reason, e.Fix)
}

func (e *DBOSContextFrameError) Is(target error) bool { return target == ErrDBOSContextFrame }

func dbosContextFrameError(field, reason, fix string) error {
	return &DBOSContextFrameError{Field: field, Reason: reason, Fix: fix}
}

// CanonicalMutationContextV1 is the deterministic byte encoding of one operation's
// replay-identity context (OperationID, committing actor, governing authority,
// command digest, and audit RecordedAt). It is the §9.4 alternate-key half of the
// input, distinct from the structural mutation.
type CanonicalMutationContextV1 []byte

// CanonicalMutationBytes is the deterministic byte encoding of one operation's
// structural mutation: its MutationDigest and its ordered effect list (§9.3.1).
type CanonicalMutationBytes []byte

// DBOSApplyInputV1 is the closed, serializable workflow input the adapter
// canonicalizes a journal.OperationInput into before durable execution. Its two
// opaque byte fields are produced only by encodeApplyInput and consumed only by
// decodeApplyInput, so the DBOS layer never interprets Provenance identity.
type DBOSApplyInputV1 struct {
	Schema   string                     `json:"schema"`
	Context  CanonicalMutationContextV1 `json:"context"`
	Mutation CanonicalMutationBytes     `json:"mutation"`
}

// DBOSApplyInputV2 transports the reviewed canonical mutation bytes directly.
// Context uses a closed ordered frame rather than JSON so missing, duplicate,
// unknown, and trailing fields cannot be silently accepted by DBOS's serializer.
type DBOSApplyInputV2 struct {
	Schema   string                     `json:"schema"`
	Context  CanonicalMutationContextV1 `json:"context"`
	Mutation CanonicalMutationBytes     `json:"mutation"`
}

func encodeApplyInputV2(in journal.OperationInput) (DBOSApplyInputV2, journal.OperationInput, error) {
	prepared, err := journal.PrepareMutationV1(in.Effects)
	if err != nil {
		return DBOSApplyInputV2{}, journal.OperationInput{}, err
	}
	in.Effects = prepared.NormalizedEffects()
	in.MutationDigest = prepared.DerivedDigest()
	contextBytes, err := encodeDBOSContextV2(in)
	if err != nil {
		return DBOSApplyInputV2{}, journal.OperationInput{}, err
	}
	return DBOSApplyInputV2{
		Schema: DBOSApplyInputSchemaV2, Context: contextBytes,
		Mutation: prepared.CanonicalBytes(),
	}, in, nil
}

func decodeApplyInputV2(input DBOSApplyInputV2) (journal.OperationInput, error) {
	if input.Schema != DBOSApplyInputSchemaV2 {
		return journal.OperationInput{}, fmt.Errorf("provenance: decode V2 apply-input: schema %q is not %q — where: DBOS V2 workflow decode; impact: no domain fold runs; fix: restore the persisted input written by DBOSAdapter.Apply", input.Schema, DBOSApplyInputSchemaV2)
	}
	in, err := decodeDBOSContextV2(input.Context)
	if err != nil {
		return journal.OperationInput{}, err
	}
	prepared, err := journal.DecodeCanonicalMutation(input.Mutation)
	if err != nil {
		return journal.OperationInput{}, fmt.Errorf("provenance: decode V2 apply-input canonical mutation: %w", err)
	}
	in.Effects = prepared.NormalizedEffects()
	in.MutationDigest = prepared.DerivedDigest()
	return in, nil
}

func encodeDBOSContextV2(in journal.OperationInput) ([]byte, error) {
	if err := journal.ValidateOperationID(in.OperationID); err != nil {
		return nil, dbosContextFrameError("operation", err.Error(), "supply a non-empty control-free OperationID")
	}
	actor := actorToWire(in.ActorID)
	if _, err := actorFromWire(actor); err != nil {
		return nil, dbosContextFrameError("actor", err.Error(), "supply a canonical non-zero actor identity")
	}
	fields := [][]byte{[]byte(dbosContextSchemaV2), []byte(in.OperationID), []byte(actor)}
	if in.AuthorityJournalID == nil {
		fields = append(fields, []byte{0})
	} else {
		var authority [9]byte
		authority[0] = 1
		binary.BigEndian.PutUint64(authority[1:], uint64(*in.AuthorityJournalID))
		fields = append(fields, authority[:])
	}
	fields = append(fields, append([]byte(nil), in.CommandDigest...))
	var recorded [8]byte
	binary.BigEndian.PutUint64(recorded[:], uint64(in.RecordedAt))
	fields = append(fields, recorded[:])
	var out bytes.Buffer
	for i, field := range fields {
		if len(field) > MaxCanonicalFieldBytes {
			return nil, dbosContextFrameError(fmt.Sprintf("field.%d", i), fmt.Sprintf("%d bytes exceeds maximum %d", len(field), MaxCanonicalFieldBytes), "use bounded operation identity and command digest operands")
		}
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		out.Write(size[:])
		out.Write(field)
	}
	if out.Len() > maxDBOSContextBytes {
		return nil, dbosContextFrameError("context", fmt.Sprintf("%d bytes exceeds maximum %d", out.Len(), maxDBOSContextBytes), "reduce context operands below the aggregate bound")
	}
	return out.Bytes(), nil
}

func decodeDBOSContextV2(data []byte) (journal.OperationInput, error) {
	if len(data) > maxDBOSContextBytes {
		return journal.OperationInput{}, dbosContextFrameError("context", fmt.Sprintf("%d bytes exceeds maximum %d", len(data), maxDBOSContextBytes), "restore bounded persisted context bytes from the original workflow input")
	}
	r := bytes.NewReader(data)
	read := func(name string) ([]byte, error) {
		var size uint32
		if err := binary.Read(r, binary.BigEndian, &size); err != nil {
			return nil, dbosContextFrameError(name, "missing or truncated length prefix: "+err.Error(), "restore the complete ordered V2 context frame")
		}
		if size > MaxCanonicalFieldBytes || uint64(size) > uint64(r.Len()) {
			return nil, dbosContextFrameError(name, fmt.Sprintf("declared length %d exceeds the field bound or remaining %d bytes", size, r.Len()), "restore the field length and bytes from the original persisted input")
		}
		value := make([]byte, int(size))
		if _, err := io.ReadFull(r, value); err != nil {
			return nil, dbosContextFrameError(name, "truncated field bytes: "+err.Error(), "restore the complete field bytes from the original persisted input")
		}
		return value, nil
	}
	names := []string{"version", "operation", "actor", "authority", "command", "recorded-at"}
	values := make([][]byte, len(names))
	for i := range names {
		var err error
		values[i], err = read(names[i])
		if err != nil {
			return journal.OperationInput{}, err
		}
	}
	if r.Len() != 0 {
		return journal.OperationInput{}, dbosContextFrameError("trailing", fmt.Sprintf("%d bytes remain after the six closed fields", r.Len()), "remove unknown or duplicate fields by restoring the exact original V2 frame")
	}
	if string(values[0]) != dbosContextSchemaV2 {
		return journal.OperationInput{}, dbosContextFrameError("version", fmt.Sprintf("unsupported version %q", values[0]), "recover with a build that supports the persisted version or restore a V2 frame")
	}
	operation := journal.OperationID(values[1])
	if err := journal.ValidateOperationID(operation); err != nil {
		return journal.OperationInput{}, dbosContextFrameError("operation", err.Error(), "restore the original non-empty control-free OperationID")
	}
	actor, err := actorFromWire(string(values[2]))
	if err != nil {
		return journal.OperationInput{}, dbosContextFrameError("actor", err.Error(), "restore the original canonical actor identity")
	}
	var authority *journal.JournalID
	switch len(values[3]) {
	case 1:
		if values[3][0] != 0 {
			return journal.OperationInput{}, dbosContextFrameError("authority", fmt.Sprintf("invalid absent tag %d", values[3][0]), "restore tag 0 for genesis or tag 1 plus eight bytes for a journal authority")
		}
	case 9:
		if values[3][0] != 1 {
			return journal.OperationInput{}, dbosContextFrameError("authority", fmt.Sprintf("invalid present tag %d", values[3][0]), "restore tag 1 followed by the original eight-byte journal authority")
		}
		value := journal.JournalID(int64(binary.BigEndian.Uint64(values[3][1:])))
		authority = &value
	default:
		return journal.OperationInput{}, dbosContextFrameError("authority", fmt.Sprintf("length %d is neither 1 nor 9", len(values[3])), "restore the one-byte absent form or nine-byte present form")
	}
	if len(values[5]) != 8 {
		return journal.OperationInput{}, dbosContextFrameError("recorded-at", fmt.Sprintf("length %d is not 8", len(values[5])), "restore the original eight-byte audit timestamp")
	}
	return journal.OperationInput{OperationID: operation, ActorID: actor, AuthorityJournalID: authority,
		CommandDigest: append([]byte(nil), values[4]...), RecordedAt: int64(binary.BigEndian.Uint64(values[5]))}, nil
}

// wireContext is the JSON-stable form of an operation's replay-identity context.
type wireContext struct {
	OperationID        string `json:"op"`
	ActorID            string `json:"actor"`
	AuthorityJournalID *int64 `json:"authority"`
	CommandDigest      []byte `json:"command"`
	RecordedAt         int64  `json:"recorded_at"`
}

// wireMutation is the JSON-stable form of an operation's structural mutation.
type wireMutation struct {
	MutationDigest []byte       `json:"mutation_digest"`
	Effects        []wireEffect `json:"effects"`
}

// wireEventContext is the (kind, identity) persistence pair of one EventContext,
// the only form through which a context can leave and re-enter the typed domain.
type wireEventContext struct {
	Kind     string `json:"k"`
	Identity string `json:"i"`
}

// wireEffect mirrors journal.Effect field-for-field in a JSON-stable shape. Typed
// struct IDs are carried as their canonical String() form (empty string for the
// zero ID) and revalidated on decode; scalar and pointer fields carry directly.
type wireEffect struct {
	Sort               int                `json:"sort"`
	ResultSlot         string             `json:"slot,omitempty"`
	ActorID            string             `json:"actor,omitempty"`
	RecordedAtOverride *int64             `json:"rao,omitempty"`
	TaskID             string             `json:"task,omitempty"`
	EventKind          string             `json:"ek,omitempty"`
	Payload            json.RawMessage    `json:"payload,omitempty"`
	Contexts           []wireEventContext `json:"ctx,omitempty"`
	Title              string             `json:"title,omitempty"`
	Description        string             `json:"desc,omitempty"`
	Type               int                `json:"type,omitempty"`
	Priority           int                `json:"prio,omitempty"`
	Phase              int                `json:"phase,omitempty"`
	CloseReason        string             `json:"close,omitempty"`
	UpdateTitle        *string            `json:"utitle,omitempty"`
	UpdateDescription  *string            `json:"udesc,omitempty"`
	UpdatePriority     *int               `json:"uprio,omitempty"`
	UpdatePhase        *int               `json:"uphase,omitempty"`
	UpdateNotes        *string            `json:"unotes,omitempty"`
	BootstrapLabel     string             `json:"blabel,omitempty"`
	Authority          string             `json:"oauth,omitempty"`
	AssignmentID       string             `json:"asg,omitempty"`
	SlotID             string             `json:"slotid,omitempty"`
	Occupant           string             `json:"occ,omitempty"`
	Predecessor        string             `json:"pred,omitempty"`
	Parent             string             `json:"parent,omitempty"`
	DecisionKind       string             `json:"dk,omitempty"`
	EvidenceKind       string             `json:"evk,omitempty"`
	ContentDigest      []byte             `json:"cd,omitempty"`
}

// encodeApplyInput canonicalizes a validated OperationInput into the closed
// DBOSApplyInputV1. It is a pure structural projection: it computes no fingerprint
// and evaluates no authority/state predicate (that is the reducer's job at fold
// time). It never mutates in.
func encodeApplyInput(in journal.OperationInput) (DBOSApplyInputV1, error) {
	ctxBytes, err := json.Marshal(wireContext{
		OperationID:        string(in.OperationID),
		ActorID:            actorToWire(in.ActorID),
		AuthorityJournalID: (*int64)(in.AuthorityJournalID),
		CommandDigest:      in.CommandDigest,
		RecordedAt:         in.RecordedAt,
	})
	if err != nil {
		return DBOSApplyInputV1{}, fmt.Errorf("provenance: encode apply-input context: %w", err)
	}
	effects := make([]wireEffect, len(in.Effects))
	for i := range in.Effects {
		we, encErr := encodeEffect(in.Effects[i])
		if encErr != nil {
			return DBOSApplyInputV1{}, fmt.Errorf("provenance: encode apply-input effect %d: %w", i, encErr)
		}
		effects[i] = we
	}
	mutBytes, err := json.Marshal(wireMutation{MutationDigest: in.MutationDigest, Effects: effects})
	if err != nil {
		return DBOSApplyInputV1{}, fmt.Errorf("provenance: encode apply-input mutation: %w", err)
	}
	return DBOSApplyInputV1{
		Schema:   DBOSApplyInputSchemaV1,
		Context:  ctxBytes,
		Mutation: mutBytes,
	}, nil
}

// decodeApplyInput is the exact inverse of encodeApplyInput. It fails closed on a
// wrong schema tag or any ID/context that no longer validates, so a corrupted or
// forged persisted input surfaces as an actionable decode error rather than a
// silently mis-shaped fold.
func decodeApplyInput(input DBOSApplyInputV1) (journal.OperationInput, error) {
	if input.Schema != DBOSApplyInputSchemaV1 {
		return journal.OperationInput{}, fmt.Errorf(
			"provenance: decode apply-input — schema tag %q is not %q — where: DBOS workflow "+
				"decode; when: before the domain fold; impact: nothing is applied; fix: re-canonicalize "+
				"the operation through DBOSAdapter.Apply so it carries the pinned schema",
			input.Schema, DBOSApplyInputSchemaV1)
	}
	var wc wireContext
	if err := json.Unmarshal(input.Context, &wc); err != nil {
		return journal.OperationInput{}, fmt.Errorf("provenance: decode apply-input context: %w", err)
	}
	var wm wireMutation
	if err := json.Unmarshal(input.Mutation, &wm); err != nil {
		return journal.OperationInput{}, fmt.Errorf("provenance: decode apply-input mutation: %w", err)
	}
	actor, err := actorFromWire(wc.ActorID)
	if err != nil {
		return journal.OperationInput{}, fmt.Errorf("provenance: decode apply-input actor: %w", err)
	}
	effects := make([]journal.Effect, len(wm.Effects))
	for i := range wm.Effects {
		e, decErr := decodeEffect(wm.Effects[i])
		if decErr != nil {
			return journal.OperationInput{}, fmt.Errorf("provenance: decode apply-input effect %d: %w", i, decErr)
		}
		effects[i] = e
	}
	return journal.OperationInput{
		OperationID:        journal.OperationID(wc.OperationID),
		ActorID:            actor,
		AuthorityJournalID: (*journal.JournalID)(wc.AuthorityJournalID),
		CommandDigest:      wc.CommandDigest,
		MutationDigest:     wm.MutationDigest,
		RecordedAt:         wc.RecordedAt,
		Effects:            effects,
	}, nil
}

func encodeEffect(e journal.Effect) (wireEffect, error) {
	ctxs := make([]wireEventContext, len(e.Contexts))
	for i := range e.Contexts {
		kind, identity, err := journal.EncodeStoredEventContext(e.Contexts[i])
		if err != nil {
			return wireEffect{}, fmt.Errorf("encode context %d: %w", i, err)
		}
		ctxs[i] = wireEventContext{Kind: string(kind), Identity: identity}
	}
	return wireEffect{
		Sort:               int(e.Sort),
		ResultSlot:         string(e.ResultSlot),
		ActorID:            actorToWire(e.ActorID),
		RecordedAtOverride: e.RecordedAtOverride,
		TaskID:             taskToWire(e.TaskID),
		EventKind:          string(e.EventKind),
		Payload:            e.Payload,
		Contexts:           ctxs,
		Title:              e.Title,
		Description:        e.Description,
		Type:               int(e.Type),
		Priority:           int(e.Priority),
		Phase:              int(e.Phase),
		CloseReason:        e.CloseReason,
		UpdateTitle:        e.UpdateTitle,
		UpdateDescription:  e.UpdateDescription,
		UpdatePriority:     priorityPtrToWire(e.UpdatePriority),
		UpdatePhase:        phasePtrToWire(e.UpdatePhase),
		UpdateNotes:        e.UpdateNotes,
		BootstrapLabel:     e.BootstrapLabel,
		Authority:          string(e.OperationAuthorityID),
		AssignmentID:       string(e.AssignmentID),
		SlotID:             string(e.SlotID),
		Occupant:           actorToWire(e.Occupant),
		Predecessor:        string(e.Predecessor),
		Parent:             string(e.Parent),
		DecisionKind:       string(e.DecisionKind),
		EvidenceKind:       string(e.EvidenceKind),
		ContentDigest:      e.ContentDigest,
	}, nil
}

func decodeEffect(w wireEffect) (journal.Effect, error) {
	actor, err := actorFromWire(w.ActorID)
	if err != nil {
		return journal.Effect{}, fmt.Errorf("effect actor: %w", err)
	}
	taskID, err := taskFromWire(w.TaskID)
	if err != nil {
		return journal.Effect{}, fmt.Errorf("effect task: %w", err)
	}
	occ, err := actorFromWire(w.Occupant)
	if err != nil {
		return journal.Effect{}, fmt.Errorf("effect occupant: %w", err)
	}
	ctxs := make([]journal.EventContext, len(w.Contexts))
	for i := range w.Contexts {
		c, decErr := journal.DecodeStoredEventContext(journal.EventContextKind(w.Contexts[i].Kind), w.Contexts[i].Identity)
		if decErr != nil {
			return journal.Effect{}, fmt.Errorf("effect context %d: %w", i, decErr)
		}
		ctxs[i] = c
	}
	if len(ctxs) == 0 {
		ctxs = nil
	}
	return journal.Effect{
		Sort:                 journal.EffectSort(w.Sort),
		ResultSlot:           journal.ResultSlotID(w.ResultSlot),
		ActorID:              actor,
		RecordedAtOverride:   w.RecordedAtOverride,
		TaskID:               taskID,
		EventKind:            journal.EventKind(w.EventKind),
		Payload:              w.Payload,
		Contexts:             ctxs,
		Title:                w.Title,
		Description:          w.Description,
		Type:                 journal.TaskType(w.Type),
		Priority:             journal.Priority(w.Priority),
		Phase:                journal.Phase(w.Phase),
		CloseReason:          w.CloseReason,
		UpdateTitle:          w.UpdateTitle,
		UpdateDescription:    w.UpdateDescription,
		UpdatePriority:       priorityPtrFromWire(w.UpdatePriority),
		UpdatePhase:          phasePtrFromWire(w.UpdatePhase),
		UpdateNotes:          w.UpdateNotes,
		BootstrapLabel:       w.BootstrapLabel,
		OperationAuthorityID: journal.OperationAuthorityID(w.Authority),
		AssignmentID:         journal.AssignmentID(w.AssignmentID),
		SlotID:               journal.AssignmentSlotID(w.SlotID),
		Occupant:             occ,
		Predecessor:          journal.AssignmentID(w.Predecessor),
		Parent:               journal.AssignmentID(w.Parent),
		DecisionKind:         journal.DecisionKind(w.DecisionKind),
		EvidenceKind:         journal.EvidenceKind(w.EvidenceKind),
		ContentDigest:        w.ContentDigest,
	}, nil
}

// ---------------------------------------------------------------------------
// Typed-ID wire helpers: canonical String() form, empty string for the zero ID.
// ---------------------------------------------------------------------------

func actorToWire(id ptypes.ActorID) string {
	if id == (ptypes.ActorID{}) {
		return ""
	}
	return id.String()
}

func actorFromWire(s string) (ptypes.ActorID, error) {
	if s == "" {
		return ptypes.ActorID{}, nil
	}
	return ptypes.ParseActorID(s)
}

func taskToWire(id ptypes.TaskID) string {
	if id == (ptypes.TaskID{}) {
		return ""
	}
	return id.String()
}

func taskFromWire(s string) (ptypes.TaskID, error) {
	if s == "" {
		return ptypes.TaskID{}, nil
	}
	return ptypes.ParseTaskID(s)
}

func priorityPtrToWire(p *ptypes.Priority) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

func priorityPtrFromWire(p *int) *ptypes.Priority {
	if p == nil {
		return nil
	}
	v := ptypes.Priority(*p)
	return &v
}

func phasePtrToWire(p *ptypes.Phase) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

func phasePtrFromWire(p *int) *ptypes.Phase {
	if p == nil {
		return nil
	}
	v := ptypes.Phase(*p)
	return &v
}
