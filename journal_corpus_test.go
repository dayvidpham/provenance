package provenance

// journal_corpus_test.go executes the relational contract's adversarial
// histories against production code. The checked behavior-area partition keeps
// every corpus operator paired with exactly one handler.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/testcorpus"
	"github.com/google/uuid"
)

// contractCorpusFiles is the explicit set of adversarial proof-corpus files
// (scope.yaml is the behavior-area table, not a corpus, and is excluded).
var contractCorpusFiles = []string{
	"ordering.yaml",
	"zero_event_operations.yaml",
	"retry_reopen_cancellation.yaml",
	"authority_evidence.yaml",
	"owner_responsibility.yaml",
	"baseline_migration.yaml",
	"topology_corruption.yaml",
	"projection_convergence.yaml",
	"genesis_bootstrap.yaml",
	"operation_results.yaml",
	"subtype_integrity.yaml",
	"actor_namespace.yaml",
	"journal_spine_corruption.yaml",
	"authority_revocation.yaml",
	"status_fsm.yaml",
	"mutation_families.yaml",
}

type anyMap = map[string]any

func loadScope(t *testing.T) testcorpus.ScopeTable {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "contract", "scope.yaml"))
	if err != nil {
		t.Fatalf("read scope table: %v", err)
	}
	scope, err := testcorpus.LoadScopeTable(data)
	if err != nil {
		t.Fatalf("load scope table: %v", err)
	}
	if err := scope.Validate(); err != nil {
		t.Fatalf("validate scope table: %v", err)
	}
	return scope
}

func loadContractCorpus(t *testing.T, file string) testcorpus.Corpus[anyMap, anyMap] {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "contract", file))
	if err != nil {
		t.Fatalf("read corpus %s: %v", file, err)
	}
	corpus, err := testcorpus.LoadCorpus[anyMap, anyMap](data)
	if err != nil {
		t.Fatalf("load corpus %s: %v", file, err)
	}
	if err := corpus.Validate(); err != nil {
		t.Fatalf("validate corpus %s: %v", file, err)
	}
	return corpus
}

// TestContractCorpusPartitionIsComplete proves the scope table exactly covers
// every operator the corpus uses (both directions), and that every behavior
// area equals its registered operator set. New operators, stale entries, and
// missing handlers therefore fail loudly instead of skipping a history.
func TestContractCorpusPartitionIsComplete(t *testing.T) {
	scope := loadScope(t)

	used := make(map[testcorpus.OperatorName]struct{})
	total := 0
	for _, file := range contractCorpusFiles {
		corpus := loadContractCorpus(t, file)
		for _, c := range corpus.Cases {
			used[c.Mutation.Operator] = struct{}{}
			total++
		}
	}
	if total == 0 {
		t.Fatal("contract corpus is empty — the harness would vacuously pass")
	}
	if err := scope.CheckPartition(used); err != nil {
		t.Fatalf("scope partition: %v", err)
	}

	registered := make([]testcorpus.OperatorName, 0, len(journalFoundationOperators))
	for name := range journalFoundationOperators {
		registered = append(registered, name)
	}
	if err := testcorpus.CheckClosedSet(scope.Operators(testcorpus.AreaJournalFoundation), registered); err != nil {
		t.Fatalf("journal-foundation registry does not match its scope partition: %v", err)
	}

	registeredOperations := make([]testcorpus.OperatorName, 0, len(operationLifecycleOperators))
	for name := range operationLifecycleOperators {
		registeredOperations = append(registeredOperations, name)
	}
	if err := testcorpus.CheckClosedSet(scope.Operators(testcorpus.AreaOperationLifecycle), registeredOperations); err != nil {
		t.Fatalf("operation-lifecycle registry does not match its scope partition: %v", err)
	}

	registeredRecovery := make([]testcorpus.OperatorName, 0, len(recoveryAndMigrationOperators))
	for name := range recoveryAndMigrationOperators {
		registeredRecovery = append(registeredRecovery, name)
	}
	if err := testcorpus.CheckClosedSet(scope.Operators(testcorpus.AreaRecoveryAndMigration), registeredRecovery); err != nil {
		t.Fatalf("recovery-and-migration registry does not match its scope partition: %v", err)
	}

	t.Logf("contract corpus: %d cases across %d files; journal=%d operations=%d recovery=%d",
		total, len(contractCorpusFiles),
		len(scope.Operators(testcorpus.AreaJournalFoundation)),
		len(scope.Operators(testcorpus.AreaOperationLifecycle)),
		len(scope.Operators(testcorpus.AreaRecoveryAndMigration)))
}

// TestContractCorpusExecutesImplementedPartitions executes every behavior area
// against real production code.
func TestContractCorpusExecutesImplementedPartitions(t *testing.T) {
	t.Parallel()
	scope := loadScope(t)

	executedJournal := 0
	executedOperations := 0
	executedRecovery := 0
	for _, file := range contractCorpusFiles {
		corpus := loadContractCorpus(t, file)
		for _, c := range corpus.Cases {
			area, ok := scope.AreaOf(c.Mutation.Operator)
			if !ok {
				t.Fatalf("%s/%s: operator %q missing from scope table", file, c.Name, c.Mutation.Operator)
			}
			var registry map[testcorpus.OperatorName]corpusHandler
			switch area {
			case testcorpus.AreaJournalFoundation:
				registry = journalFoundationOperators
			case testcorpus.AreaOperationLifecycle:
				registry = operationLifecycleOperators
			case testcorpus.AreaRecoveryAndMigration:
				registry = recoveryAndMigrationOperators
			default:
				t.Fatalf("%s/%s: operator %q has unknown behavior area %q", file, c.Name, c.Mutation.Operator, area)
			}
			op, ok := registry[c.Mutation.Operator]
			if !ok {
				t.Fatalf("%s/%s: %s operator %q has no registered handler", file, c.Name, area, c.Mutation.Operator)
			}
			t.Run(file+"/"+c.Name, func(t *testing.T) {
				t.Parallel()
				if err := op(t, c.Input, c.Expected, c.Classification); err != nil {
					t.Fatalf("execute %q: %v", c.Mutation.Operator, err)
				}
			})
			switch area {
			case testcorpus.AreaJournalFoundation:
				executedJournal++
			case testcorpus.AreaOperationLifecycle:
				executedOperations++
			case testcorpus.AreaRecoveryAndMigration:
				executedRecovery++
			}
		}
	}
	if executedJournal == 0 || executedOperations == 0 || executedRecovery == 0 {
		t.Fatalf("expected all behavior areas to execute; journal=%d operations=%d recovery=%d", executedJournal, executedOperations, executedRecovery)
	}
	t.Logf("executed %d journal + %d operation + %d recovery cases against production code", executedJournal, executedOperations, executedRecovery)
}

// corpusHandler drives one case against production code. Returning nil means
// the case's classification (must-pass/must-fail) was honoured.
type corpusHandler func(t *testing.T, input, expected anyMap, class testcorpus.Classification) error

// journalFoundationOperators is the closed registry for journal behavior.
var journalFoundationOperators = map[testcorpus.OperatorName]corpusHandler{
	"order-by-journalid":                   opOrderByJournalID,
	"order-by-journalid-concurrent":        opOrderByJournalIDConcurrent,
	"order-by-recordedat-timeline":         opOrderByRecordedAtTimeline,
	"reject-non-journalid-order-request":   opRejectNonJournalIDOrder,
	"claim-two-disjoint-namespaces":        opClaimTwoDisjoint,
	"claim-overlapping-namespace":          opClaimOverlapping,
	"register-manifest-entry-out-of-range": opManifestEntryOutOfRange,
	"write-journal-row-missing-subtype":    opWriteJournalRowMissingSubtype,
}

// ---------------------------------------------------------------------------
// Shared journal environment
// ---------------------------------------------------------------------------

type journalEnv struct {
	tr    Tracker
	actor ActorID
	task  TaskID
}

func newJournalEnv(t *testing.T) *journalEnv {
	t.Helper()
	tr, err := OpenMemory(WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	agent, err := tr.RegisterSoftwareAgent("provenance-test", "corpus-harness", "0", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	// Journal-query operators need a real task row to append
	// task-events onto (AppendTaskEvent projects onto an existing tasks row). Seed it
	// as a pre-journal (legacy-shape) row via the raw seeding seam rather than through
	// a journaled creation, so the shared env.task carries no creation operation that
	// would pollute the whole-journal ordering queries these operators assert over.
	taskID := newCorpusTaskID()
	now := time.Now().UTC()
	st := tr.(*sqliteTracker)
	if err := st.db.SeedLegacyTask(LegacyTaskRow{ID: taskID, Status: TaskStatusOpen, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed base env task: %v", err)
	}
	return &journalEnv{tr: tr, actor: agent.ID, task: taskID}
}

// newCorpusTaskID mints a fresh namespaced UUIDv7 TaskID for the corpus harness,
// replacing the id the retired Tracker.Create previously assigned.
func newCorpusTaskID() TaskID {
	return TaskID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}
}

// genesisBoot establishes the shared genesis bootstrap authority (which governs every
// task, §14.1) and returns its produced JournalID, so the ordering operators can emit
// operation-anchored task events on the base task. The producer constraint
// forbids a NULL-producer task event, so every task event flows through an operation.
func (e *journalEnv) genesisBoot(t *testing.T) JournalID {
	t.Helper()
	res, err := e.tr.Journal().Apply(OperationInput{
		OperationID:    "op-genesis",
		ActorID:        e.actor,
		CommandDigest:  []byte("genesis-c"),
		MutationDigest: []byte("genesis-m"),
		RecordedAt:     time.Now().UTC().UnixNano(),
		Effects:        []Effect{{Sort: EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth"}},
	})
	if err != nil {
		t.Fatalf("genesisBoot: %v", err)
	}
	if jid, ok := slotJournalID(res, "auth"); ok {
		return jid
	}
	t.Fatal("genesisBoot: no bootstrap authority result slot")
	return 0
}

// appendEventViaOp emits one task event on task as an operation under boot, returning
// the produced event's JournalID — the operation-anchored replacement for the retired
// bare AppendTaskEvent. The event's RecordedAt is carried honestly via a per-effect
// override (§12); the operation anchor row is not a task event, so it never appears in
// the QueryTaskEvents results these ordering operators assert over.
func appendEventViaOp(t *testing.T, tr Tracker, boot JournalID, actor ActorID, task TaskID, opID string, kind EventKind, recordedAt time.Time) JournalID {
	t.Helper()
	auth := boot
	ra := recordedAt.UTC().UnixNano()
	res, err := tr.Journal().Apply(OperationInput{
		OperationID:        OperationID(opID),
		ActorID:            actor,
		AuthorityJournalID: &auth,
		CommandDigest:      []byte(opID + "-c"),
		MutationDigest:     []byte(opID + "-m"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []Effect{{
			Sort: EffectTaskEvent, TaskID: task, EventKind: kind,
			RecordedAtOverride: &ra, ResultSlot: "ev",
		}},
	})
	if err != nil {
		t.Fatalf("appendEventViaOp %q: %v", opID, err)
	}
	if jid, ok := slotJournalID(res, "ev"); ok {
		return jid
	}
	t.Fatalf("appendEventViaOp %q: no result slot", opID)
	return 0
}

// ---------------------------------------------------------------------------
// Ordering operators (§8.3, §12)
// ---------------------------------------------------------------------------

func opOrderByJournalID(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newJournalEnv(t)
	rows, err := asList(input, "rows")
	if err != nil {
		return err
	}
	boot := env.genesisBoot(t)
	labelByJID := map[JournalID]string{}
	for _, r := range rows {
		row, err := asMap(r)
		if err != nil {
			return err
		}
		label, err := asString(row, "id")
		if err != nil {
			return err
		}
		recordedAt, err := asTime(row, "recordedAt")
		if err != nil {
			return err
		}
		jid := appendEventViaOp(t, env.tr, boot, env.actor, env.task, "op-ev-"+label, "provenance.task.updated", recordedAt)
		labelByJID[jid] = label
	}

	page, err := env.tr.Journal().QueryTaskEvents(JournalQueryV1{OrderBy: OrderByJournalID})
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	var got []string
	var lastJID JournalID
	for _, ev := range page.Events {
		if ev.JournalID <= lastJID {
			return fmt.Errorf("query returned non-ascending JournalID: %d after %d", ev.JournalID, lastJID)
		}
		lastJID = ev.JournalID
		got = append(got, labelByJID[ev.JournalID])
	}
	want, err := asStringList(expected, "order")
	if err != nil {
		return err
	}
	if !equalStrings(got, want) {
		return fmt.Errorf("JournalID order = %v, want %v (RecordedAt must not reorder)", got, want)
	}
	return nil
}

func opOrderByJournalIDConcurrent(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newJournalEnv(t)
	n, err := asInt(input, "concurrentRows")
	if err != nil {
		return err
	}
	recordedAt, err := asTime(input, "recordedAt")
	if err != nil {
		return err
	}

	boot := env.genesisBoot(t)
	var mu sync.Mutex
	var ids []JournalID
	var firstErr error
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// Operation-anchored append (a distinct OperationID per goroutine); the
			// journal write path serialises operations (§9.5), so concurrent appends
			// still receive strictly-ascending unique JournalIDs. t.Fatalf is unsafe off
			// the test goroutine, so errors are captured into firstErr.
			auth := boot
			ra := recordedAt.UTC().UnixNano()
			res, err := env.tr.Journal().Apply(OperationInput{
				OperationID:        OperationID(fmt.Sprintf("op-concurrent-%d", i)),
				ActorID:            env.actor,
				AuthorityJournalID: &auth,
				CommandDigest:      []byte(fmt.Sprintf("cc-%d", i)),
				MutationDigest:     []byte(fmt.Sprintf("cm-%d", i)),
				RecordedAt:         time.Now().UTC().UnixNano(),
				Effects: []Effect{{
					Sort: EffectTaskEvent, TaskID: env.task, EventKind: "provenance.task.updated",
					RecordedAtOverride: &ra, ResultSlot: "ev",
				}},
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			jid, ok := slotJournalID(res, "ev")
			if !ok {
				if firstErr == nil {
					firstErr = fmt.Errorf("concurrent append produced no event result slot")
				}
				return
			}
			ids = append(ids, jid)
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return fmt.Errorf("concurrent append: %w", firstErr)
	}
	if len(ids) != n {
		return fmt.Errorf("expected %d JournalIDs, got %d", n, len(ids))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			return fmt.Errorf("JournalIDs not strictly ascending/unique: %d then %d", ids[i-1], ids[i])
		}
	}
	want, _ := asBool(expected, "strictlyAscendingUniqueJournalIDs")
	if !want {
		return fmt.Errorf("case expected strictlyAscendingUniqueJournalIDs=false, which the journal never produces")
	}
	return nil
}

func opRejectNonJournalIDOrder(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newJournalEnv(t)
	// The contract exposes a closed set of order dimensions — the canonical
	// OrderByJournalID and the display OrderByRecordedAt. A request for anything
	// outside that set is represented by an out-of-range OrderDimension and must
	// be rejected by Validate before any query runs.
	dim, err := asString(input, "requestedOrderBy")
	if err != nil {
		return err
	}
	if dim == "journal_id" || dim == "recorded_at" {
		return fmt.Errorf("case requests the exposed %q ordering, which is not a rejection scenario", dim)
	}
	// A value beyond the two exposed dimensions (0=recorded_at, 1=journal_id).
	unexposed := OrderDimension(99)
	if unexposed.IsValid() {
		return fmt.Errorf("test setup error: OrderDimension(99) is unexpectedly a valid dimension")
	}
	_, qErr := env.tr.Journal().QueryTaskEvents(JournalQueryV1{OrderBy: unexposed})
	if qErr == nil {
		return fmt.Errorf("query with an unexposed order dimension %q was accepted; expected rejection", dim)
	}
	if !errors.Is(qErr, ErrUnsupportedOrderDimension) {
		return fmt.Errorf("rejected with %v, want ErrUnsupportedOrderDimension", qErr)
	}
	if accepted, _ := asBool(expected, "accepted"); accepted {
		return fmt.Errorf("case marks accepted=true but the query was rejected")
	}
	return nil
}

// opOrderByRecordedAtTimeline drives the readable-timeline display order (§12)
// against production code: it appends the case's rows in list (commit) order,
// then walks OrderByRecordedAt paginated with the composite (recorded_at,
// journal_id) cursor and asserts the walk is complete and duplicate-free in
// displayOrder, while the canonical OrderByJournalID query still returns
// canonicalOrder. This proves the display-vs-canonical firewall end to end.
func opOrderByRecordedAtTimeline(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newJournalEnv(t)
	rows, err := asList(input, "rows")
	if err != nil {
		return err
	}
	pageLimit, err := asInt(input, "pageLimit")
	if err != nil {
		return err
	}
	boot := env.genesisBoot(t)
	labelByJID := map[JournalID]string{}
	for _, r := range rows {
		row, err := asMap(r)
		if err != nil {
			return err
		}
		label, err := asString(row, "id")
		if err != nil {
			return err
		}
		recordedAt, err := asTime(row, "recordedAt")
		if err != nil {
			return err
		}
		jid := appendEventViaOp(t, env.tr, boot, env.actor, env.task, "op-ev-"+label, "provenance.task.updated", recordedAt)
		labelByJID[jid] = label
	}

	// Paginated timeline walk with the composite exclusive cursor.
	var display []string
	seen := map[JournalID]int{}
	q := JournalQueryV1{OrderBy: OrderByRecordedAt, Limit: pageLimit}
	for {
		page, err := env.tr.Journal().QueryTaskEvents(q)
		if err != nil {
			return fmt.Errorf("timeline page: %w", err)
		}
		for _, ev := range page.Events {
			seen[ev.JournalID]++
			display = append(display, labelByJID[ev.JournalID])
		}
		if page.Next == nil {
			break
		}
		q = JournalQueryV1{
			OrderBy: OrderByRecordedAt, Limit: pageLimit,
			SnapshotMaxJournalID: page.Next.SnapshotMaxJournalID,
			AfterJournalID:       page.Next.AfterJournalID,
			AfterRecordedAt:      page.Next.AfterRecordedAt,
		}
	}
	for jid, n := range seen {
		if n != 1 {
			return fmt.Errorf("journal row %d appeared %d times in the timeline walk; want exactly 1 (no skip/duplicate)", jid, n)
		}
	}
	wantDisplay, err := asStringList(expected, "displayOrder")
	if err != nil {
		return err
	}
	if !equalStrings(display, wantDisplay) {
		return fmt.Errorf("timeline display order = %v, want %v", display, wantDisplay)
	}

	// The canonical order is untouched: journal_id ascending == commit order.
	canon, err := env.tr.Journal().QueryTaskEvents(JournalQueryV1{OrderBy: OrderByJournalID})
	if err != nil {
		return fmt.Errorf("canonical query: %w", err)
	}
	var got []string
	var lastJID JournalID
	for _, ev := range canon.Events {
		if ev.JournalID <= lastJID {
			return fmt.Errorf("canonical query returned non-ascending journal_id: %d after %d", ev.JournalID, lastJID)
		}
		lastJID = ev.JournalID
		got = append(got, labelByJID[ev.JournalID])
	}
	wantCanon, err := asStringList(expected, "canonicalOrder")
	if err != nil {
		return err
	}
	if !equalStrings(got, wantCanon) {
		return fmt.Errorf("canonical journal_id order = %v, want %v (firewall intact)", got, wantCanon)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Actor-namespace operators (§7)
// ---------------------------------------------------------------------------

func opClaimTwoDisjoint(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newJournalEnv(t)
	claims, err := asList(input, "claims")
	if err != nil {
		return err
	}
	for _, c := range claims {
		claim, err := parseClaim(c)
		if err != nil {
			return err
		}
		if err := env.tr.Journal().RegisterNamespaceClaim(claim); err != nil {
			return fmt.Errorf("register %q: %w", claim.Namespace, err)
		}
	}
	stored, err := env.tr.Journal().NamespaceClaims()
	if err != nil {
		return err
	}
	want, err := asInt(expected, "claimsRegistered")
	if err != nil {
		return err
	}
	if len(stored) != want {
		return fmt.Errorf("registered %d claims, want %d", len(stored), want)
	}
	return nil
}

func opClaimOverlapping(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newJournalEnv(t)
	existing, err := parseClaimMap(mustMap(input, "existingClaim"))
	if err != nil {
		return err
	}
	newClaim, err := parseClaimMap(mustMap(input, "newClaim"))
	if err != nil {
		return err
	}
	if err := env.tr.Journal().RegisterNamespaceClaim(existing); err != nil {
		return fmt.Errorf("register existing %q: %w", existing.Namespace, err)
	}
	err = env.tr.Journal().RegisterNamespaceClaim(newClaim)
	if err == nil {
		return fmt.Errorf("overlapping claim %q was accepted; expected rejection", newClaim.Namespace)
	}
	if !errors.Is(err, ErrNamespaceRange) {
		return fmt.Errorf("rejected with %v, want ErrNamespaceRange", err)
	}
	msg := err.Error()
	if wantBoth, _ := asBool(expected, "errorNamesBothNamespaces"); wantBoth {
		if !contains(msg, existing.Namespace) || !contains(msg, newClaim.Namespace) {
			return fmt.Errorf("error %q must name both %q and %q", msg, existing.Namespace, newClaim.Namespace)
		}
	}
	return nil
}

func opManifestEntryOutOfRange(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newJournalEnv(t)
	ns, err := asString(input, "namespace")
	if err != nil {
		return err
	}
	rng, err := parseRangeMap(mustMap(input, "claimedRange"))
	if err != nil {
		return err
	}
	claim := ActorNamespaceClaim{Namespace: ns, ClaimantID: ns, Range: rng, Codec: OrdinalV1CodecName}
	if err := env.tr.Journal().RegisterNamespaceClaim(claim); err != nil {
		return fmt.Errorf("register claim %q: %w", ns, err)
	}
	ordinal, err := asInt(input, "entryActorOrdinal")
	if err != nil {
		return err
	}
	fixed := BigEndianUUID(uint64(ordinal))
	entry := FixedActorEntry{
		ActorID:   ActorID{Namespace: ns, UUID: uuid.UUID(fixed)},
		Namespace: ns,
		Name:      "out-of-range-actor",
	}
	err = env.tr.Journal().RegisterFixedActorEntry(entry)
	if err == nil {
		return fmt.Errorf("out-of-range entry (ordinal %d) was accepted; expected rejection", ordinal)
	}
	if !errors.Is(err, ErrEntryOutOfRange) {
		return fmt.Errorf("rejected with %v, want ErrEntryOutOfRange", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Subtype-integrity operator (§10 rule 8)
// ---------------------------------------------------------------------------

func opWriteJournalRowMissingSubtype(t *testing.T, input, expected anyMap, _ testcorpus.Classification) error {
	env := newJournalEnv(t)
	// Reach the adversarial bare-row seam on the concrete store: a supertype
	// journal row with no matching subtype row. Production writers never do this;
	// the seam exists only so the corpus can drive the production VerifyIntegrity
	// guard against a totality violation.
	st, ok := env.tr.(*sqliteTracker)
	if !ok {
		return fmt.Errorf("expected *sqliteTracker, got %T", env.tr)
	}
	// A bare decision row (a kind with a subtype table but no subtype row) drives the
	// totality violation; a task-event kind is no longer usable here because the
	// producer constraint forbids a NULL-producer task event at insert time.
	if _, err := st.db.AppendBareJournalRow(JournalKindDecision, env.actor, time.Now()); err != nil {
		return fmt.Errorf("append bare journal row: %w", err)
	}
	err := env.tr.Journal().VerifyIntegrity()
	if err == nil {
		return fmt.Errorf("journal row with no subtype row passed VerifyIntegrity; expected rejection")
	}
	if !errors.Is(err, ErrSubtypeIntegrity) {
		return fmt.Errorf("rejected with %v, want ErrSubtypeIntegrity", err)
	}
	if accepted, _ := asBool(expected, "accepted"); accepted {
		return fmt.Errorf("case marks accepted=true but integrity was violated")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Map / value helpers
// ---------------------------------------------------------------------------

func asString(m anyMap, key string) (string, error) {
	v, ok := m[key]
	if !ok {
		return "", fmt.Errorf("missing string field %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("field %q is %T, want string", key, v)
	}
	return s, nil
}

func asInt(m anyMap, key string) (int, error) {
	v, ok := m[key]
	if !ok {
		return 0, fmt.Errorf("missing int field %q", key)
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("field %q is %T, want int", key, v)
	}
}

func asBool(m anyMap, key string) (bool, error) {
	v, ok := m[key]
	if !ok {
		return false, fmt.Errorf("missing bool field %q", key)
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("field %q is %T, want bool", key, v)
	}
	return b, nil
}

func asList(m anyMap, key string) ([]any, error) {
	v, ok := m[key]
	if !ok {
		return nil, fmt.Errorf("missing list field %q", key)
	}
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("field %q is %T, want list", key, v)
	}
	return l, nil
}

func asStringList(m anyMap, key string) ([]string, error) {
	l, err := asList(m, key)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(l))
	for i, v := range l {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%d] is %T, want string", key, i, v)
		}
		out = append(out, s)
	}
	return out, nil
}

func asMap(v any) (anyMap, error) {
	m, ok := v.(anyMap)
	if !ok {
		return nil, fmt.Errorf("value is %T, want map", v)
	}
	return m, nil
}

func mustMap(m anyMap, key string) anyMap {
	sub, _ := m[key].(anyMap)
	return sub
}

func asTime(m anyMap, key string) (time.Time, error) {
	s, err := asString(m, key)
	if err != nil {
		return time.Time{}, err
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("field %q %q is not RFC3339: %w", key, s, err)
	}
	return ts.UTC(), nil
}

func parseClaim(v any) (ActorNamespaceClaim, error) {
	m, err := asMap(v)
	if err != nil {
		return ActorNamespaceClaim{}, err
	}
	return parseClaimMap(m)
}

func parseClaimMap(m anyMap) (ActorNamespaceClaim, error) {
	if m == nil {
		return ActorNamespaceClaim{}, fmt.Errorf("claim map is nil")
	}
	ns, err := asString(m, "namespace")
	if err != nil {
		return ActorNamespaceClaim{}, err
	}
	rng, err := parseRangeMap(m)
	if err != nil {
		return ActorNamespaceClaim{}, err
	}
	return ActorNamespaceClaim{Namespace: ns, ClaimantID: ns, Range: rng, Codec: OrdinalV1CodecName}, nil
}

func parseRangeMap(m anyMap) (UUIDRange, error) {
	if m == nil {
		return UUIDRange{}, fmt.Errorf("range map is nil")
	}
	minHex, err := asString(m, "rangeMin")
	if err != nil {
		return UUIDRange{}, err
	}
	maxHex, err := asString(m, "rangeMax")
	if err != nil {
		return UUIDRange{}, err
	}
	min, err := hex16(minHex)
	if err != nil {
		return UUIDRange{}, fmt.Errorf("rangeMin: %w", err)
	}
	max, err := hex16(maxHex)
	if err != nil {
		return UUIDRange{}, fmt.Errorf("rangeMax: %w", err)
	}
	return UUIDRange{Min: min, Max: max}, nil
}

func hex16(s string) ([16]byte, error) {
	var out [16]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, fmt.Errorf("decode hex %q: %w", s, err)
	}
	if len(b) != 16 {
		return out, fmt.Errorf("hex %q decodes to %d bytes, want 16", s, len(b))
	}
	copy(out[:], b)
	return out, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack, needle string) bool {
	return needle != "" && strings.Contains(haystack, needle)
}
