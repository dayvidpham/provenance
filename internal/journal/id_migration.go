package journal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// ConvertRetiredIdentity is private to internal offline migration callers. No
// runtime parser, lookup, or alias resolution uses the retired representation.
//
// It tolerates an identity already written in the current slash form: a
// partly-converted store (an interrupted conversion, or a writer that left keys
// mixed) must still convert, and the slash value is simply returned normalized.
// This is not a compatibility reader — nothing resolves the retired form at
// runtime — and a value that is neither a canonical slash identity nor a retired
// two-dash identity is still refused.
func ConvertRetiredIdentity(raw string) (string, error) {
	if current, err := ptypes.ParseTaskID(raw); err == nil {
		return current.String(), nil
	}
	i := strings.LastIndex(raw, "--")
	if i <= 0 || strings.LastIndexByte(raw, '/') > i {
		return "", fmt.Errorf("retired identity %q has no non-empty namespace and final two-dash UUID, or a slash follows that separator; where: offline identity preflight; no identity returned; fix: restore a single-format archive before conversion", raw)
	}
	id, err := ptypes.ParseTaskID(raw[:i] + "/" + raw[i+2:])
	if err != nil {
		return "", fmt.Errorf("retired identity %q cannot convert without changing its namespace or UUID: %w", raw, err)
	}
	return id.String(), nil
}

// ConvertRetiredCanonicalMutation authenticates the original digest before
// translating declared identity operands, then delegates complete field/order/
// semantic validation and the new digest to the production canonical owner.
// Opaque authority/operation/result IDs, payloads and prose stay byte-exact.
func ConvertRetiredCanonicalMutation(data, storedDigest []byte) (CanonicalMutation, error) {
	digest := sha256.Sum256(data)
	if !bytes.Equal(digest[:], storedDigest) {
		return CanonicalMutation{}, canonicalMutationError("stored mutation digest", "retired canonical bytes do not match their stored SHA-256", "restore bytes and digest from the same archived operation")
	}
	if len(data) > MaxCanonicalMutationBytes {
		return CanonicalMutation{}, canonicalMutationError("mutation", "retired bytes exceed the canonical bound", "restore bounded archived canonical bytes")
	}
	r := bufio.NewReader(bytes.NewReader(data))
	var out bytes.Buffer
	contextKinds := map[string]string{}
	for {
		name, err := r.ReadString(':')
		if err == io.EOF && name == "" {
			break
		}
		if err != nil {
			return CanonicalMutation{}, canonicalMutationError("retired framing", err.Error(), "restore complete length-prefixed canonical bytes")
		}
		name = strings.TrimSuffix(name, ":")
		length, err := r.ReadString(':')
		if err != nil {
			return CanonicalMutation{}, err
		}
		n, err := strconv.Atoi(strings.TrimSuffix(length, ":"))
		if err != nil || n < 0 || n > MaxCanonicalFieldBytes || length != strconv.Itoa(n)+":" {
			return CanonicalMutation{}, canonicalMutationError(name, "invalid retired field length", "restore bounded canonical framing")
		}
		value := make([]byte, n)
		if _, err := io.ReadFull(r, value); err != nil {
			return CanonicalMutation{}, err
		}
		if separator, err := r.ReadByte(); err != nil || separator != '\n' {
			return CanonicalMutation{}, canonicalMutationError(name, "missing framed newline", "restore complete canonical framing")
		}
		parts := strings.Split(name, ".")
		identity := false
		if len(parts) == 3 && parts[0] == "effect" {
			switch parts[2] {
			case "task", "occupant", "edge-target", "comment", "comment-author", "activity-id", "activity-agent-id":
				identity = true
			}
		}
		if len(parts) == 3 && parts[0] == "condition" && parts[2] == "task-id" && len(value) > 0 {
			identity = true
		}
		if len(parts) == 4 && parts[0] == "condition" && parts[2] == "actor" {
			identity = true
		}
		if len(parts) == 5 && parts[2] == "context" {
			prefix := strings.Join(parts[:4], ".")
			if parts[4] == "kind" {
				contextKinds[prefix] = string(value)
			}
			if parts[4] == "identity" {
				switch EventContextKind(contextKinds[prefix]) {
				case EventContextKindTask, EventContextKindActor, EventContextKindActivity:
					identity = true
				}
			}
		}
		if identity {
			if string(value) == "--00000000-0000-0000-0000-000000000000" {
				value = []byte(canonicalV1ZeroIdentity)
			} else {
				converted, err := ConvertRetiredIdentity(string(value))
				if err != nil {
					return CanonicalMutation{}, canonicalMutationError(name, err.Error(), "repair the named typed identity in the archived operation")
				}
				value = []byte(converted)
			}
		}
		if len(parts) == 3 && parts[0] == "condition" && parts[2] == "assignment-active" {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(value, &fields); err != nil {
				return CanonicalMutation{}, err
			}
			original, err := json.Marshal(fields)
			if err != nil || !bytes.Equal(value, original) {
				return CanonicalMutation{}, canonicalMutationError(name, "retired assignment JSON has duplicate, reordered or noncanonical fields", "restore exact previous-owner canonical assignment bytes")
			}
			for _, field := range []string{"task_id", "occupant"} {
				var raw string
				if err := json.Unmarshal(fields[field], &raw); err != nil {
					return CanonicalMutation{}, err
				}
				converted, err := ConvertRetiredIdentity(raw)
				if err != nil {
					return CanonicalMutation{}, err
				}
				fields[field], _ = json.Marshal(converted)
			}
			value, err = json.Marshal(fields)
			if err != nil {
				return CanonicalMutation{}, err
			}
		}
		fmt.Fprintf(&out, "%s:%d:", name, len(value))
		out.Write(value)
		out.WriteByte('\n')
	}
	return DecodeCanonicalMutation(out.Bytes())
}
