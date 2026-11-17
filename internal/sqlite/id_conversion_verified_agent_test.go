package sqlite

import (
	"context"
	"database/sql"
	"testing"
)

// TestConvertIDsRewritesVerifiedAgentProof is the regression for the
// classification gap on provenance_verified_agent.agent_id. The column mirrors
// agents.id, so conversion must rewrite a retired proof row to the slash form
// with the agent identity set. The proof digest is not rewritten: it is stale
// after conversion, and the next open re-verifies and re-seeds it.
func TestConvertIDsRewritesVerifiedAgentProof(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open(sqliteDriverName, ":memory:")
	if err != nil {
		t.Fatalf("open conversion fixture: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	const retired = "provenance-test--01968a3c-9d4f-7c8a-bc12-feedfacecafe"
	const slash = "provenance-test/01968a3c-9d4f-7c8a-bc12-feedfacecafe"
	const staleDigest = "digest-written-before-conversion"

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE agents (id TEXT PRIMARY KEY) STRICT;
		CREATE TABLE provenance_verified_agent (agent_id TEXT PRIMARY KEY) STRICT, WITHOUT ROWID;
		CREATE TABLE provenance_verified_agent_meta (id INTEGER PRIMARY KEY CHECK (id = 1), agent_digest TEXT NOT NULL) STRICT;
	`); err != nil {
		t.Fatalf("create conversion fixture: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agents (id) VALUES (?)`, retired); err != nil {
		t.Fatalf("insert owned agent: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO provenance_verified_agent (agent_id) VALUES (?)`, retired); err != nil {
		t.Fatalf("insert verified agent proof: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO provenance_verified_agent_meta (id, agent_digest) VALUES (1, ?)`, staleDigest); err != nil {
		t.Fatalf("insert stale proof digest: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin conversion: %v", err)
	}
	defer tx.Rollback()
	report, err := ConvertIDsInTx(ctx, tx, IDConversionOptions{})
	if err != nil {
		t.Fatalf("ConvertIDsInTx: %v", err)
	}
	if report.AlreadyCurrent || report.ChangedFields < 2 {
		t.Fatalf("conversion report = %+v, want the agent row and the proof row rewritten", report)
	}

	var proof, agent, digest string
	if err := tx.QueryRowContext(ctx, `SELECT agent_id FROM provenance_verified_agent`).Scan(&proof); err != nil {
		t.Fatalf("read converted proof: %v", err)
	}
	if proof != slash {
		t.Fatalf("proof agent_id = %q, want %q", proof, slash)
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM agents`).Scan(&agent); err != nil {
		t.Fatalf("read converted agent: %v", err)
	}
	if agent != slash {
		t.Fatalf("agents.id = %q, want %q", agent, slash)
	}
	if err := tx.QueryRowContext(ctx, `SELECT agent_digest FROM provenance_verified_agent_meta WHERE id = 1`).Scan(&digest); err != nil {
		t.Fatalf("read proof digest: %v", err)
	}
	if digest != staleDigest {
		t.Fatalf("proof digest = %q, want the pre-conversion digest %q left stale", digest, staleDigest)
	}
}
