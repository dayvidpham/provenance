package provenance

// dbos_store.go implements the borrowed-storage bridge that lets Provenance share
// one physical SQLite database with a DBOS durable-execution root (issue
// dayvidpham/provenance#6), plus the StoreUnavailableError lifecycle error.
//
// ARCHITECTURE DEVIATION (Option B; architect ruling for issue #6, flagged for UAT
// ratification):
//
// Issue #6 acceptance criterion #1 was written assuming Provenance persisted through
// database/sql, so the borrowed *sql.DB could be the single literal handle and
// migrations could run "through it". The delivered, UAT-accepted journal foundation
// (S1.0–S1.3) persists entirely on zombiezen.com/go/sqlite (*zs.Conn), which cannot
// be constructed from or share a connection object with a database/sql *sql.DB.
// Porting the whole persistence layer to database/sql would re-open every reviewed,
// accepted slice — an architectural reset no leaf may perform. The newer
// user-accepted foundation therefore governs, and the criterion's literal wording is
// amended to its INTENT: ONE shared physical database.
//
// OpenBorrowedSQLite validates the borrowed *sql.DB, derives its on-disk path via
// PRAGMA database_list ON THE BORROWED HANDLE, and opens ONE internal zombiezen
// connection on THAT SAME FILE (same WAL lineage, same busy_timeout). There is no
// second file and no second WAL; there is a second CONNECTION, but no second
// LIFECYCLE: the borrowed *sql.DB remains the sole lifecycle token. Every public
// operation first checks borrowed-handle liveness with a Ping sentinel; once the
// DBOS root that owns the handle shuts down (closing the pool), every Provenance
// read/write returns a StoreUnavailableError even though the zombiezen bridge
// connection is technically still open. Cleanup (Tracker.Close) closes ONLY the
// bridge connection, never the borrowed handle, and is repeat-safe. Provenance
// migrations apply via the bridge INTO the borrowed handle's database, so the
// caller observes the schema through its own handle. In-memory/temp/pathless
// borrowed databases are rejected (a same-file bridge is impossible there).

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StoreUnavailableError is returned by every borrowed-tracker read or write once
// the borrowed SQLite handle's owning DBOS root has shut down (or the caller has
// otherwise closed the handle). Its six fields are actionable and it is
// errors.As-discoverable with the underlying driver cause wrapped.
type StoreUnavailableError struct {
	Operation string // the Provenance operation that was attempted
	Store     string // which store was unavailable
	Stage     string // where in the operation lifecycle it failed
	Impact    string // what it means for the caller
	Fix       string // how to recover
	Cause     error  // wrapped driver/liveness cause
}

func (e *StoreUnavailableError) Error() string {
	return fmt.Sprintf(
		"provenance: store unavailable during %s — store: %s; stage: %s; impact: %s; fix: %s; cause: %v",
		e.Operation, e.Store, e.Stage, e.Impact, e.Fix, e.Cause)
}

func (e *StoreUnavailableError) Unwrap() error { return e.Cause }

// OpenBorrowedSQLite opens a Tracker that shares the borrowed *sql.DB's physical
// database via an internal zombiezen bridge connection (Option B; see the package
// deviation note above). The borrowed handle is never closed by Provenance and is
// the sole lifecycle token: after it (or its owning DBOS root) closes, every
// borrowed-tracker operation returns a StoreUnavailableError.
//
// It rejects a nil handle, a handle that cannot be pinged, and an in-memory, temp,
// or pathless database (a same-file bridge is impossible there), naming OpenMemory
// as the standalone alternative.
func OpenBorrowedSQLite(db *sql.DB, opts ...Option) (Tracker, error) {
	if db == nil {
		return nil, fmt.Errorf(
			"provenance.OpenBorrowedSQLite: the borrowed *sql.DB is nil — where: borrowed-store open; " +
				"impact: no shared database to bridge; fix: pass the exact *sql.DB configured as the DBOS " +
				"root's SqliteSystemDB, or use OpenSQLite/OpenMemory for a standalone tracker")
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf(
			"provenance.OpenBorrowedSQLite: the borrowed *sql.DB failed a liveness ping — where: borrowed-store "+
				"open; impact: the shared database is not reachable; fix: open and configure the *sql.DB before "+
				"borrowing it: %w", err)
	}
	path, err := borrowedDatabasePath(db)
	if err != nil {
		return nil, err
	}

	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	inner, err := openTracker(path, opts...)
	if err != nil {
		return nil, fmt.Errorf("provenance.OpenBorrowedSQLite: open bridge connection on shared file %q: %w", path, err)
	}
	return &borrowedTracker{inner: inner, sentinel: db, path: path}, nil
}

// borrowedDatabasePath derives the on-disk path of the borrowed handle's main
// database via PRAGMA database_list. An empty file column means an in-memory,
// temporary, or pathless database, which cannot back a same-file bridge.
func borrowedDatabasePath(db *sql.DB) (string, error) {
	rows, err := db.Query("PRAGMA database_list")
	if err != nil {
		return "", fmt.Errorf(
			"provenance.OpenBorrowedSQLite: query PRAGMA database_list on the borrowed handle: %w — "+
				"where: path derivation; impact: cannot locate the shared file to bridge; fix: the borrowed "+
				"handle must be a live SQLite connection", err)
	}
	defer func() { _ = rows.Close() }()

	var path string
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", fmt.Errorf("provenance.OpenBorrowedSQLite: scan PRAGMA database_list row: %w", err)
		}
		if name == "main" {
			path = file
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("provenance.OpenBorrowedSQLite: iterate PRAGMA database_list: %w", err)
	}
	if path == "" {
		return "", fmt.Errorf(
			"provenance.OpenBorrowedSQLite: the borrowed *sql.DB has no on-disk main database (in-memory, " +
				"temporary, or pathless) — where: path derivation; impact: a same-file zombiezen bridge is " +
				"impossible without a shared file; fix: back the DBOS root with a file-backed SQLite database, " +
				"or use OpenMemory for a standalone in-memory tracker")
	}
	return path, nil
}

// ---------------------------------------------------------------------------
// borrowedTracker — liveness-gated decorator over the bridge tracker
// ---------------------------------------------------------------------------

// borrowedTracker wraps the zombiezen bridge tracker and gates every public
// read/write on the borrowed handle's liveness. The sentinel is the borrowed
// *sql.DB; the bridge connection lives inside inner and is the only thing Close
// releases.
type borrowedTracker struct {
	inner    Tracker
	sentinel *sql.DB
	path     string
}

// available checks the borrowed handle's liveness before a public operation,
// returning a fully-populated StoreUnavailableError if the sentinel is closed.
func (b *borrowedTracker) available(op string) error {
	if err := b.sentinel.Ping(); err != nil {
		return &StoreUnavailableError{
			Operation: op,
			Store:     fmt.Sprintf("borrowed SQLite (DBOS-owned *sql.DB, shared file %q)", b.path),
			Stage:     "borrowed-handle liveness precheck",
			Impact:    "no Provenance read or write can proceed once the DBOS root closed the shared handle",
			Fix: "the DBOS root that owns this *sql.DB has shut down and closed its pool; open a fresh " +
				"borrowed tracker after re-creating the DBOS root, or use OpenSQLite for a standalone lifecycle",
			Cause: err,
		}
	}
	return nil
}

// Close releases ONLY the bridge connection, never the borrowed handle. It does
// NOT gate on liveness (a caller closes after DBOS shutdown) and is repeat-safe
// because the bridge DB.Close is idempotent.
func (b *borrowedTracker) Close() error { return b.inner.Close() }

// As returns the mutation Session over the shared database. In borrowed mode the
// Session is liveness-gated exactly like every other public borrowed operation: each
// verb (the journaled Create/Update/CloseTask/Atomic AND the un-journaled
// AddEdge/RemoveEdge/AddLabel/RemoveLabel/AddComment) first checks the borrowed
// handle's liveness and returns a StoreUnavailableError once the owning DBOS root has
// shut down — closing the sentinel bypass a raw inner Session would otherwise leave
// open. Its writes also inherit the shared-WAL transient-lock retry, so a Session
// obtained here honors the same durability contract the adapter path does.
func (b *borrowedTracker) As(actor ActorID, authority JournalID) *Session {
	sess := b.inner.As(actor, authority)
	sess.gate = b.available
	sess.retry = borrowedSessionRetry
	return sess
}

// borrowedSessionRetry adapts retryOnTransientLock to the Session's error-only write
// hook so a gated Session's domain writes absorb the same shared-WAL contention the
// borrowed journal surface does.
func borrowedSessionRetry(op string, fn func() error) error {
	_, err := retryOnTransientLock(op, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

// Journal returns the liveness-gated ordered global-journal surface.
func (b *borrowedTracker) Journal() JournalAPI {
	return &borrowedJournal{inner: b.inner.Journal(), owner: b}
}

func (b *borrowedTracker) Show(id TaskID) (Task, error) {
	if err := b.available("Show"); err != nil {
		return Task{}, err
	}
	return b.inner.Show(id)
}

func (b *borrowedTracker) List(filter ListFilter) ([]Task, error) {
	if err := b.available("List"); err != nil {
		return nil, err
	}
	return b.inner.List(filter)
}

func (b *borrowedTracker) Edges(id TaskID, kind *EdgeKind) ([]Edge, error) {
	if err := b.available("Edges"); err != nil {
		return nil, err
	}
	return b.inner.Edges(id, kind)
}

func (b *borrowedTracker) Blocked() ([]Task, error) {
	if err := b.available("Blocked"); err != nil {
		return nil, err
	}
	return b.inner.Blocked()
}

func (b *borrowedTracker) Ready() ([]Task, error) {
	if err := b.available("Ready"); err != nil {
		return nil, err
	}
	return b.inner.Ready()
}

func (b *borrowedTracker) DepTree(id TaskID) ([]Edge, error) {
	if err := b.available("DepTree"); err != nil {
		return nil, err
	}
	return b.inner.DepTree(id)
}

func (b *borrowedTracker) Ancestors(id TaskID) ([]Task, error) {
	if err := b.available("Ancestors"); err != nil {
		return nil, err
	}
	return b.inner.Ancestors(id)
}

func (b *borrowedTracker) Descendants(id TaskID) ([]Task, error) {
	if err := b.available("Descendants"); err != nil {
		return nil, err
	}
	return b.inner.Descendants(id)
}

func (b *borrowedTracker) Labels(id TaskID) ([]string, error) {
	if err := b.available("Labels"); err != nil {
		return nil, err
	}
	return b.inner.Labels(id)
}

func (b *borrowedTracker) Comments(id TaskID) ([]Comment, error) {
	if err := b.available("Comments"); err != nil {
		return nil, err
	}
	return b.inner.Comments(id)
}

func (b *borrowedTracker) RegisterHumanAgent(namespace, name, contact string) (HumanAgent, error) {
	if err := b.available("RegisterHumanAgent"); err != nil {
		return HumanAgent{}, err
	}
	return retryOnTransientLock("RegisterHumanAgent", func() (HumanAgent, error) {
		return b.inner.RegisterHumanAgent(namespace, name, contact)
	})
}

func (b *borrowedTracker) RegisterMLAgent(namespace string, role Role, provider Provider, modelName ModelID) (MLAgent, error) {
	if err := b.available("RegisterMLAgent"); err != nil {
		return MLAgent{}, err
	}
	return retryOnTransientLock("RegisterMLAgent", func() (MLAgent, error) {
		return b.inner.RegisterMLAgent(namespace, role, provider, modelName)
	})
}

func (b *borrowedTracker) RegisterSoftwareAgent(namespace, name, version, source string) (SoftwareAgent, error) {
	if err := b.available("RegisterSoftwareAgent"); err != nil {
		return SoftwareAgent{}, err
	}
	return retryOnTransientLock("RegisterSoftwareAgent", func() (SoftwareAgent, error) {
		return b.inner.RegisterSoftwareAgent(namespace, name, version, source)
	})
}

func (b *borrowedTracker) Agent(id AgentID) (Agent, error) {
	if err := b.available("Agent"); err != nil {
		return Agent{}, err
	}
	return b.inner.Agent(id)
}

func (b *borrowedTracker) HumanAgent(id AgentID) (HumanAgent, error) {
	if err := b.available("HumanAgent"); err != nil {
		return HumanAgent{}, err
	}
	return b.inner.HumanAgent(id)
}

func (b *borrowedTracker) MLAgent(id AgentID) (MLAgent, error) {
	if err := b.available("MLAgent"); err != nil {
		return MLAgent{}, err
	}
	return b.inner.MLAgent(id)
}

func (b *borrowedTracker) SoftwareAgent(id AgentID) (SoftwareAgent, error) {
	if err := b.available("SoftwareAgent"); err != nil {
		return SoftwareAgent{}, err
	}
	return b.inner.SoftwareAgent(id)
}

func (b *borrowedTracker) StartActivity(agentID AgentID, phase Phase, stage Stage, notes string) (Activity, error) {
	if err := b.available("StartActivity"); err != nil {
		return Activity{}, err
	}
	return retryOnTransientLock("StartActivity", func() (Activity, error) {
		return b.inner.StartActivity(agentID, phase, stage, notes)
	})
}

func (b *borrowedTracker) StartActivityWithID(id ActivityID, agentID AgentID, phase Phase, stage Stage, notes string) (Activity, error) {
	if err := b.available("StartActivityWithID"); err != nil {
		return Activity{}, err
	}
	return retryOnTransientLock("StartActivityWithID", func() (Activity, error) {
		return b.inner.StartActivityWithID(id, agentID, phase, stage, notes)
	})
}

func (b *borrowedTracker) EndActivity(id ActivityID) (Activity, error) {
	if err := b.available("EndActivity"); err != nil {
		return Activity{}, err
	}
	return retryOnTransientLock("EndActivity", func() (Activity, error) {
		return b.inner.EndActivity(id)
	})
}

func (b *borrowedTracker) Activities(agentID *AgentID) ([]Activity, error) {
	if err := b.available("Activities"); err != nil {
		return nil, err
	}
	return b.inner.Activities(agentID)
}

// ---------------------------------------------------------------------------
// borrowedJournal — liveness-gated JournalAPI
// ---------------------------------------------------------------------------

type borrowedJournal struct {
	inner JournalAPI
	owner *borrowedTracker
}

func (j *borrowedJournal) QueryTaskEvents(q JournalQueryV1) (JournalTaskEventPageV1, error) {
	if err := j.owner.available("Journal.QueryTaskEvents"); err != nil {
		return JournalTaskEventPageV1{}, err
	}
	return j.inner.QueryTaskEvents(q)
}

func (j *borrowedJournal) TaskAttributions(taskID TaskID) ([]TaskAttribution, error) {
	if err := j.owner.available("Journal.TaskAttributions"); err != nil {
		return nil, err
	}
	return j.inner.TaskAttributions(taskID)
}

func (j *borrowedJournal) VerifyIntegrity() error {
	if err := j.owner.available("Journal.VerifyIntegrity"); err != nil {
		return err
	}
	return j.inner.VerifyIntegrity()
}

func (j *borrowedJournal) RegisterNamespaceClaim(claim ActorNamespaceClaim) error {
	if err := j.owner.available("Journal.RegisterNamespaceClaim"); err != nil {
		return err
	}
	_, err := retryOnTransientLock("Journal.RegisterNamespaceClaim", func() (struct{}, error) {
		return struct{}{}, j.inner.RegisterNamespaceClaim(claim)
	})
	return err
}

func (j *borrowedJournal) RegisterFixedActorEntry(entry FixedActorEntry, fixedUUID [16]byte) error {
	if err := j.owner.available("Journal.RegisterFixedActorEntry"); err != nil {
		return err
	}
	_, err := retryOnTransientLock("Journal.RegisterFixedActorEntry", func() (struct{}, error) {
		return struct{}{}, j.inner.RegisterFixedActorEntry(entry, fixedUUID)
	})
	return err
}

func (j *borrowedJournal) NamespaceClaims() ([]ActorNamespaceClaim, error) {
	if err := j.owner.available("Journal.NamespaceClaims"); err != nil {
		return nil, err
	}
	return j.inner.NamespaceClaims()
}

func (j *borrowedJournal) Apply(in OperationInput) (CommittedResult, error) {
	if err := j.owner.available("Journal.Apply"); err != nil {
		return CommittedResult{}, err
	}
	return retryOnTransientLock("Journal.Apply", func() (CommittedResult, error) {
		return j.inner.Apply(in)
	})
}

func (j *borrowedJournal) LookupCommitted(op OperationID) (CommittedResult, error) {
	if err := j.owner.available("Journal.LookupCommitted"); err != nil {
		return CommittedResult{}, err
	}
	return j.inner.LookupCommitted(op)
}

func (j *borrowedJournal) AuthorityGovernsTaskAt(authJID JournalID, task TaskID, beforeJID JournalID) (bool, error) {
	if err := j.owner.available("Journal.AuthorityGovernsTaskAt"); err != nil {
		return false, err
	}
	return j.inner.AuthorityGovernsTaskAt(authJID, task, beforeJID)
}

func (j *borrowedJournal) PreflightSchema() error {
	if err := j.owner.available("Journal.PreflightSchema"); err != nil {
		return err
	}
	return j.inner.PreflightSchema()
}

func (j *borrowedJournal) ReplayProjections() (ReplayResult, error) {
	if err := j.owner.available("Journal.ReplayProjections"); err != nil {
		return ReplayResult{}, err
	}
	return retryOnTransientLock("Journal.ReplayProjections", func() (ReplayResult, error) {
		return j.inner.ReplayProjections()
	})
}

func (j *borrowedJournal) MigrateLegacyBaseline(in MigrationInput) (MigrationResult, error) {
	if err := j.owner.available("Journal.MigrateLegacyBaseline"); err != nil {
		return MigrationResult{}, err
	}
	return retryOnTransientLock("Journal.MigrateLegacyBaseline", func() (MigrationResult, error) {
		return j.inner.MigrateLegacyBaseline(in)
	})
}

// AsStoreUnavailable extracts a *StoreUnavailableError from err if present, so
// callers can branch on borrowed-store shutdown without string matching.
func AsStoreUnavailable(err error) (*StoreUnavailableError, bool) {
	var e *StoreUnavailableError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Shared-WAL transient-lock retry (borrowed-store write layer)
// ---------------------------------------------------------------------------

// retryOnTransientLock runs a borrowed-store write, retrying ONLY a transient
// shared-WAL BUSY/LOCKED condition with bounded exponential backoff. Because the
// borrowed bridge connection and the DBOS system connection share one WAL file, any
// domain write can momentarily lose SQLite's single-writer lock while DBOS's own
// background writers (queue runner, checkpointer) hold it; busy_timeout plus this
// Go-level retry absorb it. This lives in the borrowed-store layer so EVERY public
// write surface on a borrowed tracker — the journal Apply, the gated Session's verbs,
// and the direct agent/activity/registration writes — gets the same protection, not
// just the one path the DBOS adapter step happens to drive.
//
// A locked write is atomically rolled back (nothing committed), so a §9.4 replay
// short-circuit keeps a pinned-OperationID retry idempotent. Non-transient errors
// (including a shut-down store's StoreUnavailableError) return immediately. After the
// bounded attempts the last transient error is returned wrapped in an actionable
// error that STILL unwraps to the underlying SQLite result code (errors.As), so the
// adapter's infrastructure-vs-domain classifier keeps treating it as a transient
// infrastructure condition and never checkpoints it as a permanent domain failure.
func retryOnTransientLock[T any](op string, fn func() (T, error)) (T, error) {
	const maxAttempts = 12
	backoff := 5 * time.Millisecond
	const maxBackoff = 250 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}
		if !isTransientLock(err) {
			var zero T
			return zero, err
		}
		lastErr = err
		time.Sleep(backoff)
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
	var zero T
	return zero, fmt.Errorf(
		"provenance: borrowed-store %s could not acquire the shared-WAL write lock after %d bounded "+
			"retries — where: borrowed SQLite bridge write contending with the DBOS system connection on the "+
			"shared WAL; why: both connections serialize on SQLite's single-writer lock and the peer held it "+
			"past the backoff budget; when: after exhausting exponential backoff (5ms→250ms per attempt); "+
			"impact: nothing was committed (the fold rolled back atomically, so a retry is idempotent); fix: "+
			"retry the same operation once contention subsides, pinning the OperationID so a §9.4 replay "+
			"short-circuits to the original result: %w",
		op, maxAttempts, lastErr)
}
