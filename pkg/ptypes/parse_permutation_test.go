package ptypes_test

import (
	_ "embed"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/dayvidpham/providence/pkg/ptypes"
)

// ---------------------------------------------------------------------------
// Fixture types — mirrors testdata/fixtures.yaml structure
// ---------------------------------------------------------------------------

type ParseIDFixtures struct {
	ParseID ParseIDSpec `yaml:"parse_id"`
}

type ParseIDSpec struct {
	IDTypes    []IDTypeSpec     `yaml:"id_types"`
	Namespaces []NamespaceSpec  `yaml:"namespaces"`
	UUIDs      []UUIDSpec       `yaml:"uuids"`
	ErrorCases []ParseErrorCase `yaml:"error_cases"`
}

type IDTypeSpec struct {
	Name      string `yaml:"name"`
	ParseFunc string `yaml:"parse_func"`
}

type NamespaceSpec struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type UUIDSpec struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type ParseErrorCase struct {
	Name  string `yaml:"name"`
	Input string `yaml:"input"`
	Error string `yaml:"error"`
}

// ---------------------------------------------------------------------------
// Fixture loading
// ---------------------------------------------------------------------------

//go:embed testdata/fixtures.yaml
var parseIDFixtureData []byte

func loadParseIDFixtures(t *testing.T) ParseIDFixtures {
	t.Helper()
	var f ParseIDFixtures
	if err := yaml.Unmarshal(parseIDFixtureData, &f); err != nil {
		t.Fatalf(
			"parse_permutation_test: failed to unmarshal testdata/fixtures.yaml: %v — "+
				"ensure the file exists and is valid YAML",
			err,
		)
	}
	return f
}

// ---------------------------------------------------------------------------
// parseFunc dispatchers — one per ID type
// Each returns (namespace, uuidStr, error) so callers can assert field values.
// ---------------------------------------------------------------------------

func callParseTaskID(input string) (string, uuid.UUID, error) {
	id, err := ptypes.ParseTaskID(input)
	return id.Namespace, id.UUID, err
}

func callParseAgentID(input string) (string, uuid.UUID, error) {
	id, err := ptypes.ParseAgentID(input)
	return id.Namespace, id.UUID, err
}

func callParseActivityID(input string) (string, uuid.UUID, error) {
	id, err := ptypes.ParseActivityID(input)
	return id.Namespace, id.UUID, err
}

func callParseCommentID(input string) (string, uuid.UUID, error) {
	id, err := ptypes.ParseCommentID(input)
	return id.Namespace, id.UUID, err
}

// dispatchParse maps an IDTypeSpec.ParseFunc name to the actual parser.
// Returns nil if the name is unknown (test will fail with a clear message).
func dispatchParse(parseFunc string) func(string) (string, uuid.UUID, error) {
	switch parseFunc {
	case "ParseTaskID":
		return callParseTaskID
	case "ParseAgentID":
		return callParseAgentID
	case "ParseActivityID":
		return callParseActivityID
	case "ParseCommentID":
		return callParseCommentID
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Success permutations: 4 id_types × 6 namespaces × 3 uuids = 72 cases
// ---------------------------------------------------------------------------

func TestParseID_SuccessPermutations(t *testing.T) {
	fix := loadParseIDFixtures(t)
	spec := fix.ParseID

	count := 0
	for _, idType := range spec.IDTypes {
		parseFn := dispatchParse(idType.ParseFunc)
		if parseFn == nil {
			t.Errorf(
				"TestParseID_SuccessPermutations: unknown parse_func %q in fixtures.yaml — "+
					"expected one of ParseTaskID, ParseAgentID, ParseActivityID, ParseCommentID",
				idType.ParseFunc,
			)
			continue
		}

		for _, ns := range spec.Namespaces {
			for _, uuidSpec := range spec.UUIDs {
				input := ns.Value + "--" + uuidSpec.Value
				testName := fmt.Sprintf("%s/%s/%s", idType.Name, ns.Name, uuidSpec.Name)

				t.Run(testName, func(t *testing.T) {
					gotNS, gotUUID, err := parseFn(input)
					if err != nil {
						t.Fatalf(
							"%s(%q) returned unexpected error: %v — "+
								"input was constructed from namespace=%q uuid=%q",
							idType.ParseFunc, input, err, ns.Value, uuidSpec.Value,
						)
					}
					if gotNS != ns.Value {
						t.Errorf(
							"%s(%q).Namespace = %q, want %q",
							idType.ParseFunc, input, gotNS, ns.Value,
						)
					}
					wantUUID, parseErr := uuid.Parse(uuidSpec.Value)
					if parseErr != nil {
						t.Fatalf(
							"fixture uuid %q is not a valid UUID: %v — fix testdata/fixtures.yaml",
							uuidSpec.Value, parseErr,
						)
					}
					if gotUUID != wantUUID {
						t.Errorf(
							"%s(%q).UUID = %v, want %v",
							idType.ParseFunc, input, gotUUID, wantUUID,
						)
					}
				})
				count++
			}
		}
	}
	t.Logf(
		"Generated %d success permutation test cases (%d id_types × %d namespaces × %d uuids)",
		count, len(spec.IDTypes), len(spec.Namespaces), len(spec.UUIDs),
	)
}

// ---------------------------------------------------------------------------
// Error permutations: 4 id_types × 8 error_cases = 32 cases
// ---------------------------------------------------------------------------

func TestParseID_ErrorPermutations(t *testing.T) {
	fix := loadParseIDFixtures(t)
	spec := fix.ParseID

	count := 0
	for _, idType := range spec.IDTypes {
		parseFn := dispatchParse(idType.ParseFunc)
		if parseFn == nil {
			t.Errorf(
				"TestParseID_ErrorPermutations: unknown parse_func %q in fixtures.yaml — "+
					"expected one of ParseTaskID, ParseAgentID, ParseActivityID, ParseCommentID",
				idType.ParseFunc,
			)
			continue
		}

		for _, ec := range spec.ErrorCases {
			testName := fmt.Sprintf("%s/%s", idType.Name, ec.Name)

			t.Run(testName, func(t *testing.T) {
				_, _, err := parseFn(ec.Input)
				if err == nil {
					t.Errorf(
						"%s(%q) expected error wrapping ErrInvalidID, got nil — "+
							"this input should be rejected by the parser",
						idType.ParseFunc, ec.Input,
					)
					return
				}
				if !errors.Is(err, ptypes.ErrInvalidID) {
					t.Errorf(
						"%s(%q) error = %v, want an error wrapping ptypes.ErrInvalidID — "+
							"use errors.Is to detect parse failures",
						idType.ParseFunc, ec.Input, err,
					)
				}
			})
			count++
		}
	}
	t.Logf(
		"Generated %d error permutation test cases (%d id_types × %d error_cases)",
		count, len(spec.IDTypes), len(spec.ErrorCases),
	)
}
