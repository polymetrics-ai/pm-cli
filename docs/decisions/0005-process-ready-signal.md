# 0005: Ready signal from pm-server and pm-worker to their starter

Date: 2026-11-12. Status: proposed. This record does not authorize application work. Every flag, text and check below is a planned contract until the owning implementation creates and runs it. Issue: #72.

## Context

A starter starts `pm-server` and `pm-worker` as separate processes. The starter can be an acceptance-suite step, an operator launcher or a service manager. The operator owns this lifecycle, and a client invocation never starts a backend ([ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery)). The starter must know when each process completed its start.

Today neither process reports this. The acceptance lane in PR #261 repeats an HTTP request or a Temporal poller look until one passes. This record sets a requirement: to learn that each process completed its start, a starter waits for one event from each process and repeats no observation. With a repeated observation, the result and the duration of a start depend on a polling interval that the starter chooses. The starter also needs no deadline and no timing value. Each process bounds its own start, and then it sends the signal or exits (see "Start budget"). So the single blocking wait of the starter ends with the line or with end of file, except in the residual case that "Start budget" states. An operator launcher has the same need as the acceptance lane.

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
| Starter | The starter must give the process only the write end of a pipe as descriptor `n`. It must make sure that no other process that it starts holds that write end. After it starts the process, it must close its own copy of the write end, or it never reads end of file. It must read until it gets the line or end of file. It needs no deadline, because the process ends its own start. During the start, it must read each pipe that it gives as standard output or standard error, or give a destination that cannot fill, for example a file. The starter owns these pipes, and a full pipe blocks each write of the process, also the text of the timer. |
| Start budget | Each process ends its start within its configured `start_timeout`. Before its configuration is valid, a fixed bound of 15 minutes applies. It sends the signal, or it exits with code 1. See "Start budget" below. |

### Refusals

The process checks in this order and stops at the first failure.

| Order | Case | Text after `pm-server: ` or `pm-worker: ` |
| --- | --- | --- |
| 1, while the flags are read | `--ready-fd` with no value | `flag needs an argument: --ready-fd` (the text of the flag parser) |
| 1, while the flags are read | A second `--ready-fd` | `invalid argument "<value>" for "--ready-fd" flag: --ready-fd may be supplied exactly once` |
| 1, while the flags are read | A value that is empty, has a character other than an ASCII digit (for example a sign), has a leading zero, does not fit in an `int`, or is below 3 | `invalid argument "<value>" for "--ready-fd" flag: --ready-fd requires only the digits 0-9, with no leading zero, for a number from 3 to the int maximum` |
| 2, after the `--config` and `--secrets-directory` checks | A descriptor that is not open for writing, or that has the close-on-exec flag set when the process checks it | `--ready-fd requires an inherited descriptor that is open for writing` |

The flag parser adds the `invalid argument … flag: ` prefix and repeats the given value, which is not a secret. The check of a second flag comes before the check of its value. The process refuses a leading zero, for example `03`, so that no value can be read as an octal number. Decision 0001 rejects malformed values of client controls, and this record applies the same rule to this flag. Go opens each descriptor of its own with close-on-exec set, and a descriptor that a starter passes arrives without it. So a descriptor with the flag set at the check belongs to the process itself, and the process refuses it. The flag parser gives "flag needs an argument" only when `--ready-fd` is the last argument. When another argument follows, the parser takes it as the value, so the value check refuses it. The flag parser owns its own texts, so an upgrade of the parser can change them.

A process sends at most one signal in its life. A process that fails before it is ready sends no signal, exits with code 1 and prints its current error text on standard error. A process that the starter stops before it is ready sends no signal, and its exit code stays as it is today, unless the stop is still running when the start timer fires. Then the timer ends the process with exit code 1 (see "Start budget").

`pm-server` is ready when its admission probe passed, its state is serving, and its HTTP accept loop calls Accept for the first time. Temporal and the relay are not part of this. `/health/live`, `/health/ready` and the `Readiness` type do not change.

`pm-worker` is ready when these steps passed, in this order:

1. After the dial, `pm-worker` calls GetSystemInfo once and DescribeNamespace once for the configured namespace, with no retry options. It accepts only a success answer, and the DescribeNamespace answer must name the configured namespace. Any other result, including "not implemented", stops the start with exit code 1, the text `pm-worker: Temporal worker dependency is unavailable` and no signal. The dial and the two calls finish within the configured `dial_timeout`, counted from the start of the dial. Principle: fail closed. The SDK continues when the service answers either call with "not implemented" (see Evidence), so the SDK start alone does not prove these answers. The SDK already makes these two kinds of call with the same client identity, so they need no new permission.
2. The process registered its workflows and activities in its own SDK worker, which makes no Temporal service call.
3. The Temporal worker start returned without an error, and the workflow and activity pollers started.

So the signal proves each of these steps and nothing more. Principle: a signal proves exactly what it claims. The signal does not say that the Temporal service lists the poller or answered a poll. This record expects that the Temporal service keeps a task in its queue until a poller takes it, inside the task's own timeouts. Then work that a client sends after the signal waits for the first poll and is not lost. This expectation is not checked: the Temporal server source was not available for this record.

### Start budget

Each start of `pm-server` and `pm-worker` ends in one of two defined states, within a bound that the process owns: ready, with the signal, or failed, with exit code 1, which closes the descriptor. Principle: the owner of a step owns its bound. The process owns its start steps. The operator configuration owns the durations that depend on the deployment, as it already does for `dial_timeout` and `probe_timeout`.

- Value: `start_timeout` in the `server` section for `pm-server`, and in the `worker` section for `pm-worker`. It is optional, and its default is 10 minutes. It must be more than 0 and not more than 15 minutes. For `pm-server` it must be more than `probe_timeout`, and for `pm-worker` more than `dial_timeout`, so that a step with its own bound can report its own failure first. These rules apply to a configured value and to the default. This record also gives `probe_timeout` the upper bound of 5 minutes that `dial_timeout` already has, so the default always meets the rules. A value that breaks a rule gets the configuration diagnostic of that field, and the process sends no signal.
- Timer: the process sets one timer before it opens its configuration, at 15 minutes, the largest allowed value. When the configuration is valid, the process moves the timer earlier, to `start_timeout` counted from the same start time. The timer only moves earlier. So a configuration read that blocks, for example on a FIFO that has no writer, also ends at the timer. This fixed bound is necessary, because the process does not know the configured value before it reads it. Only the design review checks this path.
- One outcome: the start state is starting, ready or failed. The ready point and the timer each try to change it from starting, and the first change wins. If the timer wins, the process writes `pm-server: the start did not complete within start_timeout` or the same text with `pm-worker:` on standard error, and exits at once with code 1 and no signal. If the ready point wins, the timer does nothing. A start step that fails changes the state from starting to failed and writes its current text before its cleanup. The timer stays in force until the process is ready or exits. If it fires after a failed step, the process exits at once with code 1 and writes no timer text. So a cleanup that does not end also ends at the timer. If standard error is a pipe that has no reader, the write of the text stops the process with SIGPIPE, as for any other write to standard error (see "Failed write"). The descriptor closes in each case. Principle: a total protocol, in which every path ends in a defined state.
- The timer delays no start step and fires at most once, so it is not a sleep, a retry or a repeated observation. Every start has the timer, also a start without `--ready-fd`.
- The starter needs no deadline. Its single read ends with the line or with end of file, because the process writes the line or exits, no other process holds the write end, and the starter reads the output pipes that it gives (the starter duties above and the two layers below). The residual below is the one exception.

One residual stays, and no design inside the process can remove it: a process that cannot run its own code or cannot exit. Examples are a stopped or frozen process, and a thread in an uninterruptible kernel wait on hung storage. The line needs a write by the process. End of file needs the process to close the descriptor or to exit, and the kernel closes it only at exit. So such a process ends no wait, and the stopped-process spike in Evidence shows this. In the acceptance lane, outer guards that already exist end that case: `go test -timeout 40m` and the step limit of 50 minutes. An operator launcher needs an outer guard of its own, and Packaging (M12) owns it. This record adds no timing value to the starter.

The result of a slow start still depends on `start_timeout`. A total protocol needs a bound, and before the bound ends, no observer can tell a slow start that will complete from a start that will never complete. So no design can remove this dependence. The operator owns the value, as for `dial_timeout` and `probe_timeout`.

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

### Start limit options

| Option | Assessment for PM |
| --- | --- |
| Own GetSystemInfo and DescribeNamespace calls of `pm-worker` that accept only success | Selected. Fail closed, and the signal proves what it claims without a dependence on how the SDK treats an answer. |
| A gRPC interceptor that turns "not implemented" into an error for the two SDK calls | Rejected. It depends on which calls the SDK makes in its start and how it treats each error, and an SDK upgrade can change that without notice. |
| Keep the SDK behaviour and state the limit | Rejected. The owner does not accept the limit. |
| Start budget in the process (the `start_timeout` timer) | Selected. The owner of the steps owns their bound, and the starter needs no timing value. |
| The starter keeps one start deadline | Rejected. Each starter chooses its own value, and the starter does not own the start steps. |
| A bound for each start step, with no timer | Rejected. The SDK worker start takes no context, and a later step can come without a bound, so the total bound depends on a review of every step. |
| A fixed start budget in the code | Rejected as the start budget: no scenario can run it in a usable time, and a deployment cannot change it. The process keeps a fixed 15-minute bound only before its configuration is valid, because it does not know the configured value then. |
| The starter also waits for the process exit | Not selected. It does not end the wait for a live process that hangs. |
| The supervisor and the child list the open descriptors | Selected. Act on what exists: every descriptor number is covered, with no fixed range. |
| Limit `--ready-fd` to 3 to 255 | Rejected. It couples the public flag to an internal choice of the query child. |
| Check every number up to the open-file limit | Rejected. It is still a fixed range, and its cost grows with the limit (245,760 in the Darwin spike), not with the open descriptors. |
| The child refuses each descriptor without close-on-exec that the supervisor did not declare | Not selected. Native libraries in the child can hold such descriptors (not checked), so the child could refuse a sound start. |
| A socket pair that carries the credentials of the sender | Rejected. It stops a line from another process, but not the leak and not a start that hangs, and it replaces the reviewed channel, refusal texts and spike. |

## Consequences

- Two executables get a new operator contract: the flag, its help text, the texts above and an operator section in [setup](../development/setup.md). The operator section also states the duties of the starter. A later change to the contract is a breaking change for starters.
- The operator configuration gets one optional field, `start_timeout`, in the `server` and `worker` sections, with the rules in "Start budget". The configuration schema and the example configuration change with it. A start that needs more than 15 minutes fails. A start that hangs now ends with exit code 1, also a start without `--ready-fd`. The operator section states the field and states that the starter needs no deadline.
- `probe_timeout` gets an upper bound of 5 minutes. A configuration with a `probe_timeout` of more than 5 minutes, which is valid today, becomes invalid. No configuration file in the repository sets `probe_timeout` (see Evidence).
- A start that the timer ends does not run the normal stop steps of the process. It leaves the same state as a stop by SIGKILL before ready.
- `pm-worker` makes two more Temporal calls at each start, GetSystemInfo and DescribeNamespace, once each. A short transient error on either call now stops the start with exit code 1, and the starter or the service manager decides whether to start the process again. Under the rule-5 ruling, the acceptance lane does not start it again. `pm-worker` no longer starts against a Temporal service that does not implement either call.
- The query child needs the descriptor directory `/dev/fd`. Where the directory is missing, the query child does not start, and nothing falls back to a fixed range.
- The contract needs inherited descriptors. Go `exec.Cmd.ExtraFiles` does not work on Windows. These executables have no Windows target.
- A systemd unit with `Type=notify` cannot use this signal directly. Packaging (M12) decides an adapter or a second request form. Packaging (M12) also decides how an image or a Compose starter passes the descriptor and reads the signal.
- `pm-worker` stops using the SDK `Run` call. It calls `Start` with an `OnFatalError` callback and then waits for its context or a fatal error. When its context ends, it calls `Stop`, as `Run` does today, so the drain timeout (`WorkerStopTimeout`) still applies. Reason: in SDK v1.48.0, `Run` after `Start` panics when a fatal error stopped the worker first. Later changes must keep this. The error texts do not change.
- The worker signal is weaker than "worker poll readiness" in the Temporal reference. A worker whose polls the Temporal service refuses still sends the signal, because the SDK retries most poll errors. An end-to-end scenario, in which a worker takes and completes work, proves polling (issue #72, part b). The signal does not.
- `pm-server` still starts its relay before the listener and the admission probe. A `pm-server` that never becomes ready runs its relay for a short time. This record does not change that.
- Code owners (groups in the file ownership map): F11 for `cmd/pm-server/main.go`, `internal/server/lifecycle.go` and `internal/server/server.go`. R07 for `cmd/pm-worker/main.go`. F02 for the operator text in `docs/development/setup.md`. F11 also for `internal/config/server.go`. R01 for `internal/config/worker.go`, `schemas/server-config.schema.json` and `deploy/server.example.json`. R12 for `internal/warehouse/child.go` and `cmd/pm-query/main.go`. The record adds no dependency.
- Each main package holds its own descriptor code, so the two copies can drift. Control: the planned scenarios that check the signal content and the single signal run both executables.

## Decisions that this record touches

| Decision | Effect |
| --- | --- |
| [ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery) operator lifecycle | Kept. The starter owns the process lifecycle. No `pm` command starts these processes or waits for them. |
| [ADR475-01](../architecture/pm-server/architecture/ADRS.md#adr475-01--one-remote-stack) one remote stack | Kept. |
| [ADR475-02](../architecture/pm-server/architecture/ADRS.md#adr475-02--gin-adapter) adapters | Kept. The flag is in the Cobra command of each executable. The server hook is a plain function. No framework type enters domain or workflow code. |
| [0001](0001-cli-configuration.md) configuration | Kept. 0001 rejects repeated and malformed values of client controls. This record applies the same rule to this flag of `pm-server` and `pm-worker`. The operator configuration gets one optional field, `start_timeout`, and `probe_timeout` gets an upper bound of 5 minutes (see "Start budget"). |
| Plan design identity FND-20 and the dependency-stack readiness text ([plan](../architecture/pm-server/architecture/PLAN.md)) | Kept. The signal is one part of independently managed API and worker readiness. The operator still checks the dependencies. The `pm-worker` signal covers its own Temporal service and namespace checks, which fail closed, and the local registration of workflows and activities. It does not cover a registration in the Temporal service. |
| "Worker poll readiness" in the [Temporal report](../architecture/pm-server/reference/remote-design/tech/temporal/REPORT.md) | Kept as a check for the operator launcher, not met by the signal. The `pm-worker` signal does not claim poll readiness. An end-to-end scenario proves that a worker polls. A launcher that needs poll readiness must observe Temporal itself (FND-20, M12). |

This record supersedes nothing.

## Planned proof

Gherkin scenarios that run the real binaries. None exists yet. The scenarios are planned additions to the acceptance criteria of issue #72. Those additions are not accepted yet, and when they are accepted they own the exact titles. Each scenario uses the job-owned stack, except the refusal scenario, which stops before the process reads its configuration.

- `pm-server` answers a request at once after its ready signal.
- `pm-server` that cannot open its listener sends no ready signal.
- `pm-worker` sends its ready signal after it starts in its Temporal namespace.
- `pm-worker` that cannot start in its Temporal namespace sends no ready signal.
- Each process sends exactly one ready signal in its life. The starter reads end of file on the descriptor while the process still runs, which shows that the process closed the descriptor after the signal.
- The ready signal holds no secret or private value.
- Each executable refuses an unusable `--ready-fd` value with exit code 1, its refusal text and no signal.
- A process continues when its starter closed the ready channel, and it prints its diagnostic line.
- A process whose start does not complete within its `start_timeout` exits with code 1, prints its timer text and sends no signal. The starter reads end of file with no data. The acceptance criteria of issue #72 must name the start step that the scenario makes wait past `start_timeout`. That step must have no shorter bound of its own. One candidate is a dependency endpoint that accepts a connection and never answers while the process opens its resources (not checked).
- Each executable refuses a `start_timeout` that breaks a rule, with the configuration diagnostic and no signal.
- A process sends its signal in the same way on a descriptor of 256 or more.

The acceptance lane waits for the two signals, makes no repeated observation, keeps no deadline, and reads, or sends to a file, the standard output and standard error of each process. No scenario can prove this. A review check of these points at the lane start is one of the planned additions to the acceptance criteria of issue #72. Those additions are not accepted yet.

A process can start a query child while it still holds the descriptor. At the base commit this window may not be reachable yet (see the repository searches in Evidence). This record keeps the window for later wiring. The query child starts for a query or a sync activity. The pollers of `pm-worker` run before its signal and can take a sync activity. `pm-server` serves requests from its first Accept, and a full pipe that the starter does not read keeps the write and the close waiting while the process serves queries. In this window two layers keep the descriptor out of the child:

1. Close-on-exec on the handle. The exec of the child closes the descriptor.
2. The supervisor of the query child. Before it starts the child, it lists the open descriptors of its process in the descriptor directory `/dev/fd`, and it declares to the child each listed descriptor of 3 or more that has no close-on-exec. The child accepts a declared descriptor of any number from 3 up, closes each one, and refuses to run without the declaration. The child makes its report of remaining descriptors from the same kind of listing. If the supervisor cannot list the directory, it does not start the child. If the child cannot list it, the child does not run. Neither falls back to a fixed range. Principle: act on what exists. So this layer covers every descriptor number, and the flag contract does not depend on an internal choice of the query child.

No scenario can prove the first layer. The query child reports the descriptors that it inherited only to its supervisor, and no public API response or command output shows that report. So a scenario through the real binaries cannot see whether a child inherited the descriptor. This record does not add a public view of that internal report. The design review of the start code must check these points:

- The process refuses a descriptor that has close-on-exec set at the check, and then marks the handle close-on-exec, before it opens its configuration and before any code that can start a child process.
- The process keeps that one handle and makes no copy of the descriptor without close-on-exec.
- The process closes the handle after the write on every path, including a failed write, and never uses the number `n` again.
- The declare-and-close path of the query child stays in place. The supervisor and the child list the open descriptors by name in the descriptor directory, with no fixed range, skip a listed descriptor that is closed at the check, and stop when they cannot list the directory.
- The start timer is set before the configuration opens, only moves earlier, and stays in force until the process is ready or exits. The ready point, a failing start step and the timer change the start state at most once between them. A failing step changes it and writes its text before its cleanup. A timer that fires after a failed step exits with code 1 and writes nothing.
- The own GetSystemInfo and DescribeNamespace calls of `pm-worker` carry no retry options and accept only a success answer. No planned scenario makes the Temporal service answer "not implemented", so the design review checks that case.

The single-signal scenario shows that the process closes the descriptor after the signal while the process still runs.

## Evidence

- Read, not run: the start code of both executables, the server lifecycle and relay composition, the worker configuration, the Temporal Go SDK v1.48.0 worker start and poll code, and the Go `os/signal` and `os/exec` documentation for go1.27.1.
- Run: a local spike on Darwin with go1.26.5. A child process wrote one line to an inherited descriptor and closed it. The starter read the line and then end of file. A child that exited early gave end of file with no data. A write after the starter closed its read end returned EPIPE, and the child continued and exited with code 0. Without close-on-exec, a grandchild held the descriptor, and end of file came about 3 seconds late, when the grandchild exited. With close-on-exec, end of file came at once.
- Not run: the spike on Linux or with go1.27.1, any Gherkin scenario, and a measurement of the Temporal server long-poll default.
- Run: an offline `pm-server` built from the current main. A repeated `--config` printed `pm-server: invalid argument "b" for "--config" flag: --config may be supplied exactly once`, and `--config` with no value printed `pm-server: flag needs an argument: --config`. Both exited with code 1. This shows the flag parser texts that the refusals above use.
- Read: the Temporal Go SDK v1.48.0 calls GetSystemInfo in `ensureInitialized` and `loadCapabilities`, and DescribeNamespace in `loadNamespaceData` (`internal/internal_workflow_client.go`). Both continue on a "not implemented" answer. `cmd/pm-worker/main.go`, `internal/temporal/worker_client.go` and the relay client options in `internal/server/server.go` set no SDK logger. The owner groups come from `docs/architecture/pm-server/implementation/FILE-OWNERSHIP.json`. At the base commit the supervisor and the child check only descriptors 3 to 255 (see the searches below).
- Read, the bounds of the start steps: the admission probe of `pm-server` runs with `ProbeTimeout` (`internal/server/lifecycle.go`). The Temporal dial of `pm-worker` (`cmd/pm-worker/main.go:129` calls `DialWorkerClient`) runs with the configured dial timeout, at most 5 minutes (`internal/temporal/worker_client.go`). The SDK dial calls GetSystemInfo once, with a 5-second default timeout, and keeps the answer (SDK `internal/client.go:1605-1606`, `internal/internal_workflow_client.go:1658-1668` and `:1680-1681`). The SDK sets that default when the client options give no `GetSystemInfoTimeout` (SDK `internal/client.go:1655-1656`), and a repository search finds no `GetSystemInfoTimeout` in the Go files of pm-cli. `Worker.Start` uses the kept answer and calls DescribeNamespace (SDK `internal/internal_worker.go:1393`, `:1397` and `:1403`), with a 10-second RPC timeout when its context has no deadline (`internal/internal_utils.go`).
- Read: the SDK adds retry options to a call only when the context of the call holds retry configuration (SDK `internal/grpc_dialer.go:151-162`, `internal/common/retry/interceptor.go:114-116`). The retry interceptor of go-grpc-middleware v2.3.2, the version in `go.mod` (line 100), retries no call by default (`interceptors/retry/options.go:22-24`). `WorkflowService()` gives that same gRPC client (SDK `internal/internal_workflow_client.go:1643-1644`). So a direct call with a plain context carries no retry options, and these interceptors do not retry it.
- Read: the configuration requires only that `probe_timeout` is more than 0 (`internal/config/server.go:145-149`), and that `dial_timeout` is more than 0 and not more than 5 minutes (`internal/config/worker.go:105`). The admission probe failure stops the start before the server serves (`internal/server/lifecycle.go:121-124`). Outside Go files, only the configuration schema names `probe_timeout` (`git grep -n -F 'probe_timeout' 05ad74c -- ':!*.go'` gives `schemas/server-config.schema.json:42`).
- Read: `docs/architecture/pm-server/implementation/FILE-OWNERSHIP.json` at the base commit lists `internal/config/server.go` in entry F11; `internal/config/worker.go`, `schemas/server-config.schema.json` and `deploy/server.example.json` in R01; and `internal/warehouse/child.go` and `cmd/pm-query/main.go` in R12.
- Read: both executables build their flags with Cobra v1.10.2, which uses the flag parser pflag v1.0.10 (`go.mod`). The `invalid argument … flag: ` prefix comes from pflag (`errors.go`). A repository search finds `signal.NotifyContext` for interrupt, SIGTERM and SIGHUP in both executables and no other `signal.Notify` call in them or in `internal/`.
- Read: the SDK `RegisterWorkflow` and `RegisterActivity` add to the worker's own registry (`internal/internal_worker.go`). `Run` calls `Stop` on an interrupt, and `pm-worker` sets `WorkerStopTimeout` from its drain timeout (`cmd/pm-worker/main.go`).
- Read: the query child starts in `startChildAttempt` (`internal/warehouse/child.go:426`), which `(*QueryChild).Execute` calls (`internal/warehouse/child.go:168` and `internal/warehouse/child.go:187`). The query service calls `Execute` (`internal/warehouse/query.go:383`), and `internal/httpapi/warehouse.go:64` builds the query service with `warehouse.NewQueryService`. `warehouse.MaterializeParquetSegment` calls `Execute` (`internal/warehouse/parquet.go:223`), and `internal/sync/etl.go:197` calls `MaterializeParquetSegment`. The public query result (`warehouseQueryResponse` in `internal/httpapi/warehouse.go`) copies no field of the child report, and no code outside `internal/warehouse` reads the child report.
- Run at the base commit, the repository searches for the query child window:
  - `git grep -n -F -e 'ChildConfig{' -e 'WarehouseResources{' -e 'NewQueryChild(' 05ad74c -- '*.go' ':!*_test.go' ':!internal/warehouse/*'` found no match (exit 1).
  - The same search without `':!internal/warehouse/*'` found matches only in `internal/warehouse`: `child.go:101` (the definition of `NewQueryChild`), `child.go:141` (in `NewQueryChild`), `child.go:157` (in `ExecuteQueryChild`) and `parquet.go:189` (in `WriteSegment`).
  - `git grep -n -F -e 'NewETLCheckpoint(' -e 'ExecuteQueryChild(' -e 'WriteSegment(' -e 'Warehouse:' -e '.Warehouse =' 05ad74c -- '*.go' ':!*_test.go'` found only the definitions of `NewETLCheckpoint` (`internal/sync/etl.go:40`), `ExecuteQueryChild` (`internal/warehouse/child.go:155`) and `WriteSegment` (`internal/warehouse/parquet.go:187`).
  - So outside `internal/warehouse` no production Go file contains `ChildConfig{`, `WarehouseResources{` or `NewQueryChild(`. No production Go file contains a call of `NewETLCheckpoint`, `ExecuteQueryChild` or `WriteSegment`, or the text `Warehouse:` or `.Warehouse =`. For this reason the window above may not be reachable yet at the base commit. These searches look for text. They do not prove that no other path reaches the query child.
- Run at the base commit, the searches for the start limits, each with `git grep -n -F` at `05ad74c`:
  - `descriptorScanLimit` in production Go files: `cmd/pm-query/main.go:37` (the value 256), `:244` (the declaration check) and `:294` (the report), and `internal/warehouse/child.go:34` (the value 256) and `:511` (the sweep).
  - `start_timeout` and `StartTimeout` in Go, JSON and Markdown files: no match (exit 1).
  - `/dev/fd` in production Go files: no match (exit 1).
  - `Temporal worker dependency is unavailable`: `cmd/pm-worker/main.go:139`, the current text of a failed dial.
  - `-timeout 40m` in `scripts/ci/check-bdd.sh`: line 37. `timeout-minutes` in `.github/workflows/bdd.yml`: 60 for the job (line 21) and 50 for the suite step (line 33).
- Run: a second local spike on Darwin 25.6.0 with go1.26.5.
  - A process copied the write end of a pipe to descriptors 300 and 5000, with close-on-exec off on 300 and on on 5000. A listing of the names in `/dev/fd` gave 0 to 7, 300 and 5000. Descriptors 6 and 7 were closed at the check, and 6 was the descriptor of the listing itself. `F_GETFD` gave close-on-exec off for 300 and on for 5000. A child from `exec` (`/bin/ls /dev/fd`) listed 300 and not 5000. A check of 3 to 255 does not see 300. A listing that reads the status of each entry failed with `fstatat /dev/fd: bad file descriptor`, and a listing of names only worked.
  - The soft open-file limit of the process was 245,760.
  - A child with a 2-second timer blocked in the open of a FIFO that had no writer. The starter made one blocking read with no deadline and got end of file with no data after 2.00 seconds. The child exited with code 1.
  - The starter stopped the same kind of child with SIGSTOP at once after the start. For 4 seconds the starter got no data and no end of file. After SIGCONT the child ran its own 2-second timer, and the starter got end of file 2.01 seconds later. The child exited with code 1.
- Not run: the second spike on Linux, because no container runtime was available on the host. On Linux, `/dev/fd` is expected to name the open descriptors of the process through `/proc/self/fd` (not checked).
- Run: `pm-server --config --secrets-directory x` printed `pm-server: unknown command "x" for "pm-server"` and exited with code 1. The parser took `--secrets-directory` as the value of `--config`.
- Not checked: that the Temporal service keeps a task in its queue until a poller takes it. The Temporal server source was not read.
- Not checked: the facts about the systemd notification protocol (a socket path in the environment, with a length limit), and whether a Temporal authorizer requires a DescribeTaskQueue permission.
- Not checked: the bounds of `server.Open` (the store, the identity provider discovery and the artifact resources), of the worker runtime open, and of any other call inside `Worker.Start`. Also not checked: any retry inside the gRPC library itself. The start budget bounds all of these steps, so the start result does not depend on these facts.
- Not checked: whether native libraries in the query child hold descriptors without close-on-exec.
- Not checked: whether an image or a Compose starter can pass an inherited descriptor.

## Revisit trigger

- Packaging needs a service-manager protocol, for example systemd notification, or an image or a Compose starter cannot pass an inherited descriptor.
- An operator needs a signal that the Temporal service lists or answered the worker poll, or the worker gets a per-process identity.
- A supported target has no inherited descriptors.
- A starter needs a signal for a later state change, for example drain or the loss of a dependency.
- A Temporal SDK upgrade changes what `Worker.Start` checks before it returns, or changes the contract of `Start` and `Run`.
- A flag-parser upgrade changes a refusal text.
- A deployment needs a start of more than 15 minutes.
- A supported system has no descriptor directory `/dev/fd`, or its listing leaves out an open descriptor.
- A Temporal SDK or go-grpc-middleware upgrade changes how retry options apply to a direct call.
