// Package bdimport is an idempotent importer of Beads (bd) issue-tracker history
// into the provenance model (beads → provenance → data-labour-prov graph). It is
// the library behind cmd/bd-import; the CLI is a thin shell over Import.
//
// # Header re-survey (mandatory design contract)
//
// This package was written against provenance HEAD feat/m4-plan-derivation
// (journal/DBOS release; §2/§3 additions landed). The following contract facts were
// re-validated first-hand before relying on them; they are the load-bearing
// assumptions of this importer:
//
//   - ActorID naming. The agent identity domain is ActorID (AgentID is the retained
//     one-release deprecated alias; identical wire form). This package uses ActorID.
//
//   - ALL writes flow through the journaled Session (Tracker.As(actor, authority)).
//     The direct Tracker task/edge/label/comment mutators are retired; only reads and
//     the agent/activity/plan registration verbs remain on Tracker. Accordingly every
//     task, edge, label, and comment this importer writes is a Session verb committed
//     under a bound (actor, authority): the importer's own fixed software agent as the
//     committing actor, and a genesis bootstrap authority as the governing authority.
//
//   - Genesis discipline. A Session verb against an empty journal is ErrGenesisRequired.
//     The importer therefore establishes (or, on re-import, recovers) a genesis
//     bootstrap authority via Tracker.Journal() before binding its Session. Recovery
//     uses a pinned OperationID and Journal().LookupCommitted, which returns the
//     already-committed authority's produced JournalID without re-executing — verified
//     empirically (a second LookupCommitted returns CommittedExact with the auth slot).
//
//   - StartActivityWithID idempotency — re-validated against the journal model. Claim:
//     a second call with the same ActivityID is a no-op returning the existing row.
//     FINDING: the claim holds, and it is journal-INDEPENDENT. Activities are a
//     direct-write projection (INSERT … ON CONFLICT(id) DO NOTHING), the same
//     non-journaled projection category as agents (documented in Session.QualifyDerivation's
//     journaling judgment call), so ON CONFLICT DO NOTHING is a genuine no-op regardless
//     of journal state. The importer relies on this for deterministic activity emission.
//     (Tests TestImport_Idempotent and the probe that preceded this package confirm it.)
//
// # Idempotency model
//
// A full re-import over an existing store is a true no-op — no duplicated rows and no
// growth of the journal — achieved by deterministic IDs plus public-API existence
// checks (detect-and-skip), never by direct INSERT OR IGNORE into internals:
//
//   - Tasks carry a DETERMINISTIC TaskID (UUIDv5 of the bd id under a fixed namespace),
//     created through Session.Atomic(op.CreateTask(detID, …)) with a pinned OperationID.
//     A task that already exists (Tracker.Show) is skipped, so create is never re-journaled.
//   - Lifecycle (Start/Close) is applied only when the current status differs from the
//     target — detect-and-skip on Task.Status.
//   - Edges are added only when Tracker.Edges does not already carry (source→target,kind).
//   - Labels are added only when Tracker.Labels does not already carry the label.
//   - Comments are added only when no existing comment on the task has the same
//     (author, body) — bd comments carry no stable provenance CommentID, so identity is
//     by content+author (documented heuristic; collision = two identical comments by the
//     same author, treated as one).
//   - Activities use StartActivityWithID with a deterministic ActivityID (ON CONFLICT DO
//     NOTHING).
//   - Agents are deduplicated: the importer's software agent via the idempotent
//     RegisterFixedSoftwareAgent; human agents by (namespace, name) against Tracker.AllActors.
//   - Genesis is recovered via the pinned-OperationID LookupCommitted path above.
//
// # Mapping (the contract)
//
//   - bd issue        → Task (prov:Entity). TaskID = UUIDv5(bd id).
//   - status          → Status: open→Open, in_progress→InProgress, closed→Closed.
//   - priority (0..4) → Priority (critical..backlog); out-of-range clamped and reported.
//   - issue_type      → TaskType: bug/feature/task/epic/chore map 1:1; unknown → task + report.
//   - phase           → PhaseUnscoped for every task (bd has no phase concept; documented).
//   - labels          → Task labels verbatim; plus a "bd:assignee:<slot>" label when an
//     issue has an assignee (see the actor judgment call below).
//   - dependency kind → edge kind: blocks→EdgeBlockedBy, discovered-from→EdgeDiscoveredFrom.
//     parent-child/related are documented as unmapped (absent in the surveyed corpus);
//     an unknown kind is skipped and reported, never coerced.
//   - created_by      → HumanAgent (prov:Person); one per distinct name. The task is
//     EdgeAttributedTo its creator.
//   - owner (email)   → contact on the creator's HumanAgent when a single owner value
//     co-occurs for that creator (true here: one person, one email).
//   - comment authors → HumanAgent (same dedup); the comment records its author.
//   - lifecycle       → one Activity per issue, acted by the IMPORTER software agent
//     (the true historical actor is unknown for automated bd slots; the importer is the
//     honest acting agent per the design contract), stage derived from status; EdgeGeneratedBy
//     from the task to that activity.
//   - supersession    → NOT emitted. bd carries no unambiguous supersession relation
//     (its dependency kinds are only blocks/discovered-from; title/notes conventions are
//     ambiguous), so per the vocabulary's never-fabricate rule this importer never fabricates supersedes edges
//     or derivation qualifiers.
//
// # Actor judgment call (documented)
//
// bd `assignee` values in the surveyed corpus are pasture execution-slot labels
// ("worker-slot-S2", "architect", …), not identifiable persons, software, or models.
// Minting an agent for them would fabricate an agent KIND the data does not support, so
// the importer preserves the assignment as a "bd:assignee:<slot>" label on the task
// rather than as an agent identity. The only attributed agent is the issue's human
// creator (EdgeAttributedTo), which the data supports truthfully.
//
// # Known limitations
//
// These are deliberate, documented gaps — stated here so they are known rather than
// discovered downstream:
//
//   - Exported Turtle is NOT byte-reproducible across stores. Human agents are registered
//     with UUIDv7 ids (RegisterHumanAgent mints them; there is no fixed-id human path), so
//     a fresh store assigns different agent IRIs, and each lifecycle Activity's
//     prov:startedAtTime is the import wall-clock (StartActivityWithID takes no caller
//     timestamp). The IMPORT is idempotent within a store — the same store re-imported is a
//     no-op — but two independent stores built from identical bd input yield graphs that
//     differ in human-agent IRIs and activity timestamps. Task/edge/comment identity is
//     stable (deterministic UUIDv5), so the graph is semantically equivalent, not
//     byte-identical. The committed testdata/dogfood/graph.ttl is therefore a one-time
//     conformance witness, not a byte-golden the exporter is re-diffed against.
//
//   - Historical bd timestamps are NOT preserved. The journaled Session verbs (Create,
//     AddComment) and StartActivityWithID accept no caller-supplied time, so bd's
//     created_at / updated_at / closed_at and each comment's created_at are dropped: the
//     provenance timeline reflects IMPORT time, not the original bd history time. Faithful
//     historical timelines would need timestamp-carrying write verbs upstream (a provenance
//     library change out of scope here); until then the imported graph answers "who /
//     what / how", not "exactly when in bd history".
package bdimport
