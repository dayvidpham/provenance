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

type factMatchForm uint8

const (
	factMatchSelected factMatchForm = iota + 1
	factMatchPage
)

// factMatchBinding is the static predicate/binding contract shared by Apply
// conditions today and bounded fact pages in the next query vertical. It keeps
// subtype dispatch, normalized scope/actor/operation/context filters, and the
// snapshot/cursor bounds and argument layout in one package-private value.
type factMatchBinding struct {
	form         factMatchForm
	kind         factSelectorKind
	contexts     factContextRelation
	snapshotMax  journal.JournalID
	afterJournal journal.JournalID
	pageKinds    []string
	pageLimit    int
	args         []any
}

// pageMatchSQL is the bounded multi-kind form of the shared matcher. The
// subtype and context relation come only from the closed enum; kind values are
// data in a bounded JSON array. The result shape is the page-row shape that the
// .1.4 query layer can scan without reimplementing predicates or bindings.
func (k factSelectorKind) pageMatchSQL() string {
	switch k {
	case factSelectorDecision:
		return `SELECT d.journal_id, ja.recorded_at, d.task_id, d.decision_kind,
       d.payload, ja.effective_actor_id, jo.operation_id,
       ja.produced_by_operation_journal_id
FROM journal_decisions d
JOIN journal_attributed ja ON ja.journal_id = d.journal_id
JOIN journal_operations jo ON jo.journal_id = ja.produced_by_operation_journal_id
WHERE d.journal_id <= ?1
  AND d.journal_id > ?2
  AND d.decision_kind IN (SELECT value FROM json_each(?3))
  AND (NOT ?4 OR d.task_id IS ?5)
  AND (NOT ?6 OR ja.effective_actor_id IN (SELECT value FROM json_each(?7)))
  AND (NOT ?8 OR jo.operation_id IN (SELECT value FROM json_each(?9)))
  AND (NOT ?10 OR (SELECT COUNT(*) FROM json_each(?11) f
       WHERE EXISTS (SELECT ?10 FROM journal_decision_contexts c
                     WHERE c.decision_journal_id = d.journal_id
                       AND c.context_kind = json_extract(f.value,?12)
                       AND c.context_identity = json_extract(f.value,?13))) = ?10)
ORDER BY d.journal_id ASC
LIMIT ?14`
	case factSelectorEvidence:
		return `SELECT e.journal_id, ja.recorded_at, e.task_id, e.evidence_kind,
       e.content_digest, e.payload, ja.effective_actor_id, jo.operation_id,
       ja.produced_by_operation_journal_id
FROM journal_evidence e
JOIN journal_attributed ja ON ja.journal_id = e.journal_id
JOIN journal_operations jo ON jo.journal_id = ja.produced_by_operation_journal_id
WHERE e.journal_id <= ?1
  AND e.journal_id > ?2
  AND e.evidence_kind IN (SELECT value FROM json_each(?3))
  AND (NOT ?4 OR e.task_id IS ?5)
  AND (NOT ?6 OR ja.effective_actor_id IN (SELECT value FROM json_each(?7)))
  AND (NOT ?8 OR jo.operation_id IN (SELECT value FROM json_each(?9)))
  AND (NOT ?10 OR (SELECT COUNT(*) FROM json_each(?11) f
       WHERE EXISTS (SELECT ?10 FROM journal_evidence_contexts c
                     WHERE c.evidence_journal_id = e.journal_id
                       AND c.context_kind = json_extract(f.value,?12)
                       AND c.context_identity = json_extract(f.value,?13))) = ?10)
ORDER BY e.journal_id ASC
LIMIT ?14`
	default:
		panic("unknown factSelectorKind")
	}
}

// latestMatchSQL returns the static SQL to find the MAX(journal_id) matching
// the selector. Uses positional parameters:
//
//	?1  snapshotMaxJournalID (0/nil = no limit)
//	?2  filterByTaskScope: 0 = Any (no filter); 1 = apply scope
//	?3  taskScopeValue: nil = Unscoped (task_id IS ?3 = NULL); non-nil string = Exact
//	?4  filterActors: 0 = skip; 1 = filter by ?5 JSON array
//	?5  JSON array of effective actor_id strings
//	?6  filterOperations: 0 = skip; 1 = filter by ?7 JSON array
//	?7  JSON array of operation_id strings
//	?8  requiredContextCount: 0 = no context filter; >0 = all must match
//	?9  JSON array of {kind,identity} context entries (empty when ?8=0)
//	?10 kind-specific discriminator string (decision_kind or evidence_kind)
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
			       WHERE EXISTS (SELECT ?8 FROM journal_decision_contexts c
			                     WHERE c.decision_journal_id = d.journal_id
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
			       WHERE EXISTS (SELECT ?8 FROM journal_evidence_contexts c
			                     WHERE c.evidence_journal_id = e.journal_id
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
			       WHERE EXISTS (SELECT ?8 FROM journal_decision_contexts c
			                     WHERE c.decision_journal_id = d.journal_id
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
			       WHERE EXISTS (SELECT ?8 FROM journal_evidence_contexts c
			                     WHERE c.evidence_journal_id = e.journal_id
			                       AND c.context_kind = json_extract(f.value,?11)
			                       AND c.context_identity = json_extract(f.value,?12))) = ?8)`
	default:
		panic("unknown factSelectorKind")
	}
}

// evaluateExactFactSelector resolves a FactSelector for an ExactFact condition
// using the caller-owned connection scope.
//
// Returns:
//   - (asserted, true, nil)  — the asserted JournalID matches the selector
//   - (latest, false, nil)   — asserted does not match; latest is the highest match
//   - (0, false, nil)        — no row matches the selector at all
func evaluateExactFactSelector(scope *connScope, sel journal.FactSelector, asserted journal.JournalID) (journal.JournalID, bool, error) {
	if err := scope.requireCanonicalFactContextSchema("ExactFact condition evaluation"); err != nil {
		return 0, false, err
	}
	binding, err := buildFactMatchBinding(sel, 0, 0)
	if err != nil {
		return 0, false, err
	}
	exactArgs := append(binding.args, int64(asserted)) // ?13 (args already has ?11 and ?12)
	found := false
	if err := sqlitex.Execute(scope.conn, binding.kind.exactMatchSQL(), &sqlitex.ExecOptions{
		Args: exactArgs,
		ResultFunc: func(*zs.Stmt) error {
			found = true
			return nil
		},
	}); err != nil {
		return 0, false, fmt.Errorf("evaluateExactFactSelector (%v): %w", sel.Kind, err)
	}
	if found {
		if _, err := scope.verifySelectedFactContext(binding.contexts, int64(asserted)); err != nil {
			return 0, false, err
		}
		return asserted, true, nil
	}
	latest, anyFound, err := latestFactMatch(scope, binding)
	if err != nil {
		return 0, false, err
	}
	if !anyFound {
		return 0, false, nil
	}
	if _, err := scope.verifySelectedFactContext(binding.contexts, int64(latest)); err != nil {
		return 0, false, err
	}
	return latest, false, nil
}

// evaluateCurrentFactSelector resolves a FactSelector for a CurrentFact
// condition using the caller-owned connection scope.
func evaluateCurrentFactSelector(scope *connScope, sel journal.FactSelector) (journal.JournalID, bool, error) {
	if err := scope.requireCanonicalFactContextSchema("CurrentFact condition evaluation"); err != nil {
		return 0, false, err
	}
	binding, err := buildFactMatchBinding(sel, 0, 0)
	if err != nil {
		return 0, false, err
	}
	latest, found, err := latestFactMatch(scope, binding)
	if err != nil || !found {
		return latest, found, err
	}
	if _, err := scope.verifySelectedFactContext(binding.contexts, int64(latest)); err != nil {
		return 0, false, err
	}
	return latest, true, nil
}

func latestFactSelector(scope *connScope, kind factSelectorKind, args []any) (journal.JournalID, bool, error) {
	return latestFactMatch(scope, factMatchBinding{form: factMatchSelected, kind: kind, contexts: kind.contextRelation(), args: args})
}

func latestFactMatch(scope *connScope, binding factMatchBinding) (journal.JournalID, bool, error) {
	var latest journal.JournalID
	found := false
	if err := sqlitex.Execute(scope.conn, binding.kind.latestMatchSQL(), &sqlitex.ExecOptions{
		Args: binding.args,
		ResultFunc: func(stmt *zs.Stmt) error {
			if stmt.ColumnType(0) != zs.TypeNull {
				latest = journal.JournalID(stmt.ColumnInt64(0))
				found = true
			}
			return nil
		},
	}); err != nil {
		return 0, false, fmt.Errorf("latestFactSelector: %w", err)
	}
	return latest, found, nil
}

func (k factSelectorKind) contextRelation() factContextRelation {
	switch k {
	case factSelectorDecision:
		return factContextDecision
	case factSelectorEvidence:
		return factContextEvidence
	default:
		panic("unknown factSelectorKind")
	}
}

// normalizeFactSelectorForMatch validates and canonicalizes every filter
// dimension before it is turned into SQL arguments. Keeping this at the shared
// matcher boundary makes the condition and page forms agree on scope, actor,
// operation, and required-context semantics.
func normalizeFactSelectorForMatch(sel journal.FactSelector) (journal.FactSelector, factSelectorKind, error) {
	var kind factSelectorKind
	var validation error
	switch sel.Kind {
	case journal.FactDecision:
		kind = factSelectorDecision
		validation = (journal.DecisionQuery{Filter: sel.Filter, Kinds: []journal.DecisionKind{sel.DecisionKind}, Page: journal.FactPageRequest{Limit: 1}}).Validate()
	case journal.FactEvidence:
		kind = factSelectorEvidence
		validation = (journal.EvidenceQuery{Filter: sel.Filter, Kinds: []journal.EvidenceKind{sel.EvidenceKind}, Page: journal.FactPageRequest{Limit: 1}}).Validate()
	default:
		return journal.FactSelector{}, 0, fmt.Errorf("fact matcher: unknown FactSelector kind %d — where: matcher input; when: normalization; impact: no fact was selected; fix: use FactDecision or FactEvidence", sel.Kind)
	}
	if validation != nil {
		return journal.FactSelector{}, 0, fmt.Errorf("fact matcher: invalid selector: %w", validation)
	}
	normalized, err := normalizeFactQueryFilter(sel.Filter)
	if err != nil {
		return journal.FactSelector{}, 0, err
	}
	sel.Filter = normalized
	return sel, kind, nil
}

// ---------------------------------------------------------------------------
// Argument builder (10 base args + optional 11th for exactMatchSQL)
// ---------------------------------------------------------------------------

// buildSelectorArgs translates a normalized FactSelector into 10 positional args:
//
//	?1  snapshotMax (nil = no limit; integer = upper bound)
//	?2  filterByTaskScope (0 = Any, 1 = apply scope filter)
//	?3  taskScopeValue (nil = Unscoped assertion; string = Exact task_id)
//	?4  filterActors (0 = skip; 1 = apply actor filter)
//	?5  JSON actor array (ignored when ?4=0)
//	?6  filterOperations (0 = skip; 1 = apply operation filter)
//	?7  JSON operation array (ignored when ?6=0)
//	?8  requiredContextCount (0 = no filter; >0 = all must match)
//	?9  JSON context array [{kind,identity},...] (ignored when ?8=0)
//	?10 kind discriminator string (decision_kind or evidence_kind value)
func buildSelectorArgs(sel journal.FactSelector, snapshotMax journal.JournalID) (factSelectorKind, []any, error) {
	normalized, kind, err := normalizeFactSelectorForMatch(sel)
	if err != nil {
		return 0, nil, err
	}
	sel = normalized
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

func buildFactMatchBinding(sel journal.FactSelector, snapshotMax, afterJournal journal.JournalID) (factMatchBinding, error) {
	kind, args, err := buildSelectorArgs(sel, snapshotMax)
	if err != nil {
		return factMatchBinding{}, err
	}
	return factMatchBinding{form: factMatchSelected, kind: kind, contexts: kind.contextRelation(), snapshotMax: snapshotMax, afterJournal: afterJournal, args: args}, nil
}

// buildFactPageMatchBinding is the bounded multi-kind form of the same static
// predicate/binding contract. The subtype remains a closed enum and the kind
// set is data, capped by journal.MaxFactQueryKinds. Its positional arguments
// are: snapshot, exclusive cursor, kind JSON, then the normalized filter slots
// shared with buildSelectorArgs, followed by LIMIT.
func buildFactPageMatchBinding(kind factSelectorKind, page journal.FactPageRequest, filter journal.FactFilter, kinds []string) (factMatchBinding, error) {
	if kind != factSelectorDecision && kind != factSelectorEvidence {
		return factMatchBinding{}, fmt.Errorf("fact matcher: unknown page subtype %d — where: page binding; when: dispatch; impact: no page was queried; fix: use the closed decision or evidence subtype", kind)
	}
	if err := page.Validate(); err != nil {
		return factMatchBinding{}, fmt.Errorf("fact matcher: invalid page: %w", err)
	}
	if len(kinds) == 0 || len(kinds) > journal.MaxFactQueryKinds {
		return factMatchBinding{}, fmt.Errorf("fact matcher: page kind set must contain 1..%d values — where: page binding; when: normalization; impact: no page was queried; fix: pass a bounded non-empty kind set", journal.MaxFactQueryKinds)
	}
	for _, value := range kinds {
		if err := journal.ValidateEventKind(journal.EventKind(value)); err != nil {
			return factMatchBinding{}, fmt.Errorf("fact matcher: invalid page kind %q: %w", value, err)
		}
	}
	normalizedFilter, err := normalizeFactQueryFilter(filter)
	if err != nil {
		return factMatchBinding{}, err
	}
	var selector journal.FactSelector
	if kind == factSelectorDecision {
		selector = journal.FactSelector{Kind: journal.FactDecision, DecisionKind: journal.DecisionKind(kinds[0]), Filter: normalizedFilter}
	} else {
		selector = journal.FactSelector{Kind: journal.FactEvidence, EvidenceKind: journal.EvidenceKind(kinds[0]), Filter: normalizedFilter}
	}
	_, singleArgs, err := buildSelectorArgs(selector, page.SnapshotMaxJournalID)
	if err != nil {
		return factMatchBinding{}, err
	}
	kindJSON, err := json.Marshal(kinds)
	if err != nil {
		return factMatchBinding{}, fmt.Errorf("fact matcher: encode page kind set: %w", err)
	}
	args := []any{singleArgs[0], int64(page.AfterJournalID), string(kindJSON)}
	args = append(args, singleArgs[1:9]...)
	args = append(args, singleArgs[10:]...)
	args = append(args, page.Limit+1)
	return factMatchBinding{
		form: factMatchPage, kind: kind, contexts: kind.contextRelation(), snapshotMax: page.SnapshotMaxJournalID,
		afterJournal: page.AfterJournalID, pageKinds: append([]string(nil), kinds...), pageLimit: page.Limit + 1,
		args: args,
	}, nil
}
