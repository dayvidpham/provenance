package provenance_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

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
	task, err := tracker.As(actor, boot).Create("query-public", "query fixture", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create query fixture task: %v", err)
	}
	taskContext, err := provenance.TaskContext(task.ID)
	if err != nil {
		t.Fatalf("TaskContext: %v", err)
	}
	canonicalContexts, err := provenance.CanonicalEventContexts([]provenance.EventContext{taskContext, ctx})
	if err != nil {
		t.Fatalf("CanonicalEventContexts: %v", err)
	}
	decision := publicFactApply(t, tracker, provenance.OperationInput{
		OperationID:        "public-decision",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("decision-command"),
		RecordedAt:         101,
		Effects: []provenance.Effect{{
			Sort:         provenance.EffectDecision,
			ResultSlot:   "decision",
			DecisionKind: "query.public.decision",
			TaskID:       task.ID,
			Payload:      []byte(`{"accepted":true}`),
			Contexts:     []provenance.EventContext{taskContext, ctx},
		}},
	})
	evidence := publicFactApply(t, tracker, provenance.OperationInput{
		OperationID:        "public-evidence",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("evidence-command"),
		RecordedAt:         99,
		Effects: []provenance.Effect{{
			Sort:          provenance.EffectEvidence,
			ResultSlot:    "evidence",
			EvidenceKind:  "query.public.evidence",
			TaskID:        task.ID,
			ContentDigest: []byte{1, 3, 5},
			Payload:       []byte(`{"source":"fixture"}`),
			Contexts:      []provenance.EventContext{ctx, taskContext},
		}},
	})

	expectations := publicFactExpectations{
		actor: actor,
		decision: publicDecisionExpectation{
			id: decision.FactID, recordedAt: time.Unix(0, 101).UTC(), task: task.ID,
			kind: "query.public.decision", payload: []byte(`{"accepted":true}`), contexts: canonicalContexts,
			operation: "public-decision", anchor: decision.Anchor,
		},
		evidence: publicEvidenceExpectation{
			id: evidence.FactID, recordedAt: time.Unix(0, 99).UTC(), task: task.ID,
			kind: "query.public.evidence", digest: []byte{1, 3, 5}, payload: []byte(`{"source":"fixture"}`), contexts: canonicalContexts,
			operation: "public-evidence", anchor: evidence.Anchor,
		},
	}
	assertPublicFactRows(t, tracker.Journal().Facts(), expectations)
	if err := tracker.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}

	reopened := open()
	t.Cleanup(func() { _ = reopened.Close() })
	assertPublicFactRows(t, reopened.Journal().Facts(), expectations)
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
	decision := publicFactApply(t, tracker, provenance.OperationInput{
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
	evidence := publicFactApply(t, tracker, provenance.OperationInput{
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
	canonicalContexts, err := provenance.CanonicalEventContexts([]provenance.EventContext{ctx})
	if err != nil {
		t.Fatalf("CanonicalEventContexts: %v", err)
	}
	expectations := publicFactExpectations{
		actor: actor,
		decision: publicDecisionExpectation{
			id: decision.FactID, recordedAt: time.Unix(0, 0).UTC(), task: provenance.TaskID{},
			kind: "query.borrowed.decision", payload: []byte(`{"borrowed":true}`), contexts: canonicalContexts,
			operation: "borrowed-decision", anchor: decision.Anchor,
		},
		evidence: publicEvidenceExpectation{
			id: evidence.FactID, recordedAt: time.Unix(0, 0).UTC(), task: provenance.TaskID{},
			kind: "query.borrowed.evidence", digest: []byte{8, 6, 4}, payload: []byte(`{"borrowed":true}`), contexts: canonicalContexts,
			operation: "borrowed-evidence", anchor: evidence.Anchor,
		},
	}
	assertPublicFactRows(t, facts, expectations)
	if err := db.Close(); err != nil {
		t.Fatalf("close borrowed DBOS handle: %v", err)
	}
	for name, query := range map[string]func() error{
		"QueryDecisions": func() error {
			_, err := facts.QueryDecisions(provenance.DecisionQuery{Kinds: []provenance.DecisionKind{"query.borrowed.decision"}, Page: provenance.FactPageRequest{Limit: 1}})
			return err
		},
		"QueryEvidence": func() error {
			_, err := facts.QueryEvidence(provenance.EvidenceQuery{Kinds: []provenance.EvidenceKind{"query.borrowed.evidence"}, Page: provenance.FactPageRequest{Limit: 1}})
			return err
		},
	} {
		if err := query(); err == nil {
			t.Fatalf("pre-obtained borrowed facts remained usable after the owning handle closed (%s)", name)
		} else {
			var unavailable *provenance.StoreUnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("post-shutdown %s error = %v, want StoreUnavailableError", name, err)
			}
		}
	}
}

type publicFactExpectations struct {
	actor    provenance.ActorID
	decision publicDecisionExpectation
	evidence publicEvidenceExpectation
}

type publicDecisionExpectation struct {
	id         provenance.JournalID
	recordedAt time.Time
	task       provenance.TaskID
	kind       provenance.DecisionKind
	payload    []byte
	contexts   []provenance.EventContext
	operation  provenance.OperationID
	anchor     provenance.JournalID
}

type publicEvidenceExpectation struct {
	id         provenance.JournalID
	recordedAt time.Time
	task       provenance.TaskID
	kind       provenance.EvidenceKind
	digest     []byte
	payload    []byte
	contexts   []provenance.EventContext
	operation  provenance.OperationID
	anchor     provenance.JournalID
}

func assertPublicFactRows(t *testing.T, facts provenance.FactQueryAPI, expected publicFactExpectations) {
	t.Helper()
	decisionScope := provenance.FactTaskScope{Kind: provenance.FactTaskExact, TaskID: expected.decision.task}
	if expected.decision.task == (provenance.TaskID{}) {
		decisionScope = provenance.FactTaskScope{Kind: provenance.FactTaskUnscoped}
	}
	evidenceScope := provenance.FactTaskScope{Kind: provenance.FactTaskExact, TaskID: expected.evidence.task}
	if expected.evidence.task == (provenance.TaskID{}) {
		evidenceScope = provenance.FactTaskScope{Kind: provenance.FactTaskUnscoped}
	}
	decisionPage, err := facts.QueryDecisions(provenance.DecisionQuery{
		Filter: provenance.FactFilter{TaskScope: decisionScope, EffectiveActorIDs: []provenance.ActorID{expected.actor}, OperationIDs: []provenance.OperationID{expected.decision.operation}, RequiredContexts: expected.decision.contexts},
		Kinds:  []provenance.DecisionKind{expected.decision.kind},
		Page:   provenance.FactPageRequest{Limit: 256},
	})
	if err != nil {
		t.Fatalf("public QueryDecisions: %v", err)
	}
	if len(decisionPage.Rows) != 1 {
		t.Fatalf("public decision rows = %+v, want one row", decisionPage.Rows)
	}
	decision := decisionPage.Rows[0]
	decisionTaskMatches := decision.TaskID == nil
	if expected.decision.task != (provenance.TaskID{}) {
		decisionTaskMatches = decision.TaskID != nil && *decision.TaskID == expected.decision.task
	}
	if decision.JournalID != expected.decision.id || !decision.RecordedAt.Equal(expected.decision.recordedAt) || !decisionTaskMatches || decision.DecisionKind != expected.decision.kind || !bytes.Equal(decision.Payload, expected.decision.payload) || !reflect.DeepEqual(decision.Contexts, expected.decision.contexts) || decision.EffectiveActorID != expected.actor || decision.ProducingOperationID != expected.decision.operation || decision.ProducingOperationJournalID != expected.decision.anchor {
		t.Fatalf("public decision row = %+v, want exact fixture %+v", decision, expected.decision)
	}

	evidencePage, err := facts.QueryEvidence(provenance.EvidenceQuery{
		Filter: provenance.FactFilter{TaskScope: evidenceScope, EffectiveActorIDs: []provenance.ActorID{expected.actor}, OperationIDs: []provenance.OperationID{expected.evidence.operation}, RequiredContexts: expected.evidence.contexts},
		Kinds:  []provenance.EvidenceKind{expected.evidence.kind},
		Page:   provenance.FactPageRequest{Limit: 256},
	})
	if err != nil {
		t.Fatalf("public QueryEvidence: %v", err)
	}
	if len(evidencePage.Rows) != 1 {
		t.Fatalf("public evidence rows = %+v, want one row", evidencePage.Rows)
	}
	evidence := evidencePage.Rows[0]
	evidenceTaskMatches := evidence.TaskID == nil
	if expected.evidence.task != (provenance.TaskID{}) {
		evidenceTaskMatches = evidence.TaskID != nil && *evidence.TaskID == expected.evidence.task
	}
	if evidence.JournalID != expected.evidence.id || !evidence.RecordedAt.Equal(expected.evidence.recordedAt) || !evidenceTaskMatches || evidence.EvidenceKind != expected.evidence.kind || !bytes.Equal(evidence.ContentDigest, expected.evidence.digest) || !bytes.Equal(evidence.Payload, expected.evidence.payload) || !reflect.DeepEqual(evidence.Contexts, expected.evidence.contexts) || evidence.EffectiveActorID != expected.actor || evidence.ProducingOperationID != expected.evidence.operation || evidence.ProducingOperationJournalID != expected.evidence.anchor {
		t.Fatalf("public evidence row = %+v, want exact fixture %+v", evidence, expected.evidence)
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

type publicFactResult struct {
	FactID provenance.JournalID
	Anchor provenance.JournalID
}

func publicFactApply(t *testing.T, tracker provenance.Tracker, input provenance.OperationInput) publicFactResult {
	t.Helper()
	result, err := tracker.Journal().Apply(input)
	if err != nil {
		t.Fatalf("Apply %q: %v", input.OperationID, err)
	}
	for _, slot := range result.ResultSlots {
		if slot.Slot == input.Effects[0].ResultSlot {
			return publicFactResult{FactID: slot.ProducedJournalID, Anchor: result.AnchorJournalID}
		}
	}
	t.Fatalf("Apply %q returned no %q result slot", input.OperationID, input.Effects[0].ResultSlot)
	return publicFactResult{}
}
