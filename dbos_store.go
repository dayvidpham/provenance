package provenance

// dbos_store.go implements the borrowed-storage bridge that lets Provenance share
// one physical SQLite database with a DBOS durable-execution root, plus the
// StoreUnavailableError lifecycle error. Provenance persists through the same
// Modernc database/sql pool supplied by the DBOS root.
//
// OpenBorrowedSQLite validates the borrowed *sql.DB, retains it as the sole
// lifecycle token, and activates the Provenance schema through that exact pool.
// Every public operation checks borrowed-handle liveness first; after DBOS closes
// its pool, reads and writes return StoreUnavailableError. Tracker.Close never
// closes the borrowed pool. In-memory, temporary, and pathless databases remain
// rejected because the public DBOS integration contract requires a durable file.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/internal/dbossys"
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
		"provenance: store unavailable during %s — store: %s; stage: %s; impact: %s; fix: %s%s",
		e.Operation, e.Store, e.Stage, e.Impact, e.Fix, causeClause(e.Cause))
}

func (e *StoreUnavailableError) Unwrap() error { return e.Cause }

// OpenBorrowedSQLite opens a Tracker that shares the borrowed *sql.DB's physical
// database via that exact handle. The borrowed handle is never closed by
// Provenance and is the sole lifecycle token: after it (or its owning DBOS root)
// closes, every borrowed-tracker operation returns a StoreUnavailableError.
//
// It rejects a nil handle, a handle that cannot be pinged, and an in-memory, temp,
// or pathless database (a same-file bridge is impossible there), naming OpenMemory
// as the standalone alternative. Provenance accepts the caller-owned pool limits
// as supplied and neither validates nor mutates them.
func OpenBorrowedSQLite(db *sql.DB, opts ...Option) (Tracker, error) {
	if db == nil {
		return nil, fmt.Errorf(
			"provenance.OpenBorrowedSQLite: the borrowed *sql.DB is nil — where: borrowed-store open; " +
				"impact: no shared database to bridge; fix: pass the exact *sql.DB configured as the DBOS " +
				"root's SQLiteSystemDB, or use OpenSQLite/OpenMemory for a standalone tracker")
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
	inner, err := openBorrowedTracker(db, opts...)
	if err != nil {
		return nil, fmt.Errorf("provenance.OpenBorrowedSQLite: activate the borrowed database/sql pool for shared file %q: %w", path, err)
	}
	return &borrowedTracker{inner: inner, sentinel: db, path: path}, nil
}

// borrowedDatabasePath derives the on-disk path of the borrowed handle's main
// database. An empty path means an in-memory, temporary, or pathless database,
// which cannot back a same-file bridge. The derivation itself lives in
// internal/dbossys because the DBOS system-schema refusal names the same file.
func borrowedDatabasePath(db *sql.DB) (string, error) {
	path, err := dbossys.MainDatabaseFile(context.Background(), db)
	if err != nil {
		return "", fmt.Errorf(
			"provenance.OpenBorrowedSQLite: derive the borrowed handle's main database path: %w — "+
				"where: path derivation; impact: cannot locate the shared file to bridge; fix: the borrowed "+
				"handle must be a live SQLite connection", err)
	}
	if path == "" {
		return "", fmt.Errorf(
			"provenance.OpenBorrowedSQLite: the borrowed *sql.DB has no on-disk main database (in-memory, " +
				"temporary, or pathless) — where: path derivation; impact: the durable DBOS integration requires " +
				"a file-backed shared database; fix: back the DBOS root with a file-backed SQLite database, " +
				"or use OpenMemory for a standalone in-memory tracker")
	}
	return path, nil
}

// ---------------------------------------------------------------------------
// borrowedTracker — liveness-gated decorator over the bridge tracker
// ---------------------------------------------------------------------------

// borrowedTracker wraps the direct borrowed-pool tracker and gates every public
// read/write on the borrowed handle's liveness. The sentinel is the borrowed
// *sql.DB; Close releases no caller-owned database resource.
type borrowedTracker struct {
	inner    Tracker
	sentinel *sql.DB
	path     string
	// journalApplyFault is an internal dependency fault hook. Production leaves
	// it nil; tests inject one failure for each borrowed Journal.Apply call.
	journalApplyFault func() error
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

// Close invalidates this borrowed tracker without closing the caller-owned handle.
// It does not gate on liveness (a caller may close after DBOS shutdown) and is
// repeat-safe.
func (b *borrowedTracker) Close() error { return b.inner.Close() }

// As returns the mutation Session over the shared database. In borrowed mode the
// Session is liveness-gated exactly like every other public borrowed operation: each
// verb (Create/Update/CloseTask/Atomic and the relationship/annotation verbs
// AddEdge/RemoveEdge/AddLabel/RemoveLabel/AddComment) first checks the borrowed
// handle's liveness and returns a StoreUnavailableError once the owning DBOS root has
// shut down — closing the sentinel blocks a raw inner Session that would otherwise remain
// open. SQLite's busy_timeout is the sole local contention wait for its writes.
func (b *borrowedTracker) As(actor ActorID, authority JournalID) *Session {
	sess := b.inner.As(actor, authority)
	sess.gate = b.available
	return sess
}

func (b *borrowedTracker) InitializeGovernedRoot(ctx context.Context, request RootGenesisRequest) (OperationClosure, error) {
	if err := b.available("InitializeGovernedRoot"); err != nil {
		return OperationClosure{}, err
	}
	return b.inner.InitializeGovernedRoot(ctx, request)
}

// Journal returns the liveness-gated ordered global-journal surface.
func (b *borrowedTracker) Journal() Journal {
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
	return b.inner.RegisterHumanAgent(namespace, name, contact)
}

func (b *borrowedTracker) RegisterMLAgent(namespace string, role Role, provider Provider, modelName ModelID) (MLAgent, error) {
	if err := b.available("RegisterMLAgent"); err != nil {
		return MLAgent{}, err
	}
	return b.inner.RegisterMLAgent(namespace, role, provider, modelName)
}

func (b *borrowedTracker) RegisterSoftwareAgent(namespace, name, version, source string) (SoftwareAgent, error) {
	if err := b.available("RegisterSoftwareAgent"); err != nil {
		return SoftwareAgent{}, err
	}
	return b.inner.RegisterSoftwareAgent(namespace, name, version, source)
}

func (b *borrowedTracker) RegisterFixedSoftwareAgent(reg FixedSoftwareAgentRegistration) (SoftwareAgent, error) {
	if err := b.available("RegisterFixedSoftwareAgent"); err != nil {
		return SoftwareAgent{}, err
	}
	return b.inner.RegisterFixedSoftwareAgent(reg)
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
	return b.inner.StartActivity(agentID, phase, stage, notes)
}

func (b *borrowedTracker) StartActivityWithID(id ActivityID, agentID AgentID, phase Phase, stage Stage, notes string) (Activity, error) {
	if err := b.available("StartActivityWithID"); err != nil {
		return Activity{}, err
	}
	return b.inner.StartActivityWithID(id, agentID, phase, stage, notes)
}

func (b *borrowedTracker) EndActivity(id ActivityID) (Activity, error) {
	if err := b.available("EndActivity"); err != nil {
		return Activity{}, err
	}
	return b.inner.EndActivity(id)
}

func (b *borrowedTracker) Activities(agentID *AgentID) ([]Activity, error) {
	if err := b.available("Activities"); err != nil {
		return nil, err
	}
	return b.inner.Activities(agentID)
}

// ---------------------------------------------------------------------------
// borrowedJournal — liveness-gated Journal
// ---------------------------------------------------------------------------

type borrowedJournal struct {
	inner Journal
	owner *borrowedTracker
}

// Facts returns a liveness-gated reader over the same borrowed journal. The
// accessor itself has no error result, so each query performs the owner check.
func (j *borrowedJournal) Facts() FactQueryAPI {
	return &borrowedFactQueries{inner: j.inner.Facts(), owner: j.owner}
}

type borrowedFactQueries struct {
	inner FactQueryAPI
	owner *borrowedTracker
}

var (
	_ FactQueryAPI = (*borrowedFactQueries)(nil)
	_ Journal      = (*borrowedJournal)(nil)
)

func (q *borrowedFactQueries) QueryDecisions(in DecisionQuery) (DecisionPage, error) {
	if err := q.owner.available("Journal.Facts.QueryDecisions"); err != nil {
		return DecisionPage{}, err
	}
	return q.inner.QueryDecisions(in)
}

func (q *borrowedFactQueries) QueryEvidence(in EvidenceQuery) (EvidencePage, error) {
	if err := q.owner.available("Journal.Facts.QueryEvidence"); err != nil {
		return EvidencePage{}, err
	}
	return q.inner.QueryEvidence(in)
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
	return j.inner.RegisterNamespaceClaim(claim)
}

func (j *borrowedJournal) RegisterFixedActorEntry(entry FixedActorEntry) error {
	if err := j.owner.available("Journal.RegisterFixedActorEntry"); err != nil {
		return err
	}
	return j.inner.RegisterFixedActorEntry(entry)
}

func (j *borrowedJournal) NamespaceClaims() ([]ActorNamespaceClaim, error) {
	if err := j.owner.available("Journal.NamespaceClaims"); err != nil {
		return nil, err
	}
	return j.inner.NamespaceClaims()
}

func (j *borrowedJournal) Apply(in OperationInput) (CommittedResult, error) {
	return j.ApplyContext(context.Background(), in)
}

func (j *borrowedJournal) ApplyContext(ctx context.Context, in OperationInput) (CommittedResult, error) {
	if err := j.owner.available("Journal.Apply"); err != nil {
		return CommittedResult{}, err
	}
	if j.owner.journalApplyFault != nil {
		if err := j.owner.journalApplyFault(); err != nil {
			return CommittedResult{}, err
		}
	}
	if journal, ok := j.inner.(ContextJournal); ok {
		return journal.ApplyContext(ctx, in)
	}
	return j.inner.Apply(in)
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
	return j.inner.ReplayProjections()
}

func (j *borrowedJournal) MigrateLegacyBaseline(in MigrationInput) (MigrationResult, error) {
	if err := j.owner.available("Journal.MigrateLegacyBaseline"); err != nil {
		return MigrationResult{}, err
	}
	return j.inner.MigrateLegacyBaseline(in)
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
