package sqlite

import (
	"encoding/json"
	"errors"
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

	ctx, err := journal.TaskContext(task)
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.AppendTaskEvent(journal.AppendTaskEventInput{
		ActorID:    actor,
		TaskID:     task,
		EventKind:  "provenance.task.created",
		RecordedAt: time.Unix(1000, 0),
		Payload:    json.RawMessage(`{"k":"v"}`),
		Contexts:   []journal.EventContext{ctx, ctx}, // dedups to one
	})
	if err != nil {
		t.Fatalf("AppendTaskEvent: %v", err)
	}
	if row.JournalID != 1 {
		t.Errorf("first JournalID = %d, want 1", row.JournalID)
	}
	if len(row.Contexts) != 1 {
		t.Errorf("contexts = %d, want 1 (deduped)", len(row.Contexts))
	}

	// Watermark advanced on the projection.
	var watermark int64
	db.Lock()
	_ = sqlitex.Execute(db.Conn(),
		`SELECT last_journal_id FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{Args: []any{task.String()},
			ResultFunc: func(stmt *zs.Stmt) error { watermark = stmt.ColumnInt64(0); return nil }})
	db.Unlock()
	if watermark != int64(row.JournalID) {
		t.Errorf("tasks.last_journal_id = %d, want %d", watermark, row.JournalID)
	}

	// Attribution is first-wins: a later event by the same actor must not move
	// the FirstJournalID.
	if _, err := db.AppendTaskEvent(journal.AppendTaskEventInput{
		ActorID: actor, TaskID: task, EventKind: "provenance.task.updated", RecordedAt: time.Unix(2000, 0),
	}); err != nil {
		t.Fatalf("second append: %v", err)
	}
	attrs, err := db.TaskAttributions(task)
	if err != nil {
		t.Fatal(err)
	}
	if len(attrs) != 1 {
		t.Fatalf("attributions = %d, want 1", len(attrs))
	}
	if attrs[0].FirstJournalID != row.JournalID {
		t.Errorf("attribution FirstJournalID = %d, want %d (append-only, first wins)", attrs[0].FirstJournalID, row.JournalID)
	}
}

func TestQueryTaskEventsOrdersByJournalIDWithPaging(t *testing.T) {
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)

	// Append 5 events, deliberately backdating RecordedAt so a RecordedAt sort
	// would reverse them.
	for i := 0; i < 5; i++ {
		if _, err := db.AppendTaskEvent(journal.AppendTaskEventInput{
			ActorID: actor, TaskID: task, EventKind: "provenance.task.updated",
			RecordedAt: time.Unix(int64(1000-i), 0),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// Page size 2 across the snapshot.
	page, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Next == nil {
		t.Fatalf("first page: got %d events, next=%v", len(page.Events), page.Next)
	}
	if page.Events[0].JournalID != 1 || page.Events[1].JournalID != 2 {
		t.Errorf("first page JournalIDs = %d,%d; want 1,2", page.Events[0].JournalID, page.Events[1].JournalID)
	}
	if page.SnapshotMaxJournalID != 5 {
		t.Errorf("snapshot = %d, want 5", page.SnapshotMaxJournalID)
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

	// Commit order (journal_id 1..4) deliberately disagrees with wall-clock:
	// jid1 latest, jid2 earliest (backdated), jid3/jid4 an equal-timestamp tie.
	secs := []int64{300, 100, 200, 200}
	for _, s := range secs {
		if _, err := db.AppendTaskEvent(journal.AppendTaskEventInput{
			ActorID: actor, TaskID: task, EventKind: "provenance.task.updated",
			RecordedAt: time.Unix(s, 0),
		}); err != nil {
			t.Fatalf("append t=%d: %v", s, err)
		}
	}

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
		if p.SnapshotMaxJournalID != 4 {
			t.Fatalf("timeline snapshot = %d, want 4 (journal_id-bounded)", p.SnapshotMaxJournalID)
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

	// (recorded_at, journal_id): jid2(100), jid3(200), jid4(200), jid1(300).
	wantDisplay := []journal.JournalID{2, 3, 4, 1}
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
	wantCanon := []journal.JournalID{1, 2, 3, 4}
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

	// tagged: carries both the task context and the git context.
	tagged, err := db.AppendTaskEvent(journal.AppendTaskEventInput{
		ActorID: actor, TaskID: task, EventKind: "provenance.task.updated",
		RecordedAt: time.Unix(1, 0), Contexts: []journal.EventContext{taskCtx, gitCtx},
	})
	if err != nil {
		t.Fatalf("append tagged: %v", err)
	}
	// untagged: no contexts at all.
	if _, err := db.AppendTaskEvent(journal.AppendTaskEventInput{
		ActorID: actor, TaskID: task, EventKind: "provenance.task.updated",
		RecordedAt: time.Unix(2, 0),
	}); err != nil {
		t.Fatalf("append untagged: %v", err)
	}
	// actorTagged: carries only the actor context (used to prove OR-within-dimension
	// and to prove AND-across-dimensions against a second filter below).
	actorTagged, err := db.AppendTaskEvent(journal.AppendTaskEventInput{
		ActorID: actor, TaskID: task, EventKind: "provenance.task.updated",
		RecordedAt: time.Unix(3, 0), Contexts: []journal.EventContext{actorCtx},
	})
	if err != nil {
		t.Fatalf("append actor-tagged: %v", err)
	}

	// Positive: filtering by the git context returns only the tagged row.
	page, err := db.QueryTaskEvents(journal.JournalQueryV1{
		OrderBy: journal.OrderByJournalID, Contexts: []journal.EventContext{gitCtx},
	})
	if err != nil {
		t.Fatalf("query by git context: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].JournalID != tagged.JournalID {
		t.Fatalf("git-context filter returned %v, want exactly [%d]", journalIDsOf(page.Events), tagged.JournalID)
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
	wantIDs := []journal.JournalID{tagged.JournalID, actorTagged.JournalID}
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
	if len(page.Events) != 1 || page.Events[0].JournalID != actorTagged.JournalID {
		t.Fatalf("combined Contexts+EventKinds filter returned %v, want exactly [%d]",
			journalIDsOf(page.Events), actorTagged.JournalID)
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

	for i := 0; i < 5; i++ {
		if _, err := db.AppendTaskEvent(journal.AppendTaskEventInput{
			ActorID: actor, TaskID: task, EventKind: "provenance.task.updated",
			RecordedAt: time.Unix(int64(i), 0),
		}); err != nil {
			t.Fatalf("seed append %d: %v", i, err)
		}
	}

	// Page 1 pins the snapshot at JournalID 5.
	page, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID, Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if page.SnapshotMaxJournalID != 5 || page.Next == nil {
		t.Fatalf("page 1 snapshot=%d next=%v, want snapshot=5 with a next cursor", page.SnapshotMaxJournalID, page.Next)
	}

	// A new row is appended after the snapshot was taken; it must never appear
	// in the remaining pages of the walk started above.
	inserted, err := db.AppendTaskEvent(journal.AppendTaskEventInput{
		ActorID: actor, TaskID: task, EventKind: "provenance.task.updated",
		RecordedAt: time.Unix(100, 0),
	})
	if err != nil {
		t.Fatalf("post-snapshot append: %v", err)
	}
	if inserted.JournalID <= page.SnapshotMaxJournalID {
		t.Fatalf("post-snapshot insert JournalID %d must exceed the pinned snapshot %d", inserted.JournalID, page.SnapshotMaxJournalID)
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
		if id == inserted.JournalID {
			t.Fatalf("post-snapshot row %d leaked into the pinned-snapshot walk %v", inserted.JournalID, seen)
		}
	}

	// A fresh query (no SnapshotMaxJournalID) issued after the insert DOES see
	// the new row, positively pinning the distinction from the pinned walk above.
	fresh, err := db.QueryTaskEvents(journal.JournalQueryV1{OrderBy: journal.OrderByJournalID})
	if err != nil {
		t.Fatalf("fresh query: %v", err)
	}
	if fresh.SnapshotMaxJournalID != inserted.JournalID {
		t.Fatalf("fresh query snapshot = %d, want %d (includes the new row)", fresh.SnapshotMaxJournalID, inserted.JournalID)
	}
	found := false
	for _, ev := range fresh.Events {
		if ev.JournalID == inserted.JournalID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("fresh query did not see the post-insert row %d among %v", inserted.JournalID, journalIDsOf(fresh.Events))
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
	if _, err := db.AppendTaskEvent(journal.AppendTaskEventInput{
		ActorID: actor, TaskID: task, EventKind: "provenance.task.created", RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.VerifyIntegrity(); err != nil {
		t.Errorf("clean journal failed VerifyIntegrity: %v", err)
	}
}

func TestVerifyIntegrityRejectsBareJournalRow(t *testing.T) {
	db := newJournalDB(t)
	actor, _ := seedActorAndTask(t, db)
	if _, err := db.AppendBareJournalRow(journal.JournalKindTaskEvent, actor, time.Now()); err != nil {
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
