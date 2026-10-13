package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dayvidpham/pasture/internal/lifecycle/model"
	"github.com/dayvidpham/pasture/internal/lifecycle/projection"
	"github.com/dayvidpham/provenance"
)

const corpusSHA256 = "e2ec1b5721624ab80aa9d04f7283d5ae3fc4d403ace9fab49616056cb0e27fc0"

func TestPreviousWriterCorpus(t *testing.T) {
	// The published file is immutable; every production writer gets a private copy.
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
	if err = json.Unmarshal(metadata, &m); err != nil {
		t.Fatal(err)
	}
	if m.SHA256 != corpusSHA256 {
		t.Fatal("manifest checksum differs")
	}
	path := filepath.Join(t.TempDir(), "old.db")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	tracker, err := provenance.OpenSQLite(path, provenance.WithModelRegistry(provenance.NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tracker.Close()
	if err = tracker.Journal().VerifyIntegrity(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	root, err := tracker.InitializeGovernedRoot(ctx, m.Genesis)
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := root.Root()
	if !ok || binding != m.Root {
		t.Fatal("previous owner root receipt differs")
	}
	allocated, err := tracker.As(m.Actor, m.Root.AssignmentRow.JournalID).AllocateGovernedComposedBatch(ctx, m.Allocation)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(allocated.Closure().Children(), m.Children) {
		t.Fatal("previous owner allocation receipt differs")
	}
	committed, err := tracker.Journal().LookupCommitted("fixture/opaque/activity")
	if err != nil || len(committed.ResultSlots) != 1 {
		t.Fatalf("activity owner result: %+v, %v", committed, err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	before, err := inventory(db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, m.Blobs) {
		t.Fatal("complete raw blob inventory differs")
	}
	reader := projection.Reader{DB: db, Facts: tracker.Journal().Facts()}
	page, err := reader.Occurrences(ctx, model.OccurrenceQuery{Page: model.PageRequest{Size: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("occurrences: %d", len(page.Items))
	}
	for i, occurrence := range page.Items {
		if occurrence.JournalID() != m.OccurrenceJournalIDs[i] || occurrence.Actor != m.Actor || !reflect.DeepEqual(occurrence.Envelope, m.Envelope) || !reflect.DeepEqual(occurrence.Bindings(), m.Bindings) {
			t.Fatalf("previous lifecycle owner semantic mismatch at %d", i)
		}
		if i > 0 && occurrence.JournalID() <= page.Items[i-1].JournalID() {
			t.Fatal("occurrence order is not ascending")
		}
		body, payloadErr := reader.Payload(ctx, occurrence.Payload.Digest)
		if payloadErr != nil {
			t.Fatal(payloadErr)
		}
		var named map[string]string
		if err = json.Unmarshal(body, &named); err != nil {
			t.Fatal(err)
		}
		if named["old_task_uri"] != m.Children[0].TaskID.String() || named["old_actor_uri"] != m.Actor.String() {
			t.Fatal("named historical URI body changed")
		}
	}
	if page.Items[0].Payload.Digest != page.Items[1].Payload.Digest {
		t.Fatal("occurrences do not share their named body")
	}
	// Prove the scripted age classes using the actual old reclaimer/rebuild.
	outcome, err := projection.RebuildOccurrences(ctx, tracker.Journal(), db, projection.RebuildOptions{Clock: clock{time.Unix(0, m.SnapshotAt)}, Window: time.Duration(m.WriterWindow)})
	if err != nil || outcome.Failure != nil {
		t.Fatalf("old production reclaim: %+v, %v", outcome, err)
	}
	after, err := inventory(db)
	if err != nil {
		t.Fatal(err)
	}
	var eligible []string
	var retained []blob
	for _, b := range before {
		if b.WrittenAt > 0 && b.WrittenAt < m.SnapshotAt-m.WriterWindow && string(b.Body) == "SYNTHETIC unreferenced aged body" {
			eligible = append(eligible, b.Digest)
		} else {
			retained = append(retained, b)
		}
	}
	if len(eligible) != 1 || len(outcome.Reclaimed) != 1 || outcome.Reclaimed[0].String() != eligible[0] || !reflect.DeepEqual(after, retained) {
		t.Fatalf("wrong reclaim inventory/outcome: %+v", outcome)
	}
}

func TestBuilderUsesPreviousOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh-old.db")
	m, err := build(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Children) != 2 || len(m.OccurrenceJournalIDs) != 2 || len(m.Blobs) != 4 || len(m.Verified) != 5 {
		t.Fatalf("incomplete generated owner evidence: %+v", m)
	}
}
