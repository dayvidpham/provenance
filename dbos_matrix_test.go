package provenance_test

// dbos_matrix_test.go drives the complete required checkpoint/store matrix (issue
// dayvidpham/provenance#6) with exact callback/write/error oracles. Divergence rows
// are produced by dependency-injecting a tracker whose LookupCommitted returns the
// matrix variant while its Apply commits normally, so the adapter's post-validation
// is exercised against real DBOS checkpoints.

import (
	"context"
	"errors"
	"testing"

	"github.com/dayvidpham/provenance"
)

// fakeJournal wraps a real JournalAPI, counting fold ATTEMPTS (every Apply call,
// which the step skips entirely on a §9.4/DBOS replay) and successful COMMITS (the
// "execute once" oracle, retry-tolerant because a transient-lock retry does not
// commit), and optionally transforming LookupCommitted to inject a matrix
// divergence.
type fakeJournal struct {
	provenance.JournalAPI
	attempts *int
	commits  *int
	lookup   func(real provenance.CommittedResult, realErr error) (provenance.CommittedResult, error)
}

func (f *fakeJournal) Apply(in provenance.OperationInput) (provenance.CommittedResult, error) {
	if f.attempts != nil {
		*f.attempts++
	}
	res, err := f.JournalAPI.Apply(in)
	if err == nil && f.commits != nil {
		*f.commits++
	}
	return res, err
}

func (f *fakeJournal) LookupCommitted(op provenance.OperationID) (provenance.CommittedResult, error) {
	real, err := f.JournalAPI.LookupCommitted(op)
	if f.lookup != nil {
		return f.lookup(real, err)
	}
	return real, err
}

// wrappedTracker embeds a real Tracker and swaps in a fixed fakeJournal.
type wrappedTracker struct {
	provenance.Tracker
	journal *fakeJournal
}

func (w *wrappedTracker) Journal() provenance.JournalAPI { return w.journal }

// counters holds the attempt/commit counters a matrix test asserts against.
type counters struct {
	attempts int
	commits  int
}

// stackWithJournal wires a stack whose adapter folds through a fakeJournal that
// records into c (may be nil) and applies the optional lookup transform.
func stackWithJournal(t *testing.T, c *counters, lookup func(provenance.CommittedResult, error) (provenance.CommittedResult, error)) *dbosStack {
	t.Helper()
	return newDBOSStack(t, func(real provenance.Tracker) provenance.Tracker {
		fj := &fakeJournal{JournalAPI: real.Journal(), lookup: lookup}
		if c != nil {
			fj.attempts = &c.attempts
			fj.commits = &c.commits
		}
		return &wrappedTracker{Tracker: real, journal: fj}
	})
}

// Row 1: absent (DBOS) | absent (Provenance) → execute once, commit, checkpoint,
// post-validate, succeed.
func TestMatrix_AbsentAbsent_Succeeds(t *testing.T) {
	var c counters
	s := stackWithJournal(t, &c, nil)
	res, err := s.adapter.Apply(context.Background(), s.createTaskOp("op-r1", "aura", "r1"))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Kind != provenance.CommittedExact || res.ShortCircuited {
		t.Errorf("res = %+v, want fresh CommittedExact", res)
	}
	if c.commits != 1 {
		t.Errorf("committed folds = %d, want exactly 1 (execute once)", c.commits)
	}
}

// Row 2: absent (DBOS) | exact (Provenance) → Provenance replay short-circuit,
// checkpoint, post-validate, succeed. The operation is committed directly first, so
// the adapter's step folds onto an already-committed operation (§9.4).
func TestMatrix_AbsentExact_ReplaySucceeds(t *testing.T) {
	var c counters
	s := stackWithJournal(t, &c, nil)
	op := s.createTaskOp("op-r2", "aura", "r2")

	// Commit directly (bypassing the adapter) so DBOS has no workflow but Provenance
	// already holds the exact operation.
	if _, err := s.tracker.Journal().Apply(op); err != nil {
		t.Fatalf("direct Apply: %v", err)
	}
	before := c.commits
	res, err := s.adapter.Apply(context.Background(), op)
	if err != nil {
		t.Fatalf("adapter.Apply: %v", err)
	}
	if res.Kind != provenance.CommittedExact {
		t.Errorf("res = %+v, want CommittedExact", res)
	}
	// The adapter step folded once and that fold short-circuited on the
	// already-committed operation (§9.4) — the domain is unchanged, so the anchor
	// matches the directly-committed one.
	if c.commits-before != 1 {
		t.Errorf("adapter step committed folds = %d, want 1 (the replay fold)", c.commits-before)
	}
}

// Row 3: absent (DBOS) | conflict (Provenance) → typed conflict; no new domain/
// checkpoint success. Same OperationID committed first with different digests.
func TestMatrix_AbsentConflict_TypedConflict(t *testing.T) {
	s := stackWithJournal(t, nil, nil)
	op := s.createTaskOp("op-r3", "aura", "r3")
	if _, err := s.tracker.Journal().Apply(op); err != nil {
		t.Fatalf("direct Apply: %v", err)
	}
	// Reuse the OperationID with a different command digest → different fingerprint,
	// absent DBOS workflow, Provenance conflict at the fold.
	conflicting := op
	conflicting.CommandDigest = []byte("different-command")
	_, err := s.adapter.Apply(context.Background(), conflicting)
	if !errors.Is(err, provenance.ErrOperationConflict) {
		t.Fatalf("err = %v, want ErrOperationConflict", err)
	}
	var oc *provenance.OperationConflict
	if !errors.As(err, &oc) {
		t.Errorf("conflict not errors.As-discoverable: %v", err)
	}
}

// Row 4: present-success (DBOS) | exact equal digest/result → callback count zero;
// succeed. Re-Apply the identical operation: DBOS returns the completed workflow
// without re-running the step.
func TestMatrix_PresentSuccessExact_ZeroCallback(t *testing.T) {
	var c counters
	s := stackWithJournal(t, &c, nil)
	op := s.createTaskOp("op-r4", "aura", "r4")
	if _, err := s.adapter.Apply(context.Background(), op); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if c.commits != 1 {
		t.Fatalf("first Apply committed folds = %d, want 1", c.commits)
	}
	attemptsBefore := c.attempts
	res, err := s.adapter.Apply(context.Background(), op)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if res.Kind != provenance.CommittedExact {
		t.Errorf("res.Kind = %v, want CommittedExact", res.Kind)
	}
	// Zero callback: DBOS returns the completed workflow without re-running the step,
	// so the fold is not even attempted on the re-Apply.
	if c.attempts != attemptsBefore {
		t.Errorf("fold attempts on re-Apply = %d, want 0 (DBOS skipped the step)", c.attempts-attemptsBefore)
	}
	if c.commits != 1 {
		t.Errorf("committed folds after re-Apply = %d, want 1", c.commits)
	}
}

// Row 5: present-success (DBOS) | absent (Provenance) → typed divergence; no writes.
func TestMatrix_PresentSuccessAbsent_Divergence(t *testing.T) {
	lookup := func(real provenance.CommittedResult, err error) (provenance.CommittedResult, error) {
		return provenance.CommittedResult{Kind: provenance.CommittedAbsent}, nil
	}
	s := stackWithJournal(t, nil, lookup)
	_, err := s.adapter.Apply(context.Background(), s.createTaskOp("op-r5", "aura", "r5"))
	assertDivergence(t, err)
}

// Row 6: present-success (DBOS) | conflict (Provenance) → typed divergence.
func TestMatrix_PresentSuccessConflict_Divergence(t *testing.T) {
	lookup := func(real provenance.CommittedResult, err error) (provenance.CommittedResult, error) {
		return provenance.CommittedResult{Kind: provenance.CommittedConflict}, nil
	}
	s := stackWithJournal(t, nil, lookup)
	_, err := s.adapter.Apply(context.Background(), s.createTaskOp("op-r6", "aura", "r6"))
	assertDivergence(t, err)
}

// Row 7: present-success (DBOS) | exact digest/result mismatch → typed divergence.
func TestMatrix_PresentSuccessMismatch_Divergence(t *testing.T) {
	lookup := func(real provenance.CommittedResult, err error) (provenance.CommittedResult, error) {
		real.AnchorJournalID += 9999 // perturb the journal-anchored result
		return real, err
	}
	s := stackWithJournal(t, nil, lookup)
	_, err := s.adapter.Apply(context.Background(), s.createTaskOp("op-r7", "aura", "r7"))
	assertDivergence(t, err)
}

// Row 8: present-failure-outcome (DBOS) | corresponding failure → decode pinned
// typed failure with errors.As; no writes. Re-Apply proves the failure is permanent
// for the same OperationID.
func TestMatrix_PresentFailureOutcome_TypedFailurePermanent(t *testing.T) {
	var c counters
	s := stackWithJournal(t, &c, nil)
	// A second genesis (nil authority + bootstrap effect) against a non-empty
	// journal is a deterministic §4.6 genesis-discipline domain failure.
	op := provenance.OperationInput{
		OperationID:    "op-r8",
		ActorID:        s.actor,
		CommandDigest:  []byte("cmd-r8"),
		MutationDigest: []byte("mut-r8"),
		Effects:        []provenance.Effect{{Sort: provenance.EffectBootstrapAuthority, BootstrapLabel: "second", ResultSlot: "a"}},
	}
	_, err := s.adapter.Apply(context.Background(), op)
	if !errors.Is(err, provenance.ErrGenesis) {
		t.Fatalf("err = %v, want ErrGenesis decoded from checkpointed failure", err)
	}
	attemptsAfterFirst := c.attempts
	// Re-Apply: DBOS returns the checkpointed failure outcome without re-running the
	// step, so the fold is not attempted again — the failure is permanent.
	_, err2 := s.adapter.Apply(context.Background(), op)
	if !errors.Is(err2, provenance.ErrGenesis) {
		t.Fatalf("re-Apply err = %v, want permanent ErrGenesis", err2)
	}
	if c.attempts != attemptsAfterFirst {
		t.Errorf("fold attempted again on re-Apply (%d→%d): failure not permanent", attemptsAfterFirst, c.attempts)
	}
}

// Row 9: unknown outcome/lookup variant → fail closed actionably; no writes.
func TestMatrix_UnknownLookupVariant_FailClosed(t *testing.T) {
	lookup := func(real provenance.CommittedResult, err error) (provenance.CommittedResult, error) {
		return provenance.CommittedResult{Kind: provenance.CommittedResultKind(99)}, nil
	}
	s := stackWithJournal(t, nil, lookup)
	_, err := s.adapter.Apply(context.Background(), s.createTaskOp("op-r9", "aura", "r9"))
	assertDivergence(t, err)
}

func assertDivergence(t *testing.T, err error) {
	t.Helper()
	var div *provenance.CheckpointDivergenceError
	if !errors.As(err, &div) {
		t.Fatalf("err = %v, want *CheckpointDivergenceError", err)
	}
	if div.Operation == "" || div.Stage == "" || div.Impact == "" || div.Fix == "" {
		t.Errorf("CheckpointDivergenceError missing actionable fields: %+v", div)
	}
}
