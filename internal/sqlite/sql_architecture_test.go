package sqlite

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
	"strconv"
	"strings"
	"testing"
	"unicode"
)

const sqlitePackagePath = "github.com/dayvidpham/provenance/internal/sqlite"

type sqlProgram struct {
	fset    *token.FileSet
	files   []*ast.File
	pkg     *types.Package
	info    *types.Info
	parents map[ast.Node]ast.Node
	methods map[*types.Func]*ast.FuncDecl
	values  map[types.Object]ast.Expr
}

type sqlUse struct {
	sink    string
	options string
}

type resolvedSQL struct {
	queries  []string
	shared   []*types.Const
	selector *types.Func
	batch    bool
}

func TestProductionSQLUsesDirectStaticSinks(t *testing.T) {
	program := loadProductionSQLProgram(t)
	if findings := inspectSQLArchitecture(program); len(findings) != 0 {
		t.Fatalf("SQL architecture violations:\n%s", strings.Join(findings, "\n"))
	}
}

func TestApplyDelegatesExactlyOnceWithoutLoop(t *testing.T) {
	program := loadProductionSQLProgram(t)
	applyMethods := 0
	preparedCalls := 0
	loops := 0
	for _, file := range program.files {
		for _, declaration := range file.Decls {
			method, ok := declaration.(*ast.FuncDecl)
			if !ok || method.Name.Name != "Apply" || method.Recv == nil {
				continue
			}
			receiver, ok := method.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			name, okName := receiver.X.(*ast.Ident)
			if !okName || name.Name != "DB" {
				continue
			}
			applyMethods++
			ast.Inspect(method.Body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.ForStmt, *ast.RangeStmt:
					loops++
				case *ast.CallExpr:
					selector, ok := node.Fun.(*ast.SelectorExpr)
					if ok && selector.Sel.Name == "foldPreparedOperation" {
						preparedCalls++
					}
				}
				return true
			})
		}
	}
	if applyMethods != 1 || preparedCalls != 1 || loops != 0 {
		t.Fatalf("DB.Apply source contract: methods=%d foldPreparedOperation calls=%d loops=%d, want 1/1/0", applyMethods, preparedCalls, loops)
	}
}

func TestSQLArchitectureRejectsRuntimeAndIndirectSQL(t *testing.T) {
	controls := map[string]string{
		"runtime parameter": `func bad(conn *sqlite.Conn, query string) { sqlitex.Execute(conn, query, nil) }`,
		"query return":      `func bad(conn *sqlite.Conn) { sqlitex.Execute(conn, queryFor(), nil) }; func queryFor() string { return "SELECT value FROM rows" }`,
		"multi hop":         `func bad(conn *sqlite.Conn, query string) { first := query; second := first; sqlitex.Execute(conn, second, nil) }`,
		"method value":      `func bad(conn *sqlite.Conn) { run := sqlitex.Execute; run(conn, "SELECT value FROM rows", nil) }`,
		"concat":            `func bad(conn *sqlite.Conn, query string) { sqlitex.Execute(conn, "SELECT "+query, nil) }`,
		"format":            `func bad(conn *sqlite.Conn, query string) { sqlitex.Execute(conn, fmt.Sprintf("SELECT %s", query), nil) }`,
		"generic wrapper":   `type sealedSQLStatement interface { execute(*sqlite.Conn, *sqlitex.ExecOptions) error }; func executeStatement(conn *sqlite.Conn, query string) error { return sqlitex.Execute(conn, query, nil) }`,
		"statement class":   `type sqlStatementClass uint8; const sqlDMLStatement sqlStatementClass = 1`,
		"raw DML batch":     `func bad(conn *sqlite.Conn) { batch := []string{"DELETE FROM rows WHERE value='raw'"}; for _, query := range batch { _ = sqlitex.Execute(conn, query, nil) } }`,
		"mixed class batch": `func bad(conn *sqlite.Conn) { batch := []string{"CREATE TABLE rows (id INTEGER)", "SELECT id FROM rows"}; for _, query := range batch { _ = sqlitex.ExecuteTransient(conn, query, nil) } }`,
	}
	for name, body := range controls {
		source := `package sqlite
			import (
				"fmt"
				sqlite "zombiezen.com/go/sqlite"
				"zombiezen.com/go/sqlite/sqlitex"
			)
			var _ = fmt.Sprintf
			var _ *sqlite.Conn
			var _ = sqlitex.Execute
		` + body
		program, errors := parseAndCheckSQLProgram(name+".go", source)
		if len(errors) != 0 {
			t.Fatalf("type-check %s control: %s", name, strings.Join(errors, "; "))
		}
		if findings := inspectSQLArchitecture(program); len(findings) == 0 {
			t.Errorf("%s control was accepted", name)
		}
	}
}

func TestRawDMLValueDetectionIsNonVacuous(t *testing.T) {
	rejected := []string{
		"SELECT * FROM rows WHERE value = 7",
		"SELECT * FROM rows WHERE value = 'quoted'",
		"SELECT * FROM rows WHERE value = X'00ff'",
		"SELECT * FROM rows WHERE value IS NULL",
		"SELECT CURRENT_TIMESTAMP FROM rows",
		"SELECT * FROM rows LIMIT 7 OFFSET 1",
	}
	for _, query := range rejected {
		if rawSQLValueToken(query) == "" {
			t.Errorf("raw DML value accepted: %s", query)
		}
	}
	if reason := rawSQLValueToken("SELECT value FROM rows WHERE id>?1 LIMIT ?2"); reason != "" {
		t.Fatalf("bound DML rejected: %s", reason)
	}
}

func loadProductionSQLProgram(t *testing.T) *sqlProgram {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatal("no production sqlite files inspected")
	}
	program, typeErrors := checkSQLProgram(fset, files)
	if len(typeErrors) != 0 {
		t.Fatalf("type-check production sqlite package:\n%s", strings.Join(typeErrors, "\n"))
	}
	return program
}

func parseAndCheckSQLProgram(name, source string) (*sqlProgram, []string) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, source, 0)
	if err != nil {
		return nil, []string{err.Error()}
	}
	return checkSQLProgram(fset, []*ast.File{file})
}

func checkSQLProgram(fset *token.FileSet, files []*ast.File) (*sqlProgram, []string) {
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	var typeErrors []string
	config := types.Config{
		Importer: moduleImporter(fset),
		Error: func(err error) {
			typeErrors = append(typeErrors, err.Error())
		},
	}
	pkg, _ := config.Check(sqlitePackagePath, fset, files, info)
	program := &sqlProgram{
		fset:    fset,
		files:   files,
		pkg:     pkg,
		info:    info,
		parents: map[ast.Node]ast.Node{},
		methods: map[*types.Func]*ast.FuncDecl{},
		values:  map[types.Object]ast.Expr{},
	}
	for _, file := range files {
		for node, parent := range astParents(file) {
			program.parents[node] = parent
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				if object, ok := info.Defs[node.Name].(*types.Func); ok && node.Recv != nil {
					program.methods[object] = node
				}
			case *ast.ValueSpec:
				for i, name := range node.Names {
					if object := info.Defs[name]; object != nil && i < len(node.Values) {
						program.values[object] = node.Values[i]
					}
				}
			case *ast.AssignStmt:
				if node.Tok == token.DEFINE {
					for i, lhs := range node.Lhs {
						name, ok := lhs.(*ast.Ident)
						if ok && i < len(node.Rhs) && info.Defs[name] != nil {
							program.values[info.Defs[name]] = node.Rhs[i]
						}
					}
				}
			}
			return true
		})
	}
	return program, typeErrors
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

func inspectSQLArchitecture(program *sqlProgram) []string {
	var findings []string
	sharedUses := map[*types.Const][]sqlUse{}
	validatedSelectors := map[*types.Func]bool{}

	for _, file := range program.files {
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.GenDecl:
				if declaration.Tok != token.TYPE {
					continue
				}
				for _, spec := range declaration.Specs {
					name := spec.(*ast.TypeSpec).Name.Name
					lower := strings.ToLower(name)
					if strings.Contains(lower, "sqlstatement") || strings.Contains(lower, "statementclass") {
						findings = append(findings, "forbidden generic SQL type "+name)
					}
				}
			case *ast.FuncDecl:
				if declaration.Name.Name == "executeStatement" || declaration.Name.Name == "appendStaticSQLArgs" {
					findings = append(findings, "forbidden generic SQL helper "+declaration.Name.Name)
				}
			}
		}

		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				object, _ := program.info.Uses[node.Sel].(*types.Func)
				if !isSQLSink(object) {
					return true
				}
				call, direct := program.parents[node].(*ast.CallExpr)
				if !direct || call.Fun != node {
					findings = append(findings, "sqlitex sink escaped as alias or method value")
				}
			case *ast.CallExpr:
				selector, ok := node.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				sink, _ := program.info.Uses[selector.Sel].(*types.Func)
				if !isSQLSink(sink) {
					return true
				}
				if len(node.Args) != 3 {
					findings = append(findings, "SQL sink has unexpected arity")
					return true
				}
				resolved, reason := program.resolveSQL(node.Args[1])
				if reason != "" {
					findings = append(findings, fmt.Sprintf("SQL argument is not fully static (%s): %s", reason, expressionDescription(node.Args[1])))
					return true
				}
				if resolved.selector != nil && !validatedSelectors[resolved.selector] {
					validatedSelectors[resolved.selector] = true
					findings = append(findings, program.validateSelector(resolved.selector)...)
				}
				kind := sink.Name()
				for _, query := range resolved.queries {
					class := classifySQL(query)
					if class == "" {
						findings = append(findings, "unknown or empty SQL statement at "+kind+" sink")
						continue
					}
					if resolved.batch && !batchClassAllowed(kind, class) {
						findings = append(findings, fmt.Sprintf("%s batch contains %s statement", kind, class))
					}
					if kind == "Execute" && class != "dml" && class != "row-pragma" {
						findings = append(findings, "Execute received non-DML statement class "+class)
					}
					if kind == "ExecuteTransient" && class == "dml" {
						findings = append(findings, "ExecuteTransient received DML statement")
					}
					if kind == "Execute" && class == "dml" {
						if raw := rawSQLValueToken(query); raw != "" {
							findings = append(findings, "unbound DML value: "+raw)
						}
					}
				}
				shape := execOptionsShape(node.Args[2])
				for _, shared := range resolved.shared {
					sharedUses[shared] = append(sharedUses[shared], sqlUse{sink: kind, options: shape})
				}
			}
			return true
		})
	}

	if program.pkg != nil {
		for _, name := range program.pkg.Scope().Names() {
			shared, ok := program.pkg.Scope().Lookup(name).(*types.Const)
			if !ok || shared.Val().Kind() != constant.String || classifySQL(constant.StringVal(shared.Val())) == "" {
				continue
			}
			uses := sharedUses[shared]
			if len(uses) < 3 {
				findings = append(findings, fmt.Sprintf("SQL constant %s has %d direct sink uses; want at least 3", name, len(uses)))
				continue
			}
			for _, use := range uses[1:] {
				if use != uses[0] {
					findings = append(findings, fmt.Sprintf("SQL constant %s has inconsistent sink/options contracts", name))
					break
				}
			}
		}
	}
	return findings
}

func isSQLSink(object *types.Func) bool {
	return object != nil && object.Pkg() != nil && object.Pkg().Path() == "zombiezen.com/go/sqlite/sqlitex" && (object.Name() == "Execute" || object.Name() == "ExecuteTransient")
}

func (program *sqlProgram) resolveSQL(expression ast.Expr) (resolvedSQL, string) {
	switch expression := expression.(type) {
	case *ast.BasicLit:
		if expression.Kind != token.STRING {
			return resolvedSQL{}, "non-string literal"
		}
		query, err := strconv.Unquote(expression.Value)
		if err != nil {
			return resolvedSQL{}, "invalid string literal"
		}
		return resolvedSQL{queries: []string{query}}, ""
	case *ast.Ident:
		object := program.info.Uses[expression]
		if object == nil {
			object = program.info.Defs[expression]
		}
		if shared, ok := object.(*types.Const); ok && shared.Val().Kind() == constant.String {
			result := resolvedSQL{queries: []string{constant.StringVal(shared.Val())}}
			if shared.Parent() == program.pkg.Scope() {
				result.shared = []*types.Const{shared}
			}
			return result, ""
		}
		return program.resolveBatchRangeValue(expression)
	case *ast.CallExpr:
		selector, ok := expression.Fun.(*ast.SelectorExpr)
		if !ok || len(expression.Args) != 0 {
			return resolvedSQL{}, "runtime query call"
		}
		selection := program.info.Selections[selector]
		if selection == nil {
			return resolvedSQL{}, "unresolved query method"
		}
		method, _ := selection.Obj().(*types.Func)
		decl := program.methods[method]
		if decl == nil {
			return resolvedSQL{}, "query method is outside package"
		}
		queries, reason := selectorLiteralReturns(decl)
		if reason != "" {
			return resolvedSQL{}, reason
		}
		return resolvedSQL{queries: queries, selector: method}, ""
	default:
		return resolvedSQL{}, "runtime query dataflow"
	}
}

func (program *sqlProgram) resolveBatchRangeValue(identifier *ast.Ident) (resolvedSQL, string) {
	for node := ast.Node(identifier); node != nil; node = program.parents[node] {
		rangeStmt, ok := node.(*ast.RangeStmt)
		if !ok {
			continue
		}
		value, _ := rangeStmt.Value.(*ast.Ident)
		if value == nil || program.info.Defs[value] != program.info.Uses[identifier] {
			return resolvedSQL{}, "identifier is not this range value"
		}
		return program.resolveBatch(rangeStmt.X)
	}
	return resolvedSQL{}, "identifier is not a static batch range value"
}

func (program *sqlProgram) resolveBatch(expression ast.Expr) (resolvedSQL, string) {
	if identifier, ok := expression.(*ast.Ident); ok {
		object := program.info.Uses[identifier]
		if object == nil {
			object = program.info.Defs[identifier]
		}
		value := program.values[object]
		if value == nil {
			return resolvedSQL{}, "unknown batch source"
		}
		return program.resolveBatch(value)
	}
	literal, ok := expression.(*ast.CompositeLit)
	if !ok || len(literal.Elts) == 0 {
		return resolvedSQL{}, "empty or non-literal batch"
	}
	result := resolvedSQL{batch: true}
	for _, element := range literal.Elts {
		resolved, reason := program.resolveSQL(element)
		if reason != "" || resolved.batch || resolved.selector != nil || len(resolved.queries) != 1 {
			if reason == "" {
				reason = "nested or selector batch element"
			}
			return resolvedSQL{}, reason
		}
		result.queries = append(result.queries, resolved.queries[0])
		result.shared = append(result.shared, resolved.shared...)
	}
	return result, ""
}

func (program *sqlProgram) validateSelector(method *types.Func) []string {
	decl := program.methods[method]
	name := method.FullName()
	if decl == nil || decl.Recv == nil || len(decl.Recv.List) != 1 || len(decl.Recv.List[0].Names) != 1 {
		return []string{name + ": selector has no concrete receiver declaration"}
	}
	signature, _ := method.Type().(*types.Signature)
	if signature == nil || signature.Params().Len() != 0 || signature.Results().Len() != 1 || !types.Identical(signature.Results().At(0).Type(), types.Typ[types.String]) {
		return []string{name + ": selector must take no runtime data and return only string"}
	}
	receiver := signature.Recv().Type()
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = pointer.Elem()
	}
	named, ok := receiver.(*types.Named)
	if !ok || named.Obj().Pkg() != program.pkg {
		return []string{name + ": receiver is not a closed package domain type"}
	}
	basic, ok := named.Underlying().(*types.Basic)
	if !ok || basic.Info()&types.IsInteger == 0 {
		return []string{name + ": receiver is not an integer-backed closed enum"}
	}
	constants := map[*types.Const]int{}
	for _, candidate := range program.pkg.Scope().Names() {
		object, ok := program.pkg.Scope().Lookup(candidate).(*types.Const)
		if ok && types.Identical(object.Type(), named) {
			constants[object] = 0
		}
	}
	if len(constants) == 0 {
		return []string{name + ": receiver has no declared constants"}
	}
	if decl.Body == nil || len(decl.Body.List) != 1 {
		return []string{name + ": selector body must contain only its exhaustive switch"}
	}
	switchStmt, ok := decl.Body.List[0].(*ast.SwitchStmt)
	if !ok || switchStmt.Init != nil {
		return []string{name + ": selector must use one direct switch"}
	}
	receiverName := decl.Recv.List[0].Names[0]
	tag, ok := switchStmt.Tag.(*ast.Ident)
	if !ok || program.info.Uses[tag] != program.info.Defs[receiverName] {
		return []string{name + ": selector switch must inspect its receiver directly"}
	}
	var findings []string
	defaultSeen := false
	for _, statement := range switchStmt.Body.List {
		clause := statement.(*ast.CaseClause)
		if clause.List == nil {
			if defaultSeen || !explicitPanic(clause.Body) {
				findings = append(findings, name+": default must explicitly panic")
			}
			defaultSeen = true
			continue
		}
		if len(clause.Body) != 1 {
			findings = append(findings, name+": successful case must contain one return")
			continue
		}
		ret, ok := clause.Body[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			findings = append(findings, name+": successful case must return one SQL literal")
			continue
		}
		literal, ok := ret.Results[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			findings = append(findings, name+": successful return is not a compile-time complete SQL literal")
		} else if query, err := strconv.Unquote(literal.Value); err != nil || classifySQL(query) == "" {
			findings = append(findings, name+": successful return is empty or not a complete SQL statement")
		}
		for _, expression := range clause.List {
			identifier, ok := expression.(*ast.Ident)
			constantObject, constantOK := program.info.Uses[identifier].(*types.Const)
			if !ok || !constantOK || !types.Identical(constantObject.Type(), named) {
				findings = append(findings, name+": switch case is not an exact receiver constant")
				continue
			}
			if _, declared := constants[constantObject]; !declared {
				findings = append(findings, name+": switch case uses an undeclared receiver value")
				continue
			}
			constants[constantObject]++
		}
	}
	if !defaultSeen {
		findings = append(findings, name+": selector has no explicit fail-closed default")
	}
	for object, count := range constants {
		if count != 1 {
			findings = append(findings, fmt.Sprintf("%s: receiver constant %s appears %d times; want exactly once", name, object.Name(), count))
		}
	}
	return findings
}

func selectorLiteralReturns(decl *ast.FuncDecl) ([]string, string) {
	var queries []string
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return true
		}
		literal, ok := ret.Results[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		query, err := strconv.Unquote(literal.Value)
		if err == nil {
			queries = append(queries, query)
		}
		return true
	})
	if len(queries) == 0 {
		return nil, "selector has no literal SQL returns"
	}
	return queries, ""
}

func explicitPanic(body []ast.Stmt) bool {
	if len(body) != 1 {
		return false
	}
	expression, ok := body[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expression.X.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	identifier, ok := call.Fun.(*ast.Ident)
	return ok && identifier.Name == "panic"
}

func classifySQL(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return ""
	}
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return ""
	}
	first := strings.ToUpper(strings.Trim(fields[0], "();"))
	switch first {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "WITH", "EXPLAIN":
		return "dml"
	case "CREATE", "ALTER", "DROP", "VACUUM", "REINDEX":
		return "ddl"
	case "BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE":
		return "transaction"
	case "PRAGMA":
		if strings.Contains(query, "=") {
			return "mutating-pragma"
		}
		return "row-pragma"
	default:
		return ""
	}
}

func batchClassAllowed(sink, class string) bool {
	if sink == "ExecuteTransient" {
		return class == "ddl" || class == "transaction" || class == "mutating-pragma"
	}
	return class == "dml" || class == "row-pragma"
}

func execOptionsShape(expression ast.Expr) string {
	if identifier, ok := expression.(*ast.Ident); ok && identifier.Name == "nil" {
		return "nil"
	}
	pointer, ok := expression.(*ast.UnaryExpr)
	if !ok || pointer.Op != token.AND {
		return "dynamic"
	}
	literal, ok := pointer.X.(*ast.CompositeLit)
	if !ok {
		return "dynamic"
	}
	var fields []string
	for _, element := range literal.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return "positional"
		}
		name, _ := pair.Key.(*ast.Ident)
		shape := name.Name
		if name.Name == "Args" {
			if args, ok := pair.Value.(*ast.CompositeLit); ok {
				shape += fmt.Sprintf("[%d]", len(args.Elts))
			} else {
				shape += "[dynamic]"
			}
		}
		fields = append(fields, shape)
	}
	return strings.Join(fields, ",")
}

func expressionDescription(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.CallExpr:
		if selector, ok := expression.Fun.(*ast.SelectorExpr); ok {
			return selector.Sel.Name + "()"
		}
	}
	return fmt.Sprintf("%T", expression)
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

func rawSQLValueToken(query string) string {
	for i := 0; i < len(query); {
		c := query[i]
		if unicode.IsSpace(rune(c)) {
			i++
			continue
		}
		if c == '-' && i+1 < len(query) && query[i+1] == '-' {
			if end := strings.IndexByte(query[i+2:], '\n'); end >= 0 {
				i += end + 3
				continue
			}
			return ""
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

func isSQLWord(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}
