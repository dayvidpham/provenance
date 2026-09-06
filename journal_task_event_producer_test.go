package provenance_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	p "github.com/dayvidpham/provenance"
	_ "modernc.org/sqlite"
)

// Each case owns a fresh file and pool. No shared fixture or process state is
// changed; the borrowed case deliberately uses one connection to catch leaks.
func openTaskEventProducerTracker(t *testing.T, borrowed bool) p.Tracker {
	t.Helper()
	path := filepath.Join(t.TempDir(), "producer.db")
	if !borrowed {
		tr, err := p.OpenSQLite(path, p.WithModelRegistry(p.NewRegistry(nil)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := tr.Close(); err != nil {
				t.Error(err)
			}
		})
		return tr
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	tr, err := p.OpenBorrowedSQLite(db, p.WithModelRegistry(p.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	j := tr.Journal()
	t.Cleanup(func() {
		if err := tr.Close(); err != nil {
			t.Error(err)
		}
		if _, err := j.QueryTaskEvents(p.JournalQueryV1{}); err == nil {
			t.Error("closed borrowed QueryTaskEvents succeeded")
		}
		if err := db.Ping(); err != nil {
			t.Errorf("borrowed close invalidated caller pool: %v", err)
		}
		if db.Stats().MaxOpenConnections != 1 {
			t.Error("borrowed query changed caller pool limit")
		}
	})
	return tr
}

func taskEventProducerSlot(t *testing.T, result p.CommittedResult, slot p.ResultSlotID) p.JournalID {
	t.Helper()
	for _, binding := range result.ResultSlots {
		if binding.Slot == slot {
			return binding.ProducedJournalID
		}
	}
	t.Fatalf("committed operation %d missing slot %q", result.AnchorJournalID, slot)
	return 0
}

func requireTaskEventProducer(t *testing.T, row p.TaskEventRow, want p.JournalID) {
	t.Helper()
	// The operations schema forbids bare NULL-producer task events. An operation
	// anchor is not a task event; nil here is not a legacy compatibility case.
	if row.ProducedByOperationJournalID == nil {
		t.Fatalf("material producer is nil for task event %d; want actual operation anchor %d", row.JournalID, want)
	}
	if *row.ProducedByOperationJournalID != want {
		t.Fatalf("material producer = %d for task event %d; want actual operation anchor %d", *row.ProducedByOperationJournalID, row.JournalID, want)
	}
}

func TestTaskEventProducerSameApplyAndPaging(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		t.Run(fmt.Sprintf("borrowed=%t", borrowed), func(t *testing.T) {
			tr := openTaskEventProducerTracker(t, borrowed)
			agent, err := tr.RegisterSoftwareAgent("producer", "committer", "1", "test")
			if err != nil {
				t.Fatal(err)
			}
			occupant, err := tr.RegisterSoftwareAgent("producer", "occupant", "1", "test")
			if err != nil {
				t.Fatal(err)
			}
			actor := agent.Agent.ID
			bootstrap, err := tr.Journal().Apply(p.OperationInput{OperationID: "bootstrap", ActorID: actor, CommandDigest: []byte("bootstrap"), Effects: []p.Effect{{Sort: p.EffectBootstrapAuthority, ResultSlot: "auth", BootstrapLabel: "producer"}}})
			if err != nil {
				t.Fatal(err)
			}
			auth := taskEventProducerSlot(t, bootstrap, "auth")
			task, err := tr.As(actor, auth).Create("producer", "task", "", p.TaskTypeTask, p.PriorityMedium, p.PhaseUnscoped)
			if err != nil {
				t.Fatal(err)
			}
			other, err := tr.As(actor, auth).Create("producer", "other", "", p.TaskTypeTask, p.PriorityMedium, p.PhaseUnscoped)
			if err != nil {
				t.Fatal(err)
			}
			taskContext, err := p.TaskContext(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			actorContext, err := p.ActorContext(occupant.Agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			contexts := []p.EventContext{actorContext, taskContext}
			const kind p.EventKind = "example.assignment.started.v1"
			stamp := time.Date(2026, 1, 2, 3, 4, 5, 123, time.UTC)
			payload := json.RawMessage(`{"assignment":"episode","role":"owner-responsibility"}`)
			first, err := tr.Journal().Apply(p.OperationInput{OperationID: "start", ActorID: actor, AuthorityJournalID: &auth, CommandDigest: []byte("start"), RecordedAt: stamp.UnixNano(), Effects: []p.Effect{
				{Sort: p.EffectAssignmentStart, ResultSlot: "start", AssignmentID: "episode", TaskID: task.ID, SlotID: p.SlotOwnerResponsibility, Occupant: occupant.Agent.ID},
				{Sort: p.EffectTaskEvent, ResultSlot: "material", TaskID: task.ID, EventKind: kind, Payload: payload, Contexts: contexts},
			}})
			if err != nil {
				t.Fatal(err)
			}
			api, ok := tr.Journal().(p.AssignmentStartQueryAPI)
			if !ok {
				t.Fatal("journal lacks assignment-start query")
			}
			starts, err := api.QueryAssignmentStarts(p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 64}})
			if err != nil {
				t.Fatal(err)
			}
			if len(starts.Rows) != 1 || starts.Next != nil || starts.Rows[0].ProducingOperationJournalID != first.AnchorJournalID {
				t.Fatalf("same-Apply assignment producer: %+v, want anchor %d", starts, first.AnchorJournalID)
			}
			for _, order := range []p.OrderDimension{p.OrderByJournalID, p.OrderByRecordedAt} {
				page, err := tr.Journal().QueryTaskEvents(p.JournalQueryV1{OrderBy: order, TaskIDs: []p.TaskID{task.ID}, EventKinds: []p.EventKind{kind}, SnapshotMaxJournalID: starts.SnapshotMaxJournalID, Limit: 1})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Events) != 1 || page.Next != nil {
					t.Fatalf("same-Apply page: %+v", page)
				}
				requireTaskEventProducer(t, page.Events[0], first.AnchorJournalID)
			}
			want := map[p.JournalID]p.TaskEventRow{}
			add := func(result p.CommittedResult, slot p.ResultSlotID, taskID p.TaskID, event p.EventKind, at time.Time, body json.RawMessage, ctxs []p.EventContext) {
				id := taskEventProducerSlot(t, result, slot)
				producer := result.AnchorJournalID
				want[id] = p.TaskEventRow{Row: p.Row{JournalID: id, Kind: p.JournalKindTaskEvent, ActorID: actor, RecordedAt: at, ProducedByOperationJournalID: &producer}, TaskID: taskID, EventKind: event, Payload: body, Contexts: ctxs}
			}
			add(first, "material", task.ID, kind, stamp, payload, contexts)
			// A second operation shares a timestamp, and a third is backdated.
			// Distinct producers prevent a sticky scanner pointer from passing.
			for i, at := range []time.Time{stamp, stamp.Add(-time.Hour)} {
				body := json.RawMessage(fmt.Sprintf(`{"index":%d}`, i))
				result, err := tr.Journal().Apply(p.OperationInput{OperationID: p.OperationID(fmt.Sprintf("more-%d", i)), ActorID: actor, AuthorityJournalID: &auth, CommandDigest: []byte(fmt.Sprintf("more-%d", i)), RecordedAt: at.UnixNano(), Effects: []p.Effect{
					{Sort: p.EffectTaskEvent, ResultSlot: "material", TaskID: task.ID, EventKind: kind, Payload: body, Contexts: contexts},
					{Sort: p.EffectTaskEvent, ResultSlot: "other-task", TaskID: other.ID, EventKind: kind, Payload: body},
					{Sort: p.EffectTaskEvent, ResultSlot: "other-kind", TaskID: task.ID, EventKind: "example.other.v1", Payload: body},
					{Sort: p.EffectTaskEvent, ResultSlot: "without-context", TaskID: task.ID, EventKind: kind, Payload: body},
				}})
				if err != nil {
					t.Fatal(err)
				}
				add(result, "material", task.ID, kind, at, body, contexts)
				add(result, "other-task", other.ID, kind, at, body, nil)
				add(result, "other-kind", task.ID, "example.other.v1", at, body, nil)
				add(result, "without-context", task.ID, kind, at, body, nil)
			}
			if err := tr.Journal().VerifyIntegrity(); err != nil {
				t.Fatal(err)
			}
			for _, order := range []p.OrderDimension{p.OrderByJournalID, p.OrderByRecordedAt} {
				for _, filtered := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/filtered=%t", order, filtered), func(t *testing.T) {
						q := p.JournalQueryV1{OrderBy: order, EventKinds: []p.EventKind{kind, "example.other.v1"}, Limit: 1}
						var ids []p.JournalID
						for id, row := range want {
							if !filtered || row.TaskID == task.ID && row.EventKind == kind && len(row.Contexts) != 0 {
								ids = append(ids, id)
							}
						}
						if filtered {
							q.TaskIDs = []p.TaskID{task.ID}
							q.EventKinds = []p.EventKind{kind}
							q.Contexts = []p.EventContext{taskContext}
						}
						sort.Slice(ids, func(i, j int) bool {
							a, b := want[ids[i]], want[ids[j]]
							if order == p.OrderByRecordedAt && !a.RecordedAt.Equal(b.RecordedAt) {
								return a.RecordedAt.Before(b.RecordedAt)
							}
							return a.JournalID < b.JournalID
						})
						for i, id := range ids {
							page, err := tr.Journal().QueryTaskEvents(q)
							if err != nil {
								t.Fatal(err)
							}
							if len(page.Events) != 1 || page.Events[0].JournalID != id {
								t.Fatalf("page %d = %+v, want event %d", i, page, id)
							}
							got, expected := page.Events[0], want[id]
							requireTaskEventProducer(t, got, *expected.ProducedByOperationJournalID)
							if !reflect.DeepEqual(got, expected) {
								t.Fatalf("decoded event = %+v, want %+v", got, expected)
							}
							if q.SnapshotMaxJournalID != 0 && page.SnapshotMaxJournalID != q.SnapshotMaxJournalID {
								t.Fatal("snapshot changed across pages")
							}
							if i == len(ids)-1 {
								if page.Next != nil {
									t.Fatalf("final page has cursor: %+v", page.Next)
								}
							} else {
								if page.Next == nil || page.Next.AfterJournalID != id || page.Next.SnapshotMaxJournalID != page.SnapshotMaxJournalID {
									t.Fatalf("bad cursor: %+v", page.Next)
								}
								if order == p.OrderByRecordedAt && !page.Next.AfterRecordedAt.Equal(expected.RecordedAt) {
									t.Fatal("timeline cursor lost timestamp")
								}
								q.SnapshotMaxJournalID, q.AfterJournalID, q.AfterRecordedAt = page.Next.SnapshotMaxJournalID, page.Next.AfterJournalID, page.Next.AfterRecordedAt
							}
						}
					})
				}
			}
		})
	}
}

func TestTaskEventProducerComposedSupplement(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		t.Run(fmt.Sprintf("borrowed=%t", borrowed), func(t *testing.T) {
			tr := openTaskEventProducerTracker(t, borrowed)
			actor := registerGovernedActor(t, tr, "producer-composed")
			root := initializeRoot(t, tr, actor)
			request := composedGovernedRequest("producer-composed", actor, root, 1)
			const materialKind p.EventKind = "example.assignment.started.v1"
			payload, err := json.Marshal(map[string]string{
				"assignment": string(request.Allocation.Children[0].AssignmentID),
				"occupant":   actor.String(),
				"role":       "axis-reviewer",
				"operation":  string(request.Allocation.OperationID),
			})
			if err != nil {
				t.Fatal(err)
			}
			for i := range request.SupplementalEffects {
				if request.SupplementalEffects[i].ResultSlot == "slice-event" {
					request.SupplementalEffects[i].EventKind = materialKind
					request.SupplementalEffects[i].Payload = payload
				}
			}
			result, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGovernedComposedBatch(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			child := result.Closure().Children()[0]
			// Look up the actual reducer-owned operation, not a manufactured Apply
			// or a JournalID guessed from adjacency to the allocation anchor.
			supplement, err := tr.Journal().LookupCommitted(p.GovernedAllocationSupplementOperationID(request.Allocation.OperationID))
			if err != nil {
				t.Fatal(err)
			}
			if supplement.Kind != p.CommittedExact || supplement.AnchorJournalID == 0 || supplement.AnchorJournalID == result.Closure().AnchorJournalID() {
				t.Fatalf("invalid actual supplement: %+v", supplement)
			}
			if !reflect.DeepEqual(supplement.ResultSlots, result.SupplementalResultSlots()) || !reflect.DeepEqual(supplement.EmittedEvents, result.SupplementalEmittedEvents()) {
				t.Fatal("actual operation does not match composed supplemental receipt")
			}
			api, ok := tr.Journal().(p.AssignmentStartQueryAPI)
			if !ok {
				t.Fatal("journal lacks assignment-start query")
			}
			starts, err := api.QueryAssignmentStarts(p.AssignmentStartQuery{Page: p.AssignmentStartPageRequest{Limit: 64}})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, start := range starts.Rows {
				if start.AssignmentID == child.AssignmentID {
					found = true
					if start.ProducingOperationJournalID != result.Closure().AnchorJournalID() {
						t.Fatal("assignment producer is not the allocation anchor")
					}
				}
			}
			if !found || starts.Next != nil {
				t.Fatalf("missing composed assignment: %+v", starts)
			}
			for _, order := range []p.OrderDimension{p.OrderByJournalID, p.OrderByRecordedAt} {
				page, err := tr.Journal().QueryTaskEvents(p.JournalQueryV1{OrderBy: order, TaskIDs: []p.TaskID{child.TaskID}, EventKinds: []p.EventKind{materialKind}, Limit: 1, SnapshotMaxJournalID: starts.SnapshotMaxJournalID})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Events) != 1 || page.Next != nil || page.Events[0].JournalID != taskEventProducerSlot(t, supplement, "slice-event") {
					t.Fatalf("missing supplemental material: %+v", page)
				}
				requireTaskEventProducer(t, page.Events[0], supplement.AnchorJournalID)
				if page.Events[0].ActorID != actor || page.Events[0].TaskID != child.TaskID || string(page.Events[0].Payload) != string(payload) {
					t.Fatalf("bad supplemental material: %+v", page.Events[0])
				}
			}
			if err := tr.Journal().VerifyIntegrity(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
