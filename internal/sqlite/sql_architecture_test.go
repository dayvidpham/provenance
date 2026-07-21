package sqlite

import (
	"go/ast"
	"go/build"
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
	"unicode"
)

type typedSQLPackage struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
	pkg   *types.Package
}

func TestProductionSQLDomainDispatchIsClosedAndFullyWired(t *testing.T) {
	typed := loadTypedSQLProduction(t)
	findings := inspectTypedSQLArchitecture(typed)
	if len(findings) != 0 {
		t.Fatalf("SQL architecture violations: %v", findings)
	}
}

func TestSQLArchitectureRejectsAliasAndDataflowBypasses(t *testing.T) {
	controls := map[string]string{
		"import alias": `func bad(conn *sqlite.Conn, query string, options *sqlitex.ExecOptions) { sqlexec.Execute(conn, query, options) }`,
		"query return": `func bad(conn *sqlite.Conn, query string, options *sqlitex.ExecOptions) { sqlitex.Execute(conn, queryFor(query), options) }; func queryFor(query string) string { return query }`,
		"multi-hop":    `func bad(conn *sqlite.Conn, query string, options *sqlitex.ExecOptions) { first := query; second := first; sqlitex.Execute(conn, second, options) }`,
		"method value": `func bad(conn *sqlite.Conn, query string, options *sqlitex.ExecOptions) { run := sqlitex.Execute; run(conn, query, options) }`,
		"concat":       `func bad(conn *sqlite.Conn, query string, options *sqlitex.ExecOptions) { sqlitex.Execute(conn, "SELECT "+query, options) }`,
		"join":         `func bad(conn *sqlite.Conn, parts []string, options *sqlitex.ExecOptions) { sqlitex.Execute(conn, strings.Join(parts, ""), options) }`,
		"format":       `func bad(conn *sqlite.Conn, query string, options *sqlitex.ExecOptions) { sqlitex.Execute(conn, fmt.Sprintf("SELECT %s", query), options) }`,
		"plus equals":  `func bad(conn *sqlite.Conn, query string, options *sqlitex.ExecOptions) { built := "SELECT "; built += query; sqlitex.Execute(conn, built, options) }`,
	}
	for name, body := range controls {
		source := `package fixture
			import (
				"fmt"
				"strings"
				sqlite "zombiezen.com/go/sqlite"
				sqlitex "zombiezen.com/go/sqlite/sqlitex"
				sqlexec "zombiezen.com/go/sqlite/sqlitex"
			)
			var _ = fmt.Sprintf
			var _ = strings.Join
			var _ = sqlexec.Execute
			` + body
		typed := typeCheckSQLSources(t, map[string]string{"bypass.go": source})
		if findings := inspectTypedSQLArchitecture(typed); len(findings) == 0 {
			t.Errorf("%s bypass was accepted", name)
		}
	}
}

func TestSQLValueTokenizerRejectsEveryUnboundDMLValueClass(t *testing.T) {
	queries := []string{
		"SELECT * FROM rows WHERE value = 7",
		"SELECT * FROM rows WHERE value = 'quoted'",
		"SELECT * FROM rows WHERE value = X'00ff'",
		"SELECT * FROM rows WHERE value IS NULL",
		"SELECT CURRENT_TIMESTAMP FROM rows",
		"SELECT CURRENT_DATE FROM rows",
		"SELECT * FROM rows WHERE value IN (7)",
		"SELECT * FROM rows WHERE value BETWEEN 1 AND 2",
		"SELECT * FROM rows LIMIT 7 OFFSET 1",
		"SELECT * FROM rows WHERE value + 1 > other",
	}
	for _, query := range queries {
		if rawSQLValueToken(query) == "" {
			t.Errorf("raw value query accepted: %s", query)
		}
	}
	if reason := rawSQLValueToken("SELECT value FROM rows WHERE id>?1 LIMIT ?2"); reason != "" {
		t.Fatalf("bound DML rejected: %s", reason)
	}
}

func TestSQLArchitectureRejectsUnusedMissingAndCastStatements(t *testing.T) {
	source := `package fixture
		import (
			"fmt"
			sqlite "zombiezen.com/go/sqlite"
			"zombiezen.com/go/sqlite/sqlitex"
		)
		type statementClass uint8
		const dml statementClass = 1
		type statement uint8
		const (used statement = iota + 1; unused)
		func (statement) statementClass() statementClass { return dml }
		func (value statement) execute(conn *sqlite.Conn, options *sqlitex.ExecOptions) error {
			switch value {
			case used: return sqlitex.Execute(conn, "SELECT value FROM rows WHERE id=?1", options)
			default: return fmt.Errorf("unknown %d", value)
			}
		}
		func call(conn *sqlite.Conn, options *sqlitex.ExecOptions) error { _ = statement(99); return used.execute(conn, options) }`
	typed := typeCheckSQLSources(t, map[string]string{"incomplete.go": source})
	findings := inspectTypedSQLArchitecture(typed)
	if len(findings) < 3 {
		t.Fatalf("incomplete domain produced %d findings, want missing case, unused constant, and cast: %v", len(findings), findings)
	}
}

func TestBoundHostileSQLValuesRemainOpaque(t *testing.T) {
	values := []any{"quote' -- /* data */", "comment -- /* data */", "nul\x00data"}
	options := appendStaticSQLArgs(nil, values...)
	for i := range values {
		if options.Args[i] != values[i] {
			t.Fatalf("bound value %d changed", i)
		}
	}
}

func TestDomainDispatcherRejectsUnknownEnum(t *testing.T) {
	err := tasksStatement(^uint16(0)).execute(nil, nil)
	if !strings.Contains(err.Error(), errUnknownSQLStatement.Error()) {
		t.Fatalf("unknown statement returned %v", err)
	}
}

func inspectTypedSQLArchitecture(typed typedSQLPackage) []string {
	parents := astParents(typed.files)
	sinks := sqlSinkObjects(typed)
	allowedSinks := map[*ast.CallExpr]bool{}
	statementTypes := map[*types.Named]sqlStatementClass{}
	markerCounts := map[*types.Named]int{}
	dispatcherCounts := map[*types.Named]int{}
	constants := map[*types.Const]bool{}
	cases := map[*types.Const]int{}
	uses := map[*types.Const]int{}
	queries := map[string]*types.Const{}
	var findings []string

	for _, file := range typed.files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil {
				continue
			}
			object, _ := typed.info.Defs[function.Name].(*types.Func)
			if object == nil {
				continue
			}
			signature := object.Type().(*types.Signature)
			receiver, _ := namedType(signature.Recv().Type())
			if receiver == nil {
				continue
			}
			if class, ok := exactSQLClassMethod(function, typed.info); ok {
				statementTypes[receiver] = class
				markerCounts[receiver]++
			}
		}
	}
	for object, class := range statementTypes {
		_ = class
		for _, name := range typed.pkg.Scope().Names() {
			constant, ok := typed.pkg.Scope().Lookup(name).(*types.Const)
			constantType, _ := namedType(func() types.Type {
				if !ok {
					return nil
				}
				return constant.Type()
			}())
			if ok && constantType == object {
				constants[constant] = true
			}
		}
	}

	for _, file := range typed.files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil {
				continue
			}
			object, _ := typed.info.Defs[function.Name].(*types.Func)
			if object == nil {
				continue
			}
			signature := object.Type().(*types.Signature)
			receiver, _ := namedType(signature.Recv().Type())
			class, isStatement := statementTypes[receiver]
			if !isStatement || !isSQLExecuteSignature(signature) {
				continue
			}
			valid, methodCases, methodSinks, methodQueries, reason := exactSQLDispatcher(function, typed.info, sinks, class)
			if !valid {
				findings = append(findings, reason)
				continue
			}
			dispatcherCounts[receiver]++
			for constant := range methodCases {
				cases[constant]++
				if previous := queries[methodQueries[constant]]; previous != nil && previous != constant {
					findings = append(findings, "duplicate sealed SQL query contracts: "+previous.Name()+" and "+constant.Name())
				} else {
					queries[methodQueries[constant]] = constant
				}
			}
			for _, sink := range methodSinks {
				allowedSinks[sink] = true
			}
		}
	}

	for _, file := range typed.files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.Ident:
				constant, ok := typed.info.Uses[value].(*types.Const)
				if ok && constants[constant] {
					if _, inCase := parents[value].(*ast.CaseClause); !inCase {
						uses[constant]++
					}
				}
			case *ast.CallExpr:
				if isTypeConversionToStatement(value, typed.info, statementTypes) {
					findings = append(findings, "unregistered statement enum cast")
				}
				if sinkCallObject(value, typed.info, sinks) != nil && !allowedSinks[value] {
					findings = append(findings, "sqlitex sink outside sealed domain dispatcher")
				}
			case *ast.SelectorExpr:
				if sinks[typed.info.Uses[value.Sel]] && !isDirectAllowedSinkSelector(value, parents, allowedSinks) {
					findings = append(findings, "sqlitex sink method value or alias escaped dispatcher")
				}
			}
			return true
		})
	}
	for constant := range constants {
		if cases[constant] != 1 {
			findings = append(findings, "statement constant does not have exactly one dispatcher case: "+constant.Name())
		}
		if uses[constant] == 0 {
			findings = append(findings, "statement constant has no approved production call site: "+constant.Name())
		}
	}
	for statementType := range statementTypes {
		if markerCounts[statementType] != 1 || dispatcherCounts[statementType] != 1 {
			findings = append(findings, "statement enum lacks exactly one class marker and one dispatcher: "+statementType.Obj().Name())
		}
	}
	return findings
}

func exactSQLClassMethod(function *ast.FuncDecl, info *types.Info) (sqlStatementClass, bool) {
	object, _ := info.Defs[function.Name].(*types.Func)
	if object == nil {
		return 0, false
	}
	signature := object.Type().(*types.Signature)
	if signature.Params().Len() != 0 || signature.Results().Len() != 1 || function.Body == nil || len(function.Body.List) != 1 {
		return 0, false
	}
	returned, ok := function.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 1 {
		return 0, false
	}
	identifier, ok := returned.Results[0].(*ast.Ident)
	constant, ok := info.Uses[identifier].(*types.Const)
	if !ok {
		return 0, false
	}
	value, _ := strconv.Atoi(constant.Val().ExactString())
	if sqlStatementClass(value) != sqlDMLStatement && sqlStatementClass(value) != sqlDDLStatement {
		return 0, false
	}
	return sqlStatementClass(value), true
}

func isSQLExecuteSignature(signature *types.Signature) bool {
	if signature.Params().Len() != 2 || signature.Results().Len() != 1 {
		return false
	}
	conn := signature.Params().At(0).Type().String()
	options := signature.Params().At(1).Type().String()
	return strings.HasSuffix(conn, "zombiezen.com/go/sqlite.Conn") && strings.HasSuffix(options, "zombiezen.com/go/sqlite/sqlitex.ExecOptions") && signature.Results().At(0).Type().String() == "error"
}

func exactSQLDispatcher(function *ast.FuncDecl, info *types.Info, sinks map[types.Object]bool, class sqlStatementClass) (bool, map[*types.Const]bool, []*ast.CallExpr, map[*types.Const]string, string) {
	if function.Body == nil || len(function.Body.List) != 1 {
		return false, nil, nil, nil, "statement dispatcher has extra or missing expressions"
	}
	switchStatement, ok := function.Body.List[0].(*ast.SwitchStmt)
	if !ok {
		return false, nil, nil, nil, "statement dispatcher is not one exhaustive switch"
	}
	tag, ok := switchStatement.Tag.(*ast.Ident)
	if !ok {
		return false, nil, nil, nil, "statement dispatcher switch tag is indirect"
	}
	var receiverName string
	if function.Recv != nil && len(function.Recv.List) == 1 && len(function.Recv.List[0].Names) == 1 {
		receiverName = function.Recv.List[0].Names[0].Name
	}
	if tag.Name != receiverName {
		return false, nil, nil, nil, "statement dispatcher switches altered dataflow"
	}
	methodCases := map[*types.Const]bool{}
	methodQueries := map[*types.Const]string{}
	var methodSinks []*ast.CallExpr
	defaultSeen := false
	for _, node := range switchStatement.Body.List {
		clause := node.(*ast.CaseClause)
		if len(clause.List) == 0 {
			defaultSeen = exactUnknownStatementReturn(clause.Body, info)
			continue
		}
		if len(clause.List) != 1 || len(clause.Body) != 1 {
			return false, nil, nil, nil, "statement dispatcher case is non-atomic"
		}
		id, ok := clause.List[0].(*ast.Ident)
		constant, ok := info.Uses[id].(*types.Const)
		returned, returnOK := clause.Body[0].(*ast.ReturnStmt)
		if !ok || methodCases[constant] || !returnOK || len(returned.Results) != 1 {
			return false, nil, nil, nil, "statement dispatcher case identity/dataflow is invalid"
		}
		call, ok := returned.Results[0].(*ast.CallExpr)
		if !ok || sinkCallObject(call, info, sinks) == nil || len(call.Args) != 3 {
			return false, nil, nil, nil, "statement case does not directly call resolved sqlitex sink"
		}
		literal, ok := call.Args[1].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return false, nil, nil, nil, "statement case SQL is not static"
		}
		query, _ := strconv.Unquote(literal.Value)
		if class == sqlDMLStatement {
			if reason := rawSQLValueToken(query); reason != "" {
				return false, nil, nil, nil, "DML contains unbound value token: " + reason
			}
		} else if sinkCallObject(call, info, sinks).Name() != "ExecuteTransient" {
			return false, nil, nil, nil, "DDL case does not use transient static-literal sink"
		}
		methodCases[constant] = true
		methodQueries[constant] = sinkCallObject(call, info, sinks).Name() + "\x00" + query
		methodSinks = append(methodSinks, call)
	}
	if !defaultSeen {
		return false, nil, nil, nil, "statement dispatcher lacks typed unknown-enum failure"
	}
	return true, methodCases, methodSinks, methodQueries, ""
}

func exactUnknownStatementReturn(body []ast.Stmt, info *types.Info) bool {
	if len(body) != 1 {
		return false
	}
	returned, ok := body[0].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 1 {
		return false
	}
	call, ok := returned.Results[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	fun := calledObject(call.Fun, info)
	return fun != nil && fun.Pkg() != nil && fun.Pkg().Path() == "github.com/dayvidpham/provenance/internal/sqlite" && fun.Name() == "unknownSQLStatementError"
}

func rawSQLValueToken(query string) string {
	upper := strings.ToUpper(strings.TrimSpace(query))
	for _, prefix := range []string{"CREATE ", "ALTER ", "DROP ", "PRAGMA ", "BEGIN ", "COMMIT", "ROLLBACK"} {
		if strings.HasPrefix(upper, prefix) {
			return ""
		}
	}
	for i := 0; i < len(query); {
		c := query[i]
		if isSQLSpace(c) {
			i++
			continue
		}
		if c == '-' && i+1 < len(query) && query[i+1] == '-' {
			i += 2
			for i < len(query) && query[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(query) && query[i+1] == '*' {
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return "unterminated comment"
			}
			i += end + 4
			continue
		}
		if c == '?' || c == ':' || c == '@' || c == '$' {
			i++
			for i < len(query) && (isSQLWord(query[i]) || unicode.IsDigit(rune(query[i]))) {
				i++
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			return "quoted literal"
		}
		if unicode.IsDigit(rune(c)) {
			return "numeric literal"
		}
		if isSQLWord(c) {
			start := i
			i++
			for i < len(query) && (isSQLWord(query[i]) || unicode.IsDigit(rune(query[i]))) {
				i++
			}
			word := strings.ToUpper(query[start:i])
			if word == "NULL" || word == "TRUE" || word == "FALSE" || strings.HasPrefix(word, "CURRENT_") {
				return word
			}
			if word == "X" && i < len(query) && query[i] == '\'' {
				return "blob literal"
			}
			continue
		}
		i++
	}
	return ""
}

func loadTypedSQLProduction(t *testing.T) typedSQLPackage {
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
		if matched {
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			sources[name] = string(data)
		}
	}
	return typeCheckSQLSources(t, sources)
}

func typeCheckSQLSources(t *testing.T, sources map[string]string) typedSQLPackage {
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
			command := exec.Command("go", "list", "-export", "-f", "{{.Export}}", path)
			out, err := command.Output()
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
	pkg, err := config.Check("github.com/dayvidpham/provenance/internal/sqlite", fset, files, info)
	if err != nil {
		t.Fatalf("type check: %v", err)
	}
	return typedSQLPackage{fset, files, info, pkg}
}

func sqlSinkObjects(typed typedSQLPackage) map[types.Object]bool {
	result := map[types.Object]bool{}
	for _, file := range typed.files {
		for _, importSpec := range file.Imports {
			path, _ := strconv.Unquote(importSpec.Path.Value)
			if path != "zombiezen.com/go/sqlite/sqlitex" {
				continue
			}
			name := "sqlitex"
			if importSpec.Name != nil {
				name = importSpec.Name.Name
			}
			object := typed.pkg.Imports()
			_ = name
			for _, pkg := range object {
				if pkg.Path() == path {
					result[pkg.Scope().Lookup("Execute")] = true
					result[pkg.Scope().Lookup("ExecuteTransient")] = true
				}
			}
		}
	}
	return result
}
func sinkCallObject(call *ast.CallExpr, info *types.Info, sinks map[types.Object]bool) *types.Func {
	object := calledObject(call.Fun, info)
	if sinks[object] {
		function, _ := object.(*types.Func)
		return function
	}
	return nil
}
func calledObject(expression ast.Expr, info *types.Info) types.Object {
	switch value := expression.(type) {
	case *ast.Ident:
		return info.Uses[value]
	case *ast.SelectorExpr:
		return info.Uses[value.Sel]
	}
	return nil
}
func namedType(value types.Type) (*types.Named, bool) {
	if value == nil {
		return nil, false
	}
	if pointer, ok := value.(*types.Pointer); ok {
		value = pointer.Elem()
	}
	named, ok := value.(*types.Named)
	return named, ok
}
func isTypeConversionToStatement(call *ast.CallExpr, info *types.Info, statements map[*types.Named]sqlStatementClass) bool {
	typ := info.TypeOf(call.Fun)
	named, ok := namedType(typ)
	return ok && statements[named] != 0
}
func astParents(files []*ast.File) map[ast.Node]ast.Node {
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
func isDirectAllowedSinkSelector(selector *ast.SelectorExpr, parents map[ast.Node]ast.Node, allowed map[*ast.CallExpr]bool) bool {
	call, ok := parents[selector].(*ast.CallExpr)
	return ok && call.Fun == selector && allowed[call]
}
func isSQLSpace(value byte) bool {
	return value == ' ' || value == '\n' || value == '\r' || value == '\t'
}
func isSQLWord(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}
