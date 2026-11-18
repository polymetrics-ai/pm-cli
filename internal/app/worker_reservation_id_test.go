package app

import (
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// This check needs no dependency stack. The expected IDs were computed with
// the identity structs that reserveWorkerResult and reserveWorkerContinuation
// used before the shared helper existed. A changed ID would orphan every
// reservation that an earlier attempt made under the old ID.
func TestWorkerAttemptReservationIDIsStable(t *testing.T) {
	scope := contract.Scope{OrgID: "org-golden", WorkspaceID: "workspace-golden"}
	const digest = "cbeeabae06947fe40b8a5e735a084bef1853c12d5dcd222e21e2534f5ce711e9"
	for prefix, want := range map[string]string{
		workerResultReservationPrefix:       "worker-result-v1-" + digest,
		workerContinuationReservationPrefix: "worker-continuation-v1-" + digest,
	} {
		got, err := workerAttemptReservationID(
			prefix, scope, "run-golden", "task-golden", "dispatch-golden", "task-owner-v1-golden", 7,
		)
		if err != nil || got != want {
			t.Fatalf("reservation ID for %q = %q error=%v; want %q", prefix, got, err, want)
		}
	}
	other, err := workerAttemptReservationID(
		workerResultReservationPrefix, scope, "run-golden", "task-golden", "dispatch-golden", "task-owner-v1-golden", 8,
	)
	if err != nil || other == "worker-result-v1-"+digest {
		t.Fatalf("reservation ID for the next fence = %q error=%v; want a different ID", other, err)
	}
}
