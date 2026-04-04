package provenance

import "github.com/dayvidpham/provenance/pkg/ptypes"

// Option configures a Tracker at creation time.
type Option func(*options)

type options struct {
	registry ptypes.ModelRegistry
}

func defaultOptions() options {
	return options{
		registry: DefaultModelRegistry(),
	}
}

// WithModelRegistry overrides the default model registry used to seed
// the ml_models table. Pass a bestiary.Registry() here once available.
func WithModelRegistry(r ptypes.ModelRegistry) Option {
	return func(o *options) {
		o.registry = r
	}
}
