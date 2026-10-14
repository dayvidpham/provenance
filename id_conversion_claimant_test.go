package provenance_test

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	provenance "github.com/dayvidpham/provenance"
	"github.com/dayvidpham/provenance/internal/journal"
)

// The R9 authentic-claim must-pass fixture. actor_namespace_claims.claimant_id is
// an opaque, equality-only claimant label — the contract's §7.1 example is the
// bare namespace — and NOT an owned identity. The first row is the exact claim
// class a real writer stores ("pasture-system" over range [0,1023] under the
// ordinal codec); the second is an arbitrary label proving the rule holds
// whatever the bytes. Neither may be parsed, converted, or refused.
type opaqueClaim struct {
	namespace, claimant string
	minOrdinal          uint64
	maxOrdinal          uint64
}

var opaqueClaimRows = []opaqueClaim{
	{namespace: "pasture-system", claimant: "pasture-system", minOrdinal: 0, maxOrdinal: 1023},
	{namespace: "provenance-fixture", claimant: "an arbitrary claimant label, not a namespace--uuid", minOrdinal: 2048, maxOrdinal: 4095},
}

func insertOpaqueClaimRows(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, claim := range opaqueClaimRows {
		min, max := journal.BigEndianUUID(claim.minOrdinal), journal.BigEndianUUID(claim.maxOrdinal)
		if _, err := db.Exec(
			`INSERT INTO actor_namespace_claims (namespace, claimant_id, range_min, range_max, codec) VALUES (?1,?2,?3,?4,?5)`,
			claim.namespace, claim.claimant, min[:], max[:], journal.OrdinalV1CodecName,
		); err != nil {
			t.Fatalf("insert authentic claim %q: %v", claim.namespace, err)
		}
	}
}

func readOpaqueClaimantLabels(t *testing.T, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) map[string]string {
	t.Helper()
	rows, err := q.QueryContext(context.Background(), "SELECT namespace, claimant_id FROM actor_namespace_claims")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	labels := map[string]string{}
	for rows.Next() {
		var namespace, claimant string
		if err := rows.Scan(&namespace, &claimant); err != nil {
			t.Fatal(err)
		}
		labels[namespace] = claimant
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return labels
}

// TestOpaqueClaimantLabelsConvertByteExact is the must-pass regression for the
// R9 amendment. Before the classification fix the conversion preflight parsed
// claimant_id as an owned identity and refused this store with
// `ID conversion preflight actor_namespace_claims.claimant_id key "pasture-system"`
// (journal.ConvertRetiredIdentity rejects a bare namespace). Since every
// old-writer archive carries the same bare label, that refusal had no documented
// recovery. The column is now OUT of the owned set and preserved byte-exact.
func TestOpaqueClaimantLabelsConvertByteExact(t *testing.T) {
	ctx := context.Background()
	_, db := oldCorpusCopy(t)
	insertOpaqueClaimRows(t, db)
	before := readOpaqueClaimantLabels(t, db)
	if len(before) != len(opaqueClaimRows) {
		t.Fatalf("fixture claims not inserted: %d", len(before))
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	report, err := provenance.ConvertIDsInTx(ctx, tx, provenance.IDConversionOptions{})
	if err != nil {
		t.Fatalf("authentic claimant label refused conversion: %v", err)
	}
	if report.AlreadyCurrent || report.ChangedFields == 0 || report.ChangedMutations == 0 {
		t.Fatalf("authentic-claim store did not convert: %+v", report)
	}
	if after := readOpaqueClaimantLabels(t, tx); !reflect.DeepEqual(before, after) {
		t.Fatalf("claimant labels changed across conversion: before=%v after=%v", before, after)
	}
	if err := provenance.VerifyIDConversionInTx(ctx, tx); err != nil {
		t.Fatalf("converted authentic-claim store failed owner verification: %v", err)
	}
}

// TestOpaqueClaimantClassificationDoesNotWeakenIdentityRefusals pins the other
// half of the rule: moving claimant_id OUT changed nothing for a true identity
// column. A malformed value in tasks.id still refuses exactly as before, with the
// unchanged identity-preflight diagnostic, and no writes reach the store.
func TestOpaqueClaimantClassificationDoesNotWeakenIdentityRefusals(t *testing.T) {
	ctx := context.Background()
	_, db := oldCorpusCopy(t)
	insertOpaqueClaimRows(t, db)
	var taskID string
	if err := db.QueryRowContext(ctx, "SELECT id FROM tasks LIMIT 1").Scan(&taskID); err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tasks SET id='malformed' WHERE id=?1", taskID); err != nil {
		t.Fatal(err)
	}
	_, err = provenance.ConvertIDsInTx(ctx, tx, provenance.IDConversionOptions{})
	if err == nil {
		t.Fatal("malformed true identity column was no longer refused")
	}
	if !strings.Contains(err.Error(), "ID conversion preflight tasks.id") || !strings.Contains(err.Error(), `"malformed"`) {
		t.Fatalf("malformed identity refusal lost its owner column/key diagnostic: %v", err)
	}
}
