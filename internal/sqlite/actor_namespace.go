package sqlite

import (
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// RegisterFixedSoftwareAgent atomically installs the namespace claim, software
// agent rows, and fixed-actor manifest row. It is convergent: exact rows are an
// inert success and absent rows under an exact claim are repaired. Any drift,
// including an actor that predates its claim, fails before the first write.
func (db *DB) RegisterFixedSoftwareAgent(reg journal.FixedSoftwareAgentRegistration) (agent ptypes.SoftwareAgent, err error) {
	if err := reg.Validate(); err != nil {
		return ptypes.SoftwareAgent{}, fmt.Errorf("RegisterFixedSoftwareAgent validation: %w", err)
	}
	metadata := reg.Entry.Metadata
	if metadata == "" {
		metadata = "{}"
	}
	id := ptypes.AgentID(reg.Entry.ActorID)
	agent = ptypes.SoftwareAgent{
		Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindSoftware},
		Name:  reg.AgentName, Version: reg.Version, Source: reg.Source,
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	if err = sqlitex.ExecuteTransient(db.conn, `BEGIN IMMEDIATE`, nil); err != nil {
		return ptypes.SoftwareAgent{}, fmt.Errorf("RegisterFixedSoftwareAgent begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = sqlitex.ExecuteTransient(db.conn, `ROLLBACK`, nil)
			return
		}
		if commitErr := sqlitex.ExecuteTransient(db.conn, `COMMIT`, nil); commitErr != nil {
			_ = sqlitex.ExecuteTransient(db.conn, `ROLLBACK`, nil)
			err = fmt.Errorf("RegisterFixedSoftwareAgent commit: %w", commitErr)
			agent = ptypes.SoftwareAgent{}
		}
	}()

	claims, err := db.loadNamespaceClaimsLocked()
	if err != nil {
		return ptypes.SoftwareAgent{}, err
	}
	claimFound := false
	for _, existing := range claims {
		if existing.Namespace != reg.Claim.Namespace {
			continue
		}
		claimFound = true
		if !existing.Equal(reg.Claim) {
			return ptypes.SoftwareAgent{}, fmt.Errorf(
				"%w: namespace %q is already claimed differently; where: fixed software agent activation; when: preflight; impact: no rows changed; fix: use the exact stored claim or a distinct namespace",
				journal.ErrNamespaceClaim, reg.Claim.Namespace)
		}
	}
	if err := journal.CheckNoOverlap(reg.Claim, claims); err != nil {
		return ptypes.SoftwareAgent{}, err
	}

	storedAgent, agentFound, err := db.fixedSoftwareAgentLocked(id)
	if err != nil {
		return ptypes.SoftwareAgent{}, err
	}
	if !claimFound && agentFound {
		return ptypes.SoftwareAgent{}, fmt.Errorf(
			"%w: actor %q exists before namespace %q is claimed; where: fixed software agent activation; when: preflight; impact: no claim is attached to ambiguous prior state; fix: reconcile or remove the pre-existing actor before activation",
			ptypes.ErrAgentAlreadyExists, id.String(), reg.Claim.Namespace)
	}
	if agentFound && storedAgent != agent {
		return ptypes.SoftwareAgent{}, fmt.Errorf(
			"%w: actor %q differs from the requested software identity; where: fixed software agent activation; when: preflight; impact: no rows changed; fix: retry with the exact stored name/version/source or choose a new actor ID",
			ptypes.ErrAgentAlreadyExists, id.String())
	}

	entryFound, err := db.fixedActorEntryExactLocked(reg.Entry, metadata)
	if err != nil {
		return ptypes.SoftwareAgent{}, err
	}

	if !claimFound {
		if err = sqlitex.Execute(db.conn,
			`INSERT INTO actor_namespace_claims (namespace, claimant_id, range_min, range_max, codec) VALUES (?1, ?2, ?3, ?4, ?5)`,
			&sqlitex.ExecOptions{Args: []any{reg.Claim.Namespace, reg.Claim.ClaimantID, reg.Claim.Range.Min[:], reg.Claim.Range.Max[:], reg.Claim.Codec}}); err != nil {
			return ptypes.SoftwareAgent{}, fmt.Errorf("RegisterFixedSoftwareAgent insert namespace claim: %w", err)
		}
	}
	if !agentFound {
		if err = sqlitex.Execute(db.conn, `INSERT INTO agents (id, kind_id) VALUES (?1, ?2)`,
			&sqlitex.ExecOptions{Args: []any{id.String(), int(ptypes.AgentKindSoftware)}}); err != nil {
			return ptypes.SoftwareAgent{}, fmt.Errorf("RegisterFixedSoftwareAgent insert agent: %w", err)
		}
		if err = sqlitex.Execute(db.conn,
			`INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1, ?2, ?3, ?4)`,
			&sqlitex.ExecOptions{Args: []any{id.String(), reg.AgentName, reg.Version, reg.Source}}); err != nil {
			return ptypes.SoftwareAgent{}, fmt.Errorf("RegisterFixedSoftwareAgent insert software agent: %w", err)
		}
	}
	if !entryFound {
		if err = sqlitex.Execute(db.conn,
			`INSERT INTO fixed_actor_manifest_entries (actor_id, namespace, kind_id, name, metadata) VALUES (?1, ?2, ?3, ?4, ?5)`,
			&sqlitex.ExecOptions{Args: []any{id.String(), reg.Entry.Namespace, int(reg.Entry.ActorKind), reg.Entry.Name, metadata}}); err != nil {
			return ptypes.SoftwareAgent{}, fmt.Errorf("RegisterFixedSoftwareAgent insert manifest entry: %w", err)
		}
	}
	return agent, nil
}

func (db *DB) fixedSoftwareAgentLocked(id ptypes.AgentID) (ptypes.SoftwareAgent, bool, error) {
	var out ptypes.SoftwareAgent
	found := false
	err := sqlitex.Execute(db.conn,
		`SELECT a.kind_id, s.name, s.version, s.source FROM agents a LEFT JOIN agents_software s ON s.agent_id = a.id WHERE a.id = ?1`,
		&sqlitex.ExecOptions{Args: []any{id.String()}, ResultFunc: func(stmt *zs.Stmt) error {
			found = true
			if ptypes.AgentKind(stmt.ColumnInt(0)) != ptypes.AgentKindSoftware || stmt.ColumnIsNull(1) {
				return fmt.Errorf("%w: actor %q exists with a non-software kind or missing software row", ptypes.ErrAgentAlreadyExists, id.String())
			}
			out = ptypes.SoftwareAgent{Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindSoftware}, Name: stmt.ColumnText(1), Version: stmt.ColumnText(2), Source: stmt.ColumnText(3)}
			return nil
		}})
	if err != nil {
		return ptypes.SoftwareAgent{}, false, fmt.Errorf("fixed software agent lookup %q: %w", id.String(), err)
	}
	return out, found, nil
}

func (db *DB) fixedActorEntryExactLocked(entry journal.FixedActorEntry, metadata string) (bool, error) {
	found := false
	err := sqlitex.Execute(db.conn,
		`SELECT actor_id, namespace, kind_id, name, metadata FROM fixed_actor_manifest_entries WHERE actor_id = ?1 OR (namespace = ?2 AND name = ?3)`,
		&sqlitex.ExecOptions{Args: []any{entry.ActorID.String(), entry.Namespace, entry.Name}, ResultFunc: func(stmt *zs.Stmt) error {
			found = true
			if stmt.ColumnText(0) != entry.ActorID.String() || stmt.ColumnText(1) != entry.Namespace ||
				ptypes.AgentKind(stmt.ColumnInt(2)) != entry.ActorKind || stmt.ColumnText(3) != entry.Name || stmt.ColumnText(4) != metadata {
				return fmt.Errorf("%w: manifest identity %q/%q conflicts with an existing entry; where: fixed software agent activation; when: preflight; impact: no rows changed; fix: use the exact stored manifest or choose an unallocated actor ID and name",
					journal.ErrNamespaceClaim, entry.Namespace, entry.Name)
			}
			return nil
		}})
	if err != nil {
		return false, fmt.Errorf("fixed actor manifest lookup: %w", err)
	}
	return found, nil
}

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
// The fixed UUID is derived from entry.ActorID, preventing identity drift.
// Returns journal.ErrEntryOutOfRange when the entry falls outside its claim.
func (db *DB) RegisterFixedActorEntry(entry journal.FixedActorEntry) error {
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
	if entry.ActorID.Namespace != entry.Namespace {
		return fmt.Errorf("%w: entry namespace %q does not match actor namespace %q",
			journal.ErrNamespaceClaim, entry.Namespace, entry.ActorID.Namespace)
	}
	if err := journal.CheckEntryInRange(claim, entry.Namespace, [16]byte(entry.ActorID.UUID)); err != nil {
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
