package testcorpus

import (
	"fmt"
	"strings"
)

// Operator receives the complete typed input and expected value. This keeps the
// executable seam in Go while fixtures contain only data and symbolic names.
type Operator[I, E any] func(input I, expected E) error

// Operators is a closed registry for one statically typed corpus arm.
type Operators[I, E any] map[OperatorName]Operator[I, E]

// ExecuteCase validates and executes one case through its named typed operator.
func ExecuteCase[I, E any](testCase Case[I, E], operators Operators[I, E]) error {
	if err := testCase.Validate(); err != nil {
		return err
	}
	operator, ok := operators[testCase.Mutation.Operator]
	if !ok || operator == nil {
		return fmt.Errorf("case %q names unknown operator %q", testCase.Name, testCase.Mutation.Operator)
	}
	if err := operator(testCase.Input, testCase.Expected); err != nil {
		return fmt.Errorf("case %q operator %q: %w", testCase.Name, testCase.Mutation.Operator, err)
	}
	return nil
}

// ExecuteCorpus executes every case and rejects both unknown fixture operators
// and registry entries that no case exercises.
func ExecuteCorpus[I, E any](corpus Corpus[I, E], operators Operators[I, E]) error {
	if err := corpus.Validate(); err != nil {
		return err
	}
	used := make(map[OperatorName]struct{}, len(corpus.Cases))
	for _, testCase := range corpus.Cases {
		if err := ExecuteCase(testCase, operators); err != nil {
			return err
		}
		used[testCase.Mutation.Operator] = struct{}{}
	}
	for name, operator := range operators {
		if strings.TrimSpace(string(name)) == "" || operator == nil {
			return fmt.Errorf("operator registry contains invalid operator %q", name)
		}
		if _, ok := used[name]; !ok {
			return fmt.Errorf("operator registry entry %q is not exercised by the corpus", name)
		}
	}
	return nil
}

// Axis is one named, ordered dimension of a bounded Cartesian product.
type Axis struct {
	Name   string   `yaml:"name"`
	Values []string `yaml:"values"`
}

// Combination is one assignment from every declared axis.
type Combination map[string]string

// ExpandAxes returns a deterministic Cartesian product in declaration order.
func ExpandAxes(axes []Axis, maximum int) ([]Combination, error) {
	if maximum <= 0 {
		return nil, fmt.Errorf("combination maximum must be positive")
	}
	if len(axes) == 0 {
		return nil, fmt.Errorf("at least one axis is required")
	}
	result := []Combination{{}}
	seenAxes := make(map[string]struct{}, len(axes))
	for axisIndex, axis := range axes {
		name := strings.TrimSpace(axis.Name)
		if name == "" {
			return nil, fmt.Errorf("axis %d name is required", axisIndex)
		}
		if _, exists := seenAxes[name]; exists {
			return nil, fmt.Errorf("axis %d duplicates name %q", axisIndex, name)
		}
		seenAxes[name] = struct{}{}
		if len(axis.Values) == 0 {
			return nil, fmt.Errorf("axis %q requires at least one value", name)
		}
		seenValues := make(map[string]struct{}, len(axis.Values))
		for valueIndex, value := range axis.Values {
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("axis %q value %d is empty", name, valueIndex)
			}
			if _, exists := seenValues[value]; exists {
				return nil, fmt.Errorf("axis %q duplicates value %q", name, value)
			}
			seenValues[value] = struct{}{}
		}
		next := make([]Combination, 0, len(result)*len(axis.Values))
		for _, prefix := range result {
			for _, value := range axis.Values {
				combination := make(Combination, len(prefix)+1)
				for key, existing := range prefix {
					combination[key] = existing
				}
				combination[name] = value
				next = append(next, combination)
				if len(next) > maximum {
					return nil, fmt.Errorf("axis expansion exceeds bounded maximum %d", maximum)
				}
			}
		}
		result = next
	}
	return result, nil
}

// ExecuteAxes expands and executes every combination. Callers cannot satisfy a
// product fixture by merely checking its count.
func ExecuteAxes(axes []Axis, maximum int, execute func(Combination) error) error {
	if execute == nil {
		return fmt.Errorf("axis executor is required")
	}
	combinations, err := ExpandAxes(axes, maximum)
	if err != nil {
		return err
	}
	for index, combination := range combinations {
		if err := execute(combination); err != nil {
			return fmt.Errorf("execute combination %d %v: %w", index, combination, err)
		}
	}
	return nil
}

// CheckClosedSet requires exact, duplicate-free coverage of a live closed set.
// It is the freshness seam for enum and symbolic-operator inventories.
func CheckClosedSet[T comparable](want, got []T) error {
	wantSet, err := uniqueSet("required", want)
	if err != nil {
		return err
	}
	gotSet, err := uniqueSet("covered", got)
	if err != nil {
		return err
	}
	for value := range wantSet {
		if _, ok := gotSet[value]; !ok {
			return fmt.Errorf("closed-set coverage is stale: missing %v", value)
		}
	}
	for value := range gotSet {
		if _, ok := wantSet[value]; !ok {
			return fmt.Errorf("closed-set coverage contains unknown value %v", value)
		}
	}
	return nil
}

func uniqueSet[T comparable](name string, values []T) (map[T]struct{}, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("%s closed set is empty", name)
	}
	set := make(map[T]struct{}, len(values))
	for _, value := range values {
		if _, exists := set[value]; exists {
			return nil, fmt.Errorf("%s closed set duplicates %v", name, value)
		}
		set[value] = struct{}{}
	}
	return set, nil
}

// CoveragePredicate names one structural property that at least one case must
// retain, protecting against count-preserving fixture swaps.
type CoveragePredicate[I, E any] struct {
	Name  string
	Match func(Case[I, E]) bool
}

func CheckPredicates[I, E any](corpus Corpus[I, E], predicates []CoveragePredicate[I, E]) error {
	if len(predicates) == 0 {
		return fmt.Errorf("at least one coverage predicate is required")
	}
	seen := make(map[string]struct{}, len(predicates))
	for index, predicate := range predicates {
		if strings.TrimSpace(predicate.Name) == "" || predicate.Match == nil {
			return fmt.Errorf("coverage predicate %d requires a name and matcher", index)
		}
		if _, exists := seen[predicate.Name]; exists {
			return fmt.Errorf("coverage predicate %d duplicates name %q", index, predicate.Name)
		}
		seen[predicate.Name] = struct{}{}
		matched := false
		for _, testCase := range corpus.Cases {
			if predicate.Match(testCase) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("corpus is stale: no case satisfies coverage predicate %q", predicate.Name)
		}
	}
	return nil
}
