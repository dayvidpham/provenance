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

// enduringVocabularyRules reject names for transient delivery process artefacts
// — the review round, wave, or task that produced a change. Such a name is
// meaningless to a reader of the shipped source and goes stale the moment the
// process moves on. Names for the durable protocol domain (the Reviewer role,
// the review phase a task is in) are legitimate and are not matched here: the
// rules require a process-artefact suffix (round, wave, cycle, pass, evidence,
// finding, verdict) or an explicit round number.
var enduringVocabularyRules = []vocabularyRule{
	{"Aura task identifier", `\baura` + `-plugins-[a-z0-9]+\b`},
	{"delivery-slice label", `\b` + `SliceS` + `[0-9]+\b`},
	{"review-wave label", `\b` + `REVIEW-WAVE` + `-[A-Z0-9-]+\b`},
	{"review-process identifier", `(?i)(func|type|var|const)[ \t]+[a-z0-9_]*` + `review` + `(er)?[_]?(round|wave|cycle|pass|evidence|finding|verdict)`},
	{"numbered-round identifier", `(?i)(func|type|var|const)[ \t]+[a-z0-9_]*(` + `review|fix|blocker` + `)?[_]?(` + `round|wave|cycle` + `)[_]?[0-9]`},
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

// vocabularyRuleFixtures pairs every rule with a source fragment it must
// reject. TestRepositoryVocabularyScannerRejectsEveryRuleFixture asserts the
// pairing is total, so a rule added without a proof-of-firing fixture — or a
// rule whose pattern silently stops matching — fails the suite.
var vocabularyRuleFixtures = map[string]string{
	"Aura task identifier":      strings.Join([]string{"aura", "plugins", "forbidden"}, "-"),
	"delivery-slice label":      strings.Join([]string{"Slice", "S3"}, ""),
	"review-wave label":         strings.Join([]string{"REVIEW", "WAVE", "B2"}, "-"),
	"review-process identifier": strings.Join([]string{"func Test", "Review", "Evidence", "Rejects"}, ""),
	"numbered-round identifier": strings.Join([]string{"func fix", "Round", "2Counts"}, ""),
}

func TestRepositoryVocabularyScannerRejectsEveryRuleFixture(t *testing.T) {
	if len(vocabularyRuleFixtures) != len(enduringVocabularyRules) {
		t.Fatalf("vocabulary fixtures cover %d rules, want %d: every rule needs a known-bad fixture", len(vocabularyRuleFixtures), len(enduringVocabularyRules))
	}
	for _, rule := range enduringVocabularyRules {
		fixture, ok := vocabularyRuleFixtures[rule.name]
		if !ok {
			t.Fatalf("rule %q has no known-bad fixture; add one to vocabularyRuleFixtures", rule.name)
		}
		findings := inspectVocabulary("fixture.go", "package fixture\n"+fixture+"() {}\n", enduringVocabularyRules)
		matched := false
		for _, finding := range findings {
			if finding.rule == rule.name {
				matched = true
			}
		}
		if !matched {
			t.Errorf("rule %q did not fire on its known-bad fixture %q; the pattern no longer detects the shape it was written for", rule.name, fixture)
		}
	}
}

func TestRepositoryVocabularyScannerDetectsForbiddenFixture(t *testing.T) {
	root := t.TempDir()
	fixture := vocabularyRuleFixtures["Aura task identifier"]
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
