package journal

import (
	"errors"
	"testing"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

func mkTask(t *testing.T) TaskID {
	t.Helper()
	return TaskID{Namespace: "provenance-test", UUID: uuid.New()}
}

func mkActor(t *testing.T) ActorID {
	t.Helper()
	return ActorID{Namespace: "provenance-test", UUID: uuid.New()}
}

func TestJournalKindClosedSetRoundTrips(t *testing.T) {
	kinds := JournalKinds()
	if len(kinds) != 5 {
		t.Fatalf("expected 5 journal kinds, got %d", len(kinds))
	}
	for _, k := range kinds {
		if !k.IsValid() {
			t.Errorf("kind %s reports invalid", k)
		}
		parsed, err := ParseJournalKind(k.String())
		if err != nil || parsed != k {
			t.Errorf("ParseJournalKind(%q) = %v, %v; want %v", k.String(), parsed, err, k)
		}
		if _, err := k.SubtypeTable(); err != nil {
			t.Errorf("SubtypeTable(%s): %v", k, err)
		}
	}
	if _, err := ParseJournalKind("nonsense"); err == nil {
		t.Error("ParseJournalKind accepted an unknown name")
	}
}

func TestCanonicalEventContextsSortsAndDedups(t *testing.T) {
	a := mkTask(t)
	b := mkActor(t)
	ctxA, err := TaskContext(a)
	if err != nil {
		t.Fatal(err)
	}
	ctxB, err := ActorContext(b)
	if err != nil {
		t.Fatal(err)
	}
	// Duplicate ctxA twice, out of order; expect dedup and (kind, identity) sort.
	got, err := CanonicalEventContexts([]EventContext{ctxA, ctxB, ctxA})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 canonical contexts, got %d", len(got))
	}
	// "actor" < "task" lexically, so ctxB (actor) sorts first.
	if got[0].Kind() != EventContextKindActor || got[1].Kind() != EventContextKindTask {
		t.Errorf("sort order = [%s %s], want [actor task]", got[0].Kind(), got[1].Kind())
	}
}

func TestCanonicalEventContextsRejectsInvalid(t *testing.T) {
	bad, err := DecodeStoredEventContext(EventContextKindGit, "not-a-git-oid")
	if err == nil {
		_ = bad
		t.Fatal("expected invalid git OID to be rejected at decode")
	}
}

func TestExtensionContextRejectsReservedNamespace(t *testing.T) {
	if err := validateExtensionContextKind("provenance.custom"); err == nil {
		t.Error("reserved provenance namespace must be rejected for extension contexts")
	}
	if err := validateExtensionContextKind("task"); err == nil {
		t.Error("built-in kind must be rejected as an extension kind")
	}
	if err := validateExtensionContextKind("pasture.review"); err != nil {
		t.Errorf("valid caller-extension kind rejected: %v", err)
	}
}

func TestValidateEventKindNamespaced(t *testing.T) {
	if err := ValidateEventKind("provenance.task.created"); err != nil {
		t.Errorf("valid event kind rejected: %v", err)
	}
	if err := ValidateEventKind("nope"); !errors.Is(err, ErrInvalidEventKind) {
		t.Errorf("unnamespaced kind: got %v, want ErrInvalidEventKind", err)
	}
}

func TestJournalQueryValidateRejectsNonJournalIDOrder(t *testing.T) {
	q := JournalQueryV1{OrderBy: OrderDimension(99)}
	if err := q.Validate(); !errors.Is(err, ErrUnsupportedOrderDimension) {
		t.Errorf("got %v, want ErrUnsupportedOrderDimension", err)
	}
	if err := (JournalQueryV1{OrderBy: OrderByJournalID}).Validate(); err != nil {
		t.Errorf("JournalID order rejected: %v", err)
	}
	if err := (JournalQueryV1{OrderBy: OrderByJournalID, Limit: -1}).Validate(); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("negative limit: got %v, want ErrInvalidQuery", err)
	}
}

// TestOrderDimensionExposesTimelineAndCanonical proves the display-vs-canonical
// firewall at the type layer: both the readable-timeline display order and the
// canonical order are exposed and valid, the zero value defaults to the timeline
// (readable-by-default for displays), and any other dimension is rejected.
func TestOrderDimensionExposesTimelineAndCanonical(t *testing.T) {
	var zero OrderDimension
	if zero != OrderByRecordedAt {
		t.Errorf("zero-value OrderDimension = %v, want OrderByRecordedAt (the display default)", zero)
	}
	for _, d := range []OrderDimension{OrderByRecordedAt, OrderByJournalID} {
		if !d.IsValid() {
			t.Errorf("%v should be a valid, exposed order dimension", d)
		}
		if err := (JournalQueryV1{OrderBy: d}).Validate(); err != nil {
			t.Errorf("exposed dimension %v rejected by Validate: %v", d, err)
		}
	}
	if OrderByRecordedAt.String() != "recorded_at" {
		t.Errorf("OrderByRecordedAt.String() = %q, want recorded_at", OrderByRecordedAt.String())
	}
	if OrderByJournalID.String() != "journal_id" {
		t.Errorf("OrderByJournalID.String() = %q, want journal_id", OrderByJournalID.String())
	}
	if OrderByRecordedAt == OrderByJournalID {
		t.Fatal("the display and canonical dimensions must be distinct values")
	}
}

func TestUUIDRangeOverlapAndContains(t *testing.T) {
	a := UUIDRange{Min: BigEndianUUID(0), Max: BigEndianUUID(1023)}
	disjoint := UUIDRange{Min: BigEndianUUID(1024), Max: BigEndianUUID(2047)}
	overlapping := UUIDRange{Min: BigEndianUUID(512), Max: BigEndianUUID(1535)}

	if a.Overlaps(disjoint) || disjoint.Overlaps(a) {
		t.Error("disjoint ranges reported as overlapping")
	}
	if !a.Overlaps(overlapping) {
		t.Error("intersecting ranges reported as disjoint")
	}
	if !a.Contains(BigEndianUUID(1023)) || a.Contains(BigEndianUUID(1024)) {
		t.Error("Contains boundary handling incorrect")
	}
	if got := a.Size().Int64(); got != 1024 {
		t.Errorf("range size = %d, want 1024", got)
	}
}

func TestOrdinalCodecRoundTripAndBounds(t *testing.T) {
	rng := UUIDRange{Min: BigEndianUUID(0), Max: BigEndianUUID(1023)}
	codec := OrdinalV1Codec{}
	for _, ord := range []uint64{0, 1, 1023} {
		enc, err := codec.Encode(rng, ord)
		if err != nil {
			t.Fatalf("encode %d: %v", ord, err)
		}
		dec, err := codec.Decode(rng, enc)
		if err != nil || dec != ord {
			t.Errorf("round trip ord %d: dec=%d err=%v", ord, dec, err)
		}
	}
	if _, err := codec.Encode(rng, 1024); !errors.Is(err, ErrEntryOutOfRange) {
		t.Errorf("encoding past Max: got %v, want ErrEntryOutOfRange", err)
	}
}

func TestCheckNoOverlapNamesBothNamespaces(t *testing.T) {
	existing := []ActorNamespaceClaim{{
		Namespace: "pasture-system", ClaimantID: "pasture-system",
		Range: UUIDRange{Min: BigEndianUUID(0), Max: BigEndianUUID(1023)}, Codec: OrdinalV1CodecName,
	}}
	newClaim := ActorNamespaceClaim{
		Namespace: "pasture-intruder", ClaimantID: "pasture-intruder",
		Range: UUIDRange{Min: BigEndianUUID(512), Max: BigEndianUUID(1535)}, Codec: OrdinalV1CodecName,
	}
	err := CheckNoOverlap(newClaim, existing)
	if !errors.Is(err, ErrNamespaceRange) {
		t.Fatalf("got %v, want ErrNamespaceRange", err)
	}
	msg := err.Error()
	for _, ns := range []string{"pasture-system", "pasture-intruder"} {
		if !contains(msg, ns) {
			t.Errorf("overlap error must name %q; got %q", ns, msg)
		}
	}
	// A re-registration of the same namespace is not an overlap with itself.
	same := existing[0]
	if err := CheckNoOverlap(same, existing); err != nil {
		t.Errorf("same-namespace re-registration reported overlap: %v", err)
	}
}

func TestCheckEntryInRange(t *testing.T) {
	claim := ActorNamespaceClaim{
		Namespace: "pasture-system", ClaimantID: "pasture-system",
		Range: UUIDRange{Min: BigEndianUUID(0), Max: BigEndianUUID(1023)}, Codec: OrdinalV1CodecName,
	}
	if err := CheckEntryInRange(claim, "pasture-system", BigEndianUUID(0)); err != nil {
		t.Errorf("in-range entry rejected: %v", err)
	}
	if err := CheckEntryInRange(claim, "pasture-system", BigEndianUUID(4096)); !errors.Is(err, ErrEntryOutOfRange) {
		t.Errorf("out-of-range entry: got %v, want ErrEntryOutOfRange", err)
	}
	if err := CheckEntryInRange(claim, "other-namespace", BigEndianUUID(0)); !errors.Is(err, ErrEntryOutOfRange) {
		t.Errorf("mismatched namespace: got %v, want ErrEntryOutOfRange", err)
	}
}

func TestActorIDIsAgentIDAlias(t *testing.T) {
	// Deprecated AgentID must be the identical type, not a second identity.
	var a ptypes.ActorID
	var b ptypes.AgentID = a // compiles only if alias
	_ = b
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return len(needle) == 0
}
