package sqlite

import (
	"encoding/json"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// fact_match.go implements the shared FactFilter/FactSelector matcher used by
// both transaction-local condition evaluation (fact_conditions.go) and the
// bounded read-only fact queries owned by .1.4.
//
// Static SQL architecture: all SQL strings are returned by selector methods on
// the closed factSelectorKind integer enum (satisfying the
// TestProductionSQLUsesDirectStaticSinks requirement). Variable-length filters
// (actor IDs, operation IDs, required contexts) use json_each with positional
// args; optional task-scope and list filters use the (NOT ?N OR ...) pattern.
//
// Task scope parameterization:
//   ?2 filterByTaskScope: 0 = Any (no filter); 1 = apply task scope
//   ?3 taskScopeValue:    NULL = Unscoped (task_id IS ?3); non-NULL = Exact
//                         Using SQLite's "col IS ?param" handles both cases:
//                         IS NULL when param=NULL, exact match when param=text.
// This avoids `IS NULL` literals in SQL (which the raw-DML checker rejects).
//
// Actor / operation / context filters all use the same json_each + COUNT pattern.

// factSelectorKind is a closed two-value discriminator for query dispatch.
type factSelectorKind uint8

const (
	factSelectorDecision factSelectorKind = iota + 1
	factSelectorEvidence
)

// latestMatchSQL returns the static SQL to find the MAX(journal_id) matching
// the selector. Uses positional parameters:
//  ?1  snapshotMaxJournalID (0/nil = no limit)
//  ?2  filterByTaskScope: 0 = Any (no filter); 1 = apply scope
//  ?3  taskScopeValue: nil = Unscoped (task_id IS ?3 = NULL); non-nil string = Exact
//  ?4  filterActors: 0 = skip; 1 = filter by ?5 JSON array
//  ?5  JSON array of effective actor_id strings
//  ?6  filterOperations: 0 = skip; 1 = filter by ?7 JSON array
//  ?7  JSON array of operation_id strings
//  ?8  requiredContextCount: 0 = no context filter; >0 = all must match
//  ?9  JSON array of {kind,identity} context entries (empty when ?8=0)
//  ?10 kind-specific discriminator string (decision_kind or evidence_kind)
func (k factSelectorKind) latestMatchSQL() string {
	switch k {
	case factSelectorDecision:
		return `SELECT MAX(d.journal_id) FROM journal_decisions d
			JOIN journal_attributed ja ON ja.journal_id = d.journal_id
			JOIN journal_operations jo ON jo.journal_id = ja.produced_by_operation_journal_id
			WHERE d.decision_kind = ?10
			  AND (NOT ?1 OR d.journal_id <= ?1)
			  AND (NOT ?2 OR d.task_id IS ?3)
			  AND (NOT ?4 OR ja.effective_actor_id IN (SELECT value FROM json_each(?5)))
			  AND (NOT ?6 OR jo.operation_id IN (SELECT value FROM json_each(?7)))
			  AND ((NOT ?8) OR (SELECT COUNT(*) FROM json_each(?9) f
			       WHERE EXISTS (SELECT ?8 FROM journal_task_event_contexts c
			                     WHERE c.event_journal_id = d.journal_id
			                       AND c.context_kind = json_extract(f.value,?11)
			                       AND c.context_identity = json_extract(f.value,?12))) = ?8)`
	case factSelectorEvidence:
		return `SELECT MAX(e.journal_id) FROM journal_evidence e
			JOIN journal_attributed ja ON ja.journal_id = e.journal_id
			JOIN journal_operations jo ON jo.journal_id = ja.produced_by_operation_journal_id
			WHERE e.evidence_kind = ?10
			  AND (NOT ?1 OR e.journal_id <= ?1)
			  AND (NOT ?2 OR e.task_id IS ?3)
			  AND (NOT ?4 OR ja.effective_actor_id IN (SELECT value FROM json_each(?5)))
			  AND (NOT ?6 OR jo.operation_id IN (SELECT value FROM json_each(?7)))
			  AND ((NOT ?8) OR (SELECT COUNT(*) FROM json_each(?9) f
			       WHERE EXISTS (SELECT ?8 FROM journal_task_event_contexts c
			                     WHERE c.event_journal_id = e.journal_id
			                       AND c.context_kind = json_extract(f.value,?11)
			                       AND c.context_identity = json_extract(f.value,?12))) = ?8)`
	default:
		panic("unknown factSelectorKind")
	}
}

// exactMatchSQL returns the static SQL to check whether a specific journal_id
// matches the selector. Uses the same parameter slots as latestMatchSQL,
// plus ?11 for the asserted JournalID.
func (k factSelectorKind) exactMatchSQL() string {
	switch k {
	case factSelectorDecision:
		return `SELECT d.journal_id FROM journal_decisions d
			JOIN journal_attributed ja ON ja.journal_id = d.journal_id
			JOIN journal_operations jo ON jo.journal_id = ja.produced_by_operation_journal_id
			WHERE d.journal_id = ?13
			  AND d.decision_kind = ?10
			  AND (NOT ?2 OR d.task_id IS ?3)
			  AND (NOT ?4 OR ja.effective_actor_id IN (SELECT value FROM json_each(?5)))
			  AND (NOT ?6 OR jo.operation_id IN (SELECT value FROM json_each(?7)))
			  AND ((NOT ?8) OR (SELECT COUNT(*) FROM json_each(?9) f
			       WHERE EXISTS (SELECT ?8 FROM journal_task_event_contexts c
			                     WHERE c.event_journal_id = d.journal_id
			                       AND c.context_kind = json_extract(f.value,?11)
			                       AND c.context_identity = json_extract(f.value,?12))) = ?8)`
	case factSelectorEvidence:
		return `SELECT e.journal_id FROM journal_evidence e
			JOIN journal_attributed ja ON ja.journal_id = e.journal_id
			JOIN journal_operations jo ON jo.journal_id = ja.produced_by_operation_journal_id
			WHERE e.journal_id = ?13
			  AND e.evidence_kind = ?10
			  AND (NOT ?2 OR e.task_id IS ?3)
			  AND (NOT ?4 OR ja.effective_actor_id IN (SELECT value FROM json_each(?5)))
			  AND (NOT ?6 OR jo.operation_id IN (SELECT value FROM json_each(?7)))
			  AND ((NOT ?8) OR (SELECT COUNT(*) FROM json_each(?9) f
			       WHERE EXISTS (SELECT ?8 FROM journal_task_event_contexts c
			                     WHERE c.event_journal_id = e.journal_id
			                       AND c.context_kind = json_extract(f.value,?11)
			                       AND c.context_identity = json_extract(f.value,?12))) = ?8)`
	default:
		panic("unknown factSelectorKind")
	}
}

// ---------------------------------------------------------------------------
// Public evaluation entry-points
// ---------------------------------------------------------------------------

// EvaluateExactFactSelector resolves a FactSelector for an ExactFact condition.
// The caller must hold db.mu and be inside a SQLite transaction.
//
// Returns:
//   - (asserted, true, nil)  — the asserted JournalID matches the selector
//   - (latest, false, nil)   — asserted does not match; latest is the highest match
//   - (0, false, nil)        — no row matches the selector at all
func (db *DB) EvaluateExactFactSelector(sel journal.FactSelector, asserted journal.JournalID) (actual journal.JournalID, matched bool, err error) {
	return db.evaluateExactFactSelectorLocked(sel, asserted)
}

// EvaluateCurrentFactSelector resolves a FactSelector for a CurrentFact condition.
// The caller must hold db.mu and be inside a SQLite transaction.
//
// Returns:
//   - (highest, true, nil)   — at least one matching row exists
//   - (0, false, nil)        — no row matches the selector
func (db *DB) EvaluateCurrentFactSelector(sel journal.FactSelector, asserted journal.JournalID) (actual journal.JournalID, found bool, err error) {
	return db.evaluateCurrentFactSelectorLocked(sel, asserted)
}

// ---------------------------------------------------------------------------
// Internal evaluation
// ---------------------------------------------------------------------------

func (db *DB) evaluateExactFactSelectorLocked(sel journal.FactSelector, asserted journal.JournalID) (journal.JournalID, bool, error) {
	kind, args, err := buildSelectorArgs(sel, 0)
	if err != nil {
		return 0, false, err
	}
	exactArgs := append(args, int64(asserted)) // ?13 (args already has ?11 and ?12)
	found := false
	if err := sqlitex.Execute(db.conn, kind.exactMatchSQL(), &sqlitex.ExecOptions{
		Args: exactArgs,
		ResultFunc: func(*zs.Stmt) error {
			found = true
			return nil
		},
	}); err != nil {
		return 0, false, fmt.Errorf("evaluateExactFactSelectorLocked (%v): %w", sel.Kind, err)
	}
	if found {
		return asserted, true, nil
	}
	latest, anyFound, err := db.latestFactSelectorLocked(kind, args)
	if err != nil {
		return 0, false, err
	}
	if !anyFound {
		return 0, false, nil
	}
	return latest, false, nil
}

func (db *DB) evaluateCurrentFactSelectorLocked(sel journal.FactSelector, _ journal.JournalID) (journal.JournalID, bool, error) {
	kind, args, err := buildSelectorArgs(sel, 0)
	if err != nil {
		return 0, false, err
	}
	return db.latestFactSelectorLocked(kind, args)
}

func (db *DB) latestFactSelectorLocked(kind factSelectorKind, args []any) (journal.JournalID, bool, error) {
	var latest journal.JournalID
	found := false
	if err := sqlitex.Execute(db.conn, kind.latestMatchSQL(), &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *zs.Stmt) error {
			if stmt.ColumnType(0) != zs.TypeNull {
				latest = journal.JournalID(stmt.ColumnInt64(0))
				found = true
			}
			return nil
		},
	}); err != nil {
		return 0, false, fmt.Errorf("latestFactSelectorLocked: %w", err)
	}
	return latest, found, nil
}

// ---------------------------------------------------------------------------
// Argument builder (10 base args + optional 11th for exactMatchSQL)
// ---------------------------------------------------------------------------

// buildSelectorArgs translates a normalized FactSelector into 10 positional args:
//  ?1  snapshotMax (nil = no limit; integer = upper bound)
//  ?2  filterByTaskScope (0 = Any, 1 = apply scope filter)
//  ?3  taskScopeValue (nil = Unscoped assertion; string = Exact task_id)
//  ?4  filterActors (0 = skip; 1 = apply actor filter)
//  ?5  JSON actor array (ignored when ?4=0)
//  ?6  filterOperations (0 = skip; 1 = apply operation filter)
//  ?7  JSON operation array (ignored when ?6=0)
//  ?8  requiredContextCount (0 = no filter; >0 = all must match)
//  ?9  JSON context array [{kind,identity},...] (ignored when ?8=0)
//  ?10 kind discriminator string (decision_kind or evidence_kind value)
func buildSelectorArgs(sel journal.FactSelector, snapshotMax journal.JournalID) (factSelectorKind, []any, error) {
	var kind factSelectorKind
	var discriminator string

	switch sel.Kind {
	case journal.FactDecision:
		kind = factSelectorDecision
		discriminator = string(sel.DecisionKind)
	case journal.FactEvidence:
		kind = factSelectorEvidence
		discriminator = string(sel.EvidenceKind)
	default:
		return 0, nil, fmt.Errorf(
			"buildSelectorArgs: unknown FactSelector kind %v — "+
				"where: FactFilter matcher; when: before SQL execution; "+
				"impact: condition evaluation rejected; "+
				"fix: use FactDecision or FactEvidence",
			sel.Kind)
	}

	// ?1: snapshot limit. 0 means no limit: (NOT 0 OR ...) = TRUE for all rows.
	// Must not be nil because (NOT NULL OR ...) = NULL which excludes all rows.
	snap := int64(snapshotMax)

	// ?2 and ?3: task scope
	// Any (0) → filterByTaskScope=0, taskScopeValue=nil (ignored)
	// Unscoped (1) → filterByTaskScope=1, taskScopeValue=nil (task_id IS nil = task_id IS NULL)
	// Exact (1) → filterByTaskScope=1, taskScopeValue="namespace--uuid"
	filterByTaskScope := 0
	var taskScopeValue any // nil for Unscoped
	switch sel.Filter.TaskScope.Kind {
	case journal.FactTaskAny:
		// no filter
	case journal.FactTaskUnscoped:
		filterByTaskScope = 1
		// taskScopeValue stays nil → "task_id IS NULL" via "task_id IS ?3"
	case journal.FactTaskExact:
		filterByTaskScope = 1
		taskScopeValue = sel.Filter.TaskScope.TaskID.String()
	}

	// ?4 and ?5: actor filter
	filterActors := 0
	actorJSON := "[]"
	if len(sel.Filter.EffectiveActorIDs) > 0 {
		filterActors = 1
		ids := make([]string, len(sel.Filter.EffectiveActorIDs))
		for i, id := range sel.Filter.EffectiveActorIDs {
			ids[i] = id.String()
		}
		j, err := json.Marshal(ids)
		if err != nil {
			return 0, nil, fmt.Errorf("buildSelectorArgs: marshal actor IDs: %w", err)
		}
		actorJSON = string(j)
	}

	// ?6 and ?7: operation filter
	filterOps := 0
	opJSON := "[]"
	if len(sel.Filter.OperationIDs) > 0 {
		filterOps = 1
		ops := make([]string, len(sel.Filter.OperationIDs))
		for i, op := range sel.Filter.OperationIDs {
			ops[i] = string(op)
		}
		j, err := json.Marshal(ops)
		if err != nil {
			return 0, nil, fmt.Errorf("buildSelectorArgs: marshal operation IDs: %w", err)
		}
		opJSON = string(j)
	}

	// ?8 and ?9: required contexts
	ctxCount := 0
	ctxJSON := "[]"
	if len(sel.Filter.RequiredContexts) > 0 {
		type ctxEntry struct {
			Kind     string `json:"kind"`
			Identity string `json:"identity"`
		}
		entries := make([]ctxEntry, 0, len(sel.Filter.RequiredContexts))
		for _, ctx := range sel.Filter.RequiredContexts {
			ck, identity, err := journal.EncodeStoredEventContext(ctx)
			if err != nil {
				continue
			}
			entries = append(entries, ctxEntry{Kind: string(ck), Identity: identity})
		}
		ctxCount = len(entries)
		if ctxCount > 0 {
			j, err := json.Marshal(entries)
			if err != nil {
				return 0, nil, fmt.Errorf("buildSelectorArgs: marshal required contexts: %w", err)
			}
			ctxJSON = string(j)
		}
	}

	args := []any{
		snap,              // ?1  snapshotMax (0 = no limit; >0 = upper bound)
		filterByTaskScope, // ?2  filter by task scope
		taskScopeValue,    // ?3  nil = IS NULL (Unscoped); string = exact task_id
		filterActors,      // ?4  filter actors
		actorJSON,         // ?5  actor JSON array
		filterOps,         // ?6  filter operations
		opJSON,            // ?7  operation JSON array
		ctxCount,          // ?8  required context count
		ctxJSON,           // ?9  context JSON array
		discriminator,     // ?10 decision_kind or evidence_kind
		"$.kind",          // ?11 json_extract path for context kind
		"$.identity",      // ?12 json_extract path for context identity
		// ?13 = asserted JournalID for exactMatchSQL (appended by caller)
	}
	return kind, args, nil
}
