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
	"encoding/json"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// DBOSApplyInputSchemaV1 is the closed schema tag stamped on every
// DBOSApplyInputV1; a decoded input carrying any other tag fails closed.
const DBOSApplyInputSchemaV1 = "provenance.dbos-apply-input/v1"

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
