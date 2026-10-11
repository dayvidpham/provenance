package provenance_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	provenance "github.com/dayvidpham/provenance"
	_ "modernc.org/sqlite"
)

func TestIDFormatGuardBeforeDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retired.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE tasks(id TEXT PRIMARY KEY); INSERT INTO tasks VALUES('project--01968a3c-9d4f-7c8a-bc12-feedfacecafe')`); err != nil {
		t.Fatal(err)
	}
	inspection, err := provenance.InspectIDFormat(context.Background(), db)
	if err != nil || inspection.Format != provenance.IDFormatRetired || !inspection.Populated {
		t.Fatalf("inspection = %+v, %v", inspection, err)
	}
	tracker, err := provenance.OpenSQLite(path)
	if tracker != nil {
		tracker.Close()
		t.Fatal("retired store opened")
	}
	if !errors.Is(err, provenance.ErrIDFormat) {
		t.Fatalf("must refuse before malformed schema decode: %v", err)
	}
	for _, token := range []string{path, "stop all writers", "pasture migrate ids", "backup", "before"} {
		if !strings.Contains(err.Error(), token) {
			t.Errorf("error missing %q: %v", token, err)
		}
	}
	var marker bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='provenance_id_format')").Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker {
		t.Fatal("failed open published marker")
	}
}

func TestFreshIDFormatAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	tracker, err := provenance.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	inspection, err := provenance.InspectIDFormat(context.Background(), db)
	if err != nil || inspection.Format != provenance.IDFormatSlash || inspection.Populated {
		t.Fatalf("fresh format = %+v, %v", inspection, err)
	}
	tracker, err = provenance.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBorrowedIDFormatGuardLeavesArchivedBytesUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retired.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE tasks(id TEXT PRIMARY KEY); INSERT INTO tasks VALUES('project--01968a3c-9d4f-7c8a-bc12-feedfacecafe')`); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := provenance.OpenBorrowedSQLiteContext(context.Background(), db)
	if tracker != nil {
		tracker.Close()
		t.Fatal("retired borrowed store opened")
	}
	if !errors.Is(err, provenance.ErrIDFormat) || !strings.Contains(err.Error(), path) {
		t.Fatalf("borrowed guard lost category or actual path: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("retired borrowed open changed archived database bytes")
	}
}

func TestUnknownIDFormatFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE provenance_id_format(singleton INTEGER PRIMARY KEY,format TEXT); INSERT INTO provenance_id_format VALUES(1,'unknown')`); err != nil {
		t.Fatal(err)
	}
	if _, err := provenance.InspectIDFormat(context.Background(), db); !errors.Is(err, provenance.ErrIDFormat) {
		t.Fatalf("unknown marker accepted: %v", err)
	}
	tracker, err := provenance.OpenSQLite(path)
	if tracker != nil {
		tracker.Close()
		t.Fatal("unknown marker opened")
	}
	if !errors.Is(err, provenance.ErrIDFormat) {
		t.Fatalf("unknown marker open category: %v", err)
	}
}

func TestEmptyExistingIDFormatMarkerFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-marker.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE provenance_id_format(singleton INTEGER PRIMARY KEY,format TEXT); INSERT INTO provenance_id_format VALUES(1,'')`); err != nil {
		t.Fatal(err)
	}
	if _, err := provenance.InspectIDFormat(context.Background(), db); !errors.Is(err, provenance.ErrIDFormat) {
		t.Fatalf("empty marker escaped unknown-format refusal: %v", err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := provenance.PublishIDFormatInTx(context.Background(), tx); !errors.Is(err, provenance.ErrIDFormat) {
		t.Fatalf("empty conflicting marker reported successful publication: %v", err)
	}
}

func TestAuthenticPreviousWriterCorpusRefusedBeforeDecode(t *testing.T) {
	archive, err := os.ReadFile("testdata/id-format/previous-writer.db")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(archive)); got != "e2ec1b5721624ab80aa9d04f7283d5ae3fc4d403ace9fab49616056cb0e27fc0" {
		t.Fatalf("previous production-writer archive checksum changed: %s", got)
	}
	path := filepath.Join(t.TempDir(), "previous-writer.db")
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	tracker, err := provenance.OpenSQLite(path)
	if tracker != nil {
		tracker.Close()
		t.Fatal("authentic retired store opened")
	}
	if !errors.Is(err, provenance.ErrIDFormat) {
		t.Fatalf("authentic old store guard: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archive, after) {
		t.Fatal("failed new opener rewrote authentic archived history")
	}
}
