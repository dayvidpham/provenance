// Package provo exports a provenance Tracker graph as PROV-O / RO-Crate using the
// data-labour-prov vocabulary minted in the taxonomy-of-benchmarks project
// (its ontology/ directory).
//
// This package is the paper's reference implementation: its Turtle output must
// conform to the paper's SHACL shapes (ontology/shapes.ttl). Both exported
// functions are PURE READS over the Tracker — they never mutate the graph.
//
// # Mapping (roadmap §2.1)
//
//	Task              -> prov:Entity
//	Activity          -> prov:Activity (+ p-plan:Activity when a role can be furnished)
//	HumanAgent        -> prov:Person
//	SoftwareAgent     -> prov:SoftwareAgent
//	MLAgent           -> :LLMAgent (rdfs:subClassOf prov:SoftwareAgent) with :modelId
//	EdgeDerivedFrom   -> prov:wasDerivedFrom + prov:qualifiedDerivation node; the
//	                     node carries :derivationKind + prov:hadActivity when the
//	                     derivation is qualified (roadmap §3.3 / M4)
//	EdgeSupersedes    -> prov:wasRevisionOf  + prov:qualifiedDerivation node (as above)
//	EdgeDiscoveredFrom-> prov:wasInfluencedBy
//	EdgeGeneratedBy   -> prov:wasGeneratedBy
//	EdgeAttributedTo  -> prov:wasAttributedTo
//	EdgeBlockedBy     -> NOT exported (scheduling, not provenance)
//	Plan / PlanStep   -> p-plan:Plan / p-plan:Step (dct:title + dct:hasVersion),
//	                     read from the tracker (roadmap §3.1 / M4); each activity
//	                     p-plan:correspondsToStep the step of its own plan
//
// # Deferred to later milestones (never fabricated here)
//
//   - :modelVersion / :decodingParameters (§2.3 capture fields): only :modelId is
//     emitted, because provenance does not yet capture the other two.
//   - Act-level role: role is synthesized at AGENT level from MLAgent.Role until the
//     qualified-association layer (M5) lands. The Turtle header notes this.
package provo

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dayvidpham/provenance"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// DefaultVocabIRI is the placeholder namespace of the data-labour-prov vocabulary,
// copied verbatim from ontology/data-labour-prov.ttl. It is NOT registered and does
// NOT resolve; permanent-namespace selection (e.g. via w3id) is future work. Swap it
// by setting Options.VocabIRI once a permanent namespace is chosen.
const DefaultVocabIRI = "https://example.org/data-labour-prov#"

// DefaultBaseIRI is the default graph-instance IRI base for minted local names when
// Options.BaseIRI is empty. IDs render as BaseIRI + url.PathEscape(id.String()).
const DefaultBaseIRI = "urn:provenance:"

// Fixed external namespaces.
const (
	nsProv  = "http://www.w3.org/ns/prov#"
	nsPplan = "http://purl.org/net/p-plan#"
	nsRdfs  = "http://www.w3.org/2000/01/rdf-schema#"
	nsXsd   = "http://www.w3.org/2001/XMLSchema#"
	nsDct   = "http://purl.org/dc/terms/"
)

// derivationIndividuals maps each DerivationKind to its vocabulary individual
// local name (in the ':' namespace), which is the object of :derivationKind. It is
// indexed by the DerivationKind enum value; the mapping is the exporter's
// wire-token -> paper-individual bridge (roadmap §3.3).
var derivationIndividuals = [...]string{
	ptypes.DerivationLabelCorrection:        "LabelCorrection",
	ptypes.DerivationDeduplication:          "Deduplication",
	ptypes.DerivationDifficultyFiltering:    "DifficultyFiltering",
	ptypes.DerivationTranslation:            "Translation",
	ptypes.DerivationContaminationScrubbing: "ContaminationScrubbing",
	ptypes.DerivationAdversarialFiltering:   "AdversarialFiltering",
	ptypes.DerivationVerificationSubset:     "VerificationSubset",
}

// derivationIndividual returns the vocabulary individual local name for a kind.
func derivationIndividual(k ptypes.DerivationKind) (string, bool) {
	if int(k) >= 0 && int(k) < len(derivationIndividuals) {
		return derivationIndividuals[k], true
	}
	return "", false
}

// Options configures a PROV-O export.
type Options struct {
	// BaseIRI is the graph-instance IRI base for minted local names. IDs render as
	// BaseIRI + url.PathEscape(id.String()). Defaults to DefaultBaseIRI.
	BaseIRI string
	// VocabIRI is the data-labour-prov vocabulary namespace bound to the ':' prefix.
	// Defaults to DefaultVocabIRI (the paper's placeholder namespace).
	VocabIRI string
	// Registry enriches an MLAgent's :modelId from the (provider, name) join. When
	// nil, the agent's own Model (provider, name) is used directly.
	Registry ptypes.ModelRegistry
}

// withDefaults returns a copy of opts with empty fields filled in.
func (o Options) withDefaults() Options {
	if o.BaseIRI == "" {
		o.BaseIRI = DefaultBaseIRI
	}
	if o.VocabIRI == "" {
		o.VocabIRI = DefaultVocabIRI
	}
	return o
}

// ExportTurtle writes the Tracker graph to w as PROV-O Turtle using the
// data-labour-prov vocabulary. Output is deterministic: every iteration order is
// sorted, so the same graph always serializes byte-for-byte identically (a
// prerequisite for golden-file testing). It is a pure read.
func ExportTurtle(w io.Writer, tr provenance.Tracker, opts Options) error {
	e := &encoder{opts: opts.withDefaults()}
	if err := e.load(tr); err != nil {
		return err
	}
	e.write()
	if _, err := w.Write(e.buf.Bytes()); err != nil {
		return fmt.Errorf("provo.ExportTurtle: write: %w", err)
	}
	return nil
}

// encoder holds the loaded graph and the output buffer for one export.
type encoder struct {
	opts Options
	buf  bytes.Buffer

	tasks      []ptypes.Task
	actors     []ptypes.Agent
	activities []ptypes.Activity
	plans      []ptypes.Plan

	// detail maps keyed by ID string, populated by load().
	humans   map[string]ptypes.HumanAgent
	mls      map[string]ptypes.MLAgent
	software map[string]ptypes.SoftwareAgent
	// edgesByTask is the sorted, non-blocked-by edge list per task ID string.
	edgesByTask map[string][]ptypes.Edge
	// stepsByPlan is the ordinal-sorted step list per plan ID string.
	stepsByPlan map[string][]ptypes.PlanStep
	// planHasPhase[planID][phase] reports whether a plan carries a step for a phase,
	// so correspondsToStep is emitted only against a step that exists.
	planHasPhase map[string]map[ptypes.Phase]struct{}
	// qualByTask[sourceTaskID][targetTaskID] is the derivation qualifier on that
	// derivation relationship, if any.
	qualByTask map[string]map[string]ptypes.DerivationQualifier
}

// load performs every read against the Tracker and sorts all collections so the
// subsequent write() is deterministic.
func (e *encoder) load(tr provenance.Tracker) error {
	var err error
	if e.tasks, err = tr.List(ptypes.ListFilter{}); err != nil {
		return fmt.Errorf("provo: list tasks: %w", err)
	}
	sort.Slice(e.tasks, func(i, j int) bool { return e.tasks[i].ID.String() < e.tasks[j].ID.String() })

	if e.actors, err = tr.AllActors(); err != nil {
		return fmt.Errorf("provo: list actors: %w", err)
	}
	sort.Slice(e.actors, func(i, j int) bool { return e.actors[i].ID.String() < e.actors[j].ID.String() })

	if e.activities, err = tr.Activities(nil); err != nil {
		return fmt.Errorf("provo: list activities: %w", err)
	}
	sort.Slice(e.activities, func(i, j int) bool { return e.activities[i].ID.String() < e.activities[j].ID.String() })

	e.humans = map[string]ptypes.HumanAgent{}
	e.mls = map[string]ptypes.MLAgent{}
	e.software = map[string]ptypes.SoftwareAgent{}
	for _, a := range e.actors {
		switch a.Kind {
		case ptypes.AgentKindHuman:
			ha, err := tr.HumanAgent(a.ID)
			if err != nil {
				return fmt.Errorf("provo: dereference human agent %q: %w", a.ID.String(), err)
			}
			e.humans[a.ID.String()] = ha
		case ptypes.AgentKindMachineLearning:
			mla, err := tr.MLAgent(a.ID)
			if err != nil {
				return fmt.Errorf("provo: dereference ML agent %q: %w", a.ID.String(), err)
			}
			e.mls[a.ID.String()] = mla
		case ptypes.AgentKindSoftware:
			sa, err := tr.SoftwareAgent(a.ID)
			if err != nil {
				return fmt.Errorf("provo: dereference software agent %q: %w", a.ID.String(), err)
			}
			e.software[a.ID.String()] = sa
		}
	}

	e.edgesByTask = map[string][]ptypes.Edge{}
	e.qualByTask = map[string]map[string]ptypes.DerivationQualifier{}
	for _, t := range e.tasks {
		edges, err := tr.Edges(t.ID, nil)
		if err != nil {
			return fmt.Errorf("provo: edges for task %q: %w", t.ID.String(), err)
		}
		filtered := edges[:0:0]
		for _, ed := range edges {
			if ed.Kind == ptypes.EdgeBlockedBy {
				continue // scheduling, not provenance — never exported
			}
			filtered = append(filtered, ed)
		}
		sort.Slice(filtered, func(i, j int) bool {
			if filtered[i].Kind != filtered[j].Kind {
				return filtered[i].Kind < filtered[j].Kind
			}
			return filtered[i].TargetID < filtered[j].TargetID
		})
		e.edgesByTask[t.ID.String()] = filtered

		quals, err := tr.DerivationQualifiers(t.ID)
		if err != nil {
			return fmt.Errorf("provo: derivation qualifiers for task %q: %w", t.ID.String(), err)
		}
		if len(quals) > 0 {
			byTarget := map[string]ptypes.DerivationQualifier{}
			for _, q := range quals {
				byTarget[q.TargetID.String()] = q
			}
			e.qualByTask[t.ID.String()] = byTarget
		}
	}

	// Plans + steps (roadmap §3.1): read from the tracker, not hardcoded.
	if e.plans, err = tr.Plans(); err != nil {
		return fmt.Errorf("provo: list plans: %w", err)
	}
	sort.Slice(e.plans, func(i, j int) bool { return e.plans[i].ID.String() < e.plans[j].ID.String() })
	e.stepsByPlan = map[string][]ptypes.PlanStep{}
	e.planHasPhase = map[string]map[ptypes.Phase]struct{}{}
	for _, pl := range e.plans {
		steps, err := tr.PlanSteps(pl.ID)
		if err != nil {
			return fmt.Errorf("provo: steps for plan %q: %w", pl.ID.String(), err)
		}
		sort.Slice(steps, func(i, j int) bool { return steps[i].Ordinal < steps[j].Ordinal })
		e.stepsByPlan[pl.ID.String()] = steps
		phases := map[ptypes.Phase]struct{}{}
		for _, st := range steps {
			phases[st.Phase] = struct{}{}
		}
		e.planHasPhase[pl.ID.String()] = phases
	}
	return nil
}

func (e *encoder) write() {
	e.header()
	e.plan()
	e.agents()
	e.activitiesSection()
	e.entities()
}

// ---------------------------------------------------------------------------
// Sections
// ---------------------------------------------------------------------------

func (e *encoder) header() {
	p := &e.buf
	fmt.Fprintln(p, "# PROV-O export of a provenance Tracker graph, using the")
	fmt.Fprintln(p, "# data-labour-prov vocabulary.")
	fmt.Fprintln(p, "# Generated by github.com/dayvidpham/provenance/pkg/provo — deterministic, pure read.")
	fmt.Fprintln(p, "#")
	fmt.Fprintln(p, "# NOTE (role granularity): prov:hadRole is synthesized at AGENT level from")
	fmt.Fprintln(p, "#   MLAgent.Role. Until the qualified-association layer (roadmap §3.2 / M5) lands,")
	fmt.Fprintln(p, "#   role is a property of the acting ML agent, not of the specific act; only")
	fmt.Fprintln(p, "#   ML-agent activities are typed p-plan:Activity (they can furnish the required")
	fmt.Fprintln(p, "#   agent+role association per the paper's AnnotationActShape).")
	fmt.Fprintln(p, "# NOTE (:derivationKind): a qualified derivation carries its typed reason")
	fmt.Fprintln(p, "#   (:derivationKind, roadmap §3.3 / M4) and, when recorded, prov:hadActivity;")
	fmt.Fprintln(p, "#   an unqualified derivation carries only the fact of derivation and its time.")
	fmt.Fprintf(p, "# NOTE (namespace): the ':' vocabulary namespace <%s> is a PLACEHOLDER pending w3id registration.\n", e.opts.VocabIRI)
	fmt.Fprintln(p)
	fmt.Fprintf(p, "@prefix :      <%s> .\n", e.opts.VocabIRI)
	fmt.Fprintf(p, "@prefix prov:  <%s> .\n", nsProv)
	fmt.Fprintf(p, "@prefix pplan: <%s> .\n", nsPplan)
	fmt.Fprintf(p, "@prefix rdfs:  <%s> .\n", nsRdfs)
	fmt.Fprintf(p, "@prefix dct:   <%s> .\n", nsDct)
	fmt.Fprintf(p, "@prefix xsd:   <%s> .\n", nsXsd)
	fmt.Fprintln(p)
}

func (e *encoder) plan() {
	if len(e.plans) == 0 {
		return
	}
	p := &e.buf
	fmt.Fprintln(p, "# Plans (p-plan:Plan) and their steps (p-plan:Step), read from the tracker.")
	fmt.Fprintln(p, "# The built-in 'pasture-12-phase' plan reifies the Phase enum (one step per phase).")
	for _, pl := range e.plans {
		planIRI := e.iri(pl.ID.String())
		fmt.Fprintf(p, "%s a pplan:Plan ;\n", planIRI)
		fmt.Fprintf(p, "    dct:title %s", literal(pl.Title))
		if pl.Version != "" {
			fmt.Fprintf(p, " ;\n    dct:hasVersion %s", literal(pl.Version))
		}
		fmt.Fprintln(p, " .")
		for _, st := range e.stepsByPlan[pl.ID.String()] {
			token := phaseToken(st.Phase)
			fmt.Fprintf(p, "%s a pplan:Step ;\n", e.stepIRI(pl.ID, token))
			fmt.Fprintf(p, "    pplan:isStepOfPlan %s ;\n", planIRI)
			fmt.Fprintf(p, "    rdfs:label %s .\n", literal(token))
		}
	}
	fmt.Fprintln(p)
}

func (e *encoder) agents() {
	if len(e.actors) == 0 {
		return
	}
	p := &e.buf
	fmt.Fprintln(p, "# Agents (prov:Person / prov:SoftwareAgent / :LLMAgent).")
	for _, a := range e.actors {
		id := a.ID.String()
		switch a.Kind {
		case ptypes.AgentKindHuman:
			ha := e.humans[id]
			fmt.Fprintf(p, "%s a prov:Person", e.iri(id))
			if ha.Name != "" {
				fmt.Fprintf(p, " ;\n    rdfs:label %s", literal(ha.Name))
			}
			fmt.Fprintln(p, " .")
		case ptypes.AgentKindSoftware:
			sa := e.software[id]
			fmt.Fprintf(p, "%s a prov:SoftwareAgent", e.iri(id))
			if sa.Name != "" {
				fmt.Fprintf(p, " ;\n    rdfs:label %s", literal(sa.Name))
			}
			fmt.Fprintln(p, " .")
		case ptypes.AgentKindMachineLearning:
			mla := e.mls[id]
			fmt.Fprintf(p, "%s a :LLMAgent ;\n", e.iri(id))
			fmt.Fprintf(p, "    :modelId %s ;\n", literal(e.modelID(mla)))
			fmt.Fprintf(p, "    rdfs:label %s .\n", literal(string(mla.Model.Name)))
		}
	}
	fmt.Fprintln(p)
}

func (e *encoder) activitiesSection() {
	if len(e.activities) == 0 {
		return
	}
	p := &e.buf
	fmt.Fprintln(p, "# Activities (prov:Activity; ML-agent acts also p-plan:Activity).")
	for _, act := range e.activities {
		agentID := act.AgentID.String()
		mla, isML := e.mls[agentID]

		fmt.Fprintf(p, "%s a prov:Activity", e.iri(act.ID.String()))
		if isML {
			fmt.Fprintf(p, ", pplan:Activity")
		}
		fmt.Fprintf(p, " ;\n    prov:startedAtTime %s ;\n", dateTime(act.StartedAt))
		if act.EndedAt != nil {
			fmt.Fprintf(p, "    prov:endedAtTime %s ;\n", dateTime(*act.EndedAt))
		}
		// correspondsToStep resolves against the activity's OWN plan's step for its
		// phase (roadmap §3.1). An unplanned activity (nil PlanID), or one whose plan
		// lacks a step for its phase, emits none.
		if act.PlanID != nil {
			if phases, ok := e.planHasPhase[act.PlanID.String()]; ok {
				if _, has := phases[act.Phase]; has {
					fmt.Fprintf(p, "    pplan:correspondsToStep %s ;\n", e.stepIRI(*act.PlanID, phaseToken(act.Phase)))
				}
			}
		}
		fmt.Fprintf(p, "    prov:wasAssociatedWith %s", e.iri(agentID))
		if isML {
			// Synthesize the qualified association the AnnotationActShape requires:
			// agent + role. Role is agent-level (MLAgent.Role) until M5.
			fmt.Fprintf(p, " ;\n    prov:qualifiedAssociation [\n")
			fmt.Fprintf(p, "        a prov:Association ;\n")
			fmt.Fprintf(p, "        prov:agent %s ;\n", e.iri(agentID))
			fmt.Fprintf(p, "        prov:hadRole %s\n", e.roleIRI(roleToken(mla.Role)))
			fmt.Fprintf(p, "    ]")
		}
		if act.Notes != "" {
			fmt.Fprintf(p, " ;\n    rdfs:comment %s", literal(act.Notes))
		}
		fmt.Fprintln(p, " .")
	}
	fmt.Fprintln(p)
}

func (e *encoder) entities() {
	if len(e.tasks) == 0 {
		return
	}
	p := &e.buf
	fmt.Fprintln(p, "# Entities (prov:Entity) and their provenance edges.")
	for _, t := range e.tasks {
		src := e.iri(t.ID.String())
		fmt.Fprintf(p, "%s a prov:Entity", src)
		if t.Title != "" {
			fmt.Fprintf(p, " ;\n    rdfs:label %s", literal(t.Title))
		}
		if t.Description != "" {
			fmt.Fprintf(p, " ;\n    rdfs:comment %s", literal(t.Description))
		}
		fmt.Fprintln(p, " .")
		e.edges(t.ID.String())
	}
}

// edges emits the provenance edges for one task (blocked-by already filtered out).
func (e *encoder) edges(taskID string) {
	p := &e.buf
	src := e.iri(taskID)
	for _, ed := range e.edgesByTask[taskID] {
		tgt := e.iri(ed.TargetID)
		switch ed.Kind {
		case ptypes.EdgeDerivedFrom:
			fmt.Fprintf(p, "%s prov:wasDerivedFrom %s ;\n", src, tgt)
			e.qualifiedDerivation(tgt, ed.CreatedAt, e.qualifier(taskID, ed.TargetID))
		case ptypes.EdgeSupersedes:
			fmt.Fprintf(p, "%s prov:wasRevisionOf %s ;\n", src, tgt)
			e.qualifiedDerivation(tgt, ed.CreatedAt, e.qualifier(taskID, ed.TargetID))
		case ptypes.EdgeDiscoveredFrom:
			fmt.Fprintf(p, "%s prov:wasInfluencedBy %s .\n", src, tgt)
		case ptypes.EdgeGeneratedBy:
			fmt.Fprintf(p, "%s prov:wasGeneratedBy %s .\n", src, tgt)
		case ptypes.EdgeAttributedTo:
			fmt.Fprintf(p, "%s prov:wasAttributedTo %s .\n", src, tgt)
		}
	}
}

// qualifier returns the derivation qualifier on the source->target relationship,
// or nil if the derivation is unqualified.
func (e *encoder) qualifier(sourceID, targetID string) *ptypes.DerivationQualifier {
	if byTarget, ok := e.qualByTask[sourceID]; ok {
		if q, ok := byTarget[targetID]; ok {
			return &q
		}
	}
	return nil
}

// qualifiedDerivation emits the trailing prov:qualifiedDerivation blank node for a
// derived_from / supersedes edge. The node is typed prov:Derivation and carries the
// derivation's time (from Edge.CreatedAt). When the derivation is qualified
// (roadmap §3.3), the node also carries :derivationKind (the paper's controlled
// individual — the attachment point the SHACL DerivationKindAttachmentShape
// validates) and, when recorded, prov:hadActivity. It is written as the
// continuation of the ';'-terminated relation line above it.
func (e *encoder) qualifiedDerivation(entityIRI string, createdAt time.Time, qual *ptypes.DerivationQualifier) {
	p := &e.buf
	fmt.Fprintf(p, "    prov:qualifiedDerivation [\n")
	fmt.Fprintf(p, "        a prov:Derivation ;\n")
	fmt.Fprintf(p, "        prov:entity %s", entityIRI)
	if !createdAt.IsZero() {
		fmt.Fprintf(p, " ;\n        prov:atTime %s", dateTime(createdAt))
	}
	if qual != nil {
		if individual, ok := derivationIndividual(qual.Kind); ok {
			fmt.Fprintf(p, " ;\n        :derivationKind :%s", individual)
		}
		if qual.ActivityID != nil {
			fmt.Fprintf(p, " ;\n        prov:hadActivity %s", e.iri(qual.ActivityID.String()))
		}
	}
	fmt.Fprintf(p, "\n    ] .\n")
}

// ---------------------------------------------------------------------------
// IRI + literal helpers
// ---------------------------------------------------------------------------

// iri renders an entity/agent/activity ID as an angle-bracketed IRI:
// BaseIRI + url.PathEscape(id). PathEscape guarantees the result has no characters
// illegal in an IRI reference.
func (e *encoder) iri(id string) string {
	return "<" + e.opts.BaseIRI + url.PathEscape(id) + ">"
}

// stepIRI renders the IRI of a plan's step for a phase wire token, minted under
// the plan's own IRI local name so steps of different plans never collide.
func (e *encoder) stepIRI(planID ptypes.PlanID, phaseToken string) string {
	return "<" + e.opts.BaseIRI + url.PathEscape(planID.String()) + "/step/" + phaseToken + ">"
}

// roleIRI mints a role RESOURCE IRI (not a string literal) for a role wire token, so
// prov:hadRole's object is a resource per prov:Role's range. The wire token is the
// escaped local name under BaseIRI. Roles are graph-instance resources, not vocabulary
// individuals, so they live under BaseIRI rather than the ':' vocabulary namespace.
func (e *encoder) roleIRI(roleToken string) string {
	return "<" + e.opts.BaseIRI + "role/" + url.PathEscape(roleToken) + ">"
}

// modelID builds the :LLMAgent :modelId value from the registry-joined model,
// provider-scoped as "<provider>/<name>". Per roadmap §1.1 the run-provenance side
// (which hosted instance judged) should carry the provider-scoped canonical form.
// The value is built from typed ptypes values (Provider, ModelID), never string
// literals, so it does not trip the repo's provider/model-name ast-grep rules.
//
// SIMPLIFIED FORM: "<provider>/<name>" is a stand-in. The RECOMMENDED value space is
// bestiary's full SchemeCanonical grammar (roadmap §1.1:
// <provider>/<family>[/<variant>][/<version>][@<date>]{identity-mods}[attributes]);
// emitting it requires consuming bestiary's EntityRef/ModelRef IRI minting, which
// lands with M3 — provenance's MLAgent.Model carries only (Provider, ModelID) today.
func (e *encoder) modelID(mla ptypes.MLAgent) string {
	provider := mla.Model.Provider
	name := mla.Model.Name
	if e.opts.Registry != nil {
		if entry, ok := e.opts.Registry.Lookup(provider, string(name)); ok {
			provider = entry.Provider
			name = entry.Name
		}
	}
	return provider.String() + "/" + string(name)
}

// literal renders s as a Turtle-escaped double-quoted string literal (xsd:string).
func literal(s string) string {
	return `"` + escapeString(s) + `"`
}

// dateTime renders t as an xsd:dateTime literal in UTC (RFC 3339).
func dateTime(t time.Time) string {
	return `"` + t.UTC().Format(time.RFC3339Nano) + `"^^xsd:dateTime`
}

// escapeString escapes arbitrary text for inclusion inside a Turtle "..." literal.
// Backslash and double-quote are escaped; the common whitespace controls use their
// short forms; any remaining C0 control character is emitted as a \uXXXX escape.
// All other runes (incl. non-ASCII Unicode) pass through verbatim — Turtle files are
// UTF-8.
func escapeString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// phaseToken returns a Phase's stable snake_case wire token (MarshalText). Every
// Phase read from the tracker is valid; an out-of-range value falls back to String().
func phaseToken(ph ptypes.Phase) string {
	if b, err := ph.MarshalText(); err == nil {
		return string(b)
	}
	return ph.String()
}

// roleToken returns a Role's stable snake_case wire token (MarshalText).
func roleToken(r ptypes.Role) string {
	if b, err := r.MarshalText(); err == nil {
		return string(b)
	}
	return r.String()
}
