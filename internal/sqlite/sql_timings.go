package sqlite

import (
	"context"
	"errors"
	"time"
)

type sqlTimingKey struct{}

type sqlTimingEvent struct {
	PID       int    `json:"pid"`
	Operation uint64 `json:"op"`
	Method    string `json:"method"`
	Phase     string `json:"phase"`
	StartNS   int64  `json:"start_ns"`
	EndNS     int64  `json:"end_ns"`
	Outcome   string `json:"outcome"`
}

// Each recorder belongs to one synchronous operation, including its scope.
// No recording performs I/O. Only flush, deferred before bindScope so it runs
// AFTER scope.release, serializes private artifacts across daemon goroutines.
type sqlTimingRecorder struct {
	path   string
	pid    int
	op     uint64
	method string
	origin time.Time
	events []sqlTimingEvent
}

type sqlTimingSpan struct {
	recorder *sqlTimingRecorder
	phase    string
	start    time.Time
	end      time.Time
}

// Hooks are nil in normal builds and assigned only during package init by the
// experiment-tagged file. Keeping one set of declarations also preserves the
// SQL architecture guard, which type-checks every source file regardless of
// build tags. There is no runtime registration or mutable configuration API.
var sqlTimingNew func(context.Context, string) (context.Context, *sqlTimingRecorder)
var sqlTimingWrite func(*sqlTimingRecorder)

func sqlTimingOperation(ctx context.Context, method string) (context.Context, *sqlTimingRecorder) {
	if sqlTimingNew == nil {
		return ctx, nil
	}
	return sqlTimingNew(ctx, method)
}

func (r *sqlTimingRecorder) flush() {
	if r != nil && sqlTimingWrite != nil {
		sqlTimingWrite(r)
	}
}

func sqlTimingStart(ctx context.Context, phase string) sqlTimingSpan {
	if sqlTimingNew == nil {
		return sqlTimingSpan{}
	}
	r, _ := ctx.Value(sqlTimingKey{}).(*sqlTimingRecorder)
	if r == nil {
		return sqlTimingSpan{}
	}
	if r.origin.IsZero() {
		r.origin = time.Now()
	}
	return sqlTimingSpan{recorder: r, phase: phase, start: time.Now()}
}

func (s sqlTimingSpan) finish(err error) {
	if s.recorder == nil {
		return
	}
	s = s.stop()
	outcome := "ok"
	switch {
	case err == nil:
	case isBusyError(err):
		outcome = "busy"
	case errors.Is(err, context.Canceled):
		outcome = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		outcome = "deadline"
	default:
		outcome = "error"
	}
	s.outcome(outcome)
}

func (s sqlTimingSpan) outcome(outcome string) {
	if s.recorder == nil {
		return
	}
	s = s.stop()
	r := s.recorder
	start := r.origin.UnixNano() + s.start.Sub(r.origin).Nanoseconds()
	end := r.origin.UnixNano() + s.end.Sub(r.origin).Nanoseconds()
	r.events = append(r.events, sqlTimingEvent{PID: r.pid, Operation: r.op, Method: r.method, Phase: s.phase, StartNS: start, EndNS: end, Outcome: outcome})
}

func (s sqlTimingSpan) stop() sqlTimingSpan {
	if s.recorder != nil && s.end.IsZero() {
		s.end = time.Now()
	}
	return s
}

func (s sqlTimingSpan) endedAt(boundary sqlTimingSpan) sqlTimingSpan {
	s.end = boundary.end
	return s
}

func (s sqlTimingSpan) next(phase string) sqlTimingSpan {
	return sqlTimingSpan{recorder: s.recorder, phase: phase, start: s.end}
}
