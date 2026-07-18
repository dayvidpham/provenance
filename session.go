package provenance

// session.go is the mutation SDK over the global journal
// (docs/journal-relational-contract.md). A Session binds a committing actor and a
// governing authority once (Tracker.As) and exposes the eight task-tracker mutation
// verbs on that receiver.
//
// The three task-lifecycle verbs — Create, Update, CloseTask — are JOURNALED: each
// builds one logical operation (§9) and commits it through Apply, so every birth,
// metadata change, status transition, and closure flows through the ordered journal
// and is reproducible from journal history (§8.1, §15). Update decomposes a partial
// UpdateFields into typed effects within ONE operation (metadata → a
// provenance.task.updated event; a status change → the matching lifecycle event:
// in_progress → started, closed → closed, open → reopened), folded metadata-then-status
// (§8.1). A same-status set is a journal-honest no-op (the effect is omitted).
//
// The five relationship/annotation verbs — AddEdge, RemoveEdge, AddLabel, RemoveLabel,
// AddComment — are UN-JOURNALED direct domain writes. The ratified contract §6
// deliberately scopes the journal to the seven task-lifecycle mutation families
// (+authority/decision/evidence) and classifies typed dependency edges as relationship
// targets; labels and comments carry no journal-provenance model. These verbs therefore
// write the domain tables directly and record nothing in the journal. Their signatures
// are forward-compatible with a future (user-approved, separately reviewed) decision to
// journal them as first-class effect sorts (contract §6 amendment): adding a variadic
// option parameter or an internal journaling step is a non-breaking upgrade.
//
// Retry caveat (LOUD): every journaled verb defaults to a fresh UUIDv7 OperationID, so
// a naive retry after an ambiguous failure commits a SECOND operation. To make a
// journaled mutation safe to retry, pin a stable OperationID with WithOperationID and
// reuse it verbatim on the retry: an exact same-OperationID replay short-circuits to
// the original committed result without re-executing (§9.4), and a reused OperationID
// presenting different arguments is a typed conflict (§11). NOTE that Create mints a
// fresh TaskID on every call, so even a pinned-OperationID Create retry is idempotent
// ONLY through the §9.4 short-circuit (which returns the ORIGINAL task, never a second
// row) — the freshly minted id of the retry is discarded in that case.

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
}

// As implements Tracker.As.
func (t *sqliteTracker) As(actor ActorID, authority JournalID) *Session {
	return &Session{tr: t, actor: actor, authority: authority}
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
}

// WithOperationID pins the operation's OperationID instead of minting a fresh UUIDv7.
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

// WithMutationDigest overrides the operation's mutation digest (§3.1). See
// WithCommandDigest.
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
	return s.tr.db.Apply(OperationInput{
		OperationID:        cfg.opID,
		ActorID:            s.actor,
		AuthorityJournalID: &auth,
		CommandDigest:      cfg.commandDigest,
		MutationDigest:     cfg.mutationDigest,
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects:            effects,
	})
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
	if namespace == "" {
		return Task{}, fmt.Errorf(
			"%w: Session.Create — namespace is empty — "+
				"provide a non-empty namespace string such as 'aura-plugins' or 'my-project'",
			ErrInvalidID)
	}
	if err := s.requireInitialized("Create"); err != nil {
		return Task{}, err
	}
	id := TaskID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}
	// The digest canonical deliberately excludes the freshly minted id so a pinned
	// same-argument retry matches through the §9.4 short-circuit (which returns the
	// original task, discarding this call's minted id).
	cfg := s.resolve(opts, "create", namespace, title, description,
		fmt.Sprintf("%d/%d/%d", int(taskType), int(priority), int(phase)))
	res, err := s.applyOne(cfg, []Effect{{
		Sort:        EffectTaskCreate,
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

// Update applies a partial UpdateFields to an existing task as ONE journaled operation
// (§8.1). It decomposes into typed effects: any of Title/Description/Priority/Phase/Notes
// emit a single provenance.task.updated event (materializing those columns); a Status
// change emits the matching lifecycle event (in_progress → started, closed → closed,
// open → reopened), folded after the metadata event. A same-status set omits the status
// effect; when the whole call is a no-op (no field changes anything) nothing is
// committed and the current task is returned. Owner is NOT settable through Update —
// owner is reducer-exclusive, moved only through assignment episodes — so a non-nil
// Owner is rejected with an actionable error. Returns ErrNotFound if the task does not
// exist, ErrGenesisRequired if no genesis authority exists yet.
func (s *Session) Update(id TaskID, fields UpdateFields, opts ...ApplyOption) (Task, error) {
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
	if fields.Status != nil && *fields.Status != current.Status {
		kind, err := lifecycleKindForStatus(*fields.Status)
		if err != nil {
			return Task{}, fmt.Errorf("provenance.Session.Update: task %q: %w", id.String(), err)
		}
		effects = append(effects, Effect{Sort: EffectTaskEvent, TaskID: id, EventKind: kind})
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

// CloseTask closes an existing task as ONE journaled operation: it commits a
// provenance.task.closed lifecycle event (projecting status → closed and closed_at)
// that also materializes the close reason. Returns ErrNotFound if the task does not
// exist, ErrAlreadyClosed if it is already closed, ErrGenesisRequired if no genesis
// authority exists yet.
func (s *Session) CloseTask(id TaskID, reason string, opts ...ApplyOption) (Task, error) {
	if err := s.requireInitialized("CloseTask"); err != nil {
		return Task{}, err
	}
	current, found, err := s.tr.db.GetTask(id)
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Session.CloseTask: %w", err)
	}
	if !found {
		return Task{}, fmt.Errorf(
			"%w: Session.CloseTask — task %q does not exist — "+
				"verify the TaskID was obtained from Create or a previous List/Show call",
			ErrNotFound, id.String())
	}
	if current.Status == StatusClosed {
		return Task{}, fmt.Errorf(
			"%w: Session.CloseTask — task %q is already closed (reason: %q) — "+
				"use Update with Status=StatusOpen to reopen the task before closing again",
			ErrAlreadyClosed, id.String(), current.CloseReason)
	}
	cfg := s.resolve(opts, "close", id.String(), reason)
	if _, err := s.applyOne(cfg, []Effect{{
		Sort:        EffectTaskEvent,
		TaskID:      id,
		EventKind:   EventKindTaskClosed,
		CloseReason: reason,
	}}); err != nil {
		return Task{}, fmt.Errorf("provenance.Session.CloseTask: %w", err)
	}
	task, _, err := s.tr.db.GetTask(id)
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Session.CloseTask: read back task %q: %w", id.String(), err)
	}
	return task, nil
}

// lifecycleKindForStatus maps a target status to its journaled lifecycle event kind
// (§8.1). The caller has already excluded a same-status no-op, so open means a reopen.
func lifecycleKindForStatus(target Status) (EventKind, error) {
	switch target {
	case StatusInProgress:
		return EventKindTaskStarted, nil
	case StatusClosed:
		return EventKindTaskClosed, nil
	case StatusOpen:
		return EventKindTaskReopened, nil
	default:
		return "", fmt.Errorf(
			"unsupported target status %d — where: Session.Update status decomposition (§8.1); "+
				"impact: nothing committed; fix: set one of StatusOpen/StatusInProgress/StatusClosed",
			int(target))
	}
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
	if f.Status != nil {
		out += fmt.Sprintf("|status=%d", int(*f.Status))
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
// Un-journaled relationship / annotation verbs (contract §6)
// ---------------------------------------------------------------------------
//
// These five verbs are direct domain writes and record NOTHING in the journal.
// Typed dependency edges are §6 relationship targets (explicitly rejected as an
// authorization-reach mechanism in §14.5), and labels and comments have no
// journal-provenance model. Their behavior mirrors the pre-Session tracker verbs
// exactly; only the provenance status differs. See the package doc for the
// forward-compatibility note on a future journaling upgrade.

// AddEdge creates a typed edge from sourceID to targetID (un-journaled, §6). For
// EdgeBlockedBy, cycle detection is enforced (ErrCycleDetected).
func (s *Session) AddEdge(sourceID TaskID, targetID string, kind EdgeKind) error {
	return s.tr.AddEdge(sourceID, targetID, kind)
}

// RemoveEdge deletes the edge from sourceID to targetID (un-journaled, §6). Idempotent.
func (s *Session) RemoveEdge(sourceID TaskID, targetID string, kind EdgeKind) error {
	return s.tr.RemoveEdge(sourceID, targetID, kind)
}

// AddLabel attaches a label to a task (un-journaled, §6). Idempotent.
func (s *Session) AddLabel(id TaskID, label string) error {
	return s.tr.AddLabel(id, label)
}

// RemoveLabel detaches a label from a task (un-journaled, §6). Idempotent.
func (s *Session) RemoveLabel(id TaskID, label string) error {
	return s.tr.RemoveLabel(id, label)
}

// AddComment adds a comment to a task authored by authorID (un-journaled, §6).
func (s *Session) AddComment(id TaskID, authorID AgentID, body string) (Comment, error) {
	return s.tr.AddComment(id, authorID, body)
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
