// Command bd-import imports Beads (bd) issue-tracker history into a provenance store,
// idempotently, and optionally exports the result as data-labour-provenance Turtle.
//
// Usage:
//
//	bd-import -db <path> -source <dump.json|'bd'> -namespace <ns> [flags]
//
// Flags:
//
//	-db         provenance SQLite database path (created if absent)
//	-source     'bd' to read the live bd CLI, or a path to a pre-dumped Dump JSON
//	-namespace  provenance namespace for minted ids (e.g. taxonomy-of-benchmarks)
//	-bd-dir     when -source=bd, the repo directory to run bd in (default: cwd)
//	-bd-bin     when -source=bd, the bd binary (default: bd)
//	-dry-run    print the mapping summary without writing to the store
//	-dump-out   when -source=bd, also write the collected Dump JSON to this path
//	-export     after import, write data-labour-prov Turtle to this path
//
// bd-import follows cmd/demo's plain flag/stdlib style (no cobra).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/dayvidpham/provenance"
	"github.com/dayvidpham/provenance/pkg/bdimport"
	"github.com/dayvidpham/provenance/pkg/provo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bd-import:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dbPath    = flag.String("db", "", "provenance SQLite database path (required unless -dry-run)")
		source    = flag.String("source", "", "'bd' for the live bd CLI, or a path to a Dump JSON (required)")
		namespace = flag.String("namespace", "", "provenance namespace for minted ids (required)")
		bdDir     = flag.String("bd-dir", "", "when -source=bd, the repo directory to run bd in (default: cwd)")
		bdBin     = flag.String("bd-bin", "", "when -source=bd, the bd binary (default: bd)")
		dryRun    = flag.Bool("dry-run", false, "print the mapping summary without writing")
		dumpOut   = flag.String("dump-out", "", "when -source=bd, also write the collected Dump JSON here")
		exportTTL = flag.String("export", "", "after import, write data-labour-prov Turtle to this path")
	)
	flag.Parse()

	if *source == "" {
		return fmt.Errorf("-source is required ('bd' or a Dump JSON path)")
	}
	if *namespace == "" {
		return fmt.Errorf("-namespace is required")
	}

	src, err := resolveSource(*source, *bdBin, *bdDir)
	if err != nil {
		return err
	}

	// Optionally persist the collected Dump (only meaningful for the bd source).
	if *dumpOut != "" {
		dump, err := src.Load()
		if err != nil {
			return err
		}
		if err := bdimport.WriteDump(*dumpOut, dump); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote dump: %s (%d issues, %d comments)\n", *dumpOut, len(dump.Issues), len(dump.Comments))
		// Re-read from the persisted file so import and artifact are identical bytes.
		src = bdimport.FileSource{Path: *dumpOut}
	}

	// Dry-run never opens the store.
	if *dryRun {
		res, err := bdimport.Import(nil, src, bdimport.Options{Namespace: *namespace, DryRun: true})
		if err != nil {
			return err
		}
		return printSummary(src.Describe(), res)
	}

	if *dbPath == "" {
		return fmt.Errorf("-db is required (unless -dry-run)")
	}
	tr, err := provenance.OpenSQLite(*dbPath)
	if err != nil {
		return err
	}
	defer tr.Close()

	res, err := bdimport.Import(tr, src, bdimport.Options{Namespace: *namespace})
	if err != nil {
		return err
	}
	if err := printSummary(src.Describe(), res); err != nil {
		return err
	}

	if *exportTTL != "" {
		if err := exportTurtle(*exportTTL, tr, *namespace); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote turtle: %s\n", *exportTTL)
	}
	return nil
}

// resolveSource builds the Source from the -source value: the literal "bd" selects the
// live CLI; anything else is treated as a path to a Dump JSON.
func resolveSource(source, bin, dir string) (bdimport.Source, error) {
	if source == "bd" {
		return bdimport.BDSource{Bin: bin, Dir: dir}, nil
	}
	if _, err := os.Stat(source); err != nil {
		return nil, fmt.Errorf("source %q is not 'bd' and not a readable file: %w", source, err)
	}
	return bdimport.FileSource{Path: source}, nil
}

func exportTurtle(path string, tr provenance.Tracker, namespace string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	defer f.Close()
	opts := provo.Options{
		BaseIRI:  "urn:provenance:" + namespace + ":",
		Registry: provenance.DefaultModelRegistry(),
	}
	return provo.ExportTurtle(f, tr, opts)
}

func printSummary(source string, res bdimport.Result) error {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	mode := "import"
	if res.DryRun {
		mode = "dry-run"
	}
	fmt.Printf("bd-import %s (source %s):\n%s\n", mode, source, b)
	return nil
}
