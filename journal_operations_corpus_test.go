package provenance

// journal_operations_corpus_test.go executes operation-lifecycle proof-corpus
// histories — operations, effects, results, and authority lifecycle
// (docs/journal-relational-contract.md §2-§4, §9, §14) — against the real Apply
// / LookupCommitted production path. Each operator translates the symbolic
// corpus data (task/actor/assignment labels) into concrete registered IDs and
// drives the production reducer, honouring the case's must-pass/must-fail
// classification.

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/testcorpus"
)

// operationLifecycleOperators is the closed registry for operation behavior.
var operationLifecycleOperators = map[testcorpus.OperatorName]corpusHandler{
	// genesis_bootstrap.yaml (§4.6, §10 rules 6-7)
	"apply-genesis-operation":              opApplyGenesisOperation,
	"apply-null-authority-after-genesis":   opApplyNullAuthorityAfterGenesis,
	"apply-genesis-retry-same-operationid": opApplyGenesisRetrySameOperationID,
	"apply-second-genesis":                 opApplySecondGenesis,
	"apply-genesis-with-extra-effect":      opApplyGenesisWithExtraEffect,
	// operation_results.yaml (§3.2, §10 rule 9)
	"lookup-committed-reconstruct-result-slots":        opLookupCommittedReconstructResultSlots,
	"lookup-committed-emitted-events-from-closure":     opLookupCommittedEmittedEventsFromClosure,
	"reject-result-slot-referencing-foreign-operation": opRejectResultSlotForeignOperation,
	// authority_evidence.yaml (§9.3, §14)
	"fold-sequentially-check-effect2-sees-effect1":            opFoldSequentiallyEffect2SeesEffect1,
	"start-with-orphaned-predecessor":                         opStartWithOrphanedPredecessor,
	"two-successors-same-predecessor":                         opTwoSuccessorsSamePredecessor,
	"authority-unrelated":                                     opAuthorityUnrelated,
	"blocked-by-scheduling-edge-does-not-grant-authority":     opBlockedByEdgeDoesNotGrantAuthority,
	"transitive-parent-citation-grants-authority":             opTransitiveParentCitationGrantsAuthority,
	"parent-chain-middle-episode-ended-cuts-authority":        opParentChainMiddleEndedCutsAuthority,
	"parent-chain-middle-ended-same-operation-cuts-authority": opParentChainMiddleEndedSameOperationCutsAuthority,
	"citation-of-inactive-parent-at-start-rejected":           opCitationOfInactiveParentRejected,
	"corrupted-cyclic-parent-chain-fails-closed":              opCorruptedCyclicParentChainFailsClosed,
	"produce-subordinate-row-carrying-actor":                  opProduceSubordinateRowCarryingActor,
	"end-episode-never-started":                               opEndEpisodeNeverStarted,
	"batch-ended-before-started":                              opBatchEndedBeforeStarted,
	// owner_responsibility.yaml (§4.4, §8.1, §8.2, §9.6)
	"close-task-with-active-assignment":        opCloseTaskWithActiveAssignment,
	"close-task-omit-ended-transition":         opCloseTaskOmitEndedTransition,
	"concurrent-transfer-cas":                  opConcurrentTransferCAS,
	"attribute-episode-occupant-not-committer": opAttributeEpisodeOccupant,
	// zero_event_operations.yaml (§10 rule 1, §14.1)
	"apply-zero-task-event-operation":   opApplyZeroTaskEventOperation,
	"apply-empty-batch":                 opApplyEmptyBatch,
	"authority-precedence-by-journalid": opAuthorityPrecedenceByJournalID,
	"lookup-committed":                  opLookupCommittedAbsent,
	// ordering.yaml (§9.3, §12)
	"authorize-using-recordedat-not-journalid": opAuthorizeUsingRecordedAtNotJournalID,
	// subtype_integrity.yaml (§10 rule 8)
	"write-journal-row-two-subtypes":                   opWriteJournalRowTwoSubtypes,
	"write-subtype-mismatching-journalkind":            opWriteSubtypeMismatchingKind,
	"write-authority-detail-mismatching-authoritykind": opWriteAuthorityDetailMismatch,
}

// ---------------------------------------------------------------------------
// Operations environment: symbolic-label → concrete-ID resolution
// ---------------------------------------------------------------------------

type opsEnv struct {
	*journalEnv
	actors map[string]ActorID
	tasks  map[string]TaskID
	boot   *JournalID
	seq    int
}

func newOpsEnv(t *testing.T) *opsEnv {
	return &opsEnv{journalEnv: newJournalEnv(t), actors: map[string]ActorID{}, tasks: map[string]TaskID{}}
}

func (e *opsEnv) actorFor(t *testing.T, label string) ActorID {
	if label == "" {
		return e.actor
	}
	if a, ok := e.actors[label]; ok {
		return a
	}
	e.seq++
	sa, err := e.tr.RegisterSoftwareAgent("provenance-test", fmt.Sprintf("actor-%s-%d", label, e.seq), "0", "test")
	if err != nil {
		t.Fatalf("register actor %q: %v", label, err)
	}
	e.actors[label] = sa.ID
	return sa.ID
}

// taskFor returns the concrete TaskID for a symbolic label, journaling the birth
// of a fresh native task through Apply (EffectTaskCreate) on first use (§8.1). The
// creation runs under a single shared genesis bootstrap authority the env
// lazily establishes (ensureBoot), so a task is born through the journal exactly as
// production code will birth it once the direct-write Tracker.Create is retired.
func (e *opsEnv) taskFor(t *testing.T, label string) TaskID {
	if label == "" {
		return e.task
	}
	if tk, ok := e.tasks[label]; ok {
		return tk
	}
	boot := e.ensureBoot(t)
	id := newCorpusTaskID()
	e.seq++
	if _, err := e.tr.Journal().Apply(OperationInput{
		OperationID:        OperationID(fmt.Sprintf("op-taskcreate-%s-%d", label, e.seq)),
		ActorID:            e.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      e.digest("create-" + label + "-c"),
		MutationDigest:     e.digest("create-" + label + "-m"),
		Effects: []Effect{{
			Sort:     EffectTaskCreate,
			TaskID:   id,
			Title:    "task " + label,
			Type:     TaskTypeTask,
			Priority: PriorityMedium,
			Phase:    PhaseUnscoped,
		}},
	}); err != nil {
		t.Fatalf("journaled create task %q: %v", label, err)
	}
	e.tasks[label] = id
	return id
}

// ensureBoot lazily establishes the single genesis bootstrap authority shared by
// journaled task creation and any operator that needs a governing authority,
// returning its produced JournalID. It is idempotent: the genesis operation is
// applied at most once per env (a second genesis against a non-empty journal is a
// discipline violation, §4.6), so operators and taskFor coexist on one root.
func (e *opsEnv) ensureBoot(t *testing.T) JournalID {
	if e.boot != nil {
		return *e.boot
	}
	res, err := e.tr.Journal().Apply(OperationInput{
		OperationID:    "op-genesis-shared",
		ActorID:        e.actor,
		CommandDigest:  e.digest("genesis-shared-c"),
		MutationDigest: e.digest("genesis-shared-m"),
		Effects:        []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("ensureBoot genesis: %v", err)
	}
	jid, ok := slotJournalID(res, "auth")
	if !ok {
		t.Fatalf("ensureBoot genesis produced no bootstrap authority slot")
	}
	e.boot = &jid
	return jid
}

// seedLegacyTask writes one pre-journal (OLD-schema) task row via the raw seeding
// seam (§13), the corpus analogue of a legacy database on disk. Legacy tasks are
// NEVER journal-created — doing so would collapse the migration-vs-native contrast
// (§13.2) — so migration operators seed their legacy inputs here, not via taskFor.
func (e *opsEnv) seedLegacyTask(t *testing.T, lt LegacyTaskRow) {
	st, ok := e.tr.(*sqliteTracker)
	if !ok {
		t.Fatalf("seedLegacyTask: tracker is not *sqliteTracker")
	}
	if err := st.db.SeedLegacyTask(lt); err != nil {
		t.Fatalf("seed legacy task %q: %v", lt.ID, err)
	}
}

func (e *opsEnv) digest(s string) []byte { return []byte("digest--" + s) }

// genesis establishes the pasture-system bootstrap authority via one genesis
// operation (§4.6) and returns its produced authority JournalID, which as the
// system root governs every task.
func (e *opsEnv) genesis(t *testing.T, opID string) JournalID {
	// The env holds a single genesis root shared with journaled task creation
	// (§4.6): establishing it is idempotent, so an operator that also births tasks
	// via taskFor never trips the second-genesis discipline. opID is retained for
	// call-site readability; the shared root's identity is fixed by ensureBoot.
	_ = opID
	return e.ensureBoot(t)
}

func slotJournalID(res CommittedResult, slot string) (JournalID, bool) {
	for i := range res.ResultSlots {
		if string(res.ResultSlots[i].Slot) == slot {
			return res.ResultSlots[i].ProducedJournalID, true
		}
	}
	return 0, false
}

// startEpisode applies a zero-task-event operation that starts one
// owner-responsibility episode, returning the started transition's authority
// JournalID (via a result slot) for use as a downstream operation authority.
func (e *opsEnv) startEpisode(t *testing.T, opID string, auth JournalID, task TaskID, assignment AssignmentID, occupant ActorID) JournalID {
	res, err := e.tr.Journal().Apply(OperationInput{
		OperationID:        OperationID(opID),
		ActorID:            e.actor,
		AuthorityJournalID: &auth,
		CommandDigest:      e.digest(opID + "c"),
		MutationDigest:     e.digest(opID + "m"),
		Effects: []Effect{{
			Sort: EffectAssignmentStart, AssignmentID: assignment, TaskID: task,
			SlotID: SlotOwnerResponsibility, Occupant: occupant, ResultSlot: "auth",
		}},
	})
	if err != nil {
		t.Fatalf("startEpisode %q: %v", opID, err)
	}
	jid, ok := slotJournalID(res, "auth")
	if !ok {
		t.Fatalf("startEpisode %q produced no authority slot", opID)
	}
	return jid
}

// startEpisodeWithParent starts an owner-responsibility episode that cites
// `parent` as its governing parent episode (§14.5). The start is authorized by
// `auth` (typically the bootstrap authority, which governs every task); the
// parent citation is separate metadata declared at start. Returns the started
// transition's authority JournalID.
func (e *opsEnv) startEpisodeWithParent(t *testing.T, opID string, auth JournalID, task TaskID, assignment, parent AssignmentID, occupant ActorID) JournalID {
	res, err := e.tr.Journal().Apply(OperationInput{
		OperationID:        OperationID(opID),
		ActorID:            e.actor,
		AuthorityJournalID: &auth,
		CommandDigest:      e.digest(opID + "c"),
		MutationDigest:     e.digest(opID + "m"),
		Effects: []Effect{{
			Sort: EffectAssignmentStart, AssignmentID: assignment, TaskID: task,
			SlotID: SlotOwnerResponsibility, Occupant: occupant, Parent: parent, ResultSlot: "auth",
		}},
	})
	if err != nil {
		t.Fatalf("startEpisodeWithParent %q: %v", opID, err)
	}
	jid, ok := slotJournalID(res, "auth")
	if !ok {
		t.Fatalf("startEpisodeWithParent %q produced no authority slot", opID)
	}
	return jid
}

// endEpisode ends an owner-responsibility episode on `task` under authority
// `auth`.
func (e *opsEnv) endEpisode(t *testing.T, opID string, auth JournalID, task TaskID, assignment AssignmentID) {
	if _, err := e.tr.Journal().Apply(OperationInput{
		OperationID:        OperationID(opID),
		ActorID:            e.actor,
		AuthorityJournalID: &auth,
		CommandDigest:      e.digest(opID + "c"),
		MutationDigest:     e.digest(opID + "m"),
		Effects: []Effect{{
			Sort: EffectAssignmentEnd, AssignmentID: assignment, TaskID: task, SlotID: SlotOwnerResponsibility,
		}},
	}); err != nil {
		t.Fatalf("endEpisode %q: %v", opID, err)
	}
}

func opID(input anyMap, fallback string) OperationID {
	if s, err := asString(input, "operationId"); err == nil && s != "" {
		return OperationID(s)
	}
	return OperationID(fallback)
}

// ---------------------------------------------------------------------------
// genesis_bootstrap.yaml
// ---------------------------------------------------------------------------

func opApplyGenesisOperation(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	op := opID(input, "op-genesis-1")
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID:    op,
		ActorID:        env.actor,
		CommandDigest:  env.digest("cmd"),
		MutationDigest: env.digest("mut"),
		Effects:        []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		return fmt.Errorf("genesis rejected: %w", err)
	}
	if res.Kind != CommittedExact || res.AnchorJournalID == 0 {
		return fmt.Errorf("genesis produced no anchor: %+v", res)
	}
	authJID, ok := slotJournalID(res, "auth")
	if !ok {
		return fmt.Errorf("genesis produced no bootstrap authority")
	}
	// The bootstrap authority is produced by the genesis anchor (§4.6).
	st := env.tr.(*sqliteTracker)
	governs, err := st.db.AuthorityGovernsTaskAt(authJID, env.task, res.AnchorJournalID+1000)
	if err != nil {
		return err
	}
	if !governs {
		return fmt.Errorf("bootstrap authority does not govern as the system root")
	}
	return nil
}

func opApplyNullAuthorityAfterGenesis(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	env.genesis(t, "op-genesis-seed")
	task := env.taskFor(t, "t1")
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:    opID(input, "op-null-auth-late"),
		ActorID:        env.actor,
		CommandDigest:  env.digest("c"),
		MutationDigest: env.digest("m"),
		Effects:        []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.created"}},
	})
	return expectRejected(err, ErrGenesis, "null authority on a non-genesis operation")
}

func opApplyGenesisRetrySameOperationID(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	op := opID(input, "op-genesis-1")
	first := OperationInput{
		OperationID:    op,
		ActorID:        env.actor,
		CommandDigest:  env.digest("cmd"),
		MutationDigest: env.digest("mut"),
		Effects:        []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	}
	res1, err := env.tr.Journal().Apply(first)
	if err != nil {
		return fmt.Errorf("first genesis: %w", err)
	}
	res2, err := env.tr.Journal().Apply(first) // identical four-field identity
	if err != nil {
		return fmt.Errorf("genesis retry rejected instead of short-circuiting: %w", err)
	}
	if !res2.ShortCircuited {
		return fmt.Errorf("genesis retry was re-executed rather than short-circuited (§9.4)")
	}
	if res2.AnchorJournalID != res1.AnchorJournalID {
		return fmt.Errorf("retry returned a different anchor %d, want original %d", res2.AnchorJournalID, res1.AnchorJournalID)
	}
	n, err := countBootstrapAuthorities(env)
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("after genesis retry there are %d bootstrap authorities, want 1", n)
	}
	return nil
}

func opApplySecondGenesis(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	env.genesis(t, "op-genesis-1")
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:    opID(input, "op-genesis-2"),
		ActorID:        env.actor,
		CommandDigest:  env.digest("rogue-c"),
		MutationDigest: env.digest("rogue-m"),
		Effects:        []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "rogue-bootstrap"}},
	})
	return expectRejected(err, ErrGenesis, "a second genesis against a non-empty journal")
}

func opApplyGenesisWithExtraEffect(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	task := env.taskFor(t, "t1")
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:    opID(input, "op-genesis-bad-effect"),
		ActorID:        env.actor,
		CommandDigest:  env.digest("c"),
		MutationDigest: env.digest("m"),
		Effects: []Effect{
			{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system"},
			{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.created"},
		},
	})
	return expectRejected(err, ErrGenesis, "a genesis producing a non-bootstrap effect")
}

// ---------------------------------------------------------------------------
// operation_results.yaml
// ---------------------------------------------------------------------------

func opLookupCommittedReconstructResultSlots(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	taskA := env.taskFor(t, "t-alloc-a")
	taskB := env.taskFor(t, "t-alloc-b")
	op := opID(input, "op-alloc-2-tasks")
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        op,
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("c"),
		MutationDigest:     env.digest("m"),
		Effects: []Effect{
			{Sort: EffectTaskEvent, TaskID: taskA, EventKind: "provenance.task.created", ResultSlot: "new-task-1"},
			{Sort: EffectTaskEvent, TaskID: taskB, EventKind: "provenance.task.created", ResultSlot: "new-task-2"},
		},
	})
	if err != nil {
		return fmt.Errorf("alloc operation rejected: %w", err)
	}
	res, err := env.tr.Journal().LookupCommitted(op)
	if err != nil {
		return fmt.Errorf("lookup: %w", err)
	}
	if res.Kind != CommittedExact {
		return fmt.Errorf("lookup variant = %s, want CommittedExact", res.Kind)
	}
	want := map[string]TaskID{"new-task-1": taskA, "new-task-2": taskB}
	got := map[string]TaskID{}
	for _, b := range res.ResultSlots {
		if b.TaskID == nil {
			return fmt.Errorf("slot %q resolved to no task", b.Slot)
		}
		got[string(b.Slot)] = *b.TaskID
	}
	for slot, wantTask := range want {
		if got[slot].String() != wantTask.String() {
			return fmt.Errorf("slot %q reconstructed to %v, want %v", slot, got[slot], wantTask)
		}
	}
	return nil
}

func opLookupCommittedEmittedEventsFromClosure(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t-emit")
	op := opID(input, "op-emit-events-1")
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        op,
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("c"),
		MutationDigest:     env.digest("m"),
		Effects: []Effect{
			{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated"},
			{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated"},
			{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated"},
		},
	})
	if err != nil {
		return fmt.Errorf("emit operation rejected: %w", err)
	}
	res, err := env.tr.Journal().LookupCommitted(op)
	if err != nil {
		return fmt.Errorf("lookup: %w", err)
	}
	if len(res.EmittedEvents) != 3 {
		return fmt.Errorf("EmittedEvents = %d, want 3 (from produced closure)", len(res.EmittedEvents))
	}
	for i := 1; i < len(res.EmittedEvents); i++ {
		if res.EmittedEvents[i] <= res.EmittedEvents[i-1] {
			return fmt.Errorf("EmittedEvents not in ascending JournalID order: %v", res.EmittedEvents)
		}
	}
	if len(res.ResultSlots) != 0 {
		return fmt.Errorf("EmittedEvents needed %d slot rows, want 0 (flat closure only)", len(res.ResultSlots))
	}
	return nil
}

func opRejectResultSlotForeignOperation(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t-foreign")
	// op-b (the producing operation) produces a row that op-a will try to borrow.
	resB, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        "op-b",
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("bc"),
		MutationDigest:     env.digest("bm"),
		Effects:            []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated"}},
	})
	if err != nil {
		return fmt.Errorf("op-b rejected: %w", err)
	}
	if len(resB.EmittedEvents) == 0 {
		return fmt.Errorf("op-b produced no row to borrow")
	}
	foreignRow := resB.EmittedEvents[0]
	// op-a is a distinct operation.
	resA, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        "op-a",
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("ac"),
		MutationDigest:     env.digest("am"),
		Effects:            []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated"}},
	})
	if err != nil {
		return fmt.Errorf("op-a rejected: %w", err)
	}
	st := env.tr.(*sqliteTracker)
	err = st.db.AdversarialForeignResultSlotRejected(resA.AnchorJournalID, foreignRow)
	return expectRejected(err, ErrResultSlotIntegrity, "a result slot referencing a foreign operation's produced row")
}

// ---------------------------------------------------------------------------
// authority_evidence.yaml
// ---------------------------------------------------------------------------

func opFoldSequentiallyEffect2SeesEffect1(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	env.startEpisode(t, "op-seed-A", boot, task, "A", occupant)
	// One operation batches: (1) end A, (2) start B predecessor A. Effect 2 must
	// see effect 1's just-ended A (§9.3), or it would fail the orphan check.
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        opID(input, "op-transfer-1"),
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("c"),
		MutationDigest:     env.digest("m"),
		Effects: []Effect{
			{Sort: EffectAssignmentEnd, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility},
			{Sort: EffectAssignmentStart, AssignmentID: "B", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant, Predecessor: "A"},
		},
	})
	if err != nil {
		return fmt.Errorf("sequential transfer rejected (effect 2 did not see effect 1): %w", err)
	}
	if res.Kind != CommittedExact {
		return fmt.Errorf("transfer did not commit: %+v", res)
	}
	return nil
}

func opStartWithOrphanedPredecessor(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	// C is active (started, never ended) — an orphaned (not-ended) predecessor.
	env.startEpisode(t, "op-seed-C", boot, task, "C", occupant)
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        opID(input, "op-orphan-1"),
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("c"),
		MutationDigest:     env.digest("m"),
		Effects: []Effect{{
			Sort: EffectAssignmentStart, AssignmentID: "D", TaskID: task,
			SlotID: SlotOwnerResponsibility, Occupant: occupant, Predecessor: "C",
		}},
	})
	return expectRejected(err, ErrOrphanedEvidence, "a start naming a not-ended predecessor")
}

func opTwoSuccessorsSamePredecessor(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	// A is started then ended, so it is a legal (ended) predecessor.
	env.startEpisode(t, "op-seed-A", boot, task, "A", occupant)
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-end-A", ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("ec"), MutationDigest: env.digest("em"),
		Effects: []Effect{{Sort: EffectAssignmentEnd, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility}},
	}); err != nil {
		return fmt.Errorf("end A: %w", err)
	}
	// First successor E consumes A.
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-succ-E", ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("e1c"), MutationDigest: env.digest("e1m"),
		Effects: []Effect{{Sort: EffectAssignmentStart, AssignmentID: "E", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant, Predecessor: "A"}},
	}); err != nil {
		return fmt.Errorf("first successor E must be accepted: %w", err)
	}
	// Second successor F also claims A — must be rejected (§14.2).
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-double-consume-1"), ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("fc"), MutationDigest: env.digest("fm"),
		Effects: []Effect{{Sort: EffectAssignmentStart, AssignmentID: "F", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant, Predecessor: "A"}},
	})
	return expectRejected(err, ErrOrphanedEvidence, "a predecessor consumed by a second successor")
}

func opAuthorityUnrelated(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	target := env.taskFor(t, "t1")
	authTask := env.taskFor(t, "t2")
	occupant := env.actorFor(t, "occ")
	auth := env.startEpisode(t, "op-seed-auth", boot, authTask, "AUTH", occupant)
	// Mutate t1 under an assignment authority on the unrelated t2.
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-unrelated-1", ActorID: env.actor, AuthorityJournalID: &auth,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: target, EventKind: "provenance.task.updated"}},
	})
	return expectRejected(err, ErrAuthorityScope, "an unrelated assignment authority")
}

// opBlockedByEdgeDoesNotGrantAuthority proves a scheduling edge (blocked_by)
// grants no authority: an assignment authority on a prereq task must NOT be able
// to mutate an organizationally unrelated task that merely lists the prereq as a
// scheduling blocker. An assignment authority governs ONLY its own active
// episode's task (§14.1 — no edge-graph governance without a contract amendment).
func opBlockedByEdgeDoesNotGrantAuthority(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	prereq := env.taskFor(t, "unrelated-prereq")
	dependent := env.taskFor(t, "unrelated-dependent")
	// dependent blocked_by prereq: a pure scheduling constraint (prereq must
	// finish before dependent), carrying no ownership/governance semantics. The
	// blocked_by edge is an un-journaled §6 relationship write on the Session SDK.
	if err := env.tr.As(env.actor, boot).AddEdge(dependent, prereq.String(), EdgeBlockedBy); err != nil {
		return fmt.Errorf("add blocked_by scheduling edge: %w", err)
	}
	occupant := env.actorFor(t, "occ")
	// Authority scoped ONLY to prereq.
	auth := env.startEpisode(t, "op-seed-auth", boot, prereq, "PREREQ-AUTH", occupant)
	// Attempt to mutate the unrelated dependent under the prereq's authority.
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-over-grant-1", ActorID: env.actor, AuthorityJournalID: &auth,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: dependent, EventKind: "provenance.task.updated"}},
	})
	return expectRejected(err, ErrAuthorityScope, "an authority reaching an unrelated task via a blocked_by scheduling edge")
}

// opTransitiveParentCitationGrantsAuthority proves the §14.5 transitive
// deliberate-ownership reach: a supervisor@epic <- worker@story <- helper@subtask
// parent-citation chain, all active, lets the supervisor's own assignment
// authority govern an effect on the subtask (branch b of the governance predicate).
func opTransitiveParentCitationGrantsAuthority(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	epic := env.taskFor(t, "epic")
	story := env.taskFor(t, "story")
	subtask := env.taskFor(t, "subtask")
	// E-SUP on epic (no parent); its started transition is the supervisor authority.
	supAuth := env.startEpisode(t, "op-start-sup", boot, epic, "E-SUP", env.actorFor(t, "supervisor"))
	// E-WRK on story cites E-SUP; E-HLP on subtask cites E-WRK. Starts authorized
	// by the bootstrap authority (which governs every task); citation is metadata.
	env.startEpisodeWithParent(t, "op-start-wrk", boot, story, "E-WRK", "E-SUP", env.actorFor(t, "worker"))
	env.startEpisodeWithParent(t, "op-start-hlp", boot, subtask, "E-HLP", "E-WRK", env.actorFor(t, "helper"))
	// An effect on the subtask under the supervisor authority must be accepted: the
	// subtask episode reaches E-SUP via the active citation chain.
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-grandparent-authorizes-subtask"), ActorID: env.actor, AuthorityJournalID: &supAuth,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: subtask, EventKind: "provenance.task.updated"}},
	}); err != nil {
		return fmt.Errorf("supervisor authority rejected on the subtask despite an active citation chain: %w", err)
	}
	return nil
}

// opParentChainMiddleEndedCutsAuthority proves §14.5 whole-chain liveness: with
// the middle (worker) episode ended, the supervisor no longer governs the subtask.
func opParentChainMiddleEndedCutsAuthority(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	epic := env.taskFor(t, "epic")
	story := env.taskFor(t, "story")
	subtask := env.taskFor(t, "subtask")
	supAuth := env.startEpisode(t, "op-start-sup", boot, epic, "E-SUP", env.actorFor(t, "supervisor"))
	env.startEpisodeWithParent(t, "op-start-wrk", boot, story, "E-WRK", "E-SUP", env.actorFor(t, "worker"))
	env.startEpisodeWithParent(t, "op-start-hlp", boot, subtask, "E-HLP", "E-WRK", env.actorFor(t, "helper"))
	// End the MIDDLE episode; the subtask episode is still active but its chain to
	// the supervisor is now broken by an inactive intermediate.
	env.endEpisode(t, "op-end-wrk", boot, story, "E-WRK")
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-grandchild-effect-after-middle-ended"), ActorID: env.actor, AuthorityJournalID: &supAuth,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: subtask, EventKind: "provenance.task.updated"}},
	})
	return expectRejected(err, ErrAuthorityScope, "a grandchild effect after the middle chain episode ended")
}

// opParentChainMiddleEndedSameOperationCutsAuthority proves §14.5 whole-chain
// liveness holds WITHIN a single operation's per-effect fold (§9.3.1), not only
// across separate Apply calls (opParentChainMiddleEndedCutsAuthority covers the
// cross-operation cut). One operation batches two effects on the
// supervisor <- worker <- helper citation chain: effect 1 ends the MIDDLE (worker)
// episode E-WRK, and effect 2 mutates the helper's subtask under the supervisor
// authority. Because each effect is authorized against the state produced by all
// earlier effects of the same operation, effect 2 must see effect 1's just-ended
// middle episode and the supervisor's transitive reach to the subtask is already
// cut — the whole operation is rejected with ErrAuthorityScope and nothing is
// committed. This mirrors opFoldSequentiallyEffect2SeesEffect1's
// two-effects-in-one-op shape, but exercises the §14.5 parent-citation governance
// walk rather than the pre-existing predecessor/successor succession path.
func opParentChainMiddleEndedSameOperationCutsAuthority(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	epic := env.taskFor(t, "epic")
	story := env.taskFor(t, "story")
	subtask := env.taskFor(t, "subtask")
	// E-SUP on epic (no parent); its started transition is the supervisor authority.
	supAuth := env.startEpisode(t, "op-start-sup", boot, epic, "E-SUP", env.actorFor(t, "supervisor"))
	// E-WRK on story cites E-SUP; E-HLP on subtask cites E-WRK.
	env.startEpisodeWithParent(t, "op-start-wrk", boot, story, "E-WRK", "E-SUP", env.actorFor(t, "worker"))
	env.startEpisodeWithParent(t, "op-start-hlp", boot, subtask, "E-HLP", "E-WRK", env.actorFor(t, "helper"))
	// ONE operation batches: (1) end the MIDDLE episode E-WRK on the story, (2) a
	// subtask effect under the supervisor authority. Effect 1 is authorized because
	// the chain E-WRK -> E-SUP is still active at effect 1's journal position; effect
	// 2 then folds against effect 1's just-inserted ended-E-WRK transition, so the
	// helper's chain to the supervisor is broken and the supervisor authority no
	// longer reaches the subtask (§9.3.1, §14.5). The whole operation is rejected.
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        opID(input, "op-end-middle-then-grandchild-effect"),
		ActorID:            env.actor,
		AuthorityJournalID: &supAuth,
		CommandDigest:      env.digest("c"),
		MutationDigest:     env.digest("m"),
		Effects: []Effect{
			{Sort: EffectAssignmentEnd, AssignmentID: "E-WRK", TaskID: story, SlotID: SlotOwnerResponsibility},
			{Sort: EffectTaskEvent, TaskID: subtask, EventKind: "provenance.task.updated"},
		},
	})
	return expectRejected(err, ErrAuthorityScope, "a same-operation grandchild effect after effect 1 ended the middle chain episode")
}

// opCitationOfInactiveParentRejected proves §14.5 citation validity: citing an
// ended (inactive) parent at start is rejected before commit.
func opCitationOfInactiveParentRejected(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	epic := env.taskFor(t, "epic")
	story := env.taskFor(t, "story")
	// Start then END E-SUP so it is inactive at the citation below.
	env.startEpisode(t, "op-start-sup", boot, epic, "E-SUP", env.actorFor(t, "supervisor"))
	env.endEpisode(t, "op-end-sup", boot, epic, "E-SUP")
	// Cite the now-ended E-SUP as the parent of a new episode on the story.
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-cite-ended-parent"), ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{
			Sort: EffectAssignmentStart, AssignmentID: "E-WRK", TaskID: story,
			SlotID: SlotOwnerResponsibility, Occupant: env.actorFor(t, "worker"), Parent: "E-SUP",
		}},
	})
	return expectRejected(err, ErrParentCitation, "citing a parent episode that has already ended")
}

// opCorruptedCyclicParentChainFailsClosed proves §14.5's bounded, visited-tracked
// governance walk fails closed on a corrupt cyclic parent chain seeded directly
// (bypassing the start-effect cycle guard), rather than looping.
func opCorruptedCyclicParentChainFailsClosed(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	st, ok := env.tr.(*sqliteTracker)
	if !ok {
		return fmt.Errorf("expected *sqliteTracker, got %T", env.tr)
	}
	tX := env.taskFor(t, "tX")
	tY := env.taskFor(t, "tY")
	tZ := env.taskFor(t, "tZ")
	zAuth, target, beforeJID, err := st.db.AdversarialCyclicParentChain(env.actor, tX, tY, tZ)
	if err != nil {
		return fmt.Errorf("seed corrupt cyclic parent chain: %w", err)
	}
	governs, err := st.db.AuthorityGovernsTaskAt(zAuth, target, beforeJID)
	if governs {
		return fmt.Errorf("governance walk returned true over a corrupt cyclic parent chain, want a fail-closed error")
	}
	return expectRejected(err, ErrCorruptParentChain, "a corrupt cyclic parent-citation chain")
}

func opProduceSubordinateRowCarryingActor(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	// Part 1 — Apply's input-side guard rejects an effect that supplies a per-row
	// actor (§10 rule 5, input side): under a valid authority so genesis discipline
	// is not what rejects it.
	{
		env := newOpsEnv(t)
		boot := env.genesis(t, "op-genesis")
		task := env.taskFor(t, "t1")
		alice := env.actorFor(t, "actor-alice")
		_, applyErr := env.tr.Journal().Apply(OperationInput{
			OperationID: opID(input, "op-subordinate-actor-input-1"), ActorID: alice, AuthorityJournalID: &boot,
			CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
			Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated", ActorID: alice}},
		})
		if applyErr == nil || !errors.Is(applyErr, ErrActorPlacement) {
			return fmt.Errorf("Apply accepted an effect carrying a per-row actor (or rejected with %v), want ErrActorPlacement", applyErr)
		}
	}

	// Part 2 — the must-fail case proper: a subordinate (operation-produced) journal
	// row landed past the CHECK constraint via the adversarial seam, so the
	// production VerifyIntegrity placement guard (§10 rule 5) is what rejects it.
	env := newOpsEnv(t)
	task := env.taskFor(t, "t1")
	alice := env.actorFor(t, "actor-alice")
	st, ok := env.tr.(*sqliteTracker)
	if !ok {
		return fmt.Errorf("expected *sqliteTracker, got %T", env.tr)
	}
	if _, err := st.db.AdversarialSubordinateRowCarryingActor(alice, task); err != nil {
		return fmt.Errorf("write subordinate row carrying actor: %w", err)
	}
	err := env.tr.Journal().VerifyIntegrity()
	return expectRejected(err, ErrActorPlacement, "a subordinate row carrying a stored actor")
}

func opEndEpisodeNeverStarted(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-end-orphan-1"), ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectAssignmentEnd, AssignmentID: "G", TaskID: task, SlotID: SlotOwnerResponsibility}},
	})
	return expectRejected(err, ErrAssignmentLifecycle, "ending an episode that never started")
}

func opBatchEndedBeforeStarted(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-inverted-lifecycle-1"), ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{
			{Sort: EffectAssignmentEnd, AssignmentID: "H", TaskID: task, SlotID: SlotOwnerResponsibility},
			{Sort: EffectAssignmentStart, AssignmentID: "H", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant},
		},
	})
	return expectRejected(err, ErrAssignmentLifecycle, "an ended transition folded before its started transition")
}

// ---------------------------------------------------------------------------
// owner_responsibility.yaml
// ---------------------------------------------------------------------------

func opCloseTaskWithActiveAssignment(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	env.startEpisode(t, "op-seed-A", boot, task, "A", occupant)
	// Close ends the active episode A and clears the owner in one operation.
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-close-2"), ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{
			{Sort: EffectAssignmentEnd, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility},
			{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.closed"},
		},
	})
	if err != nil {
		return fmt.Errorf("close-with-ended-assignment rejected: %w", err)
	}
	if res.Kind != CommittedExact {
		return fmt.Errorf("close did not commit: %+v", res)
	}
	got, err := env.tr.Show(task)
	if err != nil {
		return err
	}
	if got.Owner != nil {
		return fmt.Errorf("task owner not cleared after close: %v", got.Owner)
	}
	return nil
}

func opCloseTaskOmitEndedTransition(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	env.startEpisode(t, "op-seed-A", boot, task, "A", occupant)
	// Close omits the ended transition for the still-active episode A.
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-close-3"), ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.closed"}},
	})
	return expectRejected(err, ErrCloseWithoutEnding, "closing a task without ending its active episode")
}

func opConcurrentTransferCAS(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	env.startEpisode(t, "op-seed-A", boot, task, "A", occupant)

	type outcome struct {
		op  string
		err error
	}
	attempts := []struct {
		op  string
		new AssignmentID
	}{
		{"op-transfer-winner", "B"},
		{"op-transfer-loser", "C"},
	}
	results := make([]outcome, len(attempts))
	var wg sync.WaitGroup
	wg.Add(len(attempts))
	for i, a := range attempts {
		go func(i int, opStr string, newID AssignmentID) {
			defer wg.Done()
			_, err := env.tr.Journal().Apply(OperationInput{
				OperationID: OperationID(opStr), ActorID: env.actor, AuthorityJournalID: &boot,
				CommandDigest: env.digest(opStr + "c"), MutationDigest: env.digest(opStr + "m"),
				Effects: []Effect{
					{Sort: EffectAssignmentEnd, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility},
					{Sort: EffectAssignmentStart, AssignmentID: newID, TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant, Predecessor: "A"},
				},
			})
			results[i] = outcome{op: opStr, err: err}
		}(i, a.op, a.new)
	}
	wg.Wait()

	winners, losers := 0, 0
	for _, r := range results {
		if r.err == nil {
			winners++
			continue
		}
		if !errors.Is(r.err, ErrStaleEpisode) {
			return fmt.Errorf("loser %q rejected with %v, want ErrStaleEpisode", r.op, r.err)
		}
		losers++
	}
	if winners != 1 || losers != 1 {
		return fmt.Errorf("CAS produced winners=%d losers=%d, want exactly one each", winners, losers)
	}
	// The loser wrote nothing: exactly one successor episode (the winner's) exists.
	successors, err := countSuccessorEpisodes(env, task)
	if err != nil {
		return err
	}
	if successors != 1 {
		return fmt.Errorf("after CAS there are %d successor episodes, want 1 (loser wrote nothing)", successors)
	}
	return nil
}

func opAttributeEpisodeOccupant(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	committing := env.actorFor(t, "actor-0")
	erin := env.actorFor(t, "actor-erin")
	// The system actor commits; the occupant is erin.
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-system-transfer-1"), ActorID: committing, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectAssignmentStart, AssignmentID: "J", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: erin}},
	})
	if err != nil {
		return fmt.Errorf("system transfer rejected: %w", err)
	}
	attrs, err := env.tr.Journal().TaskAttributions(task)
	if err != nil {
		return err
	}
	credited := map[string]bool{}
	for _, a := range attrs {
		credited[a.ActorID.String()] = true
	}
	if !credited[erin.String()] {
		return fmt.Errorf("occupant %v not attributed", erin)
	}
	if credited[committing.String()] {
		return fmt.Errorf("committing system actor %v was attributed but must not be (§8.2)", committing)
	}
	return nil
}

// ---------------------------------------------------------------------------
// zero_event_operations.yaml
// ---------------------------------------------------------------------------

func opApplyZeroTaskEventOperation(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-assign-only-1"), ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectAssignmentStart, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant}},
	})
	if err != nil {
		return fmt.Errorf("zero-task-event operation rejected: %w", err)
	}
	if res.AnchorJournalID == 0 {
		return fmt.Errorf("zero-task-event operation produced no anchor")
	}
	if len(res.EmittedEvents) != 0 {
		return fmt.Errorf("zero-task-event operation emitted %d events, want 0", len(res.EmittedEvents))
	}
	return nil
}

func opApplyEmptyBatch(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-empty-batch-1"), ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: nil,
	})
	if err != nil {
		return fmt.Errorf("empty batch rejected (must be a valid zero-effect operation): %w", err)
	}
	if res.AnchorJournalID == 0 || len(res.EmittedEvents) != 0 {
		return fmt.Errorf("empty batch did not anchor cleanly: %+v", res)
	}
	return nil
}

func opAuthorityPrecedenceByJournalID(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	occupant := env.actorFor(t, "occ")
	env.startEpisode(t, "op-seed-A", boot, task, "A", occupant)
	// op1 ends A with a LATER RecordedAt; op2 starts B (pred A) with an EARLIER
	// RecordedAt. Because op1 is committed first, its JournalID precedes op2's, so
	// op2's predecessor-ended precondition is satisfied by JournalID order (§12).
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op1", ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("1c"), MutationDigest: env.digest("1m"),
		RecordedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC).UnixNano(),
		Effects:    []Effect{{Sort: EffectAssignmentEnd, AssignmentID: "A", TaskID: task, SlotID: SlotOwnerResponsibility}},
	}); err != nil {
		return fmt.Errorf("op1 (end A): %w", err)
	}
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op2", ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("2c"), MutationDigest: env.digest("2m"),
		RecordedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
		Effects:    []Effect{{Sort: EffectAssignmentStart, AssignmentID: "B", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant, Predecessor: "A"}},
	})
	if err != nil {
		return fmt.Errorf("op2 rejected despite op1 preceding it by JournalID: %w", err)
	}
	if res.Kind != CommittedExact {
		return fmt.Errorf("op2 did not commit: %+v", res)
	}
	return nil
}

func opLookupCommittedAbsent(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	op := opID(input, "op-never-applied")
	res, err := env.tr.Journal().LookupCommitted(op)
	if err != nil {
		return fmt.Errorf("lookup of never-applied operation errored: %w", err)
	}
	if res.Kind != CommittedAbsent {
		return fmt.Errorf("lookup variant = %s, want CommittedAbsent", res.Kind)
	}
	if len(res.EmittedEvents) != 0 || len(res.ResultSlots) != 0 {
		return fmt.Errorf("absent lookup carried side effects: %+v", res)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ordering.yaml
// ---------------------------------------------------------------------------

func opAuthorizeUsingRecordedAtNotJournalID(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	// ev1 is committed first (smaller JournalID) with a LATER RecordedAt, emitted as an
	// operation-anchored append because bare task events are forbidden.
	ev1JID := appendEventViaOp(t, env.tr, boot, env.actor, task, "op-ev1", "provenance.task.updated",
		time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	// auth1 (an assignment authority on t1) is committed AFTER ev1 (larger
	// JournalID) but with an EARLIER RecordedAt.
	occupant := env.actorFor(t, "occ")
	auth1 := env.startEpisode(t, "op-auth1", boot, task, "AUTH1", occupant)
	if auth1 <= ev1JID {
		return fmt.Errorf("test setup invalid: auth1 %d must be committed after ev1 %d", auth1, ev1JID)
	}
	st := env.tr.(*sqliteTracker)
	governs, err := st.db.AuthorityGovernsTaskAt(auth1, task, ev1JID)
	if err != nil {
		return err
	}
	if governs {
		return fmt.Errorf("authority %d (later JournalID, earlier RecordedAt) authorized effect %d — "+
			"authorization must order by JournalID, not RecordedAt (§12)", auth1, ev1JID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// subtype_integrity.yaml (§10 rule 8) — adversarial seams + VerifyIntegrity
// ---------------------------------------------------------------------------

func opWriteJournalRowTwoSubtypes(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	st := env.tr.(*sqliteTracker)
	if _, err := st.db.AdversarialJournalRowTwoSubtypes(env.actor); err != nil {
		return fmt.Errorf("seed two-subtype violation: %w", err)
	}
	return expectRejected(env.tr.Journal().VerifyIntegrity(), ErrSubtypeIntegrity, "a journal row in two subtype tables")
}

func opWriteSubtypeMismatchingKind(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	st := env.tr.(*sqliteTracker)
	if _, err := st.db.AdversarialSubtypeMismatchingKind(env.actor); err != nil {
		return fmt.Errorf("seed discriminator mismatch: %w", err)
	}
	return expectRejected(env.tr.Journal().VerifyIntegrity(), ErrSubtypeIntegrity, "a subtype row disagreeing with its discriminator")
}

func opWriteAuthorityDetailMismatch(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	st := env.tr.(*sqliteTracker)
	if _, err := st.db.AdversarialAuthorityDetailMismatch(env.actor, env.task); err != nil {
		return fmt.Errorf("seed authority-detail mismatch: %w", err)
	}
	return expectRejected(env.tr.Journal().VerifyIntegrity(), ErrSubtypeIntegrity, "a bootstrap authority bearing an assignment transition")
}

// ---------------------------------------------------------------------------
// Shared assertions and DB probes
// ---------------------------------------------------------------------------

// expectRejected asserts a must-fail path returned a non-nil error matching the
// expected sentinel; the description names the scenario for a readable failure.
func expectRejected(err error, sentinel error, scenario string) error {
	if err == nil {
		return fmt.Errorf("%s was accepted; expected rejection with %v", scenario, sentinel)
	}
	if !errors.Is(err, sentinel) {
		return fmt.Errorf("%s rejected with %v, want %v", scenario, err, sentinel)
	}
	return nil
}

func countBootstrapAuthorities(env *opsEnv) (int, error) {
	st := env.tr.(*sqliteTracker)
	return st.db.CountAuthoritiesOfKind(int(AuthorityKindBootstrap))
}

func countSuccessorEpisodes(env *opsEnv, task TaskID) (int, error) {
	st := env.tr.(*sqliteTracker)
	return st.db.CountSuccessorEpisodes(task)
}

// TestApplyRejectsOperationIDReuseWithDifferentIdentity pins the §11/§9.4 typed
// conflict: reusing an OperationID with a different four-field replay identity is
// a typed conflict that commits nothing, never a re-execution. The executed
// corpus case for this (reuse-operationid-different-mutation-digest) is scoped to
// Recovery behavior has separate coverage, so this test targets the write path.
func TestApplyRejectsOperationIDReuseWithDifferentIdentity(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	base := OperationInput{
		OperationID:        "op-reused",
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("c"),
		MutationDigest:     env.digest("m"),
		Effects:            []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated"}},
	}
	if _, err := env.tr.Journal().Apply(base); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Same OperationID, different canonical effects → typed conflict, no write.
	conflicting := base
	conflicting.Effects = []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated", Payload: []byte(`{"changed":true}`)}}
	_, err := env.tr.Journal().Apply(conflicting)
	if !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("reused OperationID with a different identity = %v, want ErrOperationConflict", err)
	}
	// The conflicting attempt committed nothing: only the original event exists.
	res, err := env.tr.Journal().LookupCommitted("op-reused")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(res.EmittedEvents) != 1 {
		t.Fatalf("after a rejected reuse there are %d emitted events, want 1 (nothing extra committed)", len(res.EmittedEvents))
	}
}

// TestApplyConflictProducesTypedClosedSumAndErrorsAs pins the honest conflict
// surface (§11, §9.6): a reused OperationID with a differing four-field identity
// yields the closed-sum CommittedConflict variant carrying the typed
// *OperationConflict payload, AND an error that both errors.Is-matches
// ErrOperationConflict and errors.As-extracts the *OperationConflict — no dead
// enum variant, no stringified-only payload.
func TestApplyConflictProducesTypedClosedSumAndErrorsAs(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	base := OperationInput{
		OperationID: "op-x", ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated"}},
	}
	if _, err := env.tr.Journal().Apply(base); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	conflicting := base
	conflicting.Effects = []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated", Payload: []byte(`{"changed":true}`)}}
	res, err := env.tr.Journal().Apply(conflicting)
	if err == nil {
		t.Fatal("reused OperationID with a different identity was accepted; want a conflict error")
	}
	// Closed-sum production: Kind is actually CommittedConflict with a typed payload.
	if res.Kind != CommittedConflict {
		t.Fatalf("conflict res.Kind = %s, want CommittedConflict", res.Kind)
	}
	if res.Conflict == nil {
		t.Fatal("CommittedConflict result carried a nil Conflict payload")
	}
	if res.Conflict.OperationID != "op-x" {
		t.Fatalf("res.Conflict = %+v, want OperationID op-x", res.Conflict)
	}
	// errors.Is recovers the sentinel; errors.As recovers the typed *OperationConflict.
	if !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("errors.Is(err, ErrOperationConflict) = false: %v", err)
	}
	var oc *OperationConflict
	if !errors.As(err, &oc) {
		t.Fatalf("errors.As(err, &*OperationConflict) = false — typed payload not recoverable: %v", err)
	}
	if oc.OperationID != "op-x" {
		t.Fatalf("errors.As recovered incomplete typed conflict %+v", oc)
	}
	// Nothing extra committed: the original event is the only one.
	if r, lerr := env.tr.Journal().LookupCommitted("op-x"); lerr != nil {
		t.Fatalf("lookup: %v", lerr)
	} else if len(r.EmittedEvents) != 1 {
		t.Fatalf("after a conflict there are %d emitted events, want 1", len(r.EmittedEvents))
	}
}

// TestFoldDecisionEnforcesAuthorityGovernance pins §9.3's per-effect authority
// checkpoint for journal_decisions. A task-scoped decision is
// rejected under an authority that does not govern the named task (zero writes),
// accepted under one that does, and an untasked decision skips the check (§6.1).
func TestFoldDecisionEnforcesAuthorityGovernance(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	governed := env.taskFor(t, "t-governed")
	other := env.taskFor(t, "t-other")
	occupant := env.actorFor(t, "occ")
	auth := env.startEpisode(t, "op-seed-auth", boot, other, "OTHER-AUTH", occupant)

	// must-fail: a task-scoped decision on t-governed under the t-other authority.
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-decision-ungoverned", ActorID: env.actor, AuthorityJournalID: &auth,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectDecision, TaskID: governed, DecisionKind: "pasture.review.vote", Payload: []byte(`{}`)}},
	})
	if e := expectRejected(err, ErrAuthorityScope, "an ungoverned task-scoped decision"); e != nil {
		t.Fatal(e)
	}
	if r, lerr := env.tr.Journal().LookupCommitted("op-decision-ungoverned"); lerr != nil {
		t.Fatalf("lookup: %v", lerr)
	} else if r.Kind != CommittedAbsent {
		t.Fatalf("rejected decision left a committed operation (non-zero writes): %+v", r)
	}

	// must-pass: the same decision on t-other, which its authority governs.
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-decision-governed", ActorID: env.actor, AuthorityJournalID: &auth,
		CommandDigest: env.digest("c2"), MutationDigest: env.digest("m2"),
		Effects: []Effect{{Sort: EffectDecision, TaskID: other, DecisionKind: "pasture.review.vote", Payload: []byte(`{}`)}},
	})
	if err != nil {
		t.Fatalf("governed decision rejected: %v", err)
	}
	if res.Kind != CommittedExact {
		t.Fatalf("governed decision did not commit: %+v", res)
	}

	// must-pass: an untasked decision legitimately skips the governance check.
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-decision-untasked", ActorID: env.actor, AuthorityJournalID: &auth,
		CommandDigest: env.digest("c3"), MutationDigest: env.digest("m3"),
		Effects: []Effect{{Sort: EffectDecision, DecisionKind: "pasture.review.vote", Payload: []byte(`{}`)}},
	}); err != nil {
		t.Fatalf("untasked decision rejected: %v", err)
	}
}

// TestFoldEvidenceEnforcesAuthorityGovernance is the §9.3 per-effect authority
// checkpoint for journal_evidence, mirroring the decision case.
func TestFoldEvidenceEnforcesAuthorityGovernance(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	governed := env.taskFor(t, "t-governed")
	other := env.taskFor(t, "t-other")
	occupant := env.actorFor(t, "occ")
	auth := env.startEpisode(t, "op-seed-auth", boot, other, "OTHER-AUTH", occupant)

	// must-fail: a task-scoped evidence row on t-governed under the t-other authority.
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-evidence-ungoverned", ActorID: env.actor, AuthorityJournalID: &auth,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectEvidence, TaskID: governed, EvidenceKind: "pasture.git.commit", ContentDigest: env.digest("x"), Payload: []byte(`{}`)}},
	})
	if e := expectRejected(err, ErrAuthorityScope, "an ungoverned task-scoped evidence row"); e != nil {
		t.Fatal(e)
	}
	if r, lerr := env.tr.Journal().LookupCommitted("op-evidence-ungoverned"); lerr != nil {
		t.Fatalf("lookup: %v", lerr)
	} else if r.Kind != CommittedAbsent {
		t.Fatalf("rejected evidence left a committed operation (non-zero writes): %+v", r)
	}

	// must-pass: the same evidence on t-other, which its authority governs.
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-evidence-governed", ActorID: env.actor, AuthorityJournalID: &auth,
		CommandDigest: env.digest("c2"), MutationDigest: env.digest("m2"),
		Effects: []Effect{{Sort: EffectEvidence, TaskID: other, EvidenceKind: "pasture.git.commit", ContentDigest: env.digest("y"), Payload: []byte(`{}`)}},
	})
	if err != nil {
		t.Fatalf("governed evidence rejected: %v", err)
	}
	if res.Kind != CommittedExact {
		t.Fatalf("governed evidence did not commit: %+v", res)
	}

	// must-pass: an untasked evidence row legitimately skips the governance check.
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-evidence-untasked", ActorID: env.actor, AuthorityJournalID: &auth,
		CommandDigest: env.digest("c3"), MutationDigest: env.digest("m3"),
		Effects: []Effect{{Sort: EffectEvidence, EvidenceKind: "pasture.git.commit", ContentDigest: env.digest("z"), Payload: []byte(`{}`)}},
	}); err != nil {
		t.Fatalf("untasked evidence rejected: %v", err)
	}
}

// TestResolveOperationIDInsertRaceTranslatesToTypedOutcome exercises §9.6's
// bullet-2 race-translation path directly: when the
// anchor insert loses a concurrent same-new-OperationID UNIQUE race, the reducer
// re-reads the winner's committed row and returns the typed idempotent result (on
// an exact identity match) or the typed CommittedConflict (on a mismatch), never a
// raw SQLite constraint error. Under the in-process db.mu the live path is
// unreachable, so the translation is driven through the adversarial seam.
func TestResolveOperationIDInsertRaceTranslatesToTypedOutcome(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	st := env.tr.(*sqliteTracker)

	// The winner committed the OperationID first.
	winner := OperationInput{
		OperationID: "op-raced", ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated"}},
	}
	if _, err := env.tr.Journal().Apply(winner); err != nil {
		t.Fatalf("winner apply: %v", err)
	}

	// Idempotent loser: identical four-field identity → typed idempotent result.
	res, err := st.db.AdversarialResolveOperationIDInsertRace(winner)
	if err != nil {
		t.Fatalf("idempotent race translation returned a raw error: %v", err)
	}
	if res.Kind != CommittedExact || !res.ShortCircuited {
		t.Fatalf("idempotent race did not short-circuit to CommittedExact: %+v", res)
	}

	// Conflicting loser: same OperationID, different identity → typed conflict.
	conflicting := winner
	conflicting.MutationDigest = env.digest("different-m")
	conflicting.Effects = []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated", Payload: []byte(`{"changed":true}`)}}
	res2, err2 := st.db.AdversarialResolveOperationIDInsertRace(conflicting)
	if err2 == nil {
		t.Fatal("conflicting race translation returned no error")
	}
	if !errors.Is(err2, ErrOperationConflict) {
		t.Fatalf("conflicting race error = %v, want ErrOperationConflict (not a raw SQLite error)", err2)
	}
	var oc *OperationConflict
	if !errors.As(err2, &oc) {
		t.Fatalf("conflicting race error not errors.As-extractable: %v", err2)
	}
	if res2.Kind != CommittedConflict || res2.Conflict == nil {
		t.Fatalf("conflicting race did not produce the typed CommittedConflict: %+v", res2)
	}
}
