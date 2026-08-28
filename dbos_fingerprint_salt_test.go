package provenance

// dbos_fingerprint_salt_test.go pins the durable identity that
// dbosPinnedLibraryConst keys, byte for byte.
//
// The constant is a FROZEN salt, not a version claim. It is hashed into every
// durable workflow ID and step name this module has ever written, so changing
// it re-keys the whole durable namespace and orphans every in-flight workflow.
// A dependency upgrade must therefore leave it alone. Pinned digests below make
// that impossible to break quietly: a change to the constant, to the captured
// contract, or to the hashing order fails this file before it can reach a
// durable database.

import (
	"testing"
)

// The exact literal the production const block must carry. Do not update it
// when the DBOS dependency moves.
const goldenPinnedLibrarySalt = "github.com/dbos-inc/dbos-transact-golang v0.20.0"

func TestDBOSPinnedLibrarySaltIsFrozen(t *testing.T) {
	t.Parallel()
	if dbosPinnedLibraryConst != goldenPinnedLibrarySalt {
		t.Fatalf("dbosPinnedLibraryConst = %q, want the frozen salt %q.\n"+
			"This constant keys every durable workflow ID and step name already written. "+
			"Changing it orphans in-flight workflows instead of describing a new library. "+
			"If a dependency upgrade prompted this edit, revert the constant and change only its comment.",
			dbosPinnedLibraryConst, goldenPinnedLibrarySalt)
	}
	if newDBOSContractSnapshot().pinnedLibrary != goldenPinnedLibrarySalt {
		t.Fatalf("the captured contract snapshot carries pinnedLibrary %q, want %q",
			newDBOSContractSnapshot().pinnedLibrary, goldenPinnedLibrarySalt)
	}
}

// goldenWorkflowIdentity pins workflowIdentityForKind, which the corpus in
// testdata/contract does not cover: the corpus pins fingerprint only.
type goldenWorkflowIdentityCase struct {
	name               string
	applicationVersion string
	operation          OperationID
	kind               dbosOperationKind
	want               string
}

func goldenWorkflowIdentityCases() []goldenWorkflowIdentityCase {
	return []goldenWorkflowIdentityCase{
		{
			name:               "apply-kind-empty-version",
			applicationVersion: "",
			operation:          "golden-operation",
			kind:               dbosOperationKindApply,
			want:               "bfd34957edaf23ac58401d5c3e7bd1c093e4e962e9c481c5d0d53d88eefd39b3",
		},
		{
			name:               "apply-kind-pinned-version",
			applicationVersion: "golden-app-version",
			operation:          "golden-operation",
			kind:               dbosOperationKindApply,
			want:               "98423ca07108258cc31b69b7d2909af7891c9b2996407113bd227dc50d409a5b",
		},
		{
			name:               "assignment-transfer-kind",
			applicationVersion: "golden-app-version",
			operation:          "golden-operation",
			kind:               dbosOperationKindAssignmentTransfer,
			want:               "bbbfe9a25058ab7013e8e8a91a2cca751f95e823303629243d91c820b9e9c356",
		},
	}
}

func TestDBOSWorkflowIdentityGoldenDigests(t *testing.T) {
	t.Parallel()
	contract := newDBOSContractSnapshot()
	for _, testCase := range goldenWorkflowIdentityCases() {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := workflowIdentityForKind(contract, testCase.applicationVersion, testCase.operation, testCase.kind)
			if got != testCase.want {
				t.Fatalf("workflowIdentityForKind = %s, want the pinned digest %s.\n"+
					"The durable workflow namespace moved. Every workflow already written under the old digest "+
					"becomes unreachable. Restore the frozen salt, the captured contract, and the hashing order.",
					got, testCase.want)
			}
		})
	}
}

// The generic apply kind must keep the identity it had before typed extensions
// existed, so a generic workflow attaches to its own durable row.
func TestDBOSApplyKindAddsNoKindDiscriminator(t *testing.T) {
	t.Parallel()
	contract := newDBOSContractSnapshot()
	withKind := workflowIdentityForKind(contract, "golden-app-version", "golden-operation", dbosOperationKindApply)
	plain := workflowIdentity(contract, "golden-app-version", "golden-operation")
	if withKind != plain {
		t.Fatalf("workflowIdentityForKind(apply)=%s but workflowIdentity=%s: the generic durable identity moved", withKind, plain)
	}
}

// Anti-vacuity: the pinned digests must actually depend on the salt. Without
// this, a build that dropped the salt from the hash would still pass above.
func TestGoldenWorkflowIdentityDigestsDependOnTheSalt(t *testing.T) {
	t.Parallel()
	salted := newDBOSContractSnapshot()
	salted.pinnedLibrary += "-perturbed"
	for _, testCase := range goldenWorkflowIdentityCases() {
		got := workflowIdentityForKind(salted, testCase.applicationVersion, testCase.operation, testCase.kind)
		if got == testCase.want {
			t.Errorf("case %s: a perturbed salt produced the pinned digest %s; the salt is not hashed in, so these goldens prove nothing",
				testCase.name, testCase.want)
		}
	}
}
