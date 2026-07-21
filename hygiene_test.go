package provenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCandidateDiffUsesEnduringVocabulary(t *testing.T) {
	rules := []struct {
		name    string
		pattern string
	}{
		{"ticket reference", `(?i)(?:\bissue\s*(?:#|[a-z0-9_-]+/)|#\d+)`},
		{"acceptance-session label", `(?i)\b` + `UAT` + `(?:[- ]?\d|\s+[A-Z]\d)`},
		{"repair-wave label", `\b` + `FIX` + `-[A-Z0-9-]+\b`},
		{"numbered delivery area", `(?i)\b(?:s\d+\.\d+|` + `SliceS` + `\d+|slices?[- ]?\d+)\b`},
		{"delivery stratum", `(?i)\b(?:operations?|implementation|delivery|journal-base|write)[ -]layer\b`},
	}

	changed := gitOutput(t, "diff", "--name-only", "--diff-filter=ACMR", "main")
	changed += "\n" + gitOutput(t, "ls-files", "--others", "--exclude-standard")
	for _, name := range strings.Fields(changed) {
		if filepath.IsAbs(name) || strings.Contains(name, "..") {
			t.Fatalf("git returned unsafe changed path %q", name)
		}
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".go" && ext != ".yaml" && ext != ".yml" {
			continue
		}
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read changed file %s: %v", name, err)
		}
		assertEnduringVocabulary(t, name, string(content), rules)
	}

	message := gitOutput(t, "log", "-1", "--format=%B", "HEAD")
	assertEnduringVocabulary(t, "candidate commit message", message, rules)
}

func assertEnduringVocabulary(t *testing.T, source, content string, rules []struct {
	name    string
	pattern string
}) {
	t.Helper()
	for _, rule := range rules {
		re := regexp.MustCompile(rule.pattern)
		if match := re.FindString(content); match != "" {
			t.Errorf("%s contains transient %s %q", source, rule.name, match)
		}
	}
}

func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
