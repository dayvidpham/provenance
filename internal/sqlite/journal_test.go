package sqlite

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func newJournalDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedActorAndTask registers one agent and one task directly, satisfying the
// journal/task_event foreign keys, and returns their IDs.
func seedActorAndTask(t *testing.T, db *DB) (journal.ActorID, journal.TaskID) {
	t.Helper()
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	db.Lock()
	err := sqlitex.Execute(db.Conn(),
		`INSERT INTO agents (id, kind_id) VALUES (?1, ?2)`,
		&sqlitex.ExecOptions{Args: []any{actor.String(), int(ptypes.AgentKindSoftware)}})
	if err == nil {
		err = sqlitex.Execute(db.Conn(),
			`INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1,?2,?3,?4)`,
			&sqlitex.ExecOptions{Args: []any{actor.String(), "harness", "0", "test"}})
	}
	db.Unlock()
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	task := ptypes.Task{
		ID:        ptypes.TaskID{Namespace: "provenance-test", UUID: uuid.New()},
		Title:     "t",
		Phase:     ptypes.PhaseUnscoped,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := db.SeedLegacyTaskRow(task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return actor, task.ID
}

// genesisBoot applies one genesis operation establishing the pasture-system bootstrap
// authority — which governs every task (§14.1) — and returns its produced JournalID.
// The producer constraint forbids bare (NULL-producer) task events, so query and
// ordering tests emit events through operations under this authority. The
// genesis consumes two journal rows: the operation anchor (journal_id 1) and the
// bootstrap authority it produces (journal_id 2, the returned value).
func genesisBoot(t *testing.T, db *DB, actor journal.ActorID) journal.JournalID {
	t.Helper()
	res, err := db.Apply(journal.OperationInput{
		OperationID:    "op-genesis",
		ActorID:        actor,
		CommandDigest:  []byte("genesis-c"),
		MutationDigest: []byte("genesis-m"),
		RecordedAt:     time.Now().UTC().UnixNano(),
		Effects:        []journal.Effect{{Sort: journal.EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("genesisBoot: %v", err)
	}
	for i := range res.ResultSlots {
		if string(res.ResultSlots[i].Slot) == "auth" {
			return res.ResultSlots[i].ProducedJournalID
		}
	}
	t.Fatal("genesisBoot: no bootstrap authority result slot")
	return 0
}

// opEvent is one task event to emit through appendEventsOp.
type opEvent struct {
	kind       journal.EventKind
	recordedAt time.Time
	contexts   []journal.EventContext
	payload    json.RawMessage
}

type hostileContextID string

func (hostileContextID) EventContextDomain() journal.EventContextKind {
	return "fixture.hostile"
}

// appendEventsOp emits the given task events as ONE operation under boot (each event's
// RecordedAt carried via a per-effect override, §12), the operation-anchored replacement
// for a run of bare AppendTaskEvent calls. Because one operation produces one anchor row
// then its N events contiguously, the returned task-event JournalIDs are contiguous
// (offset past the genesis rows and this operation's anchor). Returns them in effect
// order.
func appendEventsOp(t *testing.T, db *DB, boot journal.JournalID, actor journal.ActorID, task journal.TaskID, opID string, evs []opEvent) []journal.JournalID {
	t.Helper()
	auth := boot
	effects := make([]journal.Effect, len(evs))
	for i := range evs {
		ra := evs[i].recordedAt.UTC().UnixNano()
		effects[i] = journal.Effect{
			Sort:               journal.EffectTaskEvent,
			TaskID:             task,
			EventKind:          evs[i].kind,
			Payload:            evs[i].payload,
			Contexts:           evs[i].contexts,
			RecordedAtOverride: &ra,
			ResultSlot:         journal.ResultSlotID(fmt.Sprintf("e%d", i)),
		}
	}
	res, err := db.Apply(journal.OperationInput{
		OperationID:        journal.OperationID(opID),
		ActorID:            actor,
		AuthorityJournalID: &auth,
		CommandDigest:      []byte(opID + "-c"),
		MutationDigest:     []byte(opID + "-m"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects:            effects,
	})
	if err != nil {
		t.Fatalf("appendEventsOp %q: %v", opID, err)
	}
	jids := make([]journal.JournalID, len(evs))
	for i := range evs {
		slot := journal.ResultSlotID(fmt.Sprintf("e%d", i))
		ok := false
		for j := range res.ResultSlots {
			if res.ResultSlots[j].Slot == slot {
				jids[i] = res.ResultSlots[j].ProducedJournalID
				ok = true
			}
		}
		if !ok {
			t.Fatalf("appendEventsOp %q: no result slot %q", opID, slot)
		}
	}
	return jids
}

// findEvent returns the queried task-event row with the given JournalID.
func findEvent(t *testing.T, db *DB, jid journal.JournalID) journal.TaskEventRow {
	t.Helper()
	page, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID})
	if err != nil {
		t.Fatalf("findEvent query: %v", err)
	}
	for _, ev := range page.Events {
		if ev.JournalID == jid {
			return ev
		}
	}
	t.Fatalf("findEvent: no task event with JournalID %d", jid)
	return journal.TaskEventRow{}
}

func TestQueryTaskEventsTreatsHostileTaskAndContextFiltersAsData(t *testing.T) {
	for _, hostile := range []string{"' OR 1=1 --", "x' /* comment */ OR '1'='1", "nul\x00suffix"} {
		t.Run(fmt.Sprintf("%q", hostile), func(t *testing.T) {
			db := newJournalDB(t)
			actor, _ := seedActorAndTask(t, db)
			boot := genesisBoot(t, db, actor)
			task := ptypes.Task{
				ID:        ptypes.TaskID{Namespace: hostile, UUID: uuid.New()},
				Title:     "hostile filter fixture",
				Phase:     ptypes.PhaseUnscoped,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}
			if err := db.SeedLegacyTaskRow(task); err != nil {
				t.Fatalf("seed hostile task: %v", err)
			}
			descriptor, err := journal.DefineExtensionContext[hostileContextID](func(hostileContextID) error { return nil })
			if err != nil {
				t.Fatalf("define context: %v", err)
			}
			context, err := journal.ExtensionContext(descriptor, hostileContextID(hostile))
			if err != nil {
				t.Fatalf("construct hostile context: %v", err)
			}
			ids := appendEventsOp(t, db, boot, actor, task.ID, "op-hostile", []opEvent{{
				kind: journal.EventKindTaskUpdated, recordedAt: time.Now().UTC(), contexts: []journal.EventContext{context}, payload: json.RawMessage(`{"safe":true}`),
			}})
			page, err := db.QueryTaskEvents(journal.JournalQueryV1{
				OrderBy:    journal.OrderByJournalID,
				TaskIDs:    []journal.TaskID{task.ID},
				EventKinds: []journal.EventKind{journal.EventKindTaskUpdated},
				Contexts:   []journal.EventContext{context},
			})
			if err != nil || len(page.Events) != 1 || page.Events[0].JournalID != ids[0] {
				t.Fatalf("hostile query filter escaped binding: events=%v err=%v", page.Events, err)
			}
		})
	}
}

func TestJournalKindsSeededMatchGoEnum(t *testing.T) {
	db := newJournalDB(t)
	got := map[int]string{}
	db.Lock()
	err := sqlitex.Execute(db.Conn(),
		`SELECT id, name FROM journal_kinds ORDER BY id`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			got[stmt.ColumnInt(0)] = stmt.ColumnText(1)
			return nil
		}})
	db.Unlock()
	if err != nil {
		t.Fatalf("read journal_kinds: %v", err)
	}
	for _, k := range journal.JournalKinds() {
		if got[int(k)] != k.String() {
			t.Errorf("journal_kinds[%d] = %q, want %q (SQL/Go enum drift)", int(k), got[int(k)], k.String())
		}
	}
	if len(got) != len(journal.JournalKinds()) {
		t.Errorf("journal_kinds has %d rows, want %d", len(got), len(journal.JournalKinds()))
	}
}

func TestAppendTaskEventAdvancesProjections(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)

	ctx, err := journal.TaskContext(task)
	if err != nil {
		t.Fatal(err)
	}
	jids := appendEventsOp(t, db, boot, actor, task, "op-append-1", []opEvent{
		{kind: "provenance.task.created", recordedAt: time.Unix(1000, 0),
			payload: json.RawMessage(`{"k":"v"}`), contexts: []journal.EventContext{ctx, ctx}}, // dedups to one
	})
	evJID := jids[0]
	if ev := findEvent(t, db, evJID); len(ev.Contexts) != 1 {
		t.Errorf("contexts = %d, want 1 (deduped)", len(ev.Contexts))
	}

	// Watermark advanced on the projection to this event's JournalID.
	var watermark int64
	db.Lock()
	_ = sqlitex.Execute(db.Conn(),
		`SELECT last_journal_id FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{Args: []any{task.String()},
			ResultFunc: func(stmt *zs.Stmt) error { watermark = stmt.ColumnInt64(0); return nil }})
	db.Unlock()
	if watermark != int64(evJID) {
		t.Errorf("tasks.last_journal_id = %d, want %d", watermark, evJID)
	}

	// Attribution is first-wins: a later event by the same actor must not move
	// the FirstJournalID.
	appendEventsOp(t, db, boot, actor, task, "op-append-2", []opEvent{
		{kind: "provenance.task.updated", recordedAt: time.Unix(2000, 0)},
	})
	attrs, err := db.TaskAttributions(task)
	if err != nil {
		t.Fatal(err)
	}
	if len(attrs) != 1 {
		t.Fatalf("attributions = %d, want 1", len(attrs))
	}
	if attrs[0].FirstJournalID != evJID {
		t.Errorf("attribution FirstJournalID = %d, want %d (append-only, first wins)", attrs[0].FirstJournalID, evJID)
	}
}

func TestQueryTaskEventsOrdersByJournalIDWithPaging(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)

	// Emit 5 events as one operation, deliberately backdating RecordedAt so a
	// RecordedAt sort would reverse them. One operation produces one anchor then its 5
	// events contiguously, so the task-event JournalIDs are consecutive (jids[0..4]).
	evs := make([]opEvent, 5)
	for i := range evs {
		evs[i] = opEvent{kind: "provenance.task.updated", recordedAt: time.Unix(int64(1000-i), 0)}
	}
	jids := appendEventsOp(t, db, boot, actor, task, "op-events", evs)

	// Page size 2 across the snapshot.
	page, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Next == nil {
		t.Fatalf("first page: got %d events, next=%v", len(page.Events), page.Next)
	}
	if page.Events[0].JournalID != jids[0] || page.Events[1].JournalID != jids[1] {
		t.Errorf("first page JournalIDs = %d,%d; want %d,%d", page.Events[0].JournalID, page.Events[1].JournalID, jids[0], jids[1])
	}
	if page.SnapshotMaxJournalID != jids[4] {
		t.Errorf("snapshot = %d, want %d", page.SnapshotMaxJournalID, jids[4])
	}

	// Walk the rest with the exclusive cursor; assert strictly ascending.
	seen := []journal.JournalID{page.Events[0].JournalID, page.Events[1].JournalID}
	cursor := page.Next
	for cursor != nil {
		p, err := db.QueryTaskEvents(journal.JournalQueryV1{
			OrderBy: journal.OrderByJournalID, Limit: 2,
			SnapshotMaxJournalID: cursor.SnapshotMaxJournalID, AfterJournalID: cursor.AfterJournalID,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range p.Events {
			if ev.JournalID <= seen[len(seen)-1] {
				t.Fatalf("cursor walk not ascending: %d after %d", ev.JournalID, seen[len(seen)-1])
			}
			seen = append(seen, ev.JournalID)
		}
		cursor = p.Next
	}
	if len(seen) != 5 {
		t.Errorf("cursor walk saw %d events, want 5", len(seen))
	}
}

// TestQueryTaskEventsTimelineWalkComposite proves the readable-timeline display
// order (§12): a paginated walk ordered by (recorded_at, journal_id) with a
// composite exclusive cursor returns every row exactly once, in wall-clock order
// with the journal_id tiebreak, even across an equal-timestamp tie and a backdated
// row — while the canonical journal_id query still returns commit order.
func TestQueryTaskEventsTimelineWalkComposite(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)

	// Commit order (jids[0..3], consecutive) deliberately disagrees with wall-clock:
	// jids[0] latest, jids[1] earliest (backdated), jids[2]/jids[3] an equal-timestamp tie.
	secs := []int64{300, 100, 200, 200}
	evs := make([]opEvent, len(secs))
	for i, s := range secs {
		evs[i] = opEvent{kind: "provenance.task.updated", recordedAt: time.Unix(s, 0)}
	}
	jids := appendEventsOp(t, db, boot, actor, task, "op-events", evs)

	// Timeline walk with page size 2 and a composite cursor.
	var display []journal.JournalID
	seen := map[journal.JournalID]int{}
	q := journal.JournalQueryV1{OrderBy: journal.OrderByRecordedAt, Limit: 2}
	for {
		p, err := db.QueryTaskEvents(q)
		if err != nil {
			t.Fatalf("timeline page: %v", err)
		}
		for _, ev := range p.Events {
			seen[ev.JournalID]++
			display = append(display, ev.JournalID)
		}
		if p.SnapshotMaxJournalID != jids[3] {
			t.Fatalf("timeline snapshot = %d, want %d (journal_id-bounded)", p.SnapshotMaxJournalID, jids[3])
		}
		if p.Next == nil {
			break
		}
		q = journal.JournalQueryV1{
			OrderBy: journal.OrderByRecordedAt, Limit: 2,
			SnapshotMaxJournalID: p.Next.SnapshotMaxJournalID,
			AfterJournalID:       p.Next.AfterJournalID,
			AfterRecordedAt:      p.Next.AfterRecordedAt,
		}
	}

	// (recorded_at, journal_id): jids[1](100), jids[2](200), jids[3](200), jids[0](300).
	wantDisplay := []journal.JournalID{jids[1], jids[2], jids[3], jids[0]}
	if !equalJournalIDs(display, wantDisplay) {
		t.Errorf("timeline display order = %v, want %v", display, wantDisplay)
	}
	for jid, n := range seen {
		if n != 1 {
			t.Errorf("journal_id %d appeared %d times in the timeline walk; want exactly 1 (no duplicate/skip)", jid, n)
		}
	}
	if len(seen) != 4 {
		t.Errorf("timeline walk saw %d distinct rows, want 4 (complete)", len(seen))
	}

	// The canonical order is untouched: journal_id ascending == commit order.
	canon, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID})
	if err != nil {
		t.Fatal(err)
	}
	wantCanon := []journal.JournalID{jids[0], jids[1], jids[2], jids[3]}
	if !equalJournalIDs(journalIDsOf(canon.Events), wantCanon) {
		t.Errorf("canonical order = %v, want %v (journal_id firewall intact)", journalIDsOf(canon.Events), wantCanon)
	}
}

func TestQueryTaskEventsFiltersByContexts(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)

	taskCtx, err := journal.TaskContext(task)
	if err != nil {
		t.Fatal(err)
	}
	gitCtx, err := journal.GitContext(journal.GitOID("0123456789abcdef0123456789abcdef01234567"))
	if err != nil {
		t.Fatal(err)
	}
	otherActor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	actorCtx, err := journal.ActorContext(otherActor)
	if err != nil {
		t.Fatal(err)
	}

	// One operation emits the three events: tagged (task+git contexts), untagged (no
	// contexts), actorTagged (actor context) — jids[0..2] consecutively.
	boot := genesisBoot(t, db, actor)
	jids := appendEventsOp(t, db, boot, actor, task, "op-ctx-events", []opEvent{
		{kind: "provenance.task.updated", recordedAt: time.Unix(1, 0), contexts: []journal.EventContext{taskCtx, gitCtx}},
		{kind: "provenance.task.updated", recordedAt: time.Unix(2, 0)},
		{kind: "provenance.task.updated", recordedAt: time.Unix(3, 0), contexts: []journal.EventContext{actorCtx}},
	})
	taggedJID, actorTaggedJID := jids[0], jids[2]

	// Positive: filtering by the git context returns only the tagged row.
	page, err := db.QueryTaskEvents(journal.JournalQueryV1{
		OrderBy: journal.OrderByJournalID, Contexts: []journal.EventContext{gitCtx},
	})
	if err != nil {
		t.Fatalf("query by git context: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].JournalID != taggedJID {
		t.Fatalf("git-context filter returned %v, want exactly [%d]", journalIDsOf(page.Events), taggedJID)
	}

	// Negative: filtering by a context no row carries returns an empty page.
	unusedCtx, err := journal.GitContext(journal.GitOID("fedcba9876543210fedcba9876543210fedcba90"))
	if err != nil {
		t.Fatal(err)
	}
	page, err = db.QueryTaskEvents(journal.JournalQueryV1{
		OrderBy: journal.OrderByJournalID, Contexts: []journal.EventContext{unusedCtx},
	})
	if err != nil {
		t.Fatalf("query by unused context: %v", err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("unused-context filter returned %v, want empty", journalIDsOf(page.Events))
	}

	// OR within Contexts: filtering by [gitCtx, actorCtx] returns both tagged
	// rows (task+git and actor), not the untagged row.
	page, err = db.QueryTaskEvents(journal.JournalQueryV1{
		OrderBy: journal.OrderByJournalID, Contexts: []journal.EventContext{gitCtx, actorCtx},
	})
	if err != nil {
		t.Fatalf("query by OR contexts: %v", err)
	}
	gotIDs := journalIDsOf(page.Events)
	wantIDs := []journal.JournalID{taggedJID, actorTaggedJID}
	if !equalJournalIDs(gotIDs, wantIDs) {
		t.Fatalf("OR-within-Contexts filter returned %v, want %v", gotIDs, wantIDs)
	}

	// AND across dimensions: combining Contexts=[actorCtx] with an EventKinds
	// filter that only the untagged/actor-tagged rows satisfy still requires
	// BOTH dimensions — only actorTagged satisfies both.
	page, err = db.QueryTaskEvents(journal.JournalQueryV1{
		OrderBy:    journal.OrderByJournalID,
		Contexts:   []journal.EventContext{actorCtx},
		EventKinds: []journal.EventKind{"provenance.task.updated"},
	})
	if err != nil {
		t.Fatalf("query combined dimensions: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].JournalID != actorTaggedJID {
		t.Fatalf("combined Contexts+EventKinds filter returned %v, want exactly [%d]",
			journalIDsOf(page.Events), actorTaggedJID)
	}
}

func journalIDsOf(rows []journal.TaskEventRow) []journal.JournalID {
	out := make([]journal.JournalID, len(rows))
	for i, r := range rows {
		out[i] = r.JournalID
	}
	return out
}

func equalJournalIDs(a, b []journal.JournalID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestQueryTaskEventsSnapshotWalkHidesLaterInserts proves the §5.2 snapshot
// guarantee end to end: a row inserted after a snapshot's watermark was
// pinned on page 1 stays invisible through every remaining page of that same
// cursor walk, while a fresh (unsnapshotted) query issued after the insert
// does see it — pinning the distinction between a pinned snapshot walk and a
// fresh query.
func TestQueryTaskEventsSnapshotWalkHidesLaterInserts(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)

	evs := make([]opEvent, 5)
	for i := range evs {
		evs[i] = opEvent{kind: "provenance.task.updated", recordedAt: time.Unix(int64(i), 0)}
	}
	jids := appendEventsOp(t, db, boot, actor, task, "op-seed", evs)

	// Page 1 pins the snapshot at the last seeded event's JournalID.
	page, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID, Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if page.SnapshotMaxJournalID != jids[4] || page.Next == nil {
		t.Fatalf("page 1 snapshot=%d next=%v, want snapshot=%d with a next cursor", page.SnapshotMaxJournalID, page.Next, jids[4])
	}

	// A new row is appended after the snapshot was taken; it must never appear
	// in the remaining pages of the walk started above.
	insertedJID := appendEventsOp(t, db, boot, actor, task, "op-post", []opEvent{
		{kind: "provenance.task.updated", recordedAt: time.Unix(100, 0)},
	})[0]
	if insertedJID <= page.SnapshotMaxJournalID {
		t.Fatalf("post-snapshot insert JournalID %d must exceed the pinned snapshot %d", insertedJID, page.SnapshotMaxJournalID)
	}

	seen := journalIDsOf(page.Events)
	cursor := page.Next
	for cursor != nil {
		p, err := db.QueryTaskEvents(journal.JournalQueryV1{
			OrderBy: journal.OrderByJournalID, Limit: 2,
			SnapshotMaxJournalID: cursor.SnapshotMaxJournalID, AfterJournalID: cursor.AfterJournalID,
		})
		if err != nil {
			t.Fatalf("walk page: %v", err)
		}
		seen = append(seen, journalIDsOf(p.Events)...)
		cursor = p.Next
	}
	if len(seen) != 5 {
		t.Fatalf("snapshot walk saw %d events, want exactly the 5 pre-snapshot rows: %v", len(seen), seen)
	}
	for _, id := range seen {
		if id == insertedJID {
			t.Fatalf("post-snapshot row %d leaked into the pinned-snapshot walk %v", insertedJID, seen)
		}
	}

	// A fresh query (no SnapshotMaxJournalID) issued after the insert DOES see
	// the new row, positively pinning the distinction from the pinned walk above.
	fresh, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID})
	if err != nil {
		t.Fatalf("fresh query: %v", err)
	}
	if fresh.SnapshotMaxJournalID != insertedJID {
		t.Fatalf("fresh query snapshot = %d, want %d (includes the new row)", fresh.SnapshotMaxJournalID, insertedJID)
	}
	found := false
	for _, ev := range fresh.Events {
		if ev.JournalID == insertedJID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("fresh query did not see the post-insert row %d among %v", insertedJID, journalIDsOf(fresh.Events))
	}
}

func TestQueryTaskEventsRejectsNonJournalIDOrder(t *testing.T) {
	db := newJournalDB(t)
	_, qErr := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderDimension(7)})
	if !errors.Is(qErr, journal.ErrUnsupportedOrderDimension) {
		t.Errorf("got %v, want ErrUnsupportedOrderDimension", qErr)
	}
}

func TestVerifyIntegrityCleanJournal(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	// Operation-anchored append: the created event is produced by an operation, so it
	// satisfies the producer CHECK, and it anchors the task's watermark, so the whole
	// database (subtype totality, actor placement, watermark presence) is converged.
	appendEventsOp(t, db, boot, actor, task, "op-ev", []opEvent{
		{kind: "provenance.task.created", recordedAt: time.Now()},
	})
	if err := db.VerifyIntegrity(); err != nil {
		t.Errorf("clean journal failed VerifyIntegrity: %v", err)
	}
}

func TestVerifyIntegrityRejectsBareJournalRow(t *testing.T) {
	db := newJournalDB(t)
	actor, _ := seedActorAndTask(t, db)
	// A bare decision row (kind with a subtype table, but no subtype row) violates
	// subtype totality. It uses a non-task-event kind because the producer
	// producer CHECK now forbids a NULL-producer task_event outright at insert time.
	if _, err := db.AppendBareJournalRow(journal.JournalKindDecision, actor, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.VerifyIntegrity(); !errors.Is(err, journal.ErrSubtypeIntegrity) {
		t.Errorf("bare journal row: got %v, want ErrSubtypeIntegrity", err)
	}
}

// TestTasksWatermarkSchemaIsNotNull proves the §8.1 tightening: a fresh native database
// enforces last_journal_id NOT NULL at the schema level, so a bare tasks-row insert
// omitting the watermark is rejected. In production the only tasks-row INSERT is the
// reducer fold, which always carries the watermark.
func TestTasksWatermarkSchemaIsNotNull(t *testing.T) {
	db := newJournalDB(t)
	db.Lock()
	err := sqlitex.Execute(db.Conn(),
		`INSERT INTO tasks (id, namespace, title, phase_id, created_at, updated_at)
		 VALUES ('provenance-test--x','provenance-test','x',12,1,1)`, nil)
	db.Unlock()
	if err == nil {
		t.Fatal("expected the NOT NULL last_journal_id constraint to reject a watermark-less tasks insert")
	}
}

// registerSoftwareActor raw-registers one software agent so it can act as a migration
// committing actor or a resolved legacy owner (both are agents.id foreign keys).
func registerSoftwareActor(t *testing.T, db *DB, name string) journal.ActorID {
	t.Helper()
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	db.Lock()
	err := sqlitex.Execute(db.Conn(),
		`INSERT INTO agents (id, kind_id) VALUES (?1, ?2)`,
		&sqlitex.ExecOptions{Args: []any{actor.String(), int(ptypes.AgentKindSoftware)}})
	if err == nil {
		err = sqlitex.Execute(db.Conn(),
			`INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1,?2,?3,?4)`,
			&sqlitex.ExecOptions{Args: []any{actor.String(), name, "0", "test"}})
	}
	db.Unlock()
	if err != nil {
		t.Fatalf("registerSoftwareActor %q: %v", name, err)
	}
	return actor
}

// TestMigrationReTightensWatermarkToNotNull proves the §8.1/§13 re-tightening: a legacy
// database whose tasks table predates the last_journal_id column entirely is migrated —
// the column is added (nullable), every row anchored, then the column RE-TIGHTENED back
// to NOT NULL — so a migrated database carries the same schema-level enforcement as a
// fresh one. It asserts the DDL notNull bit directly (not just data-level anchoring):
// column-less legacy DB -> migrate -> schema NOT NULL AND the migrated row is anchored.
func TestMigrationReTightensWatermarkToNotNull(t *testing.T) {
	db := newJournalDB(t)
	system := registerSoftwareActor(t, db, "pasture-system")
	owner := registerSoftwareActor(t, db, "actor-frank")
	boot := genesisBoot(t, db, system)

	// Model a legacy database whose tasks table predates last_journal_id entirely, then
	// confirm the column really is absent before migration (the column-add path fires).
	if err := db.DowngradeTasksToColumnlessLegacy(); err != nil {
		t.Fatalf("downgrade to column-less legacy: %v", err)
	}
	db.Lock()
	present, _, err := db.tasksWatermarkColumnInfoLocked()
	db.Unlock()
	if err != nil {
		t.Fatalf("watermark column info (pre-migration): %v", err)
	}
	if present {
		t.Fatal("column-less legacy DB should have no last_journal_id column before migration")
	}

	migrated := journal.TaskID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}
	base := time.Date(2025, 6, 10, 0, 0, 0, 0, time.UTC)
	legacy := journal.LegacyTaskRow{
		ID: migrated, RawOwner: "actor-frank", Status: journal.TaskStatusOpen, CreatedAt: base, UpdatedAt: base,
	}
	if err := db.SeedLegacyTask(legacy); err != nil {
		t.Fatalf("seed column-less legacy task: %v", err)
	}

	if _, err := db.MigrateLegacyBaseline(journal.MigrationInput{
		System: system, BootstrapAuthority: boot,
		Owners: map[string]journal.ActorID{"actor-frank": owner},
		Legacy: []journal.LegacyTaskRow{legacy},
	}); err != nil {
		t.Fatalf("migrate column-less legacy database: %v", err)
	}

	// The core assertion: the DDL is schema-level NOT NULL again post-migration, matching
	// a fresh native database — not merely data-level satisfied.
	db.Lock()
	present, notNull, err := db.tasksWatermarkColumnInfoLocked()
	db.Unlock()
	if err != nil {
		t.Fatalf("watermark column info (post-migration): %v", err)
	}
	if !present || !notNull {
		t.Fatalf("post-migration watermark column present=%v notNull=%v, want present=true notNull=true", present, notNull)
	}

	// And every migrated row is anchored: it carries a non-null, non-zero watermark.
	var watermark int64
	db.Lock()
	err = sqlitex.Execute(db.Conn(),
		`SELECT last_journal_id FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{Args: []any{migrated.String()}, ResultFunc: func(stmt *zs.Stmt) error {
			watermark = stmt.ColumnInt64(0)
			return nil
		}})
	db.Unlock()
	if err != nil {
		t.Fatalf("read migrated watermark: %v", err)
	}
	if watermark == 0 {
		t.Fatalf("migrated task watermark = 0, want a non-zero anchored journal id")
	}

	// The schema is now tight, so a bare watermark-less insert is rejected at the schema
	// level exactly as on a fresh database (the fresh-vs-migrated asymmetry is closed).
	db.Lock()
	insErr := sqlitex.Execute(db.Conn(),
		`INSERT INTO tasks (id, namespace, title, phase_id, created_at, updated_at)
		 VALUES ('provenance-test--bare','provenance-test','x',12,1,1)`, nil)
	db.Unlock()
	if insErr == nil {
		t.Fatal("post-migration schema accepted a watermark-less tasks insert; NOT NULL was not restored")
	}
}

// TestVerifyIntegrityRejectsUnwatermarkedTask proves the watermark-presence gate: the
// legacy seam relaxes the schema and seeds a NULL-watermark row (a legacy task not yet
// anchored); with no journal-row violations, VerifyIntegrity reaches the watermark gate
// and rejects the un-journaled task with ErrWatermarkMissing.
func TestVerifyIntegrityRejectsUnwatermarkedTask(t *testing.T) {
	db := newJournalDB(t)
	seedActorAndTask(t, db)
	if err := db.VerifyIntegrity(); !errors.Is(err, journal.ErrWatermarkMissing) {
		t.Errorf("un-anchored legacy task: got %v, want ErrWatermarkMissing", err)
	}
}

func TestNamespaceClaimIdempotentAndConflict(t *testing.T) {
	db := newJournalDB(t)
	claim := journal.ActorNamespaceClaim{
		Namespace: "pasture-system", ClaimantID: "pasture-system",
		Range: journal.UUIDRange{Min: journal.BigEndianUUID(0), Max: journal.BigEndianUUID(1023)},
		Codec: journal.OrdinalV1CodecName,
	}
	if err := db.RegisterNamespaceClaim(claim); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Exact re-registration is idempotent.
	if err := db.RegisterNamespaceClaim(claim); err != nil {
		t.Errorf("idempotent re-register rejected: %v", err)
	}
	// Differing re-registration of the same namespace is a conflict, not silent
	// overwrite.
	conflict := claim
	conflict.ClaimantID = "someone-else"
	if err := db.RegisterNamespaceClaim(conflict); !errors.Is(err, journal.ErrNamespaceClaim) {
		t.Errorf("conflicting re-register: got %v, want ErrNamespaceClaim", err)
	}
	claims, err := db.NamespaceClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Errorf("claims = %d, want 1", len(claims))
	}
}

func TestNamespaceClaimRejectsOverlap(t *testing.T) {
	db := newJournalDB(t)
	base := journal.ActorNamespaceClaim{
		Namespace: "pasture-system", ClaimantID: "pasture-system",
		Range: journal.UUIDRange{Min: journal.BigEndianUUID(0), Max: journal.BigEndianUUID(1023)},
		Codec: journal.OrdinalV1CodecName,
	}
	if err := db.RegisterNamespaceClaim(base); err != nil {
		t.Fatal(err)
	}
	overlap := journal.ActorNamespaceClaim{
		Namespace: "pasture-intruder", ClaimantID: "pasture-intruder",
		Range: journal.UUIDRange{Min: journal.BigEndianUUID(512), Max: journal.BigEndianUUID(1535)},
		Codec: journal.OrdinalV1CodecName,
	}
	if err := db.RegisterNamespaceClaim(overlap); !errors.Is(err, journal.ErrNamespaceRange) {
		t.Errorf("overlap: got %v, want ErrNamespaceRange", err)
	}
}

// TestRegisterFixedSoftwareAgentSatisfiesManifestFK proves the aggregate
// registration creates a readable software agent and its FK-backed manifest.
func TestRegisterFixedSoftwareAgentSatisfiesManifestFK(t *testing.T) {
	db := newJournalDB(t)
	claim := journal.ActorNamespaceClaim{
		Namespace: "pasture-system", ClaimantID: "pasture-system",
		Range: journal.UUIDRange{Min: journal.BigEndianUUID(0), Max: journal.BigEndianUUID(1023)},
		Codec: journal.OrdinalV1CodecName,
	}
	ordinalZero := journal.BigEndianUUID(0) // all-zero UUID: ordinal 0 under OrdinalV1Codec starting at Min=0.
	fixedID := ptypes.AgentID{Namespace: "pasture-system", UUID: uuid.UUID(ordinalZero)}
	entry := journal.FixedActorEntry{
		ActorID:   journal.ActorID(fixedID),
		Namespace: "pasture-system",
		ActorKind: ptypes.AgentKindSoftware,
		Name:      "pasture-system-default",
	}
	sa, err := db.RegisterFixedSoftwareAgent(journal.FixedSoftwareAgentRegistration{
		Claim: claim, Entry: entry, AgentName: "pasture-system", Version: "0", Source: "provenance",
	})
	if err != nil {
		t.Fatalf("RegisterFixedSoftwareAgent: %v", err)
	}
	if sa.ID != fixedID || sa.Kind != ptypes.AgentKindSoftware {
		t.Errorf("registered agent = %+v, want ID %v and software kind", sa, fixedID)
	}

	// Round-trip: the agent is readable through the normal typed accessor too.
	got, err := db.GetSoftwareAgent(fixedID)
	if err != nil {
		t.Fatalf("GetSoftwareAgent: %v", err)
	}
	if got.Name != "pasture-system" {
		t.Errorf("GetSoftwareAgent.Name = %q, want %q", got.Name, "pasture-system")
	}
}

// TestRegisterFixedSoftwareAgentRetryAndDrift proves exact retry convergence and
// deterministic conflict on changed agent data.
func TestRegisterFixedSoftwareAgentRetryAndDrift(t *testing.T) {
	db := newJournalDB(t)
	claim := journal.ActorNamespaceClaim{
		Namespace: "pasture-system", ClaimantID: "pasture-system",
		Range: journal.UUIDRange{Min: journal.BigEndianUUID(0), Max: journal.BigEndianUUID(1023)},
		Codec: journal.OrdinalV1CodecName,
	}
	id := ptypes.AgentID{Namespace: "pasture-system", UUID: uuid.UUID(journal.BigEndianUUID(1))}
	reg := journal.FixedSoftwareAgentRegistration{Claim: claim, AgentName: "agent-one", Version: "0", Source: "test",
		Entry: journal.FixedActorEntry{ActorID: id, Namespace: id.Namespace, ActorKind: ptypes.AgentKindSoftware, Name: "agent-one/default"}}
	if _, err := db.RegisterFixedSoftwareAgent(reg); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if _, err := db.RegisterFixedSoftwareAgent(reg); err != nil {
		t.Errorf("exact retry: %v", err)
	}
	reg.AgentName = "agent-one-again"
	if _, err := db.RegisterFixedSoftwareAgent(reg); !errors.Is(err, ptypes.ErrAgentAlreadyExists) {
		t.Errorf("drift registration: got %v, want ErrAgentAlreadyExists", err)
	}
}

// TestRegisterFixedSoftwareAgentRejectsMalformed proves shape validation.
func TestRegisterFixedSoftwareAgentRejectsMalformed(t *testing.T) {
	db := newJournalDB(t)
	reg := journal.FixedSoftwareAgentRegistration{
		Claim: journal.ActorNamespaceClaim{Namespace: "pasture-system", ClaimantID: "pasture-system", Range: journal.UUIDRange{Min: journal.BigEndianUUID(0), Max: journal.BigEndianUUID(1)}, Codec: journal.OrdinalV1CodecName},
		Entry: journal.FixedActorEntry{ActorID: ptypes.AgentID{UUID: uuid.New()}, Namespace: "pasture-system", ActorKind: ptypes.AgentKindSoftware, Name: "x"}, AgentName: "x",
	}
	if _, err := db.RegisterFixedSoftwareAgent(reg); !errors.Is(err, ptypes.ErrInvalidID) {
		t.Errorf("empty-namespace ID: got %v, want ErrInvalidID", err)
	}
}

// TestRegisterFixedSoftwareAgentRejectsOutOfRange proves the §7.3 rule 2
// consistency requirement: a fixed ID whose UUID does not decode inside its
// namespace's claimed range is rejected, exactly like RegisterFixedActorEntry.
func TestRegisterFixedSoftwareAgentRejectsOutOfRange(t *testing.T) {
	db := newJournalDB(t)
	claim := journal.ActorNamespaceClaim{
		Namespace: "pasture-system", ClaimantID: "pasture-system",
		Range: journal.UUIDRange{Min: journal.BigEndianUUID(0), Max: journal.BigEndianUUID(1023)},
		Codec: journal.OrdinalV1CodecName,
	}
	outOfRange := ptypes.AgentID{Namespace: "pasture-system", UUID: uuid.UUID(journal.BigEndianUUID(4096))}
	reg := journal.FixedSoftwareAgentRegistration{Claim: claim, AgentName: "x",
		Entry: journal.FixedActorEntry{ActorID: outOfRange, Namespace: outOfRange.Namespace, ActorKind: ptypes.AgentKindSoftware, Name: "x"}}
	if _, err := db.RegisterFixedSoftwareAgent(reg); !errors.Is(err, journal.ErrEntryOutOfRange) {
		t.Errorf("out-of-range ID: got %v, want ErrEntryOutOfRange", err)
	}
}

func TestFixedAgentActivationCommitErrorIsActionable(t *testing.T) {
	cause := ptypes.ErrAgentAlreadyExists
	err := fixedAgentActivationError(cause,
		"the activation transaction could not commit",
		"SQLite rejected COMMIT",
		"DB.RegisterFixedSoftwareAgent transaction finalization",
		"after all activation writes completed",
		"no agent is returned and the transaction was rolled back",
		"retry the complete activation after verifying database health")
	if !errors.Is(err, cause) {
		t.Fatalf("commit error lost typed identity: %v", err)
	}
	for _, marker := range []string{
		"fixed software agent activation failed:", "why:", "where:", "transaction finalization",
		"when:", "after all activation writes completed", "impact:", "fix:",
	} {
		if !strings.Contains(err.Error(), marker) {
			t.Errorf("commit error %q is missing %q", err, marker)
		}
	}
}

// TestRegisterSoftwareAgentUnchangedByFixedRegistration proves the existing
// random-UUIDv7 registration path's signature and behavior are unaffected: it
// still mints its own ID and needs no namespace claim at all.
func TestRegisterSoftwareAgentUnchangedByFixedRegistration(t *testing.T) {
	db := newJournalDB(t)
	sa, err := db.RegisterSoftwareAgent("no-claim-needed", "agent", "1.0", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	if sa.ID.Namespace != "no-claim-needed" {
		t.Errorf("agent namespace = %q, want %q", sa.ID.Namespace, "no-claim-needed")
	}
	if sa.ID.UUID == uuid.Nil {
		t.Errorf("agent UUID is nil; RegisterSoftwareAgent must still mint a UUIDv7")
	}
}
