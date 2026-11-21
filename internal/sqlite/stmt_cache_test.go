package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

type statementTrackingConnection struct {
	sqlConnection
	scope     *connScope
	prepared  map[string]*sql.Stmt
	duplicate string
	dropErr   error
	checked   bool
}

func (c *statementTrackingConnection) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	stmt, err := c.sqlConnection.PrepareContext(ctx, query)
	if err == nil {
		if _, exists := c.prepared[query]; exists {
			c.duplicate = query
		}
		c.prepared[query] = stmt
	}
	return stmt, err
}

func (c *statementTrackingConnection) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if query == "DROP TABLE IF EXISTS shadow_tasks" && len(c.prepared) != 0 && !c.checked {
		c.checked = true
		if len(c.scope.stmts) != 0 {
			c.dropErr = fmt.Errorf("%d cached statements remain before shadow drop", len(c.scope.stmts))
		}
		for text, stmt := range c.prepared {
			_, err := stmt.QueryContext(ctx)
			if err == nil || !strings.Contains(err.Error(), "statement is closed") {
				c.dropErr = fmt.Errorf("statement %q not closed before shadow drop: %v", text, err)
				break
			}
		}
	}
	return c.sqlConnection.ExecContext(ctx, query, args...)
}

func TestReplayStatementCacheClosesBeforeDrop(t *testing.T) {
	db := newJournalDB(t)
	newActorOwnershipFixture(t, db)
	scope := takePoolScope(t, db)
	defer scope.release()
	conn := &statementTrackingConnection{sqlConnection: scope.conn, scope: scope, prepared: make(map[string]*sql.Stmt)}
	scope.conn = conn
	_, _, _, _, _, err := scope.rederiveProjectionsShadow()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if conn.duplicate != "" || !conn.checked || conn.dropErr != nil {
		t.Fatalf("statement cache: duplicate=%q checked=%v drop=%v", conn.duplicate, conn.checked, conn.dropErr)
	}
	if len(conn.prepared) == 0 {
		t.Fatal("replay prepared no statements")
	}
}

func TestReplayStatementCacheDetectsOpenStatementBeforeDrop(t *testing.T) {
	db := newJournalDB(t)
	scope := takePoolScope(t, db)
	defer scope.release()
	if err := scope.createProjectionShadow(); err != nil {
		t.Fatal(err)
	}
	conn := &statementTrackingConnection{sqlConnection: scope.conn, scope: scope, prepared: make(map[string]*sql.Stmt)}
	scope.conn = conn
	scope.projectionTarget = projectionTargetShadow
	scope.cacheReplay = true
	if _, err := scope.prepared(stmtTaskStatusRead, scope.projectionTarget.readTaskStatusQuery()); err != nil {
		t.Fatal(err)
	}
	_, _ = scope.conn.ExecContext(scope.ctx, "DROP TABLE IF EXISTS shadow_tasks")
	if !conn.checked || conn.dropErr == nil {
		t.Fatalf("open statement was not detected: checked=%v error=%v", conn.checked, conn.dropErr)
	}
	if err := scope.closeStmts(projectionTargetShadow, false); err != nil {
		t.Fatal(err)
	}
	scope.cacheReplay = false
	scope.projectionTarget = projectionTargetLive
	if err := scope.dropProjectionShadow(); err != nil {
		t.Fatal(err)
	}
}
