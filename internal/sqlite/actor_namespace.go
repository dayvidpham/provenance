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
		return ptypes.SoftwareAgent{}, err
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
	if err = executeStatement(db.conn, sharedDDLBeginStatement4e51, nil); err != nil {
		return ptypes.SoftwareAgent{}, fixedAgentActivationError(err,
			"the activation transaction could not start",
			"SQLite rejected BEGIN IMMEDIATE",
			"DB.RegisterFixedSoftwareAgent transaction setup",
			"before preflight reads or writes",
			"no activation rows were changed",
			"verify the database is open and writable, then retry")
	}
	defer func() {
		if err != nil {
			if rollbackErr := executeStatement(db.conn, sharedDDLRollbackStatement4eec, nil); rollbackErr != nil {
				err = fixedAgentActivationError(errors.Join(err, rollbackErr),
					"the activation failed and its rollback also reported an error",
					"SQLite rejected ROLLBACK after an earlier activation failure",
					"DB.RegisterFixedSoftwareAgent transaction cleanup",
					"while aborting the failed activation",
					"the caller must treat the transaction outcome as unknown and no agent is returned",
					"close and reopen the database, inspect all four activation tables, reconcile partial state, and retry")
			}
			return
		}
		if commitErr := executeStatement(db.conn, sharedDDLCommitStatement696a, nil); commitErr != nil {
			rollbackErr := executeStatement(db.conn, sharedDDLRollbackStatement4eec, nil)
			cause := commitErr
			impact := "no agent is returned; SQLite rejected the commit and the transaction was rolled back"
			fix := "retry the complete activation; if it repeats, verify database health and available storage"
			if rollbackErr != nil {
				cause = errors.Join(commitErr, rollbackErr)
				impact = "no agent is returned and the transaction outcome is unknown because rollback also failed"
				fix = "close and reopen the database, inspect all four activation tables, reconcile partial state, and retry"
			}
			err = fixedAgentActivationError(cause,
				"the activation transaction could not commit",
				"SQLite rejected COMMIT",
				"DB.RegisterFixedSoftwareAgent transaction finalization",
				"after all activation writes completed",
				impact, fix)
			agent = ptypes.SoftwareAgent{}
		}
	}()

	claims, err := db.loadNamespaceClaimsLocked()
	if err != nil {
		return ptypes.SoftwareAgent{}, fixedAgentActivationError(err,
			"existing namespace claims could not be loaded",
			"the preflight query failed",
			"DB.RegisterFixedSoftwareAgent namespace preflight",
			"after the transaction began and before any writes",
			"the activation is aborted and its transaction is rolled back",
			"repair database access or schema integrity, then retry")
	}
	claimFound := false
	for _, existing := range claims {
		if existing.Namespace != reg.Claim.Namespace {
			continue
		}
		claimFound = true
		if !existing.Equal(reg.Claim) {
			return ptypes.SoftwareAgent{}, fixedAgentActivationError(journal.ErrNamespaceClaim,
				fmt.Sprintf("namespace %q is already claimed differently", reg.Claim.Namespace),
				"the stored claimant, range, or codec differs from the request",
				"DB.RegisterFixedSoftwareAgent namespace preflight",
				"before activation writes",
				"the activation is rejected and no rows are changed",
				"use the exact stored claim or choose a distinct namespace")
		}
	}
	if err := journal.CheckNoOverlap(reg.Claim, claims); err != nil {
		return ptypes.SoftwareAgent{}, err
	}

	storedAgent, agentMatch, err := db.fixedSoftwareAgentLocked(id)
	if err != nil {
		return ptypes.SoftwareAgent{}, err
	}
	if agentMatch == fixedSoftwareAgentInconsistent {
		return ptypes.SoftwareAgent{}, fixedAgentActivationError(ptypes.ErrAgentAlreadyExists,
			fmt.Sprintf("actor %q has a non-software kind or no software-agent detail", id.String()),
			"the existing base and software-agent rows do not form the requested software identity",
			"DB.RegisterFixedSoftwareAgent actor preflight",
			"before activation writes",
			"the activation is rejected and no rows are changed",
			"repair the inconsistent actor rows or choose a new actor ID")
	}
	agentFound := agentMatch == fixedSoftwareAgentExact
	if !claimFound && agentFound {
		return ptypes.SoftwareAgent{}, fixedAgentActivationError(ptypes.ErrAgentAlreadyExists,
			fmt.Sprintf("actor %q exists before namespace %q is claimed", id.String(), reg.Claim.Namespace),
			"attaching a new claim to pre-existing actor state would be ambiguous",
			"DB.RegisterFixedSoftwareAgent actor preflight",
			"before activation writes",
			"the activation is rejected and no claim is attached",
			"reconcile or remove the pre-existing actor before activation")
	}
	if agentFound && storedAgent != agent {
		return ptypes.SoftwareAgent{}, fixedAgentActivationError(ptypes.ErrAgentAlreadyExists,
			fmt.Sprintf("actor %q differs from the requested software identity", id.String()),
			"the stored name, version, or source differs from the request",
			"DB.RegisterFixedSoftwareAgent actor preflight",
			"before activation writes",
			"the activation is rejected and no rows are changed",
			"retry with the exact stored name, version, and source or choose a new actor ID")
	}

	entryMatch, err := db.fixedActorEntryExactLocked(reg.Entry, metadata)
	if err != nil {
		return ptypes.SoftwareAgent{}, err
	}
	if entryMatch == fixedActorEntryConflict {
		return ptypes.SoftwareAgent{}, fixedAgentActivationError(journal.ErrNamespaceClaim,
			fmt.Sprintf("manifest identity %q/%q conflicts with an existing entry", reg.Entry.Namespace, reg.Entry.Name),
			"the stored actor ID, namespace, kind, name, or metadata differs from the request",
			"DB.RegisterFixedSoftwareAgent manifest preflight",
			"before activation writes",
			"the activation is rejected and no rows are changed",
			"use the exact stored manifest or choose an unallocated actor ID and name")
	}
	entryFound := entryMatch == fixedActorEntryExact

	if !claimFound {
		if err = executeStatement(db.conn,
			agentsInsertActorNamespaceClaims9230,
			&sqlitex.ExecOptions{Args: []any{reg.Claim.Namespace, reg.Claim.ClaimantID, reg.Claim.Range.Min[:], reg.Claim.Range.Max[:], reg.Claim.Codec}}); err != nil {
			return ptypes.SoftwareAgent{}, fixedAgentActivationError(err,
				"the namespace claim could not be written",
				"SQLite rejected the actor_namespace_claims insert",
				"DB.RegisterFixedSoftwareAgent namespace write",
				"during the activation transaction",
				"the activation is aborted and all writes are rolled back",
				"resolve the reported constraint, schema, storage, or lock error and retry the complete activation")
		}
	}
	if !agentFound {
		if err = executeStatement(db.conn, agentsInsertAgentse4db,
			&sqlitex.ExecOptions{Args: []any{id.String(), int(ptypes.AgentKindSoftware)}}); err != nil {
			return ptypes.SoftwareAgent{}, fixedAgentActivationError(err,
				"the base agent could not be written",
				"SQLite rejected the agents insert",
				"DB.RegisterFixedSoftwareAgent base-agent write",
				"during the activation transaction",
				"the activation is aborted and all writes are rolled back",
				"resolve the reported constraint, schema, storage, or lock error and retry the complete activation")
		}
		if err = executeStatement(db.conn,
			agentsInsertAgentsSoftwaref75f,
			&sqlitex.ExecOptions{Args: []any{id.String(), reg.AgentName, reg.Version, reg.Source}}); err != nil {
			return ptypes.SoftwareAgent{}, fixedAgentActivationError(err,
				"the software-agent detail could not be written",
				"SQLite rejected the agents_software insert",
				"DB.RegisterFixedSoftwareAgent software-agent write",
				"during the activation transaction",
				"the activation is aborted and all writes are rolled back",
				"resolve the reported constraint, schema, storage, or lock error and retry the complete activation")
		}
	}
	if !entryFound {
		if err = executeStatement(db.conn,
			agentsInsertFixedActorManifestEntries823e,
			&sqlitex.ExecOptions{Args: []any{id.String(), reg.Entry.Namespace, int(reg.Entry.ActorKind), reg.Entry.Name, metadata}}); err != nil {
			return ptypes.SoftwareAgent{}, fixedAgentActivationError(err,
				"the fixed-actor manifest entry could not be written",
				"SQLite rejected the fixed_actor_manifest_entries insert",
				"DB.RegisterFixedSoftwareAgent manifest write",
				"during the activation transaction",
				"the activation is aborted and all writes are rolled back",
				"resolve the reported constraint, schema, storage, or lock error and retry the complete activation")
		}
	}
	return agent, nil
}

func fixedAgentActivationError(cause error, what, why, where, when, impact, fix string) error {
	return fmt.Errorf(
		"fixed software agent activation failed: %s; why: %s; where: %s; when: %s; impact: %s; fix: %s: %w",
		what, why, where, when, impact, fix, cause,
	)
}

type fixedSoftwareAgentMatch uint8

const (
	fixedSoftwareAgentAbsent fixedSoftwareAgentMatch = iota
	fixedSoftwareAgentExact
	fixedSoftwareAgentInconsistent
)

func (db *DB) fixedSoftwareAgentLocked(id ptypes.AgentID) (ptypes.SoftwareAgent, fixedSoftwareAgentMatch, error) {
	var out ptypes.SoftwareAgent
	match := fixedSoftwareAgentAbsent
	err := executeStatement(db.conn,
		agentsSelectAgentsca17,
		&sqlitex.ExecOptions{Args: []any{id.String()}, ResultFunc: func(stmt *zs.Stmt) error {
			if ptypes.AgentKind(stmt.ColumnInt(0)) != ptypes.AgentKindSoftware || stmt.ColumnIsNull(1) {
				match = fixedSoftwareAgentInconsistent
				return nil
			}
			match = fixedSoftwareAgentExact
			out = ptypes.SoftwareAgent{Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindSoftware}, Name: stmt.ColumnText(1), Version: stmt.ColumnText(2), Source: stmt.ColumnText(3)}
			return nil
		}})
	if err != nil {
		return ptypes.SoftwareAgent{}, fixedSoftwareAgentAbsent, fixedAgentActivationError(err,
			fmt.Sprintf("existing actor %q could not be loaded", id.String()),
			"the actor preflight query failed",
			"DB.fixedSoftwareAgentLocked",
			"before activation writes",
			"the activation is aborted and no rows are changed",
			"repair database access or schema integrity, then retry")
	}
	return out, match, nil
}

type fixedActorEntryMatch uint8

const (
	fixedActorEntryAbsent fixedActorEntryMatch = iota
	fixedActorEntryExact
	fixedActorEntryConflict
)

func (db *DB) fixedActorEntryExactLocked(entry journal.FixedActorEntry, metadata string) (fixedActorEntryMatch, error) {
	match := fixedActorEntryAbsent
	err := executeStatement(db.conn,
		agentsSelectFixedActorManifestEntriesa609,
		&sqlitex.ExecOptions{Args: []any{entry.ActorID.String(), entry.Namespace, entry.Name}, ResultFunc: func(stmt *zs.Stmt) error {
			if stmt.ColumnText(0) != entry.ActorID.String() || stmt.ColumnText(1) != entry.Namespace ||
				ptypes.AgentKind(stmt.ColumnInt(2)) != entry.ActorKind || stmt.ColumnText(3) != entry.Name || stmt.ColumnText(4) != metadata {
				match = fixedActorEntryConflict
				return nil
			}
			if match != fixedActorEntryConflict {
				match = fixedActorEntryExact
			}
			return nil
		}})
	if err != nil {
		return fixedActorEntryAbsent, fixedAgentActivationError(err,
			fmt.Sprintf("manifest identity %q/%q could not be loaded", entry.Namespace, entry.Name),
			"the manifest preflight query failed",
			"DB.fixedActorEntryExactLocked",
			"before activation writes",
			"the activation is aborted and no rows are changed",
			"repair database access or schema integrity, then retry")
	}
	return match, nil
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
	if txErr = executeStatement(db.conn,
		agentsInsertActorNamespaceClaims7b3c,
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
		return fmt.Errorf(
			"%w: fixed actor entry namespace %q does not match actor namespace %q; why: a manifest entry and its actor ID must share one namespace; where: DB.RegisterFixedActorEntry validation; when: before database lookup or mutation; impact: the entry is rejected and no rows are changed; fix: set Entry.Namespace and Entry.ActorID.Namespace to the same claimed namespace",
			journal.ErrNamespaceClaim, entry.Namespace, entry.ActorID.Namespace,
		)
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
	if txErr = executeStatement(db.conn,
		agentsInsertFixedActorManifestEntries73e8,
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
	if err := executeStatement(db.conn,
		agentsSelectActorNamespaceClaims769b,
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
	if err := executeStatement(db.conn,
		agentsSelectActorNamespaceClaims091c,
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
