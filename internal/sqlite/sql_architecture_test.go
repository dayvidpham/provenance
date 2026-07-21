package sqlite

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionSQLSinksRejectFormattedOrConcatenatedStatements(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") || strings.HasSuffix(entry.Name(), "_adversarial.go") {
			continue
		}
		source, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		files++
		findings, err := inspectSQLSinks(source)
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		for _, finding := range findings {
			t.Errorf("%s: %s", filepath.ToSlash(entry.Name()), finding)
		}
	}
	if files == 0 {
		t.Fatal("SQL architecture guard scanned no production files")
	}
}

func TestProductionSQLSinkGuardNegativeControls(t *testing.T) {
	bad := []byte(`package sqlite
func bad() { query := fmt.Sprintf("SELECT * FROM %s", table); sqlitex.Execute(conn, query, nil); sqlitex.Execute(conn, "SELECT "+column, nil) }
`)
	findings, err := inspectSQLSinks(bad)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("negative control findings=%v, want formatted variable and concatenated sink", findings)
	}
	clean := []byte(`package sqlite
func good() { sqlitex.Execute(conn, "SELECT value FROM rows WHERE id=?1", options) }
`)
	if findings, err := inspectSQLSinks(clean); err != nil || len(findings) != 0 {
		t.Fatalf("clean control findings=%v err=%v", findings, err)
	}
}

func inspectSQLSinks(source []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sql.go", source, 0)
	if err != nil {
		return nil, err
	}
	var findings []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		tainted := map[string]bool{}
		// SQLite has no bind syntax for placeholder arity or DDL identifiers.
		// These narrowly scoped functions are independently constrained: the two
		// query builders append only numbered placeholders/static clauses, and the
		// watermark migration selects from closed static schema constants.
		approvedGrammarOnly := map[string]bool{
			"QueryTaskEvents":                  true,
			"ListTasks":                        true,
			"ensureTasksWatermarkColumnLocked": true,
		}[function.Name.Name]
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					id, ok := lhs.(*ast.Ident)
					if ok && i < len(n.Rhs) && unsafeSQLExpression(n.Rhs[i]) {
						tainted[id.Name] = true
					}
				}
			case *ast.CallExpr:
				selector, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || len(n.Args) < 2 || (selector.Sel.Name != "Execute" && selector.Sel.Name != "ExecuteTransient") {
					break
				}
				query := n.Args[1]
				position := fset.Position(query.Pos())
				if unsafeSQLExpression(query) && !approvedGrammarOnly {
					findings = append(findings, function.Name.Name+":"+position.String()+": formatted or concatenated SQL passed to sqlitex sink")
				} else if id, ok := query.(*ast.Ident); ok && tainted[id.Name] && !approvedGrammarOnly {
					findings = append(findings, function.Name.Name+":"+position.String()+": formatted SQL variable passed to sqlitex sink")
				}
			}
			return true
		})
	}
	return findings, nil
}

func unsafeSQLExpression(expr ast.Expr) bool {
	switch n := expr.(type) {
	case *ast.BinaryExpr:
		return n.Op == token.ADD
	case *ast.CallExpr:
		if selector, ok := n.Fun.(*ast.SelectorExpr); ok {
			return (selector.Sel.Name == "Sprintf" || selector.Sel.Name == "Join")
		}
	}
	return false
}
