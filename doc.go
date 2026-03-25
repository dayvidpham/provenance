// Package providence provides a task dependency tracker for multi-agent workflows.
//
// Providence replaces Beads (bd) as the task dependency tracker for the Aura Protocol agent system.
// It tracks work products, their dependencies, and their provenance across multi-agent planning
// and implementation workflows.
//
// The package exposes a Tracker interface with methods to create, retrieve, update, and delete tasks.
// It also supports edges (dependencies), comments, and labels on tasks.
//
// All entity IDs follow the format {Namespace}--{UUIDv7} for scoping and global uniqueness.
package providence
