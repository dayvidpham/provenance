package fusedtx_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/dayvidpham/provenance/internal/fusedtx"
	_ "modernc.org/sqlite"
)

const callbackFailure = "sentinel callback failure"

func TestExactSystemHandleCommitsApplicationAndCheckpoint(t *testing.T) {
	system, _ := newSystem(t)
	root := system.Root()
	systemDB := system.DB()
	createSentinelTable(t, systemDB)

	workflow := func(workflowCtx dbos.DBOSContext, value string) (string, error) {
		return fusedtx.Run(workflowCtx, system, func(ctx context.Context, tx fusedtx.SQLTx) (string, error) {
			if _, err := tx.Exec(ctx, `INSERT INTO fused_sentinel (value) VALUES (?)`, value); err != nil {
				return "", err
			}
			return value, nil
		})
	}
	dbos.RegisterWorkflow(root, workflow)
	launch(t, root)

	const workflowID = "fusedtx-exact-system-handle"
	handle, err := dbos.RunWorkflow(root, workflow, "committed", dbos.WithWorkflowID(workflowID))
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	result, err := handle.GetResult()
	if err != nil {
		t.Fatalf("get workflow result: %v", err)
	}
	if result != "committed" {
		t.Fatalf("workflow result = %q, want committed", result)
	}

	if got := count(t, systemDB, `SELECT COUNT(*) FROM fused_sentinel WHERE value = ?`, "committed"); got != 1 {
		t.Fatalf("committed sentinel rows = %d, want 1", got)
	}
	if got := count(t, systemDB, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid = ?`, workflowID); got != 1 {
		t.Fatalf("operation_outputs checkpoints = %d, want 1", got)
	}
	if got := count(t, systemDB, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'transaction_completion'`); got != 0 {
		t.Fatalf("exact system handle created transaction_completion table (%d rows); want shared-system path without it", got)
	}
}

func TestCallbackFailureRollsBackSentinelWithoutSuccessfulCheckpoint(t *testing.T) {
	system, _ := newSystem(t)
	root := system.Root()
	systemDB := system.DB()
	createSentinelTable(t, systemDB)

	workflow := func(workflowCtx dbos.DBOSContext, _ string) (string, error) {
		return fusedtx.Run(workflowCtx, system, func(ctx context.Context, tx fusedtx.SQLTx) (string, error) {
			if _, err := tx.Exec(ctx, `INSERT INTO fused_sentinel (value) VALUES (?)`, "rolled-back"); err != nil {
				return "", err
			}
			return "", errors.New(callbackFailure)
		})
	}
	dbos.RegisterWorkflow(root, workflow)
	launch(t, root)

	const workflowID = "fusedtx-callback-failure"
	handle, err := dbos.RunWorkflow(root, workflow, "", dbos.WithWorkflowID(workflowID))
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	if _, err := handle.GetResult(); err == nil || !strings.Contains(err.Error(), callbackFailure) {
		t.Fatalf("workflow error = %v, want callback failure", err)
	}

	if got := count(t, systemDB, `SELECT COUNT(*) FROM fused_sentinel`); got != 0 {
		t.Fatalf("rolled-back sentinel rows = %d, want 0", got)
	}
	if got := count(t, systemDB, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid = ? AND error IS NULL`, workflowID); got != 0 {
		t.Fatalf("successful operation_outputs checkpoints = %d, want 0", got)
	}
	if got := count(t, systemDB, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid = ? AND error IS NOT NULL`, workflowID); got != 1 {
		t.Fatalf("failed operation_outputs checkpoints = %d, want 1", got)
	}
}

func TestSecondHandleCannotBecomeFusedDataSource(t *testing.T) {
	system, dsn := newSystem(t)
	root := system.Root()
	systemDB := system.DB()
	secondDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open second SQLite handle: %v", err)
	}
	secondDB.SetMaxOpenConns(8)
	secondDB.SetMaxIdleConns(4)
	t.Cleanup(func() {
		if err := secondDB.Close(); err != nil {
			t.Errorf("close second SQLite handle: %v", err)
		}
	})

	createSentinelTable(t, systemDB)

	workflow := func(workflowCtx dbos.DBOSContext, value string) (string, error) {
		return fusedtx.Run(workflowCtx, system, func(ctx context.Context, tx fusedtx.SQLTx) (string, error) {
			if _, err := tx.Exec(ctx, `INSERT INTO fused_sentinel (value) VALUES (?)`, value); err != nil {
				return "", err
			}
			return value, nil
		})
	}
	dbos.RegisterWorkflow(root, workflow)
	launch(t, root)

	const workflowID = "fusedtx-second-handle"
	handle, err := dbos.RunWorkflow(root, workflow, "exact", dbos.WithWorkflowID(workflowID))
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	if _, err := handle.GetResult(); err != nil {
		t.Fatalf("get workflow result: %v", err)
	}

	if got := count(t, systemDB, `SELECT COUNT(*) FROM fused_sentinel WHERE value = ?`, "exact"); got != 1 {
		t.Fatalf("exact-system sentinel rows = %d, want 1", got)
	}
	if got := count(t, systemDB, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid = ?`, workflowID); got != 1 {
		t.Fatalf("second-handle operation_outputs checkpoints = %d, want 1", got)
	}
	if got := count(t, systemDB, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'transaction_completion'`); got != 0 {
		t.Fatalf("second same-file handle became a fused data source (%d completion tables); want exact system path", got)
	}
}

func newSystem(t *testing.T) (*fusedtx.System, string) {
	t.Helper()
	dsn := systemDSN(t)
	system, err := fusedtx.OpenSystem(context.Background(), fusedtx.SystemConfig{
		SQLiteDSN:          dsn,
		AppName:            "provenance-fusedtx-test",
		ApplicationVersion: "fusedtx-test-v1",
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("create owned fused system: %v", err)
	}
	t.Cleanup(func() {
		system.Close(30 * time.Second)
	})
	return system, dsn
}

func systemDSN(t *testing.T) string {
	t.Helper()
	return "file:" + filepath.Join(t.TempDir(), "fusedtx.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
}

func launch(t *testing.T, root dbos.DBOSContext) {
	t.Helper()
	if err := dbos.Launch(root); err != nil {
		t.Fatalf("launch DBOS context: %v", err)
	}
}

func createSentinelTable(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE fused_sentinel (value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create application sentinel table: %v", err)
	}
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var result int
	if err := db.QueryRow(query, args...).Scan(&result); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return result
}
