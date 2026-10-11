package provenance_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/dayvidpham/provenance"
)

func TestBorrowedActivationContextValidation(t *testing.T) {
	db, _ := openFileDB(t)
	if tr, err := provenance.OpenBorrowedSQLiteContext(nil, db); tr != nil || err == nil {
		t.Fatalf("nil context: tracker=%v err=%v", tr, err)
	}
	for _, tier := range []time.Duration{0, -time.Second, time.Microsecond} {
		if tr, err := provenance.OpenBorrowedSQLiteContext(context.Background(), db, provenance.WithBorrowedActivationBusyTimeout(tier)); tr != nil || err == nil {
			t.Fatalf("tier %s: tracker=%v err=%v", tier, tr, err)
		}
	}
	cause := errors.New("caller stopped activation")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	if tr, err := provenance.OpenBorrowedSQLiteContext(ctx, db); tr != nil || !errors.Is(err, cause) {
		t.Fatalf("cancel cause: tracker=%v err=%v", tr, err)
	}
}

func TestBorrowedActivationDoesNotRetainOpenContext(t *testing.T) {
	db, _ := openFileDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr, err := provenance.OpenBorrowedSQLiteContext(ctx, db, provenance.WithModelRegistry(provenance.NewRegistry(nil)), provenance.WithBorrowedActivationBusyTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	cancel()
	if _, err := tr.List(provenance.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestBorrowedActivationWrapperDelegatesOnce(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "dbos_store.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "OpenBorrowedSQLite" {
			continue
		}
		if len(fn.Body.List) != 1 {
			t.Fatal("borrowed wrapper has a second implementation")
		}
		ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			t.Fatal("wrapper does not return one delegation")
		}
		call, ok := ret.Results[0].(*ast.CallExpr)
		if !ok {
			t.Fatal("wrapper is not a call")
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok || name.Name != "OpenBorrowedSQLiteContext" || len(call.Args) != 3 || !call.Ellipsis.IsValid() {
			t.Fatal("wrapper changed context/options delegation")
		}
		ctx, ok := call.Args[0].(*ast.CallExpr)
		if !ok {
			t.Fatal("wrapper does not create Background context")
		}
		selector, ok := ctx.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Background" {
			t.Fatal("wrapper does not use Background")
		}
		return
	}
	t.Fatal("wrapper missing")
}
