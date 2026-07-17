package testcorpus

import "testing"

// TestCheckClosedSetDetectsUnknownGotValue proves CheckClosedSet's
// unknown-value branch fires: `got` contains a value `want` never declared.
// This models the real harness's "an s1.2-scoped operator was accidentally
// given an s1.1 handler" scenario — the registered-handler set (`got`) picks
// up an operator that the s1.1 scope partition (`want`) does not name — which
// TestContractCorpusPartitionIsComplete guards against on the production
// scope.yaml.
func TestCheckClosedSetDetectsUnknownGotValue(t *testing.T) {
	want := []OperatorName{"order-by-journalid", "claim-two-disjoint-namespaces"} // the s1.1 partition
	got := []OperatorName{
		"order-by-journalid", "claim-two-disjoint-namespaces",
		"apply-genesis-operation", // stray: this operator is scoped s1.2, not s1.1
	}
	err := CheckClosedSet(want, got)
	if err == nil {
		t.Fatal("CheckClosedSet accepted a got value absent from the required set; want an error")
	}
}

// TestCheckClosedSetDetectsMissingWantValue proves CheckClosedSet's
// missing-value branch fires: `want` contains a value `got` never covers —
// modeling an s1.1-scoped operator that has NO registered handler at all.
func TestCheckClosedSetDetectsMissingWantValue(t *testing.T) {
	want := []OperatorName{"order-by-journalid", "claim-two-disjoint-namespaces"}
	got := []OperatorName{"order-by-journalid"} // claim-two-disjoint-namespaces has no handler
	err := CheckClosedSet(want, got)
	if err == nil {
		t.Fatal("CheckClosedSet accepted a want value missing from got; want an error")
	}
}

// TestCheckClosedSetAcceptsExactMatch is the control case: identical sets
// pass, proving the two failure tests above exercise real negative branches.
func TestCheckClosedSetAcceptsExactMatch(t *testing.T) {
	want := []OperatorName{"order-by-journalid", "claim-two-disjoint-namespaces"}
	got := []OperatorName{"claim-two-disjoint-namespaces", "order-by-journalid"} // order-independent
	if err := CheckClosedSet(want, got); err != nil {
		t.Fatalf("CheckClosedSet rejected an exact (reordered) match: %v", err)
	}
}

// TestCheckClosedSetRejectsDuplicates proves a duplicate within either input
// slice is rejected rather than silently deduplicated, which would otherwise
// mask a doubly-registered handler.
func TestCheckClosedSetRejectsDuplicates(t *testing.T) {
	want := []OperatorName{"order-by-journalid"}
	got := []OperatorName{"order-by-journalid", "order-by-journalid"}
	if err := CheckClosedSet(want, got); err == nil {
		t.Fatal("CheckClosedSet accepted a duplicate got value; want an error")
	}
}

// TestExecuteCaseRejectsUnknownOperator proves ExecuteCase fails loudly when
// a case names an operator absent from the registry — the "missing operator
// handler" scenario for the case-execution seam (distinct from the
// scope-table partition checked by CheckPartition/CheckClosedSet above).
func TestExecuteCaseRejectsUnknownOperator(t *testing.T) {
	testCase := Case[anyTestMap, anyTestMap]{
		Name:           "case-with-unregistered-operator",
		Classification: MustPass,
		Provenance:     Provenance{Source: SourceRequirement, Ref: "test"},
		Mutation:       Mutation{Description: "d", Operator: "does-not-exist"},
	}
	operators := Operators[anyTestMap, anyTestMap]{
		"registered-operator": func(anyTestMap, anyTestMap) error { return nil },
	}
	if err := ExecuteCase(testCase, operators); err == nil {
		t.Fatal("ExecuteCase accepted a case naming an unregistered operator; want an error")
	}
}

// TestExecuteCorpusRejectsUnexercisedRegistryEntry proves ExecuteCorpus fails
// when the operator registry contains a handler no case in the corpus
// actually exercises — the "unregistered handler leftover" direction, mirror
// of CheckPartition's stale-entry check but for the executable Go registry
// rather than the scope table.
func TestExecuteCorpusRejectsUnexercisedRegistryEntry(t *testing.T) {
	corpus := Corpus[anyTestMap, anyTestMap]{Cases: []Case[anyTestMap, anyTestMap]{
		{
			Name:           "only-case",
			Classification: MustPass,
			Provenance:     Provenance{Source: SourceRequirement, Ref: "test"},
			Mutation:       Mutation{Description: "d", Operator: "used-operator"},
		},
	}}
	operators := Operators[anyTestMap, anyTestMap]{
		"used-operator":     func(anyTestMap, anyTestMap) error { return nil },
		"unexercised-extra": func(anyTestMap, anyTestMap) error { return nil },
	}
	if err := ExecuteCorpus(corpus, operators); err == nil {
		t.Fatal("ExecuteCorpus accepted a registry entry no case exercises; want an error")
	}
}

type anyTestMap = map[string]any
