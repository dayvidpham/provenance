package provo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dayvidpham/provenance"
)

// graphTurtleFilename is the name of the PROV-O Turtle payload inside the crate.
const graphTurtleFilename = "graph.ttl"

// roCrateMetadataFilename is the fixed RO-Crate descriptor filename (spec-mandated).
const roCrateMetadataFilename = "ro-crate-metadata.json"

// ExportROCrate writes a minimal but valid RO-Crate to dir: the PROV-O Turtle graph
// (graph.ttl, produced by ExportTurtle) plus an ro-crate-metadata.json descriptor
// that declares the crate root and graph.ttl as a File entity with encodingFormat
// text/turtle. dir is created if it does not exist. It is a pure read of the tracker.
//
// This is intentionally small: full Process Run Crate profiling (typing the PROV
// activities/agents as crate entities) is future work.
func ExportROCrate(dir string, tr provenance.Tracker, opts Options) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("provo.ExportROCrate: create dir %q: %w", dir, err)
	}

	ttlPath := filepath.Join(dir, graphTurtleFilename)
	f, err := os.Create(ttlPath)
	if err != nil {
		return fmt.Errorf("provo.ExportROCrate: create %q: %w", ttlPath, err)
	}
	if err := ExportTurtle(f, tr, opts); err != nil {
		_ = f.Close()
		return fmt.Errorf("provo.ExportROCrate: write graph: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("provo.ExportROCrate: close %q: %w", ttlPath, err)
	}

	meta := roCrateMetadata()
	blob, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("provo.ExportROCrate: marshal metadata: %w", err)
	}
	blob = append(blob, '\n')
	metaPath := filepath.Join(dir, roCrateMetadataFilename)
	if err := os.WriteFile(metaPath, blob, 0o644); err != nil {
		return fmt.Errorf("provo.ExportROCrate: write %q: %w", metaPath, err)
	}
	return nil
}

// roCrateMetadata returns the RO-Crate 1.1 descriptor graph declaring the crate root
// (./) and graph.ttl as a text/turtle File entity. Field order is fixed so the JSON
// output is deterministic.
func roCrateMetadata() map[string]any {
	return map[string]any{
		"@context": "https://w3id.org/ro/crate/1.1/context",
		"@graph": []any{
			map[string]any{
				"@type":      "CreativeWork",
				"@id":        roCrateMetadataFilename,
				"conformsTo": map[string]any{"@id": "https://w3id.org/ro/crate/1.1"},
				"about":      map[string]any{"@id": "./"},
			},
			map[string]any{
				"@type":       "Dataset",
				"@id":         "./",
				"name":        "PROV-O data-labour provenance crate",
				"description": "PROV-O / data-labour-prov export of a provenance Tracker graph.",
				"hasPart":     []any{map[string]any{"@id": graphTurtleFilename}},
			},
			map[string]any{
				"@type":          "File",
				"@id":            graphTurtleFilename,
				"name":           "PROV-O data-labour provenance graph",
				"encodingFormat": "text/turtle",
			},
		},
	}
}
