// This separate module executes only released previous production writers.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dayvidpham/pasture/internal/audit"
	"github.com/dayvidpham/pasture/internal/codegen/ir"
	"github.com/dayvidpham/pasture/internal/lifecycle/gate"
	"github.com/dayvidpham/pasture/internal/lifecycle/model"
	"github.com/dayvidpham/pasture/internal/lifecycle/projection"
	"github.com/dayvidpham/pasture/internal/lifecycle/receipt"
	"github.com/dayvidpham/provenance"
	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	_ "modernc.org/sqlite"
)

const namespace = "https://github.com/fixture/my--repo/nested"

type clock struct{ instant time.Time }

func (c clock) Now() time.Time { return c.instant }

type identity struct{ value receipt.Identity }

func (i identity) ResolveLifecycleIdentity(context.Context) (receipt.Identity, error) {
	return i.value, nil
}

type operations struct{ next int }

func (o *operations) NewOperationID() (string, error) {
	o.next++
	return fmt.Sprintf("fixture/receipt/%d", o.next), nil
}

type blob struct {
	Digest    string `json:"digest"`
	Body      []byte `json:"body"`
	ByteCount int    `json:"byte_count"`
	WrittenAt int64  `json:"written_at"`
}

type manifest struct {
	SHA256               string                                       `json:"sha256"`
	Actor                provenance.ActorID                           `json:"actor"`
	Genesis              provenance.RootGenesisRequest                `json:"genesis"`
	Allocation           provenance.GovernedAllocationComposedRequest `json:"allocation"`
	Root                 provenance.GovernedChildBinding              `json:"root"`
	Children             []provenance.GovernedChildBinding            `json:"children"`
	Comment              provenance.CommentID                         `json:"comment"`
	Activity             provenance.ActivityID                        `json:"activity"`
	OccurrenceJournalIDs []provenance.JournalID                       `json:"occurrence_journal_ids"`
	Bindings             []model.NativeBinding                        `json:"native_bindings"`
	Envelope             model.OccurrenceEnvelopeRef                  `json:"envelope"`
	Blobs                []blob                                       `json:"blobs"`
	SnapshotAt           int64                                        `json:"snapshot_at"`
	WriterWindow         int64                                        `json:"writer_window"`
	Verified             []string                                     `json:"verified"`
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func child(name string, actor provenance.ActorID) provenance.GovernedChildSpec {
	return provenance.GovernedChildSpec{
		TaskID:       provenance.TaskID{Namespace: namespace, UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("fixture/task/"+name))},
		AssignmentID: provenance.AssignmentID("fixture/assignment/" + name), Occupant: actor,
		Title: "SYNTHETIC fixture " + name, Description: "Historical prose with -- stays unchanged.",
		Type: provenance.TaskTypeTask, Priority: provenance.PriorityMedium, Phase: provenance.PhaseWorkerSlices,
	}
}

func main() {
	out := flag.String("out", "../previous-writer.db", "new database path (must not exist)")
	metadata := flag.String("manifest", "../previous-writer.json", "new manifest path (must not exist)")
	flag.Parse()
	for _, path := range []string{*out, *metadata} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			panic("refusing existing or inaccessible output: " + path)
		}
	}
	m, err := build(*out)
	check(err)
	bytes, err := os.ReadFile(*out)
	check(err)
	sum := sha256.Sum256(bytes)
	m.SHA256 = hex.EncodeToString(sum[:])
	raw, err := json.MarshalIndent(m, "", "  ")
	check(err)
	check(os.WriteFile(*metadata, append(raw, '\n'), 0644))
	fmt.Printf("%s  %s\n", m.SHA256, *out)
}

func build(path string) (m manifest, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return m, err
	}
	ctx := context.Background()
	tracker, err := provenance.OpenSQLite(path, provenance.WithModelRegistry(provenance.NewRegistry(nil)))
	if err != nil {
		return m, err
	}
	defer tracker.Close()
	trail, err := audit.NewSqliteAuditTrail(path)
	if err != nil {
		return m, err
	}
	if err = trail.Close(); err != nil {
		return m, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return m, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	actor, err := tracker.RegisterSoftwareAgent(namespace, "SYNTHETIC old writer", "1", "fixture")
	if err != nil {
		return m, err
	}
	m.Actor = actor.ID
	m.Genesis = provenance.RootGenesisRequest{OperationID: "fixture/genesis", ActorID: actor.ID, Command: "fixture.root.v1", Root: child("root", actor.ID)}
	rootClosure, err := tracker.InitializeGovernedRoot(ctx, m.Genesis)
	if err != nil {
		return m, err
	}
	var ok bool
	m.Root, ok = rootClosure.Root()
	if !ok {
		return m, fmt.Errorf("missing root closure")
	}
	m.Allocation = provenance.GovernedAllocationComposedRequest{Version: provenance.GovernedAllocationCompositionV1, Allocation: provenance.GovernedAllocationRequest{OperationID: "fixture/allocation", ActorID: actor.ID, Command: "fixture.allocate.v1", ParentAssignmentID: m.Root.AssignmentID, Children: []provenance.GovernedChildSpec{child("first", actor.ID), child("second", actor.ID)}}}
	m.Allocation.SupplementalEffects = []provenance.Effect{{Sort: provenance.EffectTaskEvent, ResultSlot: "fixture/allocation-event", TaskID: m.Allocation.Allocation.Children[0].TaskID, EventKind: "fixture.allocation.supplement", Payload: []byte(`{}`)}}
	allocated, err := tracker.As(actor.ID, m.Root.AssignmentRow.JournalID).AllocateGovernedComposedBatch(ctx, m.Allocation)
	if err != nil {
		return m, err
	}
	allocationClosure := allocated.Closure()
	m.Children = allocationClosure.Children()
	session := tracker.As(actor.ID, m.Children[0].AssignmentRow.JournalID)
	comment, err := session.AddComment(m.Children[0].TaskID, actor.ID, "SYNTHETIC historical comment names "+m.Children[0].TaskID.String())
	if err != nil {
		return m, err
	}
	m.Comment = comment.ID
	m.Activity = provenance.ActivityID{Namespace: namespace, UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("fixture/activity"))}
	opaque := sha256.Sum256([]byte("SYNTHETIC opaque caller command; not an identity recipe"))
	authority := m.Children[0].AssignmentRow.JournalID
	_, err = tracker.Journal().Apply(provenance.OperationInput{
		OperationID: "fixture/opaque/activity", ActorID: actor.ID, AuthorityJournalID: &authority,
		CommandDigest: opaque[:], RecordedAt: time.Unix(1000, 0).UnixNano(),
		Effects: []provenance.Effect{{Sort: provenance.EffectActivityCreate, ResultSlot: "fixture/activity-result", ActivityID: m.Activity, ActivityAgentID: actor.ID, ActivityPhase: provenance.PhaseWorkerSlices, ActivityStage: provenance.StageInProgress, ActivityNotes: "SYNTHETIC activity"}},
	})
	if err != nil {
		return m, err
	}
	fixed := clock{time.Unix(2000, 0)}
	m.SnapshotAt, m.WriterWindow = fixed.Now().UnixNano(), int64(time.Minute)
	contract, err := ir.NewRuntimeContractID(ir.HarnessClaudeCode, "2.1.261")
	if err != nil {
		return m, err
	}
	m.Envelope = model.OccurrenceEnvelopeRef{Runtime: model.RuntimeContractDefinitionRef{Contract: contract}, HostVersion: "2.1.261", HostVersionSource: model.HostVersionCallerSupplied}
	m.Bindings = []model.NativeBinding{{Kind: model.BindingSession, NativeName: "session_id", Value: "SYNTHETIC-session"}, {Kind: model.BindingAgent, NativeName: "agent_id", Value: actor.ID.String()}, {Kind: model.BindingTask, NativeName: "task_id", Value: m.Children[0].TaskID.String()}}
	blobs := receipt.NewSQLiteBlobStore(db, receipt.WithPayloadClock(fixed))
	service := receipt.Service{Blobs: blobs, Appender: receipt.JournalAppender{Journal: tracker.Journal(), Deadline: time.Minute, Clock: fixed}, Identity: identity{receipt.Identity{Actor: actor.ID, Authority: authority}}, Clock: fixed, Operations: &operations{}, Window: time.Minute}
	_, err = receipt.EnsureActiveMetamodel(ctx, service)
	if err != nil {
		return m, err
	}
	intent, refusal := gate.NewDeliveryIntent(contract, 1)
	if refusal != nil {
		return m, refusal
	}
	warrant, refusal := gate.Legalize(intent)
	if refusal != nil {
		return m, refusal
	}
	body, err := json.Marshal(map[string]string{"fixture": "SYNTHETIC host evidence, not live capture", "session_id": "SYNTHETIC-session", "old_task_uri": m.Children[0].TaskID.String(), "old_actor_uri": actor.ID.String()})
	if err != nil {
		return m, err
	}
	for i := 0; i < 2; i++ {
		received, receiveErr := service.Receive(ctx, warrant, receipt.Delivery{Contract: contract, Event: 1, Envelope: m.Envelope, Bindings: m.Bindings, Capture: model.CaptureValid, Body: body})
		if receiveErr != nil {
			return m, receiveErr
		}
		m.OccurrenceJournalIDs = append(m.OccurrenceJournalIDs, received.JournalID())
	}
	// Rebuild before adding orphans: use the ordinary production reclaim policy,
	// never change the policy merely to retain the aged fixture row.
	outcome, err := projection.RebuildOccurrences(ctx, tracker.Journal(), db, projection.RebuildOptions{Clock: fixed, Window: time.Minute})
	if err != nil {
		return m, err
	}
	if outcome.Failure != nil || len(outcome.Reclaimed) != 0 {
		return m, fmt.Errorf("unexpected initial reclaim: %+v", outcome)
	}
	for _, orphan := range []struct {
		name string
		at   time.Time
	}{{"aged", fixed.Now().Add(-2 * time.Minute)}, {"unknown", time.Unix(0, 0)}, {"recent", fixed.Now().Add(-time.Second)}} {
		body := []byte("SYNTHETIC unreferenced " + orphan.name + " body")
		store := receipt.NewSQLiteBlobStore(db, receipt.WithPayloadClock(clock{orphan.at}))
		if err = store.Put(ctx, digest.FromBytes(body), body); err != nil {
			return m, err
		}
	}
	if err = tracker.Journal().VerifyIntegrity(); err != nil {
		return m, err
	}
	// Exact retries invoke the old reducer's receipt reconstruction/verification.
	rootRetry, err := tracker.InitializeGovernedRoot(ctx, m.Genesis)
	if err != nil {
		return m, err
	}
	allocationRetry, err := tracker.As(actor.ID, m.Root.AssignmentRow.JournalID).AllocateGovernedComposedBatch(ctx, m.Allocation)
	if err != nil {
		return m, err
	}
	if !rootClosure.Equal(rootRetry) || !allocationClosure.Equal(allocationRetry.Closure()) {
		return m, fmt.Errorf("receipt reconstruction differs")
	}
	reader := projection.Reader{DB: db, Facts: tracker.Journal().Facts()}
	page, err := reader.Records(ctx, model.OccurrenceQuery{Page: model.PageRequest{Size: 10}})
	if err != nil {
		return m, err
	}
	if len(page.Items) != 2 {
		return m, fmt.Errorf("lifecycle owner decoded %d records, want 2", len(page.Items))
	}
	named, err := reader.Payload(ctx, digest.FromBytes(body))
	if err != nil {
		return m, err
	}
	if string(named) != string(body) {
		return m, fmt.Errorf("named body differs")
	}
	m.Blobs, err = inventory(db)
	if err != nil {
		return m, err
	}
	if len(m.Blobs) != 4 {
		return m, fmt.Errorf("blob inventory has %d entries, want 4", len(m.Blobs))
	}
	if err = tracker.Close(); err != nil {
		return m, err
	}
	// A real standalone production reopen and verifier, not just SQL integrity.
	reopened, err := provenance.OpenSQLite(path, provenance.WithModelRegistry(provenance.NewRegistry(nil)))
	if err != nil {
		return m, err
	}
	err = reopened.Journal().VerifyIntegrity()
	closeErr := reopened.Close()
	if err != nil {
		return m, err
	}
	if closeErr != nil {
		return m, closeErr
	}
	var integrity string
	if err = db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return m, err
	}
	if integrity != "ok" {
		return m, fmt.Errorf("SQLite integrity: %s", integrity)
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return m, err
	}
	hasViolation := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return m, err
	}
	if hasViolation {
		return m, fmt.Errorf("foreign key violation")
	}
	var busy, log, checkpointed int
	if err = db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed); err != nil {
		return m, err
	}
	if busy != 0 || log != checkpointed {
		return m, fmt.Errorf("incomplete checkpoint: %d %d %d", busy, log, checkpointed)
	}
	if err = db.Close(); err != nil {
		return m, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, statErr := os.Stat(path + suffix); !os.IsNotExist(statErr) {
			return m, fmt.Errorf("sidecar survives: %s (%v)", suffix, statErr)
		}
	}
	m.Verified = []string{"previous Journal.VerifyIntegrity before close and after real OpenSQLite reopen", "previous root and governed allocation exact-retry receipt verification", "previous lifecycle Reader.Records and Reader.Payload", "SQLite integrity_check and foreign_key_check", "complete WAL checkpoint, all connections closed, no sidecars"}
	return m, nil
}

func inventory(db *sql.DB) ([]blob, error) {
	rows, err := db.Query("SELECT digest, body, byte_count, written_at FROM lifecycle_payload_blobs ORDER BY digest")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []blob
	for rows.Next() {
		var b blob
		if err := rows.Scan(&b.Digest, &b.Body, &b.ByteCount, &b.WrittenAt); err != nil {
			return nil, err
		}
		if digest.FromBytes(b.Body).String() != b.Digest || len(b.Body) != b.ByteCount {
			return nil, fmt.Errorf("blob integrity mismatch: %s", b.Digest)
		}
		result = append(result, b)
	}
	return result, rows.Err()
}
