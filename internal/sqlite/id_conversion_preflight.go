package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// validateRetiredPayloads checks immutable subtype bytes before conversion can
// replace them. Otherwise replay would see freshly regenerated good bytes and
// conceal a corrupted historical payload or evidence digest.
func validateRetiredPayloads(ctx context.Context, tx *sql.Tx, operation string, mutation journal.CanonicalMutation) error {
	rows, err := tx.QueryContext(ctx, `SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=(SELECT journal_id FROM journal_operations WHERE operation_id=?) ORDER BY journal_id`, operation)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	effects := mutation.NormalizedEffects()
	if len(ids) != len(effects) {
		return fmt.Errorf("retired operation %q effect/row count disagrees before conversion; no writes performed; fix: restore matching archived operation graph", operation)
	}
	for i, effect := range effects {
		kind, err := effect.Sort.JournalKind()
		if err != nil {
			return err
		}
		var expected, stored []byte
		switch kind {
		case journal.JournalKindTaskEvent:
			_, expected, err = canonicalTaskEventPayload(effect)
			switch effect.Sort {
			case journal.EffectEdgeAdd, journal.EffectEdgeRemove:
				id, parseErr := ptypes.ParseTaskID(effect.EdgeTargetID)
				if parseErr != nil {
					return parseErr
				}
				expected, err = journal.EncodeEdgeMutationPayload(id.Namespace+"--"+id.UUID.String(), effect.EdgeRelKind)
			case journal.EffectCommentAdd:
				expected, err = journal.EncodeCommentMutationPayload(effect.CommentIdentity.Namespace+"--"+effect.CommentIdentity.UUID.String(), effect.CommentAuthor.Namespace+"--"+effect.CommentAuthor.UUID.String(), effect.CommentBody)
			}
			if err != nil {
				return err
			}
			err = tx.QueryRowContext(ctx, "SELECT payload FROM journal_task_events WHERE journal_id=?", ids[i]).Scan(&stored)
		case journal.JournalKindDecision:
			expected = effect.Payload
			if len(expected) == 0 {
				expected = []byte(`{}`)
			}
			err = tx.QueryRowContext(ctx, "SELECT payload FROM journal_decisions WHERE journal_id=?", ids[i]).Scan(&stored)
		case journal.JournalKindEvidence:
			expected = effect.Payload
			if len(expected) == 0 {
				expected = []byte(`{}`)
			}
			var digest []byte
			err = tx.QueryRowContext(ctx, "SELECT payload,content_digest FROM journal_evidence WHERE journal_id=?", ids[i]).Scan(&stored, &digest)
			if err == nil && !bytes.Equal(digest, effect.ContentDigest) {
				return fmt.Errorf("retired operation %q evidence row %d content digest disagrees with canonical owner before writes; fix: restore matching evidence/canonical bytes from archive", operation, ids[i])
			}
		default:
			continue
		}
		if err != nil {
			return fmt.Errorf("retired operation %q subtype row %d unavailable before writes; fix: restore complete archived graph: %w", operation, ids[i], err)
		}
		if !bytes.Equal(expected, stored) {
			return fmt.Errorf("retired operation %q subtype row %d payload disagrees with canonical owner before writes; fix: restore matching immutable payload/canonical bytes from archive", operation, ids[i])
		}
	}
	return nil
}
