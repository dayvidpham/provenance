package ptypes_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dayvidpham/providence/pkg/ptypes"
)

// ---------------------------------------------------------------------------
// TaskID
// ---------------------------------------------------------------------------

func TestTaskIDString(t *testing.T) {
	id := ptypes.TaskID{
		Namespace: "aura-plugins",
		UUID:      uuid.MustParse("018f4b12-3456-7890-abcd-ef0123456789"),
	}
	want := "aura-plugins--018f4b12-3456-7890-abcd-ef0123456789"
	if got := id.String(); got != want {
		t.Errorf("TaskID.String() = %q, want %q", got, want)
	}
}

func TestParseTaskIDRoundTrip(t *testing.T) {
	original := ptypes.TaskID{
		Namespace: "aura-plugins",
		UUID:      uuid.MustParse("018f4b12-3456-7890-abcd-ef0123456789"),
	}
	parsed, err := ptypes.ParseTaskID(original.String())
	if err != nil {
		t.Fatalf("ParseTaskID unexpected error: %v", err)
	}
	if parsed != original {
		t.Errorf("ParseTaskID round-trip: got %+v, want %+v", parsed, original)
	}
}

func TestParseTaskIDNamespaceWithDashes(t *testing.T) {
	// Namespace that contains "--" — must split on the rightmost "--"
	raw := "my--fancy--ns--018f4b12-3456-7890-abcd-ef0123456789"
	id, err := ptypes.ParseTaskID(raw)
	if err != nil {
		t.Fatalf("ParseTaskID(%q) unexpected error: %v", raw, err)
	}
	if id.Namespace != "my--fancy--ns" {
		t.Errorf("ParseTaskID namespace: got %q, want %q", id.Namespace, "my--fancy--ns")
	}
	if id.String() != raw {
		t.Errorf("ParseTaskID round-trip: got %q, want %q", id.String(), raw)
	}
}

func TestParseTaskIDErrors(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"no separator", "nohyphenuuid"},
		{"empty namespace", "--018f4b12-3456-7890-abcd-ef0123456789"},
		{"bad uuid", "ns--not-a-uuid"},
		{"empty string", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ptypes.ParseTaskID(c.input)
			if err == nil {
				t.Errorf("ParseTaskID(%q) expected error, got nil", c.input)
			}
			if !errors.Is(err, ptypes.ErrInvalidID) {
				t.Errorf("ParseTaskID(%q) error should wrap ErrInvalidID, got: %v", c.input, err)
			}
		})
	}
}

func TestParseTaskIDErrorMessage(t *testing.T) {
	// Error messages should contain the input for actionability.
	_, err := ptypes.ParseTaskID("bad-input")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bad-input") {
		t.Errorf("error message should reference input %q, got: %v", "bad-input", err)
	}
}

// ---------------------------------------------------------------------------
// AgentID
// ---------------------------------------------------------------------------

func TestAgentIDString(t *testing.T) {
	id := ptypes.AgentID{
		Namespace: "my-project",
		UUID:      uuid.MustParse("018f4b12-3456-7890-abcd-ef0123456789"),
	}
	want := "my-project--018f4b12-3456-7890-abcd-ef0123456789"
	if got := id.String(); got != want {
		t.Errorf("AgentID.String() = %q, want %q", got, want)
	}
}

func TestParseAgentIDRoundTrip(t *testing.T) {
	original := ptypes.AgentID{
		Namespace: "my-project",
		UUID:      uuid.MustParse("018f4b12-3456-7890-abcd-ef0123456789"),
	}
	parsed, err := ptypes.ParseAgentID(original.String())
	if err != nil {
		t.Fatalf("ParseAgentID unexpected error: %v", err)
	}
	if parsed != original {
		t.Errorf("ParseAgentID round-trip: got %+v, want %+v", parsed, original)
	}
}

func TestParseAgentIDErrors(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"no separator", "nohyphen"},
		{"empty namespace", "--018f4b12-3456-7890-abcd-ef0123456789"},
		{"bad uuid", "ns--not-a-uuid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ptypes.ParseAgentID(c.input)
			if err == nil {
				t.Errorf("ParseAgentID(%q) expected error, got nil", c.input)
			}
			if !errors.Is(err, ptypes.ErrInvalidID) {
				t.Errorf("ParseAgentID(%q) error should wrap ErrInvalidID, got: %v", c.input, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ActivityID
// ---------------------------------------------------------------------------

func TestActivityIDString(t *testing.T) {
	id := ptypes.ActivityID{
		Namespace: "ns",
		UUID:      uuid.MustParse("018f4b12-3456-7890-abcd-ef0123456789"),
	}
	want := "ns--018f4b12-3456-7890-abcd-ef0123456789"
	if got := id.String(); got != want {
		t.Errorf("ActivityID.String() = %q, want %q", got, want)
	}
}

func TestParseActivityIDRoundTrip(t *testing.T) {
	original := ptypes.ActivityID{
		Namespace: "ns",
		UUID:      uuid.MustParse("018f4b12-3456-7890-abcd-ef0123456789"),
	}
	parsed, err := ptypes.ParseActivityID(original.String())
	if err != nil {
		t.Fatalf("ParseActivityID unexpected error: %v", err)
	}
	if parsed != original {
		t.Errorf("ParseActivityID round-trip: got %+v, want %+v", parsed, original)
	}
}

func TestParseActivityIDErrors(t *testing.T) {
	_, err := ptypes.ParseActivityID("no-double-dash")
	if err == nil {
		t.Error("expected error, got nil")
	}
	if !errors.Is(err, ptypes.ErrInvalidID) {
		t.Errorf("expected ErrInvalidID, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CommentID
// ---------------------------------------------------------------------------

func TestCommentIDString(t *testing.T) {
	id := ptypes.CommentID{
		Namespace: "project",
		UUID:      uuid.MustParse("018f4b12-3456-7890-abcd-ef0123456789"),
	}
	want := "project--018f4b12-3456-7890-abcd-ef0123456789"
	if got := id.String(); got != want {
		t.Errorf("CommentID.String() = %q, want %q", got, want)
	}
}

func TestParseCommentIDRoundTrip(t *testing.T) {
	original := ptypes.CommentID{
		Namespace: "project",
		UUID:      uuid.MustParse("018f4b12-3456-7890-abcd-ef0123456789"),
	}
	parsed, err := ptypes.ParseCommentID(original.String())
	if err != nil {
		t.Fatalf("ParseCommentID unexpected error: %v", err)
	}
	if parsed != original {
		t.Errorf("ParseCommentID round-trip: got %+v, want %+v", parsed, original)
	}
}

func TestParseCommentIDErrors(t *testing.T) {
	_, err := ptypes.ParseCommentID("")
	if err == nil {
		t.Error("expected error for empty string, got nil")
	}
	if !errors.Is(err, ptypes.ErrInvalidID) {
		t.Errorf("expected ErrInvalidID, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LastIndex semantics — all Parse functions use rightmost "--"
// ---------------------------------------------------------------------------

func TestAllParseIDsUseLastIndex(t *testing.T) {
	// "ns--with--dashes--<uuid>" — rightmost split gives correct namespace.
	u := uuid.MustParse("018f4b12-3456-7890-abcd-ef0123456789")

	taskID := ptypes.TaskID{Namespace: "ns--with--dashes", UUID: u}
	agentID := ptypes.AgentID{Namespace: "ns--with--dashes", UUID: u}
	actID := ptypes.ActivityID{Namespace: "ns--with--dashes", UUID: u}
	commentID := ptypes.CommentID{Namespace: "ns--with--dashes", UUID: u}

	cases := []struct {
		name  string
		raw   string
		parse func(string) (string, error)
	}{
		{"TaskID", taskID.String(), func(s string) (string, error) {
			id, err := ptypes.ParseTaskID(s)
			return id.Namespace, err
		}},
		{"AgentID", agentID.String(), func(s string) (string, error) {
			id, err := ptypes.ParseAgentID(s)
			return id.Namespace, err
		}},
		{"ActivityID", actID.String(), func(s string) (string, error) {
			id, err := ptypes.ParseActivityID(s)
			return id.Namespace, err
		}},
		{"CommentID", commentID.String(), func(s string) (string, error) {
			id, err := ptypes.ParseCommentID(s)
			return id.Namespace, err
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ns, err := c.parse(c.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ns != "ns--with--dashes" {
				t.Errorf("namespace = %q, want %q", ns, "ns--with--dashes")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Sentinel errors
// ---------------------------------------------------------------------------

func TestSentinelErrors(t *testing.T) {
	// Just verify they are distinct and non-nil.
	errs := []error{
		ptypes.ErrNotFound,
		ptypes.ErrCycleDetected,
		ptypes.ErrAlreadyClosed,
		ptypes.ErrInvalidID,
		ptypes.ErrAgentKindMismatch,
	}
	for i, e := range errs {
		if e == nil {
			t.Errorf("sentinel error at index %d is nil", i)
		}
		for j, other := range errs {
			if i != j && errors.Is(e, other) {
				t.Errorf("sentinel error %d and %d should be distinct, but errors.Is returned true", i, j)
			}
		}
	}
}
