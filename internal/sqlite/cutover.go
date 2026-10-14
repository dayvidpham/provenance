// cutover.go owns the two in-transaction primitives the offline identity-format
// cutover needs but activation's normal open path does not expose:
//
//   - EnsureSchemaInTx completes the store schema inside a transaction the
//     caller already owns (BEGIN IMMEDIATE), instead of relying on activation's
//     own post-open schema work. Without it a pre-operations store reaches the
//     conversion step with no governed_allocation_operations/journal_operations
//     relations and fails on a missing table.
//   - PauseForeignKeys brackets that transaction with connection-local
//     foreign-key enforcement OFF, because SQLite ignores PRAGMA foreign_keys
//     while a transaction is open and the schema rebuilds (the journal
//     produced-by FK completion) drop and recreate parent tables.
//
// Both keep the transaction and connection ownership with the caller: they
// begin nothing, commit nothing, roll back nothing, and never publish the
// identity-format marker.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// PauseForeignKeys disables connection-local foreign-key enforcement on the
// caller's pinned connection before it begins its transaction, and returns a
// restore that re-enables enforcement and proves it with a read-back.
//
// SQLite silently ignores PRAGMA foreign_keys while a transaction is open on the
// connection, so a caller that tries to disable enforcement *inside* its
// transaction writes a no-op and then rebuilds parent tables such as journal
// with enforcement still ON. The bracket must therefore wrap the transaction:
//
//	conn, err := db.Conn(ctx)          // one leased connection
//	restoreFK, err := PauseForeignKeys(ctx, conn)
//	if err != nil { return err }
//	tx, err := conn.BeginTx(ctx, nil)  // BEGIN IMMEDIATE
//	// ... EnsureSchemaInTx, conversion, verification ...
//	// commit or roll back, then:
//	if err := restoreFK(opErr); err != nil { return err }
//
// The restore takes the caller's own error and returns the error the caller
// should return, so a failed restore is never dropped on an already-failed path.
// Both the disable and the restore are verified by reading the pragma back; a
// connection whose enforcement cannot be proven restored is reported rather than
// silently handed back carrying the wrong pragma state.
//
// Usage requires a named error return:
//
//	restoreFK, err := PauseForeignKeys(ctx, conn)
//	if err != nil {
//		return err
//	}
//	defer func() { err = restoreFK(err) }()
func PauseForeignKeys(ctx context.Context, conn *sql.Conn) (func(error) error, error) {
	if ctx == nil || conn == nil {
		return nil, fmt.Errorf("PauseForeignKeys: nil context or connection; foreign-key enforcement unchanged; fix: pass the leased connection that will own the cutover transaction")
	}
	if err := setPragmaVerified(ctx, conn, foreignKeysOff); err != nil {
		return nil, fmt.Errorf(
			"PauseForeignKeys: disable foreign-key enforcement before the cutover transaction: %w; "+
				"where: provenance identity cutover, before BEGIN IMMEDIATE; impact: nothing ran and nothing was "+
				"written; fix: confirm no transaction is open on this connection, then retry", err)
	}
	return func(operationErr error) error {
		// The caller's context may already be canceled or expired; restoring this
		// connection's enforcement is not the caller's operation, so it is bounded
		// on its own.
		restoreCtx, cancel := context.WithTimeout(context.Background(), pragmaControlTimeout)
		defer cancel()
		if err := setPragmaVerified(restoreCtx, conn, foreignKeysOn); err != nil {
			return errors.Join(operationErr, fmt.Errorf(
				"PauseForeignKeys: restore foreign-key enforcement after the cutover transaction: %w; "+
					"where: provenance identity cutover, after COMMIT/ROLLBACK; impact: this connection still "+
					"reports foreign_keys OFF, so it must not be reused for enforced writes; fix: close and "+
					"reopen the connection before further work", err))
		}
		return operationErr
	}, nil
}

// EnsureSchemaInTx brings the caller's store schema up to this build's full
// shape inside the caller-owned transaction, mirroring activation's schema step
// without taking transaction or connection ownership of its own.
//
// It begins nothing and commits nothing. The intended offline cutover sequence
// is atomic on one connection:
//
//	conn, _ := db.Conn(ctx)
//	restoreFK, _ := PauseForeignKeys(ctx, conn)
//	tx, _ := conn.BeginTx(ctx, nil) // BEGIN IMMEDIATE
//	EnsureSchemaInTx(ctx, tx, models)
//	ConvertIDsInTx(ctx, tx, options)
//	VerifyIDConversionInTx(ctx, tx)
//	PublishIDFormatInTx(ctx, tx)     // marker written LAST
//	tx.Commit()
//	restoreFK(nil)
//
// Because the schema step can rebuild the journal table to complete its
// producing-operation foreign key, the caller MUST pause foreign-key
// enforcement on the connection before beginning the transaction (see
// PauseForeignKeys); an in-transaction PRAGMA foreign_keys=OFF is a no-op. On
// any error the caller rolls back and the store is unchanged.
//
// EnsureSchemaInTx does NOT publish the identity-format marker: that remains the
// conversion's last, verified step (PublishIDFormatInTx). It performs only
// idempotent DDL and reference-data seeding, so it is safe to run on a current
// store and does nothing there.
func EnsureSchemaInTx(ctx context.Context, tx *sql.Tx, models []ptypes.ModelEntry) error {
	if ctx == nil || tx == nil {
		return fmt.Errorf("EnsureSchemaInTx: nil context or transaction; no schema written; fix: BEGIN IMMEDIATE first and pass the active transaction")
	}
	scope := &connScope{ctx: ctx, conn: migrationConnection{tx}, projectionTarget: projectionTargetLive}
	if err := scope.ensureSchema(models); err != nil {
		return fmt.Errorf(
			"EnsureSchemaInTx: complete the store schema inside the caller's transaction: %w; "+
				"where: provenance identity cutover, after BEGIN IMMEDIATE and before conversion; impact: the "+
				"caller must roll back, which leaves the store unchanged; fix: run the cutover with foreign keys "+
				"paused and no other writer, then retry", err)
	}
	return nil
}
