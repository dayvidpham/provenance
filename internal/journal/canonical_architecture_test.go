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

func TestCanonicalWireArchitectureHasOneStructuralAuthority(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string][]byte{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		matched, err := build.Default.MatchFile(".", name)
		if err != nil {
			t.Fatalf("match %s: %v", name, err)
		}
		if matched {
			sources[name], err = os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	findings, authorities, err := inspectCanonicalArchitecture(sources)
	if err != nil {
		t.Fatal(err)
	}
	if authorities.codecRegistries != 1 || authorities.familyRegistries != 1 || authorities.renderers != 1 || authorities.constructors != 3 {
		findings = append(findings, "canonical package must have one codec registry, one family registry, one V1 renderer, and three sealed constructors")
	}
	if len(findings) != 0 {
		t.Fatalf("canonical architecture violations: %v", findings)
	}
}

func TestCanonicalWireArchitectureRejectsSameNameAndCrossFileBypasses(t *testing.T) {
	authoritySource, err := os.ReadFile("canonical_mutation.go")
	if err != nil {
		t.Fatal(err)
	}
	controls := map[string]string{
		"same-name constructor extra expression": `package journal
			func effectField(index int, field canonicalEffectField) canonicalV1FieldRef {
				_ = "extra"
				return canonicalV1EffectRef{index: index, field: field}
			}`,
		"same-name renderer helper": `package journal
			func renderFieldName(index int) string { return fmt.Sprintf("effect.%d.task", index) }`,
		"renderer extra expression": `package journal
			func (canonicalV1Codec) renderFieldName(ref canonicalV1FieldRef) (string, error) {
				_ = fmt.Sprintf("effect.%d.extra", 0)
				switch ref.(type) { default: return "", nil }
			}`,
		"same-name codec registry variable": `package journal
			var canonicalCodecRegistry = []canonicalCodecDescriptor{{version: MutationEncodingV1, wireTag: "provenance.mutation.v1", decoder: decodeCanonicalMutationV1}}`,
		"second structurally valid registry": `package journal
			var secondRegistry = canonicalCodecDescriptors{
				canonicalCodecDescriptor{version: MutationEncodingV1, wireTag: "provenance.mutation.v1", decoder: decodeCanonicalMutationV1},
			}`,
		"direct scope reference": `package journal
			func bad() canonicalV1FieldRef { return canonicalV1ContextRef{effectIndex: 0, contextIndex: 0, field: contextKind} }`,
		"field helper indirection": `package journal
			func bad(w *canonicalWriter, ref canonicalV1FieldRef) { w.field(ref, nil) }`,
		"parallel family map": `package journal
			var bad = map[EffectSort]string{EffectTaskEvent: "anything"}`,
		"misplaced version": `package journal
			func bad() string { return "provenance.mutation.v1" }`,
	}
	for name, source := range controls {
		findings, authorities, err := inspectCanonicalArchitecture(map[string][]byte{
			"authority.go":          authoritySource,
			"adversarial_helper.go": []byte(source),
		})
		if err != nil {
			t.Fatalf("%s parse: %v", name, err)
		}
		if len(findings) == 0 && authorities.codecRegistries == 1 && authorities.familyRegistries == 1 && authorities.renderers == 1 && authorities.constructors == 3 {
			t.Errorf("%s bypass was not rejected", name)
		}
	}
}

type canonicalAuthorities struct{ codecRegistries, familyRegistries, renderers, constructors int }

func inspectCanonicalArchitecture(sources map[string][]byte) ([]string, canonicalAuthorities, error) {
	type parsedFile struct {
		name string
		file *ast.File
	}
	var files []parsedFile
	for name, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		if err != nil {
			return nil, canonicalAuthorities{}, err
		}
		files = append(files, parsedFile{name, file})
	}
	allowedCodecDescriptors := map[*ast.CompositeLit]bool{}
	allowedFamilyDescriptors := map[*ast.CompositeLit]bool{}
	allowedVersionLiterals := map[*ast.BasicLit]bool{}
	allowedRefs := map[*ast.CompositeLit]bool{}
	allowedFormats := map[*ast.BasicLit]bool{}
	sealedConstructors := map[string]bool{}
	var findings []string
	var authorities canonicalAuthorities

	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			if general, ok := declaration.(*ast.GenDecl); ok {
				for _, spec := range general.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok || len(value.Values) != 1 {
						continue
					}
					registry, ok := value.Values[0].(*ast.CompositeLit)
					if !ok {
						continue
					}
					switch identNameCanonical(registry.Type) {
					case "canonicalCodecDescriptors":
						if markCodecRegistry(registry, allowedCodecDescriptors, allowedVersionLiterals) {
							authorities.codecRegistries++
						} else {
							findings = append(findings, parsed.name+": malformed codec descriptor registry")
						}
					case "canonicalV1FamilyRegistry":
						if markFamilyRegistry(registry, allowedFamilyDescriptors) {
							authorities.familyRegistries++
						} else {
							findings = append(findings, parsed.name+": malformed V1 family descriptor registry")
						}
					}
				}
			}
			if function, ok := declaration.(*ast.FuncDecl); ok {
				if ref := exactSealedRefConstructor(function); ref != nil {
					allowedRefs[ref] = true
					sealedConstructors[function.Name.Name] = true
					authorities.constructors++
				}
				if formats := exactV1Renderer(function); len(formats) == 2 {
					for _, literal := range formats {
						allowedFormats[literal] = true
					}
					authorities.renderers++
				}
			}
		}
	}

	for _, parsed := range files {
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.BasicLit:
				text := unquoteCanonical(typed)
				if text == "provenance.mutation.v1" && !allowedVersionLiterals[typed] {
					findings = append(findings, parsed.name+": V1 wire tag outside structural codec registry")
				}
				if strings.HasPrefix(text, "effect.") && !allowedFormats[typed] {
					findings = append(findings, parsed.name+": canonical path literal outside structural V1 renderer")
				}
			case *ast.CompositeLit:
				typeName := identNameCanonical(typed.Type)
				if typeName == "canonicalCodecDescriptor" && !allowedCodecDescriptors[typed] {
					findings = append(findings, parsed.name+": codec descriptor outside sole registry")
				}
				if typeName == "canonicalV1FamilyDescriptor" && !allowedFamilyDescriptors[typed] {
					findings = append(findings, parsed.name+": family descriptor outside sole registry")
				}
				if isCanonicalRefType(typeName) && !allowedRefs[typed] {
					findings = append(findings, parsed.name+": direct or malformed scope-reference construction")
				}
				if mapType, ok := typed.Type.(*ast.MapType); ok && identNameCanonical(mapType.Key) == "EffectSort" && identNameCanonical(mapType.Value) == "string" {
					findings = append(findings, parsed.name+": parallel EffectSort string map")
				}
			case *ast.CallExpr:
				selector, ok := typed.Fun.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == "field" && (len(typed.Args) == 0 || !isSealedRefCall(typed.Args[0], sealedConstructors)) {
					findings = append(findings, parsed.name+": canonical field call bypasses sealed constructor")
				}
			}
			return true
		})
	}
	return findings, authorities, nil
}

func markCodecRegistry(registry *ast.CompositeLit, allowed map[*ast.CompositeLit]bool, versions map[*ast.BasicLit]bool) bool {
	if len(registry.Elts) == 0 {
		return false
	}
	for _, element := range registry.Elts {
		descriptor, ok := element.(*ast.CompositeLit)
		if !ok || identNameCanonical(descriptor.Type) != "canonicalCodecDescriptor" || !hasExactKeys(descriptor, "version", "wireTag", "decoder") {
			return false
		}
		wireTag, ok := keyedBasicLiteral(descriptor, "wireTag")
		if !ok || unquoteCanonical(wireTag) == "" {
			return false
		}
		allowed[descriptor] = true
		versions[wireTag] = true
	}
	return true
}

func markFamilyRegistry(registry *ast.CompositeLit, allowed map[*ast.CompositeLit]bool) bool {
	if len(registry.Elts) == 0 {
		return false
	}
	for _, element := range registry.Elts {
		descriptor, ok := element.(*ast.CompositeLit)
		if !ok || identNameCanonical(descriptor.Type) != "canonicalV1FamilyDescriptor" || !hasExactKeys(descriptor, "sort", "tag") {
			return false
		}
		tag, ok := keyedBasicLiteral(descriptor, "tag")
		if !ok || unquoteCanonical(tag) == "" {
			return false
		}
		allowed[descriptor] = true
	}
	return true
}

func exactSealedRefConstructor(function *ast.FuncDecl) *ast.CompositeLit {
	if function.Body == nil || len(function.Body.List) != 1 || function.Type.Results == nil || len(function.Type.Results.List) != 1 || identNameCanonical(function.Type.Results.List[0].Type) != "canonicalV1FieldRef" {
		return nil
	}
	returned, ok := function.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 1 {
		return nil
	}
	ref, ok := returned.Results[0].(*ast.CompositeLit)
	if !ok {
		return nil
	}
	parameters := map[string]bool{}
	for _, field := range function.Type.Params.List {
		for _, name := range field.Names {
			parameters[name.Name] = true
		}
	}
	wantKeys := map[string][]string{
		"canonicalV1EnvelopeRef": {"field"},
		"canonicalV1EffectRef":   {"index", "field"},
		"canonicalV1ContextRef":  {"effectIndex", "contextIndex", "field"},
	}[identNameCanonical(ref.Type)]
	if len(wantKeys) == 0 || !hasExactKeys(ref, wantKeys...) {
		return nil
	}
	for _, element := range ref.Elts {
		value, ok := element.(*ast.KeyValueExpr).Value.(*ast.Ident)
		if !ok || !parameters[value.Name] {
			return nil
		}
	}
	return ref
}

func exactV1Renderer(function *ast.FuncDecl) []*ast.BasicLit {
	if function.Recv == nil || len(function.Recv.List) != 1 || identNameCanonical(function.Recv.List[0].Type) != "canonicalV1Codec" || function.Type.Params == nil || len(function.Type.Params.List) != 1 || identNameCanonical(function.Type.Params.List[0].Type) != "canonicalV1FieldRef" || function.Type.Results == nil || len(function.Type.Results.List) != 2 || identNameCanonical(function.Type.Results.List[0].Type) != "string" || identNameCanonical(function.Type.Results.List[1].Type) != "error" || function.Body == nil || len(function.Body.List) != 1 {
		return nil
	}
	typeSwitch, ok := function.Body.List[0].(*ast.TypeSwitchStmt)
	if !ok {
		return nil
	}
	cases := map[string]bool{}
	defaultSeen := false
	for _, node := range typeSwitch.Body.List {
		clause := node.(*ast.CaseClause)
		if len(clause.List) == 0 {
			defaultSeen = true
			continue
		}
		if len(clause.List) != 1 {
			return nil
		}
		cases[identNameCanonical(clause.List[0])] = true
	}
	if !defaultSeen || len(cases) != 3 || !cases["canonicalV1EnvelopeRef"] || !cases["canonicalV1EffectRef"] || !cases["canonicalV1ContextRef"] {
		return nil
	}
	var formats []*ast.BasicLit
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Sprintf" || len(call.Args) == 0 {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if ok && strings.HasPrefix(unquoteCanonical(literal), "effect.") {
			formats = append(formats, literal)
		}
		return true
	})
	if len(formats) != 2 || unquoteCanonical(formats[0]) != "effect.%d.context.%d.%s" || unquoteCanonical(formats[1]) != "effect.%d.%s" {
		return nil
	}
	return formats
}

func hasExactKeys(composite *ast.CompositeLit, keys ...string) bool {
	if len(composite.Elts) != len(keys) {
		return false
	}
	want := map[string]bool{}
	for _, key := range keys {
		want[key] = true
	}
	for _, element := range composite.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok || !want[identNameCanonical(pair.Key)] {
			return false
		}
		delete(want, identNameCanonical(pair.Key))
	}
	return len(want) == 0
}

func keyedBasicLiteral(composite *ast.CompositeLit, key string) (*ast.BasicLit, bool) {
	for _, element := range composite.Elts {
		pair := element.(*ast.KeyValueExpr)
		if identNameCanonical(pair.Key) == key {
			literal, ok := pair.Value.(*ast.BasicLit)
			return literal, ok
		}
	}
	return nil, false
}

func identNameCanonical(expression ast.Expr) string {
	identifier, _ := expression.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}
func isCanonicalRefType(name string) bool {
	return name == "canonicalV1EnvelopeRef" || name == "canonicalV1EffectRef" || name == "canonicalV1ContextRef"
}
func isSealedRefCall(expression ast.Expr, constructors map[string]bool) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	return constructors[identNameCanonical(call.Fun)]
}
func unquoteCanonical(literal *ast.BasicLit) string {
	if literal == nil || literal.Kind != token.STRING {
		return ""
	}
	value, _ := strconv.Unquote(literal.Value)
	return value
}
