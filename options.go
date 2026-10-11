package provenance

import (
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// Option configures a Tracker at creation time.
type Option func(*options)

type options struct {
	registry                         ptypes.ModelRegistry
	borrowedActivationBusyTimeout    time.Duration
	borrowedActivationBusyTimeoutSet bool
	activationObserver               func(string)
}

// WithBorrowedActivationBusyTimeout sets the borrowed schema activation tier
// only. It does not change ordinary leases, standalone opens, or the 30s retry
// ceiling. OpenBorrowedSQLiteContext rejects values below one millisecond.
func WithBorrowedActivationBusyTimeout(timeout time.Duration) Option {
	return func(o *options) {
		o.borrowedActivationBusyTimeout = timeout
		o.borrowedActivationBusyTimeoutSet = true
	}
}

func defaultOptions() options {
	return options{
		registry: DefaultModelRegistry(),
	}
}

// WithModelRegistry overrides the default model registry used to seed
// the ml_models table and validate model names at registration time.
// A nil registry is ignored (the default is preserved).
func WithModelRegistry(r ptypes.ModelRegistry) Option {
	return func(o *options) {
		if r == nil {
			return
		}
		o.registry = r
	}
}
