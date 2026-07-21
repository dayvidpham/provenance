package sqlite

import (
	"errors"
	"fmt"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var errUnknownSQLStatement = errors.New("sqlite: unknown sealed SQL statement")

type sqlStatementClass uint8

const (
	sqlDMLStatement sqlStatementClass = iota + 1
	sqlDDLStatement
)

type sealedSQLStatement interface {
	execute(*zs.Conn, *sqlitex.ExecOptions) error
	statementClass() sqlStatementClass
}

func executeStatement(conn *zs.Conn, statement sealedSQLStatement, options *sqlitex.ExecOptions) error {
	if statement == nil {
		return fmt.Errorf("%w: nil statement", errUnknownSQLStatement)
	}
	return statement.execute(conn, options)
}

func unknownSQLStatementError(domain string, value uint16) error {
	return fmt.Errorf("%w: %s id %d; update its exhaustive dispatcher", errUnknownSQLStatement, domain, value)
}

func appendStaticSQLArgs(options *sqlitex.ExecOptions, values ...any) *sqlitex.ExecOptions {
	result := &sqlitex.ExecOptions{}
	if options != nil {
		*result = *options
		result.Args = append([]any(nil), options.Args...)
	}
	result.Args = append(result.Args, values...)
	return result
}
