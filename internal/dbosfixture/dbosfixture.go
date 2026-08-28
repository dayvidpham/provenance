// Package dbosfixture owns the immutable DBOS system-database fixtures and the
// digests that pin them.
//
// It is a leaf on the standard library alone, deliberately. internal/testutil
// would be the natural home, but that package reaches the DBOS runtime and its
// SQLite driver through internal/sqlite, and internal/dbossys must observe the
// real missing-driver failure in a test binary that links no such driver.
package dbosfixture

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// DBOSSystemV020Fixture is the immutable database written by the superseded DBOS
// runtime (v0.20.0). It records system schema version 41, which is the exact
// durable shape the clean-cut policy refuses.
const DBOSSystemV020Fixture = "dbos_system_v020.db"

// SupersededSystemSchemaVersion is the last SQLite migration the superseded DBOS
// runtime applies, and therefore the version DBOSSystemV020Fixture records.
const SupersededSystemSchemaVersion int64 = 41

// SupportedSystemSchemaVersion is the last SQLite migration the supported DBOS
// runtime applies. A database this build created, or migrated in place, ends
// here. Source: dbos/internal/sysdb/migrations/sqlite,
// 107_create_application_versions_unclaimed_index.sql.
const SupportedSystemSchemaVersion int64 = 107

// DBOSSystemV020SHA256 pins the committed fixture's bytes. Every private copy is
// made only after the source matches it, so a test can never prove a refusal
// against a file that quietly changed.
//
// testdata/dbos/gen_dbos_system_v020.go reproduces the fixture's shape; the
// rebuilt file is not byte-identical, so replacing the fixture means updating
// this constant in the same change. See TESTING.md, "The superseded DBOS
// system-database fixture".
const DBOSSystemV020SHA256 = "dfb8213c95c0abae0a297df1e8806cfb3ca6cf977e7454f0d01ad969b3e82cae"

// PrivateDBOSSystemV020Copy verifies the pinned digest of the immutable fixture
// and only then writes a private mutable copy into the test's own temporary
// directory. It returns that copy's path and the pinned digest, so a caller can
// re-check the copy's bytes after a refusal.
//
// testdataDBOSDir is the caller's relative path to testdata/dbos.
func PrivateDBOSSystemV020Copy(t *testing.T, testdataDBOSDir string) (string, string) {
	t.Helper()
	source := filepath.Join(testdataDBOSDir, DBOSSystemV020Fixture)
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read the immutable fixture %s: %v", source, err)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if digest != DBOSSystemV020SHA256 {
		t.Fatalf("the immutable fixture %s has digest %s, want the pinned %s: "+
			"the fixture changed, so every refusal test below would prove the wrong thing; "+
			"regenerate it with testdata/dbos/gen_dbos_system_v020.go and update DBOSSystemV020SHA256",
			source, digest, DBOSSystemV020SHA256)
	}
	target := filepath.Join(t.TempDir(), DBOSSystemV020Fixture)
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatalf("write the private fixture copy %s: %v", target, err)
	}
	return target, digest
}
