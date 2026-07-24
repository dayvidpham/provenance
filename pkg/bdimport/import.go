package bdimport

// import.go orchestrates the idempotent import of a bd Dump into a provenance Tracker.
// See doc.go for the header re-survey, the idempotency model, and the mapping contract.
//
// Every mutation flows through the journaled Session (Tracker.As), bound once to the
// importer's fixed software agent (committing actor) and a genesis bootstrap authority
// (governing authority). The direct-write registration verbs (agents, activities) stay
// on the Tracker, as the contract requires.

import (
	"errors"
	"fmt"
	"sort"

	"github.com/dayvidpham/provenance"
	"github.com/google/uuid"
)

// Importer identity constants — the fixed software agent under whose identity the
// importer commits every journaled write, and the pinned genesis OperationID used to
// recover-or-create the bootstrap authority idempotently.
const (
	importerNamespace = "bd-import"
	importerName      = "bd-import"
	importerVersion   = "0.1.0"
	importerSource    = "github.com/dayvidpham/provenance/cmd/bd-import"

	genesisOpID = provenance.OperationID("bdimport:genesis:v1")
)

// Options configures an import run.
type Options struct {
	// Namespace is the provenance namespace for every minted Task/Activity/Agent id
	// (e.g. "taxonomy-of-benchmarks"). Required.
	Namespace string
	// DryRun computes and returns the mapping summary without writing to the store.
	DryRun bool
}

// Result reports what an import produced (or, in dry-run, would produce). Counts are of
// distinct entities the import is responsible for, not cumulative store totals.
type Result struct {
	Tasks      int      `json:"tasks"`
	Edges      int      `json:"edges"`
	Agents     int      `json:"agents"`
	Activities int      `json:"activities"`
	Comments   int      `json:"comments"`
	Labels     int      `json:"labels"`
	DryRun     bool     `json:"dryRun"`
	Warnings   []string `json:"warnings,omitempty"`
}

// Import maps the Dump from src into tr under opts. It is idempotent: a second Import of
// the same Dump into the same store makes no change (see doc.go). Import returns the
// per-run counts; on DryRun it returns the planned counts without writing.
func Import(tr provenance.Tracker, src Source, opts Options) (Result, error) {
	if opts.Namespace == "" {
		return Result{}, fmt.Errorf("bdimport.Import: Options.Namespace is required")
	}
	dump, err := src.Load()
	if err != nil {
		return Result{}, err
	}

	imp := &importer{tr: tr, opts: opts, humans: map[string]provenance.ActorID{}}
	imp.prepare(dump)

	if opts.DryRun {
		return imp.plan(), nil
	}
	if err := imp.execute(); err != nil {
		return Result{}, err
	}
	return imp.result, nil
}

// importer holds the mutable state of one import run.
type importer struct {
	tr   provenance.Tracker
	opts Options
	sess *provenance.Session

	// Normalized, deterministically-ordered input.
	issues []Issue
	byID   map[string]Issue // issue id → issue (for dangling-target checks)
	// comments per issue id, in bd id order.
	commentsByIssue map[string][]Comment
	// distinct actor names needing a human agent (creators + comment authors).
	actorNames []string

	// Resolved during execution.
	importerID  provenance.ActorID
	humans      map[string]provenance.ActorID // name → human agent id
	knownActors map[string]struct{}           // actor ids present before this run
	knownActIDs map[string]struct{}           // activity ids present before this run

	result Result
}

// prepare normalizes the dump: sorts issues and comments by bd id for deterministic
// write order, indexes issues, groups comments, and collects distinct actor names.
func (imp *importer) prepare(d Dump) {
	imp.issues = append([]Issue(nil), d.Issues...)
	sort.Slice(imp.issues, func(i, j int) bool { return imp.issues[i].ID < imp.issues[j].ID })

	imp.byID = make(map[string]Issue, len(imp.issues))
	for _, iss := range imp.issues {
		imp.byID[iss.ID] = iss
	}

	imp.commentsByIssue = map[string][]Comment{}
	for _, c := range d.Comments {
		imp.commentsByIssue[c.IssueID] = append(imp.commentsByIssue[c.IssueID], c)
	}
	for id := range imp.commentsByIssue {
		cs := imp.commentsByIssue[id]
		sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
	}

	names := map[string]struct{}{}
	for _, iss := range imp.issues {
		if iss.CreatedBy != "" {
			names[iss.CreatedBy] = struct{}{}
		}
	}
	for _, c := range d.Comments {
		if c.Author != "" {
			names[c.Author] = struct{}{}
		}
	}
	for n := range names {
		imp.actorNames = append(imp.actorNames, n)
	}
	sort.Strings(imp.actorNames)
}

// plan computes the counts an execute() would produce, without touching the store.
func (imp *importer) plan() Result {
	r := Result{DryRun: true, Agents: len(imp.actorNames) + 1} // +1 importer software agent
	for _, iss := range imp.issues {
		r.Tasks++
		r.Activities++
		// labels: verbatim + optional assignee label
		r.Labels += len(iss.Labels)
		if iss.Assignee != "" {
			r.Labels++
		}
		// attributed-to edge (creator) + generated-by edge (activity)
		if iss.CreatedBy != "" {
			r.Edges++
		}
		r.Edges++
		for _, dep := range iss.Deps {
			if _, ok := edgeKindForDep(dep.Type); !ok {
				r.Warnings = append(r.Warnings, fmt.Sprintf("issue %s: unmapped dependency type %q (skipped)", iss.ID, dep.Type))
				continue
			}
			if _, present := imp.byID[dep.DependsOnID]; !present {
				r.Warnings = append(r.Warnings, fmt.Sprintf("issue %s: dependency target %s not in dump (skipped)", iss.ID, dep.DependsOnID))
				continue
			}
			r.Edges++
		}
		r.Warnings = append(r.Warnings, coercionWarnings(iss)...)
	}
	// Comments count only those attached to an imported issue; orphans are skipped and
	// warned (mirroring execute), never counted.
	for id, cs := range imp.commentsByIssue {
		if _, ok := imp.byID[id]; ok {
			r.Comments += len(cs)
		}
	}
	r.Warnings = append(r.Warnings, imp.orphanCommentWarnings()...)
	return r
}

// coercionWarnings returns the status/type/priority coercion warnings for one issue.
// The dry-run plan and the primary write path share this so the two report identically
// (no silent coercion asymmetry between them).
func coercionWarnings(iss Issue) []string {
	var w []string
	if _, ok := statusForBD(iss.Status); !ok {
		w = append(w, fmt.Sprintf("issue %s: unknown status %q (defaulted to open)", iss.ID, iss.Status))
	}
	if _, ok := taskTypeForBD(iss.IssueType); !ok {
		w = append(w, fmt.Sprintf("issue %s: unknown issue_type %q (defaulted to task)", iss.ID, iss.IssueType))
	}
	if _, ok := priorityForBD(iss.Priority); !ok {
		w = append(w, fmt.Sprintf("issue %s: priority %d out of range (clamped)", iss.ID, iss.Priority))
	}
	return w
}

// orphanCommentWarnings reports, in deterministic order, every comment whose issue_id is
// not among the imported issues — a dropped comment that would otherwise vanish silently.
func (imp *importer) orphanCommentWarnings() []string {
	var ids []string
	for id := range imp.commentsByIssue {
		if _, ok := imp.byID[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var w []string
	for _, id := range ids {
		for _, c := range imp.commentsByIssue[id] {
			w = append(w, fmt.Sprintf("comment %d: issue %s not in dump (skipped)", c.ID, id))
		}
	}
	return w
}

// execute performs the import in dependency-safe order: identity/genesis, agents, then
// tasks (all created before any edge), then edges/labels/activities/comments. Result
// counts are of entities NEWLY created this run — a re-import reports all zeros.
func (imp *importer) execute() error {
	if err := imp.preScan(); err != nil {
		return err
	}
	if err := imp.bindSession(); err != nil {
		return err
	}
	if err := imp.registerHumans(); err != nil {
		return err
	}
	// Pass 1: create every task and drive it to its bd status. All tasks must exist
	// before any edge is added (an edge target is a task).
	for _, iss := range imp.issues {
		if err := imp.importTask(iss); err != nil {
			return err
		}
	}
	// Pass 2: edges (deps + attribution + generated-by), labels, activities, comments.
	for _, iss := range imp.issues {
		if err := imp.importActivity(iss); err != nil {
			return err
		}
		if err := imp.importLabels(iss); err != nil {
			return err
		}
		if err := imp.importEdges(iss); err != nil {
			return err
		}
		if err := imp.importComments(iss); err != nil {
			return err
		}
	}
	// Comments whose issue was never imported are dropped — surface them rather than
	// letting them vanish silently.
	imp.result.Warnings = append(imp.result.Warnings, imp.orphanCommentWarnings()...)
	return nil
}

// preScan records which actors and importer-activities already exist so the run's
// counts reflect only newly-created entities (and re-import reports all zeros). It also
// seeds the human name→id map from existing namespace-scoped human agents.
func (imp *importer) preScan() error {
	imp.knownActors = map[string]struct{}{}
	imp.knownActIDs = map[string]struct{}{}

	actors, err := imp.tr.AllActors()
	if err != nil {
		return fmt.Errorf("bdimport: list actors: %w", err)
	}
	for _, a := range actors {
		imp.knownActors[a.ID.String()] = struct{}{}
		if a.Kind != provenance.AgentKindHuman || a.ID.Namespace != imp.opts.Namespace {
			continue
		}
		ha, err := imp.tr.HumanAgent(a.ID)
		if err != nil {
			return fmt.Errorf("bdimport: read human agent %s: %w", a.ID, err)
		}
		imp.humans[ha.Name] = a.ID
	}

	acts, err := imp.tr.Activities(nil)
	if err != nil {
		return fmt.Errorf("bdimport: list activities: %w", err)
	}
	for _, act := range acts {
		imp.knownActIDs[act.ID.String()] = struct{}{}
	}
	return nil
}

// bindSession registers the importer's fixed software agent (idempotent), recovers or
// establishes the genesis bootstrap authority, and binds the Session.
func (imp *importer) bindSession() error {
	reg := provenance.FixedSoftwareAgentRegistration{
		Claim: provenance.ActorNamespaceClaim{
			Namespace: importerNamespace, ClaimantID: importerName,
			Codec: provenance.OrdinalV1CodecName,
			Range: provenance.UUIDRange{Min: provenance.BigEndianUUID(0), Max: provenance.BigEndianUUID(1023)},
		},
		Entry: provenance.FixedActorEntry{
			ActorID:   provenance.ActorID{Namespace: importerNamespace, UUID: uuid.UUID(provenance.BigEndianUUID(0))},
			Namespace: importerNamespace, ActorKind: provenance.AgentKindSoftware,
			Name: importerName, Metadata: `{}`,
		},
		AgentName: importerName, Version: importerVersion, Source: importerSource,
	}
	sa, err := imp.tr.RegisterFixedSoftwareAgent(reg)
	if err != nil {
		return fmt.Errorf("bdimport: register importer agent: %w", err)
	}
	imp.importerID = sa.ID
	if _, known := imp.knownActors[sa.ID.String()]; !known {
		imp.result.Agents++ // importer software agent, newly registered this run
	}

	boot, err := imp.genesisAuthority(sa.ID)
	if err != nil {
		return err
	}
	imp.sess = imp.tr.As(sa.ID, boot)
	return nil
}

// genesisAuthority recovers the existing bootstrap authority (pinned-OperationID
// LookupCommitted) or establishes it, returning its JournalID. Idempotent: on re-import
// the LookupCommitted path returns the already-committed authority without re-executing.
func (imp *importer) genesisAuthority(actor provenance.ActorID) (provenance.JournalID, error) {
	res, err := imp.tr.Journal().LookupCommitted(genesisOpID)
	if err != nil {
		return 0, fmt.Errorf("bdimport: lookup genesis: %w", err)
	}
	if res.Kind == provenance.CommittedAbsent {
		res, err = imp.tr.Journal().Apply(provenance.OperationInput{
			OperationID:    genesisOpID,
			ActorID:        actor,
			CommandDigest:  []byte("bdimport-genesis-command"),
			MutationDigest: []byte("bdimport-genesis-mutation"),
			Effects: []provenance.Effect{{
				Sort: provenance.EffectBootstrapAuthority, BootstrapLabel: importerName, ResultSlot: "auth",
			}},
		})
		if err != nil {
			return 0, fmt.Errorf("bdimport: establish genesis: %w", err)
		}
	}
	for i := range res.ResultSlots {
		if string(res.ResultSlots[i].Slot) == "auth" {
			return res.ResultSlots[i].ProducedJournalID, nil
		}
	}
	return 0, fmt.Errorf("bdimport: genesis authority produced no 'auth' result slot")
}

// registerHumans ensures one human agent per distinct actor name, deduplicated by
// (namespace, name) against the store (seeded in preScan) so re-import reuses agents.
func (imp *importer) registerHumans() error {
	for _, name := range imp.actorNames {
		if _, ok := imp.humans[name]; ok {
			continue
		}
		ha, err := imp.tr.RegisterHumanAgent(imp.opts.Namespace, name, imp.contactFor(name))
		if err != nil {
			return fmt.Errorf("bdimport: register human %q: %w", name, err)
		}
		imp.humans[name] = ha.ID
		imp.result.Agents++
	}
	return nil
}

// contactFor returns the owner email to use as a human's contact when a single distinct
// owner value co-occurs for that creator across the corpus; otherwise "" (no fabrication
// of a contact when the mapping is ambiguous).
func (imp *importer) contactFor(name string) string {
	owners := map[string]struct{}{}
	for _, iss := range imp.issues {
		if iss.CreatedBy == name && iss.Owner != "" {
			owners[iss.Owner] = struct{}{}
		}
	}
	if len(owners) == 1 {
		for o := range owners {
			return o
		}
	}
	return ""
}

// importTask creates the task (deterministic id) if absent and drives it to its bd
// status. Detect-and-skip on existence and on status keeps re-import a no-op.
func (imp *importer) importTask(iss Issue) error {
	id := TaskID(imp.opts.Namespace, iss.ID)
	taskType, _ := taskTypeForBD(iss.IssueType)
	priority, _ := priorityForBD(iss.Priority)
	status, _ := statusForBD(iss.Status)
	// Report every coercion/clamp in the PRIMARY write path, identically to the
	// dry-run plan() — the two paths must not diverge in what they surface.
	imp.result.Warnings = append(imp.result.Warnings, coercionWarnings(iss)...)

	existing, err := imp.tr.Show(id)
	switch {
	case err == nil:
		// Task already imported; ensure status, then done (no new task created).
		return imp.driveStatus(id, existing.Status, status)
	case isNotFound(err):
		// Create at Open with a pinned OperationID (idempotent create).
		opID := provenance.OperationID("bdimport:create:" + iss.ID)
		if _, err := imp.sess.Atomic(func(op *provenance.Operation) {
			op.CreateTask(id, iss.Title, iss.Description, taskType, priority, provenance.PhaseUnscoped)
		}, provenance.WithOperationID(opID)); err != nil {
			return fmt.Errorf("bdimport: create task %s: %w", iss.ID, err)
		}
		imp.result.Tasks++
		return imp.driveStatus(id, provenance.StatusOpen, status)
	default:
		return fmt.Errorf("bdimport: show task %s: %w", iss.ID, err)
	}
}

// driveStatus transitions a task from its current status to the target bd status using
// the dedicated lifecycle verbs, skipping when already there.
func (imp *importer) driveStatus(id provenance.TaskID, current, target provenance.Status) error {
	if current == target {
		return nil
	}
	switch target {
	case provenance.StatusInProgress:
		if current == provenance.StatusOpen {
			_, err := imp.sess.Start(id)
			return wrapLifecycle("start", id, err)
		}
	case provenance.StatusClosed:
		// {open,in_progress} → closed is a single legal transition.
		_, err := imp.sess.CloseTask(id, "")
		return wrapLifecycle("close", id, err)
	case provenance.StatusOpen:
		// Reachable only if a prior import left it in a non-open state that bd has since
		// reopened; closed→open is Reopen, in_progress→open is Stop.
		if current == provenance.StatusClosed {
			_, err := imp.sess.Reopen(id)
			return wrapLifecycle("reopen", id, err)
		}
		_, err := imp.sess.Stop(id)
		return wrapLifecycle("stop", id, err)
	}
	return nil
}

func wrapLifecycle(verb string, id provenance.TaskID, err error) error {
	if err != nil {
		return fmt.Errorf("bdimport: %s task %s: %w", verb, id.String(), err)
	}
	return nil
}

// importActivity emits one deterministic lifecycle activity per issue, acted by the
// importer software agent, and links the task to it with EdgeGeneratedBy.
func (imp *importer) importActivity(iss Issue) error {
	taskID := TaskID(imp.opts.Namespace, iss.ID)
	actID := ActivityID(imp.opts.Namespace, iss.ID)
	status, _ := statusForBD(iss.Status)
	notes := fmt.Sprintf("bd-import: materialized issue %s (bd status=%s)", iss.ID, iss.Status)

	act, err := imp.tr.StartActivityWithID(actID, imp.importerID, provenance.PhaseUnscoped, stageForStatus(status), notes)
	if err != nil {
		return fmt.Errorf("bdimport: start activity for %s: %w", iss.ID, err)
	}
	if _, known := imp.knownActIDs[actID.String()]; !known {
		imp.result.Activities++ // newly created this run
	}
	// End the activity for closed issues (work complete), unless already ended.
	if status == provenance.StatusClosed && act.EndedAt == nil {
		if _, err := imp.tr.EndActivity(actID); err != nil {
			return fmt.Errorf("bdimport: end activity for %s: %w", iss.ID, err)
		}
	}
	return imp.addEdge(taskID, actID.String(), provenance.EdgeGeneratedBy)
}

// importLabels attaches every bd label plus an optional bd:assignee:<slot> label,
// detect-and-skip on the current label set.
func (imp *importer) importLabels(iss Issue) error {
	id := TaskID(imp.opts.Namespace, iss.ID)
	existing, err := imp.tr.Labels(id)
	if err != nil {
		return fmt.Errorf("bdimport: labels of %s: %w", iss.ID, err)
	}
	have := map[string]struct{}{}
	for _, l := range existing {
		have[l] = struct{}{}
	}
	want := append([]string(nil), iss.Labels...)
	if iss.Assignee != "" {
		want = append(want, "bd:assignee:"+iss.Assignee)
	}
	sort.Strings(want)
	for _, l := range want {
		if _, ok := have[l]; ok {
			continue
		}
		if err := imp.sess.AddLabel(id, l); err != nil {
			return fmt.Errorf("bdimport: add label %q to %s: %w", l, iss.ID, err)
		}
		have[l] = struct{}{}
		imp.result.Labels++
	}
	return nil
}

// importEdges emits the issue's dependency edges and its attributed-to edge.
func (imp *importer) importEdges(iss Issue) error {
	src := TaskID(imp.opts.Namespace, iss.ID)

	// Attribution: the issue's human creator (prov:wasAttributedTo).
	if iss.CreatedBy != "" {
		if agentID, ok := imp.humans[iss.CreatedBy]; ok {
			if err := imp.addEdge(src, agentID.String(), provenance.EdgeAttributedTo); err != nil {
				return err
			}
		}
	}

	// Dependencies, in a deterministic order.
	deps := append([]Dependency(nil), iss.Deps...)
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].Type != deps[j].Type {
			return deps[i].Type < deps[j].Type
		}
		return deps[i].DependsOnID < deps[j].DependsOnID
	})
	for _, dep := range deps {
		kind, ok := edgeKindForDep(dep.Type)
		if !ok {
			imp.warn("issue %s: unmapped dependency type %q (skipped)", iss.ID, dep.Type)
			continue
		}
		if _, present := imp.byID[dep.DependsOnID]; !present {
			imp.warn("issue %s: dependency target %s not in dump (skipped)", iss.ID, dep.DependsOnID)
			continue
		}
		target := TaskID(imp.opts.Namespace, dep.DependsOnID)
		// A dependency edge from hostile/degenerate bd JSON can be cycle-inducing
		// (self-dep, 2-cycle); Session.AddEdge rejects it (ErrCycleDetected). Warn and
		// skip that one edge rather than aborting the whole import mid-way with a
		// partial commit — every other issue and edge still lands.
		if err := imp.addEdge(src, target.String(), kind); err != nil {
			imp.warn("issue %s: %s edge to %s rejected (%v) (skipped)", iss.ID, kind, dep.DependsOnID, err)
		}
	}
	return nil
}

// importComments adds each bd comment, authored by its human agent, detect-and-skip on
// (author, body) since bd comments carry no stable provenance CommentID.
func (imp *importer) importComments(iss Issue) error {
	id := TaskID(imp.opts.Namespace, iss.ID)
	cs := imp.commentsByIssue[iss.ID]
	if len(cs) == 0 {
		return nil
	}
	existing, err := imp.tr.Comments(id)
	if err != nil {
		return fmt.Errorf("bdimport: comments of %s: %w", iss.ID, err)
	}
	seen := map[string]struct{}{}
	for _, c := range existing {
		seen[commentKey(c.AuthorID, c.Body)] = struct{}{}
	}
	for _, c := range cs {
		authorID, ok := imp.humans[c.Author]
		if !ok {
			imp.warn("issue %s: comment %d author %q has no agent (skipped)", iss.ID, c.ID, c.Author)
			continue
		}
		if _, dup := seen[commentKey(authorID, c.Text)]; dup {
			continue
		}
		if _, err := imp.sess.AddComment(id, authorID, c.Text); err != nil {
			return fmt.Errorf("bdimport: add comment %d to %s: %w", c.ID, iss.ID, err)
		}
		seen[commentKey(authorID, c.Text)] = struct{}{}
		imp.result.Comments++
	}
	return nil
}

// addEdge adds a typed edge, detect-and-skip on the source's current edge set of that
// kind so re-import neither duplicates the projection nor re-journals the edge.
func (imp *importer) addEdge(src provenance.TaskID, target string, kind provenance.EdgeKind) error {
	existing, err := imp.tr.Edges(src, &kind)
	if err != nil {
		return fmt.Errorf("bdimport: edges of %s: %w", src.String(), err)
	}
	for _, e := range existing {
		if e.TargetID == target {
			return nil
		}
	}
	if err := imp.sess.AddEdge(src, target, kind); err != nil {
		return fmt.Errorf("bdimport: add %s edge %s→%s: %w", kind, src.String(), target, err)
	}
	imp.result.Edges++
	return nil
}

func (imp *importer) warn(format string, args ...any) {
	imp.result.Warnings = append(imp.result.Warnings, fmt.Sprintf(format, args...))
}

func commentKey(author provenance.ActorID, body string) string {
	return author.String() + "\x00" + body
}

// isNotFound reports whether err is the tracker's not-found sentinel.
func isNotFound(err error) bool {
	return errors.Is(err, provenance.ErrNotFound)
}
