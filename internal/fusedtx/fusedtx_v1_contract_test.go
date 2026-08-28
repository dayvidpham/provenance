package fusedtx_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/dbossys"
	"github.com/dayvidpham/provenance/internal/fusedtx"
)

// The DBOS runtime refuses to open a SQLite system database unless the binary
// blank-imports its SQLite driver package, and it degrades busy/locked
// classification without it. The import is therefore a production requirement,
// not a convenience, so this guard reads the production source directly.
func TestOpenSystemSourceLinksTheDBOSSQLiteDriver(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("fusedtx.go")
	if err != nil {
		t.Fatalf("read production source: %v", err)
	}
	const blankImport = `_ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite"`
	if !strings.Contains(string(source), blankImport) {
		t.Fatalf("fusedtx.go does not contain %s: every binary that opens a DBOS SQLite system database must link that driver", blankImport)
	}
}

// copyImmutableFixture writes a testdata database's bytes to a private mutable
// path and returns the path and the source digest.
func copyImmutableFixture(t *testing.T, name string) (string, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "dbos", name))
	if err != nil {
		t.Fatalf("read immutable fixture %s: %v", name, err)
	}
	target := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatalf("write private fixture copy: %v", err)
	}
	sum := sha256.Sum256(data)
	return target, hex.EncodeToString(sum[:])
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// The supported DBOS runtime would silently migrate a superseded system
// database in place. Provenance supports no such upgrade, so OpenSystem must
// refuse before it creates any DBOS context, and must leave the file untouched.
func TestOpenSystemRefusesSupersededSystemDatabase(t *testing.T) {
	t.Parallel()
	path, wantDigest := copyImmutableFixture(t, "dbos_system_v020.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&mode=rw"

	system, err := fusedtx.OpenSystem(context.Background(), fusedtx.SystemConfig{
		SQLiteDSN:          dsn,
		AppName:            "provenance-superseded-schema-test",
		ApplicationVersion: "superseded-test",
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil {
		if system != nil {
			_ = system.Close(30 * time.Second)
		}
		t.Fatal("OpenSystem accepted a superseded system database; it must refuse and migrate nothing")
	}
	if system != nil {
		t.Error("OpenSystem returned a system alongside its refusal")
	}
	if !errors.Is(err, dbossys.ErrSupersededSystemSchema) {
		t.Errorf("refusal does not match ErrSupersededSystemSchema: %v", err)
	}
	if !strings.Contains(err.Error(), "fusedtx.OpenSystem") {
		t.Errorf("refusal does not name the failing operation: %v", err)
	}
	if got := fileDigest(t, path); got != wantDigest {
		t.Errorf("the refused database changed on disk: digest %s want %s", got, wantDigest)
	}
	for _, sibling := range []string{path + "-wal", path + "-shm"} {
		if _, statErr := os.Stat(sibling); statErr == nil {
			t.Errorf("the refusal created %s; nothing must be written to a refused database", sibling)
		}
	}
}

// A fresh database must open, launch, and reach the supported schema floor.
func TestOpenSystemCreatesSupportedSystemSchema(t *testing.T) {
	t.Parallel()
	system, _ := newSystem(t)
	launch(t, system.Root())

	state, version, err := dbossys.InspectSchema(context.Background(), system.DB())
	if err != nil {
		t.Fatalf("InspectSchema after launch: %v", err)
	}
	if state != dbossys.SchemaStateSupported {
		t.Errorf("state=%s want %s", state, dbossys.SchemaStateSupported)
	}
	if version < dbossys.FirstSupportedMigrationVersion {
		t.Errorf("version=%d want at least the supported floor %d", version, dbossys.FirstSupportedMigrationVersion)
	}
}

// Shutdown reports a timeout that left resources running. Close must surface
// that outcome so a caller never treats an incomplete shutdown as complete.
func TestSystemCloseReportsShutdownOutcome(t *testing.T) {
	t.Parallel()
	dsn := systemDSN(t)
	system, err := fusedtx.OpenSystem(context.Background(), fusedtx.SystemConfig{
		SQLiteDSN:          dsn,
		AppName:            "provenance-close-outcome-test",
		ApplicationVersion: "close-outcome-test",
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("open owned fused system: %v", err)
	}
	launch(t, system.Root())
	if closeErr := system.Close(30 * time.Second); closeErr != nil {
		t.Fatalf("Close on a healthy system: %v", closeErr)
	}
	// Close is idempotent and reports the same outcome on every call.
	if closeErr := system.Close(30 * time.Second); closeErr != nil {
		t.Fatalf("second Close on a healthy system: %v", closeErr)
	}
}
