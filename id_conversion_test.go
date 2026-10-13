package provenance_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	provenance "github.com/dayvidpham/provenance"
)

func oldCorpusCopy(t *testing.T) (string, *sql.DB) {
	t.Helper()
	archive, err := os.ReadFile("testdata/id-format/previous-writer.db")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "corpus.db")
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return path, db
}

type corpusBlob struct {
	Digest         string
	Body           []byte
	Count, Written int64
}

func corpusInventory(t *testing.T, reader provenance.IDFormatReader) []corpusBlob {
	t.Helper()
	rows, err := reader.QueryContext(context.Background(), "SELECT digest,body,byte_count,written_at FROM lifecycle_payload_blobs ORDER BY digest")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var inventory []corpusBlob
	for rows.Next() {
		var b corpusBlob
		if err := rows.Scan(&b.Digest, &b.Body, &b.Count, &b.Written); err != nil {
			t.Fatal(err)
		}
		inventory = append(inventory, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return inventory
}

func TestAuthenticCorpusConversionRollbackThenMarkerLastReopen(t *testing.T) {
	ctx := context.Background()
	path, db := oldCorpusCopy(t)
	before := corpusInventory(t, db)
	if len(before) != 4 {
		t.Fatalf("raw anti-vacuity inventory=%d", len(before))
	}
	var opaque, commentCommand []byte
	if err := db.QueryRow("SELECT command_digest FROM journal_operations WHERE operation_id='fixture/opaque/activity'").Scan(&opaque); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT command_digest FROM journal_operations WHERE operation_id LIKE 'provenance.op.%' LIMIT 1").Scan(&commentCommand); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := provenance.ConvertIDsInTx(ctx, tx, provenance.IDConversionOptions{})
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if report.AlreadyCurrent || report.ChangedMutations != 8 || report.ChangedAllocations != 2 || report.ChangedFields == 0 {
		t.Fatalf("nonempty conversion report: %+v", report)
	}
	if err := provenance.VerifyIDConversionInTx(ctx, tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	inspection, err := provenance.InspectIDFormat(ctx, tx)
	if err != nil || inspection.Format != provenance.IDFormatRetired {
		t.Fatalf("marker published early: %+v,%v", inspection, err)
	}
	if !reflect.DeepEqual(before, corpusInventory(t, tx)) {
		t.Fatal("conversion reclaimed or rewrote raw evidence")
	}
	reader, err := provenance.NewTransactionEvidenceReader(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	kindRows, err := tx.Query("SELECT DISTINCT evidence_kind FROM journal_evidence ORDER BY evidence_kind")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []provenance.EvidenceKind
	for kindRows.Next() {
		var kind provenance.EvidenceKind
		if err := kindRows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, kind)
	}
	if err := kindRows.Err(); err != nil {
		t.Fatal(err)
	}
	kindRows.Close()
	page, err := reader.QueryEvidence(provenance.EvidenceQuery{Kinds: kinds, Page: provenance.FactPageRequest{Limit: 256}})
	if err != nil || len(page.Rows) < 2 {
		t.Fatalf("converted transaction evidence: %+v,%v", page, err)
	}
	for _, row := range page.Rows {
		if !strings.Contains(row.EffectiveActorID.String(), "my--repo/nested/") {
			t.Fatalf("transaction actor=%s", row.EffectiveActorID)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	inspection, err = provenance.InspectIDFormat(ctx, db)
	if err != nil || inspection.Format != provenance.IDFormatRetired {
		t.Fatalf("rollback did not retain retired format: %+v,%v", inspection, err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := provenance.ConvertIDsInTx(ctx, tx, provenance.IDConversionOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := provenance.PublishIDFormatInTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	var convertedOpaque, convertedComment []byte
	if err := tx.QueryRow("SELECT command_digest FROM journal_operations WHERE operation_id='fixture/opaque/activity'").Scan(&convertedOpaque); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow("SELECT command_digest FROM journal_operations WHERE operation_id LIKE 'provenance.op.%' LIMIT 1").Scan(&convertedComment); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opaque, convertedOpaque) || bytes.Equal(commentCommand, convertedComment) {
		t.Fatal("opaque command changed or Session owner's derived command did not change")
	}
	if !reflect.DeepEqual(before, corpusInventory(t, tx)) {
		t.Fatal("marker publication changed raw evidence")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	tracker, err := provenance.OpenSQLite(path, provenance.WithModelRegistry(provenance.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Journal().VerifyIntegrity(); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	report, err = provenance.ConvertIDsInTx(ctx, tx, provenance.IDConversionOptions{})
	if err != nil || !report.AlreadyCurrent || report.ChangedFields != 0 || report.ChangedMutations != 0 {
		t.Fatalf("repeat conversion: %+v,%v", report, err)
	}
	if err := provenance.VerifyIDConversionInTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticCorpusIndependentIntegrityCorruptions(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"semantic-canonical-field", `UPDATE journal_operations SET canonical_mutation=CAST(REPLACE(CAST(canonical_mutation AS TEXT),'SYNTHETIC','SYNTHETID') AS BLOB) WHERE operation_id='fixture/opaque/activity'`},
		{"mutation-digest", `UPDATE journal_operations SET mutation_digest=zeroblob(32) WHERE operation_id='fixture/opaque/activity'`},
		{"allocation-digest", `UPDATE governed_allocation_operations SET canonical_digest=zeroblob(32) WHERE operation_id='fixture/allocation'`},
		{"evidence-digest", `UPDATE journal_evidence SET content_digest=zeroblob(32) WHERE journal_id=(SELECT MIN(journal_id) FROM journal_evidence)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, db := oldCorpusCopy(t)
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := provenance.ConvertIDsInTx(context.Background(), tx, provenance.IDConversionOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := provenance.VerifyIDConversionInTx(context.Background(), tx); err != nil {
				t.Fatalf("valid converted positive control: %v", err)
			}
			if err := provenance.PublishIDFormatInTx(context.Background(), tx); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			tx, err = db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			tracker, openErr := provenance.OpenSQLite(path, provenance.WithModelRegistry(provenance.NewRegistry(nil)))
			if tracker != nil {
				if err := tracker.Close(); err != nil {
					t.Fatal(err)
				}
			} else if openErr == nil {
				t.Fatal("opener returned neither tracker nor diagnostic")
			}
			reopened, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=foreign_keys(1)")
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			tx, err = reopened.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := provenance.VerifyIDConversionInTx(context.Background(), tx); err == nil {
				t.Fatal("real owner verification accepted independent corruption")
			}
			if err := provenance.PublishIDFormatInTx(context.Background(), tx); err == nil {
				t.Fatal("corrupt conversion published marker")
			}
		})
	}
}

func TestRetiredCorpusPreflightRefusesCollisionMalformedAndCorruptEvidence(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"collision", `INSERT INTO tasks SELECT namespace||'--'||UPPER(SUBSTR(id,LENGTH(namespace)+3)),namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM tasks LIMIT 1`},
		{"malformed", `UPDATE journal SET actor_id='malformed' WHERE journal_id=1`},
		{"subtype-payload", `UPDATE journal_evidence SET payload='{}' WHERE journal_id=(SELECT MIN(journal_id) FROM journal_evidence)`},
		{"evidence-digest", `UPDATE journal_evidence SET content_digest=zeroblob(32) WHERE journal_id=(SELECT MIN(journal_id) FROM journal_evidence)`},
		{"opaque-edge", `INSERT INTO edges(source_id,target_id,kind_id,created_at) SELECT id,'opaque-external-handle',1,1 FROM tasks LIMIT 1`},
		{"partially-converted", `UPDATE tasks SET id=namespace||'/'||SUBSTR(id,LENGTH(namespace)+3) WHERE id=(SELECT MIN(id) FROM tasks)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, db := oldCorpusCopy(t)
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec("PRAGMA defer_foreign_keys=ON"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			var before int
			if err := tx.QueryRow("SELECT COUNT(*) FROM tasks WHERE INSTR(id,'--')>0").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := provenance.ConvertIDsInTx(context.Background(), tx, provenance.IDConversionOptions{}); err == nil {
				t.Fatal("preflight accepted independent retired corruption")
			} else if tc.name == "opaque-edge" && (!strings.Contains(err.Error(), "edges.target_id") || !strings.Contains(err.Error(), "opaque-external-handle")) {
				t.Fatalf("unsupported opaque edge lacks owner column/key diagnostic: %v", err)
			}
			var after int
			if err := tx.QueryRow("SELECT COUNT(*) FROM tasks WHERE INSTR(id,'--')>0").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("preflight failure changed task keys before writes")
			}
			format, err := provenance.InspectIDFormat(context.Background(), tx)
			if err != nil || format.Format != provenance.IDFormatRetired {
				t.Fatalf("failed preflight published marker: %+v,%v", format, err)
			}
		})
	}
}
