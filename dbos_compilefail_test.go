package provenance_test

// dbos_compilefail_test.go asserts that the negative fixtures in ./compilefail do
// NOT compile under the compilefail build tag, proving at the type level that
// adapter callers cannot pass raw DBOS options or override the durable identity
// (issue #6 external compile-fail fixtures).

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCompileFail_AdapterRejectsRawOptionsAndOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	cmd := exec.Command("go", "build", "-tags", "compilefail", "./compilefail/")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("compilefail fixtures unexpectedly compiled; the adapter surface does not reject the illegal calls\n%s", out)
	}
	got := string(out)
	// Each intended diagnostic must appear (too-many-args for the option calls, and
	// unexported-field access for the identity override).
	wants := []string{
		"too many arguments",
		"applicationVersion",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("compiler output missing expected diagnostic %q\nfull output:\n%s", w, got)
		}
	}
}
