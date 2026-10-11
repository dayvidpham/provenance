package allocation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
)

type ConvertedRequest struct {
	Canonical                  []byte
	Digest                     [sha256.Size]byte
	SupplementOperation        journal.OperationID
	OldSupplementCommandDigest []byte
	NewSupplementCommandDigest []byte
	Command                    string
}

// ConvertRetiredRequest is an offline owner-codec boundary. It authenticates
// stored bytes, refuses unknown/duplicate/noncanonical JSON fields by exact
// reencoding, translates declared typed identities, and invokes the ordinary
// request decoder before returning the new receipt representation.
func ConvertRetiredRequest(encoded, storedDigest []byte) (ConvertedRequest, error) {
	digest := sha256.Sum256(encoded)
	if !bytes.Equal(digest[:], storedDigest) {
		return ConvertedRequest{}, fmt.Errorf("governed allocation offline conversion: stored canonical digest disagrees with request bytes; where: allocation codec; when: preflight; no validated request returned; fix: restore request and digest from the same archive")
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &header); err != nil {
		return ConvertedRequest{}, err
	}
	if _, composed := header["allocationCanonical"]; composed {
		var wire composedCanonicalWire
		if err := json.Unmarshal(encoded, &wire); err != nil {
			return ConvertedRequest{}, err
		}
		original, err := json.Marshal(wire)
		if err != nil || !bytes.Equal(original, encoded) {
			return ConvertedRequest{}, fmt.Errorf("allocation offline conversion: unknown or noncanonical composed fields; no validated receipt returned; fix: restore exact previous-owner canonical bytes")
		}
		allocationDigest := sha256.Sum256(wire.AllocationCanonical)
		convertedAllocation, err := ConvertRetiredRequest(wire.AllocationCanonical, allocationDigest[:])
		if err != nil {
			return ConvertedRequest{}, err
		}
		supplementDigest := sha256.Sum256(wire.SupplementalCanonical)
		convertedSupplement, err := journal.ConvertRetiredCanonicalMutation(wire.SupplementalCanonical, supplementDigest[:])
		if err != nil {
			return ConvertedRequest{}, err
		}
		result := ConvertedRequest{OldSupplementCommandDigest: supplementalCommandDigest(wire.AllocationCanonical), NewSupplementCommandDigest: supplementalCommandDigest(convertedAllocation.Canonical), Command: convertedAllocation.Command}
		wire.AllocationCanonical = convertedAllocation.Canonical
		wire.SupplementalCanonical = convertedSupplement.CanonicalBytes()
		if wire.ReferenceScope != nil {
			for i, raw := range wire.ReferenceScope.Subjects {
				wire.ReferenceScope.Subjects[i], err = journal.ConvertRetiredIdentity(raw)
				if err != nil {
					return ConvertedRequest{}, err
				}
			}
		}
		result.Canonical, result.Digest, err = marshalComposedCanonical(wire)
		if err != nil {
			return ConvertedRequest{}, err
		}
		request, err := DecodeComposedRequest(result.Canonical)
		if err != nil {
			return ConvertedRequest{}, err
		}
		result.SupplementOperation = GovernedAllocationSupplementOperationID(request.Allocation.OperationID)
		return result, nil
	}
	var wire canonicalWire
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return ConvertedRequest{}, err
	}
	original, err := json.Marshal(wire)
	if err != nil || !bytes.Equal(original, encoded) {
		return ConvertedRequest{}, fmt.Errorf("allocation offline conversion: unknown or noncanonical request fields; no validated receipt returned; fix: restore exact previous-owner canonical bytes")
	}
	wire.ActorID, err = journal.ConvertRetiredIdentity(wire.ActorID)
	if err != nil {
		return ConvertedRequest{}, err
	}
	for i := range wire.Children {
		wire.Children[i].TaskID, err = journal.ConvertRetiredIdentity(wire.Children[i].TaskID)
		if err != nil {
			return ConvertedRequest{}, err
		}
		wire.Children[i].Occupant, err = journal.ConvertRetiredIdentity(wire.Children[i].Occupant)
		if err != nil {
			return ConvertedRequest{}, err
		}
	}
	result := ConvertedRequest{Command: wire.Command}
	result.Canonical, result.Digest, err = marshalCanonical(wire)
	if err != nil {
		return ConvertedRequest{}, err
	}
	if _, _, _, _, _, err := DecodeCanonical(result.Canonical); err != nil {
		return ConvertedRequest{}, err
	}
	return result, nil
}
