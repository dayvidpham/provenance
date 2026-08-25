package sqlite

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

func openTaskGraphPoolDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir()+"/task-graph.db", nil)
	if err != nil {
		t.Fatalf("Open file-backed task graph DB: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close task graph DB: %v", err)
		}
	})
	return db
}

func taskGraphTask(namespace, title string) ptypes.Task {
	now := time.Now().UTC()
	return ptypes.Task{ID: ptypes.TaskID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}, Title: title, Status: ptypes.StatusOpen, Priority: ptypes.PriorityMedium, Type: ptypes.TaskTypeTask, Phase: ptypes.PhaseUnscoped, CreatedAt: now, UpdatedAt: now}
}

func seedTaskGraphTask(t *testing.T, db *DB, task ptypes.Task) {
	t.Helper()
	if err := db.SeedLegacyTaskRow(task); err != nil {
		t.Fatalf("SeedLegacyTaskRow(%q): %v", task.ID.String(), err)
	}
}

func TestTaskGraphPoolCRUDRoundTrips(t *testing.T) {
	t.Parallel()
	db := openTaskGraphPoolDB(t)
	parent := taskGraphTask("pool-crud", "parent")
	child := taskGraphTask("pool-crud", "child")
	seedTaskGraphTask(t, db, parent)
	seedTaskGraphTask(t, db, child)
	agent, err := db.RegisterSoftwareAgent("pool-crud", "commenter", "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}

	gotTask, found, err := db.GetTask(parent.ID)
	if err != nil || !found || gotTask.ID != parent.ID || gotTask.Title != parent.Title {
		t.Fatalf("GetTask = (%+v, %t, %v), want parent", gotTask, found, err)
	}
	tasks, err := db.ListTasks(ptypes.ListFilter{Namespace: parent.ID.Namespace})
	if err != nil || len(tasks) != 2 {
		t.Fatalf("ListTasks = (%d tasks, %v), want 2 tasks", len(tasks), err)
	}
	if count, err := db.TaskCount(); err != nil || count != 2 {
		t.Fatalf("TaskCount = (%d, %v), want (2, nil)", count, err)
	}

	if err := db.InsertEdge(parent.ID, child.ID.String(), ptypes.EdgeBlockedBy, time.Now().UTC()); err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}
	edges, err := db.GetEdges(parent.ID, nil)
	if err != nil || len(edges) != 1 || edges[0].TargetID != child.ID.String() {
		t.Fatalf("GetEdges = (%+v, %v), want parent -> child", edges, err)
	}
	blockedBy, err := db.GetBlockedByEdges()
	if err != nil || !reflect.DeepEqual(blockedBy, edges) {
		t.Fatalf("GetBlockedByEdges = (%+v, %v), want %+v", blockedBy, err, edges)
	}
	tree, err := db.GetDepTree(parent.ID)
	if err != nil || !reflect.DeepEqual(tree, edges) {
		t.Fatalf("GetDepTree = (%+v, %v), want %+v", tree, err, edges)
	}
	blocked, err := db.BlockedTasks()
	if err != nil || len(blocked) != 1 || blocked[0].ID != parent.ID {
		t.Fatalf("BlockedTasks = (%+v, %v), want parent", blocked, err)
	}
	ready, err := db.ReadyTasks()
	if err != nil || len(ready) != 1 || ready[0].ID != child.ID {
		t.Fatalf("ReadyTasks = (%+v, %v), want child", ready, err)
	}
	if err := db.DeleteEdge(parent.ID, child.ID.String(), ptypes.EdgeBlockedBy); err != nil {
		t.Fatalf("DeleteEdge: %v", err)
	}
	if edges, err := db.GetEdges(parent.ID, nil); err != nil || len(edges) != 0 {
		t.Fatalf("GetEdges after delete = (%+v, %v), want empty", edges, err)
	}

	if err := db.AddLabel(parent.ID, "pool"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if labels, err := db.GetLabels(parent.ID); err != nil || !reflect.DeepEqual(labels, []string{"pool"}) {
		t.Fatalf("GetLabels = (%v, %v), want [pool]", labels, err)
	}
	if err := db.RemoveLabel(parent.ID, "pool"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if labels, err := db.GetLabels(parent.ID); err != nil || len(labels) != 0 {
		t.Fatalf("GetLabels after remove = (%v, %v), want empty", labels, err)
	}
	comment, err := db.AddComment(parent.ID, agent.ID, "pooled comment")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	gotComment, found, err := db.GetComment(comment.ID)
	if err != nil || !found || gotComment != comment {
		t.Fatalf("GetComment = (%+v, %t, %v), want %+v", gotComment, found, err, comment)
	}
	comments, err := db.GetComments(parent.ID)
	if err != nil || len(comments) != 1 || comments[0] != comment {
		t.Fatalf("GetComments = (%+v, %v), want comment", comments, err)
	}
}

func TestTaskGraphPoolConcurrentReadersAndWriter(t *testing.T) {
	t.Parallel()
	db := openTaskGraphPoolDB(t)
	first, second, writer := taskGraphTask("pool-wal", "first"), taskGraphTask("pool-wal", "second"), taskGraphTask("pool-wal", "writer")
	for _, task := range []ptypes.Task{first, second, writer} {
		seedTaskGraphTask(t, db, task)
	}
	start := make(chan struct{})
	errs := make(chan error, 3)
	var wg sync.WaitGroup
	for _, task := range []ptypes.Task{first, second} {
		id := task.ID
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, found, err := db.GetTask(id)
			if err == nil && (!found || got.ID != id) {
				err = &taskGraphReadError{id: id.String()}
			}
			errs <- err
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); <-start; errs <- db.AddLabel(writer.ID, "committed-under-readers") }()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent task graph operation: %v", err)
		}
	}
	if labels, err := db.GetLabels(writer.ID); err != nil || !reflect.DeepEqual(labels, []string{"committed-under-readers"}) {
		t.Fatalf("GetLabels after concurrent writer = (%v, %v), want committed label", labels, err)
	}
}

type taskGraphReadError struct{ id string }

func (err *taskGraphReadError) Error() string {
	return "GetTask did not return expected task " + err.id
}

func TestTaskGraphPoolCloseRejectsNewCRUD(t *testing.T) {
	t.Parallel()
	dbPath := t.TempDir() + "/task-graph-close.db"
	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open file-backed task graph DB: %v", err)
	}
	task := taskGraphTask("pool-close", "closed pool")
	seedTaskGraphTask(t, db, task)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := db.TaskCount(); err == nil {
		t.Fatal("TaskCount after Close succeeded, want a connection-lease failure")
	}
	if err := db.AddLabel(task.ID, "after-close"); err == nil {
		t.Fatal("AddLabel after Close succeeded, want a connection-lease failure")
	}
}
