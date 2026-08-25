# Attribution And Authorization Design

This document explains why Provenance separates attribution, authorization,
delegation, and causal ordering, and how those concerns constrain task
allocation. It is an explanatory design guide. The normative storage and
authorization rules remain in
[`journal-relational-contract.md`](journal-relational-contract.md), especially
sections 3, 4, 9, 11, and 14.

## Terms

| Concern | Question answered | Durable source |
|---|---|---|
| Attribution | Who committed the operation? | The operation anchor's `ActorID` |
| Authorization | Why was the operation allowed? | The operation's `AuthorityJournalID` |
| Responsibility | Who occupies a role for a task? | An assignment episode |
| Delegation | How does authority reach a child task? | An explicit `ParentAssignmentID` citation |
| Causal order | Was authority active when the effect committed? | Database-assigned `JournalID` order |
| Replay identity | Is this the same logical command? | Stable `OperationID` plus canonical input |

Attribution and authorization are deliberately different. A system process may
commit an operation attributed to its process actor while citing an assignment
authority, or it may commit on behalf of a selected human whose identity is
recorded as the operation actor. Neither representation should infer authority
from the actor's identity alone.

## Motivations, Invariants, And Constraints

The architecture flows from user-facing motivations to invariants that must
always hold. The invariants then produce concrete implementation constraints.

```text
MOTIVATIONS                         REQUIRED INVARIANTS
===========                         ===================

M1  Trustworthy attribution ------> I1  Exact actor attribution
    "Who actually acted?"               The committing actor is recorded once.
          |                              Produced rows derive that attribution.
          |
          +------------------------> I2  Attribution is not authorization
                                         Actor identity alone grants no power.

M2  Least-privilege mutation ------> I3  Exact governing authority
    "Why was this allowed?"              Every task-bearing effect is authorized
          |                              at its own journal position.
          |
          +------------------------> I4  Exact task scope
                                         Authority cannot leak through an
                                         unrelated task or scheduling edge.

M3  Safe delegation --------------> I5  Deliberate ownership reach
    "Who may govern child work?"         Cross-task authority requires an
          |                              explicit parent-assignment citation.
          |
          +------------------------> I6  Whole-chain liveness
                                         Every assignment in the parent chain
                                         must still be active.

M4  Atomic workflows -------------> I7  No partial mutation
    "Can races leave debris?"            Graph, evidence, events, activities,
          |                              and projections commit or roll back.
          |
          +------------------------> I8  No stale-authority TOCTOU
                                         Revocation ordered first rejects the
                                         operation; operation ordered first is
                                         completely committed.

M5  Reliable retries -------------> I9  Exact replay
    "Can callers retry safely?"          Same operation and canonical input
          |                              return the original complete result.
          |
          +------------------------> I10 Changed-input conflict
                                         Reusing an operation ID with different
                                         semantics cannot create new history.

M6  Maintainable evolution -------> I11 One authority model
    "Can future features compose?"       No application-specific ACL, second
                                         ownership graph, or shadow ledger.
                                         |
                                         v
IMPLEMENTATION CONSTRAINTS                WHY THEY ARISE
==========================                ==============

C1  One authority per Apply <------------- I3, I7, I9
C2  Bootstrap is the sole root <---------- I3, I11
C3  Assignment authority is task-scoped <- I4
C4  Scheduling edges grant no authority <- I4, I5
C5  Parent citations alone cross tasks <-- I5, I6
C6  Authorization uses JournalID order <-- I3, I6, I8
C7  Assignment start needs a real task <-- relational integrity
C8  Task allocation precedes ownership <-- task lifecycle integrity
C9  Replay runs before mutable preflight <- I9, I10
C10 Invalid or stale input writes nothing <- I7, I8
```

The arrows are intentional: constraints are not arbitrary implementation
preferences. They exist because weakening them would violate one or more
motivating invariants.

## Core Invariants

| Invariant | Required outcome |
|---|---|
| Least authority | Bootstrap authority does not become the normal authority for assignment-controlled work. |
| Exact scope | An assignment for one task cannot authorize an unrelated task merely because both appear in one workflow. |
| Explicit delegation | Authority crosses tasks only through deliberate parent-assignment citations. |
| Current authority | If assignment revocation commits first, the dependent operation writes nothing. |
| Atomic allocation | Task birth, ownership, graph edges, evidence, events, activities, and projections succeed or fail together. |
| Actor attribution | The selected actor remains visible independently of the authority that permitted the operation. |
| No privilege amplification | Creating a child cannot grant authority over unrelated existing tasks or an entire workflow. |
| Durable ownership | A generated task has an explicit, queryable explanation of who governs it. |
| Exact replay | A retry returns original task, assignment, event, activity, evidence, and result-slot identities. |
| Conflict detection | Changed canonical input under an existing operation ID returns a typed conflict. |
| Zero-write rejection | Invalid actor, authority, role, task, parent, epoch, or state leaves no partial rows. |
| One authority model | Applications consume Provenance authority instead of maintaining a parallel ACL or ownership ledger. |

## Authority Reach

Scheduling and ownership are separate graphs.

```text
Scheduling graph                         Authority graph
----------------                         ---------------

PLAN                                     assignment A: supervisor @ PLAN
  | blocked_by                                      |
  v                                                 | ParentAssignmentID
SLICE                                    assignment B: worker @ SLICE
  | blocked_by                                      |
  v                                                 | ParentAssignmentID
TASK                                     assignment C: helper @ TASK

Answers: "What must finish first?"       Answers: "Why may this mutate TASK?"

The left graph NEVER implies the right graph.
```

An assignment authority governs its own active task. It may govern another task
only when an active assignment on that task reaches it through explicit parent
citations and every episode in the chain remains active. Ending any intermediate
episode cuts the chain.

## The Allocation Boundary

Assignment-controlled allocation exposes a real dependency cycle:

```text
             task must exist
       +--------------------------+
       |                          v
  start assignment -----------> TASK
       ^                          |
       |                          | must already be governed
       +------ assignment authority

But:

  assignment authority cannot govern TASK before TASK exists
  assignment cannot start on TASK before TASK exists
```

Bootstrap authority can allocate the task because it is the authority base case.
An assignment authority can mutate the task only after deliberate ownership has
been established. A workflow that promises one atomic allocation command must
therefore define how allocation and delegated ownership are joined without
weakening stale-authority or least-privilege guarantees.

## Governed Allocation Requirements

Any governed-allocation design, whether represented by a narrowly constrained
bootstrap operation or a future first-class effect, must assure all of the
following:

1. The parent assignment is identified exactly, not inferred from a task or
   graph edge.
2. The parent assignment is active at the transaction's canonical journal
   position.
3. The operation actor is independently recorded and validated.
4. The child task identity is deterministic for replay.
5. Child ownership or delegation is created in the same transaction as task
   birth.
6. Parent revocation ordered first rejects the whole allocation.
7. Allocation ordered first commits the complete child and ownership record
   before a later revocation.
8. Exact retry restores every original result binding after authority changes.
9. Changed input under the same operation ID returns an operation conflict.
10. The mechanism cannot allocate or adopt an unrelated existing task.

The desired long-term relationship is:

```text
active parent assignment
          |
          | exact, transaction-validated citation
          v
+-------------------------------------------+
| ONE ATOMIC GOVERNED-ALLOCATION OPERATION  |
|                                           |
|  create child task                        |
|  establish child assignment/ownership     |
|  record parent citation                    |
|  add domain graph edges                    |
|  emit evidence, events, and activity       |
+-------------------------------------------+
          |
          | commit all or roll back all
          v
governed child task with replayable history
```

## Design Options

### Narrow Bootstrap Allocation

Bootstrap formally authorizes an allocation operation, while the same
transaction validates the selected active assignment and establishes explicit
child ownership.

This requires no new Provenance effect family, but bootstrap use must be
centralized and mechanically limited to allocation. The audit meaning is
"the system allocated on behalf of this assignment." A missing assignment
witness must make the operation invalid rather than silently privileged.

### First-Class Parent-Authorized Allocation

Provenance could expose one typed effect that atomically creates a task and its
child assignment under an active parent assignment. The authority model would
then directly say "this assignment allocated this governed child."

This is the stronger reusable contract when governed task allocation is common.
It also requires canonical encoding, SQLite reduction, DBOS parity, replay,
result-slot, race, and migration/versioning work. It must remain a constrained
child-allocation primitive, not a general relaxation allowing assignment
authorities to create arbitrary tasks.

## Decision

First-class parent-authorized allocation is the selected long-term contract.
Governed task creation is a recurring foundation for review rounds, severity
groups, findings, implementation slices, candidates, replacements, and future
workflow-generated task families. Treating each occurrence as a bootstrap
exception would spread privileged choreography throughout consumers and make
the authority model harder to audit and maintain.

The accepted direction is therefore one closed typed Provenance primitive that:

- cites the exact active parent assignment as the operation authority;
- allocates only a new child task, never adopts an unrelated existing task;
- creates the child assignment and parent citation atomically with task birth;
- preserves independent committing-actor attribution;
- serializes allocation against parent revocation using journal order;
- returns complete deterministic task and authority result bindings;
- participates in canonical immediate and reopened replay;
- rejects changed input with a typed operation conflict; and
- supplies DBOS and direct-SQLite behavior through the same canonical contract.

Bootstrap remains the authority base case and may establish root authority, but
application workflows do not use it as an allocation exception for delegated
work. The new primitive must preserve least privilege rather than generally
expanding what assignment authorities may create.

## Prior Art Alignment

No single external system supplies the complete governed-allocation contract.
The selected design composes established principles while keeping Provenance's
journal as the sole authority source.

| Prior art | Principle adopted | Boundary |
|---|---|---|
| Object capabilities | Explicit child-scoped delegation, least authority, and no ambient privilege | Object references alone do not provide durable revocation, transactional creation, or replay. |
| Confused-deputy prevention | Execute with request-specific delegated authority instead of service authority | Caller identity or a privileged process identity is not a substitute for authority. |
| Macaroons | Delegated authority may only be attenuated by additional restrictions | Bearer caveats alone do not eliminate revocation or check/use races. |
| Zanzibar | Explicit relationships and causally ordered authorization state | A remote authorization check followed by a local write is not atomic and must not become a second authority model. |
| Serializable transactions | Validate authority and create all governed state in one serialization order | Authorization state and resource creation must share the transaction boundary. |
| DBOS | Durable transaction completion, workflow recovery, and recorded operation results | DBOS does not define delegation, authority attenuation, or canonical invocation equality. |

The resulting rule is:

```text
authenticate at ingress
         |
         v
bind actor + parent authority + child intent + operation identity
         |
         v
validate authority AT COMMIT in the same serializable transaction
         |
         +--> create child task
         +--> create child assignment and parent citation
         +--> persist canonical invocation and result bindings
         |
         v
DBOS may durably invoke/recover this transaction,
but Provenance remains the sole authorization authority
```

Capability credentials, consistency tokens, workflow IDs, and role decorators
may carry or protect inputs. None may independently authorize governed creation.
If an external side effect follows allocation, use a transactional outbox or
downstream idempotency key rather than extending the local atomicity claim.

## Future Maintenance Rules

- Keep attribution and authorization independently queryable.
- Keep bootstrap use explicit, narrow, and test-enforced.
- Never derive authority from scheduling or lineage edges.
- Use one shared governed-allocation helper or effect; do not copy effect-order
  choreography into each workflow command.
- Make child task, child assignment, and parent citation deterministic under
  replay.
- Test both deterministic orderings of allocation versus parent revocation.
- Test revocation of every level in a multi-level parent chain.
- Preserve exact result slots across immediate and reopened replay.
- Reject adoption of pre-existing unrelated tasks.
- Prefer closed typed effect and authority variants over stringly typed policy.
- Add new authority reach only when it can be expressed and verified in the
  canonical journal; do not add application-side exceptions.

## References

- [`architecture.md`](architecture.md) describes the runtime layers and public
  composition boundary.
- [`journal-relational-contract.md`](journal-relational-contract.md) defines the
  normative authority lifecycle, per-effect authorization, parent-citation
  governance, replay, and projection invariants.
- [`test-performance.md`](test-performance.md) records the measured constraints
  for authority race and concurrency tests.
- Dennis and Van Horn, [Programming Semantics for Multiprogrammed
  Computations](https://dl.acm.org/doi/10.1145/365230.365252), and Hardy,
  [The Confused Deputy](http://www.cap-lore.com/CapTheory/ConfusedDeputy.html),
  motivate explicit capability delegation and the rejection of ambient service
  authority.
- Pang et al., [Zanzibar: Google's Consistent, Global Authorization
  System](https://www.usenix.org/conference/atc19/presentation/pang), motivates
  causally ordered authorization state while remaining outside the transaction
  boundary used here.
- [DBOS durable execution documentation](https://docs.dbos.dev/) describes the
  transaction and workflow recovery substrate; it does not replace the
  Provenance authority model.
