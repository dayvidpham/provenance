package provenance

// dbos_runtime_contract_test.go pins the assumptions Provenance makes about the
// DBOS runtime's own error text and system-schema policy. Each assertion reads
// the runtime's real behaviour, so a library change breaks a test rather than
// silently disabling a branch.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
)

// The runtime persists a workflow error as text. On retrieval Provenance
// reconstructs the closed retry code from that text, so the marker it searches
// for must actually appear in the runtime's own formatting.
func TestTerminalCauseMarkerAppearsInTheRuntimeErrorText(t *testing.T) {
	t.Parallel()
	typed := &dbos.Error{
		Code:    dbos.ErrorCodeMaxStepRetriesExceeded,
		Message: "Step provenance.apply-step/x has exceeded its maximum of 3 retries: boom",
	}
	if !strings.Contains(typed.Error(), terminalMaxStepRetriesMarker) {
		t.Fatalf("the reconstruction marker %q does not appear in the runtime's error text %q: that branch is dead code",
			terminalMaxStepRetriesMarker, typed.Error())
	}
}

func TestTerminalDBOSCauseReconstructsTheRetryCodeFromPersistedText(t *testing.T) {
	t.Parallel()
	typed := &dbos.Error{
		Code:    dbos.ErrorCodeMaxStepRetriesExceeded,
		Message: "Step provenance.apply-step/x has exceeded its maximum of 3 retries: boom",
	}
	// Retrieval returns the persisted text without the original Go type.
	cause := terminalDBOSCause(errors.New(typed.Error()), "provenance.apply/golden")
	var reconstructed *dbos.Error
	if !errors.As(cause, &reconstructed) {
		t.Fatalf("terminalDBOSCause did not reconstruct a typed runtime error from %q", typed.Error())
	}
	if reconstructed.Code != dbos.ErrorCodeMaxStepRetriesExceeded {
		t.Errorf("reconstructed code = %s, want %s", reconstructed.Code, dbos.ErrorCodeMaxStepRetriesExceeded)
	}
	if reconstructed.WorkflowID != "provenance.apply/golden" {
		t.Errorf("reconstructed WorkflowID = %q, want the requested workflow", reconstructed.WorkflowID)
	}
}

func TestTerminalDBOSCausePassesAnAlreadyTypedErrorThrough(t *testing.T) {
	t.Parallel()
	typed := &dbos.Error{Code: dbos.ErrorCodeWorkflowExecution, Message: "already typed"}
	if got := terminalDBOSCause(typed, "wf"); got != error(typed) {
		t.Fatalf("terminalDBOSCause replaced an already-typed error: %v", got)
	}
}

func TestTerminalDBOSCauseLeavesUnrelatedErrorsAlone(t *testing.T) {
	t.Parallel()
	plain := errors.New("connection reset")
	if got := terminalDBOSCause(plain, "wf"); got != plain {
		t.Fatalf("terminalDBOSCause rewrote an unrelated error: %v", got)
	}
}

// The exported gate lets a host refuse a superseded system database before it
// builds its own DBOS context, which is the only moment at which the refusal is
// still possible: the runtime migrates in place during construction.
func TestRequireSupportedDBOSSystemSchemaRefusesASupersededDatabase(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "dbos", "dbos_system_v020.db"))
	if err != nil {
		t.Fatalf("read immutable fixture: %v", err)
	}
	before := sha256.Sum256(data)
	path := filepath.Join(t.TempDir(), "superseded.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write private fixture copy: %v", err)
	}

	db := openRawSQLite(t, path)
	gateErr := RequireSupportedDBOSSystemSchema(t.Context(), db, path)
	if gateErr == nil {
		t.Fatal("RequireSupportedDBOSSystemSchema accepted a superseded system database")
	}
	if !errors.Is(gateErr, ErrSupersededDBOSSystemSchema) {
		t.Errorf("gate error does not match ErrSupersededDBOSSystemSchema: %v", gateErr)
	}
	if !strings.Contains(gateErr.Error(), path) {
		t.Errorf("gate error does not name the database: %v", gateErr)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read the refused database: %v", err)
	}
	afterDigest := sha256.Sum256(after)
	if hex.EncodeToString(before[:]) != hex.EncodeToString(afterDigest[:]) {
		t.Error("the refused database changed on disk; the gate must read only")
	}
}

func TestRequireSupportedDBOSSystemSchemaAcceptsAFreshDatabase(t *testing.T) {
	t.Parallel()
	db := openRawSQLite(t, filepath.Join(t.TempDir(), "fresh.db"))
	if err := RequireSupportedDBOSSystemSchema(t.Context(), db, "fresh.db"); err != nil {
		t.Fatalf("RequireSupportedDBOSSystemSchema rejected a fresh database: %v", err)
	}
}

// openRawSQLite opens a private test database on the same driver production
// uses. It returns the pool because the schema gate takes a *sql.DB.
func openRawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open private SQLite test database %q: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(t.Context()); err != nil {
		_ = db.Close()
		t.Fatalf("ping private SQLite test database %q: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
