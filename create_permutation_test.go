package provenance_test

import (
	_ "embed"
	"errors"
	"fmt"
	"testing"

	"github.com/dayvidpham/provenance"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Fixture types — mirrors testdata/fixtures.yaml structure
// ---------------------------------------------------------------------------

type CreateFixtures struct {
	Create CreateSuite `yaml:"create"`
}

type CreateSuite struct {
	TaskTypes      []EnumEntry     `yaml:"task_types"`
	Priorities     []EnumEntry     `yaml:"priorities"`
	Phases         []EnumEntry     `yaml:"phases"`
	NamespaceCases []NamespaceCase `yaml:"namespace_cases"`
}

// EnumEntry holds a named enum axis value. The Value field is the iota integer.
type EnumEntry struct {
	Name  string `yaml:"name"`
	Value int    `yaml:"value"`
}

// NamespaceCase is a namespace validation test case.
type NamespaceCase struct {
	Name        string `yaml:"name"`
	Value       string `yaml:"value"`
	ExpectError bool   `yaml:"expect_error"`
}

// ---------------------------------------------------------------------------
// Fixture loading
// ---------------------------------------------------------------------------

//go:embed testdata/fixtures.yaml
var createFixtureData []byte

func loadCreateFixtures(t *testing.T) CreateFixtures {
	t.Helper()
	var f CreateFixtures
	if err := yaml.Unmarshal(createFixtureData, &f); err != nil {
		t.Fatalf("create_permutation_test: failed to parse testdata/fixtures.yaml: %v", err)
	}
	return f
}

// ---------------------------------------------------------------------------
// Create: combinatorial permutation tests (325 success cases)
// ---------------------------------------------------------------------------

// TestCreate_Permutations exercises all 5×5×13 = 325 combinations of
// TaskType × Priority × Phase, verifying that Create returns a well-formed
// Task with Status=StatusOpen and correctly round-tripped enum fields.
func TestCreate_Permutations(t *testing.T) {
	t.Parallel()
	fix := loadCreateFixtures(t)
	suite := fix.Create

	count := 0
	for _, tt := range suite.TaskTypes {
		count += len(suite.Priorities) * len(suite.Phases)
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()
			tr := openTestTrackerWithOptions(t, provenance.WithModelRegistry(provenance.NewRegistry(nil)))
			taskType := provenance.TaskType(tt.Value)

			for _, pr := range suite.Priorities {
				for _, ph := range suite.Phases {
					priority := provenance.Priority(pr.Value)
					phase := provenance.Phase(ph.Value)

					t.Run(fmt.Sprintf("%s/%s", pr.Name, ph.Name), func(t *testing.T) {
						task, err := tr.Create(
							"test-ns",
							"Permutation title",
							"Permutation description",
							taskType,
							priority,
							phase,
						)
						if err != nil {
							t.Fatalf(
								"Create(task_type=%s, priority=%s, phase=%s) returned unexpected error: %v — "+
									"each valid enum combination must succeed without error",
								tt.Name, pr.Name, ph.Name, err,
							)
						}

						// Status must always be StatusOpen on creation.
						if task.Status != provenance.StatusOpen {
							t.Errorf(
								"Status = %v, want StatusOpen — "+
									"Create must initialize all tasks with Status=StatusOpen",
								task.Status,
							)
						}

						// ID namespace must match the argument.
						if task.ID.Namespace != "test-ns" {
							t.Errorf(
								"ID.Namespace = %q, want %q — "+
									"Create must preserve the namespace argument in the returned Task.ID",
								task.ID.Namespace, "test-ns",
							)
						}

						// Enum fields must round-trip exactly.
						if task.Type != taskType {
							t.Errorf(
								"Type = %v (%d), want %v (%d) — "+
									"Create must store and return the TaskType argument unchanged",
								task.Type, int(task.Type), taskType, int(taskType),
							)
						}
						if task.Priority != priority {
							t.Errorf(
								"Priority = %v (%d), want %v (%d) — "+
									"Create must store and return the Priority argument unchanged",
								task.Priority, int(task.Priority), priority, int(priority),
							)
						}
						if task.Phase != phase {
							t.Errorf(
								"Phase = %v (%d), want %v (%d) — "+
									"Create must store and return the Phase argument unchanged",
								task.Phase, int(task.Phase), phase, int(phase),
							)
						}
					})
				}
			}
		})
	}

	t.Logf(
		"Generated %d permutation test cases (%d task_types × %d priorities × %d phases)",
		count, len(suite.TaskTypes), len(suite.Priorities), len(suite.Phases),
	)
}

// ---------------------------------------------------------------------------
// Create: namespace validation cases
// ---------------------------------------------------------------------------

// TestCreate_NamespaceValidation verifies that Create returns ErrInvalidID
// for an empty namespace and succeeds for valid namespace values.
func TestCreate_NamespaceValidation(t *testing.T) {
	fix := loadCreateFixtures(t)

	for _, nc := range fix.Create.NamespaceCases {
		nc := nc // capture loop variable
		t.Run(nc.Name, func(t *testing.T) {
			tr := openTestTracker(t)

			_, err := tr.Create(
				nc.Value,
				"title",
				"desc",
				provenance.TaskTypeBug,
				provenance.PriorityMedium,
				provenance.PhaseUnscoped,
			)

			if nc.ExpectError {
				if !errors.Is(err, provenance.ErrInvalidID) {
					t.Errorf(
						"Create(namespace=%q) error = %v, want ErrInvalidID — "+
							"an empty namespace is invalid and must be rejected with ErrInvalidID; "+
							"fix by providing a non-empty namespace string",
						nc.Value, err,
					)
				}
			} else {
				if err != nil {
					t.Errorf(
						"Create(namespace=%q) returned unexpected error: %v — "+
							"a non-empty namespace must be accepted without error",
						nc.Value, err,
					)
				}
			}
		})
	}
}
