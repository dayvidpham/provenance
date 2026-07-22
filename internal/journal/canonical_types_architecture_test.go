package journal

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// canonicalV1RendererBodyHash is SHA-256 of go/format's rendering of the
// renderFieldName function body. A legitimate renderer change fails with the
// replacement hash, which must be reviewed together with the V1 byte corpus
// before updating this seal.
const canonicalV1RendererBodyHash = "10dd69c44061b716c9987446c22c2fed109a606a0b7bce8f63f7f7002557c5b0"

type typedCanonicalPackage struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
	pkg   *types.Package
}

func TestCanonicalArchitectureUsesResolvedObjectsAndExactDataflow(t *testing.T) {
	typed := loadTypedCanonicalProduction(t)
	findings := inspectTypedCanonicalArchitecture(typed)
	if len(findings) != 0 {
		t.Fatalf("typed canonical architecture violations: %v", findings)
	}
}

func TestCanonicalTypedArchitectureRejectsAliasMethodValueAndAlteredRenderer(t *testing.T) {
	controls := map[string]func(map[string]string){
		"reference alias": func(sources map[string]string) {
			sources["alias_bypass.go"] = "package journal\ntype hiddenRef = canonicalV1EffectRef\n"
		},
		"field method value": func(sources map[string]string) {
			sources["canonical_mutation.go"] = strings.Replace(sources["canonical_mutation.go"],
				"w.field(envelopeField(envelopeVersion), []byte(wireTag))",
				"write := w.field\n\twrite(envelopeField(envelopeVersion), []byte(wireTag))", 1)
		},
		"renderer dead expression": func(sources map[string]string) {
			sources["canonical_mutation.go"] = strings.Replace(sources["canonical_mutation.go"],
				"func (canonicalV1Codec) renderFieldName(ref canonicalV1FieldRef) (string, error) {",
				"func (canonicalV1Codec) renderFieldName(ref canonicalV1FieldRef) (string, error) {\n\t_ = \"dead renderer evidence\"", 1)
		},
		"renderer altered return": func(sources map[string]string) {
			sources["canonical_mutation.go"] = strings.Replace(sources["canonical_mutation.go"],
				"return fmt.Sprintf(\"effect.%d.%s\", typed.index, name), nil",
				"return \"attacker-controlled-field-\" + name, nil", 1)
		},
	}
	for name, mutate := range controls {
		sources := canonicalProductionSources(t)
		mutate(sources)
		typed := typeCheckCanonicalSources(t, sources)
		if findings := inspectTypedCanonicalArchitecture(typed); len(findings) == 0 {
			t.Errorf("%s bypass was accepted", name)
		}
	}
}

func inspectTypedCanonicalArchitecture(typed typedCanonicalPackage) []string {
	parents := canonicalParents(typed.files)
	scope := typed.pkg.Scope()
	versionType := scope.Lookup("MutationEncodingVersion").Type()
	effectSortType := scope.Lookup("EffectSort").Type()
	fieldRefType := scope.Lookup("canonicalV1FieldRef").Type()
	codecType := scope.Lookup("canonicalV1Codec").Type()
	refTypes := map[types.Type]bool{
		scope.Lookup("canonicalV1EnvelopeRef").Type(): true,
		scope.Lookup("canonicalV1EffectRef").Type():   true,
		scope.Lookup("canonicalV1ContextRef").Type():  true,
	}
	versionConstants := map[*types.Const]bool{}
	effectConstants := map[*types.Const]bool{}
	for _, name := range scope.Names() {
		constant, ok := scope.Lookup(name).(*types.Const)
		if !ok {
			continue
		}
		if types.Identical(constant.Type(), versionType) {
			versionConstants[constant] = true
		}
		if types.Identical(constant.Type(), effectSortType) {
			effectConstants[constant] = true
		}
	}

	allowedRefs := map[*ast.CompositeLit]bool{}
	allowedWireLiterals := map[*ast.BasicLit]bool{}
	constructors := map[*types.Func]bool{}
	fieldMethods := map[*types.Func]bool{}
	var renderer *types.Func
	var rendererBody *ast.BlockStmt
	var codecDescriptorVersions = map[*types.Const]int{}
	var familyDescriptorSorts = map[*types.Const]int{}
	registryUses := map[types.Object]int{}
	var codecRegistryObject, familyRegistryObject types.Object
	var findings []string

	for _, file := range typed.files {
		for _, declaration := range file.Decls {
			switch value := declaration.(type) {
			case *ast.GenDecl:
				for _, spec := range value.Specs {
					if typeSpec, ok := spec.(*ast.TypeSpec); ok && typeSpec.Assign.IsValid() && refTypes[typed.info.TypeOf(typeSpec.Type)] {
						findings = append(findings, "canonical reference alias bypass")
					}
					valueSpec, ok := spec.(*ast.ValueSpec)
					if !ok || len(valueSpec.Values) != 1 {
						continue
					}
					composite, ok := valueSpec.Values[0].(*ast.CompositeLit)
					if !ok {
						continue
					}
					switch typed.info.TypeOf(composite).String() {
					case "github.com/dayvidpham/provenance/internal/journal.canonicalCodecDescriptors":
						if codecRegistryObject != nil {
							findings = append(findings, "multiple typed codec registries")
						}
						codecRegistryObject = typed.info.Defs[valueSpec.Names[0]]
						for _, element := range composite.Elts {
							descriptor, ok := element.(*ast.CompositeLit)
							version, wireTag, complete := exactTypedCodecDescriptor(descriptor, typed.info)
							if !ok || !complete {
								findings = append(findings, "codec registry contains an incomplete/non-static descriptor")
								continue
							}
							allowedWireLiterals[wireTag] = true
							constant, _ := typed.info.Uses[version].(*types.Const)
							codecDescriptorVersions[constant]++
						}
					case "github.com/dayvidpham/provenance/internal/journal.canonicalV1FamilyRegistry":
						if familyRegistryObject != nil {
							findings = append(findings, "multiple typed family registries")
						}
						familyRegistryObject = typed.info.Defs[valueSpec.Names[0]]
						for _, element := range composite.Elts {
							descriptor, ok := element.(*ast.CompositeLit)
							sortID, complete := exactTypedFamilyDescriptor(descriptor, typed.info)
							if !ok || !complete {
								findings = append(findings, "family registry contains an incomplete/non-static descriptor")
								continue
							}
							constant, _ := typed.info.Uses[sortID].(*types.Const)
							familyDescriptorSorts[constant]++
						}
					}
				}
			case *ast.FuncDecl:
				object, _ := typed.info.Defs[value.Name].(*types.Func)
				if object == nil {
					continue
				}
				signature := object.Type().(*types.Signature)
				if signature.Recv() == nil && signature.Results().Len() == 1 && types.Identical(signature.Results().At(0).Type(), fieldRefType) {
					if ref := exactTypedRefConstructor(value, typed.info, refTypes); ref != nil {
						constructors[object] = true
						allowedRefs[ref] = true
					} else {
						findings = append(findings, "altered canonical reference constructor dataflow")
					}
				}
				if signature.Recv() != nil && !types.Identical(derefCanonical(signature.Recv().Type()), codecType) && signature.Params().Len() > 0 && types.Identical(signature.Params().At(0).Type(), fieldRefType) {
					fieldMethods[object] = true
				}
				if signature.Recv() != nil && types.Identical(derefCanonical(signature.Recv().Type()), codecType) && signature.Params().Len() == 1 && types.Identical(signature.Params().At(0).Type(), fieldRefType) && signature.Results().Len() == 2 {
					hash := canonicalBodyHash(typed.fset, value.Body)
					if hash != canonicalV1RendererBodyHash {
						findings = append(findings, fmt.Sprintf("V1 renderer body/return dataflow differs from sealed shape; reviewed replacement hash would be %s", hash))
					} else if renderer != nil {
						findings = append(findings, "multiple V1 renderers")
					} else {
						renderer = object
						rendererBody = value.Body
					}
				}
			}
		}
	}

	for _, file := range typed.files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.BasicLit:
				text, _ := strconv.Unquote(value.Value)
				if text == "provenance.mutation.v1" && !allowedWireLiterals[value] {
					findings = append(findings, "V1 wire literal outside typed codec descriptor")
				}
				if strings.HasPrefix(text, "effect.") && !canonicalNodeWithin(value, rendererBody, parents) {
					findings = append(findings, "canonical field path literal outside exact renderer")
				}
			case *ast.CompositeLit:
				if refTypes[typed.info.TypeOf(value)] && !allowedRefs[value] {
					findings = append(findings, "direct/aliased canonical reference construction")
				}
				if mapType, ok := typed.info.TypeOf(value).Underlying().(*types.Map); ok && types.Identical(mapType.Key(), effectSortType) && mapType.Elem().String() == "string" {
					findings = append(findings, "parallel typed EffectSort string map")
				}
			case *ast.Ident:
				object := typed.info.Uses[value]
				if object == codecRegistryObject || object == familyRegistryObject {
					registryUses[object]++
				}
			case *ast.SelectorExpr:
				selection := typed.info.Selections[value]
				if selection == nil {
					return true
				}
				method, _ := selection.Obj().(*types.Func)
				if fieldMethods[method] {
					call, direct := parents[value].(*ast.CallExpr)
					if !direct || call.Fun != value || len(call.Args) == 0 || !typedConstructorCall(call.Args[0], typed.info, constructors) {
						findings = append(findings, "canonical field method value/helper indirection")
					}
				}
				if method == renderer {
					call, direct := parents[value].(*ast.CallExpr)
					if !direct || call.Fun != value {
						findings = append(findings, "V1 renderer method value/helper indirection")
					}
				}
			}
			return true
		})
	}
	if renderer == nil || len(constructors) != 3 || len(fieldMethods) != 2 {
		findings = append(findings, "canonical renderer/constructor/field method cardinality mismatch")
	}
	for constant := range versionConstants {
		if codecDescriptorVersions[constant] != 1 {
			findings = append(findings, "MutationEncodingVersion constant lacks exactly one complete descriptor: "+constant.Name())
		}
	}
	for constant := range codecDescriptorVersions {
		if !versionConstants[constant] {
			findings = append(findings, "codec descriptor references unregistered typed version")
		}
	}
	for constant := range effectConstants {
		if familyDescriptorSorts[constant] != 1 {
			findings = append(findings, "EffectSort constant lacks exactly one V1 family descriptor: "+constant.Name())
		}
	}
	for constant := range familyDescriptorSorts {
		if !effectConstants[constant] {
			findings = append(findings, "family descriptor references unregistered EffectSort")
		}
	}
	if codecRegistryObject == nil || registryUses[codecRegistryObject] < 5 {
		findings = append(findings, "codec registry is not consumed by all production lookup/prepare/decode paths")
	}
	if familyRegistryObject == nil || registryUses[familyRegistryObject] < 2 {
		findings = append(findings, "family registry is not consumed by both production encode and decode paths")
	}
	return findings
}

func exactTypedRefConstructor(function *ast.FuncDecl, info *types.Info, refTypes map[types.Type]bool) *ast.CompositeLit {
	if function.Body == nil || len(function.Body.List) != 1 {
		return nil
	}
	returned, ok := function.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 1 {
		return nil
	}
	composite, ok := returned.Results[0].(*ast.CompositeLit)
	if !ok || !refTypes[info.TypeOf(composite)] {
		return nil
	}
	named, _ := info.TypeOf(composite).(*types.Named)
	structure, _ := named.Underlying().(*types.Struct)
	if structure == nil || len(composite.Elts) != structure.NumFields() {
		return nil
	}
	parameters := map[types.Object]bool{}
	for _, field := range function.Type.Params.List {
		for _, name := range field.Names {
			parameters[info.Defs[name]] = true
		}
	}
	fields := map[types.Object]bool{}
	for index := 0; index < structure.NumFields(); index++ {
		fields[structure.Field(index)] = true
	}
	for _, element := range composite.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return nil
		}
		key, ok := pair.Key.(*ast.Ident)
		if !ok || !fields[info.Uses[key]] {
			return nil
		}
		delete(fields, info.Uses[key])
		identifier, ok := pair.Value.(*ast.Ident)
		if !ok || !parameters[info.Uses[identifier]] {
			return nil
		}
	}
	if len(fields) != 0 {
		return nil
	}
	return composite
}

func typedConstructorCall(expression ast.Expr, info *types.Info, constructors map[*types.Func]bool) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	identifier, ok := call.Fun.(*ast.Ident)
	if !ok {
		return false
	}
	function, _ := info.Uses[identifier].(*types.Func)
	return constructors[function]
}

func canonicalKeyValue(composite *ast.CompositeLit, key string) ast.Expr {
	for _, element := range composite.Elts {
		pair := element.(*ast.KeyValueExpr)
		if pair.Key.(*ast.Ident).Name == key {
			return pair.Value
		}
	}
	return nil
}

func exactTypedCodecDescriptor(descriptor *ast.CompositeLit, info *types.Info) (*ast.Ident, *ast.BasicLit, bool) {
	if descriptor == nil || len(descriptor.Elts) != 4 {
		return nil, nil, false
	}
	version, versionOK := canonicalKeyValue(descriptor, "version").(*ast.Ident)
	wireTag, wireOK := canonicalKeyValue(descriptor, "wireTag").(*ast.BasicLit)
	prepare, prepareOK := canonicalKeyValue(descriptor, "prepare").(*ast.Ident)
	decoder, decoderOK := canonicalKeyValue(descriptor, "decoder").(*ast.Ident)
	_, versionTyped := info.Uses[version].(*types.Const)
	_, prepareTyped := info.Uses[prepare].(*types.Func)
	_, decoderTyped := info.Uses[decoder].(*types.Func)
	wireText, _ := strconv.Unquote(func() string {
		if wireTag == nil {
			return ""
		}
		return wireTag.Value
	}())
	return version, wireTag, versionOK && wireOK && wireText != "" && prepareOK && decoderOK && versionTyped && prepareTyped && decoderTyped
}

func exactTypedFamilyDescriptor(descriptor *ast.CompositeLit, info *types.Info) (*ast.Ident, bool) {
	if descriptor == nil || len(descriptor.Elts) != 2 {
		return nil, false
	}
	sortID, sortOK := canonicalKeyValue(descriptor, "sort").(*ast.Ident)
	tag, tagOK := canonicalKeyValue(descriptor, "tag").(*ast.BasicLit)
	_, sortTyped := info.Uses[sortID].(*types.Const)
	tagText, _ := strconv.Unquote(func() string {
		if tag == nil {
			return ""
		}
		return tag.Value
	}())
	return sortID, sortOK && tagOK && tagText != "" && sortTyped
}

func canonicalBodyHash(fset *token.FileSet, body *ast.BlockStmt) string {
	var buffer bytes.Buffer
	if err := format.Node(&buffer, fset, body); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(buffer.Bytes()))
}

func loadTypedCanonicalProduction(t *testing.T) typedCanonicalPackage {
	t.Helper()
	return typeCheckCanonicalSources(t, canonicalProductionSources(t))
}

func canonicalProductionSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		matched, err := build.Default.MatchFile(".", name)
		if err != nil {
			t.Fatal(err)
		}
		if !matched {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(data)
	}
	return sources
}

func typeCheckCanonicalSources(t *testing.T, sources map[string]string) typedCanonicalPackage {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	for name, source := range sources {
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	cache := map[string]string{}
	lookup := func(path string) (io.ReadCloser, error) {
		export := cache[path]
		if export == "" {
			out, err := exec.Command("go", "list", "-export", "-f", "{{.Export}}", path).Output()
			if err != nil {
				return nil, err
			}
			export = strings.TrimSpace(string(out))
			cache[path] = export
		}
		return os.Open(export)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	config := types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}
	pkg, err := config.Check("github.com/dayvidpham/provenance/internal/journal", fset, files, info)
	if err != nil {
		t.Fatal(err)
	}
	return typedCanonicalPackage{fset, files, info, pkg}
}

func canonicalParents(files []*ast.File) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
	for _, file := range files {
		var stack []ast.Node
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			if len(stack) > 0 {
				parents[node] = stack[len(stack)-1]
			}
			stack = append(stack, node)
			return true
		})
	}
	return parents
}

func canonicalNodeWithin(node ast.Node, ancestor ast.Node, parents map[ast.Node]ast.Node) bool {
	for current := node; current != nil; current = parents[current] {
		if current == ancestor {
			return true
		}
	}
	return false
}

func derefCanonical(value types.Type) types.Type {
	if pointer, ok := value.(*types.Pointer); ok {
		return pointer.Elem()
	}
	return value
}
