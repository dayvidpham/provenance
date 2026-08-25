package sqlite

import "testing"

// TestIsBusyResultCodeClassifiesEveryLockedExtension pins the retry boundary:
// every SQLite result code whose primary code is BUSY or LOCKED is retryable
// regardless of its sub-code, and nothing else is. *moderncsqlite.Error cannot be
// constructed outside its package (its fields are unexported and it has no
// constructor), so the classification is tested on the code itself.
func TestIsBusyResultCodeClassifiesEveryLockedExtension(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		code int
		want bool
	}{
		{name: "SQLITE_BUSY", code: 5, want: true},
		{name: "SQLITE_LOCKED", code: 6, want: true},
		{name: "SQLITE_BUSY_RECOVERY", code: 261, want: true},
		{name: "SQLITE_LOCKED_SHAREDCACHE", code: 262, want: true},
		{name: "SQLITE_BUSY_SNAPSHOT", code: 517, want: true},
		{name: "SQLITE_LOCKED_VTAB", code: 518, want: true},
		{name: "SQLITE_BUSY_TIMEOUT", code: 773, want: true},
		{name: "SQLITE_OK", code: 0, want: false},
		{name: "SQLITE_ERROR", code: 1, want: false},
		{name: "SQLITE_IOERR", code: 10, want: false},
		{name: "SQLITE_CONSTRAINT", code: 19, want: false},
		{name: "SQLITE_IOERR_READ", code: 266, want: false},
		{name: "SQLITE_CONSTRAINT_FOREIGNKEY", code: 787, want: false},
		{name: "SQLITE_CONSTRAINT_UNIQUE", code: 2067, want: false},
	} {
		if got := isBusyResultCode(testCase.code); got != testCase.want {
			t.Errorf("isBusyResultCode(%s = %d) = %t, want %t", testCase.name, testCase.code, got, testCase.want)
		}
	}
}

// TestIsBusyErrorIgnoresNonSQLiteErrors keeps the predicate total for the callers
// that hand it any error from the operation path.
func TestIsBusyErrorIgnoresNonSQLiteErrors(t *testing.T) {
	t.Parallel()
	if isBusyError(nil) {
		t.Error("isBusyError(nil) = true, want false")
	}
	if isBusyError(errUniqueSentinel) {
		t.Error("isBusyError(non-SQLite error) = true, want false")
	}
}
