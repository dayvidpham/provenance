package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
)

type replayScanCountingConnection struct {
	sqlConnection
	scans, columns, pointReads int
}

func TestReplayParallelDecodeParity(t *testing.T) {
	previous := canonicalDecodeWorkersOverride.Swap(2)
	defer canonicalDecodeWorkersOverride.Store(previous)
	db := newJournalDB(t)
	f := newActorOwnershipFixture(t, db)
	inputs := make([]journal.OperationInput, 150)
	for i := range inputs {
		inputs[i] = batchDecisionInput(fmt.Sprintf("parallel-decode-%d", i), f.actor, f.boot, `{}`)
	}
	if result, err := db.ApplyBatch(inputs); err != nil || !result.Committed {
		t.Fatalf("apply operations: committed=%v err=%v", result.Committed, err)
	}
	// More than one worker chunk exercises the concurrent producer path.
	assertReplayOracleParity(t, db, false)
}

func TestReplayParallelFirstPayloadErrorAcrossBatches(t *testing.T) {
	db := newJournalDB(t)
	f := newActorOwnershipFixture(t, db)
	if _, err := db.Apply(journal.OperationInput{
		OperationID: "parallel-malformed", ActorID: f.actor, AuthorityJournalID: &f.boot,
		CommandDigest: []byte("parallel-malformed"),
		Effects: []journal.Effect{
			{Sort: journal.EffectLabelAdd, TaskID: f.task, Label: "first"},
			{Sort: journal.EffectLabelAdd, TaskID: f.task, Label: "second"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	scope := takePoolScope(t, db)
	_, err := scope.conn.ExecContext(scope.ctx, `UPDATE journal_task_events SET payload='{"label":7}' WHERE event_kind=?1`, string(journal.EventKindLabelAdded))
	scope.release()
	if err != nil {
		t.Fatalf("damage payloads: %v", err)
	}
	assertReplayOracleParityOptions(t, db, true, false, 2)
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
	// Force the eight-effect operation across several batch boundaries.
	assertReplayOracleParityOptions(t, db, false, false, 2)
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
