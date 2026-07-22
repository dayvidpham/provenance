package provenance

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/internal/testcorpus"
)

func TestDBOSCorpusInventoryHasExactlyFourAuthorities(t *testing.T) {
	entries, err := os.ReadDir("testdata/contract")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "dbos_") && strings.HasSuffix(entry.Name(), ".yaml") {
			got = append(got, entry.Name())
		}
	}
	slices.Sort(got)
	want := []string{"dbos_outcome_failure.yaml", "dbos_retry_baseline.yaml", "dbos_wire_negative.yaml", "dbos_wire_positive.yaml"}
	if !slices.Equal(got, want) {
		t.Fatalf("DBOS corpus authority inventory=%v want exactly %v", got, want)
	}
}

//go:embed testdata/contract/dbos_wire_positive.yaml
var dbosWireYAML []byte

//go:embed testdata/contract/dbos_wire_negative.yaml
var dbosWireInvalidYAML []byte

type dbosWireInput struct {
	ContextHex string `yaml:"contextHex"`
	Mutation   string `yaml:"mutation"`
}

type dbosWireExpected struct {
	Sort        string              `yaml:"sort"`
	DigestHex   string              `yaml:"digestHex"`
	Fingerprint string              `yaml:"fingerprint"`
	Context     dbosExpectedContext `yaml:"context"`
	Effect      dbosExpectedEffect  `yaml:"effect"`
}

type dbosExpectedContext struct {
	OperationID string `yaml:"operationID"`
	ActorID     string `yaml:"actorID"`
	Authority   int64  `yaml:"authority"`
	Command     string `yaml:"command"`
	RecordedAt  int64  `yaml:"recordedAt"`
}

type dbosExpectedEventContext struct {
	Kind     string `yaml:"kind"`
	Identity string `yaml:"identity"`
}

type dbosExpectedEffect struct {
	Sort               string                     `yaml:"sort"`
	ResultSlot         string                     `yaml:"resultSlot"`
	RecordedAtOverride *int64                     `yaml:"recordedAtOverride"`
	TaskID             string                     `yaml:"taskID"`
	EventKind          string                     `yaml:"eventKind"`
	Payload            string                     `yaml:"payload"`
	Contexts           []dbosExpectedEventContext `yaml:"contexts"`
	Title              string                     `yaml:"title"`
	Description        string                     `yaml:"description"`
	Type               string                     `yaml:"type"`
	Priority           string                     `yaml:"priority"`
	Phase              string                     `yaml:"phase"`
	CloseReason        string                     `yaml:"closeReason"`
	Forced             bool                       `yaml:"forced"`
	UpdateTitle        *string                    `yaml:"updateTitle"`
	UpdateDescription  *string                    `yaml:"updateDescription"`
	UpdatePriority     *string                    `yaml:"updatePriority"`
	UpdatePhase        *string                    `yaml:"updatePhase"`
	UpdateNotes        *string                    `yaml:"updateNotes"`
	BootstrapLabel     string                     `yaml:"bootstrapLabel"`
	OperationAuthority string                     `yaml:"operationAuthority"`
	AssignmentID       string                     `yaml:"assignmentID"`
	AssignmentSlot     string                     `yaml:"assignmentSlot"`
	Occupant           string                     `yaml:"occupant"`
	Predecessor        string                     `yaml:"predecessor"`
	Parent             string                     `yaml:"parent"`
	DecisionKind       string                     `yaml:"decisionKind"`
	EvidenceKind       string                     `yaml:"evidenceKind"`
	ContentDigestHex   string                     `yaml:"contentDigestHex"`
	EdgeTarget         string                     `yaml:"edgeTarget"`
	EdgeKind           string                     `yaml:"edgeKind"`
	Label              string                     `yaml:"label"`
	CommentID          string                     `yaml:"commentID"`
	CommentAuthor      string                     `yaml:"commentAuthor"`
	CommentBody        string                     `yaml:"commentBody"`
}

type dbosInvalidInput struct {
	Schema     string `yaml:"schema"`
	ContextHex string `yaml:"contextHex"`
	Mutation   string `yaml:"mutation"`
}

type dbosInvalidExpected struct {
	ErrorClass DBOSDiagnosticClass `yaml:"errorClass"`
	Field      DBOSDiagnosticField `yaml:"field"`
}

func loadDBOSWireCorpus(t *testing.T) testcorpus.Corpus[dbosWireInput, dbosWireExpected] {
	t.Helper()
	corpus, err := testcorpus.LoadCorpus[dbosWireInput, dbosWireExpected](dbosWireYAML)
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := corpus.CheckExact(14); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"task-event-update": "task_event", "task-event-close": "task_event",
		"bootstrap-authority": "bootstrap_authority", "assignment-start": "assignment_start",
		"assignment-end": "assignment_end", "decision": "decision", "evidence": "evidence",
		"task-create": "task_create", "task-create-allocated": "task_create_allocated",
		"edge-add": "edge_add", "edge-remove": "edge_remove", "label-add": "label_add",
		"label-remove": "label_remove", "comment-add": "comment_add",
	}
	seen := make(map[string]struct{}, len(corpus.Cases))
	for _, c := range corpus.Cases {
		want, ok := expected[c.Name]
		if !ok || want != c.Expected.Sort || c.Expected.Effect.Sort != want || string(c.Mutation.Operator) != c.Name {
			t.Fatalf("wire fixture %q is outside the closed name/operator/sort membership", c.Name)
		}
		if c.Classification != testcorpus.MustPass || c.Input.ContextHex == "" || c.Input.Mutation == "" || c.Expected.DigestHex == "" || c.Expected.Fingerprint == "" || c.Expected.Context.OperationID == "" || c.Expected.Context.ActorID == "" || c.Expected.Context.Command == "" || c.Expected.Effect.RecordedAtOverride == nil {
			t.Fatalf("wire fixture %q is incomplete", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	if len(seen) != len(expected) {
		t.Fatalf("wire fixture membership=%v want exactly %v", seen, expected)
	}
	return corpus
}

func expectedDBOSOperation(t *testing.T, expected dbosWireExpected) journal.OperationInput {
	t.Helper()
	actor, err := ParseActorID(expected.Context.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	authority := JournalID(expected.Context.Authority)
	e := expected.Effect
	task, err := ParseTaskID(e.TaskID)
	if err != nil && e.TaskID != "" {
		t.Fatal(err)
	}
	occupant, err := ParseActorID(e.Occupant)
	if err != nil && e.Occupant != "" {
		t.Fatal(err)
	}
	comment, err := ParseCommentID(e.CommentID)
	if err != nil && e.CommentID != "" {
		t.Fatal(err)
	}
	var contexts []EventContext
	if len(e.Contexts) > 0 {
		contexts = make([]EventContext, len(e.Contexts))
	}
	for i, source := range e.Contexts {
		if source.Kind != "task" {
			t.Fatalf("unknown expected context kind %q", source.Kind)
		}
		contextTask, parseErr := ParseTaskID(source.Identity)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		contexts[i], err = TaskContext(contextTask)
		if err != nil {
			t.Fatal(err)
		}
	}
	var priority *Priority
	if e.UpdatePriority != nil {
		value := map[string]Priority{"high": PriorityHigh}[*e.UpdatePriority]
		priority = &value
	}
	var phase *Phase
	if e.UpdatePhase != nil {
		value := map[string]Phase{"code_review": PhaseCodeReview}[*e.UpdatePhase]
		phase = &value
	}
	effect := Effect{
		Sort:       map[string]EffectSort{"task_event": EffectTaskEvent, "bootstrap_authority": EffectBootstrapAuthority, "assignment_start": EffectAssignmentStart, "assignment_end": EffectAssignmentEnd, "decision": EffectDecision, "evidence": EffectEvidence, "task_create": EffectTaskCreate, "task_create_allocated": EffectTaskCreateAllocated, "edge_add": EffectEdgeAdd, "edge_remove": EffectEdgeRemove, "label_add": EffectLabelAdd, "label_remove": EffectLabelRemove, "comment_add": EffectCommentAdd}[e.Sort],
		ResultSlot: ResultSlotID(e.ResultSlot), RecordedAtOverride: e.RecordedAtOverride,
		TaskID: task, EventKind: EventKind(e.EventKind), Contexts: contexts,
		Title: e.Title, Description: e.Description,
		Type: map[string]TaskType{"task": TaskTypeTask}[e.Type], Priority: map[string]Priority{"medium": PriorityMedium}[e.Priority], Phase: map[string]Phase{"unscoped": PhaseUnscoped}[e.Phase],
		CloseReason: e.CloseReason, Forced: e.Forced, UpdateTitle: e.UpdateTitle, UpdateDescription: e.UpdateDescription, UpdatePriority: priority, UpdatePhase: phase, UpdateNotes: e.UpdateNotes,
		BootstrapLabel: e.BootstrapLabel, OperationAuthorityID: OperationAuthorityID(e.OperationAuthority),
		AssignmentID: AssignmentID(e.AssignmentID), SlotID: AssignmentSlotID(e.AssignmentSlot), Occupant: occupant, Predecessor: AssignmentID(e.Predecessor), Parent: AssignmentID(e.Parent),
		DecisionKind: DecisionKind(e.DecisionKind), EvidenceKind: EvidenceKind(e.EvidenceKind), EdgeTargetID: e.EdgeTarget, EdgeRelKind: map[string]EdgeKind{"derived_from": EdgeDerivedFrom}[e.EdgeKind], Label: e.Label,
		CommentIdentity: comment, CommentAuthor: func() ActorID { value, _ := ParseActorID(e.CommentAuthor); return value }(), CommentBody: e.CommentBody,
	}
	if e.Payload != "" {
		effect.Payload = json.RawMessage(e.Payload)
	}
	if e.ContentDigestHex != "" {
		effect.ContentDigest, err = hex.DecodeString(e.ContentDigestHex)
		if err != nil {
			t.Fatal(err)
		}
	}
	digest, err := hex.DecodeString(expected.DigestHex)
	if err != nil {
		t.Fatal(err)
	}
	return OperationInput{OperationID: OperationID(expected.Context.OperationID), ActorID: actor, AuthorityJournalID: &authority, CommandDigest: []byte(expected.Context.Command), MutationDigest: digest, RecordedAt: expected.Context.RecordedAt, Effects: []Effect{effect}}
}

func TestDBOSIndependentImmutableWireFixturesEveryFamily(t *testing.T) {
	corpus := loadDBOSWireCorpus(t)
	closedSorts := make(map[journal.EffectSort]struct{})
	for _, c := range corpus.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			contextBytes, err := hex.DecodeString(c.Input.ContextHex)
			if err != nil {
				t.Fatalf("literal context hex: %v", err)
			}
			contract := newDBOSContractSnapshot()
			input := DBOSApplyInput{Schema: contract.applyInputSchema, Context: contextBytes, Mutation: []byte(c.Input.Mutation)}
			decoded, err := decodeApplyInput(contract, input)
			if err != nil {
				t.Fatalf("decode independently authored bytes: %v", err)
			}
			expectedOperation := expectedDBOSOperation(t, c.Expected)
			if !reflect.DeepEqual(decoded, expectedOperation) {
				t.Fatalf("decoded complete semantics=%#v want independently authored %#v", decoded, expectedOperation)
			}
			closedSorts[decoded.Effects[0].Sort] = struct{}{}
			digest := sha256.Sum256([]byte(c.Input.Mutation))
			if hex.EncodeToString(digest[:]) != c.Expected.DigestHex || !bytes.Equal(decoded.MutationDigest, digest[:]) {
				t.Fatalf("independently pinned mutation digest drifted: got %x want %s", digest, c.Expected.DigestHex)
			}
			encoded, _, err := encodeApplyInput(contract, decoded)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded.Context, contextBytes) || !bytes.Equal(encoded.Mutation, []byte(c.Input.Mutation)) {
				t.Fatal("production encoding differs byte-for-byte from immutable YAML bytes")
			}
			gotFingerprint, err := fingerprint(contract, "fixture-app", input)
			if err != nil || gotFingerprint != c.Expected.Fingerprint {
				t.Fatalf("fingerprint=%q err=%v want independently pinned %q", gotFingerprint, err, c.Expected.Fingerprint)
			}
		})
	}
	if len(closedSorts) != 13 {
		t.Fatalf("wire corpus covers %d EffectSort values, want closed set of 13", len(closedSorts))
	}
}

func TestDBOSWireYAMLSemanticsAreAuthoritative(t *testing.T) {
	corpus := loadDBOSWireCorpus(t)
	original := corpus.Cases[7].Expected
	changed := original
	changed.Effect.Title = "changed-title"
	if reflect.DeepEqual(expectedDBOSOperation(t, original), expectedDBOSOperation(t, changed)) {
		t.Fatal("changing a YAML semantic expectation did not alter the expected operation")
	}
}

func TestDBOSIndependentStrictNegativeFixturesAndBounds(t *testing.T) {
	corpus, err := testcorpus.LoadCorpus[dbosInvalidInput, dbosInvalidExpected](dbosWireInvalidYAML)
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := corpus.CheckExact(10); err != nil {
		t.Fatal(err)
	}
	operators := map[testcorpus.OperatorName]struct{}{
		"unknown-mutation-version": {}, "unknown-mutation-field": {}, "missing-mutation-field": {},
		"duplicate-mutation-field": {}, "trailing-mutation-field": {}, "duplicate-context-field": {},
		"out-of-order-context": {}, "unknown-context-version": {},
		"zero-actor": {}, "empty-command-digest": {},
	}
	seen := map[testcorpus.OperatorName]struct{}{}
	for _, c := range corpus.Cases {
		if _, ok := operators[c.Mutation.Operator]; !ok || c.Expected.ErrorClass == "" || c.Expected.Field == "" || c.Classification != testcorpus.MustFail {
			t.Fatalf("invalid fixture %q is outside closed malformed membership", c.Name)
		}
		if _, duplicate := seen[c.Mutation.Operator]; duplicate {
			t.Fatalf("duplicate malformed operator %q", c.Mutation.Operator)
		}
		seen[c.Mutation.Operator] = struct{}{}
		contextBytes, contextErr := hex.DecodeString(c.Input.ContextHex)
		if contextErr != nil || c.Input.Mutation == "" {
			t.Fatalf("fixture %q invalid bytes: context=%v mutation-empty=%v", c.Name, contextErr, c.Input.Mutation == "")
		}
		contract := newDBOSContractSnapshot()
		_, err := decodeApplyInput(contract, DBOSApplyInput{Schema: c.Input.Schema, Context: contextBytes, Mutation: []byte(c.Input.Mutation)})
		if err == nil {
			t.Fatalf("strict negative fixture %q decoded", c.Name)
		}
		switch c.Expected.ErrorClass {
		case DBOSDiagClassContextFrame:
			if _, mutationErr := journal.DecodeCanonicalMutation([]byte(c.Input.Mutation)); mutationErr != nil {
				t.Fatalf("context fixture %q is masked by invalid mutation: %v", c.Name, mutationErr)
			}
			var typed *DBOSContextFrameError
			if !errors.Is(err, ErrDBOSContextFrame) || !errors.As(err, &typed) || typed.Field != c.Expected.Field {
				t.Fatalf("fixture %q error=%v, want context field %q", c.Name, err, c.Expected.Field)
			}
		case DBOSDiagClassCanonicalMutation:
			if _, contextErr := decodeDBOSContext(contract, contextBytes); contextErr != nil {
				t.Fatalf("mutation fixture %q is masked by invalid context: %v", c.Name, contextErr)
			}
			var typed *journal.CanonicalMutationError
			if !errors.Is(err, journal.ErrCanonicalMutation) || !errors.As(err, &typed) || typed.Field != string(c.Expected.Field) {
				t.Fatalf("fixture %q error=%v, want canonical field %q", c.Name, err, c.Expected.Field)
			}
		default:
			t.Fatalf("fixture %q has unknown expected error class %q", c.Name, c.Expected.ErrorClass)
		}
	}
	if len(seen) != len(operators) {
		t.Fatalf("malformed fixture membership=%v want %v", seen, operators)
	}
	exact := journal.OperationInput{OperationID: journal.OperationID(strings.Repeat("x", MaxCanonicalFieldBytes)), ActorID: testActorID(t), CommandDigest: bytes.Repeat([]byte{'c'}, MaxCanonicalFieldBytes)}
	contract := newDBOSContractSnapshot()
	if _, err := encodeDBOSContext(contract, exact); err != nil {
		t.Fatalf("exact field bounds rejected: %v", err)
	}
	over := exact
	over.CommandDigest = append(over.CommandDigest, 'x')
	if _, err := encodeDBOSContext(contract, over); err == nil {
		t.Fatal("field over bound accepted")
	}
	for name, mutate := range map[string]func(*journal.OperationInput){
		"zero-actor":    func(in *journal.OperationInput) { in.ActorID = journal.ActorID{} },
		"empty-command": func(in *journal.OperationInput) { in.CommandDigest = nil },
	} {
		candidate := exact
		mutate(&candidate)
		_, err := encodeDBOSContext(contract, candidate)
		var typed *DBOSContextFrameError
		if !errors.Is(err, ErrDBOSContextFrame) || !errors.As(err, &typed) || typed.Field == "" {
			t.Fatalf("encode %s error=%v, want typed context rejection", name, err)
		}
	}
	valid := loadDBOSWireCorpus(t).Cases[0]
	contextBytes, _ := hex.DecodeString(valid.Input.ContextHex)
	oversized := DBOSApplyInput{Schema: contract.applyInputSchema, Context: contextBytes, Mutation: bytes.Repeat([]byte{'x'}, MaxCanonicalMutationBytes+1)}
	if _, err := decodeApplyInput(contract, oversized); err == nil {
		t.Fatal("mutation over bound accepted")
	}
}
