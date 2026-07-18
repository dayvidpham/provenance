package journal

import (
	"encoding/json"
	"errors"
	"fmt"
)

// status_fsm.go defines the static finite-state machine over the closed task-status
// enum (docs/journal-relational-contract.md §8.1, §16). The lifecycle verbs
// (Session.Start/Stop/CloseTask/Reopen) each journal one transition lifecycle event
// kind; the shared reducer folds that event and, before materializing the new status,
// consults this FSM to reject an illegal transition. The table is intrinsic to the
// kind: each transition kind has a fixed target status (StatusForEventKind) and a
// closed set of source statuses it is legal FROM, so the (from, kind) pair is the sole
// authority on legality — never a payload read.
//
// The one escape hatch is a FORCED transition (Session verbs invoked WithForce): the
// journal row carries a forced marker in its payload (EncodeForcedTransitionPayload),
// the reducer sees that marker and SKIPS the FSM check for that one row, and the
// coercion is thereby journal-reproducible and audit-visible. Force never bypasses
// authorization (§9.3) — only the FSM — and because the marker lives in the journal
// row, Open's from-empty replay re-derives the identical (coerced) status.

// ErrStatusTransition is the sentinel wrapped by every illegal-transition rejection,
// recoverable with errors.Is. The typed InvalidStatusTransition carries the From/To
// detail and reports it via errors.As.
var ErrStatusTransition = errors.New("provenance: invalid task status transition")

// InvalidStatusTransition is the typed, actionable error the FSM raises for an illegal
// status transition (including a same-state no-op). It names the current status, the
// attempted target, and the transition kind, and points at the --force escape hatch.
type InvalidStatusTransition struct {
	From TaskStatus
	To   TaskStatus
	Kind EventKind
}

func (e InvalidStatusTransition) Error() string {
	return fmt.Sprintf(
		"%v: cannot transition task status from %s to %s via %s — where: shared-reducer status "+
			"FSM (§8.1); when: folding the lifecycle event, before the status projection is "+
			"materialized; impact: nothing is committed; fix: reach %s through a legal path "+
			"(open→in_progress=Start, in_progress→open=Stop, {open,in_progress}→closed=CloseTask, "+
			"closed→open=Reopen), or, if a deliberate out-of-FSM coercion is intended, re-issue the "+
			"verb WithForce so the coercion is journaled with a forced marker",
		ErrStatusTransition, e.From, e.To, e.Kind, e.To)
}

// Is lets errors.Is recover the sentinel for the typed transition error.
func (e InvalidStatusTransition) Is(target error) bool { return target == ErrStatusTransition }

// IsTransitionLifecycleKind reports whether kind is one of the four FSM-governed
// TRANSITION lifecycle kinds (started/stopped/closed/reopened) — the kinds that move an
// EXISTING task between statuses. The baseline-seeding kinds (created, migrated) are NOT
// transitions: created seeds status=open at birth and migrated seeds the captured legacy
// status (§13); neither is validated against the FSM.
func IsTransitionLifecycleKind(kind EventKind) bool {
	switch kind {
	case EventKindTaskStarted, EventKindTaskStopped, EventKindTaskClosed, EventKindTaskReopened:
		return true
	default:
		return false
	}
}

// legalStatusSources is the static FSM transition table, keyed by transition kind:
// each kind's closed set of source statuses it may legally transition FROM. Its target
// status is StatusForEventKind(kind). A same-state transition is illegal because a
// kind's own target status never appears in its source set (e.g. started's target
// in_progress is not among its sources {open}). The exhaustive-switch corpus pins that
// this table equals exactly the four transition kinds and their documented arrows:
//
//	open        --Start (started)--> in_progress
//	in_progress --Stop  (stopped)--> open
//	open        --CloseTask (closed)--> closed
//	in_progress --CloseTask (closed)--> closed
//	closed      --Reopen (reopened)--> open
//
// closed→in_progress is deliberately absent: a closed task is reached back to
// in_progress only by Reopen (→open) THEN Start (→in_progress), never directly.
func legalStatusSources(kind EventKind) ([]TaskStatus, bool) {
	switch kind {
	case EventKindTaskStarted:
		return []TaskStatus{TaskStatusOpen}, true
	case EventKindTaskStopped:
		return []TaskStatus{TaskStatusInProgress}, true
	case EventKindTaskClosed:
		return []TaskStatus{TaskStatusOpen, TaskStatusInProgress}, true
	case EventKindTaskReopened:
		return []TaskStatus{TaskStatusClosed}, true
	default:
		return nil, false
	}
}

// ValidateStatusTransition enforces the static FSM: given a task's current status and a
// TRANSITION lifecycle kind, it returns nil for a legal transition and a typed
// InvalidStatusTransition otherwise (including a same-state no-op, and the
// closed→in_progress case). It is the single source of truth the shared reducer step
// consults for both Apply and Open (§9.2, §8.1); a forced coercion bypasses this check
// at the reducer, never here. Passing a non-transition kind is itself an error — callers
// gate on IsTransitionLifecycleKind first.
func ValidateStatusTransition(from TaskStatus, kind EventKind) error {
	to, _ := StatusForEventKind(kind)
	sources, ok := legalStatusSources(kind)
	if !ok {
		return InvalidStatusTransition{From: from, To: to, Kind: kind}
	}
	for _, s := range sources {
		if from == s {
			return nil
		}
	}
	return InvalidStatusTransition{From: from, To: to, Kind: kind}
}

// TransitionLifecycleKinds returns the closed set of FSM-governed transition kinds in a
// stable order, so the exhaustive-switch corpus can assert the table is exactly these
// four and freshness-guard against a kind added without a source rule.
func TransitionLifecycleKinds() []EventKind {
	return []EventKind{
		EventKindTaskStarted, EventKindTaskStopped, EventKindTaskClosed, EventKindTaskReopened,
	}
}

// ---------------------------------------------------------------------------
// Forced-transition marker (the FSM escape hatch, §8.1/§16)
// ---------------------------------------------------------------------------

// ForcedTransitionPayloadKey is the JSON object key a forced lifecycle transition
// records in its journal_task_events payload so the coercion is captured IN the journal
// (not inferred from surrounding state) and is thus reproducible solely from ordered
// journal history when Open re-derives the status projection from empty. The reducer
// reads it to know whether to skip the FSM for that one row.
const ForcedTransitionPayloadKey = "forced"

// EncodeForcedTransitionPayload builds the journal payload for a forced lifecycle
// transition: {"forced":true}. It is written by hand from a fixed shape so the marker is
// a stable canonical object independent of struct field/tag ordering, mirroring the
// migration-marker discipline.
func EncodeForcedTransitionPayload() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{%q:true}`, ForcedTransitionPayloadKey))
}

// DecodeForcedTransition reports whether a task_event payload carries the forced marker
// (EncodeForcedTransitionPayload). A missing key or empty payload is a non-forced
// transition (forced=false, nil error); a present-but-non-boolean marker is a typed
// error so a corrupted marker fails closed rather than silently coercing.
func DecodeForcedTransition(payload []byte) (bool, error) {
	if len(payload) == 0 {
		return false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return false, fmt.Errorf("provenance: decode forced-transition payload: %w", err)
	}
	raw, ok := fields[ForcedTransitionPayloadKey]
	if !ok {
		return false, nil
	}
	var forced bool
	if err := json.Unmarshal(raw, &forced); err != nil {
		return false, fmt.Errorf(
			"provenance: forced-transition marker %s is not a boolean — where: status FSM reducer "+
				"(§8.1); impact: the transition's FSM-bypass intent cannot be reproduced from journal "+
				"history; fix: the marker payload must record %s as a JSON boolean",
			ForcedTransitionPayloadKey, ForcedTransitionPayloadKey)
	}
	return forced, nil
}
