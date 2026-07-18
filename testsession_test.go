package provenance_test

import (
	"testing"

	"github.com/dayvidpham/provenance"
)

// testsession_test.go bridges the pre-Session integration suite to the journaled
// Session SDK. Task/edge/label/comment mutations moved off the Tracker interface onto
// Session (Tracker.As); testTracker re-exposes those eight verbs so every existing
// behavioral assertion runs UNCHANGED against the real production journaled path:
// reads pass through the embedded Tracker, and the mutation verbs route through one
// genesis-bound Session (Create/Update/CloseTask journaled per §9; the five
// relationship/annotation verbs the un-journaled §6 direct writes).

// testTracker adapts a genesis-bound Session back to the pre-Session verb surface.
type testTracker struct {
	provenance.Tracker
	s     *provenance.Session
	actor provenance.ActorID
}

// wrapTracker registers a system actor, establishes the genesis bootstrap authority,
// and binds a Session, returning the verb-surface adapter over tr. tr's Close is the
// caller's responsibility (openTestTracker registers the cleanup; the persistence
// demos close explicitly).
func wrapTracker(t *testing.T, tr provenance.Tracker) *testTracker {
	t.Helper()
	sys, err := tr.RegisterSoftwareAgent("provenance-test", "pasture-system", "0", "test")
	if err != nil {
		t.Fatalf("wrapTracker: RegisterSoftwareAgent: %v", err)
	}
	boot := establishGenesis(t, tr, sys.ID)
	return &testTracker{Tracker: tr, s: tr.As(sys.ID, boot), actor: sys.ID}
}

// openMemorySession opens a fresh in-memory tracker wrapped with a genesis Session.
// The tracker is NOT auto-closed (the caller closes it explicitly).
func openMemorySession(t *testing.T) *testTracker {
	t.Helper()
	tr, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("openMemorySession: OpenMemory: %v", err)
	}
	return wrapTracker(t, tr)
}

// openSQLiteSession opens a SQLite tracker at path wrapped with a genesis Session.
// The tracker is NOT auto-closed (the caller closes it explicitly).
func openSQLiteSession(t *testing.T, path string) *testTracker {
	t.Helper()
	tr, err := provenance.OpenSQLite(path)
	if err != nil {
		t.Fatalf("openSQLiteSession: OpenSQLite: %v", err)
	}
	return wrapTracker(t, tr)
}

// --- Journaled task-lifecycle verbs (route through the Session) ---

func (tt *testTracker) Create(namespace, title, description string, taskType provenance.TaskType, priority provenance.Priority, phase provenance.Phase) (provenance.Task, error) {
	return tt.s.Create(namespace, title, description, taskType, priority, phase)
}

func (tt *testTracker) Update(id provenance.TaskID, fields provenance.UpdateFields) (provenance.Task, error) {
	return tt.s.Update(id, fields)
}

func (tt *testTracker) CloseTask(id provenance.TaskID, reason string) (provenance.Task, error) {
	return tt.s.CloseTask(id, reason)
}

// --- Un-journaled relationship / annotation verbs (§6) ---

func (tt *testTracker) AddEdge(sourceID provenance.TaskID, targetID string, kind provenance.EdgeKind) error {
	return tt.s.AddEdge(sourceID, targetID, kind)
}

func (tt *testTracker) RemoveEdge(sourceID provenance.TaskID, targetID string, kind provenance.EdgeKind) error {
	return tt.s.RemoveEdge(sourceID, targetID, kind)
}

func (tt *testTracker) AddLabel(id provenance.TaskID, label string) error {
	return tt.s.AddLabel(id, label)
}

func (tt *testTracker) RemoveLabel(id provenance.TaskID, label string) error {
	return tt.s.RemoveLabel(id, label)
}

func (tt *testTracker) AddComment(id provenance.TaskID, authorID provenance.AgentID, body string) (provenance.Comment, error) {
	return tt.s.AddComment(id, authorID, body)
}
