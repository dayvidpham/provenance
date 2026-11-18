package sqlite

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"strings"
)

// Verified-prefix cache (issue #74, slice 3).
//
// The verified-as-of ledger proves the whole store at journal tail T. This
// table records a digest of the journal and fact-context rows at or below T that
// the same verification covered. A later whole-store verification that finds the
// ledger stale can then trust that prefix and validate only the rows above T:
//
//   - when the per-table generations of the prefix's tables still equal the
//     ledger's vector, nothing in the prefix moved and no digest is read;
//   - otherwise the digest is recomputed from the stored rows, and a match
//     proves the prefix is byte-for-byte what was verified;
//   - a mismatch, a missing or torn row, or a row whose through_journal_id is
//     not the ledger's journal id runs the full validation exactly as before.
//
// The table is ledger machinery in the same way as the verified agent proof: it
// is created by ensureVerifiedLedgerSchema, listed in verifiedLedgerObjects, and
// deliberately kept out of expectedActivationObjects and verifiedShapeID so that
// adding it does not invalidate a ledger an older build wrote. The digests
// detect accidental or out-of-band edits; they are not tamper-proof against a
// writer who rewrites both the rows and the digest.
const (
	verifiedPrefixTable = "provenance_verified_prefix"

	verifiedPrefixDDL = `CREATE TABLE IF NOT EXISTS provenance_verified_prefix (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		through_journal_id INTEGER NOT NULL,
		journal_digest TEXT NOT NULL,
		contexts_digest TEXT NOT NULL) STRICT`
)

// verifiedPrefixValidatorVersion names the canonical decoder and validator
// generation the recorded digests were verified under. It is part of both digest
// domains and is stored as the prefix of each recorded digest, so a build whose
// decoder or validators are stricter than the one that verified the prefix finds
// a version mismatch before it consults the generation shortcut and runs the
// full validation.
//
// Bump it whenever the canonical decoder (journal.DecodeCanonicalMutation and
// the effect normalization), validateCanonicalOperations,
// validateCanonicalEffectRow, or the fact-context validators change what they
// accept. The adoption seed deliberately does NOT re-verify: it hashes the rows
// of a ledger an older build proved, and records them under the current version,
// so the older build's proof is trusted once at the upgrade boundary. The next
// version bump after that forces the full path.
const verifiedPrefixValidatorVersion = "provenance.verified-prefix.validator.1"

// prefixDigestRecorded renders a digest for storage: version, then the hash.
func prefixDigestRecorded(sum string) string { return verifiedPrefixValidatorVersion + ":" + sum }

// prefixDigestCurrent reports whether a recorded digest carries this build's
// validator version.
func prefixDigestCurrent(recorded string) bool {
	prefix := verifiedPrefixValidatorVersion + ":"
	if !strings.HasPrefix(recorded, prefix) || len(recorded) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(recorded[len(prefix):])
	return err == nil
}

// verifiedPrefixDigests are the two prefix digests. They are separate so a change
// in either relation family invalidates only the validation that reads it.
type verifiedPrefixDigests struct {
	journal  string
	contexts string
}

// verifiedPrefixTrust is the outcome of resolving the cache for one
// verification. through is the verified journal id; journal and contexts report
// which prefix families may be skipped. The zero value trusts nothing, which is
// the full validation.
type verifiedPrefixTrust struct {
	through  int64
	journal  bool
	contexts bool
}

// operationsAfter is the journal id above which canonical operations must be
// validated. Zero means every operation.
func (trust verifiedPrefixTrust) operationsAfter() int64 {
	if trust.journal {
		return trust.through
	}
	return 0
}

// contextsAfter is the journal id above which fact contexts must be validated.
// The expected context set of a prefix fact comes from its canonical operation,
// so the prefix may be skipped only when the operations and the stored contexts
// are both trusted.
func (trust verifiedPrefixTrust) contextsAfter() int64 {
	if trust.journal && trust.contexts {
		return trust.through
	}
	return 0
}

// trustVerifiedPrefix resolves the cache and arms it on this scope for the
// whole-store verification that follows. The returned function disarms it; the
// caller must defer it so no later check on the same scope inherits the
// boundary. Every call site is a whole-store verification path
// (activation, the read-only preflight, and the convergence probe).
func (scope *connScope) trustVerifiedPrefix() (release func()) {
	scope.prefix = scope.resolveVerifiedPrefix()
	return func() { scope.prefix = verifiedPrefixTrust{} }
}

// resolveVerifiedPrefix applies the trust rules. Every failure to read or prove
// the cache reports the zero value, which runs the full validation.
func (scope *connScope) resolveVerifiedPrefix() verifiedPrefixTrust {
	if scope.ephemeralStore {
		return verifiedPrefixTrust{}
	}
	ledger, ok := scope.readVerifiedLedger()
	if !ok || ledger.shapeID != verifiedShapeID() {
		return verifiedPrefixTrust{}
	}
	through, recorded, ok := scope.readVerifiedPrefix()
	if !ok || through != ledger.identity.journalID ||
		!prefixDigestCurrent(recorded.journal) || !prefixDigestCurrent(recorded.contexts) {
		return verifiedPrefixTrust{}
	}
	columns, err := scope.tableColumns("journal_operations")
	if err != nil || isLegacyOperationsColumnSet(columns) {
		return verifiedPrefixTrust{}
	}
	if state, err := scope.classifyFactContextSchema(); err != nil || state != factContextSchemaCanonical {
		return verifiedPrefixTrust{}
	}
	now, err := scope.readTableGenerationVector()
	if err != nil {
		return verifiedPrefixTrust{}
	}
	// A matching generation proves the prefix unchanged only while every covered
	// table still carries this build's change trigger: a table dropped and
	// recreated out of band loses its trigger and its rows without moving a
	// counter. Without current coverage the digest is the only authority.
	coverage, err := scope.activationLedgerCoverageCurrent()
	if err != nil {
		coverage = false
	}
	trust := verifiedPrefixTrust{through: through}
	trust.journal = coverage && scope.generationsEqual(ledger.identity.generations, now, prefixJournalTables()...)
	if !trust.journal {
		digest, err := scope.journalPrefixDigest(through)
		trust.journal = err == nil && digest == recorded.journal
	}
	trust.contexts = coverage && scope.generationsEqual(ledger.identity.generations, now, "journal_decision_contexts", "journal_evidence_contexts")
	if !trust.contexts {
		digest, err := scope.contextsPrefixDigest(through)
		trust.contexts = err == nil && digest == recorded.contexts
	}
	return trust
}

// generationsEqual compares the named tables' counters across two vectors. An
// unknown table or a vector of the wrong length is not equal.
func (scope *connScope) generationsEqual(ledger, now []int64, tables ...string) bool {
	if len(ledger) != verifiedVectorLength() || len(now) != verifiedVectorLength() {
		return false
	}
	for _, table := range tables {
		id := verifiedTableID(table)
		if id == 0 || ledger[id] != now[id] {
			return false
		}
	}
	return true
}

// readVerifiedPrefix reads the singleton. A missing table, a missing row, a
// negative id, or an empty digest is not a usable cache.
func (scope *connScope) readVerifiedPrefix() (through int64, digests verifiedPrefixDigests, ok bool) {
	err := scope.conn.QueryRowContext(scope.ctx,
		"SELECT through_journal_id, journal_digest, contexts_digest FROM provenance_verified_prefix WHERE id = 1").
		Scan(&through, &digests.journal, &digests.contexts)
	if err != nil || through < 0 || digests.journal == "" || digests.contexts == "" {
		return 0, verifiedPrefixDigests{}, false
	}
	return through, digests, true
}

// computeVerifiedPrefixDigests hashes the prefix at or below through.
func (scope *connScope) computeVerifiedPrefixDigests(through int64) (verifiedPrefixDigests, error) {
	journalDigest, err := scope.journalPrefixDigest(through)
	if err != nil {
		return verifiedPrefixDigests{}, err
	}
	contextsDigest, err := scope.contextsPrefixDigest(through)
	if err != nil {
		return verifiedPrefixDigests{}, err
	}
	return verifiedPrefixDigests{journal: journalDigest, contexts: contextsDigest}, nil
}

// prefixStream writes the canonical digest encoding: every field is tagged for
// NULL versus present and length-prefixed, so no two distinct row sequences
// share an encoding, and SQLite row order never reaches the hash because every
// query orders explicitly.
type prefixStream struct{ h hash.Hash }

func newPrefixStream(domain string) *prefixStream {
	stream := &prefixStream{h: sha256.New()}
	stream.text(domain)
	return stream
}

func (s *prefixStream) u64(v uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	_, _ = s.h.Write(buf[:])
}

func (s *prefixStream) int(v int64) { s.u64(uint64(v)) }

func (s *prefixStream) bytes(b []byte) {
	_, _ = s.h.Write([]byte{1})
	s.u64(uint64(len(b)))
	_, _ = s.h.Write(b)
}

func (s *prefixStream) text(v string) { s.bytes([]byte(v)) }

func (s *prefixStream) null() { _, _ = s.h.Write([]byte{0}) }

func (s *prefixStream) sum() string { return hex.EncodeToString(s.h.Sum(nil)) }

// prefixRelation is one relation whose rows at or below the verified journal id
// feed the journal digest. It is a closed enum with selector methods, not a
// struct of statements, so every query this package executes stays decidable
// from source.
type prefixRelation uint8

const (
	prefixJournal prefixRelation = iota + 1
	prefixOperations
	prefixTaskEvents
	prefixTaskEventContexts
	prefixDecisions
	prefixEvidence
	prefixResultSlots
	prefixAuthorities
	prefixBootstraps
	prefixTransitions
	// prefixEpisodes covers only episodes that have a transition at or below the
	// verified journal id. An episode with no prefix transition is intentionally
	// outside the digest: the canonical validators reach an episode only through a
	// transition row, and the whole-store verification still reads every episode.
	prefixEpisodes
)

// prefixJournalRelations is the closed set of relations the canonical validators
// read for an operation at or below the verified journal id. The skipped
// validators (validateCanonicalOperations, validateCanonicalEffectRow,
// validateCanonicalFactContextSets, verifyFactContextIntegrity) read ONLY these
// relations plus the two decision/evidence context relations hashed by
// contextsPrefixDigest; any new read added to them must be added here, to
// prefixRelation.table and query, and to the digest-scope test. Everything else
// is outside the skip on purpose: the agent registry, the closed seed lookups,
// the projections, and the actor claims are checked by their own checks, which
// the prefix trust never bypasses (see TestVerifiedPrefixTrustDoesNotBypassRegistryChecks).
// It is the covered-table list, not the digest, that detects a change to them.
// The digests catch accidental or out-of-band edits and are not tamper-proof
// against a writer who rewrites both the rows and the digest. The set is: the journal and
// operation rows, and every subtype, authority, assignment, task-event, and
// result-slot row the per-effect comparison reads. A prefix operation is skipped
// only when all of them are unchanged, so an out-of-band edit to any of them
// still reaches the full validation.
var prefixJournalRelations = []prefixRelation{
	prefixJournal, prefixOperations, prefixTaskEvents, prefixTaskEventContexts,
	prefixDecisions, prefixEvidence, prefixResultSlots, prefixAuthorities,
	prefixBootstraps, prefixTransitions, prefixEpisodes,
}

// table is the covered table whose generation must be unchanged for the relation
// to be skipped without rehashing it.
func (relation prefixRelation) table() string {
	switch relation {
	case prefixJournal:
		return "journal"
	case prefixOperations:
		return "journal_operations"
	case prefixTaskEvents:
		return "journal_task_events"
	case prefixTaskEventContexts:
		return "journal_task_event_contexts"
	case prefixDecisions:
		return "journal_decisions"
	case prefixEvidence:
		return "journal_evidence"
	case prefixResultSlots:
		return "journal_operation_result_slots"
	case prefixAuthorities:
		return "journal_authorities"
	case prefixBootstraps:
		return "journal_authority_bootstraps"
	case prefixTransitions:
		return "journal_authority_assignment_transitions"
	case prefixEpisodes:
		return "journal_authority_assignment_episodes"
	default:
		panic("unknown prefixRelation")
	}
}

// query selects every column the skipped canonical validation reads, bound to
// ?1 = through, in a total order.
func (relation prefixRelation) query() string {
	switch relation {
	case prefixJournal:
		return "SELECT journal_id,kind_id,actor_id,recorded_at,produced_by_operation_journal_id FROM journal WHERE journal_id<=?1 ORDER BY journal_id"
	case prefixOperations:
		return "SELECT journal_id,operation_id,authority_journal_id,command_digest,mutation_digest,mutation_encoding_version,canonical_mutation FROM journal_operations WHERE journal_id<=?1 ORDER BY journal_id"
	case prefixTaskEvents:
		return "SELECT journal_id,task_id,event_kind,payload FROM journal_task_events WHERE journal_id<=?1 ORDER BY journal_id"
	case prefixTaskEventContexts:
		return "SELECT event_journal_id,context_kind,context_identity,attached_by_journal_id FROM journal_task_event_contexts WHERE event_journal_id<=?1 ORDER BY event_journal_id,context_kind,context_identity"
	case prefixDecisions:
		return "SELECT journal_id,decision_kind,task_id,payload FROM journal_decisions WHERE journal_id<=?1 ORDER BY journal_id"
	case prefixEvidence:
		return "SELECT journal_id,evidence_kind,task_id,content_digest,payload FROM journal_evidence WHERE journal_id<=?1 ORDER BY journal_id"
	case prefixResultSlots:
		return "SELECT journal_id,result_slot_id,produced_journal_id FROM journal_operation_result_slots WHERE journal_id<=?1 ORDER BY journal_id,result_slot_id"
	case prefixAuthorities:
		return "SELECT journal_id,authority_kind_id,operation_authority_id FROM journal_authorities WHERE journal_id<=?1 ORDER BY journal_id"
	case prefixBootstraps:
		return "SELECT journal_id,label FROM journal_authority_bootstraps WHERE journal_id<=?1 ORDER BY journal_id"
	case prefixTransitions:
		return "SELECT journal_id,assignment_id,transition_id FROM journal_authority_assignment_transitions WHERE journal_id<=?1 ORDER BY journal_id"
	case prefixEpisodes:
		return "SELECT e.assignment_id,e.task_id,e.slot_id,e.actor_id,e.predecessor_assignment_id,e.parent_assignment_id FROM journal_authority_assignment_episodes e WHERE e.assignment_id IN (SELECT t.assignment_id FROM journal_authority_assignment_transitions t WHERE t.journal_id<=?1) ORDER BY e.assignment_id"
	default:
		panic("unknown prefixRelation")
	}
}

// prefixJournalTables are the covered tables whose generations gate the journal
// prefix.
func prefixJournalTables() []string {
	tables := make([]string, 0, len(prefixJournalRelations))
	for _, relation := range prefixJournalRelations {
		tables = append(tables, relation.table())
	}
	return tables
}

// any encodes one scanned column with a type tag, so NULL, an integer, a
// text value, and a blob never share an encoding.
func (s *prefixStream) any(v any) error {
	switch v := v.(type) {
	case nil:
		s.null()
	case int64:
		_, _ = s.h.Write([]byte{2})
		s.int(v)
	case string:
		_, _ = s.h.Write([]byte{3})
		s.text(v)
	case []byte:
		_, _ = s.h.Write([]byte{4})
		s.bytes(v)
	case float64:
		_, _ = s.h.Write([]byte{5})
		s.u64(math.Float64bits(v))
	default:
		return fmt.Errorf("unsupported column type %T in a verified-prefix row", v)
	}
	return nil
}

// journalPrefixDigest covers the rows of every relation in prefixJournalRelations
// at or below through, each section followed by its row count.
func (scope *connScope) journalPrefixDigest(through int64) (string, error) {
	stream := newPrefixStream("provenance.verified-prefix.journal.v2/" + verifiedPrefixValidatorVersion)
	for _, relation := range prefixJournalRelations {
		stream.text(relation.table())
		var count int64
		if err := scope.queryRows(relation.query(), []any{through}, func(rows *sql.Rows) error {
			columns, err := rows.Columns()
			if err != nil {
				return err
			}
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				return err
			}
			for _, value := range values {
				if err := stream.any(value); err != nil {
					return err
				}
			}
			count++
			return nil
		}); err != nil {
			return "", fmt.Errorf("digest %s prefix through %d: %w", relation.table(), through, err)
		}
		stream.int(count)
	}
	return prefixDigestRecorded(stream.sum()), nil
}

// contextsPrefixDigest covers the decision and evidence context rows whose
// parent journal id is at or below through, ordered by parent, kind, identity.
func (scope *connScope) contextsPrefixDigest(through int64) (string, error) {
	stream := newPrefixStream("provenance.verified-prefix.contexts.v1/" + verifiedPrefixValidatorVersion)
	for _, relation := range []factContextRelation{factContextDecision, factContextEvidence} {
		stream.text(relation.tableName())
		var count int64
		if err := scope.queryRows(relation.contextPrefixSQL(), []any{through}, func(rows *sql.Rows) error {
			var parent int64
			var kind, identity string
			if err := rows.Scan(&parent, &kind, &identity); err != nil {
				return err
			}
			stream.int(parent)
			stream.text(kind)
			stream.text(identity)
			count++
			return nil
		}); err != nil {
			return "", fmt.Errorf("digest %s prefix through %d: %w", relation.tableName(), through, err)
		}
		stream.int(count)
	}
	return prefixDigestRecorded(stream.sum()), nil
}

// contextPrefixSQL enumerates one relation's rows at or below a parent id in
// primary-key order.
func (relation factContextRelation) contextPrefixSQL() string {
	switch relation {
	case factContextDecision:
		return "SELECT decision_journal_id,context_kind,context_identity FROM journal_decision_contexts WHERE decision_journal_id<=?1 ORDER BY decision_journal_id,context_kind,context_identity"
	case factContextEvidence:
		return "SELECT evidence_journal_id,context_kind,context_identity FROM journal_evidence_contexts WHERE evidence_journal_id<=?1 ORDER BY evidence_journal_id,context_kind,context_identity"
	default:
		panic("unknown factContextRelation")
	}
}

// writeVerifiedPrefix records the digests for the prefix at or below through, in
// the caller's write transaction. digests may be supplied by a caller that
// computed them outside the write lock for the same through; nil computes them
// here. A prefix that cannot be hashed (a legacy shape, a missing relation) is
// removed instead of recorded, so the next whole-store verification runs the
// full validation: the cache is an optimisation and never vetoes a verification.
func (scope *connScope) writeVerifiedPrefix(through int64, digests *verifiedPrefixDigests) error {
	if scope.ephemeralStore {
		return nil
	}
	if digests == nil {
		computed, err := scope.computeVerifiedPrefixDigests(through)
		if err != nil {
			return scope.clearVerifiedPrefix()
		}
		digests = &computed
	}
	if _, err := scope.conn.ExecContext(scope.ctx,
		`INSERT INTO provenance_verified_prefix (id, through_journal_id, journal_digest, contexts_digest) VALUES (1, ?1, ?2, ?3)
		ON CONFLICT(id) DO UPDATE SET through_journal_id = excluded.through_journal_id, journal_digest = excluded.journal_digest, contexts_digest = excluded.contexts_digest`,
		through, digests.journal, digests.contexts); err != nil {
		return fmt.Errorf("write verified prefix through %d: %w", through, err)
	}
	return nil
}

func (scope *connScope) clearVerifiedPrefix() error {
	if _, err := scope.conn.ExecContext(scope.ctx, "DELETE FROM provenance_verified_prefix"); err != nil {
		return fmt.Errorf("clear verified prefix: %w", err)
	}
	return nil
}

// seedVerifiedPrefixIfAbsent permits the one-time hash-only legacy adoption
// only when the table was absent before activation. A missing row in an
// existing prefix-aware table is torn proof and must be validated in full.
func (scope *connScope) seedVerifiedPrefixIfAbsent(through int64, legacyPrefixTableAbsent bool) (bool, error) {
	var exists bool
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT EXISTS(SELECT 1 FROM provenance_verified_prefix WHERE id = 1)").Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return scope.verifiedPrefixRowCurrent(through), nil
	}
	if !legacyPrefixTableAbsent {
		return false, nil
	}
	return true, scope.writeVerifiedPrefix(through, nil)
}

func (scope *connScope) verifiedPrefixVersionCurrent() bool {
	ledger, ok := scope.readVerifiedLedger()
	return ok && scope.verifiedPrefixUsableForLedger(ledger.identity.journalID)
}

// verifiedPrefixUsableForLedger permits a sanctioned write to advance the
// ledger without rehashing the prefix. It never arms prefix trust: a full
// validation requires the exact boundary in verifiedPrefixRowCurrent.
func (scope *connScope) verifiedPrefixUsableForLedger(through int64) bool {
	recorded, digests, ok := scope.readVerifiedPrefix()
	return ok && recorded <= through && prefixDigestCurrent(digests.journal) && prefixDigestCurrent(digests.contexts)
}

// verifiedPrefixRowCurrent reports whether the recorded row is usable, is for
// exactly through, and carries this build's validator version.
func (scope *connScope) verifiedPrefixRowCurrent(through int64) bool {
	recorded, digests, ok := scope.readVerifiedPrefix()
	return ok && recorded == through && prefixDigestCurrent(digests.journal) && prefixDigestCurrent(digests.contexts)
}

// tailReferencesPrefix reports whether a journal row above after is produced by
// an operation at or below after. Such a row would extend a prefix operation's
// effect set, which the tail-only walk cannot see, so the caller must widen to
// the full validation.
func (scope *connScope) tailReferencesPrefix(after int64) (bool, error) {
	var journalID int64
	err := scope.conn.QueryRowContext(scope.ctx,
		"SELECT journal_id FROM journal WHERE journal_id>?1 AND produced_by_operation_journal_id<=?1 LIMIT 1", after).Scan(&journalID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check tail rows against the verified prefix: %w", err)
	}
	return true, nil
}

// effectiveBoundary returns after, or zero (the full validation) when the tail
// is not self-contained. Each boundary-taking validator calls it, so a
// verification runs the tail probe a few times; it is a primary-key range scan
// over the tail only, so the repetition is negligible and is not cached.
func (scope *connScope) effectiveBoundary(after int64) (int64, error) {
	if after == 0 {
		return 0, nil
	}
	reaches, err := scope.tailReferencesPrefix(after)
	if err != nil {
		return 0, err
	}
	if reaches {
		return 0, nil
	}
	return after, nil
}
