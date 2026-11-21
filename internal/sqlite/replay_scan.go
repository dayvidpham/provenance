package sqlite

import (
	"database/sql"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
)

// replayScanRow holds only a single row from the ordered journal scan. Null
// fields preserve missing subtype rows, for which the reducer uses its original
// point lookup to retain the same error.
type replayScanRow struct {
	id, kind, recordedAt int64
	actor                sql.NullString
	anchor, operation    sql.NullInt64
	version              sql.NullString
	wire                 []byte
	taskID, eventKind    sql.NullString
	payload              []byte
	decisionID           sql.NullInt64
	decisionTask         sql.NullString
	evidenceID           sql.NullInt64
	evidenceTask         sql.NullString
	assignment           sql.NullString
	transition           sql.NullInt64
	ordinal              int
}

type replayAnchorCache struct {
	anchor    int64
	maxAnchor int64
	ordinal   int
	noncontig bool
}

type decodedReplayOperation struct {
	canonical bool
	effects   []journal.Effect
	err       error
}

type decodedReplayRow struct {
	actor    journal.ActorID
	actorErr error
	event    decodedTaskEvent
	op       decodedReplayOperation
}

type replayBatchGroup struct {
	start, end int
}

const defaultReplayBatchSize = 64 * 1024
const maxReplayBatchBytes = 32 * 1024 * 1024

func (scope *connScope) orderedReplayRows(consume func(*replayScanRow) error) error {
	columns, err := scope.tableColumns("journal_operations")
	if err != nil {
		return err
	}
	canonicalColumns := "o.mutation_encoding_version,o.canonical_mutation"
	if isLegacyOperationsColumnSet(columns) {
		canonicalColumns = "NULL,NULL"
	}
	query := `SELECT j.journal_id,j.kind_id,j.effective_actor_id,j.recorded_at,
		j.produced_by_operation_journal_id,o.journal_id,` + canonicalColumns + `,
		te.task_id,te.event_kind,te.payload,d.journal_id,d.task_id,e.journal_id,e.task_id,
		tr.assignment_id,tr.transition_id
		FROM journal_attributed j
		LEFT JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id
		LEFT JOIN journal_task_events te ON te.journal_id=j.journal_id
		LEFT JOIN journal_decisions d ON d.journal_id=j.journal_id
		LEFT JOIN journal_evidence e ON e.journal_id=j.journal_id
		LEFT JOIN journal_authority_assignment_transitions tr ON tr.journal_id=j.journal_id
		ORDER BY j.journal_id`
	rows, err := scope.conn.QueryContext(scope.ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	batchSize := scope.replayBatchSize
	if batchSize <= 0 {
		batchSize = defaultReplayBatchSize
	}
	batch := make([]replayScanRow, 0, batchSize)
	batchBytes := 0
	var previousAnchor int64
	var previousOp decodedReplayOperation
	var previousOpValid bool
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		groups := make([]replayBatchGroup, 0)
		for start := 0; start < len(batch); {
			end := start + 1
			for end < len(batch) && batch[start].anchor.Valid &&
				batch[end].anchor.Valid && batch[start].anchor.Int64 == batch[end].anchor.Int64 {
				end++
			}
			groups = append(groups, replayBatchGroup{start: start, end: end})
			start = end
		}
		type groupResult struct {
			op      decodedReplayOperation
			rows    []decodedReplayRow
			opValid bool
		}
		carryAnchor, carryOp, carryValid := previousAnchor, previousOp, previousOpValid
		err := forEachOrdered(len(groups), canonicalDecodeWorkers(), func(i int) groupResult {
			group := groups[i]
			result := groupResult{rows: make([]decodedReplayRow, group.end-group.start)}
			first := batch[group.start]
			// The preceding batch's last group is already consumed before this
			// flush starts; it is immutable while workers decode this batch.
			if i == 0 && first.anchor.Valid && first.anchor.Int64 == carryAnchor && carryValid {
				result.op, result.opValid = carryOp, true
			}
			for n := group.start; n < group.end; n++ {
				row := &batch[n]
				decoded := &result.rows[n-group.start]
				if row.actor.Valid {
					decoded.actor, decoded.actorErr = journalParseActor(row.actor.String)
				}
				if row.taskID.Valid && row.eventKind.Valid {
					decoded.event = decodeTaskEventRow(row.taskID.String, row.eventKind.String, row.payload)
					if !result.opValid && row.operation.Valid {
						result.op = decodeReplayOperation(row)
						result.opValid = true
					}
				}
			}
			return result
		}, func(i int, result groupResult) error {
			group := groups[i]
			for n := group.start; n < group.end; n++ {
				decoded := &result.rows[n-group.start]
				decoded.op = result.op
				scope.replayDecoded = decoded
				if err := consume(&batch[n]); err != nil {
					return err
				}
			}
			last := batch[group.end-1]
			previousAnchor, previousOp, previousOpValid = last.anchor.Int64, result.op, last.anchor.Valid && result.opValid
			return nil
		})
		scope.replayDecoded = nil
		batch = batch[:0]
		batchBytes = 0
		return err
	}
	for rows.Next() {
		var row replayScanRow
		if err := rows.Scan(&row.id, &row.kind, &row.actor, &row.recordedAt,
			&row.anchor, &row.operation, &row.version, &row.wire,
			&row.taskID, &row.eventKind, &row.payload, &row.decisionID, &row.decisionTask,
			&row.evidenceID, &row.evidenceTask, &row.assignment, &row.transition); err != nil {
			if prior := flush(); prior != nil {
				return prior
			}
			return err
		}
		if row.anchor.Valid {
			cache := &scope.replayAnchor
			if cache.anchor != row.anchor.Int64 {
				maxAnchor := cache.maxAnchor
				noncontig := row.anchor.Int64 <= maxAnchor
				if row.anchor.Int64 > maxAnchor {
					maxAnchor = row.anchor.Int64
				}
				*cache = replayAnchorCache{anchor: row.anchor.Int64, maxAnchor: maxAnchor, noncontig: noncontig}
			}
			if cache.noncontig {
				if err := scope.projectionRow(stmtCanonicalOrdinal, "SELECT COUNT(*) FROM journal WHERE produced_by_operation_journal_id=?1 AND journal_id<=?2", row.anchor.Int64, row.id).Scan(&cache.ordinal); err != nil {
					if prior := flush(); prior != nil {
						return prior
					}
					return fmt.Errorf("resolve canonical effect ordinal for row %d: %w", row.id, err)
				}
			} else {
				cache.ordinal++
			}
			row.ordinal = cache.ordinal - 1
		}
		batch = append(batch, row)
		batchBytes += len(row.wire) + len(row.payload)
		if len(batch) == batchSize || batchBytes >= maxReplayBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return rows.Err()
}

func decodeReplayOperation(row *replayScanRow) decodedReplayOperation {
	var result decodedReplayOperation
	version := row.version.String
	if (version == "") != (len(row.wire) == 0) {
		result.err = fmt.Errorf("provenance: operation %d has mixed canonical encoding facts (version=%q bytes=%d) — where: startup canonical validation; impact: database open fails without mutation; fix: restore both canonical columns together, or NULL both only for a genuine legacy operation", row.operation.Int64, version, len(row.wire))
	} else if version != "" {
		prepared, err := journal.DecodeCanonicalMutation(row.wire)
		if err != nil {
			result.err = fmt.Errorf("operation %d canonical mutation: %w — where: canonical effect reconstruction during startup replay; when: decoding the producing operation's stored canonical bytes; impact: replay fails closed and no projection is written; fix: %s", row.operation.Int64, err, unsupportedPreV004DatabaseFix)
		} else if prepared.EncodingVersion().String() != version {
			result.err = fmt.Errorf("operation %d canonical column version %q differs from registered wire version %q", row.operation.Int64, version, prepared.EncodingVersion())
		} else {
			result.canonical = true
			result.effects = prepared.NormalizedEffects()
		}
	}
	return result
}

func (scope *connScope) scannedCanonicalEffect(row *replayScanRow) (journal.Effect, bool, error) {
	if !row.operation.Valid {
		return journal.Effect{}, false, nil
	}
	op := scope.replayDecoded.op
	if op.err != nil || !op.canonical {
		return journal.Effect{}, false, op.err
	}
	if row.ordinal < 0 || row.ordinal >= len(op.effects) {
		return journal.Effect{}, false, fmt.Errorf("provenance: operation %d row %d has effect ordinal %d outside canonical effect count %d", row.operation.Int64, row.id, row.ordinal, len(op.effects))
	}
	return op.effects[row.ordinal], true, nil
}
