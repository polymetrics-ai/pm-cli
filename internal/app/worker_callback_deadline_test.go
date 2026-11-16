package app

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	namespacepb "go.temporal.io/api/namespace/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	sdkclient "go.temporal.io/sdk/client"
	sdkworker "go.temporal.io/sdk/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// TestWorkerCallbackStopsAtTransportDeadline checks when the provider callback
// of one ExecuteUnit attempt stops. It runs the worker heartbeats and the
// provider callback context of ExecuteUnit (startWorkerHeartbeats and
// workerCallbackContext, as worker.go calls them) inside a real Temporal SDK
// worker. An in-process fake frontend gives the worker one attempt with the
// R12 C09 timeouts (20s, 10s, 2s and 5s) and answers its heartbeats. No shared
// Temporal server is used, so no check here depends on how fast such a server
// answers a heartbeat (issue #292).
func TestWorkerCallbackStopsAtTransportDeadline(t *testing.T) {
	t.Run("every heartbeat answered: the transport deadline stops the callback", func(t *testing.T) {
		frontend := startHeartbeatFrontend(t, 0)
		stop := runProviderCallbackAttempt(t, frontend)
		if stop.errAtStart != nil {
			t.Fatalf("provider callback context ended before the callback started: %v", stop.errAtStart)
		}
		if !errors.Is(stop.err, context.DeadlineExceeded) || !errors.Is(stop.cause, context.DeadlineExceeded) {
			t.Fatalf("provider callback stopped with error %v, cause %v; want its own transport deadline", stop.err, stop.cause)
		}
		if stop.stoppedAt.Before(stop.deadline) {
			t.Fatalf("provider callback stopped at %s, before its transport deadline %s", stop.stoppedAt, stop.deadline)
		}
		if stop.elapsed < 4500*time.Millisecond || stop.elapsed >= 10*time.Second {
			t.Fatalf("provider callback stopped after %s; want transport stop before 10s attempt expiry", stop.elapsed)
		}
		if !stop.deadline.Before(stop.attemptDeadline) || !stop.stoppedAt.Before(stop.attemptDeadline) {
			t.Fatalf("provider callback deadline %s and stop %s; want both before the attempt deadline %s",
				stop.deadline, stop.stoppedAt, stop.attemptDeadline)
		}
		t.Logf("provider callback stopped after %s with cause %v", stop.elapsed, stop.cause)
	})

	t.Run("one heartbeat held: the SDK cancels the attempt before the transport deadline", func(t *testing.T) {
		// The fake holds the second heartbeat after the callback starts. That is
		// the third heartbeat of the attempt, as in CI run 43823609721.
		frontend := startHeartbeatFrontend(t, 2)
		stop := runProviderCallbackAttempt(t, frontend)
		select {
		case <-frontend.held:
		default:
			t.Fatal("the provider callback stopped, but the fake frontend held no heartbeat")
		}
		if stop.errAtStart != nil {
			t.Fatalf("provider callback context ended before the callback started: %v", stop.errAtStart)
		}
		if !errors.Is(stop.err, context.Canceled) {
			t.Fatalf("provider callback stopped with error %v; want the canceled activity attempt", stop.err)
		}
		var heartbeatTimeout *serviceerror.DeadlineExceeded
		if !errors.As(stop.cause, &heartbeatTimeout) {
			t.Fatalf("provider callback cause = %T(%v); want the heartbeat RPC timeout", stop.cause, stop.cause)
		}
		if !stop.stoppedAt.Before(stop.deadline) {
			t.Fatalf("provider callback stopped at %s; want a stop before its transport deadline %s", stop.stoppedAt, stop.deadline)
		}
		t.Logf("provider callback stopped after %s with cause %T(%v); the R12 C09 stop-time check before #292 required at least 4.5s",
			stop.elapsed, stop.cause, stop.cause)
	})
}

// providerCallbackStop is what the provider callback saw: its context deadline
// and the attempt deadline when it started, and why and when it stopped.
type providerCallbackStop struct {
	deadline        time.Time
	attemptDeadline time.Time
	errAtStart      error
	stoppedAt       time.Time
	elapsed         time.Duration
	err             error
	cause           error
}

func runProviderCallbackAttempt(t *testing.T, frontend *heartbeatFrontend) providerCallbackStop {
	t.Helper()
	timeouts := contract.ActivityTimeouts{
		ScheduleToClose: 20 * time.Second,
		StartToClose:    10 * time.Second,
		Heartbeat:       2 * time.Second,
		Transport:       5 * time.Second,
	}
	if err := timeouts.Validate(); err != nil {
		t.Fatal(err)
	}
	progress := contract.WorkflowProgress{RunID: "run-292", TaskID: "task-292", ProgressRef: "receipt-292"}
	if err := progress.Validate(); err != nil {
		t.Fatal(err)
	}
	acceptedRunDeadline := time.Now().Add(time.Minute)
	stops := make(chan providerCallbackStop, 1)
	executeUnit := func(ctx context.Context) error {
		// The activity layer gives ExecuteUnit a function that records the
		// heartbeat with the SDK (internal/activities/service.go).
		heartbeat := func(next contract.WorkflowProgress) { activity.RecordHeartbeat(ctx, next) }
		stopHeartbeats := startWorkerHeartbeats(ctx, heartbeat, progress, timeouts.Heartbeat)
		defer stopHeartbeats()
		callbackCtx, cancel := workerCallbackContext(ctx, acceptedRunDeadline, timeouts.Heartbeat, timeouts.Transport)
		defer cancel()

		// The provider callback, with the body that R12 C09 used before #292.
		started := time.Now()
		deadline, ok := callbackCtx.Deadline()
		if !ok {
			return errors.New("provider transport context has no deadline")
		}
		stop := providerCallbackStop{
			deadline:        deadline,
			attemptDeadline: activity.GetInfo(callbackCtx).Deadline,
			errAtStart:      callbackCtx.Err(),
		}
		close(frontend.callbackStarted)
		<-callbackCtx.Done()
		stop.stoppedAt = time.Now()
		stop.elapsed = time.Since(started)
		stop.err = callbackCtx.Err()
		stop.cause = context.Cause(callbackCtx)
		stops <- stop
		return callbackCtx.Err()
	}

	client, err := sdkclient.Dial(sdkclient.Options{
		HostPort:  "passthrough:///pm-heartbeat-frontend",
		Namespace: heartbeatFrontendNamespace,
		Identity:  "pm-worker-callback-deadline-test",
		ConnectionOptions: sdkclient.ConnectionOptions{
			DialOptions: []grpc.DialOption{
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
					return frontend.listener.DialContext(ctx)
				}),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	worker := sdkworker.New(client, contract.ControlTaskQueue, sdkworker.Options{DisableWorkflowWorker: true})
	worker.RegisterActivityWithOptions(executeUnit, activity.RegisterOptions{Name: contract.ExecuteActivityName})
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	select {
	case stop := <-stops:
		return stop
	case <-ctx.Done():
		t.Fatal("provider callback did not stop")
		return providerCallbackStop{}
	}
}

const heartbeatFrontendNamespace = "pm-heartbeat-frontend"

// heartbeatFrontend is an in-process fake of the Temporal frontend. It gives
// out one ExecuteUnit attempt and answers every heartbeat at once, except the
// heartbeat that it holds. It holds the holdAfterStart-th heartbeat that
// arrives after the provider callback starts, until that RPC ends.
type heartbeatFrontend struct {
	workflowservice.UnimplementedWorkflowServiceServer

	listener        *bufconn.Listener
	holdAfterStart  int32
	afterStart      atomic.Int32
	delivered       atomic.Bool
	callbackStarted chan struct{}
	held            chan struct{}
	release         chan struct{}
}

func startHeartbeatFrontend(t *testing.T, holdAfterStart int32) *heartbeatFrontend {
	t.Helper()
	frontend := &heartbeatFrontend{
		listener:        bufconn.Listen(1 << 20),
		holdAfterStart:  holdAfterStart,
		callbackStarted: make(chan struct{}),
		held:            make(chan struct{}),
		release:         make(chan struct{}),
	}
	server := grpc.NewServer()
	workflowservice.RegisterWorkflowServiceServer(server, frontend)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = server.Serve(frontend.listener)
	}()
	t.Cleanup(func() {
		close(frontend.release)
		server.Stop()
		<-served
	})
	return frontend
}

func (frontend *heartbeatFrontend) GetSystemInfo(
	context.Context,
	*workflowservice.GetSystemInfoRequest,
) (*workflowservice.GetSystemInfoResponse, error) {
	return &workflowservice.GetSystemInfoResponse{
		Capabilities: &workflowservice.GetSystemInfoResponse_Capabilities{},
	}, nil
}

func (frontend *heartbeatFrontend) DescribeNamespace(
	context.Context,
	*workflowservice.DescribeNamespaceRequest,
) (*workflowservice.DescribeNamespaceResponse, error) {
	return &workflowservice.DescribeNamespaceResponse{
		NamespaceInfo: &namespacepb.NamespaceInfo{
			Name:         heartbeatFrontendNamespace,
			Capabilities: &namespacepb.NamespaceInfo_Capabilities{},
		},
	}, nil
}

func (frontend *heartbeatFrontend) PollActivityTaskQueue(
	ctx context.Context,
	_ *workflowservice.PollActivityTaskQueueRequest,
) (*workflowservice.PollActivityTaskQueueResponse, error) {
	if !frontend.delivered.CompareAndSwap(false, true) {
		<-ctx.Done()
		return &workflowservice.PollActivityTaskQueueResponse{}, nil
	}
	now := timestamppb.Now()
	return &workflowservice.PollActivityTaskQueueResponse{
		TaskToken:                   []byte("execute-unit-attempt-1"),
		WorkflowNamespace:           heartbeatFrontendNamespace,
		WorkflowType:                &commonpb.WorkflowType{Name: contract.RunWorkflowType},
		WorkflowExecution:           &commonpb.WorkflowExecution{WorkflowId: "pm/run/heartbeat-frontend", RunId: "run-1"},
		ActivityType:                &commonpb.ActivityType{Name: contract.ExecuteActivityName},
		ActivityId:                  "1",
		ScheduledTime:               now,
		CurrentAttemptScheduledTime: now,
		StartedTime:                 now,
		Attempt:                     1,
		ScheduleToCloseTimeout:      durationpb.New(20 * time.Second),
		StartToCloseTimeout:         durationpb.New(10 * time.Second),
		HeartbeatTimeout:            durationpb.New(2 * time.Second),
	}, nil
}

func (frontend *heartbeatFrontend) RecordActivityTaskHeartbeat(
	ctx context.Context,
	_ *workflowservice.RecordActivityTaskHeartbeatRequest,
) (*workflowservice.RecordActivityTaskHeartbeatResponse, error) {
	select {
	case <-frontend.callbackStarted:
		if frontend.holdAfterStart > 0 && frontend.afterStart.Add(1) == frontend.holdAfterStart {
			close(frontend.held)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-frontend.release:
				return nil, errors.New("fake frontend stopped")
			}
		}
	default:
	}
	return &workflowservice.RecordActivityTaskHeartbeatResponse{}, nil
}

func (frontend *heartbeatFrontend) RespondActivityTaskFailed(
	context.Context,
	*workflowservice.RespondActivityTaskFailedRequest,
) (*workflowservice.RespondActivityTaskFailedResponse, error) {
	return &workflowservice.RespondActivityTaskFailedResponse{}, nil
}

func (frontend *heartbeatFrontend) ShutdownWorker(
	context.Context,
	*workflowservice.ShutdownWorkerRequest,
) (*workflowservice.ShutdownWorkerResponse, error) {
	return &workflowservice.ShutdownWorkerResponse{}, nil
}
