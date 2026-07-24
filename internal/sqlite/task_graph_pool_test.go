package sqlite

import (
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
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
	return ptypes.Task{
		ID:        ptypes.TaskID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())},
		Title:     title,
		Status:    ptypes.StatusOpen,
		Priority:  ptypes.PriorityMedium,
		Type:      ptypes.TaskTypeTask,
		Phase:     ptypes.PhaseUnscoped,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func seedTaskGraphTask(t *testing.T, db *DB, task ptypes.Task) {
	t.Helper()
	if err := db.SeedLegacyTaskRow(task); err != nil {
		t.Fatalf("SeedLegacyTaskRow(%q): %v", task.ID.String(), err)
	}
}

func TestTaskGraphPoolCRUDRoundTrips(t *testing.T) {
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

	now := time.Now().UTC()
	if err := db.InsertEdge(parent.ID, child.ID.String(), ptypes.EdgeBlockedBy, now); err != nil {
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

func TestTaskGraphPoolDistinctReadersDoNotBlockWALWriter(t *testing.T) {
	db := openTaskGraphPoolDB(t)
	readerTasks := []ptypes.Task{
		taskGraphTask("pool-wal", "reader one"),
		taskGraphTask("pool-wal", "reader two"),
	}
	writerTask := taskGraphTask("pool-wal", "unrelated writer")
	for _, task := range append(readerTasks, writerTask) {
		seedTaskGraphTask(t, db, task)
	}

	type collationEntry struct {
		connection int
	}
	entered := make(chan collationEntry, len(readerTasks))
	releaseReaders := make(chan struct{})
	armed := make(chan struct{})
	var reported [runtimePoolSize - 1]atomic.Bool
	setupScopes := make([]*connScope, 0, runtimePoolSize-1)
	for range runtimePoolSize - 1 {
		setupScopes = append(setupScopes, takePoolScope(t, db))
	}
	const collationName = "task_graph_binary_barrier"
	for i, scope := range setupScopes {
		connection := i
		if err := scope.conn.SetCollation(collationName, func(a, b string) int {
			select {
			case <-armed:
				matchesReader := false
				for _, task := range readerTasks {
					matchesReader = matchesReader || a == task.ID.String() || b == task.ID.String()
				}
				if matchesReader && reported[connection].CompareAndSwap(false, true) {
					entered <- collationEntry{connection: connection}
					<-releaseReaders
				}
			default:
			}
			return strings.Compare(a, b)
		}); err != nil {
			t.Fatalf("connection %d install binary collation marker: %v", connection, err)
		}
		if err := sqlitex.ExecuteTransient(scope.conn, `CREATE TEMP VIEW tasks AS
			SELECT id COLLATE task_graph_binary_barrier AS id, namespace, title, description,
			       status_id, priority_id, type_id, phase_id, owner_id, notes, created_at,
			       updated_at, closed_at, close_reason, last_journal_id
			FROM main.tasks`, nil); err != nil {
			t.Fatalf("connection %d install collation-bearing tasks view: %v", connection, err)
		}
	}
	for _, scope := range setupScopes {
		scope.release()
	}
	close(armed)

	type taskResult struct {
		task  ptypes.Task
		found bool
		err   error
	}
	readerDone := make(chan taskResult, len(readerTasks))
	for _, task := range readerTasks {
		id := task.ID
		go func() {
			got, found, err := db.GetTask(id)
			readerDone <- taskResult{task: got, found: found, err: err}
		}()
	}
	seenConnections := make(map[int]struct{}, len(readerTasks))
	earlyReaderResults := make([]taskResult, 0, len(readerTasks))
	for range readerTasks {
		select {
		case marker := <-entered:
			seenConnections[marker.connection] = struct{}{}
		case result := <-readerDone:
			earlyReaderResults = append(earlyReaderResults, result)
			close(releaseReaders)
			for len(earlyReaderResults) < len(readerTasks) {
				earlyReaderResults = append(earlyReaderResults, <-readerDone)
			}
			t.Fatalf("exported GetTask returned before its collation marker: %+v", earlyReaderResults)
		case <-time.After(poolTestTimeout):
			close(releaseReaders)
			for range readerTasks {
				<-readerDone
			}
			t.Fatalf("exported GetTask did not reach both collation markers within %v", poolTestTimeout)
		}
	}
	if len(seenConnections) != len(readerTasks) {
		close(releaseReaders)
		for range readerTasks {
			<-readerDone
		}
		t.Fatalf("exported GetTask calls used %d pooled connections, want %d distinct leases", len(seenConnections), len(readerTasks))
	}

	writeDone := make(chan error, 1)
	go func() { writeDone <- db.AddLabel(writerTask.ID, "committed-under-readers") }()
	writeErr := waitPoolError(t, "exported WAL writer while GetTask calls are active", writeDone)
	close(releaseReaders)
	results := make([]taskResult, 0, len(readerTasks))
	for range readerTasks {
		results = append(results, <-readerDone)
	}
	if writeErr != nil {
		t.Fatalf("AddLabel while exported readers hold snapshots: %v", writeErr)
	}
	for _, result := range results {
		if result.err != nil || !result.found {
			t.Fatalf("concurrent GetTask = (%+v, %t, %v), want found task", result.task, result.found, result.err)
		}
	}
	cleanupScopes := make([]*connScope, 0, runtimePoolSize-1)
	for range runtimePoolSize - 1 {
		cleanupScopes = append(cleanupScopes, takePoolScope(t, db))
	}
	for i, scope := range cleanupScopes {
		if err := sqlitex.ExecuteTransient(scope.conn, "DROP VIEW temp.tasks", nil); err != nil {
			t.Fatalf("connection %d drop collation-bearing tasks view: %v", i, err)
		}
		if err := scope.conn.SetCollation(collationName, nil); err != nil {
			t.Fatalf("connection %d remove binary collation marker: %v", i, err)
		}
		scope.release()
	}
	if labels, err := db.GetLabels(writerTask.ID); err != nil || !reflect.DeepEqual(labels, []string{"committed-under-readers"}) {
		t.Fatalf("GetLabels after reader snapshots = (%v, %v), want committed label", labels, err)
	}
}

func TestTaskGraphPoolCloseInterruptsActiveExportedCRUDAndDrains(t *testing.T) {
	dbPath := t.TempDir() + "/task-graph-close.db"
	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open file-backed task graph DB: %v", err)
	}
	task := taskGraphTask("pool-close", "active writer")
	seedTaskGraphTask(t, db, task)

	markerEntered := make(chan int, 1)
	markerRelease := make(chan struct{})
	var releaseMarker sync.Once
	release := func() { releaseMarker.Do(func() { close(markerRelease) }) }
	defer release()
	setupScopes := make([]*connScope, 0, runtimePoolSize-1)
	for range runtimePoolSize - 1 {
		setupScopes = append(setupScopes, takePoolScope(t, db))
	}
	for i, scope := range setupScopes {
		connection := i
		if err := scope.conn.CreateFunction("task_graph_close_marker", &zs.FunctionImpl{
			NArgs:         0,
			AllowIndirect: true,
			Scalar: func(zs.Context, []zs.Value) (zs.Value, error) {
				markerEntered <- connection
				<-markerRelease
				return zs.IntegerValue(0), nil
			},
		}); err != nil {
			t.Fatalf("connection %d install close scalar marker: %v", connection, err)
		}
	}
	const triggerName = "task_graph_pool_close_interrupt"
	if err := sqlitex.ExecuteTransient(setupScopes[0].conn, `CREATE TRIGGER task_graph_pool_close_interrupt
		BEFORE INSERT ON labels BEGIN
			SELECT task_graph_close_marker();
			SELECT count(*) FROM (
				WITH RECURSIVE forever(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM forever)
				SELECT n FROM forever
			);
		END`, nil); err != nil {
		t.Fatalf("install persistent close-interrupt trigger: %v", err)
	}
	for _, scope := range setupScopes {
		scope.release()
	}

	writeDone := make(chan error, 1)
	go func() { writeDone <- db.AddLabel(task.ID, "close-interrupted") }()
	select {
	case <-markerEntered:
	case err := <-writeDone:
		release()
		_ = db.Close()
		t.Fatalf("exported AddLabel returned before scalar marker: %v", err)
	case <-time.After(poolTestTimeout):
		release()
		_ = db.Close()
		<-writeDone
		t.Fatalf("exported AddLabel did not reach scalar marker within %v", poolTestTimeout)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- db.Close() }()
	poolClosed := make(chan error, 1)
	go func() {
		for {
			if _, err := db.TaskCount(); err != nil {
				poolClosed <- err
				return
			}
		}
	}()
	select {
	case err := <-poolClosed:
		if err == nil {
			t.Fatal("TaskCount close probe returned nil error")
		}
	case <-time.After(poolTestTimeout):
		release()
		<-writeDone
		<-closeDone
		t.Fatalf("exported TaskCount did not observe pool closure within %v", poolTestTimeout)
	}
	select {
	case err := <-closeDone:
		release()
		<-writeDone
		t.Fatalf("Close returned before active AddLabel could unwind and release its lease: %v", err)
	default:
	}

	release()
	writeErr := waitPoolError(t, "Close-interrupted exported AddLabel", writeDone)
	closeErr := waitPoolError(t, "Close draining exported AddLabel lease", closeDone)
	if code := zs.ErrCode(writeErr); code != zs.ResultInterrupt {
		t.Fatalf("Close-interrupted AddLabel error = %v (%v), want SQLITE_INTERRUPT", writeErr, code)
	}
	if closeErr != nil {
		t.Fatalf("Close after AddLabel released its lease: %v", closeErr)
	}

	cleanupConn, err := zs.OpenConn(dbPath, zs.OpenReadWrite|zs.OpenURI)
	if err != nil {
		t.Fatalf("open post-Close trigger cleanup connection: %v", err)
	}
	cleanupErr := sqlitex.ExecuteTransient(cleanupConn, "DROP TRIGGER "+triggerName, nil)
	closeCleanupErr := cleanupConn.Close()
	if cleanupErr != nil {
		t.Fatalf("drop persistent close-interrupt trigger: %v", cleanupErr)
	}
	if closeCleanupErr != nil {
		t.Fatalf("close trigger cleanup connection: %v", closeCleanupErr)
	}
}
