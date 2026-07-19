package journal

import (
	"testing"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// TestMutationFamilyKindsMatchSorts pins the closed set of journaled relationship/
// annotation families and the sort→kind mapping: each of the five effect sorts maps to a
// distinct family kind, and MutationFamilyKinds enumerates exactly those.
func TestMutationFamilyKindsMatchSorts(t *testing.T) {
	sorts := []EffectSort{EffectEdgeAdd, EffectEdgeRemove, EffectLabelAdd, EffectLabelRemove, EffectCommentAdd}
	seen := map[EventKind]bool{}
	for _, s := range sorts {
		kind, ok := MutationFamilyKindForSort(s)
		if !ok {
			t.Errorf("MutationFamilyKindForSort(%s) reported not-a-family", s)
			continue
		}
		if !IsMutationFamilyKind(kind) {
			t.Errorf("kind %s for sort %s is not IsMutationFamilyKind", kind, s)
		}
		if seen[kind] {
			t.Errorf("sort %s mapped to a duplicate family kind %s", s, kind)
		}
		seen[kind] = true
	}
	fams := MutationFamilyKinds()
	if len(fams) != len(sorts) {
		t.Fatalf("MutationFamilyKinds has %d kinds, want %d", len(fams), len(sorts))
	}
	for _, k := range fams {
		if !seen[k] {
			t.Errorf("MutationFamilyKinds includes %s, not produced by any sort", k)
		}
	}
	// A non-family sort/kind is correctly classified as such.
	if _, ok := MutationFamilyKindForSort(EffectTaskEvent); ok {
		t.Errorf("EffectTaskEvent classified as a mutation family")
	}
	if IsMutationFamilyKind(EventKindTaskUpdated) {
		t.Errorf("EventKindTaskUpdated classified as a mutation family")
	}
	// Family kinds are non-lifecycle: they never move the status projection.
	for _, k := range fams {
		if _, isLifecycle := StatusForEventKind(k); isLifecycle {
			t.Errorf("mutation-family kind %s is (wrongly) a status lifecycle kind", k)
		}
	}
}

// TestEdgeMutationPayloadRoundTrip pins the edge operand codec and its fail-closed guards.
func TestEdgeMutationPayloadRoundTrip(t *testing.T) {
	enc, err := EncodeEdgeMutationPayload("aura--tgt", ptypes.EdgeBlockedBy)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	p, err := DecodeEdgeMutationPayload(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Target != "aura--tgt" || p.EdgeKind != ptypes.EdgeBlockedBy {
		t.Errorf("round-trip = %+v, want target=aura--tgt kind=blocked_by", p)
	}
	if _, err := DecodeEdgeMutationPayload([]byte(`{"edge_kind":0}`)); err == nil {
		t.Errorf("decode of an empty-target edge payload = nil, want a fail-closed error")
	}
	if _, err := DecodeEdgeMutationPayload([]byte(`not json`)); err == nil {
		t.Errorf("decode of malformed edge payload = nil, want a fail-closed error")
	}
}

// TestLabelAndCommentPayloadRoundTrip pins the label/comment operand codecs.
func TestLabelAndCommentPayloadRoundTrip(t *testing.T) {
	le, err := EncodeLabelMutationPayload("priority")
	if err != nil {
		t.Fatalf("encode label: %v", err)
	}
	lp, err := DecodeLabelMutationPayload(le)
	if err != nil || lp.Label != "priority" {
		t.Errorf("label round-trip = (%+v, %v), want label=priority", lp, err)
	}
	if _, err := DecodeLabelMutationPayload([]byte(`{}`)); err == nil {
		t.Errorf("decode of empty-label payload = nil, want a fail-closed error")
	}

	ce, err := EncodeCommentMutationPayload("aura--c1", "aura--author", "hello")
	if err != nil {
		t.Fatalf("encode comment: %v", err)
	}
	cp, err := DecodeCommentMutationPayload(ce)
	if err != nil || cp.CommentID != "aura--c1" || cp.Author != "aura--author" || cp.Body != "hello" {
		t.Errorf("comment round-trip = (%+v, %v)", cp, err)
	}
	if _, err := DecodeCommentMutationPayload([]byte(`{"body":"x"}`)); err == nil {
		t.Errorf("decode of comment payload missing id/author = nil, want a fail-closed error")
	}
}
