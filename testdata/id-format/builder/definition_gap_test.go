package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/pasture/internal/lifecycle/gate"
	"github.com/dayvidpham/pasture/internal/lifecycle/metamodel"
	"github.com/dayvidpham/pasture/internal/lifecycle/model"
	"github.com/dayvidpham/pasture/internal/lifecycle/receipt"
	"github.com/dayvidpham/provenance"
)

// This is an executable source/API boundary check against the pinned release,
// not a test implementation of a missing definition writer. Changing the old
// owner pin must reopen the investigation rather than silently keep this claim.
func TestPreviousDefinitionWriterAPIBoundary(t *testing.T) {
	constructor := reflect.TypeOf(receipt.NewDefinitionActivation)
	if constructor.NumIn() != 2 || constructor.In(0) != reflect.TypeOf(model.LifecycleMetamodelManifest{}) || constructor.In(1) != reflect.TypeOf([]byte{}) || constructor.NumOut() != 2 || constructor.Out(0) != reflect.TypeOf(receipt.GatedWrite{}) {
		t.Fatal("old definition constructor signature changed; reassess supported kinds")
	}
	write := reflect.TypeOf(receipt.GatedWrite{})
	for i := 0; i < write.NumField(); i++ {
		if write.Field(i).IsExported() {
			t.Fatalf("GatedWrite now exposes field %s; reassess constructor ownership", write.Field(i).Name)
		}
	}
	for _, typ := range []reflect.Type{write, reflect.TypeOf(receipt.Service{}), reflect.TypeOf((*receipt.Service)(nil))} {
		if _, exists := typ.MethodByName("WriteReference"); exists {
			t.Fatalf("%s now has WriteReference; investigate its actual owner contract", typ)
		}
	}
	fn := runtime.FuncForPC(reflect.ValueOf(receipt.NewDefinitionActivation).Pointer())
	file, _ := fn.FileLine(fn.Entry())
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	if filepath.Base(root) != "pasture@v0.0.16" {
		t.Fatalf("source inventory must inspect the pinned released owner, got %s", root)
	}
	kinds := map[string]int{
		"DefinitionRuntimeContract":     0,
		"DefinitionLifecycleSchema":     0,
		"DefinitionEpochImplementation": 0,
		"DefinitionRetentionPolicy":     0,
	}
	var factories []string
	set := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(set, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			if identifier.Name == "WriteReference" {
				t.Errorf("unexpected WriteReference declaration/use in old production source: %s", set.Position(identifier.Pos()))
			}
			if _, tracked := kinds[identifier.Name]; tracked {
				kinds[identifier.Name]++
				if filepath.ToSlash(strings.TrimPrefix(path, root+string(os.PathSeparator))) != "internal/lifecycle/model/definition.go" {
					t.Errorf("%s now has an owner use at %s; reassess the gap", identifier.Name, set.Position(identifier.Pos()))
				}
			}
			return true
		})
		if filepath.Dir(path) != filepath.Dir(file) {
			return nil
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !function.Name.IsExported() || function.Type.Results == nil {
				continue
			}
			for _, result := range function.Type.Results.List {
				if identifier, ok := result.Type.(*ast.Ident); ok && identifier.Name == "GatedWrite" {
					factories = append(factories, function.Name.Name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for kind, count := range kinds {
		if count != 1 {
			t.Errorf("%s has %d production declarations/uses, want only its single unused enum declaration", kind, count)
		}
	}
	sort.Strings(factories)
	if !reflect.DeepEqual(factories, []string{"NewDefinitionActivation", "NewDisclosure", "NewLineageLinks"}) {
		t.Fatalf("old gated-write factory inventory changed: %v", factories)
	}
	if _, err := os.Stat(filepath.Join(root, "internal/lifecycle/definitions")); !os.IsNotExist(err) {
		t.Fatalf("planned generic definition owner unexpectedly exists or cannot be inspected: %v", err)
	}
}

func TestPreviousDefinitionCommitRequiresConstructedWrite(t *testing.T) {
	tracker, m := privatePreviousTracker(t)
	active := metamodel.Active()
	intent, refusal := gate.NewDefinitionActivationIntent(active.Content)
	if refusal != nil {
		t.Fatal(refusal)
	}
	warrant, refusal := gate.Legalize(intent)
	if refusal != nil {
		t.Fatal(refusal)
	}
	service := previousDefinitionService(tracker, m)
	query := provenance.EvidenceQuery{Kinds: []provenance.EvidenceKind{"pasture.lifecycle.definition.v1", "pasture.lifecycle.definition-state.v1", "pasture.lifecycle.occurrence.v1"}, Page: provenance.FactPageRequest{Limit: provenance.MaxFactPageSize}}
	before, err := tracker.Journal().Facts().QueryEvidence(query)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Commit(context.Background(), warrant, receipt.GatedWrite{})
	var gateRefusal *gate.Refusal
	if !errors.As(err, &gateRefusal) || result.Definition != 0 || result.DefinitionState != 0 {
		t.Fatalf("zero/unconstructed write with a real activation warrant must refuse: %+v, %v", result, err)
	}
	after, err := tracker.Journal().Facts().QueryEvidence(query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refused unconstructed write changed committed evidence")
	}
	if err := tracker.Journal().VerifyIntegrity(); err != nil {
		t.Fatal(err)
	}
}

func TestPreviousDefinitionConstructorRejectsMissingOwnerInputs(t *testing.T) {
	for _, test := range []struct {
		name       string
		coordinate model.LifecycleMetamodelManifest
		body       []byte
	}{
		{"missing-coordinate", model.LifecycleMetamodelManifest{}, metamodel.Body()},
		{"missing-owner-body", metamodel.Active(), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			write, err := receipt.NewDefinitionActivation(test.coordinate, test.body)
			if err == nil || write.Class().IsValid() || write.OperationID() != "" {
				t.Fatalf("constructor accepted missing owner inputs: %+v, %v", write, err)
			}
		})
	}
}

func TestPreviousDefinitionOwnerResolvesOnlyMetamodel(t *testing.T) {
	tracker, m := privatePreviousTracker(t)
	service := previousDefinitionService(tracker, m)
	active := metamodel.Active()
	write, err := receipt.NewDefinitionActivation(active, metamodel.Body())
	if err != nil {
		t.Fatal(err)
	}
	intent, refusal := gate.NewDefinitionActivationIntent(active.Content)
	if refusal != nil {
		t.Fatal(refusal)
	}
	warrant, refusal := gate.Legalize(intent)
	if refusal != nil {
		t.Fatal(refusal)
	}
	committed, err := service.Commit(context.Background(), warrant, write)
	if err != nil {
		t.Fatal(err)
	}
	resolved, present, err := receipt.ResolveActiveMetamodel(tracker.Journal())
	if err != nil || !present {
		t.Fatalf("real supported definition resolution failed: %v, %v", present, err)
	}
	if resolved.Definition.Kind != model.DefinitionMetamodel || resolved.Definition.Content != active.Content || resolved.Definition.Definition.JournalID() != committed.Definition || committed.DefinitionState == 0 {
		t.Fatalf("activation/resolver did not return the actual owner metamodel reference: %+v, %+v", committed, resolved)
	}
	page, err := tracker.Journal().Facts().QueryEvidence(provenance.EvidenceQuery{Kinds: []provenance.EvidenceKind{"pasture.lifecycle.definition.v1"}, Page: provenance.FactPageRequest{Limit: provenance.MaxFactPageSize}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Rows[0].JournalID != committed.Definition {
		t.Fatal("old writer did not retain exactly the actual metamodel owner definition")
	}
	// The released codec also normalizes empty arrays to null. Use that real
	// owner's public codec, not handwritten canonical JSON or a guessed digest.
	canonical, err := provenance.Canonicalize(provenance.OperationInput{Effects: []provenance.Effect{{
		Sort: provenance.EffectEvidence, ResultSlot: "definition",
		EvidenceKind: "pasture.lifecycle.definition.v1", ContentDigest: active.Content[:], Payload: metamodel.Body(),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical.NormalizedEffects()[0].Payload, page.Rows[0].Payload) {
		t.Fatal("old writer did not retain the actual owner-normalized metamodel body")
	}
	// Each of the four missing kinds is distinct from the one actual owner-
	// resolved reference. Re-labelling that reference would fabricate evidence.
	for _, missing := range []struct {
		name string
		kind model.DefinitionKind
		ref  model.DefinitionRef
	}{
		{"runtime", model.DefinitionRuntimeContract, m.Envelope.Runtime.Definition},
		{"schema", model.DefinitionLifecycleSchema, m.Envelope.Schema.Definition},
		{"implementation", model.DefinitionEpochImplementation, m.Envelope.Implementation.Definition},
		{"retention", model.DefinitionRetentionPolicy, m.Envelope.Retention.Definition},
	} {
		t.Run(missing.name, func(t *testing.T) {
			if missing.kind == resolved.Definition.Kind || missing.ref != (model.DefinitionRef{}) {
				t.Fatalf("%s is not the documented unsupported zero reference: %+v", missing.name, missing.ref)
			}
		})
	}
	if err := tracker.Journal().VerifyIntegrity(); err != nil {
		t.Fatal(err)
	}
}

func privatePreviousTracker(t *testing.T) (provenance.Tracker, manifest) {
	t.Helper()
	raw, err := os.ReadFile("../previous-writer.db")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != corpusSHA256 {
		t.Fatal("published corpus checksum mismatch")
	}
	metadata, err := os.ReadFile("../previous-writer.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(metadata, &m); err != nil {
		t.Fatal(err)
	}
	if m.SHA256 != corpusSHA256 {
		t.Fatal("manifest checksum differs")
	}
	path := filepath.Join(t.TempDir(), "previous.db")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	tracker, err := provenance.OpenSQLite(path, provenance.WithModelRegistry(provenance.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tracker.Close(); err != nil {
			t.Error(err)
		}
	})
	return tracker, m
}

func previousDefinitionService(tracker provenance.Tracker, m manifest) receipt.Service {
	fixed := clock{time.Unix(0, m.SnapshotAt)}
	return receipt.Service{
		Appender: receipt.JournalAppender{Journal: tracker.Journal(), Deadline: time.Minute, Clock: fixed},
		Identity: identity{receipt.Identity{Actor: m.Actor, Authority: m.Children[0].AssignmentRow.JournalID}},
		Clock:    fixed,
	}
}
