//go:build provenance_testhooks

package provenance

// WithBorrowedActivationObserverForTest observes this invocation's activation
// phases synchronously. It is available only in explicitly tagged test builds;
// normal production builds have no exported observer option.
func WithBorrowedActivationObserverForTest(observer func(string)) Option {
	return func(o *options) { o.activationObserver = observer }
}
