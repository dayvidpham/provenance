package provenance_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	p "github.com/dayvidpham/provenance"
	_ "modernc.org/sqlite"
)

// Real older stores must reconcile by exact identity, even when their catalogs
// have as many or more rows than the incoming registry. No raw catalog repair is
// used: both catalog generations and registration go through public APIs.
func TestModelRegistryReconcilesOlderStore(t *testing.T) {
	t.Parallel()
	for _, larger := range []bool{false, true} {
		name := "equal-catalog-size"
		if larger {
			name = "larger-catalog-size"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "models.db")
			oldModels := []p.ModelEntry{
				{Provider: p.ProviderOpenAI, Name: "historical"},
				{Provider: p.ProviderAnthropic, Name: "unrelated"},
			}
			if larger {
				oldModels = append(oldModels, p.ModelEntry{Provider: p.ProviderGoogle, Name: "extra"})
			}
			tr, err := p.OpenSQLite(path, p.WithModelRegistry(p.NewRegistry(oldModels)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tr.Close() })
			oldAgent, err := tr.RegisterMLAgent("test-ns", p.RoleWorker, p.ProviderOpenAI, "historical")
			if err != nil {
				t.Fatal(err)
			}
			session := wrapTracker(t, tr)
			task := mustCreateTask(t, session, "test-ns")
			if _, err := session.AddComment(task.ID, oldAgent.ID, "retain this history"); err != nil {
				t.Fatal(err)
			}
			comments, err := tr.Comments(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			history, err := tr.Journal().QueryTaskEvents(p.JournalQueryV1{})
			if err != nil {
				t.Fatal(err)
			}
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}

			// One physical connection makes query_only and total_changes apply
			// to the same connection used for activation below.
			pool, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			providerIDs := modelProviderIDs(t, pool)
			modelIDs := modelCatalogIDs(t, pool)
			registry := p.NewRegistry([]p.ModelEntry{
				{Provider: p.ProviderOpenAI, Name: "gpt-6.1-sol-ultrafast"},
				{Provider: p.Provider("openrouter"), Name: "openai/gpt-5"},
			})
			reconciled, err := p.OpenBorrowedSQLite(pool, p.WithModelRegistry(registry))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reconciled.Close() })
			for _, entry := range registry.Models() {
				agent, err := reconciled.RegisterMLAgent("test-ns", p.RoleWorker, entry.Provider, entry.Name)
				if err != nil {
					t.Fatalf("register exact pair %+v: %v", entry, err)
				}
				if agent.Model.Provider != entry.Provider || agent.Model.Name != entry.Name {
					t.Fatalf("identity changed: %+v", agent)
				}
			}
			if _, err := reconciled.RegisterMLAgent("test-ns", p.RoleWorker, p.ProviderOpenAI, "historical"); !errors.Is(err, p.ErrNotFound) {
				t.Fatalf("historical SQL membership must not grant registry admission: %v", err)
			}
			gotAgent, err := reconciled.MLAgent(oldAgent.ID)
			if err != nil || !reflect.DeepEqual(gotAgent, oldAgent) {
				t.Fatalf("historical author/model changed: got %+v, err %v; want %+v", gotAgent, err, oldAgent)
			}
			gotTask, err := reconciled.Show(task.ID)
			if err != nil || !reflect.DeepEqual(gotTask, task) {
				t.Fatalf("task changed: got %+v, err %v; want %+v", gotTask, err, task)
			}
			gotComments, err := reconciled.Comments(task.ID)
			if err != nil || !reflect.DeepEqual(gotComments, comments) {
				t.Fatalf("comments changed: %v, %v", gotComments, err)
			}
			gotHistory, err := reconciled.Journal().QueryTaskEvents(p.JournalQueryV1{})
			if err != nil || !reflect.DeepEqual(gotHistory, history) {
				t.Fatalf("journal history changed: %v, %v", gotHistory, err)
			}
			afterIDs := modelProviderIDs(t, pool)
			for provider, id := range providerIDs {
				if afterIDs[provider] != id {
					t.Fatalf("provider %s ID changed: %d -> %d", provider, id, afterIDs[provider])
				}
			}
			afterModels := modelCatalogIDs(t, pool)
			for pair, id := range modelIDs {
				if afterModels[pair] != id {
					t.Fatalf("historical model %v ID changed: %d -> %d", pair, id, afterModels[pair])
				}
			}
			if len(afterModels) != len(modelIDs)+2 {
				t.Fatalf("unexpected reconciled catalog: %v", afterModels)
			}
			if err := reconciled.Close(); err != nil {
				t.Fatal(err)
			}
			// Agent registrations invalidate the existing verified-as-of ledger.
			// Let one ordinary open verify that new history; subsequent current
			// opens must be entirely write-free, not just idempotent inserts.
			verified, err := p.OpenBorrowedSQLite(pool, p.WithModelRegistry(registry))
			if err != nil {
				t.Fatal(err)
			}
			if err := verified.Close(); err != nil {
				t.Fatal(err)
			}
			var before int
			if err := pool.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec("PRAGMA query_only=ON"); err != nil {
				t.Fatal(err)
			}
			current, err := p.OpenBorrowedSQLite(pool, p.WithModelRegistry(registry))
			if err != nil {
				t.Fatalf("current registry must reopen read-only: %v", err)
			}
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			var after int
			if err := pool.QueryRow("SELECT total_changes()").Scan(&after); err != nil || before != after {
				t.Fatalf("current reopen wrote rows: before=%d after=%d err=%v", before, after, err)
			}
			if got := modelCatalogIDs(t, pool); !reflect.DeepEqual(got, afterModels) {
				t.Fatalf("reopen changed catalog IDs: %v -> %v", afterModels, got)
			}
		})
	}
}

func modelCatalogIDs(t *testing.T, pool *sql.DB) map[[2]string]int64 {
	t.Helper()
	rows, err := pool.Query("SELECT p.name, m.name, m.id FROM ml_models m JOIN providers p ON p.id=m.provider_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := make(map[[2]string]int64)
	for rows.Next() {
		var provider, model string
		var id int64
		if err := rows.Scan(&provider, &model, &id); err != nil {
			t.Fatal(err)
		}
		ids[[2]string{provider, model}] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func modelProviderIDs(t *testing.T, pool *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := pool.Query("SELECT name, id FROM providers")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := make(map[string]int64)
	for rows.Next() {
		var name string
		var id int64
		if err := rows.Scan(&name, &id); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}
