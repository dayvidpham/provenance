package provenance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A static rule that matches nothing looks exactly like a clean tree. The
// no-production-time-sleep rule spent its whole life in that state: its bare
// `time.Sleep($DURATION)` pattern parses as a Go type conversion rather than a
// call, so the gate never fired once. These tests close that gap: every rule
// under ast-grep/ must have a known-bad fixture under testdata/astgrep/ that it
// still reports, and every fixture must have a rule.

const (
	astGrepRuleDirectory    = "ast-grep"
	astGrepFixtureDirectory = "testdata/astgrep"
)

type astGrepMatch struct {
	RuleID string `json:"ruleId"`
	File   string `json:"file"`
}

func TestASTGrepRulesFireOnKnownBadFixtures(t *testing.T) {
	binary := requireASTGrep(t)
	rules := astGrepRuleFiles(t)
	if len(rules) == 0 {
		t.Fatalf("no ast-grep rules found in %s -- where: TestASTGrepRulesFireOnKnownBadFixtures; why: the rule directory is empty or was moved; impact: the lint gate enforces nothing; fix: restore the rule files or update astGrepRuleDirectory", astGrepRuleDirectory)
	}
	for ruleID, rulePath := range rules {
		t.Run(ruleID, func(t *testing.T) {
			fixture := filepath.Join(astGrepFixtureDirectory, ruleID+".go")
			if _, err := os.Stat(fixture); err != nil {
				t.Fatalf("rule %q has no known-bad fixture at %s -- when: proving the rule still matches; impact: the rule could match nothing and the scan would still pass; fix: add a minimal Go file that the rule must reject", ruleID, fixture)
			}
			matches := scanWithRule(t, binary, rulePath, stagedFixture(t, fixture))
			if len(matches) == 0 {
				t.Fatalf("rule %q reported nothing on its known-bad fixture %s -- why: the pattern no longer matches the shape it was written for (a bare call pattern parses as a type conversion in Go); impact: the rule is inert and the gate passes regardless of the code; fix: express the pattern with a context/selector pair and re-run this test", ruleID, fixture)
			}
			for _, match := range matches {
				if match.RuleID != ruleID {
					t.Errorf("fixture %s was reported by rule %q, not by %q; each fixture must isolate one rule", fixture, match.RuleID, ruleID)
				}
			}
		})
	}
}

func TestASTGrepFixturesAllBelongToARule(t *testing.T) {
	rules := astGrepRuleFiles(t)
	entries, err := os.ReadDir(astGrepFixtureDirectory)
	if err != nil {
		t.Fatalf("read %s: %v", astGrepFixtureDirectory, err)
	}
	fixtures := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		fixtures++
		ruleID := strings.TrimSuffix(name, ".go")
		if _, ok := rules[ruleID]; !ok {
			t.Errorf("fixture %s has no rule %q in %s; delete the fixture with its rule so a retired rule cannot look enforced", name, ruleID, astGrepRuleDirectory)
		}
	}
	if fixtures != len(rules) {
		t.Errorf("found %d fixtures for %d rules; every rule needs exactly one known-bad fixture", fixtures, len(rules))
	}
}

func astGrepRuleFiles(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(astGrepRuleDirectory)
	if err != nil {
		t.Fatalf("read %s: %v", astGrepRuleDirectory, err)
	}
	rules := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (filepath.Ext(name) != ".yml" && filepath.Ext(name) != ".yaml") {
			continue
		}
		path := filepath.Join(astGrepRuleDirectory, name)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		ruleID := ""
		for _, line := range strings.Split(string(content), "\n") {
			if strings.HasPrefix(line, "id:") {
				ruleID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
				break
			}
		}
		if ruleID == "" {
			t.Fatalf("rule file %s declares no id; the fixture pairing is keyed by rule id", path)
		}
		rules[ruleID] = path
	}
	return rules
}

// stagedFixture copies a fixture outside testdata/, which the rules ignore so
// that the repository-wide scan stays clean, into a scratch directory the rule
// will scan.
func stagedFixture(t *testing.T, fixture string) string {
	t.Helper()
	content, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	staged := filepath.Join(t.TempDir(), filepath.Base(fixture))
	if err := os.WriteFile(staged, content, 0o600); err != nil {
		t.Fatalf("stage fixture %s: %v", fixture, err)
	}
	return staged
}

func scanWithRule(t *testing.T, binary, rulePath, target string) []astGrepMatch {
	t.Helper()
	command := exec.Command(binary, "scan", "--rule", rulePath, "--json=compact", target)
	output, err := command.Output()
	if err != nil {
		// ast-grep exits non-zero when it reports an error-severity match, which
		// is the expected outcome for a known-bad fixture. Only an empty stdout
		// means the run itself failed.
		if len(output) == 0 {
			t.Fatalf("run %s scan --rule %s %s: %v", binary, rulePath, target, err)
		}
	}
	var matches []astGrepMatch
	if err := json.Unmarshal(output, &matches); err != nil {
		t.Fatalf("decode ast-grep JSON for rule %s: %v (output: %s)", rulePath, err, string(output))
	}
	return matches
}

func requireASTGrep(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("ast-grep")
	if err != nil {
		t.Fatalf("ast-grep is not on PATH -- where: the ast-grep rule meta-gate; when: before scanning the known-bad fixtures; why: this repository's lint gate is ast-grep and the meta-gate proves the rules still fire; impact: the rules would be unverified; fix: run the suite inside the development shell (nix develop), which provides ast-grep")
	}
	return binary
}
