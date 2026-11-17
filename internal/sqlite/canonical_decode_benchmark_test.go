package sqlite

import (
	"database/sql"
	"os"
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
var canonicalDecodeSamples [][]byte

func BenchmarkCanonicalDecode(b *testing.B) {
	samples := loadCanonicalDecodeSample(b)
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

func loadCanonicalDecodeSample(b *testing.B) [][]byte {
	b.Helper()
	if canonicalDecodeSamples != nil {
		return canonicalDecodeSamples
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
	const want = 4096
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
	canonicalDecodeSamples = samples
	return samples
}
