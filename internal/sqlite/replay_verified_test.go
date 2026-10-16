package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
)

// Count the actual whole-journal enumeration, not elapsed time or ledger
// writes (exact replay never writes the ledger, even on the slow path).
type replayScanConnection struct {
	sqlConnection
	scans      int
	failLedger *sql.DB
}

func (c *replayScanConnection) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if c.failLedger != nil && strings.Contains(query, "FROM provenance_verified_store") {
		return c.failLedger.QueryRowContext(ctx, "SELECT 1")
	}
	return c.sqlConnection.QueryRowContext(ctx, query, args...)
}

func (c *replayScanConnection) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "SELECT journal_id,canonical_mutation FROM journal_operations WHERE canonical_mutation IS NOT") {
		c.scans++
	}
	return c.sqlConnection.QueryContext(ctx, query, args...)
}

func replayVerifiedFixture(t *testing.T) (*DB, journal.OperationInput) {
	t.Helper()
	path := t.TempDir() + "/replay.db"
	db, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor, task, authority := newFactContextEnvironment(t, db)
	in := batchDecisionInput("replay-verified", actor, authority, `{}`)
	in.Effects[0].TaskID = task
	taskContext, err := journal.TaskContext(task)
	if err != nil {
		t.Fatal(err)
	}
	in.Effects[0].Contexts = []journal.EventContext{taskContext}
	if _, err := db.Apply(in); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// A real production reopen verifies and publishes the proof; never fabricate
	// a ledger row for data that has not passed the full verification.
	db, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, in
}

func TestExactReplayVerifiedSnapshotSkipsJournalScan(t *testing.T) {
	db, in := replayVerifiedFixture(t)
	before := snapshotAllDurableTables(t, db)
	want, err := db.LookupCommitted(in.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	want.ShortCircuited = true
	scope := takePoolScope(t, db)
	defer scope.release()
	counted := &replayScanConnection{sqlConnection: scope.conn}
	scope.conn = counted
	err = runImmediateTransaction(scope.ctx, scope.conn, func() error {
		res, err := scope.foldOperation(in, nil)
		if err == nil && !reflect.DeepEqual(res, want) {
			t.Fatalf("replay = %+v, want %+v", res, want)
		}
		return err
	})
	scope.release()
	if err != nil {
		t.Fatal(err)
	}
	if counted.scans != 0 {
		t.Fatalf("clean replay enumerated all canonical operations %d times", counted.scans)
	}
	// Exercise the public entrypoint too, and pin all durable data including
	// generation, verification timestamp/count and result-slot rows.
	got, err := db.Apply(in)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Apply replay = %+v, %v; want %+v", got, err, want)
	}
	if after := snapshotAllDurableTables(t, db); !reflect.DeepEqual(before, after) {
		t.Fatal("exact replay changed durable data")
	}
}

func TestExactReplayProofFallback(t *testing.T) {
	for _, tc := range []struct {
		name, damage string
		reject       bool
	}{
		{"absent-ledger", "DELETE FROM provenance_verified_store", false},
		{"stale-ledger", "UPDATE provenance_store_generation SET generation=generation+1", false},
		{"unrelated-context-damage", "DELETE FROM journal_decision_contexts", true},
		{"nonfact-canonical-damage", "UPDATE journal_operations SET canonical_mutation=x'00' WHERE operation_id != 'replay-verified'", true},
		{"missing-trigger", "DROP TRIGGER provenance_store_generation_on_journal_operations_UPDATE", false},
		{"altered-trigger", "DROP TRIGGER provenance_store_generation_on_journal_operations_UPDATE; CREATE TRIGGER provenance_store_generation_on_journal_operations_UPDATE AFTER UPDATE ON journal_operations BEGIN SELECT 1; END", false},
		{"altered-trigger-hidden-context-dml", "DROP TRIGGER provenance_store_generation_on_journal_decision_contexts_DELETE; CREATE TRIGGER provenance_store_generation_on_journal_decision_contexts_DELETE AFTER DELETE ON journal_decision_contexts BEGIN SELECT 1; END; DELETE FROM journal_decision_contexts", true},
		{"context-schema-trigger", "CREATE TRIGGER extra_context_trigger AFTER INSERT ON journal_decision_contexts BEGIN SELECT 1; END", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, in := replayVerifiedFixture(t)
			// Replay a different fact, not the damaged fact, proving unrelated context
			// corruption still runs the store-wide gate.
			if tc.name == "unrelated-context-damage" || tc.name == "altered-trigger-hidden-context-dml" {
				in.OperationID = "unrelated-clean-replay"
				in.Effects[0].Contexts = nil
				if _, err := db.Apply(in); err != nil {
					t.Fatal(err)
				}
			}
			scope := takePoolScope(t, db)
			defer scope.release()
			if _, err := scope.conn.ExecContext(scope.ctx, tc.damage); err != nil {
				t.Fatalf("damage: %v", err)
			}
			counted := &replayScanConnection{sqlConnection: scope.conn}
			scope.conn = counted
			err := runImmediateTransaction(scope.ctx, scope.conn, func() error { _, err := scope.foldOperation(in, nil); return err })
			scope.release()
			if (err != nil) != tc.reject {
				t.Fatalf("replay error = %v, reject=%v", err, tc.reject)
			}
			if !tc.reject && counted.scans != 1 {
				t.Fatalf("fallback scans=%d, want 1", counted.scans)
			}
			if _, err := db.Apply(in); (err != nil) != tc.reject {
				t.Fatalf("public replay error=%v, reject=%v", err, tc.reject)
			}
		})
	}
}

func TestExactReplayMixedBatchSnapshot(t *testing.T) {
	db, original := replayVerifiedFixture(t)
	// An unrelated host table must not disable the proof (Pasture borrows this
	// physical file). It is outside the verification contract.
	scope := takePoolScope(t, db)
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "CREATE TABLE host_private (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	scope.release()
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict-%v", conflict), func(t *testing.T) {
			before := snapshotAllDurableTables(t, db)
			fresh := original
			fresh.OperationID = journal.OperationID(fmt.Sprintf("mixed-%v", conflict))
			fresh.CommandDigest = []byte(fresh.OperationID)
			last := original
			if conflict {
				last.CommandDigest = []byte("conflict")
			}
			res, err := db.ApplyBatch([]journal.OperationInput{original, fresh, last})
			if conflict {
				if err == nil || res.Committed {
					t.Fatalf("conflicting batch = %+v, %v", res, err)
				}
				if after := snapshotAllDurableTables(t, db); !reflect.DeepEqual(before, after) {
					t.Fatal("conflicting mixed batch changed durable data")
				}
			} else if err != nil || !res.Committed || !res.Records[0].Result.ShortCircuited || res.Records[1].Result.ShortCircuited || !res.Records[2].Result.ShortCircuited {
				t.Fatalf("mixed batch = %+v, %v", res, err)
			}
		})
	}
}

func TestExactReplayLedgerReadFailureFallsBack(t *testing.T) {
	db, in := replayVerifiedFixture(t)
	failed, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.Close(); err != nil {
		t.Fatal(err)
	}
	scope := takePoolScope(t, db)
	defer scope.release()
	counted := &replayScanConnection{sqlConnection: scope.conn, failLedger: failed}
	scope.conn = counted
	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error { _, err := scope.foldOperation(in, nil); return err }); err != nil {
		t.Fatal(err)
	}
	if counted.scans != 1 {
		t.Fatalf("unreadable ledger scans=%d, want original full scan", counted.scans)
	}
}

func TestExactReplayEphemeralFallsBack(t *testing.T) {
	db, err := Open(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	actor, task, authority := newFactContextEnvironment(t, db)
	in := batchDecisionInput("ephemeral-replay", actor, authority, `{}`)
	in.Effects[0].TaskID = task
	if _, err := db.Apply(in); err != nil {
		t.Fatal(err)
	}
	scope := takePoolScope(t, db)
	defer scope.release()
	counted := &replayScanConnection{sqlConnection: scope.conn}
	scope.conn = counted
	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		if scope.exactReplayFactContextsVerified() {
			t.Fatal("ephemeral store acquired a durable verification proof")
		}
		_, err := scope.foldOperation(in, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if counted.scans != 1 {
		t.Fatalf("ephemeral replay scans=%d, want original full scan", counted.scans)
	}
}

func TestExactReplayFreshFoldInvalidatesWithinBatchProof(t *testing.T) {
	db, original := replayVerifiedFixture(t)
	before := snapshotAllDurableTables(t, db)
	fresh := original
	fresh.OperationID = "fresh-before-replay"
	fresh.CommandDigest = []byte("fresh-before-replay")
	inputs := []journal.OperationInput{original, fresh, original}
	prepared := make([]preparedBatchOperation, len(inputs))
	for i, input := range inputs {
		in, canonical, digest, err := prepareApplyInput(input)
		if err != nil {
			t.Fatal(err)
		}
		prepared[i] = preparedBatchOperation{in: in, prepared: canonical, callerMutationDigest: digest}
	}
	scope := takePoolScope(t, db)
	defer scope.release()
	counted := &replayScanConnection{sqlConnection: scope.conn}
	scope.conn = counted
	rollback := errors.New("intentional rollback after observing batch boundaries")
	err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		for i, p := range prepared {
			if _, err := scope.foldPreparedOperation(p.in, p.prepared, p.callerMutationDigest, foldOptions{deferStoreGates: true}); err != nil {
				return err
			}
			want := 0
			if i == 2 {
				want = 1
			}
			if counted.scans != want {
				t.Fatalf("after batch record %d scans=%d, want %d", i, counted.scans, want)
			}
		}
		return rollback
	})
	scope.release()
	if !errors.Is(err, rollback) {
		t.Fatalf("transaction result: %v", err)
	}
	if after := snapshotAllDurableTables(t, db); !reflect.DeepEqual(before, after) {
		t.Fatal("rolled-back batch changed durable data")
	}
}
