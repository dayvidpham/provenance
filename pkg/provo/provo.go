// Package provo exports a provenance Tracker graph as PROV-O / RO-Crate using the
// data-labour-prov vocabulary minted in the taxonomy-of-benchmarks project
// (its ontology/ directory).
//
// This package is the vocabulary's reference implementation: its Turtle output must
// conform to the vocabulary's SHACL shapes (ontology/shapes.ttl). Both exported
// functions are PURE READS over the Tracker — they never mutate the graph.
//
// # Mapping
//
//	Task              -> prov:Entity
//	Activity          -> prov:Activity (+ p-plan:Activity when a role can be furnished)
//	HumanAgent        -> prov:Person
//	SoftwareAgent     -> prov:SoftwareAgent
//	MLAgent           -> :LLMAgent (rdfs:subClassOf prov:SoftwareAgent) with :modelId
//	EdgeDerivedFrom   -> prov:wasDerivedFrom + an (empty) prov:qualifiedDerivation node
//	EdgeSupersedes    -> prov:wasRevisionOf  + an (empty) prov:qualifiedDerivation node
//	EdgeDiscoveredFrom-> prov:wasInfluencedBy
//	EdgeGeneratedBy   -> prov:wasGeneratedBy
//	EdgeAttributedTo  -> prov:wasAttributedTo
//	EdgeBlockedBy     -> NOT exported (scheduling, not provenance)
//	Phase             -> a static p-plan:Plan of Step-per-phase; each activity
//	                     p-plan:correspondsToStep its phase's step
//
// # Deferred to later milestones (never fabricated here)
//
//   - :derivationKind on the qualified-derivation node (M4): the derivation node is
//     emitted WITHOUT a kind. SHACL's DerivationShape permits maxCount 0.
//   - :modelVersion / :decodingParameters (§2.3 capture fields): only :modelId is
//     emitted, because provenance does not yet capture the other two.
//   - Act-level role: role is synthesized at AGENT level from MLAgent.Role until the
//     qualified-association layer lands. The Turtle header notes this.
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

// planLocalName is the local name of the single built-in plan reifying the Phase enum.
const planLocalName = "plan/pasture-12-phase"

// Fixed external namespaces.
const (
	nsProv  = "http://www.w3.org/ns/prov#"
	nsPplan = "http://purl.org/net/p-plan#"
	nsRdfs  = "http://www.w3.org/2000/01/rdf-schema#"
	nsXsd   = "http://www.w3.org/2001/XMLSchema#"
)

// allPhases is the ordered Phase enum, reified as the built-in plan's steps. It is
// static (not reflected) so the plan preamble is deterministic and reviewable.
var allPhases = []ptypes.Phase{
	ptypes.PhaseRequest, ptypes.PhaseElicit, ptypes.PhasePropose, ptypes.PhaseReview,
	ptypes.PhasePlanUAT, ptypes.PhaseRatify, ptypes.PhaseHandoff, ptypes.PhaseImplPlan,
	ptypes.PhaseWorkerSlices, ptypes.PhaseCodeReview, ptypes.PhaseImplUAT, ptypes.PhaseLanding,
	ptypes.PhaseUnscoped,
}

// Options configures a PROV-O export.
type Options struct {
	// BaseIRI is the graph-instance IRI base for minted local names. IDs render as
	// BaseIRI + url.PathEscape(id.String()). Defaults to DefaultBaseIRI.
	BaseIRI string
	// VocabIRI is the data-labour-prov vocabulary namespace bound to the ':' prefix.
	// Defaults to DefaultVocabIRI (the vocabulary's placeholder namespace).
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

	// detail maps keyed by ID string, populated by load().
	humans   map[string]ptypes.HumanAgent
	mls      map[string]ptypes.MLAgent
	software map[string]ptypes.SoftwareAgent
	// edgesByTask is the sorted, non-blocked-by edge list per task ID string.
	edgesByTask map[string][]ptypes.Edge
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
	fmt.Fprintln(p, "#   MLAgent.Role. Until the qualified-association layer lands,")
	fmt.Fprintln(p, "#   role is a property of the acting ML agent, not of the specific act; only")
	fmt.Fprintln(p, "#   ML-agent activities are typed p-plan:Activity (they can furnish the required")
	fmt.Fprintln(p, "#   agent+role association per the AnnotationActShape).")
	fmt.Fprintln(p, "# NOTE (:derivationKind): NOT emitted here. Qualified")
	fmt.Fprintln(p, "#   derivation nodes carry the fact of derivation and its time, but no typed reason.")
	fmt.Fprintf(p, "# NOTE (namespace): the ':' vocabulary namespace <%s> is a PLACEHOLDER pending w3id registration.\n", e.opts.VocabIRI)
	fmt.Fprintln(p)
	fmt.Fprintf(p, "@prefix :      <%s> .\n", e.opts.VocabIRI)
	fmt.Fprintf(p, "@prefix prov:  <%s> .\n", nsProv)
	fmt.Fprintf(p, "@prefix pplan: <%s> .\n", nsPplan)
	fmt.Fprintf(p, "@prefix rdfs:  <%s> .\n", nsRdfs)
	fmt.Fprintf(p, "@prefix xsd:   <%s> .\n", nsXsd)
	fmt.Fprintln(p)
}

func (e *encoder) plan() {
	p := &e.buf
	planIRI := e.rawIRI(planLocalName)
	fmt.Fprintln(p, "# Built-in plan: the 13-value Phase enum reified as p-plan:Step-per-phase.")
	fmt.Fprintf(p, "%s a pplan:Plan ;\n", planIRI)
	fmt.Fprintf(p, "    rdfs:label %s .\n", literal("pasture-12-phase"))
	for _, ph := range allPhases {
		token := phaseToken(ph)
		fmt.Fprintf(p, "%s a pplan:Step ;\n", e.stepIRI(token))
		fmt.Fprintf(p, "    pplan:isStepOfPlan %s ;\n", planIRI)
		fmt.Fprintf(p, "    rdfs:label %s .\n", literal(token))
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
		fmt.Fprintf(p, "    pplan:correspondsToStep %s ;\n", e.stepIRI(phaseToken(act.Phase)))
		fmt.Fprintf(p, "    prov:wasAssociatedWith %s", e.iri(agentID))
		if isML {
			// Synthesize the qualified association the AnnotationActShape requires:
			// agent + role. Role is agent-level (MLAgent.Role) until the qualified-association layer lands.
			fmt.Fprintf(p, " ;\n    prov:qualifiedAssociation [\n")
			fmt.Fprintf(p, "        a prov:Association ;\n")
			fmt.Fprintf(p, "        prov:agent %s ;\n", e.iri(agentID))
			fmt.Fprintf(p, "        prov:hadRole %s\n", literal(roleToken(mla.Role)))
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
			e.qualifiedDerivation(tgt, ed.CreatedAt)
		case ptypes.EdgeSupersedes:
			fmt.Fprintf(p, "%s prov:wasRevisionOf %s ;\n", src, tgt)
			e.qualifiedDerivation(tgt, ed.CreatedAt)
		case ptypes.EdgeDiscoveredFrom:
			fmt.Fprintf(p, "%s prov:wasInfluencedBy %s .\n", src, tgt)
		case ptypes.EdgeGeneratedBy:
			fmt.Fprintf(p, "%s prov:wasGeneratedBy %s .\n", src, tgt)
		case ptypes.EdgeAttributedTo:
			fmt.Fprintf(p, "%s prov:wasAttributedTo %s .\n", src, tgt)
		}
	}
}

// qualifiedDerivation emits the trailing prov:qualifiedDerivation blank node for a
// derived_from / supersedes edge. The node is typed prov:Derivation and carries the
// derivation's time (from Edge.CreatedAt) but NO :derivationKind (deferred to M4).
// It is written as the continuation of the ';'-terminated relation line above it.
func (e *encoder) qualifiedDerivation(entityIRI string, createdAt time.Time) {
	p := &e.buf
	fmt.Fprintf(p, "    prov:qualifiedDerivation [\n")
	fmt.Fprintf(p, "        a prov:Derivation ;\n")
	fmt.Fprintf(p, "        prov:entity %s", entityIRI)
	if !createdAt.IsZero() {
		fmt.Fprintf(p, " ;\n        prov:atTime %s", dateTime(createdAt))
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

// rawIRI renders a synthetic local name (ASCII, exporter-controlled) under BaseIRI.
func (e *encoder) rawIRI(localName string) string {
	return "<" + e.opts.BaseIRI + localName + ">"
}

// stepIRI renders the plan-step IRI for a phase wire token.
func (e *encoder) stepIRI(phaseToken string) string {
	return e.rawIRI(planLocalName + "/step/" + phaseToken)
}

// modelID builds the :LLMAgent :modelId value from the registry-joined model,
// provider-scoped as "<provider>/<name>". The run-provenance side
// (which hosted instance judged) should carry the provider-scoped canonical form.
// The value is built from typed ptypes values (Provider, ModelID), never string
// literals, so it does not trip the repo's provider/model-name ast-grep rules.
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
