// Package assert is the testing-only assertion seam for testcorpus.
package assert

import (
	"testing"

	"github.com/dayvidpham/provenance/internal/testcorpus"
)

func RequireValid[I, E any](t testing.TB, corpus testcorpus.Corpus[I, E]) {
	t.Helper()
	if err := corpus.Validate(); err != nil {
		t.Fatalf("validate test corpus: %v", err)
	}
}

func RequireMin[I, E any](t testing.TB, corpus testcorpus.Corpus[I, E], minimum int) {
	t.Helper()
	if err := corpus.CheckMin(minimum); err != nil {
		t.Fatalf("guard test corpus minimum: %v", err)
	}
}

func RequireExact[I, E any](t testing.TB, corpus testcorpus.Corpus[I, E], exact int) {
	t.Helper()
	if err := corpus.CheckExact(exact); err != nil {
		t.Fatalf("guard test corpus exact count: %v", err)
	}
}
