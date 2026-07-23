package provenance

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type shippedSurfaceRoot string

type shippedExtension string

type vocabularyRule struct {
	name    string
	pattern string
}

type vocabularyFinding struct {
	source string
	rule   string
	match  string
}

const shippedRepositoryTree shippedSurfaceRoot = "."

const (
	goSourceExtension shippedExtension = ".go"
	yamlExtension     shippedExtension = ".yaml"
	ymlExtension      shippedExtension = ".yml"
)

var enduringVocabularyRules = []vocabularyRule{
	{"Aura task identifier", `\baura` + `-plugins-[a-z0-9]+\b`},
	{"delivery-slice label", `\b` + `SliceS` + `[0-9]+\b`},
	{"review-wave label", `\b` + `REVIEW-WAVE` + `-[A-Z0-9-]+\b`},
}

var shippedExtensions = map[shippedExtension]struct{}{
	goSourceExtension: {},
	yamlExtension:     {},
	ymlExtension:      {},
}

// These trees contain repository-internal state or archived proposals, not the
// source and fixtures shipped as the current library surface.
var excludedRepositoryTrees = map[string]struct{}{
	".beads":           {},
	".direnv":          {},
	"archive":          {},
	"docs":             {},
	"internal/archive": {},
	"vendor":           {},
	"worktree":         {},
}

func TestRepositoryTreeUsesEnduringVocabulary(t *testing.T) {
	files, findings, err := scanRepositoryVocabulary(shippedRepositoryTree, enduringVocabularyRules)
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("repository vocabulary scan examined no shipped Go or YAML files")
	}
	for _, finding := range findings {
		t.Errorf("%s contains transient %s %q", finding.source, finding.rule, finding.match)
	}
}

func TestRepositoryVocabularyScannerDetectsForbiddenFixture(t *testing.T) {
	root := t.TempDir()
	fixture := strings.Join([]string{"aura", "plugins", "forbidden"}, "-")
	if err := os.WriteFile(filepath.Join(root, "shipped.go"), []byte("package fixture // "+fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "internal", "archive")
	if err := os.MkdirAll(archive, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "ignored.go"), []byte("package archive // "+fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	files, findings, err := scanRepositoryVocabulary(shippedSurfaceRoot(root), enduringVocabularyRules)
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Fatalf("mutation fixture scanned %d files, want only the shipped Go file", files)
	}
	if len(findings) != 1 || findings[0].rule != "Aura task identifier" {
		t.Fatalf("forbidden-token mutation produced findings %+v, want one Aura task identifier finding", findings)
	}
}

func scanRepositoryVocabulary(root shippedSurfaceRoot, rules []vocabularyRule) (int, []vocabularyFinding, error) {
	files := 0
	var findings []vocabularyFinding
	err := filepath.WalkDir(string(root), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relativePath, err := filepath.Rel(string(root), path)
		if err != nil {
			return err
		}
		relativePath = filepath.ToSlash(relativePath)
		if entry.IsDir() {
			if relativePath != "." {
				if _, excluded := excludedRepositoryTrees[relativePath]; excluded {
					return filepath.SkipDir
				}
			}
			return nil
		}
		ext := shippedExtension(strings.ToLower(filepath.Ext(path)))
		if _, shipped := shippedExtensions[ext]; !shipped {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		findings = append(findings, inspectVocabulary(relativePath, string(content), rules)...)
		return nil
	})
	return files, findings, err
}

func inspectVocabulary(source, content string, rules []vocabularyRule) []vocabularyFinding {
	var findings []vocabularyFinding
	for _, rule := range rules {
		re := regexp.MustCompile(rule.pattern)
		if match := re.FindString(content); match != "" {
			findings = append(findings, vocabularyFinding{source: source, rule: rule.name, match: match})
		}
	}
	return findings
}
