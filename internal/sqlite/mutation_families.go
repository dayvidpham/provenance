package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// mutation_families.go implements the reducer half of the journaled
// relationship/annotation mutation families (docs/journal-relational-contract.md §6):
// edge-add/edge-remove/label-add/label-remove/comment-add. Each family is
// journaled ON a journal_task_events row carrying its fixed per-family EventKind and the
// operands in its payload, so who added/removed the relationship, under which authority,
// at which journal position is derivable from the journal (who-provenance).
//
// The split mirrors the rest of the reducer: the FOLD (Apply-only) authorizes the effect
// against the operation's authority (§9.3, exactly like a task_event), cycle-checks a
// blocked_by edge-add, and writes the journal row + payload — it does NOT touch the
// domain tables. The PROJECTION step (projectMutationFamilyRow, the single shared
// reducer step Apply and Open both run, §9.2) reads the journaled row and folds it into
// the edges/labels/comments domain projection, targeting the real tables during a live
// Apply and the shadow tables during a from-empty replay derivation (§15). Because every
// edge/label/comment now flows through the journal, the domain tables are re-derivable
// from ordered history and the §15 convergence check covers them.

// ---------------------------------------------------------------------------
// Fold (Apply-only): authorize + cycle-check + journal the row
// ---------------------------------------------------------------------------

// foldMutationFamily journals one relationship/annotation mutation family row. The
// supertype journal row is already inserted by foldEffect (kind task_event); this
// step authorizes the effect against the operation's authority at its own JournalID
// (§9.3), validates a blocked_by edge-add for cycles, and writes the journal_task_events
// subtype row with the fixed per-family kind and the operand payload. The domain write is
// deferred to the shared projection step so it is reproducible from history.
func (scope *connScope) foldMutationFamily(in journal.OperationInput, jid int64, eff journal.Effect) error {
	if eff.Sort == journal.EffectEdgeAdd {
		recordedAt := in.RecordedAt
		if eff.RecordedAtOverride != nil {
			recordedAt = *eff.RecordedAtOverride
		}
		return foldV1EdgeAdd(scope.ctx, allocationSQLTx{conn: scope.conn}, in, jid, recordedAt, eff)
	}
	if eff.TaskID.Namespace == "" {
		return fmt.Errorf(
			"provenance: operation %q %s effect has an empty subject task id — where: mutation-family fold "+
				"(§6); when: before commit; impact: nothing committed; fix: supply the source/subject TaskID",
			in.OperationID, eff.Sort)
	}
	// Same per-effect authorization discipline as a task_event (§9.3): the committing
	// authority must govern the subject/source task at this effect's own JournalID.
	if err := scope.requireAuthorityGoverns(in, jid, eff.TaskID); err != nil {
		return err
	}
	kind, ok := journal.MutationFamilyKindForSort(eff.Sort)
	if !ok {
		return fmt.Errorf("provenance: operation %q effect sort %s is not a mutation family", in.OperationID, eff.Sort)
	}
	payload, err := encodeMutationFamilyPayload(eff)
	if err != nil {
		return fmt.Errorf("Apply: operation %q: %w", in.OperationID, err)
	}
	// Cycle detection for a blocked_by edge-add is enforced in the fold, before the row
	// commits (§6): the graph store reads the same edges table this family projects into,
	// so a cycle-free journal keeps the graph acyclic. Replay never re-validates — it only
	// reconstructs — so the check lives here, not in the projection step.
	if eff.Sort == journal.EffectEdgeAdd && eff.EdgeRelKind == ptypes.EdgeBlockedBy {
		creates, cerr := scope.edgeCreatesCycle(eff.TaskID.String(), eff.EdgeTargetID)
		if cerr != nil {
			return cerr
		}
		if creates {
			return fmt.Errorf(
				"%w: adding a blocked-by edge from %q to %q would create a cycle — where: edge-add fold "+
					"(§6); when: before commit; impact: nothing committed; the target must be work that "+
					"finishes BEFORE the source; fix: inspect the dependency graph (DepTree/Ancestors) and "+
					"cite a target that does not already depend on the source",
				ptypes.ErrCycleDetected, eff.TaskID.String(), eff.EdgeTargetID)
		}
	}
	if _, err := scope.conn.ExecContext(scope.ctx, insertJournalTaskEventSQL, jid, eff.TaskID.String(), string(kind), string(payload)); err != nil {
		return fmt.Errorf("Apply: insert journal_task_events (%s): %w", kind, err)
	}
	contexts, err := journal.CanonicalEventContexts(eff.Contexts)
	if err != nil {
		return fmt.Errorf("Apply: canonical contexts (%s): %w", kind, err)
	}
	for _, context := range contexts {
		contextKind, identity, err := journal.EncodeStoredEventContext(context)
		if err != nil {
			return fmt.Errorf("Apply: encode context (%s): %w", kind, err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_task_event_contexts (event_journal_id, context_kind, context_identity, attached_by_journal_id) VALUES (?1, ?2, ?3, ?4)", jid, string(contextKind), identity, jid); err != nil {
			return fmt.Errorf("Apply: insert context (%s): %w", kind, err)
		}
	}
	return nil
}

func encodeMutationFamilyPayload(eff journal.Effect) ([]byte, error) {
	switch eff.Sort {
	case journal.EffectEdgeAdd, journal.EffectEdgeRemove:
		return journal.EncodeEdgeMutationPayload(eff.EdgeTargetID, eff.EdgeRelKind)
	case journal.EffectLabelAdd, journal.EffectLabelRemove:
		return journal.EncodeLabelMutationPayload(eff.Label)
	case journal.EffectCommentAdd:
		return journal.EncodeCommentMutationPayload(
			eff.CommentIdentity.String(), eff.CommentAuthor.String(), eff.CommentBody)
	default:
		return nil, fmt.Errorf("provenance: effect sort %s is not a mutation family", eff.Sort)
	}
}

// edgeCreatesCycle reports whether adding a blocked_by edge source→target would
// create a cycle in the blocked_by subgraph: it does iff source is already reachable FROM
// target through blocked_by edges (a self-edge source==target is the degenerate cycle).
// The bounded recursive walk reads the projection edges table (real during Apply), the
// same table the graph store reads, so journal and graph stay consistent (§6).
func (scope *connScope) edgeCreatesCycle(source, target string) (bool, error) {
	var found int
	err := scope.conn.QueryRowContext(scope.ctx, scope.projectionTarget.edgeCycleQuery(), target, source, int(ptypes.EdgeBlockedBy), 1, 1).Scan(&found)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("edge cycle check %s->%s: %w", source, target, err)
	}
	return err == nil, nil
}

// ---------------------------------------------------------------------------
// Projection (shared by Apply and Open replay): fold the row into the domain table
// ---------------------------------------------------------------------------

// projectMutationFamilyRow folds one journaled relationship/annotation row into the
// edges/labels/comments domain projection (§6, §15). It targets the real base table during
// a live Apply and the shadow table during a from-empty replay derivation, so the domain
// tables are re-derivable solely from ordered journal history and the §15 convergence
// check covers them. The committing-actor attribution and watermark advance are handled by
// the caller (projectTaskEventRow) around this step, exactly as for any task_event.
func (scope *connScope) projectMutationFamilyRow(task journal.TaskID, kind journal.EventKind, payload []byte, jid, recordedAt int64) error {
	switch kind {
	case journal.EventKindEdgeAdded:
		p, err := journal.DecodeEdgeMutationPayload(payload)
		if err != nil {
			return err
		}
		if scope.projectionTarget == projectionTargetLive {
			if err := v1ProjectEdgeAdd(scope.ctx, allocationSQLTx{conn: scope.conn}, task, p.Target, p.EdgeKind, recordedAt); err != nil {
				return err
			}
		} else if _, err := scope.conn.ExecContext(scope.ctx, scope.projectionTarget.projectEdgeAddQuery(), task.String(), p.Target, int(p.EdgeKind), recordedAt); err != nil {
			return fmt.Errorf("project edge-add %s->%s: %w", task, p.Target, err)
		}
	case journal.EventKindEdgeRemoved:
		p, err := journal.DecodeEdgeMutationPayload(payload)
		if err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, scope.projectionTarget.projectEdgeRemoveQuery(), task.String(), p.Target, int(p.EdgeKind)); err != nil {
			return fmt.Errorf("project edge-remove %s->%s: %w", task, p.Target, err)
		}
	case journal.EventKindLabelAdded:
		p, err := journal.DecodeLabelMutationPayload(payload)
		if err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, scope.projectionTarget.projectLabelAddQuery(), task.String(), p.Label); err != nil {
			return fmt.Errorf("project label-add %s %q: %w", task, p.Label, err)
		}
	case journal.EventKindLabelRemoved:
		p, err := journal.DecodeLabelMutationPayload(payload)
		if err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, scope.projectionTarget.projectLabelRemoveQuery(), task.String(), p.Label); err != nil {
			return fmt.Errorf("project label-remove %s %q: %w", task, p.Label, err)
		}
	case journal.EventKindCommentAdded:
		p, err := journal.DecodeCommentMutationPayload(payload)
		if err != nil {
			return err
		}
		// INSERT OR IGNORE keeps a from-empty replay of the SAME journaled comment
		// idempotent (the caller-minted id is carried in the payload, §6), so the
		// projection reproduces exactly one row.
		if _, err := scope.conn.ExecContext(scope.ctx, scope.projectionTarget.projectCommentAddQuery(), p.CommentID, task.String(), p.Author, p.Body, recordedAt); err != nil {
			return fmt.Errorf("project comment-add %s %q: %w", task, p.CommentID, err)
		}
	default:
		return fmt.Errorf("provenance: %q is not a journaled mutation-family kind (§6)", kind)
	}
	return scope.advanceWatermark(task, jid)
}

func (target projectionTarget) edgeCycleQuery() string {
	switch target {
	case projectionTargetLive:
		return "WITH RECURSIVE reach(node) AS (SELECT ?1 UNION SELECT e.target_id FROM edges e JOIN reach r ON e.source_id=r.node WHERE e.kind_id=?3) SELECT ?4 FROM reach WHERE node=?2 LIMIT ?5"
	case projectionTargetShadow:
		return "WITH RECURSIVE reach(node) AS (SELECT ?1 UNION SELECT e.target_id FROM shadow_edges e JOIN reach r ON e.source_id=r.node WHERE e.kind_id=?3) SELECT ?4 FROM reach WHERE node=?2 LIMIT ?5"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) projectEdgeAddQuery() string {
	switch target {
	case projectionTargetLive:
		return "INSERT OR IGNORE INTO edges (source_id,target_id,kind_id,created_at) VALUES (?1,?2,?3,?4)"
	case projectionTargetShadow:
		return "INSERT OR IGNORE INTO shadow_edges (source_id,target_id,kind_id,created_at) VALUES (?1,?2,?3,?4)"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) projectEdgeRemoveQuery() string {
	switch target {
	case projectionTargetLive:
		return "DELETE FROM edges WHERE source_id=?1 AND target_id=?2 AND kind_id=?3"
	case projectionTargetShadow:
		return "DELETE FROM shadow_edges WHERE source_id=?1 AND target_id=?2 AND kind_id=?3"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) projectLabelAddQuery() string {
	switch target {
	case projectionTargetLive:
		return "INSERT OR IGNORE INTO labels (task_id,name) VALUES (?1,?2)"
	case projectionTargetShadow:
		return "INSERT OR IGNORE INTO shadow_labels (task_id,name) VALUES (?1,?2)"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) projectLabelRemoveQuery() string {
	switch target {
	case projectionTargetLive:
		return "DELETE FROM labels WHERE task_id=?1 AND name=?2"
	case projectionTargetShadow:
		return "DELETE FROM shadow_labels WHERE task_id=?1 AND name=?2"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) projectCommentAddQuery() string {
	switch target {
	case projectionTargetLive:
		return "INSERT OR IGNORE INTO comments (id,task_id,author_id,body,created_at) VALUES (?1,?2,?3,?4,?5)"
	case projectionTargetShadow:
		return "INSERT OR IGNORE INTO shadow_comments (id,task_id,author_id,body,created_at) VALUES (?1,?2,?3,?4,?5)"
	default:
		panic("unknown projection target")
	}
}
