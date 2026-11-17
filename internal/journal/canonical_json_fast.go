package journal

import (
	"bytes"
	"unicode/utf8"
)

// payloadWireIsCanonical reports whether raw is already the exact byte
// sequence canonicalJSON would return. False is not a rejection: callers fall
// back to canonicalJSON, so a miss only costs the old path. True must be sound.
// A true result that canonicalJSON would not reproduce would accept a
// non-canonical payload during verification.
//
// The scanner is deliberately narrower than encoding/json. Strings with an
// escape it does not prove canonical, keys longer than canonicalJSONMaxKey, and
// values nested deeper than canonicalJSONMaxDepth return false.
func payloadWireIsCanonical(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	n, ok := scanCanonicalJSONValue(raw, canonicalJSONMaxDepth)
	return ok && n == len(raw)
}

// canonicalPayloadWire is the payload field encodeEffect writes. Canonical
// bytes are copied through; every other input, including invalid JSON, uses
// canonicalJSON and keeps its ignored-error behavior.
func canonicalPayloadWire(raw []byte) []byte {
	if payloadWireIsCanonical(raw) {
		return raw
	}
	encoded, _ := mutationV1Codec.canonicalJSON(raw)
	return encoded
}

// payloadCollapsesToEmptyObject is the normalizeEffect empty-object rule.
// Exact `{}` collapses without a parse. Any other proven-canonical payload
// does not. Inputs the scanner will not prove fall back to canonicalJSON so
// spaced empty objects still collapse and invalid JSON still does not.
func payloadCollapsesToEmptyObject(raw []byte) bool {
	if len(raw) == 2 && raw[0] == '{' && raw[1] == '}' {
		return true
	}
	if payloadWireIsCanonical(raw) {
		return false
	}
	payload, err := mutationV1Codec.canonicalJSON(raw)
	return err == nil && bytes.Equal(payload, []byte("{}"))
}

const (
	canonicalJSONMaxDepth = 64
	canonicalJSONMaxKey   = 256
)

var (
	canonEscapeBackspace  = [1]byte{'\b'}
	canonEscapeFormFeed   = [1]byte{'\f'}
	canonEscapeNewline    = [1]byte{'\n'}
	canonEscapeCarriage   = [1]byte{'\r'}
	canonEscapeTab        = [1]byte{'\t'}
	canonEscapeLT         = []byte(`\u003c`)
	canonEscapeGT         = []byte(`\u003e`)
	canonEscapeAmp        = []byte(`\u0026`)
	canonEscapeLS         = []byte(`\u2028`)
	canonEscapePS         = []byte(`\u2029`)
	canonEscapeLTDecoded  = [1]byte{'<'}
	canonEscapeGTDecoded  = [1]byte{'>'}
	canonEscapeAmpDecoded = [1]byte{'&'}
	canonEscapeLSDecoded  = [3]byte{0xe2, 0x80, 0xa8}
	canonEscapePSDecoded  = [3]byte{0xe2, 0x80, 0xa9}
	canonControls         = func() [32]byte {
		var out [32]byte
		for i := range out {
			out[i] = byte(i)
		}
		return out
	}()
)

func scanCanonicalJSONValue(s []byte, depth int) (int, bool) {
	if depth <= 0 || len(s) == 0 {
		return 0, false
	}
	switch s[0] {
	case '{':
		return scanCanonicalJSONObject(s, depth)
	case '[':
		return scanCanonicalJSONArray(s, depth)
	case '"':
		n, _, ok := scanCanonicalJSONString(s, nil)
		return n, ok
	case 't':
		if len(s) >= 4 && s[1] == 'r' && s[2] == 'u' && s[3] == 'e' {
			return 4, true
		}
		return 0, false
	case 'f':
		if len(s) >= 5 && s[1] == 'a' && s[2] == 'l' && s[3] == 's' && s[4] == 'e' {
			return 5, true
		}
		return 0, false
	case 'n':
		if len(s) >= 4 && s[1] == 'u' && s[2] == 'l' && s[3] == 'l' {
			return 4, true
		}
		return 0, false
	default:
		return scanCanonicalJSONNumber(s)
	}
}

func scanCanonicalJSONObject(s []byte, depth int) (int, bool) {
	if len(s) < 2 || s[0] != '{' {
		return 0, false
	}
	if s[1] == '}' {
		return 2, true
	}
	var prev [canonicalJSONMaxKey]byte
	prevLen := 0
	i := 1
	for {
		if i >= len(s) || s[i] != '"' {
			return 0, false
		}
		// Zero length, remaining cap: append must write into prev, not allocate.
		n, key, ok := scanCanonicalJSONString(s[i:], prev[prevLen:prevLen])
		if !ok {
			return 0, false
		}
		// scanCanonicalJSONString appends into the spare tail. Move the decoded
		// key to the front of prev only after the order check, which still needs
		// the previous key in prev[:prevLen].
		decoded := key
		if prevLen > 0 && bytes.Compare(decoded, prev[:prevLen]) <= 0 {
			return 0, false
		}
		if copy(prev[:], decoded) != len(decoded) {
			return 0, false
		}
		prevLen = len(decoded)
		i += n
		if i >= len(s) || s[i] != ':' {
			return 0, false
		}
		i++
		vn, ok := scanCanonicalJSONValue(s[i:], depth-1)
		if !ok {
			return 0, false
		}
		i += vn
		if i >= len(s) {
			return 0, false
		}
		switch s[i] {
		case '}':
			return i + 1, true
		case ',':
			i++
		default:
			return 0, false
		}
	}
}

func scanCanonicalJSONArray(s []byte, depth int) (int, bool) {
	if len(s) < 2 || s[0] != '[' {
		return 0, false
	}
	// decodeUniqueJSON builds a nil slice for an empty array, and json.Marshal
	// encodes that nil slice as null. `[]` is therefore not this codec's
	// canonical form; accepting it would let verification skip the mismatch.
	if s[1] == ']' {
		return 0, false
	}
	i := 1
	for {
		n, ok := scanCanonicalJSONValue(s[i:], depth-1)
		if !ok {
			return 0, false
		}
		i += n
		if i >= len(s) {
			return 0, false
		}
		switch s[i] {
		case ']':
			return i + 1, true
		case ',':
			i++
			if i >= len(s) {
				return 0, false
			}
		default:
			return 0, false
		}
	}
}

// scanCanonicalJSONString consumes one canonical JSON string. When dst is
// non-nil it also appends the decoded bytes; a key that does not fit is not
// canonical for this scanner. dst nil validates without retaining the text.
func scanCanonicalJSONString(s []byte, dst []byte) (int, []byte, bool) {
	if len(s) < 2 || s[0] != '"' {
		return 0, dst, false
	}
	i := 1
	for i < len(s) {
		c := s[i]
		if c == '"' {
			return i + 1, dst, true
		}
		if c == '\\' {
			adv, decoded, ok := takeCanonicalJSONEscape(s[i:])
			if !ok {
				return 0, dst, false
			}
			if dst != nil {
				if len(decoded) > cap(dst)-len(dst) {
					return 0, dst, false
				}
				dst = append(dst, decoded...)
			}
			i += adv
			continue
		}
		if c < 0x20 || c == '<' || c == '>' || c == '&' {
			return 0, dst, false
		}
		if c < utf8.RuneSelf {
			if dst != nil {
				if len(dst) == cap(dst) {
					return 0, dst, false
				}
				dst = append(dst, c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(s[i:])
		if r == utf8.RuneError && size == 1 || r == '\u2028' || r == '\u2029' {
			return 0, dst, false
		}
		if dst != nil {
			if size > cap(dst)-len(dst) {
				return 0, dst, false
			}
			dst = append(dst, s[i:i+size]...)
		}
		i += size
	}
	return 0, dst, false
}

func takeCanonicalJSONEscape(s []byte) (int, []byte, bool) {
	if len(s) < 2 || s[0] != '\\' {
		return 0, nil, false
	}
	switch s[1] {
	case '"', '\\':
		return 2, s[1:2], true
	case 'b':
		return 2, canonEscapeBackspace[:], true
	case 'f':
		return 2, canonEscapeFormFeed[:], true
	case 'n':
		return 2, canonEscapeNewline[:], true
	case 'r':
		return 2, canonEscapeCarriage[:], true
	case 't':
		return 2, canonEscapeTab[:], true
	case 'u':
		if len(s) < 6 {
			return 0, nil, false
		}
		switch {
		case bytes.Equal(s[:6], canonEscapeLT):
			return 6, canonEscapeLTDecoded[:], true
		case bytes.Equal(s[:6], canonEscapeGT):
			return 6, canonEscapeGTDecoded[:], true
		case bytes.Equal(s[:6], canonEscapeAmp):
			return 6, canonEscapeAmpDecoded[:], true
		case bytes.Equal(s[:6], canonEscapeLS):
			return 6, canonEscapeLSDecoded[:], true
		case bytes.Equal(s[:6], canonEscapePS):
			return 6, canonEscapePSDecoded[:], true
		}
		if s[2] == '0' && s[3] == '0' {
			h1, ok1 := canonicalHex(s[4])
			h2, ok2 := canonicalHex(s[5])
			if !ok1 || !ok2 {
				return 0, nil, false
			}
			b := h1<<4 | h2
			switch b {
			case '\b', '\t', '\n', '\f', '\r':
				return 0, nil, false
			}
			if b < 0x20 {
				return 6, canonControls[b : b+1], true
			}
		}
		return 0, nil, false
	default:
		return 0, nil, false
	}
}

func canonicalHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}

func scanCanonicalJSONNumber(s []byte) (int, bool) {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	if i >= len(s) {
		return 0, false
	}
	switch {
	case s[i] == '0':
		i++
	case s[i] >= '1' && s[i] <= '9':
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	default:
		return 0, false
	}
	if i < len(s) && s[i] == '.' {
		j := i + 1
		if j >= len(s) || s[j] < '0' || s[j] > '9' {
			return 0, false
		}
		i = j + 1
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j >= len(s) || s[j] < '0' || s[j] > '9' {
			return 0, false
		}
		i = j + 1
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	return i, true
}
