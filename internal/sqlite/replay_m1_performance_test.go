package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// resetM1Copy stales the tasks generation without changing any projection value.
// It is called only on an explicit disposable copy, never on the live store.
func resetM1Copy(scope *connScope) (int, error) {
	var before, after int64
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT generation FROM provenance_table_generation WHERE table_name='tasks'").Scan(&before); err != nil {
		return 0, err
	}
	result, err := scope.conn.ExecContext(scope.ctx, "UPDATE tasks SET title=title WHERE id=(SELECT id FROM tasks ORDER BY id LIMIT 1)")
	if err != nil {
		return 0, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return 0, fmt.Errorf("reset affected %d rows: %v", affected, err)
	}
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT generation FROM provenance_table_generation WHERE table_name='tasks'").Scan(&after); err != nil {
		return 0, err
	}
	if after <= before {
		return 0, fmt.Errorf("reset did not advance tasks generation: before=%d after=%d", before, after)
	}
	var rows int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT count(*) FROM journal").Scan(&rows); err != nil {
		return 0, err
	}
	return rows, nil
}

func measureM1Fold(path string, oracle bool, reportCount func(int)) (replayObservation, error) {
	db, err := sql.Open(sqliteDriverName, "file:"+path+"?mode=rw")
	if err != nil {
		return replayObservation{}, fmt.Errorf("open disposable copy: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		return replayObservation{}, fmt.Errorf("lease disposable copy connection: %w", err)
	}
	defer conn.Close()
	scope := borrowConnScope(conn, projectionTargetLive)
	defer scope.release()
	rows, err := resetM1Copy(scope)
	if err != nil {
		return replayObservation{}, fmt.Errorf("reset disposable copy: %w", err)
	}
	if reportCount != nil {
		reportCount(rows)
	}
	start := time.Now()
	var folded int
	if oracle {
		_, _, _, _, folded, err = scope.oracleRederiveProjectionsShadow()
	} else {
		_, _, _, _, folded, err = scope.rederiveProjectionsShadow()
	}
	duration := time.Since(start)
	if err != nil {
		return replayObservation{}, fmt.Errorf("scoped fold after reset: %w", err)
	}
	if folded != rows || duration <= 0 {
		return replayObservation{}, fmt.Errorf("folded %d rows in %s, want all %d journal rows", folded, duration, rows)
	}
	caller := "scoped_fold"
	if oracle {
		caller = "frozen_oracle"
	}
	return replayObservation{Path: "full", Caller: caller, Folded: folded, Duration: duration}, nil
}

func TestReplayM1ObserverAndReset(t *testing.T) {
	fixture := buildPrefixFixture(t)
	var rows int
	for repetition := 0; repetition < 3; repetition++ {
		event, err := measureM1Fold(fixture.path, false, nil)
		if err != nil {
			t.Fatalf("repetition %d: %v", repetition+1, err)
		}
		if event.Path != "full" || event.Folded == 0 || event.Caller != "scoped_fold" {
			t.Fatalf("invalid observation: %+v", event)
		}
		if repetition > 0 && event.Folded != rows {
			t.Fatalf("reset changed journal row count: first=%d repetition=%d", rows, event.Folded)
		}
		rows = event.Folded
	}
	oracle, err := measureM1Fold(fixture.path, true, nil)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if oracle.Caller != "frozen_oracle" || oracle.Folded != rows {
		t.Fatalf("invalid oracle observation: %+v, want %d rows", oracle, rows)
	}
}

// TestReplayM1Performance is intentionally opt-in. The accompanying script
// verifies free space, creates exactly one disposable live-store copy and
// deletes that copy and its sidecars on exit.
func TestReplayM1Performance(t *testing.T) {
	path := os.Getenv("PROVENANCE_M1_COPY")
	if path == "" {
		t.Skip("run scripts/measure-replay-m1.sh to measure a disposable copy")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != "/tmp/opencode/m1-measure.db" {
		t.Fatalf("refusing non-disposable measurement path %q resolved=%q err=%v", path, resolved, err)
	}
	durations := make([]time.Duration, 0, 3)
	var journalRows int
	for i := 0; i < 3; i++ {
		event, err := measureM1Fold(resolved, false, func(rows int) {
			t.Logf("M1 repetition=%d journal_rows=%d", i+1, rows)
		})
		if err != nil {
			t.Fatalf("repetition %d: %v", i+1, err)
		}
		if i > 0 && event.Folded != journalRows {
			t.Fatalf("journal count changed: first=%d repetition=%d", journalRows, event.Folded)
		}
		journalRows = event.Folded
		t.Logf("M1 repetition=%d path=%s caller=%s folded=%d duration=%s", i+1, event.Path, event.Caller, event.Folded, event.Duration)
		durations = append(durations, event.Duration)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	t.Logf("M1 median=%s max=%s", durations[1], durations[2])
	oracle, err := measureM1Fold(resolved, true, func(rows int) {
		t.Logf("M1 oracle journal_rows=%d", rows)
	})
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if oracle.Folded != journalRows {
		t.Fatalf("oracle folded %d rows, earlier repetitions folded %d", oracle.Folded, journalRows)
	}
	t.Logf("M1 oracle folded=%d duration=%s speedup=%.3fx", oracle.Folded, oracle.Duration, float64(oracle.Duration)/float64(durations[1]))
	if durations[1] >= 2*time.Minute {
		t.Errorf("M1 median %s is not under 2 minutes", durations[1])
	}
}
