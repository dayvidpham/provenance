package sqlite

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
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

func fixedRegistration(namespace string, min, max, ordinal uint64) journal.FixedSoftwareAgentRegistration {
	id := ptypes.AgentID{Namespace: namespace, UUID: uuid.UUID(journal.BigEndianUUID(ordinal))}
	return journal.FixedSoftwareAgentRegistration{
		Claim:     journal.ActorNamespaceClaim{Namespace: namespace, ClaimantID: namespace, Range: journal.UUIDRange{Min: journal.BigEndianUUID(min), Max: journal.BigEndianUUID(max)}, Codec: journal.OrdinalV1CodecName},
		Entry:     journal.FixedActorEntry{ActorID: journal.ActorID(id), Namespace: namespace, ActorKind: ptypes.AgentKindSoftware, Name: namespace + "/default"},
		AgentName: namespace, Version: "1", Source: "identity-activity-pool-test",
	}
}

func TestIdentityActivityReadsLeaseIndependentlyUnderWAL(t *testing.T) {
	t.Parallel()
	db := openIdentityActivityDB(t)
	agent, err := db.RegisterHumanAgent("pooled", "reader", "")
	if err != nil {
		t.Fatalf("RegisterHumanAgent: %v", err)
	}
	if _, err := db.StartActivity(agent.ID, ptypes.PhaseWorkerSlices, ptypes.StageInProgress, "readable"); err != nil {
		t.Fatalf("StartActivity: %v", err)
	}
	start := make(chan struct{})
	type result struct {
		activities []ptypes.Activity
		err        error
	}
	agentRead, activityRead := make(chan result, 1), make(chan result, 1)
	go func() { <-start; _, err := db.GetHumanAgent(agent.ID); agentRead <- result{err: err} }()
	go func() {
		<-start
		activities, err := db.GetActivities(&agent.ID)
		activityRead <- result{activities: activities, err: err}
	}()
	close(start)
	if _, err := db.RegisterHumanAgent("pooled", "concurrent-writer", ""); err != nil {
		t.Fatalf("unrelated writer during readers: %v", err)
	}
	for _, result := range []result{<-agentRead, <-activityRead} {
		if result.err != nil {
			t.Errorf("concurrent reader: %v", result.err)
		}
		if result.activities != nil && len(result.activities) != 1 {
			t.Errorf("activity reader returned %d rows, want one", len(result.activities))
		}
	}
}

func TestFixedAgentActivationContendingLeasesAreAtomic(t *testing.T) {
	t.Parallel()
	db := openIdentityActivityDB(t)
	winner := fixedRegistration("fixed-a", 0, 10, 1)
	contender := fixedRegistration("fixed-b", 5, 15, 6)
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() { <-start; _, err := db.RegisterFixedSoftwareAgent(winner); errs <- err }()
	go func() { <-start; _, err := db.RegisterFixedSoftwareAgent(contender); errs <- err }()
	close(start)
	first, second := <-errs, <-errs
	if (first == nil) == (second == nil) {
		t.Fatalf("overlapping fixed registration results = %v, %v; want exactly one success", first, second)
	}
	if first != nil && !errors.Is(first, journal.ErrNamespaceRange) {
		t.Fatalf("first fixed registration error = %v, want ErrNamespaceRange", first)
	}
	if second != nil && !errors.Is(second, journal.ErrNamespaceRange) {
		t.Fatalf("second fixed registration error = %v, want ErrNamespaceRange", second)
	}
	claims, err := db.NamespaceClaims()
	if err != nil {
		t.Fatalf("NamespaceClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claim count = %d, want 1", len(claims))
	}
}

func TestNamespaceClaimConflictAcrossContendingLeases(t *testing.T) {
	t.Parallel()
	db := openIdentityActivityDB(t)
	firstClaim := journal.ActorNamespaceClaim{Namespace: "shared", ClaimantID: "first", Range: journal.UUIDRange{Min: journal.BigEndianUUID(20), Max: journal.BigEndianUUID(29)}, Codec: journal.OrdinalV1CodecName}
	secondClaim := journal.ActorNamespaceClaim{Namespace: "shared", ClaimantID: "second", Range: journal.UUIDRange{Min: journal.BigEndianUUID(30), Max: journal.BigEndianUUID(39)}, Codec: journal.OrdinalV1CodecName}
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() { <-start; errs <- db.RegisterNamespaceClaim(firstClaim) }()
	go func() { <-start; errs <- db.RegisterNamespaceClaim(secondClaim) }()
	close(start)
	first, second := <-errs, <-errs
	if (first == nil) == (second == nil) {
		t.Fatalf("same-namespace claim results = %v, %v; want exactly one success", first, second)
	}
	if first != nil && !errors.Is(first, journal.ErrNamespaceClaim) {
		t.Fatalf("first namespace result = %v, want ErrNamespaceClaim", first)
	}
	if second != nil && !errors.Is(second, journal.ErrNamespaceClaim) {
		t.Fatalf("second namespace result = %v, want ErrNamespaceClaim", second)
	}
	claims, err := db.NamespaceClaims()
	if err != nil || len(claims) != 1 {
		t.Fatalf("NamespaceClaims = (%+v, %v), want one row", claims, err)
	}
}

func TestStartActivityWithIDConcurrentReplayReturnsCanonicalRow(t *testing.T) {
	t.Parallel()
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
		go func(i int) {
			<-start
			activity, err := db.StartActivityWithID(id, agent.ID, ptypes.PhaseWorkerSlices, ptypes.StageInProgress, fmt.Sprintf("caller-%d", i))
			results <- activity
			errorsOut <- err
		}(i)
	}
	close(start)
	var canonical string
	for range callers {
		if err := <-errorsOut; err != nil {
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
	if err != nil || len(activities) != 1 || activities[0].Notes != canonical {
		t.Fatalf("stored activities = %+v, %v; want canonical one-row activity", activities, err)
	}
}

func TestIdentityActivityCloseRejectsNewLeases(t *testing.T) {
	t.Parallel()
	db := openIdentityActivityDB(t)
	agent, err := db.RegisterHumanAgent("close", "reader", "")
	if err != nil {
		t.Fatalf("RegisterHumanAgent: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := db.GetAgent(agent.ID); err == nil {
		t.Fatal("GetAgent after Close succeeded, want lease failure")
	}
}

func TestConcurrentActivityLeaseReturn(t *testing.T) {
	t.Parallel()
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
		go func() { defer wg.Done(); <-start; _, err := db.GetActivities(&agent.ID); results <- err }()
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
