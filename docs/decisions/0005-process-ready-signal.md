# 0005: Ready signal from pm-server and pm-worker to their starter

Date: 2026-11-12. Status: proposed. This record does not authorize application work. Every flag, text and check below is a planned contract until the owning implementation creates and runs it. Issue: #72.

## Context

A starter starts `pm-server` and `pm-worker` as separate processes. The starter can be an acceptance-suite step, an operator launcher or a service manager. The operator owns this lifecycle, and a client invocation never starts a backend ([ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery)). The starter must know when each process completed its start.

Today neither process reports this. The acceptance lane in PR #261 repeats an HTTP request or a Temporal poller look until one passes. This record sets a requirement: a starter waits for one event from each process and repeats no observation. With a repeated observation, the result and the duration of a start depend on a polling interval and a deadline that the starter chooses. An operator launcher has the same need as the acceptance lane.

The plan already sets the meaning of readiness:

- A listening socket is not readiness. The operator checks PostgreSQL schema compatibility, secret-key availability, the Temporal namespace and worker registration, and object access ([plan, dependency stack](../architecture/pm-server/architecture/PLAN.md)).
- The Temporal reference asks for "Temporal namespace/health and worker poll readiness rather than only open ports" ([Temporal report](../architecture/pm-server/reference/remote-design/tech/temporal/REPORT.md)).
- The design identity FND-20 owns independently managed API and worker readiness.
- `/health/ready` answers one request with the current dependency state. A starter must ask it again and again to see a change. Today it returns HTTP 503 with dispatch false ([setup](../development/setup.md)).

These facts limit the choice:

- `pm-worker` and the relay client of `pm-server` set no Temporal SDK logger. So the SDK can write its log lines to the standard output of both executables.
- When a Go program does not call `signal.Notify` for SIGPIPE, a write to a closed pipe on descriptor 1 or 2 stops the program with SIGPIPE. On other descriptors the write fails with EPIPE ([Go `os/signal`](https://pkg.go.dev/os/signal#hdr-SIGPIPE)). These executables call `signal.Notify` only for interrupt, SIGTERM and SIGHUP.
- Temporal Go SDK v1.48.0 starts the worker pollers and returns. It gives no event when the Temporal service receives a poll. A poll returns only with a task or at the end of the long poll.
- The worker identity `<deployment ID>/pm-worker` is the same for every process of one deployment.
- `pm-server` answers requests after its admission probe passed and its HTTP accept loop runs. Its relay connects to Temporal only when it needs to. Admission stays available during a Temporal outage, up to a cap.

## Decision

PM uses a one-line ready signal on an inherited file descriptor. The starter names the descriptor with a flag.

| Item | Contract |
| --- | --- |
| Request | `--ready-fd <n>` on `pm-server` and on `pm-worker`. The value is a decimal number of 3 or more. The flag may appear once. Without the flag, the process sends no signal. |
| Refusal | A repeated flag, a value below 3, or a descriptor that is not open for writing stops the process with exit code 1. The process does this before it reads its configuration. |
| Channel | Descriptor `n`, inherited from the starter. The process marks it close-on-exec at once, so that a child process does not hold it. |
| Signal | The exact bytes `pm-server ready` or `pm-worker ready`, then one line feed, in one write. Then the process closes descriptor `n`. |
| No signal | End of file with no data. The process exit closes the descriptor. |
| Content | The fixed text only. No secret, credential, host name, address, endpoint, path or identifier. |
| Failed write | The process writes `pm-server: ready signal was not delivered` or `pm-worker: ready signal was not delivered` on standard error once. It continues, and its exit code does not change. |

A process sends at most one signal in its life. A process that fails before it is ready sends no signal, exits with code 1 and prints its current error text on standard error. A process that the starter stops before it is ready sends no signal, and its exit code stays as it is today.

`pm-server` is ready when its admission probe passed, its state is serving, and its HTTP accept loop calls Accept for the first time. Temporal and the relay are not part of this. `/health/live`, `/health/ready` and the `Readiness` type do not change.

`pm-worker` is ready when the Temporal worker start returned without an error. At that point, the Temporal service answered GetSystemInfo and DescribeNamespace for the configured namespace. The process registered its workflows and activities in its own SDK worker, which makes no Temporal service call, and the workflow and activity pollers started. The signal does not say that the Temporal service lists the poller or answered a poll. Temporal keeps a task in its queue until a poller takes it, inside the task's own timeouts. So work that a client sends after the signal is not lost, but it can wait for the first poll.

### Channel options

| Option | Assessment for PM |
| --- | --- |
| Inherited descriptor named by `--ready-fd` | Selected. One blocking read gives the signal or end of file. The channel holds only the signal. The process cannot be stopped by SIGPIPE on it. It has no path, no socket file and no environment value. |
| One line on standard output, always on | Rejected. Standard output can carry SDK log lines. The starter must read standard output for the life of the process, or the process blocks when the pipe is full. A closed standard output stops the process with SIGPIPE at its next log line. |
| systemd notification protocol (`NOTIFY_SOCKET`, `READY=1`) | Not selected now. The socket path comes in the environment, so each later child process that does not build its own environment inherits it. It needs a socket file with a path length limit. The starter must watch the process exit separately. A later packaging decision can add it as a second request form. |
| A ready file | Rejected. The starter must choose a path and remove the file after use. A process that stops can leave a stale file. A file watch has a different interface on each platform, and without a watch the starter must check the file again and again. |
| The starter asks `/health/ready` or the Temporal poller list | Rejected. This is the repeated observation that this record removes. `/health/ready` also reports dispatch false today. |

### Request options

| Option | Assessment for PM |
| --- | --- |
| A command-line flag | Selected. The descriptor belongs to one start. The flag is explicit, shows in `--help` and follows the strict once-only rule of the other flags of these executables ([0001](0001-cli-configuration.md)). The value is not a secret. |
| An environment value | Rejected. An environment value goes to each later child process that does not build its own environment. It does not show in `--help`. |
| The operator configuration file | Rejected. The configuration describes the deployment. The descriptor changes with each start. |

### Worker readiness options

| Option | Assessment for PM |
| --- | --- |
| Signal when the pollers start | Selected. The process knows every fact in its promise. No repeated check exists in the product or in a test. |
| `pm-worker` asks Temporal for its own poller (DescribeTaskQueue) until Temporal lists it | Not selected. It is a repeated check in the product. With a deployment-wide worker identity, a listed poller can belong to an earlier process or another replica. So the promise fails after a restart unless the identity gets a per-process part. It also needs DescribeTaskQueue permission for the worker. |
| Signal at the first answered poll | Not selected. On an idle queue a poll answers only at the end of the long poll. So each start waits that long (about one minute with the Temporal default, not measured here). |
| `pm-worker` sends a probe task to itself | Rejected. It writes workflow history at each start. Another worker on the same queue can take the task, so it does not prove that this process polls. |

## Consequences

- Two executables get a new operator contract: the flag, its help text, the texts above and an operator section in [setup](../development/setup.md). A later change to it is a breaking change for starters.
- The contract needs inherited descriptors. Go `exec.Cmd.ExtraFiles` does not work on Windows. These executables have no Windows target.
- A systemd unit with `Type=notify` cannot use this signal directly. Packaging (M12) decides an adapter or a second request form.
- `pm-worker` stops using the SDK `Run` call. It calls `Start` with an `OnFatalError` callback and then waits for its context or a fatal error. Reason: in SDK v1.48.0, `Run` after `Start` panics when a fatal error stopped the worker first. Later changes must keep this. The error texts do not change.
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
| [0001](0001-cli-configuration.md) configuration | Kept. The flag is strict and presence-aware. The operator configuration file does not change. |
| Plan design identity FND-20 and the dependency-stack readiness text ([plan](../architecture/pm-server/architecture/PLAN.md)) | Kept. The signal is one part of independently managed API and worker readiness. The operator still checks the dependencies. The `pm-worker` signal covers the Temporal namespace check and the local registration of workflows and activities. It does not cover a registration in the Temporal service. |
| "Worker poll readiness" in the [Temporal report](../architecture/pm-server/reference/remote-design/tech/temporal/REPORT.md) | Kept as a check for the operator launcher, not met by the signal. The `pm-worker` signal does not claim poll readiness. An end-to-end scenario proves that a worker polls. A launcher that needs poll readiness must observe Temporal itself (FND-20, M12). |

This record supersedes nothing.

## Planned proof

Gherkin scenarios that run the real binaries on the job-owned stack. None exists yet. The accepted criteria of issue #72 own the exact titles.

- `pm-server` answers a request at once after its ready signal.
- `pm-server` that cannot open its listener sends no ready signal.
- `pm-worker` sends its ready signal after it starts in its Temporal namespace.
- `pm-worker` that cannot start in its Temporal namespace sends no ready signal.
- Each process sends exactly one ready signal in its life.
- The ready signal holds no secret or private value.
- The acceptance lane waits for the two signals and makes no repeated observation.
- Proposed, if the criteria accept them: each executable refuses an unusable `--ready-fd` value, and a process continues when its starter closed the ready channel.

## Evidence

- Read, not run: the start code of both executables, the server lifecycle and relay composition, the worker configuration, the Temporal Go SDK v1.48.0 worker start and poll code, and the Go `os/signal` and `os/exec` documentation for go1.27.1.
- Run: a local spike on Darwin with go1.26.5. A child process wrote one line to an inherited descriptor and closed it. The starter read the line and then end of file. A child that exited early gave end of file with no data. A write after the starter closed its read end returned EPIPE, and the child continued and exited with code 0. Without close-on-exec, a grandchild held the descriptor, and end of file came about 3 seconds late, when the grandchild exited. With close-on-exec, end of file came at once.
- Not run: the spike on Linux or with go1.27.1, any Gherkin scenario, and a measurement of the Temporal server long-poll default.

## Revisit trigger

- Packaging needs a service-manager protocol, for example systemd notification.
- An operator needs a signal that the Temporal service lists or answered the worker poll, or the worker gets a per-process identity.
- A supported target has no inherited descriptors.
- A starter needs a signal for a later state change, for example drain or the loss of a dependency.
- A Temporal SDK upgrade changes what `Worker.Start` checks before it returns, or changes the contract of `Start` and `Run`.
