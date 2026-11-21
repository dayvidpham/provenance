package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

func TestAdmittedMLRegistrationKeepsVerifiedStoreCurrent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "verified.db")
	seedLedgerStore(t, path)
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw := openVerifiedTablesTestDB(t, path)
	before, verified := readLedgerGeneration(t, path)
	const provider, model = "new-provider", "nested/exact-model"
	agent, err := store.RegisterAdmittedMLAgentContext(t.Context(), "repo/%_fixture", ptypes.RoleWorker, provider, model)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Model.Provider != provider || agent.Model.Name != model || agent.ID.Namespace != "repo/%_fixture" {
		t.Fatalf("registration: %+v", agent)
	}
	after, count := readLedgerGeneration(t, path)
	if after != before+4 || after != readVectorSum(t, path) || count != verified {
		t.Fatalf("ledger after registration: generation=%d, verification count=%d; before=(%d,%d)", after, count, before, verified)
	}
	requireVerifiedAgentSetEqualsRegistry(t, raw)
	if _, err := store.RegisterAdmittedMLAgentContext(t.Context(), agent.ID.Namespace, agent.Role, provider, model); err == nil {
		t.Fatal("duplicate registration succeeded")
	} else {
		var duplicate *DuplicateMLAgentError
		if !errors.As(err, &duplicate) || duplicate.ExistingID != agent.ID {
			t.Fatalf("duplicate error: %v; want existing agent %s", err, agent.ID)
		}
	}
	if err := reopenWithModels(t, path, nil); err != nil {
		t.Fatal(err)
	}
	if generation, count := readLedgerGeneration(t, path); generation != after || count != verified {
		t.Fatalf("ordinary reopen changed the ledger: (%d,%d); want (%d,%d)", generation, count, after, verified)
	}
}

func TestAdmittedMLRegistrationDoesNotAttestStaleProof(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "stale.db")
	seedLedgerStore(t, path)
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	before, verified := readLedgerGeneration(t, path)
	bumpStoreGeneration(t, path)
	if _, err := store.RegisterAdmittedMLAgentContext(t.Context(), "repo", ptypes.RoleWorker, "new-provider", "new-model"); err != nil {
		t.Fatal(err)
	}
	if generation, count := readLedgerGeneration(t, path); generation != before || count != verified || generation == readVectorSum(t, path) {
		t.Fatalf("registration attested a stale proof: got (%d,%d), want unadvanced (%d,%d)", generation, count, before, verified)
	}
}

func TestAdmittedMLRegistrationSerializesDuplicateAcrossHandles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "concurrent.db")
	first, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	type result struct {
		agent ptypes.MLAgent
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, db := range []*DB{first, second} {
		go func() {
			<-start
			agent, err := db.RegisterAdmittedMLAgentContext(t.Context(), "repo", ptypes.RoleWorker, "test-provider", "test-model")
			results <- result{agent, err}
		}()
	}
	close(start)
	winner, loser := <-results, <-results
	if winner.err != nil {
		winner, loser = loser, winner
	}
	var duplicate *DuplicateMLAgentError
	if winner.err != nil || !errors.As(loser.err, &duplicate) || duplicate.ExistingID != winner.agent.ID {
		t.Fatalf("want one success and exact duplicate: winner=%+v loser=%+v", winner, loser)
	}
	raw := openVerifiedTablesTestDB(t, path)
	if ids := requireVerifiedAgentSetEqualsRegistry(t, raw); len(ids) != 1 || ids[0] != winner.agent.ID.String() {
		t.Fatalf("registered agents = %v, want only %s", ids, winner.agent.ID)
	}
}

func TestAdmittedMLRegistrationRollsBackModelOnSubtypeFailure(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "failure.db")
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw := openVerifiedTablesTestDB(t, path)
	before, count := readLedgerGeneration(t, path)
	if _, err := raw.Exec(`CREATE TRIGGER fail_registration BEFORE INSERT ON agents_ml BEGIN SELECT RAISE(ABORT, 'fixture subtype failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterAdmittedMLAgentContext(t.Context(), "repo", ptypes.RoleWorker, "failure-provider", "failure-model"); err == nil {
		t.Fatal("registration committed despite subtype failure")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.RegisterAdmittedMLAgentContext(ctx, "repo", ptypes.RoleWorker, "canceled-provider", "canceled-model"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled registration: %v", err)
	}
	var providers, agents int
	if err := raw.QueryRow(`SELECT (SELECT count(*) FROM providers WHERE name IN ('failure-provider','canceled-provider')), (SELECT count(*) FROM agents)`).Scan(&providers, &agents); err != nil {
		t.Fatal(err)
	}
	after, verified := readLedgerGeneration(t, path)
	if providers != 0 || agents != 0 || readVectorSum(t, path) != before || after != before || verified != count {
		t.Fatalf("partial write after failures: providers=%d agents=%d ledger=(%d,%d), before=(%d,%d)", providers, agents, after, verified, before, count)
	}
}
