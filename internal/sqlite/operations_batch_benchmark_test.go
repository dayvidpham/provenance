package sqlite

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
)

// operations_batch_benchmark_test.go measures the serial p50 latency of
// committing records through the per-record path (DB.Apply) and through the
// batched path (DB.ApplyBatchContext), on private file-backed WAL stores.
//
// FAIRNESS. Both arms commit the SAME number of records on their own freshly
// opened store, so neither arm is measured against a larger journal than the
// other: a growing store is the dominant confounder for this reducer because the
// post-fold integrity gate scans the journal, and comparing a 200-record store
// against a 4,000-record store measures the store size, not the batching.
//
// The benchmark reports, per arm, the p50 latency of one commit call and the
// derived p50 latency per RECORD, so the arms compare directly:
//
//   - per-record: one call commits one record; p50-ns/record == p50-ns/op.
//   - batched: one call commits `records/op` records in one transaction;
//     p50-ns/record is the per-batch p50 divided by that batch size.
//
// Run it as one controlled repetition per arm with -benchtime=1x. Correctness
// stays in the ordinary tests; these numbers are performance observations only.

const (
	batchBenchmarkTotalRecords = 2000
	batchBenchmarkBatchSize    = 20
)

func benchmarkBatchStore(b *testing.B) (*DB, journal.ActorID, journal.JournalID) {
	b.Helper()
	db, agentID := openBenchmarkPoolDB(b)
	actor := journal.ActorID(agentID)
	authority := bootstrapBenchmarkPoolDB(b, db, actor)
	return db, actor, authority
}

func benchmarkBatchInput(operationID string, actor journal.ActorID, authority journal.JournalID) journal.OperationInput {
	return journal.OperationInput{
		OperationID:        journal.OperationID(operationID),
		ActorID:            actor,
		AuthorityJournalID: &authority,
		CommandDigest:      []byte(operationID),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort: journal.EffectDecision, DecisionKind: batchDecisionKind, Payload: []byte(`{}`), ResultSlot: "decision",
		}},
	}
}

// reportP50PerRecord sorts the per-call latencies, reports the median call
// latency and the derived median latency per record.
func reportP50PerRecord(b *testing.B, latencies []time.Duration, recordsPerCall int) {
	b.Helper()
	if len(latencies) == 0 {
		b.Fatal("no latency samples")
	}
	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 := sorted[len(sorted)/2]
	b.ReportMetric(float64(p50.Nanoseconds()), "p50-ns/op")
	b.ReportMetric(float64(recordsPerCall), "records/op")
	b.ReportMetric(float64(p50.Nanoseconds())/float64(recordsPerCall), "p50-ns/record")
}

func BenchmarkBatchApplySerialLatency(b *testing.B) {
	b.Run("per-record", func(b *testing.B) {
		db, actor, authority := benchmarkBatchStore(b)
		latencies := make([]time.Duration, 0, batchBenchmarkTotalRecords)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < batchBenchmarkTotalRecords; i++ {
			start := time.Now()
			_, err := db.Apply(benchmarkBatchInput(fmt.Sprintf("bench-per-record-%d", i), actor, authority))
			latencies = append(latencies, time.Since(start))
			if err != nil {
				b.Fatalf("per-record Apply %d: %v", i, err)
			}
		}
		b.StopTimer()
		reportP50PerRecord(b, latencies, 1)
	})

	b.Run(fmt.Sprintf("batched/B=%d", batchBenchmarkBatchSize), func(b *testing.B) {
		db, actor, authority := benchmarkBatchStore(b)
		latencies := make([]time.Duration, 0, batchBenchmarkTotalRecords/batchBenchmarkBatchSize)
		b.ReportAllocs()
		b.ResetTimer()
		for committed := 0; committed < batchBenchmarkTotalRecords; committed += batchBenchmarkBatchSize {
			inputs := make([]journal.OperationInput, batchBenchmarkBatchSize)
			for j := range inputs {
				inputs[j] = benchmarkBatchInput(fmt.Sprintf("bench-batched-%d-%d", committed, j), actor, authority)
			}
			start := time.Now()
			result, err := db.ApplyBatch(inputs)
			latencies = append(latencies, time.Since(start))
			if err != nil {
				b.Fatalf("ApplyBatch at %d: %v", committed, err)
			}
			if !result.Committed {
				b.Fatalf("batch at %d did not commit", committed)
			}
		}
		b.StopTimer()
		reportP50PerRecord(b, latencies, batchBenchmarkBatchSize)
	})
}
