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
// ARCHITECTURE DEVIATION (Option B; architect ruling on aura-plugins-9y8gt, flagged
// for UAT ratification): issue #6 acceptance criterion #1 assumed Provenance
// persisted through database/sql, so the borrowed *sql.DB could be the single
// literal handle and migrations could run "through it". The delivered,
// UAT-accepted journal foundation persists on zombiezen.com/go/sqlite, which cannot
// be constructed from a database/sql *sql.DB. Rather than re-open every reviewed,
// accepted slice by porting the persistence layer, OpenBorrowedSQLite derives the
// borrowed handle's on-disk path and opens ONE internal zombiezen bridge connection
// on that SAME file: one shared physical database (the criterion's intent), a second
// connection but no second file/WAL/lifecycle. The borrowed *sql.DB stays the sole
// lifecycle token via a Ping liveness sentinel; migrations apply INTO the borrowed
// handle's database (the caller observes the schema through its own handle). See
// dbos_store.go for the full note.
package provenance
