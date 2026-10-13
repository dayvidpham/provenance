package provenance_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	p "github.com/dayvidpham/provenance"
)

// The consuming definition resolver requires both methods on one reader.
var _ interface {
	QueryEvidence(p.EvidenceQuery) (p.EvidencePage, error)
	LookupCommitted(p.OperationID) (p.CommittedResult, error)
} = (*p.TransactionEvidenceReader)(nil)

func TestTransactionEvidenceReaderCommittedLookupUsesCallerTransaction(t *testing.T) {
	t.Parallel()
	_, db := oldCorpusCopy(t) // Authentic owner-written history, private copy.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := p.ConvertIDsInTx(ctx, tx, p.IDConversionOptions{}); err != nil {
		t.Fatal(err)
	}
	reader, err := p.NewTransactionEvidenceReader(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	const operation p.OperationID = "fixture/opaque/activity"
	result, err := reader.LookupCommitted(operation)
	if err != nil || result.Kind != p.CommittedExact || len(result.ResultSlots) != 1 {
		t.Fatalf("converted same-transaction activity: %+v, %v", result, err)
	}
	slot := result.ResultSlots[0]
	if slot.ActivityID == nil || slot.Kind != p.JournalKindActivity || slot.ProducedJournalID <= result.AnchorJournalID {
		t.Fatalf("actual typed activity slot: %+v", slot)
	}
	if _, err := p.ParseActivityID(slot.ActivityID.String()); err != nil {
		t.Fatalf("lookup must see uncommitted converted identity: %v", err)
	}
	// A one-connection pool is held by tx. Any pooled fallback cannot complete.
	if db.Stats().MaxOpenConnections != 1 {
		t.Fatal("test must hold the pool's only connection")
	}
	kindRows, err := tx.QueryContext(ctx, "SELECT DISTINCT evidence_kind FROM journal_evidence ORDER BY evidence_kind")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []p.EvidenceKind
	for kindRows.Next() {
		var kind p.EvidenceKind
		if err := kindRows.Scan(&kind); err != nil {
			kindRows.Close()
			t.Fatal(err)
		}
		kinds = append(kinds, kind)
	}
	if err := errors.Join(kindRows.Err(), kindRows.Close()); err != nil {
		t.Fatal(err)
	}
	page, err := reader.QueryEvidence(p.EvidenceQuery{Kinds: kinds, Page: p.FactPageRequest{Limit: p.MaxFactPageSize}})
	if err != nil || len(page.Rows) == 0 {
		t.Fatalf("same reader evidence positive control: %+v, %v", page, err)
	}
	for _, row := range page.Rows {
		committed, err := reader.LookupCommitted(row.ProducingOperationID)
		if err != nil || committed.Kind != p.CommittedExact || committed.AnchorJournalID != row.ProducingOperationJournalID {
			t.Fatalf("evidence producer closure: %+v, %v", committed, err)
		}
	}
	if absent, err := reader.LookupCommitted("never-applied"); err != nil || !reflect.DeepEqual(absent, p.CommittedResult{Kind: p.CommittedAbsent}) {
		t.Fatalf("absent lookup: %+v, %v", absent, err)
	}
	if _, err := reader.LookupCommitted(""); err == nil {
		t.Fatal("empty operation must fail validation")
	}
	// Well-formed slot pointing to a real row of another operation must fail
	// through the same integrity validator ordinary LookupCommitted uses.
	if _, err := tx.ExecContext(ctx, "UPDATE journal_operation_result_slots SET produced_journal_id=?1 WHERE journal_id=?2", page.Rows[0].JournalID, result.AnchorJournalID); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.LookupCommitted(operation); !errors.Is(err, p.ErrResultSlotIntegrity) {
		t.Fatalf("uncommitted foreign producer: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.LookupCommitted(operation); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("reader must not outlive caller transaction: %v", err)
	}
	// The caller owns rollback; conversion and slot corruption did not persist.
	inspection, err := p.InspectIDFormat(ctx, db)
	if err != nil || inspection.Format != p.IDFormatRetired {
		t.Fatalf("rollback preserved authentic history: %+v, %v", inspection, err)
	}
}
