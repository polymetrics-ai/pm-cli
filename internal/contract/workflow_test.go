package contract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWorkflowUnitRefPreservesQualifiedAndInitialMeaning(t *testing.T) {
	t.Run("retained unqualified history stays unqualified", func(t *testing.T) {
		var reference WorkflowUnitRef
		if err := json.Unmarshal([]byte(`{"task_id":"task-root"}`), &reference); err != nil {
			t.Fatalf("decode retained unit reference: %v", err)
		}
		if reference.TaskID != "task-root" || reference.DispatchID != "" {
			t.Fatalf("retained unit reference = %#v; want the original unqualified root", reference)
		}
		encoded, err := json.Marshal(reference)
		if err != nil {
			t.Fatalf("encode retained unit reference: %v", err)
		}
		if strings.Contains(string(encoded), "dispatch_id") {
			t.Fatalf("retained unit reference invented retry qualification: %s", encoded)
		}
	})

	t.Run("qualified history retains the exact dispatch", func(t *testing.T) {
		want := WorkflowUnitRef{TaskID: "task-retry", DispatchID: "dispatch-attempt-2"}
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("encode qualified unit reference: %v", err)
		}
		var decoded WorkflowUnitRef
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("decode qualified unit reference: %v", err)
		}
		if decoded != want {
			t.Fatalf("qualified unit reference = %#v; want %#v", decoded, want)
		}
	})

	for _, document := range []string{
		`{"task_id":"task-retry","dispatch_id":""}`,
		`{"task_id":"task-retry","dispatch_id":null}`,
	} {
		t.Run(document, func(t *testing.T) {
			var reference WorkflowUnitRef
			if err := json.Unmarshal([]byte(document), &reference); err == nil {
				t.Fatalf("accepted malformed qualified unit reference: %s", document)
			}
		})
	}
}

func TestWorkflowRunSnapshotBoundsRetryEligibility(t *testing.T) {
	t.Run("qualified retry eligibility round trips", func(t *testing.T) {
		snapshot := workflowSnapshotForTest()
		retryNotBefore := snapshot.Deadline.Add(-time.Minute)
		snapshot.RetryNotBefore = &retryNotBefore
		if err := snapshot.Validate(); err != nil {
			t.Fatalf("validate retry snapshot: %v", err)
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatalf("encode retry snapshot: %v", err)
		}
		var decoded WorkflowRunSnapshot
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("decode retry snapshot: %v", err)
		}
		if decoded.RetryNotBefore == nil || !decoded.RetryNotBefore.Equal(retryNotBefore) {
			t.Fatalf("retry eligibility = %v; want %s", decoded.RetryNotBefore, retryNotBefore)
		}
	})

	t.Run("retained snapshot omits retry eligibility", func(t *testing.T) {
		snapshot := workflowSnapshotForTest()
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatalf("encode retained snapshot: %v", err)
		}
		if strings.Contains(string(encoded), "retry_not_before") {
			t.Fatalf("retained snapshot invented retry eligibility: %s", encoded)
		}
		var decoded WorkflowRunSnapshot
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("decode retained snapshot: %v", err)
		}
		if decoded.RetryNotBefore != nil {
			t.Fatalf("retained snapshot retry eligibility = %s; want absent", decoded.RetryNotBefore)
		}
	})

	t.Run("retry eligibility cannot exceed the original deadline", func(t *testing.T) {
		snapshot := workflowSnapshotForTest()
		retryNotBefore := snapshot.Deadline.Add(time.Nanosecond)
		snapshot.RetryNotBefore = &retryNotBefore
		if err := snapshot.Validate(); err == nil {
			t.Fatal("snapshot accepted retry eligibility after its original deadline")
		}
	})

	t.Run("retry eligibility must use canonical UTC", func(t *testing.T) {
		snapshot := workflowSnapshotForTest()
		retryNotBefore := snapshot.Deadline.Add(-time.Minute).In(time.FixedZone("offset", int(time.Hour/time.Second)))
		snapshot.RetryNotBefore = &retryNotBefore
		if err := snapshot.Validate(); err == nil {
			t.Fatal("snapshot accepted non-UTC retry eligibility")
		}
	})

	t.Run("runnable work and retry waiting are mutually exclusive", func(t *testing.T) {
		snapshot := workflowSnapshotForTest()
		retryNotBefore := snapshot.Deadline.Add(-time.Minute)
		snapshot.RetryNotBefore = &retryNotBefore
		snapshot.Units = []WorkflowUnitRef{{TaskID: "task-retry", DispatchID: "dispatch-attempt-2"}}
		if err := snapshot.Validate(); err == nil {
			t.Fatal("snapshot exposed qualified work before its retry wait was cleared")
		}
	})

	t.Run("completed coordination cannot retain a retry timer", func(t *testing.T) {
		snapshot := workflowSnapshotForTest()
		retryNotBefore := snapshot.Deadline.Add(-time.Minute)
		snapshot.RetryNotBefore = &retryNotBefore
		snapshot.Done = true
		if err := snapshot.Validate(); err == nil {
			t.Fatal("completed snapshot retained retry eligibility")
		}
	})

	t.Run("duplicate qualified work is refused", func(t *testing.T) {
		for _, units := range [][]WorkflowUnitRef{
			{
				{TaskID: "task-retry", DispatchID: "dispatch-attempt-2"},
				{TaskID: "task-retry", DispatchID: "dispatch-attempt-3"},
			},
			{
				{TaskID: "task-retry-a", DispatchID: "dispatch-attempt-2"},
				{TaskID: "task-retry-b", DispatchID: "dispatch-attempt-2"},
			},
		} {
			snapshot := workflowSnapshotForTest()
			snapshot.Units = units
			if err := snapshot.Validate(); err == nil {
				t.Fatalf("snapshot accepted duplicate qualified work: %#v", units)
			}
		}
	})

	t.Run("retained and qualified units cannot be mixed", func(t *testing.T) {
		snapshot := workflowSnapshotForTest()
		snapshot.Units = []WorkflowUnitRef{
			{TaskID: "task-retained-root"},
			{TaskID: "task-retry", DispatchID: "dispatch-attempt-2"},
		}
		if err := snapshot.Validate(); err == nil {
			t.Fatal("snapshot mixed retained initial and qualified retry meanings")
		}
	})

	t.Run("explicit null cannot erase retry eligibility meaning", func(t *testing.T) {
		encoded, err := json.Marshal(workflowSnapshotForTest())
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		fields["retry_not_before"] = json.RawMessage("null")
		encoded, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		var decoded WorkflowRunSnapshot
		if err := json.Unmarshal(encoded, &decoded); err == nil {
			t.Fatal("snapshot accepted explicit null retry eligibility")
		}
	})
}

func workflowSnapshotForTest() WorkflowRunSnapshot {
	return WorkflowRunSnapshot{
		Run: WorkflowRunRef{
			Scope:          Scope{OrgID: "org-workflow", WorkspaceID: "workspace-workflow"},
			RunID:          "run-workflow",
			WorkflowID:     "pm/run/run-workflow",
			StartCommandID: "command-workflow",
			Generation:     1,
			IntentDigest:   SemanticDigest("sha256:" + strings.Repeat("0", 64)),
		},
		Deadline: time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC),
		Timeouts: ActivityTimeouts{
			ScheduleToClose: 2 * time.Minute,
			StartToClose:    time.Minute,
			Heartbeat:       10 * time.Second,
			Transport:       30 * time.Second,
		},
		Units:    []WorkflowUnitRef{},
		Commands: []WorkflowCommandRef{},
		Fanout:   1,
	}
}
