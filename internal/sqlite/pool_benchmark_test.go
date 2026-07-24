package sqlite

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const benchmarkDecisionKind journal.DecisionKind = "benchmark.pool.decision"

func openBenchmarkPoolDB(b *testing.B) (*DB, ptypes.AgentID) {
	b.Helper()
	db, err := Open(b.TempDir()+"/pool-benchmark.db", nil)
	if err != nil {
		b.Fatalf("open private file-backed benchmark DB: %v", err)
	}
	b.Cleanup(func() {
		if err := db.Close(); err != nil {
			b.Errorf("close private file-backed benchmark DB: %v", err)
		}
	})

	agent, err := db.RegisterSoftwareAgent("benchmark", "pool", "1", "pool_benchmark_test")
	if err != nil {
		b.Fatalf("register benchmark actor: %v", err)
	}

	scope, err := db.bindConn(b.Context())
	if err != nil {
		b.Fatalf("lease connection to verify benchmark WAL mode: %v", err)
	}
	defer scope.release()
	journalMode := ""
	if err := sqlitex.ExecuteTransient(scope.conn, "PRAGMA journal_mode", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *zs.Stmt) error {
			journalMode = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		b.Fatalf("read benchmark journal mode: %v", err)
	}
	if journalMode != "wal" {
		b.Fatalf("private file-backed benchmark journal mode = %q, want wal", journalMode)
	}
	return db, agent.ID
}

func bootstrapBenchmarkPoolDB(b *testing.B, db *DB, actor journal.ActorID) journal.JournalID {
	b.Helper()
	result, err := db.Apply(journal.OperationInput{
		OperationID:   "benchmark-genesis",
		ActorID:       actor,
		CommandDigest: []byte("benchmark-genesis"),
		Effects: []journal.Effect{{
			Sort: journal.EffectBootstrapAuthority, BootstrapLabel: "benchmark", ResultSlot: "authority",
		}},
	})
	if err != nil {
		b.Fatalf("apply benchmark genesis: %v", err)
	}
	for _, slot := range result.ResultSlots {
		if slot.Slot == "authority" {
			return slot.ProducedJournalID
		}
	}
	b.Fatal("apply benchmark genesis returned no authority result slot")
	return 0
}

func BenchmarkPoolReadOnly(b *testing.B) {
	for _, goroutines := range []int{1, 2, 4, 8} {
		b.Run("goroutines="+strconv.Itoa(goroutines), func(b *testing.B) {
			db, agentID := openBenchmarkPoolDB(b)
			if _, err := db.GetSoftwareAgent(agentID); err != nil {
				b.Fatalf("warm benchmark software-agent read: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			start := make(chan struct{})
			errorsOut := make(chan error, goroutines)
			var workers sync.WaitGroup
			workers.Add(goroutines)
			for worker := range goroutines {
				go func() {
					defer workers.Done()
					<-start
					for operation := worker; operation < b.N; operation += goroutines {
						if _, err := db.GetSoftwareAgent(agentID); err != nil {
							errorsOut <- err
							return
						}
					}
				}()
			}
			close(start)
			workers.Wait()
			close(errorsOut)
			b.StopTimer()
			for err := range errorsOut {
				b.Fatalf("pooled read-only benchmark call: %v", err)
			}
			b.ReportMetric(1, "reads/op")
		})
	}
}

func BenchmarkPoolApplyWriterWithReaders(b *testing.B) {
	const readers = 2
	db, agentID := openBenchmarkPoolDB(b)
	actor := journal.ActorID(agentID)
	authority := bootstrapBenchmarkPoolDB(b, db, actor)

	b.ReportAllocs()
	b.ResetTimer()
	start := make(chan struct{})
	errorsOut := make(chan error, readers+1)
	var workers sync.WaitGroup
	workers.Add(readers + 1)
	go func() {
		defer workers.Done()
		<-start
		for operation := range b.N {
			opID := journal.OperationID("benchmark-mixed-write-" + strconv.Itoa(operation))
			if _, err := db.Apply(journal.OperationInput{
				OperationID: opID, ActorID: actor, AuthorityJournalID: &authority,
				CommandDigest: []byte(opID),
				Effects: []journal.Effect{{
					Sort: journal.EffectDecision, DecisionKind: benchmarkDecisionKind, Payload: []byte(`{}`),
				}},
			}); err != nil {
				errorsOut <- fmt.Errorf("Apply operation %q: %w", opID, err)
				return
			}
		}
	}()
	for range readers {
		go func() {
			defer workers.Done()
			<-start
			for range b.N {
				if _, err := db.GetSoftwareAgent(agentID); err != nil {
					errorsOut <- fmt.Errorf("GetSoftwareAgent: %w", err)
					return
				}
			}
		}()
	}
	close(start)
	workers.Wait()
	close(errorsOut)
	b.StopTimer()
	for err := range errorsOut {
		b.Fatalf("mixed pooled benchmark call: %v", err)
	}
	b.ReportMetric(1, "applies/op")
	b.ReportMetric(readers, "reads/op")
}

func BenchmarkPoolContendingApplyCurrentFact(b *testing.B) {
	for _, writers := range []int{2, 4} {
		b.Run("writers="+strconv.Itoa(writers), func(b *testing.B) {
			db, agentID := openBenchmarkPoolDB(b)
			actor := journal.ActorID(agentID)
			authority := bootstrapBenchmarkPoolDB(b, db, actor)
			base := applyBenchmarkDecision(b, db, actor, authority, "benchmark-contention-base", nil)

			var wins, conditionFailures int
			b.ReportAllocs()
			b.ResetTimer()
			for operation := range b.N {
				start := make(chan struct{})
				results := make(chan benchmarkApplyResult, writers)
				for writer := range writers {
					go func() {
						<-start
						opID := journal.OperationID(fmt.Sprintf("benchmark-contention-%d-%d", operation, writer))
						result, err := db.Apply(journal.OperationInput{
							OperationID: opID, ActorID: actor, AuthorityJournalID: &authority,
							CommandDigest: []byte(opID),
							Conditions: []journal.Condition{{
								Kind: journal.ConditionCurrentFact,
								Selector: journal.FactSelector{
									Kind: journal.FactDecision, DecisionKind: benchmarkDecisionKind,
									Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
								},
								AssertedJournalID: base,
							}},
							Effects: []journal.Effect{{
								Sort: journal.EffectDecision, DecisionKind: benchmarkDecisionKind,
								Payload: []byte(`{}`), ResultSlot: "decision",
							}},
						})
						results <- benchmarkApplyResult{result: result, err: err}
					}()
				}
				close(start)

				nextBase := journal.JournalID(0)
				for range writers {
					outcome := <-results
					switch {
					case outcome.err == nil:
						wins++
						for _, slot := range outcome.result.ResultSlots {
							if slot.Slot == "decision" {
								nextBase = slot.ProducedJournalID
							}
						}
					case errors.Is(outcome.err, journal.ErrConditionFailed):
						conditionFailures++
					default:
						b.Fatalf("contending Apply benchmark call: %v", outcome.err)
					}
				}
				if nextBase == 0 {
					b.Fatal("contending Apply benchmark produced no next CurrentFact base")
				}
				base = nextBase
			}
			b.StopTimer()
			b.ReportMetric(float64(writers), "writers/op")
			b.ReportMetric(float64(wins)/float64(b.N), "wins/op")
			b.ReportMetric(float64(conditionFailures)/float64(b.N), "condition-failures/op")
		})
	}
}

type benchmarkApplyResult struct {
	result journal.CommittedResult
	err    error
}

func applyBenchmarkDecision(b *testing.B, db *DB, actor journal.ActorID, authority journal.JournalID, operationID journal.OperationID, conditions []journal.Condition) journal.JournalID {
	b.Helper()
	result, err := db.Apply(journal.OperationInput{
		OperationID: operationID, ActorID: actor, AuthorityJournalID: &authority,
		CommandDigest: []byte(operationID), Conditions: conditions,
		Effects: []journal.Effect{{
			Sort: journal.EffectDecision, DecisionKind: benchmarkDecisionKind,
			Payload: []byte(`{}`), ResultSlot: "decision",
		}},
	})
	if err != nil {
		b.Fatalf("apply benchmark decision %q: %v", operationID, err)
	}
	for _, slot := range result.ResultSlots {
		if slot.Slot == "decision" {
			return slot.ProducedJournalID
		}
	}
	b.Fatalf("apply benchmark decision %q returned no decision result slot", operationID)
	return 0
}
