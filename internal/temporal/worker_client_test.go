package temporal

import (
	"testing"

	"go.temporal.io/sdk/workflow"
)

// TestWorkflowVersionMarkersAreClosed checks the version-marker scalars that
// the safe data converter admits to workflow history. The change IDs are
// literal on purpose: a rename of an admitted marker fails this test.
func TestWorkflowVersionMarkersAreClosed(t *testing.T) {
	dataConverter := newSafeWorkflowDataConverter()
	for _, changeID := range []string{"run-deadline-projection", "run-unit-failure-end"} {
		if _, err := dataConverter.ToPayload(changeID); err != nil {
			t.Fatalf("version marker change %q refused: %v", changeID, err)
		}
	}
	for _, refused := range []string{"", "run-unit-failure-end-v2", "pm.execute-unit.v1", "arbitrary text"} {
		if _, err := dataConverter.ToPayload(refused); err == nil {
			t.Fatalf("version marker change %q admitted; want a refusal", refused)
		}
	}
	if _, err := dataConverter.ToPayload(workflow.Version(1)); err != nil {
		t.Fatalf("version marker version 1 refused: %v", err)
	}
	for _, refused := range []workflow.Version{workflow.DefaultVersion, 0, 2} {
		if _, err := dataConverter.ToPayload(refused); err == nil {
			t.Fatalf("version marker version %d admitted; want a refusal", refused)
		}
	}
}
