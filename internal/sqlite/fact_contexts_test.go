package sqlite

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestFactContextsPersistCanonicalReopenAndExactReplay(t *testing.T) {
	path := t.TempDir() + "/fact-contexts.db"
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	actor, task, boot := newFactContextEnvironment(t, db)
	taskContext, err := journal.TaskContext(task)
	if err != nil {
		t.Fatal(err)
	}
	actorContext, err := journal.ActorContext(actor)
	if err != nil {
		t.Fatal(err)
	}
	op := journal.OperationInput{
		OperationID:        "fact-context-persist",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("fact-context-persist"),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{
			{Sort: journal.EffectDecision, ResultSlot: "decision", TaskID: task, DecisionKind: "fixture.context.decision", Payload: []byte(`{}`), Contexts: []journal.EventContext{actorContext, taskContext, actorContext}},
			{Sort: journal.EffectEvidence, ResultSlot: "evidence", TaskID: task, EvidenceKind: "fixture.context.evidence", ContentDigest: []byte("evidence"), Payload: []byte(`{}`), Contexts: []journal.EventContext{taskContext, actorContext, taskContext}},
		},
	}
	result, err := db.Apply(op)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	decisionJID := factContextResultSlot(t, result, "decision")
	evidenceJID := factContextResultSlot(t, result, "evidence")
	expected, err := journal.CanonicalEventContexts([]journal.EventContext{actorContext, taskContext})
	if err != nil {
		t.Fatal(err)
	}
	assertStoredFactContexts(t, db, factContextDecision, decisionJID, expected)
	assertStoredFactContexts(t, db, factContextEvidence, evidenceJID, expected)

	beforeReplay := snapshotAllDurableTables(t, db)
	replayed, err := db.Apply(op)
	if err != nil || !replayed.ShortCircuited || replayed.AnchorJournalID != result.AnchorJournalID {
		t.Fatalf("exact replay = %+v, %v", replayed, err)
	}
	if afterReplay := snapshotAllDurableTables(t, db); !reflect.DeepEqual(afterReplay, beforeReplay) {
		t.Fatalf("exact replay changed durable state\nbefore: %#v\nafter: %#v", beforeReplay, afterReplay)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err = Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	assertStoredFactContexts(t, db, factContextDecision, decisionJID, expected)
	assertStoredFactContexts(t, db, factContextEvidence, evidenceJID, expected)
	if err := db.VerifyIntegrity(); err != nil {
		t.Fatalf("VerifyIntegrity after reopen: %v", err)
	}
	if _, err := db.ReplayProjections(); err != nil {
		t.Fatalf("ReplayProjections after reopen: %v", err)
	}
}

func TestFactContextApplyRollbackLeavesNoAnchorsFactsOrContexts(t *testing.T) {
	db := newJournalDB(t)
	actor, task, boot := newFactContextEnvironment(t, db)
	ctx, err := journal.TaskContext(task)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotAllDurableTables(t, db)
	_, err = db.AdversarialApplyWithFault(journal.OperationInput{
		OperationID:        "fact-context-late-fault",
		ActorID:            actor,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("fact-context-late-fault"),
		Effects: []journal.Effect{
			{Sort: journal.EffectDecision, TaskID: task, DecisionKind: "fixture.context.rollback", Payload: []byte(`{}`), Contexts: []journal.EventContext{ctx}},
			{Sort: journal.EffectEvidence, TaskID: task, EvidenceKind: "fixture.context.rollback", ContentDigest: []byte("rollback"), Payload: []byte(`{}`), Contexts: []journal.EventContext{ctx}},
		},
	}, 1)
	if !errors.Is(err, journal.ErrInjectedFault) {
		t.Fatalf("AdversarialApplyWithFault error = %v, want ErrInjectedFault", err)
	}
	if after := snapshotAllDurableTables(t, db); !reflect.DeepEqual(after, before) {
		t.Fatalf("late Apply fault changed durable state\nbefore: %#v\nafter: %#v", before, after)
	}
}

func TestFactContextLegacyActivationBackfillsCanonicalOnly(t *testing.T) {
	t.Run("canonical backfill is one-time", func(t *testing.T) {
		path := t.TempDir() + "/e66-canonical.db"
		db, actor, task, boot := openFactContextFixture(t, path)
		ctx, err := journal.TaskContext(task)
		if err != nil {
			t.Fatal(err)
		}
		result, err := db.Apply(journal.OperationInput{
			OperationID: "e66-canonical-context", ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("e66-canonical-context"),
			Effects: []journal.Effect{{Sort: journal.EffectDecision, ResultSlot: "decision", TaskID: task, DecisionKind: "fixture.e66.canonical", Payload: []byte(`{}`), Contexts: []journal.EventContext{ctx}}},
		})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		decisionJID := factContextResultSlot(t, result, "decision")
		dropFactContextRelations(t, db)
		if err := db.Close(); err != nil {
			t.Fatalf("close e66 fixture: %v", err)
		}

		db, err = Open(path, nil)
		if err != nil {
			t.Fatalf("activate e66 canonical file: %v", err)
		}
		assertStoredFactContexts(t, db, factContextDecision, decisionJID, []journal.EventContext{ctx})
		afterFirstActivation := snapshotAllDurableTables(t, db)
		if err := db.Close(); err != nil {
			t.Fatalf("close first activation: %v", err)
		}
		db, err = Open(path, nil)
		if err != nil {
			t.Fatalf("reopen activated file: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if afterSecondActivation := snapshotAllDurableTables(t, db); !reflect.DeepEqual(afterSecondActivation, afterFirstActivation) {
			t.Fatalf("repeated activation changed durable state\nfirst:  %#v\nsecond: %#v", afterFirstActivation, afterSecondActivation)
		}
	})

	t.Run("opaque legacy operation has no synthetic contexts", func(t *testing.T) {
		path := t.TempDir() + "/e66-opaque.db"
		db, actor, task, boot := openFactContextFixture(t, path)
		ctx, err := journal.TaskContext(task)
		if err != nil {
			t.Fatal(err)
		}
		result, err := db.Apply(journal.OperationInput{
			OperationID: "e66-opaque-context", ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("e66-opaque-context"),
			Effects: []journal.Effect{{Sort: journal.EffectEvidence, ResultSlot: "evidence", EvidenceKind: "fixture.e66.opaque", ContentDigest: []byte("opaque"), Payload: []byte(`{}`), Contexts: []journal.EventContext{ctx}}},
		})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		evidenceJID := factContextResultSlot(t, result, "evidence")
		scope := takePoolScope(t, db)
		if err := sqlitex.Execute(scope.conn, "UPDATE journal_operations SET mutation_encoding_version=?1,canonical_mutation=?2 WHERE journal_id=?3", &sqlitex.ExecOptions{Args: []any{nil, nil, int64(result.AnchorJournalID)}}); err != nil {
			scope.release()
			t.Fatalf("make operation opaque: %v", err)
		}
		scope.release()
		dropFactContextRelations(t, db)
		if err := db.Close(); err != nil {
			t.Fatalf("close e66 opaque fixture: %v", err)
		}

		db, err = Open(path, nil)
		if err != nil {
			t.Fatalf("activate e66 opaque file: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		assertStoredFactContexts(t, db, factContextEvidence, evidenceJID, nil)
		if err := db.VerifyIntegrity(); err != nil {
			t.Fatalf("VerifyIntegrity opaque legacy activation: %v", err)
		}
	})
}

func TestFactContextStartupFailuresPreserveFiles(t *testing.T) {
	t.Run("only one relation", func(t *testing.T) {
		path := t.TempDir() + "/one-relation.db"
		db, _, _, _ := openFactContextFixture(t, path)
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		conn, err := zs.OpenConn(path, zs.OpenReadWrite)
		if err != nil {
			t.Fatalf("open raw fixture: %v", err)
		}
		if err := sqlitex.ExecuteTransient(conn, "DROP TABLE journal_evidence_contexts", nil); err != nil {
			_ = conn.Close()
			t.Fatalf("drop evidence context relation: %v", err)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("close raw fixture: %v", err)
		}
		assertOpenFactContextFailurePreservesBytes(t, path)
	})

	t.Run("malformed relation", func(t *testing.T) {
		path := t.TempDir() + "/malformed-relation.db"
		db, _, _, _ := openFactContextFixture(t, path)
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		conn, err := zs.OpenConn(path, zs.OpenReadWrite)
		if err != nil {
			t.Fatalf("open raw fixture: %v", err)
		}
		if err := sqlitex.ExecuteTransient(conn, "DROP TABLE journal_decision_contexts", nil); err != nil {
			_ = conn.Close()
			t.Fatalf("drop decision context relation: %v", err)
		}
		if err := sqlitex.ExecuteTransient(conn, "CREATE TABLE journal_decision_contexts (decision_journal_id INTEGER NOT NULL REFERENCES journal_decisions(journal_id), context_kind TEXT NOT NULL, context_identity TEXT NOT NULL, PRIMARY KEY (decision_journal_id, context_kind, context_identity)) STRICT", nil); err != nil {
			_ = conn.Close()
			t.Fatalf("create malformed relation: %v", err)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("close raw fixture: %v", err)
		}
		assertOpenFactContextFailurePreservesBytes(t, path)
	})

	t.Run("malformed row", func(t *testing.T) {
		path := t.TempDir() + "/malformed-row.db"
		db, actor, task, boot := openFactContextFixture(t, path)
		ctx, err := journal.TaskContext(task)
		if err != nil {
			t.Fatal(err)
		}
		result, err := db.Apply(journal.OperationInput{
			OperationID: "malformed-context-row", ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("malformed-context-row"),
			Effects: []journal.Effect{{Sort: journal.EffectDecision, ResultSlot: "decision", TaskID: task, DecisionKind: "fixture.context.malformed", Payload: []byte(`{}`), Contexts: []journal.EventContext{ctx}}},
		})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		conn, err := zs.OpenConn(path, zs.OpenReadWrite)
		if err != nil {
			t.Fatalf("open raw fixture: %v", err)
		}
		if err := sqlitex.Execute(conn, "INSERT INTO journal_decision_contexts (decision_journal_id,context_kind,context_identity) VALUES (?1,?2,?3)", &sqlitex.ExecOptions{Args: []any{int64(factContextResultSlot(t, result, "decision")), "git", "not-a-git-oid"}}); err != nil {
			_ = conn.Close()
			t.Fatalf("insert malformed context row: %v", err)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("close raw fixture: %v", err)
		}
		assertOpenFactContextFailurePreservesBytes(t, path)
	})
}

func TestFactContextIntegrityRejectsStoredCorruption(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt func(t *testing.T, db *DB, result journal.CommittedResult, actor journal.ActorID)
	}{
		{
			name: "missing",
			corrupt: func(t *testing.T, db *DB, result journal.CommittedResult, _ journal.ActorID) {
				scope := takePoolScope(t, db)
				defer scope.release()
				if err := sqlitex.Execute(scope.conn, "DELETE FROM journal_decision_contexts WHERE decision_journal_id=?1", &sqlitex.ExecOptions{Args: []any{int64(factContextResultSlot(t, result, "decision"))}}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "extra",
			corrupt: func(t *testing.T, db *DB, result journal.CommittedResult, actor journal.ActorID) {
				ctx, err := journal.ActorContext(actor)
				if err != nil {
					t.Fatal(err)
				}
				kind, identity, _ := journal.EncodeStoredEventContext(ctx)
				scope := takePoolScope(t, db)
				defer scope.release()
				if err := sqlitex.Execute(scope.conn, "INSERT INTO journal_decision_contexts (decision_journal_id,context_kind,context_identity) VALUES (?1,?2,?3)", &sqlitex.ExecOptions{Args: []any{int64(factContextResultSlot(t, result, "decision")), string(kind), identity}}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed",
			corrupt: func(t *testing.T, db *DB, result journal.CommittedResult, _ journal.ActorID) {
				scope := takePoolScope(t, db)
				defer scope.release()
				if err := sqlitex.Execute(scope.conn, "INSERT INTO journal_decision_contexts (decision_journal_id,context_kind,context_identity) VALUES (?1,?2,?3)", &sqlitex.ExecOptions{Args: []any{int64(factContextResultSlot(t, result, "decision")), "git", "malformed"}}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "opaque legacy",
			corrupt: func(t *testing.T, db *DB, result journal.CommittedResult, _ journal.ActorID) {
				scope := takePoolScope(t, db)
				defer scope.release()
				if err := sqlitex.Execute(scope.conn, "UPDATE journal_operations SET mutation_encoding_version=?1,canonical_mutation=?2 WHERE journal_id=?3", &sqlitex.ExecOptions{Args: []any{nil, nil, int64(result.AnchorJournalID)}}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "cross subtype",
			corrupt: func(t *testing.T, db *DB, result journal.CommittedResult, _ journal.ActorID) {
				decisionJID := factContextResultSlot(t, result, "decision")
				scope := takePoolScope(t, db)
				defer scope.release()
				if err := sqlitex.Execute(scope.conn, "INSERT INTO journal_evidence (journal_id,evidence_kind,task_id,content_digest,payload) VALUES (?1,?2,?3,?4,?5)", &sqlitex.ExecOptions{Args: []any{int64(decisionJID), "fixture.context.cross", nil, []byte("cross"), "{}"}}); err != nil {
					t.Fatal(err)
				}
				if err := sqlitex.Execute(scope.conn, "INSERT INTO journal_evidence_contexts (evidence_journal_id,context_kind,context_identity) VALUES (?1,?2,?3)", &sqlitex.ExecOptions{Args: []any{int64(decisionJID), "git", "0123456789012345678901234567890123456789"}}); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newJournalDB(t)
			actor, task, boot := newFactContextEnvironment(t, db)
			ctx, err := journal.TaskContext(task)
			if err != nil {
				t.Fatal(err)
			}
			result, err := db.Apply(journal.OperationInput{
				OperationID: journal.OperationID("fact-context-integrity-" + test.name), ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("fact-context-integrity-" + test.name),
				Effects: []journal.Effect{{Sort: journal.EffectDecision, ResultSlot: "decision", TaskID: task, DecisionKind: "fixture.context.integrity", Payload: []byte(`{}`), Contexts: []journal.EventContext{ctx}}},
			})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			test.corrupt(t, db, result, actor)
			if err := db.VerifyIntegrity(); !errors.Is(err, journal.ErrFactContextIntegrity) {
				t.Fatalf("VerifyIntegrity error = %v, want ErrFactContextIntegrity", err)
			}
			if _, err := db.ReplayProjections(); !errors.Is(err, journal.ErrFactContextIntegrity) {
				t.Fatalf("ReplayProjections error = %v, want ErrFactContextIntegrity", err)
			}
		})
	}
}

func openFactContextFixture(t *testing.T, path string) (*DB, journal.ActorID, journal.TaskID, journal.JournalID) {
	t.Helper()
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open fixture: %v", err)
	}
	actor, task, boot := newFactContextEnvironment(t, db)
	return db, actor, task, boot
}

func newFactContextEnvironment(t *testing.T, db *DB) (journal.ActorID, journal.TaskID, journal.JournalID) {
	t.Helper()
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	scope := takePoolScope(t, db)
	err := sqlitex.Execute(scope.conn, "INSERT INTO agents (id,kind_id) VALUES (?1,?2)", &sqlitex.ExecOptions{Args: []any{actor.String(), int(ptypes.AgentKindSoftware)}})
	if err == nil {
		err = sqlitex.Execute(scope.conn, "INSERT INTO agents_software (agent_id,name,version,source) VALUES (?1,?2,?3,?4)", &sqlitex.ExecOptions{Args: []any{actor.String(), "fact-context", "0", "test"}})
	}
	scope.release()
	if err != nil {
		t.Fatalf("seed fact-context actor: %v", err)
	}
	boot := genesisBoot(t, db, actor)
	task := ptypes.TaskID{Namespace: "provenance-test", UUID: uuid.New()}
	if _, err := db.Apply(journal.OperationInput{
		OperationID: "fact-context-task-" + journal.OperationID(task.UUID.String()), ActorID: actor, AuthorityJournalID: &boot, CommandDigest: []byte("fact-context-task"),
		Effects: []journal.Effect{{Sort: journal.EffectTaskCreate, TaskID: task, Title: "fact context", Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped}},
	}); err != nil {
		t.Fatalf("create fact-context task: %v", err)
	}
	return actor, task, boot
}

func dropFactContextRelations(t *testing.T, db *DB) {
	t.Helper()
	scope := takePoolScope(t, db)
	defer scope.release()
	for _, statement := range []string{"DROP TABLE journal_decision_contexts", "DROP TABLE journal_evidence_contexts"} {
		if err := sqlitex.ExecuteTransient(scope.conn, statement, nil); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func factContextResultSlot(t *testing.T, result journal.CommittedResult, slot journal.ResultSlotID) journal.JournalID {
	t.Helper()
	for _, binding := range result.ResultSlots {
		if binding.Slot == slot {
			return binding.ProducedJournalID
		}
	}
	t.Fatalf("missing result slot %q in %+v", slot, result)
	return 0
}

func assertStoredFactContexts(t *testing.T, db *DB, relation factContextRelation, journalID journal.JournalID, want []journal.EventContext) {
	t.Helper()
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bind context read scope: %v", err)
	}
	defer scope.release()
	got, err := scope.loadVerifiedFactContexts(relation, int64(journalID))
	if err != nil {
		t.Fatalf("load verified contexts: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("contexts for %s journal %d = %#v, want %#v", relation.tableName(), journalID, got, want)
	}
}

func assertOpenFactContextFailurePreservesBytes(t *testing.T, path string) {
	t.Helper()
	before := factContextFileBytes(t, path)
	_, err := Open(path, nil)
	if !errors.Is(err, journal.ErrFactContextIntegrity) {
		t.Fatalf("Open error = %v, want ErrFactContextIntegrity", err)
	}
	if after := factContextFileBytes(t, path); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed Open changed database sidecars\nbefore: %#v\nafter: %#v", before, after)
	}
}

func factContextFileBytes(t *testing.T, path string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(path + suffix)
		if err == nil {
			files[suffix] = data
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read %s: %v", path+suffix, err)
		}
	}
	return files
}
