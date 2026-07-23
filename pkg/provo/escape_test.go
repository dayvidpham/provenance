package provo

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// These are white-box tests (package provo) for the low-level IRI and literal
// escaping helpers, covering the edge cases the paper's arbitrary-text fields
// (titles, descriptions, notes) can contain.

func TestEscapeString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"double-quote", `say "hi"`, `say \"hi\"`},
		{"backslash", `a\b`, `a\\b`},
		{"newline", "line1\nline2", `line1\nline2`},
		{"carriage-return", "a\rb", `a\rb`},
		{"tab", "a\tb", `a\tb`},
		{"unicode-passthrough", "snowman ☃ and é", "snowman ☃ and é"},
		{"backslash-then-quote", "\\\"", `\\\"`},
		{"c0-control", "a\x00b\x1fc", `a\u0000b\u001Fc`},
		{"emoji", "🚀", "🚀"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := escapeString(c.in); got != c.want {
				t.Errorf("escapeString(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestLiteralWraps(t *testing.T) {
	got := literal(`he said "x"` + "\n")
	want := `"he said \"x\"\n"`
	if got != want {
		t.Errorf("literal() = %q, want %q", got, want)
	}
}

func TestLiteralRoundTripsQuotesAndBackslash(t *testing.T) {
	// A literal must start and end with an unescaped double-quote and contain no
	// bare (unescaped) interior double-quote.
	got := literal(`a"b\c`)
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Fatalf("literal not quote-delimited: %q", got)
	}
	interior := got[1 : len(got)-1]
	// Every double-quote in the interior must be preceded by a backslash.
	for i := 0; i < len(interior); i++ {
		if interior[i] == '"' && (i == 0 || interior[i-1] != '\\') {
			t.Fatalf("unescaped interior quote at %d in %q", i, got)
		}
	}
}

func TestDateTime(t *testing.T) {
	ts := time.Date(2026, 7, 23, 15, 4, 5, 0, time.UTC)
	got := dateTime(ts)
	want := `"2026-07-23T15:04:05Z"^^xsd:dateTime`
	if got != want {
		t.Errorf("dateTime = %q, want %q", got, want)
	}
	// A non-UTC input must be normalized to UTC (Z).
	loc := time.FixedZone("PST", -8*3600)
	got2 := dateTime(time.Date(2026, 7, 23, 7, 4, 5, 0, loc))
	if !strings.HasSuffix(got2, `Z"^^xsd:dateTime`) {
		t.Errorf("dateTime did not normalize to UTC: %q", got2)
	}
	if !strings.Contains(got2, "15:04:05") {
		t.Errorf("dateTime did not convert PST->UTC: %q", got2)
	}
}

func TestIRIEscaping(t *testing.T) {
	e := &encoder{opts: Options{BaseIRI: "urn:provenance:"}}
	cases := []struct {
		name string
		id   string
	}{
		{"simple", "provo--0192abcd-1234-7890-abcd-ef0123456789"},
		{"space-in-namespace", "my project--0192abcd-1234-7890-abcd-ef0123456789"},
		{"url-namespace", "https://github.com/x/y--0192abcd-1234-7890-abcd-ef0123456789"},
		{"unicode-namespace", "проект--0192abcd-1234-7890-abcd-ef0123456789"},
		{"angle-and-quote", `a<b>"c--0192abcd-1234-7890-abcd-ef0123456789`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.iri(c.id)
			if !strings.HasPrefix(got, "<") || !strings.HasSuffix(got, ">") {
				t.Fatalf("iri not angle-bracketed: %q", got)
			}
			body := got[1 : len(got)-1]
			// No character illegal in an IRI reference may survive: PathEscape must
			// have removed spaces, angle brackets, quotes, and control chars.
			for _, bad := range []string{" ", "<", ">", `"`, "{", "}", "|", "\\", "^", "`"} {
				if strings.Contains(strings.TrimPrefix(body, e.opts.BaseIRI), bad) {
					t.Errorf("iri body contains illegal %q: %q", bad, got)
				}
			}
			// Round-trip: unescaping the local part recovers the original id.
			local := strings.TrimPrefix(body, e.opts.BaseIRI)
			back, err := url.PathUnescape(local)
			if err != nil {
				t.Fatalf("PathUnescape(%q): %v", local, err)
			}
			if back != c.id {
				t.Errorf("round-trip mismatch: got %q, want %q", back, c.id)
			}
		})
	}
}

func TestModelIDBuiltFromTypedValues(t *testing.T) {
	e := &encoder{opts: Options{}}
	mla := ptypes.MLAgent{
		Role: ptypes.RoleWorker,
		Model: ptypes.MLModel{
			Provider: ptypes.ProviderAnthropic,
			Name:     ptypes.ModelID("claude-opus-4-6"),
		},
	}
	got := e.modelID(mla)
	if got != "anthropic/claude-opus-4-6" {
		t.Errorf("modelID = %q, want %q", got, "anthropic/claude-opus-4-6")
	}
}
