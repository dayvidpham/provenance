package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
)

type replayScanCountingConnection struct {
	sqlConnection
	scans, columns, pointReads int
}

func (c *replayScanCountingConnection) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "FROM journal_attributed j") && strings.Contains(query, "LEFT JOIN journal_task_events") {
		c.scans++
	}
	if strings.Contains(query, "pragma_table_info") {
		c.columns++
	}
	return c.sqlConnection.QueryContext(ctx, query, args...)
}

func (c *replayScanCountingConnection) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	if strings.Contains(query, "FROM journal_attributed WHERE journal_id") ||
		strings.Contains(query, "FROM journal_task_events WHERE journal_id") ||
		strings.Contains(query, "FROM journal WHERE produced_by_operation_journal_id") ||
		strings.Contains(query, "FROM journal j JOIN journal_operations o") {
		c.pointReads++
	}
	return c.sqlConnection.PrepareContext(ctx, query)
}

func TestReplayOrderedScanAvoidsPerRowReads(t *testing.T) {
	db := newJournalDB(t)
	f := newActorOwnershipFixture(t, db)
	effects := make([]journal.Effect, 8)
	for i := range effects {
		effects[i] = journal.Effect{Sort: journal.EffectTaskEvent, TaskID: f.task, EventKind: actorOwnershipMaterialKind, Payload: []byte(`{}`)}
	}
	if _, err := db.Apply(journal.OperationInput{OperationID: "scan-many-events", ActorID: f.actor, AuthorityJournalID: &f.boot, CommandDigest: []byte("scan"), Effects: effects}); err != nil {
		t.Fatal(err)
	}
	assertReplayOracleParity(t, db, false)
	scope := takePoolScope(t, db)
	defer scope.release()
	counted := &replayScanCountingConnection{sqlConnection: scope.conn}
	scope.conn = counted
	if _, _, _, _, _, err := scope.rederiveProjectionsShadow(); err != nil {
		t.Fatal(err)
	}
	if counted.scans != 1 || counted.columns != 2 || counted.pointReads != 0 {
		t.Fatalf("ordered scans=%d schema probes=%d point reads=%d; want 1, 2, 0", counted.scans, counted.columns, counted.pointReads)
	}
}
