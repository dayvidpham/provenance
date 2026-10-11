//go:build provenance_testhooks

package provenance_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/dayvidpham/provenance"
)

func TestBorrowedActivationPublicCancelDuringContendedAttempt(t *testing.T) {
	db, path := openFileDB(t)
	seed, err := provenance.OpenBorrowedSQLite(db, provenance.WithModelRegistry(provenance.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()
	holder, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	// A completed BEGIN IMMEDIATE proves file-lock ownership before launch.
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("host cancelled borrowed activation")
	secondAttempt := make(chan struct{})
	type outcome struct {
		tracker  provenance.Tracker
		err      error
		attempts int
	}
	done := make(chan outcome, 1)
	released, joined := false, false
	release := func() {
		if !released {
			released = true
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}
	t.Cleanup(func() {
		cancel(cause)
		release()
		if !joined {
			select {
			case result := <-done:
				if result.tracker != nil {
					_ = result.tracker.Close()
				}
			case <-time.After(2 * time.Second):
				t.Error("activation worker did not join")
			}
		}
	})
	// Cleanup (cancel/release/join) is installed before launching the opener.
	go func() {
		attempts, contended := 0, false
		observer := func(phase string) {
			if phase == "contended" {
				contended = true
			}
			if phase == "attempt" {
				attempts++
				if attempts == 2 && contended {
					close(secondAttempt)
				}
			}
		}
		tracker, err := provenance.OpenBorrowedSQLiteContext(ctx, db,
			provenance.WithModelRegistry(provenance.NewRegistry(nil)),
			provenance.WithBorrowedActivationBusyTimeout(500*time.Millisecond),
			provenance.WithBorrowedActivationObserverForTest(observer))
		done <- outcome{tracker, err, attempts}
	}()
	select {
	case <-secondAttempt:
	case result := <-done:
		joined = true
		t.Fatalf("opener stopped before observed contention: %v", result.err)
	case <-time.After(2 * time.Second):
		t.Fatal("no observed contended second attempt")
	}
	// This timer is the cancellation stimulus, NOT lock-readiness evidence.
	// The independent holder remains locked through worker completion.
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	started := time.Now()
	cancel(cause)
	select {
	case result := <-done:
		joined = true
		if result.tracker != nil || !errors.Is(result.err, cause) || result.attempts != 2 {
			t.Fatalf("tracker=%v attempts=%d err=%v", result.tracker, result.attempts, result.err)
		}
		const schedulingTolerance = 30 * time.Millisecond
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond+schedulingTolerance {
			t.Fatalf("cancellation+cleanup+diagnostics+close took %s", elapsed)
		}
	case <-time.After(530 * time.Millisecond):
		t.Fatal("activation worker outlived one busy tier after cancellation with holder still locked")
	}
	release()
	if err := db.Ping(); err != nil {
		t.Fatalf("borrowed pool closed: %v", err)
	}
}
