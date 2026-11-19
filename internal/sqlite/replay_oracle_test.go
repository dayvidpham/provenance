package sqlite

// Frozen from the pre-M1 replay reducer. Keep independent of the optimized decode and fold.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// Capture the actual SQLite values immediately before the shadow tables are
// dropped. The initial defensive DROP has no tables to capture.
type oracleCaptureConnection struct {
	sqlConnection
	tables map[string][]byte
	err    error
}

func (c *oracleCaptureConnection) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if query == "DROP TABLE IF EXISTS shadow_tasks" && c.tables == nil && c.err == nil {
		var exists int
		c.err = c.sqlConnection.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_temp_master WHERE type='table' AND name='shadow_tasks'").Scan(&exists)
		if c.err == nil && exists != 0 {
			c.tables = make(map[string][]byte)
			for _, table := range []string{"shadow_tasks", "shadow_task_attributions", "shadow_edges", "shadow_labels", "shadow_comments"} {
				rows, err := c.sqlConnection.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY rowid")
				if err != nil {
					c.err = err
					break
				}
				columns, err := rows.Columns()
				var values [][]any
				for err == nil && rows.Next() {
					row := make([]any, len(columns))
					ptrs := make([]any, len(columns))
					for i := range row {
						ptrs[i] = &row[i]
					}
					err = rows.Scan(ptrs...)
					values = append(values, row)
				}
				if err == nil {
					err = rows.Err()
				}
				if closeErr := rows.Close(); err == nil {
					err = closeErr
				}
				if err == nil {
					c.tables[table], err = json.Marshal(values)
				}
				if err != nil {
					c.err = err
					break
				}
			}
		}
	}
	return c.sqlConnection.ExecContext(ctx, query, args...)
}

func assertReplayOracleParity(t *testing.T, db *DB, wantError bool) {
	t.Helper()
	assertReplayOracleParityWithTrust(t, db, wantError, false)
}

func assertReplayOracleParityWithTrust(t *testing.T, db *DB, wantError, trusted bool) {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	if trusted {
		trust := scope.resolveVerifiedPrefix()
		if !trust.journal || !trust.contexts {
			t.Fatalf("expected trusted prefix, got %+v", trust)
		}
		release := scope.trustVerifiedPrefix()
		defer release()
	}
	original := scope.conn
	run := func(oracle bool) (map[string][]byte, int, error) {
		capture := &oracleCaptureConnection{sqlConnection: original}
		scope.conn = capture
		var folded int
		var err error
		if oracle {
			_, _, _, _, folded, err = scope.oracleRederiveProjectionsShadow()
		} else {
			_, _, _, _, folded, err = scope.rederiveProjectionsShadow()
		}
		if capture.err != nil {
			t.Fatalf("capture shadow tables: %v", capture.err)
		}
		return capture.tables, folded, err
	}
	want, wantFolded, wantErr := run(true)
	got, gotFolded, gotErr := run(false)
	scope.conn = original
	if (wantErr != nil) != wantError || (gotErr != nil) != wantError {
		t.Fatalf("oracle error %v, production error %v; want error %v", wantErr, gotErr, wantError)
	}
	if wantErr != nil && wantErr.Error() != gotErr.Error() {
		t.Fatalf("first error differs: oracle %q; production %q", wantErr, gotErr)
	}
	if gotFolded != wantFolded || !reflect.DeepEqual(got, want) {
		t.Fatalf("shadow rows or folded count differ: oracle=%d production=%d\noracle=%s\nproduction=%s", wantFolded, gotFolded, want, got)
	}
}

func TestReplayOracleTrustedPrefix(t *testing.T) {
	fixture := buildStaleTailFixture(t)
	assertReplayOracleParityWithTrust(t, fixture.db, false, false)
	assertReplayOracleParityWithTrust(t, fixture.db, false, true)
}

func TestReplayOracleParity(t *testing.T) {
	seeds := 20
	if os.Getenv("PROVENANCE_REPLAY_ORACLE_200") != "" {
		seeds = 200
	}
	for seed := 0; seed < seeds; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			db := newJournalDB(t)
			f := newActorOwnershipFixture(t, db)
			rng := rand.New(rand.NewSource(int64(seed)))
			// One operation containing at least five effects also tests the
			// ordinal of scoped decisions and evidence within a shared anchor.
			effects := make([]journal.Effect, 0, 9)
			for i := 0; i < 8; i++ {
				switch rng.Intn(3) {
				case 0:
					effects = append(effects, journal.Effect{Sort: journal.EffectDecision, TaskID: f.task, DecisionKind: "oracle.decision", Payload: []byte(`{}`)})
				case 1:
					effects = append(effects, journal.Effect{Sort: journal.EffectEvidence, TaskID: f.task, EvidenceKind: "oracle.evidence", ContentDigest: []byte(fmt.Sprintf("seed-%d-%d", seed, i)), Payload: []byte(`{}`)})
				default:
					effects = append(effects, journal.Effect{Sort: journal.EffectTaskEvent, TaskID: f.task, EventKind: actorOwnershipMaterialKind, Payload: []byte(`{}`)})
				}
			}
			effects = append(effects, journal.Effect{Sort: journal.EffectLabelAdd, TaskID: f.task, Label: "oracle-label"})
			id := journal.OperationID(fmt.Sprintf("oracle-%d", seed))
			_, err := db.Apply(journal.OperationInput{OperationID: id, ActorID: f.actor, AuthorityJournalID: &f.boot, CommandDigest: []byte(id), Effects: effects})
			if err != nil {
				t.Fatalf("seed %d: Apply: %v", seed, err)
			}
			assertReplayOracleParity(t, db, false)
		})
	}
}

func TestReplayOracleMutationFamilies(t *testing.T) {
	db := newJournalDB(t)
	f := newActorOwnershipFixture(t, db)
	target := createActorOwnershipTask(t, db, f.actor, f.boot, "oracle-target", ptypes.PhaseUnscoped)
	comment := ptypes.CommentID{Namespace: f.task.Namespace, UUID: uuid.New()}
	_, err := db.Apply(journal.OperationInput{
		OperationID: "oracle-mutations", ActorID: f.actor, AuthorityJournalID: &f.boot,
		CommandDigest: []byte("oracle-mutations"),
		Effects: []journal.Effect{
			{Sort: journal.EffectEdgeAdd, TaskID: f.task, EdgeTargetID: target.String(), EdgeRelKind: ptypes.EdgeBlockedBy},
			{Sort: journal.EffectLabelAdd, TaskID: f.task, Label: "keep"},
			{Sort: journal.EffectLabelAdd, TaskID: f.task, Label: "remove"},
			{Sort: journal.EffectLabelRemove, TaskID: f.task, Label: "remove"},
			{Sort: journal.EffectCommentAdd, TaskID: f.task, CommentIdentity: comment, CommentAuthor: f.actor, CommentBody: "oracle comment"},
		},
	})
	if err != nil {
		t.Fatalf("apply mutation families: %v", err)
	}
	assertReplayOracleParity(t, db, false)
}

func TestReplayOracleFirstError(t *testing.T) {
	db := newJournalDB(t)
	f := newActorOwnershipFixture(t, db)
	scope := takePoolScope(t, db)
	_, err := scope.conn.ExecContext(scope.ctx, "UPDATE journal_operations SET canonical_mutation=x'00' WHERE journal_id IN (SELECT j.produced_by_operation_journal_id FROM journal j JOIN journal_task_events e ON e.journal_id=j.journal_id WHERE e.task_id=?1 ORDER BY j.journal_id LIMIT 2)", f.task.String())
	scope.release()
	if err != nil {
		t.Fatalf("corrupt canonical rows: %v", err)
	}
	assertReplayOracleParity(t, db, true)
}

func (scope *connScope) oracleProjectJournalRow(jid int64) error {
	var (
		kind       = -1
		actorRaw   string
		recordedAt int64
	)
	// Read the committing actor through journal_attributed (§8.5): a subordinate row
	// stores actor_id NULL, so effective_actor_id derives it from the row's anchor —
	// never a bare read of the NULL column. The single shared reducer step therefore
	// attributes the same committing actor whether Apply folds a just-produced
	// subordinate row or Open replays it (§9.2).
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT kind_id, effective_actor_id, recorded_at FROM journal_attributed WHERE journal_id = ?1", jid).Scan(&kind, &actorRaw, &recordedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("project journal row %d: no such journal row", jid)
		}
		return fmt.Errorf("project journal row %d: load supertype: %w", jid, err)
	}
	committing, err := journalParseActor(actorRaw)
	if err != nil {
		return err
	}

	switch journal.JournalKind(kind) {
	case journal.JournalKindOperation:
		return nil // an operation anchor projects nothing
	case journal.JournalKindTaskEvent:
		return scope.oracleProjectTaskEventRow(jid, committing, recordedAt)
	case journal.JournalKindAuthority:
		return scope.oracleProjectAuthorityRow(jid)
	case journal.JournalKindDecision:
		return scope.oracleProjectTaskScopedRow(jid, committing, taskScopedDecision)
	case journal.JournalKindEvidence:
		return scope.oracleProjectTaskScopedRow(jid, committing, taskScopedEvidence)
	case journal.JournalKindActivity:
		// Activity birth rows have no task-scoped watermark or attribution to advance.
		// The journal_activity_creations row exists and the activities row was inserted
		// by the fold; no additional projection state changes here.
		return nil
	default:
		return fmt.Errorf("project journal row %d: unknown journal kind %d", jid, kind)
	}
}

func (scope *connScope) oracleProjectTaskEventRow(jid int64, committing journal.ActorID, recordedAt int64) error {
	var (
		taskRaw string
		kindStr string
		payload []byte
	)
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT task_id, event_kind, payload FROM journal_task_events WHERE journal_id = ?1", jid).Scan(&taskRaw, &kindStr, &payload); err != nil {
		return fmt.Errorf("project task_event %d: %w", jid, err)
	}
	payload = append([]byte(nil), payload...)
	task, err := journalParseTask(taskRaw)
	if err != nil {
		return err
	}
	if err := scope.insertAttribution(task, committing, jid); err != nil {
		return err
	}
	canonicalEffect, canonical, err := scope.oracleCanonicalEffectForJournalRow(jid)
	if err != nil {
		return err
	}
	if canonical {
		if canonicalEffect.TaskID != task || canonicalEffect.EventKind != "" && canonicalEffect.EventKind != journal.EventKind(kindStr) {
			return fmt.Errorf("provenance: canonical mutation row %d disagrees with journal_task_events task/event facts — where: startup canonical validation; impact: database open fails without projection writes; fix: restore the journal row and canonical bytes from the same committed operation", jid)
		}
		if scope.projectionTarget == projectionTargetShadow {
			switch canonicalEffect.Sort {
			case journal.EffectTaskCreate, journal.EffectTaskCreateAllocated:
				if err := scope.insertCanonicalShadowTask(canonicalEffect, recordedAt, jid); err != nil {
					return err
				}
			case journal.EffectTaskEvent:
				if err := scope.materializeCanonicalShadowTaskEvent(canonicalEffect, recordedAt); err != nil {
					return err
				}
			}
		}
		if scope.projectionTarget == projectionTargetLive && canonicalEffect.Sort == journal.EffectTaskEvent && !journal.IsMutationFamilyKind(canonicalEffect.EventKind) && canonicalEffect.EventKind != journal.EventKindTaskMigrated {
			return projectV1TaskEvent(scope.ctx, allocationSQLTx{conn: scope.conn}, committing, jid, recordedAt, canonicalEffect)
		}
	}
	// Journaled relationship/annotation mutation families (§6 amendment) fold into the
	// edges/labels/comments domain projection through the same shared reducer step Apply
	// and Open both run (§9.2), so those tables are re-derivable from ordered history and
	// covered by the §15 convergence check. They are non-lifecycle, so they never reach
	// the status branches below.
	if journal.IsMutationFamilyKind(journal.EventKind(kindStr)) {
		return scope.oracleProjectMutationFamilyRow(task, journal.EventKind(kindStr), payload, jid, recordedAt)
	}
	// A migration marker seeds the status projection from the legacy status it
	// captured in its own payload (§13, §15) — read from the journal row, never from
	// the mutable tasks row — so both live migration and Open's from-empty replay
	// derive the identical status from the identical journal fact (§9.2).
	if journal.EventKind(kindStr) == journal.EventKindTaskMigrated {
		status, ok, derr := journal.DecodeLegacyStatus(payload)
		if derr != nil {
			return fmt.Errorf("project task_event %d: %w", jid, derr)
		}
		if ok {
			return scope.projectTaskStatus(task, status, jid, recordedAt)
		}
		return scope.advanceWatermark(task, jid)
	}
	if status, isLifecycle := journal.StatusForEventKind(journal.EventKind(kindStr)); isLifecycle {
		// The static status FSM (§8.1) governs the four TRANSITION lifecycle kinds
		// (started/stopped/closed/reopened). It is enforced HERE — in the single shared
		// reducer step Apply and Open both run (§9.2) — so a live Apply rejects an illegal
		// transition before commit and Open's from-empty replay applies the identical rule
		// to the identical row. The current status is read from the projection target
		// (real tasks during Apply, the shadow table during replay), which reflects the
		// history strictly before this row. A forced coercion carries a marker in this
		// row's payload and skips the FSM, so the coercion is reproducible either way.
		if journal.IsTransitionLifecycleKind(journal.EventKind(kindStr)) {
			forced, ferr := journal.DecodeForcedTransition(payload)
			if ferr != nil {
				return fmt.Errorf("project task_event %d: %w", jid, ferr)
			}
			if !forced {
				current, cerr := scope.readProjTaskStatus(task)
				if cerr != nil {
					return cerr
				}
				if verr := journal.ValidateStatusTransition(current, journal.EventKind(kindStr)); verr != nil {
					return fmt.Errorf("project task_event %d: %w", jid, verr)
				}
			}
		}
		return scope.projectTaskStatus(task, status, jid, recordedAt)
	}
	return scope.advanceWatermark(task, jid)
}

func (scope *connScope) oracleProjectAuthorityRow(jid int64) error {
	var (
		assignment string
		transition = -1
		hasTrans   bool
	)
	err := scope.conn.QueryRowContext(scope.ctx, "SELECT assignment_id, transition_id FROM journal_authority_assignment_transitions WHERE journal_id = ?1", jid).Scan(&assignment, &transition)
	if err == nil {
		hasTrans = true
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("project authority %d: load transition: %w", jid, err)
	}
	if !hasTrans {
		return nil // bootstrap authority: no task projection
	}
	var (
		taskRaw     string
		occupantRaw string
		slot        = -1
	)
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT task_id, actor_id, slot_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", assignment).Scan(&taskRaw, &occupantRaw, &slot); err != nil {
		return fmt.Errorf("project authority %d: load episode %q: %w", jid, assignment, err)
	}
	task, err := journalParseTask(taskRaw)
	if err != nil {
		return err
	}
	if transition == transitionStartedID {
		occupant, err := journalParseActor(occupantRaw)
		if err != nil {
			return err
		}
		if err := scope.insertAttribution(task, occupant, jid); err != nil {
			return err
		}
	}
	if slot == slotOwnerResponsibilityID {
		return scope.recomputeTaskOwner(task, jid)
	}
	return scope.advanceWatermark(task, jid)
}

func (scope *connScope) oracleProjectTaskScopedRow(jid int64, committing journal.ActorID, table taskScopedTable) error {
	var taskRaw string
	hasTask := false
	var taskValue sql.NullString
	if err := scope.conn.QueryRowContext(scope.ctx, table.query(), jid).Scan(&taskValue); err != nil {
		return fmt.Errorf("project %s %d: %w", table.label(), jid, err)
	}
	if taskValue.Valid {
		taskRaw = taskValue.String
		hasTask = true
	}
	if !hasTask {
		return nil
	}
	task, err := journalParseTask(taskRaw)
	if err != nil {
		return err
	}
	if err := scope.insertAttribution(task, committing, jid); err != nil {
		return err
	}
	return scope.advanceWatermark(task, jid)
}

func (scope *connScope) oracleRederiveProjectionsShadow() (
	tasks map[string]journal.TaskProjection,
	taskState map[string]completeTaskState,
	attribs map[string]map[string]int64,
	domain domainProjection,
	folded int,
	err error,
) {
	if err = scope.createProjectionShadow(); err != nil {
		return nil, nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: stage shadow projection tables: %w", err)
	}
	// Repoint the shared reducer's projection-write steps at the shadow tables, and
	// unconditionally restore the real targets + drop the shadow tables on return. The
	// relationship/annotation domain projections (§6 amendment) are repointed alongside
	// the task/attribution projections so their from-empty refold lands in the shadow
	// tables too.
	scope.projectionTarget = projectionTargetShadow
	defer func() {
		scope.projectionTarget = projectionTargetLive
		if derr := scope.dropProjectionShadow(); derr != nil && err == nil {
			err = fmt.Errorf("ReplayProjections: drop shadow projection tables: %w", derr)
		}
	}()

	var order []int64
	if err = scope.queryRows("SELECT journal_id FROM journal ORDER BY journal_id ASC", nil, func(rows *sql.Rows) error {
		var journalID int64
		if err := rows.Scan(&journalID); err != nil {
			return err
		}
		order = append(order, journalID)
		return nil
	}); err != nil {
		return nil, nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: enumerate journal: %w", err)
	}
	for _, jid := range order {
		if err = scope.oracleProjectJournalRow(jid); err != nil {
			return nil, nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: fold row %d: %w", jid, err)
		}
		folded++
	}

	tasks, err = scope.snapshotTaskProjections(projectionTargetShadow)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	taskState, err = scope.snapshotCompleteTaskState(projectionTargetShadow)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	attribs, err = scope.snapshotAttributions(projectionTargetShadow)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	domain, err = scope.snapshotDomainProjections(projectionTargetShadow)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	return tasks, taskState, attribs, domain, folded, nil
}

func (scope *connScope) oracleCanonicalEffectForJournalRow(jid int64) (journal.Effect, bool, error) {
	var version string
	var wire []byte
	var anchor int64
	columns, err := scope.tableColumns("journal_operations")
	if err != nil {
		return journal.Effect{}, false, err
	}
	shape := canonicalColumnsPresent
	if isLegacyOperationsColumnSet(columns) {
		shape = canonicalColumnsLegacy
	}
	args := []any{jid}
	if shape == canonicalColumnsLegacy {
		args = append(args, nil, nil)
	}
	var storedVersion sql.NullString
	var storedWire []byte
	err = scope.conn.QueryRowContext(scope.ctx, shape.effectForRowQuery(), args...).Scan(&anchor, &storedVersion, &storedWire)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return journal.Effect{}, false, nil
		}
		return journal.Effect{}, false, fmt.Errorf("load canonical mutation for row %d: %w", jid, err)
	}
	if storedVersion.Valid {
		version = storedVersion.String
	}
	if storedWire != nil {
		wire = append([]byte(nil), storedWire...)
	}
	if (version == "") != (len(wire) == 0) {
		return journal.Effect{}, false, fmt.Errorf("provenance: operation %d has mixed canonical encoding facts (version=%q bytes=%d) — where: startup canonical validation; impact: database open fails without mutation; fix: restore both canonical columns together, or NULL both only for a genuine legacy operation", anchor, version, len(wire))
	}
	if version == "" {
		return journal.Effect{}, false, nil
	}
	prepared, err := journal.DecodeCanonicalMutation(wire)
	if err != nil {
		return journal.Effect{}, false, fmt.Errorf("operation %d canonical mutation: %w — where: canonical effect reconstruction during startup replay; when: decoding the producing operation's stored canonical bytes; impact: replay fails closed and no projection is written; fix: %s", anchor, err, unsupportedPreV004DatabaseFix)
	}
	if prepared.EncodingVersion().String() != version {
		return journal.Effect{}, false, fmt.Errorf("operation %d canonical column version %q differs from registered wire version %q", anchor, version, prepared.EncodingVersion())
	}
	ordinal := 0
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM journal WHERE produced_by_operation_journal_id=?1 AND journal_id<=?2", anchor, jid).Scan(&ordinal); err != nil {
		return journal.Effect{}, false, fmt.Errorf("resolve canonical effect ordinal for row %d: %w", jid, err)
	}
	ordinal--
	effects := prepared.NormalizedEffects()
	if ordinal < 0 || ordinal >= len(effects) {
		return journal.Effect{}, false, fmt.Errorf("provenance: operation %d row %d has effect ordinal %d outside canonical effect count %d", anchor, jid, ordinal, len(effects))
	}
	return effects[ordinal], true, nil
}

func (scope *connScope) oracleProjectMutationFamilyRow(task journal.TaskID, kind journal.EventKind, payload []byte, jid, recordedAt int64) error {
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
