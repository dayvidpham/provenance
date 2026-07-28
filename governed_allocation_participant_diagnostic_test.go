package provenance

import (
	"errors"
	"strings"
	"testing"
)

func TestSimpleGovernedAllocationParticipantFailureDiagnostic(t *testing.T) {
	t.Parallel()

	cause := errors.New("audit write rejected")
	err := newGovernedAllocationParticipantFailure(
		OperationID("allocation-operation"),
		cause,
		governedAllocationParticipantFailureAfterAllocation,
	)

	wantParts := []string{
		"provenance.FusedGovernedAllocator.allocateWorkflow",
		"after allocation reducer success and before checkpoint",
		"DBOS rolls back governed domain rows, participant writes, and the successful operation_outputs checkpoint",
		cause.Error(),
	}
	for _, want := range wantParts {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("participant failure diagnostic = %q, want it to contain %q", err, want)
		}
	}

	for _, unwanted := range []string{"allocateComposedWorkflow", "supplemental reducer", "supplemental journal rows"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("simple participant failure diagnostic = %q, should not contain composed-stage wording %q", err, unwanted)
		}
	}

	failure, ok := err.(*governedAllocationParticipantFailure)
	if !ok {
		t.Fatalf("participant failure type = %T, want *governedAllocationParticipantFailure", err)
	}
	if failure.cause != cause {
		t.Errorf("participant failure cause = %v, want original cause %v", failure.cause, cause)
	}
}
