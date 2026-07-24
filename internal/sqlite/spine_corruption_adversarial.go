package sqlite

import (
	"context"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// spine_corruption_adversarial.go holds narrow write seams that deliberately
// corrupt the JOURNAL SPINE ITSELF — the supertype journal rows and their
// class-table-inheritance subtype rows — in ways a production writer never
// produces, so the negative-path corpus (testdata/contract/journal_spine_corruption.yaml)
// can drive the production fail-closed guards (VerifyIntegrity §10 rule 8,
// ReplayProjections §15 convergence) against real spine damage: a subtype row
// deleted out from under a surviving supertype row, a supertype discriminator
// rewritten to disagree with its subtype table, a truncated journal tail, and a
// non-contiguous journal_id gap punched into the middle of the spine.
//
// These differ from operations_adversarial.go's AppendBareJournalRow /
// AdversarialSubtypeMismatchingKind seams, which corrupt at INSERT time: the seams
// here mutate an ALREADY-COMMITTED, valid journaled history after the fact (the
// on-disk-corruption / partial-write shape), which is the class of damage §15's
// from-empty convergence and §10's whole-journal scan exist to catch. Production
// paths (Apply, migration) never delete or renumber committed rows; these seams are
// used only by the corpus and are never part of the JournalAPI surface.

// AdversarialDeleteSubtypeRow deletes the subtype row for a surviving supertype
// journal row (an "orphaned anchor" — the subtype row deleted out from under it),
// leaving the supertype row with zero subtype rows: a totality violation (§10 rule
// 8) a production writer never leaves behind, because it writes the supertype and
// subtype rows in one transaction and never deletes committed rows. Foreign-key
// enforcement is toggled off around the delete so a subtype row referenced by the
// spine can be removed; the database is left in the deliberately corrupt state the
// corpus then drives VerifyIntegrity against (expecting ErrSubtypeIntegrity). The
// table name comes from the closed corpus, never caller input.
type AdversarialSubtypeTable uint8

const (
	AdversarialSubtypeOperations AdversarialSubtypeTable = iota + 1
	AdversarialSubtypeTaskEvents
	AdversarialSubtypeAuthorities
	AdversarialSubtypeDecisions
	AdversarialSubtypeEvidence
)

func (table AdversarialSubtypeTable) deleteQuery() string {
	switch table {
	case AdversarialSubtypeOperations:
		return "DELETE FROM journal_operations WHERE journal_id=?1"
	case AdversarialSubtypeTaskEvents:
		return "DELETE FROM journal_task_events WHERE journal_id=?1"
	case AdversarialSubtypeAuthorities:
		return "DELETE FROM journal_authorities WHERE journal_id=?1"
	case AdversarialSubtypeDecisions:
		return "DELETE FROM journal_decisions WHERE journal_id=?1"
	case AdversarialSubtypeEvidence:
		return "DELETE FROM journal_evidence WHERE journal_id=?1"
	default:
		panic("unknown adversarial subtype table")
	}
}

func (db *DB) AdversarialDeleteSubtypeRow(jid journal.JournalID, table AdversarialSubtypeTable) error {
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialDeleteSubtypeRow: lease connection: %w", err)
	}
	defer scope.release()
	if err := sqlitex.ExecuteTransient(scope.conn, "PRAGMA foreign_keys=OFF", nil); err != nil {
		return fmt.Errorf("AdversarialDeleteSubtypeRow %q: disable FK: %w", table, err)
	}
	defer func() { _ = sqlitex.ExecuteTransient(scope.conn, "PRAGMA foreign_keys=ON", nil) }()
	// The table is one of the closed subtype-table constants above, never caller
	// input, so identifier interpolation is safe here.
	if err := sqlitex.Execute(scope.conn, table.deleteQuery(), &sqlitex.ExecOptions{Args: []any{int64(jid)}}); err != nil {
		return fmt.Errorf("AdversarialDeleteSubtypeRow %q journal_id=%d: %w", table, jid, err)
	}
	if changes := scope.conn.Changes(); changes != 1 {
		return fmt.Errorf("AdversarialDeleteSubtypeRow %q journal_id=%d: deleted %d rows, want exactly 1 (the seam must corrupt a real committed row)", table, jid, changes)
	}
	return nil
}

// AdversarialRewriteDiscriminator rewrites a surviving supertype journal row's
// kind_id to a DIFFERENT JournalKind, so the supertype discriminator no longer
// agrees with the subtype table the row actually carries (§10 rule 8 discriminator
// agreement). A production writer never rewrites a committed row's kind_id — the
// discriminator is fixed at insert. The corpus drives VerifyIntegrity against this
// (expecting ErrSubtypeIntegrity). newKind must differ from the row's current kind.
func (db *DB) AdversarialRewriteDiscriminator(jid journal.JournalID, newKind journal.JournalKind) error {
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialRewriteDiscriminator: lease connection: %w", err)
	}
	defer scope.release()
	var current int = -1
	if err := sqlitex.Execute(scope.conn, "SELECT kind_id FROM journal WHERE journal_id = ?1", &sqlitex.ExecOptions{Args: []any{int64(jid)}, ResultFunc: func(stmt *zs.Stmt) error { current = stmt.ColumnInt(0); return nil }}); err != nil {
		return fmt.Errorf("AdversarialRewriteDiscriminator journal_id=%d: read current kind: %w", jid, err)
	}
	if current == -1 {
		return fmt.Errorf("AdversarialRewriteDiscriminator journal_id=%d: no such journal row", jid)
	}
	if current == int(newKind) {
		return fmt.Errorf("AdversarialRewriteDiscriminator journal_id=%d: new kind %s equals the current kind, which is no corruption", jid, newKind)
	}
	if err := sqlitex.Execute(scope.conn, "UPDATE journal SET kind_id = ?1 WHERE journal_id = ?2", &sqlitex.ExecOptions{Args: []any{int(newKind), int64(jid)}}); err != nil {
		return fmt.Errorf("AdversarialRewriteDiscriminator journal_id=%d -> %s: %w", jid, newKind, err)
	}
	return nil
}

// AdversarialTruncateTail deletes the highest-JournalID n supertype journal rows
// and every subtype/detail row that hangs off them (a truncated journal tail — the
// shape a partial write, a restore from a stale backup, or a byte-level file
// truncation leaves), so the stored incremental projection reflects rows the
// journal no longer contains. §15's from-empty convergence re-derives the
// projection over the truncated spine and fails closed with a
// ProjectionDivergenceError when a surviving anchored task's stored owner / status
// / watermark no longer equals the re-derivation. Foreign-key enforcement is
// toggled off so the anchor rows can be removed; the database is left in the
// deliberately truncated state the corpus drives ReplayProjections against. n must
// be positive and smaller than the journal length.
func (db *DB) AdversarialTruncateTail(n int) error {
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialTruncateTail: lease connection: %w", err)
	}
	defer scope.release()
	if n <= 0 {
		return fmt.Errorf("AdversarialTruncateTail: n must be positive, got %d", n)
	}
	var total int
	if err := sqlitex.Execute(scope.conn, "SELECT COUNT(*) FROM journal", &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error { total = stmt.ColumnInt(0); return nil }}); err != nil {
		return fmt.Errorf("AdversarialTruncateTail: count journal: %w", err)
	}
	if n >= total {
		return fmt.Errorf("AdversarialTruncateTail: n=%d must be smaller than the journal length %d (truncating the whole spine is a different case)", n, total)
	}
	if err := sqlitex.ExecuteTransient(scope.conn, "PRAGMA foreign_keys=OFF", nil); err != nil {
		return fmt.Errorf("AdversarialTruncateTail: disable FK: %w", err)
	}
	defer func() { _ = sqlitex.ExecuteTransient(scope.conn, "PRAGMA foreign_keys=ON", nil) }()
	// The n highest JournalIDs are the tail. Delete their subtype/detail rows first,
	// then the supertype rows, so no dangling subtype row is left behind (the tail is
	// removed cleanly — only the projection, not the spine's own integrity, diverges).
	var tail []int64
	if err := sqlitex.Execute(scope.conn, "SELECT journal_id FROM journal ORDER BY journal_id DESC LIMIT ?1", &sqlitex.ExecOptions{Args: []any{n}, ResultFunc: func(stmt *zs.Stmt) error {
		tail = append(tail, stmt.ColumnInt64(0))
		return nil
	}}); err != nil {
		return fmt.Errorf("AdversarialTruncateTail: enumerate tail: %w", err)
	}
	for _, jid := range tail {
		if err := scope.deleteSpineRowCascadeLocked(jid); err != nil {
			return fmt.Errorf("AdversarialTruncateTail: delete tail row %d: %w", jid, err)
		}
	}
	return nil
}

// AdversarialInsertNonContiguousSupertype inserts one BARE supertype journal row
// at a NON-CONTIGUOUS JournalID — the current maximum plus gap — with a stored
// actor but NO subtype row, the shape a corrupt out-of-band write (a foreign tool
// appending to the spine, a botched restore that skips subtype tables) leaves.
// AUTOINCREMENT permits a gap-stable ascending order, so the non-contiguous id
// itself is legal; the corruption is that the supertype row has zero subtype rows,
// a totality violation (§10 rule 8) VerifyIntegrity fails closed on with
// ErrSubtypeIntegrity, naming the offending JournalID. The kind is fixed to
// decision (a kind with no producer CHECK, so a bare row is insertable) and never
// caller-supplied. gap must be positive so the row lands beyond the current tail.
// Returns the non-contiguous JournalID it wrote.
func (db *DB) AdversarialInsertNonContiguousSupertype(actor journal.ActorID, gap int) (journal.JournalID, error) {
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("AdversarialInsertNonContiguousSupertype: lease connection: %w", err)
	}
	defer scope.release()
	if gap <= 0 {
		return 0, fmt.Errorf("AdversarialInsertNonContiguousSupertype: gap must be positive, got %d", gap)
	}
	var maxJID int64
	if err := sqlitex.Execute(scope.conn, "SELECT COALESCE(MAX(journal_id), ?1) FROM journal",
		&sqlitex.ExecOptions{Args: []any{0}, ResultFunc: func(stmt *zs.Stmt) error { maxJID = stmt.ColumnInt64(0); return nil }}); err != nil {
		return 0, fmt.Errorf("AdversarialInsertNonContiguousSupertype: read max journal_id: %w", err)
	}
	target := maxJID + int64(gap)
	if err := sqlitex.Execute(scope.conn, "INSERT INTO journal (journal_id, kind_id, actor_id, recorded_at, produced_by_operation_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5)", &sqlitex.ExecOptions{Args: []any{target, int(journal.JournalKindDecision), actor.String(), 0, nil}}); err != nil {
		return 0, fmt.Errorf("AdversarialInsertNonContiguousSupertype journal_id=%d: %w", target, err)
	}
	return journal.JournalID(target), nil
}

// deleteSpineRowCascadeLocked removes one supertype journal row and every
// subtype/detail row that hangs off it, so a truncate/gap seam leaves no dangling
// subtype row (which would instead trip the totality guard). It runs with FK
// enforcement already disabled by the caller. It is deliberately exhaustive over the
// class-table-inheritance and authority-detail tables the spine uses.
func (db *connScope) deleteSpineRowCascadeLocked(jid int64) error {
	// Detail tables that reference the subtype rows (deepest first).
	details := []string{
		"DELETE FROM journal_operation_result_slots WHERE journal_id = ?1",
		"DELETE FROM journal_authority_bootstraps WHERE journal_id = ?1",
		"DELETE FROM journal_authority_assignment_transitions WHERE journal_id = ?1",
		"DELETE FROM journal_authorities WHERE journal_id = ?1",
		"DELETE FROM journal_task_event_contexts WHERE event_journal_id = ?1",
		"DELETE FROM journal_task_events WHERE journal_id = ?1",
		"DELETE FROM journal_operations WHERE journal_id = ?1",
		"DELETE FROM journal_decisions WHERE journal_id = ?1",
		"DELETE FROM journal_evidence WHERE journal_id = ?1",
	}
	for _, stmt := range details {
		if err := sqlitex.Execute(db.conn, stmt, &sqlitex.ExecOptions{Args: []any{jid}}); err != nil {
			return fmt.Errorf("cascade static subtype statement: %w", err)
		}
	}
	if err := sqlitex.Execute(db.conn, "DELETE FROM journal WHERE journal_id = ?1", &sqlitex.ExecOptions{Args: []any{jid}}); err != nil {
		return fmt.Errorf("delete supertype row %d: %w", jid, err)
	}
	return nil
}

// AdversarialJournalRows returns every committed (journal_id, kind) pair in
// ascending JournalID order, so a corpus handler can pick a concrete interior or
// tail row to corrupt without hardcoding an id. It writes nothing.
func (db *DB) AdversarialJournalRows() ([]journal.JournalID, []journal.JournalKind, error) {
	scope, err := db.bindJournalScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, nil, fmt.Errorf("AdversarialJournalRows: lease connection: %w", err)
	}
	defer scope.release()
	var ids []journal.JournalID
	var kinds []journal.JournalKind
	if err := sqlitex.Execute(scope.conn, "SELECT journal_id, kind_id FROM journal ORDER BY journal_id ASC", &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
		ids = append(ids, journal.JournalID(stmt.ColumnInt64(0)))
		kinds = append(kinds, journal.JournalKind(stmt.ColumnInt(1)))
		return nil
	}}); err != nil {
		return nil, nil, fmt.Errorf("AdversarialJournalRows: %w", err)
	}
	return ids, kinds, nil
}

// knownSubtypeTable reports whether name is one of the closed class-table-inheritance
// subtype tables, so a corruption seam stays a closed set rather than accepting an
// arbitrary caller-supplied identifier for interpolation.
func knownSubtypeTable(name string) bool {
	switch name {
	case "journal_operations", "journal_task_events", "journal_authorities",
		"journal_decisions", "journal_evidence":
		return true
	default:
		return false
	}
}
