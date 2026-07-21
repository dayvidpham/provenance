package journal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"testing"
)

func TestCanonicalWireArchitectureRejectsStringlyFieldsAndVersions(t *testing.T) {
	source, err := os.ReadFile("canonical_mutation.go")
	if err != nil {
		t.Fatal(err)
	}
	findings, err := inspectCanonicalArchitecture(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("canonical architecture violations: %v", findings)
	}
}

func TestCanonicalWireArchitectureGuardNegativeControls(t *testing.T) {
	bad := []byte(`package journal
func mutationEncodingText() string { return "other" }
func bad(w *canonicalWriter, r *canonicalReader) { w.field("effect.0.task", nil); r.field("version"); _ = "provenance.mutation.v1" }
`)
	findings, err := inspectCanonicalArchitecture(bad)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) < 3 {
		t.Fatalf("negative control produced %d findings, want field writer, field reader, and misplaced version literal: %v", len(findings), findings)
	}
	clean := []byte(`package journal
func mutationEncodingText() string { return "provenance.mutation.v1" }
func good(w *canonicalWriter, r *canonicalReader, ref canonicalFieldRef) { w.field(ref, nil); r.field(ref) }
`)
	if findings, err := inspectCanonicalArchitecture(clean); err != nil || len(findings) != 0 {
		t.Fatalf("clean control findings=%v err=%v", findings, err)
	}
}

func inspectCanonicalArchitecture(source []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "canonical.go", source, 0)
	if err != nil {
		return nil, err
	}
	var findings []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.BasicLit:
				value, _ := strconv.Unquote(n.Value)
				if value == "provenance.mutation.v1" && function.Name.Name != "mutationEncodingText" {
					findings = append(findings, "version literal outside codec mapping")
				}
			case *ast.CallExpr:
				selector, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "field" || len(n.Args) == 0 {
					break
				}
				if _, raw := n.Args[0].(*ast.BasicLit); raw {
					findings = append(findings, "raw string canonical field argument")
				}
			}
			return true
		})
	}
	return findings, nil
}
