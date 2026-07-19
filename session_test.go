package provenance_test

import (
	"errors"
	"testing"

	"github.com/dayvidpham/provenance"
)

// session_test.go exercises the production Session SDK (Tracker.As): the journaled
// task-lifecycle verbs, the un-journaled relationship/annotation verbs, Atomic
// multi-effect composition, the auto/pinned OperationID behavior, and the
// empty-journal genesis-required guard. Every test drives the real production path
// through the public provenance API and asserts §15 convergence where a journaled
// mutation must stay reproducible from journal history.

func newSessionTracker(t *testing.T) (provenance.Tracker, provenance.ActorID) {
	t.Helper()
	tr, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	actor, err := tr.RegisterSoftwareAgent("provenance-test", "session-actor", "0", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	return tr, actor.ID
}

// establishGenesis applies one genesis operation and returns the produced bootstrap
// authority's JournalID, the system root every task-governing Session binds to.
func establishGenesis(t *testing.T, tr provenance.Tracker, actor provenance.ActorID) provenance.JournalID {
	t.Helper()
	res, err := tr.Journal().Apply(provenance.OperationInput{
		OperationID:    "op-genesis",
		ActorID:        actor,
		CommandDigest:  []byte("genesis-c"),
		MutationDigest: []byte("genesis-m"),
		Effects:        []provenance.Effect{{Sort: provenance.EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("establishGenesis: %v", err)
	}
	for i := range res.ResultSlots {
		if string(res.ResultSlots[i].Slot) == "auth" {
			return res.ResultSlots[i].ProducedJournalID
		}
	}
	t.Fatal("establishGenesis: no bootstrap authority slot produced")
	return 0
}

func TestSession_CreateJournalsBirth(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)

	task, err := s.Create("aura", "REQUEST: port codegen", "desc",
		provenance.TaskTypeFeature, provenance.PriorityHigh, provenance.PhaseRequest)
	if err != nil {
		t.Fatalf("Session.Create: %v", err)
	}
	if task.Status != provenance.StatusOpen {
		t.Errorf("Status = %v, want StatusOpen", task.Status)
	}
	if task.ID.Namespace != "aura" {
		t.Errorf("ID.Namespace = %q, want aura", task.ID.Namespace)
	}
	if task.Type != provenance.TaskTypeFeature || task.Priority != provenance.PriorityHigh || task.Phase != provenance.PhaseRequest {
		t.Errorf("classification not round-tripped: %+v", task)
	}
	// The birth is journaled and reproducible from history.
	if _, err := tr.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections after Create: %v", err)
	}
}

func TestSession_CreateEmptyNamespaceRejected(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	_, err := tr.As(actor, boot).Create("", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if !errors.Is(err, provenance.ErrInvalidID) {
		t.Errorf("Create(empty namespace) err = %v, want ErrInvalidID", err)
	}
}

func TestSession_JournaledVerbsRequireGenesis(t *testing.T) {
	tr, actor := newSessionTracker(t)
	// No genesis established: the journal is empty.
	_, err := tr.As(actor, 0).Create("aura", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if !errors.Is(err, provenance.ErrGenesisRequired) {
		t.Errorf("Create on empty journal err = %v, want ErrGenesisRequired", err)
	}
}

func TestSession_UpdateMetadataMaterializesAndJournals(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)
	task, err := s.Create("aura", "old title", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	newTitle := "new title"
	updated, err := s.Update(task.ID, provenance.UpdateFields{Title: &newTitle})
	if err != nil {
		t.Fatalf("Update(Title): %v", err)
	}
	if updated.Title != newTitle {
		t.Errorf("Title = %q, want %q", updated.Title, newTitle)
	}
	if updated.Status != provenance.StatusOpen {
		t.Errorf("Status = %v, want unchanged StatusOpen", updated.Status)
	}
	if _, err := tr.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections after Update(Title): %v", err)
	}
}

func TestSession_StartJournalsStarted(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)
	task, err := s.Create("aura", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	updated, err := s.Start(task.ID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if updated.Status != provenance.StatusInProgress {
		t.Fatalf("Status = %v, want StatusInProgress", updated.Status)
	}
	// The native in_progress transition is journal-reproducible via the started event.
	if _, err := tr.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections after Start: %v", err)
	}
}

// TestSession_StopHaltsInProgress covers the in_progress → open transition (Session.Stop,
// provenance.task.stopped): an FSM arrow with no pre-tightening analogue.
func TestSession_StopHaltsInProgress(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)
	task, err := s.Create("aura", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Start(task.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopped, err := s.Stop(task.ID)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped.Status != provenance.StatusOpen {
		t.Fatalf("Status = %v, want StatusOpen after Stop", stopped.Status)
	}
	if _, err := tr.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections after Stop: %v", err)
	}
}

// TestSession_IllegalTransitionRejected pins the static FSM: a same-state transition and
// the direct closed → in_progress jump are rejected with the typed ErrStatusTransition,
// and WithForce coerces the illegal transition while keeping the journal reproducible.
func TestSession_IllegalTransitionRejected(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)
	task, err := s.Create("aura", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Reopen on an open task is a same-state (open→open) transition — rejected.
	if _, err := s.Reopen(task.ID); !errors.Is(err, provenance.ErrStatusTransition) {
		t.Errorf("Reopen(open task) err = %v, want ErrStatusTransition", err)
	}
	// Close, then the direct closed → in_progress jump is FSM-illegal (must Reopen then Start).
	if _, err := s.CloseTask(task.ID, "done"); err != nil {
		t.Fatalf("CloseTask: %v", err)
	}
	if _, err := s.Start(task.ID); !errors.Is(err, provenance.ErrStatusTransition) {
		t.Errorf("Start(closed task) err = %v, want ErrStatusTransition", err)
	}
	// WithForce coerces the FSM-illegal closed → in_progress transition; the coercion is
	// journaled with a forced marker and remains reproducible from history.
	forced, err := s.Start(task.ID, provenance.WithForce())
	if err != nil {
		t.Fatalf("Start(WithForce) on closed task: %v", err)
	}
	if forced.Status != provenance.StatusInProgress {
		t.Fatalf("Status = %v, want StatusInProgress after forced Start", forced.Status)
	}
	if _, err := tr.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections after forced coercion: %v", err)
	}
}

// TestSession_ForcedTransitionUnderNonGoverningAuthorityRejected is the direct regression
// pinning the authz-before-forced-branch ordering (§9.3, §8.1): WithForce coerces the FSM
// ONLY — never authorization. A Session bound to an assignment authority scoped to task X
// attempts a forced Start on a CLOSED task Y that authority does NOT govern; it must fail
// with ErrAuthorityScope and commit nothing, so a future refactor that reordered the forced
// branch ahead of the per-effect authorization check is caught immediately.
// TestSession_IllegalTransitionRejected only forces under the all-governing bootstrap
// authority, so it never proves this negative (force + wrong authority => still rejected).
func TestSession_ForcedTransitionUnderNonGoverningAuthorityRejected(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	sBoot := tr.As(actor, boot)

	// Two tasks born under the bootstrap authority.
	taskX, err := sBoot.Create("aura", "governed task X", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create taskX: %v", err)
	}
	taskY, err := sBoot.Create("aura", "ungoverned task Y", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create taskY: %v", err)
	}
	// Close taskY so a plain Start (closed → in_progress) is FSM-illegal — only WithForce
	// could coerce it. That makes the forced branch genuinely in play, so the test proves
	// authorization is checked BEFORE (not bypassed by) that branch, not merely that an
	// unforced illegal transition is rejected on other grounds.
	if _, err := sBoot.CloseTask(taskY.ID, "done"); err != nil {
		t.Fatalf("CloseTask taskY: %v", err)
	}

	// An assignment authority whose episode is on taskX — it governs taskX ONLY.
	authX := startAssignmentAuthority(t, tr, actor, boot, taskX.ID, "AUTH-X")
	sAssignX := tr.As(actor, authX)

	// Forced Start on the ungoverned taskY must fail with ErrAuthorityScope: force skips the
	// FSM, NEVER the §9.3 per-effect authorization check that runs unconditionally first.
	if _, err := sAssignX.Start(taskY.ID, provenance.WithForce()); !errors.Is(err, provenance.ErrAuthorityScope) {
		t.Fatalf("forced Start on ungoverned task err = %v, want ErrAuthorityScope", err)
	}

	// Nothing committed: taskY stays closed and no started event landed on it.
	got, err := tr.Show(taskY.ID)
	if err != nil {
		t.Fatalf("Show taskY: %v", err)
	}
	if got.Status != provenance.StatusClosed {
		t.Errorf("taskY status = %v, want StatusClosed (the forced write must not have landed)", got.Status)
	}
	page, err := tr.Journal().QueryTaskEvents(provenance.JournalQueryV1{
		TaskIDs:    []provenance.TaskID{taskY.ID},
		EventKinds: []provenance.EventKind{provenance.EventKindTaskStarted},
	})
	if err != nil {
		t.Fatalf("QueryTaskEvents taskY started: %v", err)
	}
	if len(page.Events) != 0 {
		t.Errorf("rejected forced Start committed %d started events on taskY, want 0", len(page.Events))
	}
	// The whole journal remains reproducible from history.
	if _, err := tr.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections after rejected forced Start: %v", err)
	}
}

// startAssignmentAuthority starts one owner-responsibility episode on `task` under `auth`
// through the public Journal().Apply path and returns the started transition's authority
// JournalID — an assignment authority governing ONLY `task`, for binding a Session with a
// bounded governance scope.
func startAssignmentAuthority(t *testing.T, tr provenance.Tracker, actor provenance.ActorID, auth provenance.JournalID, task provenance.TaskID, assignment provenance.AssignmentID) provenance.JournalID {
	t.Helper()
	res, err := tr.Journal().Apply(provenance.OperationInput{
		OperationID:        provenance.OperationID("op-start-" + string(assignment)),
		ActorID:            actor,
		AuthorityJournalID: &auth,
		CommandDigest:      []byte("start-" + string(assignment) + "-c"),
		MutationDigest:     []byte("start-" + string(assignment) + "-m"),
		Effects: []provenance.Effect{{
			Sort: provenance.EffectAssignmentStart, AssignmentID: assignment, TaskID: task,
			SlotID: provenance.SlotOwnerResponsibility, Occupant: actor, ResultSlot: "auth",
		}},
	})
	if err != nil {
		t.Fatalf("startAssignmentAuthority %q: %v", assignment, err)
	}
	for i := range res.ResultSlots {
		if string(res.ResultSlots[i].Slot) == "auth" {
			return res.ResultSlots[i].ProducedJournalID
		}
	}
	t.Fatalf("startAssignmentAuthority %q produced no authority slot", assignment)
	return 0
}

func TestSession_UpdateEmptyIsNoOp(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)
	task, err := s.Create("aura", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// An Update carrying no metadata field must not journal an operation (Status is not
	// an Update field; the lifecycle is governed by the dedicated verbs under the FSM).
	before, err := tr.Journal().QueryTaskEvents(provenance.JournalQueryV1{})
	if err != nil {
		t.Fatalf("QueryTaskEvents before: %v", err)
	}
	if _, err := s.Update(task.ID, provenance.UpdateFields{}); err != nil {
		t.Fatalf("Update(empty): %v", err)
	}
	after, err := tr.Journal().QueryTaskEvents(provenance.JournalQueryV1{})
	if err != nil {
		t.Fatalf("QueryTaskEvents after: %v", err)
	}
	if len(after.Events) != len(before.Events) {
		t.Errorf("empty Update journaled %d new rows, want 0", len(after.Events)-len(before.Events))
	}
}

func TestSession_UpdateOwnerRejected(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)
	task, err := s.Create("aura", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	owner := actor
	if _, err := s.Update(task.ID, provenance.UpdateFields{Owner: &owner}); err == nil {
		t.Error("Update(Owner) succeeded, want an actionable rejection (owner is reducer-exclusive)")
	}
}

func TestSession_CloseTaskJournalsClosure(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)
	task, err := s.Create("aura", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	closed, err := s.CloseTask(task.ID, "done")
	if err != nil {
		t.Fatalf("CloseTask: %v", err)
	}
	if closed.Status != provenance.StatusClosed {
		t.Errorf("Status = %v, want StatusClosed", closed.Status)
	}
	if closed.CloseReason != "done" {
		t.Errorf("CloseReason = %q, want done", closed.CloseReason)
	}
	// Closing an already-closed task is an FSM-illegal same-state transition.
	if _, err := s.CloseTask(task.ID, "again"); !errors.Is(err, provenance.ErrStatusTransition) {
		t.Errorf("re-close err = %v, want ErrStatusTransition", err)
	}
	if _, err := tr.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections after CloseTask: %v", err)
	}
}

func TestSession_CloseThenReopenConverges(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)
	task, err := s.Create("aura", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.CloseTask(task.ID, "done"); err != nil {
		t.Fatalf("CloseTask: %v", err)
	}
	reopened, err := s.Reopen(task.ID)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if reopened.Status != provenance.StatusOpen {
		t.Errorf("Status = %v, want StatusOpen after reopen", reopened.Status)
	}
	if _, err := tr.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections after reopen: %v", err)
	}
}

func TestSession_PinnedOperationIDIsIdempotent(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)
	task, err := s.Create("aura", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	desc := "materialized"
	pin := provenance.WithOperationID("op-update-pinned")
	if _, err := s.Update(task.ID, provenance.UpdateFields{Description: &desc}, pin); err != nil {
		t.Fatalf("Update pinned (1): %v", err)
	}
	before, err := tr.Journal().QueryTaskEvents(provenance.JournalQueryV1{})
	if err != nil {
		t.Fatalf("QueryTaskEvents: %v", err)
	}
	// Same pinned OperationID + same arguments short-circuits (§9.4): no new rows.
	if _, err := s.Update(task.ID, provenance.UpdateFields{Description: &desc}, pin); err != nil {
		t.Fatalf("Update pinned (2): %v", err)
	}
	after, err := tr.Journal().QueryTaskEvents(provenance.JournalQueryV1{})
	if err != nil {
		t.Fatalf("QueryTaskEvents: %v", err)
	}
	if len(after.Events) != len(before.Events) {
		t.Errorf("pinned retry journaled %d new rows, want 0 (idempotent replay)", len(after.Events)-len(before.Events))
	}
}

// TestSession_RelationshipVerbsAreJournaled pins the §6 amendment: edge/label/comment
// verbs journal one mutation-family event each (who-provenance), the domain projections
// take effect, and the whole history replays from empty (convergence).
func TestSession_RelationshipVerbsAreJournaled(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)
	a, err := s.Create("aura", "a", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := s.Create("aura", "b", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	before, err := tr.Journal().QueryTaskEvents(provenance.JournalQueryV1{})
	if err != nil {
		t.Fatalf("QueryTaskEvents: %v", err)
	}
	if err := s.AddEdge(b.ID, a.ID.String(), provenance.EdgeBlockedBy); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := s.AddLabel(a.ID, "priority"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if _, err := s.AddComment(a.ID, actor, "looks good"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	after, err := tr.Journal().QueryTaskEvents(provenance.JournalQueryV1{})
	if err != nil {
		t.Fatalf("QueryTaskEvents: %v", err)
	}
	// §6 amendment: each relationship/annotation verb journals exactly one row.
	if got := len(after.Events) - len(before.Events); got != 3 {
		t.Errorf("relationship verbs journaled %d rows, want 3 (edge+label+comment)", got)
	}
	// Who-provenance: the edge-add row is attributed to the committing actor at a
	// definite journal position, and its operands decode from the journal payload.
	edgePage, err := tr.Journal().QueryTaskEvents(provenance.JournalQueryV1{
		TaskIDs: []provenance.TaskID{b.ID}, EventKinds: []provenance.EventKind{provenance.EventKindEdgeAdded},
	})
	if err != nil {
		t.Fatalf("QueryTaskEvents(edge): %v", err)
	}
	if len(edgePage.Events) != 1 {
		t.Fatalf("edge-added events = %d, want 1", len(edgePage.Events))
	}
	ev := edgePage.Events[0]
	if ev.ActorID.String() != actor.String() {
		t.Errorf("edge-added committer = %s, want %s", ev.ActorID.String(), actor.String())
	}
	if ev.JournalID == 0 {
		t.Errorf("edge-added has no journal position")
	}
	edgePayload, err := provenance.DecodeEdgeMutationPayload(ev.Payload)
	if err != nil {
		t.Fatalf("decode edge payload: %v", err)
	}
	if edgePayload.Target != a.ID.String() || edgePayload.EdgeKind != provenance.EdgeBlockedBy {
		t.Errorf("edge payload = %+v, want target=%s kind=blocked_by", edgePayload, a.ID.String())
	}
	// The domain projections took effect.
	edges, err := tr.Edges(b.ID, nil)
	if err != nil || len(edges) != 1 {
		t.Errorf("Edges(b) = %v (err %v), want 1 edge", edges, err)
	}
	labels, err := tr.Labels(a.ID)
	if err != nil || len(labels) != 1 {
		t.Errorf("Labels(a) = %v (err %v), want 1 label", labels, err)
	}
	comments, err := tr.Comments(a.ID)
	if err != nil || len(comments) != 1 {
		t.Errorf("Comments(a) = %v (err %v), want 1 comment", comments, err)
	}
	// The whole journaled history — task births plus edge/label/comment — replays from
	// empty and converges (the domain projections are journal-reproducible).
	if _, err := tr.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections after relationship verbs: %v", err)
	}
}

func TestSession_AtomicStartEpisodeSetsOwner(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	s := tr.As(actor, boot)
	task, err := s.Create("aura", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Atomic(func(op *provenance.Operation) {
		op.StartEpisode("assign-1", task.ID, actor)
	}); err != nil {
		t.Fatalf("Atomic(StartEpisode): %v", err)
	}
	got, err := tr.Show(task.ID)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if got.Owner == nil || got.Owner.String() != actor.String() {
		t.Errorf("Owner = %v, want %s", got.Owner, actor.String())
	}
	if _, err := tr.Journal().ReplayProjections(); err != nil {
		t.Errorf("ReplayProjections after Atomic episode: %v", err)
	}
}

func TestSession_AtomicEmptyRejected(t *testing.T) {
	tr, actor := newSessionTracker(t)
	boot := establishGenesis(t, tr, actor)
	if _, err := tr.As(actor, boot).Atomic(func(op *provenance.Operation) {}); err == nil {
		t.Error("Atomic with no effects succeeded, want an actionable rejection")
	}
}
