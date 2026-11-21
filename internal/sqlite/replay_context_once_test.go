package sqlite

import (
	"errors"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
)

func TestReplayVerifiedFactContextsChecksOnce(t *testing.T) {
	db, _ := replayVerifiedFixture(t)
	scope := takePoolScope(t, db)
	defer scope.release()
	if err := scope.verifyIntegrity(); err != nil {
		t.Fatal(err)
	}
	visited := scope.factContextOperationsVisited
	if visited == 0 {
		t.Fatal("integrity verification did not visit canonical fact contexts")
	}
	if _, err := scope.replayProjectionsVerifiedFactContexts(false); err != nil {
		t.Fatal(err)
	}
	if got := scope.factContextOperationsVisited; got != visited {
		t.Fatalf("verified replay visited fact contexts again: before=%d after=%d", visited, got)
	}
	if _, err := scope.replayProjections(); err != nil {
		t.Fatal(err)
	}
	if got := scope.factContextOperationsVisited; got <= visited {
		t.Fatalf("unverified replay skipped the fact-context check: before=%d after=%d", visited, got)
	}
}

func TestPublicReplayRejectsFactContextDamage(t *testing.T) {
	db, _ := replayVerifiedFixture(t)
	scope := takePoolScope(t, db)
	_, err := scope.conn.ExecContext(scope.ctx, "DELETE FROM journal_decision_contexts")
	scope.release()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReplayProjections(); !errors.Is(err, journal.ErrFactContextIntegrity) {
		t.Fatalf("ReplayProjections error = %v, want ErrFactContextIntegrity", err)
	}
}
