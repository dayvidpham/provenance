package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// actorOwnershipTestHooks are per-DB, no-op-by-default test seams. Production
// never installs them; package tests use them to hold the read at the exact
// statement boundaries that prove snapshot and cancellation behavior.
type actorOwnershipTestHooks struct {
	stageBarrier func(stage int)
}

func (db *DB) installActorOwnershipStageBarrier(barrier func(stage int)) {
	db.actorOwnershipHooks.stageBarrier = barrier
}

var _ journal.ActorOwnershipQueryAPI = (*DB)(nil)

const actorOwnershipOwnedTasksSQL = `SELECT
 length(CAST(t.id AS BLOB)),
 CASE WHEN length(CAST(t.id AS BLOB))<=?2 THEN t.id END,
 t.phase_id,
 length(CAST(e.assignment_id AS BLOB)),
 CASE WHEN length(CAST(e.assignment_id AS BLOB))<=?2 THEN e.assignment_id END,
 e.actor_id,
 started.journal_id,
 op.journal_id,
 length(CAST(op.operation_id AS BLOB)),
 CASE WHEN length(CAST(op.operation_id AS BLOB))<=?2 THEN op.operation_id END,
 length(CAST(e.predecessor_assignment_id AS BLOB)),
 CASE WHEN length(CAST(e.predecessor_assignment_id AS BLOB))<=?2 THEN e.predecessor_assignment_id END
 FROM tasks t
 LEFT JOIN journal_authority_assignment_episodes e ON e.assignment_id=(
   SELECT winner.assignment_id
   FROM journal_authority_assignment_episodes winner
   JOIN journal_authority_assignment_transitions winner_started
     ON winner_started.assignment_id=winner.assignment_id AND winner_started.transition_id=?3
   WHERE winner.task_id=t.id AND winner.slot_id=?4
     AND NOT EXISTS (
       SELECT 1 FROM journal_authority_assignment_transitions winner_ended
       WHERE winner_ended.assignment_id=winner.assignment_id AND winner_ended.transition_id=?5)
   ORDER BY winner_started.journal_id DESC LIMIT 1)
 LEFT JOIN journal_authority_assignment_transitions started
   ON started.assignment_id=e.assignment_id AND started.transition_id=?3
 LEFT JOIN journal produced ON produced.journal_id=started.journal_id
 LEFT JOIN journal_operations op ON op.journal_id=produced.produced_by_operation_journal_id
 WHERE t.owner_id=?1
 ORDER BY started.journal_id ASC,t.id ASC`

const actorOwnershipProducerRowsCTE = `WITH owned(task_id,start_producer,supplement_operation) AS (
 SELECT json_each.value->>'task_id',
        json_each.value->>'start_producer',
        json_each.value->>'supplement_operation'
 FROM json_each(?1)
), producers(task_id,producer_journal_id) AS (
 SELECT task_id,CAST(start_producer AS INTEGER) FROM owned
 UNION
 SELECT owned.task_id,supplement.journal_id
 FROM owned JOIN journal_operations supplement
   ON supplement.operation_id=owned.supplement_operation
) `

const actorOwnershipMaterialsSQL = actorOwnershipProducerRowsCTE + `SELECT
 te.journal_id,producers.task_id,
 length(CAST(te.event_kind AS BLOB)),
 CASE WHEN length(CAST(te.event_kind AS BLOB))<=?2 THEN te.event_kind END,
 length(CAST(te.payload AS BLOB)),
 CASE WHEN length(CAST(te.payload AS BLOB))<=?2 THEN te.payload END
 FROM journal_task_events te
 JOIN journal produced ON produced.journal_id=te.journal_id
 JOIN producers ON producers.task_id=te.task_id
  AND producers.producer_journal_id=produced.produced_by_operation_journal_id
 WHERE te.event_kind IN (SELECT value FROM json_each(?3))
 ORDER BY te.journal_id ASC`

const actorOwnershipEvidenceSQL = actorOwnershipProducerRowsCTE + `SELECT
 e.journal_id,producers.task_id,
 length(CAST(e.evidence_kind AS BLOB)),
 CASE WHEN length(CAST(e.evidence_kind AS BLOB))<=?2 THEN e.evidence_kind END,
 length(CAST(e.payload AS BLOB)),
 CASE WHEN length(CAST(e.payload AS BLOB))<=?2 THEN e.payload END
 FROM journal_evidence e
 JOIN journal produced ON produced.journal_id=e.journal_id
 JOIN producers ON producers.producer_journal_id=produced.produced_by_operation_journal_id
 WHERE e.evidence_kind IN (SELECT value FROM json_each(?3))
 ORDER BY e.journal_id ASC`

type actorOwnershipOwnedBinding struct {
	TaskID              string              `json:"task_id"`
	StartProducer       int64               `json:"start_producer"`
	SupplementOperation journal.OperationID `json:"supplement_operation"`
}

type actorOwnershipOwnedResult struct {
	tasks    []journal.OwnedTaskRow
	bindings []actorOwnershipOwnedBinding
}

// QueryActorOwnership reads one actor's live ownership projection and the
// material/evidence produced by each winning assignment operation in one
// snapshot transaction.
func (db *DB) QueryActorOwnership(ctx context.Context, query journal.ActorOwnershipQuery) (journal.ActorOwnershipSnapshot, error) {
	if ctx == nil {
		return journal.ActorOwnershipSnapshot{}, fmt.Errorf("%w: QueryActorOwnership input is invalid because context is nil. Why: a store read requires cancellation and deadline propagation; where: QueryActorOwnership input; when: before leasing a store connection; impact: no read started; fix: pass a non-nil context", journal.ErrInvalidQuery)
	}
	normalized, err := normalizeActorOwnershipQuery(query)
	if err != nil {
		return journal.ActorOwnershipSnapshot{}, err
	}
	snapshot, err := db.queryActorOwnership(ctx, normalized, journal.MaxActorOwnershipResultBytes)
	if err != nil {
		return journal.ActorOwnershipSnapshot{}, err
	}
	return snapshot, nil
}

func normalizeActorOwnershipQuery(query journal.ActorOwnershipQuery) (journal.ActorOwnershipQuery, error) {
	if err := query.Validate(); err != nil {
		return journal.ActorOwnershipQuery{}, err
	}
	query.MaterialKinds = deduplicateActorOwnershipEventKinds(query.MaterialKinds)
	query.EvidenceKinds = deduplicateActorOwnershipEvidenceKinds(query.EvidenceKinds)
	return query, nil
}

func deduplicateActorOwnershipEventKinds(values []journal.EventKind) []journal.EventKind {
	if len(values) == 0 {
		return nil
	}
	out := make([]journal.EventKind, 0, len(values))
	seen := make(map[journal.EventKind]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func deduplicateActorOwnershipEvidenceKinds(values []journal.EvidenceKind) []journal.EvidenceKind {
	if len(values) == 0 {
		return nil
	}
	out := make([]journal.EvidenceKind, 0, len(values))
	seen := make(map[journal.EvidenceKind]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (db *DB) queryActorOwnership(ctx context.Context, query journal.ActorOwnershipQuery, limit int64) (journal.ActorOwnershipSnapshot, error) {
	scope, err := db.bindScope(ctx, projectionTargetLive)
	if err != nil {
		return journal.ActorOwnershipSnapshot{}, actorOwnershipReadFault("could not lease a live read connection", "the tracker is closed, still opening, or has no available connection", "connection lease before BEGIN", err)
	}
	defer scope.release()

	snapshot := journal.ActorOwnershipSnapshot{Tasks: make([]journal.OwnedTaskRow, 0)}
	err = runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COALESCE(MAX(journal_id),0) FROM journal").Scan(&snapshot.Through); err != nil {
			return actorOwnershipReadFault("could not read the journal high-water mark", "the store is unavailable or its journal relation is damaged", "statement 1 inside the read transaction", err)
		}
		db.fireActorOwnershipStage(1)

		var known int
		err := scope.conn.QueryRowContext(scope.ctx, "SELECT 1 FROM agents WHERE id=?1", query.Actor.String()).Scan(&known)
		if errors.Is(err, sql.ErrNoRows) {
			snapshot.ActorKnown = false
		} else if err != nil {
			return actorOwnershipReadFault("could not read actor registration", "the store is unavailable or its agents relation is damaged", "statement 2 inside the read transaction", err)
		} else {
			snapshot.ActorKnown = true
		}
		db.fireActorOwnershipStage(2)

		owned, err := db.readActorOwnershipTasks(scope, query.Actor, limit, &snapshot)
		if err != nil {
			return err
		}
		db.fireActorOwnershipStage(3)

		if len(owned.tasks) == 0 {
			return nil
		}
		if len(query.MaterialKinds) != 0 {
			if err := db.readActorOwnershipMaterials(scope, query.Actor, owned.bindings, query.MaterialKinds, limit, &snapshot); err != nil {
				return err
			}
		}
		if len(query.EvidenceKinds) != 0 {
			if err := db.readActorOwnershipEvidence(scope, query.Actor, owned.bindings, query.EvidenceKinds, limit, &snapshot); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return journal.ActorOwnershipSnapshot{}, err
	}
	return snapshot, nil
}

func (db *DB) fireActorOwnershipStage(stage int) {
	if barrier := db.actorOwnershipHooks.stageBarrier; barrier != nil {
		barrier(stage)
	}
}

func (db *DB) readActorOwnershipTasks(scope *connScope, actor journal.ActorID, limit int64, snapshot *journal.ActorOwnershipSnapshot) (actorOwnershipOwnedResult, error) {
	rows, err := scope.conn.QueryContext(scope.ctx, actorOwnershipOwnedTasksSQL,
		actor.String(), limit-snapshot.Work.ResultBytes, transitionStartedID, slotOwnerResponsibilityID, transitionEndedID)
	if err != nil {
		return actorOwnershipOwnedResult{}, actorOwnershipReadFault("could not read owned tasks and their winning episodes", "the store is unavailable or its ownership topology is damaged", "statement 3 inside the read transaction", err)
	}
	defer rows.Close()

	owned := actorOwnershipOwnedResult{tasks: make([]journal.OwnedTaskRow, 0), bindings: make([]actorOwnershipOwnedBinding, 0)}
	for rows.Next() {
		var (
			taskLength, assignmentLength, operationLength, predecessorLength sql.NullInt64
			taskID, assignmentID, occupant, operationID, predecessorID       sql.NullString
			phase, started, producer                                         sql.NullInt64
		)
		if err := rows.Scan(&taskLength, &taskID, &phase, &assignmentLength, &assignmentID, &occupant, &started, &producer, &operationLength, &operationID, &predecessorLength, &predecessorID); err != nil {
			return actorOwnershipOwnedResult{}, actorOwnershipReadFault("could not decode an owned-task row", "SQLite returned an unexpected column type or shape", "statement 3 row decoding", err)
		}

		// A task ID is always part of the result, so resolve it before constructing
		// either a projection mismatch or a limit diagnostic.
		if !taskID.Valid {
			return actorOwnershipOwnedResult{}, actorOwnershipLimit(actor, journal.ActorOwnershipStageOwnedTasks, limit, snapshot.Work.ResultBytes+actorOwnershipLength(taskLength), snapshot.Work)
		}
		task, err := journalParseTask(taskID.String)
		if err != nil {
			return actorOwnershipOwnedResult{}, actorOwnershipIntegrity(journal.ActorOwnershipStageOwnedTasks, fmt.Sprintf("owned task %q has a malformed stored task ID", taskID.String), "restore the task identity and its episode rows from the same committed backup", err)
		}
		if !occupant.Valid {
			return actorOwnershipOwnedResult{}, &journal.OwnerProjectionMismatchError{Task: task, Owner: actor}
		}
		active, err := journalParseActor(occupant.String)
		if err != nil {
			return actorOwnershipOwnedResult{}, actorOwnershipIntegrity(journal.ActorOwnershipStageOwnedTasks, fmt.Sprintf("owned task %s has malformed active occupant %q", task, occupant.String), "restore the episode occupant from the same committed backup", err)
		}
		if active != actor {
			return actorOwnershipOwnedResult{}, &journal.OwnerProjectionMismatchError{Task: task, Owner: actor, ActiveOccupant: &active}
		}

		rowBytes := actorOwnershipLength(taskLength) + actorOwnershipLength(assignmentLength) + actorOwnershipLength(operationLength) + actorOwnershipLength(predecessorLength)
		observed := snapshot.Work.ResultBytes + rowBytes
		// A wire-suppressed value has no valid scan destination, and its declared
		// length is already part of observed, so the running total refuses the row
		// first. guardNull names that refusal explicitly, which is why deleting it
		// on its own changes no outcome: it is defence in depth, not a reachable
		// arm.
		guardNull := assignmentLength.Valid && !assignmentID.Valid || operationLength.Valid && !operationID.Valid || predecessorLength.Valid && !predecessorID.Valid
		if observed > limit || guardNull {
			return actorOwnershipOwnedResult{}, actorOwnershipLimit(actor, journal.ActorOwnershipStageOwnedTasks, limit, observed, snapshot.Work)
		}
		if !assignmentID.Valid || !started.Valid || started.Int64 <= 0 || !producer.Valid || producer.Int64 <= 0 || !operationID.Valid {
			return actorOwnershipOwnedResult{}, actorOwnershipIntegrity(journal.ActorOwnershipStageOwnedTasks, fmt.Sprintf("owned task %s has an active episode without a complete start journal and journal_operations producer", task), "restore the started transition, journal supertype row, and journal_operations row from the same committed backup", nil)
		}
		assignment := journal.AssignmentID(assignmentID.String)
		if err := journal.ValidateOperationID(journal.OperationID(assignment)); err != nil {
			return actorOwnershipOwnedResult{}, actorOwnershipIntegrity(journal.ActorOwnershipStageOwnedTasks, fmt.Sprintf("owned task %s has malformed assignment ID %q", task, assignment), "restore the episode identity from the same committed backup", err)
		}
		operation := journal.OperationID(operationID.String)
		if err := journal.ValidateOperationID(operation); err != nil {
			return actorOwnershipOwnedResult{}, actorOwnershipIntegrity(journal.ActorOwnershipStageOwnedTasks, fmt.Sprintf("owned task %s has malformed producing operation ID %q", task, operation), "restore journal_operations and the started transition's journal producer", err)
		}
		phaseValue := ptypes.Phase(phase.Int64)
		if !phaseValue.IsValid() {
			return actorOwnershipOwnedResult{}, actorOwnershipIntegrity(journal.ActorOwnershipStageOwnedTasks, fmt.Sprintf("owned task %s has unknown phase %d", task, phase.Int64), "restore the task and its canonical phase from the same committed backup", nil)
		}
		row := journal.OwnedTaskRow{TaskID: task, Phase: phaseValue, AssignmentID: assignment, StartedJournalID: journal.JournalID(started.Int64), ProducingOperationID: operation}
		if predecessorID.Valid {
			value := journal.AssignmentID(predecessorID.String)
			if err := journal.ValidateOperationID(journal.OperationID(value)); err != nil {
				return actorOwnershipOwnedResult{}, actorOwnershipIntegrity(journal.ActorOwnershipStageOwnedTasks, fmt.Sprintf("owned task %s has malformed predecessor assignment %q", task, value), "restore the episode predecessor identity from the same committed backup", err)
			}
			row.PredecessorAssignmentID = &value
		}
		owned.tasks = append(owned.tasks, row)
		owned.bindings = append(owned.bindings, actorOwnershipOwnedBinding{TaskID: task.String(), StartProducer: producer.Int64, SupplementOperation: journal.NewGovernedAllocationSupplementOperationID(operation).OperationID()})
		snapshot.Tasks = append(snapshot.Tasks, row)
		snapshot.Work.TaskRows++
		snapshot.Work.ResultBytes = observed
	}
	if err := rows.Err(); err != nil {
		return actorOwnershipOwnedResult{}, actorOwnershipReadFault("could not finish reading owned tasks", "the context changed or SQLite could not continue the result stream", "statement 3 iteration", err)
	}
	return owned, rows.Close()
}

func actorOwnershipLimit(actor journal.ActorID, stage journal.ActorOwnershipStage, limit, observed int64, work journal.ActorOwnershipWork) error {
	return &journal.ActorOwnershipLimitError{Actor: actor, Stage: stage, LimitBytes: limit, ObservedBytes: observed, Work: work}
}

func actorOwnershipLength(value sql.NullInt64) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

func actorOwnershipIntegrity(stage journal.ActorOwnershipStage, problem, fix string, cause error) error {
	return &journal.ActorOwnershipIntegrityError{Stage: stage, Problem: problem, Fix: fix, Cause: cause}
}

func actorOwnershipReadFault(problem, why, when string, cause error) error {
	return fmt.Errorf("QueryActorOwnership: %s; why: %s; where: internal/sqlite pinned read connection; when: %s; impact: no result returned and nothing was written; fix: reopen a live store, then run Journal.VerifyIntegrity and restore damaged rows from a known-good copy: %w", problem, why, when, cause)
}

func actorOwnershipTaskIndex(snapshot *journal.ActorOwnershipSnapshot) map[journal.TaskID]int {
	index := make(map[journal.TaskID]int, len(snapshot.Tasks))
	for i := range snapshot.Tasks {
		index[snapshot.Tasks[i].TaskID] = i
	}
	return index
}

func (db *DB) readActorOwnershipMaterials(scope *connScope, actor journal.ActorID, bindings []actorOwnershipOwnedBinding, kinds []journal.EventKind, limit int64, snapshot *journal.ActorOwnershipSnapshot) error {
	bound, err := json.Marshal(bindings)
	if err != nil {
		return actorOwnershipReadFault("could not encode owned task producer bindings", "the in-memory binding contains an unsupported value", "before statement 4", err)
	}
	kindJSON, err := json.Marshal(kinds)
	if err != nil {
		return actorOwnershipReadFault("could not encode material kinds", "the validated in-memory kind list contains an unsupported value", "before statement 4", err)
	}
	rows, err := scope.conn.QueryContext(scope.ctx, actorOwnershipMaterialsSQL, string(bound), limit-snapshot.Work.ResultBytes, string(kindJSON))
	if err != nil {
		return actorOwnershipReadFault("could not read owned task-event material", "the store is unavailable or its material topology is damaged", "statement 4 inside the read transaction", err)
	}
	defer rows.Close()
	index := actorOwnershipTaskIndex(snapshot)
	for rows.Next() {
		var (
			journalID                 int64
			ownerTask                 string
			kindLength, payloadLength sql.NullInt64
			kind                      sql.NullString
			payload                   []byte
		)
		if err := rows.Scan(&journalID, &ownerTask, &kindLength, &kind, &payloadLength, &payload); err != nil {
			return actorOwnershipReadFault("could not decode an owned task-event material row", "SQLite returned an unexpected column type or shape", "statement 4 row decoding", err)
		}
		rowBytes := actorOwnershipLength(kindLength) + actorOwnershipLength(payloadLength)
		observed := snapshot.Work.ResultBytes + rowBytes
		if observed > limit || !kind.Valid || payloadLength.Valid && payload == nil {
			return actorOwnershipLimit(actor, journal.ActorOwnershipStageMaterials, limit, observed, snapshot.Work)
		}
		task, err := journalParseTask(ownerTask)
		if err != nil {
			return actorOwnershipIntegrity(journal.ActorOwnershipStageMaterials, fmt.Sprintf("material owner task %q is malformed", ownerTask), "restore the owned task and producer binding from the same committed backup", err)
		}
		taskIndex, exists := index[task]
		if !exists {
			return actorOwnershipIntegrity(journal.ActorOwnershipStageMaterials, fmt.Sprintf("material row %d names non-owned task %s", journalID, task), "restore the task, owner episode, and material producer rows from the same committed backup", nil)
		}
		if !json.Valid(payload) {
			return actorOwnershipIntegrity(journal.ActorOwnershipStageMaterials, fmt.Sprintf("material row %d has invalid JSON", journalID), "restore the canonical task-event payload from the same committed backup", nil)
		}
		snapshot.Tasks[taskIndex].Materials = append(snapshot.Tasks[taskIndex].Materials, journal.OwnedMaterialRow{JournalID: journal.JournalID(journalID), EventKind: journal.EventKind(kind.String), Payload: append(json.RawMessage(nil), payload...)})
		snapshot.Work.MaterialRows++
		snapshot.Work.ResultBytes = observed
	}
	if err := rows.Err(); err != nil {
		return actorOwnershipReadFault("could not finish reading owned task-event material", "the context changed or SQLite could not continue the result stream", "statement 4 iteration", err)
	}
	return rows.Close()
}

func (db *DB) readActorOwnershipEvidence(scope *connScope, actor journal.ActorID, bindings []actorOwnershipOwnedBinding, kinds []journal.EvidenceKind, limit int64, snapshot *journal.ActorOwnershipSnapshot) error {
	bound, err := json.Marshal(bindings)
	if err != nil {
		return actorOwnershipReadFault("could not encode owned task producer bindings", "the in-memory binding contains an unsupported value", "before statement 5", err)
	}
	kindJSON, err := json.Marshal(kinds)
	if err != nil {
		return actorOwnershipReadFault("could not encode evidence kinds", "the validated in-memory kind list contains an unsupported value", "before statement 5", err)
	}
	rows, err := scope.conn.QueryContext(scope.ctx, actorOwnershipEvidenceSQL, string(bound), limit-snapshot.Work.ResultBytes, string(kindJSON))
	if err != nil {
		return actorOwnershipReadFault("could not read owned evidence", "the store is unavailable or its evidence topology is damaged", "statement 5 inside the read transaction", err)
	}
	defer rows.Close()
	index := actorOwnershipTaskIndex(snapshot)
	for rows.Next() {
		var (
			journalID                 int64
			ownerTask                 string
			kindLength, payloadLength sql.NullInt64
			kind                      sql.NullString
			payload                   []byte
		)
		if err := rows.Scan(&journalID, &ownerTask, &kindLength, &kind, &payloadLength, &payload); err != nil {
			return actorOwnershipReadFault("could not decode an owned evidence row", "SQLite returned an unexpected column type or shape", "statement 5 row decoding", err)
		}
		rowBytes := actorOwnershipLength(kindLength) + actorOwnershipLength(payloadLength)
		observed := snapshot.Work.ResultBytes + rowBytes
		if observed > limit || !kind.Valid || payloadLength.Valid && payload == nil {
			return actorOwnershipLimit(actor, journal.ActorOwnershipStageEvidence, limit, observed, snapshot.Work)
		}
		task, err := journalParseTask(ownerTask)
		if err != nil {
			return actorOwnershipIntegrity(journal.ActorOwnershipStageEvidence, fmt.Sprintf("evidence owner task %q is malformed", ownerTask), "restore the owned task and producer binding from the same committed backup", err)
		}
		taskIndex, exists := index[task]
		if !exists {
			return actorOwnershipIntegrity(journal.ActorOwnershipStageEvidence, fmt.Sprintf("evidence row %d names non-owned task %s", journalID, task), "restore the task, owner episode, and evidence producer rows from the same committed backup", nil)
		}
		if !json.Valid(payload) {
			return actorOwnershipIntegrity(journal.ActorOwnershipStageEvidence, fmt.Sprintf("evidence row %d has invalid JSON", journalID), "restore the canonical evidence payload from the same committed backup", nil)
		}
		snapshot.Tasks[taskIndex].Evidence = append(snapshot.Tasks[taskIndex].Evidence, journal.OwnedEvidenceRow{JournalID: journal.JournalID(journalID), EvidenceKind: journal.EvidenceKind(kind.String), Payload: append([]byte(nil), payload...)})
		snapshot.Work.EvidenceRows++
		snapshot.Work.ResultBytes = observed
	}
	if err := rows.Err(); err != nil {
		return actorOwnershipReadFault("could not finish reading owned evidence", "the context changed or SQLite could not continue the result stream", "statement 5 iteration", err)
	}
	return rows.Close()
}
