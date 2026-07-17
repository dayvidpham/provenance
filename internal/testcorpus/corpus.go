// Package testcorpus provides a pure, typed contract for YAML-backed test
// corpora. It deliberately does not import testing; loud test failures live in
// the sibling assert package.
package testcorpus

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Classification is the closed acceptance classification for a corpus case.
type Classification string

const (
	MustPass Classification = "must-pass"
	MustFail Classification = "must-fail"
)

func (c Classification) IsValid() bool { return c == MustPass || c == MustFail }

// ProvenanceSource is the closed set of reasons a case belongs in a corpus.
type ProvenanceSource string

const (
	SourceRequirement ProvenanceSource = "requirement"
	SourceBug         ProvenanceSource = "bug"
	SourceEnum        ProvenanceSource = "enum"
	SourceBoundary    ProvenanceSource = "boundary"
	SourceManual      ProvenanceSource = "manual"
)

func (s ProvenanceSource) IsValid() bool {
	switch s {
	case SourceRequirement, SourceBug, SourceEnum, SourceBoundary, SourceManual:
		return true
	default:
		return false
	}
}

// Provenance records why a case exists and a concrete reference to its source.
type Provenance struct {
	Source ProvenanceSource `yaml:"source"`
	Ref    string           `yaml:"ref"`
}

// OperatorName identifies one closed, typed Go mutation operator. YAML may
// select an operator, but can never contain executable code or untyped payloads.
type OperatorName string

// Mutation records the single controlled change represented by a case.
type Mutation struct {
	Description string       `yaml:"description"`
	Operator    OperatorName `yaml:"operator"`
}

// Case is one statically typed input-to-expected example.
type Case[I, E any] struct {
	Name           string         `yaml:"name"`
	Classification Classification `yaml:"classification"`
	Provenance     Provenance     `yaml:"provenance"`
	Mutation       Mutation       `yaml:"mutation"`
	Input          I              `yaml:"input"`
	Expected       E              `yaml:"expected"`
}

// Corpus is an ordered collection of cases with one input/expected shape.
type Corpus[I, E any] struct {
	Cases []Case[I, E] `yaml:"cases"`
}

// LoadCorpus strictly decodes exactly one YAML document into a typed corpus.
func LoadCorpus[I, E any](data []byte) (Corpus[I, E], error) {
	return LoadYAML[Corpus[I, E]](data)
}

// LoadYAML strictly decodes exactly one YAML document. Unknown fields and
// trailing documents are rejected so misspelled or decorative fixture fields
// cannot silently evade the typed harness.
func LoadYAML[T any](data []byte) (T, error) {
	var value T
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode typed corpus YAML: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return value, fmt.Errorf("decode typed corpus YAML: multiple YAML documents are not allowed")
		}
		return value, fmt.Errorf("decode typed corpus YAML trailing document: %w", err)
	}
	return value, nil
}

func (c Case[I, E]) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("case name is required")
	}
	if !c.Classification.IsValid() {
		return fmt.Errorf("case %q classification %q is not must-pass or must-fail", c.Name, c.Classification)
	}
	if !c.Provenance.Source.IsValid() {
		return fmt.Errorf("case %q provenance source %q is not in the closed set", c.Name, c.Provenance.Source)
	}
	if strings.TrimSpace(c.Provenance.Ref) == "" {
		return fmt.Errorf("case %q provenance ref is required", c.Name)
	}
	if strings.TrimSpace(c.Mutation.Description) == "" {
		return fmt.Errorf("case %q mutation description is required", c.Name)
	}
	if strings.TrimSpace(string(c.Mutation.Operator)) == "" {
		return fmt.Errorf("case %q mutation operator is required", c.Name)
	}
	return nil
}

func (c Corpus[I, E]) Validate() error {
	if len(c.Cases) == 0 {
		return fmt.Errorf("corpus has no cases")
	}
	seen := make(map[string]struct{}, len(c.Cases))
	for index, testCase := range c.Cases {
		if err := testCase.Validate(); err != nil {
			return fmt.Errorf("case %d: %w", index, err)
		}
		if _, exists := seen[testCase.Name]; exists {
			return fmt.Errorf("case %d duplicates name %q", index, testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
	}
	return nil
}

func (c Corpus[I, E]) CheckMin(minimum int) error {
	if minimum <= 0 {
		return fmt.Errorf("corpus minimum must be positive, got %d", minimum)
	}
	if len(c.Cases) < minimum {
		return fmt.Errorf("corpus has %d cases, requires at least %d", len(c.Cases), minimum)
	}
	return nil
}

func (c Corpus[I, E]) CheckExact(exact int) error {
	if exact <= 0 {
		return fmt.Errorf("corpus exact count must be positive, got %d", exact)
	}
	if len(c.Cases) != exact {
		return fmt.Errorf("corpus has %d cases, requires exactly %d", len(c.Cases), exact)
	}
	return nil
}
