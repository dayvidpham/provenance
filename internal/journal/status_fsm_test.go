package journal

import (
	"errors"
	"testing"
)

// TestValidateStatusTransitionExhaustive pins the static FSM table by enumerating the
// FULL Cartesian product of every task status against every transition lifecycle kind
// and asserting each (from, kind) pair against the one expected-legal set. A kind added
// without a source rule, or an arrow silently added/removed, breaks this test.
func TestValidateStatusTransitionExhaustive(t *testing.T) {
	allStatuses := []TaskStatus{TaskStatusOpen, TaskStatusInProgress, TaskStatusClosed}
	// The authoritative arrows, stated once here as the fixture the reducer is pinned to.
	legal := map[EventKind]map[TaskStatus]bool{
		EventKindTaskStarted:  {TaskStatusOpen: true},                             // open → in_progress
		EventKindTaskStopped:  {TaskStatusInProgress: true},                       // in_progress → open
		EventKindTaskClosed:   {TaskStatusOpen: true, TaskStatusInProgress: true}, // {open,in_progress} → closed
		EventKindTaskReopened: {TaskStatusClosed: true},                           // closed → open
	}

	kinds := TransitionLifecycleKinds()
	if len(kinds) != len(legal) {
		t.Fatalf("TransitionLifecycleKinds has %d kinds but the fixture pins %d — the FSM table drifted", len(kinds), len(legal))
	}
	for _, kind := range kinds {
		if !IsTransitionLifecycleKind(kind) {
			t.Errorf("%s is in TransitionLifecycleKinds but IsTransitionLifecycleKind reports false", kind)
		}
		want, ok := legal[kind]
		if !ok {
			t.Fatalf("transition kind %s has no fixture entry — add its arrow to the pinned table", kind)
		}
		for _, from := range allStatuses {
			err := ValidateStatusTransition(from, kind)
			if want[from] {
				if err != nil {
					t.Errorf("ValidateStatusTransition(%s, %s) = %v, want legal (nil)", from, kind, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("ValidateStatusTransition(%s, %s) = nil, want ErrStatusTransition (illegal)", from, kind)
				continue
			}
			if !errors.Is(err, ErrStatusTransition) {
				t.Errorf("ValidateStatusTransition(%s, %s) err = %v, want ErrStatusTransition", from, kind, err)
			}
			var typed InvalidStatusTransition
			if !errors.As(err, &typed) {
				t.Errorf("ValidateStatusTransition(%s, %s) is not an InvalidStatusTransition: %v", from, kind, err)
			} else if typed.From != from || typed.Kind != kind {
				t.Errorf("typed error = %+v, want From=%s Kind=%s", typed, from, kind)
			}
		}
	}
}

// TestValidateStatusTransitionRejectsSameStateAndClosedToInProgress spells out the two
// rulings the exhaustive product also covers, as named regressions.
func TestValidateStatusTransitionRejectsSameStateAndClosedToInProgress(t *testing.T) {
	// Same-state repeats are illegal: a kind's target never appears in its own sources.
	sameState := []struct {
		from TaskStatus
		kind EventKind
	}{
		{TaskStatusInProgress, EventKindTaskStarted}, // already in_progress
		{TaskStatusOpen, EventKindTaskStopped},       // already open
		{TaskStatusClosed, EventKindTaskClosed},      // already closed
		{TaskStatusOpen, EventKindTaskReopened},      // already open
	}
	for _, c := range sameState {
		if err := ValidateStatusTransition(c.from, c.kind); !errors.Is(err, ErrStatusTransition) {
			t.Errorf("same-state ValidateStatusTransition(%s, %s) = %v, want ErrStatusTransition", c.from, c.kind, err)
		}
	}
	// The direct closed → in_progress jump is illegal (Reopen then Start is the path).
	if err := ValidateStatusTransition(TaskStatusClosed, EventKindTaskStarted); !errors.Is(err, ErrStatusTransition) {
		t.Errorf("closed→in_progress = %v, want ErrStatusTransition", err)
	}
}

// TestNonTransitionKindsAreNotFSMGoverned confirms the baseline-seeding kinds are not
// transitions (created/migrated) and other kinds report non-transition.
func TestNonTransitionKindsAreNotFSMGoverned(t *testing.T) {
	for _, kind := range []EventKind{EventKindTaskCreated, EventKindTaskMigrated, EventKindTaskUpdated, "pasture.review.recorded"} {
		if IsTransitionLifecycleKind(kind) {
			t.Errorf("%s reported as a transition lifecycle kind, want false", kind)
		}
	}
}

// TestStatusForEventKindStoppedMapsToOpen pins the new stopped kind's projection.
func TestStatusForEventKindStoppedMapsToOpen(t *testing.T) {
	got, ok := StatusForEventKind(EventKindTaskStopped)
	if !ok || got != TaskStatusOpen {
		t.Errorf("StatusForEventKind(stopped) = (%v, %v), want (open, true)", got, ok)
	}
}

// TestForcedTransitionPayloadRoundTrip pins the forced-marker encode/decode: an encoded
// marker decodes to forced=true, an empty/absent payload to false, and a malformed
// marker fails closed.
func TestForcedTransitionPayloadRoundTrip(t *testing.T) {
	forced, err := DecodeForcedTransition(EncodeForcedTransitionPayload())
	if err != nil || !forced {
		t.Errorf("decode(encode()) = (%v, %v), want (true, nil)", forced, err)
	}
	for _, empty := range [][]byte{nil, []byte(`{}`), []byte(`{"other":1}`)} {
		forced, err := DecodeForcedTransition(empty)
		if err != nil || forced {
			t.Errorf("decode(%s) = (%v, %v), want (false, nil)", string(empty), forced, err)
		}
	}
	if _, err := DecodeForcedTransition([]byte(`{"forced":"yes"}`)); err == nil {
		t.Errorf("decode of a non-boolean forced marker = nil error, want a fail-closed error")
	}
}
