package provenance

import (
	"testing"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
)

// shutdownDBOSRoot stops a DBOS root and reports a shutdown that did not
// finish. The runtime returns an error when the timeout expires with resources
// still running; a test that ignored it could then close a shared SQLite handle
// the runtime is still writing to, and the failure would surface later as an
// unrelated corruption.
func shutdownDBOSRoot(t *testing.T, root dbos.Context, timeout time.Duration) {
	t.Helper()
	if root == nil {
		return
	}
	if err := dbos.Shutdown(root, timeout); err != nil {
		t.Errorf("DBOS shutdown did not finish within %s: %v", timeout, err)
	}
}
