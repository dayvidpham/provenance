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
		assertCanonicalAgreement(t, canon)
		assertCanonicalAgreement(t, raw)
	}

	notFast := [][]byte{
		[]byte(`{"b":1,"a":2}`),
		[]byte(`{"a":1,"a":2}`),
		[]byte(`{"a": 1}`),
		[]byte(`{"a":1} `),
		[]byte(` [1]`),
		[]byte(`{"a":"\u0041"}`),
		[]byte(`{"a":"<"}`),
		[]byte(`{"a":"\/"}`),
		[]byte(`{"a":"\u003C"}`),
		[]byte(`[1, 2]`),
		[]byte(`{"z":1,"a":2}`),
		[]byte(`[]`),
		[]byte(`[[]]`),
		[]byte(`{"a":[]}`),
		[]byte(`{"a":1,}`),
		[]byte(`{"a":`),
		nil,
		[]byte{},
	}
	for _, raw := range notFast {
		if payloadWireIsCanonical(raw) {
			t.Fatalf("non-canonical payload took the fast path: %q", raw)
		}
		assertCanonicalAgreement(t, raw)
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

	for name, bad := range map[string][]byte{
		"unsorted":    []byte(`{"b":1,"a":2}`),
		"duplicate":   []byte(`{"a":1,"a":2}`),
		"space":       []byte(`{"a": 1}`),
		"uppercase":   []byte(`{"a":"\u003C"}`),
		"raw-lt":      []byte(`{"a":"<"}`),
		"empty-array": []byte(`[]`),
	} {
		t.Run(name, func(t *testing.T) {
			wire := replaceEffectPayload(t, prepared.CanonicalBytes(), bad)
			_, err := DecodeCanonicalMutation(wire)
			if !errors.Is(err, ErrCanonicalMutation) {
				t.Fatalf("error = %v; want ErrCanonicalMutation", err)
			}
			if name == "duplicate" && !strings.Contains(err.Error(), "duplicate JSON field") {
				t.Fatalf("duplicate payload error = %v; want the canonicalJSON duplicate-field text", err)
			}
		})
	}
}

func assertCanonicalAgreement(t *testing.T, raw []byte) {
	t.Helper()
	got, err := mutationV1Codec.canonicalJSON(raw)
	fast := payloadWireIsCanonical(raw)
	if fast && (err != nil || !bytes.Equal(got, raw)) {
		t.Fatalf("fast path disagrees with canonicalJSON: raw=%q got=%q err=%v", raw, got, err)
	}
	if !fast && err == nil && bytes.Equal(got, raw) && len(raw) > 0 {
		// A miss is allowed only outside the scanner's proven subset. The
		// recognized corpus asserts the subset we rely on for the live store.
		return
	}
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
