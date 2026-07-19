package provenance

// journal_task_create_test.go drives the journaled task-creation reducer
// (EffectTaskCreate) against the real Apply production path
// (docs/journal-relational-contract.md §8.1, §9.3): a task's birth INSERTs the
// tasks row AND emits a provenance.task.created journal event in one atomic fold,
// the projection seeds status=Open, the created row is born with a non-NULL
// watermark, and Open's from-empty SHADOW-DERIVATION replay re-derives the identical
// projection without mutating the real tables (§15).

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// newTaskID mints a fresh namespaced task id for a create effect.
func newTaskID() TaskID {
	return TaskID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}
}

func (e *opsEnv) applyCreate(boot JournalID, id TaskID, title string, tt TaskType, pr Priority, ph Phase) (CommittedResult, error) {
	op := OperationID("op-create--" + id.String())
	return e.tr.Journal().Apply(OperationInput{
		OperationID:        op,
		ActorID:            e.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      e.digest(string(op) + "c"),
		MutationDigest:     e.digest(string(op) + "m"),
		Effects:            []Effect{{Sort: EffectTaskCreate, TaskID: id, Title: title, Type: tt, Priority: pr, Phase: ph}},
	})
}

func TestEffectTaskCreate_JournalsBirth(t *testing.T) {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	id := newTaskID()

	res, err := env.applyCreate(boot, id, "Journaled Task", TaskTypeFeature, PriorityHigh, PhaseImplPlan)
	if err != nil {
		t.Fatalf("journaled task create rejected: %v", err)
	}
	if res.Kind != CommittedExact {
		t.Fatalf("create result kind = %v, want CommittedExact", res.Kind)
	}

	// The tasks row was inserted by the reducer, with the create metadata and a
	// status=Open projection seeded from the created event.
	got, err := env.tr.Show(id)
	if err != nil {
		t.Fatalf("Show created task: %v", err)
	}
	if got.Title != "Journaled Task" || got.Type != TaskTypeFeature || got.Priority != PriorityHigh || got.Phase != PhaseImplPlan {
		t.Fatalf("created task metadata = %+v, want title/feature/high/impl_plan", got)
	}
	if got.Status != StatusOpen {
		t.Fatalf("created task status = %v, want open", got.Status)
	}
	if got.Owner != nil {
		t.Fatalf("created task has an owner %v, want none", got.Owner)
	}

	// The created row is born with a non-NULL watermark (the create event's journal
	// id), and the creator is attributed at that event.
	replay, err := env.tr.Journal().ReplayProjections()
	if err != nil {
		t.Fatalf("ReplayProjections after journaled create diverged: %v", err)
	}
	p, ok := replay.ProjectionForTask(id)
	if !ok {
		t.Fatalf("replay produced no projection for the created task")
	}
	if p.LastJournalID == 0 {
		t.Fatalf("created task watermark is zero; a journaled create must set last_journal_id")
	}
	if p.Status != TaskStatusOpen {
		t.Fatalf("replayed status = %v, want open", p.Status)
	}

	attribs, err := env.tr.Journal().TaskAttributions(id)
	if err != nil {
		t.Fatalf("TaskAttributions: %v", err)
	}
	found := false
	for _, a := range attribs {
		if a.ActorID.String() == env.actor.String() {
			found = true
		}
	}
	if !found {
		t.Fatalf("creator %s not attributed on the created task (attribs=%+v)", env.actor, attribs)
	}
}

func TestEffectTaskCreate_ShadowReplayConverges(t *testing.T) {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")

	// A handful of journaled-created tasks, one later closed, so replay exercises
	// the created + lifecycle projections through the shadow derivation.
	var ids []TaskID
	for i := 0; i < 3; i++ {
		id := newTaskID()
		if _, err := env.applyCreate(boot, id, "t", TaskTypeTask, PriorityMedium, PhaseUnscoped); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	// Close the first task via a lifecycle task_event under the bootstrap authority.
	closeOp := OperationID("op-close--" + ids[0].String())
	if _, err := env.tr.Journal().Apply(OperationInput{
		OperationID: closeOp, ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("cc"), MutationDigest: env.digest("cm"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: ids[0], EventKind: EventKindTaskClosed}},
	}); err != nil {
		t.Fatalf("close task: %v", err)
	}

	// The from-empty shadow refold converges with the stored incremental projection,
	// and leaves the real projection untouched (a second replay still converges).
	if _, err := env.tr.Journal().ReplayProjections(); err != nil {
		t.Fatalf("first replay diverged: %v", err)
	}
	if _, err := env.tr.Journal().ReplayProjections(); err != nil {
		t.Fatalf("second replay diverged (shadow derivation mutated real tables?): %v", err)
	}
	got, err := env.tr.Show(ids[0])
	if err != nil {
		t.Fatalf("Show closed task: %v", err)
	}
	if got.Status != StatusClosed {
		t.Fatalf("closed task status = %v, want closed", got.Status)
	}
}

func TestEffectTaskCreate_RejectsDuplicateAndInvalid(t *testing.T) {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	id := newTaskID()
	if _, err := env.applyCreate(boot, id, "first", TaskTypeTask, PriorityMedium, PhaseUnscoped); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Re-creating the same id is rejected (a fresh OperationID so it is not a replay
	// short-circuit, but the tasks row already exists).
	dupOp := OperationID("op-create-dup--" + id.String())
	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID: dupOp, ActorID: env.actor, AuthorityJournalID: &boot,
		CommandDigest: env.digest("dc"), MutationDigest: env.digest("dm"),
		Effects: []Effect{{Sort: EffectTaskCreate, TaskID: id, Title: "dup", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}},
	})
	if err == nil {
		t.Fatalf("duplicate task create was accepted; expected rejection")
	}

	// An invalid classification (out-of-range TaskType) is rejected before commit.
	_, err = env.applyCreate(boot, newTaskID(), "bad", TaskType(99), PriorityMedium, PhaseUnscoped)
	if err == nil {
		t.Fatalf("task create with invalid TaskType was accepted; expected rejection")
	}
}

func TestEffectTaskCreate_RejectsNonGoverningAuthority(t *testing.T) {
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis")
	// A task and an owner-responsibility episode whose (assignment) authority governs
	// only that task.
	owned := newTaskID()
	if _, err := env.applyCreate(boot, owned, "owned", TaskTypeTask, PriorityMedium, PhaseUnscoped); err != nil {
		t.Fatalf("create owned task: %v", err)
	}
	occupant := env.actorFor(t, "occ")
	assignAuth := env.startEpisode(t, "op-start-owned", boot, owned, "A-owned", occupant)

	// Creating a NEW task under that assignment authority must fail: an assignment
	// authority governs no task without an episode, so a brand-new task is unreached.
	_, err := env.applyCreate(assignAuth, newTaskID(), "unreached", TaskTypeTask, PriorityMedium, PhaseUnscoped)
	if err == nil {
		t.Fatalf("task create under a non-governing assignment authority was accepted")
	}
	if !errors.Is(err, ErrAuthorityScope) {
		t.Fatalf("create-under-non-governing-authority rejected with %v, want ErrAuthorityScope", err)
	}
}
