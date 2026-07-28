package provenance

// session.go is the mutation SDK over the global journal
// (docs/journal-relational-contract.md). A Session binds a committing actor and a
// governing authority once (Tracker.As) and exposes the task-tracker mutation verbs on
// that receiver. Every verb is JOURNALED: each builds one logical operation (§9) and
// commits it through Apply, so every birth, metadata change, status transition, closure,
// edge, label, and comment flows through the ordered journal and is reproducible from
// journal history (§8.1, §15).
//
// Task lifecycle. Create mints a TaskID and journals the birth (status open). Update is
// METADATA-ONLY (title/description/priority/phase/notes → one provenance.task.updated
// event); it never changes status. The status lifecycle is governed by four DEDICATED
// verbs under a static FSM (§8.1): Start (open → in_progress), Stop (in_progress → open),
// CloseTask ({open,in_progress} → closed), Reopen (closed → open). Each journals its own
// fixed lifecycle kind; the shared reducer rejects an illegal transition (e.g. a direct
// closed → in_progress, or a same-state repeat) with the typed ErrStatusTransition. Any
// lifecycle verb may be invoked WithForce as the escape hatch: the coercion is journaled
// with a forced marker, skips the FSM (only the FSM — never authorization, §9.3), and is
// reproducible from history.
//
// Relationship / annotation verbs — AddEdge, RemoveEdge, AddLabel, RemoveLabel,
// AddComment — are ALSO journaled (§6): each commits one typed
// mutation-family effect (provenance.edge.added/removed, provenance.label.added/removed,
// provenance.comment.added) under the same per-effect authorization discipline as task
// events, so who added/removed an edge/label/comment, under which authority, at which
// journal position, is queryable from the journal (who-provenance). The edges/labels/
// comments domain tables are shared-reducer projections re-derivable from journal history;
// the verb signatures are unchanged. A journaled edge's CREATION being recorded does NOT
// make the edge grant authority — §14.5 (a blocked_by edge delegates no ownership) stands.
//
// Retry caveat (LOUD): every journaled verb defaults to a fresh UUIDv7 OperationID, so
// a naive retry after an ambiguous failure commits a SECOND operation. To make a
// journaled mutation safe to retry, pin a stable OperationID with WithOperationID and
// reuse it verbatim on the retry: an exact same-OperationID replay short-circuits to
// the original committed result without re-executing (§9.4), and a reused OperationID
// presenting different arguments is a typed conflict (§11). Create marks its freshly
// minted UUIDv7 as an allocation, so overlapping pinned retries reconcile different
// provisional UUIDs from the committed result and return the original task.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/internal/allocation"
	"github.com/google/uuid"
)

// Session is the mutation SDK bound to one committing actor and one governing
// authority (Tracker.As). It is a thin value; obtain a fresh one per (actor,
// authority) pair. Its verbs are safe for concurrent use to the same extent as the
// underlying Tracker (the journal write path serializes operations, §9.5).
type Session struct {
	tr        *sqliteTracker
	actor     ActorID
	authority JournalID
	// gate, when non-nil, is the borrowed-handle liveness precheck a borrowed Tracker
	// installs (borrowedTracker.As): every public verb calls it FIRST so, once the
	// owning DBOS root has shut down, every Session mutation returns a
	// StoreUnavailableError instead of writing through the still-open bridge
	// connection. A standalone Session (OpenSQLite/OpenMemory) leaves it nil.
	gate func(op string) error
}

// As implements Tracker.As.
func (t *sqliteTracker) As(actor ActorID, authority JournalID) *Session {
	return &Session{tr: t, actor: actor, authority: authority}
}

// checkGate runs the borrowed liveness precheck (a no-op for a standalone Session),
// returning a *StoreUnavailableError when the borrowed handle's owning root has shut
// down. Every public Session verb calls it before touching the store.
func (s *Session) checkGate(op string) error {
	if s.gate == nil {
		return nil
	}
	return s.gate("Session." + op)
}

// AllocateGoverned creates a governed child batch under this Session's exact
// bound assignment authority. The request repeats ActorID deliberately so its
// canonical operation identity is self-contained; it must match the Session
// actor before the transaction begins.
func (s *Session) AllocateGoverned(ctx context.Context, request GovernedAllocationRequest) (OperationClosure, error) {
	if err := s.checkGate("AllocateGoverned"); err != nil {
		return OperationClosure{}, err
	}
	if request.ActorID != s.actor {
		return OperationClosure{}, allocation.NewError(
			allocation.ErrorAuthority, request.OperationID, "Session.AllocateGoverned",
			"the request ActorID differs from the actor bound to this Session",
			"nothing was written; actor attribution cannot be silently substituted",
			"construct the request with the Session actor or bind a Session for the request actor", nil)
	}
	closure, err := s.tr.db.AllocateGovernedForAuthority(ctx, request, s.authority)
	if err != nil {
		return OperationClosure{}, fmt.Errorf("provenance.Session.AllocateGoverned: %w", err)
	}
	return closure, nil
}

// AllocateGovernedComposed is the source-compatible one-child convenience
// wrapper over AllocateGovernedComposedBatch.
func (s *Session) AllocateGovernedComposed(ctx context.Context, request GovernedAllocationComposedRequest) (GovernedAllocationComposedResult, error) {
	if len(request.Allocation.Children) != 1 {
		return GovernedAllocationComposedResult{}, allocation.NewError(allocation.ErrorValidation, request.Allocation.OperationID, "Session.AllocateGovernedComposed", "the legacy composed allocation wrapper requires exactly one child", "nothing was written", "use AllocateGovernedComposedBatch for 1..128 ordered children", nil)
	}
	return s.AllocateGovernedComposedBatch(ctx, request)
}

// AllocateGovernedComposedBatch creates 1..128 ordered governed children and
// reduces the shared ordered supplemental closure in the same SQLite
// transaction. Supplements are authenticated against the complete child list.
func (s *Session) AllocateGovernedComposedBatch(ctx context.Context, request GovernedAllocationComposedBatchRequest) (GovernedAllocationComposedBatchResult, error) {
	if err := s.checkGate("AllocateGovernedComposedBatch"); err != nil {
		return GovernedAllocationComposedBatchResult{}, err
	}
	canonical, _, err := allocation.CanonicalizeComposed(request)
	if err != nil {
		return GovernedAllocationComposedBatchResult{}, fmt.Errorf("provenance.Session.AllocateGovernedComposedBatch: %w", err)
	}
	request, err = allocation.DecodeComposedRequest(canonical)
	if err != nil {
		return GovernedAllocationComposedBatchResult{}, fmt.Errorf("provenance.Session.AllocateGovernedComposedBatch: copy canonical request: %w", err)
	}
	if request.Allocation.ActorID != s.actor {
		return GovernedAllocationComposedResult{}, allocation.NewError(
			allocation.ErrorAuthority, request.Allocation.OperationID, "Session.AllocateGovernedComposedBatch",
			"the request ActorID differs from the actor bound to this Session",
			"nothing was written; actor attribution cannot be silently substituted",
			"construct the request with the Session actor or bind a Session for the request actor", nil)
	}
	result, err := s.tr.db.AllocateGovernedComposedForAuthority(ctx, request, s.authority)
	if err != nil {
		return GovernedAllocationComposedBatchResult{}, fmt.Errorf("provenance.Session.AllocateGovernedComposedBatch: %w", err)
	}
	return result, nil
}

// ErrGenesisRequired is returned by a journaled Session verb invoked against a journal
// that holds no rows at all: no genesis bootstrap authority has been established and no
// legacy baseline migrated, so there is no authority any operation could execute under
// (§4.6, §13). errors.Is recovers it from the wrapped, actionable error.
var ErrGenesisRequired = errors.New("provenance: journal has no genesis authority")

// ---------------------------------------------------------------------------
// Per-operation options
// ---------------------------------------------------------------------------

// ApplyOption customizes one journaled Session operation.
type ApplyOption func(*applyConfig)

type applyConfig struct {
	opID           OperationID
	commandDigest  []byte
	mutationDigest []byte
	forced         bool
}

// WithForce marks a lifecycle transition verb (Start/Stop/CloseTask/Reopen) as a
// FORCED coercion: the reducer records a forced marker in the journal row and skips the
// static status FSM (§8.1) for that one transition, so an out-of-FSM status change is
// committed, journal-reproducible, and audit-visible. Force is the deliberate escape
// hatch (the CLI `--force`); it NEVER bypasses authorization (§9.3), only the FSM, and
// it is ignored by the metadata-only Update. A forced transition digests differently
// from an unforced one, so a forced retry never collides with an unforced attempt (§9.4).
func WithForce() ApplyOption {
	return func(c *applyConfig) { c.forced = true }
}

// WithOperationID pins the operation's caller-defined, nonempty, control-free
// OperationID instead of minting a fresh UUIDv7-backed key.
// Reuse the SAME id verbatim on a retry to make the mutation idempotent: an exact
// same-identity replay short-circuits to the original result (§9.4); a reused id with
// different arguments is a typed conflict (§11). See the package retry caveat.
func WithOperationID(id OperationID) ApplyOption {
	return func(c *applyConfig) { c.opID = id }
}

// WithCommandDigest overrides the operation's command digest (§3.1). By default the
// Session derives a deterministic digest from the verb and its arguments, so a pinned
// same-argument retry matches and a changed-argument reuse conflicts; override only to
// supply a caller-computed provenance digest.
func WithCommandDigest(d []byte) ApplyOption {
	return func(c *applyConfig) { c.commandDigest = append([]byte(nil), d...) }
}

// WithMutationDigest preserves the legacy opaque-digest option for source compatibility
// and retries of explicit legacy operation rows. Canonical new writes always derive the
// persisted mutation digest from their canonical effects; this value cannot override or
// forge that identity.
func WithMutationDigest(d []byte) ApplyOption {
	return func(c *applyConfig) { c.mutationDigest = append([]byte(nil), d...) }
}

func (s *Session) resolve(opts []ApplyOption, canonical ...string) applyConfig {
	cfg := applyConfig{
		opID:           OperationID("provenance.op." + uuid.Must(uuid.NewV7()).String()),
		commandDigest:  digestOf(append([]string{"command", s.actor.String()}, canonical...)...),
		mutationDigest: digestOf(append([]string{"mutation", s.actor.String()}, canonical...)...),
	}
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

func digestOf(parts ...string) []byte {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum(nil)
}

// ---------------------------------------------------------------------------
// Shared journaled-apply core
// ---------------------------------------------------------------------------

// requireInitialized turns a journaled mutation against a never-initialized journal
// into an actionable ErrGenesisRequired rather than a raw authority-not-found deep in
// Apply (§4.6, §13).
func (s *Session) requireInitialized(verb string) error {
	empty, err := s.tr.db.JournalIsEmpty()
	if err != nil {
		return fmt.Errorf("provenance.Session.%s: check journal state: %w", verb, err)
	}
	if empty {
		return fmt.Errorf(
			"%w: Session.%s — the journal is empty, so no authority governs this mutation — "+
				"where: Session journaled write path (§4.6, §13); when: before any row is written; "+
				"impact: nothing was committed; fix: establish a genesis bootstrap authority first "+
				"(Tracker.Journal().Apply with a nil AuthorityJournalID producing one EffectBootstrapAuthority) "+
				"or migrate a legacy baseline (MigrateLegacyBaseline), then bind Tracker.As to the produced "+
				"authority's JournalID",
			ErrGenesisRequired, verb)
	}
	return nil
}

// applyOne commits a single logical operation carrying the given effects under this
// Session's actor and authority.
func (s *Session) applyOne(cfg applyConfig, effects []Effect) (CommittedResult, error) {
	auth := s.authority
	in := OperationInput{
		OperationID:        cfg.opID,
		ActorID:            s.actor,
		AuthorityJournalID: &auth,
		CommandDigest:      cfg.commandDigest,
		MutationDigest:     cfg.mutationDigest,
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects:            effects,
	}
	return s.tr.db.Apply(in)
}

// ---------------------------------------------------------------------------
// Journaled task-lifecycle verbs (§8.1, §9)
// ---------------------------------------------------------------------------

// Create journals the birth of a new task (§8.1): it mints a UUIDv7 TaskID in the
// given namespace and commits one EffectTaskCreate, so the task is born through the
// journal (status Open, a non-NULL watermark, creator attribution) rather than a
// direct write. Returns ErrInvalidID for an empty namespace, ErrGenesisRequired if no
// genesis authority exists yet.
func (s *Session) Create(namespace, title, description string, taskType TaskType, priority Priority, phase Phase, opts ...ApplyOption) (Task, error) {
	if err := s.checkGate("Create"); err != nil {
		return Task{}, err
	}
	if namespace == "" {
		return Task{}, fmt.Errorf(
			"%w: Session.Create — namespace is empty — "+
				"provide a non-empty namespace string such as 'aura-plugins' or 'my-project'",
			ErrInvalidID)
	}
	cfg := s.resolve(opts, "create", namespace, title, description,
		fmt.Sprintf("%d/%d/%d", int(taskType), int(priority), int(phase)))
	// Every attempt allocates a genuine UUIDv7. The allocated-create effect tells
	// Apply that a retry's provisional UUID may be reconciled from the committed
	// "task" result slot before canonical replay comparison.
	taskUUID := uuid.Must(uuid.NewV7())
	if err := s.requireInitialized("Create"); err != nil {
		return Task{}, err
	}
	id := TaskID{Namespace: namespace, UUID: taskUUID}
	res, err := s.applyOne(cfg, []Effect{{
		Sort:        EffectTaskCreateAllocated,
		TaskID:      id,
		Title:       title,
		Description: description,
		Type:        taskType,
		Priority:    priority,
		Phase:       phase,
		ResultSlot:  "task",
	}})
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Session.Create: %w", err)
	}
	// Resolve the committed task id from the result slot: on a §9.4 short-circuit this
	// is the ORIGINAL task, not this call's discarded minted id.
	if tid, ok := taskSlotID(res, "task"); ok {
		id = tid
	}
	task, found, err := s.tr.db.GetTask(id)
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Session.Create: read back task %q: %w", id.String(), err)
	}
	if !found {
		return Task{}, fmt.Errorf(
			"provenance.Session.Create: task %q was committed but could not be read back — "+
				"where: Session.Create read-back; impact: the create succeeded but the returned Task is empty; "+
				"fix: re-open the tracker and Show the task", id.String())
	}
	return task, nil
}

// Update applies a partial METADATA UpdateFields to an existing task as ONE journaled
// operation (§8.1, §16). It is metadata-only: any of Title/Description/Priority/Phase/Notes
// emit a single provenance.task.updated event materializing those columns. Status is NOT
// a metadata field — the lifecycle is governed by the dedicated verbs Start/Stop/CloseTask/
// Reopen under the static FSM (§8.1), so a status change never rides an Update. Owner is
// likewise not settable — owner is reducer-exclusive, moved only through assignment
// episodes — so a non-nil Owner is rejected with an actionable error. When no field
// changes anything nothing is committed and the current task is returned. Returns
// ErrNotFound if the task does not exist, ErrGenesisRequired if no genesis authority
// exists yet.
func (s *Session) Update(id TaskID, fields UpdateFields, opts ...ApplyOption) (Task, error) {
	if err := s.checkGate("Update"); err != nil {
		return Task{}, err
	}
	if err := s.requireInitialized("Update"); err != nil {
		return Task{}, err
	}
	current, found, err := s.tr.db.GetTask(id)
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Session.Update: %w", err)
	}
	if !found {
		return Task{}, fmt.Errorf(
			"%w: Session.Update — task %q does not exist — "+
				"verify the TaskID was obtained from Create or a previous List/Show call",
			ErrNotFound, id.String())
	}
	if fields.Owner != nil {
		return Task{}, fmt.Errorf(
			"provenance.Session.Update: task %q — Owner cannot be set through Update — "+
				"where: Session.Update decomposition (§8.1); why: owner is a reducer-exclusive projection "+
				"moved only by owner-responsibility assignment episodes, never a direct column write; "+
				"impact: nothing committed; fix: transfer ownership with an assignment start/end effect via "+
				"Session.Atomic (EffectAssignmentStart/EffectAssignmentEnd)", id.String())
	}

	var effects []Effect
	if fields.Title != nil || fields.Description != nil || fields.Priority != nil ||
		fields.Phase != nil || fields.Notes != nil {
		effects = append(effects, Effect{
			Sort:              EffectTaskEvent,
			TaskID:            id,
			EventKind:         EventKindTaskUpdated,
			UpdateTitle:       fields.Title,
			UpdateDescription: fields.Description,
			UpdatePriority:    fields.Priority,
			UpdatePhase:       fields.Phase,
			UpdateNotes:       fields.Notes,
		})
	}
	if len(effects) == 0 {
		// Journal-honest no-op: nothing changed, so no operation is committed.
		return current, nil
	}
	cfg := s.resolve(opts, "update", id.String(), updateCanonical(fields))
	if _, err := s.applyOne(cfg, effects); err != nil {
		return Task{}, fmt.Errorf("provenance.Session.Update: %w", err)
	}
	task, _, err := s.tr.db.GetTask(id)
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Session.Update: read back task %q: %w", id.String(), err)
	}
	return task, nil
}

// Start transitions an existing task open → in_progress as ONE journaled operation,
// committing a provenance.task.started lifecycle event. The static FSM rejects a Start
// from any non-open status (§8.1) with ErrStatusTransition unless WithForce is passed.
func (s *Session) Start(id TaskID, opts ...ApplyOption) (Task, error) {
	return s.setStatus("Start", id, EventKindTaskStarted, "", opts)
}

// Stop transitions an existing task in_progress → open as ONE journaled operation,
// committing a provenance.task.stopped lifecycle event — halting active work without
// closing the task. The static FSM rejects a Stop from any non-in_progress status (§8.1)
// with ErrStatusTransition unless WithForce is passed.
func (s *Session) Stop(id TaskID, opts ...ApplyOption) (Task, error) {
	return s.setStatus("Stop", id, EventKindTaskStopped, "", opts)
}

// Reopen transitions an existing task closed → open as ONE journaled operation,
// committing a provenance.task.reopened lifecycle event. The static FSM rejects a Reopen
// from any non-closed status (§8.1) with ErrStatusTransition unless WithForce is passed.
func (s *Session) Reopen(id TaskID, opts ...ApplyOption) (Task, error) {
	return s.setStatus("Reopen", id, EventKindTaskReopened, "", opts)
}

// CloseTask transitions an existing task {open,in_progress} → closed as ONE journaled
// operation, committing a provenance.task.closed lifecycle event that also materializes
// the close reason. Closing an already-closed task is rejected by the static FSM (§8.1)
// with ErrStatusTransition unless WithForce is passed. Returns ErrNotFound if the task
// does not exist, ErrGenesisRequired if no genesis authority exists yet.
func (s *Session) CloseTask(id TaskID, reason string, opts ...ApplyOption) (Task, error) {
	return s.setStatus("CloseTask", id, EventKindTaskClosed, reason, opts)
}

// setStatus is the single unexported implementation behind the four dedicated lifecycle
// verbs (§16). It journals one transition lifecycle event of the given kind under this
// Session's actor and authority; the shared reducer enforces the static FSM against the
// task's current status (§8.1). When cfg.forced (WithForce) is set it records a forced
// marker in the journal row so the reducer skips the FSM for that one transition — the
// escape hatch is journal-reproducible and audit-visible, and never bypasses
// authorization (§9.3), only the FSM. closeReason is materialized only for the close
// kind. Returns ErrNotFound if the task does not exist, ErrGenesisRequired if no genesis
// authority exists yet, and the typed ErrStatusTransition on an FSM-illegal unforced
// transition.
func (s *Session) setStatus(verb string, id TaskID, kind EventKind, closeReason string, opts []ApplyOption) (Task, error) {
	// Borrowed-mode liveness gate for the whole lifecycle-verb family (Start, Stop,
	// Reopen, CloseTask): once the owning DBOS root has shut down, every transition
	// returns a StoreUnavailableError instead of writing through the still-open bridge
	// connection. A no-op for a standalone Session.
	if err := s.checkGate(verb); err != nil {
		return Task{}, err
	}
	if err := s.requireInitialized(verb); err != nil {
		return Task{}, err
	}
	_, found, err := s.tr.db.GetTask(id)
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Session.%s: %w", verb, err)
	}
	if !found {
		return Task{}, fmt.Errorf(
			"%w: Session.%s — task %q does not exist — "+
				"verify the TaskID was obtained from Create or a previous List/Show call",
			ErrNotFound, verb, id.String())
	}
	cfg := s.resolve(opts, "lifecycle", id.String(), string(kind), closeReason,
		fmt.Sprintf("forced=%t", forcedOf(opts)))
	if _, err := s.applyOne(cfg, []Effect{{
		Sort:        EffectTaskEvent,
		TaskID:      id,
		EventKind:   kind,
		CloseReason: closeReason,
		Forced:      cfg.forced,
	}}); err != nil {
		return Task{}, fmt.Errorf("provenance.Session.%s: %w", verb, err)
	}
	task, _, err := s.tr.db.GetTask(id)
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Session.%s: read back task %q: %w", verb, id.String(), err)
	}
	return task, nil
}

// forcedOf reports whether the option set requests a forced transition, so the digest
// canonical distinguishes a forced coercion from the same unforced transition (a forced
// retry never collides with an unforced attempt under §9.4).
func forcedOf(opts []ApplyOption) bool {
	var c applyConfig
	for _, o := range opts {
		o(&c)
	}
	return c.forced
}

func updateCanonical(f UpdateFields) string {
	part := func(name string, set bool, val string) string {
		if !set {
			return name + "=-"
		}
		return name + "=" + val
	}
	sp := func(p *string) (bool, string) {
		if p == nil {
			return false, ""
		}
		return true, *p
	}
	tSet, tVal := sp(f.Title)
	dSet, dVal := sp(f.Description)
	nSet, nVal := sp(f.Notes)
	out := part("title", tSet, tVal) + "|" + part("desc", dSet, dVal) + "|" + part("notes", nSet, nVal)
	if f.Priority != nil {
		out += fmt.Sprintf("|prio=%d", int(*f.Priority))
	}
	if f.Phase != nil {
		out += fmt.Sprintf("|phase=%d", int(*f.Phase))
	}
	return out
}

func taskSlotID(res CommittedResult, slot string) (TaskID, bool) {
	for i := range res.ResultSlots {
		b := res.ResultSlots[i]
		if string(b.Slot) == slot && b.TaskID != nil {
			return *b.TaskID, true
		}
	}
	return TaskID{}, false
}

// ---------------------------------------------------------------------------
// Journaled relationship / annotation verbs (contract §6)
// ---------------------------------------------------------------------------
//
// These five verbs are journaled single-effect wrappers over Apply: each commits one
// typed mutation-family effect (provenance.edge.added/removed, provenance.label.added/
// removed, provenance.comment.added) under this Session's actor and authority, so who
// added/removed the relationship, under which authority, at which journal position is
// queryable from the journal (who-provenance). The signatures are UNCHANGED from the
// pre-journal forms. The edges/labels/comments domain tables are shared-reducer
// projections re-derivable from journal history (§6, §15). Authorization is the same
// per-effect discipline as a task event (§9.3): the source/subject task must be governed.
// A journaled edge's CREATION being recorded does NOT make the edge grant authority —
// §14.5 (a blocked_by edge delegates no ownership) is unchanged.

// AddEdge journals a typed edge from sourceID to targetID (§6). For EdgeBlockedBy, the
// fold enforces cycle detection (ErrCycleDetected). Adding an edge that already exists is
// a journal-honest re-assertion whose projection is idempotent.
func (s *Session) AddEdge(sourceID TaskID, targetID string, kind EdgeKind) error {
	if err := s.checkGate("AddEdge"); err != nil {
		return err
	}
	if err := s.requireInitialized("AddEdge"); err != nil {
		return err
	}
	cfg := s.resolve(nil, "edge-add", sourceID.String(), targetID, fmt.Sprintf("k=%d", int(kind)))
	if _, err := s.applyOne(cfg, []Effect{{
		Sort: EffectEdgeAdd, TaskID: sourceID, EdgeTargetID: targetID, EdgeRelKind: kind,
	}}); err != nil {
		return fmt.Errorf("provenance.Session.AddEdge: %w", err)
	}
	return nil
}

// RemoveEdge journals the removal of the edge from sourceID to targetID (§6). Idempotent:
// removing an absent edge journals the intent and projects a no-op.
func (s *Session) RemoveEdge(sourceID TaskID, targetID string, kind EdgeKind) error {
	if err := s.checkGate("RemoveEdge"); err != nil {
		return err
	}
	if err := s.requireInitialized("RemoveEdge"); err != nil {
		return err
	}
	cfg := s.resolve(nil, "edge-remove", sourceID.String(), targetID, fmt.Sprintf("k=%d", int(kind)))
	if _, err := s.applyOne(cfg, []Effect{{
		Sort: EffectEdgeRemove, TaskID: sourceID, EdgeTargetID: targetID, EdgeRelKind: kind,
	}}); err != nil {
		return fmt.Errorf("provenance.Session.RemoveEdge: %w", err)
	}
	return nil
}

// AddLabel journals attaching a label to a task (§6). Idempotent.
func (s *Session) AddLabel(id TaskID, label string) error {
	if err := s.checkGate("AddLabel"); err != nil {
		return err
	}
	if err := s.requireInitialized("AddLabel"); err != nil {
		return err
	}
	cfg := s.resolve(nil, "label-add", id.String(), label)
	if _, err := s.applyOne(cfg, []Effect{{Sort: EffectLabelAdd, TaskID: id, Label: label}}); err != nil {
		return fmt.Errorf("provenance.Session.AddLabel: %w", err)
	}
	return nil
}

// RemoveLabel journals detaching a label from a task (§6). Idempotent.
func (s *Session) RemoveLabel(id TaskID, label string) error {
	if err := s.checkGate("RemoveLabel"); err != nil {
		return err
	}
	if err := s.requireInitialized("RemoveLabel"); err != nil {
		return err
	}
	cfg := s.resolve(nil, "label-remove", id.String(), label)
	if _, err := s.applyOne(cfg, []Effect{{Sort: EffectLabelRemove, TaskID: id, Label: label}}); err != nil {
		return fmt.Errorf("provenance.Session.RemoveLabel: %w", err)
	}
	return nil
}

// AddComment journals a comment on a task authored by authorID (§6). A UUIDv7 CommentID
// is minted and carried in the journal payload so a from-empty replay reproduces the SAME
// comment. The committing actor (this Session's actor) is the who-provenance witness; the
// authored-by actor may differ and is recorded on the comment.
func (s *Session) AddComment(id TaskID, authorID AgentID, body string) (Comment, error) {
	if err := s.checkGate("AddComment"); err != nil {
		return Comment{}, err
	}
	if err := s.requireInitialized("AddComment"); err != nil {
		return Comment{}, err
	}
	commentID := CommentID{Namespace: id.Namespace, UUID: uuid.Must(uuid.NewV7())}
	cfg := s.resolve(nil, "comment-add", id.String(), commentID.String(), authorID.String(), body)
	if _, err := s.applyOne(cfg, []Effect{{
		Sort: EffectCommentAdd, TaskID: id, CommentIdentity: commentID, CommentAuthor: authorID, CommentBody: body,
	}}); err != nil {
		return Comment{}, fmt.Errorf("provenance.Session.AddComment: %w", err)
	}
	comment, found, err := s.tr.db.GetComment(commentID)
	if err != nil {
		return Comment{}, fmt.Errorf("provenance.Session.AddComment: read back comment %q: %w", commentID.String(), err)
	}
	if !found {
		return Comment{}, fmt.Errorf(
			"provenance.Session.AddComment: comment %q was committed but could not be read back — "+
				"where: AddComment read-back; impact: the comment was journaled but the returned value is "+
				"empty; fix: re-open the tracker and list the task's comments", commentID.String())
	}
	return comment, nil
}

// ---------------------------------------------------------------------------
// Atomic multi-effect composition (§9.3)
// ---------------------------------------------------------------------------

// Operation accumulates the ordered effects of one logical operation for
// Session.Atomic. Effects fold in the order they are added (§9.3.1) — e.g. start an
// owner-responsibility episode and immediately consume it under the same operation.
type Operation struct {
	effects []Effect
}

// Add appends an already-built effect, the escape hatch for effect shapes without a
// named helper. Returns the builder for chaining.
func (o *Operation) Add(e Effect) *Operation {
	o.effects = append(o.effects, e)
	return o
}

// CreateTask appends an EffectTaskCreate for a caller-supplied id.
func (o *Operation) CreateTask(id TaskID, title, description string, taskType TaskType, priority Priority, phase Phase) *Operation {
	return o.Add(Effect{
		Sort: EffectTaskCreate, TaskID: id, Title: title, Description: description,
		Type: taskType, Priority: priority, Phase: phase,
	})
}

// Emit appends a caller-domain task_event (§9.3).
func (o *Operation) Emit(id TaskID, kind EventKind, payload json.RawMessage) *Operation {
	return o.Add(Effect{Sort: EffectTaskEvent, TaskID: id, EventKind: kind, Payload: payload})
}

// StartEpisode appends an owner-responsibility episode start (§14): the started
// transition's authority can then govern later effects in the same operation.
func (o *Operation) StartEpisode(assignment AssignmentID, task TaskID, occupant ActorID) *Operation {
	return o.Add(Effect{
		Sort: EffectAssignmentStart, AssignmentID: assignment, TaskID: task,
		SlotID: SlotOwnerResponsibility, Occupant: occupant,
	})
}

// EndEpisode appends an owner-responsibility episode end (§14).
func (o *Operation) EndEpisode(assignment AssignmentID, task TaskID) *Operation {
	return o.Add(Effect{
		Sort: EffectAssignmentEnd, AssignmentID: assignment, TaskID: task,
		SlotID: SlotOwnerResponsibility,
	})
}

// Atomic commits the effects the build function accumulates as ONE journaled
// operation under this Session's actor and authority (§9.5), folding them in builder
// order with per-effect authorization (§9.3). It returns the committed result (anchor,
// emitted-event closure, and result-slot bindings). An empty operation is rejected.
func (s *Session) Atomic(build func(op *Operation), opts ...ApplyOption) (CommittedResult, error) {
	if err := s.checkGate("Atomic"); err != nil {
		return CommittedResult{}, err
	}
	if err := s.requireInitialized("Atomic"); err != nil {
		return CommittedResult{}, err
	}
	op := &Operation{}
	build(op)
	if len(op.effects) == 0 {
		return CommittedResult{}, fmt.Errorf(
			"provenance.Session.Atomic: the operation builder added no effects — " +
				"where: Session.Atomic; impact: nothing committed; fix: add at least one effect " +
				"(e.g. op.CreateTask/op.StartEpisode/op.Emit) inside the build function")
	}
	cfg := s.resolve(opts, "atomic", atomicCanonical(op.effects))
	res, err := s.applyOne(cfg, op.effects)
	if err != nil {
		return CommittedResult{}, fmt.Errorf("provenance.Session.Atomic: %w", err)
	}
	return res, nil
}

func atomicCanonical(effects []Effect) string {
	out := fmt.Sprintf("n=%d", len(effects))
	for i := range effects {
		e := effects[i]
		out += fmt.Sprintf("|%d:%s:%s:%s:%s", i, e.Sort, e.TaskID.String(), string(e.EventKind), string(e.AssignmentID))
	}
	return out
}
