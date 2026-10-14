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

// ConvertRetiredIdentity is the private offline converter, not a runtime reader.
// It now tolerates a value already in the current slash form (so a
// partly-converted store still converts) while still converting the retired
// two-dash form and still refusing a value that is neither.
func TestRetiredIdentityToleratesSlashFormAndConvertsRetired(t *testing.T) {
	const suffix = "018f0000-0000-7000-8000-000000000002"
	for _, namespace := range []string{"https://github.com/fixture/my--repo/nested", "file:///tmp/my--repo/", "my--project"} {
		if got, err := ConvertRetiredIdentity(namespace + "/" + suffix); err != nil || got != namespace+"/"+suffix {
			t.Fatalf("slash-form identity in opaque namespace %q conversion=%q,%v", namespace, got, err)
		}
		got, err := ConvertRetiredIdentity(namespace + "--" + suffix)
		if err != nil || got != namespace+"/"+suffix {
			t.Fatalf("authentic retired %q conversion=%q,%v", namespace, got, err)
		}
		if _, err := ConvertRetiredIdentity(namespace + "::" + suffix); err == nil {
			t.Fatalf("malformed identity in opaque namespace %q was accepted", namespace)
		}
	}
}
