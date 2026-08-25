package provenance

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// This test is parallel: newRaceTracker gives it a private in-memory tracker with
// its own genesis authority and actors, its task ids are fresh UUIDv7 values, and
// it touches no package-level state, file, environment variable, or goroutine count.

func TestOrdinaryJournalAuthorityBeyondGovernedDepthAndReplay(t *testing.T) {
	t.Parallel()

	r := newRaceTracker(t)
	const lineageDepth = 66

	var (
		parent        AssignmentID
		rootAuthority JournalID
		rootTask      TaskID
		deepest       TaskID
	)
	for i := 0; i < lineageDepth; i++ {
		task := r.createTask(t, fmt.Sprintf("ordinary-depth-%d", i))
		assignment := AssignmentID(fmt.Sprintf("ordinary-depth-assignment-%d", i))
		result, err := r.tr.Journal().Apply(OperationInput{
			OperationID: OperationID(fmt.Sprintf("ordinary-depth-start-%d", i)), ActorID: r.actorA,
			AuthorityJournalID: &r.boot, CommandDigest: []byte(fmt.Sprintf("ordinary-depth-start-%d", i)),
			Effects: []Effect{{Sort: EffectAssignmentStart, AssignmentID: assignment, TaskID: task,
				SlotID: SlotOwnerResponsibility, Occupant: r.actorB, Parent: parent, ResultSlot: "authority"}},
		})
		if err != nil {
			t.Fatalf("start ordinary lineage episode %d: %v", i, err)
		}
		if i == 0 {
			var ok bool
			rootAuthority, ok = slotJournalID(result, "authority")
			if !ok {
				t.Fatal("root ordinary episode produced no authority slot")
			}
			rootTask = task
		}
		parent, deepest = assignment, task
	}

	input := OperationInput{OperationID: "ordinary-depth-event", ActorID: r.actorA, AuthorityJournalID: &rootAuthority,
		CommandDigest: []byte("ordinary-depth-event"), Effects: []Effect{{Sort: EffectTaskEvent, TaskID: deepest, EventKind: "provenance.review.recorded"}}}
	first, err := r.tr.Journal().Apply(input)
	if err != nil {
		t.Fatalf("ordinary lineage deeper than governed allocation bound was rejected: %v", err)
	}

	_, err = r.tr.Journal().Apply(OperationInput{OperationID: "ordinary-depth-revoke-root", ActorID: r.actorA,
		AuthorityJournalID: &r.boot, CommandDigest: []byte("ordinary-depth-revoke-root"),
		Effects: []Effect{{Sort: EffectAssignmentEnd, AssignmentID: "ordinary-depth-assignment-0", TaskID: rootTask, SlotID: SlotOwnerResponsibility}}})
	if err != nil {
		t.Fatalf("revoke root ordinary authority: %v", err)
	}
	replayed, err := r.tr.Journal().Apply(input)
	if err != nil {
		t.Fatalf("exact ordinary replay revalidated current authority: %v", err)
	}
	if !replayed.ShortCircuited || replayed.AnchorJournalID != first.AnchorJournalID {
		t.Fatalf("ordinary replay did not return the stored result: %#v", replayed)
	}

	unrelated := r.createTask(t, "ordinary-depth-unrelated")
	_, err = r.tr.Journal().Apply(OperationInput{OperationID: "ordinary-authority-diagnostic", ActorID: r.actorA,
		AuthorityJournalID: &rootAuthority, CommandDigest: []byte("ordinary-authority-diagnostic"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: unrelated, EventKind: "provenance.review.recorded"}}})
	if !errors.Is(err, ErrAuthorityScope) {
		t.Fatalf("ordinary authority failure = %v, want ErrAuthorityScope", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "composed") || strings.Contains(strings.ToLower(err.Error()), "supplemental") {
		t.Fatalf("ordinary authority error uses composition-only wording: %v", err)
	}
}
