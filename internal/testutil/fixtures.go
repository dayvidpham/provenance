// Package testutil provides shared test fixtures and helpers for Providence
// internal packages. Import this package from any _test.go file that needs
// an in-memory database or pre-built tasks.
package testutil

import (
	"testing"
	"time"

	dbsqlite "github.com/dayvidpham/provenance/internal/sqlite"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// TestModels returns the default model entries used for testing.
// These match the canonical Anthropic API model identifiers.
func TestModels() []ptypes.ModelEntry {
	return []ptypes.ModelEntry{
		{Provider: ptypes.ProviderAnthropic, Name: "claude-opus-4-6", DisplayName: "Claude Opus 4.6", Family: "claude-opus"},
		{Provider: ptypes.ProviderAnthropic, Name: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", Family: "claude-sonnet"},
		{Provider: ptypes.ProviderAnthropic, Name: "claude-haiku-4-5", DisplayName: "Claude Haiku 4.5", Family: "claude-haiku"},
	}
}

// OpenTestDB returns a fresh in-memory sqlite.DB for testing.
// The database is closed automatically when the test ends.
func OpenTestDB(t *testing.T) *dbsqlite.DB {
	t.Helper()
	db, err := dbsqlite.Open(":memory:", TestModels())
	if err != nil {
		t.Fatalf("testutil.OpenTestDB: sqlite.Open(:memory:) failed: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("testutil.OpenTestDB: db.Close() failed: %v", err)
		}
	})
	return db
}

// MakeTask creates a minimal Task with a unique UUIDv7 for testing.
// The task uses StatusOpen, PriorityMedium, TaskTypeTask, and PhaseUnscoped.
func MakeTask(ns, title string) ptypes.Task {
	now := time.Now().UTC()
	return ptypes.Task{
		ID:        ptypes.TaskID{Namespace: ns, UUID: uuid.Must(uuid.NewV7())},
		Title:     title,
		Status:    ptypes.StatusOpen,
		Priority:  ptypes.PriorityMedium,
		Type:      ptypes.TaskTypeTask,
		Phase:     ptypes.PhaseUnscoped,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// MakeTaskID creates a TaskID with a unique UUIDv7 for testing.
func MakeTaskID(ns string) ptypes.TaskID {
	return ptypes.TaskID{Namespace: ns, UUID: uuid.Must(uuid.NewV7())}
}

// ContainsTask checks if a task with the given ID is in the slice.
func ContainsTask(tasks []ptypes.Task, id ptypes.TaskID) bool {
	for _, t := range tasks {
		if t.ID == id {
			return true
		}
	}
	return false
}
