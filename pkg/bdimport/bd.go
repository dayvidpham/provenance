package bdimport

// bd.go models the subset of the Beads (bd) JSON surface this importer consumes and
// the Source abstraction that yields it. Two sources implement it: FileSource reads a
// pre-dumped Dump JSON (so tests and CI need no bd binary), and BDSource shells out to
// the real `bd` CLI (READ-ONLY verbs only: `bd list` and `bd comments`).
//
// The JSON shapes were discovered empirically against a real bd 0.57.0 database:
//   - `bd list --all --json --limit 0` returns every issue with `labels` and
//     `dependencies` inline (each dependency is {issue_id, depends_on_id, type, …}).
//   - Comments are NOT inline; `bd comments <id> --json` returns
//     [{id, issue_id, author, text, created_at}].
// A Dump is the union of those two: the list array plus the flattened comments.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"
)

// Issue is one bd issue, decoded from `bd list --json`. Only the fields the importer
// maps are modelled; unknown JSON fields are ignored by the decoder.
type Issue struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Status      string       `json:"status"`
	Priority    int          `json:"priority"`
	IssueType   string       `json:"issue_type"`
	Owner       string       `json:"owner"`
	CreatedBy   string       `json:"created_by"`
	Assignee    string       `json:"assignee"`
	Labels      []string     `json:"labels"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	ClosedAt    *time.Time   `json:"closed_at"`
	CloseReason string       `json:"close_reason"`
	Deps        []Dependency `json:"dependencies"`
}

// Dependency is one bd dependency edge as it appears inline in `bd list --json`.
// Semantics: IssueID depends on DependsOnID with the given Type. For Type "blocks",
// IssueID is blocked by DependsOnID (must finish first).
type Dependency struct {
	IssueID     string    `json:"issue_id"`
	DependsOnID string    `json:"depends_on_id"`
	Type        string    `json:"type"`
	CreatedAt   time.Time `json:"created_at"`
}

// Comment is one bd comment, decoded from `bd comments <id> --json`.
type Comment struct {
	ID        int       `json:"id"`
	IssueID   string    `json:"issue_id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// Dump is the self-contained, bd-independent import input: every issue plus every
// comment. It is what FileSource reads and BDSource produces, and what the dog-food
// artifact persists under testdata/dogfood.
type Dump struct {
	Issues   []Issue   `json:"issues"`
	Comments []Comment `json:"comments"`
}

// Source yields a Dump. Implementations must be deterministic in the data they return
// (ordering is normalized by the importer, not the source).
type Source interface {
	Load() (Dump, error)
	// Describe returns a short human label for the source (for CLI/dry-run output).
	Describe() string
}

// ---------------------------------------------------------------------------
// FileSource — a pre-dumped Dump JSON file
// ---------------------------------------------------------------------------

// FileSource reads a Dump from a JSON file on disk. This is the source tests and CI
// use: it needs no bd binary.
type FileSource struct{ Path string }

// Load reads and decodes the Dump JSON at the configured path.
func (s FileSource) Load() (Dump, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return Dump{}, fmt.Errorf("bdimport.FileSource: read %q: %w", s.Path, err)
	}
	var d Dump
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&d); err != nil {
		return Dump{}, fmt.Errorf("bdimport.FileSource: decode %q: %w", s.Path, err)
	}
	return d, nil
}

// Describe implements Source.
func (s FileSource) Describe() string { return "file:" + s.Path }

// ---------------------------------------------------------------------------
// BDSource — the real `bd` CLI (read-only)
// ---------------------------------------------------------------------------

// BDSource loads a Dump by shelling out to the `bd` CLI in a repository directory.
// It uses only READ-ONLY verbs — `bd list` and `bd comments` — and never mutates the
// database. Bin defaults to "bd"; Dir defaults to the current working directory.
type BDSource struct {
	Bin string // path to the bd binary; "" → "bd"
	Dir string // working directory (a repo with a .beads db); "" → cwd
}

func (s BDSource) bin() string {
	if s.Bin == "" {
		return "bd"
	}
	return s.Bin
}

// Load runs `bd list --all --json --limit 0` for the issues (labels + dependencies
// inline) and `bd comments <id> --json` for every issue that has comments.
func (s BDSource) Load() (Dump, error) {
	listOut, err := s.run("list", "--all", "--json", "--limit", "0")
	if err != nil {
		return Dump{}, fmt.Errorf("bdimport.BDSource: bd list: %w", err)
	}
	var issues []Issue
	if err := json.Unmarshal(listOut, &issues); err != nil {
		return Dump{}, fmt.Errorf("bdimport.BDSource: decode bd list: %w", err)
	}
	// Deterministic order for the comment sweep.
	sort.Slice(issues, func(i, j int) bool { return issues[i].ID < issues[j].ID })

	var comments []Comment
	for _, iss := range issues {
		out, err := s.run("comments", iss.ID, "--json")
		if err != nil {
			return Dump{}, fmt.Errorf("bdimport.BDSource: bd comments %s: %w", iss.ID, err)
		}
		trimmed := bytes.TrimSpace(out)
		if len(trimmed) == 0 || string(trimmed) == "null" {
			continue
		}
		var cs []Comment
		if err := json.Unmarshal(trimmed, &cs); err != nil {
			return Dump{}, fmt.Errorf("bdimport.BDSource: decode bd comments %s: %w", iss.ID, err)
		}
		comments = append(comments, cs...)
	}
	return Dump{Issues: issues, Comments: comments}, nil
}

// Describe implements Source.
func (s BDSource) Describe() string {
	dir := s.Dir
	if dir == "" {
		dir = "."
	}
	return "bd:" + dir
}

func (s BDSource) run(args ...string) ([]byte, error) {
	cmd := exec.Command(s.bin(), args...)
	if s.Dir != "" {
		cmd.Dir = s.Dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// WriteDump serializes a Dump to indented JSON at path (used to persist the dog-food
// artifact and test fixtures).
func WriteDump(path string, d Dump) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("bdimport.WriteDump: marshal: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("bdimport.WriteDump: write %q: %w", path, err)
	}
	return nil
}
