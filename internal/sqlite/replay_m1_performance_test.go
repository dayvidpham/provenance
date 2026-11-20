package sqlite

import (
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
func resetM1Copy(path string) (int, error) {
	db, err := sql.Open(sqliteDriverName, "file:"+path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var before, after int64
	if err := db.QueryRow("SELECT generation FROM provenance_table_generation WHERE table_name='tasks'").Scan(&before); err != nil {
		return 0, err
	}
	result, err := db.Exec("UPDATE tasks SET title=title WHERE id=(SELECT id FROM tasks ORDER BY id LIMIT 1)")
	if err != nil {
		return 0, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return 0, fmt.Errorf("reset affected %d rows: %v", affected, err)
	}
	if err := db.QueryRow("SELECT generation FROM provenance_table_generation WHERE table_name='tasks'").Scan(&after); err != nil {
		return 0, err
	}
	if after <= before {
		return 0, fmt.Errorf("reset did not advance tasks generation: before=%d after=%d", before, after)
	}
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM journal").Scan(&rows); err != nil {
		return 0, err
	}
	return rows, nil
}

func measureM1Open(path string) (replayObservation, error) {
	rows, err := resetM1Copy(path)
	if err != nil {
		return replayObservation{}, fmt.Errorf("reset disposable copy: %w", err)
	}
	var observations []replayObservation
	db, err := openWithReplayObserver(path, nil, func(event replayObservation) {
		observations = append(observations, event)
	})
	if err != nil {
		return replayObservation{}, fmt.Errorf("production Open after reset: %w", err)
	}
	if err := db.Close(); err != nil {
		return replayObservation{}, err
	}
	for _, event := range observations {
		if event.Path == "full" && event.Folded == rows && event.Duration > 0 &&
			(event.Caller == "read_only_preflight" || event.Caller == "convergence_probe" || event.Caller == "locked_activation") {
			return event, nil
		}
	}
	return replayObservation{}, fmt.Errorf("no full replay of all %d journal rows; observations: %+v", rows, observations)
}

func TestReplayM1ObserverAndReset(t *testing.T) {
	fixture := buildPrefixFixture(t)
	event, err := measureM1Open(fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	if event.Path != "full" || event.Folded == 0 || event.Caller == "" {
		t.Fatalf("invalid observation: %+v", event)
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
	for i := 0; i < 3; i++ {
		event, err := measureM1Open(resolved)
		if err != nil {
			t.Fatalf("repetition %d: %v", i+1, err)
		}
		t.Logf("M1 repetition=%d path=%s caller=%s folded=%d duration=%s", i+1, event.Path, event.Caller, event.Folded, event.Duration)
		durations = append(durations, event.Duration)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	t.Logf("M1 median=%s max=%s", durations[1], durations[2])
	if durations[1] >= 2*time.Minute {
		t.Errorf("M1 median %s is not under 2 minutes", durations[1])
	}
}
