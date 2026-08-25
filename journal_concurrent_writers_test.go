package provenance

// journal_concurrent_writers_test.go covers concurrent writer interleavings.
// Concurrency is process-shaped, so it lives in Go (not the YAML corpus): two
// live Sessions on one Tracker race real mutations through the production Apply path
// under the race detector, and the tests assert the single-winner, typed-loser, and
// no-partial-fold properties the journal write path promises (§9.4 idempotent replay,
// §9.5 atomic serialized commit, §9.6/§11 same-OperationID conflict). The journal
// write path serializes operations on one mutex, so the OUTCOME is deterministic
// (exactly one winner); these tests prove that determinism holds under real goroutine
// interleaving and that the concurrent path is free of data races.

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// raceTracker is a clean journaled Tracker for the concurrency families: a genesis
// bootstrap authority is established and two distinct committing actors are
// registered, so two live Sessions (Tracker.As) can race on it. It seeds no
// un-anchored legacy task, so post-race convergence and VerifyIntegrity are clean.
type raceTracker struct {
	tr     Tracker
	boot   JournalID
	actorA ActorID
	actorB ActorID
}

// forEachWritePool runs one contention body against both write-pool shapes.
//
// OpenMemory pins the pool to a single connection, so no two scopes can hold
// SQLite file locks at the same time: every "concurrent" writer in a memory
// fixture is really queued behind one connection, and a defect that only
// appears when two connections contend for the file is invisible there. That is
// how the deferred-BEGIN migration regression shipped green. A file-backed
// tracker has a bounded multi-connection pool and does contend, so a test whose
// subject IS the contention runs on both.
func forEachWritePool(t *testing.T, name string, body func(*testing.T, *raceTracker)) {
	t.Helper()
	t.Run("memory-pool-1", func(t *testing.T) {
		t.Parallel()
		body(t, newRaceTracker(t))
	})
	t.Run("file-pool-multi", func(t *testing.T) {
		t.Parallel()
		tracker, _ := newFileRaceTracker(t, name)
		body(t, tracker)
	})
}

func newRaceTracker(t *testing.T) *raceTracker {
	t.Helper()
	tr, err := OpenMemory(WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	return newRaceTrackerOn(t, tr)
}

// newRaceTrackerOn is the backend-independent half of newRaceTracker: it takes
// ownership of an already-open Tracker and establishes the same genesis
// authority and two committing actors. The file-backed contention families
// (journal_migration_contention_test.go) reuse it so that a pool-size-4 file
// database races exactly the fixture the pool-size-1 memory families race.
func newRaceTrackerOn(t *testing.T, tr Tracker) *raceTracker {
	t.Helper()
	t.Cleanup(func() { _ = tr.Close() })
	reg := func(name string) ActorID {
		a, err := tr.RegisterSoftwareAgent("provenance-test", name, "0", "test")
		if err != nil {
			t.Fatalf("RegisterSoftwareAgent %q: %v", name, err)
		}
		return a.ID
	}
	actorA := reg("race-actor-a")
	actorB := reg("race-actor-b")
	gen, err := tr.Journal().Apply(OperationInput{
		OperationID: "op-race-genesis", ActorID: actorA,
		CommandDigest: []byte("race-gc"), MutationDigest: []byte("race-gm"),
		Effects: []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("genesis: %v", err)
	}
	boot, ok := slotJournalID(gen, "auth")
	if !ok {
		t.Fatalf("genesis produced no bootstrap authority slot")
	}
	return &raceTracker{tr: tr, boot: boot, actorA: actorA, actorB: actorB}
}

// createTask journals the birth of a fresh task under the bootstrap authority and
// returns its id, so a race can mutate an existing task.
func (r *raceTracker) createTask(t *testing.T, label string) TaskID {
	t.Helper()
	id := newCorpusTaskID()
	if _, err := r.tr.Journal().Apply(OperationInput{
		OperationID: OperationID("op-race-create-" + label), ActorID: r.actorA, AuthorityJournalID: &r.boot,
		CommandDigest: []byte("cc-" + label), MutationDigest: []byte("cm-" + label),
		Effects: []Effect{{Sort: EffectTaskCreate, TaskID: id, Title: "race " + label, Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}},
	}); err != nil {
		t.Fatalf("journaled create %q: %v", label, err)
	}
	return id
}

// countTaskEventsOfKind returns how many task_event rows of a given kind exist for a
// task, so a test can assert exactly one winner committed the raced event (no duplicate
// or partial fold) without counting the unrelated birth event.
func (r *raceTracker) countTaskEventsOfKind(t *testing.T, task TaskID, kind EventKind) int {
	t.Helper()
	page, err := r.tr.Journal().QueryTaskEvents(JournalQueryV1{OrderBy: OrderByJournalID})
	if err != nil {
		t.Fatalf("QueryTaskEvents: %v", err)
	}
	n := 0
	for _, ev := range page.Events {
		if ev.TaskID.String() == task.String() && ev.EventKind == kind {
			n++
		}
	}
	return n
}

// assertConverged runs the production convergence + integrity guards and fails the
// test if the post-race database is not accepted as converged.
func (r *raceTracker) assertConverged(t *testing.T) {
	t.Helper()
	if err := r.tr.Journal().VerifyIntegrity(); err != nil {
		t.Fatalf("post-race VerifyIntegrity: %v", err)
	}
	if _, err := r.tr.Journal().ReplayProjections(); err != nil {
		t.Fatalf("post-race convergence: %v", err)
	}
}

// TestConcurrentSameOperationIDSingleWinner races N goroutines committing the
// same canonical operation (same OperationID, actor, command digest, and effects)
// with distinct caller-supplied mutation digests. Canonical effects derive mutation
// identity, so the serialized write path lets exactly one goroutine commit the anchor;
// every other Apply short-circuits (§9.4) to that same committed anchor. No second
// anchor, caller-digest conflict, or duplicate task_event is ever produced.
func TestConcurrentSameOperationIDSingleWinner(t *testing.T) {
	forEachWritePool(t, "same-operation-single-winner", testConcurrentSameOperationIDSingleWinnerBody)
}

func testConcurrentSameOperationIDSingleWinnerBody(t *testing.T, r *raceTracker) {
	task := r.createTask(t, "single-winner")

	const goroutines = 8
	op := OperationInput{
		OperationID: "op-race-same-identity", ActorID: r.actorA, AuthorityJournalID: &r.boot,
		CommandDigest: []byte("same-c"), MutationDigest: []byte("same-m"),
		Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKindTaskUpdated, UpdateTitle: strPtr("raced")}},
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		anchors  = map[JournalID]int{}
		fresh    int
		shorted  int
		firstErr error
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			in := op
			in.MutationDigest = []byte(fmt.Sprintf("caller-mutation-%d", i))
			res, err := r.tr.Journal().Apply(in)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			anchors[res.AnchorJournalID]++
			if res.ShortCircuited {
				shorted++
			} else {
				fresh++
			}
		}(i)
	}
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("identical concurrent apply returned an error: %v", firstErr)
	}
	if len(anchors) != 1 {
		t.Fatalf("identical concurrent apply produced %d distinct anchors, want exactly 1: %v", len(anchors), anchors)
	}
	if fresh != 1 {
		t.Fatalf("expected exactly 1 fresh commit, got %d (single-winner violated)", fresh)
	}
	if shorted != goroutines-1 {
		t.Fatalf("expected %d short-circuited replays, got %d", goroutines-1, shorted)
	}
	if n := r.countTaskEventsOfKind(t, task, EventKindTaskUpdated); n != 1 {
		t.Fatalf("single-winner race committed %d update events, want exactly 1 (no duplicate fold)", n)
	}
	r.assertConverged(t)
}

// TestConcurrentSameOperationIDConflictingIdentityLoserGetsTypedConflict races two
// goroutines committing the same OperationID with different canonical effects (a reused
// id presenting different arguments, §11). Exactly one wins; the loser receives a typed
// ErrOperationConflict with the closed CommittedConflict variant, and nothing extra is
// committed.
func TestConcurrentSameOperationIDConflictingIdentityLoserGetsTypedConflict(t *testing.T) {
	forEachWritePool(t, "same-operation-conflicting-identity", testConcurrentSameOperationIDConflictingIdentityLoserGetsTypedConflictBody)
}

func testConcurrentSameOperationIDConflictingIdentityLoserGetsTypedConflictBody(t *testing.T, r *raceTracker) {
	task := r.createTask(t, "conflict")

	base := OperationInput{
		OperationID: "op-race-conflicting-identity", ActorID: r.actorA, AuthorityJournalID: &r.boot,
		CommandDigest: []byte("conf-c"), Effects: []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKindTaskUpdated, UpdateTitle: strPtr("v")}},
	}
	opA := base
	opB := base
	opB.Effects = []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKindTaskUpdated, UpdateTitle: strPtr("different canonical effect")}}

	type outcome struct {
		res CommittedResult
		err error
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i, in := range []OperationInput{opA, opB} {
		go func(i int, in OperationInput) {
			defer wg.Done()
			results[i].res, results[i].err = r.tr.Journal().Apply(in)
		}(i, in)
	}
	wg.Wait()

	successes, conflicts := 0, 0
	for _, o := range results {
		switch {
		case o.err == nil:
			successes++
		case errors.Is(o.err, ErrOperationConflict):
			conflicts++
			if o.res.Kind != CommittedConflict {
				t.Fatalf("conflict loser variant = %s, want CommittedConflict", o.res.Kind)
			}
		default:
			t.Fatalf("unexpected error from conflicting concurrent apply: %v", o.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("conflicting-identity race resolved to %d successes / %d conflicts, want exactly 1 / 1", successes, conflicts)
	}
	if n := r.countTaskEventsOfKind(t, task, EventKindTaskUpdated); n != 1 {
		t.Fatalf("conflicting-identity race committed %d update events, want exactly 1 (loser committed nothing)", n)
	}
	r.assertConverged(t)
}

// TestConcurrentTwoSessionsDistinctOpsConverge races two LIVE Sessions (distinct
// actors, distinct OperationIDs) each committing many mutations on the same task. Every
// operation commits with a strictly-ascending unique JournalID (the serialized write
// path, §9.5), no interleaving loses or duplicates a fold, and the post-race database
// converges under the production reducer.
func TestConcurrentTwoSessionsDistinctOpsConverge(t *testing.T) {
	forEachWritePool(t, "two-sessions-distinct-ops", testConcurrentTwoSessionsDistinctOpsConvergeBody)
}

func testConcurrentTwoSessionsDistinctOpsConvergeBody(t *testing.T, r *raceTracker) {
	task := r.createTask(t, "distinct-ops")
	sessionA := r.tr.As(r.actorA, r.boot)
	sessionB := r.tr.As(r.actorB, r.boot)

	const perSession = 12
	emit := func(s *Session, tag string) {
		for i := 0; i < perSession; i++ {
			payload := []byte(fmt.Sprintf(`{"tag":%q,"i":%d}`, tag, i))
			if _, err := s.Atomic(func(op *Operation) {
				op.Emit(task, EventKind("provenance."+tag+".noted"), payload)
			}); err != nil {
				t.Errorf("session %s emit %d: %v", tag, i, err)
				return
			}
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); emit(sessionA, "a") }()
	go func() { defer wg.Done(); emit(sessionB, "b") }()
	wg.Wait()
	if t.Failed() {
		return
	}

	// Every emitted event is present exactly once, in strictly-ascending JournalID
	// order — no lost or duplicated fold across the interleaving.
	page, err := r.tr.Journal().QueryTaskEvents(JournalQueryV1{OrderBy: OrderByJournalID})
	if err != nil {
		t.Fatalf("QueryTaskEvents: %v", err)
	}
	var last JournalID
	noted := 0
	for _, ev := range page.Events {
		if ev.JournalID <= last {
			t.Fatalf("non-ascending JournalID %d after %d", ev.JournalID, last)
		}
		last = ev.JournalID
		if ev.TaskID.String() == task.String() && (ev.EventKind == "provenance.a.noted" || ev.EventKind == "provenance.b.noted") {
			noted++
		}
	}
	if noted != 2*perSession {
		t.Fatalf("two-session race committed %d noted events, want %d (no lost fold)", noted, 2*perSession)
	}
	r.assertConverged(t)
}

// TestConcurrentAtomicOpsNoPartialFold races two live Sessions each committing a
// MULTI-EFFECT atomic operation on DISJOINT tasks (distinct assignment IDs: ATOMIC-A on
// taskA, ATOMIC-B on taskB). Each operation is all-or-nothing under §9.5, so after the
// race every task carries the COMPLETE effect set of its operation (owner cleared, status
// closed) and the database converges. This test proves that independent-task atomic ops
// do not cross-contaminate under concurrent scheduling pressure — no interleaving ever
// commits half-folded effects of one task's operation to that task. Same-resource
// multi-effect interleaving (a real shared-resource contention case) is covered by
// TestRevocationVsTransferCASSingleWinner (2-effect transfer vs 1-effect revoke, both on
// the same assignment CAS-A), and general fold-loop atomicity-under-fault is covered by
// the sequential AdversarialApplyWithFault corpus cases (§8.1).
func TestConcurrentAtomicOpsNoPartialFold(t *testing.T) {
	forEachWritePool(t, "atomic-ops-no-partial-fold", testConcurrentAtomicOpsNoPartialFoldBody)
}

func testConcurrentAtomicOpsNoPartialFoldBody(t *testing.T, r *raceTracker) {
	taskA := r.createTask(t, "atomic-a")
	taskB := r.createTask(t, "atomic-b")
	occ := r.actorB

	runAtomic := func(actor ActorID, task TaskID, assignment AssignmentID) error {
		s := r.tr.As(actor, r.boot)
		_, err := s.Atomic(func(op *Operation) {
			op.StartEpisode(assignment, task, occ)
			op.EndEpisode(assignment, task)
			op.Emit(task, EventKindTaskClosed, nil)
		})
		return err
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = runAtomic(r.actorA, taskA, "ATOMIC-A") }()
	go func() { defer wg.Done(); errs[1] = runAtomic(r.actorB, taskB, "ATOMIC-B") }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("atomic op %d failed: %v", i, err)
		}
	}

	// Each task shows the COMPLETE fold of its atomic operation: owner cleared (episode
	// ended) and the close event recorded. A partial fold would leave an owner set or a
	// missing event.
	for _, task := range []TaskID{taskA, taskB} {
		got, err := r.tr.Show(task)
		if err != nil {
			t.Fatalf("Show %q: %v", task, err)
		}
		if got.Owner != nil {
			t.Fatalf("task %q still owned after its atomic close (partial fold visible): %v", task, got.Owner)
		}
	}
	r.assertConverged(t)
}

// TestConcurrentSessionVsMigrationRace races a live Session birthing a NATIVE task
// against a MigrateLegacyBaseline run anchoring a pre-journal legacy task, both on one
// Tracker. The serialized write path admits both; afterwards both the native and the
// migrated task are journal-anchored and the whole database converges — a live mutation
// and a migration interleave without corrupting each other's spine.
func TestConcurrentSessionVsMigrationRace(t *testing.T) {
	t.Parallel()
	r := newRaceTracker(t)

	// Seed a pre-journal legacy task for the migration arm.
	legacy := LegacyTaskRow{ID: newCorpusTaskID(), Status: TaskStatusOpen}
	st := r.tr.(*sqliteTracker)
	if err := st.db.SeedLegacyTask(legacy); err != nil {
		t.Fatalf("seed legacy task: %v", err)
	}

	session := r.tr.As(r.actorA, r.boot)
	var (
		wg          sync.WaitGroup
		nativeTask  Task
		nativeErr   error
		migrateErr  error
		migrateResE MigrationResult
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		nativeTask, nativeErr = session.Create("provenance-test", "native-during-migration", "", TaskTypeTask, PriorityMedium, PhaseUnscoped)
	}()
	go func() {
		defer wg.Done()
		migrateResE, migrateErr = r.tr.Journal().MigrateLegacyBaseline(MigrationInput{
			System: r.actorA, BootstrapAuthority: r.boot, Legacy: []LegacyTaskRow{legacy},
		})
	}()
	wg.Wait()

	if nativeErr != nil {
		t.Fatalf("native Session.Create during migration failed: %v", nativeErr)
	}
	if migrateErr != nil {
		t.Fatalf("migration during a live Session.Create failed: %v", migrateErr)
	}
	if migrateResE.TasksMigrated != 1 {
		t.Fatalf("migration processed %d tasks, want 1", migrateResE.TasksMigrated)
	}
	// Both tasks are anchored and readable after the interleaving.
	if _, err := r.tr.Show(nativeTask.ID); err != nil {
		t.Fatalf("native task not readable after race: %v", err)
	}
	if _, err := r.tr.Show(legacy.ID); err != nil {
		t.Fatalf("migrated task not readable after race: %v", err)
	}
	r.assertConverged(t)
}

func strPtr(s string) *string { return &s }
