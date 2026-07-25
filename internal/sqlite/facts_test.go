package sqlite

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestFactQueryReturnsExactDecisionAndEvidenceRows(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)

	decisionOperation := journal.OperationID("facts-decision-1")
	decisionID := applyDecisionFact(t, db, boot, actor, decisionOperation, task, "fixture.decision.v1", []byte(`{"accepted":true}`))
	evidenceOperation := journal.OperationID("facts-evidence-1")
	digest := []byte{1, 2, 3, 4}
	evidenceID := applyEvidenceFact(t, db, boot, actor, evidenceOperation, task, "fixture.evidence.v1", digest, []byte(`{"source":"test"}`))

	api := db.Facts()
	decisions, err := api.QueryDecisions(journal.DecisionQuery{
		Filter: journal.FactFilter{
			TaskScope:         journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: task},
			EffectiveActorIDs: []journal.ActorID{actor},
			OperationIDs:      []journal.OperationID{decisionOperation},
		},
		Kinds: []journal.DecisionKind{"fixture.decision.v1"},
		Page:  journal.FactPageRequest{Limit: 10},
	})
	if err != nil {
		t.Fatalf("QueryDecisions: %v", err)
	}
	if len(decisions.Rows) != 1 || decisions.Rows[0].JournalID != decisionID {
		t.Fatalf("decision rows = %+v, want one row %d", decisions.Rows, decisionID)
	}
	decision := decisions.Rows[0]
	if decision.TaskID == nil || *decision.TaskID != task {
		t.Fatalf("decision task = %v, want %s", decision.TaskID, task)
	}
	if decision.EffectiveActorID != actor || decision.ProducingOperationID != decisionOperation {
		t.Fatalf("decision attribution = actor %s operation %q, want actor %s operation %q", decision.EffectiveActorID, decision.ProducingOperationID, actor, decisionOperation)
	}
	if decision.ProducingOperationJournalID <= 0 || decision.Payload == nil || !bytes.Equal(decision.Payload, []byte(`{"accepted":true}`)) {
		t.Fatalf("decision metadata = %+v", decision)
	}

	evidence, err := api.QueryEvidence(journal.EvidenceQuery{
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: task}},
		Kinds:  []journal.EvidenceKind{"fixture.evidence.v1"},
		Page:   journal.FactPageRequest{Limit: 10},
	})
	if err != nil {
		t.Fatalf("QueryEvidence: %v", err)
	}
	if len(evidence.Rows) != 1 || evidence.Rows[0].JournalID != evidenceID {
		t.Fatalf("evidence rows = %+v, want one row %d", evidence.Rows, evidenceID)
	}
	evidenceRow := evidence.Rows[0]
	if evidenceRow.TaskID == nil || *evidenceRow.TaskID != task || evidenceRow.EffectiveActorID != actor {
		t.Fatalf("evidence attribution = %+v", evidenceRow)
	}
	if evidenceRow.ProducingOperationID != evidenceOperation || !bytes.Equal(evidenceRow.ContentDigest, digest) || !bytes.Equal(evidenceRow.Payload, []byte(`{"source":"test"}`)) {
		t.Fatalf("evidence metadata = %+v", evidenceRow)
	}
}

func TestFactQueryPaginationPinsSnapshotAndCursor(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	for i := 0; i < 3; i++ {
		applyDecisionFact(t, db, boot, actor, journal.OperationID("facts-page-"+string(rune('a'+i))), task, "fixture.page.v1", []byte(`{"page":1}`))
	}

	first, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Kinds: []journal.DecisionKind{"fixture.page.v1"},
		Page:  journal.FactPageRequest{Limit: 1},
	})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Rows) != 1 || first.Next == nil {
		t.Fatalf("first page = %+v, want one row and next cursor", first)
	}
	oldSnapshot := first.SnapshotMaxJournalID
	oldCursor := *first.Next

	newID := applyDecisionFact(t, db, boot, actor, "facts-page-new", task, "fixture.page.v1", []byte(`{"page":1}`))
	second, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Kinds: []journal.DecisionKind{"fixture.page.v1"},
		Page:  journal.FactPageRequest{Limit: 2, SnapshotMaxJournalID: oldCursor.SnapshotMaxJournalID, AfterJournalID: oldCursor.AfterJournalID},
	})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if second.SnapshotMaxJournalID != oldSnapshot {
		t.Fatalf("second snapshot = %d, want %d", second.SnapshotMaxJournalID, oldSnapshot)
	}
	for _, row := range second.Rows {
		if row.JournalID == newID {
			t.Fatalf("later decision %d leaked into pinned snapshot page", newID)
		}
	}
	if len(second.Rows) != 2 || second.Next != nil {
		t.Fatalf("second page = %+v, want final two pre-snapshot rows", second)
	}

	fresh, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Kinds: []journal.DecisionKind{"fixture.page.v1"},
		Page:  journal.FactPageRequest{Limit: 10},
	})
	if err != nil {
		t.Fatalf("fresh page: %v", err)
	}
	if len(fresh.Rows) != 4 {
		t.Fatalf("fresh rows = %d, want 4 including later decision", len(fresh.Rows))
	}
}

func TestFactQuerySurvivesCloseAndReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "facts.db")
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	actor := seedFactActor(t, db)
	boot := genesisBoot(t, db, actor)
	task := createFactTask(t, db, boot, actor)
	operation := journal.OperationID("facts-reopen-1")
	decisionID := applyDecisionFact(t, db, boot, actor, operation, task, "fixture.reopen.v1", []byte(`{"reopen":true}`))
	before, err := db.Facts().QueryDecisions(journal.DecisionQuery{Kinds: []journal.DecisionKind{"fixture.reopen.v1"}, Page: journal.FactPageRequest{Limit: 10}})
	if err != nil {
		t.Fatalf("query before reopen: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err = Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	after, err := db.Facts().QueryDecisions(journal.DecisionQuery{Kinds: []journal.DecisionKind{"fixture.reopen.v1"}, Page: journal.FactPageRequest{Limit: 10}})
	if err != nil {
		t.Fatalf("query after reopen: %v", err)
	}
	if len(before.Rows) != 1 || len(after.Rows) != 1 || after.Rows[0].JournalID != decisionID || after.Rows[0].ProducingOperationID != operation {
		t.Fatalf("reopen rows before=%+v after=%+v", before.Rows, after.Rows)
	}
}

func TestFactQueryRejectsInvalidBoundsBeforeOpeningAConnection(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	_, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Kinds: []journal.DecisionKind{"fixture.invalid.v1"},
		Page:  journal.FactPageRequest{Limit: 0},
	})
	if err == nil || !errors.Is(err, journal.ErrInvalidQuery) {
		t.Fatalf("invalid limit error = %v, want ErrInvalidQuery", err)
	}
}

func TestFactQueryContextFilterStaysScopedToFactRows(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	context, err := journal.TaskContext(task)
	if err != nil {
		t.Fatalf("TaskContext: %v", err)
	}
	if _, err := db.Apply(journal.OperationInput{
		OperationID:        "facts-context-event",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("facts-context-event-command"),
		MutationDigest:     []byte("facts-context-event-mutation"),
		Effects: []journal.Effect{{
			Sort:      journal.EffectTaskEvent,
			TaskID:    task,
			EventKind: "fixture.context.event",
			Contexts:  []journal.EventContext{context},
		}},
	}); err != nil {
		t.Fatalf("Apply context event: %v", err)
	}
	applyDecisionFact(t, db, boot, actor, "facts-context-decision", journal.TaskID{}, "fixture.context.decision.v1", []byte(`{"context":false}`))

	page, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Filter: journal.FactFilter{RequiredContexts: []journal.EventContext{context}},
		Kinds:  []journal.DecisionKind{"fixture.context.decision.v1"},
		Page:   journal.FactPageRequest{Limit: 10},
	})
	if err != nil {
		t.Fatalf("QueryDecisions with context filter: %v", err)
	}
	if len(page.Rows) != 0 {
		t.Fatalf("context filter matched a task event's context: %+v", page.Rows)
	}
}

func applyDecisionFact(t *testing.T, db *DB, boot journal.JournalID, actor journal.ActorID, operation journal.OperationID, task journal.TaskID, kind journal.DecisionKind, payload []byte) journal.JournalID {
	t.Helper()
	effect := journal.Effect{Sort: journal.EffectDecision, ResultSlot: "decision", DecisionKind: kind, Payload: payload}
	if task != (journal.TaskID{}) {
		effect.TaskID = task
	}
	result, err := db.Apply(journal.OperationInput{
		OperationID:        operation,
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("command-" + string(operation)),
		MutationDigest:     []byte("mutation-" + string(operation)),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects:            []journal.Effect{effect},
	})
	if err != nil {
		t.Fatalf("Apply decision %q: %v", operation, err)
	}
	for _, slot := range result.ResultSlots {
		if slot.Slot == "decision" {
			return slot.ProducedJournalID
		}
	}
	t.Fatalf("Apply decision %q returned no decision result slot", operation)
	return 0
}

func seedFactActor(t *testing.T, db *DB) journal.ActorID {
	t.Helper()
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bind actor seed scope: %v", err)
	}
	err = sqlitex.Execute(scope.conn, "INSERT INTO agents (id, kind_id) VALUES (?1, ?2)", &sqlitex.ExecOptions{Args: []any{actor.String(), int(ptypes.AgentKindSoftware)}})
	if err == nil {
		err = sqlitex.Execute(scope.conn, "INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1, ?2, ?3, ?4)", &sqlitex.ExecOptions{Args: []any{actor.String(), "facts-test", "0", "test"}})
	}
	scope.release()
	if err != nil {
		t.Fatalf("seed actor: %v", err)
	}
	return actor
}

func createFactTask(t *testing.T, db *DB, boot journal.JournalID, actor journal.ActorID) journal.TaskID {
	t.Helper()
	task := ptypes.TaskID{Namespace: "provenance-test", UUID: uuid.New()}
	if _, err := db.Apply(journal.OperationInput{
		OperationID:        "facts-task-create",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("facts-task-command"),
		MutationDigest:     []byte("facts-task-mutation"),
		Effects: []journal.Effect{{
			Sort:     journal.EffectTaskCreate,
			TaskID:   task,
			Title:    "facts test task",
			Type:     ptypes.TaskTypeTask,
			Priority: ptypes.PriorityMedium,
			Phase:    ptypes.PhaseUnscoped,
		}},
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

func applyEvidenceFact(t *testing.T, db *DB, boot journal.JournalID, actor journal.ActorID, operation journal.OperationID, task journal.TaskID, kind journal.EvidenceKind, digest, payload []byte) journal.JournalID {
	t.Helper()
	effect := journal.Effect{Sort: journal.EffectEvidence, ResultSlot: "evidence", EvidenceKind: kind, ContentDigest: digest, Payload: payload}
	if task != (journal.TaskID{}) {
		effect.TaskID = task
	}
	result, err := db.Apply(journal.OperationInput{
		OperationID:        operation,
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("command-" + string(operation)),
		MutationDigest:     []byte("mutation-" + string(operation)),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects:            []journal.Effect{effect},
	})
	if err != nil {
		t.Fatalf("Apply evidence %q: %v", operation, err)
	}
	for _, slot := range result.ResultSlots {
		if slot.Slot == "evidence" {
			return slot.ProducedJournalID
		}
	}
	t.Fatalf("Apply evidence %q returned no evidence result slot", operation)
	return 0
}
