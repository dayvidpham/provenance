# Context-aware software-agent registration

All built-in trackers implement the optional `SoftwareAgentRegistrarContext`
capability without adding a method to `Tracker`:

```go
registrar, ok := tracker.(provenance.SoftwareAgentRegistrarContext)
if !ok {
    // Report unsupported registration; do not silently drop the caller's context.
    return errors.New("tracker lacks context-aware software-agent registration")
}
agent, err := registrar.RegisterSoftwareAgentContext(ctx, namespace, name, version, source)
```

The context reaches borrowed-pool liveness checking, connection acquisition,
`BEGIN IMMEDIATE`, both agent inserts, and commit. Registration makes one lock
attempt under the connection's busy tier, never a private retry loop. Cancelled
waiters preserve `errors.Is(err, context.Canceled)` or `DeadlineExceeded` and
custom `context.Cause`. A caller-cancelled borrowed precheck is not classified as
`StoreUnavailableError`; an unavailable live-context store still is.
An inner tracker missing the optional capability also returns
`StoreUnavailableError`, with `Stage` set to `capability check before registration`
and repair guidance naming the required interface. Legacy entry points keep
their operation labels; shared lower-level registration diagnostics name the
context-aware implementation.

The transaction is all-or-nothing and releases its lease. No invocation context
is retained by the tracker. Connection creation/settings remain the pool owner's
policy, so callers requiring first-use bounds must provide a context-aware
connector. The existing `RegisterSoftwareAgent` delegates with Background.

Registration allocates a fresh UUIDv7 on each successful invocation. It is not
an idempotent well-known-agent registry; consumers still own their existing
canonical-name lookup/cache protocol.
