package sqlite

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
)

func TestTransactionEvidenceReaderSharesProductionPageAndSeesUncommittedTruth(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	applyEvidenceFactAt(t, db, boot, actor, "tx-evidence", task, "fixture.tx.v1", []byte{1, 2}, []byte(`{"source":"test"}`), 99)
	query := journal.EvidenceQuery{Kinds: []journal.EvidenceKind{"fixture.tx.v1"}, Page: journal.FactPageRequest{Limit: 10}}
	want, err := db.QueryEvidence(query)
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Rows) != 1 || want.Rows[0].EffectiveActorID != actor {
		t.Fatalf("positive control: %+v", want)
	}
	wantResult, err := db.LookupCommitted("tx-evidence")
	if err != nil || wantResult.Kind != journal.CommittedExact || len(wantResult.ResultSlots) != 1 || wantResult.ResultSlots[0].ProducedJournalID != want.Rows[0].JournalID {
		t.Fatalf("committed slot positive control: %+v, %v", wantResult, err)
	}
	// Pool size one makes any accidental second lease fail at the context ceiling,
	// not quietly verify committed pre-conversion data through another connection.
	db.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	reader, err := NewTransactionEvidenceReader(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.QueryEvidence(query)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("same production semantics: %+v, %v; want %+v", got, err, want)
	}
	gotResult, err := reader.LookupCommitted("tx-evidence")
	if err != nil || !reflect.DeepEqual(gotResult, wantResult) {
		t.Fatalf("same production result semantics: %+v, %v; want %+v", gotResult, err, wantResult)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM journal_evidence"); err != nil {
		t.Fatal(err)
	}
	got, err = reader.QueryEvidence(query)
	if err != nil || len(got.Rows) != 0 {
		t.Fatalf("reader must see uncommitted deletion: %+v, %v", got, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM journal_operations WHERE operation_id='tx-evidence'"); err != nil {
		t.Fatal(err)
	}
	gotResult, err = reader.LookupCommitted("tx-evidence")
	if err != nil || !reflect.DeepEqual(gotResult, journal.CommittedResult{Kind: journal.CommittedAbsent}) {
		t.Fatalf("lookup must see uncommitted deletion: %+v, %v", gotResult, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	got, err = db.QueryEvidence(query)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("caller rollback retained journal truth: %+v, %v", got, err)
	}
	gotResult, err = db.LookupCommitted("tx-evidence")
	if err != nil || !reflect.DeepEqual(gotResult, wantResult) {
		t.Fatalf("caller rollback retained operation: %+v, %v", gotResult, err)
	}
}
