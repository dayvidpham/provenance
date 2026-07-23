package provenance

// dbos_config_test.go verifies DBOSStepOptions validation: zero defaults, partial
// nonzero overrides, valid boundaries, and typed invalid-boundary diagnostics.
// Per Proposal 54 BDD criterion 1 and 2.

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
)

func TestResolveDBOSStepOptions(t *testing.T) {
	valid := []struct {
		name string
		in   DBOSStepOptions
		want resolvedDBOSStepOptions
	}{
		{"defaults", DBOSStepOptions{}, resolvedDBOSStepOptions{3, 50 * time.Millisecond, 2}},
		{"MaxRetries override", DBOSStepOptions{MaxRetries: 5}, resolvedDBOSStepOptions{5, 50 * time.Millisecond, 2}},
		{"BaseInterval override", DBOSStepOptions{BaseInterval: 100 * time.Millisecond}, resolvedDBOSStepOptions{3, 100 * time.Millisecond, 2}},
		{"BackoffFactor override", DBOSStepOptions{BackoffFactor: 1.5}, resolvedDBOSStepOptions{3, 50 * time.Millisecond, 1.5}},
		{"all overrides", DBOSStepOptions{MaxRetries: 6, BaseInterval: time.Second, BackoffFactor: 2.5}, resolvedDBOSStepOptions{6, time.Second, 2.5}},
		{"MaxRetries lower boundary", DBOSStepOptions{MaxRetries: 1}, resolvedDBOSStepOptions{1, 50 * time.Millisecond, 2}},
		{"MaxRetries upper boundary", DBOSStepOptions{MaxRetries: 12}, resolvedDBOSStepOptions{12, 50 * time.Millisecond, 2}},
		{"BaseInterval lower boundary", DBOSStepOptions{BaseInterval: time.Nanosecond}, resolvedDBOSStepOptions{3, time.Nanosecond, 2}},
		{"BaseInterval upper boundary", DBOSStepOptions{BaseInterval: 5 * time.Second}, resolvedDBOSStepOptions{3, 5 * time.Second, 2}},
		{"BackoffFactor lower boundary", DBOSStepOptions{BackoffFactor: 1}, resolvedDBOSStepOptions{3, 50 * time.Millisecond, 1}},
		{"BackoffFactor proportional value", DBOSStepOptions{BackoffFactor: 3}, resolvedDBOSStepOptions{3, 50 * time.Millisecond, 3}},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveDBOSStepOptions(test.in)
			if err != nil || got != test.want {
				t.Fatalf("resolve=%+v err=%v, want %+v", got, err, test.want)
			}
			if len(dbosStepOptions("step", got)) != 4 {
				t.Fatal("sole SDK translation must contain StepName and exactly three retry options")
			}
		})
	}

	invalid := []struct {
		name  string
		in    DBOSStepOptions
		field DBOSDiagnosticField
	}{
		{"MaxRetries negative", DBOSStepOptions{MaxRetries: -1}, DBOSDiagFieldMaxRetries},
		{"MaxRetries above boundary", DBOSStepOptions{MaxRetries: 13}, DBOSDiagFieldMaxRetries},
		{"MaxRetries far above boundary", DBOSStepOptions{MaxRetries: 100}, DBOSDiagFieldMaxRetries},
		{"BaseInterval negative", DBOSStepOptions{BaseInterval: -time.Millisecond}, DBOSDiagFieldBaseInterval},
		{"BaseInterval above boundary", DBOSStepOptions{BaseInterval: 5*time.Second + time.Nanosecond}, DBOSDiagFieldBaseInterval},
		{"BackoffFactor below boundary", DBOSStepOptions{BackoffFactor: 0.5}, DBOSDiagFieldBackoffFactor},
		{"BackoffFactor negative", DBOSStepOptions{BackoffFactor: -1}, DBOSDiagFieldBackoffFactor},
		{"BackoffFactor NaN", DBOSStepOptions{BackoffFactor: math.NaN()}, DBOSDiagFieldBackoffFactor},
		{"BackoffFactor positive infinity", DBOSStepOptions{BackoffFactor: math.Inf(1)}, DBOSDiagFieldBackoffFactor},
		{"BackoffFactor negative infinity", DBOSStepOptions{BackoffFactor: math.Inf(-1)}, DBOSDiagFieldBackoffFactor},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			_, err := resolveDBOSStepOptions(test.in)
			var cfgErr *DBOSConfigError
			if !errors.As(err, &cfgErr) {
				t.Fatalf("error type %T, want *DBOSConfigError", err)
			}
			if cfgErr.Class != DBOSDiagClassConfig || cfgErr.Field != test.field || cfgErr.Stage != DBOSDiagStageAdapterConstruction || cfgErr.Value == "" || cfgErr.Reason == "" || cfgErr.Impact == "" || cfgErr.Fix == "" {
				t.Fatalf("diagnostic=%+v", cfgErr)
			}
		})
	}
}

func TestResolveDBOSResultPollingInterval(t *testing.T) {
	for _, test := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"default", 0, 50 * time.Millisecond},
		{"lower boundary", 10 * time.Millisecond, 10 * time.Millisecond},
		{"measured 50 ms", 50 * time.Millisecond, 50 * time.Millisecond},
		{"measured 200 ms", 200 * time.Millisecond, 200 * time.Millisecond},
		{"upper boundary", 5 * time.Second, 5 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveDBOSResultPollingInterval(test.in)
			if err != nil || got != test.want {
				t.Fatalf("resolve=%v err=%v, want %v", got, err, test.want)
			}
		})
	}

	for _, interval := range []time.Duration{-time.Millisecond, time.Millisecond, 5*time.Second + time.Nanosecond} {
		_, err := resolveDBOSResultPollingInterval(interval)
		var cfgErr *DBOSConfigError
		if !errors.As(err, &cfgErr) || cfgErr.Field != DBOSDiagFieldResultPollingInterval || cfgErr.Stage != DBOSDiagStageAdapterConstruction || cfgErr.Value == "" || cfgErr.Reason == "" || cfgErr.Impact == "" || cfgErr.Fix == "" {
			t.Fatalf("interval %v diagnostic=%#v err=%v", interval, cfgErr, err)
		}
	}
}

func TestNewDBOSAdapterRejectsInvalidConfigBeforeRegistration(t *testing.T) {
	db, err := openSharedSQL(t.TempDir() + "/config.db")
	if err != nil {
		t.Fatal(err)
	}
	root, err := dbos.NewDBOSContext(context.Background(), dbos.Config{AppName: "config-integration", SqliteSystemDB: db, ApplicationVersion: "config-integration"})
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Shutdown(time.Second); _ = tracker.Close(); _ = db.Close() })

	_, err = NewDBOSAdapter(root, tracker, DBOSAdapterConfig{StepOptions: DBOSStepOptions{MaxRetries: 13}})
	var cfgErr *DBOSConfigError
	if !errors.As(err, &cfgErr) || cfgErr.Field != DBOSDiagFieldMaxRetries {
		t.Fatalf("invalid adapter config error=%#v, want typed MaxRetries diagnostic", err)
	}
	_, err = NewDBOSAdapter(root, tracker, DBOSAdapterConfig{ResultPollingInterval: time.Millisecond})
	if !errors.As(err, &cfgErr) || cfgErr.Field != DBOSDiagFieldResultPollingInterval {
		t.Fatalf("invalid adapter config error=%#v, want typed ResultPollingInterval diagnostic", err)
	}
	adapter, err := NewDBOSAdapter(root, tracker, DBOSAdapterConfig{StepOptions: DBOSStepOptions{MaxRetries: 1, BaseInterval: time.Millisecond, BackoffFactor: 1}, ResultPollingInterval: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("valid registration after rejected config: %v", err)
	}
	if adapter.stepOptions != (resolvedDBOSStepOptions{1, time.Millisecond, 1}) {
		t.Fatalf("registered step options=%+v", adapter.stepOptions)
	}
	if adapter.resultPollingInterval != 200*time.Millisecond {
		t.Fatalf("registered result polling interval=%v", adapter.resultPollingInterval)
	}
}
