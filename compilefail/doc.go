// Package compilefail holds external negative fixtures that MUST NOT compile,
// proving at the type level that adapter callers cannot pass raw DBOS options or
// override the durable identity. The offending files are gated behind the
// "compilefail" build tag so normal builds and tests exclude them; the fixture
// test (dbos_compilefail_test.go) invokes `go build -tags compilefail` on this
// package and asserts the compile fails with the expected diagnostics.
package compilefail
