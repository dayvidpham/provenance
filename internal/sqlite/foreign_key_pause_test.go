package sqlite

import (
	"errors"
	"strings"
	"testing"
)

// TestPauseForeignKeysRestoresAndReportsBoth covers the two dispositions of the
// bracketing helper every rebuild and adversarial seam uses: on the happy path the
// operation's own error is returned unchanged and enforcement is back on; on a
// failed restore the restore error is reported alongside the operation's error
// instead of being dropped on the already-failed path.
func TestPauseForeignKeysRestoresAndReportsBoth(t *testing.T) {
	t.Parallel()
	db, err := Open(t.TempDir()+"/fk-pause.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	t.Run("restores enforcement and passes the operation error through", func(t *testing.T) {
		scope := takePoolScope(t, db)
		defer scope.release()
		restoreFK, err := scope.pauseForeignKeys("test-restore")
		if err != nil {
			t.Fatalf("pauseForeignKeys: %v", err)
		}
		var paused int64
		if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA foreign_keys").Scan(&paused); err != nil {
			t.Fatalf("read foreign_keys while paused: %v", err)
		}
		if paused != 0 {
			t.Fatalf("foreign_keys while paused = %d, want 0", paused)
		}
		operationErr := errors.New("operation failed")
		if got := restoreFK(operationErr); !errors.Is(got, operationErr) {
			t.Fatalf("restore(operationErr) = %v, want the operation error preserved", got)
		}
		var restored int64
		if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA foreign_keys").Scan(&restored); err != nil {
			t.Fatalf("read foreign_keys after restore: %v", err)
		}
		if restored != 1 {
			t.Fatalf("foreign_keys after restore = %d, want 1", restored)
		}
		if got := restoreFK(nil); got != nil {
			t.Fatalf("restore(nil) on a healthy connection = %v, want nil", got)
		}
	})

	t.Run("reports a failed restore joined with the operation error", func(t *testing.T) {
		scope := takePoolScope(t, db)
		defer scope.release()
		restoreFK, err := scope.pauseForeignKeys("test-failed-restore")
		if err != nil {
			t.Fatalf("pauseForeignKeys: %v", err)
		}
		// Closing the leased connection is the deterministic way to make the restore
		// statement fail: the pragma can no longer be executed or proven.
		if err := scope.conn.Close(); err != nil {
			t.Fatalf("close leased connection to force a restore failure: %v", err)
		}
		operationErr := errors.New("operation failed")
		got := restoreFK(operationErr)
		if got == nil {
			t.Fatal("failed restore returned nil; the restore error was dropped on the already-failed path")
		}
		if !errors.Is(got, operationErr) {
			t.Errorf("failed restore error = %v, want the original operation error joined in", got)
		}
		if !strings.Contains(got.Error(), "restore foreign-key enforcement") {
			t.Errorf("failed restore error = %v, want it to name the lost enforcement", got)
		}
		if !scope.discarded {
			t.Error("connection was not retired after an unprovable restore; it must never return to the pool")
		}
	})
}

// TestSuppressCheckConstraintsRestoresAndReportsBoth pins the sibling bracket the
// adversarial seams use. A fixture that left CHECK enforcement off on a pooled
// connection would disarm the schema for every later operation that drew it, so
// the restore is proven and an unprovable one retires the connection.
func TestSuppressCheckConstraintsRestoresAndReportsBoth(t *testing.T) {
	t.Parallel()
	db, err := Open(t.TempDir()+"/check-suppress.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	t.Run("restores enforcement and passes the operation error through", func(t *testing.T) {
		scope := takePoolScope(t, db)
		defer scope.release()
		restoreChecks, err := scope.suppressCheckConstraints("test-restore")
		if err != nil {
			t.Fatalf("suppressCheckConstraints: %v", err)
		}
		var suppressed int64
		if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA ignore_check_constraints").Scan(&suppressed); err != nil {
			t.Fatalf("read ignore_check_constraints while suppressed: %v", err)
		}
		if suppressed != 1 {
			t.Fatalf("ignore_check_constraints while suppressed = %d, want 1", suppressed)
		}
		operationErr := errors.New("operation failed")
		if got := restoreChecks(operationErr); !errors.Is(got, operationErr) {
			t.Fatalf("restore(operationErr) = %v, want the operation error preserved", got)
		}
		var restored int64
		if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA ignore_check_constraints").Scan(&restored); err != nil {
			t.Fatalf("read ignore_check_constraints after restore: %v", err)
		}
		if restored != 0 {
			t.Fatalf("ignore_check_constraints after restore = %d, want 0", restored)
		}
		if got := restoreChecks(nil); got != nil {
			t.Fatalf("restore(nil) on a healthy connection = %v, want nil", got)
		}
	})

	t.Run("reports a failed restore joined with the operation error", func(t *testing.T) {
		scope := takePoolScope(t, db)
		defer scope.release()
		restoreChecks, err := scope.suppressCheckConstraints("test-failed-restore")
		if err != nil {
			t.Fatalf("suppressCheckConstraints: %v", err)
		}
		if err := scope.conn.Close(); err != nil {
			t.Fatalf("close leased connection to force a restore failure: %v", err)
		}
		operationErr := errors.New("operation failed")
		got := restoreChecks(operationErr)
		if got == nil {
			t.Fatal("failed restore returned nil; the restore error was dropped on the already-failed path")
		}
		if !errors.Is(got, operationErr) {
			t.Errorf("failed restore error = %v, want the original operation error joined in", got)
		}
		if !strings.Contains(got.Error(), "restore CHECK enforcement") {
			t.Errorf("failed restore error = %v, want it to name the lost enforcement", got)
		}
		if !scope.discarded {
			t.Error("connection was not retired after an unprovable restore; it must never return to the pool")
		}
	})
}
