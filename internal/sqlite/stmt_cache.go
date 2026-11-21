package sqlite

import (
	"database/sql"
	"fmt"
)

// Each ID identifies one static SQL statement (with target-specific variants).
// The cache belongs to one scope and is used only while replaying into TEMP
// tables; live Apply retains its existing execution path.
type stmtID uint8

const (
	stmtJournalRow stmtID = iota + 1
	stmtTaskEvent
	stmtTaskStatusRead
	stmtAuthorityTransition
	stmtAuthorityEpisode
	stmtScopedDecision
	stmtScopedEvidence
	stmtTaskStatusWrite
	stmtCanonicalEffect
	stmtCanonicalOrdinal
	stmtCanonicalTaskCreate
	stmtCanonicalTaskUpdate
	stmtAttribution
	stmtWatermark
	stmtOwnerRead
	stmtOwnerWrite
	stmtEdgeAdd
	stmtEdgeRemove
	stmtLabelAdd
	stmtLabelRemove
	stmtCommentAdd
)

type stmtKey struct {
	target projectionTarget
	id     stmtID
}

// stmtError implements the row scanner contract for preparation errors.
type stmtError struct{ err error }

func (s stmtError) Scan(...any) error { return s.err }

func (scope *connScope) prepared(id stmtID, query string) (*sql.Stmt, error) {
	key := stmtKey{target: scope.projectionTarget, id: id}
	if stmt := scope.stmts[key]; stmt != nil {
		return stmt, nil
	}
	stmt, err := scope.conn.PrepareContext(scope.ctx, query)
	if err != nil {
		return nil, fmt.Errorf("prepare replay statement %d: %w", id, err)
	}
	if scope.stmts == nil {
		scope.stmts = make(map[stmtKey]*sql.Stmt)
	}
	scope.stmts[key] = stmt
	return stmt, nil
}

func (scope *connScope) projectionRow(id stmtID, query string, args ...any) sqlRowScanner {
	if !scope.cacheReplay || scope.projectionTarget != projectionTargetShadow {
		return scope.conn.QueryRowContext(scope.ctx, query, args...)
	}
	stmt, err := scope.prepared(id, query)
	if err != nil {
		return stmtError{err}
	}
	return stmt.QueryRowContext(scope.ctx, args...)
}

func (scope *connScope) projectionExec(id stmtID, query string, args ...any) (sql.Result, error) {
	if !scope.cacheReplay || scope.projectionTarget != projectionTargetShadow {
		return scope.conn.ExecContext(scope.ctx, query, args...)
	}
	stmt, err := scope.prepared(id, query)
	if err != nil {
		return nil, err
	}
	return stmt.ExecContext(scope.ctx, args...)
}

func (scope *connScope) closeStmts(target projectionTarget, all bool) error {
	if scope.discarded {
		return nil
	}
	var first error
	for key, stmt := range scope.stmts {
		if all || key.target == target {
			if err := stmt.Close(); err != nil && first == nil {
				first = err
			}
			delete(scope.stmts, key)
		}
	}
	return first
}
