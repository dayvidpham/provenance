package sqlite

import (
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
)

// TestFactPageBindingDedupesRepeatedKinds pins the one semantic difference
// between the old multi-kind IN and the per-kind UNION ALL: IN de-duplicates a
// repeated kind, UNION ALL would return that kind's rows once per branch and
// could therefore emit duplicate page rows or consume the page limit twice.
func TestFactPageBindingDedupesRepeatedKinds(t *testing.T) {
	page := journal.FactPageRequest{Limit: 10, SnapshotMaxJournalID: 100}
	kinds := []string{"fixture.traverse", "fixture.other", "fixture.traverse"}
	binding, err := buildFactPageMatchBinding(factSelectorEvidence, page, journal.FactFilter{}, kinds)
	if err != nil {
		t.Fatalf("build page binding: %v", err)
	}
	if got, want := len(binding.pageKinds), 2; got != want {
		t.Fatalf("page kinds = %v, want %d de-duplicated values", binding.pageKinds, want)
	}
	if got := strings.Count(binding.kind.pageMatchSQL(len(binding.pageKinds)), "UNION ALL"); got != 1 {
		t.Fatalf("page SQL has %d UNION ALL separators, want one per extra kind (1)", got)
	}
	// The shared predicate ends at ?15 and each branch binds one kind after it.
	if got := strings.Count(binding.kind.pageMatchSQL(len(binding.pageKinds)), "= ?16"); got != 1 {
		t.Fatalf("page SQL has %d ?16 kind slots, want 1", got)
	}
	if got := strings.Count(binding.kind.pageMatchSQL(len(binding.pageKinds)), "= ?17"); got != 1 {
		t.Fatalf("page SQL has %d ?17 kind slots, want 1", got)
	}
}
