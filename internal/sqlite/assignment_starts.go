package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
)

var _ journal.AssignmentStartQueryAPI = (*DB)(nil)

// Only the ordered union defines population. Diagnostic joins and domain
// filters must never remove damaged candidates before validation.
const assignmentStartCandidatesSQL = `WITH candidate(journal_id) AS (
 SELECT journal_id FROM journal WHERE kind_id=?1 AND journal_id>?2 AND journal_id<=?3
 UNION SELECT journal_id FROM journal_authorities WHERE journal_id>?2 AND journal_id<=?3
 UNION SELECT journal_id FROM journal_authority_assignment_transitions WHERE journal_id>?2 AND journal_id<=?3
) `

const assignmentStartDiagnosticsSQL = `SELECT c.journal_id,
 j.journal_id, j.kind_id, j.recorded_at, j.produced_by_operation_journal_id,
 a.authority_kind_id, b.journal_id, b.label,
 t.assignment_id, t.transition_id, e.assignment_id, e.task_id, e.actor_id,
 e.slot_id, s.name, e.parent_assignment_id, e.predecessor_assignment_id,
 producer.kind_id, op.operation_id, task.id, actor.id,
 (SELECT COUNT(*) FROM (SELECT 1 FROM journal_authority_assignment_transitions markers
   WHERE markers.assignment_id=t.assignment_id AND markers.transition_id=t.transition_id
     AND markers.journal_id<=?3 LIMIT 2))
 FROM candidate c
 LEFT JOIN journal j ON j.journal_id=c.journal_id
 LEFT JOIN journal_authorities a ON a.journal_id=c.journal_id
 LEFT JOIN journal_authority_bootstraps b ON b.journal_id=c.journal_id
 LEFT JOIN journal_authority_assignment_transitions t ON t.journal_id=c.journal_id
 LEFT JOIN journal_authority_assignment_episodes e ON e.assignment_id=t.assignment_id
 LEFT JOIN assignment_slots s ON s.id=e.slot_id
 LEFT JOIN journal producer ON producer.journal_id=j.produced_by_operation_journal_id
 LEFT JOIN journal_operations op ON op.journal_id=producer.journal_id
 LEFT JOIN tasks task ON task.id=e.task_id
 LEFT JOIN agents actor ON actor.id=e.actor_id
 ORDER BY c.journal_id LIMIT ?4`

type assignmentStartDiagnostic struct {
	id                                                              journal.JournalID
	journalID, kind, recordedAt, producer, authorityKind, bootstrap sql.NullInt64
	bootstrapLabel, assignment                                      sql.NullString
	transition                                                      sql.NullInt64
	episode, task, actor                                            sql.NullString
	slot                                                            sql.NullInt64
	slotName, parent, predecessor                                   sql.NullString
	producerKind                                                    sql.NullInt64
	operation, taskExists, actorExists                              sql.NullString
	markerCount                                                     int
}

func assignmentStartIntegrity(id journal.JournalID, problem string) error {
	return fmt.Errorf("%w: %s — where: QueryAssignmentStarts candidate journal %d; when: read-only candidate validation before filters; impact: page rejected, not partial or silently omitted; fix: run VerifyIntegrity and restore the canonical authority/episode/producer rows from a trusted backup", journal.ErrSubtypeIntegrity, problem, id)
}

func loadAssignmentStartDiagnostics(scope *connScope, query string, args ...any) ([]assignmentStartDiagnostic, error) {
	rows, err := scope.conn.QueryContext(scope.ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("QueryAssignmentStarts: read diagnostic page; no page returned, check store schema and availability: %w", err)
	}
	defer rows.Close()
	out := make([]assignmentStartDiagnostic, 0)
	for rows.Next() {
		var d assignmentStartDiagnostic
		if err := rows.Scan(&d.id, &d.journalID, &d.kind, &d.recordedAt, &d.producer, &d.authorityKind, &d.bootstrap, &d.bootstrapLabel, &d.assignment, &d.transition, &d.episode, &d.task, &d.actor, &d.slot, &d.slotName, &d.parent, &d.predecessor, &d.producerKind, &d.operation, &d.taskExists, &d.actorExists, &d.markerCount); err != nil {
			return nil, assignmentStartIntegrity(d.id, "cannot decode diagnostic columns: "+err.Error())
		}
		if len(out) > 0 && out[len(out)-1].id >= d.id {
			return nil, assignmentStartIntegrity(d.id, "duplicate or contradictory subtype/transition rows")
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("QueryAssignmentStarts: iterate diagnostic page; no page returned, check store availability: %w", err)
	}
	return out, nil
}

// decode validates one candidate without following another transition. This is
// also used for the one-level prior-start diagnostic; it cannot recurse.
func (d assignmentStartDiagnostic) decode() (journal.AssignmentStartRow, bool, error) {
	var out journal.AssignmentStartRow
	fail := func(s string) (journal.AssignmentStartRow, bool, error) {
		return out, false, assignmentStartIntegrity(d.id, s)
	}
	if d.id <= 0 || !d.journalID.Valid || d.journalID.Int64 != int64(d.id) || !d.kind.Valid || journal.JournalKind(d.kind.Int64) != journal.JournalKindAuthority {
		return fail("missing or wrong journal supertype/discriminator")
	}
	if !d.authorityKind.Valid {
		return fail("missing authority subtype")
	}
	if !d.recordedAt.Valid {
		return fail("missing recorded timestamp")
	}
	if !d.producer.Valid || d.producer.Int64 <= 0 || d.producer.Int64 >= int64(d.id) || !d.producerKind.Valid || journal.JournalKind(d.producerKind.Int64) != journal.JournalKindOperation || !d.operation.Valid || journal.ValidateOperationID(journal.OperationID(d.operation.String)) != nil {
		return fail("missing or malformed producing operation identity or wrong producer discriminator")
	}
	switch journal.AuthorityKind(d.authorityKind.Int64) {
	case journal.AuthorityKindBootstrap:
		if !d.bootstrap.Valid || !d.bootstrapLabel.Valid || d.assignment.Valid || d.transition.Valid {
			return fail("bootstrap detail missing or conflicts with assignment transition")
		}
		return out, false, nil
	case journal.AuthorityKindAssignment:
		if d.bootstrap.Valid || !d.assignment.Valid || !d.transition.Valid {
			return fail("assignment transition missing or conflicts with bootstrap detail")
		}
	default:
		return fail("unknown authority subtype")
	}
	if d.transition.Int64 != int64(journal.TransitionStarted) && d.transition.Int64 != int64(journal.TransitionEnded) {
		return fail("unknown assignment transition")
	}
	if d.markerCount != 1 {
		return fail("duplicate or contradictory assignment markers")
	}
	if !d.episode.Valid || d.episode.String != d.assignment.String || journal.ValidateOperationID(journal.OperationID(d.assignment.String)) != nil {
		return fail("missing episode or malformed assignment ID")
	}
	if !d.task.Valid || !d.taskExists.Valid || !d.actor.Valid || !d.actorExists.Valid {
		return fail("missing task or occupant actor")
	}
	task, err := journalParseTask(d.task.String)
	if err != nil {
		return fail("malformed task ID: " + err.Error())
	}
	if _, err := journal.TaskContext(task); err != nil {
		return fail("malformed task ID: " + err.Error())
	}
	actor, err := journalParseActor(d.actor.String)
	if err != nil {
		return fail("malformed occupant actor ID: " + err.Error())
	}
	if _, err := journal.ActorContext(actor); err != nil {
		return fail("malformed occupant actor ID: " + err.Error())
	}
	if !d.slot.Valid || d.slot.Int64 != slotOwnerResponsibilityID || !d.slotName.Valid || d.slotName.String != string(journal.SlotOwnerResponsibility) {
		return fail("invalid DB assignment slot or missing/mismatched slot lookup")
	}
	out = journal.AssignmentStartRow{AuthorityJournalID: d.id, RecordedAt: time.Unix(0, d.recordedAt.Int64).UTC(), AssignmentID: journal.AssignmentID(d.assignment.String), TaskID: task, SlotID: journal.SlotOwnerResponsibility, Occupant: actor, ProducingOperationID: journal.OperationID(d.operation.String), ProducingOperationJournalID: journal.JournalID(d.producer.Int64)}
	if d.parent.Valid {
		if journal.ValidateOperationID(journal.OperationID(d.parent.String)) != nil {
			return fail("malformed parent assignment ID")
		}
		id := journal.AssignmentID(d.parent.String)
		out.ParentAssignmentID = &id
	}
	if d.predecessor.Valid {
		if journal.ValidateOperationID(journal.OperationID(d.predecessor.String)) != nil {
			return fail("malformed predecessor assignment ID")
		}
		id := journal.AssignmentID(d.predecessor.String)
		out.PredecessorAssignmentID = &id
	}
	return out, d.transition.Int64 == int64(journal.TransitionStarted), nil
}

func (scope *connScope) validateAssignmentEnd(d assignmentStartDiagnostic, snapshot journal.JournalID) error {
	// At most one earlier start under the schema's UNIQUE(assignment,transition).
	// LIMIT 2 also rejects duplicate starts if that constraint was damaged. The
	// same diagnostic joins validate it; no recursive history walk is possible.
	const prior = `WITH candidate(journal_id) AS (
 SELECT journal_id FROM journal_authority_assignment_transitions
 WHERE assignment_id=?1 AND journal_id<?2 AND journal_id<=?3 AND transition_id=0
 ORDER BY journal_id LIMIT 2) `
	rows, err := loadAssignmentStartDiagnostics(scope, prior+assignmentStartDiagnosticsSQL, d.assignment.String, int64(d.id), int64(snapshot), 2)
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return assignmentStartIntegrity(d.id, "end-with-no-prior-start or duplicate prior starts")
	}
	start, isStart, err := rows[0].decode()
	if err != nil {
		return err
	}
	if !isStart || start.AssignmentID != journal.AssignmentID(d.assignment.String) || start.AuthorityJournalID >= d.id {
		return assignmentStartIntegrity(d.id, "end has no valid strictly earlier started transition")
	}
	return nil
}

func assignmentFilterSet[T comparable](values []T) map[T]struct{} {
	out := make(map[T]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}

func assignmentFilterMatches[T comparable](values map[T]struct{}, v T) bool {
	if len(values) == 0 {
		return true
	}
	_, ok := values[v]
	return ok
}

func (db *DB) QueryAssignmentStarts(q journal.AssignmentStartQuery) (journal.AssignmentStartPage, error) {
	if err := q.Validate(); err != nil {
		return journal.AssignmentStartPage{}, err
	}
	// Maps normalize and deduplicate without mutating caller slices.
	tasks, assignments := assignmentFilterSet(q.TaskIDs), assignmentFilterSet(q.AssignmentIDs)
	actors, operations := assignmentFilterSet(q.ActorIDs), assignmentFilterSet(q.OperationIDs)
	slots := make(map[int64]struct{}, len(q.SlotIDs))
	for _, slot := range q.SlotIDs {
		id, err := slotDBID(slot) // Validate already rejects the writer's empty-slot shorthand.
		if err != nil {
			return journal.AssignmentStartPage{}, fmt.Errorf("%w: QueryAssignmentStarts slot mapping before SQL: %v; no page returned; fix: use a registered slot", journal.ErrInvalidQuery, err)
		}
		slots[int64(id)] = struct{}{}
	}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return journal.AssignmentStartPage{}, fmt.Errorf("QueryAssignmentStarts: lease read connection; reopen a live store before retrying: %w", err)
	}
	defer scope.release()
	page := journal.AssignmentStartPage{Rows: make([]journal.AssignmentStartRow, 0), SnapshotPinned: true}
	err = runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		if !q.Page.SnapshotPinned {
			if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COALESCE(MAX(journal_id),0) FROM journal").Scan(&q.Page.SnapshotMaxJournalID); err != nil {
				return fmt.Errorf("QueryAssignmentStarts: resolve fresh snapshot; check store availability: %w", err)
			}
		} else if q.Page.SnapshotMaxJournalID > 0 {
			var boundary int64
			err := scope.conn.QueryRowContext(scope.ctx, "SELECT journal_id FROM journal WHERE journal_id=?1", int64(q.Page.SnapshotMaxJournalID)).Scan(&boundary)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: QueryAssignmentStarts snapshot boundary does not exist; before page SQL, no page returned; fix: replay a returned pinned boundary or start a fresh scan", journal.ErrInvalidQuery)
			}
			if err != nil {
				return fmt.Errorf("QueryAssignmentStarts: validate snapshot boundary; check store availability: %w", err)
			}
		}
		page.SnapshotMaxJournalID = q.Page.SnapshotMaxJournalID
		if q.Page.AfterJournalID > page.SnapshotMaxJournalID {
			return fmt.Errorf("%w: QueryAssignmentStarts cursor exceeds resolved snapshot; before page SQL, no page returned; fix: restart from a valid committed high-water mark", journal.ErrInvalidQuery)
		}
		rows, err := loadAssignmentStartDiagnostics(scope, assignmentStartCandidatesSQL+assignmentStartDiagnosticsSQL, int(journal.JournalKindAuthority), int64(q.Page.AfterJournalID), int64(page.SnapshotMaxJournalID), q.Page.Limit+1)
		if err != nil {
			return err
		}
		for i, d := range rows {
			r, start, err := d.decode()
			if err != nil {
				return err
			}
			if d.transition.Valid && d.transition.Int64 == int64(journal.TransitionEnded) {
				if err := scope.validateAssignmentEnd(d, page.SnapshotMaxJournalID); err != nil {
					return err
				}
			}
			if i < q.Page.Limit && start && assignmentFilterMatches(tasks, r.TaskID) && assignmentFilterMatches(assignments, r.AssignmentID) && assignmentFilterMatches(actors, r.Occupant) && assignmentFilterMatches(operations, r.ProducingOperationID) && assignmentFilterMatches(slots, d.slot.Int64) {
				page.Rows = append(page.Rows, r)
			}
		}
		if len(rows) > q.Page.Limit {
			page.Next = &journal.AssignmentStartCursor{SnapshotMaxJournalID: page.SnapshotMaxJournalID, SnapshotPinned: true, AfterJournalID: rows[q.Page.Limit-1].id}
		}
		return nil
	})
	if err != nil {
		return journal.AssignmentStartPage{}, err
	}
	return page, nil
}
