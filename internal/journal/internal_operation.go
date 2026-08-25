package journal

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// InternalOperationID is an operation identity minted by a reducer-owned
// namespace. The type is deliberately defined in an internal package so only
// Provenance implementation packages can construct the token that authorizes
// a reserved operation identity; public Journal.Apply callers only provide an
// OperationID and are rejected before a transaction begins.
type InternalOperationID struct {
	value OperationID
}

const governedAllocationSupplementOperationPrefix = "provenance.governed-supplement.v1."

// NewGovernedAllocationSupplementOperationID derives the one internal journal
// identity owned by a composed governed allocation. The derivation is stable so
// a persisted composed receipt can be reconstructed after reopen, but callers
// cannot obtain its internal capability type through the public package API.
func NewGovernedAllocationSupplementOperationID(external OperationID) InternalOperationID {
	sum := sha256.Sum256(append([]byte("provenance.governed-allocation.supplement.v1\x00"), []byte(external)...))
	return InternalOperationID{value: OperationID(governedAllocationSupplementOperationPrefix + fmt.Sprintf("%x", sum[:]))}
}

// OperationID returns the persistence identity carried by this internal token.
func (id InternalOperationID) OperationID() OperationID { return id.value }

// IsReservedInternalOperationID reports whether an identity belongs to a
// reducer-owned operation namespace. It must be checked at every public generic
// apply ingress; the composed reducer writes its own typed token directly in
// the enclosing transaction rather than routing it through public Apply.
func IsReservedInternalOperationID(id OperationID) bool {
	return strings.HasPrefix(string(id), governedAllocationSupplementOperationPrefix)
}

// ValidateExternalOperationID validates a public operation identity and rejects
// reducer-owned namespaces before a generic operation can claim them.
func ValidateExternalOperationID(id OperationID) error {
	if err := ValidateOperationID(id); err != nil {
		return err
	}
	if IsReservedInternalOperationID(id) {
		return fmt.Errorf("operation ID %q is a reserved internal governed-allocation supplemental reducer operation", id)
	}
	return nil
}
