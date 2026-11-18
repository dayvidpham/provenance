package sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
)

// BenchmarkCanonicalDecode measures the full-path per-operation decode:
// DecodeCanonicalMutation, DerivedDigest, and NormalizedEffects. It reads a
// caller-supplied store read-only (PROVENANCE_BENCH_DB) and does not copy it.
// The sample is a spread of canonical mutations, not the first page, because
// early journal rows are smaller than the 2–4 KB mass of the live store.
//
// Correctness stays in the ordinary journal tests. These numbers are
// performance observations only.
var canonicalDecodeSamples = map[int][][]byte{}

func BenchmarkCanonicalDecode(b *testing.B) {
	samples := loadCanonicalDecodeSample(b, canonicalDecodeSampleSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wire := samples[i%len(samples)]
		mutation, err := journal.DecodeCanonicalMutation(wire)
		if err != nil {
			b.Fatalf("decode sample %d: %v", i%len(samples), err)
		}
		if digest := mutation.DerivedDigest(); len(digest) != 32 {
			b.Fatalf("sample %d digest length = %d; want 32", i%len(samples), len(digest))
		}
		if effects := mutation.NormalizedEffects(); effects == nil && len(wire) > 0 {
			b.Fatalf("sample %d returned a nil effect slice", i%len(samples))
		}
	}
}

// canonicalDecodeSampleSize is the default spread; set PROVENANCE_BENCH_SAMPLE
// to a larger count (for example 100000) for the parallel benchmark.
const canonicalDecodeSampleSize = 4096

func loadCanonicalDecodeSample(b *testing.B, want int) [][]byte {
	b.Helper()
	if cached, ok := canonicalDecodeSamples[want]; ok {
		return cached
	}
	path := os.Getenv("PROVENANCE_BENCH_DB")
	if path == "" {
		b.Skip("PROVENANCE_BENCH_DB is unset; set it to a store whose journal_operations.canonical_mutation rows should be sampled read-only")
	}
	db, err := sql.Open(sqliteDriverName, "file:"+path+"?mode=ro")
	if err != nil {
		b.Fatalf("open %q read-only: %v", path, err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			b.Errorf("close read-only sample connection: %v", err)
		}
	}()
	if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
		b.Fatalf("set query_only on %q: %v", path, err)
	}
	var lo, hi int64
	if err := db.QueryRow(`SELECT MIN(journal_id), MAX(journal_id) FROM journal_operations WHERE canonical_mutation IS NOT NULL`).Scan(&lo, &hi); err != nil {
		b.Fatalf("read canonical journal bounds from %q: %v", path, err)
	}
	if hi < lo {
		b.Fatalf("store %q has no canonical mutations", path)
	}
	samples := make([][]byte, 0, want)
	stmt, err := db.Prepare(`SELECT canonical_mutation FROM journal_operations WHERE canonical_mutation IS NOT NULL AND journal_id >= ?1 ORDER BY journal_id LIMIT 1`)
	if err != nil {
		b.Fatalf("prepare canonical sample query: %v", err)
	}
	defer func() {
		if err := stmt.Close(); err != nil {
			b.Errorf("close canonical sample statement: %v", err)
		}
	}()
	span := hi - lo
	for i := 0; i < want; i++ {
		var target int64
		if want == 1 || span == 0 {
			target = lo
		} else {
			target = lo + span*int64(i)/int64(want-1)
		}
		var wire []byte
		if err := stmt.QueryRow(target).Scan(&wire); err != nil {
			b.Fatalf("sample canonical mutation at journal_id >= %d (%d/%d): %v", target, i, want, err)
		}
		if len(wire) == 0 {
			b.Fatalf("sample %d at journal_id >= %d is empty", i, target)
		}
		samples = append(samples, append([]byte(nil), wire...))
	}
	b.Logf("sampled %d canonical mutations from %q (journal_id %d..%d, first %d bytes, last %d bytes)", len(samples), path, lo, hi, len(samples[0]), len(samples[len(samples)-1]))
	canonicalDecodeSamples[want] = samples
	return samples
}

// BenchmarkCanonicalDecodeParallel measures the startup walk's decode phase
// (decode, digest, effect normalization) over the sampled spread on the same
// worker pool the validators use, at 1, 2, 4, 8, and 16 workers. One iteration
// is one walk over the whole sample, so ns/op is the wall time of the decode
// loop; ns/operation is reported alongside. It reads the store read-only and
// does not copy it.
func BenchmarkCanonicalDecodeParallel(b *testing.B) {
	size := canonicalDecodeSampleSize
	if raw := os.Getenv("PROVENANCE_BENCH_SAMPLE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			b.Fatalf("PROVENANCE_BENCH_SAMPLE=%q; want a positive integer", raw)
		}
		size = n
	}
	samples := loadCanonicalDecodeSample(b, size)
	for _, workers := range []int{1, 2, 4, 8, 16} {
		b.Run(fmt.Sprintf("workers-%d", workers), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				walked := 0
				err := forEachOrdered(len(samples), workers, func(i int) decodedCanonicalOperation {
					mutation, err := journal.DecodeCanonicalMutation(samples[i])
					if err != nil {
						return decodedCanonicalOperation{err: err}
					}
					return decodedCanonicalOperation{prepared: mutation, digest: mutation.DerivedDigest(), effects: mutation.NormalizedEffects()}
				}, func(i int, decoded decodedCanonicalOperation) error {
					if decoded.err != nil {
						return fmt.Errorf("decode sample %d: %w", i, decoded.err)
					}
					if len(decoded.digest) != 32 || decoded.effects == nil {
						return fmt.Errorf("sample %d produced digest length %d, effects nil=%t", i, len(decoded.digest), decoded.effects == nil)
					}
					walked++
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				if walked != len(samples) {
					b.Fatalf("walked %d of %d samples", walked, len(samples))
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(samples)), "ns/operation")
		})
	}
}
