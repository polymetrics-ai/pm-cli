# 0005: Ready signal from pm-server and pm-worker to their starter

Date: 2026-11-12. Status: proposed. This record does not authorize application work. Every flag, text and check below is a planned contract until the owning implementation creates and runs it. Issue: #72.

## Context

A starter starts `pm-server` and `pm-worker` as separate processes. The starter can be an acceptance-suite step, an operator launcher or a service manager. The operator owns this lifecycle, and a client invocation never starts a backend ([ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery)). The starter must know when each process completed its start.

Today neither process reports this. The acceptance lane in PR #261 repeats an HTTP request or a Temporal poller look until one passes. This record sets a requirement: to learn that each process completed its start, a starter waits for one event from each process and repeats no observation. With a repeated observation, the result and the duration of a start depend on a polling interval that the starter chooses. The starter still keeps one start deadline on its single blocking wait, because this record does not show that every start step is bounded (see Evidence). One deadline on one wait is not a repeated observation: the starter observes nothing until the line, the end of file or the deadline comes. An operator launcher has the same need as the acceptance lane.

The plan already sets the meaning of readiness:

- A listening socket is not readiness. The operator checks PostgreSQL schema compatibility, secret-key availability, the Temporal namespace and worker registration, and object access ([plan, dependency stack](../architecture/pm-server/architecture/PLAN.md)).
- The Temporal reference asks for "Temporal namespace/health and worker poll readiness rather than only open ports" ([Temporal report](../architecture/pm-server/reference/remote-design/tech/temporal/REPORT.md)).
- The design identity FND-20 owns independently managed API and worker readiness.
- `/health/ready` answers one request with the current dependency state. A starter must ask it again and again to see a change. Today it returns HTTP 503 with dispatch false ([setup](../development/setup.md)).

These facts limit the choice:

- `pm-worker` and the relay client of `pm-server` set no Temporal SDK logger. So the SDK can write its log lines to the standard output of both executables.
- When a Go program does not call `signal.Notify` for SIGPIPE, a write to a closed pipe on descriptor 1 or 2 stops the program with SIGPIPE. On other descriptors the write fails with EPIPE ([Go `os/signal`](https://pkg.go.dev/os/signal#hdr-SIGPIPE)). These executables call `signal.Notify` only for interrupt, SIGTERM and SIGHUP.
- Temporal Go SDK v1.48.0 starts the worker pollers and returns. It gives no event when the Temporal service receives a poll. A poll returns with a task, with an error, or at the end of the long poll.
- The worker identity `<deployment ID>/pm-worker` is the same for every process of one deployment.
- `pm-server` answers requests after its admission probe passed and its HTTP accept loop runs. Its relay connects to Temporal only when it needs to. The plan keeps admission available during a Temporal outage, up to a cap ([plan](../architecture/pm-server/architecture/PLAN.md)).

## Decision

PM uses a one-line ready signal on an inherited file descriptor. The starter names the descriptor with a flag.

| Item | Contract |
| --- | --- |
| Request | `--ready-fd <n>` on `pm-server` and on `pm-worker`. The value has only ASCII decimal digits, has no leading zero, fits in an `int` and is 3 or more. The flag may appear once. Without the flag, the process sends no signal. |
| Refusal | The process stops with exit code 1, one line on standard error and no signal. It does this before it opens or reads its configuration. See "Refusals" below for the texts and the order. |
| Channel | Descriptor `n`, inherited from the starter. When the process checks the flag, it confirms that the descriptor has no close-on-exec flag, which shows that it was inherited. Then it puts the descriptor in one file handle and marks it close-on-exec, so that a child process does not hold it. The process uses only that handle. After it closes the handle, it never uses the number `n` again. |
| Signal | The exact bytes `pm-server ready` or `pm-worker ready`, then one line feed, in one write. Then the process closes the handle. The write runs apart from the serving path, so a full pipe cannot stop `pm-server` from serving or `pm-worker` from working. The signal then waits until the starter reads. |
| No signal | End of file with no data. The process exit closes the descriptor. |
| Content | The fixed text only. No secret, credential, host name, address, endpoint, path or identifier. |
| Failed write | The process writes `pm-server: the ready signal write failed. The process continues.` or the same text with `pm-worker:` on standard error once. It continues, and its exit code does not change. A standard error pipe that has no reader still stops the process with SIGPIPE at that write, as it does today at any write to standard error. |
| Starter | The starter must give the process only the write end of a pipe as descriptor `n`. It must make sure that no other process that it starts holds that write end. After it starts the process, it must close its own copy of the write end, or it never reads end of file. It must read until it gets the line or end of file, and it must keep one start deadline on that single wait. If the deadline passes first, the starter treats the start as failed and stops the process. |

### Refusals

The process checks in this order and stops at the first failure.

| Order | Case | Text after `pm-server: ` or `pm-worker: ` |
| --- | --- | --- |
| 1, while the flags are read | `--ready-fd` with no value | `flag needs an argument: --ready-fd` (the text of the flag parser) |
| 1, while the flags are read | A second `--ready-fd` | `invalid argument "<value>" for "--ready-fd" flag: --ready-fd may be supplied exactly once` |
| 1, while the flags are read | A value that is empty, has a character other than an ASCII digit (for example a sign), has a leading zero, does not fit in an `int`, or is below 3 | `invalid argument "<value>" for "--ready-fd" flag: --ready-fd requires only the digits 0-9, with no leading zero, for a number from 3 to the int maximum` |
| 2, after the `--config` and `--secrets-directory` checks | A descriptor that is not open for writing, or that has the close-on-exec flag set when the process checks it | `--ready-fd requires an inherited descriptor that is open for writing` |

The flag parser adds the `invalid argument … flag: ` prefix and repeats the given value, which is not a secret. The check of a second flag comes before the check of its value. The process refuses a leading zero, for example `03`, so that no value can be read as an octal number. Decision 0001 rejects malformed values of client controls, and this record applies the same rule to this flag. Go opens each descriptor of its own with close-on-exec set, and a descriptor that a starter passes arrives without it. So a descriptor with the flag set at the check belongs to the process itself, and the process refuses it. The flag parser gives "flag needs an argument" only when `--ready-fd` is the last argument. When another argument follows, the parser takes it as the value, so the value check refuses it. The flag parser owns its own texts, so an upgrade of the parser can change them.

A process sends at most one signal in its life. A process that fails before it is ready sends no signal, exits with code 1 and prints its current error text on standard error. A process that the starter stops before it is ready sends no signal, and its exit code stays as it is today.

`pm-server` is ready when its admission probe passed, its state is serving, and its HTTP accept loop calls Accept for the first time. Temporal and the relay are not part of this. `/health/live`, `/health/ready` and the `Readiness` type do not change.

`pm-worker` is ready when the Temporal worker start returned without an error. At that point, the Temporal service answered GetSystemInfo and DescribeNamespace for the configured namespace without an error. The SDK also continues when the service answers either call with "not implemented", so the signal does not prove that the service supports these calls. The process registered its workflows and activities in its own SDK worker, which makes no Temporal service call, and the workflow and activity pollers started. The signal does not say that the Temporal service lists the poller or answered a poll. This record expects that the Temporal service keeps a task in its queue until a poller takes it, inside the task's own timeouts. Then work that a client sends after the signal waits for the first poll and is not lost. This expectation is not checked: the Temporal server source was not available for this record.

### Channel options

| Option | Assessment for PM |
| --- | --- |
| Inherited descriptor named by `--ready-fd` | Selected. One blocking read gives the signal or end of file. The channel holds only the signal. The process cannot be stopped by SIGPIPE on it. It has no path, no socket file and no environment value. |
| One line on standard output, always on | Rejected. Standard output can carry SDK log lines. The starter must read standard output for the life of the process, or the process blocks when the pipe is full. A standard output pipe that has no reader stops the process with SIGPIPE at its next log line. |
| systemd notification protocol (`NOTIFY_SOCKET`, `READY=1`) | Not selected now. The socket path comes in the environment, which a later child process inherits unless the process removes it. It needs a socket file with a path length limit. The starter must watch the process exit separately. A later packaging decision can add it as a second request form. |
| A ready file | Rejected. The starter must choose a path and remove the file after use. A process that stops can leave a stale file. A file watch has a different interface on each platform, and without a watch the starter must check the file again and again. |
| The starter asks `/health/ready` or the Temporal poller list | Rejected. This is the repeated observation that this record removes. `/health/ready` also reports dispatch false today. |

### Request options

| Option | Assessment for PM |
| --- | --- |
| A command-line flag | Selected. The descriptor belongs to one start. The flag is explicit, shows in `--help` and follows the once-only rule that the other flags of these executables have in their own code (`singleStringFlag` in `cmd/pm-server/main.go` and `cmd/pm-worker/main.go`). The value is not a secret. |
| An environment value | Rejected. It does not show in `--help`. An environment value also goes to each later child process unless the process removes it at start, so inheritance alone is not the reason. |
| The operator configuration file | Rejected. The configuration describes the deployment. The descriptor changes with each start. |

### Worker readiness options

| Option | Assessment for PM |
| --- | --- |
| Signal when the pollers start | Selected. The process knows every fact in its promise. No repeated check exists in the product or in a test. |
| `pm-worker` asks Temporal for its own poller (DescribeTaskQueue) until Temporal lists it | Not selected. It is a repeated check in the product. With a deployment-wide worker identity, a listed poller can belong to an earlier process or another replica. So the promise fails after a restart unless the identity gets a per-process part. It can also need DescribeTaskQueue permission for the worker when the Temporal service checks permissions (not checked). |
| Signal at the first answered poll | Not selected. On an idle queue a poll answers only at the end of the long poll. So each start waits that long (about one minute with the Temporal default, not measured here). |
| `pm-worker` sends a probe task to itself | Rejected. It writes workflow history at each start. Another worker on the same queue can take the task, so it does not prove that this process polls. |

## Consequences

- Two executables get a new operator contract: the flag, its help text, the texts above and an operator section in [setup](../development/setup.md). The operator section also states the duties of the starter. A later change to the contract is a breaking change for starters.
- The contract needs inherited descriptors. Go `exec.Cmd.ExtraFiles` does not work on Windows. These executables have no Windows target.
- A systemd unit with `Type=notify` cannot use this signal directly. Packaging (M12) decides an adapter or a second request form. Packaging (M12) also decides how an image or a Compose starter passes the descriptor and reads the signal.
- `pm-worker` stops using the SDK `Run` call. It calls `Start` with an `OnFatalError` callback and then waits for its context or a fatal error. When its context ends, it calls `Stop`, as `Run` does today, so the drain timeout (`WorkerStopTimeout`) still applies. Reason: in SDK v1.48.0, `Run` after `Start` panics when a fatal error stopped the worker first. Later changes must keep this. The error texts do not change.
- The worker signal is weaker than "worker poll readiness" in the Temporal reference. A worker whose polls the Temporal service refuses still sends the signal, because the SDK retries most poll errors. An end-to-end scenario, in which a worker takes and completes work, proves polling (issue #72, part b). The signal does not.
- `pm-server` still starts its relay before the listener and the admission probe. A `pm-server` that never becomes ready runs its relay for a short time. This record does not change that.
- Code owners (groups in the file ownership map): F11 for `cmd/pm-server/main.go`, `internal/server/lifecycle.go` and `internal/server/server.go`. R07 for `cmd/pm-worker/main.go`. F02 for the operator text in `docs/development/setup.md`. The record adds no dependency.
- Each main package holds its own descriptor code, so the two copies can drift. Control: the planned scenarios that check the signal content and the single signal run both executables.

## Decisions that this record touches

| Decision | Effect |
| --- | --- |
| [ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery) operator lifecycle | Kept. The starter owns the process lifecycle. No `pm` command starts these processes or waits for them. |
| [ADR475-01](../architecture/pm-server/architecture/ADRS.md#adr475-01--one-remote-stack) one remote stack | Kept. |
| [ADR475-02](../architecture/pm-server/architecture/ADRS.md#adr475-02--gin-adapter) adapters | Kept. The flag is in the Cobra command of each executable. The server hook is a plain function. No framework type enters domain or workflow code. |
| [0001](0001-cli-configuration.md) configuration | Kept. 0001 rejects repeated and malformed values of client controls. This record applies the same rule to this flag of `pm-server` and `pm-worker`. The operator configuration file does not change. |
| Plan design identity FND-20 and the dependency-stack readiness text ([plan](../architecture/pm-server/architecture/PLAN.md)) | Kept. The signal is one part of independently managed API and worker readiness. The operator still checks the dependencies. The `pm-worker` signal covers the Temporal namespace check, with the "not implemented" limit stated for `pm-worker` readiness, and the local registration of workflows and activities. It does not cover a registration in the Temporal service. |
| "Worker poll readiness" in the [Temporal report](../architecture/pm-server/reference/remote-design/tech/temporal/REPORT.md) | Kept as a check for the operator launcher, not met by the signal. The `pm-worker` signal does not claim poll readiness. An end-to-end scenario proves that a worker polls. A launcher that needs poll readiness must observe Temporal itself (FND-20, M12). |

This record supersedes nothing.

## Planned proof

Gherkin scenarios that run the real binaries. None exists yet. The scenarios are planned for the acceptance criteria of issue #72. Those criteria are not accepted yet, and when they are accepted they own the exact titles. Each scenario uses the job-owned stack, except the refusal scenario, which stops before the process reads its configuration.

- `pm-server` answers a request at once after its ready signal.
- `pm-server` that cannot open its listener sends no ready signal.
- `pm-worker` sends its ready signal after it starts in its Temporal namespace.
- `pm-worker` that cannot start in its Temporal namespace sends no ready signal.
- Each process sends exactly one ready signal in its life. The starter reads end of file on the descriptor while the process still runs, which shows that the process closed the descriptor after the signal.
- The ready signal holds no secret or private value.
- Each executable refuses an unusable `--ready-fd` value with exit code 1, its refusal text and no signal.
- A process continues when its starter closed the ready channel, and it prints its diagnostic line.

The acceptance lane waits for the two signals and makes no repeated observation. No scenario can prove this. A review check that the lane start makes no repeated observation is planned for the acceptance criteria of issue #72. Those criteria are not accepted yet.

A process can start a query child while it still holds the descriptor. At the base commit this window may not be reachable yet, because no production code outside `internal/warehouse` builds a query child configuration (see Evidence). This record keeps the window for later wiring. The query child starts for a query or a sync activity. The pollers of `pm-worker` run before its signal and can take a sync activity. `pm-server` serves requests from its first Accept, and a full pipe that the starter does not read keeps the write and the close waiting while the process serves queries. In this window two layers keep the descriptor out of the child:

1. Close-on-exec on the handle. The exec of the child closes the descriptor.
2. The supervisor of the query child, for descriptors 3 to 255 only. It declares to the child each inherited descriptor in that range that has no close-on-exec, and the child closes each declared descriptor and refuses to run without the declaration. The supervisor and the child scan only that range (`internal/warehouse/child.go` and `cmd/pm-query/main.go`). For a ready descriptor of 256 or more, close-on-exec is the only control. This record does not limit `--ready-fd` to that range, because the range is an internal choice of the query child and can change without a change to this contract.

No scenario can prove the first layer. The query child reports the descriptors that it inherited only to its supervisor, and no public API response or command output shows that report. So a scenario through the real binaries cannot see whether a child inherited the descriptor. This record does not add a public view of that internal report. The design review of the start code must check these points:

- The process refuses a descriptor that has close-on-exec set at the check, and then marks the handle close-on-exec, before it opens its configuration and before any code that can start a child process.
- For a ready descriptor of 256 or more, close-on-exec is the only control, because the second layer scans only descriptors 3 to 255.
- The process keeps that one handle and makes no copy of the descriptor without close-on-exec.
- The process closes the handle after the write on every path, including a failed write, and never uses the number `n` again.
- The declare-and-close path of the query child supervisor stays in place.

The single-signal scenario shows that the process closes the descriptor after the signal while the process still runs.

## Evidence

- Read, not run: the start code of both executables, the server lifecycle and relay composition, the worker configuration, the Temporal Go SDK v1.48.0 worker start and poll code, and the Go `os/signal` and `os/exec` documentation for go1.27.1.
- Run: a local spike on Darwin with go1.26.5. A child process wrote one line to an inherited descriptor and closed it. The starter read the line and then end of file. A child that exited early gave end of file with no data. A write after the starter closed its read end returned EPIPE, and the child continued and exited with code 0. Without close-on-exec, a grandchild held the descriptor, and end of file came about 3 seconds late, when the grandchild exited. With close-on-exec, end of file came at once.
- Not run: the spike on Linux or with go1.27.1, any Gherkin scenario, and a measurement of the Temporal server long-poll default.
- Run: an offline `pm-server` built from the current main. A repeated `--config` printed `pm-server: invalid argument "b" for "--config" flag: --config may be supplied exactly once`, and `--config` with no value printed `pm-server: flag needs an argument: --config`. Both exited with code 1. This shows the flag parser texts that the refusals above use.
- Read: the Temporal Go SDK v1.48.0 calls GetSystemInfo in `ensureInitialized` and `loadCapabilities`, and DescribeNamespace in `loadNamespaceData` (`internal/internal_workflow_client.go`). Both continue on a "not implemented" answer. `cmd/pm-worker/main.go`, `internal/temporal/worker_client.go` and the relay client options in `internal/server/server.go` set no SDK logger. The owner groups come from `docs/architecture/pm-server/implementation/FILE-OWNERSHIP.json`. The scan range 3 to 255 comes from `descriptorScanLimit` in `internal/warehouse/child.go` and `cmd/pm-query/main.go`.
- Read, the bounds of the start steps: the admission probe of `pm-server` runs with `ProbeTimeout` (`internal/server/lifecycle.go`). The Temporal dial of `pm-worker` runs with the configured dial timeout, at most 5 minutes (`internal/temporal/worker_client.go`). In `Worker.Start`, GetSystemInfo has a 5-second default timeout (`internal/internal_workflow_client.go`), and DescribeNamespace has a 10-second RPC timeout when its context has no deadline (`internal/internal_utils.go`).
- Read: both executables build their flags with Cobra v1.10.2, which uses the flag parser pflag v1.0.10 (`go.mod`). The `invalid argument … flag: ` prefix comes from pflag (`errors.go`). A repository search finds `signal.NotifyContext` for interrupt, SIGTERM and SIGHUP in both executables and no other `signal.Notify` call in them or in `internal/`.
- Read: the SDK `RegisterWorkflow` and `RegisterActivity` add to the worker's own registry (`internal/internal_worker.go`). `Run` calls `Stop` on an interrupt, and `pm-worker` sets `WorkerStopTimeout` from its drain timeout (`cmd/pm-worker/main.go`).
- Read: the query child starts in `startChildAttempt` (`internal/warehouse/child.go`). Its callers are the warehouse query service (`internal/httpapi/warehouse.go`) and the sync materialization (`internal/sync/etl.go`). A repository search found no production code outside `internal/warehouse` that builds a query child configuration, so at the base commit the window above may not be reachable yet. This record keeps the window for later wiring. The public query result (`warehouseQueryResponse` in `internal/httpapi/warehouse.go`) copies no field of the child report, and no code outside `internal/warehouse` reads the child report.
- Run: `pm-server --config --secrets-directory x` printed `pm-server: unknown command "x" for "pm-server"` and exited with code 1. The parser took `--secrets-directory` as the value of `--config`.
- Not checked: that the Temporal service keeps a task in its queue until a poller takes it. The Temporal server source was not read.
- Not checked: the facts about the systemd notification protocol (a socket path in the environment, with a length limit), and whether a Temporal authorizer requires a DescribeTaskQueue permission.
- Not checked: the bounds of `server.Open` (the store, the identity provider discovery and the artifact resources) and of the worker runtime open. So a process may need the starter's deadline.
- Not checked: whether an image or a Compose starter can pass an inherited descriptor.

## Revisit trigger

- Packaging needs a service-manager protocol, for example systemd notification, or an image or a Compose starter cannot pass an inherited descriptor.
- An operator needs a signal that the Temporal service lists or answered the worker poll, or the worker gets a per-process identity.
- A supported target has no inherited descriptors.
- A starter needs a signal for a later state change, for example drain or the loss of a dependency.
- A Temporal SDK upgrade changes what `Worker.Start` checks before it returns, or changes the contract of `Start` and `Run`.
- A flag-parser upgrade changes a refusal text.
