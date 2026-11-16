//go:build linux

package stack_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	pmtemporal "github.com/polymetrics-ai/pm-cli/internal/temporal"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// The limits of the work test. Each wait has one limit, and nothing repeats.
const (
	readyWorkerBuildLimit   = 10 * time.Minute
	readyWorkerWorkLimit    = 30 * time.Second
	readyWorkerRequestLimit = 30 * time.Second
	readyWorkerExitLimit    = 45 * time.Second
	readyWorkerKillLimit    = 10 * time.Second
	readyWorkerReadLimit    = 10 * time.Second
	readyWorkerOutputLimit  = 64 << 10
	readyWorkerWriteFailure = "pm-worker: the ready signal write failed. The process continues."
)

// TestWorkerReadyWriteRunsApartFromTheWork is fact (b) of the ready signal
// (#72) with the real pm-worker binary. It is a Go developer test, not
// acceptance proof. The ready pipe is full before the start, so the ready
// write cannot complete. While it cannot complete, pm-worker completes one
// workflow task and then exits with code 0 after SIGTERM. The test reads
// nothing from the ready pipe before the exit.
//
// Defects: a ready write that blocks the worker, so that no workflow task
// completes (wait 1); a ready write that blocks the stop, so that pm-worker
// does not exit after SIGTERM (wait 3); a ready write that gives up while the
// pipe stays full (its write-failure line on stderr).
//
// The unit of work is one workflow task. The workflow input is valid, but the
// Temporal workflow ID is not the admitted workflow_id, so the workflow fails
// at its execution identity check before it schedules an activity. So no
// PostgreSQL row is needed and no provider effect happens.
//
// Isolation: every pm-worker polls pm-control in the same namespace. This test
// does not call t.Parallel, and no other test in this package starts a
// Temporal worker. If another worker takes the task, the identity check of
// wait 2 fails.
func TestWorkerReadyWriteRunsApartFromTheWork(t *testing.T) {
	artifact := testfixture.NewWorkerConfiguration(t)
	configuration := artifact.Configuration
	namespace := configuration.Temporal().Namespace
	secrets := os.Getenv("PM_STACK_SECRETS_DIR")
	if secrets == "" {
		t.Fatal("PM_STACK_SECRETS_DIR is required for the selected worker fixture")
	}
	directory := t.TempDir()
	deploymentID := "pm-ready-test-" + readyTestToken(t, 6)
	configurationPath := writeReadyWorkerConfiguration(t, artifact.Path, directory, deploymentID)
	binary := buildReadyWorker(t, directory)

	readEnd, writeEnd, filler := fullReadyPipe(t)
	stdout := createReadyWorkerOutput(t, directory, "pm-worker.stdout")
	stderr := createReadyWorkerOutput(t, directory, "pm-worker.stderr")
	command := exec.Command(binary,
		"--config", configurationPath,
		"--secrets-directory", secrets,
		"--ready-fd", "3",
	)
	command.ExtraFiles = []*os.File{writeEnd}
	command.Stdout = stdout
	command.Stderr = stderr
	command.Dir = directory
	command.Env = []string{"HOME=" + directory, "PATH=/usr/bin:/bin", "LANG=C", "TMPDIR=" + directory}
	if err := command.Start(); err != nil {
		t.Fatalf("start pm-worker: %v", err)
	}
	// The starter closes its own copy of the write end after the start.
	if err := writeEnd.Close(); err != nil {
		t.Fatalf("close the starter copy of the ready write end: %v", err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
			return
		default:
		}
		_ = command.Process.Kill()
		select {
		case <-exited:
		case <-time.After(readyWorkerKillLimit):
			t.Errorf("pm-worker (process %d) did not exit within %s after SIGKILL", command.Process.Pid, readyWorkerKillLimit)
		}
	})

	// The test client uses the converters and the client interceptor of
	// pm-worker.
	dialTimeout := configuration.Worker().DialTimeout
	dialContext, cancelDial := context.WithTimeout(context.Background(), dialTimeout)
	client, err := pmtemporal.DialWorkerClient(dialContext,
		testfixture.TemporalOptions(t, configuration, namespace, "pm-ready-test-client"), dialTimeout)
	cancelDial()
	if err != nil {
		t.Fatalf("dial the test Temporal client: %v", err)
	}
	t.Cleanup(client.Close)

	token := readyTestToken(t, 8)
	runID := "ready-test-run-" + token
	digest := make([]byte, 32)
	if _, err := rand.Read(digest); err != nil {
		t.Fatal(err)
	}
	input := contract.StartRunInput{
		Schema:       contract.StartRunInputSchema,
		Scope:        contract.Scope{OrgID: "ready-test-org", WorkspaceID: "ready-test-workspace"},
		RunID:        runID,
		CommandID:    "ready-test-command-" + token,
		WorkflowID:   "pm/run/" + runID,
		Generation:   1,
		Origin:       contract.RunOriginAPI,
		IntentDigest: contract.SemanticDigest("sha256:" + hex.EncodeToString(digest)),
	}
	workflowID := "pm-ready-test/" + token
	startContext, cancelStart := context.WithTimeout(context.Background(), readyWorkerRequestLimit)
	run, err := client.ExecuteWorkflow(startContext, sdkclient.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: contract.ControlTaskQueue,
	}, contract.RunWorkflowType, input)
	cancelStart()
	if err != nil {
		t.Fatalf("start the test workflow: %v", err)
	}
	testfixture.CleanupWorkflowChain(t, client, namespace, workflowID, run.GetRunID())

	// Wait 1: one Get on the workflow run.
	getContext, cancelGet := context.WithTimeout(context.Background(), readyWorkerWorkLimit)
	err = run.Get(getContext, nil)
	cancelGet()
	var applicationErr *temporal.ApplicationError
	if !errors.As(err, &applicationErr) ||
		applicationErr.Message() != "workflow application failure" ||
		applicationErr.Type() != "WorkflowApplicationFailure" {
		t.Fatalf("wait 1: no failed workflow task within %s while the ready write cannot complete: %v\npm-worker stderr:\n%s",
			readyWorkerWorkLimit, err, readyWorkerOutputTail(t, stderr))
	}

	// Wait 2: one request for the workflow history.
	historyContext, cancelHistory := context.WithTimeout(context.Background(), readyWorkerRequestLimit)
	history, err := client.WorkflowService().GetWorkflowExecutionHistory(historyContext,
		&workflowservice.GetWorkflowExecutionHistoryRequest{
			Namespace:       namespace,
			Execution:       &commonpb.WorkflowExecution{WorkflowId: workflowID, RunId: run.GetRunID()},
			MaximumPageSize: 64,
		})
	cancelHistory()
	if err != nil {
		t.Fatalf("wait 2: read the workflow history: %v", err)
	}
	events := history.GetHistory().GetEvents()
	wantIdentity := deploymentID + "/pm-worker"
	completed := 0
	for _, event := range events {
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_TASK_COMPLETED:
			if identity := event.GetWorkflowTaskCompletedEventAttributes().GetIdentity(); identity != wantIdentity {
				t.Fatalf("wait 2: a workflow task completed with identity %q, want %q", identity, wantIdentity)
			}
			completed++
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			t.Fatalf("wait 2: the history holds a scheduled activity task (event %d)", event.GetEventId())
		}
	}
	if completed == 0 {
		t.Fatalf("wait 2: no completed workflow task in the history of %d events", len(events))
	}
	if len(events) == 0 || events[len(events)-1].GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED {
		t.Fatalf("wait 2: the history of %d events does not end with the failure of the workflow", len(events))
	}

	// Wait 3: SIGTERM, then one wait for the exit, while the ready pipe is
	// still full and unread.
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("wait 3: send SIGTERM to pm-worker: %v\npm-worker stderr:\n%s", err, readyWorkerOutputTail(t, stderr))
	}
	select {
	case <-exited:
	case <-time.After(readyWorkerExitLimit):
		t.Fatalf("wait 3: pm-worker did not exit within %s after SIGTERM while its ready write cannot complete\npm-worker stderr:\n%s",
			readyWorkerExitLimit, readyWorkerOutputTail(t, stderr))
	}
	if waitErr != nil {
		t.Fatalf("wait 3: pm-worker exit after SIGTERM: %v, want exit code 0\npm-worker stderr:\n%s",
			waitErr, readyWorkerOutputTail(t, stderr))
	}

	// Wait 4: one read of the ready pipe until end of file. The ready write
	// never completed, so the pipe holds exactly the filler bytes.
	if err := readEnd.SetReadDeadline(time.Now().Add(readyWorkerReadLimit)); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(readEnd)
	if err != nil {
		t.Fatalf("wait 4: read the ready pipe until end of file: %v", err)
	}
	if !bytes.Equal(data, filler) {
		t.Fatalf("wait 4: the ready pipe held %d bytes, want exactly the %d filler bytes; tail %q",
			len(data), len(filler), data[max(0, len(data)-32):])
	}

	// A ready write that gave up while the pipe stayed full prints its
	// write-failure line.
	if output := readyWorkerOutputTail(t, stderr); bytes.Contains(output, []byte(readyWorkerWriteFailure)) {
		t.Fatalf("pm-worker stderr holds the write-failure line: the ready write gave up while the pipe was full\n%s", output)
	}
}

// fullReadyPipe makes the ready pipe and fills it to its capacity with one
// write. The capacity is one page, set with F_SETPIPE_SZ (Linux).
func fullReadyPipe(t *testing.T) (*os.File, *os.File, []byte) {
	t.Helper()
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("make the ready pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = readEnd.Close()
		_ = writeEnd.Close()
	})
	capacity, _, errno := syscall.Syscall(syscall.SYS_FCNTL, writeEnd.Fd(), syscall.F_SETPIPE_SZ, uintptr(os.Getpagesize()))
	if errno != 0 {
		t.Fatalf("set the ready pipe capacity: %v", errno)
	}
	filler := bytes.Repeat([]byte{'f'}, int(capacity))
	written, err := writeEnd.Write(filler)
	if err != nil || written != len(filler) {
		t.Fatalf("fill the ready pipe: wrote %d of %d bytes: %v", written, len(filler), err)
	}
	return readEnd, writeEnd, filler
}

// writeReadyWorkerConfiguration writes the worker configuration of the stack
// fixture with a new deployment ID. The new deployment ID changes only the
// worker identity, not the task queue.
func writeReadyWorkerConfiguration(t *testing.T, fixturePath, directory, deploymentID string) string {
	t.Helper()
	contents, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read the fixture worker configuration: %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode the fixture worker configuration: %v", err)
	}
	if document["deployment_id"], err = json.Marshal(deploymentID); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "pm-worker.json")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write the test worker configuration: %v", err)
	}
	return path
}

// buildReadyWorker builds pm-worker once, with one deadline. The build output
// goes to a file, so the deadline also ends the wait for the output.
func buildReadyWorker(t *testing.T, directory string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find the go tool: %v", err)
	}
	output := filepath.Join(directory, "pm-worker")
	log := createReadyWorkerOutput(t, directory, "pm-worker-build.log")
	ctx, cancel := context.WithTimeout(context.Background(), readyWorkerBuildLimit)
	defer cancel()
	build := exec.CommandContext(ctx, goTool, "build", "-o", output, "./cmd/pm-worker")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.Stdout = log
	build.Stderr = log
	if err := build.Run(); err != nil {
		t.Fatalf("build pm-worker within %s: %v (deadline: %v)\n%s", readyWorkerBuildLimit, err, ctx.Err(), readyWorkerOutputTail(t, log))
	}
	return output
}

func createReadyWorkerOutput(t *testing.T, directory, name string) *os.File {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(directory, name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// readyWorkerOutputTail reads the last bytes of an output file, up to the
// output limit.
func readyWorkerOutputTail(t *testing.T, file *os.File) []byte {
	t.Helper()
	info, err := file.Stat()
	if err != nil {
		return []byte("(stat the output: " + err.Error() + ")")
	}
	offset := max(0, info.Size()-readyWorkerOutputLimit)
	contents, err := io.ReadAll(io.NewSectionReader(file, offset, info.Size()-offset))
	if err != nil {
		return []byte("(read the output: " + err.Error() + ")")
	}
	return contents
}

func readyTestToken(t *testing.T, size int) string {
	t.Helper()
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(value)
}
