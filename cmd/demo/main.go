// Command demo exercises the full provenance + bestiary integration stack.
// Run with: go run ./cmd/demo
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dayvidpham/provenance"
)

func main() {
	dbPath := filepath.Join(os.TempDir(), "provenance-demo.db")
	defer os.Remove(dbPath)

	fmt.Println("=== Provenance + Bestiary Integration Demo ===")
	fmt.Println()

	// --- Catalog ---

	reg := provenance.DefaultModelRegistry()
	models := reg.Models()
	anthropic := reg.ModelsByProvider(provenance.ProviderAnthropic)
	google := reg.ModelsByProvider(provenance.ProviderGoogle)
	openai := reg.ModelsByProvider(provenance.ProviderOpenAI)
	fmt.Printf("Bestiary catalog: %d models (%d Anthropic, %d Google, %d OpenAI)\n\n",
		len(models), len(anthropic), len(google), len(openai))

	// --- Provider validation ---

	fmt.Println("Provider validation (catalog-membership, case-sensitive):")
	for _, p := range []provenance.Provider{"anthropic", "ANTHROPIC", "Google", "", "unknown"} {
		fmt.Printf("  provenance.IsValid(%q) = %v\n", p, provenance.IsValid(p))
	}
	fmt.Println()

	// --- Open tracker ---

	tr, err := provenance.OpenSQLite(dbPath)
	if err != nil {
		fatal("OpenSQLite: %v", err)
	}
	defer tr.Close()

	// --- Register multi-provider agents ---

	fmt.Println("Registering agents from bestiary catalog:")

	architect, err := tr.RegisterMLAgent("aura",
		provenance.RoleArchitect, provenance.ProviderAnthropic, provenance.ModelID("claude-opus-4-6"))
	if err != nil {
		fatal("RegisterMLAgent(architect): %v", err)
	}
	fmt.Printf("  Architect: %s\n    Role: %s | Model: %s | Provider: %s\n",
		architect.ID, architect.Role, architect.Model.Name, architect.Model.Provider)

	worker, err := tr.RegisterMLAgent("aura",
		provenance.RoleWorker, provenance.ProviderGoogle, provenance.ModelID("gemini-2.0-flash"))
	if err != nil {
		fatal("RegisterMLAgent(worker): %v", err)
	}
	fmt.Printf("  Worker:    %s\n    Role: %s | Model: %s | Provider: %s\n",
		worker.ID, worker.Role, worker.Model.Name, worker.Model.Provider)
	fmt.Println()

	// --- DB round-trip ---

	fmt.Println("Read-back from SQLite (verify string Provider, not integer):")
	got, err := tr.MLAgent(architect.ID)
	if err != nil {
		fatal("MLAgent: %v", err)
	}
	fmt.Printf("  Architect: Provider=%q  Model=%q  Role=%s\n",
		got.Model.Provider, got.Model.Name, got.Role)

	got2, err := tr.MLAgent(worker.ID)
	if err != nil {
		fatal("MLAgent: %v", err)
	}
	fmt.Printf("  Worker:    Provider=%q  Model=%q  Role=%s\n",
		got2.Model.Provider, got2.Model.Name, got2.Role)
	fmt.Println()

	// --- Registry rejection ---

	fmt.Println("Registry rejects unknown models before DB:")
	_, err = tr.RegisterMLAgent("aura",
		provenance.RoleWorker, provenance.ProviderLocal, provenance.ModelID("nonexistent-model"))
	fmt.Printf("  Error: %v\n\n", err)

	// --- Provenance chain ---

	task, err := tr.Create("aura", "REQUEST: Multi-provider demo", "",
		provenance.TaskTypeFeature, provenance.PriorityHigh, provenance.PhaseRequest)
	if err != nil {
		fatal("Create: %v", err)
	}

	activity, err := tr.StartActivity(architect.ID,
		provenance.PhasePropose, provenance.StageInProgress, "Writing proposal")
	if err != nil {
		fatal("StartActivity: %v", err)
	}
	tr.AddEdge(task.ID, activity.ID.String(), provenance.EdgeGeneratedBy)
	tr.AddEdge(task.ID, architect.ID.String(), provenance.EdgeAttributedTo)
	tr.AddEdge(task.ID, worker.ID.String(), provenance.EdgeAttributedTo)
	tr.EndActivity(activity.ID)

	fmt.Println("Provenance chain:")
	fmt.Printf("  Task: %s\n", task.ID)
	fmt.Printf("  GeneratedBy: activity %s\n", activity.ID)
	fmt.Printf("  AttributedTo: %s (claude-opus-4-6)\n", architect.ID)
	fmt.Printf("  AttributedTo: %s (gemini-2.0-flash)\n", worker.ID)
	fmt.Println()

	// --- Persistence ---

	tr.Close()
	tr2, err := provenance.OpenSQLite(dbPath)
	if err != nil {
		fatal("OpenSQLite reopen: %v", err)
	}

	found, err := tr2.MLAgent(worker.ID)
	if err != nil {
		fatal("MLAgent after reopen: %v", err)
	}

	attrTo := provenance.EdgeAttributedTo
	edges, err := tr2.Edges(task.ID, &attrTo)
	if err != nil {
		fatal("Edges after reopen: %v", err)
	}

	fmt.Println("Persistence (reopen DB):")
	fmt.Printf("  Worker survived: Provider=%q  Model=%q\n", found.Model.Provider, found.Model.Name)
	fmt.Printf("  AttributedTo edges survived: %d\n", len(edges))
	tr2.Close()

	fmt.Println()
	fmt.Println("=== Demo complete ===")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
