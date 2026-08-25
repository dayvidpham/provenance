package provenance

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This guard keeps every SQL statement executed by production code decidable by
// reading the source: the statement text must be a compile-time string at the
// sink, or reach the sink through package-local indirection whose every call
// site supplies a compile-time string. A statement assembled at run time is
// indistinguishable, from the database's point of view, from an injected one,
// and it also defeats the module's replay and canonical-receipt guarantees,
// which assume the exact statement text is fixed by the source.
//
// The guard covers the three trees that own SQL: internal/sqlite (schema,
// journal, and fact reducers), internal/allocation (governed-allocation
// reducers), and internal/fusedtx (the DBOS-fused transaction seam).

type sqlGuardedPackage struct {
	directory  string
	importPath string
}

var sqlGuardedPackages = []sqlGuardedPackage{
	{"internal/sqlite", "github.com/dayvidpham/provenance/internal/sqlite"},
	{"internal/allocation", "github.com/dayvidpham/provenance/internal/allocation"},
	{"internal/fusedtx", "github.com/dayvidpham/provenance/internal/fusedtx"},
}

// sqlSinkMethods are the database/sql statement-executing methods, plus the
// package-local scope wrappers that forward to them. A sink is recognised by
// method name because the wrappers deliberately mirror the standard surface.
var sqlSinkMethods = map[string]struct{}{
	"Exec": {}, "ExecContext": {},
	"Query": {}, "QueryContext": {},
	"QueryRow": {}, "QueryRowContext": {},
	"Prepare": {}, "PrepareContext": {},
}

// retiredSQLDriverModule is the driver this module migrated away from. An
// import of it means a partial driver migration: two drivers on one file, each
// with its own connection state and locking behaviour.
const retiredSQLDriverModule = "zombiezen.com/go/sqlite"

// retiredSQLMethods belong to that driver. They can reappear without the import
// (through a local wrapper), so both the import and the method names are banned.
var retiredSQLMethods = map[string]struct{}{
	"Execute": {}, "ExecuteTransient": {}, "LastInsertRowID": {}, "Changes": {},
}

type sqlFinding struct {
	position string
	reason   string
}

func (finding sqlFinding) String() string { return finding.position + ": " + finding.reason }

// sqlParamObligation names a function parameter that carried SQL text into a
// sink. Every call site of that function must supply a compile-time string.
type sqlParamObligation struct {
	owner types.Object
	index int
}

type sqlProgram struct {
	fset        *token.FileSet
	files       []*ast.File
	pkg         *types.Package
	info        *types.Info
	parents     map[ast.Node]ast.Node
	decls       map[*types.Func]*ast.FuncDecl
	values      map[types.Object][]ast.Expr
	literals    map[types.Object]*ast.FuncLit
	sinks       int
	obligations map[sqlParamObligation][]string
	findings    []sqlFinding
}

func TestProductionSQLTextIsDecidableFromSource(t *testing.T) {
	sinks := 0
	for _, guarded := range sqlGuardedPackages {
		program := loadSQLProgram(t, guarded)
		findings := program.inspect()
		sinks += program.sinks
		for _, finding := range findings {
			t.Errorf("%s", finding)
		}
	}
	// Non-vacuity: an analyzer that recognised no sink would report no violation
	// for any tree, including one full of them.
	if sinks == 0 {
		t.Fatal("SQL architecture guard recognised no statement sink -- where: TestProductionSQLTextIsDecidableFromSource; when: after loading the guarded packages; why: the sink method set or the package list no longer matches production code; impact: the guard passes whatever those trees contain; fix: update sqlSinkMethods or sqlGuardedPackages to match the current SQL surface")
	}
}

func TestSQLGuardRejectsEverySeededViolationClass(t *testing.T) {
	const preamble = `package guarded

import (
	"context"
	"database/sql"
	"fmt"
)

const staticQuery = "SELECT id FROM rows WHERE id=?1"

type kind uint8

const kindRow kind = 1

func (k kind) query() string {
	switch k {
	case kindRow:
		return "SELECT id FROM rows"
	default:
		panic("unreachable")
	}
}

var _ = fmt.Sprintf
var _ = staticQuery
var _ = kindRow
var _ *sql.DB
var _ = context.Background

`
	seeded := map[string]string{
		"runtime parameter":     `func bad(ctx context.Context, db *sql.DB, query string) { _, _ = db.ExecContext(ctx, query) }`,
		"runtime call":          "func bad(ctx context.Context, db *sql.DB) { _, _ = db.ExecContext(ctx, queryFor()) }\nfunc queryFor() string { return \"SELECT id FROM \" + tableName() }\nfunc tableName() string { return \"rows\" }",
		"multi hop":             `func bad(ctx context.Context, db *sql.DB, query string) { first := query; second := first; _, _ = db.ExecContext(ctx, second) }`,
		"concatenated column":   `func bad(ctx context.Context, db *sql.DB, column string) { _, _ = db.ExecContext(ctx, "SELECT "+column+" FROM rows") }`,
		"formatted table":       `func bad(ctx context.Context, db *sql.DB, table string) { _, _ = db.QueryContext(ctx, fmt.Sprintf("SELECT id FROM %s", table)) }`,
		"reassigned local":      `func bad(ctx context.Context, db *sql.DB, table string) { query := staticQuery; query = "SELECT id FROM " + table; _, _ = db.ExecContext(ctx, query) }`,
		"non-context sink":      "func bad(ctx context.Context, db *sql.DB, query string) { _ = ctx; _, _ = db.Exec(query) }",
		"prepared statement":    `func bad(ctx context.Context, db *sql.DB, query string) { _, _ = db.PrepareContext(ctx, query) }`,
		"retired driver method": "type retiredConn struct{}\nfunc (retiredConn) ExecuteTransient(query string) error { return nil }\nfunc bad(ctx context.Context, conn retiredConn) { _ = ctx; _ = conn.ExecuteTransient }",
		"unverified helper": "func bad(ctx context.Context, db *sql.DB, query string) { run(ctx, db, query) }\n" +
			`func run(ctx context.Context, db *sql.DB, query string) { _, _ = db.ExecContext(ctx, query) }`,
		"runtime closure": "func bad(ctx context.Context, db *sql.DB, table string) {\n" +
			"\texec := func(query string) { _, _ = db.ExecContext(ctx, query) }\n" +
			"\texec(\"SELECT id FROM \" + table)\n}",
		"runtime selector return": "func bad(ctx context.Context, db *sql.DB, table string) { _, _ = db.ExecContext(ctx, selector{table}.query()) }\n" +
			"type selector struct{ table string }\n" +
			`func (s selector) query() string { return "SELECT id FROM " + s.table }`,
	}
	for name, body := range seeded {
		t.Run(name, func(t *testing.T) {
			program := parseSQLProgram(t, name, preamble+body+"\n")
			if findings := program.inspect(); len(findings) == 0 {
				t.Errorf("seeded %q violation was accepted; the guard no longer detects that class", name)
			}
		})
	}
}

// TestSQLGuardRejectsRetiredDriverImport covers the import half of the ban. It
// parses rather than type-checks its fixture because the retired driver is not
// a build dependency of this module — which is the very property being kept.
func TestSQLGuardRejectsRetiredDriverImport(t *testing.T) {
	source := "package guarded\n\nimport (\n\tsqlite \"" + retiredSQLDriverModule + "\"\n\t\"" + retiredSQLDriverModule + "/sqlitex\"\n)\n\nvar _ *sqlite.Conn\nvar _ = sqlitex.Execute\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "retired-import.go", source, 0)
	if err != nil {
		t.Fatalf("parse retired-driver fixture: %v", err)
	}
	program := &sqlProgram{fset: fset, files: []*ast.File{file}}
	program.inspectRetiredDriverImports(file)
	if len(program.findings) != 2 {
		t.Fatalf("retired-driver import fixture produced %d findings, want one per import: %+v", len(program.findings), program.findings)
	}
}

func TestSQLGuardAcceptsDecidableStatementShapes(t *testing.T) {
	const accepted = `package guarded

import (
	"context"
	"database/sql"
	"fmt"
)

const staticQuery = "SELECT id FROM rows WHERE id=?1"
const staticPrefix = "SELECT id FROM rows"

type kind uint8

const kindRow kind = 1

func (k kind) query() string {
	switch k {
	case kindRow:
		return "SELECT id FROM rows WHERE id=?1"
	default:
		panic("unreachable")
	}
}

func fine(ctx context.Context, db *sql.DB, id int64, timeoutMS int) error {
	if _, err := db.ExecContext(ctx, staticQuery, id); err != nil {
		return err
	}
	local := staticPrefix + " WHERE id=?1"
	if _, err := db.QueryContext(ctx, local, id); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, kindRow.query(), id); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", timeoutMS)); err != nil {
		return err
	}
	for _, statement := range []string{"CREATE TABLE IF NOT EXISTS rows (id INTEGER)", "CREATE INDEX IF NOT EXISTS idx ON rows (id)"} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return each(ctx, db, staticQuery)
}

func each(ctx context.Context, db *sql.DB, query string) error {
	_, err := db.QueryContext(ctx, query)
	return err
}
`
	program := parseSQLProgram(t, "accepted", accepted)
	findings := program.inspect()
	if program.sinks != 6 {
		t.Fatalf("accepted fixture recognised %d sinks, want 6", program.sinks)
	}
	for _, finding := range findings {
		t.Errorf("decidable statement shape rejected: %s", finding)
	}
}

func loadSQLProgram(t *testing.T, guarded sqlGuardedPackage) *sqlProgram {
	t.Helper()
	entries, err := os.ReadDir(guarded.directory)
	if err != nil {
		t.Fatalf("read %s: %v", guarded.directory, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(guarded.directory, name)
		file, err := parser.ParseFile(fset, filepath.ToSlash(path), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatalf("guarded package %s contains no production Go files", guarded.directory)
	}
	program, typeErrors := checkSQLProgram(fset, guarded.importPath, files)
	if len(typeErrors) != 0 {
		t.Fatalf("type-check %s:\n%s", guarded.importPath, strings.Join(typeErrors, "\n"))
	}
	return program
}

func parseSQLProgram(t *testing.T, name, source string) *sqlProgram {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name+".go", source, 0)
	if err != nil {
		t.Fatalf("parse %s fixture: %v", name, err)
	}
	program, typeErrors := checkSQLProgram(fset, "guarded", []*ast.File{file})
	if len(typeErrors) != 0 {
		t.Fatalf("type-check %s fixture:\n%s", name, strings.Join(typeErrors, "\n"))
	}
	return program
}

func checkSQLProgram(fset *token.FileSet, importPath string, files []*ast.File) (*sqlProgram, []string) {
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	var typeErrors []string
	config := types.Config{
		Importer: moduleImporter(fset),
		Error:    func(err error) { typeErrors = append(typeErrors, err.Error()) },
	}
	pkg, _ := config.Check(importPath, fset, files, info)
	program := &sqlProgram{
		fset:        fset,
		files:       files,
		pkg:         pkg,
		info:        info,
		parents:     map[ast.Node]ast.Node{},
		decls:       map[*types.Func]*ast.FuncDecl{},
		values:      map[types.Object][]ast.Expr{},
		literals:    map[types.Object]*ast.FuncLit{},
		obligations: map[sqlParamObligation][]string{},
	}
	for _, file := range files {
		for node, parent := range astParents(file) {
			program.parents[node] = parent
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				if object, ok := info.Defs[node.Name].(*types.Func); ok {
					program.decls[object] = node
				}
			case *ast.ValueSpec:
				for i, name := range node.Names {
					if object := info.Defs[name]; object != nil && i < len(node.Values) {
						program.bind(object, node.Values[i])
					}
				}
			case *ast.AssignStmt:
				for i, target := range node.Lhs {
					name, ok := target.(*ast.Ident)
					if !ok || i >= len(node.Rhs) {
						continue
					}
					object := info.Defs[name]
					if object == nil {
						object = info.Uses[name]
					}
					if object == nil {
						continue
					}
					if node.Tok == token.ADD_ASSIGN {
						program.bind(object, node.Rhs[i])
						continue
					}
					program.bind(object, node.Rhs[i])
				}
			}
			return true
		})
	}
	return program, typeErrors
}

// bind records every expression assigned to an object. A statement is
// decidable only when every one of its bindings is.
func (program *sqlProgram) bind(object types.Object, value ast.Expr) {
	program.values[object] = append(program.values[object], value)
	if literal, ok := value.(*ast.FuncLit); ok {
		program.literals[object] = literal
	}
}

func moduleImporter(fset *token.FileSet) types.Importer {
	exports := map[string]string{}
	lookup := func(path string) (io.ReadCloser, error) {
		export, ok := exports[path]
		if !ok {
			output, err := exec.Command("go", "list", "-export", "-f={{.Export}}", path).Output()
			if err != nil {
				return nil, fmt.Errorf("resolve export data for %s: %w", path, err)
			}
			export = strings.TrimSpace(string(output))
			if export == "" {
				return nil, fmt.Errorf("resolve export data for %s: go list returned an empty path", path)
			}
			exports[path] = export
		}
		return os.Open(export)
	}
	return importer.ForCompiler(fset, "gc", lookup)
}

func (program *sqlProgram) position(node ast.Node) string {
	at := program.fset.Position(node.Pos())
	return fmt.Sprintf("%s:%d", at.Filename, at.Line)
}

func (program *sqlProgram) report(node ast.Node, format string, args ...any) {
	program.findings = append(program.findings, sqlFinding{program.position(node), fmt.Sprintf(format, args...)})
}

func (program *sqlProgram) inspect() []sqlFinding {
	for _, file := range program.files {
		program.inspectRetiredDriverImports(file)
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if _, retired := retiredSQLMethods[node.Sel.Name]; retired {
					program.report(node, "references retired driver method %s; this module executes SQL through database/sql only", node.Sel.Name)
				}
			case *ast.CallExpr:
				program.inspectCall(node)
			}
			return true
		})
	}
	program.verifyObligations()
	sort.Slice(program.findings, func(i, j int) bool {
		if program.findings[i].position == program.findings[j].position {
			return program.findings[i].reason < program.findings[j].reason
		}
		return program.findings[i].position < program.findings[j].position
	})
	return program.findings
}

// inspectRetiredDriverImports keeps the driver migration total. Running the
// retired driver alongside database/sql would put two independent connection
// pools, with two independent lock views, on one database file.
func (program *sqlProgram) inspectRetiredDriverImports(file *ast.File) {
	for _, imported := range file.Imports {
		path := strings.Trim(imported.Path.Value, `"`)
		if path == retiredSQLDriverModule || strings.HasPrefix(path, retiredSQLDriverModule+"/") {
			program.report(imported, "imports the retired driver %q; this module reaches SQLite through database/sql only, and a second driver on the same file would hold its own connections and locks", path)
		}
	}
}

func (program *sqlProgram) inspectCall(call *ast.CallExpr) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	if _, sink := sqlSinkMethods[selector.Sel.Name]; !sink {
		return
	}
	index, argument := program.stringArgument(call)
	if argument == nil {
		return
	}
	program.sinks++
	_ = index
	if reason := program.decide(argument, 0); reason != "" {
		program.report(call, "%s receives SQL text that is not decidable from source: %s", selector.Sel.Name, reason)
	}
}

// stringArgument returns the first string-typed argument, which is the
// statement text for every sink surface: the context-aware methods take the
// context first, and no sink takes another string before the statement.
func (program *sqlProgram) stringArgument(call *ast.CallExpr) (int, ast.Expr) {
	for i, argument := range call.Args {
		argumentType := program.info.TypeOf(argument)
		if argumentType == nil {
			continue
		}
		basic, ok := argumentType.Underlying().(*types.Basic)
		if ok && basic.Kind() == types.String {
			return i, argument
		}
	}
	return -1, nil
}

// decide returns "" when the expression's string value is fixed by the source,
// or an actionable reason otherwise.
func (program *sqlProgram) decide(expression ast.Expr, depth int) string {
	if depth > 12 {
		return "the statement passes through more indirection than the guard follows; bind it to a package-level constant"
	}
	if value := program.info.Types[expression]; value.Value != nil && value.Value.Kind() == constant.String {
		return ""
	}
	switch expression := expression.(type) {
	case *ast.BasicLit:
		if expression.Kind == token.STRING {
			return ""
		}
		return "the argument is not a string literal"
	case *ast.ParenExpr:
		return program.decide(expression.X, depth+1)
	case *ast.BinaryExpr:
		if expression.Op != token.ADD {
			return "the statement is built with an operator other than concatenation"
		}
		if reason := program.decide(expression.X, depth+1); reason != "" {
			return reason
		}
		return program.decide(expression.Y, depth+1)
	case *ast.Ident:
		return program.decideIdent(expression, depth)
	case *ast.CallExpr:
		return program.decideCall(expression, depth)
	case *ast.SelectorExpr:
		return "the statement is read from a struct field; hold it in a package-level constant or a closed-enum selector method instead"
	default:
		return "the statement is not fixed by the source"
	}
}

func (program *sqlProgram) decideIdent(identifier *ast.Ident, depth int) string {
	object := program.info.Uses[identifier]
	if object == nil {
		object = program.info.Defs[identifier]
	}
	if object == nil {
		return fmt.Sprintf("%q does not resolve to a package symbol", identifier.Name)
	}
	if shared, ok := object.(*types.Const); ok && shared.Val().Kind() == constant.String {
		return ""
	}
	if owner, index, ok := program.parameterOf(identifier, object); ok {
		obligation := sqlParamObligation{owner, index}
		program.obligations[obligation] = append(program.obligations[obligation], program.position(identifier))
		return ""
	}
	if reason := program.decideRangeValue(identifier, object, depth); reason != "notARangeValue" {
		return reason
	}
	if bindings, ok := program.values[object]; ok && len(bindings) > 0 {
		for _, binding := range bindings {
			if reason := program.decide(binding, depth+1); reason != "" {
				return fmt.Sprintf("%q is bound to a statement that is not decidable: %s", identifier.Name, reason)
			}
		}
		return ""
	}
	return fmt.Sprintf("%q is a run-time value rather than a constant, a static binding, or a checked parameter", identifier.Name)
}

// decideRangeValue accepts a range over a slice literal of static statements,
// the shape used for schema DDL batches. It returns "notARangeValue" when the
// identifier is not a range value, so the caller can keep resolving.
func (program *sqlProgram) decideRangeValue(identifier *ast.Ident, object types.Object, depth int) string {
	for node := ast.Node(identifier); node != nil; node = program.parents[node] {
		rangeStmt, ok := node.(*ast.RangeStmt)
		if !ok {
			continue
		}
		value, _ := rangeStmt.Value.(*ast.Ident)
		if value == nil || program.info.Defs[value] != object {
			continue
		}
		return program.decideBatch(rangeStmt.X, depth+1)
	}
	return "notARangeValue"
}

func (program *sqlProgram) decideBatch(expression ast.Expr, depth int) string {
	if identifier, ok := expression.(*ast.Ident); ok {
		object := program.info.Uses[identifier]
		if object == nil {
			object = program.info.Defs[identifier]
		}
		bindings, ok := program.values[object]
		if !ok || len(bindings) == 0 {
			return fmt.Sprintf("the batch %q has no static definition", identifier.Name)
		}
		for _, binding := range bindings {
			if reason := program.decideBatch(binding, depth+1); reason != "" {
				return reason
			}
		}
		return ""
	}
	literal, ok := expression.(*ast.CompositeLit)
	if !ok || len(literal.Elts) == 0 {
		return "the ranged statement batch is empty or is not a literal"
	}
	for _, element := range literal.Elts {
		if reason := program.decide(element, depth+1); reason != "" {
			return reason
		}
	}
	return ""
}

func (program *sqlProgram) decideCall(call *ast.CallExpr, depth int) string {
	if format, ok := program.formatCall(call); ok {
		return program.decideFormat(call, format)
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) != 0 {
		return "the statement is produced by a call that takes run-time data; write it literally or return it from a closed-enum selector method"
	}
	selection := program.info.Selections[selector]
	if selection == nil {
		return "the statement is produced by an unresolved call"
	}
	method, _ := selection.Obj().(*types.Func)
	declaration := program.decls[method]
	if declaration == nil || declaration.Body == nil {
		return "the statement is produced by a method declared outside the guarded package"
	}
	return program.decideSelectorReturns(declaration)
}

// decideSelectorReturns accepts a zero-argument selector method whose every
// return is a compile-time string: the typed-registry shape, where a closed
// enum maps to one exact statement.
func (program *sqlProgram) decideSelectorReturns(declaration *ast.FuncDecl) string {
	returns := 0
	reason := ""
	ast.Inspect(declaration.Body, func(node ast.Node) bool {
		statement, ok := node.(*ast.ReturnStmt)
		if !ok || len(statement.Results) != 1 {
			return true
		}
		returns++
		if value := program.info.Types[statement.Results[0]]; value.Value != nil && value.Value.Kind() == constant.String {
			return true
		}
		if reason == "" {
			reason = fmt.Sprintf("selector %s returns a statement that is not a compile-time string; every branch must return one exact literal or constant", declaration.Name.Name)
		}
		return true
	})
	if returns == 0 {
		return fmt.Sprintf("selector %s returns no statement literal", declaration.Name.Name)
	}
	return reason
}

func (program *sqlProgram) formatCall(call *ast.CallExpr) (*ast.BasicLit, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	base, ok := selector.X.(*ast.Ident)
	if !ok || base.Name != "fmt" || selector.Sel.Name != "Sprintf" || len(call.Args) == 0 {
		return nil, false
	}
	literal, ok := call.Args[0].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return nil, false
	}
	return literal, true
}

// decideFormat accepts a formatted statement only when every formatted value
// is itself fixed by the source: an integer (a number cannot carry statement
// text, as in PRAGMA busy_timeout=%d), or a string that the guard can decide
// through the same rules — a constant, or a parameter whose every call site
// supplies one. Anything else can carry statement grammar and is rejected.
func (program *sqlProgram) decideFormat(call *ast.CallExpr, format *ast.BasicLit) string {
	if _, err := strconv.Unquote(format.Value); err != nil {
		return "the format string is not a compile-time literal"
	}
	for _, argument := range call.Args[1:] {
		argumentType := program.info.TypeOf(argument)
		if argumentType != nil {
			if basic, ok := argumentType.Underlying().(*types.Basic); ok && basic.Info()&types.IsInteger != 0 {
				continue
			}
		}
		if reason := program.decide(argument, 1); reason != "" {
			return fmt.Sprintf("the statement is formatted from a value that is not decidable from source (%s); bind the value with a ?N parameter instead", reason)
		}
	}
	return ""
}

// parameterOf reports the function or closure that owns identifier as a
// parameter, so the guard can follow the statement to that owner's call sites.
func (program *sqlProgram) parameterOf(identifier *ast.Ident, object types.Object) (types.Object, int, bool) {
	variable, ok := object.(*types.Var)
	if !ok {
		return nil, 0, false
	}
	for node := ast.Node(identifier); node != nil; node = program.parents[node] {
		switch owner := node.(type) {
		case *ast.FuncLit:
			signature, _ := program.info.TypeOf(owner).(*types.Signature)
			if index, found := parameterIndex(signature, variable); found {
				if bound := program.closureVariable(owner); bound != nil {
					return bound, index, true
				}
				return nil, 0, false
			}
		case *ast.FuncDecl:
			function, _ := program.info.Defs[owner.Name].(*types.Func)
			if function == nil {
				return nil, 0, false
			}
			signature, _ := function.Type().(*types.Signature)
			if index, found := parameterIndex(signature, variable); found {
				return function, index, true
			}
			return nil, 0, false
		}
	}
	return nil, 0, false
}

func parameterIndex(signature *types.Signature, variable *types.Var) (int, bool) {
	if signature == nil {
		return 0, false
	}
	for i := 0; i < signature.Params().Len(); i++ {
		if signature.Params().At(i) == variable {
			return i, true
		}
	}
	return 0, false
}

// closureVariable returns the variable a function literal is bound to, which is
// the name its call sites use.
func (program *sqlProgram) closureVariable(literal *ast.FuncLit) types.Object {
	for object, bound := range program.literals {
		if bound == literal {
			return object
		}
	}
	return nil
}

// verifyObligations discharges each parameter that carried SQL into a sink: the
// function must be package-local, and every call site must supply a statement
// that is itself decidable from source.
func (program *sqlProgram) verifyObligations() {
	for len(program.obligations) > 0 {
		pending := program.obligations
		program.obligations = map[sqlParamObligation][]string{}
		for obligation, sinkPositions := range pending {
			program.verifyObligation(obligation, sinkPositions)
		}
	}
}

func (program *sqlProgram) verifyObligation(obligation sqlParamObligation, sinkPositions []string) {
	name := obligation.owner.Name()
	var declaration ast.Node
	seam := false
	if function, ok := obligation.owner.(*types.Func); ok {
		if program.decls[function] == nil {
			return
		}
		declaration = program.decls[function]
		// An exported method or function is a declared transaction seam: it
		// forwards a statement supplied by another guarded package, or by a
		// participant callback that owns its own SQL by contract. Its call
		// sites inside the guarded trees are still checked below; a caller
		// outside them is outside this guard's reach by design.
		seam = function.Exported()
	}
	callSites := 0
	for _, file := range program.files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			var used *ast.Ident
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				used = fun
			case *ast.SelectorExpr:
				used = fun.Sel
			default:
				return true
			}
			if program.info.Uses[used] != obligation.owner || obligation.index >= len(call.Args) {
				return true
			}
			callSites++
			if reason := program.decide(call.Args[obligation.index], 0); reason != "" {
				program.report(call, "passes SQL text to %s, which executes it at %s, but the text is not decidable from source: %s", name, strings.Join(sinkPositions, ", "), reason)
			}
			return true
		})
	}
	if callSites == 0 && !seam && declaration != nil {
		program.report(declaration, "%s executes SQL supplied through parameter %d but has no call site in the package; the executed statement text cannot be decided from source", name, obligation.index)
	}
}

func astParents(file *ast.File) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
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
	return parents
}
