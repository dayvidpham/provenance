package provenance

// dbos_wire.go is the deterministic serialization bridge between validated
// journal operations and DBOS durable workflow history. The sole supported
// contract transports the reviewed canonical mutation bytes directly.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

const (
	// The /v2 strings remain pinned durable identities. Earlier experimental /v1
	// tokens are not reassigned to incompatible bytes.
	DBOSApplyInputSchema = "provenance.dbos-apply-input/v2"
	dbosContextSchema    = "provenance.dbos-context/v2"
	maxDBOSContextBytes  = 3*MaxCanonicalFieldBytes + 64
)

var ErrDBOSContextFrame = errors.New("provenance: invalid DBOS context frame")

type DBOSContextFrameError struct {
	Field  string
	Reason string
	Fix    string
}

func (e *DBOSContextFrameError) Error() string {
	return fmt.Sprintf("%v: what: input field %s is invalid; why: %s; where: DBOS envelope/context codec; when: before workflow identity or domain fold; impact: the persisted input is rejected and no domain write runs; fix: %s", ErrDBOSContextFrame, e.Field, e.Reason, e.Fix)
}

func (e *DBOSContextFrameError) Is(target error) bool { return target == ErrDBOSContextFrame }

func dbosContextFrameError(field, reason, fix string) error {
	return &DBOSContextFrameError{Field: field, Reason: reason, Fix: fix}
}

// DBOSOperationContextBytes carries operation identity and audit context.
type DBOSOperationContextBytes []byte

// DBOSMutationBytes carries the canonical mutation. Its digest is always derived.
type DBOSMutationBytes []byte

// DBOSApplyInput is the closed durable workflow input.
type DBOSApplyInput struct {
	Schema   string                    `json:"schema"`
	Context  DBOSOperationContextBytes `json:"context"`
	Mutation DBOSMutationBytes         `json:"mutation"`
}

func encodeApplyInput(in journal.OperationInput) (DBOSApplyInput, journal.OperationInput, error) {
	prepared, err := journal.PrepareMutationV1(in.Effects)
	if err != nil {
		return DBOSApplyInput{}, journal.OperationInput{}, err
	}
	in.Effects = prepared.NormalizedEffects()
	in.MutationDigest = prepared.DerivedDigest()
	contextBytes, err := encodeDBOSContext(in)
	if err != nil {
		return DBOSApplyInput{}, journal.OperationInput{}, err
	}
	return DBOSApplyInput{Schema: DBOSApplyInputSchema, Context: contextBytes, Mutation: prepared.CanonicalBytes()}, in, nil
}

func decodeApplyInput(input DBOSApplyInput) (journal.OperationInput, error) {
	if input.Schema != DBOSApplyInputSchema {
		return journal.OperationInput{}, dbosContextFrameError("schema", fmt.Sprintf("outer input schema %q is not supported schema %q", input.Schema, DBOSApplyInputSchema), "restore the original envelope or recover it with a build supporting its schema")
	}
	in, err := decodeDBOSContext(input.Context)
	if err != nil {
		return journal.OperationInput{}, err
	}
	prepared, err := journal.DecodeCanonicalMutation(input.Mutation)
	if err != nil {
		return journal.OperationInput{}, fmt.Errorf("provenance: decode apply-input canonical mutation: %w", err)
	}
	in.Effects = prepared.NormalizedEffects()
	in.MutationDigest = prepared.DerivedDigest()
	return in, nil
}

func encodeDBOSContext(in journal.OperationInput) ([]byte, error) {
	if err := journal.ValidateOperationID(in.OperationID); err != nil {
		return nil, dbosContextFrameError("operation", err.Error(), "supply a non-empty control-free OperationID")
	}
	actor := actorToWire(in.ActorID)
	if _, err := actorFromWire(actor); err != nil {
		return nil, dbosContextFrameError("actor", err.Error(), "supply a canonical non-zero actor identity")
	}
	fields := [][]byte{[]byte(dbosContextSchema), []byte(in.OperationID), []byte(actor)}
	if in.AuthorityJournalID == nil {
		fields = append(fields, []byte{0})
	} else {
		var authority [9]byte
		authority[0] = 1
		binary.BigEndian.PutUint64(authority[1:], uint64(*in.AuthorityJournalID))
		fields = append(fields, authority[:])
	}
	fields = append(fields, append([]byte(nil), in.CommandDigest...))
	var recorded [8]byte
	binary.BigEndian.PutUint64(recorded[:], uint64(in.RecordedAt))
	fields = append(fields, recorded[:])
	var out bytes.Buffer
	for i, field := range fields {
		if len(field) > MaxCanonicalFieldBytes {
			return nil, dbosContextFrameError(fmt.Sprintf("field.%d", i), fmt.Sprintf("%d bytes exceeds maximum %d", len(field), MaxCanonicalFieldBytes), "use bounded operation identity and command digest operands")
		}
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		out.Write(size[:])
		out.Write(field)
	}
	if out.Len() > maxDBOSContextBytes {
		return nil, dbosContextFrameError("context", fmt.Sprintf("%d bytes exceeds maximum %d", out.Len(), maxDBOSContextBytes), "reduce context operands below the aggregate bound")
	}
	return out.Bytes(), nil
}

func decodeDBOSContext(data []byte) (journal.OperationInput, error) {
	if len(data) > maxDBOSContextBytes {
		return journal.OperationInput{}, dbosContextFrameError("context", fmt.Sprintf("%d bytes exceeds maximum %d", len(data), maxDBOSContextBytes), "restore bounded persisted context bytes from the original workflow input")
	}
	r := bytes.NewReader(data)
	read := func(name string) ([]byte, error) {
		var size uint32
		if err := binary.Read(r, binary.BigEndian, &size); err != nil {
			return nil, dbosContextFrameError(name, "missing or truncated length prefix: "+err.Error(), "restore the complete ordered context frame")
		}
		if size > MaxCanonicalFieldBytes || uint64(size) > uint64(r.Len()) {
			return nil, dbosContextFrameError(name, fmt.Sprintf("declared length %d exceeds the field bound or remaining %d bytes", size, r.Len()), "restore the field length and bytes from the original persisted input")
		}
		value := make([]byte, int(size))
		if _, err := io.ReadFull(r, value); err != nil {
			return nil, dbosContextFrameError(name, "truncated field bytes: "+err.Error(), "restore the complete field bytes from the original persisted input")
		}
		return value, nil
	}
	names := []string{"version", "operation", "actor", "authority", "command", "recorded-at"}
	values := make([][]byte, len(names))
	for i := range names {
		var err error
		values[i], err = read(names[i])
		if err != nil {
			return journal.OperationInput{}, err
		}
	}
	if r.Len() != 0 {
		return journal.OperationInput{}, dbosContextFrameError("trailing", fmt.Sprintf("%d bytes remain after the six closed fields", r.Len()), "remove unknown or duplicate fields by restoring the exact original frame")
	}
	if string(values[0]) != dbosContextSchema {
		return journal.OperationInput{}, dbosContextFrameError("version", fmt.Sprintf("unsupported version %q", values[0]), "recover with a build that supports the persisted version or restore a supported frame")
	}
	operation := journal.OperationID(values[1])
	if err := journal.ValidateOperationID(operation); err != nil {
		return journal.OperationInput{}, dbosContextFrameError("operation", err.Error(), "restore the original non-empty control-free OperationID")
	}
	actor, err := actorFromWire(string(values[2]))
	if err != nil {
		return journal.OperationInput{}, dbosContextFrameError("actor", err.Error(), "restore the original canonical actor identity")
	}
	var authority *journal.JournalID
	switch len(values[3]) {
	case 1:
		if values[3][0] != 0 {
			return journal.OperationInput{}, dbosContextFrameError("authority", fmt.Sprintf("invalid absent tag %d", values[3][0]), "restore tag 0 for genesis or tag 1 plus eight bytes for a journal authority")
		}
	case 9:
		if values[3][0] != 1 {
			return journal.OperationInput{}, dbosContextFrameError("authority", fmt.Sprintf("invalid present tag %d", values[3][0]), "restore tag 1 followed by the original eight-byte journal authority")
		}
		value := journal.JournalID(int64(binary.BigEndian.Uint64(values[3][1:])))
		authority = &value
	default:
		return journal.OperationInput{}, dbosContextFrameError("authority", fmt.Sprintf("length %d is neither 1 nor 9", len(values[3])), "restore the one-byte absent form or nine-byte present form")
	}
	if len(values[5]) != 8 {
		return journal.OperationInput{}, dbosContextFrameError("recorded-at", fmt.Sprintf("length %d is not 8", len(values[5])), "restore the original eight-byte audit timestamp")
	}
	return journal.OperationInput{
		OperationID: operation, ActorID: actor, AuthorityJournalID: authority,
		CommandDigest: append([]byte(nil), values[4]...), RecordedAt: int64(binary.BigEndian.Uint64(values[5])),
	}, nil
}

func actorToWire(id ptypes.ActorID) string {
	if id == (ptypes.ActorID{}) {
		return ""
	}
	return id.String()
}

func actorFromWire(value string) (ptypes.ActorID, error) {
	if value == "" {
		return ptypes.ActorID{}, nil
	}
	return ptypes.ParseActorID(value)
}
