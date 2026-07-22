package provenance

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/internal/testcorpus"
)

//go:embed testdata/contract/dbos_wire.yaml
var dbosWireYAML []byte

//go:embed testdata/contract/dbos_wire_invalid.yaml
var dbosWireInvalidYAML []byte

type dbosWireInput struct {
	ContextHex string `yaml:"contextHex"`
	Mutation   string `yaml:"mutation"`
}

type dbosWireExpected struct {
	Sort        string `yaml:"sort"`
	DigestHex   string `yaml:"digestHex"`
	Fingerprint string `yaml:"fingerprint"`
}

type dbosInvalidInput struct {
	Schema     string `yaml:"schema"`
	ContextHex string `yaml:"contextHex"`
	Mutation   string `yaml:"mutation"`
}

type dbosInvalidExpected struct {
	Rejected bool `yaml:"rejected"`
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
		if !ok || want != c.Expected.Sort || string(c.Mutation.Operator) != c.Name {
			t.Fatalf("wire fixture %q is outside the closed name/operator/sort membership", c.Name)
		}
		if c.Classification != testcorpus.MustPass || c.Input.ContextHex == "" || c.Input.Mutation == "" || c.Expected.DigestHex == "" || c.Expected.Fingerprint == "" {
			t.Fatalf("wire fixture %q is incomplete", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	if len(seen) != len(expected) {
		t.Fatalf("wire fixture membership=%v want exactly %v", seen, expected)
	}
	return corpus
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
			input := DBOSApplyInput{Schema: DBOSApplyInputSchema, Context: contextBytes, Mutation: []byte(c.Input.Mutation)}
			decoded, err := decodeApplyInput(input)
			if err != nil {
				t.Fatalf("decode independently authored bytes: %v", err)
			}
			if len(decoded.Effects) != 1 || decoded.Effects[0].Sort.String() != c.Expected.Sort {
				t.Fatalf("decoded semantics=%#v want sort %q", decoded.Effects, c.Expected.Sort)
			}
			closedSorts[decoded.Effects[0].Sort] = struct{}{}
			digest := sha256.Sum256([]byte(c.Input.Mutation))
			if hex.EncodeToString(digest[:]) != c.Expected.DigestHex || !bytes.Equal(decoded.MutationDigest, digest[:]) {
				t.Fatalf("independently pinned mutation digest drifted: got %x want %s", digest, c.Expected.DigestHex)
			}
			encoded, _, err := encodeApplyInput(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded.Context, contextBytes) || !bytes.Equal(encoded.Mutation, []byte(c.Input.Mutation)) {
				t.Fatal("production encoding differs byte-for-byte from immutable YAML bytes")
			}
			gotFingerprint, err := fingerprint("fixture-app", input)
			if err != nil || gotFingerprint != c.Expected.Fingerprint {
				t.Fatalf("fingerprint=%q err=%v want independently pinned %q", gotFingerprint, err, c.Expected.Fingerprint)
			}
		})
	}
	if len(closedSorts) != 13 {
		t.Fatalf("wire corpus covers %d EffectSort values, want closed set of 13", len(closedSorts))
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
	if err := corpus.CheckExact(8); err != nil {
		t.Fatal(err)
	}
	operators := map[testcorpus.OperatorName]struct{}{
		"unknown-mutation-version": {}, "unknown-mutation-field": {}, "missing-mutation-field": {},
		"duplicate-mutation-field": {}, "trailing-mutation-field": {}, "duplicate-context-field": {},
		"out-of-order-context": {}, "unknown-context-version": {},
	}
	seen := map[testcorpus.OperatorName]struct{}{}
	for _, c := range corpus.Cases {
		if _, ok := operators[c.Mutation.Operator]; !ok || !c.Expected.Rejected || c.Classification != testcorpus.MustFail {
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
		if _, err := decodeApplyInput(DBOSApplyInput{Schema: c.Input.Schema, Context: contextBytes, Mutation: []byte(c.Input.Mutation)}); err == nil {
			t.Fatalf("strict negative fixture %q decoded", c.Name)
		}
	}
	if len(seen) != len(operators) {
		t.Fatalf("malformed fixture membership=%v want %v", seen, operators)
	}
	exact := journal.OperationInput{OperationID: journal.OperationID(strings.Repeat("x", MaxCanonicalFieldBytes)), ActorID: testActorID(t), CommandDigest: bytes.Repeat([]byte{'c'}, MaxCanonicalFieldBytes)}
	if _, err := encodeDBOSContext(exact); err != nil {
		t.Fatalf("exact field bounds rejected: %v", err)
	}
	over := exact
	over.CommandDigest = append(over.CommandDigest, 'x')
	if _, err := encodeDBOSContext(over); err == nil {
		t.Fatal("field over bound accepted")
	}
	valid := loadDBOSWireCorpus(t).Cases[0]
	contextBytes, _ := hex.DecodeString(valid.Input.ContextHex)
	oversized := DBOSApplyInput{Schema: DBOSApplyInputSchema, Context: contextBytes, Mutation: bytes.Repeat([]byte{'x'}, MaxCanonicalMutationBytes+1)}
	if _, err := decodeApplyInput(oversized); err == nil {
		t.Fatal("mutation over bound accepted")
	}
}
