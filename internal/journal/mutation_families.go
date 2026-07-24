package journal

import (
	"encoding/json"
	"fmt"
)

// mutation_families.go defines the journaled relationship/annotation mutation families
// (docs/journal-relational-contract.md §6, as amended by #5): edge-add/edge-remove/
// label-add/label-remove/comment-add. Each is a fixed per-family EventKind carried on a
// journal_task_events row, so who added/removed the relationship, under which authority,
// at which journal position is derivable from the journal (who-provenance). The operands
// live in the row payload, encoded/decoded here from closed shapes so the edges/labels/
// comments domain projections are reproducible solely from ordered journal history (§15).
// This mirrors the migration-marker and forced-transition discipline: the reducer
// dispatches on the fixed kind (never a payload-generalized status/behaviour read), and
// reads the payload only for the operands.

// MutationFamilyKinds returns the closed set of journaled relationship/annotation family
// kinds in a stable order, so a corpus freshness guard can pin exactly these five.
func MutationFamilyKinds() []EventKind {
	return []EventKind{
		EventKindEdgeAdded, EventKindEdgeRemoved,
		EventKindLabelAdded, EventKindLabelRemoved,
		EventKindCommentAdded,
	}
}

// IsMutationFamilyKind reports whether kind is one of the five journaled
// relationship/annotation family kinds — the kinds whose reducer fold performs an
// edges/labels/comments domain write rather than a task-status/metadata projection.
func IsMutationFamilyKind(kind EventKind) bool {
	switch kind {
	case EventKindEdgeAdded, EventKindEdgeRemoved,
		EventKindLabelAdded, EventKindLabelRemoved, EventKindCommentAdded:
		return true
	default:
		return false
	}
}

// MutationFamilyKindForSort maps a relationship/annotation effect sort to the fixed
// per-family EventKind its journal row carries. A non-family sort returns ok=false.
func MutationFamilyKindForSort(sort EffectSort) (EventKind, bool) {
	return semanticMutationFamilyKind(sort)
}

// ---------------------------------------------------------------------------
// Payload codecs (closed shapes)
// ---------------------------------------------------------------------------

// EdgeMutationPayload is the journaled operand set of an edge-add/edge-remove family row:
// the opaque target handle and the typed edge relationship kind. The source task is the
// row's TaskID (authorized/attributed), not part of the payload.
type EdgeMutationPayload struct {
	Target   string   `json:"target"`
	EdgeKind EdgeKind `json:"edge_kind"`
}

// LabelMutationPayload is the journaled operand of a label-add/label-remove family row.
type LabelMutationPayload struct {
	Label string `json:"label"`
}

// CommentMutationPayload is the journaled operand set of a comment-add family row: the
// caller-minted comment id (carried so a replay reproduces the SAME id, never a fresh
// one), the comment's author, and its body. The committing actor is the row anchor's
// actor; the author may differ and is recorded here.
type CommentMutationPayload struct {
	CommentID string `json:"comment_id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
}

// EncodeEdgeMutationPayload / DecodeEdgeMutationPayload round-trip the edge operands.
func EncodeEdgeMutationPayload(target string, kind EdgeKind) (json.RawMessage, error) {
	b, err := json.Marshal(EdgeMutationPayload{Target: target, EdgeKind: kind})
	if err != nil {
		return nil, fmt.Errorf("provenance: encode edge mutation payload: %w", err)
	}
	return b, nil
}

func DecodeEdgeMutationPayload(payload []byte) (EdgeMutationPayload, error) {
	var p EdgeMutationPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return EdgeMutationPayload{}, fmt.Errorf(
			"provenance: decode edge mutation payload %q: %w — where: edges projection fold (§6, §15); "+
				"impact: the edge cannot be reproduced from journal history; fix: the row payload must "+
				"record {target, edge_kind}", string(payload), err)
	}
	if p.Target == "" {
		return EdgeMutationPayload{}, fmt.Errorf(
			"provenance: edge mutation payload %q has an empty target — where: edges projection fold (§6); "+
				"impact: the edge is not reproducible; fix: journal a non-empty edge target", string(payload))
	}
	return p, nil
}

// EncodeLabelMutationPayload / DecodeLabelMutationPayload round-trip the label operand.
func EncodeLabelMutationPayload(label string) (json.RawMessage, error) {
	b, err := json.Marshal(LabelMutationPayload{Label: label})
	if err != nil {
		return nil, fmt.Errorf("provenance: encode label mutation payload: %w", err)
	}
	return b, nil
}

func DecodeLabelMutationPayload(payload []byte) (LabelMutationPayload, error) {
	var p LabelMutationPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return LabelMutationPayload{}, fmt.Errorf(
			"provenance: decode label mutation payload %q: %w — where: labels projection fold (§6, §15); "+
				"impact: the label cannot be reproduced from journal history; fix: the row payload must "+
				"record {label}", string(payload), err)
	}
	if p.Label == "" {
		return LabelMutationPayload{}, fmt.Errorf(
			"provenance: label mutation payload %q has an empty label — where: labels projection fold (§6); "+
				"impact: the label is not reproducible; fix: journal a non-empty label", string(payload))
	}
	return p, nil
}

// EncodeCommentMutationPayload / DecodeCommentMutationPayload round-trip the comment
// operands. commentID/author/body are all required; a replay re-inserts the same comment.
func EncodeCommentMutationPayload(commentID, author, body string) (json.RawMessage, error) {
	b, err := json.Marshal(CommentMutationPayload{CommentID: commentID, Author: author, Body: body})
	if err != nil {
		return nil, fmt.Errorf("provenance: encode comment mutation payload: %w", err)
	}
	return b, nil
}

func DecodeCommentMutationPayload(payload []byte) (CommentMutationPayload, error) {
	var p CommentMutationPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return CommentMutationPayload{}, fmt.Errorf(
			"provenance: decode comment mutation payload %q: %w — where: comments projection fold (§6, §15); "+
				"impact: the comment cannot be reproduced from journal history; fix: the row payload must "+
				"record {comment_id, author, body}", string(payload), err)
	}
	if p.CommentID == "" || p.Author == "" {
		return CommentMutationPayload{}, fmt.Errorf(
			"provenance: comment mutation payload %q is missing comment_id or author — where: comments "+
				"projection fold (§6); impact: the comment is not reproducible; fix: journal a non-empty "+
				"comment_id and author", string(payload))
	}
	return p, nil
}
