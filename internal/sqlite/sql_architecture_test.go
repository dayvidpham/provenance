package sqlite

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestProductionSQLSinksAreStaticOrSealed(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		source, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		files++
		findings, err := inspectSQLArchitecture(source)
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		for _, finding := range findings {
			t.Errorf("%s: %s", filepath.ToSlash(entry.Name()), finding)
		}
	}
	if files == 0 {
		t.Fatal("SQL architecture guard scanned no compiled production files")
	}
}

func TestSQLArchitectureGuardNegativeControls(t *testing.T) {
	cases := map[string]string{
		"formatted-direct":      `sqlitex.Execute(conn, fmt.Sprintf("SELECT * FROM %s", table), nil)`,
		"concatenated-direct":   `sqlitex.Execute(conn, "SELECT * FROM "+table, nil)`,
		"unrestricted-variable": `query := "SELECT 1"; sqlitex.Execute(conn, query, nil)`,
		"plus-equals":           `query := "SELECT "; query += value; sqlitex.Execute(conn, query, nil)`,
		"helper-return":         `sqlitex.Execute(conn, queryFor(table), nil)`,
		"append-builder":        `parts := append(parts, value); sqlitex.Execute(conn, strings.Join(parts, ""), nil)`,
		"copy-indirection":      `query := other; copy := query; sqlitex.Execute(conn, copy, nil)`,
		"raw-bindable-value":    `sqlitex.Execute(conn, "SELECT * FROM rows WHERE name='attacker'", nil)`,
		"raw-bindable-number":   `sqlitex.Execute(conn, "SELECT * FROM rows WHERE kind_id=7", nil)`,
		"unsealed-statement":    `executeStatement(conn, sqlStatement{text: value}, nil)`,
	}
	for name, body := range cases {
		source := []byte("package sqlite\nfunc bad() { " + body + " }")
		findings, err := inspectSQLArchitecture(source)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(findings) == 0 {
			t.Errorf("%s bypass was not rejected", name)
		}
	}
	methodBypass := []byte(`package sqlite
	type evil struct{}
	func (evil) statement() sqlStatement { return sqlStatement{text: value} }`)
	if findings, err := inspectSQLArchitecture(methodBypass); err != nil || len(findings) == 0 {
		t.Errorf("method variable bypass findings=%v err=%v", findings, err)
	}
}

func TestSQLArchitectureGuardPositiveControls(t *testing.T) {
	source := []byte(`package sqlite
func direct() { sqlitex.Execute(conn, "SELECT value FROM rows WHERE id=?1", options) }
func ddl() { sqlitex.ExecuteTransient(conn, "CREATE TABLE rows (id INTEGER PRIMARY KEY)", nil) }
func sealed() { executeStatement(conn, selector.statement(), options) }
func executeStatement(conn *Conn, statement sqlStatement, options *Options) { sqlitex.Execute(conn, statement.text, options) }
`)
	if findings, err := inspectSQLArchitecture(source); err != nil || len(findings) != 0 {
		t.Fatalf("positive controls findings=%v err=%v", findings, err)
	}
}

func inspectSQLArchitecture(source []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sql.go", source, 0)
	if err != nil {
		return nil, err
	}
	var findings []string
	staticStringConstants := map[string]bool{}
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != len(value.Values) {
				continue
			}
			for i, expression := range value.Values {
				literal, ok := expression.(*ast.BasicLit)
				if ok && literal.Kind == token.STRING {
					staticStringConstants[value.Names[i].Name] = true
				}
			}
		}
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		staticCollections := map[string]bool{}
		staticRangeVariables := map[string]bool{}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assign, ok := node.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
				return true
			}
			id, ok := assign.Lhs[0].(*ast.Ident)
			composite, compositeOK := assign.Rhs[0].(*ast.CompositeLit)
			if !ok || !compositeOK {
				return true
			}
			safe := len(composite.Elts) > 0
			for _, element := range composite.Elts {
				literal, literalOK := element.(*ast.BasicLit)
				if !literalOK || literal.Kind != token.STRING {
					safe = false
					break
				}
				text, _ := strconv.Unquote(literal.Value)
				upper := strings.ToUpper(strings.TrimSpace(text))
				if sqlHasEmbeddedBindableValue(text) || (!strings.HasPrefix(upper, "CREATE ") && !strings.HasPrefix(upper, "ALTER ") && !strings.HasPrefix(upper, "DROP ") && !strings.HasPrefix(upper, "PRAGMA ") && !strings.HasPrefix(upper, "INSERT ") && !strings.HasPrefix(upper, "UPDATE ") && !strings.HasPrefix(upper, "DELETE ")) {
					safe = false
					break
				}
			}
			staticCollections[id.Name] = safe
			return true
		})
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assign, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || !staticCollections[id.Name] {
					continue
				}
				if len(assign.Rhs) != 1 {
					staticCollections[id.Name] = false
					continue
				}
				if _, original := assign.Rhs[0].(*ast.CompositeLit); !original {
					staticCollections[id.Name] = false
				}
			}
			return true
		})
		ast.Inspect(function.Body, func(node ast.Node) bool {
			rangeStatement, ok := node.(*ast.RangeStmt)
			if !ok {
				return true
			}
			value, valueOK := rangeStatement.Value.(*ast.Ident)
			if !valueOK {
				return true
			}
			if collection, collectionOK := rangeStatement.X.(*ast.Ident); collectionOK && staticCollections[collection.Name] {
				staticRangeVariables[value.Name] = true
			}
			if composite, compositeOK := rangeStatement.X.(*ast.CompositeLit); compositeOK {
				safe := len(composite.Elts) > 0
				for _, element := range composite.Elts {
					literal, literalOK := element.(*ast.BasicLit)
					if !literalOK || literal.Kind != token.STRING {
						safe = false
						break
					}
					text, _ := strconv.Unquote(literal.Value)
					upper := strings.ToUpper(strings.TrimSpace(text))
					if !strings.HasPrefix(upper, "CREATE ") && !strings.HasPrefix(upper, "ALTER ") && !strings.HasPrefix(upper, "DROP ") && !strings.HasPrefix(upper, "PRAGMA ") {
						safe = false
						break
					}
				}
				if safe {
					staticRangeVariables[value.Name] = true
				}
			}
			return true
		})
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if composite, ok := node.(*ast.CompositeLit); ok {
				if id, isIdent := composite.Type.(*ast.Ident); isIdent && id.Name == "sqlStatement" {
					for _, element := range composite.Elts {
						keyValue, ok := element.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, keyOK := keyValue.Key.(*ast.Ident)
						if keyOK && key.Name == "text" {
							literalNode, literal := keyValue.Value.(*ast.BasicLit)
							descriptor, descriptorConst := keyValue.Value.(*ast.Ident)
							if !literal && !(descriptorConst && staticStringConstants[descriptor.Name]) {
								findings = append(findings, position(fset, composite)+": sqlStatement text is not static")
							} else if literal {
								text, _ := strconv.Unquote(literalNode.Value)
								if sqlHasEmbeddedBindableValue(text) {
									findings = append(findings, position(fset, composite)+": sealed DML embeds a bindable value")
								}
							}
						}
					}
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calledName(call.Fun)
			if name == "Execute" || name == "ExecuteTransient" {
				if len(call.Args) < 2 {
					return true
				}
				query := call.Args[1]
				if literal, ok := query.(*ast.BasicLit); ok && literal.Kind == token.STRING {
					text, _ := strconv.Unquote(literal.Value)
					if sqlHasEmbeddedBindableValue(text) {
						findings = append(findings, position(fset, query)+": static DML embeds a bindable value")
					}
					return true
				}
				if id, ok := query.(*ast.Ident); ok && staticRangeVariables[id.Name] {
					return true
				}
				selector, sealed := query.(*ast.SelectorExpr)
				if sealed && function.Name.Name == "executeStatement" || sealed && function.Name.Name == "executeTransientStatement" {
					id, receiverOK := selector.X.(*ast.Ident)
					if receiverOK && id.Name == "statement" && selector.Sel.Name == "text" {
						return true
					}
				}
				findings = append(findings, position(fset, query)+": sqlitex sink requires a direct static literal or sealed sqlStatement")
			}
			if name == "executeStatement" || name == "executeTransientStatement" {
				if len(call.Args) < 2 {
					return true
				}
				if composite, raw := call.Args[1].(*ast.CompositeLit); raw {
					findings = append(findings, position(fset, composite)+": sealed sink rejects direct statement construction")
				}
			}
			return true
		})
	}
	return findings, nil
}

func calledName(expression ast.Expr) string {
	switch called := expression.(type) {
	case *ast.Ident:
		return called.Name
	case *ast.SelectorExpr:
		return called.Sel.Name
	default:
		return ""
	}
}

func position(fset *token.FileSet, node ast.Node) string { return fset.Position(node.Pos()).String() }

var quotedSQLValue = regexp.MustCompile(`'([^']|'')*'`)
var numericSQLValue = regexp.MustCompile(`(?i)(?:=|<>|!=)\s*-?[0-9]+|COALESCE\([^,]+,\s*-?[0-9]+\)|VALUES\s*\([^?)]*[0-9]+`)

func sqlHasEmbeddedBindableValue(statement string) bool {
	upper := strings.ToUpper(strings.TrimSpace(statement))
	if strings.HasPrefix(upper, "CREATE ") || strings.HasPrefix(upper, "ALTER ") || strings.HasPrefix(upper, "DROP ") || strings.HasPrefix(upper, "PRAGMA ") {
		return false
	}
	return quotedSQLValue.MatchString(statement) || numericSQLValue.MatchString(statement)
}
