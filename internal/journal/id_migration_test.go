package journal

import (
	"bytes"
	"crypto/sha256"
	"os"
	"testing"
)

func TestRetiredCanonicalConversionUsesOwnerAndIndependentFixture(t *testing.T) {
	old, err := os.ReadFile("../../testdata/contract/mutation_v2_assignment_retired.bin")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(old)
	if _, err := DecodeCanonicalMutation(old); err == nil {
		t.Fatal("runtime decoder accepted retired typed identities")
	}
	converted, err := ConvertRetiredCanonicalMutation(old, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../../testdata/contract/mutation_v2_assignment.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(converted.CanonicalBytes(), want) {
		t.Fatalf("owner conversion differs from independent slash fixture: %q", converted.CanonicalBytes())
	}
	wantDigest := sha256.Sum256(want)
	if !bytes.Equal(converted.DerivedDigest(), wantDigest[:]) {
		t.Fatal("dependent digest not converted")
	}
	if _, err := DecodeCanonicalMutation(converted.CanonicalBytes()); err != nil {
		t.Fatal(err)
	}
	// Independent canonical semantic-byte and digest-only corruptions both fail
	// before a converted result can be returned; expected digests are fixture pins.
	corrupt := bytes.Replace(old, []byte(`"action"`), []byte(`"change"`), 1)
	if _, err := ConvertRetiredCanonicalMutation(corrupt, digest[:]); err == nil {
		t.Fatal("semantic byte corruption accepted")
	}
	badDigest := append([]byte(nil), digest[:]...)
	badDigest[0] ^= 1
	if _, err := ConvertRetiredCanonicalMutation(old, badDigest); err == nil {
		t.Fatal("digest-only corruption accepted")
	}
}

func TestRetiredCanonicalConversionPreservesOpaqueAuthority(t *testing.T) {
	old, err := os.ReadFile("../../testdata/contract/mutation_v1_v004.bin")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(old)
	converted, err := ConvertRetiredCanonicalMutation(old, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old, converted.CanonicalBytes()) || !bytes.Equal(digest[:], converted.DerivedDigest()) {
		t.Fatal("opaque retired-looking authority token changed")
	}
}
