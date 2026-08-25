// Package astgrepfixture holds known-bad sources. Each file must be rejected by
// the ast-grep rule that shares its name; TestASTGrepRulesFireOnKnownBadFixtures
// proves it. The files are never compiled: testdata is not a Go package.
package astgrepfixture

import "time"

func waitsWithoutSynchronisation() {
	time.Sleep(50 * time.Millisecond)
}
