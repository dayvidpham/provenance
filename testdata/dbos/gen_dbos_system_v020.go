//go:build ignore

// Command gen_dbos_system_v020 reproduces testdata/dbos/dbos_system_v020.db:
// a real DBOS system database written by the superseded runtime
// (github.com/dbos-inc/dbos-transact-golang v0.20.0), whose recorded system
// schema version is 41.
//
// It CANNOT run inside this module: this module pins the supported runtime
// (v1.2.0), and Go builds one version per module graph. Run it from a scratch
// module pinned to the superseded runtime; TESTING.md, "The superseded DBOS
// system-database fixture", gives the exact commands.
//
// The rebuilt file is not byte-identical to the committed fixture. It carries a
// fresh executor identity and its own SQLite page layout. What reproduces is the
// shape the clean-cut policy refuses: dbos_migrations.version == 41. If you
// replace the committed fixture, update the pinned digest in
// internal/dbosfixture/dbosfixture.go in the same change.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	_ "modernc.org/sqlite"
)

// supersededSchemaVersion is the last SQLite migration the superseded runtime
// applies. The fixture exists to record exactly this value.
const supersededSchemaVersion = 41

func main() {
	out := flag.String("out", "dbos_system_v020.db", "path of the fixture database to write")
	flag.Parse()

	for _, path := range []string{*out, *out + "-wal", *out + "-shm"} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Fatalf("remove a stale %s: %v", path, err)
		}
	}

	db, err := sql.Open("sqlite", "file:"+*out+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		log.Fatalf("open the fixture database: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		log.Fatalf("ping the fixture database: %v", err)
	}

	root, err := dbos.NewDBOSContext(context.Background(), dbos.Config{
		AppName:        "provenance-superseded-fixture",
		SqliteSystemDB: db,
	})
	if err != nil {
		log.Fatalf("create the superseded DBOS context (this must run against v0.20.0): %v", err)
	}
	if err := dbos.Launch(root); err != nil {
		log.Fatalf("launch the superseded DBOS context: %v", err)
	}
	dbos.Shutdown(root, 30*time.Second)

	var version int64
	if err := db.QueryRow(`SELECT version FROM dbos_migrations LIMIT 1`).Scan(&version); err != nil {
		log.Fatalf("read the recorded system schema version: %v", err)
	}
	if version != supersededSchemaVersion {
		log.Fatalf("recorded system schema version = %d, want %d: this build is not the superseded runtime",
			version, supersededSchemaVersion)
	}

	// Fold the WAL back into the main file and prove the sidecars are gone, so
	// the committed fixture is one self-contained immutable file.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Fatalf("checkpoint the write-ahead log: %v", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode(DELETE)`); err != nil {
		log.Fatalf("leave write-ahead logging: %v", err)
	}
	if err := db.Close(); err != nil {
		log.Fatalf("close the fixture database: %v", err)
	}
	for _, sidecar := range []string{*out + "-wal", *out + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			log.Fatalf("%s still exists: the fixture is not self-contained", sidecar)
		}
	}
	fmt.Printf("wrote %s at system schema version %d\n", *out, version)
}
