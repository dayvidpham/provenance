package sqlite

import (
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// derivations.go implements the derivation-qualifier persistence:
// the direct-write projection into derivation_qualifiers plus the qualifier reads.
// The who-provenance of the qualification is journaled separately by the Session
// verb (see provenance.Session.QualifyDerivation); this layer performs only the
// domain projection. A qualifier can attach only to an existing derived_from or
// supersedes edge — resolved here on the write path and additionally enforced by
// the table's composite FK + CHECK, so a qualifier on a non-derivation edge is
// impossible.

// QualifyDerivation attaches (or re-attaches) a typed derivation kind to the
// derivation relationship from source to target. The edge kind (derived_from vs
// supersedes) is resolved from the existing edge: if neither exists,
// ErrNoDerivationEdge is returned and nothing is written. Re-qualifying an
// existing (source, target) qualifier replaces its kind and activity (a qualifier
// is keyed by the relationship, single-valued per the vocabulary's DerivationShape).
// activity, when non-nil, records prov:hadActivity. Acquires the DB mutex.
func (db *DB) QualifyDerivation(source, target ptypes.TaskID, kind ptypes.DerivationKind, activity *ptypes.ActivityID) error {
	if !kind.IsValid() {
		return fmt.Errorf("sqlite.QualifyDerivation: invalid DerivationKind(%d)", int(kind))
	}
	now := time.Now().UTC()

	db.mu.Lock()
	defer db.mu.Unlock()

	// Resolve the derivation edge kind: prefer derived_from(1), else supersedes(2).
	// A missing edge fails BEFORE any write with the typed, actionable error.
	edgeKindID, ok, err := db.derivationEdgeKindLocked(source.String(), target.String())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf(
			"%w: QualifyDerivation source=%q target=%q — where: derivation-qualifier write path (§3.3); "+
				"when: before any row is written; impact: nothing committed; fix: AddEdge a derived_from or "+
				"supersedes edge from source to target first, then qualify it",
			ptypes.ErrNoDerivationEdge, source.String(), target.String())
	}

	if err := sqlitex.Execute(db.conn,
		"INSERT INTO derivation_qualifiers (source_id, target_id, edge_kind_id, derivation_kind_id, activity_id, created_at)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6)\n\t\t ON CONFLICT(source_id, target_id) DO UPDATE SET\n\t\t   edge_kind_id=excluded.edge_kind_id, derivation_kind_id=excluded.derivation_kind_id,\n\t\t   activity_id=excluded.activity_id, created_at=excluded.created_at",
		&sqlitex.ExecOptions{Args: []any{
			source.String(), target.String(), edgeKindID, int(kind), activityIDArg(activity), now.UnixNano(),
		}}); err != nil {
		return fmt.Errorf("sqlite.QualifyDerivation %q->%q: %w", source.String(), target.String(), err)
	}
	return nil
}

// HasDerivationEdge reports whether a derived_from or supersedes edge exists from
// source to target. Used by the Session verb as an authorization-time pre-check
// before journaling the who-provenance of a qualification. Acquires the DB mutex.
func (db *DB) HasDerivationEdge(source, target ptypes.TaskID) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, ok, err := db.derivationEdgeKindLocked(source.String(), target.String())
	return ok, err
}

// derivationEdgeKindLocked returns the edge kind id (derived_from preferred, else
// supersedes) of an existing derivation edge from source to target, or ok=false if
// neither exists. The edge kinds are bound parameters (never SQL literals — the
// package's static-SQL discipline). ORDER BY kind_id ASC with a first-row guard
// yields the preferred (lower) kind without a literal LIMIT. Assumes db.mu is held.
func (db *DB) derivationEdgeKindLocked(source, target string) (int, bool, error) {
	var kindID int
	found := false
	if err := sqlitex.Execute(db.conn,
		"SELECT kind_id FROM edges WHERE source_id=?1 AND target_id=?2 AND kind_id IN (?3, ?4) ORDER BY kind_id ASC",
		&sqlitex.ExecOptions{Args: []any{source, target, int(ptypes.EdgeDerivedFrom), int(ptypes.EdgeSupersedes)}, ResultFunc: func(stmt *zs.Stmt) error {
			if !found {
				kindID = stmt.ColumnInt(0)
				found = true
			}
			return nil
		}}); err != nil {
		return 0, false, fmt.Errorf("sqlite.derivationEdgeKind %q->%q: %w", source, target, err)
	}
	return kindID, found, nil
}

// activityIDArg renders a nullable ActivityID for a SQLite bind arg (nil → NULL).
func activityIDArg(id *ptypes.ActivityID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// GetDerivationQualifiers returns every derivation qualifier whose SOURCE is id,
// ordered deterministically by target. Acquires the DB mutex.
func (db *DB) GetDerivationQualifiers(id ptypes.TaskID) ([]ptypes.DerivationQualifier, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	var qualifiers []ptypes.DerivationQualifier
	err := sqlitex.Execute(db.conn,
		"SELECT source_id, target_id, derivation_kind_id, activity_id FROM derivation_qualifiers WHERE source_id=?1 ORDER BY target_id ASC",
		&sqlitex.ExecOptions{Args: []any{id.String()}, ResultFunc: func(stmt *zs.Stmt) error {
			q, serr := scanDerivationQualifier(stmt)
			if serr != nil {
				return serr
			}
			qualifiers = append(qualifiers, q)
			return nil
		}})
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetDerivationQualifiers %q: %w", id.String(), err)
	}
	return qualifiers, nil
}

// scanDerivationQualifier reads a (source_id, target_id, derivation_kind_id,
// activity_id) row. activity_id is nullable.
func scanDerivationQualifier(stmt *zs.Stmt) (ptypes.DerivationQualifier, error) {
	source, err := ptypes.ParseTaskID(stmt.ColumnText(0))
	if err != nil {
		return ptypes.DerivationQualifier{}, fmt.Errorf("scan derivation qualifier: invalid source_id %q: %w", stmt.ColumnText(0), err)
	}
	target, err := ptypes.ParseTaskID(stmt.ColumnText(1))
	if err != nil {
		return ptypes.DerivationQualifier{}, fmt.Errorf("scan derivation qualifier: invalid target_id %q: %w", stmt.ColumnText(1), err)
	}
	var activity *ptypes.ActivityID
	if !stmt.ColumnIsNull(3) {
		aid, aerr := ptypes.ParseActivityID(stmt.ColumnText(3))
		if aerr != nil {
			return ptypes.DerivationQualifier{}, fmt.Errorf("scan derivation qualifier: invalid activity_id %q: %w", stmt.ColumnText(3), aerr)
		}
		activity = &aid
	}
	return ptypes.DerivationQualifier{
		SourceID:   source,
		TargetID:   target,
		Kind:       ptypes.DerivationKind(stmt.ColumnInt(2)),
		ActivityID: activity,
	}, nil
}
