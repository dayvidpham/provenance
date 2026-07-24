package journal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// ---------------------------------------------------------------------------
// Effect wire-name and field constants (replaces deleted semantic_operands.go)
// ---------------------------------------------------------------------------

// effectWireName returns the canonical field name for effect N, sub-field name.
func effectWireName(index int, name string) string {
	return fmt.Sprintf("effect.%d.%s", index, name)
}

// canonicalEffectField identifies a per-effect wire sub-field by its ordinal.
// Only effectFamily is used through the typed FieldRef path; all other
// sub-fields are addressed by name through rawField.
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
	effectActivityID
	effectActivityAgentID
	effectActivityPhase
	effectActivityStage
	effectActivityNotes
)

// canonicalV1Families is the canonical ordered registry of effect-family wire tags.
type canonicalV1FamilyDescriptor struct {
	sort EffectSort
	tag  string
}

type canonicalV1FamilyRegistry []canonicalV1FamilyDescriptor

var canonicalV1Families = canonicalV1FamilyRegistry{
	{sort: EffectTaskEvent, tag: "task_event"},
	{sort: EffectBootstrapAuthority, tag: "bootstrap_authority"},
	{sort: EffectAssignmentStart, tag: "assignment_start"},
	{sort: EffectAssignmentEnd, tag: "assignment_end"},
	{sort: EffectDecision, tag: "decision"},
	{sort: EffectEvidence, tag: "evidence"},
	{sort: EffectTaskCreate, tag: "task_create"},
	{sort: EffectEdgeAdd, tag: "edge_add"},
	{sort: EffectEdgeRemove, tag: "edge_remove"},
	{sort: EffectLabelAdd, tag: "label_add"},
	{sort: EffectLabelRemove, tag: "label_remove"},
	{sort: EffectCommentAdd, tag: "comment_add"},
	{sort: EffectTaskCreateAllocated, tag: "task_create_allocated"},
	{sort: EffectActivityCreate, tag: "activity_create"},
}

func validEffectSort(sort EffectSort) bool {
	_, ok := mutationV1Codec.familyTag(sort)
	return ok
}

func (canonicalV1Codec) familyTag(sort EffectSort) (string, bool) {
	for _, d := range canonicalV1Families {
		if d.sort == sort {
			return d.tag, true
		}
	}
	return "", false
}

func (canonicalV1Codec) parseFamilyTag(tag string) (EffectSort, error) {
	for _, d := range canonicalV1Families {
		if d.tag == tag {
			return d.sort, nil
		}
	}
	return 0, fmt.Errorf("provenance: decode canonical mutation: unknown effect family %q", tag)
}

// semanticEffectTag returns the wire tag for an EffectSort (used by EffectSort.String).
func semanticEffectTag(sort EffectSort) (string, bool) {
	return mutationV1Codec.familyTag(sort)
}

// semanticEffectJournalKind returns the JournalKind for an EffectSort.
func semanticEffectJournalKind(sort EffectSort) (JournalKind, bool) {
	switch sort {
	case EffectTaskEvent, EffectTaskCreate, EffectTaskCreateAllocated,
		EffectEdgeAdd, EffectEdgeRemove, EffectLabelAdd, EffectLabelRemove, EffectCommentAdd:
		return JournalKindTaskEvent, true
	case EffectBootstrapAuthority, EffectAssignmentStart, EffectAssignmentEnd:
		return JournalKindAuthority, true
	case EffectDecision:
		return JournalKindDecision, true
	case EffectEvidence:
		return JournalKindEvidence, true
	case EffectActivityCreate:
		return JournalKindActivity, true
	}
	return 0, false
}

// semanticMutationFamilyKind returns the fixed EventKind for a mutation-family sort.
func semanticMutationFamilyKind(sort EffectSort) (EventKind, bool) {
	switch sort {
	case EffectEdgeAdd:
		return EventKindEdgeAdded, true
	case EffectEdgeRemove:
		return EventKindEdgeRemoved, true
	case EffectLabelAdd:
		return EventKindLabelAdded, true
	case EffectLabelRemove:
		return EventKindLabelRemoved, true
	case EffectCommentAdd:
		return EventKindCommentAdded, true
	}
	return "", false
}

// semanticConditionKinds returns the closed set of ConditionKind values.
func semanticConditionKinds() []ConditionKind {
	// ConditionKind zero is invalid; return only the two valid nonzero constants.
	return []ConditionKind{ConditionExactFact, ConditionCurrentFact}
}

// SemanticEffectSorts returns the closed set of EffectSort values.
func SemanticEffectSorts() []EffectSort {
	out := make([]EffectSort, len(canonicalV1Families))
	for i, d := range canonicalV1Families {
		out[i] = d.sort
	}
	return out
}

// ---------------------------------------------------------------------------
// Effect clone (deep copy for safe external return)
// ---------------------------------------------------------------------------

// cloneCanonicalEffect deep-copies an Effect so callers cannot mutate canonical state.
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

// cloneCanonicalCondition deep-copies a Condition for safe external return.
func cloneCanonicalCondition(in Condition, _ int) Condition {
	out := Condition{
		Kind:              in.Kind,
		AssertedJournalID: in.AssertedJournalID,
		Selector: FactSelector{
			Kind:         in.Selector.Kind,
			DecisionKind: in.Selector.DecisionKind,
			EvidenceKind: in.Selector.EvidenceKind,
			Filter: FactFilter{
				TaskScope: in.Selector.Filter.TaskScope,
			},
		},
	}
	if len(in.Selector.Filter.RequiredContexts) > 0 {
		out.Selector.Filter.RequiredContexts = append([]EventContext(nil), in.Selector.Filter.RequiredContexts...)
	}
	if len(in.Selector.Filter.EffectiveActorIDs) > 0 {
		out.Selector.Filter.EffectiveActorIDs = append([]ActorID(nil), in.Selector.Filter.EffectiveActorIDs...)
	}
	if len(in.Selector.Filter.OperationIDs) > 0 {
		out.Selector.Filter.OperationIDs = append([]OperationID(nil), in.Selector.Filter.OperationIDs...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Effect encode/decode/normalize (explicit switches — no callback metamodel)
// ---------------------------------------------------------------------------

func (codec canonicalV1Codec) encodeEffect(w *canonicalWriter, e Effect, index int) error {
	family, _ := mutationV1Codec.familyTag(e.Sort)
	w.rawField(effectWireName(index, "family"), []byte(family))
	w.rawField(effectWireName(index, "result-slot"), []byte(e.ResultSlot))
	w.rawField(effectWireName(index, "recorded-at-override"), codec.encodeOptionalInt64(e.RecordedAtOverride))
	contexts := func() {
		w.rawField(effectWireName(index, "context-count"), []byte(strconv.Itoa(len(e.Contexts))))
		for j, c := range e.Contexts {
			k, id, _ := EncodeStoredEventContext(c)
			w.field(contextField(index, j, contextKind), []byte(k))
			w.field(contextField(index, j, contextIdentity), []byte(id))
		}
	}
	payload := func() {
		p, _ := codec.canonicalJSON(e.Payload)
		w.rawField(effectWireName(index, "payload"), p)
	}
	switch e.Sort {
	case EffectTaskCreate, EffectTaskCreateAllocated:
		w.rawField(effectWireName(index, "task"), []byte(e.TaskID.String()))
		payload()
		contexts()
		w.rawField(effectWireName(index, "title"), []byte(e.Title))
		w.rawField(effectWireName(index, "description"), []byte(e.Description))
		w.rawField(effectWireName(index, "type"), []byte(e.Type.String()))
		w.rawField(effectWireName(index, "priority"), []byte(e.Priority.String()))
		w.rawField(effectWireName(index, "phase"), []byte(e.Phase.String()))
	case EffectTaskEvent:
		w.rawField(effectWireName(index, "task"), []byte(e.TaskID.String()))
		w.rawField(effectWireName(index, "event-kind"), []byte(e.EventKind))
		payload()
		contexts()
		if e.EventKind == EventKindTaskUpdated {
			w.rawField(effectWireName(index, "update-title"), codec.encodeOptionalString(e.UpdateTitle))
			w.rawField(effectWireName(index, "update-description"), codec.encodeOptionalString(e.UpdateDescription))
			w.rawField(effectWireName(index, "update-priority"), codec.encodeOptionalPriority(e.UpdatePriority))
			w.rawField(effectWireName(index, "update-phase"), codec.encodeOptionalPhase(e.UpdatePhase))
			w.rawField(effectWireName(index, "update-notes"), codec.encodeOptionalString(e.UpdateNotes))
		}
		if IsTransitionLifecycleKind(e.EventKind) {
			w.rawField(effectWireName(index, "forced"), []byte(strconv.FormatBool(e.Forced)))
			if e.EventKind == EventKindTaskClosed {
				w.rawField(effectWireName(index, "close-reason"), []byte(e.CloseReason))
			}
		}
	case EffectBootstrapAuthority:
		w.rawField(effectWireName(index, "bootstrap-label"), []byte(e.BootstrapLabel))
		w.rawField(effectWireName(index, "operation-authority"), []byte(e.OperationAuthorityID))
	case EffectAssignmentStart:
		w.rawField(effectWireName(index, "task"), []byte(e.TaskID.String()))
		w.rawField(effectWireName(index, "assignment"), []byte(e.AssignmentID))
		w.rawField(effectWireName(index, "slot"), []byte(e.SlotID))
		w.rawField(effectWireName(index, "occupant"), []byte(idString(e.Occupant)))
		w.rawField(effectWireName(index, "predecessor"), []byte(e.Predecessor))
		w.rawField(effectWireName(index, "parent"), []byte(e.Parent))
	case EffectAssignmentEnd:
		w.rawField(effectWireName(index, "task"), []byte(idString(e.TaskID)))
		w.rawField(effectWireName(index, "assignment"), []byte(e.AssignmentID))
		w.rawField(effectWireName(index, "slot"), []byte(e.SlotID))
	case EffectDecision:
		w.rawField(effectWireName(index, "task"), []byte(idString(e.TaskID)))
		w.rawField(effectWireName(index, "decision-kind"), []byte(e.DecisionKind))
		payload()
		contexts()
	case EffectEvidence:
		w.rawField(effectWireName(index, "task"), []byte(idString(e.TaskID)))
		w.rawField(effectWireName(index, "evidence-kind"), []byte(e.EvidenceKind))
		w.rawField(effectWireName(index, "content-digest"), e.ContentDigest)
		payload()
		contexts()
	case EffectEdgeAdd, EffectEdgeRemove:
		w.rawField(effectWireName(index, "task"), []byte(e.TaskID.String()))
		w.rawField(effectWireName(index, "edge-target"), []byte(e.EdgeTargetID))
		w.rawField(effectWireName(index, "edge-kind"), []byte(e.EdgeRelKind.String()))
		contexts()
	case EffectLabelAdd, EffectLabelRemove:
		w.rawField(effectWireName(index, "task"), []byte(e.TaskID.String()))
		w.rawField(effectWireName(index, "label"), []byte(e.Label))
		contexts()
	case EffectCommentAdd:
		w.rawField(effectWireName(index, "task"), []byte(e.TaskID.String()))
		w.rawField(effectWireName(index, "comment"), []byte(e.CommentIdentity.String()))
		w.rawField(effectWireName(index, "comment-author"), []byte(e.CommentAuthor.String()))
		w.rawField(effectWireName(index, "comment-body"), []byte(e.CommentBody))
		contexts()
	case EffectActivityCreate:
		// ResultSlot is required for ActivityCreate; validateSemanticResultSlots enforces it.
		w.rawField(effectWireName(index, "activity-id"), []byte(e.ActivityID.String()))
		w.rawField(effectWireName(index, "activity-agent-id"), []byte(idString(e.ActivityAgentID)))
		w.rawField(effectWireName(index, "activity-phase"), []byte(e.ActivityPhase.String()))
		w.rawField(effectWireName(index, "activity-stage"), []byte(e.ActivityStage.String()))
		w.rawField(effectWireName(index, "activity-notes"), []byte(e.ActivityNotes))
	}
	return w.err
}

func (codec canonicalV1Codec) decodeEffect(r *canonicalReader, index int) (Effect, error) {
	family, err := r.rawField(effectWireName(index, "family"))
	if err != nil {
		return Effect{}, err
	}
	sort, err := mutationV1Codec.parseFamilyTag(string(family))
	if err != nil {
		return Effect{}, canonicalMutationError(effectWireName(index, "family"), err.Error(), "restore a declared evolved V1 family tag")
	}
	e := Effect{Sort: sort}
	b, err := r.rawField(effectWireName(index, "result-slot"))
	if err != nil {
		return e, err
	}
	e.ResultSlot = ResultSlotID(b)
	b, err = r.rawField(effectWireName(index, "recorded-at-override"))
	if err != nil {
		return e, err
	}
	e.RecordedAtOverride, err = codec.decodeOptionalInt64(b)
	if err != nil {
		return e, err
	}
	payload := func() error {
		raw, x := r.rawField(effectWireName(index, "payload"))
		if x == nil {
			e.Payload = append(json.RawMessage(nil), raw...)
		}
		return x
	}
	contexts := func() error {
		raw, x := r.rawField(effectWireName(index, "context-count"))
		if x != nil {
			return x
		}
		n, x := strconv.Atoi(string(raw))
		if x != nil || n < 0 || n > MaxCanonicalContextsPerEffect {
			return canonicalMutationError(effectWireName(index, "context-count"), fmt.Sprintf("invalid bounded count %q", raw), "use a non-negative bounded count")
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
		raw, x := r.rawField(effectWireName(index, "task"))
		if x != nil {
			return x
		}
		e.TaskID, x = parseOptionalTask(string(raw))
		return x
	}
	switch e.Sort {
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
		if b, err = r.rawField(effectWireName(index, "title")); err == nil {
			e.Title = string(b)
		} else {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "description")); err == nil {
			e.Description = string(b)
		} else {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "type")); err == nil {
			err = e.Type.UnmarshalText(b)
		}
		if err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "priority")); err == nil {
			err = e.Priority.UnmarshalText(b)
		}
		if err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "phase")); err == nil {
			err = e.Phase.UnmarshalText(b)
		}
	case EffectTaskEvent:
		if err = task(); err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "event-kind")); err == nil {
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
			if b, err = r.rawField(effectWireName(index, "update-title")); err == nil {
				e.UpdateTitle, err = codec.decodeOptionalString(b)
			}
			if err != nil {
				return e, err
			}
			if b, err = r.rawField(effectWireName(index, "update-description")); err == nil {
				e.UpdateDescription, err = codec.decodeOptionalString(b)
			}
			if err != nil {
				return e, err
			}
			if b, err = r.rawField(effectWireName(index, "update-priority")); err == nil {
				e.UpdatePriority, err = codec.decodeOptionalPriority(b)
			}
			if err != nil {
				return e, err
			}
			if b, err = r.rawField(effectWireName(index, "update-phase")); err == nil {
				e.UpdatePhase, err = codec.decodeOptionalPhase(b)
			}
			if err != nil {
				return e, err
			}
			if b, err = r.rawField(effectWireName(index, "update-notes")); err == nil {
				e.UpdateNotes, err = codec.decodeOptionalString(b)
			}
		}
		if IsTransitionLifecycleKind(e.EventKind) {
			if b, err = r.rawField(effectWireName(index, "forced")); err == nil {
				e.Forced, err = strconv.ParseBool(string(b))
			}
			if err != nil {
				return e, err
			}
			if e.EventKind == EventKindTaskClosed {
				if b, err = r.rawField(effectWireName(index, "close-reason")); err == nil {
					e.CloseReason = string(b)
				}
			}
		}
	case EffectBootstrapAuthority:
		if b, err = r.rawField(effectWireName(index, "bootstrap-label")); err == nil {
			e.BootstrapLabel = string(b)
		} else {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "operation-authority")); err == nil {
			e.OperationAuthorityID = OperationAuthorityID(b)
		}
	case EffectAssignmentStart:
		if err = task(); err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "assignment")); err == nil {
			e.AssignmentID = AssignmentID(b)
		} else {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "slot")); err == nil {
			e.SlotID = AssignmentSlotID(b)
		} else {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "occupant")); err == nil {
			e.Occupant, err = parseOptionalActor(string(b))
		}
		if err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "predecessor")); err == nil {
			e.Predecessor = AssignmentID(b)
		} else {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "parent")); err == nil {
			e.Parent = AssignmentID(b)
		}
	case EffectAssignmentEnd:
		if err = task(); err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "assignment")); err == nil {
			e.AssignmentID = AssignmentID(b)
		} else {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "slot")); err == nil {
			e.SlotID = AssignmentSlotID(b)
		}
	case EffectDecision:
		if err = task(); err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "decision-kind")); err == nil {
			e.DecisionKind = DecisionKind(b)
		} else {
			return e, err
		}
		if err = payload(); err != nil {
			return e, err
		}
		err = contexts()
	case EffectEvidence:
		if err = task(); err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "evidence-kind")); err == nil {
			e.EvidenceKind = EvidenceKind(b)
		} else {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "content-digest")); err == nil {
			e.ContentDigest = append([]byte(nil), b...)
		} else {
			return e, err
		}
		if err = payload(); err != nil {
			return e, err
		}
		err = contexts()
	case EffectEdgeAdd, EffectEdgeRemove:
		if err = task(); err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "edge-target")); err == nil {
			e.EdgeTargetID = string(b)
		} else {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "edge-kind")); err == nil {
			err = e.EdgeRelKind.UnmarshalText(b)
		}
		if err == nil {
			err = contexts()
		}
	case EffectLabelAdd, EffectLabelRemove:
		if err = task(); err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "label")); err == nil {
			e.Label = string(b)
		} else {
			return e, err
		}
		err = contexts()
	case EffectCommentAdd:
		if err = task(); err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "comment")); err == nil {
			e.CommentIdentity, err = parseOptionalComment(string(b))
		}
		if err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "comment-author")); err == nil {
			e.CommentAuthor, err = parseOptionalActor(string(b))
		}
		if err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "comment-body")); err == nil {
			e.CommentBody = string(b)
		} else {
			return e, err
		}
		err = contexts()
	case EffectActivityCreate:
		if b, err = r.rawField(effectWireName(index, "activity-id")); err == nil {
			e.ActivityID, err = ptypes.ParseActivityID(string(b))
		}
		if err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "activity-agent-id")); err == nil {
			e.ActivityAgentID, err = ptypes.ParseAgentID(string(b))
		}
		if err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "activity-phase")); err == nil {
			err = e.ActivityPhase.UnmarshalText(b)
		}
		if err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "activity-stage")); err == nil {
			err = e.ActivityStage.UnmarshalText(b)
		}
		if err != nil {
			return e, err
		}
		if b, err = r.rawField(effectWireName(index, "activity-notes")); err == nil {
			e.ActivityNotes = string(b)
		} else {
			return e, err
		}
	}
	if err != nil {
		return e, err
	}
	return codec.normalizeEffect(e, index)
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
		return Effect{}, canonicalMutationError(effectWireName(index, "family"), fmt.Sprintf("unknown family %d", e.Sort), "use a declared EffectSort")
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
			return Effect{}, canonicalMutationError(effectWireName(index, "result-slot"), "allocated task create requires a result slot for committed identity reconciliation", "supply a stable non-empty result slot")
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
			return Effect{}, canonicalMutationError(effectWireName(index, "task"), "task event requires a task", "supply a namespaced TaskID")
		}
		if err := ValidateEventKind(e.EventKind); err != nil {
			return Effect{}, canonicalMutationError(effectWireName(index, "event-kind"), err.Error(), "use a valid namespaced event kind")
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
				return Effect{}, canonicalMutationError(effectWireName(index, "payload"), "forced lifecycle payload is reducer-generated", "leave payload empty when Forced is true")
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
			return Effect{}, canonicalMutationError(effectWireName(index, "assignment"), "assignment is required", "supply the episode identity")
		}
		n.AssignmentID = e.AssignmentID
		n.TaskID = e.TaskID
		n.SlotID = e.SlotID
	case EffectDecision:
		if err := ValidateEventKind(EventKind(e.DecisionKind)); err != nil {
			return Effect{}, canonicalMutationError(effectWireName(index, "decision-kind"), err.Error(), "use a lower-case namespaced decision kind such as caller.decision")
		}
		n.TaskID = e.TaskID
		n.DecisionKind = e.DecisionKind
		n.Payload = e.Payload
		n.Contexts = e.Contexts
	case EffectEvidence:
		if err := ValidateEventKind(EventKind(e.EvidenceKind)); err != nil {
			return Effect{}, canonicalMutationError(effectWireName(index, "evidence-kind"), err.Error(), "use a lower-case namespaced evidence kind such as caller.evidence")
		}
		n.TaskID = e.TaskID
		n.EvidenceKind = e.EvidenceKind
		n.ContentDigest = e.ContentDigest
		n.Payload = e.Payload
		n.Contexts = e.Contexts
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
			return Effect{}, canonicalMutationError(effectWireName(index, "label"), "task and label are required", "supply both operands")
		}
		n.TaskID = e.TaskID
		n.Label = e.Label
		n.Contexts = e.Contexts
	case EffectCommentAdd:
		if e.TaskID.Namespace == "" || e.CommentIdentity.Namespace == "" || e.CommentAuthor.Namespace == "" {
			return Effect{}, canonicalMutationError(effectWireName(index, "comment"), "task, comment identity, and author are required", "supply all comment operands")
		}
		n.TaskID = e.TaskID
		n.CommentIdentity = e.CommentIdentity
		n.CommentAuthor = e.CommentAuthor
		n.CommentBody = e.CommentBody
		n.Contexts = e.Contexts
	case EffectActivityCreate:
		if err := validateActivityID(e.ActivityID); err != nil {
			return Effect{}, canonicalMutationError(effectWireName(index, "activity-id"), "ActivityCreate requires a non-zero namespaced ActivityID: "+err.Error(), "supply a valid namespaced ActivityID")
		}
		if e.ActivityAgentID == (AgentID{}) {
			return Effect{}, canonicalMutationError(effectWireName(index, "activity-agent-id"), "ActivityCreate requires a non-zero AgentID", "supply a namespaced AgentID matching the responsible agent")
		}
		if !e.ActivityPhase.IsValid() {
			return Effect{}, canonicalMutationError(effectWireName(index, "activity-phase"), "ActivityCreate requires a valid Phase", "use a declared Phase value such as PhaseWorkerSlices")
		}
		if !e.ActivityStage.IsValid() {
			return Effect{}, canonicalMutationError(effectWireName(index, "activity-stage"), "ActivityCreate requires a valid Stage", "use a declared Stage value such as StageInProgress")
		}
		if e.ResultSlot == "" {
			return Effect{}, canonicalMutationError(effectWireName(index, "result-slot"), "ActivityCreate requires a result slot for committed identity reconciliation", "supply a stable non-empty result slot")
		}
		n.ActivityID = e.ActivityID
		n.ActivityAgentID = e.ActivityAgentID
		n.ActivityPhase = e.ActivityPhase
		n.ActivityStage = e.ActivityStage
		n.ActivityNotes = e.ActivityNotes
	}
	// Bounded reflection: exhaustive check that no Effect field populated by the caller
	// was silently ignored by this family's normalization arm. Reflection does not control
	// wire order or dispatch — those are the explicit switches above. If Effect gains a new
	// field, this check fails closed until an explicit arm copies it.
	if !reflect.DeepEqual(e, n) {
		v1, v2 := reflect.ValueOf(e), reflect.ValueOf(n)
		for i := 0; i < v1.NumField(); i++ {
			if !reflect.DeepEqual(v1.Field(i).Interface(), v2.Field(i).Interface()) {
				return Effect{}, canonicalMutationError(fmt.Sprintf("effect[%d] input %s", index, reflect.TypeOf(e).Field(i).Name), "field is populated but not consumed by this effect family", "clear the irrelevant field")
			}
		}
	}
	ctxs, err := CanonicalEventContexts(n.Contexts)
	if err != nil {
		return Effect{}, canonicalMutationError(fmt.Sprintf("effect[%d] contexts input", index), err.Error(), "supply valid bounded event contexts")
	}
	if len(ctxs) > MaxCanonicalContextsPerEffect {
		return Effect{}, canonicalMutationError(effectWireName(index, "context-count"), fmt.Sprintf("%d exceeds maximum %d", len(ctxs), MaxCanonicalContextsPerEffect), "reduce contexts")
	}
	if len(ctxs) > 0 {
		n.Contexts = ctxs
	} else {
		n.Contexts = nil
	}
	if len(n.Payload) > 0 {
		if _, err := codec.canonicalJSON(n.Payload); err != nil {
			return Effect{}, canonicalMutationError(effectWireName(index, "payload"), err.Error(), "supply one strict JSON value without duplicate fields")
		}
	}
	return n, nil
}

// validateRawCanonicalEffectBounds enforces per-field and aggregate size limits before
// normalization allocates any output. Bounded reflection iterates the flat Effect DTO
// to find oversized string/byte fields without enumerating each field explicitly; it does
// not control order or dispatch. Fails closed when new fields are added to Effect.
func validateRawCanonicalEffectBounds(effect Effect, index int) error {
	if len(effect.Contexts) > MaxCanonicalContextsPerEffect {
		return canonicalMutationError(effectWireName(index, "context-count"), fmt.Sprintf("raw count %d exceeds maximum %d before canonicalization", len(effect.Contexts), MaxCanonicalContextsPerEffect), "reduce contexts before retrying")
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

// ---------------------------------------------------------------------------
// Condition encode/decode (explicit named functions — no descriptor metamodel)
// ---------------------------------------------------------------------------

// encodeSemanticCondition encodes one normalized Condition to the canonical writer.
func encodeSemanticCondition(w *canonicalWriter, c Condition, index int) {
	w.rawField(conditionName(index, "kind"), []byte(strconv.Itoa(int(c.Kind))))
	w.rawField(conditionName(index, "fact-kind"), []byte(strconv.Itoa(int(c.Selector.Kind))))
	w.rawField(conditionName(index, "task-scope"), []byte(strconv.Itoa(int(c.Selector.Filter.TaskScope.Kind))))
	if c.Selector.Filter.TaskScope.Kind == FactTaskExact {
		w.rawField(conditionName(index, "task-id"), []byte(c.Selector.Filter.TaskScope.TaskID.String()))
	} else {
		w.rawField(conditionName(index, "task-id"), nil)
	}
	w.rawField(conditionName(index, "context-count"), []byte(strconv.Itoa(len(c.Selector.Filter.RequiredContexts))))
	for i, ctx := range c.Selector.Filter.RequiredContexts {
		kind, identity, _ := EncodeStoredEventContext(ctx)
		w.rawField(conditionName(index, fmt.Sprintf("context.%d.kind", i)), []byte(kind))
		w.rawField(conditionName(index, fmt.Sprintf("context.%d.identity", i)), []byte(identity))
	}
	w.rawField(conditionName(index, "actor-count"), []byte(strconv.Itoa(len(c.Selector.Filter.EffectiveActorIDs))))
	for i, actor := range c.Selector.Filter.EffectiveActorIDs {
		w.rawField(conditionName(index, fmt.Sprintf("actor.%d", i)), []byte(actor.String()))
	}
	w.rawField(conditionName(index, "operation-count"), []byte(strconv.Itoa(len(c.Selector.Filter.OperationIDs))))
	for i, opID := range c.Selector.Filter.OperationIDs {
		w.rawField(conditionName(index, fmt.Sprintf("operation.%d", i)), []byte(opID))
	}
	w.rawField(conditionName(index, "decision-kind"), []byte(c.Selector.DecisionKind))
	w.rawField(conditionName(index, "evidence-kind"), []byte(c.Selector.EvidenceKind))
	w.rawField(conditionName(index, "asserted-journal-id"), []byte(strconv.FormatInt(int64(c.AssertedJournalID), 10)))
}

// decodeSemanticCondition decodes one Condition from the canonical reader.
func decodeSemanticCondition(r *canonicalReader, index int) (Condition, error) {
	var c Condition
	raw, err := r.rawField(conditionName(index, "kind"))
	if err != nil {
		return c, err
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil || n < 0 || n > int(^ConditionKind(0)) {
		return c, canonicalMutationError(conditionName(index, "kind"), fmt.Sprintf("invalid condition kind %q", raw), "use a declared ConditionKind")
	}
	c.Kind = ConditionKind(n)
	raw, err = r.rawField(conditionName(index, "fact-kind"))
	if err != nil {
		return c, err
	}
	n, err = strconv.Atoi(string(raw))
	if err != nil || n < 0 || n > int(^FactKind(0)) {
		return c, canonicalMutationError(conditionName(index, "fact-kind"), fmt.Sprintf("invalid fact kind %q", raw), "use a declared FactKind")
	}
	c.Selector.Kind = FactKind(n)
	raw, err = r.rawField(conditionName(index, "task-scope"))
	if err != nil {
		return c, err
	}
	n, err = strconv.Atoi(string(raw))
	if err != nil || n < 0 || n > int(^FactTaskScopeKind(0)) {
		return c, canonicalMutationError(conditionName(index, "task-scope"), fmt.Sprintf("invalid task scope %q", raw), "use a declared FactTaskScopeKind")
	}
	c.Selector.Filter.TaskScope.Kind = FactTaskScopeKind(n)
	raw, err = r.rawField(conditionName(index, "task-id"))
	if err != nil {
		return c, err
	}
	if c.Selector.Filter.TaskScope.Kind == FactTaskExact {
		c.Selector.Filter.TaskScope.TaskID, err = ptypes.ParseTaskID(string(raw))
		if err != nil {
			return c, canonicalMutationError(conditionName(index, "task-id"), err.Error(), "supply a valid namespaced task ID for FactTaskExact scope")
		}
	} else if len(raw) != 0 {
		return c, canonicalMutationError(conditionName(index, "task-id"), "non-exact scope must have empty task-id", "leave task-id empty for Any/Unscoped scope")
	}
	raw, err = r.rawField(conditionName(index, "context-count"))
	if err != nil {
		return c, err
	}
	contextCount, err := parseBoundedCount(raw, MaxCanonicalContextsPerEffect, conditionName(index, "context-count"))
	if err != nil {
		return c, err
	}
	for i := 0; i < contextCount; i++ {
		kindRaw, x := r.rawField(conditionName(index, fmt.Sprintf("context.%d.kind", i)))
		if x != nil {
			return c, x
		}
		identityRaw, x := r.rawField(conditionName(index, fmt.Sprintf("context.%d.identity", i)))
		if x != nil {
			return c, x
		}
		ctx, x := DecodeStoredEventContext(EventContextKind(kindRaw), string(identityRaw))
		if x != nil {
			return c, x
		}
		c.Selector.Filter.RequiredContexts = append(c.Selector.Filter.RequiredContexts, ctx)
	}
	raw, err = r.rawField(conditionName(index, "actor-count"))
	if err != nil {
		return c, err
	}
	actorCount, err := parseBoundedCount(raw, MaxFactFilterValues, conditionName(index, "actor-count"))
	if err != nil {
		return c, err
	}
	for i := 0; i < actorCount; i++ {
		actorRaw, x := r.rawField(conditionName(index, fmt.Sprintf("actor.%d", i)))
		if x != nil {
			return c, x
		}
		actor, x := ptypes.ParseActorID(string(actorRaw))
		if x != nil {
			return c, x
		}
		c.Selector.Filter.EffectiveActorIDs = append(c.Selector.Filter.EffectiveActorIDs, actor)
	}
	raw, err = r.rawField(conditionName(index, "operation-count"))
	if err != nil {
		return c, err
	}
	opCount, err := parseBoundedCount(raw, MaxFactFilterValues, conditionName(index, "operation-count"))
	if err != nil {
		return c, err
	}
	for i := 0; i < opCount; i++ {
		opRaw, x := r.rawField(conditionName(index, fmt.Sprintf("operation.%d", i)))
		if x != nil {
			return c, x
		}
		c.Selector.Filter.OperationIDs = append(c.Selector.Filter.OperationIDs, OperationID(opRaw))
	}
	raw, err = r.rawField(conditionName(index, "decision-kind"))
	if err != nil {
		return c, err
	}
	c.Selector.DecisionKind = DecisionKind(raw)
	raw, err = r.rawField(conditionName(index, "evidence-kind"))
	if err != nil {
		return c, err
	}
	c.Selector.EvidenceKind = EvidenceKind(raw)
	raw, err = r.rawField(conditionName(index, "asserted-journal-id"))
	if err != nil {
		return c, err
	}
	jid, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return c, canonicalMutationError(conditionName(index, "asserted-journal-id"), fmt.Sprintf("invalid journal id %q", raw), "use a non-negative decimal journal id")
	}
	c.AssertedJournalID = JournalID(jid)
	return normalizeCondition(c, index)
}

// ---------------------------------------------------------------------------
// Result-slot validation
// ---------------------------------------------------------------------------

// validateSemanticResultSlots checks result slots are unique, printable,
// and that EffectTaskCreateAllocated always has a non-empty slot.
func validateSemanticResultSlots(effects []Effect) error {
	seen := make(map[ResultSlotID]struct{}, len(effects))
	for index, effect := range effects {
		if effect.ResultSlot == "" {
			switch effect.Sort {
			case EffectTaskCreateAllocated:
				return canonicalMutationError(effectWireName(index, "result-slot"), "allocated task create requires a result slot", "supply a non-empty printable slot identity")
			case EffectActivityCreate:
				return canonicalMutationError(effectWireName(index, "result-slot"), "ActivityCreate requires a result slot for committed identity reconciliation", "supply a stable non-empty result slot")
			}
			continue
		}
		if err := validateResultSlotID(effect.ResultSlot); err != nil {
			return canonicalMutationError(effectWireName(index, "result-slot"), err.Error(), "use a non-empty printable slot identity unique within the operation")
		}
		if _, duplicate := seen[effect.ResultSlot]; duplicate {
			return canonicalMutationError(effectWireName(index, "result-slot"), "duplicate result slot", "assign each produced row a unique slot identity")
		}
		seen[effect.ResultSlot] = struct{}{}
	}
	return nil
}

// ValidateResultSlotBinding checks the structural validity of one result-slot binding.
// Shared by SQLite Apply and DBOS encode/decode paths.
func ValidateResultSlotBinding(binding ResultSlotBinding) error {
	if binding.Slot == "" || binding.ProducedJournalID <= 0 {
		return fmt.Errorf("%w: slot and positive produced JournalID are required — "+
			"where: result-slot binding validation; when: before checkpointing; "+
			"impact: the binding is rejected; fix: supply a non-empty slot and positive journal id",
			ErrResultSlotIntegrity)
	}
	if err := validateResultSlotID(binding.Slot); err != nil {
		return fmt.Errorf("%w: invalid slot syntax: %v — "+
			"where: result-slot binding validation; when: before checkpointing; "+
			"impact: the binding is rejected; fix: use a printable non-control slot identity",
			ErrResultSlotIntegrity, err)
	}
	switch binding.Kind {
	case JournalKindTaskEvent:
		if binding.TaskID == nil {
			return fmt.Errorf("%w: TaskEvent slot requires TaskID — "+
				"where: result-slot binding validation; when: before checkpointing; "+
				"impact: the binding is rejected; fix: supply the resolved TaskID for this slot",
				ErrResultSlotIntegrity)
		}
		if err := validateTaskID(*binding.TaskID); err != nil {
			return fmt.Errorf("%w: invalid TaskID: %v", ErrResultSlotIntegrity, err)
		}
		if binding.ActivityID != nil {
			return fmt.Errorf("%w: TaskEvent slot must not carry ActivityID",
				ErrResultSlotIntegrity)
		}
	case JournalKindActivity:
		if binding.ActivityID == nil {
			return fmt.Errorf("%w: Activity slot requires ActivityID — "+
				"where: result-slot binding validation; when: before checkpointing; "+
				"impact: the binding is rejected; fix: supply the resolved ActivityID for this slot",
				ErrResultSlotIntegrity)
		}
		if err := validateActivityID(*binding.ActivityID); err != nil {
			return fmt.Errorf("%w: invalid ActivityID: %v", ErrResultSlotIntegrity, err)
		}
		if binding.TaskID != nil {
			return fmt.Errorf("%w: Activity slot must not carry TaskID",
				ErrResultSlotIntegrity)
		}
	case JournalKindAuthority, JournalKindDecision, JournalKindEvidence:
		if binding.TaskID != nil {
			return fmt.Errorf("%w: non-entity slot must not carry TaskID — "+
				"where: result-slot binding validation; when: before checkpointing; "+
				"impact: the binding is rejected; fix: leave TaskID nil for this kind",
				ErrResultSlotIntegrity)
		}
		if binding.ActivityID != nil {
			return fmt.Errorf("%w: non-entity slot must not carry ActivityID",
				ErrResultSlotIntegrity)
		}
	case JournalKindOperation:
		return fmt.Errorf("%w: operation anchors cannot be result slots — "+
			"where: result-slot binding validation; when: before checkpointing; "+
			"impact: the binding is rejected; fix: use a produced row kind",
			ErrResultSlotIntegrity)
	default:
		return fmt.Errorf("%w: unknown result slot journal kind %d — "+
			"where: result-slot binding validation; when: before checkpointing; "+
			"impact: the binding is rejected; fix: use a known JournalKind",
			ErrResultSlotIntegrity, binding.Kind)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Structural identity helpers
// ---------------------------------------------------------------------------

func equalJournalIDPointers(a, b *JournalID) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// CompareOperationIdentity compares two OperationInput values using five broad
// conflict axes. It returns the first OperationConflict found, or nil.
//
// Index is -1 for scalar axes (Actor/Authority/Command) or when collection lengths differ.
// Index ≥ 0 identifies the first differing Condition or Effect element.
func CompareOperationIdentity(operationID OperationID, stored, candidate OperationInput) *OperationConflict {
	conflict := func(axis ConflictAxis, index int) *OperationConflict {
		return &OperationConflict{OperationID: operationID, Axis: axis, Index: index}
	}
	if stored.ActorID != candidate.ActorID {
		return conflict(ConflictActor, -1)
	}
	if !equalJournalIDPointers(stored.AuthorityJournalID, candidate.AuthorityJournalID) {
		return conflict(ConflictAuthority, -1)
	}
	if !bytes.Equal(stored.CommandDigest, candidate.CommandDigest) {
		return conflict(ConflictCommand, -1)
	}
	if len(stored.Conditions) != len(candidate.Conditions) {
		return conflict(ConflictCondition, -1)
	}
	for i := range stored.Conditions {
		if !conditionsEqual(stored.Conditions[i], candidate.Conditions[i]) {
			return conflict(ConflictCondition, i)
		}
	}
	if len(stored.Effects) != len(candidate.Effects) {
		return conflict(ConflictEffect, -1)
	}
	for i := range stored.Effects {
		left, errA := mutationV1Codec.normalizeEffect(stored.Effects[i], i)
		right, errB := mutationV1Codec.normalizeEffect(candidate.Effects[i], i)
		// reflect.DeepEqual on already-normalized Effect values: correct because
		// normalizeEffect has eliminated all representational ambiguity.
		if errA != nil || errB != nil || !reflect.DeepEqual(left, right) {
			return conflict(ConflictEffect, i)
		}
	}
	return nil
}

// conditionsEqual reports structural equality of two normalized conditions.
func conditionsEqual(a, b Condition) bool {
	if a.Kind != b.Kind || a.AssertedJournalID != b.AssertedJournalID {
		return false
	}
	if a.Selector.Kind != b.Selector.Kind {
		return false
	}
	if a.Selector.DecisionKind != b.Selector.DecisionKind || a.Selector.EvidenceKind != b.Selector.EvidenceKind {
		return false
	}
	if a.Selector.Filter.TaskScope != b.Selector.Filter.TaskScope {
		return false
	}
	if !equalContexts(a.Selector.Filter.RequiredContexts, b.Selector.Filter.RequiredContexts) {
		return false
	}
	return equalActorSlices(a.Selector.Filter.EffectiveActorIDs, b.Selector.Filter.EffectiveActorIDs) &&
		equalOperationSlices(a.Selector.Filter.OperationIDs, b.Selector.Filter.OperationIDs)
}

func equalActorSlices(a, b []ActorID) bool {
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

func equalOperationSlices(a, b []OperationID) bool {
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
