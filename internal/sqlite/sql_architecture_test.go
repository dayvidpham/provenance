package sqlite

import (
	"errors"
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

func TestProductionSQLUsesOneStructurallySealedDispatcher(t *testing.T) {
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
	findings, dispatchers, err := inspectSQLArchitecture(sources)
	if err != nil {
		t.Fatal(err)
	}
	if dispatchers != 1 {
		findings = append(findings, "package must contain exactly one structurally sealed SQL dispatcher")
	}
	if len(findings) != 0 {
		t.Fatalf("SQL architecture violations: %v", findings)
	}
}

func TestSQLArchitectureGuardRejectsOpenAndAlteredDataflow(t *testing.T) {
	statementBodies := map[string]string{
		"query parameter":        `sqlitex.Execute(conn, query, options)`,
		"query return":           `sqlitex.Execute(conn, queryFor(input), options)`,
		"multi-hop copy":         `first := query; second := first; sqlitex.Execute(conn, second, options)`,
		"concatenation":          `sqlitex.Execute(conn, "SELECT "+column, options)`,
		"plus equals":            `query := "SELECT "; query += column; sqlitex.Execute(conn, query, options)`,
		"join":                   `sqlitex.Execute(conn, strings.Join(parts, ""), options)`,
		"format":                 `sqlitex.Execute(conn, fmt.Sprintf("SELECT %s", column), options)`,
		"positional wrapper":     `sqlitex.Execute(conn, wrapper{query}.text, options)`,
		"mutated wrapper":        `wrapped := wrapper{text: "SELECT ?1"}; wrapped.text = query; sqlitex.Execute(conn, wrapped.text, options)`,
		"same-name altered body": `statement = statementFromCaller(); sqlitex.Execute(conn, query, options)`,
	}
	for name, body := range statementBodies {
		source := []byte("package sqlite\nfunc executeStatement(conn *Conn, statement Statement, query string, options *Options) { " + body + " }")
		assertSQLGuardRejects(t, name, source)
	}

	valueForms := []string{
		"SELECT * FROM rows WHERE value = 7",
		"SELECT * FROM rows WHERE value <> 7",
		"SELECT * FROM rows WHERE value > 7",
		"SELECT * FROM rows LIMIT 7",
		"SELECT * FROM rows OFFSET 7",
		"SELECT * FROM rows WHERE value IN (7)",
		"SELECT * FROM rows WHERE value BETWEEN 1 AND 2",
		"SELECT * FROM rows WHERE value + 1 > other",
		"SELECT COALESCE(value, 7) FROM rows",
		"INSERT INTO rows (value) VALUES (7)",
		"SELECT * FROM rows WHERE value = 'quoted'",
		"SELECT * FROM rows WHERE value = X'00ff'",
		"SELECT * FROM rows WHERE value IS NULL",
		"SELECT * FROM rows WHERE value IS TRUE",
	}
	for _, query := range valueForms {
		source := []byte(`package sqlite
		type Statement uint8
		const statementOne Statement = 1
		func dispatch(conn *Conn, statement Statement, options *Options) error {
			switch statement {
			case statementOne:
				return sqlitex.Execute(conn, ` + strconv.Quote(query) + `, options)
			default:
				return fmt.Errorf("%w: %d", errUnknown, statement)
			}
		}`)
		assertSQLGuardRejects(t, query, source)
	}
}

func TestSQLArchitectureGuardAcceptsBoundDMLAndStaticDDL(t *testing.T) {
	source := []byte(`package sqlite
		type Statement uint8
		const (
			statementRead Statement = iota + 1
			statementDDL
		)
		func dispatch(conn *Conn, statement Statement, options *Options) error {
			switch statement {
			case statementRead:
				return sqlitex.Execute(conn, "SELECT value FROM rows WHERE id>?1 LIMIT ?2", options)
			case statementDDL:
				return sqlitex.ExecuteTransient(conn, "CREATE TABLE rows (id INTEGER PRIMARY KEY, value TEXT DEFAULT '')", options)
			default:
				return fmt.Errorf("%w: %d", errUnknown, statement)
			}
		}`)
	findings, dispatchers, err := inspectSQLArchitecture(map[string][]byte{"fixture.go": source})
	if err != nil || dispatchers != 1 || len(findings) != 0 {
		t.Fatalf("bound/static positive control findings=%v dispatchers=%d err=%v", findings, dispatchers, err)
	}
	values := []any{"quote' -- /* data */", "comment -- /* data */", "nul\x00data"}
	options := appendStaticSQLArgs(nil, values...)
	if len(options.Args) != len(values) {
		t.Fatalf("bound hostile values count=%d want=%d", len(options.Args), len(values))
	}
	for i := range values {
		if options.Args[i] != values[i] {
			t.Fatalf("bound hostile value %d changed: got=%q want=%q", i, options.Args[i], values[i])
		}
	}
}

func TestSealedSQLDispatcherRejectsUnknownEnum(t *testing.T) {
	err := executeStatement(nil, sqlStatement(^uint16(0)), nil)
	if !errors.Is(err, errUnknownSQLStatement) {
		t.Fatalf("unknown statement returned %v", err)
	}
}

func assertSQLGuardRejects(t *testing.T, name string, source []byte) {
	t.Helper()
	findings, dispatchers, err := inspectSQLArchitecture(map[string][]byte{"bypass.go": source})
	if err != nil {
		t.Fatalf("%s parse: %v", name, err)
	}
	if len(findings) == 0 && dispatchers == 1 {
		t.Errorf("%s bypass was accepted", name)
	}
}

func inspectSQLArchitecture(sources map[string][]byte) ([]string, int, error) {
	type parsedFile struct {
		name string
		fset *token.FileSet
		file *ast.File
	}
	integerTypes := map[string]bool{}
	enumValues := map[string]map[string]bool{}
	var files []parsedFile
	for name, source := range sources {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			return nil, 0, err
		}
		files = append(files, parsedFile{name, fset, file})
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				underlying, integer := func() (*ast.Ident, bool) {
					if !ok {
						return nil, false
					}
					id, yes := typeSpec.Type.(*ast.Ident)
					return id, yes
				}()
				if integer && strings.HasPrefix(underlying.Name, "uint") {
					integerTypes[typeSpec.Name.Name] = true
				}
			}
		}
	}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			currentType := ""
			for _, spec := range general.Specs {
				value := spec.(*ast.ValueSpec)
				if id, ok := value.Type.(*ast.Ident); ok {
					currentType = id.Name
				}
				if integerTypes[currentType] {
					if enumValues[currentType] == nil {
						enumValues[currentType] = map[string]bool{}
					}
					for _, name := range value.Names {
						enumValues[currentType][name.Name] = true
					}
				}
			}
		}
	}

	allowedSinks := map[*ast.CallExpr]bool{}
	dispatchers := 0
	var findings []string
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			valid, sinks, reason := structurallySealedDispatcher(function, enumValues)
			if valid {
				dispatchers++
				for _, sink := range sinks {
					allowedSinks[sink] = true
				}
			} else if reason != "" {
				findings = append(findings, parsed.name+": "+reason)
			}
		}
	}
	for _, parsed := range files {
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isSQLSink(call) {
				return true
			}
			if !allowedSinks[call] {
				findings = append(findings, parsed.name+": SQL sink is outside the structurally sealed dispatcher")
			}
			return true
		})
	}
	return findings, dispatchers, nil
}

func structurallySealedDispatcher(function *ast.FuncDecl, enumValues map[string]map[string]bool) (bool, []*ast.CallExpr, string) {
	if function.Body == nil || len(function.Body.List) != 1 || function.Type.Params == nil || len(function.Type.Params.List) != 3 {
		return false, nil, ""
	}
	connName := fieldName(function.Type.Params.List[0])
	statementName := fieldName(function.Type.Params.List[1])
	optionsName := fieldName(function.Type.Params.List[2])
	statementType, ok := function.Type.Params.List[1].Type.(*ast.Ident)
	values := enumValues[statementTypeName(statementType)]
	if !ok || connName == "" || statementName == "" || optionsName == "" || len(values) == 0 {
		return false, nil, ""
	}
	switchStatement, ok := function.Body.List[0].(*ast.SwitchStmt)
	tag, tagOK := switchStatement.Tag.(*ast.Ident)
	if !ok || !tagOK || tag.Name != statementName {
		return false, nil, ""
	}
	seen := map[string]bool{}
	defaultSeen := false
	var sinks []*ast.CallExpr
	for _, clauseNode := range switchStatement.Body.List {
		clause := clauseNode.(*ast.CaseClause)
		if len(clause.List) == 0 {
			defaultSeen = isTypedDispatcherFailure(clause.Body, statementName)
			continue
		}
		if len(clause.List) != 1 || len(clause.Body) != 1 {
			return false, nil, "candidate SQL dispatcher has a non-atomic case"
		}
		caseID, caseOK := clause.List[0].(*ast.Ident)
		returned, returnOK := clause.Body[0].(*ast.ReturnStmt)
		if !caseOK || !values[caseID.Name] || seen[caseID.Name] || !returnOK || len(returned.Results) != 1 {
			return false, nil, "candidate SQL dispatcher has an invalid or duplicate enum case"
		}
		call, callOK := returned.Results[0].(*ast.CallExpr)
		if !callOK || !isSQLSink(call) || len(call.Args) != 3 || identName(call.Args[0]) != connName {
			return false, nil, "candidate SQL dispatcher case does not directly invoke a SQL sink"
		}
		literal, literalOK := call.Args[1].(*ast.BasicLit)
		if !literalOK || literal.Kind != token.STRING || !validOptionsExpression(call.Args[2], optionsName) {
			return false, nil, "candidate SQL dispatcher case accepts non-static SQL or caller dataflow"
		}
		query, _ := strconv.Unquote(literal.Value)
		if reason := rawDMLValue(query); reason != "" {
			return false, nil, "sealed DML contains raw bindable value: " + reason
		}
		seen[caseID.Name] = true
		sinks = append(sinks, call)
	}
	if !defaultSeen || len(seen) != len(values) {
		return false, nil, "candidate SQL dispatcher is not exhaustive with typed default failure"
	}
	return true, sinks, ""
}

func rawDMLValue(query string) string {
	upper := strings.ToUpper(strings.TrimSpace(query))
	for _, prefix := range []string{"CREATE ", "ALTER ", "DROP ", "PRAGMA ", "BEGIN ", "COMMIT", "ROLLBACK"} {
		if strings.HasPrefix(upper, prefix) {
			return ""
		}
	}
	for i := 0; i < len(query); {
		c := query[i]
		if c == '?' {
			i++
			for i < len(query) && query[i] >= '0' && query[i] <= '9' {
				i++
			}
			continue
		}
		if c == '\'' || c == '"' {
			return "quoted literal"
		}
		if c >= '0' && c <= '9' {
			return "numeric literal"
		}
		if isSQLWordByte(c) {
			start := i
			for i < len(query) && (isSQLWordByte(query[i]) || query[i] >= '0' && query[i] <= '9') {
				i++
			}
			word := strings.ToUpper(query[start:i])
			if word == "NULL" || word == "TRUE" || word == "FALSE" {
				return word
			}
			continue
		}
		i++
	}
	return ""
}

func isSQLWordByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}
func statementTypeName(identifier *ast.Ident) string {
	if identifier == nil {
		return ""
	}
	return identifier.Name
}
func fieldName(field *ast.Field) string {
	if len(field.Names) != 1 {
		return ""
	}
	return field.Names[0].Name
}
func identName(expression ast.Expr) string {
	identifier, _ := expression.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}

func validOptionsExpression(expression ast.Expr, optionsName string) bool {
	if identName(expression) == optionsName {
		return true
	}
	call, ok := expression.(*ast.CallExpr)
	if !ok || identName(call.Fun) != "appendStaticSQLArgs" || len(call.Args) < 2 || identName(call.Args[0]) != optionsName {
		return false
	}
	for _, argument := range call.Args[1:] {
		switch value := argument.(type) {
		case *ast.BasicLit:
			if value.Kind != token.INT && value.Kind != token.STRING {
				return false
			}
		case *ast.Ident:
			if value.Name != "nil" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isTypedDispatcherFailure(statements []ast.Stmt, statementName string) bool {
	if len(statements) != 1 {
		return false
	}
	returned, ok := statements[0].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 1 {
		return false
	}
	call, ok := returned.Results[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, selectorOK := call.Fun.(*ast.SelectorExpr)
	if !selectorOK || selector.Sel.Name != "Errorf" || len(call.Args) < 3 {
		return false
	}
	format, formatOK := call.Args[0].(*ast.BasicLit)
	return formatOK && strings.Contains(unquoteSQLLiteral(format), "%w") && identName(call.Args[len(call.Args)-1]) == statementName
}

func isSQLSink(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	packageName, packageOK := selector.X.(*ast.Ident)
	return packageOK && packageName.Name == "sqlitex" && (selector.Sel.Name == "Execute" || selector.Sel.Name == "ExecuteTransient")
}

func unquoteSQLLiteral(literal *ast.BasicLit) string {
	value, _ := strconv.Unquote(literal.Value)
	return value
}
