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

// TestProductionSQLUsesDatabaseSQL prevents a partial driver migration from
// silently restoring zombiezen/sqlitex alongside database/sql. It deliberately
// permits package-local SQL helpers, but all production calls must end at the
// standard ExecContext, QueryContext, or QueryRowContext surface.
func TestProductionSQLUsesDatabaseSQL(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	contextSinks := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imported := range file.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			if strings.HasPrefix(path, "zombiezen.com/go/sqlite") {
				t.Errorf("%s imports retired Zombiezen driver %q", name, path)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "ExecContext", "QueryContext", "QueryRowContext":
				contextSinks++
			case "Execute", "ExecuteTransient", "LastInsertRowID", "Changes":
				t.Errorf("%s retains retired driver-specific SQL method %s", name, selector.Sel.Name)
			}
			return true
		})
	}
	if contextSinks == 0 {
		t.Fatal("production sqlite package contains no database/sql context-aware SQL calls")
	}
}
