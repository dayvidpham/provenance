package provenance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

type fixedAgentFixture struct {
	ValidationCases []struct {
		Name      string `yaml:"name"`
		Mutation  string `yaml:"mutation"`
		WantError string `yaml:"wantError"`
	} `yaml:"validationCases"`
	RollbackCases []struct {
		Name     string `yaml:"name"`
		Boundary string `yaml:"boundary"`
	} `yaml:"rollbackCases"`
	ActionableErrorCases []struct {
		Name      string `yaml:"name"`
		Mode      string `yaml:"mode"`
		WantError string `yaml:"wantError"`
		WantWhere string `yaml:"wantWhere"`
		WantWhen  string `yaml:"wantWhen"`
	} `yaml:"actionableErrorCases"`
}

func loadFixedAgentFixture(t *testing.T) fixedAgentFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/fixed_software_agent.yaml")
	if err != nil {
		t.Fatalf("read fixed software-agent fixture: %v", err)
	}
	var fixture fixedAgentFixture
	if err := yaml.Unmarshal(b, &fixture); err != nil {
		t.Fatalf("decode fixed software-agent fixture: %v", err)
	}
	if len(fixture.ValidationCases) < 8 || len(fixture.RollbackCases) != 4 || len(fixture.ActionableErrorCases) != 5 {
		t.Fatalf("fixed software-agent fixture is incomplete: %d validation, %d rollback, %d actionable errors",
			len(fixture.ValidationCases), len(fixture.RollbackCases), len(fixture.ActionableErrorCases))
	}
	return fixture
}

func testFixedSoftwareAgentRegistration() FixedSoftwareAgentRegistration {
	ns := "pasture-system"
	id := ActorID{Namespace: ns, UUID: uuid.UUID(BigEndianUUID(0))}
	return FixedSoftwareAgentRegistration{
		Claim: ActorNamespaceClaim{
			Namespace: ns, ClaimantID: "pasture", Codec: OrdinalV1CodecName,
			Range: UUIDRange{Min: BigEndianUUID(0), Max: BigEndianUUID(1023)},
		},
		Entry: FixedActorEntry{
			ActorID: id, Namespace: ns, ActorKind: AgentKindSoftware,
			Name: "pasture-system/default", Metadata: `{"manifest":"v1"}`,
		},
		AgentName: "pasture-system", Version: "1", Source: "pasture",
	}
}

func newFixedAgentSQLTracker(t *testing.T) (Tracker, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixed-agent-sql.sqlite")
	tr, err := OpenSQLite(path, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatalf("open file-backed fixed-agent SQL fixture: %v", err)
	}
	return tr, path
}

func TestRegisterFixedSoftwareAgentValidationCorpus(t *testing.T) {
	t.Parallel()
	fixture := loadFixedAgentFixture(t)
	for _, tc := range fixture.ValidationCases {
		t.Run(tc.Name, func(t *testing.T) {
			reg := testFixedSoftwareAgentRegistration()
			switch tc.Mutation {
			case "none":
			case "empty-claim-namespace":
				reg.Claim.Namespace = ""
			case "empty-actor-namespace":
				reg.Entry.ActorID.Namespace = ""
			case "mismatched-entry-namespace":
				reg.Entry.Namespace = "other"
			case "wrong-kind":
				reg.Entry.ActorKind = AgentKindHuman
			case "malformed-metadata":
				reg.Entry.Metadata = "{"
			case "out-of-range":
				reg.Entry.ActorID.UUID = uuid.UUID(BigEndianUUID(4096))
			case "reversed-range":
				reg.Claim.Range = UUIDRange{Min: BigEndianUUID(2), Max: BigEndianUUID(1)}
			default:
				t.Fatalf("unknown fixture mutation %q", tc.Mutation)
			}

			tr, err := OpenMemory(WithModelRegistry(NewRegistry(nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer tr.Close()
			_, err = tr.RegisterFixedSoftwareAgent(reg)
			want := fixedAgentSentinel(tc.WantError)
			if want == nil && err != nil {
				t.Fatalf("RegisterFixedSoftwareAgent: %v", err)
			}
			if want != nil && !errors.Is(err, want) {
				t.Fatalf("RegisterFixedSoftwareAgent error = %v, want errors.Is(%v)", err, want)
			}
		})
	}
}

func fixedAgentSentinel(name string) error {
	switch name {
	case "":
		return nil
	case "namespace-claim":
		return ErrNamespaceClaim
	case "invalid-id":
		return ErrInvalidID
	case "agent-kind":
		return ErrAgentKindMismatch
	case "agent-exists":
		return ErrAgentAlreadyExists
	case "entry-range":
		return ErrEntryOutOfRange
	case "namespace-range":
		return ErrNamespaceRange
	default:
		panic("unknown fixed-agent sentinel: " + name)
	}
}

func TestRegisterFixedSoftwareAgentErrorsAreActionable(t *testing.T) {
	t.Parallel()
	for _, tc := range loadFixedAgentFixture(t).ActionableErrorCases {
		t.Run(tc.Name, func(t *testing.T) {
			tr, path := newFixedAgentSQLTracker(t)
			defer tr.Close()
			reg := testFixedSoftwareAgentRegistration()
			switch tc.Mode {
			case "validation":
				reg.Entry.ActorID.Namespace = ""
			case "range":
				reg.Entry.ActorID.UUID = uuid.UUID(BigEndianUUID(4096))
			case "preflight":
				if _, err := tr.RegisterFixedSoftwareAgent(reg); err != nil {
					t.Fatalf("seed activation: %v", err)
				}
				reg.AgentName = "different"
			case "write":
				cleanup := installFixedAgentAbortTrigger(t, path, "claim")
				defer cleanup()
			case "transaction":
				if err := tr.Close(); err != nil {
					t.Fatalf("close tracker before transaction test: %v", err)
				}
			default:
				t.Fatalf("unknown actionable error mode %q", tc.Mode)
			}

			_, err := tr.RegisterFixedSoftwareAgent(reg)
			if err == nil {
				t.Fatal("RegisterFixedSoftwareAgent succeeded; want actionable error")
			}
			if want := fixedAgentSentinel(tc.WantError); want != nil && !errors.Is(err, want) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, want)
			}
			message := err.Error()
			for _, marker := range []string{
				"why:", "where:", tc.WantWhere,
				"when:", tc.WantWhen, "impact:", "fix:",
			} {
				if !strings.Contains(message, marker) {
					t.Errorf("error %q is missing %q", message, marker)
				}
			}
			for _, marker := range []string{"why:", "where:", "when:", "impact:", "fix:"} {
				if count := strings.Count(message, marker); count != 1 {
					t.Errorf("error has %d %q sections, want one authoritative section: %q", count, marker, message)
				}
			}
		})
	}
}

func TestRegisterFixedSoftwareAgentOverlapIsActionableOnce(t *testing.T) {
	t.Parallel()
	tr, err := OpenMemory(WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	if _, err := tr.RegisterFixedSoftwareAgent(testFixedSoftwareAgentRegistration()); err != nil {
		t.Fatalf("seed activation: %v", err)
	}
	overlap := testFixedSoftwareAgentRegistration()
	overlap.Claim.Namespace = "pasture-overlap"
	overlap.Claim.ClaimantID = "pasture-overlap"
	overlap.Claim.Range = UUIDRange{Min: BigEndianUUID(512), Max: BigEndianUUID(1535)}
	overlap.Entry.Namespace = overlap.Claim.Namespace
	overlap.Entry.ActorID.Namespace = overlap.Claim.Namespace
	overlap.Entry.ActorID.UUID = uuid.UUID(BigEndianUUID(512))
	overlap.Entry.Name = "pasture-overlap/default"

	_, err = tr.RegisterFixedSoftwareAgent(overlap)
	if !errors.Is(err, ErrNamespaceRange) {
		t.Fatalf("overlap error = %v, want errors.Is(ErrNamespaceRange)", err)
	}
	message := err.Error()
	for _, text := range []string{"pasture-system", "pasture-overlap"} {
		if !strings.Contains(message, text) {
			t.Errorf("overlap error %q is missing namespace %q", message, text)
		}
	}
	for _, marker := range []string{"what:", "why:", "where:", "when:", "impact:", "fix:"} {
		if count := strings.Count(message, marker); count != 1 {
			t.Errorf("overlap error has %d %q sections, want one authoritative section: %q", count, marker, message)
		}
	}
}

func TestRegisterFixedSoftwareAgentManifestConflictIsActionableOnce(t *testing.T) {
	t.Parallel()
	tr, err := OpenMemory(WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	reg := testFixedSoftwareAgentRegistration()
	if _, err := tr.RegisterFixedSoftwareAgent(reg); err != nil {
		t.Fatalf("seed activation: %v", err)
	}
	reg.Entry.Metadata = `{"manifest":"different"}`
	_, err = tr.RegisterFixedSoftwareAgent(reg)
	if !errors.Is(err, ErrNamespaceClaim) {
		t.Fatalf("manifest conflict = %v, want errors.Is(ErrNamespaceClaim)", err)
	}
	message := err.Error()
	for _, marker := range []string{
		"manifest identity", "why:", "where:", "manifest preflight",
		"when:", "before activation writes", "impact:", "fix:",
	} {
		if !strings.Contains(message, marker) {
			t.Errorf("manifest conflict %q is missing %q", message, marker)
		}
	}
	for _, marker := range []string{"why:", "where:", "when:", "impact:", "fix:"} {
		if count := strings.Count(message, marker); count != 1 {
			t.Errorf("manifest conflict has %d %q sections, want one authoritative section: %q", count, marker, message)
		}
	}
}

func TestRegisterFixedSoftwareAgentReplayRepairAndDrift(t *testing.T) {
	t.Parallel()
	tr, err := OpenMemory(WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	reg := testFixedSoftwareAgentRegistration()

	if err := tr.Journal().RegisterNamespaceClaim(reg.Claim); err != nil {
		t.Fatalf("seed exact claim: %v", err)
	}
	first, err := tr.RegisterFixedSoftwareAgent(reg)
	if err != nil {
		t.Fatalf("repair claim-only state: %v", err)
	}
	second, err := tr.RegisterFixedSoftwareAgent(reg)
	if err != nil || second != first {
		t.Fatalf("exact replay = (%+v, %v), want (%+v, nil)", second, err, first)
	}
	got, err := tr.SoftwareAgent(reg.Entry.ActorID)
	if err != nil || got != first {
		t.Fatalf("software-agent FK target = (%+v, %v), want (%+v, nil)", got, err, first)
	}

	drift := reg
	drift.AgentName = "different"
	if _, err := tr.RegisterFixedSoftwareAgent(drift); !errors.Is(err, ErrAgentAlreadyExists) {
		t.Fatalf("agent drift error = %v, want ErrAgentAlreadyExists", err)
	}
	drift = reg
	drift.Entry.Metadata = `{"manifest":"v2"}`
	if _, err := tr.RegisterFixedSoftwareAgent(drift); !errors.Is(err, ErrNamespaceClaim) {
		t.Fatalf("manifest drift error = %v, want ErrNamespaceClaim", err)
	}
	drift = reg
	drift.Claim.ClaimantID = "different"
	if _, err := tr.RegisterFixedSoftwareAgent(drift); !errors.Is(err, ErrNamespaceClaim) {
		t.Fatalf("claim drift error = %v, want ErrNamespaceClaim", err)
	}
}

func TestRegisterFixedSoftwareAgentRejectsPreClaimActor(t *testing.T) {
	t.Parallel()
	tr, path := newFixedAgentSQLTracker(t)
	defer tr.Close()
	reg := testFixedSoftwareAgentRegistration()
	var err error
	withRawSQLiteTestConn(t, path, func(conn *rawSQLiteConn) {
		err = rawExecute(conn, `INSERT INTO agents (id, kind_id) VALUES (?1, ?2)`,
			&rawExecOptions{Args: []any{reg.Entry.ActorID.String(), int(AgentKindSoftware)}})
		if err == nil {
			err = rawExecute(conn,
				`INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1, ?2, ?3, ?4)`,
				&rawExecOptions{Args: []any{reg.Entry.ActorID.String(), reg.AgentName, reg.Version, reg.Source}})
		}
	})
	if err != nil {
		t.Fatalf("seed pre-claim actor: %v", err)
	}
	if _, err := tr.RegisterFixedSoftwareAgent(reg); !errors.Is(err, ErrAgentAlreadyExists) {
		t.Fatalf("pre-claim actor error = %v, want ErrAgentAlreadyExists", err)
	}
	assertFixedAgentRowCounts(t, path, [4]int{0, 1, 1, 0})
}

func TestRegisterSoftwareAgentRandomIDPathUnchanged(t *testing.T) {
	t.Parallel()
	tr, err := OpenMemory(WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	agent, err := tr.RegisterSoftwareAgent("unclaimed", "ordinary", "1", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	if agent.ID.Namespace != "unclaimed" || agent.ID.UUID.Version() != 7 {
		t.Fatalf("random registration ID = %v, want unclaimed namespace and UUIDv7", agent.ID)
	}
}

func TestRegisterFixedSoftwareAgentRollsBackEveryInsertBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range loadFixedAgentFixture(t).RollbackCases {
		t.Run(tc.Name, func(t *testing.T) {
			tr, path := newFixedAgentSQLTracker(t)
			defer tr.Close()
			cleanup := installFixedAgentAbortTrigger(t, path, tc.Boundary)
			defer cleanup()
			if _, err := tr.RegisterFixedSoftwareAgent(testFixedSoftwareAgentRegistration()); err == nil {
				t.Fatal("RegisterFixedSoftwareAgent succeeded with an injected insert failure")
			}
			assertFixedAgentRowCounts(t, path, [4]int{})
		})
	}
}

func installFixedAgentAbortTrigger(t *testing.T, path string, boundary string) func() {
	t.Helper()
	var createStmt, dropStmt string
	switch boundary {
	case "claim":
		createStmt = `CREATE TRIGGER fail_fixed_claim BEFORE INSERT ON actor_namespace_claims BEGIN SELECT RAISE(ABORT, 'injected claim failure'); END`
		dropStmt = `DROP TRIGGER IF EXISTS fail_fixed_claim`
	case "agent":
		createStmt = `CREATE TRIGGER fail_fixed_agent BEFORE INSERT ON agents BEGIN SELECT RAISE(ABORT, 'injected agent failure'); END`
		dropStmt = `DROP TRIGGER IF EXISTS fail_fixed_agent`
	case "software":
		createStmt = `CREATE TRIGGER fail_fixed_software BEFORE INSERT ON agents_software BEGIN SELECT RAISE(ABORT, 'injected software failure'); END`
		dropStmt = `DROP TRIGGER IF EXISTS fail_fixed_software`
	case "manifest":
		createStmt = `CREATE TRIGGER fail_fixed_manifest BEFORE INSERT ON fixed_actor_manifest_entries BEGIN SELECT RAISE(ABORT, 'injected manifest failure'); END`
		dropStmt = `DROP TRIGGER IF EXISTS fail_fixed_manifest`
	default:
		t.Fatalf("unknown rollback boundary %q", boundary)
	}
	withRawSQLiteTestConn(t, path, func(conn *rawSQLiteConn) {
		if err := rawExecuteTransient(conn, createStmt, nil); err != nil {
			t.Fatalf("install %s failure trigger: %v", boundary, err)
		}
	})
	return func() {
		withRawSQLiteTestConn(t, path, func(conn *rawSQLiteConn) {
			if err := rawExecuteTransient(conn, dropStmt, nil); err != nil {
				t.Errorf("remove %s failure trigger before tracker close: %v", boundary, err)
			}
		})
	}
}

func assertFixedAgentRowCounts(t *testing.T, path string, want [4]int) {
	t.Helper()
	tables := []struct {
		name  string
		query string
	}{
		{"actor_namespace_claims", `SELECT COUNT(*) FROM actor_namespace_claims`},
		{"agents", `SELECT COUNT(*) FROM agents`},
		{"agents_software", `SELECT COUNT(*) FROM agents_software`},
		{"fixed_actor_manifest_entries", `SELECT COUNT(*) FROM fixed_actor_manifest_entries`},
	}
	withRawSQLiteTestConn(t, path, func(conn *rawSQLiteConn) {
		for i, table := range tables {
			var got int
			if err := rawExecute(conn, table.query, &rawExecOptions{ResultFunc: func(stmt *rawSQLiteStmt) error {
				got = stmt.ColumnInt(0)
				return nil
			}}); err != nil {
				t.Fatalf("count %s: %v", table.name, err)
			}
			if got != want[i] {
				t.Errorf("%s rows = %d, want %d", table.name, got, want[i])
			}
		}
	})
}

func TestRegisterFixedSoftwareAgentConcurrentStartup(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fixed-agent.db")
	const writers = 8
	trackers := make([]Tracker, writers)
	for i := range trackers {
		tr, err := OpenSQLite(path, WithModelRegistry(NewRegistry(nil)))
		if err != nil {
			t.Fatalf("open writer %d: %v", i, err)
		}
		trackers[i] = tr
		defer tr.Close()
	}

	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for _, tr := range trackers {
		wg.Add(1)
		go func(tr Tracker) {
			defer wg.Done()
			<-start
			_, err := tr.RegisterFixedSoftwareAgent(testFixedSoftwareAgentRegistration())
			errs <- err
		}(tr)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent registration: %v", err)
		}
	}
	assertFixedAgentRowCounts(t, path, [4]int{1, 1, 1, 1})
}
