package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/testcorpus"
)

//go:embed testdata/activation_context.yaml
var activationContextCases []byte

type activationCaseName string
type activationPhase string
type activationContextCase struct {
	Name   activationCaseName `yaml:"name"`
	Phase  activationPhase    `yaml:"phase"`
	TierMS int                `yaml:"tier_ms"`
	Holder bool               `yaml:"holder"`
}
type activationContextTable struct {
	Cases []activationContextCase `yaml:"cases"`
}

func loadActivationCases(data []byte) (activationContextTable, error) {
	table, err := testcorpus.LoadYAML[activationContextTable](data)
	if err != nil {
		return table, err
	}
	want := map[activationCaseName]activationPhase{
		"cancel-after-contention": "contended", "cancel-backoff": "backoff",
		"cancel-integrity": "integrity", "cancel-replay": "replay", "cancel-tiny-tier": "contended",
	}
	seen := make(map[activationCaseName]activationPhase)
	for _, c := range table.Cases {
		phase, ok := want[c.Name]
		if !ok || seen[c.Name] != "" || c.Phase != phase || c.TierMS < 1 || c.Holder != (phase == "contended" || phase == "backoff") {
			return table, fmt.Errorf("invalid activation case %q", c.Name)
		}
		seen[c.Name] = c.Phase
	}
	if !reflect.DeepEqual(seen, want) {
		return table, errors.New("activation case membership mismatch")
	}
	return table, nil
}

// These tests stay serial: contention completion is bounded by a small named
// scheduling tolerance, not by the suite's global failure timeout.
const activationSchedulingTolerance = 30 * time.Millisecond

func activationContextPool(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := t.TempDir() + "/context.db"
	pool, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if _, err := pool.Exec("PRAGMA busy_timeout=4321"); err != nil {
		t.Fatal(err)
	}
	return pool, path
}

func assertActivationPoolRestored(t *testing.T, pool *sql.DB) {
	t.Helper()
	if err := pool.Ping(); err != nil {
		t.Fatal(err)
	}
	var busy, fk int
	if err := pool.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if busy != 4321 || fk != 0 {
		t.Fatalf("returned connection PRAGMAs busy=%d fk=%d", busy, fk)
	}
}

func activationConnectionIdentity(t *testing.T, pool *sql.DB) any {
	t.Helper()
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var identity any
	if err := conn.Raw(func(raw any) error { identity = raw; return nil }); err != nil {
		t.Fatal(err)
	}
	return identity
}

// activationSnapshot captures the store's schema and every table's content, so a
// test can assert that an activation attempt (or a reopen) changed nothing that
// was already committed. The verified-as-of ledger's two tables are excluded:
// they are the open path's own verification bookkeeping (open_verified.go), not
// schema, history, or projection, and a slow-path open legitimately re-persists
// the ledger row after it re-verifies the store.
func activationSnapshot(t *testing.T, pool *sql.DB) []string {
	t.Helper()
	rows, err := pool.Query("SELECT name, sql FROM sqlite_schema WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var names, snapshot []string
	for rows.Next() {
		var name string
		var ddl sql.NullString
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatal(err)
		}
		if name == storeGenerationTable || name == verifiedStoreTable {
			continue
		}
		names = append(names, name)
		snapshot = append(snapshot, name+":"+ddl.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	for _, name := range names {
		rows, err := pool.Query(`SELECT * FROM "` + strings.ReplaceAll(name, `"`, `""`) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var content []string
		for rows.Next() {
			values := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			content = append(content, fmt.Sprintf("%#v", values))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		sort.Strings(content)
		snapshot = append(snapshot, name+fmt.Sprint(content))
	}
	return snapshot
}

func TestBorrowedActivationCancellationPhases(t *testing.T) {
	table, err := loadActivationCases(activationContextCases)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range table.Cases {
		t.Run(string(c.Name), func(t *testing.T) {
			pool, path := activationContextPool(t)
			// A prior journal and projection exercise rollback without vacuous
			// empty-history replay. The incomplete activation must not alter them.
			seed, err := OpenBorrowed(pool, nil)
			if err != nil {
				t.Fatal(err)
			}
			exerciseBorrowedStore(t, seed, string(c.Name))
			_ = seed.Close()
			var beforeJournal, beforeActors int
			if err := pool.QueryRow("SELECT count(*) FROM journal").Scan(&beforeJournal); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow("SELECT count(*) FROM agents").Scan(&beforeActors); err != nil {
				t.Fatal(err)
			}
			if beforeJournal == 0 || beforeActors == 0 {
				t.Fatal("vacuous baseline")
			}
			release := func() {}
			if c.Holder {
				release = holdWriteLock(t, path)
			}
			before := activationSnapshot(t, pool)
			beforeConnection := activationConnectionIdentity(t, pool)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			cause := errors.New("activation caller stopped")
			attempts, atCancel := 0, 0
			var cancelledAt time.Time
			config := BorrowedActivationConfig{Path: path, BusyTimeout: time.Duration(c.TierMS) * time.Millisecond, Observer: func(phase string) {
				if phase == "attempt" {
					attempts++
				}
				if phase == string(c.Phase) && cancelledAt.IsZero() {
					cancelledAt = time.Now()
					atCancel = attempts
					cancel(cause)
				}
			}}
			store, err := OpenBorrowedContext(ctx, pool, nil, config)
			if store != nil || !errors.Is(err, cause) {
				t.Fatalf("store=%v err=%v", store, err)
			}
			if cancelledAt.IsZero() || attempts != atCancel {
				t.Fatalf("cancellation phase not reached or later attempt: %d/%d", attempts, atCancel)
			}
			if elapsed := time.Since(cancelledAt); elapsed > config.BusyTimeout+activationSchedulingTolerance {
				t.Fatalf("cleanup after cancellation %s > %s + %s", elapsed, config.BusyTimeout, activationSchedulingTolerance)
			}
			if afterConnection := activationConnectionIdentity(t, pool); afterConnection != beforeConnection {
				if !strings.Contains(err.Error(), "retired") {
					t.Fatalf("unreported connection discard: %v", err)
				}
				// A tiny finite cleanup budget may require retirement rather
				// than restoration. The old connection must not survive.
				if err := pool.Ping(); err != nil {
					t.Fatal(err)
				}
			} else {
				assertActivationPoolRestored(t, pool)
			}
			var afterJournal, afterActors int
			if err := pool.QueryRow("SELECT count(*) FROM journal").Scan(&afterJournal); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow("SELECT count(*) FROM agents").Scan(&afterActors); err != nil {
				t.Fatal(err)
			}
			if beforeJournal != afterJournal || beforeActors != afterActors {
				t.Fatal("incomplete activation altered prior history/projection")
			}
			if after := activationSnapshot(t, pool); !reflect.DeepEqual(before, after) {
				t.Fatal("activation changed schema, seed, journal or projection")
			}
			release()
			reopened, err := OpenBorrowed(pool, nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = reopened.Close()
			if after := activationSnapshot(t, pool); !reflect.DeepEqual(before, after) {
				t.Fatal("reopen changed prior schema/history/projection")
			}
		})
	}
}

func TestBorrowedActivationShortDeadlineUnderRealLock(t *testing.T) {
	pool, path := activationContextPool(t)
	seed, err := OpenBorrowed(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()
	holdWriteLock(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	attempts := 0
	started := time.Now()
	store, err := OpenBorrowedContext(ctx, pool, nil, BorrowedActivationConfig{Path: path, BusyTimeout: 500 * time.Millisecond, Observer: func(phase string) {
		if phase == "attempt" {
			attempts++
		}
	}})
	if store != nil || !errors.Is(err, context.DeadlineExceeded) || attempts == 0 {
		t.Fatalf("store=%v attempts=%d err=%v", store, attempts, err)
	}
	if elapsed := time.Since(started); elapsed > 25*time.Millisecond+activationSchedulingTolerance {
		t.Fatalf("short deadline took %s", elapsed)
	}
	assertActivationPoolRestored(t, pool)
}

func TestBorrowedActivationCancelledPoolWait(t *testing.T) {
	pool, _ := activationContextPool(t)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	store, err := OpenBorrowedContext(ctx, pool, nil, BorrowedActivationConfig{BusyTimeout: 500 * time.Millisecond})
	if store != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("store=%v err=%v", store, err)
	}
	if elapsed := time.Since(started); elapsed > 25*time.Millisecond+activationSchedulingTolerance {
		t.Fatalf("pool cancellation took %s", elapsed)
	}
}

func TestBorrowedActivationDefaultPolicy(t *testing.T) {
	p := defaultActivationRetryPolicy()
	if p.attemptBusyTimeout != 5*time.Second || p.ceiling != 30*time.Second {
		t.Fatalf("compatibility policy changed: %+v", p)
	}
}

func TestActivationCaseLoaderRejectsBrokenTables(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("cases: []\n"),
		[]byte("cases: []\nunknown: true\n"),
		[]byte("cases: []\n---\ncases: []\n"),
		append(append([]byte(nil), activationContextCases...), []byte("  - name: cancel-backoff\n    phase: backoff\n    tier_ms: 500\n    holder: true\n")...),
	} {
		if _, err := loadActivationCases(data); err == nil {
			t.Fatal("broken table accepted")
		}
	}
}
