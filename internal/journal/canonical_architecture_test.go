package journal

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCanonicalWireArchitectureRejectsCrossFileBypasses(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var findings []string
	versionOwners := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		matched, err := build.Default.MatchFile(".", name)
		if err != nil {
			t.Fatalf("match compiled file %s: %v", name, err)
		}
		if !matched {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		fileFindings, owners, err := inspectCanonicalArchitecture(name, source)
		if err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		findings = append(findings, fileFindings...)
		versionOwners += owners
	}
	if versionOwners != 1 {
		findings = append(findings, "V1 wire tag must have exactly one codec-registry owner")
	}
	if len(findings) != 0 {
		t.Fatalf("canonical architecture violations: %v", findings)
	}
}

func TestCanonicalWireArchitectureGuardNegativeControls(t *testing.T) {
	controls := map[string]string{
		"raw field":       `package journal; func bad(w *canonicalWriter) { w.field("effect.0.task", nil) }`,
		"field variable":  `package journal; func bad(w *canonicalWriter, ref canonicalV1FieldRef) { w.field(ref, nil) }`,
		"formatted field": `package journal; import "fmt"; func bad() { _ = fmt.Sprintf("effect.%d.task", 0) }`,
		"direct reference": `package journal; func bad() canonicalV1FieldRef {
			return canonicalV1EffectRef{index: 0, field: effectTask}
		}`,
		"misplaced version": `package journal; func bad() string { return "provenance.mutation.v1" }`,
		"parallel family map": `package journal; var bad = map[EffectSort]string{
			EffectTaskEvent: "task_event",
		}`,
		"string version API": `package journal; func IsSupportedMutationEncoding(version string) bool {
			return version != ""
		}`,
	}
	for name, source := range controls {
		findings, _, err := inspectCanonicalArchitecture("bypass.go", []byte(source))
		if err != nil {
			t.Fatalf("%s parse: %v", name, err)
		}
		if len(findings) == 0 {
			t.Errorf("%s bypass was not rejected", name)
		}
	}
	clean := []byte(`package journal
	func good(w *canonicalWriter, r *canonicalReader) {
		w.field(effectField(0, effectTask), nil)
		_, _ = r.field(contextField(0, 0, contextKind))
	}`)
	if findings, _, err := inspectCanonicalArchitecture("clean.go", clean); err != nil || len(findings) != 0 {
		t.Fatalf("clean control findings=%v err=%v", findings, err)
	}
}

func inspectCanonicalArchitecture(filename string, source []byte) ([]string, int, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, source, 0)
	if err != nil {
		return nil, 0, err
	}
	allowedVersionLiteral := map[*ast.BasicLit]bool{}
	allowedFamilyLiteral := map[*ast.BasicLit]bool{}
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 {
				continue
			}
			name := value.Names[0].Name
			ast.Inspect(value, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if ok && name == "canonicalCodecRegistry" && unquote(literal) == "provenance.mutation.v1" {
					allowedVersionLiteral[literal] = true
				}
				if ok && name == "canonicalV1Families" && canonicalV1FamilyTags[unquote(literal)] {
					allowedFamilyLiteral[literal] = true
				}
				return true
			})
		}
	}

	var findings []string
	versionOwners := 0
	functionName := ""
	for _, declaration := range file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if isFunction {
			functionName = function.Name.Name
		} else {
			functionName = ""
		}
		ast.Inspect(declaration, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.BasicLit:
				if unquote(n) == "provenance.mutation.v1" {
					if filename == "canonical_mutation.go" && allowedVersionLiteral[n] {
						versionOwners++
					} else {
						findings = append(findings, filename+": V1 wire tag outside codec registry")
					}
				}
			case *ast.CompositeLit:
				if isCanonicalReferenceType(n.Type) && !isCanonicalReferenceConstructor(functionName) {
					findings = append(findings, filename+": direct scope-reference construction")
				}
				if compositeDefinesEffectFamily(n, allowedFamilyLiteral) {
					findings = append(findings, filename+": EffectSort family tag outside V1 family registry")
				}
			case *ast.CallExpr:
				selector, ok := n.Fun.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == "field" && (len(n.Args) == 0 || !isCanonicalReferenceCall(n.Args[0])) {
					findings = append(findings, filename+": canonical field call bypasses sealed constructor")
				}
				if ok && selector.Sel.Name == "Sprintf" && len(n.Args) > 0 {
					format, _ := n.Args[0].(*ast.BasicLit)
					if strings.HasPrefix(unquote(format), "effect.") && functionName != "renderFieldName" {
						findings = append(findings, filename+": formatted canonical field outside V1 renderer")
					}
				}
			}
			return true
		})
		if function, ok := declaration.(*ast.FuncDecl); ok {
			if function.Name.Name == "IsSupportedMutationEncoding" && firstParameterType(function) != "MutationEncodingVersion" {
				findings = append(findings, filename+": supported-version API is stringly typed")
			}
			if function.Name.Name == "EncodingVersion" && resultType(function) != "MutationEncodingVersion" {
				findings = append(findings, filename+": encoding-version API is stringly typed")
			}
		}
	}
	return findings, versionOwners, nil
}

var canonicalV1FamilyTags = map[string]bool{
	"task_event": true, "bootstrap_authority": true, "assignment_start": true,
	"assignment_end": true, "decision": true, "evidence": true,
	"task_create": true, "edge_add": true, "edge_remove": true,
	"label_add": true, "label_remove": true, "comment_add": true,
	"task_create_allocated": true,
}

func compositeDefinesEffectFamily(composite *ast.CompositeLit, allowed map[*ast.BasicLit]bool) bool {
	hasEffectSort := false
	hasUnownedTag := false
	ast.Inspect(composite, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok && strings.HasPrefix(identifier.Name, "Effect") && identifier.Name != "EffectSort" {
			hasEffectSort = true
		}
		if literal, ok := node.(*ast.BasicLit); ok && canonicalV1FamilyTags[unquote(literal)] && !allowed[literal] {
			hasUnownedTag = true
		}
		return true
	})
	return hasEffectSort && hasUnownedTag
}

func unquote(literal *ast.BasicLit) string {
	if literal == nil || literal.Kind != token.STRING {
		return ""
	}
	value, _ := strconv.Unquote(literal.Value)
	return value
}

func isCanonicalReferenceCall(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	identifier, ok := call.Fun.(*ast.Ident)
	return ok && (identifier.Name == "envelopeField" || identifier.Name == "effectField" || identifier.Name == "contextField")
}

func isCanonicalReferenceType(expression ast.Expr) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && (identifier.Name == "canonicalV1EnvelopeRef" || identifier.Name == "canonicalV1EffectRef" || identifier.Name == "canonicalV1ContextRef")
}

func isCanonicalReferenceConstructor(name string) bool {
	return name == "envelopeField" || name == "effectField" || name == "contextField"
}

func firstParameterType(function *ast.FuncDecl) string {
	if function.Type.Params == nil || len(function.Type.Params.List) == 0 {
		return ""
	}
	identifier, _ := function.Type.Params.List[0].Type.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}

func resultType(function *ast.FuncDecl) string {
	if function.Type.Results == nil || len(function.Type.Results.List) != 1 {
		return ""
	}
	identifier, _ := function.Type.Results.List[0].Type.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}
