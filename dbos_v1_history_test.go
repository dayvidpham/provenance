package provenance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
)

func immutableV1Fingerprint(applicationVersion string, in journal.OperationInput) string {
	h := sha256.New()
	write := func(value []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(value)
	}
	for _, value := range [][]byte{
		[]byte("provenance.dbos-apply-input/v1"), []byte("provenance.apply/v1"),
		[]byte("provenance.dbos-step-outcome/v1"), []byte("github.com/dbos-inc/dbos-transact-golang v0.16.0"),
		[]byte(applicationVersion), []byte(in.ActorID.String()),
	} {
		write(value)
	}
	if in.AuthorityJournalID == nil {
		write([]byte("authority:genesis"))
	} else {
		var authority [8]byte
		binary.BigEndian.PutUint64(authority[:], uint64(*in.AuthorityJournalID))
		write(authority[:])
	}
	write([]byte(in.OperationID))
	write(in.CommandDigest)
	write(in.MutationDigest)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestPersistedV1WorkflowCheckpointRecoversUnderDualRegistration(t *testing.T) {
	dbPath := t.TempDir() + "/v1-history.db"
	actor, authority := seedCrashGenesis(t, dbPath)
	task, _ := ptypes.ParseTaskID("aura--018f0000-0000-7000-8000-000000000021")
	const appName, appVersion = "provenance-v1-history", "history-v1"
	// These are independently authored historical V1 inner bytes, not produced by
	// encodeApplyInput. Only actor/authority are substituted because the fixture DB
	// creates those real foreign-key identities during setup.
	input := DBOSApplyInputV1{
		Schema:   DBOSApplyInputSchemaV1,
		Context:  []byte(fmt.Sprintf(`{"op":"legacy-v1-operation","actor":"%s","authority":%d,"command":"bGVnYWN5LWNvbW1hbmQ=","recorded_at":123}`, actor, authority)),
		Mutation: []byte(`{"mutation_digest":"bGVnYWN5LW11dGF0aW9u","effects":[{"sort":6,"slot":"task","task":"aura--018f0000-0000-7000-8000-000000000021","title":"persisted-v1","desc":"historical durable input","type":2,"prio":2,"phase":8}]}`),
	}
	decoded, err := decodeApplyInput(input)
	if err != nil {
		t.Fatalf("decode independent historical V1 fixture: %v", err)
	}
	if decoded.Effects[0].TaskID != task || decoded.Effects[0].Title != "persisted-v1" {
		t.Fatalf("historical fixture reinterpreted: %#v", decoded)
	}
	fp := immutableV1Fingerprint(appVersion, decoded)
	if got := fingerprintV1(appVersion, decoded); got != fp {
		t.Fatalf("historical V1 fingerprint drifted: got %s want independent %s", got, fp)
	}
	workflowID := "provenance.apply/v1/" + fp

	db1, err := openSharedSQL(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	root1, err := dbos.NewDBOSContext(context.Background(), dbos.Config{AppName: appName, SqliteSystemDB: db1, ApplicationVersion: appVersion})
	if err != nil {
		t.Fatal(err)
	}
	tracker1, err := OpenBorrowedSQLite(db1)
	if err != nil {
		t.Fatal(err)
	}
	adapter1, err := NewDBOSAdapter(root1, tracker1, DBOSAdapterConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := dbos.Launch(root1); err != nil {
		t.Fatal(err)
	}
	handle, err := dbos.RunWorkflow(root1, adapter1.applyWorkflow, input, dbos.WithWorkflowID(workflowID), dbos.WithApplicationVersion(appVersion))
	if err != nil {
		t.Fatalf("persist historical V1 workflow: %v", err)
	}
	firstOutcome, err := handle.GetResult()
	if err != nil {
		t.Fatalf("complete historical V1 workflow: %v", err)
	}
	firstResult, err := tracker1.Journal().LookupCommitted(decoded.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	root1.Shutdown(5 * time.Second)
	_ = tracker1.Close()
	_ = db1.Close()

	// Model a process loss after the V1 step checkpoint but before workflow
	// completion. Keep operation_outputs intact so upgraded recovery must resume the
	// historical V1 step identity rather than execute/reinterpret it.
	stateDB, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stateDB.Exec(`UPDATE workflow_status SET status='PENDING', output=NULL, error=NULL WHERE workflow_uuid=?`, workflowID); err != nil {
		t.Fatalf("mark persisted V1 workflow pending: %v", err)
	}
	_ = stateDB.Close()

	db2, err := openSharedSQL(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	root2, err := dbos.NewDBOSContext(context.Background(), dbos.Config{AppName: appName, SqliteSystemDB: db2, ApplicationVersion: appVersion})
	if err != nil {
		t.Fatal(err)
	}
	tracker2, err := OpenBorrowedSQLite(db2)
	if err != nil {
		t.Fatal(err)
	}
	adapter2, err := NewDBOSAdapter(root2, tracker2, DBOSAdapterConfig{})
	if err != nil {
		t.Fatal(err)
	}
	domainCallbacks := 0
	adapter2.testHooks.afterDomainCommit = func() { domainCallbacks++ }
	if err := dbos.Launch(root2); err != nil {
		t.Fatal(err)
	}
	defer func() { root2.Shutdown(5 * time.Second); _ = tracker2.Close() }()
	recoveredHandle, err := dbos.RetrieveWorkflow[DBOSStepOutcomeV1](root2, workflowID)
	if err != nil {
		t.Fatalf("retrieve recovered V1 workflow: %v", err)
	}
	recoveredOutcome, err := recoveredHandle.GetResult()
	if err != nil {
		t.Fatalf("await recovered V1 workflow: %v", err)
	}
	recoveredResult, err := tracker2.Journal().LookupCommitted(decoded.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if domainCallbacks != 0 {
		t.Fatalf("persisted V1 checkpoint re-executed domain callback %d times", domainCallbacks)
	}
	if !reflect.DeepEqual(recoveredOutcome, firstOutcome) || !reflect.DeepEqual(recoveredResult, firstResult) {
		t.Fatalf("V1 recovery drifted\noutcome got=%#v want=%#v\nresult got=%#v want=%#v", recoveredOutcome, firstOutcome, recoveredResult, firstResult)
	}
	recoveredTask, err := tracker2.Show(task)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredTask.ID != task || recoveredTask.Title != "persisted-v1" || recoveredTask.Description != "historical durable input" || recoveredTask.Status != ptypes.StatusOpen || recoveredTask.Type != ptypes.TaskTypeTask || recoveredTask.Priority != ptypes.PriorityMedium || recoveredTask.Phase != ptypes.PhaseWorkerSlices || recoveredTask.Owner != nil || recoveredTask.Notes != "" || recoveredTask.CreatedAt.UnixNano() != 123 || !recoveredTask.UpdatedAt.Equal(recoveredTask.CreatedAt) || recoveredTask.ClosedAt != nil || recoveredTask.CloseReason != "" {
		t.Fatalf("persisted V1 complete task tuple drifted: %#v", recoveredTask)
	}
	var v1Workflows, v2Workflows, legacySteps int
	if err := db2.QueryRow(`SELECT count(*) FROM workflow_status WHERE workflow_uuid=?`, workflowID).Scan(&v1Workflows); err != nil {
		t.Fatal(err)
	}
	if err := db2.QueryRow(`SELECT count(*) FROM workflow_status WHERE workflow_uuid LIKE 'provenance.apply/v2/%'`).Scan(&v2Workflows); err != nil {
		t.Fatal(err)
	}
	if err := db2.QueryRow(`SELECT count(*) FROM operation_outputs WHERE workflow_uuid=? AND function_name LIKE 'provenance.apply-step/v1/%'`, workflowID).Scan(&legacySteps); err != nil {
		t.Fatal(err)
	}
	if v1Workflows != 1 || v2Workflows != 0 || legacySteps != 1 {
		t.Fatalf("V1/V2 durable identity collision or reinterpretation: v1=%d v2=%d legacySteps=%d", v1Workflows, v2Workflows, legacySteps)
	}
}
