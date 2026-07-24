package sqlite

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const identityActivityTestTimeout = 5 * time.Second

func openIdentityActivityDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir()+"/identity-activity.db", nil)
	if err != nil {
		t.Fatalf("Open file DB: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close file DB: %v", err)
		}
	})
	return db
}

func waitIdentityActivityError(t *testing.T, operation string, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(identityActivityTestTimeout):
		t.Fatalf("%s did not finish within %v", operation, identityActivityTestTimeout)
		return nil
	}
}

func fixedRegistration(namespace string, min, max, ordinal uint64) journal.FixedSoftwareAgentRegistration {
	id := ptypes.AgentID{Namespace: namespace, UUID: uuid.UUID(journal.BigEndianUUID(ordinal))}
	return journal.FixedSoftwareAgentRegistration{
		Claim: journal.ActorNamespaceClaim{
			Namespace: namespace, ClaimantID: namespace,
			Range: journal.UUIDRange{Min: journal.BigEndianUUID(min), Max: journal.BigEndianUUID(max)},
			Codec: journal.OrdinalV1CodecName,
		},
		Entry: journal.FixedActorEntry{
			ActorID: journal.ActorID(id), Namespace: namespace,
			ActorKind: ptypes.AgentKindSoftware, Name: namespace + "/default",
		},
		AgentName: namespace, Version: "1", Source: "identity-activity-pool-test",
	}
}

func takeIdentityRuntimeScopes(t *testing.T, db *DB) []*connScope {
	t.Helper()
	scopes := make([]*connScope, 0, runtimePoolSize-1)
	for range runtimePoolSize - 1 {
		scopes = append(scopes, takePoolScope(t, db))
	}
	return scopes
}

func releaseIdentityRuntimeScopes(scopes []*connScope) {
	for _, scope := range scopes {
		scope.release()
	}
}

type identityReadBarrier struct {
	entered chan int
	release chan struct{}
	once    sync.Once
	db      *DB
}

func installIdentityReadBarrier(t *testing.T, db *DB, target string) *identityReadBarrier {
	t.Helper()
	barrier := &identityReadBarrier{entered: make(chan int, runtimePoolSize-1), release: make(chan struct{}), db: db}
	scopes := takeIdentityRuntimeScopes(t, db)
	for id, scope := range scopes {
		id := id
		var enteredOnce sync.Once
		if err := scope.conn.SetCollation("BINARY", func(a, b string) int {
			if a == target && b == target {
				enteredOnce.Do(func() { barrier.entered <- id })
				<-barrier.release
			}
			return strings.Compare(a, b)
		}); err != nil {
			releaseIdentityRuntimeScopes(scopes)
			barrier.unblock()
			t.Fatalf("install reader collation on runtime connection %d: %v", id, err)
		}
	}
	releaseIdentityRuntimeScopes(scopes)
	return barrier
}

func (barrier *identityReadBarrier) unblock() {
	barrier.once.Do(func() { close(barrier.release) })
}

func (barrier *identityReadBarrier) cleanup(t *testing.T) {
	t.Helper()
	barrier.unblock()
	scopes := takeIdentityRuntimeScopes(t, barrier.db)
	defer releaseIdentityRuntimeScopes(scopes)
	for id, scope := range scopes {
		if err := scope.conn.SetCollation("BINARY", nil); err != nil {
			t.Errorf("remove reader collation from runtime connection %d: %v", id, err)
		}
	}
}

type identityWriterBarrier struct {
	begin   chan int
	entered chan int
	release chan struct{}
	once    sync.Once
	db      *DB
}

const identityWriterFunction = "identity_activity_hold_claim"
const identityWriterTrigger = "identity_activity_hold_claim_insert"

func installIdentityWriterBarrier(t *testing.T, db *DB) *identityWriterBarrier {
	t.Helper()
	barrier := &identityWriterBarrier{
		begin: make(chan int, 16), entered: make(chan int, runtimePoolSize-1),
		release: make(chan struct{}), db: db,
	}
	scopes := takeIdentityRuntimeScopes(t, db)
	for id, scope := range scopes {
		id := id
		if err := sqlitex.ExecuteTransient(scope.conn, "PRAGMA busy_timeout=0", nil); err != nil {
			releaseIdentityRuntimeScopes(scopes)
			barrier.unblock()
			t.Fatalf("set zero busy timeout on runtime connection %d: %v", id, err)
		}
		if err := scope.conn.SetAuthorizer(zs.AuthorizeFunc(func(action zs.Action) zs.AuthResult {
			if action.Type() == zs.OpTransaction && action.Operation() == "BEGIN" {
				barrier.begin <- id
			}
			return zs.AuthResultOK
		})); err != nil {
			releaseIdentityRuntimeScopes(scopes)
			barrier.unblock()
			t.Fatalf("install BEGIN authorizer on runtime connection %d: %v", id, err)
		}
		var enteredOnce sync.Once
		if err := scope.conn.CreateFunction(identityWriterFunction, &zs.FunctionImpl{
			NArgs: 0, AllowIndirect: true,
			Scalar: func(zs.Context, []zs.Value) (zs.Value, error) {
				enteredOnce.Do(func() { barrier.entered <- id })
				<-barrier.release
				return zs.IntegerValue(1), nil
			},
		}); err != nil {
			releaseIdentityRuntimeScopes(scopes)
			barrier.unblock()
			t.Fatalf("install trigger scalar on runtime connection %d: %v", id, err)
		}
	}
	createTrigger := fmt.Sprintf(
		"CREATE TRIGGER %s BEFORE INSERT ON actor_namespace_claims BEGIN SELECT %s(); END",
		identityWriterTrigger, identityWriterFunction,
	)
	if err := sqlitex.ExecuteTransient(scopes[0].conn, createTrigger, nil); err != nil {
		releaseIdentityRuntimeScopes(scopes)
		barrier.unblock()
		t.Fatalf("install persistent writer barrier trigger: %v", err)
	}
	releaseIdentityRuntimeScopes(scopes)
	return barrier
}

func (barrier *identityWriterBarrier) unblock() {
	barrier.once.Do(func() { close(barrier.release) })
}

func (barrier *identityWriterBarrier) cleanup(t *testing.T) {
	t.Helper()
	barrier.unblock()
	scopes := takeIdentityRuntimeScopes(t, barrier.db)
	defer releaseIdentityRuntimeScopes(scopes)
	if err := sqlitex.ExecuteTransient(scopes[0].conn, "DROP TRIGGER IF EXISTS "+identityWriterTrigger, nil); err != nil {
		t.Errorf("remove persistent writer barrier trigger: %v", err)
	}
	for id, scope := range scopes {
		if err := scope.conn.SetAuthorizer(nil); err != nil {
			t.Errorf("remove BEGIN authorizer from runtime connection %d: %v", id, err)
		}
		if err := sqlitex.ExecuteTransient(scope.conn, "PRAGMA busy_timeout=5000", nil); err != nil {
			t.Errorf("restore busy timeout on runtime connection %d: %v", id, err)
		}
		if err := scope.conn.CreateFunction(identityWriterFunction, &zs.FunctionImpl{
			NArgs: 0, AllowIndirect: true,
			Scalar: func(zs.Context, []zs.Value) (zs.Value, error) {
				return zs.IntegerValue(1), nil
			},
		}); err != nil {
			t.Errorf("replace writer barrier scalar on runtime connection %d: %v", id, err)
		}
	}
}

func waitIdentityConnection(t *testing.T, operation string, events <-chan int) int {
	t.Helper()
	select {
	case id := <-events:
		return id
	case <-time.After(identityActivityTestTimeout):
		t.Fatalf("%s did not reach instrumented SQL within %v", operation, identityActivityTestTimeout)
		return -1
	}
}

func requireIdentityOwnershipConflict(t *testing.T, operation string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s succeeded while another leased BEGIN IMMEDIATE owned the writer lock", operation)
	}
	switch zs.ErrCode(err).ToPrimary() {
	case zs.ResultBusy, zs.ResultLocked:
	default:
		t.Fatalf("%s error = %v (%v), want SQLITE_BUSY or SQLITE_LOCKED", operation, err, zs.ErrCode(err))
	}
}

func TestIdentityActivityReadsLeaseIndependentlyUnderWAL(t *testing.T) {
	db := openIdentityActivityDB(t)
	agent, err := db.RegisterHumanAgent("pooled", "reader", "")
	if err != nil {
		t.Fatalf("RegisterHumanAgent: %v", err)
	}
	if _, err := db.StartActivity(agent.ID, ptypes.PhaseWorkerSlices, ptypes.StageInProgress, "readable"); err != nil {
		t.Fatalf("StartActivity: %v", err)
	}

	barrier := installIdentityReadBarrier(t, db, agent.ID.String())
	defer barrier.cleanup(t)
	type readResult struct {
		kind       string
		activities []ptypes.Activity
		err        error
	}
	results := make(chan readResult, 2)
	go func() {
		_, err := db.GetHumanAgent(agent.ID)
		results <- readResult{kind: "agent", err: err}
	}()
	go func() {
		activities, err := db.GetActivities(&agent.ID)
		results <- readResult{kind: "activities", activities: activities, err: err}
	}()
	firstConn := waitIdentityConnection(t, "first exported reader", barrier.entered)
	secondConn := waitIdentityConnection(t, "second exported reader", barrier.entered)
	if firstConn == secondConn {
		t.Fatalf("exported readers entered on the same runtime connection %d", firstConn)
	}
	if _, err := db.RegisterHumanAgent("pooled", "concurrent-writer", ""); err != nil {
		t.Fatalf("unrelated exported writer could not commit while distinct WAL readers held snapshots: %v", err)
	}
	barrier.unblock()
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Errorf("%s reader: %v", result.kind, result.err)
		}
		if result.kind == "activities" && len(result.activities) != 1 {
			t.Errorf("activity reader returned %d rows, want pinned one-row snapshot", len(result.activities))
		}
	}
}

func TestFixedAgentActivationContendingLeasesAreAtomic(t *testing.T) {
	db := openIdentityActivityDB(t)
	winner := fixedRegistration("fixed-a", 0, 10, 1)
	contender := fixedRegistration("fixed-b", 5, 15, 6)
	barrier := installIdentityWriterBarrier(t, db)
	defer barrier.cleanup(t)
	winnerDone := make(chan error, 1)
	go func() {
		_, err := db.RegisterFixedSoftwareAgent(winner)
		winnerDone <- err
	}()
	winnerConn := waitIdentityConnection(t, "winning fixed-agent trigger", barrier.entered)
	contenderDone := make(chan error, 1)
	go func() {
		_, err := db.RegisterFixedSoftwareAgent(contender)
		contenderDone <- err
	}()
	contenderConn := waitIdentityConnection(t, "contending fixed-agent BEGIN", barrier.begin)
	for contenderConn == winnerConn {
		contenderConn = waitIdentityConnection(t, "distinct contending fixed-agent BEGIN", barrier.begin)
	}
	initialErr := waitIdentityActivityError(t, "contending fixed-agent ownership attempt", contenderDone)
	requireIdentityOwnershipConflict(t, "contending fixed-agent ownership attempt", initialErr)
	barrier.unblock()
	if err := waitIdentityActivityError(t, "winning fixed-agent commit", winnerDone); err != nil {
		t.Fatalf("winning fixed-agent activation: %v", err)
	}
	if _, err := db.RegisterFixedSoftwareAgent(contender); !errors.Is(err, journal.ErrNamespaceRange) {
		t.Fatalf("fixed-agent retry error = %v, want ErrNamespaceRange", err)
	}

	claims, err := db.NamespaceClaims()
	if err != nil {
		t.Fatalf("NamespaceClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claim count = %d, want 1", len(claims))
	}
	for _, registration := range []journal.FixedSoftwareAgentRegistration{winner, contender} {
		id := ptypes.AgentID(registration.Entry.ActorID)
		_, getErr := db.GetSoftwareAgent(id)
		if registration.Claim.Namespace == claims[0].Namespace && getErr != nil {
			t.Errorf("winning agent %q missing: %v", id, getErr)
		}
		if registration.Claim.Namespace != claims[0].Namespace && !errors.Is(getErr, ptypes.ErrNotFound) {
			t.Errorf("losing agent %q lookup = %v, want ErrNotFound", id, getErr)
		}
	}
}

func TestNamespaceClaimConflictAcrossContendingLeases(t *testing.T) {
	db := openIdentityActivityDB(t)
	winner := journal.ActorNamespaceClaim{Namespace: "shared", ClaimantID: "first", Range: journal.UUIDRange{Min: journal.BigEndianUUID(20), Max: journal.BigEndianUUID(29)}, Codec: journal.OrdinalV1CodecName}
	contender := journal.ActorNamespaceClaim{Namespace: "shared", ClaimantID: "second", Range: journal.UUIDRange{Min: journal.BigEndianUUID(30), Max: journal.BigEndianUUID(39)}, Codec: journal.OrdinalV1CodecName}
	barrier := installIdentityWriterBarrier(t, db)
	defer barrier.cleanup(t)
	winnerDone := make(chan error, 1)
	go func() { winnerDone <- db.RegisterNamespaceClaim(winner) }()
	winnerConn := waitIdentityConnection(t, "winning namespace trigger", barrier.entered)
	contenderDone := make(chan error, 1)
	go func() { contenderDone <- db.RegisterNamespaceClaim(contender) }()
	contenderConn := waitIdentityConnection(t, "contending namespace BEGIN", barrier.begin)
	for contenderConn == winnerConn {
		contenderConn = waitIdentityConnection(t, "distinct contending namespace BEGIN", barrier.begin)
	}
	initialErr := waitIdentityActivityError(t, "contending namespace ownership attempt", contenderDone)
	requireIdentityOwnershipConflict(t, "contending namespace ownership attempt", initialErr)
	barrier.unblock()
	if err := waitIdentityActivityError(t, "winning namespace commit", winnerDone); err != nil {
		t.Fatalf("winning namespace claim: %v", err)
	}
	if err := db.RegisterNamespaceClaim(contender); !errors.Is(err, journal.ErrNamespaceClaim) {
		t.Fatalf("namespace retry error = %v, want ErrNamespaceClaim", err)
	}
	claims, err := db.NamespaceClaims()
	if err != nil {
		t.Fatalf("NamespaceClaims: %v", err)
	}
	if len(claims) != 1 || !claims[0].Equal(winner) {
		t.Fatalf("stored claims = %+v, want only winning claim %+v", claims, winner)
	}
}

func TestStartActivityWithIDConcurrentReplayReturnsCanonicalRow(t *testing.T) {
	db := openIdentityActivityDB(t)
	agent, err := db.RegisterSoftwareAgent("direct", "caller", "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	id := ptypes.ActivityID{Namespace: agent.ID.Namespace, UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("direct-replay"))}

	const callers = 8
	start := make(chan struct{})
	results := make(chan ptypes.Activity, callers)
	errorsOut := make(chan error, callers)
	for i := range callers {
		i := i
		go func() {
			<-start
			activity, err := db.StartActivityWithID(id, agent.ID, ptypes.PhaseWorkerSlices, ptypes.StageInProgress, fmt.Sprintf("caller-%d", i))
			results <- activity
			errorsOut <- err
		}()
	}
	close(start)

	var canonical string
	for range callers {
		if err := waitIdentityActivityError(t, "direct activity replay", errorsOut); err != nil {
			t.Fatalf("StartActivityWithID: %v", err)
		}
		activity := <-results
		if canonical == "" {
			canonical = activity.Notes
		}
		if activity.ID != id || activity.Notes != canonical {
			t.Errorf("replay result = (%v, %q), want (%v, %q)", activity.ID, activity.Notes, id, canonical)
		}
	}
	activities, err := db.GetActivities(&agent.ID)
	if err != nil {
		t.Fatalf("GetActivities: %v", err)
	}
	if len(activities) != 1 || activities[0].Notes != canonical {
		t.Fatalf("stored activities = %+v, want one canonical row with notes %q", activities, canonical)
	}
}

func TestIdentityActivityActiveLeaseIsDrainedByClose(t *testing.T) {
	db := openIdentityActivityDB(t)
	agent, err := db.RegisterHumanAgent("close", "reader", "")
	if err != nil {
		t.Fatalf("RegisterHumanAgent: %v", err)
	}

	held := takePoolScope(t, db)
	defer held.release()
	closeDone := make(chan error, 1)
	go func() { closeDone <- db.Close() }()

	// Observe pool shutdown through the exported read path before checking that
	// Close is still draining the explicitly held lease.
	readDone := make(chan error, 1)
	go func() {
		for {
			_, err := db.GetAgent(agent.ID)
			if err != nil {
				readDone <- err
				return
			}
		}
	}()
	if readErr := waitIdentityActivityError(t, "identity read after pool shutdown", readDone); readErr == nil {
		t.Fatal("GetAgent succeeded after pool shutdown; want lease acquisition failure")
	}
	select {
	case closeErr := <-closeDone:
		t.Fatalf("Close returned while an explicitly owned lease was still active: %v", closeErr)
	default:
	}
	held.release()
	if closeErr := waitIdentityActivityError(t, "Close after identity lease return", closeDone); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
}

func TestConcurrentActivityLeaseReturn(t *testing.T) {
	db := openIdentityActivityDB(t)
	agent, err := db.RegisterHumanAgent("return", "activity", "")
	if err != nil {
		t.Fatalf("RegisterHumanAgent: %v", err)
	}
	const operations = 16
	start := make(chan struct{})
	results := make(chan error, operations)
	var wg sync.WaitGroup
	wg.Add(operations)
	for range operations {
		go func() {
			defer wg.Done()
			<-start
			_, err := db.GetActivities(&agent.ID)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("GetActivities: %v", err)
		}
	}
}
