package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"

	"github.com/dayvidpham/provenance/internal/allocation"
	"github.com/dayvidpham/provenance/internal/fusedtx"
	"github.com/dayvidpham/provenance/internal/journal"
)

type IDConversionOptions struct {
	RewriteEffect  func(journal.Effect) (journal.Effect, error)
	RewriteContext func(string, string) (string, error)
	RewriteCommand func(string, string, []byte, journal.CanonicalMutation, journal.CanonicalMutation) ([]byte, error)
}

type IDConversionReport struct {
	ChangedFields, ChangedMutations, ChangedAllocations int
	AlreadyCurrent                                      bool
}

// identityColumnSQL is the closed set of identity fields owned by conversion.
// Each selector returns a complete statement, never runtime identifiers.
type identityColumnSQL uint8

const (
	identityTaskID identityColumnSQL = iota + 1
	identityTaskOwner
	identityAgentID
	identityHumanAgent
	identityMLAgent
	identitySoftwareAgent
	identityActivityID
	identityActivityAgent
	identityCommentID
	identityCommentTask
	identityCommentAuthor
	identityLabelTask
	identityEdgeSource
	identityEdgeTarget
	identityAttributionTask
	identityAttributionActor
	identityNamespaceClaimant
	identityManifestActor
	identityJournalActor
	identityJournalActivity
	identityAssignmentTask
	identityAssignmentActor
	identityDecisionTask
	identityEvidenceTask
	identityEventTask
	identityAllocationActor
	identityDecisionContext
	identityEvidenceContext
	identityEventContext
)

func (field identityColumnSQL) readSQL() string {
	switch field {
	case identityTaskID:
		return `SELECT DISTINCT "id" FROM "tasks" WHERE "id" IS NOT NULL`
	case identityTaskOwner:
		return `SELECT DISTINCT "owner_id" FROM "tasks" WHERE "owner_id" IS NOT NULL`
	case identityAgentID:
		return `SELECT DISTINCT "id" FROM "agents" WHERE "id" IS NOT NULL`
	case identityHumanAgent:
		return `SELECT DISTINCT "agent_id" FROM "agents_human" WHERE "agent_id" IS NOT NULL`
	case identityMLAgent:
		return `SELECT DISTINCT "agent_id" FROM "agents_ml" WHERE "agent_id" IS NOT NULL`
	case identitySoftwareAgent:
		return `SELECT DISTINCT "agent_id" FROM "agents_software" WHERE "agent_id" IS NOT NULL`
	case identityActivityID:
		return `SELECT DISTINCT "id" FROM "activities" WHERE "id" IS NOT NULL`
	case identityActivityAgent:
		return `SELECT DISTINCT "agent_id" FROM "activities" WHERE "agent_id" IS NOT NULL`
	case identityCommentID:
		return `SELECT DISTINCT "id" FROM "comments" WHERE "id" IS NOT NULL`
	case identityCommentTask:
		return `SELECT DISTINCT "task_id" FROM "comments" WHERE "task_id" IS NOT NULL`
	case identityCommentAuthor:
		return `SELECT DISTINCT "author_id" FROM "comments" WHERE "author_id" IS NOT NULL`
	case identityLabelTask:
		return `SELECT DISTINCT "task_id" FROM "labels" WHERE "task_id" IS NOT NULL`
	case identityEdgeSource:
		return `SELECT DISTINCT "source_id" FROM "edges" WHERE "source_id" IS NOT NULL`
	case identityEdgeTarget:
		return `SELECT DISTINCT "target_id" FROM "edges" WHERE "target_id" IS NOT NULL`
	case identityAttributionTask:
		return `SELECT DISTINCT "task_id" FROM "task_attributions" WHERE "task_id" IS NOT NULL`
	case identityAttributionActor:
		return `SELECT DISTINCT "actor_id" FROM "task_attributions" WHERE "actor_id" IS NOT NULL`
	case identityNamespaceClaimant:
		return `SELECT DISTINCT "claimant_id" FROM "actor_namespace_claims" WHERE "claimant_id" IS NOT NULL`
	case identityManifestActor:
		return `SELECT DISTINCT "actor_id" FROM "fixed_actor_manifest_entries" WHERE "actor_id" IS NOT NULL`
	case identityJournalActor:
		return `SELECT DISTINCT "actor_id" FROM "journal" WHERE "actor_id" IS NOT NULL`
	case identityJournalActivity:
		return `SELECT DISTINCT "activity_id" FROM "journal_activity_creations" WHERE "activity_id" IS NOT NULL`
	case identityAssignmentTask:
		return `SELECT DISTINCT "task_id" FROM "journal_authority_assignment_episodes" WHERE "task_id" IS NOT NULL`
	case identityAssignmentActor:
		return `SELECT DISTINCT "actor_id" FROM "journal_authority_assignment_episodes" WHERE "actor_id" IS NOT NULL`
	case identityDecisionTask:
		return `SELECT DISTINCT "task_id" FROM "journal_decisions" WHERE "task_id" IS NOT NULL`
	case identityEvidenceTask:
		return `SELECT DISTINCT "task_id" FROM "journal_evidence" WHERE "task_id" IS NOT NULL`
	case identityEventTask:
		return `SELECT DISTINCT "task_id" FROM "journal_task_events" WHERE "task_id" IS NOT NULL`
	case identityAllocationActor:
		return `SELECT DISTINCT "actor_id" FROM "governed_allocation_operations" WHERE "actor_id" IS NOT NULL`
	case identityDecisionContext:
		return `SELECT DISTINCT context_kind,context_identity FROM "journal_decision_contexts"`
	case identityEvidenceContext:
		return `SELECT DISTINCT context_kind,context_identity FROM "journal_evidence_contexts"`
	case identityEventContext:
		return `SELECT DISTINCT context_kind,context_identity FROM "journal_task_event_contexts"`
	default:
		panic("invalid identity conversion column")
	}
}

func (field identityColumnSQL) updateSQL() string {
	switch field {
	case identityTaskID:
		return `UPDATE "tasks" SET "id"=? WHERE "id"=?`
	case identityTaskOwner:
		return `UPDATE "tasks" SET "owner_id"=? WHERE "owner_id"=?`
	case identityAgentID:
		return `UPDATE "agents" SET "id"=? WHERE "id"=?`
	case identityHumanAgent:
		return `UPDATE "agents_human" SET "agent_id"=? WHERE "agent_id"=?`
	case identityMLAgent:
		return `UPDATE "agents_ml" SET "agent_id"=? WHERE "agent_id"=?`
	case identitySoftwareAgent:
		return `UPDATE "agents_software" SET "agent_id"=? WHERE "agent_id"=?`
	case identityActivityID:
		return `UPDATE "activities" SET "id"=? WHERE "id"=?`
	case identityActivityAgent:
		return `UPDATE "activities" SET "agent_id"=? WHERE "agent_id"=?`
	case identityCommentID:
		return `UPDATE "comments" SET "id"=? WHERE "id"=?`
	case identityCommentTask:
		return `UPDATE "comments" SET "task_id"=? WHERE "task_id"=?`
	case identityCommentAuthor:
		return `UPDATE "comments" SET "author_id"=? WHERE "author_id"=?`
	case identityLabelTask:
		return `UPDATE "labels" SET "task_id"=? WHERE "task_id"=?`
	case identityEdgeSource:
		return `UPDATE "edges" SET "source_id"=? WHERE "source_id"=?`
	case identityEdgeTarget:
		return `UPDATE "edges" SET "target_id"=? WHERE "target_id"=?`
	case identityAttributionTask:
		return `UPDATE "task_attributions" SET "task_id"=? WHERE "task_id"=?`
	case identityAttributionActor:
		return `UPDATE "task_attributions" SET "actor_id"=? WHERE "actor_id"=?`
	case identityNamespaceClaimant:
		return `UPDATE "actor_namespace_claims" SET "claimant_id"=? WHERE "claimant_id"=?`
	case identityManifestActor:
		return `UPDATE "fixed_actor_manifest_entries" SET "actor_id"=? WHERE "actor_id"=?`
	case identityJournalActor:
		return `UPDATE "journal" SET "actor_id"=? WHERE "actor_id"=?`
	case identityJournalActivity:
		return `UPDATE "journal_activity_creations" SET "activity_id"=? WHERE "activity_id"=?`
	case identityAssignmentTask:
		return `UPDATE "journal_authority_assignment_episodes" SET "task_id"=? WHERE "task_id"=?`
	case identityAssignmentActor:
		return `UPDATE "journal_authority_assignment_episodes" SET "actor_id"=? WHERE "actor_id"=?`
	case identityDecisionTask:
		return `UPDATE "journal_decisions" SET "task_id"=? WHERE "task_id"=?`
	case identityEvidenceTask:
		return `UPDATE "journal_evidence" SET "task_id"=? WHERE "task_id"=?`
	case identityEventTask:
		return `UPDATE "journal_task_events" SET "task_id"=? WHERE "task_id"=?`
	case identityAllocationActor:
		return `UPDATE "governed_allocation_operations" SET "actor_id"=? WHERE "actor_id"=?`
	case identityDecisionContext:
		return `UPDATE "journal_decision_contexts" SET "context_identity"=? WHERE "context_identity"=?`
	case identityEvidenceContext:
		return `UPDATE "journal_evidence_contexts" SET "context_identity"=? WHERE "context_identity"=?`
	case identityEventContext:
		return `UPDATE "journal_task_event_contexts" SET "context_identity"=? WHERE "context_identity"=?`
	default:
		panic("invalid identity conversion column")
	}
}

func (field identityColumnSQL) updateContextSQL() string {
	switch field {
	case identityDecisionContext:
		return `UPDATE "journal_decision_contexts" SET "context_identity"=? WHERE "context_identity"=? AND context_kind=?`
	case identityEvidenceContext:
		return `UPDATE "journal_evidence_contexts" SET "context_identity"=? WHERE "context_identity"=? AND context_kind=?`
	case identityEventContext:
		return `UPDATE "journal_task_event_contexts" SET "context_identity"=? WHERE "context_identity"=? AND context_kind=?`
	default:
		panic("invalid identity conversion context column")
	}
}

type identityColumn struct {
	table, column string
	selector      identityColumnSQL
}

var ownedIdentityColumns = []identityColumn{
	{"tasks", "id", identityTaskID}, {"tasks", "owner_id", identityTaskOwner}, {"agents", "id", identityAgentID}, {"agents_human", "agent_id", identityHumanAgent}, {"agents_ml", "agent_id", identityMLAgent}, {"agents_software", "agent_id", identitySoftwareAgent},
	{"activities", "id", identityActivityID}, {"activities", "agent_id", identityActivityAgent}, {"comments", "id", identityCommentID}, {"comments", "task_id", identityCommentTask}, {"comments", "author_id", identityCommentAuthor}, {"labels", "task_id", identityLabelTask},
	{"edges", "source_id", identityEdgeSource}, {"edges", "target_id", identityEdgeTarget}, {"task_attributions", "task_id", identityAttributionTask}, {"task_attributions", "actor_id", identityAttributionActor},
	{"actor_namespace_claims", "claimant_id", identityNamespaceClaimant}, {"fixed_actor_manifest_entries", "actor_id", identityManifestActor}, {"journal", "actor_id", identityJournalActor},
	{"journal_activity_creations", "activity_id", identityJournalActivity}, {"journal_authority_assignment_episodes", "task_id", identityAssignmentTask}, {"journal_authority_assignment_episodes", "actor_id", identityAssignmentActor},
	{"journal_decisions", "task_id", identityDecisionTask}, {"journal_evidence", "task_id", identityEvidenceTask}, {"journal_task_events", "task_id", identityEventTask}, {"governed_allocation_operations", "actor_id", identityAllocationActor},
}

type identityChange struct {
	identityColumn
	old, value, kind string
}
type mutationChange struct {
	operation              string
	bytes, digest, command []byte
	version                string
	effects                []journal.Effect
}
type allocationChange struct {
	operation string
	converted allocation.ConvertedRequest
	oldDigest []byte
}

// ConvertIDsInTx never begins, commits, rolls back, or publishes the marker.
// All planning and owner decoding precede writes; the caller must roll back on
// every error and verify its own projections/evidence before publishing LAST.
func ConvertIDsInTx(ctx context.Context, tx *sql.Tx, options IDConversionOptions) (IDConversionReport, error) {
	var report IDConversionReport
	if ctx == nil || tx == nil {
		return report, fmt.Errorf("ConvertIDsInTx: no caller transaction; no conversion started; fix: stop writers and begin the offline migration transaction")
	}
	inspection, err := InspectIDFormat(ctx, tx)
	if err != nil {
		return report, err
	}
	if inspection.Format == IDFormatSlash {
		report.AlreadyCurrent = true
		return report, nil
	}
	if !inspection.Populated {
		return report, nil
	}
	var changes []identityChange
	for _, field := range ownedIdentityColumns {
		seen := map[string]string{}
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)", field.table).Scan(&exists); err != nil {
			return report, err
		}
		if !exists {
			continue
		}
		rows, err := tx.QueryContext(ctx, field.selector.readSQL())
		if err != nil {
			return report, fmt.Errorf("ID conversion preflight %s.%s: %w", field.table, field.column, err)
		}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return report, err
			}
			if raw == "" {
				rows.Close()
				return report, fmt.Errorf("ID conversion preflight %s.%s: empty typed identity; store unchanged; fix: repair from archive", field.table, field.column)
			}
			value, err := journal.ConvertRetiredIdentity(raw)
			if err != nil {
				rows.Close()
				return report, fmt.Errorf("ID conversion preflight %s.%s key %q: %w", field.table, field.column, raw, err)
			}
			if previous, exists := seen[value]; exists && previous != raw {
				rows.Close()
				return report, fmt.Errorf("ID conversion preflight collision in %s.%s: %q and %q map to %q; no writes performed; fix: repair duplicate identity keys from archive", field.table, field.column, previous, raw, value)
			}
			seen[value] = raw
			changes = append(changes, identityChange{field, raw, value, ""})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return report, err
		}
	}
	for _, field := range []identityColumn{
		{"journal_decision_contexts", "context_identity", identityDecisionContext},
		{"journal_evidence_contexts", "context_identity", identityEvidenceContext},
		{"journal_task_event_contexts", "context_identity", identityEventContext},
	} {
		table := field.table
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)", table).Scan(&exists); err != nil {
			return report, err
		}
		if !exists {
			continue
		}
		rows, err := tx.QueryContext(ctx, field.selector.readSQL())
		if err != nil {
			return report, err
		}
		for rows.Next() {
			var kind, raw, value string
			if err := rows.Scan(&kind, &raw); err != nil {
				rows.Close()
				return report, err
			}
			value = raw
			switch journal.EventContextKind(kind) {
			case journal.EventContextKindTask, journal.EventContextKindActor, journal.EventContextKindActivity:
				value, err = journal.ConvertRetiredIdentity(raw)
			default:
				if options.RewriteContext != nil {
					value, err = options.RewriteContext(kind, raw)
				}
			}
			if err != nil {
				rows.Close()
				return report, fmt.Errorf("ID conversion context %s kind %q key %q: %w", table, kind, raw, err)
			}
			if value != raw {
				changes = append(changes, identityChange{field, raw, value, kind})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return report, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT operation_id,canonical_request,canonical_digest FROM governed_allocation_operations ORDER BY anchor_journal_id`)
	if err != nil {
		return report, err
	}
	var allocations []allocationChange
	allocationByOperation := map[string]allocationChange{}
	supplements := map[string]allocation.ConvertedRequest{}
	for rows.Next() {
		var operation string
		var canonical, digest []byte
		if err := rows.Scan(&operation, &canonical, &digest); err != nil {
			rows.Close()
			return report, err
		}
		converted, err := allocation.ConvertRetiredRequest(canonical, digest)
		if err != nil {
			rows.Close()
			return report, fmt.Errorf("allocation %q preflight: %w", operation, err)
		}
		change := allocationChange{operation, converted, append([]byte(nil), digest...)}
		allocations = append(allocations, change)
		allocationByOperation[operation] = change
		if converted.SupplementOperation != "" {
			supplements[string(converted.SupplementOperation)] = converted
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return report, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT operations.operation_id,journal.actor_id,operations.canonical_mutation,operations.mutation_digest,operations.command_digest,operations.mutation_encoding_version FROM journal_operations operations JOIN journal USING(journal_id) ORDER BY journal_id`)
	if err != nil {
		return report, err
	}
	var mutations []mutationChange
	for rows.Next() {
		var operation, actor string
		var storedVersion sql.NullString
		var canonical, digest, command []byte
		if err := rows.Scan(&operation, &actor, &canonical, &digest, &command, &storedVersion); err != nil {
			rows.Close()
			return report, err
		}
		if receipt, owned := allocationByOperation[operation]; owned {
			if len(canonical) != 0 || storedVersion.Valid || !bytes.Equal(digest, receipt.oldDigest) || !bytes.Equal(command, []byte(receipt.converted.Command)) {
				rows.Close()
				return report, fmt.Errorf("governed operation %q anchor mutation digest/canonical state disagrees with its owner receipt; fix: restore matching archived receipt and anchor", operation)
			}
			mutations = append(mutations, mutationChange{operation, nil, receipt.converted.Digest[:], command, "", nil})
			continue
		}
		old, err := journal.ConvertRetiredCanonicalMutation(canonical, digest)
		if err != nil {
			rows.Close()
			return report, fmt.Errorf("operation %q preflight: %w", operation, err)
		}
		if !storedVersion.Valid || storedVersion.String != old.EncodingVersion().String() {
			rows.Close()
			return report, fmt.Errorf("operation %q has mismatched canonical encoding column/bytes; no writes performed; fix: restore matching archived encoding version", operation)
		}
		if err := validateRetiredPayloads(ctx, tx, operation, old); err != nil {
			rows.Close()
			return report, err
		}
		converted := old
		if options.RewriteEffect != nil {
			effects := old.NormalizedEffects()
			for i := range effects {
				effects[i], err = options.RewriteEffect(effects[i])
				if err != nil {
					rows.Close()
					return report, err
				}
			}
			converted, err = journal.Canonicalize(journal.OperationInput{Effects: effects, Conditions: old.NormalizedConditions()})
			if err != nil {
				rows.Close()
				return report, err
			}
		}
		newActor, err := journal.ConvertRetiredIdentity(actor)
		if err != nil {
			rows.Close()
			return report, fmt.Errorf("operation %q journal.actor_id %q preflight failed before writes: %w", operation, actor, err)
		}
		if supplement, ok := supplements[operation]; ok {
			if !bytes.Equal(command, supplement.OldSupplementCommandDigest) {
				rows.Close()
				return report, fmt.Errorf("operation %q: old composed command digest disagrees with allocation owner's recipe; no conversion writes; fix: restore the same committed receipt", operation)
			}
			command = supplement.NewSupplementCommandDigest
		} else if options.RewriteCommand != nil {
			command, err = options.RewriteCommand(actor, newActor, command, old, converted)
			if err != nil {
				rows.Close()
				return report, err
			}
		}
		mutations = append(mutations, mutationChange{operation, converted.CanonicalBytes(), converted.DerivedDigest(), command, converted.EncodingVersion().String(), converted.NormalizedEffects()})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		return report, err
	}
	for _, change := range changes {
		query := change.selector.updateSQL()
		args := []any{change.value, change.old}
		if change.kind != "" {
			query = change.selector.updateContextSQL()
			args = append(args, change.kind)
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return report, fmt.Errorf("ID conversion %s.%s key %q collision or storage failure; caller must roll back: %w", change.table, change.column, change.old, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return report, err
		}
		report.ChangedFields += int(count)
	}
	for _, change := range mutations {
		var version any
		if change.version != "" {
			version = change.version
		}
		if _, err := tx.ExecContext(ctx, `UPDATE journal_operations SET canonical_mutation=?,mutation_digest=?,command_digest=?,mutation_encoding_version=? WHERE operation_id=?`, change.bytes, change.digest, change.command, version, change.operation); err != nil {
			return report, err
		}
		if change.effects != nil {
			rows, err := tx.QueryContext(ctx, `SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=(SELECT journal_id FROM journal_operations WHERE operation_id=?) ORDER BY journal_id`, change.operation)
			if err != nil {
				return report, err
			}
			var ids []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return report, err
				}
				ids = append(ids, id)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return report, err
			}
			if len(ids) != len(change.effects) {
				return report, fmt.Errorf("operation %q effect/produced-row count differs; caller must roll back and restore matching archive", change.operation)
			}
			for i, effect := range change.effects {
				kind, err := effect.Sort.JournalKind()
				if err != nil {
					return report, err
				}
				switch kind {
				case journal.JournalKindTaskEvent:
					_, payload, err := canonicalTaskEventPayload(effect)
					if err != nil {
						return report, err
					}
					if _, err := tx.ExecContext(ctx, "UPDATE journal_task_events SET payload=? WHERE journal_id=?", string(payload), ids[i]); err != nil {
						return report, err
					}
				case journal.JournalKindDecision:
					payload := effect.Payload
					if len(payload) == 0 {
						payload = []byte(`{}`)
					}
					if _, err := tx.ExecContext(ctx, "UPDATE journal_decisions SET payload=? WHERE journal_id=?", string(payload), ids[i]); err != nil {
						return report, err
					}
				case journal.JournalKindEvidence:
					payload := effect.Payload
					if len(payload) == 0 {
						payload = []byte(`{}`)
					}
					if _, err := tx.ExecContext(ctx, "UPDATE journal_evidence SET payload=?,content_digest=? WHERE journal_id=?", string(payload), effect.ContentDigest, ids[i]); err != nil {
						return report, err
					}
				}
			}
		}
		report.ChangedMutations++
	}
	for _, change := range allocations {
		if _, err := tx.ExecContext(ctx, `UPDATE governed_allocation_operations SET canonical_request=?,canonical_digest=? WHERE operation_id=?`, change.converted.Canonical, change.converted.Digest[:], change.operation); err != nil {
			return report, err
		}
		report.ChangedAllocations++
	}
	return report, nil
}

type migrationSQLTx struct{ transactionFactReader }

func (adapter migrationSQLTx) Exec(ctx context.Context, query string, args ...any) (fusedtx.Result, error) {
	return adapter.reader.ExecContext(ctx, query, args...)
}

type migrationConnection struct{ *sql.Tx }

func (migrationConnection) Raw(func(any) error) error {
	return fmt.Errorf("migration owner scope cannot access or retire the caller's physical connection; caller must roll back after integrity failure")
}
func (migrationConnection) Close() error {
	return fmt.Errorf("migration owner scope cannot close the caller's transaction; caller retains Commit/Rollback ownership")
}

// VerifyIDConversionInTx checks canonical and allocation integrity through their
// real production owners on the same transaction; it never opens a tracker.
func VerifyIDConversionInTx(ctx context.Context, tx *sql.Tx) error {
	if ctx == nil || tx == nil {
		return fmt.Errorf("VerifyIDConversionInTx: missing caller transaction; no verification performed; fix: pass the active conversion transaction")
	}
	scope := &connScope{ctx: ctx, conn: migrationConnection{tx}, projectionTarget: projectionTargetLive}
	if err := scope.verifyIntegrity(); err != nil {
		return fmt.Errorf("converted whole-journal owner integrity: %w", err)
	}
	if _, err := scope.replayProjections(); err != nil {
		return fmt.Errorf("converted projection owner replay: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT operations.operation_id,operations.canonical_mutation,operations.mutation_digest,operations.mutation_encoding_version,governed.canonical_digest FROM journal_operations operations LEFT JOIN governed_allocation_operations governed ON governed.operation_id=operations.operation_id ORDER BY operations.journal_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var operation string
		var version sql.NullString
		var canonical, digest, allocationDigest []byte
		if err := rows.Scan(&operation, &canonical, &digest, &version, &allocationDigest); err != nil {
			rows.Close()
			return err
		}
		if len(canonical) == 0 && !version.Valid {
			if len(allocationDigest) == 0 || !bytes.Equal(digest, allocationDigest) {
				rows.Close()
				return fmt.Errorf("converted governed operation %q anchor digest disagrees with its owner receipt; marker not published; fix: roll back and restore the complete archive", operation)
			}
			continue
		}
		prepared, err := journal.DecodeCanonicalMutation(canonical)
		if err != nil || !bytes.Equal(prepared.DerivedDigest(), digest) || prepared.EncodingVersion().String() != version.String {
			rows.Close()
			return fmt.Errorf("converted operation %q canonical integrity failed before marker publication; store must roll back; fix: restore matching owner bytes/digest/version: %v", operation, err)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT governed.canonical_request,governed.canonical_digest,COALESCE(operation.authority_journal_id,0) FROM governed_allocation_operations governed JOIN journal_operations operation ON operation.journal_id=governed.anchor_journal_id ORDER BY governed.anchor_journal_id`)
	if err != nil {
		return err
	}
	type receipt struct {
		canonical, digest []byte
		authority         journal.JournalID
	}
	var receipts []receipt
	for rows.Next() {
		var r receipt
		if err := rows.Scan(&r.canonical, &r.digest, &r.authority); err != nil {
			rows.Close()
			return err
		}
		receipts = append(receipts, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	adapter := migrationSQLTx{transactionFactReader{reader: tx}}
	for _, r := range receipts {
		digest := sha256.Sum256(r.canonical)
		if !bytes.Equal(digest[:], r.digest) {
			return fmt.Errorf("converted allocation canonical digest mismatch; when: before marker publication; fix: roll back and restore matching request bytes/digest")
		}
		if composed, decodeErr := allocation.DecodeComposedRequest(r.canonical); decodeErr == nil {
			_, err = allocation.VerifyComposedAllocationReceipt(ctx, adapter, composed, r.authority)
		} else {
			kind, _, _, _, _, decodeErr := allocation.DecodeCanonical(r.canonical)
			if decodeErr != nil {
				return decodeErr
			}
			if kind == allocation.RequestKindGenesis {
				request, decodeErr := allocation.DecodeGenesisRequest(r.canonical)
				if decodeErr != nil {
					return decodeErr
				}
				_, err = allocation.VerifyGenesisReceipt(ctx, adapter, request)
			} else {
				request, decodeErr := allocation.DecodeAllocationRequest(r.canonical)
				if decodeErr != nil {
					return decodeErr
				}
				_, err = allocation.VerifyAllocationReceipt(ctx, adapter, request, r.authority, false)
			}
		}
		if err != nil {
			return err
		}
	}
	reader, err := NewTransactionEvidenceReader(ctx, tx)
	if err != nil {
		return err
	}
	kindRows, err := tx.QueryContext(ctx, "SELECT DISTINCT evidence_kind FROM journal_evidence ORDER BY evidence_kind")
	if err != nil {
		return err
	}
	var kinds []journal.EvidenceKind
	for kindRows.Next() {
		var kind journal.EvidenceKind
		if err := kindRows.Scan(&kind); err != nil {
			kindRows.Close()
			return err
		}
		kinds = append(kinds, kind)
	}
	err = kindRows.Err()
	kindRows.Close()
	if err != nil {
		return err
	}
	var snapshot journal.JournalID
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(journal_id),0) FROM journal").Scan(&snapshot); err != nil {
		return err
	}
	for _, kind := range kinds {
		pageRequest := journal.FactPageRequest{Limit: 256, SnapshotMaxJournalID: snapshot}
		for {
			page, err := reader.QueryEvidence(journal.EvidenceQuery{Kinds: []journal.EvidenceKind{kind}, Page: pageRequest})
			if err != nil {
				return fmt.Errorf("converted evidence integrity before marker publication: %w", err)
			}
			if page.Next == nil {
				break
			}
			pageRequest.AfterJournalID = page.Next.AfterJournalID
		}
	}
	return nil
}

func PublishIDFormatInTx(ctx context.Context, tx *sql.Tx) error {
	inspection, err := InspectIDFormat(ctx, tx)
	if err != nil {
		return err
	}
	if err := VerifyIDConversionInTx(ctx, tx); err != nil {
		return err
	}
	if inspection.Format == IDFormatSlash {
		return nil
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	violation := rows.Next()
	iterationErr := rows.Err()
	rows.Close()
	if iterationErr != nil {
		return iterationErr
	}
	if violation {
		return fmt.Errorf("ID format marker not published: foreign key violations remain; caller must roll back and repair from archive")
	}
	var integrity string
	if err := tx.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("ID format marker not published: SQLite integrity_check returned %q; caller must roll back", integrity)
	}
	if _, err := tx.ExecContext(ctx, idFormatSchema); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO provenance_id_format(singleton,format) VALUES(1,?) ON CONFLICT(singleton) DO NOTHING", string(IDFormatSlash))
	return err
}
