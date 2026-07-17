package provenance

// journal_s13_corpus_test.go executes the S1.3 adversarial proof-corpus histories
// — the shared Apply/Open reducer, replay, retry/reopen/cancellation recovery,
// legacy-baseline migration, external-schema preflight, and fail-closed topology
// detection (docs/journal-relational-contract.md §9, §13, §15) — against the real
// production path (Apply, ReplayProjections, MigrateLegacyBaseline, PreflightSchema).
// Each operator translates the symbolic corpus data into concrete registered IDs
// and drives the production reducer, honouring the case's must-pass/must-fail
// classification. These operators are the executable half of the s1.3 partition
// recorded in testdata/contract/scope.yaml.

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/testcorpus"
)

// s13Operators is the closed registry of executable S1.3 operators. Its key set
// must equal the s1.3 partition of scope.yaml (asserted by the partition test).
var s13Operators = map[testcorpus.OperatorName]s11Handler{
	// retry_reopen_cancellation.yaml (§9.2, §9.4, §9.5)
	"fault-then-retry-same-operation":                 opFaultThenRetrySameOperation,
	"apply-then-close-then-reopen-compare-projection": opApplyCloseReopenCompareProjection,
	"concurrent-open-then-reopen-converge":            opConcurrentOpenThenReopenConverge,
	"cancel-mid-batch":                                opCancelMidBatch,
	"reuse-operationid-different-mutation-digest":     opReuseOperationIDDifferentMutationDigest,
	// baseline_migration.yaml (§13, §13.1, §13.2)
	"migrate-legacy-task":                           opMigrateLegacyTask,
	"migrate-legacy-task-with-fabricated-timestamp": opMigrateLegacyTaskFabricatedTimestamp,
	"migrate-batch-with-one-unmappable-owner":       opMigrateBatchOneUnmappableOwner,
	"migrate-then-extend-compare-to-native-only":    opMigrateThenExtendCompareNativeOnly,
	"migrate-then-remigrate-same-legacy-set":        opMigrateThenRemigrateSameSet,
	// topology_corruption.yaml (§13 preflight)
	"schema-preflight":                  opSchemaPreflight,
	"schema-preflight-missing-table":    opSchemaPreflightMissingTable,
	"schema-preflight-extra-column":     opSchemaPreflightExtraColumn,
	"schema-preflight-missing-column":   opSchemaPreflightMissingColumn,
	"fault-mid-migration":               opFaultMidMigration,
	"concurrent-open-during-corruption": opConcurrentOpenDuringCorruption,
	// owner_responsibility.yaml (§8.1, §9.5)
	"reopen-task": opReopenTask,
	"fault-between-transfer-ended-and-started": opFaultBetweenTransferEndedAndStarted,
	// authority_evidence.yaml (§9.4)
	"replay-with-ended-authority": opReplayWithEndedAuthority,
	// zero_event_operations.yaml (§9.4)
	"replay-zero-task-event-operation": opReplayZeroTaskEventOperation,
}

// ---------------------------------------------------------------------------
// retry / reopen / cancellation (§9.2, §9.4, §9.5)
// ---------------------------------------------------------------------------

func opFaultThenRetrySameOperation(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	env.startEpisode(t, "op-seed-A", boot, task, "A", occupant)
	op := opID(input, "op-close-task-1")
	st := env.tr.(*sqliteTracker)

	closeOp := OperationInput{
		OperationID: op, ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{
			{Sort: EffectAssignmentEnd, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility},
			{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKindTaskClosed},
		},
	}
	// First attempt: a fault injected after the first effect rolls the whole
	// operation back (§9.5) — nothing committed.
	if _, err := st.db.AdversarialApplyWithFault(closeOp, 0); !errors.Is(err, ErrInjectedFault) {
		return fmt.Errorf("faulted first attempt = %v, want ErrInjectedFault", err)
	}
	if r, err := env.tr.Journal().LookupCommitted(op); err != nil {
		return err
	} else if r.Kind != CommittedAbsent {
		return fmt.Errorf("faulted first attempt left committed state: %+v", r)
	}
	// Retry the identical OperationID: it must now succeed and commit exactly the
	// effects the original attempt would have.
	res, err := env.tr.Journal().Apply(closeOp)
	if err != nil {
		return fmt.Errorf("retry after fault rejected: %w", err)
	}
	if res.Kind != CommittedExact || res.AnchorJournalID == 0 {
		return fmt.Errorf("retry did not commit a single anchor: %+v", res)
	}
	got, err := env.tr.Show(task)
	if err != nil {
		return err
	}
	if got.Owner != nil {
		return fmt.Errorf("after retry-close the owner is not cleared: %v", got.Owner)
	}
	return nil
}

func opApplyCloseReopenCompareProjection(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	dir := t.TempDir()
	path := filepath.Join(dir, "reopen.db")
	tr, err := OpenSQLite(path)
	if err != nil {
		return fmt.Errorf("open file tracker: %w", err)
	}
	actor, task := s13SeedActorTask(t, tr, "t1")
	boot := s13Genesis(t, tr, actor)
	s13StartOwner(t, tr, boot, actor, task, "A", actor)
	// Close: end the active episode and record the close event in one operation.
	if _, err := tr.Journal().Apply(OperationInput{
		OperationID: "op-close-1", ActorID: actor, AuthorityJournalID: &boot,
		CommandDigest: []byte("c"), MutationDigest: []byte("m"),
		Effects: []Effect{
			{Sort: EffectAssignmentEnd, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility},
			{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKindTaskClosed},
		},
	}); err != nil {
		_ = tr.Close()
		return fmt.Errorf("close operation: %w", err)
	}
	live, err := tr.Show(task)
	if err != nil {
		_ = tr.Close()
		return err
	}
	if live.Status != StatusClosed || live.Owner != nil {
		_ = tr.Close()
		return fmt.Errorf("live projection = {status:%v owner:%v}, want {closed nil}", live.Status, live.Owner)
	}
	if err := tr.Close(); err != nil {
		return fmt.Errorf("close tracker before reopen: %w", err)
	}

	// Reopen the same database: Open's full replay must derive the identical
	// projection using the same fold, not a second lifecycle switch (§9.2).
	reopened, err := OpenSQLite(path)
	if err != nil {
		return fmt.Errorf("reopen file tracker: %w", err)
	}
	defer func() { _ = reopened.Close() }()
	replay, err := reopened.Journal().ReplayProjections()
	if err != nil {
		return fmt.Errorf("reopen replay: %w", err)
	}
	rp, ok := replay.ProjectionForTask(task)
	if !ok {
		return fmt.Errorf("replay produced no projection for the closed task")
	}
	if rp.Status != TaskStatusClosed || rp.Owner != nil {
		return fmt.Errorf("reopened projection = {status:%v owner:%v}, want {closed nil}", rp.Status, rp.Owner)
	}
	after, err := reopened.Show(task)
	if err != nil {
		return err
	}
	if after.Status != StatusClosed || after.Owner != nil {
		return fmt.Errorf("reopened stored projection diverged: {status:%v owner:%v}", after.Status, after.Owner)
	}
	return nil
}

func opConcurrentOpenThenReopenConverge(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	system := env.actorFor(t, "pasture-system")
	st := env.tr.(*sqliteTracker)

	// Five legacy tasks to activate.
	legacy := make([]LegacyTaskRow, 0, 5)
	base := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		task := env.taskFor(t, fmt.Sprintf("legacy-%d", i))
		legacy = append(legacy, LegacyTaskRow{
			ID: task, Status: TaskStatusOpen,
			CreatedAt: base.Add(time.Duration(i) * time.Hour), UpdatedAt: base.Add(time.Duration(i) * time.Hour),
		})
	}
	in := MigrationInput{System: system, BootstrapAuthority: boot, Legacy: legacy}

	// Two concurrent activations (serialized by the store's single-writer lock,
	// §9.6): the first commits five baselines, the second short-circuits to zero
	// via the deterministic OperationID (§9.4). No duplicate baseline anchors.
	var wg sync.WaitGroup
	results := make([]MigrationResult, 2)
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = env.tr.Journal().MigrateLegacyBaseline(in)
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			return fmt.Errorf("concurrent activation %d failed: %w", i, e)
		}
	}
	created := results[0].BaselineAnchorsCreated + results[1].BaselineAnchorsCreated
	if created != 5 {
		return fmt.Errorf("concurrent activations created %d baseline anchors, want 5 (no duplicates)", created)
	}
	anchors, err := st.db.CountBaselineAnchors()
	if err != nil {
		return err
	}
	if anchors != 5 {
		return fmt.Errorf("after two activations there are %d baseline anchors, want 5", anchors)
	}
	// Subsequent reopen: full replay converges on the identical projection (§9.2).
	replay, err := env.tr.Journal().ReplayProjections()
	if err != nil {
		return fmt.Errorf("subsequent reopen replay diverged: %w", err)
	}
	if replay.RowsFolded == 0 {
		return fmt.Errorf("reopen replay folded no journal rows")
	}
	return nil
}

func opCancelMidBatch(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	op := opID(input, "op-batch-cancel-1")
	st := env.tr.(*sqliteTracker)

	batch := OperationInput{
		OperationID: op, ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{
			{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated"},
			{Sort: EffectAssignmentStart, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant},
			{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.status-changed"},
			{Sort: EffectEvidence, TaskID: task, EvidenceKind: "pasture.git.commit", ContentDigest: env.digest("x"), Payload: []byte(`{}`)},
		},
	}
	// Cancel after effect index 1 (two effects folded in-memory), before commit.
	_, err := st.db.AdversarialApplyWithFault(batch, 1)
	if err == nil {
		return fmt.Errorf("cancelled batch committed; expected a fail-closed rollback")
	}
	// No effect, including effects 1-2, may remain committed (§9.5).
	r, err := env.tr.Journal().LookupCommitted(op)
	if err != nil {
		return err
	}
	if r.Kind != CommittedAbsent {
		return fmt.Errorf("cancelled batch left committed state (anchor created): %+v", r)
	}
	if got, err := env.tr.Show(task); err != nil {
		return err
	} else if got.Owner != nil {
		return fmt.Errorf("cancelled batch left an owner projection: %v", got.Owner)
	}
	return nil
}

func opReuseOperationIDDifferentMutationDigest(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	op := opID(input, "op-close-task-1")
	base := OperationInput{
		OperationID: op, ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("cmd"), MutationDigest: env.digest("digest-a"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated"}},
	}
	if _, err := env.tr.Journal().Apply(base); err != nil {
		return fmt.Errorf("original apply: %w", err)
	}
	conflicting := base
	conflicting.MutationDigest = env.digest("digest-b")
	res, err := env.tr.Journal().Apply(conflicting)
	if err == nil {
		return fmt.Errorf("OperationID reuse with a different mutation digest was accepted; expected a typed conflict")
	}
	if res.Kind != CommittedConflict {
		return fmt.Errorf("reuse variant = %s, want CommittedConflict", res.Kind)
	}
	if !errors.Is(err, ErrOperationConflict) {
		return fmt.Errorf("reuse rejected with %v, want ErrOperationConflict", err)
	}
	if r, lerr := env.tr.Journal().LookupCommitted(op); lerr != nil {
		return lerr
	} else if len(r.EmittedEvents) != 1 {
		return fmt.Errorf("after a rejected reuse there are %d emitted events, want 1 (nothing extra committed)", len(r.EmittedEvents))
	}
	return nil
}

// ---------------------------------------------------------------------------
// legacy-baseline migration (§13)
// ---------------------------------------------------------------------------

func opMigrateLegacyTask(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	system := env.actorFor(t, "pasture-system")
	st := env.tr.(*sqliteTracker)

	lt, owners, label, err := env.parseLegacyTask(t, mustMap(input, "legacyTask"))
	if err != nil {
		return err
	}
	in := MigrationInput{System: system, BootstrapAuthority: boot, Owners: owners, Legacy: []LegacyTaskRow{lt}}
	res, err := env.tr.Journal().MigrateLegacyBaseline(in)
	if err != nil {
		return fmt.Errorf("migrate legacy task %q: %w", label, err)
	}
	if res.BaselineAnchorsCreated != 1 {
		return fmt.Errorf("migration created %d anchors, want 1", res.BaselineAnchorsCreated)
	}
	// migrationEventCreated: the deterministic baseline anchor exists (§13 item 1).
	if anchors, err := st.db.CountBaselineAnchors(); err != nil {
		return err
	} else if anchors != 1 {
		return fmt.Errorf("expected 1 baseline anchor, found %d", anchors)
	}
	wantEpisodes, _ := asInt(expected, "episodesCreated")
	gotEpisodes, err := st.db.CountEpisodesForTask(lt.ID)
	if err != nil {
		return err
	}
	if gotEpisodes != wantEpisodes {
		return fmt.Errorf("episodesCreated = %d, want %d", gotEpisodes, wantEpisodes)
	}
	if wantEpisodes == 0 {
		return nil // fresh baseline: marker only
	}
	assignment := MigrationBaselineAssignmentID(lt.ID)
	if wantActive, ok := asBoolOK(expected, "episodeActiveAfterMigration"); ok && wantActive {
		active, err := st.db.EpisodeActive(assignment)
		if err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("migrated episode is not active after migration")
		}
	}
	// Honest RecordedAt (§13): started == legacy updatedAt; ended == legacy closedAt.
	if startedWant, err := asTime(expected, "startedTransitionRecordedAt"); err == nil {
		got, ok, err := st.db.EpisodeTransitionRecordedAt(assignment, true)
		if err != nil {
			return err
		}
		if !ok || got != startedWant.UnixNano() {
			return fmt.Errorf("started transition RecordedAt = %d, want %d (legacy updated_at)", got, startedWant.UnixNano())
		}
	}
	if endedWant, err := asTime(expected, "endedTransitionRecordedAt"); err == nil {
		got, ok, err := st.db.EpisodeTransitionRecordedAt(assignment, false)
		if err != nil {
			return err
		}
		if !ok || got != endedWant.UnixNano() {
			return fmt.Errorf("ended transition RecordedAt = %d, want %d (legacy closed_at, never wall-clock)", got, endedWant.UnixNano())
		}
		if wallClock, err := asTime(input, "migrationRunWallClock"); err == nil && got == wallClock.UnixNano() {
			return fmt.Errorf("ended transition RecordedAt equals the migration wall-clock time; must trace to legacy closed_at")
		}
	}
	return nil
}

func opMigrateLegacyTaskFabricatedTimestamp(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	system := env.actorFor(t, "pasture-system")
	st := env.tr.(*sqliteTracker)

	lt, owners, _, err := env.parseLegacyTask(t, mustMap(input, "legacyTask"))
	if err != nil {
		return err
	}
	wallClock, err := asTime(input, "migrationRunWallClock")
	if err != nil {
		return err
	}
	in := MigrationInput{System: system, BootstrapAuthority: boot, Owners: owners, Legacy: []LegacyTaskRow{lt}}
	_, err = st.db.AdversarialMigrateFabricatedEndedTimestamp(in, wallClock.UnixNano())
	if err == nil {
		return fmt.Errorf("a fabricated wall-clock ended-transition timestamp was accepted; expected rejection (regression g)")
	}
	if !errors.Is(err, ErrDishonestMigrationTimestamp) {
		return fmt.Errorf("fabricated timestamp rejected with %v, want ErrDishonestMigrationTimestamp", err)
	}
	if anchors, err := st.db.CountBaselineAnchors(); err != nil {
		return err
	} else if anchors != 0 {
		return fmt.Errorf("fabricated-timestamp migration committed %d baseline anchors, want 0", anchors)
	}
	return nil
}

func opMigrateBatchOneUnmappableOwner(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	system := env.actorFor(t, "pasture-system")
	st := env.tr.(*sqliteTracker)

	rows, err := asList(input, "legacyTasks")
	if err != nil {
		return err
	}
	owners := map[string]ActorID{}
	var legacy []LegacyTaskRow
	for _, r := range rows {
		m, err := asMap(r)
		if err != nil {
			return err
		}
		lt, o, _, err := env.parseLegacyTask(t, m)
		if err != nil {
			return err
		}
		for k, v := range o {
			owners[k] = v
		}
		legacy = append(legacy, lt)
	}
	in := MigrationInput{System: system, BootstrapAuthority: boot, Owners: owners, Legacy: legacy}
	_, err = env.tr.Journal().MigrateLegacyBaseline(in)
	if err == nil {
		return fmt.Errorf("migration with an unmappable owner was accepted; expected whole-batch rejection")
	}
	if !errors.Is(err, ErrMigrationOwnerUnmappable) {
		return fmt.Errorf("rejected with %v, want ErrMigrationOwnerUnmappable", err)
	}
	var typed *MigrationOwnerUnmappableError
	if !errors.As(err, &typed) {
		return fmt.Errorf("error is not a typed *MigrationOwnerUnmappableError: %v", err)
	}
	if fld := firstEmptyField(map[string]string{
		"Operation": typed.Operation, "RawOwner": typed.RawOwner, "Stage": typed.Stage,
		"Why": typed.Why, "Impact": typed.Impact, "Fix": typed.Fix,
	}); fld != "" {
		return fmt.Errorf("MigrationOwnerUnmappableError field %q is empty (§13.1 six-field contract)", fld)
	}
	if typed.Task.Namespace == "" {
		return fmt.Errorf("MigrationOwnerUnmappableError Task (what) is empty")
	}
	// No baseline row for ANY task in the run (§13 item 4).
	if anchors, err := st.db.CountBaselineAnchors(); err != nil {
		return err
	} else if anchors != 0 {
		return fmt.Errorf("unmappable-owner migration left %d baseline anchors, want 0 (whole-batch rollback)", anchors)
	}
	return nil
}

func opMigrateThenExtendCompareNativeOnly(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	system := env.actorFor(t, "pasture-system")
	frank := env.actorFor(t, "actor-frank")
	grace := env.actorFor(t, "actor-grace")

	// Migrated history: baseline episode, then a native transfer chaining off it.
	migrated := env.taskFor(t, "t8")
	base := time.Date(2025, 6, 10, 0, 0, 0, 0, time.UTC)
	if _, err := env.tr.Journal().MigrateLegacyBaseline(MigrationInput{
		System: system, BootstrapAuthority: boot,
		Owners: map[string]ActorID{"actor-frank": frank},
		Legacy: []LegacyTaskRow{{ID: migrated, RawOwner: "actor-frank", Status: TaskStatusInProgress, CreatedAt: base, UpdatedAt: base}},
	}); err != nil {
		return fmt.Errorf("migrate t8: %w", err)
	}
	migratedEpisode := MigrationBaselineAssignmentID(migrated)
	if err := env.transfer(t, "op-migrated-transfer", boot, migrated, migratedEpisode, "t8-succ", grace); err != nil {
		return fmt.Errorf("native transfer off migrated episode: %w", err)
	}

	// Native-only equivalent: create, start the first episode, apply the same transfer.
	native := env.taskFor(t, "t8n")
	env.startEpisode(t, "op-native-start", boot, native, "t8n-ep1", frank)
	if err := env.transfer(t, "op-native-transfer", boot, native, "t8n-ep1", "t8n-succ", grace); err != nil {
		return fmt.Errorf("native transfer: %w", err)
	}

	migratedOwner, err := env.tr.Show(migrated)
	if err != nil {
		return err
	}
	nativeOwner, err := env.tr.Show(native)
	if err != nil {
		return err
	}
	if migratedOwner.Owner == nil || migratedOwner.Owner.String() != grace.String() {
		return fmt.Errorf("migrated task owner = %v, want actor-grace", migratedOwner.Owner)
	}
	if nativeOwner.Owner == nil || nativeOwner.Owner.String() != grace.String() {
		return fmt.Errorf("native-only task owner = %v, want actor-grace", nativeOwner.Owner)
	}
	// Observational equivalence beyond RecordedAt provenance (§13.2): both reach the
	// same owner projection through the identical fold.
	return nil
}

func opMigrateThenRemigrateSameSet(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	system := env.actorFor(t, "pasture-system")
	heidi := env.actorFor(t, "actor-heidi")
	st := env.tr.(*sqliteTracker)

	task := env.taskFor(t, "t9")
	base := time.Date(2025, 6, 11, 0, 0, 0, 0, time.UTC)
	in := MigrationInput{
		System: system, BootstrapAuthority: boot,
		Owners: map[string]ActorID{"actor-heidi": heidi},
		Legacy: []LegacyTaskRow{{ID: task, RawOwner: "actor-heidi", Status: TaskStatusOpen, CreatedAt: base, UpdatedAt: base}},
	}
	first, err := env.tr.Journal().MigrateLegacyBaseline(in)
	if err != nil {
		return fmt.Errorf("first migration: %w", err)
	}
	if first.BaselineAnchorsCreated != 1 {
		return fmt.Errorf("first run created %d anchors, want 1", first.BaselineAnchorsCreated)
	}
	// Re-run the same legacy set: the deterministic OperationID hits §9.4's
	// idempotent short-circuit, producing zero duplicate baseline anchors.
	second, err := env.tr.Journal().MigrateLegacyBaseline(in)
	if err != nil {
		return fmt.Errorf("second migration: %w", err)
	}
	if second.BaselineAnchorsCreated != 0 || second.ShortCircuited != 1 {
		return fmt.Errorf("second run = {created:%d shortCircuited:%d}, want {0 1}", second.BaselineAnchorsCreated, second.ShortCircuited)
	}
	if anchors, err := st.db.CountBaselineAnchors(); err != nil {
		return err
	} else if anchors != 1 {
		return fmt.Errorf("after re-migration there are %d baseline anchors, want 1 (no duplicates)", anchors)
	}
	return nil
}

// ---------------------------------------------------------------------------
// external-schema preflight / topology corruption (§13)
// ---------------------------------------------------------------------------

func opSchemaPreflight(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	if err := env.tr.Journal().PreflightSchema(); err != nil {
		return fmt.Errorf("preflight failed on an exact-match schema: %w", err)
	}
	return nil
}

func opSchemaPreflightMissingTable(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	st := env.tr.(*sqliteTracker)
	if err := st.db.AdversarialDropTable("journal"); err != nil {
		return fmt.Errorf("corrupt schema (drop journal): %w", err)
	}
	return expectPreflightFailure(env.tr.Journal().PreflightSchema(), "a missing journal table")
}

func opSchemaPreflightExtraColumn(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	st := env.tr.(*sqliteTracker)
	if err := st.db.AdversarialAddColumn("journal_task_events", "unreviewed"); err != nil {
		return fmt.Errorf("corrupt schema (extra column): %w", err)
	}
	return expectPreflightFailure(env.tr.Journal().PreflightSchema(), "an unexpected extra column")
}

func opSchemaPreflightMissingColumn(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	st := env.tr.(*sqliteTracker)
	if err := st.db.AdversarialDropColumn("journal_task_events", "payload"); err != nil {
		return fmt.Errorf("corrupt schema (drop expected column): %w", err)
	}
	return expectPreflightFailure(env.tr.Journal().PreflightSchema(), "a missing expected column")
}

func opFaultMidMigration(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	system := env.actorFor(t, "pasture-system")
	st := env.tr.(*sqliteTracker)

	n, err := asInt(input, "legacyTaskCount")
	if err != nil {
		return err
	}
	faultAfter, err := asInt(input, "faultAfterBaselineIndex")
	if err != nil {
		return err
	}
	var legacy []LegacyTaskRow
	base := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		task := env.taskFor(t, fmt.Sprintf("fault-legacy-%d", i))
		legacy = append(legacy, LegacyTaskRow{ID: task, Status: TaskStatusOpen, CreatedAt: base.Add(time.Duration(i) * time.Hour), UpdatedAt: base.Add(time.Duration(i) * time.Hour)})
	}
	in := MigrationInput{System: system, BootstrapAuthority: boot, Legacy: legacy}
	_, err = st.db.AdversarialMigrateWithFault(in, faultAfter)
	if err == nil {
		return fmt.Errorf("a fault mid-migration did not abort; expected whole-batch rollback")
	}
	if !errors.Is(err, ErrMigrationFault) {
		return fmt.Errorf("mid-migration fault surfaced %v, want ErrMigrationFault", err)
	}
	if !errorIsActionable(err.Error()) {
		return fmt.Errorf("mid-migration fault error is not actionable (missing why/where/when/impact/fix): %v", err)
	}
	// The whole transaction — every baseline already written in this run — rolled
	// back atomically (§9.5, §13).
	if anchors, err := st.db.CountBaselineAnchors(); err != nil {
		return err
	} else if anchors != 0 {
		return fmt.Errorf("mid-migration fault left %d baseline anchors, want 0 (whole-batch rollback)", anchors)
	}
	return nil
}

func opConcurrentOpenDuringCorruption(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	st := env.tr.(*sqliteTracker)
	if err := st.db.AdversarialAddColumn("journal_task_events", "unreviewed"); err != nil {
		return fmt.Errorf("corrupt schema: %w", err)
	}
	// Two concurrent opens race against the corrupted topology; both must observe
	// the same fail-closed outcome (§13). Replay runs preflight first.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = env.tr.Journal().ReplayProjections()
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if err := expectPreflightFailure(e, fmt.Sprintf("concurrent open %d against a corrupted schema", i)); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// reopen / transfer-crash (§8.1, §9.5)
// ---------------------------------------------------------------------------

func opReopenTask(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	env.startEpisode(t, "op-seed-A", boot, task, "A", occupant)
	// Close the task (end episode A + close event).
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-close-for-reopen", ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{
			{Sort: EffectAssignmentEnd, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility},
			{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKindTaskClosed},
		},
	}); err != nil {
		return fmt.Errorf("close before reopen: %w", err)
	}
	// Reopen: a lifecycle reopen event, no assignment. It must not resurrect the
	// ended episode or start a new one.
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-reopen-1"), ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("rc"), MutationDigest: env.digest("rm"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKindTaskReopened}},
	}); err != nil {
		return fmt.Errorf("reopen operation: %w", err)
	}
	got, err := env.tr.Show(task)
	if err != nil {
		return err
	}
	if got.Status != StatusOpen {
		return fmt.Errorf("reopened task status = %v, want open", got.Status)
	}
	if got.Owner != nil {
		return fmt.Errorf("reopened task has owner %v, want nil (stays unassigned)", got.Owner)
	}
	st := env.tr.(*sqliteTracker)
	episodes, err := st.db.CountEpisodesForTask(task)
	if err != nil {
		return err
	}
	if episodes != 1 {
		return fmt.Errorf("reopen created a new episode (found %d, want 1 — the prior ended one)", episodes)
	}
	return nil
}

func opFaultBetweenTransferEndedAndStarted(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	env.startEpisode(t, "op-seed-A", boot, task, "A", occupant)
	op := opID(input, "op-transfer-crash-1")
	st := env.tr.(*sqliteTracker)

	transfer := OperationInput{
		OperationID: op, ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{
			{Sort: EffectAssignmentEnd, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility},
			{Sort: EffectAssignmentStart, AssignmentID: "B", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant, Predecessor: "A"},
		},
	}
	// Fault injected between the ended (effect 0) and started (effect 1) rows: the
	// transfer is all-or-nothing (§9.5), so neither row nor the anchor survives.
	if _, err := st.db.AdversarialApplyWithFault(transfer, 0); !errors.Is(err, ErrInjectedFault) {
		return fmt.Errorf("faulted transfer = %v, want ErrInjectedFault", err)
	}
	r, err := env.tr.Journal().LookupCommitted(op)
	if err != nil {
		return err
	}
	if r.Kind != CommittedAbsent {
		return fmt.Errorf("faulted transfer left committed state (anchor created): %+v", r)
	}
	// Episode A remains active (its ended transition rolled back); the loser wrote
	// no successor.
	successors, err := st.db.CountSuccessorEpisodes(task)
	if err != nil {
		return err
	}
	if successors != 0 {
		return fmt.Errorf("faulted transfer left %d successor episodes, want 0", successors)
	}
	got, err := env.tr.Show(task)
	if err != nil {
		return err
	}
	if got.Owner == nil || got.Owner.String() != occupant.String() {
		return fmt.Errorf("faulted transfer changed the owner to %v, want the original occupant", got.Owner)
	}
	return nil
}

// ---------------------------------------------------------------------------
// replay idempotency (§9.4)
// ---------------------------------------------------------------------------

func opReplayWithEndedAuthority(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	// An assignment authority governs its own episode's task.
	auth := env.startEpisode(t, "op-seed-auth", boot, task, "AUTH", occupant)
	op := opID(input, "op-review-1")
	review := OperationInput{
		OperationID: op, ActorID: env.actor, AuthorityJournalID: &auth,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.review.recorded"}},
	}
	if _, err := env.tr.Journal().Apply(review); err != nil {
		return fmt.Errorf("original review under assignment authority: %w", err)
	}
	// End the episode, so the authority the review used has since ended.
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-end-auth", ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("ec"), MutationDigest: env.digest("em"),
		Effects: []Effect{{Sort: EffectAssignmentEnd, AssignmentID: "AUTH", TaskID: task, SlotID: SlotOwnerResponsibility}},
	}); err != nil {
		return fmt.Errorf("end the authority episode: %w", err)
	}
	// Sanity: a NEW operation under the now-ended authority is rejected.
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-new-under-ended-auth", ActorID: env.actor, AuthorityJournalID: &auth,
		CommandDigest: env.digest("nc"), MutationDigest: env.digest("nm"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.review.recorded"}},
	}); !errors.Is(err, ErrAuthorityScope) {
		return fmt.Errorf("a new op under an ended authority = %v, want ErrAuthorityScope", err)
	}
	// Exact replay of the original op must still succeed via §9.4's short-circuit,
	// which skips the current-authority check entirely.
	res, err := env.tr.Journal().Apply(review)
	if err != nil {
		return fmt.Errorf("exact replay after the authority ended was rejected: %w", err)
	}
	if !res.ShortCircuited {
		return fmt.Errorf("exact replay was re-executed rather than short-circuited (§9.4)")
	}
	return nil
}

func opReplayZeroTaskEventOperation(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	op := opID(input, "op-assign-only-1")
	assign := OperationInput{
		OperationID: op, ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectAssignmentStart, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant}},
	}
	res1, err := env.tr.Journal().Apply(assign)
	if err != nil {
		return fmt.Errorf("first zero-task-event apply: %w", err)
	}
	res2, err := env.tr.Journal().Apply(assign) // identical four-field identity
	if err != nil {
		return fmt.Errorf("replay rejected instead of short-circuiting: %w", err)
	}
	if !res2.ShortCircuited {
		return fmt.Errorf("replay was re-executed rather than short-circuited (§9.4)")
	}
	if res2.AnchorJournalID != res1.AnchorJournalID {
		return fmt.Errorf("replay returned anchor %d, want the original %d", res2.AnchorJournalID, res1.AnchorJournalID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Shared helpers for the S1.3 operators
// ---------------------------------------------------------------------------

// parseLegacyTask maps a symbolic corpus legacyTask into a concrete created task
// plus the owner→ActorID resolution map. The task is created so migration's
// task-scoped foreign keys resolve; a non-null owner registers a real actor.
func (e *opsEnv) parseLegacyTask(t *testing.T, m anyMap) (LegacyTaskRow, map[string]ActorID, string, error) {
	if m == nil {
		return LegacyTaskRow{}, nil, "", fmt.Errorf("legacyTask map is nil")
	}
	label, err := asString(m, "id")
	if err != nil {
		return LegacyTaskRow{}, nil, "", err
	}
	task := e.taskFor(t, "legacy-"+label)
	updatedAt, err := asTime(m, "updatedAt")
	if err != nil {
		return LegacyTaskRow{}, nil, "", err
	}
	lt := LegacyTaskRow{ID: task, Status: parseTaskStatus(m), CreatedAt: updatedAt, UpdatedAt: updatedAt}
	if closedAt, err := asTime(m, "closedAt"); err == nil {
		c := closedAt
		lt.ClosedAt = &c
	}
	owners := map[string]ActorID{}
	if raw, ok := m["owner"].(string); ok && raw != "" {
		lt.RawOwner = raw
		// Register only owner strings that follow the corpus's registered-actor
		// convention (an "actor-" prefix); a deliberately unregistered legacy owner
		// (e.g. orphaned free-text) is left unmapped so migration fails closed (§13).
		if strings.HasPrefix(raw, "actor-") {
			owners[raw] = e.actorFor(t, raw)
		}
	}
	return lt, owners, label, nil
}

// transfer applies a responsibility transfer: end the old episode and start a new
// successor episode chaining off it, in one operation (§4.4).
func (e *opsEnv) transfer(t *testing.T, opStr string, auth JournalID, task TaskID, from, to AssignmentID, occupant ActorID) error {
	_, err := e.tr.Journal().Apply(OperationInput{
		OperationID: OperationID(opStr), ActorID: e.actor, AuthorityJournalID: &auth,
		CommandDigest: e.digest(opStr + "c"), MutationDigest: e.digest(opStr + "m"),
		Effects: []Effect{
			{Sort: EffectAssignmentEnd, AssignmentID: from, TaskID: task, SlotID: SlotOwnerResponsibility},
			{Sort: EffectAssignmentStart, AssignmentID: to, TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant, Predecessor: from},
		},
	})
	return err
}

func parseTaskStatus(m anyMap) TaskStatus {
	s, _ := m["status"].(string)
	switch s {
	case "closed":
		return TaskStatusClosed
	case "in_progress":
		return TaskStatusInProgress
	default:
		return TaskStatusOpen
	}
}

// s13SeedActorTask registers an actor and creates a task on a bare tracker (used
// by the file-based reopen operator, which cannot reuse the OpenMemory env).
func s13SeedActorTask(t *testing.T, tr Tracker, label string) (ActorID, TaskID) {
	t.Helper()
	agent, err := tr.RegisterSoftwareAgent("provenance-test", "s13-"+label, "0", "test")
	if err != nil {
		t.Fatalf("register actor: %v", err)
	}
	task, err := tr.Create("provenance-test", "task "+label, "", TaskTypeTask, PriorityMedium, PhaseUnscoped)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return agent.ID, task.ID
}

func s13Genesis(t *testing.T, tr Tracker, actor ActorID) JournalID {
	t.Helper()
	res, err := tr.Journal().Apply(OperationInput{
		OperationID: "op-genesis", ActorID: actor,
		CommandDigest: []byte("gc"), MutationDigest: []byte("gm"),
		Effects: []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("genesis: %v", err)
	}
	for _, b := range res.ResultSlots {
		if string(b.Slot) == "auth" {
			return b.ProducedJournalID
		}
	}
	t.Fatalf("genesis produced no bootstrap authority")
	return 0
}

func s13StartOwner(t *testing.T, tr Tracker, boot JournalID, actor ActorID, task TaskID, assignment AssignmentID, occupant ActorID) {
	t.Helper()
	if _, err := tr.Journal().Apply(OperationInput{
		OperationID: OperationID("op-start-" + string(assignment)), ActorID: actor, AuthorityJournalID: &boot,
		CommandDigest: []byte("sc"), MutationDigest: []byte("sm"),
		Effects: []Effect{{Sort: EffectAssignmentStart, AssignmentID: assignment, TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant}},
	}); err != nil {
		t.Fatalf("start owner episode: %v", err)
	}
}

func expectPreflightFailure(err error, scenario string) error {
	if err == nil {
		return fmt.Errorf("%s was accepted; expected a fail-closed SchemaPreflightError", scenario)
	}
	if !errors.Is(err, ErrSchemaPreflight) {
		return fmt.Errorf("%s rejected with %v, want ErrSchemaPreflight", scenario, err)
	}
	var typed *SchemaPreflightError
	if !errors.As(err, &typed) {
		return fmt.Errorf("%s: error is not a typed *SchemaPreflightError: %v", scenario, err)
	}
	if fld := firstEmptyField(map[string]string{
		"Operation": typed.Operation, "ExpectedShape": typed.ExpectedShape, "FoundShape": typed.FoundShape,
		"Stage": typed.Stage, "Why": typed.Why, "Impact": typed.Impact, "Fix": typed.Fix,
	}); fld != "" {
		return fmt.Errorf("%s: SchemaPreflightError field %q is empty (§13.1 six-field contract)", scenario, fld)
	}
	return nil
}

func firstEmptyField(fields map[string]string) string {
	for name, val := range fields {
		if strings.TrimSpace(val) == "" {
			return name
		}
	}
	return ""
}

// errorIsActionable checks a fail-closed error message carries the six-part
// actionable-error components (what is the leading description; the rest are
// labelled), per the repo's actionable-error contract.
func errorIsActionable(msg string) bool {
	for _, marker := range []string{"why:", "where:", "when:", "impact:", "fix:"} {
		if !strings.Contains(msg, marker) {
			return false
		}
	}
	return true
}

// asBoolOK returns a bool field and whether it was present.
func asBoolOK(m anyMap, key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}
