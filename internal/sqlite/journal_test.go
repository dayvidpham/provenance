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
	if err := db.InsertTask(task); err != nil {
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
