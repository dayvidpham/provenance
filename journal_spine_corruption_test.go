package provenance

// journal_spine_corruption_test.go executes the corrupted-journal-spine negative-path
// family (testdata/contract/journal_spine_corruption.yaml) against the real production
// fail-closed guards — VerifyIntegrity's whole-journal subtype-integrity scan (§10
// rule 8) and ReplayProjections' from-empty convergence (§15). Each operator commits a
// valid journaled history through Apply, damages the persisted spine via a disclosed
// raw-SQL corruption seam (internal/sqlite/spine_corruption_adversarial.go), and asserts
// the production guard fails closed with the case's typed expected error and repairs
// nothing. These operators are the executable half of the family's s1.3 scope partition
// recorded in testdata/contract/scope.yaml.
//
// This family is the corrupted-journal (not merely corrupted-projection) recovery
// requirement of the Impl-UAT C8a ruling: the spine's own supertype/subtype rows are
// damaged, so the guards must reject the whole database rather than silently proceeding
// on a partial or renumbered history.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/internal/testcorpus"
)

// spineEnv is a CLEAN journaled environment for the spine-corruption family: unlike
// newOpsEnv it seeds no un-anchored legacy base task, so a converged history passes
// VerifyIntegrity's watermark-presence gate and the must-pass control is not masked by
// a coexisting legacy row. Every task it holds is born through the journal fold.
type spineEnv struct {
	tr    *sqliteTracker
	actor ActorID
	boot  JournalID
	task  TaskID
}

// newSpineHistory opens a fresh in-memory tracker and commits a well-formed journaled
// history: a genesis bootstrap authority, a task born through the fold, an owner
// episode started then ended, and a close event. The result is a converged spine the
// family's corruption seams then damage.
func newSpineHistory(t *testing.T) *spineEnv {
	t.Helper()
	tr, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	st, ok := tr.(*sqliteTracker)
	if !ok {
		t.Fatalf("expected *sqliteTracker, got %T", tr)
	}
	agent, err := tr.RegisterSoftwareAgent("provenance-test", "spine-corpus", "0", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	actor := agent.ID
	occAgent, err := tr.RegisterSoftwareAgent("provenance-test", "spine-occupant", "0", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent occupant: %v", err)
	}

	digest := func(s string) []byte { return []byte("spine-digest--" + s) }

	// Genesis bootstrap authority (§4.6).
	gen, err := tr.Journal().Apply(OperationInput{
		OperationID: "op-spine-genesis", ActorID: actor,
		CommandDigest: digest("gc"), MutationDigest: digest("gm"),
		Effects: []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("genesis: %v", err)
	}
	boot, ok := slotJournalID(gen, "auth")
	if !ok {
		t.Fatalf("genesis produced no bootstrap authority slot")
	}

	// Task born through the fold (§8.1).
	task := newCorpusTaskID()
	if _, err := tr.Journal().Apply(OperationInput{
		OperationID: "op-spine-create", ActorID: actor, AuthorityJournalID: &boot,
		CommandDigest: digest("cc"), MutationDigest: digest("cm"),
		Effects: []Effect{{Sort: EffectTaskCreate, TaskID: task, Title: "spine task", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}},
	}); err != nil {
		t.Fatalf("journaled create: %v", err)
	}

	// Start an owner episode (§14).
	if _, err := tr.Journal().Apply(OperationInput{
		OperationID: "op-spine-start", ActorID: actor, AuthorityJournalID: &boot,
		CommandDigest: digest("sc"), MutationDigest: digest("sm"),
		Effects: []Effect{{Sort: EffectAssignmentStart, AssignmentID: "SPINE-A", TaskID: task, SlotID: SlotOwnerResponsibility, Occupant: occAgent.ID}},
	}); err != nil {
		t.Fatalf("start episode: %v", err)
	}

	// Close: end the episode and record the close event in one operation (the tail).
	if _, err := tr.Journal().Apply(OperationInput{
		OperationID: "op-spine-close", ActorID: actor, AuthorityJournalID: &boot,
		CommandDigest: digest("xc"), MutationDigest: digest("xm"),
		Effects: []Effect{
			{Sort: EffectAssignmentEnd, AssignmentID: "SPINE-A", TaskID: task, SlotID: SlotOwnerResponsibility},
			{Sort: EffectTaskEvent, TaskID: task, EventKind: EventKindTaskClosed},
		},
	}); err != nil {
		t.Fatalf("close: %v", err)
	}
	return &spineEnv{tr: st, actor: actor, boot: boot, task: task}
}

// firstJournalIDOfKind returns the lowest committed JournalID whose kind matches, so a
// handler can target a concrete task_event row without hardcoding an id.
func (e *spineEnv) firstJournalIDOfKind(t *testing.T, want JournalKind) JournalID {
	t.Helper()
	ids, kinds, err := e.tr.db.AdversarialJournalRows()
	if err != nil {
		t.Fatalf("enumerate journal rows: %v", err)
	}
	for i, k := range kinds {
		if k == want {
			return ids[i]
		}
	}
	t.Fatalf("no committed journal row of kind %s", want)
	return 0
}

// spineErrorActionable reports whether a fail-closed spine error carries the
// where/when/impact/fix guidance the actionable-error standard requires. (The
// subtype-integrity errors state the why in prose rather than a literal "why:" token,
// so this floor is where/when/impact/fix, which every guard here emits.)
func spineErrorActionable(msg string) bool {
	for _, marker := range []string{"where:", "when:", "impact:", "fix:"} {
		if !strings.Contains(msg, marker) {
			return false
		}
	}
	return true
}

// assertSpineFailClosed checks a must-fail spine case: the guard returned the expected
// typed sentinel, the error is actionable, and (silentRepair=false) it rejected rather
// than repaired. It is the shared tail of every corruption operator.
func assertSpineFailClosed(err error, sentinel error, expected anyMap) error {
	if err == nil {
		return fmt.Errorf("corruption was accepted; expected a fail-closed rejection")
	}
	if !errors.Is(err, sentinel) {
		return fmt.Errorf("rejected with %v, want sentinel %v", err, sentinel)
	}
	if want, _ := asBool(expected, "errorActionable"); want && !spineErrorActionable(err.Error()) {
		return fmt.Errorf("fail-closed error is not actionable (missing where/when/impact/fix): %v", err)
	}
	if silent, _ := asBool(expected, "silentRepair"); silent {
		return fmt.Errorf("case marks silentRepair=true, which the fail-closed guards never do")
	}
	return nil
}

// ---------------------------------------------------------------------------
// journal_spine_corruption.yaml (§10 rule 8, §15)
// ---------------------------------------------------------------------------

// opVerifyCleanSpine is the non-vacuous control: a converged history passes both the
// subtype-integrity scan and the from-empty convergence re-derivation.
func opVerifyCleanSpine(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newSpineHistory(t)
	if err := env.tr.Journal().VerifyIntegrity(); err != nil {
		return fmt.Errorf("clean spine failed VerifyIntegrity: %w", err)
	}
	if _, err := env.tr.Journal().ReplayProjections(); err != nil {
		return fmt.Errorf("clean spine failed convergence: %w", err)
	}
	if pass, _ := asBool(expected, "verifyIntegrityPasses"); !pass {
		return fmt.Errorf("control case must mark verifyIntegrityPasses=true")
	}
	if pass, _ := asBool(expected, "convergencePasses"); !pass {
		return fmt.Errorf("control case must mark convergencePasses=true")
	}
	return nil
}

// opCorruptDeleteSubtypeRow deletes a task_event's subtype row out from under its
// surviving supertype row and asserts VerifyIntegrity's totality scan fails closed.
func opCorruptDeleteSubtypeRow(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newSpineHistory(t)
	target := env.firstJournalIDOfKind(t, JournalKindTaskEvent)
	table, err := asString(input, "subtypeTable")
	if err != nil {
		return err
	}
	if err := env.tr.db.AdversarialDeleteSubtypeRow(target, table); err != nil {
		return fmt.Errorf("inject subtype-row deletion: %w", err)
	}
	verr := env.tr.Journal().VerifyIntegrity()
	if err := assertSpineFailClosed(verr, ErrSubtypeIntegrity, expected); err != nil {
		return err
	}
	if !strings.Contains(verr.Error(), fmt.Sprintf("%d", target)) {
		return fmt.Errorf("fail-closed error does not name the offending JournalID %d: %v", target, verr)
	}
	return nil
}

// opCorruptRewriteDiscriminator rewrites a task_event supertype row's kind_id to
// disagree with its subtype table and asserts VerifyIntegrity fails closed.
func opCorruptRewriteDiscriminator(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newSpineHistory(t)
	target := env.firstJournalIDOfKind(t, JournalKindTaskEvent)
	rewriteTo, err := asString(input, "rewriteTo")
	if err != nil {
		return err
	}
	kind, err := spineJournalKind(rewriteTo)
	if err != nil {
		return err
	}
	if err := env.tr.db.AdversarialRewriteDiscriminator(target, kind); err != nil {
		return fmt.Errorf("inject discriminator rewrite: %w", err)
	}
	return assertSpineFailClosed(env.tr.Journal().VerifyIntegrity(), ErrSubtypeIntegrity, expected)
}

// opCorruptTruncateTail deletes the highest-JournalID tail rows and asserts the
// from-empty convergence re-derivation fails closed with a projection divergence.
func opCorruptTruncateTail(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newSpineHistory(t)
	n, err := asInt(input, "truncateTailRows")
	if err != nil {
		return err
	}
	if err := env.tr.db.AdversarialTruncateTail(n); err != nil {
		return fmt.Errorf("inject tail truncation: %w", err)
	}
	_, rerr := env.tr.Journal().ReplayProjections()
	if err := assertSpineFailClosed(rerr, ErrProjectionDivergence, expected); err != nil {
		return err
	}
	if !strings.Contains(rerr.Error(), env.task.String()) {
		return fmt.Errorf("divergence error does not name the diverging task %q: %v", env.task, rerr)
	}
	return nil
}

// opCorruptNoncontiguousInsert inserts a bare supertype row at a non-contiguous
// JournalID with no subtype row and asserts VerifyIntegrity's totality scan fails closed.
func opCorruptNoncontiguousInsert(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newSpineHistory(t)
	gap, err := asInt(input, "journalIdGap")
	if err != nil {
		return err
	}
	jid, err := env.tr.db.AdversarialInsertNonContiguousSupertype(env.actor, gap)
	if err != nil {
		return fmt.Errorf("inject non-contiguous supertype row: %w", err)
	}
	verr := env.tr.Journal().VerifyIntegrity()
	if err := assertSpineFailClosed(verr, ErrSubtypeIntegrity, expected); err != nil {
		return err
	}
	if !strings.Contains(verr.Error(), fmt.Sprintf("%d", jid)) {
		return fmt.Errorf("fail-closed error does not name the non-contiguous JournalID %d: %v", jid, verr)
	}
	return nil
}

// spineJournalKind maps a symbolic corpus kind name to its typed JournalKind, keeping
// the fixture a data selector over a closed set rather than an executable payload.
func spineJournalKind(name string) (JournalKind, error) {
	switch name {
	case "operation":
		return JournalKindOperation, nil
	case "task_event":
		return JournalKindTaskEvent, nil
	case "authority":
		return JournalKindAuthority, nil
	case "decision":
		return JournalKindDecision, nil
	case "evidence":
		return JournalKindEvidence, nil
	default:
		return 0, fmt.Errorf("unknown journal kind %q (closed set: operation/task_event/authority/decision/evidence)", name)
	}
}
