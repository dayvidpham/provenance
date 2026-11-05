package sqlite

import (
	"strings"
	"testing"
)

// TestFactPageSQLNeverJoinsTheAttributedView pins the fix for a real
// performance failure: journal_attributed is a VIEW over the whole journal
// table, and SQLite planned MATERIALIZE journal_attributed for every page. A
// bounded page that returned 256 rows paid a full materialization of a
// 1.8M-row journal, measured at 6.25s per page on a real store; a full replay
// needs 1,743 pages.
//
// The view is inlined instead: the journal row is joined directly and
// effective_actor_id is computed as the view computes it. A future edit that
// reaches for the view again is a silent reintroduction of that cost, so the
// statement text is checked directly.
func TestFactPageSQLNeverJoinsTheAttributedView(t *testing.T) {
	for _, kind := range []factSelectorKind{factSelectorDecision, factSelectorEvidence} {
		statement := kind.pageMatchSQL(3)
		if strings.Contains(statement, "journal_attributed") {
			t.Fatalf("page SQL for selector %d joins journal_attributed again: SQLite materializes that view over the whole journal for every page. Join journal directly and compute effective_actor_id with COALESCE", kind)
		}
		if !strings.Contains(statement, "COALESCE(ja.actor_id, ja_anchor.actor_id)") {
			t.Fatalf("page SQL for selector %d does not compute effective_actor_id as the attributed view defines it", kind)
		}
		if !strings.Contains(statement, "ja_anchor.journal_id = ja.produced_by_operation_journal_id") {
			t.Fatalf("page SQL for selector %d does not resolve the operation anchor the attributed view resolves", kind)
		}
	}
}
