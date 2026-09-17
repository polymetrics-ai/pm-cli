package temporal

import (
	"context"
	"errors"
	"sync"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	sdkclient "go.temporal.io/sdk/client"
)

var ErrClientClosed = errors.New("Temporal client is closed")

// WorkflowClient is intentionally narrower than sdkclient.Client. In
// particular, reconciliation has no cancellation or termination capability.
type WorkflowClient interface {
	ExecuteWorkflow(context.Context, sdkclient.StartWorkflowOptions, any, ...any) (sdkclient.WorkflowRun, error)
	DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	GetWorkflowHistory(context.Context, string, string, bool, enumspb.HistoryEventFilterType) sdkclient.HistoryEventIterator
	UpdateWorkflow(context.Context, sdkclient.UpdateWorkflowOptions) (sdkclient.WorkflowUpdateHandle, error)
	Close()
}

type DialFunc func(context.Context) (WorkflowClient, error)

type ClientConfig struct {
	Namespace            string
	ControlTaskQueue     string
	WorkflowType         string
	ProjectionUpdateName string
	DialTimeout          time.Duration
	RequestTimeout       time.Duration
	Dial                 DialFunc
}

type Client struct {
	config ClientConfig

	mu      sync.Mutex
	backend WorkflowClient
	dialing chan struct{}
	closed  bool
}

// NewClient validates only local finite configuration. It intentionally does
// not dial Temporal, so a dependency outage cannot remove PostgreSQL admission.
func NewClient(configuration ClientConfig) (*Client, error) {
	if !validTemporalIdentity(configuration.Namespace) {
		return nil, errors.New("Temporal namespace is invalid")
	}
	if configuration.ControlTaskQueue != ControlTaskQueue {
		return nil, errors.New("Temporal control task queue must be pm-control")
	}
	if configuration.WorkflowType != RunWorkflowType {
		return nil, errors.New("Temporal run workflow type is invalid")
	}
	if configuration.ProjectionUpdateName != ProjectionUpdateName {
		return nil, errors.New("Temporal projection update name is invalid")
	}
	if configuration.DialTimeout <= 0 || configuration.RequestTimeout <= 0 ||
		configuration.DialTimeout > 5*time.Minute || configuration.RequestTimeout > 5*time.Minute {
		return nil, errors.New("Temporal client time bounds are invalid")
	}
	if configuration.Dial == nil {
		return nil, errors.New("Temporal dial function is required")
	}
	return &Client{config: configuration}, nil
}

func (client *Client) Close() {
	if client == nil {
		return
	}
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return
	}
	client.closed = true
	backend := client.backend
	client.backend = nil
	client.mu.Unlock()
	if backend != nil {
		backend.Close()
	}
}

func (client *Client) connection(ctx context.Context) (WorkflowClient, error) {
	if client == nil {
		return nil, errors.New("Temporal client is required")
	}
	if ctx == nil {
		return nil, errors.New("Temporal client context is required")
	}
	for {
		client.mu.Lock()
		if client.closed {
			client.mu.Unlock()
			return nil, ErrClientClosed
		}
		if client.backend != nil {
			backend := client.backend
			client.mu.Unlock()
			return backend, nil
		}
		if client.dialing != nil {
			waiting := client.dialing
			client.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-waiting:
				continue
			}
		}
		dialing := make(chan struct{})
		client.dialing = dialing
		client.mu.Unlock()

		dialCtx, cancel := context.WithTimeout(ctx, client.config.DialTimeout)
		backend, err := client.config.Dial(dialCtx)
		cancel()
		if err == nil && backend == nil {
			err = errors.New("Temporal dial returned no client")
		}

		client.mu.Lock()
		closed := client.closed
		if err == nil && !closed {
			client.backend = backend
		}
		client.dialing = nil
		close(dialing)
		client.mu.Unlock()

		if closed {
			if backend != nil {
				backend.Close()
			}
			return nil, ErrClientClosed
		}
		if err != nil {
			return nil, err
		}
		return backend, nil
	}
}

func (client *Client) requestContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if client == nil {
		return nil, nil, errors.New("Temporal client is required")
	}
	if ctx == nil {
		return nil, nil, errors.New("Temporal request context is required")
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.config.RequestTimeout)
	return requestCtx, cancel, nil
}
