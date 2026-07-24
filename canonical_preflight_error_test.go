package provenance

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const canonicalPreflightOperationID = "canonical-preflight-operation"

func buildCanonicalPreflightErrorFixture(t *testing.T, path string) Tracker {
	t.Helper()
	tr, err := OpenSQLite(path, WithModelRegistry(NewRegistry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	actor, err := tr.RegisterSoftwareAgent("canonical-preflight", "actor", "1", "test")
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := tr.Journal().Apply(OperationInput{
		OperationID:   "canonical-preflight-genesis",
		ActorID:       actor.ID,
		CommandDigest: []byte("genesis"),
		Effects:       []Effect{{Sort: EffectBootstrapAuthority, ResultSlot: "authority"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	boot, _ := slotJournalID(genesis, "authority")
	task := newCorpusTaskID()
	if _, err := tr.Journal().Apply(OperationInput{
		OperationID:        canonicalPreflightOperationID,
		ActorID:            actor.ID,
		AuthorityJournalID: &boot,
		CommandDigest:      []byte("create"),
		Effects: []Effect{{Sort: EffectTaskCreate, ResultSlot: "task", TaskID: task,
			Title: "preflight", Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped}},
	}); err != nil {
		t.Fatal(err)
	}
	return tr
}

func readCanonicalPreflightWire(t *testing.T, path string) []byte {
	t.Helper()
	var wire []byte
	withRawSQLiteTestConn(t, path, func(conn *sqlite.Conn) {
		if err := sqlitex.Execute(conn, `SELECT canonical_mutation FROM journal_operations WHERE operation_id=?1`, &sqlitex.ExecOptions{Args: []any{canonicalPreflightOperationID}, ResultFunc: func(stmt *sqlite.Stmt) error {
			wire = make([]byte, stmt.ColumnLen(0))
			stmt.ColumnBytes(0, wire)
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
	})
	return wire
}

func corruptCanonicalPreflightRow(t *testing.T, path string, statement string, args ...any) {
	t.Helper()
	corruptDDL(t, path, `DROP TRIGGER journal_operations_canonical_update`)
	corruptSQL(t, path, statement, args...)
}

func assertCanonicalStartupActionable(t *testing.T, err error, category string) {
	t.Helper()
	if err == nil {
		t.Fatal("corrupt canonical preflight fixture opened")
	}
	message := strings.ToLower(err.Error())
	for _, field := range []string{"what:", "why:", "where:", "when:", "impact:", "fix:"} {
		if !strings.Contains(message, field) {
			t.Fatalf("startup error lacks actionable field %q: %v", field, err)
		}
	}
	if !strings.Contains(message, strings.ToLower(category)) {
		t.Fatalf("startup error does not distinguish category %q: %v", category, err)
	}
}

func TestCanonicalColumnPreflightErrorsAreTypedActionableAndReadOnly(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		mutate       func(*testing.T, string)
		category     string
		detail       string
		sentinel     error
		canonicalErr bool
	}{
		"one-column-version-only": {
			mutate: func(t *testing.T, path string) {
				makeOperationsSchemaLegacy(t, path)
				corruptDDL(t, path, `ALTER TABLE journal_operations ADD COLUMN mutation_encoding_version TEXT`)
			}, category: "one-column canonical shape", sentinel: ErrProjectionDivergence,
		},
		"one-column-bytes-only": {
			mutate: func(t *testing.T, path string) {
				makeOperationsSchemaLegacy(t, path)
				corruptDDL(t, path, `ALTER TABLE journal_operations ADD COLUMN canonical_mutation BLOB`)
			}, category: "one-column canonical shape", sentinel: ErrProjectionDivergence,
		},
		"pair-version-null": {
			mutate: func(t *testing.T, path string) {
				corruptCanonicalPreflightRow(t, path, `UPDATE journal_operations SET mutation_encoding_version=NULL WHERE operation_id=?1`, canonicalPreflightOperationID)
			}, category: "malformed canonical pairing", detail: "version=NULL, bytes=nonempty", sentinel: ErrProjectionDivergence,
		},
		"pair-bytes-null": {
			mutate: func(t *testing.T, path string) {
				corruptCanonicalPreflightRow(t, path, `UPDATE journal_operations SET canonical_mutation=NULL WHERE operation_id=?1`, canonicalPreflightOperationID)
			}, category: "malformed canonical pairing", detail: "version=nonempty", sentinel: ErrProjectionDivergence,
		},
		"pair-version-empty": {
			mutate: func(t *testing.T, path string) {
				corruptCanonicalPreflightRow(t, path, `UPDATE journal_operations SET mutation_encoding_version='' WHERE operation_id=?1`, canonicalPreflightOperationID)
			}, category: "malformed canonical pairing", detail: "version=empty, bytes=nonempty", sentinel: ErrProjectionDivergence,
		},
		"pair-bytes-empty": {
			mutate: func(t *testing.T, path string) {
				corruptCanonicalPreflightRow(t, path, `UPDATE journal_operations SET canonical_mutation=X'' WHERE operation_id=?1`, canonicalPreflightOperationID)
			}, category: "malformed canonical pairing", detail: "bytes=empty", sentinel: ErrProjectionDivergence,
		},
		"pair-both-empty": {
			mutate: func(t *testing.T, path string) {
				corruptCanonicalPreflightRow(t, path, `UPDATE journal_operations SET mutation_encoding_version='',canonical_mutation=X'' WHERE operation_id=?1`, canonicalPreflightOperationID)
			}, category: "malformed canonical pairing", detail: "version=empty, bytes=empty", sentinel: ErrProjectionDivergence,
		},
		"oversized-wire": {
			mutate: func(t *testing.T, path string) {
				corruptCanonicalPreflightRow(t, path, `UPDATE journal_operations SET canonical_mutation=zeroblob(?1) WHERE operation_id=?2`, MaxCanonicalMutationBytes+1, canonicalPreflightOperationID)
			}, category: "oversized canonical mutation", sentinel: ErrCanonicalMutation, canonicalErr: true,
		},
		"malformed-wire": {
			mutate: func(t *testing.T, path string) {
				corruptCanonicalPreflightRow(t, path, `UPDATE journal_operations SET canonical_mutation=X'01' WHERE operation_id=?1`, canonicalPreflightOperationID)
			}, category: "malformed canonical wire-version frame", sentinel: ErrCanonicalMutation, canonicalErr: true,
		},
		"malformed-supported-wire": {
			mutate: func(t *testing.T, path string) {
				wire := append(readCanonicalPreflightWire(t, path), []byte("trailing-garbage")...)
				corruptCanonicalPreflightRow(t, path, `UPDATE journal_operations SET canonical_mutation=?1 WHERE operation_id=?2`, wire, canonicalPreflightOperationID)
			}, category: "malformed canonical wire for supported version", sentinel: ErrCanonicalMutation, canonicalErr: true,
		},
		"matching-unknown-codec": {
			mutate: func(t *testing.T, path string) {
				wire := readCanonicalPreflightWire(t, path)
				from, to := []byte("version:22:provenance.mutation.v1\n"), []byte("version:22:provenance.mutation.v9\n")
				if bytes.Count(wire, from) != 1 {
					t.Fatalf("wire version marker count=%d, want one", bytes.Count(wire, from))
				}
				corruptCanonicalPreflightRow(t, path, `UPDATE journal_operations SET mutation_encoding_version='provenance.mutation.v9',canonical_mutation=?1 WHERE operation_id=?2`, bytes.Replace(wire, from, to, 1), canonicalPreflightOperationID)
			}, category: "unsupported canonical codec version", sentinel: ErrCanonicalMutation, canonicalErr: true,
		},
		"column-wire-version-mismatch": {
			mutate: func(t *testing.T, path string) {
				corruptCanonicalPreflightRow(t, path, `UPDATE journal_operations SET mutation_encoding_version='provenance.mutation.v9' WHERE operation_id=?1`, canonicalPreflightOperationID)
			}, category: "differs from wire version", sentinel: ErrProjectionDivergence,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "canonical-preflight.sqlite")
			tr := buildCanonicalPreflightErrorFixture(t, path)
			test.mutate(t, path)
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			opened, openErr := OpenSQLite(path, WithModelRegistry(NewRegistry(nil)))
			if opened != nil {
				_ = opened.Close()
			}
			assertCanonicalStartupActionable(t, openErr, test.category)
			if test.detail != "" && !strings.Contains(openErr.Error(), test.detail) {
				t.Fatalf("startup error does not report exact pair state %q: %v", test.detail, openErr)
			}
			if !errors.Is(openErr, test.sentinel) {
				t.Fatalf("startup error=%v, want errors.Is(%v)", openErr, test.sentinel)
			}
			var canonical *CanonicalMutationError
			if errors.As(openErr, &canonical) != test.canonicalErr {
				t.Fatalf("CanonicalMutationError presence=%v, want %v: %v", canonical != nil, test.canonicalErr, openErr)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed canonical preflight changed database bytes")
			}
		})
	}
}
