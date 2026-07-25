package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	_, err := db.Facts().QueryDecisions(journal.DecisionQuery{
		Kinds: []journal.DecisionKind{"fixture.snapshot.v2"},
		Page:  journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: 1 << 20},
	})
	if err == nil || !errors.Is(err, journal.ErrInvalidQuery) {
		t.Fatalf("forged snapshot error = %v, want ErrInvalidQuery", err)
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
	queries := []journal.DecisionQuery{
		{Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 0}},
		{Kinds: kinds, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskScopeKind(99)}}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact}}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{EffectiveActorIDs: []journal.ActorID{{}}}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{OperationIDs: []journal.OperationID{""}}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Filter: journal.FactFilter{RequiredContexts: []journal.EventContext{{}}}, Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1}},
		{Kinds: []journal.DecisionKind{"fixture.invalid"}, Page: journal.FactPageRequest{Limit: 1, SnapshotMaxJournalID: 10, AfterJournalID: 11}},
	}
	for i, query := range queries {
		if _, err := db.QueryDecisions(query); err == nil || !errors.Is(err, journal.ErrInvalidQuery) {
			t.Errorf("decision malformed case %d error = %v, want ErrInvalidQuery before lease", i, err)
		}
		evidenceKinds := []journal.EvidenceKind{"fixture.invalid"}
		if len(query.Kinds) > journal.MaxFactQueryKinds {
			evidenceKinds = make([]journal.EvidenceKind, len(query.Kinds))
			for j := range evidenceKinds {
				evidenceKinds[j] = journal.EvidenceKind(fmt.Sprintf("fixture.invalid.evidence.%d", j))
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
	if err := sqlitex.Execute(scope.conn, "UPDATE journal_operations SET operation_id=char(1) WHERE operation_id=?1", &sqlitex.ExecOptions{Args: []any{"facts-corrupt-query"}}); err != nil {
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

func applyEvidenceFact(t *testing.T, db *DB, boot journal.JournalID, actor journal.ActorID, operation journal.OperationID, task journal.TaskID, kind journal.EvidenceKind, digest, payload []byte, contexts ...journal.EventContext) journal.JournalID {
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
