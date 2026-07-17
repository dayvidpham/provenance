package provenance

// journal_operations_corpus_test.go executes the S1.2 adversarial proof-corpus
// histories — operations, effects, results, and authority lifecycle
// (docs/journal-relational-contract.md §2-§4, §9, §14) — against the real Apply
// / LookupCommitted production path. Each operator translates the symbolic
// corpus data (task/actor/assignment labels) into concrete registered IDs and
// drives the production reducer, honouring the case's must-pass/must-fail
// classification. These operators are the executable half of the s1.2 partition
// recorded in testdata/contract/scope.yaml.

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/testcorpus"
)

// s12Operators is the closed registry of executable S1.2 operators. Its key set
// must equal the s1.2 partition of scope.yaml (asserted by the partition test).
var s12Operators = map[testcorpus.OperatorName]s11Handler{
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
	"fold-sequentially-check-effect2-sees-effect1":    opFoldSequentiallyEffect2SeesEffect1,
	"start-with-orphaned-predecessor":                 opStartWithOrphanedPredecessor,
	"two-successors-same-predecessor":                 opTwoSuccessorsSamePredecessor,
	"authority-unrelated":                             opAuthorityUnrelated,
	"authority-governing-parent":                      opAuthorityGoverningParent,
	"produce-effect-with-actor-differing-from-anchor": opProduceEffectActorMismatch,
	"end-episode-never-started":                       opEndEpisodeNeverStarted,
	"batch-ended-before-started":                      opBatchEndedBeforeStarted,
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

func (e *opsEnv) taskFor(t *testing.T, label string) TaskID {
	if label == "" {
		return e.task
	}
	if tk, ok := e.tasks[label]; ok {
		return tk
	}
	tsk, err := e.tr.Create("provenance-test", "task "+label, "", TaskTypeTask, PriorityMedium, PhaseUnscoped)
	if err != nil {
		t.Fatalf("create task %q: %v", label, err)
	}
	e.tasks[label] = tsk.ID
	return tsk.ID
}

func (e *opsEnv) digest(s string) []byte { return []byte("digest--" + s) }

// genesis establishes the pasture-system bootstrap authority via one genesis
// operation (§4.6) and returns its produced authority JournalID, which as the
// system root governs every task.
func (e *opsEnv) genesis(t *testing.T, opID string) JournalID {
	res, err := e.tr.Journal().Apply(OperationInput{
		OperationID:    OperationID(opID),
		ActorID:        e.actor,
		CommandDigest:  e.digest(opID + "-cmd"),
		MutationDigest: e.digest(opID + "-mut"),
		Effects:        []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("genesis %q: %v", opID, err)
	}
	jid, ok := slotJournalID(res, "auth")
	if !ok {
		t.Fatalf("genesis %q produced no bootstrap authority slot", opID)
	}
	e.boot = &jid
	return jid
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

func opAuthorityGoverningParent(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	parent := env.taskFor(t, "parent1")
	child := env.taskFor(t, "child1")
	// child blocked_by parent → parent is a governing ancestor of child.
	if err := env.tr.AddEdge(child, parent.String(), EdgeBlockedBy); err != nil {
		return fmt.Errorf("add governing-parent edge: %w", err)
	}
	occupant := env.actorFor(t, "occ")
	auth := env.startEpisode(t, "op-seed-auth", boot, parent, "PAUTH", occupant)
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID: "op-governed-1", ActorID: env.actor, AuthorityJournalID: &auth,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: child, EventKind: "provenance.task.updated"}},
	})
	if err != nil {
		return fmt.Errorf("governing-parent authority was rejected: %w", err)
	}
	if res.Kind != CommittedExact {
		return fmt.Errorf("governed operation did not commit: %+v", res)
	}
	return nil
}

func opProduceEffectActorMismatch(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	task := env.taskFor(t, "t1")
	alice := env.actorFor(t, "actor-alice")
	bob := env.actorFor(t, "actor-bob")
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: opID(input, "op-actor-mismatch-1"), ActorID: alice, AuthorityJournalID: &boot,
		CommandDigest: env.digest("c"), MutationDigest: env.digest("m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.task.updated", ActorID: bob}},
	})
	return expectRejected(err, ErrEffectActorMismatch, "an effect actor differing from the anchor")
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
	// ev1 is committed first (smaller JournalID) with a LATER RecordedAt.
	ev1, err := env.tr.Journal().AppendTaskEvent(AppendTaskEventInput{
		ActorID: env.actor, TaskID: task, EventKind: "provenance.task.updated",
		RecordedAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		return fmt.Errorf("append ev1: %w", err)
	}
	// auth1 (an assignment authority on t1) is committed AFTER ev1 (larger
	// JournalID) but with an EARLIER RecordedAt.
	occupant := env.actorFor(t, "occ")
	auth1 := env.startEpisode(t, "op-auth1", boot, task, "AUTH1", occupant)
	if auth1 <= ev1.JournalID {
		return fmt.Errorf("test setup invalid: auth1 %d must be committed after ev1 %d", auth1, ev1.JournalID)
	}
	st := env.tr.(*sqliteTracker)
	governs, err := st.db.AuthorityGovernsTaskAt(auth1, task, ev1.JournalID)
	if err != nil {
		return err
	}
	if governs {
		return fmt.Errorf("authority %d (later JournalID, earlier RecordedAt) authorized effect %d — "+
			"authorization must order by JournalID, not RecordedAt (§12)", auth1, ev1.JournalID)
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
// S1.3, so this direct production test covers the S1.2 write-path requirement.
func TestApplyRejectsOperationIDReuseWithDifferentIdentity(t *testing.T) {
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
	// Same OperationID, different MutationDigest → typed conflict, no write.
	conflicting := base
	conflicting.MutationDigest = env.digest("different-m")
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
