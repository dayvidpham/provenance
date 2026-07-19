package testcorpus

import (
	"fmt"
	"strings"
)

// Slice is the closed set of implementation leaves a corpus operator can belong
// to. It is the executable-scope partition consumed by the corpus harness so
// out-of-scope histories become recorded obligations rather than skipped tests.
type Slice string

const (
	SliceS11 Slice = "s1.1" // journal base + task_event subtype + projections + actor namespace
	SliceS12 Slice = "s1.2" // operations, effects, results, authority lifecycle
	SliceS13 Slice = "s1.3" // shared reducer, replay, migration
)

// IsValid reports whether s is a known slice.
func (s Slice) IsValid() bool {
	switch s {
	case SliceS11, SliceS12, SliceS13:
		return true
	default:
		return false
	}
}

// ScopeEntry assigns one operator to the slice that makes it executable.
type ScopeEntry struct {
	Operator OperatorName `yaml:"operator"`
	Slice    Slice        `yaml:"slice"`
	Reason   string       `yaml:"reason,omitempty"`
}

// ScopeTable is the checked-in operator→slice partition.
type ScopeTable struct {
	Entries []ScopeEntry `yaml:"operators"`
}

// LoadScopeTable strictly decodes exactly one scope-table YAML document.
func LoadScopeTable(data []byte) (ScopeTable, error) {
	return LoadYAML[ScopeTable](data)
}

// Validate rejects an empty, duplicate, or malformed partition.
func (t ScopeTable) Validate() error {
	if len(t.Entries) == 0 {
		return fmt.Errorf("scope table has no operator entries")
	}
	seen := make(map[OperatorName]struct{}, len(t.Entries))
	for i, e := range t.Entries {
		if strings.TrimSpace(string(e.Operator)) == "" {
			return fmt.Errorf("scope entry %d: operator is required", i)
		}
		if !e.Slice.IsValid() {
			return fmt.Errorf("scope entry %q: slice %q is not one of s1.1/s1.2/s1.3", e.Operator, e.Slice)
		}
		if _, dup := seen[e.Operator]; dup {
			return fmt.Errorf("scope entry %d duplicates operator %q", i, e.Operator)
		}
		seen[e.Operator] = struct{}{}
	}
	return nil
}

// SliceOf returns the slice an operator is partitioned to.
func (t ScopeTable) SliceOf(op OperatorName) (Slice, bool) {
	for _, e := range t.Entries {
		if e.Operator == op {
			return e.Slice, true
		}
	}
	return "", false
}

// Operators returns every operator partitioned to a given slice, in table order.
func (t ScopeTable) Operators(slice Slice) []OperatorName {
	var out []OperatorName
	for _, e := range t.Entries {
		if e.Slice == slice {
			out = append(out, e.Operator)
		}
	}
	return out
}

// CheckPartition verifies the table exactly covers the operators the corpus
// actually uses: every used operator has a scope entry, and every scope entry
// names an operator some case uses. This keeps the partition honest in both
// directions — a new corpus operator with no scope entry, or a stale table entry
// for a deleted operator, both fail.
func (t ScopeTable) CheckPartition(used map[OperatorName]struct{}) error {
	for op := range used {
		if _, ok := t.SliceOf(op); !ok {
			return fmt.Errorf("corpus operator %q has no scope-table entry — add it to testdata/contract/scope.yaml", op)
		}
	}
	for _, e := range t.Entries {
		if _, ok := used[e.Operator]; !ok {
			return fmt.Errorf("scope-table entry %q names an operator no corpus case uses — remove the stale entry", e.Operator)
		}
	}
	return nil
}
