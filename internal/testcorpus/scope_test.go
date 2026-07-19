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
		{Operator: "order-by-journalid", Slice: SliceS11},
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
		{Operator: "order-by-journalid", Slice: SliceS11},
		{Operator: "deleted-operator", Slice: SliceS12}, // stale: no case uses this anymore
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
		{Operator: "order-by-journalid", Slice: SliceS11},
		{Operator: "apply-genesis-operation", Slice: SliceS12},
	}}
	used := map[OperatorName]struct{}{
		"order-by-journalid":      {},
		"apply-genesis-operation": {},
	}
	if err := table.CheckPartition(used); err != nil {
		t.Fatalf("CheckPartition rejected exact coverage: %v", err)
	}
}

// TestScopeTableValidateRejectsUnregisteredHandlerSlice proves that an
// scope-entry naming a slice outside the closed s1.1/s1.2/s1.3 set (which
// would otherwise let a typo silently route an operator to no handler stage
// at all) is rejected by Validate before CheckPartition ever runs.
func TestScopeTableValidateRejectsUnregisteredHandlerSlice(t *testing.T) {
	table := ScopeTable{Entries: []ScopeEntry{
		{Operator: "mystery-operator", Slice: Slice("s1.4")},
	}}
	if err := table.Validate(); err == nil {
		t.Fatal("Validate accepted an out-of-range slice label; want an error")
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
