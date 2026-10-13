package provenance

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
)

func TestIDMigrationAssignmentTransferUsesReleasedSessionOwnerRecipe(t *testing.T) {
	actor, err := ParseActorID("my--project/018f0000-0000-7000-8000-000000000002")
	if err != nil {
		t.Fatal(err)
	}
	task, err := ParseTaskID("my--project/018f0000-0000-7000-8000-000000000003")
	if err != nil {
		t.Fatal(err)
	}
	occupant, err := ParseActorID("my--project/018f0000-0000-7000-8000-000000000004")
	if err != nil {
		t.Fatal(err)
	}
	request := AssignmentTransferRequest{TaskID: task, SlotID: SlotOwnerResponsibility, PreviousAssignmentID: "previous", NextAssignmentID: "next", NextOccupant: occupant}
	effects := assignmentTransferEffects(request)
	prepared, err := journal.Canonicalize(journal.OperationInput{Effects: effects})
	if err != nil {
		t.Fatal(err)
	}
	oldActor := actor.Namespace + "--" + actor.UUID.String()
	// These verb arguments are the published v0.4.0 owner contract; the
	// expected new digest comes from the actual current transfer entry point.
	oldDigest := digestOf("command", oldActor, "assignment-transfer", task.Namespace+"--"+task.UUID.String(), string(request.SlotID), "previous", "next", occupant.Namespace+"--"+occupant.UUID.String())
	got, err := migrateSessionCommandDigest(oldActor, actor.String(), oldDigest, prepared, prepared)
	if err != nil {
		t.Fatal(err)
	}
	want := (&Session{actor: actor}).resolveAssignmentTransfer(request, nil).commandDigest
	if !bytes.Equal(got, want) || bytes.Equal(got, oldDigest) {
		t.Fatal("released transfer's exact retry digest was not translated by its Session owner")
	}
	// Same effect shape written by Atomic must bind to Atomic, not Transfer.
	oldAtomic := "n=2"
	for i, e := range effects {
		oldAtomic += fmt.Sprintf("|%d:%s:%s:%s:%s", i, e.Sort, retiredTaskString(e.TaskID), string(e.EventKind), string(e.AssignmentID))
	}
	got, err = migrateSessionCommandDigest(oldActor, actor.String(), digestOf("command", oldActor, "atomic", oldAtomic), prepared, prepared)
	want = (&Session{actor: actor}).resolve(nil, "atomic", atomicCanonical(effects)).commandDigest
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("Atomic transfer-shaped operation no longer binds to its original owner recipe")
	}
	opaque := []byte("caller-supplied transfer command")
	got, err = migrateSessionCommandDigest(oldActor, actor.String(), opaque, prepared, prepared)
	if err != nil || !bytes.Equal(got, opaque) {
		t.Fatalf("opaque command changed: %x,%v", got, err)
	}
}
