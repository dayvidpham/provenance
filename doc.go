// Package provenance provides a task dependency tracker for multi-agent workflows.
//
// Provenance replaces Beads (bd) as the task dependency tracker for the Aura Protocol agent system.
// It tracks work products, their dependencies, and their provenance across multi-agent planning
// and implementation workflows.
//
// The package exposes a Tracker interface with methods to create, retrieve, update, and delete tasks.
// It also supports edges (dependencies), comments, and labels on tasks.
//
// All entity IDs follow the format {Namespace}--{UUIDv7} for scoping and global uniqueness.
//
// # DBOS durable-execution adapter (issue dayvidpham/provenance#6)
//
// NewDBOSAdapter runs a Provenance operation as a deterministic DBOS workflow whose
// single step is the atomic journal fold, binding the workflow/step guards and
// restart semantics to the SAME OperationID alternate key and journal contract (no
// parallel commit ledger). OpenBorrowedSQLite shares one physical SQLite database
// with a DBOS root.
//
// # Host obligations on the borrowed path
//
// A host that builds its own DBOS root (NewHostBoundGovernedAllocator,
// NewDBOSAdapter) owns two steps that Provenance cannot take for it:
//
//   - The binary must blank-import
//     "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite". The DBOS
//     runtime keeps no SQLite driver of its own and uses whichever driver that
//     import registers, even for a caller-supplied handle.
//   - RequireSupportedDBOSSystemSchema must be called on the exact *sql.DB
//     before the root is built, by either dbos.NewContext or dbos.NewClient
//     (which builds a context of its own). Both migrate a superseded system
//     database in place during construction, and this build supports no in-place
//     upgrade, so no later moment can refuse one.
//
// The factory-owned constructors (OpenBoundGovernedAllocator,
// OpenFusedGovernedAllocator) take both steps themselves.
//
// OpenBorrowedSQLite uses the exact caller-owned database/sql pool for both DBOS
// and Provenance. Provenance never closes that pool; a Ping liveness sentinel
// makes post-shutdown access return StoreUnavailableError. See dbos_store.go for
// the borrowed-store lifecycle details.
package provenance
