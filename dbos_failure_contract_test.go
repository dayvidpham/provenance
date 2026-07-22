package provenance

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/internal/testcorpus"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
)

//go:embed testdata/contract/dbos_outcome_failure.yaml
var dbosOutcomeYAML []byte

type dbosOutcomeInput struct {
	Sentinel  string                     `yaml:"sentinel"`
	Control   *dbosMalformedOutcomeInput `yaml:"control"`
	Malformed *dbosMalformedOutcomeInput `yaml:"malformed"`
}

type dbosMalformedOutcomeInput struct {
	Schema            string           `yaml:"schema"`
	OperationID       string           `yaml:"operationID"`
	Kind              ApplyFailureKind `yaml:"kind"`
	Message           string           `yaml:"message"`
	ConflictField     string           `yaml:"conflictField"`
	NestedOperationID string           `yaml:"nestedOperationID"`
	Arms              string           `yaml:"arms"`
}

type dbosOutcomeExpected struct {
	Kind  ApplyFailureKind    `yaml:"kind"`
	JSON  string              `yaml:"json"`
	Class DBOSDiagnosticClass `yaml:"class"`
	Field DBOSDiagnosticField `yaml:"field"`
	Stage DBOSDiagnosticStage `yaml:"stage"`
}

func TestDBOSFailureWireCorpus(t *testing.T) {
	corpus, err := testcorpus.LoadCorpus[dbosOutcomeInput, dbosOutcomeExpected](dbosOutcomeYAML)
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Validate(); err != nil {
		t.Fatal(err)
	}
	const malformedCases = 23
	descriptors := canonicalApplyFailureDescriptors()
	if err := corpus.CheckExact(len(descriptors) + malformedCases); err != nil {
		t.Fatal(err)
	}
	sentinels := map[string]error{
		"operation-conflict": errors.Join(journal.ErrOperationConflict, &journal.OperationConflict{OperationID: "fixture-operation", Field: "mutation digest"}),
		"genesis":            journal.ErrGenesis, "authority-scope": journal.ErrAuthorityScope,
		"assignment-lifecycle": journal.ErrAssignmentLifecycle, "orphaned-evidence": journal.ErrOrphanedEvidence,
		"stale-episode": journal.ErrStaleEpisode, "result-slot-integrity": journal.ErrResultSlotIntegrity,
		"close-without-ending": journal.ErrCloseWithoutEnding, "parent-citation": journal.ErrParentCitation,
		"corrupt-parent-chain": journal.ErrCorruptParentChain, "invalid-id": ptypes.ErrInvalidID,
		"not-found": ErrNotFound, "already-closed": ErrAlreadyClosed, "genesis-required": ErrGenesisRequired,
	}
	seen := map[ApplyFailureKind]struct{}{}
	for _, c := range corpus.Cases {
		if c.Classification == testcorpus.MustFail {
			assertMalformedDBOSOutcome(t, c)
			continue
		}
		sentinel, ok := sentinels[c.Input.Sentinel]
		if !ok || c.Classification != testcorpus.MustPass || c.Expected.Kind == "" || c.Expected.JSON == "" {
			t.Fatalf("outcome fixture %q is outside closed symbolic membership", c.Name)
		}
		if _, duplicate := seen[c.Expected.Kind]; duplicate {
			t.Fatalf("duplicate failure kind %q", c.Expected.Kind)
		}
		seen[c.Expected.Kind] = struct{}{}
		contract := newDBOSContractSnapshot()
		outcome, err := encodeDBOSApplyFailure(contract, "fixture-operation", []byte("m"), fmt.Errorf("fixture failure: %w", sentinel))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(outcome)
		if err != nil || string(raw) != c.Expected.JSON {
			t.Fatalf("failure wire drift for %q: got %s err=%v want %s", c.Name, raw, err, c.Expected.JSON)
		}
		_, decodedErr := decodeDBOSStepOutcome(contract, outcome)
		expectedSentinel := sentinel
		if c.Input.Sentinel == "operation-conflict" {
			expectedSentinel = journal.ErrOperationConflict
		}
		if !errors.Is(decodedErr, expectedSentinel) {
			t.Fatalf("decoded %q error=%v does not preserve %v", c.Name, decodedErr, sentinel)
		}
	}
	if len(seen) != len(descriptors) {
		t.Fatalf("failure wire covers %d kinds, want %d", len(seen), len(descriptors))
	}
}

func assertMalformedDBOSOutcome(t *testing.T, c testcorpus.Case[dbosOutcomeInput, dbosOutcomeExpected]) {
	t.Helper()
	if c.Input.Control == nil || c.Input.Malformed == nil || c.Expected.Class == "" || c.Expected.Field == "" || c.Expected.Stage == "" {
		t.Fatalf("malformed outcome fixture %q lacks typed input or diagnostic metadata", c.Name)
	}
	control := materializeMalformedOutcome(t, c.Name+" control", c.Input.Control)
	if _, err := decodeDBOSStepOutcome(newDBOSContractSnapshot(), control); err == nil {
		t.Fatalf("malformed outcome fixture %q control unexpectedly decoded without its domain failure", c.Name)
	} else {
		var diagnostic *DBOSDiagnosticError
		if errors.As(err, &diagnostic) {
			t.Fatalf("malformed outcome fixture %q control is structurally invalid: %v", c.Name, err)
		}
	}
	malformed := materializeMalformedOutcome(t, c.Name+" malformed", c.Input.Malformed)
	diffs := diffDBOSOutcomeFields(control, malformed)
	if len(diffs) != 1 || diffs[0] != c.Expected.Field {
		t.Fatalf("malformed outcome fixture %q changed fields %v, want exactly [%s]", c.Name, diffs, c.Expected.Field)
	}
	_, err := decodeDBOSStepOutcome(newDBOSContractSnapshot(), malformed)
	var diagnostic *DBOSDiagnosticError
	if !errors.As(err, &diagnostic) || diagnostic.Class != c.Expected.Class || diagnostic.Field != c.Expected.Field || diagnostic.Stage != c.Expected.Stage {
		t.Fatalf("malformed outcome fixture %q diagnostic=%+v err=%v, want %s/%s/%s", c.Name, diagnostic, err, c.Expected.Class, c.Expected.Field, c.Expected.Stage)
	}
}

func materializeMalformedOutcome(t *testing.T, name string, source *dbosMalformedOutcomeInput) DBOSStepOutcome {
	t.Helper()
	outcome := DBOSStepOutcome{Schema: source.Schema, OperationID: source.OperationID}
	failure := &CanonicalApplyFailure{Kind: source.Kind, Message: source.Message, ConflictField: source.ConflictField, OperationID: source.NestedOperationID}
	switch source.Arms {
	case "failure":
		outcome.Failure = failure
	case "success":
		outcome.Success = &CanonicalMutationResult{}
	case "both":
		outcome.Success, outcome.Failure = &CanonicalMutationResult{}, failure
	case "neither":
	default:
		t.Fatalf("outcome fixture %q has unknown arms %q", name, source.Arms)
	}
	return outcome
}

func diffDBOSOutcomeFields(control, malformed DBOSStepOutcome) []DBOSDiagnosticField {
	var diffs []DBOSDiagnosticField
	if control.Schema != malformed.Schema {
		diffs = append(diffs, DBOSDiagFieldSchema)
	}
	if control.OperationID != malformed.OperationID {
		diffs = append(diffs, DBOSDiagFieldOperation)
	}
	if (control.Success == nil) != (malformed.Success == nil) || (control.Failure == nil) != (malformed.Failure == nil) {
		diffs = append(diffs, DBOSDiagFieldSuccessFailure)
	}
	if control.Failure != nil && malformed.Failure != nil {
		if control.Failure.Kind != malformed.Failure.Kind {
			diffs = append(diffs, DBOSDiagFieldKind)
		}
		if control.Failure.Message != malformed.Failure.Message {
			diffs = append(diffs, DBOSDiagFieldMessage)
		}
		if control.Failure.OperationID != malformed.Failure.OperationID {
			diffs = append(diffs, DBOSDiagFieldNestedOpID)
		}
		if control.Failure.ConflictField != malformed.Failure.ConflictField {
			diffs = append(diffs, DBOSDiagFieldConflictField)
		}
	}
	return diffs
}

func TestDBOSOnlyKnownDomainFailuresAreCheckpointable(t *testing.T) {
	descriptors := canonicalApplyFailureDescriptors()
	mutated := canonicalApplyFailureDescriptors()
	mutated[0].kind = "mutated"
	if fresh := canonicalApplyFailureDescriptors(); fresh[0].kind != FailureOperationConflict {
		t.Fatalf("descriptor caller mutation escaped into fresh authority: %q", fresh[0].kind)
	}
	for _, descriptor := range descriptors {
		if matched, err := classifyDomainFailure(fmt.Errorf("wrapped: %w", descriptor.sentinel)); err != nil || matched.kind != descriptor.kind {
			t.Fatalf("known domain failure %q classified as %#v err=%v", descriptor.kind, matched, err)
		}
	}
	for _, code := range []zs.ResultCode{zs.ResultIOErr, zs.ResultFull, zs.ResultReadOnly, zs.ResultCantOpen, zs.ResultBusy, zs.ResultLocked} {
		err := code.ToError()
		if descriptor, classifyErr := classifyDomainFailure(err); classifyErr != err {
			t.Fatalf("operational SQLite error %v classified as durable domain descriptor %#v err=%v", err, descriptor, classifyErr)
		}
		if _, encodeErr := encodeDBOSApplyFailure(newDBOSContractSnapshot(), "operational", []byte("m"), err); encodeErr == nil {
			t.Fatalf("operational SQLite error %v was checkpointable", err)
		}
		outcome, stepErr := checkpointDomainApplyResult(newDBOSContractSnapshot(), journal.OperationInput{OperationID: "operational", MutationDigest: []byte("m")}, journal.CommittedResult{}, err)
		if !errors.Is(stepErr, err) || outcome.Schema != "" || outcome.Success != nil || outcome.Failure != nil {
			t.Fatalf("operational SQLite error %v did not remain on the DBOS Go-error channel: outcome=%#v error=%v", err, outcome, stepErr)
		}
	}
	domainOutcome, domainGoErr := checkpointDomainApplyResult(newDBOSContractSnapshot(), journal.OperationInput{OperationID: "domain", MutationDigest: []byte("m")}, journal.CommittedResult{}, journal.ErrAuthorityScope)
	if domainGoErr != nil || domainOutcome.Failure == nil || domainOutcome.Failure.Kind != FailureAuthorityScope {
		t.Fatalf("known domain error was not checkpointed: outcome=%#v error=%v", domainOutcome, domainGoErr)
	}
	untyped := errors.New("untyped failure")
	if _, classifyErr := classifyDomainFailure(untyped); classifyErr != untyped {
		t.Fatalf("untyped failure classification err=%v, want original", classifyErr)
	}
	joined := errors.Join(journal.ErrGenesis, journal.ErrAuthorityScope)
	_, classifyErr := classifyDomainFailure(joined)
	var ambiguous *AmbiguousApplyFailureError
	if !errors.As(classifyErr, &ambiguous) || !errors.Is(classifyErr, journal.ErrGenesis) || !errors.Is(classifyErr, journal.ErrAuthorityScope) || ambiguous.Class != DBOSDiagClassClassify || ambiguous.Field != DBOSDiagFieldDescriptorMatch || ambiguous.Stage != DBOSDiagStageDomainFoldClassify || ambiguous.Reason == "" || ambiguous.Impact == "" || ambiguous.Fix == "" {
		t.Fatalf("multi-match error is not typed actionable ambiguity: %#v err=%v", ambiguous, classifyErr)
	}
	wantKinds := []ApplyFailureKind{FailureGenesis, FailureAuthorityScope}
	gotKinds := ambiguous.MatchedKinds()
	if fmt.Sprint(gotKinds) != fmt.Sprint(wantKinds) {
		t.Fatalf("matched kinds=%v want %v", gotKinds, wantKinds)
	}
	gotKinds[0] = "mutated"
	if ambiguous.MatchedKinds()[0] != FailureGenesis {
		t.Fatal("MatchedKinds exposed mutable internal evidence")
	}
	typedConflict := &journal.OperationConflict{OperationID: "multi", Field: "command digest"}
	joinedTyped := errors.Join(errors.Join(journal.ErrOperationConflict, typedConflict), journal.ErrGenesis)
	_, typedErr := classifyDomainFailure(joinedTyped)
	var recoveredConflict *journal.OperationConflict
	if !errors.As(typedErr, &recoveredConflict) || recoveredConflict != typedConflict {
		t.Fatalf("ambiguity did not preserve original errors.As cause: recovered=%#v err=%v", recoveredConflict, typedErr)
	}
	if outcome, err := checkpointDomainApplyResult(newDBOSContractSnapshot(), journal.OperationInput{OperationID: "multi", MutationDigest: []byte("m")}, journal.CommittedResult{}, joined); !errors.As(err, &ambiguous) || outcome.Failure != nil {
		t.Fatalf("multi-match failure did not remain on Go-error channel: outcome=%#v err=%v", outcome, err)
	}
}
