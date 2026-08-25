package provenance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/internal/allocation"
)

// Every actionable error in this module renders one message that has to carry
// what failed, where, what it means, and how to fix it. Half of them have no
// underlying error to wrap, so the message must not claim a lost cause. These
// tests pin the rendered text at representative sites rather than trusting the
// format strings by inspection.

type messageOracle struct {
	name        string
	err         error
	mustHave    []string
	mustNotHave []string
}

func TestActionableErrorMessagesRenderCauseOnlyWhenThereIsOne(t *testing.T) {
	underlying := errors.New("driver: database is closed")
	oracles := []messageOracle{
		{
			name: "governed allocation without cause",
			err:  allocation.NewError(allocation.ErrorValidation, "op-1", "child validation", "child 0 has an empty title", "nothing was written", "supply a title", nil),
			mustHave: []string{
				`provenance: governed allocation validation for operation "op-1"`,
				"where: child validation",
				"why: child 0 has an empty title",
				"impact: nothing was written",
				"fix: supply a title",
			},
			mustNotHave: []string{"cause"},
		},
		{
			name:        "governed allocation with cause",
			err:         allocation.NewError(allocation.ErrorCorruption, "op-2", "receipt replay", "the stored receipt cannot be decoded", "the result cannot be trusted", "restore the row from a consistent backup", underlying),
			mustHave:    []string{"fix: restore the row from a consistent backup", "cause: driver: database is closed"},
			mustNotHave: []string{"<nil>"},
		},
		{
			name:        "store unavailable without cause",
			err:         &StoreUnavailableError{Operation: "Show", Store: "tasks", Stage: "read", Impact: "the read returned nothing", Fix: "reopen the tracker on a live handle"},
			mustHave:    []string{"provenance: store unavailable during Show", "fix: reopen the tracker on a live handle"},
			mustNotHave: []string{"cause"},
		},
		{
			name:        "store unavailable with cause",
			err:         &StoreUnavailableError{Operation: "Show", Store: "tasks", Stage: "read", Impact: "the read returned nothing", Fix: "reopen the tracker on a live handle", Cause: underlying},
			mustHave:    []string{"cause: driver: database is closed"},
			mustNotHave: []string{"<nil>"},
		},
		{
			name:        "apply wait canceled without cause",
			err:         &ApplyWaitCanceledError{Operation: "op-3", Stage: DBOSDiagStageWorkflowAwait, Impact: "the durable work continues", Fix: "re-issue the same operation"},
			mustHave:    []string{`provenance: apply wait canceled for operation "op-3"`, "fix: re-issue the same operation"},
			mustNotHave: []string{"cause"},
		},
		{
			name:        "checkpoint divergence with cause",
			err:         &CheckpointDivergenceError{Operation: "op-4", Stage: DBOSDiagStageWorkflowAwait, Impact: "nothing was repaired", Fix: "inspect the journal", Cause: underlying},
			mustHave:    []string{"checkpoint divergence", "cause: driver: database is closed"},
			mustNotHave: []string{"<nil>"},
		},
		{
			name:        "DBOS diagnostic without cause",
			err:         &DBOSDiagnosticError{Class: DBOSDiagClassContextFrame, Field: DBOSDiagFieldOperation, Stage: DBOSDiagStageContextDecode, Operation: "op-5", Reason: "the operation ID is empty", Impact: "the workflow was not entered", Fix: "supply an operation ID"},
			mustHave:    []string{"provenance DBOS diagnostic", "fix: supply an operation ID"},
			mustNotHave: []string{"cause"},
		},
	}
	for _, oracle := range oracles {
		t.Run(oracle.name, func(t *testing.T) {
			message := oracle.err.Error()
			for _, want := range oracle.mustHave {
				if !strings.Contains(message, want) {
					t.Errorf("message omits %q\nmessage: %s", want, message)
				}
			}
			for _, unwanted := range oracle.mustNotHave {
				if strings.Contains(message, unwanted) {
					t.Errorf("message contains %q, which is not true of this failure\nmessage: %s", unwanted, message)
				}
			}
		})
	}
}

// TestNoErrorFormatsAnAlwaysPresentCauseClause is the completeness half: a new
// error type that hard-codes "cause: %v" would print "cause: <nil>" for every
// failure diagnosed without an underlying error, which is exactly the shape the
// oracle above was written to remove.
func TestNoErrorFormatsAnAlwaysPresentCauseClause(t *testing.T) {
	roots := []string{".", "internal/allocation", "internal/fusedtx", "internal/journal", "internal/sqlite", "pkg/ptypes"}
	scanned := 0
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read %s: %v", root, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(root, name)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			scanned++
			if strings.Contains(string(content), "cause: %v") {
				t.Errorf("%s formats an unconditional cause clause; render it with causeClause so a failure with no underlying error does not claim a lost one", path)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("cause-clause scan examined no production Go file -- where: TestNoErrorFormatsAnAlwaysPresentCauseClause; why: the root list no longer matches the module layout; impact: the check passes regardless of what the code formats; fix: update the roots list")
	}
}
