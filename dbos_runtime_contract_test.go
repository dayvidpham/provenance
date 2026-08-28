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
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/dayvidpham/provenance/internal/dbosfixture"
	"github.com/dayvidpham/provenance/internal/dbossys"
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

// The reconstruction accepts two independent clauses. The retry-exhaustion
// sentence alone satisfies one of them, so a wrong marker stays invisible unless
// a case exercises the marker branch by itself. This is that case: the text
// carries the runtime's code marker and none of the sentence.
func TestTerminalDBOSCauseReconstructsFromTheCodeMarkerAlone(t *testing.T) {
	t.Parallel()
	// The text comes from the runtime's own formatting, never from the marker
	// under test: a text built out of that marker would follow it into any wrong
	// value and prove nothing.
	typed := &dbos.Error{Code: dbos.ErrorCodeMaxStepRetriesExceeded, Message: "the step gave up"}
	markerOnly := typed.Error()
	if strings.Contains(markerOnly, "exceeded its maximum") {
		t.Fatalf("the marker-only text %q also matches the sentence clause: it proves nothing about the marker", markerOnly)
	}
	cause := terminalDBOSCause(errors.New(markerOnly), "provenance.apply/marker-only")
	var reconstructed *dbos.Error
	if !errors.As(cause, &reconstructed) {
		t.Fatalf("terminalDBOSCause did not reconstruct a typed runtime error from the code marker alone: %q", markerOnly)
	}
	if reconstructed.Code != dbos.ErrorCodeMaxStepRetriesExceeded {
		t.Errorf("reconstructed code = %s, want %s", reconstructed.Code, dbos.ErrorCodeMaxStepRetriesExceeded)
	}
	if reconstructed.WorkflowID != "provenance.apply/marker-only" {
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
	path, before := dbosfixture.PrivateDBOSSystemV020Copy(t, testdataDBOSDir)
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

	if after := fileDigest(t, path); after != before {
		t.Errorf("the refused database changed on disk: digest %s want %s; the gate must read only", after, before)
	}
}

// testdataDBOSDir is the root package's relative path to the immutable DBOS
// fixtures.
var testdataDBOSDir = filepath.Join("testdata", "dbos")

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// The borrowed path has a real hazard: a host that builds its own DBOS context
// over a superseded system database gets it migrated in place, silently, while
// that context is constructed. BindSystem receives the finished root and can no
// longer refuse. Prove both halves on private copies of the same fixture: the
// gate refuses and writes nothing, and the ungated runtime migrates the file to
// the supported runtime's own end state.
func TestTheBorrowedPathIsSafeOnlyWhenTheHostCallsTheGateFirst(t *testing.T) {
	t.Parallel()

	// Gated: the host calls the gate on the exact handle before it builds a
	// context, so no context is ever created and the file is untouched.
	gatedPath, gatedDigest := dbosfixture.PrivateDBOSSystemV020Copy(t, testdataDBOSDir)
	gatedDB := openRawSQLite(t, gatedPath)
	if err := RequireSupportedDBOSSystemSchema(t.Context(), gatedDB, gatedPath); err == nil {
		t.Fatal("the gate accepted a superseded system database on the borrowed path")
	} else if !errors.Is(err, ErrSupersededDBOSSystemSchema) {
		t.Errorf("gate error does not match ErrSupersededDBOSSystemSchema: %v", err)
	}
	if after := fileDigest(t, gatedPath); after != gatedDigest {
		t.Errorf("the gated copy changed on disk: digest %s want %s", after, gatedDigest)
	}
	state, version, err := dbossys.InspectSchema(t.Context(), gatedDB)
	if err != nil {
		t.Fatalf("inspect the gated copy: %v", err)
	}
	if state != dbossys.SchemaStateSuperseded || version != dbosfixture.SupersededSystemSchemaVersion {
		t.Errorf("gated copy state=%s version=%d, want %s at version %d: the refusal migrated something",
			state, version, dbossys.SchemaStateSuperseded, dbosfixture.SupersededSystemSchemaVersion)
	}

	// Ungated: the same file, handed straight to the runtime, is migrated in
	// place. This is the loss the gate prevents, so it is asserted, not assumed.
	ungatedPath, ungatedDigest := dbosfixture.PrivateDBOSSystemV020Copy(t, testdataDBOSDir)
	ungatedDB := openRawSQLite(t, ungatedPath)
	root, err := dbos.NewContext(t.Context(), dbos.Config{
		AppName:            "provenance-borrowed-path-hazard",
		SQLiteSystemDB:     ungatedDB,
		ApplicationVersion: "borrowed-path-hazard",
	})
	if err != nil {
		t.Fatalf("the runtime refused the superseded database on its own: %v", err)
	}
	// The runtime owns closing the pool it was given, so read the migrated file
	// back through a fresh handle.
	shutdownDBOSRoot(t, root, 30*time.Second)
	state, version, err = dbossys.InspectSchema(t.Context(), openRawSQLite(t, ungatedPath))
	if err != nil {
		t.Fatalf("inspect the ungated copy: %v", err)
	}
	if state != dbossys.SchemaStateSupported || version != dbosfixture.SupportedSystemSchemaVersion {
		t.Fatalf("ungated copy state=%s version=%d, want %s at version %d: the in-place migration this gate exists to prevent did not happen as documented",
			state, version, dbossys.SchemaStateSupported, dbosfixture.SupportedSystemSchemaVersion)
	}
	if after := fileDigest(t, ungatedPath); after == ungatedDigest {
		t.Error("the ungated copy's bytes are unchanged, so nothing was migrated in place")
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
