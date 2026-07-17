package provenance

// journal_corpus_test.go ports the salvage corpus harness onto the relational
// contract and executes the S1.1-scoped adversarial histories against real
// journal-base production code. Every contract operator that is not yet
// implementable is a recorded s1.2/s1.3 obligation in testdata/contract/scope.yaml
// — checked by the harness, never a skipped or disabled test.

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
// (scope.yaml is the partition table, not a corpus, and is excluded).
var contractCorpusFiles = []string{
	"ordering.yaml",
	"zero_event_operations.yaml",
	"retry_reopen_cancellation.yaml",
	"authority_evidence.yaml",
	"owner_responsibility.yaml",
	"baseline_migration.yaml",
	"topology_corruption.yaml",
	"genesis_bootstrap.yaml",
	"operation_results.yaml",
	"subtype_integrity.yaml",
	"actor_namespace.yaml",
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
// every operator the corpus uses (both directions), and that the executable
// S1.1 partition equals the registered S1.1 operator set — so a new corpus
// operator, a stale scope entry, or an executable operator missing its handler
// all fail loudly rather than silently skipping a history.
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

	// The registered executable operators must be exactly the s1.1 partition.
	registered := make([]testcorpus.OperatorName, 0, len(s11Operators))
	for name := range s11Operators {
		registered = append(registered, name)
	}
	if err := testcorpus.CheckClosedSet(scope.Operators(testcorpus.SliceS11), registered); err != nil {
		t.Fatalf("executable s1.1 registry does not match the s1.1 scope partition: %v", err)
	}

	// The S1.2 operations layer is now executable too: its registry must equal
	// the s1.2 partition exactly (a new corpus operator or a stale handler fails).
	registered12 := make([]testcorpus.OperatorName, 0, len(s12Operators))
	for name := range s12Operators {
		registered12 = append(registered12, name)
	}
	if err := testcorpus.CheckClosedSet(scope.Operators(testcorpus.SliceS12), registered12); err != nil {
		t.Fatalf("executable s1.2 registry does not match the s1.2 scope partition: %v", err)
	}

	// The S1.3 shared-reducer/replay/migration layer is now executable too: its
	// registry must equal the s1.3 partition exactly (a new corpus operator or a
	// stale handler fails).
	registered13 := make([]testcorpus.OperatorName, 0, len(s13Operators))
	for name := range s13Operators {
		registered13 = append(registered13, name)
	}
	if err := testcorpus.CheckClosedSet(scope.Operators(testcorpus.SliceS13), registered13); err != nil {
		t.Fatalf("executable s1.3 registry does not match the s1.3 scope partition: %v", err)
	}

	t.Logf("contract corpus: %d cases across %d files; s1.1 executable=%d s1.2 executable=%d s1.3 executable=%d",
		total, len(contractCorpusFiles),
		len(scope.Operators(testcorpus.SliceS11)),
		len(scope.Operators(testcorpus.SliceS12)),
		len(scope.Operators(testcorpus.SliceS13)))
}

// TestContractCorpusExecutesImplementedPartitions executes every S1.1- and
// S1.2-scoped case against real production code and asserts each remaining s1.3
// case is a recorded, not-yet-executable obligation (no handler registered for
// it). The S1.2 operations layer moves from recorded obligation to executed here.
func TestContractCorpusExecutesImplementedPartitions(t *testing.T) {
	scope := loadScope(t)

	executed11 := 0
	executed12 := 0
	executed13 := 0
	for _, file := range contractCorpusFiles {
		corpus := loadContractCorpus(t, file)
		for _, c := range corpus.Cases {
			slice, ok := scope.SliceOf(c.Mutation.Operator)
			if !ok {
				t.Fatalf("%s/%s: operator %q missing from scope table", file, c.Name, c.Mutation.Operator)
			}
			var registry map[testcorpus.OperatorName]s11Handler
			switch slice {
			case testcorpus.SliceS11:
				registry = s11Operators
			case testcorpus.SliceS12:
				registry = s12Operators
			case testcorpus.SliceS13:
				registry = s13Operators
			default:
				t.Fatalf("%s/%s: operator %q has unknown slice %q", file, c.Name, c.Mutation.Operator, slice)
			}
			op, ok := registry[c.Mutation.Operator]
			if !ok {
				t.Fatalf("%s/%s: %s operator %q has no registered handler", file, c.Name, slice, c.Mutation.Operator)
			}
			t.Run(file+"/"+c.Name, func(t *testing.T) {
				if err := op(t, c.Input, c.Expected, c.Classification); err != nil {
					t.Fatalf("execute %q: %v", c.Mutation.Operator, err)
				}
			})
			switch slice {
			case testcorpus.SliceS11:
				executed11++
			case testcorpus.SliceS12:
				executed12++
			case testcorpus.SliceS13:
				executed13++
			}
		}
	}
	if executed11 == 0 || executed12 == 0 || executed13 == 0 {
		t.Fatalf("expected all partitions to execute; s1.1=%d s1.2=%d s1.3=%d — the harness would vacuously pass", executed11, executed12, executed13)
	}
	t.Logf("executed %d S1.1 + %d S1.2 + %d S1.3 cases against production code", executed11, executed12, executed13)
}

// s11Handler drives one S1.1 case against production code. Returning nil means
// the case's classification (must-pass/must-fail) was honoured.
type s11Handler func(t *testing.T, input, expected anyMap, class testcorpus.Classification) error

// s11Operators is the closed registry of executable S1.1 operators. Its key set
// must equal the s1.1 partition of scope.yaml (asserted above).
var s11Operators = map[testcorpus.OperatorName]s11Handler{
	"order-by-journalid":                   opOrderByJournalID,
	"order-by-journalid-concurrent":        opOrderByJournalIDConcurrent,
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
	tr, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	agent, err := tr.RegisterSoftwareAgent("provenance-test", "corpus-harness", "0", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	task, err := tr.Create("provenance-test", "corpus task", "", TaskTypeTask, PriorityMedium, PhaseUnscoped)
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	return &journalEnv{tr: tr, actor: agent.ID, task: task.ID}
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
		out, err := env.tr.Journal().AppendTaskEvent(AppendTaskEventInput{
			ActorID:    env.actor,
			TaskID:     env.task,
			EventKind:  "provenance.task.updated",
			RecordedAt: recordedAt,
		})
		if err != nil {
			return fmt.Errorf("append %q: %w", label, err)
		}
		labelByJID[out.JournalID] = label
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

	var mu sync.Mutex
	var ids []JournalID
	var firstErr error
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			out, err := env.tr.Journal().AppendTaskEvent(AppendTaskEventInput{
				ActorID:    env.actor,
				TaskID:     env.task,
				EventKind:  "provenance.task.updated",
				RecordedAt: recordedAt,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			ids = append(ids, out.JournalID)
		}()
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
	// The contract exposes only OrderByJournalID; a request to order by any
	// other dimension (here RecordedAt) is represented by an out-of-range
	// OrderDimension and must be rejected by Validate before any query runs.
	dim, err := asString(input, "requestedOrderBy")
	if err != nil {
		return err
	}
	if dim == "journal_id" {
		return fmt.Errorf("case requests journal_id ordering, which is not a rejection scenario")
	}
	_, qErr := env.tr.Journal().QueryTaskEvents(JournalQueryV1{OrderBy: OrderDimension(1)})
	if qErr == nil {
		return fmt.Errorf("query with a non-JournalID order dimension %q was accepted; expected rejection", dim)
	}
	if !errors.Is(qErr, ErrUnsupportedOrderDimension) {
		return fmt.Errorf("rejected with %v, want ErrUnsupportedOrderDimension", qErr)
	}
	if accepted, _ := asBool(expected, "accepted"); accepted {
		return fmt.Errorf("case marks accepted=true but the query was rejected")
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
	err = env.tr.Journal().RegisterFixedActorEntry(entry, fixed)
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
	if _, err := st.db.AppendBareJournalRow(JournalKindTaskEvent, env.actor, time.Now()); err != nil {
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
