package testcorpus

import (
	"fmt"
	"strings"
)

// BehaviorArea is the stable behavior family a corpus operator exercises.
// The corpus harness uses these areas to verify that every operator has exactly
// one production handler.
type BehaviorArea string

const (
	AreaJournalFoundation    BehaviorArea = "journal-foundation"
	AreaOperationLifecycle   BehaviorArea = "operation-lifecycle"
	AreaRecoveryAndMigration BehaviorArea = "recovery-and-migration"
)

// IsValid reports whether a is a known behavior area.
func (a BehaviorArea) IsValid() bool {
	switch a {
	case AreaJournalFoundation, AreaOperationLifecycle, AreaRecoveryAndMigration:
		return true
	default:
		return false
	}
}

// ScopeEntry assigns one operator to its behavior area.
type ScopeEntry struct {
	Operator OperatorName `yaml:"operator"`
	Area     BehaviorArea `yaml:"area"`
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
		if !e.Area.IsValid() {
			return fmt.Errorf("scope entry %q: behavior area %q is not registered", e.Operator, e.Area)
		}
		if _, dup := seen[e.Operator]; dup {
			return fmt.Errorf("scope entry %d duplicates operator %q", i, e.Operator)
		}
		seen[e.Operator] = struct{}{}
	}
	return nil
}

// AreaOf returns the behavior area an operator is partitioned to.
func (t ScopeTable) AreaOf(op OperatorName) (BehaviorArea, bool) {
	for _, e := range t.Entries {
		if e.Operator == op {
			return e.Area, true
		}
	}
	return "", false
}

// Operators returns every operator in a behavior area, in table order.
func (t ScopeTable) Operators(area BehaviorArea) []OperatorName {
	var out []OperatorName
	for _, e := range t.Entries {
		if e.Area == area {
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
		if _, ok := t.AreaOf(op); !ok {
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
