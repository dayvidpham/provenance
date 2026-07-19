package sqlite

import (
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// This file persists the actor-namespace registry of
// docs/journal-relational-contract.md §7 and enforces the two reducer rules SQL
// alone cannot express (§7.3): range disjointness across claims and fixed-actor
// entry membership within a claimed range.

// RegisterNamespaceClaim registers one actor_namespace_claims row (§7.1),
// enforcing range disjointness (§7.3 rule 1). A re-registration that exactly
// matches the stored row is idempotent; a differing re-registration of the same
// namespace is an explicit conflict, never a silent overwrite. Returns
// journal.ErrNamespaceRange on an overlap (naming both namespaces) and
// journal.ErrNamespaceClaim on a non-matching re-registration.
func (db *DB) RegisterNamespaceClaim(claim journal.ActorNamespaceClaim) error {
	if err := claim.Validate(); err != nil {
		return err
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	existing, err := db.loadNamespaceClaimsLocked()
	if err != nil {
		return err
	}

	// Idempotency / conflict on the same namespace name.
	for _, ex := range existing {
		if ex.Namespace == claim.Namespace {
			if ex.Equal(claim) {
				return nil // idempotent no-op
			}
			return fmt.Errorf(
				"%w: namespace %q is already claimed with a different "+
					"claimant/range/codec — where: actor namespace registration; "+
					"when: before commit; impact: the differing re-registration is "+
					"rejected rather than silently overwriting the existing claim; "+
					"fix: register a distinct namespace or reconcile the existing "+
					"claim first",
				journal.ErrNamespaceClaim, claim.Namespace)
		}
	}

	// Range disjointness against every other namespace (§7.3 rule 1).
	if err := journal.CheckNoOverlap(claim, existing); err != nil {
		return err
	}

	var txErr error
	endTx := sqlitex.Transaction(db.conn)
	defer endTx(&txErr)
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO actor_namespace_claims (namespace, claimant_id, range_min, range_max, codec)
		 VALUES (?1, ?2, ?3, ?4, ?5)`,
		&sqlitex.ExecOptions{Args: []any{
			claim.Namespace, claim.ClaimantID, claim.Range.Min[:], claim.Range.Max[:], claim.Codec,
		}},
	); txErr != nil {
		return fmt.Errorf("RegisterNamespaceClaim %q: %w", claim.Namespace, txErr)
	}
	return nil
}

// RegisterFixedActorEntry registers one fixed_actor_manifest_entries row (§7.2),
// enforcing entry-in-range membership (§7.3 rule 2): the entry's ActorID must
// decode, under the namespace codec, to an ordinal inside the claimed range.
// fixedUUID is the entry's 16-byte fixed-UUID form (as produced by
// journal.OrdinalUUID). Returns journal.ErrEntryOutOfRange when the entry falls
// outside its claim.
func (db *DB) RegisterFixedActorEntry(entry journal.FixedActorEntry, fixedUUID [16]byte) error {
	if entry.Namespace == "" {
		return fmt.Errorf("%w: entry namespace is required", journal.ErrNamespaceClaim)
	}
	if entry.Name == "" {
		return fmt.Errorf("%w: entry name is required for namespace %q", journal.ErrNamespaceClaim, entry.Namespace)
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	claim, found, err := db.getNamespaceClaimLocked(entry.Namespace)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf(
			"%w: namespace %q has no registered claim — where: fixed-actor "+
				"registration; when: before commit; impact: the entry is rejected; "+
				"fix: register the namespace claim before adding entries",
			journal.ErrNamespaceClaim, entry.Namespace)
	}
	if err := journal.CheckEntryInRange(claim, entry.Namespace, fixedUUID); err != nil {
		return err
	}

	metadata := entry.Metadata
	if metadata == "" {
		metadata = "{}"
	}

	var txErr error
	endTx := sqlitex.Transaction(db.conn)
	defer endTx(&txErr)
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO fixed_actor_manifest_entries (actor_id, namespace, kind_id, name, metadata)
		 VALUES (?1, ?2, ?3, ?4, ?5)`,
		&sqlitex.ExecOptions{Args: []any{
			entry.ActorID.String(), entry.Namespace, int(entry.ActorKind), entry.Name, metadata,
		}},
	); txErr != nil {
		return fmt.Errorf("RegisterFixedActorEntry %q: %w", entry.ActorID.String(), txErr)
	}
	return nil
}

// NamespaceClaims returns every registered claim, namespace-ordered.
func (db *DB) NamespaceClaims() ([]journal.ActorNamespaceClaim, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.loadNamespaceClaimsLocked()
}

func (db *DB) loadNamespaceClaimsLocked() ([]journal.ActorNamespaceClaim, error) {
	var claims []journal.ActorNamespaceClaim
	if err := sqlitex.Execute(db.conn,
		`SELECT namespace, claimant_id, range_min, range_max, codec
		 FROM actor_namespace_claims ORDER BY namespace ASC`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			claim, err := scanNamespaceClaim(stmt)
			if err != nil {
				return err
			}
			claims = append(claims, claim)
			return nil
		}},
	); err != nil {
		return nil, fmt.Errorf("loadNamespaceClaims: %w", err)
	}
	return claims, nil
}

func (db *DB) getNamespaceClaimLocked(namespace string) (journal.ActorNamespaceClaim, bool, error) {
	var claim journal.ActorNamespaceClaim
	var found bool
	if err := sqlitex.Execute(db.conn,
		`SELECT namespace, claimant_id, range_min, range_max, codec
		 FROM actor_namespace_claims WHERE namespace = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{namespace},
			ResultFunc: func(stmt *zs.Stmt) error {
				c, err := scanNamespaceClaim(stmt)
				if err != nil {
					return err
				}
				claim = c
				found = true
				return nil
			},
		},
	); err != nil {
		return journal.ActorNamespaceClaim{}, false, fmt.Errorf("getNamespaceClaim %q: %w", namespace, err)
	}
	return claim, found, nil
}

func scanNamespaceClaim(stmt *zs.Stmt) (journal.ActorNamespaceClaim, error) {
	claim := journal.ActorNamespaceClaim{
		Namespace:  stmt.ColumnText(0),
		ClaimantID: stmt.ColumnText(1),
		Codec:      stmt.ColumnText(4),
	}
	min, err := readBlob16(stmt, 2)
	if err != nil {
		return journal.ActorNamespaceClaim{}, fmt.Errorf("namespace %q range_min: %w", claim.Namespace, err)
	}
	max, err := readBlob16(stmt, 3)
	if err != nil {
		return journal.ActorNamespaceClaim{}, fmt.Errorf("namespace %q range_max: %w", claim.Namespace, err)
	}
	claim.Range = journal.UUIDRange{Min: min, Max: max}
	return claim, nil
}

var errBadBlobWidth = errors.New("sqlite: stored range bound is not 16 bytes")

func readBlob16(stmt *zs.Stmt, col int) ([16]byte, error) {
	var out [16]byte
	n := stmt.ColumnLen(col)
	if n != 16 {
		return out, fmt.Errorf("%w: got %d bytes in column %d", errBadBlobWidth, n, col)
	}
	stmt.ColumnBytes(col, out[:])
	return out, nil
}
