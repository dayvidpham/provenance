package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// RegisterFixedSoftwareAgent atomically installs the claim, software agent,
// and manifest entry. Exact existing rows are an idempotent success; drift is
// rejected before any write.
func (db *DB) RegisterFixedSoftwareAgent(reg journal.FixedSoftwareAgentRegistration) (ptypes.SoftwareAgent, error) {
	if err := reg.Validate(); err != nil {
		return ptypes.SoftwareAgent{}, err
	}
	metadata := reg.Entry.Metadata
	if metadata == "" {
		metadata = "{}"
	}
	id := ptypes.AgentID(reg.Entry.ActorID)
	agent := ptypes.SoftwareAgent{Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindSoftware}, Name: reg.AgentName, Version: reg.Version, Source: reg.Source}

	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.SoftwareAgent{}, fixedAgentActivationError(fixedAgentLeaseError{cause: err}, "the activation transaction could not start", "a database/sql connection could not be leased", "DB.RegisterFixedSoftwareAgent transaction setup", "before preflight reads or writes", "no activation rows were changed", "release outstanding scopes or reopen the database, then retry")
	}
	defer scope.release()
	var operationErr error
	if transactionErr := runImmediateTransaction(scope.ctx, scope.conn, func() (callbackErr error) {
		defer func() { operationErr = callbackErr }()
		claims, err := loadNamespaceClaims(scope.ctx, scope.conn)
		if err != nil {
			return fixedAgentActivationError(err, "existing namespace claims could not be loaded", "the preflight query failed", "DB.RegisterFixedSoftwareAgent namespace preflight", "after the transaction began and before any writes", "the activation is aborted and its transaction is rolled back", "repair database access or schema integrity, then retry")
		}
		claimFound := false
		for _, existing := range claims {
			if existing.Namespace != reg.Claim.Namespace {
				continue
			}
			claimFound = true
			if !existing.Equal(reg.Claim) {
				return fixedAgentActivationError(journal.ErrNamespaceClaim, fmt.Sprintf("namespace %q is already claimed differently", reg.Claim.Namespace), "the stored claimant, range, or codec differs from the request", "DB.RegisterFixedSoftwareAgent namespace preflight", "before activation writes", "the activation is rejected and no rows are changed", "use the exact stored claim or choose a distinct namespace")
			}
		}
		if err := journal.CheckNoOverlap(reg.Claim, claims); err != nil {
			return err
		}

		storedAgent, agentMatch, err := fixedSoftwareAgent(scope.ctx, scope.conn, id)
		if err != nil {
			return err
		}
		if agentMatch == fixedSoftwareAgentInconsistent {
			return fixedAgentActivationError(ptypes.ErrAgentAlreadyExists, fmt.Sprintf("actor %q has a non-software kind or no software-agent detail", id.String()), "the existing base and software-agent rows do not form the requested software identity", "DB.RegisterFixedSoftwareAgent actor preflight", "before activation writes", "the activation is rejected and no rows are changed", "repair the inconsistent actor rows or choose a new actor ID")
		}
		agentFound := agentMatch == fixedSoftwareAgentExact
		if !claimFound && agentFound {
			return fixedAgentActivationError(ptypes.ErrAgentAlreadyExists, fmt.Sprintf("actor %q exists before namespace %q is claimed", id.String(), reg.Claim.Namespace), "attaching a new claim to pre-existing actor state would be ambiguous", "DB.RegisterFixedSoftwareAgent actor preflight", "before activation writes", "the activation is rejected and no claim is attached", "reconcile or remove the pre-existing actor before activation")
		}
		if agentFound && storedAgent != agent {
			return fixedAgentActivationError(ptypes.ErrAgentAlreadyExists, fmt.Sprintf("actor %q differs from the requested software identity", id.String()), "the stored name, version, or source differs from the request", "DB.RegisterFixedSoftwareAgent actor preflight", "before activation writes", "the activation is rejected and no rows are changed", "retry with the exact stored name, version, and source or choose a new actor ID")
		}

		entryMatch, err := inspectFixedActorEntry(scope.ctx, scope.conn, reg.Entry, metadata)
		if err != nil {
			return err
		}
		if entryMatch == fixedActorEntryConflict {
			return fixedAgentActivationError(journal.ErrNamespaceClaim, fmt.Sprintf("manifest identity %q/%q conflicts with an existing entry", reg.Entry.Namespace, reg.Entry.Name), "the stored actor ID, namespace, kind, name, or metadata differs from the request", "DB.RegisterFixedSoftwareAgent manifest preflight", "before activation writes", "the activation is rejected and no rows are changed", "use the exact stored manifest or choose an unallocated actor ID and name")
		}

		if !claimFound {
			if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO actor_namespace_claims (namespace, claimant_id, range_min, range_max, codec) VALUES (?1, ?2, ?3, ?4, ?5)", reg.Claim.Namespace, reg.Claim.ClaimantID, reg.Claim.Range.Min[:], reg.Claim.Range.Max[:], reg.Claim.Codec); err != nil {
				return fixedAgentActivationError(err, "the namespace claim could not be written", "SQLite rejected the actor_namespace_claims insert", "DB.RegisterFixedSoftwareAgent namespace write", "during the activation transaction", "the activation is aborted and all writes are rolled back", "resolve the reported constraint, schema, storage, or lock error and retry the complete activation")
			}
		}
		if !agentFound {
			if _, err := scope.conn.ExecContext(scope.ctx, insertAgentSQL, id.String(), int(ptypes.AgentKindSoftware)); err != nil {
				return fixedAgentActivationError(err, "the base agent could not be written", "SQLite rejected the agents insert", "DB.RegisterFixedSoftwareAgent base-agent write", "during the activation transaction", "the activation is aborted and all writes are rolled back", "resolve the reported constraint, schema, storage, or lock error and retry the complete activation")
			}
			if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1, ?2, ?3, ?4)", id.String(), reg.AgentName, reg.Version, reg.Source); err != nil {
				return fixedAgentActivationError(err, "the software-agent detail could not be written", "SQLite rejected the agents_software insert", "DB.RegisterFixedSoftwareAgent software-agent write", "during the activation transaction", "the activation is aborted and all writes are rolled back", "resolve the reported constraint, schema, storage, or lock error and retry the complete activation")
			}
		}
		if entryMatch == fixedActorEntryAbsent {
			if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO fixed_actor_manifest_entries (actor_id, namespace, kind_id, name, metadata) VALUES (?1, ?2, ?3, ?4, ?5)", id.String(), reg.Entry.Namespace, int(reg.Entry.ActorKind), reg.Entry.Name, metadata); err != nil {
				return fixedAgentActivationError(err, "the fixed-actor manifest entry could not be written", "SQLite rejected the fixed_actor_manifest_entries insert", "DB.RegisterFixedSoftwareAgent manifest write", "during the activation transaction", "the activation is aborted and all writes are rolled back", "resolve the reported constraint, schema, storage, or lock error and retry the complete activation")
			}
		}
		return nil
	}); transactionErr != nil {
		// Preflight and write-path failures already describe their own cause,
		// location, timing, impact, and remediation. Wrapping them again would
		// produce two competing actionable narratives. Only BEGIN/COMMIT failures
		// that occurred outside the callback need the transaction-level envelope.
		if operationErr != nil {
			return ptypes.SoftwareAgent{}, operationErr
		}
		return ptypes.SoftwareAgent{}, fixedAgentActivationError(transactionErr, "the activation transaction could not commit", "SQLite rejected BEGIN IMMEDIATE, COMMIT, or a required activation operation", "DB.RegisterFixedSoftwareAgent transaction", "during activation", "no agent is returned and the transaction was rolled back where possible", "inspect the reported database error, reopen if necessary, and retry")
	}
	return agent, nil
}

func fixedAgentActivationError(cause error, what, why, where, when, impact, fix string) error {
	return fmt.Errorf("fixed software agent activation failed: %s; why: %s; where: %s; when: %s; impact: %s; fix: %s: %w", what, why, where, when, impact, fix, cause)
}

type fixedAgentLeaseError struct{ cause error }

func (err fixedAgentLeaseError) Error() string { return "database/sql connection lease failed" }
func (err fixedAgentLeaseError) Unwrap() error { return err.cause }

type fixedSoftwareAgentMatch uint8

const (
	fixedSoftwareAgentAbsent fixedSoftwareAgentMatch = iota
	fixedSoftwareAgentExact
	fixedSoftwareAgentInconsistent
)

func fixedSoftwareAgent(ctx context.Context, conn *sql.Conn, id ptypes.AgentID) (ptypes.SoftwareAgent, fixedSoftwareAgentMatch, error) {
	rows, err := conn.QueryContext(ctx, "SELECT a.kind_id, s.name, s.version, s.source FROM agents a LEFT JOIN agents_software s ON s.agent_id=a.id WHERE a.id=?1", id.String())
	if err != nil {
		return ptypes.SoftwareAgent{}, fixedSoftwareAgentAbsent, fixedAgentActivationError(err, fmt.Sprintf("existing actor %q could not be loaded", id.String()), "the actor preflight query failed", "fixedSoftwareAgent", "before activation writes", "the activation is aborted and no rows are changed", "repair database access or schema integrity, then retry")
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return ptypes.SoftwareAgent{}, fixedSoftwareAgentAbsent, err
		}
		return ptypes.SoftwareAgent{}, fixedSoftwareAgentAbsent, nil
	}
	var kind int
	var name, version, source sql.NullString
	if err := rows.Scan(&kind, &name, &version, &source); err != nil {
		return ptypes.SoftwareAgent{}, fixedSoftwareAgentAbsent, fmt.Errorf("scan existing actor %q: %w", id.String(), err)
	}
	if ptypes.AgentKind(kind) != ptypes.AgentKindSoftware || !name.Valid || !version.Valid || !source.Valid {
		return ptypes.SoftwareAgent{}, fixedSoftwareAgentInconsistent, nil
	}
	return ptypes.SoftwareAgent{Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindSoftware}, Name: name.String, Version: version.String, Source: source.String}, fixedSoftwareAgentExact, nil
}

type fixedActorEntryMatch uint8

const (
	fixedActorEntryAbsent fixedActorEntryMatch = iota
	fixedActorEntryExact
	fixedActorEntryConflict
)

func inspectFixedActorEntry(ctx context.Context, conn *sql.Conn, entry journal.FixedActorEntry, metadata string) (fixedActorEntryMatch, error) {
	rows, err := conn.QueryContext(ctx, "SELECT actor_id, namespace, kind_id, name, metadata FROM fixed_actor_manifest_entries WHERE actor_id=?1 OR (namespace=?2 AND name=?3)", entry.ActorID.String(), entry.Namespace, entry.Name)
	if err != nil {
		return fixedActorEntryAbsent, fixedAgentActivationError(err, fmt.Sprintf("manifest identity %q/%q could not be loaded", entry.Namespace, entry.Name), "the manifest preflight query failed", "inspectFixedActorEntry", "before activation writes", "the activation is aborted and no rows are changed", "repair database access or schema integrity, then retry")
	}
	defer rows.Close()
	match := fixedActorEntryAbsent
	for rows.Next() {
		var actorID, namespace, name, storedMetadata string
		var kind int
		if err := rows.Scan(&actorID, &namespace, &kind, &name, &storedMetadata); err != nil {
			return fixedActorEntryAbsent, fmt.Errorf("scan fixed actor manifest entry: %w", err)
		}
		if actorID != entry.ActorID.String() || namespace != entry.Namespace || ptypes.AgentKind(kind) != entry.ActorKind || name != entry.Name || storedMetadata != metadata {
			match = fixedActorEntryConflict
			continue
		}
		if match != fixedActorEntryConflict {
			match = fixedActorEntryExact
		}
	}
	if err := rows.Err(); err != nil {
		return fixedActorEntryAbsent, fmt.Errorf("iterate fixed actor manifest entry: %w", err)
	}
	return match, nil
}

// RegisterNamespaceClaim records a non-overlapping namespace range.
func (db *DB) RegisterNamespaceClaim(claim journal.ActorNamespaceClaim) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("RegisterNamespaceClaim %q: lease connection: %w", claim.Namespace, err)
	}
	defer scope.release()
	return runImmediateTransaction(scope.ctx, scope.conn, func() error {
		existing, err := loadNamespaceClaims(scope.ctx, scope.conn)
		if err != nil {
			return err
		}
		for _, current := range existing {
			if current.Namespace != claim.Namespace {
				continue
			}
			if current.Equal(claim) {
				return nil
			}
			return fmt.Errorf("%w: namespace %q is already claimed with a different claimant/range/codec; where: actor namespace registration; when: before commit; impact: the differing re-registration is rejected; fix: register a distinct namespace or reconcile the existing claim first", journal.ErrNamespaceClaim, claim.Namespace)
		}
		if err := journal.CheckNoOverlap(claim, existing); err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO actor_namespace_claims (namespace, claimant_id, range_min, range_max, codec) VALUES (?1, ?2, ?3, ?4, ?5)", claim.Namespace, claim.ClaimantID, claim.Range.Min[:], claim.Range.Max[:], claim.Codec); err != nil {
			return fmt.Errorf("RegisterNamespaceClaim %q: %w", claim.Namespace, err)
		}
		return nil
	})
}

// RegisterFixedActorEntry validates membership in the claimed range then stores
// one immutable manifest row.
func (db *DB) RegisterFixedActorEntry(entry journal.FixedActorEntry) error {
	if entry.Namespace == "" {
		return fmt.Errorf("%w: entry namespace is required", journal.ErrNamespaceClaim)
	}
	if entry.Name == "" {
		return fmt.Errorf("%w: entry name is required for namespace %q", journal.ErrNamespaceClaim, entry.Namespace)
	}
	if entry.ActorID.Namespace != entry.Namespace {
		return fmt.Errorf("%w: fixed actor entry namespace %q does not match actor namespace %q; where: DB.RegisterFixedActorEntry validation; impact: no rows are changed; fix: use the same claimed namespace", journal.ErrNamespaceClaim, entry.Namespace, entry.ActorID.Namespace)
	}
	metadata := entry.Metadata
	if metadata == "" {
		metadata = "{}"
	}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("RegisterFixedActorEntry %q: lease connection: %w", entry.ActorID.String(), err)
	}
	defer scope.release()
	return runImmediateTransaction(scope.ctx, scope.conn, func() error {
		claim, found, err := getNamespaceClaim(scope.ctx, scope.conn, entry.Namespace)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: namespace %q has no registered claim; where: fixed-actor registration; impact: the entry is rejected; fix: register the namespace claim before adding entries", journal.ErrNamespaceClaim, entry.Namespace)
		}
		if err := journal.CheckEntryInRange(claim, entry.Namespace, [16]byte(entry.ActorID.UUID)); err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO fixed_actor_manifest_entries (actor_id, namespace, kind_id, name, metadata) VALUES (?1, ?2, ?3, ?4, ?5)", entry.ActorID.String(), entry.Namespace, int(entry.ActorKind), entry.Name, metadata); err != nil {
			return fmt.Errorf("RegisterFixedActorEntry %q: %w", entry.ActorID.String(), err)
		}
		return nil
	})
}

func (db *DB) NamespaceClaims() ([]journal.ActorNamespaceClaim, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, fmt.Errorf("NamespaceClaims: lease connection: %w", err)
	}
	defer scope.release()
	return loadNamespaceClaims(scope.ctx, scope.conn)
}

func loadNamespaceClaims(ctx context.Context, conn *sql.Conn) ([]journal.ActorNamespaceClaim, error) {
	rows, err := conn.QueryContext(ctx, "SELECT namespace, claimant_id, range_min, range_max, codec FROM actor_namespace_claims ORDER BY namespace ASC")
	if err != nil {
		return nil, fmt.Errorf("loadNamespaceClaims: %w", err)
	}
	defer rows.Close()
	var claims []journal.ActorNamespaceClaim
	for rows.Next() {
		claim, err := scanNamespaceClaim(rows)
		if err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loadNamespaceClaims: %w", err)
	}
	return claims, nil
}

func getNamespaceClaim(ctx context.Context, conn *sql.Conn, namespace string) (journal.ActorNamespaceClaim, bool, error) {
	rows, err := conn.QueryContext(ctx, "SELECT namespace, claimant_id, range_min, range_max, codec FROM actor_namespace_claims WHERE namespace=?1", namespace)
	if err != nil {
		return journal.ActorNamespaceClaim{}, false, fmt.Errorf("getNamespaceClaim %q: %w", namespace, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return journal.ActorNamespaceClaim{}, false, fmt.Errorf("getNamespaceClaim %q: %w", namespace, err)
		}
		return journal.ActorNamespaceClaim{}, false, nil
	}
	claim, err := scanNamespaceClaim(rows)
	if err != nil {
		return journal.ActorNamespaceClaim{}, false, err
	}
	return claim, true, nil
}

func scanNamespaceClaim(row sqlRowScanner) (journal.ActorNamespaceClaim, error) {
	var claim journal.ActorNamespaceClaim
	var min, max []byte
	if err := row.Scan(&claim.Namespace, &claim.ClaimantID, &min, &max, &claim.Codec); err != nil {
		return journal.ActorNamespaceClaim{}, fmt.Errorf("scan namespace claim: %w", err)
	}
	minValue, err := readBlob16(min, "range_min")
	if err != nil {
		return journal.ActorNamespaceClaim{}, fmt.Errorf("namespace %q range_min: %w", claim.Namespace, err)
	}
	maxValue, err := readBlob16(max, "range_max")
	if err != nil {
		return journal.ActorNamespaceClaim{}, fmt.Errorf("namespace %q range_max: %w", claim.Namespace, err)
	}
	claim.Range = journal.UUIDRange{Min: minValue, Max: maxValue}
	return claim, nil
}

var errBadBlobWidth = errors.New("sqlite: stored range bound is not 16 bytes")

func readBlob16(value []byte, column string) ([16]byte, error) {
	var out [16]byte
	if len(value) != len(out) {
		return out, fmt.Errorf("%w: got %d bytes in %s", errBadBlobWidth, len(value), column)
	}
	copy(out[:], value)
	return out, nil
}
