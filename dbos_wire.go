package provenance

// dbos_wire.go is the deterministic serialization bridge between validated
// journal operations and DBOS durable workflow history. The sole supported
// contract transports the reviewed canonical mutation bytes directly.
//
// Identity constants (schema tags, prefixes, library version) are defined in
// dbos_contract.go as the sole authority. This file must not redeclare them.

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
	// maxDBOSContextBytes is the maximum allowed byte length for the encoded
	// operation context frame. 3 field-max-bytes covers operation+actor+command;
	// 64 covers schema tag, authority, recorded-at, and length prefixes.
	maxDBOSContextBytes = 3*MaxCanonicalFieldBytes + 64
)

var ErrDBOSContextFrame = errors.New("provenance: invalid DBOS context frame")

// DBOSContextFrameError is a typed, errors.Is-matchable error for any validation
// failure in the DBOS context encode/decode path. Field, Reason, and Fix provide
// the actionable detail required by [C-actionable-errors].
type DBOSContextFrameError = DBOSDiagnosticError

func dbosContextFrameError(field DBOSDiagnosticField, stage DBOSDiagnosticStage, reason, fix string) error {
	return dbosContextFramePositionError(field, stage, nil, reason, fix)
}

func dbosContextFramePositionError(field DBOSDiagnosticField, stage DBOSDiagnosticStage, position *int, reason, fix string) error {
	return &DBOSDiagnosticError{Class: DBOSDiagClassContextFrame, Field: field, Stage: stage, Reason: reason,
		Position: position, Impact: "the persisted input is rejected and no domain write runs", Fix: fix, Cause: ErrDBOSContextFrame}
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

func encodeApplyInput(contract dbosContractSnapshot, in journal.OperationInput) (DBOSApplyInput, journal.OperationInput, error) {
	prepared, err := journal.PrepareMutationV1(in.Effects)
	if err != nil {
		return DBOSApplyInput{}, journal.OperationInput{}, err
	}
	in.Effects = prepared.NormalizedEffects()
	in.MutationDigest = prepared.DerivedDigest()
	contextBytes, err := encodeDBOSContext(contract, in)
	if err != nil {
		return DBOSApplyInput{}, journal.OperationInput{}, err
	}
	return DBOSApplyInput{Schema: contract.applyInputSchema, Context: contextBytes, Mutation: prepared.CanonicalBytes()}, in, nil
}

func decodeApplyInput(contract dbosContractSnapshot, input DBOSApplyInput) (journal.OperationInput, error) {
	if input.Schema != contract.applyInputSchema {
		return journal.OperationInput{}, dbosContextFrameError(
			DBOSDiagFieldSchema, DBOSDiagStageContextDecode,
			fmt.Sprintf("outer input schema %q is not supported schema %q", input.Schema, contract.applyInputSchema),
			"restore the original envelope or recover it with a build supporting its schema")
	}
	in, err := decodeDBOSContext(contract, input.Context)
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

func encodeDBOSContext(contract dbosContractSnapshot, in journal.OperationInput) ([]byte, error) {
	if err := journal.ValidateOperationID(in.OperationID); err != nil {
		return nil, dbosContextFrameError(DBOSDiagFieldOperation, DBOSDiagStageContextEncode, err.Error(), "supply a non-empty control-free OperationID")
	}
	actor := actorToWire(in.ActorID)
	if actor == "" {
		return nil, dbosContextFrameError(DBOSDiagFieldActor, DBOSDiagStageContextEncode, "the actor identity is the zero value", "supply the registered non-zero actor committing this operation")
	}
	if _, err := actorFromWire(actor); err != nil {
		return nil, dbosContextFrameError(DBOSDiagFieldActor, DBOSDiagStageContextEncode, err.Error(), "supply a canonical non-zero actor identity")
	}
	if len(in.CommandDigest) == 0 {
		return nil, dbosContextFrameError(DBOSDiagFieldCommand, DBOSDiagStageContextEncode, "the command digest is empty", "supply a non-empty digest of the command that produced this operation")
	}
	type contextField struct {
		name  DBOSDiagnosticField
		value []byte
	}
	fields := []contextField{{DBOSDiagFieldContextVersion, []byte(contract.contextSchema)}, {DBOSDiagFieldOperation, []byte(in.OperationID)}, {DBOSDiagFieldActor, []byte(actor)}}
	if in.AuthorityJournalID == nil {
		fields = append(fields, contextField{DBOSDiagFieldAuthority, []byte{0}})
	} else {
		var authority [9]byte
		authority[0] = 1
		binary.BigEndian.PutUint64(authority[1:], uint64(*in.AuthorityJournalID))
		fields = append(fields, contextField{DBOSDiagFieldAuthority, authority[:]})
	}
	fields = append(fields, contextField{DBOSDiagFieldCommand, append([]byte(nil), in.CommandDigest...)})
	var recorded [8]byte
	binary.BigEndian.PutUint64(recorded[:], uint64(in.RecordedAt))
	fields = append(fields, contextField{DBOSDiagFieldRecordedAt, recorded[:]})
	var out bytes.Buffer
	for i, field := range fields {
		if len(field.value) > MaxCanonicalFieldBytes {
			position := i
			return nil, dbosContextFramePositionError(
				field.name, DBOSDiagStageContextEncode, &position,
				fmt.Sprintf("%d bytes exceeds maximum %d", len(field.value), MaxCanonicalFieldBytes),
				"use bounded operation identity and command digest operands")
		}
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field.value)))
		out.Write(size[:])
		out.Write(field.value)
	}
	if out.Len() > maxDBOSContextBytes {
		return nil, dbosContextFrameError(
			DBOSDiagFieldContext, DBOSDiagStageContextEncode,
			fmt.Sprintf("%d bytes exceeds maximum %d", out.Len(), maxDBOSContextBytes),
			"reduce context operands below the aggregate bound")
	}
	return out.Bytes(), nil
}

func decodeDBOSContext(contract dbosContractSnapshot, data []byte) (journal.OperationInput, error) {
	if len(data) > maxDBOSContextBytes {
		return journal.OperationInput{}, dbosContextFrameError(
			DBOSDiagFieldContext, DBOSDiagStageContextDecode,
			fmt.Sprintf("%d bytes exceeds maximum %d", len(data), maxDBOSContextBytes),
			"restore bounded persisted context bytes from the original workflow input")
	}
	r := bytes.NewReader(data)
	read := func(field DBOSDiagnosticField, position int) ([]byte, error) {
		var size uint32
		if err := binary.Read(r, binary.BigEndian, &size); err != nil {
			return nil, dbosContextFramePositionError(field, DBOSDiagStageContextDecode, &position, "missing or truncated length prefix: "+err.Error(), "restore the complete ordered context frame")
		}
		if size > MaxCanonicalFieldBytes || uint64(size) > uint64(r.Len()) {
			return nil, dbosContextFramePositionError(field, DBOSDiagStageContextDecode, &position, fmt.Sprintf("declared length %d exceeds the field bound or remaining %d bytes", size, r.Len()), "restore the field length and bytes from the original persisted input")
		}
		value := make([]byte, int(size))
		if _, err := io.ReadFull(r, value); err != nil {
			return nil, dbosContextFramePositionError(field, DBOSDiagStageContextDecode, &position, "truncated field bytes: "+err.Error(), "restore the complete field bytes from the original persisted input")
		}
		return value, nil
	}
	fields := []DBOSDiagnosticField{DBOSDiagFieldContextVersion, DBOSDiagFieldOperation, DBOSDiagFieldActor, DBOSDiagFieldAuthority, DBOSDiagFieldCommand, DBOSDiagFieldRecordedAt}
	values := make([][]byte, len(fields))
	for i := range fields {
		var err error
		values[i], err = read(fields[i], i)
		if err != nil {
			return journal.OperationInput{}, err
		}
	}
	if r.Len() != 0 {
		return journal.OperationInput{}, dbosContextFrameError(
			DBOSDiagFieldTrailing, DBOSDiagStageContextDecode,
			fmt.Sprintf("%d bytes remain after the six closed fields", r.Len()),
			"remove unknown or duplicate fields by restoring the exact original frame")
	}
	if string(values[0]) != contract.contextSchema {
		return journal.OperationInput{}, dbosContextFrameError(
			DBOSDiagFieldContextVersion, DBOSDiagStageContextDecode,
			fmt.Sprintf("unsupported version %q", values[0]),
			"recover with a build that supports the persisted version or restore a supported frame")
	}
	operation := journal.OperationID(values[1])
	if err := journal.ValidateOperationID(operation); err != nil {
		return journal.OperationInput{}, dbosContextFrameError(DBOSDiagFieldOperation, DBOSDiagStageContextDecode, err.Error(), "restore the original non-empty control-free OperationID")
	}
	actor, err := actorFromWire(string(values[2]))
	if err != nil {
		return journal.OperationInput{}, dbosContextFrameError(DBOSDiagFieldActor, DBOSDiagStageContextDecode, err.Error(), "restore the original canonical actor identity")
	}
	if actor == (ptypes.ActorID{}) {
		return journal.OperationInput{}, dbosContextFrameError(DBOSDiagFieldActor, DBOSDiagStageContextDecode, "the persisted actor identity is the zero value", "restore the registered non-zero actor that committed the operation")
	}
	var authority *journal.JournalID
	switch len(values[3]) {
	case 1:
		if values[3][0] != 0 {
			return journal.OperationInput{}, dbosContextFrameError(DBOSDiagFieldAuthority, DBOSDiagStageContextDecode, fmt.Sprintf("invalid absent tag %d", values[3][0]), "restore tag 0 for genesis or tag 1 plus eight bytes for a journal authority")
		}
	case 9:
		if values[3][0] != 1 {
			return journal.OperationInput{}, dbosContextFrameError(DBOSDiagFieldAuthority, DBOSDiagStageContextDecode, fmt.Sprintf("invalid present tag %d", values[3][0]), "restore tag 1 followed by the original eight-byte journal authority")
		}
		value := journal.JournalID(int64(binary.BigEndian.Uint64(values[3][1:])))
		authority = &value
	default:
		return journal.OperationInput{}, dbosContextFrameError(DBOSDiagFieldAuthority, DBOSDiagStageContextDecode, fmt.Sprintf("length %d is neither 1 nor 9", len(values[3])), "restore the one-byte absent form or nine-byte present form")
	}
	if len(values[5]) != 8 {
		return journal.OperationInput{}, dbosContextFrameError(DBOSDiagFieldRecordedAt, DBOSDiagStageContextDecode, fmt.Sprintf("length %d is not 8", len(values[5])), "restore the original eight-byte audit timestamp")
	}
	if len(values[4]) == 0 {
		return journal.OperationInput{}, dbosContextFrameError(DBOSDiagFieldCommand, DBOSDiagStageContextDecode, "the persisted command digest is empty", "restore the non-empty command digest from the original workflow input")
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
