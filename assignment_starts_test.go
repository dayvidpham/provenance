package provenance_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	p "github.com/dayvidpham/provenance"
)

func TestAssignmentStartsPublicTransferAndEnd(t *testing.T) {
	tr, err := p.OpenMemory(p.WithModelRegistry(p.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	a, b, task := assignmentQueryFixture(t, tr)
	first := assignmentQueryStart(t, tr, a, b, task, "predecessor")
	result, err := tr.Journal().Apply(p.OperationInput{OperationID: "transfer", ActorID: a, AuthorityJournalID: &b, CommandDigest: []byte("transfer"), Effects: []p.Effect{
		{Sort: p.EffectAssignmentEnd, AssignmentID: "predecessor", TaskID: task, SlotID: p.SlotOwnerResponsibility, ResultSlot: "end"},
		{Sort: p.EffectAssignmentStart, AssignmentID: "successor", TaskID: task, SlotID: p.SlotOwnerResponsibility, Occupant: a, Predecessor: "predecessor", ResultSlot: "start"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	api := assignmentQueryAPI(t, tr)
	page, err := api.QueryAssignmentStarts(p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 8}})
	if err != nil || len(page.Rows) != 2 || page.Next != nil {
		t.Fatalf("valid start plus end: %+v %v", page, err)
	}
	r := page.Rows[1]
	if page.Rows[0].AuthorityJournalID != first.FactID || r.PredecessorAssignmentID == nil || *r.PredecessorAssignmentID != "predecessor" || r.AssignmentID != "successor" || r.SlotID != p.SlotOwnerResponsibility || r.ProducingOperationID != "transfer" || r.ProducingOperationJournalID != result.AnchorJournalID {
		t.Fatalf("transfer identities: %+v", page.Rows)
	}
	// Query only the end candidate, with its start below the cursor. A prior
	// start remains necessary even though it is outside the consumed page.
	var end p.JournalID
	for _, s := range result.ResultSlots {
		if s.Slot == "end" {
			end = s.ProducedJournalID
		}
		if s.Slot == "start" && r.AuthorityJournalID != s.ProducedJournalID {
			t.Fatal("not exact successor authority")
		}
	}
	ended, err := api.QueryAssignmentStarts(p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 1, SnapshotPinned: true, SnapshotMaxJournalID: end, AfterJournalID: first.FactID}})
	if err != nil || len(ended.Rows) != 0 || ended.Next != nil {
		t.Fatalf("end-only page: %+v %v", ended, err)
	}
}

func TestAssignmentStartsPublicCorruptionBeforeFilters(t *testing.T) {
	// Each case owns a fresh shared-WAL pool: no shared live handles or fixtures.
	cases := []struct{ name, sql, token string }{
		{"missing-subtype", "DELETE FROM journal_authorities WHERE journal_id=%d", "subtype"},
		{"missing-transition", "DELETE FROM journal_authority_assignment_transitions WHERE journal_id=%d", "transition"},
		{"missing-supertype", "DELETE FROM journal WHERE journal_id=%d", "supertype"},
		{"transition-only-anchor", "DELETE FROM journal_authorities WHERE journal_id=%[1]d; DELETE FROM journal WHERE journal_id=%[1]d", "supertype"},
		{"authority-only-anchor", "DELETE FROM journal_authority_assignment_transitions WHERE journal_id=%[1]d; DELETE FROM journal WHERE journal_id=%[1]d", "supertype"},
		{"wrong-supertype", "UPDATE journal SET kind_id=0 WHERE journal_id=%d", "discriminator"},
		{"sole-start-ended", "UPDATE journal_authority_assignment_transitions SET transition_id=1 WHERE journal_id=%d", "prior-start"},
		{"unknown-transition", "UPDATE journal_authority_assignment_transitions SET transition_id=99 WHERE journal_id=%d", "transition"},
		{"unknown-authority", "UPDATE journal_authorities SET authority_kind_id=99 WHERE journal_id=%d", "subtype"},
		{"missing-episode", "DELETE FROM journal_authority_assignment_episodes WHERE assignment_id=(SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id=%d)", "episode"},
		{"bad-slot", "UPDATE journal_authority_assignment_episodes SET slot_id=99 WHERE assignment_id=(SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id=%d)", "slot"},
		{"bad-slot-lookup", "UPDATE assignment_slots SET name='wrong' WHERE id=(SELECT e.slot_id FROM journal_authority_assignment_episodes e JOIN journal_authority_assignment_transitions t USING(assignment_id) WHERE t.journal_id=%d)", "slot"},
		{"bad-task", "UPDATE journal_authority_assignment_episodes SET task_id='bad' WHERE assignment_id=(SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id=%d)", "task"},
		{"malformed-existing-task", "UPDATE tasks SET id='bad' WHERE id=(SELECT e.task_id FROM journal_authority_assignment_episodes e JOIN journal_authority_assignment_transitions t USING(assignment_id) WHERE t.journal_id=%[1]d); UPDATE journal_authority_assignment_episodes SET task_id='bad' WHERE assignment_id=(SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id=%[1]d)", "malformed task"},
		{"bad-actor", "UPDATE journal_authority_assignment_episodes SET actor_id='bad' WHERE assignment_id=(SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id=%d)", "actor"},
		{"malformed-existing-actor", "UPDATE agents SET id='bad' WHERE id=(SELECT e.actor_id FROM journal_authority_assignment_episodes e JOIN journal_authority_assignment_transitions t USING(assignment_id) WHERE t.journal_id=%[1]d); UPDATE journal_authority_assignment_episodes SET actor_id='bad' WHERE assignment_id=(SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id=%[1]d)", "malformed occupant"},
		{"bad-assignment", "UPDATE journal_authority_assignment_transitions SET assignment_id='' WHERE journal_id=%d", "episode"},
		{"bad-assignment-both", "UPDATE journal_authority_assignment_episodes SET assignment_id='' WHERE assignment_id=(SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id=%[1]d); UPDATE journal_authority_assignment_transitions SET assignment_id='' WHERE journal_id=%[1]d", "assignment"},
		{"bad-parent", "UPDATE journal_authority_assignment_episodes SET parent_assignment_id='' WHERE assignment_id=(SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id=%d)", "parent"},
		{"bad-predecessor", "UPDATE journal_authority_assignment_episodes SET predecessor_assignment_id='' WHERE assignment_id=(SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id=%d)", "predecessor"},
		{"missing-bootstrap-detail", "DELETE FROM journal_authority_bootstraps WHERE journal_id<%d", "bootstrap"},
		{"missing-producer", "DELETE FROM journal_operations WHERE journal_id=(SELECT produced_by_operation_journal_id FROM journal WHERE journal_id=%d)", "produc"},
		{"missing-producer-journal", "DELETE FROM journal WHERE journal_id=(SELECT produced_by_operation_journal_id FROM journal WHERE journal_id=%d)", "produc"},
		{"wrong-producer-kind", "UPDATE journal SET kind_id=1 WHERE journal_id=(SELECT produced_by_operation_journal_id FROM journal WHERE journal_id=%d)", "produc"},
		{"bad-operation", "UPDATE journal_operations SET operation_id='' WHERE journal_id=(SELECT produced_by_operation_journal_id FROM journal WHERE journal_id=%d)", "operation"},
		{"conflicting-bootstrap", "INSERT INTO journal_authority_bootstraps(journal_id,label) VALUES(%d,'conflict')", "bootstrap"},
		{"duplicate-transition", "CREATE TABLE duplicate_transitions AS SELECT * FROM journal_authority_assignment_transitions; DROP TABLE journal_authority_assignment_transitions; ALTER TABLE duplicate_transitions RENAME TO journal_authority_assignment_transitions; INSERT INTO journal_authority_assignment_transitions SELECT * FROM journal_authority_assignment_transitions WHERE journal_id=%d", "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := openFileDB(t)
			tr, err := p.OpenBorrowedSQLite(db, p.WithModelRegistry(p.NewRegistry(nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer tr.Close()
			a, b, task := assignmentQueryFixture(t, tr)
			start := assignmentQueryStart(t, tr, a, b, task, "start")
			api := assignmentQueryAPI(t, tr)
			q := p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 1}, AssignmentIDs: []p.AssignmentID{"does-not-match"}, OperationIDs: []p.OperationID{"other"}}
			control, err := api.QueryAssignmentStarts(q)
			if err != nil || len(control.Rows) != 0 {
				t.Fatalf("positive control: %+v %v", control, err)
			}
			// Keep the boundary above the damaged anchor, including when the anchor
			// is deleted. Candidate union discovery must not rely on journal alone.
			publicFactApply(t, tr, p.OperationInput{OperationID: "tail", ActorID: a, AuthorityJournalID: &b, CommandDigest: []byte("tail"), Effects: []p.Effect{{Sort: p.EffectTaskEvent, TaskID: task, EventKind: "query.tail", ResultSlot: "tail"}}})
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = conn.ExecContext(context.Background(), "PRAGMA foreign_keys=OFF"); err != nil {
				t.Fatal(err)
			}
			if _, err = conn.ExecContext(context.Background(), "PRAGMA ignore_check_constraints=ON"); err != nil {
				t.Fatal(err)
			}
			res, err := conn.ExecContext(context.Background(), fmt.Sprintf(tc.sql, start.FactID))
			if err != nil {
				t.Fatal(err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				t.Fatalf("mutation affected %d rows", n)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			_, err = api.QueryAssignmentStarts(q)
			if !errors.Is(err, p.ErrSubtypeIntegrity) || !strings.Contains(err.Error(), tc.token) || !strings.Contains(err.Error(), "fix:") {
				t.Fatalf("corruption omitted or not actionable (%s): %v", tc.name, err)
			}
		})
	}
}

func TestAssignmentStartsFilteredStartCursor(t *testing.T) {
	tr, actor := openGovernedTracker(t)
	root := initializeRoot(t, tr, actor)
	req := governedRequest("filtered-batch", actor, root.AssignmentID, 2)
	closure, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	children := closure.Children()
	api := assignmentQueryAPI(t, tr)
	q := p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 1, AfterJournalID: root.AssignmentRow.JournalID}, AssignmentIDs: []p.AssignmentID{children[1].AssignmentID}}
	first, err := api.QueryAssignmentStarts(q)
	if err != nil || len(first.Rows) != 0 || first.Next == nil || first.Next.AfterJournalID != children[0].AssignmentRow.JournalID {
		t.Fatalf("filtered start consumed prefix: %+v %v", first, err)
	}
	later := governedRequest("later-batch", actor, root.AssignmentID, 1)
	lateClosure, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(context.Background(), later)
	if err != nil {
		t.Fatal(err)
	}
	q.Page = p.AssignmentStartPageRequest{Limit: 1, SnapshotPinned: true, SnapshotMaxJournalID: first.SnapshotMaxJournalID, AfterJournalID: first.Next.AfterJournalID}
	second, err := api.QueryAssignmentStarts(q)
	if err != nil || len(second.Rows) != 1 || second.Rows[0].AuthorityJournalID != children[1].AssignmentRow.JournalID || second.Next != nil {
		t.Fatalf("lookahead skipped/repeated: %+v %v", second, err)
	}
	// Unfiltered continuation at the same boundary cannot see the intervening
	// real assignment. A fresh high-water request can see it.
	q.AssignmentIDs = nil
	second, err = api.QueryAssignmentStarts(q)
	if err != nil || len(second.Rows) != 1 || second.Next != nil {
		t.Fatalf("snapshot writer leaked: %+v %v", second, err)
	}
	q.Page = p.AssignmentStartPageRequest{Limit: 1, AfterJournalID: first.SnapshotMaxJournalID}
	fresh, err := api.QueryAssignmentStarts(q)
	if err != nil || len(fresh.Rows) != 1 || fresh.Rows[0].AuthorityJournalID != lateClosure.Children()[0].AssignmentRow.JournalID || fresh.SnapshotMaxJournalID <= first.SnapshotMaxJournalID {
		t.Fatalf("fresh high-water missed writer: %+v %v", fresh, err)
	}
}

func TestAssignmentStartsPublicValidation(t *testing.T) {
	tr, err := p.OpenMemory(p.WithModelRegistry(p.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	api := assignmentQueryAPI(t, tr)
	base := p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 1}}
	cases := map[string]func(*p.AssignmentStartQuery){
		"zero-limit":         func(q *p.AssignmentStartQuery) { q.Page.Limit = 0 },
		"large-limit":        func(q *p.AssignmentStartQuery) { q.Page.Limit = p.MaxFactPageSize + 1 },
		"negative-snapshot":  func(q *p.AssignmentStartQuery) { q.Page.SnapshotMaxJournalID = -1 },
		"negative-cursor":    func(q *p.AssignmentStartQuery) { q.Page.AfterJournalID = -1 },
		"unpinned-nonzero":   func(q *p.AssignmentStartQuery) { q.Page.SnapshotMaxJournalID = 1 },
		"pinned-zero-cursor": func(q *p.AssignmentStartQuery) { q.Page.SnapshotPinned = true; q.Page.AfterJournalID = 1 },
		"missing-boundary":   func(q *p.AssignmentStartQuery) { q.Page.SnapshotPinned = true; q.Page.SnapshotMaxJournalID = 123 },
		"past-boundary": func(q *p.AssignmentStartQuery) {
			q.Page.SnapshotPinned = true
			q.Page.SnapshotMaxJournalID = 1
			q.Page.AfterJournalID = 2
		},
		"fresh-past-max":   func(q *p.AssignmentStartQuery) { q.Page.AfterJournalID = 1 },
		"task":             func(q *p.AssignmentStartQuery) { q.TaskIDs = []p.TaskID{{}} },
		"actor":            func(q *p.AssignmentStartQuery) { q.ActorIDs = []p.ActorID{{}} },
		"assignment":       func(q *p.AssignmentStartQuery) { q.AssignmentIDs = []p.AssignmentID{"bad\n"} },
		"operation":        func(q *p.AssignmentStartQuery) { q.OperationIDs = []p.OperationID{""} },
		"slot":             func(q *p.AssignmentStartQuery) { q.SlotIDs = []p.AssignmentSlotID{""} },
		"many-tasks":       func(q *p.AssignmentStartQuery) { q.TaskIDs = make([]p.TaskID, p.MaxFactFilterValues+1) },
		"many-actors":      func(q *p.AssignmentStartQuery) { q.ActorIDs = make([]p.ActorID, p.MaxFactFilterValues+1) },
		"many-assignments": func(q *p.AssignmentStartQuery) { q.AssignmentIDs = make([]p.AssignmentID, p.MaxFactFilterValues+1) },
		"many-operations":  func(q *p.AssignmentStartQuery) { q.OperationIDs = make([]p.OperationID, p.MaxFactFilterValues+1) },
		"many-slots": func(q *p.AssignmentStartQuery) {
			q.SlotIDs = []p.AssignmentSlotID{p.SlotOwnerResponsibility, p.SlotOwnerResponsibility}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			q := base
			mutate(&q)
			_, err := api.QueryAssignmentStarts(q)
			if !errors.Is(err, p.ErrInvalidQuery) || !strings.Contains(err.Error(), "fix:") {
				t.Fatalf("validation: %v", err)
			}
		})
	}
	// A malformed slot is rejected without even leasing a closed DB.
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	base.SlotIDs = []p.AssignmentSlotID{"unknown"}
	_, err = api.QueryAssignmentStarts(base)
	if !errors.Is(err, p.ErrInvalidQuery) {
		t.Fatalf("slot validation did SQL first: %v", err)
	}
}

func TestAssignmentStartsPublicGovernedChildrenBelowWatermark(t *testing.T) {
	tr, actor := openGovernedTracker(t)
	root := initializeRoot(t, tr, actor)
	req := governedRequest("recover-batch", actor, root.AssignmentID, 4)
	closure, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	api := assignmentQueryAPI(t, tr)
	page, err := api.QueryAssignmentStarts(p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 8}})
	if err != nil || len(page.Rows) != 5 || page.Next != nil {
		t.Fatalf("governed page: %+v %v", page, err)
	}
	// A consumer's arbitrary later material watermark does not hide starts when
	// it rebuilds from zero. Governed allocation emits no consumer material facts.
	watermark := page.SnapshotMaxJournalID + 100
	for i, child := range closure.Children() {
		r := page.Rows[i+1]
		if r.AuthorityJournalID != child.AssignmentRow.JournalID || r.AuthorityJournalID >= watermark || r.TaskID != req.Children[i].TaskID || r.AssignmentID != req.Children[i].AssignmentID || r.Occupant != actor || r.ParentAssignmentID == nil || *r.ParentAssignmentID != root.AssignmentID || r.ProducingOperationID != req.OperationID {
			t.Fatalf("child %d: %+v; closure %+v", i, r, child)
		}
		committed, err := tr.Journal().LookupCommitted(req.OperationID)
		if err != nil || r.ProducingOperationJournalID != committed.AnchorJournalID {
			t.Fatalf("child producer anchor: %+v %v", committed, err)
		}
	}
	q := p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 8}, TaskIDs: []p.TaskID{req.Children[1].TaskID, req.Children[0].TaskID, req.Children[0].TaskID}, ActorIDs: []p.ActorID{actor, actor}, OperationIDs: []p.OperationID{req.OperationID}, SlotIDs: []p.AssignmentSlotID{p.SlotOwnerResponsibility}}
	filtered, err := api.QueryAssignmentStarts(q)
	if err != nil || len(filtered.Rows) != 2 || filtered.Rows[0].TaskID != req.Children[0].TaskID || filtered.Rows[1].TaskID != req.Children[1].TaskID {
		t.Fatalf("OR/AND filters: %+v %v", filtered, err)
	}
	q.AssignmentIDs = []p.AssignmentID{req.Children[1].AssignmentID}
	filtered, err = api.QueryAssignmentStarts(q)
	if err != nil || len(filtered.Rows) != 1 || filtered.Rows[0].AssignmentID != req.Children[1].AssignmentID {
		t.Fatalf("AND assignment: %+v %v", filtered, err)
	}
	facts, err := tr.Journal().QueryTaskEvents(p.JournalQueryV1{TaskIDs: []p.TaskID{req.Children[0].TaskID}, EventKinds: []p.EventKind{"consumer.assignment.started"}, Limit: 8})
	if err != nil || len(facts.Events) != 0 {
		t.Fatalf("consumer material facts unexpectedly present: %+v %v", facts, err)
	}
}

func TestAssignmentStartsPublicOrphanAuditBoundary(t *testing.T) {
	db, _ := openFileDB(t)
	tr, err := p.OpenBorrowedSQLite(db, p.WithModelRegistry(p.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	a, b, task := assignmentQueryFixture(t, tr)
	assignmentQueryStart(t, tr, a, b, task, "valid")
	if err := tr.Journal().VerifyIntegrity(); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO journal_authority_assignment_episodes(assignment_id,task_id,slot_id,actor_id) SELECT 'orphan',task_id,slot_id,actor_id FROM journal_authority_assignment_episodes WHERE assignment_id='valid'`)
	if err != nil {
		t.Fatal(err)
	}
	page, err := assignmentQueryAPI(t, tr).QueryAssignmentStarts(p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 8}})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].AssignmentID != "valid" || page.Next != nil {
		t.Fatalf("anchor-only boundary: %+v %v", page, err)
	}
	err = tr.Journal().VerifyIntegrity()
	if !errors.Is(err, p.ErrSubtypeIntegrity) || !strings.Contains(err.Error(), "orphan") || !strings.Contains(err.Error(), "no transition") {
		t.Fatalf("whole-store audit missed orphan: %v", err)
	}
}

func assignmentQueryAPI(t *testing.T, tr p.Tracker) p.AssignmentStartQueryAPI {
	t.Helper()
	api, ok := tr.Journal().(p.AssignmentStartQueryAPI)
	if !ok {
		t.Fatal("public Journal lacks AssignmentStartQueryAPI")
	}
	return api
}

func assignmentQueryFixture(t *testing.T, tr p.Tracker) (p.ActorID, p.JournalID, p.TaskID) {
	t.Helper()
	agent, err := tr.RegisterSoftwareAgent("assignment-query", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	a := agent.Agent.ID
	b := publicFactGenesis(t, tr, a, "query-bootstrap")
	task, err := tr.As(a, b).Create("assignment-query", "task", "", p.TaskTypeTask, p.PriorityMedium, p.PhaseUnscoped)
	if err != nil {
		t.Fatal(err)
	}
	return a, b, task.ID
}

func assignmentQueryStart(t *testing.T, tr p.Tracker, a p.ActorID, b p.JournalID, task p.TaskID, id p.AssignmentID) publicFactResult {
	t.Helper()
	return publicFactApply(t, tr, p.OperationInput{OperationID: p.OperationID(id), ActorID: a, AuthorityJournalID: &b, CommandDigest: []byte(id), RecordedAt: 123,
		Effects: []p.Effect{{Sort: p.EffectAssignmentStart, ResultSlot: "start", AssignmentID: id, TaskID: task, SlotID: p.SlotOwnerResponsibility, Occupant: a}}})
}

func TestAssignmentStartsPublicPaginationSnapshot(t *testing.T) {
	tr, err := p.OpenSQLite(filepath.Join(t.TempDir(), "query.db"), p.WithModelRegistry(p.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	api := assignmentQueryAPI(t, tr)
	a, b, task := assignmentQueryFixture(t, tr)
	start := assignmentQueryStart(t, tr, a, b, task, "first")
	q := p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 1}, AssignmentIDs: []p.AssignmentID{"first", "first"}}
	page, err := api.QueryAssignmentStarts(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 0 || page.Next == nil || page.Next.AfterJournalID != b || !page.SnapshotPinned || !page.Next.SnapshotPinned {
		t.Fatalf("filtered bootstrap page: %+v", page)
	}
	// The write is strictly between pages. Continuation must keep the first
	// boundary, not observe the new bootstrap authority.
	publicFactApply(t, tr, p.OperationInput{OperationID: "later-bootstrap", ActorID: a, AuthorityJournalID: &b, CommandDigest: []byte("later"), Effects: []p.Effect{{Sort: p.EffectBootstrapAuthority, ResultSlot: "auth", BootstrapLabel: "later"}}})
	q.Page = p.AssignmentStartPageRequest{Limit: 1, SnapshotPinned: true, SnapshotMaxJournalID: page.Next.SnapshotMaxJournalID, AfterJournalID: page.Next.AfterJournalID}
	second, err := api.QueryAssignmentStarts(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Rows) != 1 || second.Next != nil || second.SnapshotMaxJournalID != page.SnapshotMaxJournalID {
		t.Fatalf("continuation: %+v", second)
	}
	r := second.Rows[0]
	if r.AuthorityJournalID != start.FactID || r.AssignmentID != "first" || r.TaskID != task || r.Occupant != a || r.SlotID != p.SlotOwnerResponsibility || r.ProducingOperationID != "first" || r.ProducingOperationJournalID != start.Anchor || !r.RecordedAt.Equal(time.Unix(0, 123).UTC()) || r.ParentAssignmentID != nil || r.PredecessorAssignmentID != nil {
		t.Fatalf("wrong start identity: %+v", r)
	}
	q.Page.AfterJournalID = start.FactID
	exhausted, err := api.QueryAssignmentStarts(q)
	if err != nil || len(exhausted.Rows) != 0 || exhausted.Next != nil {
		t.Fatalf("exhausted: %+v %v", exhausted, err)
	}
}

func TestAssignmentStartsPublicPinnedZero(t *testing.T) {
	tr, err := p.OpenMemory(p.WithModelRegistry(p.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	api := assignmentQueryAPI(t, tr)
	q := p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 1}}
	empty, err := api.QueryAssignmentStarts(q)
	if err != nil || !empty.SnapshotPinned || empty.SnapshotMaxJournalID != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	assignmentQueryFixture(t, tr)
	q.Page.SnapshotPinned = true
	pinned, err := api.QueryAssignmentStarts(q)
	if err != nil || pinned.SnapshotMaxJournalID != 0 || len(pinned.Rows) != 0 || pinned.Next != nil {
		t.Fatalf("pinned zero re-resolved: %+v %v", pinned, err)
	}
	q.Page.SnapshotPinned = false
	fresh, err := api.QueryAssignmentStarts(q)
	if err != nil || fresh.SnapshotMaxJournalID == 0 {
		t.Fatalf("fresh missed writer: %+v %v", fresh, err)
	}
}

func TestAssignmentStartsPublicBorrowedLifecycle(t *testing.T) {
	db, _ := openFileDB(t)
	tr, err := p.OpenBorrowedSQLite(db, p.WithModelRegistry(p.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	api := assignmentQueryAPI(t, tr)
	a, b, task := assignmentQueryFixture(t, tr)
	start := assignmentQueryStart(t, tr, a, b, task, "borrowed-start")
	q := p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 4}}
	page, err := api.QueryAssignmentStarts(q)
	if err != nil || len(page.Rows) != 1 || page.Rows[0].AuthorityJournalID != start.FactID {
		t.Fatalf("borrowed query: %+v %v", page, err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("caller pool closed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = api.QueryAssignmentStarts(q)
	var unavailable *p.StoreUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "QueryAssignmentStarts") || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("owner close must return typed actionable lifecycle error: %v", err)
	}
}
