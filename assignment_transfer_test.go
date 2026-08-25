package provenance

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func transferRequest(task TaskID, previous, next AssignmentID, occupant ActorID) AssignmentTransferRequest {
	return AssignmentTransferRequest{
		TaskID:               task,
		SlotID:               SlotOwnerResponsibility,
		PreviousAssignmentID: previous,
		NextAssignmentID:     next,
		NextOccupant:         occupant,
	}
}

func assertEpisodeActive(t *testing.T, tracker Tracker, assignment AssignmentID, want bool) {
	t.Helper()
	store, ok := tracker.(*sqliteTracker)
	if !ok {
		t.Fatalf("tracker type = %T, want *sqliteTracker", tracker)
	}
	got, err := store.db.EpisodeActive(assignment)
	if err != nil {
		t.Fatalf("EpisodeActive(%q): %v", assignment, err)
	}
	if got != want {
		t.Fatalf("EpisodeActive(%q) = %t, want %t", assignment, got, want)
	}
}

func assertEpisodeCount(t *testing.T, tracker Tracker, task TaskID, want int) {
	t.Helper()
	store, ok := tracker.(*sqliteTracker)
	if !ok {
		t.Fatalf("tracker type = %T, want *sqliteTracker", tracker)
	}
	got, err := store.db.CountEpisodesForTask(task)
	if err != nil {
		t.Fatalf("CountEpisodesForTask(%q): %v", task, err)
	}
	if got != want {
		t.Fatalf("CountEpisodesForTask(%q) = %d, want %d", task, got, want)
	}
}

func assertOperationAbsent(t *testing.T, tracker Tracker, operation OperationID) {
	t.Helper()
	result, err := tracker.Journal().LookupCommitted(operation)
	if err != nil {
		t.Fatalf("LookupCommitted(%q): %v", operation, err)
	}
	if result.Kind != CommittedAbsent {
		t.Fatalf("LookupCommitted(%q) = %+v, want CommittedAbsent", operation, result)
	}
}

func newFileAssignmentTransferTracker(t *testing.T, path string) *raceTracker {
	t.Helper()
	tracker, err := OpenSQLite(path, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatalf("OpenSQLite(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = tracker.Close() })
	register := func(name string) ActorID {
		actor, err := tracker.RegisterSoftwareAgent("assignment-transfer", name, "1", "test")
		if err != nil {
			t.Fatalf("RegisterSoftwareAgent(%q): %v", name, err)
		}
		return actor.ID
	}
	actorA := register("actor-a")
	actorB := register("actor-b")
	genesis, err := tracker.Journal().Apply(OperationInput{
		OperationID:    "assignment-transfer-genesis",
		ActorID:        actorA,
		CommandDigest:  []byte("assignment-transfer-genesis-command"),
		MutationDigest: []byte("assignment-transfer-genesis-mutation"),
		Effects: []Effect{{
			Sort: EffectBootstrapAuthority, BootstrapLabel: "assignment-transfer-root", ResultSlot: "auth",
		}},
	})
	if err != nil {
		t.Fatalf("assignment transfer genesis: %v", err)
	}
	boot, ok := slotJournalID(genesis, "auth")
	if !ok {
		t.Fatal("assignment transfer genesis produced no authority result slot")
	}
	return &raceTracker{tr: tracker, boot: boot, actorA: actorA, actorB: actorB}
}

func TestSessionTransferAssignmentSuccessAndExactRetry(t *testing.T) {
	r := newRaceTracker(t)
	task := r.createTask(t, "transfer-success")
	authority := r.startEpisodeAuthority(t, "transfer-success-start", task, "TRANSFER-A", r.actorA)
	request := transferRequest(task, "TRANSFER-A", "TRANSFER-B", r.actorB)
	session := r.tr.As(r.actorA, authority)

	first, err := session.TransferAssignment(request, WithOperationID("transfer-success-operation"))
	if err != nil {
		t.Fatalf("TransferAssignment(first): %v", err)
	}
	want := AssignmentTransferResult{
		TaskID:               task,
		SlotID:               SlotOwnerResponsibility,
		PreviousAssignmentID: "TRANSFER-A",
		NextAssignmentID:     "TRANSFER-B",
		NextOccupant:         r.actorB,
	}
	if first != want {
		t.Fatalf("TransferAssignment(first) = %+v, want %+v", first, want)
	}

	// The predecessor is already ended by the first call. Exact replay must win
	// admission before current liveness is consulted.
	replayed, err := session.TransferAssignment(request, WithOperationID("transfer-success-operation"))
	if err != nil {
		t.Fatalf("TransferAssignment(exact retry after predecessor end): %v", err)
	}
	want.Replayed = true
	if replayed != want {
		t.Fatalf("TransferAssignment(retry) = %+v, want %+v", replayed, want)
	}
	// A different absent operation has no replay receipt to recover. The same
	// ended predecessor must therefore fail as a typed stale episode and write no
	// anchor or successor.
	staleOperation := OperationID("transfer-success-stale-operation")
	if _, err := session.TransferAssignment(
		transferRequest(task, "TRANSFER-A", "TRANSFER-C", r.actorA),
		WithOperationID(staleOperation),
	); !errors.Is(err, ErrStaleEpisode) {
		t.Fatalf("fresh transfer of ended predecessor error = %v, want ErrStaleEpisode", err)
	}
	assertOperationAbsent(t, r.tr, staleOperation)

	assertEpisodeActive(t, r.tr, "TRANSFER-A", false)
	assertEpisodeActive(t, r.tr, "TRANSFER-B", true)
	assertEpisodeActive(t, r.tr, "TRANSFER-C", false)
	assertEpisodeCount(t, r.tr, task, 2)
	gotTask, err := r.tr.Show(task)
	if err != nil {
		t.Fatalf("Show transferred task: %v", err)
	}
	if gotTask.Owner == nil || *gotTask.Owner != r.actorB {
		t.Fatalf("transferred task owner = %v, want %s", gotTask.Owner, r.actorB)
	}
	r.assertConverged(t)
}

func TestSessionTransferAssignmentChangedInputConflictsBeforeLiveness(t *testing.T) {
	r := newRaceTracker(t)
	task := r.createTask(t, "transfer-conflict")
	authority := r.startEpisodeAuthority(t, "transfer-conflict-start", task, "CONFLICT-A", r.actorA)
	session := r.tr.As(r.actorA, authority)
	operation := OperationID("transfer-conflict-operation")

	if _, err := session.TransferAssignment(
		transferRequest(task, "CONFLICT-A", "CONFLICT-B", r.actorB),
		WithOperationID(operation),
	); err != nil {
		t.Fatalf("TransferAssignment(first): %v", err)
	}
	_, err := session.TransferAssignment(
		transferRequest(task, "CONFLICT-A", "CONFLICT-C", r.actorA),
		WithOperationID(operation),
	)
	if !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("changed-input retry error = %v, want ErrOperationConflict", err)
	}
	var conflict *OperationConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("changed-input retry error has no *OperationConflict: %v", err)
	}
	assertEpisodeCount(t, r.tr, task, 2)
	assertEpisodeActive(t, r.tr, "CONFLICT-B", true)
	assertEpisodeActive(t, r.tr, "CONFLICT-C", false)
	r.assertConverged(t)
}

func TestSessionTransferAssignmentMalformedRequestsRejected(t *testing.T) {
	r := newRaceTracker(t)
	task := r.createTask(t, "transfer-malformed")
	authority := r.startEpisodeAuthority(t, "transfer-malformed-start", task, "MALFORMED-A", r.actorA)
	session := r.tr.As(r.actorA, authority)
	valid := transferRequest(task, "MALFORMED-A", "MALFORMED-B", r.actorB)

	tests := []struct {
		name   string
		mutate func(*AssignmentTransferRequest)
	}{
		{name: "empty task", mutate: func(request *AssignmentTransferRequest) { request.TaskID = TaskID{} }},
		{name: "empty slot", mutate: func(request *AssignmentTransferRequest) { request.SlotID = "" }},
		{name: "unsupported slot", mutate: func(request *AssignmentTransferRequest) { request.SlotID = "reviewer" }},
		{name: "empty previous assignment", mutate: func(request *AssignmentTransferRequest) { request.PreviousAssignmentID = "" }},
		{name: "empty next assignment", mutate: func(request *AssignmentTransferRequest) { request.NextAssignmentID = "" }},
		{name: "same assignment", mutate: func(request *AssignmentTransferRequest) { request.NextAssignmentID = request.PreviousAssignmentID }},
		{name: "empty next occupant", mutate: func(request *AssignmentTransferRequest) { request.NextOccupant = ActorID{} }},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid
			test.mutate(&request)
			operation := OperationID(fmt.Sprintf("transfer-malformed-%d", index))
			if _, err := session.TransferAssignment(request, WithOperationID(operation)); !errors.Is(err, ErrCanonicalMutation) {
				t.Fatalf("TransferAssignment malformed error = %v, want ErrCanonicalMutation", err)
			}
			assertOperationAbsent(t, r.tr, operation)
		})
	}
	assertEpisodeActive(t, r.tr, "MALFORMED-A", true)
	assertEpisodeActive(t, r.tr, "MALFORMED-B", false)
	assertEpisodeCount(t, r.tr, task, 1)
	r.assertConverged(t)
}

func TestSessionAtomicCannotCreateAssignmentTransferLease(t *testing.T) {
	r := newRaceTracker(t)
	task := r.createTask(t, "generic-atomic-transfer")
	authority := r.startEpisodeAuthority(t, "generic-atomic-transfer-start", task, "ATOMIC-A", r.actorA)
	operation := OperationID("generic-atomic-self-transfer")

	_, err := r.tr.As(r.actorA, authority).Atomic(func(op *Operation) {
		op.Add(Effect{
			Sort: EffectAssignmentEnd, AssignmentID: "ATOMIC-A", TaskID: task, SlotID: SlotOwnerResponsibility,
		})
		op.Add(Effect{
			Sort: EffectAssignmentStart, AssignmentID: "ATOMIC-B", TaskID: task,
			SlotID: SlotOwnerResponsibility, Occupant: r.actorB, Predecessor: "ATOMIC-A",
		})
	}, WithOperationID(operation))
	if !errors.Is(err, ErrAuthorityScope) {
		t.Fatalf("generic Atomic self-transfer error = %v, want ErrAuthorityScope", err)
	}
	assertOperationAbsent(t, r.tr, operation)
	assertEpisodeActive(t, r.tr, "ATOMIC-A", true)
	assertEpisodeActive(t, r.tr, "ATOMIC-B", false)
	assertEpisodeCount(t, r.tr, task, 1)
	r.assertConverged(t)
}

func TestSessionTransferAssignmentExactReplayAfterLaterTransferAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assignment-transfer-replay.db")
	r := newFileAssignmentTransferTracker(t, path)
	actorC, err := r.tr.RegisterSoftwareAgent("assignment-transfer", "actor-c", "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent(actor-c): %v", err)
	}
	task := r.createTask(t, "transfer-reopen")
	authorityA := r.startEpisodeAuthority(t, "transfer-reopen-start-a", task, "REOPEN-A", r.actorA)
	requestAB := transferRequest(task, "REOPEN-A", "REOPEN-B", r.actorB)
	if _, err := r.tr.As(r.actorA, authorityA).TransferAssignment(requestAB, WithOperationID("transfer-reopen-a-b")); err != nil {
		t.Fatalf("TransferAssignment(A->B): %v", err)
	}

	// actorB had no earlier contribution to this task, so its first attribution is
	// the exact B started-transition authority needed for the next transfer.
	authorityB := JournalID(0)
	attributions, err := r.tr.Journal().TaskAttributions(task)
	if err != nil {
		t.Fatalf("TaskAttributions: %v", err)
	}
	for _, attribution := range attributions {
		if attribution.ActorID == r.actorB {
			authorityB = attribution.FirstJournalID
		}
	}
	if authorityB == 0 {
		t.Fatal("successor B produced no resolvable started-transition attribution")
	}
	if _, err := r.tr.As(r.actorA, authorityB).TransferAssignment(
		transferRequest(task, "REOPEN-B", "REOPEN-C", actorC.ID),
		WithOperationID("transfer-reopen-b-c"),
	); err != nil {
		t.Fatalf("TransferAssignment(B->C): %v", err)
	}
	if err := r.tr.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}

	reopened, err := OpenSQLite(path, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatalf("OpenSQLite after chained transfers: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	replayed, err := reopened.As(r.actorA, authorityA).TransferAssignment(requestAB, WithOperationID("transfer-reopen-a-b"))
	if err != nil {
		t.Fatalf("exact replay of A->B after B->C and reopen: %v", err)
	}
	if !replayed.Replayed {
		t.Fatalf("exact replay after later transfer and reopen = %+v, want Replayed=true", replayed)
	}
	assertEpisodeActive(t, reopened, "REOPEN-A", false)
	assertEpisodeActive(t, reopened, "REOPEN-B", false)
	assertEpisodeActive(t, reopened, "REOPEN-C", true)
	assertEpisodeCount(t, reopened, task, 3)
	gotTask, err := reopened.Show(task)
	if err != nil {
		t.Fatalf("Show after reopen: %v", err)
	}
	if gotTask.Owner == nil || *gotTask.Owner != actorC.ID {
		t.Fatalf("owner after replaying old transfer = %v, want current occupant %s", gotTask.Owner, actorC.ID)
	}
	if err := reopened.Journal().VerifyIntegrity(); err != nil {
		t.Fatalf("VerifyIntegrity after reopen replay: %v", err)
	}
	if _, err := reopened.Journal().ReplayProjections(); err != nil {
		t.Fatalf("ReplayProjections after reopen replay: %v", err)
	}
}

func TestSessionTransferAssignmentConcurrentTransfersSingleWinner(t *testing.T) {
	const iterations = 20
	r := newFileAssignmentTransferTracker(t, filepath.Join(t.TempDir(), "assignment-transfer-race.db"))
	for index := 0; index < iterations; index++ {
		suffix := fmt.Sprintf("-%d", index)
		task := r.createTask(t, "transfer-race"+suffix)
		previous := AssignmentID("TRANSFER-RACE-A" + suffix)
		authority := r.startEpisodeAuthority(t, "transfer-race-start"+suffix, task, previous, r.actorA)
		requests := [2]AssignmentTransferRequest{
			transferRequest(task, previous, AssignmentID("TRANSFER-RACE-B"+suffix), r.actorA),
			transferRequest(task, previous, AssignmentID("TRANSFER-RACE-C"+suffix), r.actorB),
		}
		operations := [2]OperationID{
			OperationID("transfer-race-b" + suffix),
			OperationID("transfer-race-c" + suffix),
		}
		var (
			wait sync.WaitGroup
			errs [2]error
		)
		start := make(chan struct{})
		wait.Add(2)
		for contender := range requests {
			go func(contender int) {
				defer wait.Done()
				<-start
				_, errs[contender] = r.tr.As(r.actorA, authority).TransferAssignment(
					requests[contender], WithOperationID(operations[contender]),
				)
			}(contender)
		}
		close(start)
		wait.Wait()

		winner, loser := -1, -1
		for contender, err := range errs {
			switch {
			case err == nil:
				if winner != -1 {
					t.Fatalf("iteration %d produced two transfer winners: %v", index, errs)
				}
				winner = contender
			case errors.Is(err, ErrStaleEpisode):
				if loser != -1 {
					t.Fatalf("iteration %d produced two stale losers: %v", index, errs)
				}
				loser = contender
			default:
				t.Fatalf("iteration %d contender %d error = %v, want nil or ErrStaleEpisode", index, contender, err)
			}
		}
		if winner == -1 || loser == -1 {
			t.Fatalf("iteration %d winner=%d loser=%d errors=%v, want one of each", index, winner, loser, errs)
		}
		assertOperationAbsent(t, r.tr, operations[loser])
		assertEpisodeActive(t, r.tr, previous, false)
		assertEpisodeActive(t, r.tr, requests[winner].NextAssignmentID, true)
		assertEpisodeActive(t, r.tr, requests[loser].NextAssignmentID, false)
		assertEpisodeCount(t, r.tr, task, 2)
		gotTask, err := r.tr.Show(task)
		if err != nil {
			t.Fatalf("iteration %d Show: %v", index, err)
		}
		if gotTask.Owner == nil || *gotTask.Owner != requests[winner].NextOccupant {
			t.Fatalf("iteration %d owner = %v, want winner occupant %s", index, gotTask.Owner, requests[winner].NextOccupant)
		}
	}
	r.assertConverged(t)
}

func TestSessionTransferAssignmentVsRevocationSingleWinner(t *testing.T) {
	const iterations = 20
	r := newFileAssignmentTransferTracker(t, filepath.Join(t.TempDir(), "assignment-transfer-revocation-race.db"))
	for index := 0; index < iterations; index++ {
		suffix := fmt.Sprintf("-%d", index)
		task := r.createTask(t, "transfer-revoke-race"+suffix)
		previous := AssignmentID("TRANSFER-REVOKE-A" + suffix)
		next := AssignmentID("TRANSFER-REVOKE-B" + suffix)
		authority := r.startEpisodeAuthority(t, "transfer-revoke-start"+suffix, task, previous, r.actorA)
		transferOperation := OperationID("transfer-revoke-transfer" + suffix)
		revokeOperation := OperationID("transfer-revoke-revocation" + suffix)
		revoke := OperationInput{
			OperationID: revokeOperation, ActorID: r.actorA, AuthorityJournalID: &r.boot,
			CommandDigest:  []byte("transfer-revoke-command" + suffix),
			MutationDigest: []byte("transfer-revoke-mutation" + suffix),
			Effects: []Effect{{
				Sort: EffectAssignmentEnd, AssignmentID: previous, TaskID: task, SlotID: SlotOwnerResponsibility,
			}},
		}
		var (
			wait                       sync.WaitGroup
			transferErr, revocationErr error
		)
		start := make(chan struct{})
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			_, transferErr = r.tr.As(r.actorA, authority).TransferAssignment(
				transferRequest(task, previous, next, r.actorB),
				WithOperationID(transferOperation),
			)
		}()
		go func() {
			defer wait.Done()
			<-start
			_, revocationErr = r.tr.Journal().Apply(revoke)
		}()
		close(start)
		wait.Wait()

		switch {
		case transferErr == nil && errors.Is(revocationErr, ErrStaleEpisode):
			assertOperationAbsent(t, r.tr, revokeOperation)
			assertEpisodeActive(t, r.tr, next, true)
			assertEpisodeCount(t, r.tr, task, 2)
			gotTask, err := r.tr.Show(task)
			if err != nil {
				t.Fatalf("iteration %d Show transfer winner: %v", index, err)
			}
			if gotTask.Owner == nil || *gotTask.Owner != r.actorB {
				t.Fatalf("iteration %d transfer winner owner = %v, want %s", index, gotTask.Owner, r.actorB)
			}
		case revocationErr == nil && errors.Is(transferErr, ErrStaleEpisode):
			assertOperationAbsent(t, r.tr, transferOperation)
			assertEpisodeActive(t, r.tr, next, false)
			assertEpisodeCount(t, r.tr, task, 1)
			gotTask, err := r.tr.Show(task)
			if err != nil {
				t.Fatalf("iteration %d Show revocation winner: %v", index, err)
			}
			if gotTask.Owner != nil {
				t.Fatalf("iteration %d revocation winner owner = %v, want nil", index, gotTask.Owner)
			}
		default:
			t.Fatalf("iteration %d transfer/revocation outcome = transfer %v, revocation %v; want one winner and one ErrStaleEpisode loser", index, transferErr, revocationErr)
		}
		assertEpisodeActive(t, r.tr, previous, false)
	}
	r.assertConverged(t)
}
