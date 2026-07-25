package provenance_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	provenance "github.com/dayvidpham/provenance"
)

func TestJournalFactsPublicFileBackedDecisionEvidenceSurviveReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "journal-facts.db")
	open := func() provenance.Tracker {
		tracker, err := provenance.OpenSQLite(path)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		return tracker
	}

	tracker := open()
	agent, err := tracker.RegisterSoftwareAgent("query-public", "actor", "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	actor := agent.Agent.ID
	ctx, err := provenance.ActorContext(actor)
	if err != nil {
		t.Fatalf("ActorContext: %v", err)
	}
	boot := publicFactGenesis(t, tracker, actor, "public-genesis")
	decisionID := publicFactApply(t, tracker, provenance.OperationInput{
		OperationID:        "public-decision",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("decision-command"),
		RecordedAt:         101,
		Effects: []provenance.Effect{{
			Sort:         provenance.EffectDecision,
			ResultSlot:   "decision",
			DecisionKind: "query.public.decision",
			Payload:      []byte(`{"accepted":true}`),
			Contexts:     []provenance.EventContext{ctx},
		}},
	})
	evidenceID := publicFactApply(t, tracker, provenance.OperationInput{
		OperationID:        "public-evidence",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("evidence-command"),
		RecordedAt:         99,
		Effects: []provenance.Effect{{
			Sort:          provenance.EffectEvidence,
			ResultSlot:    "evidence",
			EvidenceKind:  "query.public.evidence",
			ContentDigest: []byte{1, 3, 5},
			Payload:       []byte(`{"source":"fixture"}`),
			Contexts:      []provenance.EventContext{ctx},
		}},
	})

	assertPublicFactRows(t, tracker.Journal().Facts(), actor, ctx, decisionID, evidenceID)
	if err := tracker.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}

	reopened := open()
	t.Cleanup(func() { _ = reopened.Close() })
	assertPublicFactRows(t, reopened.Journal().Facts(), actor, ctx, decisionID, evidenceID)
}

func TestJournalFactsPublicBorrowedFileBackedLiveness(t *testing.T) {
	t.Parallel()
	db, _ := openFileDB(t)
	tracker, err := provenance.OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("OpenBorrowedSQLite: %v", err)
	}
	t.Cleanup(func() { _ = tracker.Close() })
	agent, err := tracker.RegisterSoftwareAgent("query-borrowed", "actor", "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	actor := agent.Agent.ID
	ctx, err := provenance.ActorContext(actor)
	if err != nil {
		t.Fatalf("ActorContext: %v", err)
	}
	boot := publicFactGenesis(t, tracker, actor, "borrowed-genesis")
	decisionID := publicFactApply(t, tracker, provenance.OperationInput{
		OperationID:        "borrowed-decision",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("borrowed-decision-command"),
		Effects: []provenance.Effect{{
			Sort:         provenance.EffectDecision,
			ResultSlot:   "decision",
			DecisionKind: "query.borrowed.decision",
			Payload:      []byte(`{"borrowed":true}`),
			Contexts:     []provenance.EventContext{ctx},
		}},
	})
	evidenceID := publicFactApply(t, tracker, provenance.OperationInput{
		OperationID:        "borrowed-evidence",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("borrowed-evidence-command"),
		Effects: []provenance.Effect{{
			Sort:          provenance.EffectEvidence,
			ResultSlot:    "evidence",
			EvidenceKind:  "query.borrowed.evidence",
			ContentDigest: []byte{8, 6, 4},
			Payload:       []byte(`{"borrowed":true}`),
			Contexts:      []provenance.EventContext{ctx},
		}},
	})

	facts := tracker.Journal().Facts()
	assertPublicFactRows(t, facts, actor, ctx, decisionID, evidenceID)
	if err := db.Close(); err != nil {
		t.Fatalf("close borrowed DBOS handle: %v", err)
	}
	if _, err := facts.QueryDecisions(provenance.DecisionQuery{
		Kinds: []provenance.DecisionKind{"query.borrowed.decision"},
		Page:  provenance.FactPageRequest{Limit: 1},
	}); err == nil {
		t.Fatal("pre-obtained borrowed facts remained usable after the owning handle closed")
	} else {
		var unavailable *provenance.StoreUnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("post-shutdown QueryDecisions error = %v, want StoreUnavailableError", err)
		}
	}
}

func assertPublicFactRows(t *testing.T, facts provenance.FactQueryAPI, actor provenance.ActorID, ctx provenance.EventContext, decisionID, evidenceID provenance.JournalID) {
	t.Helper()
	decisionPage, err := facts.QueryDecisions(provenance.DecisionQuery{
		Filter: provenance.FactFilter{RequiredContexts: []provenance.EventContext{ctx}},
		Kinds:  []provenance.DecisionKind{"query.public.decision", "query.borrowed.decision"},
		Page:   provenance.FactPageRequest{Limit: 256},
	})
	if err != nil {
		t.Fatalf("public QueryDecisions: %v", err)
	}
	if len(decisionPage.Rows) != 1 {
		t.Fatalf("public decision rows = %+v, want one row", decisionPage.Rows)
	}
	decision := decisionPage.Rows[0]
	if decision.JournalID != decisionID || decision.EffectiveActorID != actor || decision.ProducingOperationID == "" || decision.ProducingOperationJournalID <= 0 || len(decision.Payload) == 0 || !reflect.DeepEqual(decision.Contexts, []provenance.EventContext{ctx}) {
		t.Fatalf("public decision row = %+v, want id=%d actor=%s exact context", decision, decisionID, actor)
	}

	evidencePage, err := facts.QueryEvidence(provenance.EvidenceQuery{
		Filter: provenance.FactFilter{RequiredContexts: []provenance.EventContext{ctx}},
		Kinds:  []provenance.EvidenceKind{"query.public.evidence", "query.borrowed.evidence"},
		Page:   provenance.FactPageRequest{Limit: 256},
	})
	if err != nil {
		t.Fatalf("public QueryEvidence: %v", err)
	}
	if len(evidencePage.Rows) != 1 {
		t.Fatalf("public evidence rows = %+v, want one row", evidencePage.Rows)
	}
	evidence := evidencePage.Rows[0]
	if evidence.JournalID != evidenceID || evidence.EffectiveActorID != actor || evidence.ProducingOperationID == "" || evidence.ProducingOperationJournalID <= 0 || len(evidence.ContentDigest) == 0 || len(evidence.Payload) == 0 || !reflect.DeepEqual(evidence.Contexts, []provenance.EventContext{ctx}) {
		t.Fatalf("public evidence row = %+v, want id=%d actor=%s exact context", evidence, evidenceID, actor)
	}
}

func publicFactGenesis(t *testing.T, tracker provenance.Tracker, actor provenance.ActorID, operation provenance.OperationID) provenance.JournalID {
	t.Helper()
	result, err := tracker.Journal().Apply(provenance.OperationInput{
		OperationID:    operation,
		ActorID:        actor,
		CommandDigest:  []byte(operation + "-command"),
		MutationDigest: []byte(operation + "-mutation"),
		Effects:        []provenance.Effect{{Sort: provenance.EffectBootstrapAuthority, ResultSlot: "auth", BootstrapLabel: "query-test"}},
	})
	if err != nil {
		t.Fatalf("genesis %q: %v", operation, err)
	}
	for _, slot := range result.ResultSlots {
		if slot.Slot == "auth" {
			return slot.ProducedJournalID
		}
	}
	t.Fatalf("genesis %q returned no authority slot", operation)
	return 0
}

func publicFactApply(t *testing.T, tracker provenance.Tracker, input provenance.OperationInput) provenance.JournalID {
	t.Helper()
	result, err := tracker.Journal().Apply(input)
	if err != nil {
		t.Fatalf("Apply %q: %v", input.OperationID, err)
	}
	for _, slot := range result.ResultSlots {
		if slot.Slot == input.Effects[0].ResultSlot {
			return slot.ProducedJournalID
		}
	}
	t.Fatalf("Apply %q returned no %q result slot", input.OperationID, input.Effects[0].ResultSlot)
	return 0
}
