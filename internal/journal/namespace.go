package journal

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// This file implements the generic actor-namespace registry of
// docs/journal-relational-contract.md §7. Provenance stores a claimant, a fixed
// UUID range, and a codec name; it never embeds any consumer's ordinal→name
// mapping (the pasture-system claim itself belongs to the consumer). The
// reducer enforces the two properties SQL cannot express (§7.3): non-overlapping
// ranges and entry-in-range membership.

// UUIDRange is an inclusive [Min, Max] interval of 16-byte fixed-UUID values,
// compared big-endian (BLOB(16), §7.1).
type UUIDRange struct {
	Min [16]byte
	Max [16]byte
}

// Ordered reports whether Min <= Max (the only well-formed orientation).
func (r UUIDRange) Ordered() bool { return bytes.Compare(r.Min[:], r.Max[:]) <= 0 }

// Contains reports whether v lies within [Min, Max] inclusive.
func (r UUIDRange) Contains(v [16]byte) bool {
	return bytes.Compare(v[:], r.Min[:]) >= 0 && bytes.Compare(v[:], r.Max[:]) <= 0
}

// Overlaps reports whether two inclusive ranges intersect.
func (r UUIDRange) Overlaps(o UUIDRange) bool {
	// Disjoint iff one ends strictly before the other begins.
	return !(bytes.Compare(r.Max[:], o.Min[:]) < 0 || bytes.Compare(o.Max[:], r.Min[:]) < 0)
}

// Size returns the number of ordinals the range admits (Max - Min + 1) as a
// big.Int, since a 128-bit range can exceed uint64.
func (r UUIDRange) Size() *big.Int {
	min := new(big.Int).SetBytes(r.Min[:])
	max := new(big.Int).SetBytes(r.Max[:])
	return new(big.Int).Add(new(big.Int).Sub(max, min), big.NewInt(1))
}

// NamespaceCodec maps small ordinals to fixed UUIDs within a claimed range and
// back. Different claimants may register different codecs; Provenance selects
// one by its opaque name (§7.1 Codec column).
type NamespaceCodec interface {
	// Name is the stable codec identifier stored in actor_namespace_claims.Codec.
	Name() string
	// Encode maps an ordinal to its fixed UUID within the range.
	Encode(r UUIDRange, ordinal uint64) ([16]byte, error)
	// Decode maps a fixed UUID back to its ordinal within the range.
	Decode(r UUIDRange, id [16]byte) (uint64, error)
}

// OrdinalV1Codec is the default ordinal codec: the UUID for ordinal n is
// RangeMin + n interpreted as a big-endian 128-bit integer. This is the
// deterministic small-ordinal mapping §7.2 describes; a consumer claiming
// "ordinals 0–1023" gets [RangeMin, RangeMin+1023].
type OrdinalV1Codec struct{}

// OrdinalV1CodecName is the registered name of OrdinalV1Codec.
const OrdinalV1CodecName = "provenance.ordinal.v1"

func (OrdinalV1Codec) Name() string { return OrdinalV1CodecName }

func (OrdinalV1Codec) Encode(r UUIDRange, ordinal uint64) ([16]byte, error) {
	var out [16]byte
	if !r.Ordered() {
		return out, fmt.Errorf("%w: range Min exceeds Max", ErrNamespaceRange)
	}
	base := new(big.Int).SetBytes(r.Min[:])
	val := new(big.Int).Add(base, new(big.Int).SetUint64(ordinal))
	max := new(big.Int).SetBytes(r.Max[:])
	if val.Cmp(max) > 0 {
		return out, fmt.Errorf("%w: ordinal %d encodes past range Max", ErrEntryOutOfRange, ordinal)
	}
	val.FillBytes(out[:])
	return out, nil
}

func (OrdinalV1Codec) Decode(r UUIDRange, id [16]byte) (uint64, error) {
	if !r.Ordered() {
		return 0, fmt.Errorf("%w: range Min exceeds Max", ErrNamespaceRange)
	}
	base := new(big.Int).SetBytes(r.Min[:])
	val := new(big.Int).SetBytes(id[:])
	delta := new(big.Int).Sub(val, base)
	if delta.Sign() < 0 || !delta.IsUint64() {
		return 0, fmt.Errorf("%w: UUID does not decode to a valid ordinal under %s",
			ErrEntryOutOfRange, OrdinalV1CodecName)
	}
	return delta.Uint64(), nil
}

// codecRegistry is the closed set of built-in codecs, selectable by name.
var codecRegistry = map[string]NamespaceCodec{
	OrdinalV1CodecName: OrdinalV1Codec{},
}

// LookupCodec resolves a codec name registered with Provenance.
func LookupCodec(name string) (NamespaceCodec, error) {
	c, ok := codecRegistry[name]
	if !ok {
		return nil, fmt.Errorf("%w: codec %q is not registered", ErrNamespaceCodec, name)
	}
	return c, nil
}

// ActorNamespaceClaim is one actor_namespace_claims row (§7.1).
type ActorNamespaceClaim struct {
	Namespace  string
	ClaimantID string
	Range      UUIDRange
	Codec      string
}

// Validate checks a claim's own shape (non-empty fields, ordered range,
// registered codec) independent of any other claim.
func (c ActorNamespaceClaim) Validate() error {
	if c.Namespace == "" {
		return fmt.Errorf("%w: namespace is required", ErrNamespaceClaim)
	}
	if c.ClaimantID == "" {
		return fmt.Errorf("%w: claimant is required for namespace %q", ErrNamespaceClaim, c.Namespace)
	}
	if !c.Range.Ordered() {
		return fmt.Errorf("%w: namespace %q range Min exceeds Max", ErrNamespaceRange, c.Namespace)
	}
	if _, err := LookupCodec(c.Codec); err != nil {
		return fmt.Errorf("namespace %q: %w", c.Namespace, err)
	}
	return nil
}

// Equal reports whether two claims are identical in every stored field, the
// condition §7.1 requires for an idempotent re-registration.
func (c ActorNamespaceClaim) Equal(o ActorNamespaceClaim) bool {
	return c.Namespace == o.Namespace && c.ClaimantID == o.ClaimantID &&
		c.Range == o.Range && c.Codec == o.Codec
}

// CheckNoOverlap rejects a new claim whose range intersects any existing claim
// under a different namespace, with an actionable error naming BOTH namespaces
// (§7.3 rule 1). An identical re-registration of the same namespace is not an
// overlap (the caller handles idempotency separately via Equal).
func CheckNoOverlap(newClaim ActorNamespaceClaim, existing []ActorNamespaceClaim) error {
	for _, ex := range existing {
		if ex.Namespace == newClaim.Namespace {
			continue
		}
		if newClaim.Range.Overlaps(ex.Range) {
			return fmt.Errorf(
				"%w: namespace %q claims range [%x, %x] which overlaps the "+
					"existing claim of namespace %q ([%x, %x]) — where: actor "+
					"namespace registration; when: before commit; impact: the new "+
					"claim is rejected and nothing is written, since an accepted "+
					"overlap would later surface as an opaque fixed-actor primary-"+
					"key collision; fix: choose a disjoint [RangeMin, RangeMax] "+
					"for namespace %q or reconcile it with %q",
				ErrNamespaceRange,
				newClaim.Namespace, newClaim.Range.Min, newClaim.Range.Max,
				ex.Namespace, ex.Range.Min, ex.Range.Max,
				newClaim.Namespace, ex.Namespace,
			)
		}
	}
	return nil
}

// FixedActorEntry is one fixed_actor_manifest_entries row (§7.2).
type FixedActorEntry struct {
	ActorID   ActorID
	Namespace string
	ActorKind ptypes.AgentKind
	Name      string
	Metadata  string
}

// FixedSoftwareAgentRegistration is the complete input for atomically claiming
// a namespace and registering one fixed software actor and its manifest entry.
// Entry.ActorID is the single source of truth for the actor identity.
type FixedSoftwareAgentRegistration struct {
	Claim     ActorNamespaceClaim
	Entry     FixedActorEntry
	AgentName string
	Version   string
	Source    string
}

// Validate checks all cross-row invariants before a registration transaction
// starts. Empty metadata is normalized to an empty JSON object by persistence.
func (r FixedSoftwareAgentRegistration) Validate() error {
	if err := r.Claim.Validate(); err != nil {
		return err
	}
	if r.Entry.ActorID.Namespace == "" {
		return fmt.Errorf("%w: fixed software actor ID requires a namespace", ptypes.ErrInvalidID)
	}
	if r.Entry.Namespace != r.Claim.Namespace || r.Entry.ActorID.Namespace != r.Claim.Namespace {
		return fmt.Errorf("%w: claim namespace %q, entry namespace %q, and actor namespace %q must match",
			ErrNamespaceClaim, r.Claim.Namespace, r.Entry.Namespace, r.Entry.ActorID.Namespace)
	}
	if r.Entry.ActorKind != ptypes.AgentKindSoftware {
		return fmt.Errorf("%w: fixed software actor %q has kind %d; use software kind %d",
			ptypes.ErrAgentKindMismatch, r.Entry.ActorID.String(), r.Entry.ActorKind, ptypes.AgentKindSoftware)
	}
	if r.AgentName == "" {
		return fmt.Errorf("%w: fixed software actor %q requires an agent name", ErrNamespaceClaim, r.Entry.ActorID.String())
	}
	if r.Entry.Name == "" {
		return fmt.Errorf("%w: fixed software actor %q requires a manifest name", ErrNamespaceClaim, r.Entry.ActorID.String())
	}
	metadata := r.Entry.Metadata
	if metadata == "" {
		metadata = "{}"
	}
	if !json.Valid([]byte(metadata)) {
		return fmt.Errorf("%w: fixed software actor %q metadata is not valid JSON",
			ErrNamespaceClaim, r.Entry.ActorID.String())
	}
	return CheckEntryInRange(r.Claim, r.Entry.Namespace, [16]byte(r.Entry.ActorID.UUID))
}

// CheckEntryInRange rejects a fixed-actor entry whose 16-byte fixed UUID does
// not decode, under the namespace codec, to an ordinal inside the claimed range
// (§7.3 rule 2). actorUUID is derived from the entry's ActorID.
func CheckEntryInRange(claim ActorNamespaceClaim, entryNamespace string, actorUUID [16]byte) error {
	if entryNamespace != claim.Namespace {
		return fmt.Errorf("%w: entry namespace %q does not match claim namespace %q",
			ErrEntryOutOfRange, entryNamespace, claim.Namespace)
	}
	codec, err := LookupCodec(claim.Codec)
	if err != nil {
		return fmt.Errorf("namespace %q: %w", claim.Namespace, err)
	}
	if !claim.Range.Contains(actorUUID) {
		return fmt.Errorf(
			"%w: fixed actor %x is outside namespace %q's claimed range "+
				"[%x, %x] — where: fixed-actor manifest registration; when: before "+
				"commit; impact: the entry is rejected so it cannot collide with an "+
				"actor from a neighbouring range; fix: register the entry within "+
				"[RangeMin, RangeMax] or widen the namespace claim",
			ErrEntryOutOfRange, actorUUID, claim.Namespace, claim.Range.Min, claim.Range.Max,
		)
	}
	if _, err := codec.Decode(claim.Range, actorUUID); err != nil {
		return fmt.Errorf("namespace %q entry-in-range: %w", claim.Namespace, err)
	}
	return nil
}

// OrdinalUUID is a helper that renders a small ordinal to its 16-byte fixed
// UUID within a range under the default codec — the encoding §7.2 uses to
// pre-register the reserved system actors.
func OrdinalUUID(r UUIDRange, ordinal uint64) ([16]byte, error) {
	return OrdinalV1Codec{}.Encode(r, ordinal)
}

// BigEndianUUID renders a uint64 ordinal as a 16-byte big-endian value (a
// convenience for building test ranges/entries anchored at zero).
func BigEndianUUID(ordinal uint64) [16]byte {
	var out [16]byte
	binary.BigEndian.PutUint64(out[8:], ordinal)
	return out
}

var (
	// ErrNamespaceClaim is a malformed namespace claim.
	ErrNamespaceClaim = errors.New("provenance: invalid actor namespace claim")
	// ErrNamespaceRange is a malformed or overlapping fixed-UUID range.
	ErrNamespaceRange = errors.New("provenance: actor namespace range error")
	// ErrNamespaceCodec is an unknown or invalid namespace codec.
	ErrNamespaceCodec = errors.New("provenance: actor namespace codec error")
	// ErrEntryOutOfRange is a fixed-actor entry outside its claimed range.
	ErrEntryOutOfRange = errors.New("provenance: fixed actor entry out of claimed range")
)
