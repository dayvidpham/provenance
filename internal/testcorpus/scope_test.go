package testcorpus

import (
	"strings"
	"testing"
)

// TestCheckPartitionDetectsMissingScopeEntry proves CheckPartition's first
// direction fires: a corpus operator that is actually used (present in the
// `used` set the harness builds from real fixture files) but has no
// corresponding entry in the checked-in scope table.
func TestCheckPartitionDetectsMissingScopeEntry(t *testing.T) {
	table := ScopeTable{Entries: []ScopeEntry{
		{Operator: "order-by-journalid", Area: AreaJournalFoundation},
	}}
	used := map[OperatorName]struct{}{
		"order-by-journalid":      {},
		"apply-genesis-operation": {}, // used by a case, but never entered into the table
	}
	err := table.CheckPartition(used)
	if err == nil {
		t.Fatal("CheckPartition accepted a used operator with no scope-table entry; want an error naming it")
	}
	if got := err.Error(); !containsAll(got, "apply-genesis-operation") {
		t.Errorf("CheckPartition error %q does not name the unregistered operator", got)
	}
}

// TestCheckPartitionDetectsStaleScopeEntry proves CheckPartition's second
// direction fires: a scope-table entry naming an operator no corpus case
// actually uses anymore (e.g. left behind after a fixture was deleted or
// renamed).
func TestCheckPartitionDetectsStaleScopeEntry(t *testing.T) {
	table := ScopeTable{Entries: []ScopeEntry{
		{Operator: "order-by-journalid", Area: AreaJournalFoundation},
		{Operator: "deleted-operator", Area: AreaOperationLifecycle}, // stale: no case uses this anymore
	}}
	used := map[OperatorName]struct{}{
		"order-by-journalid": {},
	}
	err := table.CheckPartition(used)
	if err == nil {
		t.Fatal("CheckPartition accepted a stale scope-table entry; want an error naming it")
	}
	if got := err.Error(); !containsAll(got, "deleted-operator") {
		t.Errorf("CheckPartition error %q does not name the stale entry", got)
	}
}

// TestCheckPartitionAcceptsExactCoverage is the control case: a scope table
// that exactly matches the used set passes, proving the two failure tests
// above are exercising real negative branches and not a permanently-erroring
// function.
func TestCheckPartitionAcceptsExactCoverage(t *testing.T) {
	table := ScopeTable{Entries: []ScopeEntry{
		{Operator: "order-by-journalid", Area: AreaJournalFoundation},
		{Operator: "apply-genesis-operation", Area: AreaOperationLifecycle},
	}}
	used := map[OperatorName]struct{}{
		"order-by-journalid":      {},
		"apply-genesis-operation": {},
	}
	if err := table.CheckPartition(used); err != nil {
		t.Fatalf("CheckPartition rejected exact coverage: %v", err)
	}
}

// TestScopeTableValidateRejectsUnregisteredBehaviorArea proves a typo cannot
// silently route an operator away from every production handler.
func TestScopeTableValidateRejectsUnregisteredBehaviorArea(t *testing.T) {
	table := ScopeTable{Entries: []ScopeEntry{
		{Operator: "mystery-operator", Area: BehaviorArea("unknown")},
	}}
	if err := table.Validate(); err == nil {
		t.Fatal("Validate accepted an unregistered behavior area; want an error")
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			return false
		}
	}
	return true
}
