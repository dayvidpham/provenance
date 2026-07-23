package provenance

// journal_authority_revocation_race_test.go is the authority-revocation-races
// negative-path family required by the Impl-UAT C8a ruling. The races are
// process-shaped, so per the C8b split they live in Go: two live operations on one
// Tracker race an authority revocation (ending an owner episode) against work that
// depends on that authority, and the tests assert the no-TOCTOU property — every
// per-effect authorization is decided against COMMITTED state at the effect's own
// JournalID (§9.3, §14.1), so an effect can never commit under an authority that was
// already revoked at a smaller JournalID — and the single-winner CAS on a revocation
// racing a transfer (§9.6). The history-shaped revoke-then-pinned-retry cases live in
// the sibling YAML corpus (authority_revocation.yaml) per the same C8b split.

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/dayvidpham/provenance/internal/testcorpus"
)

// startEpisodeAuthority starts an owner-responsibility episode and returns the started
// transition's authority JournalID, so a racing operation can cite that assignment
// authority.
func (r *raceTracker) startEpisodeAuthority(t *testing.T, opID string, task TaskID, assignment AssignmentID, occupant ActorID) JournalID {
	t.Helper()
	res, err := r.tr.Journal().Apply(OperationInput{
		OperationID: OperationID(opID), ActorID: r.actorA, AuthorityJournalID: &r.boot,
		CommandDigest: []byte(opID + "c"), MutationDigest: []byte(opID + "m"),
		Effects: []Effect{{Sort: EffectAssignmentStart, AssignmentID: assignment, TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occupant, ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("start episode %q: %v", assignment, err)
	}
	jid, ok := slotJournalID(res, "auth")
	if !ok {
		t.Fatalf("start episode %q produced no authority slot", assignment)
	}
	return jid
}

// TestRevokeVsInFlightCitationNoTOCTOU races an authority revocation (ending the owner
// episode whose started transition is the authority) against an in-flight operation
// citing that authority. Across many iterations it asserts the no-TOCTOU invariant: the
// citing operation commits IFF it was ordered strictly before the revocation
// (citeAnchor < endAnchor); otherwise it is rejected with ErrAuthorityScope. An effect
// authorized against pre-revocation state can never be committed after the revocation —
// authorization is decided at the effect's own JournalID against committed state, not
// snapshotted at operation entry.
func TestRevokeVsInFlightCitationNoTOCTOU(t *testing.T) {
	t.Parallel()
	const iterations = 40
	committedWins, rejectedWins := 0, 0
	r := newRaceTracker(t)

	for i := 0; i < iterations; i++ {
		suffix := fmt.Sprintf("-%d", i)
		task := r.createTask(t, "toctou"+suffix)
		occ := r.actorB
		assignment := AssignmentID("AUTH" + suffix)
		auth := r.startEpisodeAuthority(t, "op-auth-start"+suffix, task, assignment, occ)

		citeOp := OperationInput{
			OperationID: OperationID("op-cite-authority" + suffix), ActorID: r.actorA, AuthorityJournalID: &auth,
			CommandDigest: []byte("cite-c" + suffix), MutationDigest: []byte("cite-m" + suffix),
			Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKind("provenance.review.recorded")}},
		}
		revokeOp := OperationInput{
			OperationID: OperationID("op-revoke-authority" + suffix), ActorID: r.actorA, AuthorityJournalID: &r.boot,
			CommandDigest: []byte("revoke-c" + suffix), MutationDigest: []byte("revoke-m" + suffix),
			Effects: []Effect{{Sort: EffectAssignmentEnd, AssignmentID: assignment, TaskID: task, SlotID: SlotOwnerResponsibility}},
		}

		var (
			wg              sync.WaitGroup
			citeRes, revRes CommittedResult
			citeErr, revErr error
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; citeRes, citeErr = r.tr.Journal().Apply(citeOp) }()
		go func() { defer wg.Done(); <-start; revRes, revErr = r.tr.Journal().Apply(revokeOp) }()
		close(start) // release both goroutines into tight contention
		wg.Wait()

		// The revocation always succeeds: it ends a genuinely active episode (the cite
		// op never ends it), regardless of ordering.
		if revErr != nil {
			t.Fatalf("iteration %d: revocation of an active episode failed: %v", i, revErr)
		}

		switch {
		case citeErr == nil:
			// The citing op committed: it MUST have been ordered strictly before the
			// revocation, so the authority was still active at its effect's JournalID.
			if citeRes.AnchorJournalID >= revRes.AnchorJournalID {
				t.Fatalf("iteration %d TOCTOU VIOLATION: citing op committed at anchor %d >= revocation anchor %d — an effect was authorized under an authority revoked at a smaller JournalID",
					i, citeRes.AnchorJournalID, revRes.AnchorJournalID)
			}
			committedWins++
		case errors.Is(citeErr, ErrAuthorityScope):
			// The revocation was ordered first, so the citing op saw the ended episode at
			// its effect JournalID and failed closed. Nothing citing was committed.
			if citeRes.AnchorJournalID != 0 {
				t.Fatalf("iteration %d: rejected citing op still reported an anchor %d", i, citeRes.AnchorJournalID)
			}
			rejectedWins++
		default:
			t.Fatalf("iteration %d: citing op failed with an unexpected error: %v", i, citeErr)
		}
		if i == iterations-1 {
			// Convergence is a whole-database property; asserting it on a representative
			// final iteration keeps the -race loop fast without weakening the invariant
			// checked every iteration. The deterministic sibling test converges each ordering.
			r.assertConverged(t)
		}
	}
	t.Logf("no-TOCTOU race outcomes over %d iterations: cite-committed=%d cite-rejected=%d", iterations, committedWins, rejectedWins)
}

// TestRevokeAndCitationBothOrderingsDeterministic exercises BOTH interleavings of the
// no-TOCTOU race deterministically (independent of the scheduler, which may favor one
// ordering under contention), so each branch of the invariant is proven: a citation
// ordered before the revocation commits, and a citation ordered after it fails closed
// with ErrAuthorityScope. This is the deterministic complement to
// TestRevokeVsInFlightCitationNoTOCTOU's concurrent invariant.
func TestRevokeAndCitationBothOrderingsDeterministic(t *testing.T) {
	t.Parallel()
	cite := func(r *raceTracker, task TaskID, auth JournalID) (CommittedResult, error) {
		return r.tr.Journal().Apply(OperationInput{
			OperationID: "op-cite", ActorID: r.actorA, AuthorityJournalID: &auth,
			CommandDigest: []byte("cite-c"), MutationDigest: []byte("cite-m"),
			Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKind("provenance.review.recorded")}},
		})
	}
	revoke := func(r *raceTracker, task TaskID) (CommittedResult, error) {
		return r.tr.Journal().Apply(OperationInput{
			OperationID: "op-revoke", ActorID: r.actorA, AuthorityJournalID: &r.boot,
			CommandDigest: []byte("revoke-c"), MutationDigest: []byte("revoke-m"),
			Effects: []Effect{{Sort: EffectAssignmentEnd, AssignmentID: "AUTH", TaskID: task, SlotID: SlotOwnerResponsibility}},
		})
	}

	t.Run("cite-before-revoke-commits", func(t *testing.T) {
		r := newRaceTracker(t)
		task := r.createTask(t, "order-a")
		auth := r.startEpisodeAuthority(t, "op-start", task, "AUTH", r.actorB)
		citeRes, err := cite(r, task, auth)
		if err != nil {
			t.Fatalf("citation before revocation was rejected: %v", err)
		}
		revRes, err := revoke(r, task)
		if err != nil {
			t.Fatalf("revocation after citation failed: %v", err)
		}
		if citeRes.AnchorJournalID >= revRes.AnchorJournalID {
			t.Fatalf("expected citation anchor %d before revocation anchor %d", citeRes.AnchorJournalID, revRes.AnchorJournalID)
		}
		r.assertConverged(t)
	})

	t.Run("revoke-before-cite-fails-closed", func(t *testing.T) {
		r := newRaceTracker(t)
		task := r.createTask(t, "order-b")
		auth := r.startEpisodeAuthority(t, "op-start", task, "AUTH", r.actorB)
		if _, err := revoke(r, task); err != nil {
			t.Fatalf("revocation failed: %v", err)
		}
		if _, err := cite(r, task, auth); !errors.Is(err, ErrAuthorityScope) {
			t.Fatalf("citation after revocation = %v, want ErrAuthorityScope", err)
		}
		r.assertConverged(t)
	})
}

// TestRevocationVsTransferCASSingleWinner races a plain revocation (ending owner
// episode A) against a transfer (ending A and starting successor B) — both attempt to
// end A. The §9.6 compare-and-set on the episode's ended transition admits exactly one:
// the loser observes A already ended and is rejected with a typed ErrStaleEpisode,
// writing nothing. The surviving state is single-valued and converges.
func TestRevocationVsTransferCASSingleWinner(t *testing.T) {
	t.Parallel()
	const iterations = 40
	revokeWins, transferWins := 0, 0
	r := newRaceTracker(t)

	for i := 0; i < iterations; i++ {
		suffix := fmt.Sprintf("-%d", i)
		task := r.createTask(t, "cas"+suffix)
		occ := r.actorB
		assignmentA := AssignmentID("CAS-A" + suffix)
		assignmentB := AssignmentID("CAS-B" + suffix)
		r.startEpisodeAuthority(t, "op-cas-start"+suffix, task, assignmentA, occ)

		revokeOp := OperationInput{
			OperationID: OperationID("op-cas-revoke" + suffix), ActorID: r.actorA, AuthorityJournalID: &r.boot,
			CommandDigest: []byte("cas-rev-c" + suffix), MutationDigest: []byte("cas-rev-m" + suffix),
			Effects: []Effect{{Sort: EffectAssignmentEnd, AssignmentID: assignmentA, TaskID: task, SlotID: SlotOwnerResponsibility}},
		}
		transferOp := OperationInput{
			OperationID: OperationID("op-cas-transfer" + suffix), ActorID: r.actorA, AuthorityJournalID: &r.boot,
			CommandDigest: []byte("cas-xfer-c" + suffix), MutationDigest: []byte("cas-xfer-m" + suffix),
			Effects: []Effect{
				{Sort: EffectAssignmentEnd, AssignmentID: assignmentA, TaskID: task, SlotID: SlotOwnerResponsibility},
				{Sort: EffectAssignmentStart, AssignmentID: assignmentB, TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occ, Predecessor: assignmentA},
			},
		}

		var (
			wg              sync.WaitGroup
			revErr, xferErr error
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, revErr = r.tr.Journal().Apply(revokeOp) }()
		go func() { defer wg.Done(); <-start; _, xferErr = r.tr.Journal().Apply(transferOp) }()
		close(start) // release both goroutines into tight contention
		wg.Wait()

		switch {
		case revErr == nil && errors.Is(xferErr, ErrStaleEpisode):
			revokeWins++
		case xferErr == nil && errors.Is(revErr, ErrStaleEpisode):
			transferWins++
		default:
			t.Fatalf("iteration %d: revocation-vs-transfer CAS did not resolve to one winner + one ErrStaleEpisode loser: revErr=%v xferErr=%v", i, revErr, xferErr)
		}
		if i == iterations-1 {
			r.assertConverged(t)
		}
	}
	t.Logf("revocation-vs-transfer CAS outcomes over %d iterations: revoke-won=%d transfer-won=%d", iterations, revokeWins, transferWins)
}

// ---------------------------------------------------------------------------
// authority_revocation.yaml (§9.4, §14.1) — history-shaped revoke-then-retry
// ---------------------------------------------------------------------------

// revocationEnv builds a clean journaled history with one active owner episode whose
// started transition is an assignment authority, and returns the tracker, task, and the
// assignment authority JournalID a citing operation can pin.
func revocationEnv(t *testing.T) (*raceTracker, TaskID, JournalID) {
	t.Helper()
	r := newRaceTracker(t)
	task := r.createTask(t, "revocation")
	auth := r.startEpisodeAuthority(t, "op-rev-start", task, "AUTH", r.actorB)
	return r, task, auth
}

// opRevokeThenPinnedRetryUncommitted drives the must-fail history: a pinned-OperationID
// op citing an assignment authority faults (uncommitted), the authority is revoked, and
// the pinned retry re-executes and is rejected with ErrAuthorityScope — a pinned id
// grants no free pass under a since-revoked authority.
func opRevokeThenPinnedRetryUncommitted(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	r, task, auth := revocationEnv(t)
	st := r.tr.(*sqliteTracker)
	op := opID(input, "op-pinned-retry-1")
	citeOp := OperationInput{
		OperationID: op, ActorID: r.actorA, AuthorityJournalID: &auth,
		CommandDigest: []byte("cite-c"), MutationDigest: []byte("cite-m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKind("provenance.review.recorded")}},
	}
	// First attempt faults after the first effect: nothing commits (§9.5).
	if _, err := st.db.AdversarialApplyWithFault(citeOp, 0); !errors.Is(err, ErrInjectedFault) {
		return fmt.Errorf("faulted first attempt = %v, want ErrInjectedFault", err)
	}
	if lr, err := r.tr.Journal().LookupCommitted(op); err != nil {
		return err
	} else if lr.Kind != CommittedAbsent {
		return fmt.Errorf("faulted first attempt left committed state: %+v", lr)
	}
	if want, _ := asBool(expected, "committedBefore"); want {
		return fmt.Errorf("case marks committedBefore=true but the first attempt faulted")
	}
	// Revoke the authority's episode.
	if _, err := r.tr.Journal().Apply(OperationInput{
		OperationID: "op-rev-end", ActorID: r.actorA, AuthorityJournalID: &r.boot,
		CommandDigest: []byte("end-c"), MutationDigest: []byte("end-m"),
		Effects: []Effect{{Sort: EffectAssignmentEnd, AssignmentID: "AUTH", TaskID: task, SlotID: SlotOwnerResponsibility}},
	}); err != nil {
		return fmt.Errorf("revoke authority: %w", err)
	}
	// The pinned retry re-executes (no committed result to short-circuit to) and is
	// authorized against the now-revoked authority: fail closed.
	_, err := r.tr.Journal().Apply(citeOp)
	if !errors.Is(err, ErrAuthorityScope) {
		return fmt.Errorf("pinned retry after revocation = %v, want ErrAuthorityScope", err)
	}
	if fc, _ := asBool(expected, "failsClosed"); !fc {
		return fmt.Errorf("case must mark failsClosed=true")
	}
	return nil
}

// opRevokeThenExactReplayCommitted drives the must-pass history: a committed op citing
// an assignment authority is exactly replayed after the authority is revoked and still
// short-circuits (§9.4) — replay reproduces a committed fact, it does not re-authorize.
func opRevokeThenExactReplayCommitted(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	r, task, auth := revocationEnv(t)
	op := opID(input, "op-committed-replay-1")
	citeOp := OperationInput{
		OperationID: op, ActorID: r.actorA, AuthorityJournalID: &auth,
		CommandDigest: []byte("cite-c"), MutationDigest: []byte("cite-m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKind("provenance.review.recorded")}},
	}
	if _, err := r.tr.Journal().Apply(citeOp); err != nil {
		return fmt.Errorf("original citing op: %w", err)
	}
	// Revoke the authority's episode.
	if _, err := r.tr.Journal().Apply(OperationInput{
		OperationID: "op-rev-end", ActorID: r.actorA, AuthorityJournalID: &r.boot,
		CommandDigest: []byte("end-c"), MutationDigest: []byte("end-m"),
		Effects: []Effect{{Sort: EffectAssignmentEnd, AssignmentID: "AUTH", TaskID: task, SlotID: SlotOwnerResponsibility}},
	}); err != nil {
		return fmt.Errorf("revoke authority: %w", err)
	}
	// Exact replay still succeeds via the short-circuit, skipping the current-authority
	// check entirely.
	res, err := r.tr.Journal().Apply(citeOp)
	if err != nil {
		return fmt.Errorf("exact replay after revocation was rejected: %w", err)
	}
	if !res.ShortCircuited {
		return fmt.Errorf("exact replay was re-executed rather than short-circuited (§9.4)")
	}
	if want, _ := asBool(expected, "shortCircuits"); !want {
		return fmt.Errorf("case must mark shortCircuits=true")
	}
	return nil
}
