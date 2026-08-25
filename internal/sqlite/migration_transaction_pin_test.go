package sqlite

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestMigrationBaselineTransactionIsImmediate pins the migration half of the
// read-to-write promotion fix, the sibling of TestActivationTransactionIsImmediate:
// anchorLegacyBaselines folds operations, which reads before it writes, so a
// deferred BEGIN would need a promotion on the first baseline INSERT and SQLite
// never invokes the busy handler for a promotion. The behavioural proof is the
// file-backed contention test in the root package
// (TestMigrationBaselineWaitsForConcurrentFileWriter); this pin stops a future edit
// from silently reintroducing the defect.
//
// What it does not cover: the pin is syntactic, so a deferred BEGIN reached
// through a named constant, a variable, or a helper call would slip past it. The
// contention test is the backstop for that.
func TestMigrationBaselineTransactionIsImmediate(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "migration.go", nil, 0)
	if err != nil {
		t.Fatalf("parse migration.go: %v", err)
	}
	var anchor *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv != nil && fn.Name.Name == "anchorLegacyBaselines" {
			anchor = fn
			break
		}
	}
	if anchor == nil {
		t.Fatal("migration.go no longer declares anchorLegacyBaselines; update this pin to follow the baseline-anchoring path")
	}
	immediate := false
	ast.Inspect(anchor, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.Ident:
			if typed.Name == "runImmediateTransaction" {
				immediate = true
			}
		case *ast.BasicLit:
			if strings.EqualFold(strings.Trim(typed.Value, `"`), "BEGIN") {
				t.Errorf("anchorLegacyBaselines issues a deferred %s: contention would bypass busy_timeout on the "+
					"read-to-write promotion and roll back the whole baseline batch; use runImmediateTransaction", typed.Value)
			}
		}
		return true
	})
	if !immediate {
		t.Error("anchorLegacyBaselines does not use runImmediateTransaction; the migration transaction must take the write lock at BEGIN")
	}
}
