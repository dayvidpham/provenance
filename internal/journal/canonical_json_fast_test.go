package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

func TestPayloadWireIsCanonicalAgreesWithCanonicalJSON(t *testing.T) {
	task, err := ptypes.ParseTaskID("fixture/018f0000-0000-7000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	recognized := []any{
		map[string]any{},
		[]any{},
		[]any{[]any{}},
		nil,
		true,
		false,
		0,
		1,
		-12,
		1.5,
		"plain",
		"line\n\t\"quote\\",
		"a<b>&c",
		"é",
		"\u2028\u2029",
		"\u0000\u0001\u000b",
		map[string]any{
			"bindings":    []any{map[string]any{"Kind": 1, "NativeName": "sessionID", "Value": "ses_x"}},
			"body_digest": "sha256:16356c905683a44b9556bc7b8a02e3993491ba422e3db576992d9e4a9289ee5c",
			"envelope": map[string]any{
				"HostVersion": "2.2.7",
				"Runtime":     map[string]any{"Contract": "opencode/2.0.20"},
				"content":     []any{51, 230, 6, 30, 207, 255},
			},
			"event": 108,
			"note":  "a < b & c > d\n",
		},
		map[string]any{"<": 1, "=": 2, "a": 3, "é": 4},
		map[string]any{"": 1},
	}
	for i, value := range recognized {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		canon, err := mutationV1Codec.canonicalJSON(raw)
		if err != nil {
			t.Fatalf("canonicalJSON %d (%s): %v", i, raw, err)
		}
		if !payloadWireIsCanonical(canon) {
			t.Fatalf("canonicalJSON output not recognized as canonical: %s (from %s)", canon, raw)
		}
		assertCanonicalPayloadWire(t, canon)
		assertCanonicalPayloadWire(t, raw)
	}

	emptyKey := []byte(`{"":1}`)
	if !payloadWireIsCanonical(emptyKey) {
		t.Fatalf("empty key %s did not take the fast path", emptyKey)
	}
	assertCanonicalPayloadWire(t, emptyKey)

	notFast := []struct {
		name string
		raw  []byte
	}{
		{"unsorted", []byte(`{"b":1,"a":2}`)},
		{"duplicate", []byte(`{"a":1,"a":2}`)},
		{"duplicate-empty-key", []byte(`{"":1,"":2}`)},
		{"space", []byte(`{"a": 1}`)},
		{"trailing-space", []byte(`{"a":1} `)},
		{"leading-space", []byte(` [1]`)},
		{"non-shortest-escape", []byte(`{"a":"\u0041"}`)},
		{"raw-lt", []byte(`{"a":"<"}`)},
		{"escaped-slash", []byte(`{"a":"\/"}`)},
		{"uppercase-hex", []byte(`{"a":"\u003C"}`)},
		{"spaced-array", []byte(`[1, 2]`)},
		{"unsorted-z", []byte(`{"z":1,"a":2}`)},
		{"empty-array", []byte(`[]`)},
		{"nested-empty-array", []byte(`[[]]`)},
		{"empty-array-value", []byte(`{"a":[]}`)},
		{"trailing-comma", []byte(`{"a":1,}`)},
		{"truncated", []byte(`{"a":`)},
		{"nil", nil},
		{"empty", []byte{}},
		{"short-control-backspace", []byte(`{"a":"\u0008"}`)},
		{"short-control-tab", []byte(`{"a":"\u0009"}`)},
		{"short-control-lf", []byte(`{"a":"\u000a"}`)},
		{"short-control-ff", []byte(`{"a":"\u000c"}`)},
		{"short-control-cr", []byte(`{"a":"\u000d"}`)},
		{"raw-line-separator", append(append([]byte(`{"a":"`), '\xe2', '\x80', '\xa8'), []byte(`"}`)...)},
		{"raw-paragraph-separator", append(append([]byte(`{"a":"`), '\xe2', '\x80', '\xa9'), []byte(`"}`)...)},
		{"spaced-empty-object", []byte(`{ }`)},
		{"newline-empty-object", []byte("{\n}")},
		{"leading-zero", []byte(`01`)},
		{"unsorted-key-at-length-limit", unsortedLongKey(canonicalJSONMaxKey)},
		{"unsorted-key-past-length-limit", unsortedLongKey(canonicalJSONMaxKey + 1)},
		{"unsorted-nested", nestedObject(canonicalJSONMaxDepth, []byte(`{"b":1,"a":2}`))},
		{"past-depth-limit", nestedObject(canonicalJSONMaxDepth+1, []byte(`1`))},
	}
	for _, test := range notFast {
		t.Run(test.name, func(t *testing.T) {
			if payloadWireIsCanonical(test.raw) {
				t.Fatalf("%s took the fast path: %q", test.name, test.raw)
			}
			assertCanonicalPayloadWire(t, test.raw)
		})
	}

	mustHit := []struct {
		name string
		raw  []byte
	}{
		{"negative-zero", []byte(`-0`)},
		{"exponent", []byte(`1e2`)},
		{"exponent-plus", []byte(`1E+2`)},
		{"key-at-length-limit", sortedLongKey(canonicalJSONMaxKey)},
		{"depth-limit", nestedObject(canonicalJSONMaxDepth, []byte(`1`))},
	}
	for _, test := range mustHit {
		t.Run(test.name, func(t *testing.T) {
			canon, err := mutationV1Codec.canonicalJSON(test.raw)
			if err != nil || !bytes.Equal(canon, test.raw) {
				t.Fatalf("%s is not canonicalJSON output: raw=%q canon=%q err=%v", test.name, test.raw, canon, err)
			}
			if !payloadWireIsCanonical(test.raw) {
				t.Fatalf("%s did not take the fast path: %s", test.name, test.raw)
			}
			assertCanonicalPayloadWire(t, test.raw)
		})
	}
	overLimit := sortedLongKey(canonicalJSONMaxKey + 1)
	if payloadWireIsCanonical(overLimit) {
		t.Fatalf("key longer than %d took the fast path", canonicalJSONMaxKey)
	}
	assertCanonicalPayloadWire(t, overLimit)

	if !payloadCollapsesToEmptyObject([]byte(`{}`)) {
		t.Fatal(`{} did not collapse`)
	}
	if !payloadCollapsesToEmptyObject([]byte(`{ }`)) {
		t.Fatal("`{ }` did not collapse through the canonicalJSON fallback")
	}
	if !payloadCollapsesToEmptyObject([]byte("{\n}")) {
		t.Fatal("`{\\n}` did not collapse through the canonicalJSON fallback")
	}
	if payloadCollapsesToEmptyObject([]byte(`{"a":1}`)) {
		t.Fatal(`non-empty canonical object collapsed`)
	}

	prepared, err := Canonicalize(OperationInput{Effects: []Effect{{
		Sort: EffectDecision, DecisionKind: "fixture.decision", TaskID: task,
		Payload: []byte(`{"b":1,"a":"x<y>&z\n"}`),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCanonicalMutation(prepared.CanonicalBytes())
	if err != nil {
		t.Fatalf("canonical payload decode: %v", err)
	}
	got := decoded.NormalizedEffects()[0].Payload
	want, err := mutationV1Codec.canonicalJSON([]byte(`{"b":1,"a":"x<y>&z\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded payload = %s; want %s", got, want)
	}

	assertDecodedPayload(t, prepared.CanonicalBytes(), []byte(`{}`), nil)
	assertDecodedPayload(t, prepared.CanonicalBytes(), []byte(`{"a":1}`), []byte(`{"a":1}`))
	assertDecodedPayload(t, prepared.CanonicalBytes(), emptyKey, emptyKey)
	if _, err := DecodeCanonicalMutation(replaceEffectPayload(t, prepared.CanonicalBytes(), overLimit)); err != nil {
		t.Fatalf("over-limit but canonical key rejected by fallback: %v", err)
	}

	for _, test := range notFast {
		if len(test.raw) == 0 {
			continue
		}
		t.Run("decode-"+test.name, func(t *testing.T) {
			wire := replaceEffectPayload(t, prepared.CanonicalBytes(), test.raw)
			_, err := DecodeCanonicalMutation(wire)
			canon, canonErr := mutationV1Codec.canonicalJSON(test.raw)
			if canonErr == nil && bytes.Equal(canon, test.raw) {
				if err != nil {
					t.Fatalf("canonical value past the scanner limit rejected by fallback: %v", err)
				}
				return
			}
			want := expectedPayloadRejection(test.raw)
			if err == nil || err.Error() != want || !errors.Is(err, ErrCanonicalMutation) {
				t.Fatalf("error = %q; want %q", err, want)
			}
		})
	}
}

func assertCanonicalPayloadWire(t *testing.T, raw []byte) {
	t.Helper()
	got := canonicalPayloadWire(raw)
	want, err := mutationV1Codec.canonicalJSON(raw)
	if !bytes.Equal(got, want) {
		t.Fatalf("canonicalPayloadWire(%q) = %q; canonicalJSON = %q (err=%v)", raw, got, want, err)
	}
}

func expectedPayloadRejection(payload []byte) string {
	_, err := mutationV1Codec.canonicalJSON(payload)
	if err != nil {
		return canonicalMutationError(effectWireName(0, "payload"), err.Error(), "supply one strict JSON value without duplicate fields").Error()
	}
	return canonicalMutationError("wire", "decoded semantics do not re-encode to the identical canonical bytes", "use sorted unique collections, canonical JSON/scalars, and the exact registered field representation").Error()
}

func assertDecodedPayload(t *testing.T, wire, payload, want []byte) {
	t.Helper()
	decoded, err := DecodeCanonicalMutation(replaceEffectPayload(t, wire, payload))
	if err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	got := decoded.NormalizedEffects()[0].Payload
	if !bytes.Equal(got, want) {
		t.Fatalf("payload %s decoded to %s; want %s", payload, got, want)
	}
}

func sortedLongKey(n int) []byte {
	return []byte(`{"` + strings.Repeat("a", n) + `":1}`)
}

func unsortedLongKey(n int) []byte {
	return []byte(`{"b":1,"` + strings.Repeat("a", n) + `":2}`)
}

func nestedObject(depth int, leaf []byte) []byte {
	out := append([]byte(nil), leaf...)
	for i := 0; i < depth; i++ {
		wrapped := make([]byte, 0, len(out)+6)
		wrapped = append(wrapped, `{"k":`...)
		wrapped = append(wrapped, out...)
		wrapped = append(wrapped, '}')
		out = wrapped
	}
	return out
}

func replaceEffectPayload(t *testing.T, wire, payload []byte) []byte {
	t.Helper()
	const marker = "effect.0.payload:"
	i := bytes.Index(wire, []byte(marker))
	if i < 0 {
		t.Fatal("effect.0.payload field missing")
	}
	rest := wire[i+len(marker):]
	colon := bytes.IndexByte(rest, ':')
	if colon < 0 {
		t.Fatal("payload length frame missing")
	}
	n, err := strconv.Atoi(string(rest[:colon]))
	if err != nil {
		t.Fatal(err)
	}
	valueStart := colon + 1
	if valueStart+n >= len(rest) || rest[valueStart+n] != '\n' {
		t.Fatalf("payload frame length %d does not end at a newline", n)
	}
	var out bytes.Buffer
	out.Write(wire[:i])
	out.WriteString(marker)
	out.WriteString(strconv.Itoa(len(payload)))
	out.WriteByte(':')
	out.Write(payload)
	out.WriteByte('\n')
	out.Write(rest[valueStart+n+1:])
	return out.Bytes()
}
