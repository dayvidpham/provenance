# PROPOSAL-3: Per-Activity Qualified Association

**References:**
- The one remaining BREAKING model gap in the data-labour integration work (§1)
- Depends on: the plan-layer + derivation-qualifier change — `provenance-11--feat--data-labour-integration` (PR #17)
- Prior art in-repo: `Session.QualifyDerivation`, the journaled mutation families, `task_attributions`

**Status:** breaking-change design, submitted to this repo's own review cycle. It contradicts a documented design decision (`pkg/ptypes/types.go:249` — "Role stays on the agent"), so it lands as a ratified proposal, not a fait accompli.

**Changes from the current model (PR #17 HEAD):** introduces an `Association` value and an `associations` table modelling `prov:qualifiedAssociation` per activity; moves `Role` off the ML agent onto the association and extends it to all agent kinds; back-fills one association per legacy activity; deprecates `Activity.AgentID`-as-role-carrier and `RegisterMLAgent`'s `role` parameter over one minor version.

---

## 1. Problem Space

The plan-layer + derivation-qualifier change delivered the plan layer (§3.1) and the typed derivation qualifier (§3.3). The remaining model gap the consuming project — the data-labour provenance vocabulary — exposes in this library is **role granularity**. Role today is a property of the *agent*, not of the *act*. This has three concrete consequences, visible right now in the exporter's own output:

1. **One registered agent cannot be worker in one activity and reviewer in another.** `agents_ml.role_id` is a single column keyed by agent identity (`internal/sqlite/db.go:289`). The documented workaround is "same model with different roles = different registrations" (`pkg/ptypes/types.go:249`), i.e. register the model twice. That splits one actor's history across two identities and corrupts any per-actor attribution.

2. **Human and software agents cannot carry a role at all.** `Role` lives only on `agents_ml`; `agents_human` and `agents_software` have no role column (`db.go:288,290`). A human reviewer and a software linter are role-less in every export.

3. **The `prov:qualifiedAssociation` we export is not truthful.** The current exporter already emits a `prov:qualifiedAssociation` node for ML-agent activities, but synthesizes `prov:hadRole` from `MLAgent.Role` at *agent* level and says so in a loud caveat (`pkg/provo/provo.go:278-282, 385-391`):

   > `# NOTE (role granularity): prov:hadRole is synthesized at AGENT level from MLAgent.Role. Until the qualified-association layer lands, role is a property of the acting ML agent, not of the specific act …`

   Only ML-agent activities are typed `p-plan:Activity` because only they can furnish the agent+role association the data-labour-prov vocabulary's `AnnotationActShape` requires. Human/software activities are silently second-class.

Per-activity roles are what make `prov:qualifiedAssociation` exports truthful: the association is `(activity, agent, role, plan)`, precisely the shape the ecosystem specifies but — as CWLProv's unimplemented association roles show — routinely omits. Closing this gap is what lets *every* activity, regardless of agent kind, carry the qualified association its export needs.

### Axes of the Problem

| Axis | Assessment |
|------|-----------|
| **Parallelism** | None new. The association is a satellite projection of an activity; writes serialize on the same single-writer SQLite path as everything else. |
| **Scale** | O(activities × agents-per-activity). In practice ≤ a few associations per activity; well under the < 10K-entity envelope. |
| **Entity relationships** | Has-a: an Activity *has* zero-or-more Associations. Is-a: an Association *is* a reified `prov:qualifiedAssociation`. The association is the reification of the many-to-many (activity, agent) relation qualified by role and plan. |
| **Domain novelty** | Low-to-medium. `prov:qualifiedAssociation` is a well-defined PROV-O pattern; the novelty is entirely in the write-path placement (journaled vs projection) against this repo's journal contract. |
| **Blast radius** | One documented design reversal, one lookup-table extension, one new table, one back-fill migration, a two-release deprecation of the old role carriers. No wire-format change to the journal (see §5). |

---

## 2. The Documented Design Intent, Steel-manned

The current model is a deliberate decision, recorded verbatim on the type:

```go
// MLAgent represents a machine learning model acting as an agent.
// Role stays on the agent: same model with different roles = different registrations.
type MLAgent struct {
    Agent
    Role  Role    `json:"role"`
    Model MLModel `json:"model"`
}
```
— `pkg/ptypes/types.go:248-254`; ratified as UAT decision #6 in PROPOSAL-2 §12 ("Role on ML Agent … cleaner than attaching role to every activity, and matches how agents actually work in the protocol").

**The steelman.** Attaching role to the agent is the simpler model and it was the *right* call for PROPOSAL-2's scope:

- **It matches how the Aura protocol actually spawns agents.** A worker agent and a reviewer agent are launched as distinct processes with distinct system prompts. Modelling them as distinct registrations mirrors operational reality — they really are two different actors from the orchestrator's point of view.
- **It keeps `agents_ml` in BCNF with no nullable columns.** `agent_id → (role_id, model_id)` is a clean functional dependency. Role is a property of the registration and always present.
- **It avoids per-activity role bookkeeping** in a system whose original scope (task dependency tracking) never needed activity-level attribution at all. Roles-on-activities would have been dead weight in PROPOSAL-2.
- **Registration is cheap.** Registering `claude-opus` twice (once as worker, once as reviewer) costs two rows. If roles rarely cross within one model, the "double registration" objection is theoretical.

This proposal does not claim the original decision was wrong for its scope. It claims the scope changed: the library is now the reference PROV-O exporter of a provenance vocabulary whose central artifact is the qualified association, and against *that* requirement the agent-level model is structurally unable to produce a truthful export.

### 2.1 Why the reversal — the three payoffs, and the costs

| Payoff | Agent-level (today) | Per-activity (proposed) |
|---|---|---|
| Roles for humans/software | Impossible — no role column outside `agents_ml` | An association carries a role for **any** agent kind |
| One registration across roles | "Register twice"; splits one actor's identity/attribution | One registration; role is chosen per activity |
| Truthful `prov:qualifiedAssociation` export | Synthesized at agent level, flagged as such in a caveat; only ML activities are `p-plan:Activity` | Exported at act level, no caveat; every activity can furnish its association |

**The costs, stated plainly:**

- **A documented reversal.** UAT decision #6 is overturned. That is a governance cost, paid by routing this through the review cycle rather than committing it.
- **`agents_ml.role_id` becomes redundant, then is removed.** A two-release deprecation window (§6) — during which role lives in *both* places for legacy readers — is added maintenance surface.
- **A new nullable-free satellite table and a back-fill migration** must ship and be replay/convergence-audited.
- **Multi-agent activities become expressible.** An activity can now name several agents with several roles. That is a feature, but it means `Activity.AgentID` (a single FK) is no longer the whole story; readers that assumed one-agent-per-activity must migrate (§6.3).

The judgment: the export-truthfulness payoff is not incremental — without it the library cannot honestly claim to be the vocabulary's reference implementation for the one PROV-O feature the vocabulary is *about*. That clears the bar for a documented reversal.

---

## 3. The Association Shape

```go
// Association reifies prov:qualifiedAssociation: the fact that Actor acted in
// Activity in a given Role, optionally following a Plan (prov:hadPlan). It is the
// per-activity replacement for the agent-level MLAgent.Role, extended to ALL agent
// kinds (human, ML, software). An activity may carry several associations (several
// agents, several roles); an agent may appear in many activities in different roles.
type Association struct {
    ActivityID ActivityID  `json:"activityId"` // ≙ prov:Activity
    ActorID    ActorID     `json:"actorId"`    // ≙ prov:agent (canonical spelling; AgentID is the deprecated alias)
    Role       Role        `json:"role"`       // ≙ prov:hadRole — moves here; valid for every agent kind
    PlanID     *PlanID     `json:"planId,omitempty"` // ≙ prov:hadPlan — the guideline THIS agent followed; nil = none
}
```

Naming follows the completed `AgentID → ActorID` rename (`pkg/ptypes/types.go:85-116`; `AgentID` is a one-release alias). An earlier design sketch wrote `AgentID`; this proposal uses `ActorID` throughout and keeps `AgentID` working only through the existing alias.

### 3.1 Schema

```sql
-- FD: (activity_id, actor_id, role_id) → plan_id   (composite PK)
-- associations ≙ prov:qualifiedAssociation, one row per (activity, actor, role).
-- Satellite of activities (same non-journaled projection category — see §5).
CREATE TABLE IF NOT EXISTS associations (
    activity_id TEXT    NOT NULL REFERENCES activities(id),
    actor_id    TEXT    NOT NULL REFERENCES agents(id),
    role_id     INTEGER NOT NULL REFERENCES roles(id),
    plan_id     TEXT    REFERENCES plans(id),        -- NULL = no guideline plan (prov:hadPlan absent)
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (activity_id, actor_id, role_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_associations_actor ON associations (actor_id);
CREATE INDEX IF NOT EXISTS idx_associations_role  ON associations (role_id);
```

This mirrors the `derivation_qualifiers` DDL exactly (`db.go:332`): a satellite table with composite PK, FK to its parent projection, integer role FK to the already-seeded `roles` lookup (`db.go:281`), and an explicit `created_at`. No new lookup table is needed — `roles` already carries `human/architect/supervisor/worker/reviewer` (`enums.go:425-433`); the reversal simply lets every agent kind reference it, not just `agents_ml`.

**PK choice — `(activity_id, actor_id, role_id)` vs `(activity_id, actor_id)`.** An earlier design sketch put `role_id` in the PK, which permits one actor to hold *multiple* roles in a single activity (e.g. an agent that both authored and self-reviewed). That is faithful to PROV — an activity may carry two `prov:qualifiedAssociation` nodes for the same agent with different `prov:hadRole`. The alternative `(activity_id, actor_id)` forbids that and makes "the role of actor X in activity Y" a function. This proposal keeps that sketch's `(activity_id, actor_id, role_id)` PK and raises the single-role-per-actor question to reviewers (§9, Q1).

**`plan_id` on the association vs `plan_id` on the activity.** PR #17 added `activities.plan_id` (`db.go:342`), the plan the activity *corresponds to* (`p-plan:correspondsToStep` resolves against it — `provo.go:376-382`). The association's `plan_id` is `prov:hadPlan`: the guideline *this agent* followed in this act, which can differ per agent within one activity. They coincide for the built-in `pasture-12-phase` default but are semantically distinct (structural correspondence vs per-agent guideline). Both are retained; §9 Q2 asks reviewers whether the distinction earns its keep or the association should simply inherit `activities.plan_id` when unset.

### 3.2 Exporter change (the payoff, realized)

The synthesized block in `activitiesSection` (`provo.go:383-392`) is replaced by a per-association emit: for each `Association` of the activity, emit a `prov:qualifiedAssociation [ a prov:Association ; prov:agent <actor> ; prov:hadRole <role> ; prov:hadPlan <plan> ]`. The agent-level caveat in the header (`provo.go:278-282`) is deleted; `p-plan:Activity` typing is extended to any activity carrying an association, not only ML ones. `roleToken`/`roleIRI` (`provo.go:578-579`) are reused verbatim. This section is a pure read over the new table and is out of scope for this proposal's write path but is stated here as the acceptance target: **the exporter emits no role-granularity caveat once this proposal lands.**

---

## 4. `task_attributions`: adjacent prior art, not a substitute

The release added a `task_attributions` projection (`journal.go:276-282`):

```go
// TaskAttribution is one append-only cumulative attribution edge (§8.2): the
// earliest journal row establishing an actor's material contribution to a task.
type TaskAttribution struct {
    TaskID         TaskID
    ActorID        ActorID
    FirstJournalID JournalID
}
```

It is genuinely related — it is a *who-provenance* projection over actors — and a reviewer might reasonably ask why this proposal does not simply lean on it. It is **not** a substitute, for four precise reasons:

1. **Wrong subject.** `TaskAttribution` keys on `(task, actor)`. An association keys on `(activity, actor, role)`. Attribution answers "who materially touched this task"; an association answers "who acted in this activity, in what role." An activity need not even have an associated task (`EdgeGeneratedBy` is optional), so there is no task to hang the attribution on.
2. **No role.** `TaskAttribution` has no role field and no room for one — it is the *cumulative* set of contributing actors, deliberately role-blind (§8.2). Role granularity is the entire point of this proposal.
3. **No plan.** No `prov:hadPlan` seat.
4. **Cumulative, not per-act.** It records the *earliest* journal position an actor touched a task and folds all later touches away. An association is per-activity and must distinguish an actor's worker-act in activity A from the same actor's reviewer-act in activity B — exactly the fold `task_attributions` collapses.

So `task_attributions` stays as-is (it feeds `prov:wasAttributedTo`, a *derived* edge in the data-labour-prov vocabulary's model); the association feeds `prov:qualifiedAssociation`, a distinct and richer PROV relation. They are complementary projections, cited here so reviewers do not mistake one for the other.

---

## 5. Write-Path Design

This is the load-bearing decision, and it is constrained by the journal contract (`docs/journal-relational-contract.md`) that landed in the release. Three options were considered against two hard constraints — the **sealed V1 canonical-mutation codec** (`internal/journal/canonical_mutation.go`, `MutationEncodingV1`) and the **§15 replay/convergence set** — plus the TOCTOU lesson from `QualifyDerivation` (PR #17).

### 5.1 The three options

**(A) Full journaled mutation family.** A new `EffectAssociationAdd`/`EffectAssociationRemove`, a per-family `EventKind`, a V1 codec descriptor with encode/decode, a replay projection into `associations`, and corpus enum-freshness tests. This is what the §6 relationship/annotation families do (`mutation_families.go`).

**(B) `QualifyDerivation` event-gates-projection shape.** Journal one who-provenance event through the same `Apply` path, then direct-write the projection into a §15-excluded satellite table, with authorization-time pre-checks before journaling (`session.go:584-679`).

**(C) Pure satellite direct-write projection.** Write the `associations` row the same way activities, plans, agents, and — for its *projection half* — derivation qualifiers are written: a direct `INSERT` into a non-journaled satellite table, gated by pre-write validation, in one transaction. No journal event.

### 5.2 Recommendation: (C), on `Tracker`, not `Session`

**This proposal writes associations as a pure satellite direct-write projection, exposed as `Tracker` methods (parallel to `StartActivity`/`RegisterMLAgent`), not as journaled `Session` verbs.** An earlier design assumption held that these association writes must be journaled `Session` verbs; this proposal revises that (§5.4).

The reasoning, against the constraints:

1. **An association qualifies an activity, and activities are *not journaled.*** `StartActivity` is a `Tracker` method (`provenance.go:160`), not a `Session` verb, and its rows land in the direct-write projection category. `QualifyDerivation`'s own doc comment names this category explicitly: "the same non-journaled projection category as activities/agents, which §15 convergence already excludes" (`session.go:604-606`). An association is activity metadata; symmetry demands it inherit the activity's write treatment. Journaling an association while its parent activity carries no who-provenance would be half-provenance — incoherent.

2. **The journal's authorization model has no seat for an association.** Per-effect authorization is "an authority *governs a task*" (§9.3; `AuthorityGovernsTaskAt(authJID, task, beforeJID)` — `journal_api.go:265`). Every journaled effect — including `QualifyDerivation`'s who-provenance event — anchors on a `TaskID` and is authorized against it. An association references `(activity, actor)` and *no task*. There is no task to authorize it against. This blocks options (A) and (B) at the authorization layer, not merely the codec layer: even a full mutation family (A) would have nothing to govern it. `QualifyDerivation` escaped this only because a derivation qualifier has a natural *source task* to anchor on (`session.go:667-672`); an association does not.

3. **The V1 codec is sealed and §15 is a wire-format risk.** Option (A) is exactly the change `QualifyDerivation` rejected as "a large, wire-format-risky change … extending the VERSIONED canonical-mutation V1 codec … the §15 replay shadow-table / convergence set … and the corpus enum-freshness tests" (`session.go:590-599`). This proposal has *less* justification for it than `QualifyDerivation` did, because `QualifyDerivation` at least had a task anchor; an association has none. Adding a journaled family for a task-less entity would either force a fake task anchor or a new authority kind — both far larger than this proposal.

4. **Direct-write *eliminates* the TOCTOU residual rather than inheriting it.** `QualifyDerivation` notes a two-transaction residual: the who-provenance event commits in txn #1, the projection in txn #2, so a crash between them leaves the journal recording a qualification whose projection never materialized (`session.go:632-649` mitigates but does not remove it). Option (C) has **no journal event**, so there is no two-txn split for the association at all — the `associations` row is written in a single transaction, and (for the create path, §5.3) the *same* transaction as the activity it qualifies. The TOCTOU residual is designed out, not carried forward.

The one thing (C) gives up relative to (B) is a journaled record of *who recorded the association row*. That provenance is genuinely absent — but it is *already* absent for activities (`StartActivity` records the actor the activity is *for*, never who invoked it). The data-labour-prov vocabulary's truthfulness requirement is about the *content* of the association (per-activity role), which (C) delivers in full; "who recorded the row" is a separate concern the model does not track for activities either, and this proposal does not regress it.

### 5.3 The verbs

Two write surfaces, both on `Tracker`, both direct-write, both validate-before-write (the A1 lesson — reject bad operands *before* the single write, so nothing partial lands):

```go
// StartActivity / StartActivityWithID gain a trailing association option so an
// activity records its qualified association(s) in the SAME transaction as its own
// row — no cross-table TOCTOU on the create path.
func WithAssociation(actor ActorID, role Role, plan *PlanID) StartActivityOption

// AssociateActivity attaches (or re-attaches) an association to an EXISTING activity
// — the back-fill and the multi-agent-after-the-fact path. Validates activity/actor
// existence, role validity, and plan existence before the single INSERT ... ON
// CONFLICT(activity_id, actor_id, role_id) DO UPDATE. Idempotent by PK.
func (t Tracker) AssociateActivity(activityID ActivityID, actor ActorID, role Role, plan *PlanID) error

// Associations reads every association of an activity, ordered deterministically by
// (actor_id, role_id) — the exporter's read surface.
func (t Tracker) Associations(activityID ActivityID) ([]Association, error)
```

`WithAssociation` composes with the existing `InPlan`/`Unplanned` plan options (`activity_plan.go:12-51`) — it is a peer `StartActivityOption`, so the additive-options convention PR #17 established is preserved and no existing `StartActivity` call site breaks. The pre-write validation mirrors `QualifyDerivation`'s activity/edge gates (`session.go:619-649`), keeping the projection honest without a journal event to keep honest.

### 5.4 Consequence for the earlier framing

An earlier design assumption held that `QualifyDerivation`, these association writes, and the bd-import path must all be `Session` verbs. For `QualifyDerivation` that resolved to a Session verb whose *projection half* is a direct write. For association writes it does **not** hold: an association has no source task, so it cannot be a journaled Session verb at all under the current authorization model, and — because its parent activity is a non-journaled `Tracker` write — it should not pretend to be. **Association writes belong on `Tracker`, alongside `StartActivity`.** This is the single point where this proposal revises that earlier framing.

---

## 6. Migration & Deprecation

### 6.1 Back-fill (one association per legacy activity)

On the migration that ships this proposal, for every existing activity, insert one association from the activity's current `(actor, role)`:

```sql
-- One association per legacy activity: actor = activities.agent_id,
-- role = the actor's agents_ml.role_id (ML agents only — legacy activities are
-- ML-agent acts, the only kind that carried a role). plan_id = activities.plan_id
-- (the default built-in plan for planned activities, else NULL).
INSERT OR IGNORE INTO associations (activity_id, actor_id, role_id, plan_id, created_at)
SELECT a.id, a.agent_id, m.role_id, a.plan_id, a.started_at
FROM   activities a
JOIN   agents_ml  m ON m.agent_id = a.agent_id;
```

Non-ML legacy activities (if any exist) have no `agents_ml` row and are skipped — they had no role to preserve, and after this change they can be associated explicitly. The back-fill is idempotent (`INSERT OR IGNORE` on the PK) and re-runnable, matching the schema's every-open idempotence discipline (`db.go:56`).

### 6.2 Deprecation timeline (one minor version)

| Release | `agents_ml.role_id` | `RegisterMLAgent(role …)` | `associations` |
|---|---|---|---|
| N (this proposal) | kept, still written, **deprecated** | `role` param kept, **deprecated**; also writes an association-less registration | authoritative for export; back-filled |
| N+1 | dropped (migration removes the column) | `role` param removed; new `RegisterMLAgent(namespace, provider, modelName)` | sole role carrier |

During release N both carriers coexist so existing readers of `MLAgent.Role` keep working. `RegisterMLAgent`'s `role` parameter, when supplied in release N, is recorded on `agents_ml` as today *and* is available to seed a default association — but the association is now chosen per activity, so the recommended path is `RegisterMLAgent(…)` once + `WithAssociation`/`AssociateActivity` per act.

### 6.3 Back-compat guarantees for existing readers

- **`MLAgent.Role` keeps returning a value through release N.** `GetMLAgent` (`agents.go:208-240`) is unchanged in N; the field is populated from `agents_ml.role_id` exactly as today. In N+1 the field is removed from the struct (a source-breaking change gated on the minor-version bump, announced in N).
- **`Activity.AgentID` stays a single FK and keeps meaning "the primary actor."** It is not removed. Multi-agent activities add *associations*; they do not repurpose `Activity.AgentID`. Readers that only ever read `Activity.AgentID` continue to see the primary actor. (Whether `Activity.AgentID` should eventually become derived-from-associations is deferred — §9 Q3.)
- **The journal is untouched.** No new effect sorts, no codec version bump, no replay/convergence change (§5). A database written by release N replays byte-identically to one written by PR #17 for every journaled projection; `associations` is outside the convergence set, exactly like `activities` and `derivation_qualifiers`.

---

## 7. Implementation Slices

Ordered by dependency; each independently testable.

### Slice 1: Type + schema foundation
- `pkg/ptypes/types.go` — `Association` struct.
- `internal/sqlite/db.go` — `associations` table + indexes in `ensureSchema` (idempotent).
- Extend `Role` usage: no enum change (values already seeded); document that `roles` is now referenced by all agent kinds via associations.
- **Exit:** schema idempotent; `Association` round-trips through the sqlite scan; `go test -race ./...` green.

### Slice 2: Write path (direct-write projection)
- `internal/sqlite/associations.go` — `InsertAssociation` (validate-before-write; `INSERT … ON CONFLICT(activity_id,actor_id,role_id) DO UPDATE SET plan_id, created_at`), `AssociationsByActivity`.
- `activity_plan.go` — `WithAssociation` option; `StartActivity`/`StartActivityWithID` write the association in the same txn as the activity row.
- `provenance.go` (Tracker) — `AssociateActivity`, `Associations`.
- **Exit:** create-path association is single-txn with the activity; `AssociateActivity` is idempotent and rejects unknown activity/actor/plan and invalid role *before* writing; `-race` green.

### Slice 3: Back-fill migration
- `internal/sqlite/migration.go` (or the seed path) — the §6.1 back-fill, idempotent, re-runnable.
- **Exit:** a database written by PR #17, opened by this proposal, has exactly one association per ML-agent activity, matching the pre-migration `(agent, role)`; re-opening is a no-op; replay/convergence over the journal is unchanged (`ReplayProjections` still converges).

### Slice 4: Exporter truthfulness (read-only; may land with this proposal or follow)
- `pkg/provo/provo.go` — emit per-association `prov:qualifiedAssociation`; extend `p-plan:Activity` typing to any activity with an association; delete the agent-level role caveat.
- **Exit:** the SHACL suite (`shacl validate --shapes ontology/shapes.ttl`) passes with associations sourced from the table; the output contains no "role granularity" caveat; `AnnotationActShape`/`AnnotationActShape` binds for human and software activities too.

---

## 8. Test Strategy

Same principles as PROPOSAL-2 §10: integration over unit, real `:memory:` SQLite via `OpenMemory`, `-race` everywhere.

### 8.1 BDD Acceptance Criteria

**Scenario: one actor, two roles, two activities**
- **Given** one registered ML agent X and two activities A1, A2
- **When** `WithAssociation(X, RoleWorker, …)` records A1 and `AssociateActivity(A2, X, RoleReviewer, nil)` records A2
- **Then** `Associations(A1)` yields role worker and `Associations(A2)` yields role reviewer for the *same* actor X
- **Should not** require a second registration of X

**Scenario: human agent carries a role**
- **Given** a registered human agent H and an activity A
- **When** `AssociateActivity(A, H, RoleReviewer, nil)` is called
- **Then** `Associations(A)` yields (H, reviewer)
- **Should not** reject the association for H lacking an `agents_ml` row

**Scenario: create-path association is atomic with the activity**
- **Given** a fault injected between the activity insert and the association insert
- **When** `StartActivity(..., WithAssociation(...))` runs and the fault fires
- **Then** neither the activity row nor the association row is present (single transaction)
- **Should not** leave an activity with a missing intended association (no cross-table TOCTOU)

**Scenario: unknown operands rejected before write**
- **Given** an activity A
- **When** `AssociateActivity(A, unknownActor, RoleWorker, nil)` (or unknown plan, or invalid role) is called
- **Then** an actionable `ErrNotFound`/validation error is returned and *no* row is written
- **Should not** insert a partial or dangling association

**Scenario: back-fill preserves legacy role**
- **Given** a database written by PR #17 with an ML-agent activity whose agent had `RoleWorker`
- **When** this change opens it and runs the back-fill
- **Then** exactly one association `(activity, agent, worker, activity.plan_id)` exists
- **Should not** create duplicates on a second open (idempotent)

**Scenario: journal replay unaffected**
- **Given** a database with activities and associations
- **When** `ReplayProjections()` folds the whole journal
- **Then** it converges (associations are outside the convergence set, like activities)
- **Should not** report `ErrProjectionDivergence`

**Scenario: exporter emits truthful per-act role**
- **Given** an activity with two associations (two agents, two roles)
- **When** `ExportTurtle` runs
- **Then** two `prov:qualifiedAssociation` nodes are emitted with distinct `prov:hadRole`, and no agent-level role caveat appears
- **Should not** synthesize role from `MLAgent.Role`

---

## 9. Open Questions (for reviewers)

1. **Single role per actor per activity?** The PK `(activity_id, actor_id, role_id)` permits one actor to hold several roles in one activity (faithful to PROV's multiple `qualifiedAssociation` nodes). Should we instead enforce one role per actor via PK `(activity_id, actor_id)`? Recommendation: keep the three-column PK; the extra generality is free and PROV-faithful.

2. **Association `plan_id` vs activity `plan_id`.** PR #17 put a plan on the activity (`correspondsToStep`); this proposal also puts one on the association (`prov:hadPlan`). Keep both (distinct PROV semantics), or let the association inherit `activities.plan_id` when unset and drop the column? Recommendation: keep both but default the association's to the activity's — decide the defaulting rule here.

3. **Should `Activity.AgentID` eventually become derived from associations?** This proposal keeps it as the "primary actor" FK for back-compat. A later release could make it a view over "the association with the authoring role," fully unifying the model. In or out of scope for the deprecation window?

4. **Who-recorded-the-association provenance.** Option (C) records no journaled who-provenance for the association (§5.2), consistent with activities. If reviewers want that provenance, the honest way is to *first* give activities who-provenance (a larger change), then associations inherit it — not to bolt half-provenance onto associations alone. Confirm this is acceptable for this proposal.

5. **Role for software agents in the seed.** `roles` seeds `human/architect/supervisor/worker/reviewer`. Do software agents need a distinct role value (e.g. `tool`), or do they reuse the existing set? No schema change either way; a seed decision.

---

## 10. Non-Goals

- **No journal wire-format change.** No new effect sort, no `MutationEncodingV1` descriptor, no §15 convergence-set change. (This is a hard constraint, §5.)
- **No new authority kind.** This proposal does not invent activity-authority to make associations journaled.
- **No change to `task_attributions`.** It stays as the cumulative `prov:wasAttributedTo` source (§4).
- **No exporter w3id/namespace change.** The vocabulary namespace stays the placeholder pending w3id registration.
- **No `Activity.AgentID` removal.** It stays a single FK; multi-agent is additive via associations.
- **No edits to consuming repositories.** This proposal is provenance-repo-local; the exporter change consumes existing `ontology/` shapes unchanged.

---

## 11. Engineering Tradeoffs

| Decision | Options | Choice | Rationale |
|---|---|---|---|
| Role placement | Agent-level (today) vs per-activity association | Per-activity association | Only per-act roles make `prov:qualifiedAssociation` truthful and give humans/software roles; overturns UAT #6 with cause (§2). |
| Write path | Full mutation family (A) vs QualifyDerivation shape (B) vs satellite direct-write (C) | (C) direct-write on `Tracker` | Associations qualify non-journaled activities and have no source task to authorize/anchor a journal event; (A)/(B) are blocked at the authorization layer, not just the sealed codec (§5.2). |
| Verb host | `Session` (journaled) vs `Tracker` (projection) | `Tracker` | Symmetry with `StartActivity`/`RegisterMLAgent`, the non-journaled writes an association is a satellite of; revises the earlier "must be Session verbs" framing (§5.4). |
| TOCTOU | Inherit QualifyDerivation's two-txn residual vs design it out | Design out | No journal event → single-txn association; create-path folded into the activity's txn (§5.2.4). |
| Create-path capture | Separate call vs `StartActivity` option | `WithAssociation` option | Atomic with the activity row; preserves PR #17's additive-option convention (`activity_plan.go`). |
| PK | `(activity, actor)` vs `(activity, actor, role)` | `(activity, actor, role)` | PROV-faithful (multiple roles per actor per act); from the earlier sketch; raised as Q1. |
| Lookup table | New role table vs reuse `roles` | Reuse `roles` | Values already seeded; reversal only widens who may reference it. |
| Migration | Lazy vs eager back-fill | Eager, idempotent | One association per legacy activity at open; re-runnable; leaves journal replay untouched (§6.1). |

---

## Appendix A: File Touch-List

| File | Change |
|---|---|
| `pkg/ptypes/types.go` | `Association` struct |
| `internal/sqlite/db.go` | `associations` table + indexes in `ensureSchema`; N+1: drop `agents_ml.role_id` |
| `internal/sqlite/associations.go` (new) | `InsertAssociation`, `AssociationsByActivity` (validate-before-write) |
| `internal/sqlite/migration.go` | §6.1 back-fill |
| `activity_plan.go` | `WithAssociation` `StartActivityOption` |
| `provenance.go` | `AssociateActivity`, `Associations` on `Tracker` |
| `pkg/provo/provo.go` | per-association `prov:qualifiedAssociation`; drop role caveat; widen `p-plan:Activity` typing |
| `CONCEPTS.md` | document the association layer; correct the "Role stays on the agent" note |
| `docs/PROPOSAL-3.md` | this document |

## Appendix B: Provenance of the Design Constraints

Every constraint this proposal reasons against is quoted from the tree it branches off (`provenance-11--feat--data-labour-integration` (PR #17), HEAD `ee4e837`):

- "Role stays on the agent: same model with different roles = different registrations" — `pkg/ptypes/types.go:249`.
- The exporter's own agent-level role caveat — `pkg/provo/provo.go:278-282, 385-391`.
- The non-journaled projection category (activities/agents) `§15` excludes, and the sealed V1 codec / corpus rationale for *not* extending the mutation families — `session.go:590-606` (`QualifyDerivation`'s judgment call).
- The two-txn TOCTOU residual and its A1 mitigation — `session.go:632-649`; commit `ee4e837` ("gate QualifyDerivation on activity existence [A1]").
- Task-centric authorization (`AuthorityGovernsTaskAt(auth, task, before)`) — `journal_api.go:265`.
- `task_attributions` shape — `internal/journal/journal.go:276-282`.
- `derivation_qualifiers` DDL this schema mirrors — `internal/sqlite/db.go:332`.
</content>
</invoke>
