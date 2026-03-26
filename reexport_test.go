package providence_test

// reexport_test.go verifies that every exported symbol from pkg/ptypes is
// re-exported by the root providence package. Uses reflect to scan both
// packages so that adding a new type, constant, or function to pkg/ptypes
// without a corresponding re-export will be caught automatically.
//
// Strategy:
//   - Reflect-based: scan all exported names from ptypes and verify each
//     has a corresponding export in the root providence package
//   - Runtime: errors.Is checks to confirm sentinel errors are the same objects
//   - Runtime: parse function results match between packages

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dayvidpham/providence"
	"github.com/dayvidpham/providence/pkg/ptypes"
)

// collectExportedNames parses Go source files in dir and returns all exported
// top-level names (types, constants, variables, functions). Skips _test.go files.
func collectExportedNames(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("collectExportedNames: ReadDir(%q) failed: %v", dir, err)
	}

	seen := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("collectExportedNames: ParseFile(%q) failed: %v", path, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							seen[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								seen[n.Name] = true
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() {
					seen[d.Name.Name] = true
				}
			}
		}
	}

	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// TestReexportCompleteness scans pkg/ptypes for all exported names and verifies
// that each one has a corresponding export in the root providence package.
// This replaces the old hand-maintained assignment checks.
func TestReexportCompleteness(t *testing.T) {
	// Find the module root by locating go.mod relative to test binary working dir.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd failed: %v", err)
	}

	ptypesDir := filepath.Join(wd, "pkg", "ptypes")
	rootDir := wd

	ptypesNames := collectExportedNames(t, ptypesDir)
	rootNames := collectExportedNames(t, rootDir)

	rootSet := make(map[string]bool, len(rootNames))
	for _, n := range rootNames {
		rootSet[n] = true
	}

	// Names that are intentionally NOT re-exported (internal helpers, etc.).
	// Currently empty — all ptypes exports should be re-exported.
	skip := map[string]bool{}

	var missing []string
	for _, name := range ptypesNames {
		if skip[name] {
			continue
		}
		if !rootSet[name] {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		t.Errorf("pkg/ptypes exports %d names not re-exported by root package:\n  %s\n"+
			"Fix: add re-exports in reexports.go for each missing name.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// Runtime: sentinel error identity
// ---------------------------------------------------------------------------
// errors.Is confirms the re-exported vars point to the same error values.

func TestReexportSentinelErrorIdentity(t *testing.T) {
	cases := []struct {
		name     string
		rootErr  error
		ptypeErr error
	}{
		{"ErrNotFound", providence.ErrNotFound, ptypes.ErrNotFound},
		{"ErrCycleDetected", providence.ErrCycleDetected, ptypes.ErrCycleDetected},
		{"ErrAlreadyClosed", providence.ErrAlreadyClosed, ptypes.ErrAlreadyClosed},
		{"ErrInvalidID", providence.ErrInvalidID, ptypes.ErrInvalidID},
		{"ErrAgentKindMismatch", providence.ErrAgentKindMismatch, ptypes.ErrAgentKindMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !errors.Is(c.rootErr, c.ptypeErr) {
				t.Errorf("providence.%s is not errors.Is-identical to ptypes.%s", c.name, c.name)
			}
			if !errors.Is(c.ptypeErr, c.rootErr) {
				t.Errorf("ptypes.%s is not errors.Is-identical to providence.%s", c.name, c.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Runtime: parse function identity
// ---------------------------------------------------------------------------
// Verifies that the root parse functions produce the same results.

func TestReexportParseFunctionIdentity(t *testing.T) {
	validID := "ns--018f4b12-3456-7890-abcd-ef0123456789"

	// ParseTaskID
	rootTask, rootErr := providence.ParseTaskID(validID)
	ptypeTask, ptypeErr := ptypes.ParseTaskID(validID)
	if rootErr != nil || ptypeErr != nil {
		t.Fatalf("ParseTaskID errors: root=%v, ptypes=%v", rootErr, ptypeErr)
	}
	if rootTask != ptypeTask {
		t.Errorf("ParseTaskID result mismatch: root=%+v, ptypes=%+v", rootTask, ptypeTask)
	}

	// ParseAgentID
	rootAgent, rootErr := providence.ParseAgentID(validID)
	ptypeAgent, ptypeErr := ptypes.ParseAgentID(validID)
	if rootErr != nil || ptypeErr != nil {
		t.Fatalf("ParseAgentID errors: root=%v, ptypes=%v", rootErr, ptypeErr)
	}
	if rootAgent != ptypeAgent {
		t.Errorf("ParseAgentID result mismatch: root=%+v, ptypes=%+v", rootAgent, ptypeAgent)
	}

	// ParseActivityID
	rootAct, rootErr := providence.ParseActivityID(validID)
	ptypeAct, ptypeErr := ptypes.ParseActivityID(validID)
	if rootErr != nil || ptypeErr != nil {
		t.Fatalf("ParseActivityID errors: root=%v, ptypes=%v", rootErr, ptypeErr)
	}
	if rootAct != ptypeAct {
		t.Errorf("ParseActivityID result mismatch: root=%+v, ptypes=%+v", rootAct, ptypeAct)
	}

	// ParseCommentID
	rootComment, rootErr := providence.ParseCommentID(validID)
	ptypeComment, ptypeErr := ptypes.ParseCommentID(validID)
	if rootErr != nil || ptypeErr != nil {
		t.Fatalf("ParseCommentID errors: root=%v, ptypes=%v", rootErr, ptypeErr)
	}
	if rootComment != ptypeComment {
		t.Errorf("ParseCommentID result mismatch: root=%+v, ptypes=%+v", rootComment, ptypeComment)
	}
}
