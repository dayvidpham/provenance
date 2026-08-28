package provenance

// dbos_system_schema.go exposes the DBOS system-database preflight to hosts
// that create their own DBOS context. Provenance's factory-owned constructor
// (OpenFusedGovernedAllocator) runs the same gate itself.

import (
	"context"
	"database/sql"

	"github.com/dayvidpham/provenance/internal/dbossys"
)

// ErrSupersededDBOSSystemSchema marks a refusal of a DBOS system database that
// a superseded DBOS runtime created. Match it with errors.Is.
var ErrSupersededDBOSSystemSchema = dbossys.ErrSupersededSystemSchema

// RequireSupportedDBOSSystemSchema refuses a DBOS system database that a
// superseded DBOS runtime created. origin names the database for the operator,
// normally its path or DSN.
//
// There is no supported in-place upgrade of such a database. The DBOS runtime
// would migrate it silently while it builds its context, so a host that creates
// its own context must call this gate on the exact *sql.DB it is about to pass
// as dbos.Config.SQLiteSystemDB, BEFORE it creates that context. The gate only
// reads: on refusal the file is unchanged and nothing was opened or launched.
//
// dbos.NewClient is the same moment under another name: it builds a context of
// its own from ClientConfig.SQLiteSystemDB and therefore migrates in place too.
// Call this gate before that constructor as well.
//
// A database with no DBOS system schema is fresh and is accepted; the runtime
// creates the schema on its first launch.
func RequireSupportedDBOSSystemSchema(ctx context.Context, systemDB *sql.DB, origin string) error {
	return dbossys.RequireSupportedSchema(ctx, systemDB, origin)
}
