package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

func TestFactQueryReturnsExactDecisionAndEvidenceRows(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	actorContext, err := journal.ActorContext(actor)
	if err != nil {
		t.Fatalf("ActorContext: %v", err)
	}

	decisionOperation := journal.OperationID("facts-decision-1")
	decisionID := applyDecisionFactAt(t, db, boot, actor, decisionOperation, task, "fixture.decision.v1", []byte(`{"accepted":true}`), 101, actorContext)
	evidenceOperation := journal.OperationID("facts-evidence-1")
	digest := []byte{1, 2, 3, 4}
	evidenceID := applyEvidenceFactAt(t, db, boot, actor, evidenceOperation, task, "fixture.evidence.v1", digest, []byte(`{"source":"test"}`), 99, actorContext)

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
	decisionAnchor := operationAnchor(t, db, decisionOperation)
	if !decision.RecordedAt.Equal(time.Unix(0, 101).UTC()) || decision.DecisionKind != "fixture.decision.v1" || decision.EffectiveActorID != actor || decision.ProducingOperationID != decisionOperation || decision.ProducingOperationJournalID != decisionAnchor || !reflect.DeepEqual(decision.Contexts, []journal.EventContext{actorContext}) {
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
	evidenceAnchor := operationAnchor(t, db, evidenceOperation)
	if !evidenceRow.RecordedAt.Equal(time.Unix(0, 99).UTC()) || evidenceRow.EvidenceKind != "fixture.evidence.v1" || evidenceRow.TaskID == nil || *evidenceRow.TaskID != task || evidenceRow.EffectiveActorID != actor || evidenceRow.ProducingOperationJournalID != evidenceAnchor || !reflect.DeepEqual(evidenceRow.Contexts, []journal.EventContext{actorContext}) {
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

func TestFactQueryReturnsCanonicalContextsAndUsesRequiredSubset(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	first, err := journal.ActorContext(actor)
	if err != nil {
		t.Fatalf("ActorContext: %v", err)
	}
	secondActor := seedFactActor(t, db)
	second, err := journal.ActorContext(secondActor)
	if err != nil {
		t.Fatalf("second ActorContext: %v", err)
	}
	taskContext, err := journal.TaskContext(task)
	if err != nil {
		t.Fatalf("TaskContext: %v", err)
	}
	decisionID := applyDecisionFact(t, db, boot, actor, "facts-context-decision", task, "fixture.context.decision.v2", []byte(`{"context":true}`), second, first)
	evidenceID := applyEvidenceFact(t, db, boot, actor, "facts-context-evidence", task, "fixture.context.evidence.v2", []byte{9, 8, 7}, []byte(`{"context":true}`), first, second)

	page, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Filter: journal.FactFilter{RequiredContexts: []journal.EventContext{first}},
		Kinds:  []journal.DecisionKind{"fixture.context.decision.v2"},
		Page:   journal.FactPageRequest{Limit: 1},
	})
	if err != nil {
		t.Fatalf("QueryDecisions required subset: %v", err)
	}
	if len(page.Rows) != 1 || page.Rows[0].JournalID != decisionID || len(page.Rows[0].Contexts) != 2 {
		t.Fatalf("decision page = %+v, want decision %d with both canonical contexts", page.Rows, decisionID)
	}
	if page.Rows[0].Contexts[0] != first && page.Rows[0].Contexts[1] != first {
		t.Fatalf("decision contexts = %+v, want %v and %v", page.Rows[0].Contexts, first, second)
	}
	exact, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Filter: journal.FactFilter{RequiredContexts: []journal.EventContext{first, second}},
		Kinds:  []journal.DecisionKind{"fixture.context.decision.v2"},
		Page:   journal.FactPageRequest{Limit: 1},
	})
	if err != nil || len(exact.Rows) != 1 {
		t.Fatalf("exact required contexts = rows=%d err=%v, want one row", len(exact.Rows), err)
	}
	for name, required := range map[string][]journal.EventContext{
		"missing":    {taskContext},
		"partial":    {first, taskContext},
		"wrong-kind": {taskContext},
	} {
		filtered, filterErr := db.Facts().QueryDecisions(journal.DecisionQuery{
			Filter: journal.FactFilter{RequiredContexts: required},
			Kinds:  []journal.DecisionKind{"fixture.context.decision.v2"},
			Page:   journal.FactPageRequest{Limit: 1},
		})
		if filterErr != nil {
			t.Fatalf("%s required contexts: %v", name, filterErr)
		}
		if len(filtered.Rows) != 0 {
			t.Fatalf("%s required contexts matched rows: %+v", name, filtered.Rows)
		}
	}

	missing, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Filter: journal.FactFilter{RequiredContexts: []journal.EventContext{{}}},
		Kinds:  []journal.DecisionKind{"fixture.context.decision.v2"},
		Page:   journal.FactPageRequest{Limit: 1},
	})
	if err == nil || !errors.Is(err, journal.ErrInvalidQuery) {
		t.Fatalf("malformed required context error = %v, want ErrInvalidQuery before SQL", err)
	}
	if len(missing.Rows) != 0 {
		t.Fatalf("malformed context returned rows: %+v", missing.Rows)
	}

	evidence, err := db.Facts().QueryEvidence(journal.EvidenceQuery{
		Filter: journal.FactFilter{RequiredContexts: []journal.EventContext{second}},
		Kinds:  []journal.EvidenceKind{"fixture.context.evidence.v2"},
		Page:   journal.FactPageRequest{Limit: 1},
	})
	if err != nil {
		t.Fatalf("QueryEvidence required subset: %v", err)
	}
	if len(evidence.Rows) != 1 || evidence.Rows[0].JournalID != evidenceID || len(evidence.Rows[0].Contexts) != 2 {
		t.Fatalf("evidence page = %+v, want evidence %d with both canonical contexts", evidence.Rows, evidenceID)
	}
}

func TestFactQueryRejectsForgedSnapshotWatermark(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	applyDecisionFact(t, db, boot, actor, "facts-forged-snapshot", task, "fixture.snapshot.v2", []byte(`{"snapshot":true}`))
	decisionPage, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Kinds: []journal.DecisionKind{"fixture.snapshot.v2"},
		Page:  journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: 1 << 20},
	})
	if err == nil || len(decisionPage.Rows) != 0 || !errors.Is(err, journal.ErrInvalidQuery) {
		t.Fatalf("forged snapshot error = %v, want ErrInvalidQuery", err)
	}
	applyEvidenceFact(t, db, boot, actor, "facts-forged-snapshot-evidence", task, "fixture.snapshot.evidence.v2", []byte{1}, []byte(`{"snapshot":true}`))
	evidencePage, err := db.Facts().QueryEvidence(journal.EvidenceQuery{
		Kinds: []journal.EvidenceKind{"fixture.snapshot.evidence.v2"},
		Page:  journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: 1 << 20},
	})
	if err == nil || len(evidencePage.Rows) != 0 || !errors.Is(err, journal.ErrInvalidQuery) {
		t.Fatalf("forged evidence snapshot error = %v, want ErrInvalidQuery", err)
	}
}

func TestFactQueriesRejectMalformedInputBeforeConnectionLease(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("Close unavailable query fixture: %v", err)
	}
	kinds := make([]journal.DecisionKind, journal.MaxFactQueryKinds+1)
	for i := range kinds {
		kinds[i] = journal.DecisionKind(fmt.Sprintf("fixture.invalid.%d", i))
	}
	validActors := make([]journal.ActorID, journal.MaxFactFilterValues+1)
	validContexts := make([]journal.EventContext, journal.MaxCanonicalContextsPerEffect+1)
	validOperations := make([]journal.OperationID, journal.MaxFactFilterValues+1)
	for i := range validActors {
		validActors[i] = ptypes.ActorID{Namespace: "provenance-query", UUID: uuid.New()}
		validContexts[i], _ = journal.ActorContext(validActors[i])
		validOperations[i] = journal.OperationID(fmt.Sprintf("fixture.query.operation.%d", i))
	}
	queries := []journal.DecisionQuery{
		{Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: -1}},
		{Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 0}},
		{Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: journal.MaxFactPageSize + 1}},
		{Kinds: kinds, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskScopeKind(99)}}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact}}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: ptypes.TaskID{Namespace: "malformed", UUID: uuid.Nil}}}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{EffectiveActorIDs: []journal.ActorID{{}}}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{EffectiveActorIDs: validActors}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{OperationIDs: []journal.OperationID{""}}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{OperationIDs: validOperations}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{RequiredContexts: []journal.EventContext{{}}}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{RequiredContexts: validContexts}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Kinds: []journal.DecisionKind{"not a valid event kind"}, Page: journal.FactPageRequest{Limit: 1}},
		{Kinds: []journal.DecisionKind{""}, Page: journal.FactPageRequest{Limit: 1}},
		{Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: -1}},
		{Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1, AfterJournalID: -1}},
		{Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: 0, AfterJournalID: 1}},
		{Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: 10, AfterJournalID: 11}},
	}
	for i, query := range queries {
		if _, err := db.QueryDecisions(query); err == nil || !errors.Is(err, journal.ErrInvalidQuery) {
			t.Errorf("decision malformed case %d error = %v, want ErrInvalidQuery before lease", i, err)
		}
		evidenceKinds := make([]journal.EvidenceKind, len(query.Kinds))
		for j := range evidenceKinds {
			if len(query.Kinds) > journal.MaxFactQueryKinds {
				evidenceKinds[j] = journal.EvidenceKind(fmt.Sprintf("fixture.invalid.evidence.%d", j))
			} else {
				evidenceKinds[j] = journal.EvidenceKind(query.Kinds[j])
			}
		}
		if _, err := db.QueryEvidence(journal.EvidenceQuery{Filter: query.Filter, Kinds: evidenceKinds, Page: query.Page}); err == nil || !errors.Is(err, journal.ErrInvalidQuery) {
			t.Errorf("evidence malformed case %d error = %v, want ErrInvalidQuery before lease", i, err)
		}
	}
}

func TestFactQueryScopesAndDimensionsUseOrWithinAndAcross(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	otherActor := seedFactActor(t, db)
	boot := genesisBoot(t, db, actor)
	first := applyDecisionFact(t, db, boot, actor, "facts-filter-first", task, "fixture.filter.first", []byte(`{"row":"first"}`))
	second := applyDecisionFact(t, db, boot, actor, "facts-filter-second", journal.TaskID{}, "fixture.filter.second", []byte(`{"row":"second"}`))
	third := applyDecisionFact(t, db, boot, otherActor, "facts-filter-third", task, "fixture.filter.third", []byte(`{"row":"third"}`))

	anyScope, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Filter: journal.FactFilter{
			TaskScope:         journal.FactTaskScope{Kind: journal.FactTaskAny},
			EffectiveActorIDs: []journal.ActorID{actor, otherActor},
			OperationIDs:      []journal.OperationID{"facts-filter-first", "facts-filter-second", "facts-filter-third"},
		},
		Kinds: []journal.DecisionKind{"fixture.filter.first", "fixture.filter.second", "fixture.filter.third"},
		Page:  journal.FactPageRequest{Limit: 256},
	})
	if err != nil {
		t.Fatalf("Any scope OR dimensions: %v", err)
	}
	if got := factIDs(anyScope.Rows); !reflect.DeepEqual(got, []journal.JournalID{first, second, third}) {
		t.Fatalf("Any scope IDs = %v, want [%d %d %d]", got, first, second, third)
	}

	exactScope, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Filter: journal.FactFilter{
			TaskScope:         journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: task},
			EffectiveActorIDs: []journal.ActorID{actor},
			OperationIDs:      []journal.OperationID{"facts-filter-first", "facts-filter-second", "facts-filter-third"},
		},
		Kinds: []journal.DecisionKind{"fixture.filter.first", "fixture.filter.second", "fixture.filter.third"},
		Page:  journal.FactPageRequest{Limit: 256},
	})
	if err != nil {
		t.Fatalf("Exact scope AND dimensions: %v", err)
	}
	if got := factIDs(exactScope.Rows); !reflect.DeepEqual(got, []journal.JournalID{first}) {
		t.Fatalf("Exact scope IDs = %v, want [%d]", got, first)
	}

	unscoped, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskUnscoped}},
		Kinds:  []journal.DecisionKind{"fixture.filter.first", "fixture.filter.second", "fixture.filter.third"},
		Page:   journal.FactPageRequest{Limit: 256},
	})
	if err != nil {
		t.Fatalf("Unscoped scope: %v", err)
	}
	if got := factIDs(unscoped.Rows); !reflect.DeepEqual(got, []journal.JournalID{second}) {
		t.Fatalf("Unscoped IDs = %v, want [%d]", got, second)
	}
}

func TestFactQueryCorpusCoversBothSubtypesScopesDimensionsAndContexts(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actorA, taskA := seedActorAndTask(t, db)
	actorB := seedFactActor(t, db)
	actorC := seedFactActor(t, db)
	boot := genesisBoot(t, db, actorA)
	taskB := createFactTaskWithOperation(t, db, boot, actorA, "facts-corpus-task-b")
	actorContextA, err := journal.ActorContext(actorA)
	if err != nil {
		t.Fatalf("ActorContext A: %v", err)
	}
	actorContextB, err := journal.ActorContext(actorB)
	if err != nil {
		t.Fatalf("ActorContext B: %v", err)
	}
	taskContextA, err := journal.TaskContext(taskA)
	if err != nil {
		t.Fatalf("TaskContext A: %v", err)
	}
	taskContextB, err := journal.TaskContext(taskB)
	if err != nil {
		t.Fatalf("TaskContext B: %v", err)
	}
	wrongKind, err := journal.GitContext(journal.GitOID("0123456789abcdef0123456789abcdef01234567"))
	if err != nil {
		t.Fatalf("GitContext: %v", err)
	}

	decisionRows := []struct {
		id       journal.JournalID
		actor    journal.ActorID
		op       journal.OperationID
		kind     journal.DecisionKind
		task     journal.TaskID
		contexts []journal.EventContext
	}{
		{actor: actorA, op: "facts-corpus-decision-a", kind: "corpus.decision.alpha", task: taskA, contexts: []journal.EventContext{taskContextA, actorContextA}},
		{actor: actorB, op: "facts-corpus-decision-b", kind: "corpus.decision.beta", task: taskA, contexts: []journal.EventContext{actorContextB}},
		{actor: actorA, op: "facts-corpus-decision-c", kind: "corpus.decision.alpha", contexts: []journal.EventContext{actorContextA}},
		{actor: actorC, op: "facts-corpus-decision-d", kind: "corpus.decision.gamma", task: taskB, contexts: []journal.EventContext{actorContextA}},
		{actor: actorA, op: "facts-corpus-decision-e", kind: "corpus.decision.gamma", task: taskA, contexts: []journal.EventContext{actorContextA}},
	}
	for i := range decisionRows {
		decisionRows[i].id = applyDecisionFact(t, db, boot, decisionRows[i].actor, decisionRows[i].op, decisionRows[i].task, decisionRows[i].kind, []byte(fmt.Sprintf(`{"row":"decision-%d"}`, i)), decisionRows[i].contexts...)
	}
	evidenceRows := []struct {
		id       journal.JournalID
		actor    journal.ActorID
		op       journal.OperationID
		kind     journal.EvidenceKind
		task     journal.TaskID
		contexts []journal.EventContext
	}{
		{actor: actorA, op: "facts-corpus-evidence-a", kind: "corpus.evidence.alpha", task: taskA, contexts: []journal.EventContext{taskContextA, actorContextA}},
		{actor: actorB, op: "facts-corpus-evidence-b", kind: "corpus.evidence.beta", task: taskA, contexts: []journal.EventContext{actorContextB}},
		{actor: actorA, op: "facts-corpus-evidence-c", kind: "corpus.evidence.alpha", contexts: []journal.EventContext{actorContextA}},
		{actor: actorC, op: "facts-corpus-evidence-d", kind: "corpus.evidence.gamma", task: taskB, contexts: []journal.EventContext{actorContextA}},
		{actor: actorA, op: "facts-corpus-evidence-e", kind: "corpus.evidence.gamma", task: taskA, contexts: []journal.EventContext{actorContextA}},
	}
	for i := range evidenceRows {
		evidenceRows[i].id = applyEvidenceFact(t, db, boot, evidenceRows[i].actor, evidenceRows[i].op, evidenceRows[i].task, evidenceRows[i].kind, []byte{byte(i + 1)}, []byte(fmt.Sprintf(`{"row":"evidence-%d"}`, i)), evidenceRows[i].contexts...)
	}

	decisionIDs := func(rows []journal.DecisionRow) []journal.JournalID { return factIDs(rows) }
	evidenceIDs := func(rows []journal.EvidenceRow) []journal.JournalID {
		ids := make([]journal.JournalID, len(rows))
		for i, row := range rows {
			ids[i] = row.JournalID
		}
		return ids
	}
	decisionQuery := func(filter journal.FactFilter, kinds ...journal.DecisionKind) []journal.JournalID {
		t.Helper()
		page, queryErr := db.Facts().QueryDecisions(journal.DecisionQuery{Filter: filter, Kinds: kinds, Page: journal.FactPageRequest{Limit: 256}})
		if queryErr != nil {
			t.Fatalf("decision corpus query: %v", queryErr)
		}
		return decisionIDs(page.Rows)
	}
	evidenceQuery := func(filter journal.FactFilter, kinds ...journal.EvidenceKind) []journal.JournalID {
		t.Helper()
		page, queryErr := db.Facts().QueryEvidence(journal.EvidenceQuery{Filter: filter, Kinds: kinds, Page: journal.FactPageRequest{Limit: 256}})
		if queryErr != nil {
			t.Fatalf("evidence corpus query: %v", queryErr)
		}
		return evidenceIDs(page.Rows)
	}

	anyFilter := journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}, EffectiveActorIDs: []journal.ActorID{actorA, actorB}, OperationIDs: []journal.OperationID{"facts-corpus-decision-a", "facts-corpus-decision-b"}}
	if got := decisionQuery(anyFilter, "corpus.decision.alpha", "corpus.decision.beta"); !reflect.DeepEqual(got, []journal.JournalID{decisionRows[0].id, decisionRows[1].id}) {
		t.Fatalf("decision Any OR/AND IDs = %v, want %v", got, []journal.JournalID{decisionRows[0].id, decisionRows[1].id})
	}
	if got := decisionQuery(journal.FactFilter{EffectiveActorIDs: []journal.ActorID{actorA}, TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}, "corpus.decision.alpha"); !reflect.DeepEqual(got, []journal.JournalID{decisionRows[0].id, decisionRows[2].id}) {
		t.Fatalf("decision actor OR IDs = %v, want first and unscoped rows", got)
	}
	if got := decisionQuery(journal.FactFilter{OperationIDs: []journal.OperationID{"facts-corpus-decision-a", "facts-corpus-decision-c"}}, "corpus.decision.alpha"); !reflect.DeepEqual(got, []journal.JournalID{decisionRows[0].id, decisionRows[2].id}) {
		t.Fatalf("decision operation OR IDs = %v, want first and unscoped rows", got)
	}
	if got := decisionQuery(journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: taskA}, EffectiveActorIDs: []journal.ActorID{actorA, actorB}, OperationIDs: []journal.OperationID{"facts-corpus-decision-a", "facts-corpus-decision-b"}}, "corpus.decision.alpha", "corpus.decision.beta"); !reflect.DeepEqual(got, []journal.JournalID{decisionRows[0].id, decisionRows[1].id}) {
		t.Fatalf("decision Exact IDs = %v, want first and second rows", got)
	}
	if got := decisionQuery(journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskUnscoped}}, "corpus.decision.alpha", "corpus.decision.beta"); !reflect.DeepEqual(got, []journal.JournalID{decisionRows[2].id}) {
		t.Fatalf("decision Unscoped IDs = %v, want third row", got)
	}

	evidenceAnyFilter := journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}, EffectiveActorIDs: []journal.ActorID{actorA, actorB}, OperationIDs: []journal.OperationID{"facts-corpus-evidence-a", "facts-corpus-evidence-b"}}
	if got := evidenceQuery(evidenceAnyFilter, "corpus.evidence.alpha", "corpus.evidence.beta"); !reflect.DeepEqual(got, []journal.JournalID{evidenceRows[0].id, evidenceRows[1].id}) {
		t.Fatalf("evidence Any OR/AND IDs = %v, want %v", got, []journal.JournalID{evidenceRows[0].id, evidenceRows[1].id})
	}
	if got := evidenceQuery(journal.FactFilter{EffectiveActorIDs: []journal.ActorID{actorA}}, "corpus.evidence.alpha"); !reflect.DeepEqual(got, []journal.JournalID{evidenceRows[0].id, evidenceRows[2].id}) {
		t.Fatalf("evidence actor OR IDs = %v, want first and unscoped rows", got)
	}
	if got := evidenceQuery(journal.FactFilter{OperationIDs: []journal.OperationID{"facts-corpus-evidence-a", "facts-corpus-evidence-c"}}, "corpus.evidence.alpha"); !reflect.DeepEqual(got, []journal.JournalID{evidenceRows[0].id, evidenceRows[2].id}) {
		t.Fatalf("evidence operation OR IDs = %v, want first and unscoped rows", got)
	}
	if got := evidenceQuery(journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: taskA}, EffectiveActorIDs: []journal.ActorID{actorA, actorB}, OperationIDs: []journal.OperationID{"facts-corpus-evidence-a", "facts-corpus-evidence-b"}}, "corpus.evidence.alpha", "corpus.evidence.beta"); !reflect.DeepEqual(got, []journal.JournalID{evidenceRows[0].id, evidenceRows[1].id}) {
		t.Fatalf("evidence Exact IDs = %v, want first and second rows", got)
	}
	if got := evidenceQuery(journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskUnscoped}}, "corpus.evidence.alpha", "corpus.evidence.beta"); !reflect.DeepEqual(got, []journal.JournalID{evidenceRows[2].id}) {
		t.Fatalf("evidence Unscoped IDs = %v, want third row", got)
	}

	decisionContexts := []struct {
		name     string
		required []journal.EventContext
		want     bool
	}{
		{name: "exact", required: []journal.EventContext{actorContextA, taskContextA}, want: true},
		{name: "extra-stored-subset", required: []journal.EventContext{actorContextA}, want: true},
		{name: "missing", required: []journal.EventContext{wrongKind}, want: false},
		{name: "partial", required: []journal.EventContext{actorContextA, wrongKind}, want: false},
		{name: "wrong-kind", required: []journal.EventContext{taskContextB}, want: false},
	}
	expectedCanonicalContexts, err := journal.CanonicalEventContexts([]journal.EventContext{taskContextA, actorContextA})
	if err != nil {
		t.Fatalf("CanonicalEventContexts corpus fixture: %v", err)
	}
	for _, test := range decisionContexts {
		t.Run("decision context "+test.name, func(t *testing.T) {
			page, queryErr := db.Facts().QueryDecisions(journal.DecisionQuery{Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: taskA}, RequiredContexts: test.required}, Kinds: []journal.DecisionKind{"corpus.decision.alpha"}, Page: journal.FactPageRequest{Limit: 256}})
			if queryErr != nil {
				t.Fatalf("decision required contexts: %v", queryErr)
			}
			got := decisionIDs(page.Rows)
			want := []journal.JournalID{}
			if test.want {
				want = []journal.JournalID{decisionRows[0].id}
				if !reflect.DeepEqual(page.Rows[0].Contexts, expectedCanonicalContexts) {
					t.Fatalf("decision returned contexts = %+v, want canonical %+v", page.Rows[0].Contexts, expectedCanonicalContexts)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decision required contexts IDs = %v, want %v", got, want)
			}
		})
	}
	for _, test := range decisionContexts {
		t.Run("evidence context "+test.name, func(t *testing.T) {
			page, queryErr := db.Facts().QueryEvidence(journal.EvidenceQuery{Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: taskA}, RequiredContexts: test.required}, Kinds: []journal.EvidenceKind{"corpus.evidence.alpha"}, Page: journal.FactPageRequest{Limit: 256}})
			if queryErr != nil {
				t.Fatalf("evidence required contexts: %v", queryErr)
			}
			got := evidenceIDs(page.Rows)
			want := []journal.JournalID{}
			if test.want {
				want = []journal.JournalID{evidenceRows[0].id}
				if !reflect.DeepEqual(page.Rows[0].Contexts, expectedCanonicalContexts) {
					t.Fatalf("evidence returned contexts = %+v, want canonical %+v", page.Rows[0].Contexts, expectedCanonicalContexts)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("evidence required contexts IDs = %v, want %v", got, want)
			}
		})
	}
	if got := decisionQuery(journal.FactFilter{}, "corpus.evidence.alpha"); len(got) != 0 {
		t.Fatalf("decision subtype query crossed into evidence rows: %v", got)
	}
	if got := evidenceQuery(journal.FactFilter{}, "corpus.decision.alpha"); len(got) != 0 {
		t.Fatalf("evidence subtype query crossed into decision rows: %v", got)
	}
}

func TestFactQueryPaginationTraversesJournalIDSnapshotExactlyOnce(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	want := []journal.JournalID{
		applyDecisionFactAt(t, db, boot, actor, "facts-traverse-a", task, "fixture.traverse", []byte(`{"row":"a"}`), 200),
		applyDecisionFactAt(t, db, boot, actor, "facts-traverse-b", task, "fixture.traverse", []byte(`{"row":"b"}`), 200),
		applyDecisionFactAt(t, db, boot, actor, "facts-traverse-c", task, "fixture.traverse", []byte(`{"row":"c"}`), 100),
		applyDecisionFactAt(t, db, boot, actor, "facts-traverse-d", task, "fixture.traverse", []byte(`{"row":"d"}`), 50),
	}
	first, err := db.Facts().QueryDecisions(journal.DecisionQuery{Kinds: []journal.DecisionKind{"fixture.traverse"}, Page: journal.FactPageRequest{Limit: 1}})
	if err != nil {
		t.Fatalf("first traversal page: %v", err)
	}
	if len(first.Rows) != 1 || first.Next == nil {
		t.Fatalf("first traversal page = %+v, want one row and cursor", first)
	}
	// This append is after the first page's transaction snapshot and must never
	// appear in a continuation using the cursor returned above.
	later := applyDecisionFactAt(t, db, boot, actor, "facts-traverse-later", task, "fixture.traverse", []byte(`{"row":"later"}`), 1)
	seen := []journal.JournalID{first.Rows[0].JournalID}
	cursor := *first.Next
	for cursor.AfterJournalID != 0 {
		page, pageErr := db.Facts().QueryDecisions(journal.DecisionQuery{
			Kinds: []journal.DecisionKind{"fixture.traverse"},
			Page:  journal.FactPageRequest{Limit: 2, SnapshotMaxJournalID: cursor.SnapshotMaxJournalID, AfterJournalID: cursor.AfterJournalID},
		})
		if pageErr != nil {
			t.Fatalf("continuation after %d: %v", cursor.AfterJournalID, pageErr)
		}
		for _, row := range page.Rows {
			seen = append(seen, row.JournalID)
			if row.JournalID == later {
				t.Fatalf("later journal %d leaked into pinned traversal", later)
			}
		}
		if page.Next == nil {
			break
		}
		cursor = *page.Next
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("traversal IDs = %v, want %v", seen, want)
	}
	terminal, err := db.Facts().QueryDecisions(journal.DecisionQuery{Kinds: []journal.DecisionKind{"fixture.traverse"}, Page: journal.FactPageRequest{Limit: 256}})
	if err != nil {
		t.Fatalf("Limit=256 traversal page: %v", err)
	}
	if len(terminal.Rows) != len(want)+1 || terminal.Next != nil {
		t.Fatalf("Limit=256 page rows/next = %d/%v, want %d/nil", len(terminal.Rows), terminal.Next, len(want)+1)
	}
	empty, err := db.Facts().QueryDecisions(journal.DecisionQuery{Kinds: []journal.DecisionKind{"fixture.no-such-kind"}, Page: journal.FactPageRequest{Limit: 1}})
	if err != nil {
		t.Fatalf("empty page: %v", err)
	}
	if len(empty.Rows) != 0 || empty.Next != nil {
		t.Fatalf("empty page = %+v, want terminal empty page", empty)
	}
	evidenceFirst := applyEvidenceFact(t, db, boot, actor, "facts-traverse-evidence-a", task, "fixture.traverse.evidence", []byte{1}, []byte(`{"row":"a"}`))
	evidenceSecond := applyEvidenceFact(t, db, boot, actor, "facts-traverse-evidence-b", task, "fixture.traverse.evidence", []byte{2}, []byte(`{"row":"b"}`))
	evidencePage, err := db.Facts().QueryEvidence(journal.EvidenceQuery{Kinds: []journal.EvidenceKind{"fixture.traverse.evidence"}, Page: journal.FactPageRequest{Limit: 1}})
	if err != nil || len(evidencePage.Rows) != 1 || evidencePage.Next == nil {
		t.Fatalf("first evidence page = %+v err=%v, want one row and cursor", evidencePage, err)
	}
	evidenceTail, err := db.Facts().QueryEvidence(journal.EvidenceQuery{Kinds: []journal.EvidenceKind{"fixture.traverse.evidence"}, Page: journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: evidencePage.Next.SnapshotMaxJournalID, AfterJournalID: evidencePage.Next.AfterJournalID}})
	if err != nil || !reflect.DeepEqual([]journal.JournalID{evidencePage.Rows[0].JournalID, evidenceTail.Rows[0].JournalID}, []journal.JournalID{evidenceFirst, evidenceSecond}) || evidenceTail.Next != nil {
		t.Fatalf("evidence traversal first=%+v tail=%+v err=%v, want [%d %d] terminal", evidencePage.Rows, evidenceTail.Rows, err, evidenceFirst, evidenceSecond)
	}
}

func TestFactPageSQLKeepsTopologyValidationBoundedToCandidatePage(t *testing.T) {
	t.Parallel()
	for _, kind := range []factSelectorKind{factSelectorDecision, factSelectorEvidence} {
		sql := kind.pageMatchSQL()
		if !strings.Contains(sql, "WITH candidates AS") || strings.Count(sql, "LIMIT ?14") != 1 || !strings.Contains(sql, "LEFT JOIN journal_attributed") || !strings.Contains(sql, "LEFT JOIN journal_operations") {
			t.Errorf("fact subtype %d page SQL is not candidate/page bounded diagnostics: %q", kind, sql)
		}
		if strings.Contains(sql, "WHERE d.journal_id <= ?1\nORDER BY") || strings.Contains(sql, "WHERE e.journal_id <= ?1\nORDER BY") {
			t.Errorf("fact subtype %d retains an unbounded history topology scan", kind)
		}
	}
}

func TestFactQueryDecisionSnapshotBarrierPinsPreCommitRows(t *testing.T) {
	// The snapshot barrier is installed on this test's own DB instance, so this
	// test runs alongside the other fact-query readers.
	t.Parallel()
	db, actor, task, boot := newFileFactDB(t, "decision-barrier.db")
	defer db.Close()
	want := []journal.JournalID{
		applyDecisionFactAt(t, db, boot, actor, "facts-barrier-decision-a", task, "fixture.barrier.decision", []byte(`{"row":"a"}`), 1),
		applyDecisionFactAt(t, db, boot, actor, "facts-barrier-decision-b", task, "fixture.barrier.decision", []byte(`{"row":"b"}`), 2),
		applyDecisionFactAt(t, db, boot, actor, "facts-barrier-decision-c", task, "fixture.barrier.decision", []byte(`{"row":"c"}`), 3),
		applyDecisionFactAt(t, db, boot, actor, "facts-barrier-decision-d", task, "fixture.barrier.decision", []byte(`{"row":"d"}`), 4),
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	db.installFactQuerySnapshotBarrier(func(kind factSelectorKind, _ int64) {
		if kind != factSelectorDecision {
			return
		}
		once.Do(func() { close(entered) })
		<-release
	})
	type result struct {
		page journal.DecisionPage
		err  error
	}
	pageDone := make(chan result, 1)
	go func() {
		page, err := db.Facts().QueryDecisions(journal.DecisionQuery{Kinds: []journal.DecisionKind{"fixture.barrier.decision"}, Page: journal.FactPageRequest{Limit: 1}})
		pageDone <- result{page: page, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("decision query did not reach the post-snapshot barrier")
	}
	later := applyDecisionFactAt(t, db, boot, actor, "facts-barrier-decision-later", task, "fixture.barrier.decision", []byte(`{"row":"later"}`), 5)
	if later <= want[len(want)-1] {
		t.Fatalf("later decision JournalID %d <= pre-barrier watermark %d", later, want[len(want)-1])
	}
	close(release)
	first := <-pageDone
	if first.err != nil {
		t.Fatalf("barriered decision page: %v", first.err)
	}
	if first.page.SnapshotMaxJournalID != want[len(want)-1] || first.page.Next == nil {
		t.Fatalf("barriered decision page snapshot/next = %d/%v, want %d/non-nil", first.page.SnapshotMaxJournalID, first.page.Next, want[len(want)-1])
	}
	seen := []journal.JournalID{first.page.Rows[0].JournalID}
	cursor := *first.page.Next
	for {
		page, err := db.Facts().QueryDecisions(journal.DecisionQuery{Kinds: []journal.DecisionKind{"fixture.barrier.decision"}, Page: journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: cursor.SnapshotMaxJournalID, AfterJournalID: cursor.AfterJournalID}})
		if err != nil {
			t.Fatalf("decision continuation after %d: %v", cursor.AfterJournalID, err)
		}
		if page.SnapshotMaxJournalID != first.page.SnapshotMaxJournalID {
			t.Fatalf("decision continuation snapshot = %d, want %d", page.SnapshotMaxJournalID, first.page.SnapshotMaxJournalID)
		}
		for _, row := range page.Rows {
			seen = append(seen, row.JournalID)
			if row.JournalID == later {
				t.Fatalf("post-barrier decision %d leaked into pinned traversal", later)
			}
		}
		if page.Next == nil {
			break
		}
		cursor = *page.Next
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("barriered decision traversal = %v, want %v", seen, want)
	}
	terminal, err := db.Facts().QueryDecisions(journal.DecisionQuery{Kinds: []journal.DecisionKind{"fixture.barrier.decision"}, Page: journal.FactPageRequest{Limit: 256, SnapshotMaxJournalID: first.page.SnapshotMaxJournalID}})
	if err != nil || !reflect.DeepEqual(factIDs(terminal.Rows), want) || terminal.Next != nil {
		t.Fatalf("barriered decision Limit=256 page = %+v err=%v, want exact terminal pre-snapshot page", terminal, err)
	}
	atWatermark, err := db.Facts().QueryDecisions(journal.DecisionQuery{Kinds: []journal.DecisionKind{"fixture.barrier.decision"}, Page: journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: first.page.SnapshotMaxJournalID, AfterJournalID: first.page.SnapshotMaxJournalID}})
	if err != nil || len(atWatermark.Rows) != 0 || atWatermark.Next != nil || atWatermark.SnapshotMaxJournalID != first.page.SnapshotMaxJournalID {
		t.Fatalf("decision cursor at watermark = %+v err=%v, want empty terminal page", atWatermark, err)
	}
}

func TestFactQueryEvidenceSnapshotBarrierPinsPreCommitRows(t *testing.T) {
	// The snapshot barrier is installed on this test's own DB instance, so this
	// test runs alongside the other fact-query readers.
	t.Parallel()
	db, actor, task, boot := newFileFactDB(t, "evidence-barrier.db")
	defer db.Close()
	want := []journal.JournalID{
		applyEvidenceFactAt(t, db, boot, actor, "facts-barrier-evidence-a", task, "fixture.barrier.evidence", []byte{1}, []byte(`{"row":"a"}`), 1),
		applyEvidenceFactAt(t, db, boot, actor, "facts-barrier-evidence-b", task, "fixture.barrier.evidence", []byte{2}, []byte(`{"row":"b"}`), 2),
		applyEvidenceFactAt(t, db, boot, actor, "facts-barrier-evidence-c", task, "fixture.barrier.evidence", []byte{3}, []byte(`{"row":"c"}`), 3),
		applyEvidenceFactAt(t, db, boot, actor, "facts-barrier-evidence-d", task, "fixture.barrier.evidence", []byte{4}, []byte(`{"row":"d"}`), 4),
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	db.installFactQuerySnapshotBarrier(func(kind factSelectorKind, _ int64) {
		if kind != factSelectorEvidence {
			return
		}
		once.Do(func() { close(entered) })
		<-release
	})
	type result struct {
		page journal.EvidencePage
		err  error
	}
	pageDone := make(chan result, 1)
	go func() {
		page, err := db.Facts().QueryEvidence(journal.EvidenceQuery{Kinds: []journal.EvidenceKind{"fixture.barrier.evidence"}, Page: journal.FactPageRequest{Limit: 1}})
		pageDone <- result{page: page, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("evidence query did not reach the post-snapshot barrier")
	}
	later := applyEvidenceFactAt(t, db, boot, actor, "facts-barrier-evidence-later", task, "fixture.barrier.evidence", []byte{5}, []byte(`{"row":"later"}`), 5)
	if later <= want[len(want)-1] {
		t.Fatalf("later evidence JournalID %d <= pre-barrier watermark %d", later, want[len(want)-1])
	}
	close(release)
	first := <-pageDone
	if first.err != nil {
		t.Fatalf("barriered evidence page: %v", first.err)
	}
	if first.page.SnapshotMaxJournalID != want[len(want)-1] || first.page.Next == nil {
		t.Fatalf("barriered evidence page snapshot/next = %d/%v, want %d/non-nil", first.page.SnapshotMaxJournalID, first.page.Next, want[len(want)-1])
	}
	evidenceIDs := func(rows []journal.EvidenceRow) []journal.JournalID {
		ids := make([]journal.JournalID, len(rows))
		for i, row := range rows {
			ids[i] = row.JournalID
		}
		return ids
	}
	seen := []journal.JournalID{first.page.Rows[0].JournalID}
	cursor := *first.page.Next
	for {
		page, err := db.Facts().QueryEvidence(journal.EvidenceQuery{Kinds: []journal.EvidenceKind{"fixture.barrier.evidence"}, Page: journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: cursor.SnapshotMaxJournalID, AfterJournalID: cursor.AfterJournalID}})
		if err != nil {
			t.Fatalf("evidence continuation after %d: %v", cursor.AfterJournalID, err)
		}
		if page.SnapshotMaxJournalID != first.page.SnapshotMaxJournalID {
			t.Fatalf("evidence continuation snapshot = %d, want %d", page.SnapshotMaxJournalID, first.page.SnapshotMaxJournalID)
		}
		for _, row := range page.Rows {
			seen = append(seen, row.JournalID)
			if row.JournalID == later {
				t.Fatalf("post-barrier evidence %d leaked into pinned traversal", later)
			}
		}
		if page.Next == nil {
			break
		}
		cursor = *page.Next
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("barriered evidence traversal = %v, want %v", seen, want)
	}
	terminal, err := db.Facts().QueryEvidence(journal.EvidenceQuery{Kinds: []journal.EvidenceKind{"fixture.barrier.evidence"}, Page: journal.FactPageRequest{Limit: 256, SnapshotMaxJournalID: first.page.SnapshotMaxJournalID}})
	if err != nil || !reflect.DeepEqual(evidenceIDs(terminal.Rows), want) || terminal.Next != nil {
		t.Fatalf("barriered evidence Limit=256 page = %+v err=%v, want exact terminal pre-snapshot page", terminal, err)
	}
	atWatermark, err := db.Facts().QueryEvidence(journal.EvidenceQuery{Kinds: []journal.EvidenceKind{"fixture.barrier.evidence"}, Page: journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: first.page.SnapshotMaxJournalID, AfterJournalID: first.page.SnapshotMaxJournalID}})
	if err != nil || len(atWatermark.Rows) != 0 || atWatermark.Next != nil || atWatermark.SnapshotMaxJournalID != first.page.SnapshotMaxJournalID {
		t.Fatalf("evidence cursor at watermark = %+v err=%v, want empty terminal page", atWatermark, err)
	}
}

func TestFactQueryCorruptionFailsClosedWithoutChangingSQLiteArtifacts(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "facts-corrupt.db")
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	actor := seedFactActor(t, db)
	boot := genesisBoot(t, db, actor)
	applyDecisionFact(t, db, boot, actor, "facts-corrupt-query", journal.TaskID{}, "fixture.corrupt.query", []byte(`{"corrupt":true}`))

	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bind corruption scope: %v", err)
	}
	if err := execFactTestSQL(scope, "UPDATE journal_operations SET operation_id=char(1) WHERE operation_id=?1", "facts-corrupt-query"); err != nil {
		scope.release()
		t.Fatalf("corrupt operation identity: %v", err)
	}
	scope.release()
	if err := db.Close(); err != nil {
		t.Fatalf("Close corrupted fixture: %v", err)
	}
	before := sqliteArtifactBytes(t, path)
	db, err = Open(path, nil)
	if err != nil {
		t.Fatalf("reopen corrupted fixture: %v", err)
	}

	_, err = db.Facts().QueryDecisions(journal.DecisionQuery{
		Kinds: []journal.DecisionKind{"fixture.corrupt.query"},
		Page:  journal.FactPageRequest{Limit: 1},
	})
	if err == nil || !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Fatalf("corrupt query error = %v, want ErrSubtypeIntegrity", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close after corrupt read: %v", err)
	}
	after := sqliteArtifactBytes(t, path)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("corrupt read changed SQLite database or WAL/SHM artifacts")
	}
}

func TestFactQueriesCorruptionMatrixFailsClosedForBothSubtypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*connScope, journal.JournalID, journal.JournalID, journal.ActorID, factContextRelation) error
	}{
		{name: "dangling-producer", mutate: func(scope *connScope, factID, _ journal.JournalID, actor journal.ActorID, _ factContextRelation) error {
			return execFactTestSQL(scope, "UPDATE journal SET actor_id=NULL, produced_by_operation_journal_id=?1 WHERE journal_id=?2", int64(1<<60), int64(factID))
		}},
		{name: "missing-attribution-producer", mutate: func(scope *connScope, factID, _ journal.JournalID, actor journal.ActorID, _ factContextRelation) error {
			return execFactTestSQL(scope, "UPDATE journal SET actor_id=?1, produced_by_operation_journal_id=NULL WHERE journal_id=?2", actor.String(), int64(factID))
		}},
		{name: "malformed-attribution-actor", mutate: func(scope *connScope, factID, operationAnchor journal.JournalID, _ journal.ActorID, _ factContextRelation) error {
			if err := execFactTestSQL(scope, "UPDATE journal SET actor_id=NULL WHERE journal_id=?1", int64(factID)); err != nil {
				return err
			}
			return execFactTestSQL(scope, "UPDATE journal SET actor_id=?1 WHERE journal_id=?2", "\x01", int64(operationAnchor))
		}},
		{name: "wrong-subtype", mutate: func(scope *connScope, factID, _ journal.JournalID, _ journal.ActorID, _ factContextRelation) error {
			return execFactTestSQL(scope, "UPDATE journal SET kind_id=?1 WHERE journal_id=?2", int(journal.JournalKindOperation), int64(factID))
		}},
		{name: "malformed-producer-operation-id", mutate: func(scope *connScope, _ journal.JournalID, operationAnchor journal.JournalID, _ journal.ActorID, _ factContextRelation) error {
			return execFactTestSQL(scope, "UPDATE journal_operations SET operation_id=char(1) WHERE journal_id=?1", int64(operationAnchor))
		}},
		{name: "invalid-payload", mutate: func(scope *connScope, factID, _ journal.JournalID, _ journal.ActorID, relation factContextRelation) error {
			if err := execFactTestSQL(scope, "PRAGMA ignore_check_constraints=ON"); err != nil {
				return err
			}
			table := "journal_decisions"
			if relation == factContextEvidence {
				table = "journal_evidence"
			}
			return execFactTestSQL(scope, "UPDATE "+table+" SET payload=?1 WHERE journal_id=?2", "not-json", int64(factID))
		}},
		{name: "wrong-evidence-digest", mutate: func(scope *connScope, factID, _ journal.JournalID, _ journal.ActorID, relation factContextRelation) error {
			if relation != factContextEvidence {
				return nil
			}
			return execFactTestSQL(scope, "UPDATE journal_evidence SET content_digest=?1 WHERE journal_id=?2", []byte("wrong-digest"), int64(factID))
		}},
		{name: "missing-context", mutate: func(scope *connScope, factID, _ journal.JournalID, _ journal.ActorID, relation factContextRelation) error {
			return execFactTestSQL(scope, "DELETE FROM "+relation.tableName()+" WHERE "+relation.parentColumn()+"=?1", int64(factID))
		}},
		{name: "extra-context", mutate: func(scope *connScope, factID, _ journal.JournalID, _ journal.ActorID, relation factContextRelation) error {
			return execFactTestSQL(scope, relation.insertSQL(), int64(factID), "git", "0123456789abcdef0123456789abcdef01234567")
		}},
		{name: "malformed-context", mutate: func(scope *connScope, factID, _ journal.JournalID, _ journal.ActorID, relation factContextRelation) error {
			return execFactTestSQL(scope, "UPDATE "+relation.tableName()+" SET context_kind=?1, context_identity=?2 WHERE "+relation.parentColumn()+"=?3", "git", "not-a-git-object", int64(factID))
		}},
		{name: "cross-subtype-context", mutate: func(scope *connScope, factID, _ journal.JournalID, actor journal.ActorID, relation factContextRelation) error {
			opposite := factContextEvidence
			if relation == factContextEvidence {
				opposite = factContextDecision
			}
			return execFactTestSQL(scope, opposite.insertSQL(), int64(factID), "actor", actor.String())
		}},
	}
	for _, subtype := range []factContextRelation{factContextDecision, factContextEvidence} {
		for _, testCase := range cases {
			subtype, testCase := subtype, testCase
			if subtype == factContextDecision && testCase.name == "wrong-evidence-digest" {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s", subtype.tableName(), testCase.name), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "corruption.db")
				db, err := Open(path, nil)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				defer db.Close()
				actor, task := seedActorAndTask(t, db)
				boot := genesisBoot(t, db, actor)
				operation := journal.OperationID("facts-corrupt-matrix-" + subtype.tableName() + "-" + testCase.name)
				var factID journal.JournalID
				var anchor journal.JournalID
				if subtype == factContextDecision {
					factID = applyDecisionFact(t, db, boot, actor, operation, task, "fixture.corrupt.matrix.decision", []byte(`{"valid":true}`), mustFactActorContext(t, actor))
				} else {
					factID = applyEvidenceFact(t, db, boot, actor, operation, task, "fixture.corrupt.matrix.evidence", []byte{4, 2}, []byte(`{"valid":true}`), mustFactActorContext(t, actor))
				}
				anchor = operationAnchor(t, db, operation)
				scope, err := db.bindScope(context.Background(), projectionTargetLive)
				if err != nil {
					t.Fatalf("bind corruption scope: %v", err)
				}
				if err := execFactTestSQL(scope, "PRAGMA foreign_keys=OFF"); err != nil {
					scope.release()
					t.Fatalf("disable FK checks for corruption fixture: %v", err)
				}
				if err := testCase.mutate(scope, factID, anchor, actor, subtype); err != nil {
					scope.release()
					t.Fatalf("mutate %s: %v", testCase.name, err)
				}
				scope.release()
				stabilizeReadArtifacts(t, db)
				before := sqliteArtifactBytes(t, path)
				if subtype == factContextDecision {
					page, queryErr := db.Facts().QueryDecisions(journal.DecisionQuery{Kinds: []journal.DecisionKind{"fixture.corrupt.matrix.decision"}, Page: journal.FactPageRequest{Limit: 256}})
					assertCorruptFactQueryError(t, page.Rows, queryErr, testCase.name)
				} else {
					page, queryErr := db.Facts().QueryEvidence(journal.EvidenceQuery{Kinds: []journal.EvidenceKind{"fixture.corrupt.matrix.evidence"}, Page: journal.FactPageRequest{Limit: 256}})
					assertCorruptFactQueryError(t, page.Rows, queryErr, testCase.name)
				}
				after := sqliteArtifactBytes(t, path)
				if !reflect.DeepEqual(before, after) {
					for name, contents := range before {
						if !bytes.Equal(contents, after[name]) {
							t.Logf("artifact changed: %s", name)
						}
					}
					t.Fatalf("corrupt %s query changed database/WAL/SHM artifacts", testCase.name)
				}
			})
		}
	}
}

func stabilizeReadArtifacts(t *testing.T, db *DB) {
	t.Helper()
	for i := 0; i < runtimePoolSize; i++ {
		scope, err := db.bindScope(context.Background(), projectionTargetLive)
		if err != nil {
			t.Fatalf("bind artifact stabilization scope: %v", err)
		}
		var count int
		if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM journal").Scan(&count); err != nil {
			scope.release()
			t.Fatalf("stabilize artifact read: %v", err)
		}
		scope.release()
	}
}

func mustFactActorContext(t *testing.T, actor journal.ActorID) journal.EventContext {
	t.Helper()
	context, err := journal.ActorContext(actor)
	if err != nil {
		t.Fatalf("ActorContext: %v", err)
	}
	return context
}

func assertCorruptFactQueryError[T any](t *testing.T, rows []T, err error, name string) {
	t.Helper()
	if len(rows) != 0 {
		t.Fatalf("corrupt %s query returned partial rows: %v", name, rows)
	}
	if err == nil || (!errors.Is(err, journal.ErrSubtypeIntegrity) && !errors.Is(err, journal.ErrFactContextIntegrity)) {
		t.Fatalf("corrupt %s query error = %v, want typed subtype/context integrity error", name, err)
	}
}

func sqliteArtifactBytes(t *testing.T, path string) map[string][]byte {
	t.Helper()
	artifacts := make(map[string][]byte)
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		contents, err := os.ReadFile(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("read SQLite artifact %q: %v", name, err)
		}
		artifacts[name] = contents
	}
	return artifacts
}

func newFileFactDB(t *testing.T, name string) (*DB, journal.ActorID, journal.TaskID, journal.JournalID) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open file-backed fact database: %v", err)
	}
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	t.Cleanup(func() { _ = db.Close() })
	return db, actor, task, boot
}

func factIDs(rows []journal.DecisionRow) []journal.JournalID {
	ids := make([]journal.JournalID, len(rows))
	for i, row := range rows {
		ids[i] = row.JournalID
	}
	return ids
}

func applyDecisionFact(t *testing.T, db *DB, boot journal.JournalID, actor journal.ActorID, operation journal.OperationID, task journal.TaskID, kind journal.DecisionKind, payload []byte, contexts ...journal.EventContext) journal.JournalID {
	return applyDecisionFactAt(t, db, boot, actor, operation, task, kind, payload, time.Now().UTC().UnixNano(), contexts...)
}

func applyDecisionFactAt(t *testing.T, db *DB, boot journal.JournalID, actor journal.ActorID, operation journal.OperationID, task journal.TaskID, kind journal.DecisionKind, payload []byte, recordedAt int64, contexts ...journal.EventContext) journal.JournalID {
	t.Helper()
	effect := journal.Effect{Sort: journal.EffectDecision, ResultSlot: "decision", DecisionKind: kind, Payload: payload, Contexts: contexts}
	if task != (journal.TaskID{}) {
		effect.TaskID = task
	}
	result, err := db.Apply(journal.OperationInput{
		OperationID:        operation,
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("command-" + string(operation)),
		MutationDigest:     []byte("mutation-" + string(operation)),
		RecordedAt:         recordedAt,
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
	_, err = scope.conn.ExecContext(scope.ctx, "INSERT INTO agents (id, kind_id) VALUES (?1, ?2)", actor.String(), int(ptypes.AgentKindSoftware))
	if err == nil {
		_, err = scope.conn.ExecContext(scope.ctx, "INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1, ?2, ?3, ?4)", actor.String(), "facts-test", "0", "test")
	}
	scope.release()
	if err != nil {
		t.Fatalf("seed actor: %v", err)
	}
	return actor
}

func createFactTask(t *testing.T, db *DB, boot journal.JournalID, actor journal.ActorID) journal.TaskID {
	return createFactTaskWithOperation(t, db, boot, actor, "facts-task-create")
}

func createFactTaskWithOperation(t *testing.T, db *DB, boot journal.JournalID, actor journal.ActorID, operation journal.OperationID) journal.TaskID {
	t.Helper()
	task := ptypes.TaskID{Namespace: "provenance-test", UUID: uuid.New()}
	if _, err := db.Apply(journal.OperationInput{
		OperationID:        operation,
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

func applyEvidenceFact(t *testing.T, db *DB, boot journal.JournalID, actor journal.ActorID, operation journal.OperationID, task journal.TaskID, kind journal.EvidenceKind, digest, payload []byte, contexts ...journal.EventContext) journal.JournalID {
	return applyEvidenceFactAt(t, db, boot, actor, operation, task, kind, digest, payload, time.Now().UTC().UnixNano(), contexts...)
}

func applyEvidenceFactAt(t *testing.T, db *DB, boot journal.JournalID, actor journal.ActorID, operation journal.OperationID, task journal.TaskID, kind journal.EvidenceKind, digest, payload []byte, recordedAt int64, contexts ...journal.EventContext) journal.JournalID {
	t.Helper()
	effect := journal.Effect{Sort: journal.EffectEvidence, ResultSlot: "evidence", EvidenceKind: kind, ContentDigest: digest, Payload: payload, Contexts: contexts}
	if task != (journal.TaskID{}) {
		effect.TaskID = task
	}
	result, err := db.Apply(journal.OperationInput{
		OperationID:        operation,
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("command-" + string(operation)),
		MutationDigest:     []byte("mutation-" + string(operation)),
		RecordedAt:         recordedAt,
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

func operationAnchor(t *testing.T, db *DB, operation journal.OperationID) journal.JournalID {
	t.Helper()
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bind operation anchor scope: %v", err)
	}
	defer scope.release()
	var rawAnchor int64
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT journal_id FROM journal_operations WHERE operation_id=?1", string(operation)).Scan(&rawAnchor); err != nil {
		t.Fatalf("operation anchor %q: %v", operation, err)
	}
	anchor := journal.JournalID(rawAnchor)
	if anchor <= 0 {
		t.Fatalf("operation anchor %q = %d, want positive", operation, anchor)
	}
	return anchor
}

func execFactTestSQL(scope *connScope, query string, args ...any) error {
	_, err := scope.conn.ExecContext(scope.ctx, query, args...)
	return err
}
