# 0005: Ready signal from pm-server and pm-worker to their starter

Date: 2026-11-12. Status: accepted by the owner on 2026-11-13. This record does not authorize application work. Every flag, text and check below is a planned contract until the owning implementation creates and runs it. Issue: #72.

Amended on 2026-11-15: the starter owns the start time limit, and the process has no start timer. The owner accepted the amendment on 2026-11-15.

## Context

A starter starts `pm-server` and `pm-worker` as separate processes. The starter can be an acceptance-suite step, an operator launcher or a service manager. The operator owns this lifecycle, and a client invocation never starts a backend ([ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery)). The starter must know when each process completed its start.

Today neither process reports this. The acceptance lane in PR #261 repeats an HTTP request or a Temporal poller look until one passes. This record sets a requirement: to learn that each process completed its start, a starter waits for one event from each process and repeats no observation. With a repeated observation, the result and the duration of a start depend on a polling interval that the starter chooses. The starter keeps one time limit on its wait, as a supervisor keeps a start limit (see "Start limit"). So the single blocking wait of the starter ends with the line, with end of file, or at the limit. An operator launcher has the same need as the acceptance lane.

The plan already sets the meaning of readiness:

- A listening socket is not readiness. The operator checks PostgreSQL schema compatibility, secret-key availability, the Temporal namespace and worker registration, and object integrity access ([plan, dependency stack](../architecture/pm-server/architecture/PLAN.md)).
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
| Request | `--ready-fd <n>` on `pm-server` and on `pm-worker`. The value has only ASCII decimal digits, has no leading zero, fits in a 32-bit signed `int`, the type of a descriptor in the system calls, and is 3 or more. The flag may appear once. Without the flag, the process sends no signal. |
| Refusal | The process stops with exit code 1, one line on standard error and no signal. It does this before it opens or reads its configuration. See "Refusals" below for the texts and the order. |
| Channel | Descriptor `n`, inherited from the starter. When the process checks the flag, it confirms that the descriptor has no close-on-exec flag, which shows that it was inherited, on the condition that each descriptor that the process opens before the check has close-on-exec set. Then it puts the descriptor in one file handle and marks it close-on-exec, so that a child process does not hold it. The process uses only that handle. After it closes the handle, it never uses the number `n` again. |
| Signal | The exact bytes `pm-server ready` or `pm-worker ready`, then one line feed, in one write. Then the process closes the handle. The write runs apart from the serving path, so a full pipe cannot stop `pm-server` from serving or `pm-worker` from working. The signal then waits until the starter reads. |
| No signal | End of file with no data. The process exit closes the descriptor. |
| Content | The fixed text only. No secret, credential, host name, address, endpoint, path or identifier. |
| Failed write | The process writes `pm-server: the ready signal write failed. The process continues.` or the same text with `pm-worker:` on standard error once. It continues, and its exit code does not change. A standard error pipe that has no reader still stops the process with SIGPIPE at that write, as it does today at any write to standard error. |
| Starter | The starter must give the process only the write end of a pipe as descriptor `n`. It must make sure that no other process that it starts holds that write end. After it starts the process, it must close its own copy of the write end, or it never reads end of file. It must read until it gets the line or end of file, or until its own start limit passes. At the limit it stops the process (see "Start limit"). During the start, it must read each pipe that it gives as standard output or standard error, or give a destination that cannot fill, for example a file. The starter owns these pipes, and a full pipe blocks each write of the process, also its error text. |
| Start limit | The starter owns the start time limit. The process has no start timer, no start setting and no timer text. See "Start limit" below. |

### Refusals

The process checks in this order and stops at the first failure.

| Order | Case | Text after `pm-server: ` or `pm-worker: ` |
| --- | --- | --- |
| 1, while the flags are read | `--ready-fd` with no value | `flag needs an argument: --ready-fd` (the text of the flag parser) |
| 1, while the flags are read | A second `--ready-fd` | `invalid argument "<value>" for "--ready-fd" flag: --ready-fd may be supplied exactly once` |
| 1, while the flags are read | A value that is empty, has a character other than an ASCII digit (for example a sign), has a leading zero, does not fit in a 32-bit signed `int`, or is below 3 | `invalid argument "<value>" for "--ready-fd" flag: --ready-fd requires only the digits 0-9, with no leading zero, for a number from 3 to the int maximum` |
| 2, after the `--config` and `--secrets-directory` checks | A descriptor that is not open for writing, or that has the close-on-exec flag set when the process checks it | `--ready-fd requires an inherited descriptor that is open for writing` |

The flag parser adds the `invalid argument … flag: ` prefix and repeats the given value, which is not a secret. The check of a second flag comes before the check of its value. The process refuses a leading zero, for example `03`, so that no value can be read as an octal number. Decision 0001 rejects malformed values of client controls, and this record applies the same rule to this flag. Go opens each descriptor of its own with close-on-exec set, and a descriptor that a starter passes arrives without it. So a descriptor with the flag set at the check belongs to the process itself, and the process refuses it. The flag parser gives "flag needs an argument" only when `--ready-fd` is the last argument. When another argument follows, the parser takes it as the value, so the value check refuses it. The flag parser owns its own texts, so an upgrade of the parser can change them.

A process sends at most one signal in its life. A process that fails before it is ready sends no signal, exits with code 1 and prints its current error text on standard error. A process that the starter stops before it is ready sends no signal, and its exit code stays as it is today.

`pm-server` is ready when its admission probe passed, its state is serving, and its HTTP accept loop calls Accept for the first time. Temporal and the relay are not part of this. `/health/live`, `/health/ready` and the `Readiness` type do not change.

`pm-worker` is ready only while its context is not done. A `pm-worker` whose context is done before it is ready, for example after SIGTERM, sends no signal. It is ready when these steps passed, in this order:

1. After the dial, `pm-worker` calls GetSystemInfo once and DescribeNamespace once for the configured namespace, with no retry options. It accepts only a success answer, and the DescribeNamespace answer must name the configured namespace. Any other result, including "not implemented", stops the start with exit code 1 and no signal. A failed GetSystemInfo call prints `pm-worker: Temporal worker dependency is unavailable`, the text of a failed dial. A failed DescribeNamespace call, or an answer that names another namespace, prints `pm-worker: Temporal namespace is unavailable`. The text depends on the call, not on the status code. A namespace that does not exist takes the DescribeNamespace path. Today that case prints `pm-worker: Temporal worker stopped after a fatal runtime error` (`cmd/pm-worker/main.go:185-189`), and this record replaces that text with `pm-worker: Temporal namespace is unavailable`. The dial and the two calls finish within the configured `dial_timeout`, counted from the start of the dial. Principle: fail closed. The SDK continues when the service answers either call with "not implemented" (see Evidence), so the SDK start alone does not prove these answers. The SDK already makes these two kinds of call with the same client identity, so they need no new permission.
2. The process registered its workflows and activities in its own SDK worker, which makes no Temporal service call.
3. The Temporal worker start returned without an error, and the workflow and activity pollers started.

So the signal proves each of these steps and nothing more. Principle: a signal proves exactly what it claims. The signal does not say that the Temporal service lists the poller or answered a poll. This record expects that the Temporal service keeps a task in its queue until a poller takes it, inside the task's own timeouts. Then work that a client sends after the signal waits for the first poll and is not lost. This expectation is not checked: the Temporal server source was not available for this record.

### Start limit

The starter owns the start time limit. A total protocol needs a bound: before the bound ends, no observer can tell a slow start that will complete from a start that will never complete. So the result of a slow start depends on a bound, and no design can remove this dependence. The bound belongs to the starter, as the start limit of a service belongs to its supervisor: systemd has `TimeoutStartSec`, s6 has `s6-svwait -t`, and Kubernetes has a startup probe (reported in the design brief of 2026-11-15, not checked again in this record). In each case the supervisor, not the service, owns the limit. This record already uses the s6 readiness pattern: one line on a descriptor that the starter names. Principle: the owner of the process lifecycle owns its time limits ([ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery)).

The process has no start timer, no start setting and no timer text. The starter reads the ready pipe with one blocking reader and one deadline: the start limit, counted from the start of the process. The deadline covers the whole read, the ready line and the end of file that follows when the process closes the descriptor after its one write. Readiness is the ready line. A starter that needs only readiness acts at the line and does not wait for end of file. A starter that checks the close after the signal waits for end of file up to the same deadline. If the deadline passes first, the starter fails that check and runs the stop sequence below. No wait repeats. A start can end in these ways:

- A start with a ready channel (`--ready-fd`) that completes: the starter reads the ready line and then end of file, or end of file with no data. End of file with no data comes when the process fails before it is ready and exits with code 1 and its current error text, or when a stop by the starter or by SIGPIPE at a write to standard error ends it (see "Failed write"). A stop by the starter keeps today's exit code.
- A start without the flag: the process sends no signal. A successful start gives the starter no event, and this record defines no other observation for it.
- A start that hangs: the process neither writes the line nor exits. Without an effective action by a supervisor, it can wait with no end (see Evidence: a FIFO as the secrets directory).
- A start limit that passes: the starter ends its wait and runs the stop sequence below. The limit itself does not make the process exit or close the descriptor, so the starter assumes neither.

The acceptance suite follows the owner's test-wait rule of 2026-11-15: "A test waits only for an event from the product: a ready line, one line of output, the answer to one request, end-of-file, a process exit or a --wait result. Each wait has one time limit. Nothing repeats."

The stop sequence. The starter keeps one observation of the process exit for the whole sequence, for example one channel that closes when its wait for the process returns. This observation waits only for the exit of the process, not for end of file on the output pipes of the process. The starter waits on it in two phases, and each phase has its own time limit. Nothing repeats: the starter sends each signal at most once and makes no check again.

1. The starter sends SIGTERM. Both executables cancel their run context on SIGTERM, so a start step that uses the context ends, and the process runs its normal stop steps.
2. Phase 1: the starter waits on the exit observation, up to the stop limit.
3. If the exit has not come, the starter sends SIGKILL. Phase 2: it waits on the same exit observation, up to the kill limit.
4. The starter reports the failed start. The report holds a bounded snapshot of the standard output and standard error that it read up to that time, and the result: the process exited after SIGTERM, exited after SIGKILL, or did not exit within the kill limit. After the kill limit, the starter does not wait for end of file on the output pipes. It closes its read ends, so that its pipe readers end. When the process did not exit, the report names its process ID as a possible survivor, and the starter leaves the process.

The acceptance suite uses three named constants in its shared harness:

- Start limit: 120 seconds. This value is provisional.
  - Known bounds of some start steps. For `pm-server`, the store open and the admission probe each have `probe_timeout` (default 5 seconds), and OIDC discovery has a fixed 15 seconds: 25 seconds for these steps. For `pm-worker`, the store open and the Temporal dial with the two own calls each have `dial_timeout` (5 seconds in the suite), and the DescribeNamespace call inside `Worker.Start` has a 10-second RPC timeout (Evidence, the bullet "Read, the bounds of the start steps"; reported, not checked again in this amendment): 20 seconds for these steps. These sums are not a full bound, because the bounds of `server.Open`, of the worker runtime open and of the other calls inside `Worker.Start` are not checked (see Evidence). They show only that a limit below them can end a start before such a step reports its own failure.
  - No measurement of one start exists. The bdd logs of the two runs of PR #261 show only the total time of the acceptance package: 54.2 seconds at `4be05ea` and 58.3 seconds at `652de58`. Each start of `pm-server` and `pm-worker` in those runs, up to the moment when the lane saw it ready, took less than that total (see Evidence). This is an upper bound, not a measurement of a start.
  - 120 seconds is about twice the larger total, so a start as slow as the whole observed package still passes.
  - Trade-off: a healthy start that is slower than the limit fails its step, and the test-wait rule allows no second try. A start that hangs fails only after the start limit and the stop sequence, up to 120 + 45 + 10 seconds. Each start records the time from the start to the line or end of file in its output. The value stays provisional until a reviewer checks it against these recorded durations after the first ten bdd runs that start the processes. A change is proposed only with them.
- Stop limit: 45 seconds, the value of the reviewed lane in PR #261. It is more than the default `drain_timeout` of `pm-server` (30 seconds) and the drain timeouts of the suite (5 seconds), so a normal stop can complete inside it.
- Kill limit: 10 seconds.

Operators: a supervisor of `pm-server` or `pm-worker` has this minimum duty. It bounds its wait for the ready line or end of file. It bounds its wait for the exit after SIGTERM. It bounds its wait for the exit after SIGKILL. It reports a process that did not exit after SIGKILL. PM adds no setting for these limits. The operator section in [setup](../development/setup.md) states this duty. Packaging (M12) chooses the values and the implementation for the images and launchers that PM ships.

One residual stays: a process that does not exit after SIGKILL, for example a thread in an uninterruptible kernel wait on hung storage. The kernel ends such a process only when the wait ends. The waits of the starter still end at their limits, so the starter reports the failure and the possible survivor. No design in the starter can end or reap such a process. A stopped process is not this residual, because SIGKILL ends a stopped process (POSIX signal rules; reported, not checked again in this amendment). In the acceptance suite, the outer guards (`go test -timeout 40m`, the step limit of 50 minutes and the cleanup of the job container) bound how long the run waits. This record does not claim that they end or reap the process.

### Server readiness options

| Option | Assessment for PM |
| --- | --- |
| Signal when the admission probe passed, the state is serving and the accept loop calls Accept for the first time | Selected. The process knows each fact in its promise. |
| Signal when the listener opens | Rejected. A listening socket is not readiness ([plan, dependency stack](../architecture/pm-server/architecture/PLAN.md)). |
| Signal only when Temporal and the relay are also available | Rejected. The plan keeps admission available during a Temporal outage, up to a cap, and the relay connects to Temporal only when it needs to. |

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
| A bounded retry of the two read-only calls within `dial_timeout` | Rejected. AGENTS.md allows a retry only when the declared operation and observed attempt state make replay safe, and both calls are read-only, so that rule alone allows it. But a retry of a readiness check is a repeated observation, and its result depends on a retry interval. The starter or the service manager decides whether to start the process again. Cost: a short transient error stops the start (see Consequences). |
| A gRPC interceptor that turns "not implemented" into an error for the two SDK calls | Rejected. It depends on which calls the SDK makes in its start and how it treats each error, and an SDK upgrade can change that without notice. |
| Keep the SDK behaviour and state the limit | Rejected. The owner does not accept the limit. |
| Start budget in the process (a `start_timeout` setting and an in-process timer) | Rejected by the amendment of 2026-11-15. The supervisor of a process owns its start limit, so the timer repeated the limit of every starter. It added an operator setting, a breaking bound on `probe_timeout`, a fixed 15-minute bound before the configuration is valid, and its own texts. No scenario could show that the timer ended a start within the configured value without a time check in the test. |
| The starter keeps one start time limit | Selected by the amendment of 2026-11-15. The supervisor owns the process lifecycle and its limits, as systemd, s6 and Kubernetes do. The process needs no timer, no setting and no timer text. Cost: each starter chooses its own value, and a starter without a limit can wait with no end. |
| A bound for each start step, with no timer | Rejected. The SDK worker start takes no context, and a later step can come without a bound, so the total bound depends on a review of every step. |
| A fixed start budget in the code | Rejected: no scenario can run it in a usable time, and a deployment cannot change it. |
| The starter also waits for the process exit | Not selected. It does not end the wait for a live process that hangs. |
| The supervisor and the child list the open descriptors | Selected. Act on what exists: every descriptor number is covered, with no fixed range. |
| Limit `--ready-fd` to 3 to 255 | Rejected. It couples the public flag to an internal choice of the query child. |
| Check every number up to the open-file limit | Rejected. It is still a fixed range, and its cost grows with the limit (245,760 in the Darwin spike), not with the open descriptors. |
| The child refuses each descriptor without close-on-exec that the supervisor did not declare | Not selected. Native libraries in the child can hold such descriptors (not checked), so the child could refuse a sound start. |
| A socket pair that carries the credentials of the sender | Rejected. It stops a line from another process, but not the leak and not a start that hangs, and it replaces the reviewed channel, refusal texts and spike. |

## Consequences

- Two executables get a new operator contract: the flag, its help text, the texts above and an operator section in [setup](../development/setup.md). The operator section also states the duties of the starter. A later change to the contract is a breaking change for starters.
- The operator configuration does not change. A supervisor of `pm-server` or `pm-worker` has the minimum duty in "Start limit": it bounds its wait for the start, its wait after SIGTERM and its wait after SIGKILL, and it reports a process that did not exit. A start that waits on a step with no bound of its own ends only when its starter stops it and the stop succeeds (see Evidence: a FIFO as the secrets directory). The operator section states this duty of the starter.
- When the start limit passes, the starter sends SIGTERM, and the process runs its normal stop steps. If the process does not exit within the stop limit, the starter sends SIGKILL, which runs no stop step and leaves the same state as any stop by SIGKILL before ready. Principle: such a stop is the same as a crash, it adds no new failure mode, and crash safety stays with the relay and activity designs. A process can already stop at any instant, before or after ready, for example by an operator SIGKILL. At the base commit, the relay commands and the activities that can run before ready already cover such a stop (see Evidence). The relay of `pm-server` keeps a start command pending until it records the binding, and a later claim starts the workflow once, reconciles the started one, or ends the hand-off as repair required or unsafe to restart with no new start. An activity of `pm-worker` that a poller took before ready ends by its Temporal timeouts, and its retry reconciles a recorded send attempt and does not send it again. When SIGKILL ends `pm-worker` while it holds a workflow task, that task runs no provider effect: workflow code makes no provider call (AGENTS.md), and the production Go files start no local activity (no match for `ExecuteLocalActivity` or `LocalActivityOptions`). This record expects that Temporal gives the task to a poller again after its workflow task timeout, but it did not check this against the Temporal documentation or the server source.
- `pm-worker` makes two more Temporal calls at each start, GetSystemInfo and DescribeNamespace, once each. A short transient error on either call now stops the start with exit code 1, and the starter or the service manager decides whether to start the process again. The acceptance suite does not start it again, because its test-wait rule says "Nothing repeats" (owner decision of 2026-11-15). `pm-worker` no longer starts against a Temporal service that does not implement either call. A missing namespace now prints `pm-worker: Temporal namespace is unavailable` in place of `pm-worker: Temporal worker stopped after a fatal runtime error`.
- The query child keeps two protection layers as its final contract (see "Planned proof"). Layer 2, the listing of the descriptor directory `/dev/fd`, comes in a separate hardening change outside issue #72. From that change on, the query child needs `/dev/fd`: where the directory is missing, the query child does not start, and nothing falls back to a fixed range. Until that change lands, the supervisor and the child check only descriptors 3 to 255. In that interim, layer 1 alone keeps a ready descriptor of 256 or more out of a child: one layer, not two.
- The contract needs inherited descriptors. Go `exec.Cmd.ExtraFiles` does not work on Windows. These executables have no Windows target.
- A systemd unit with `Type=notify` cannot use this signal directly. Packaging (M12) decides an adapter or a second request form. Packaging (M12) also decides how an image or a Compose starter passes the descriptor and reads the signal.
- `pm-worker` stops using the SDK `Run` call. It calls `Start` with an `OnFatalError` callback and then waits for its context or a fatal error. When its context ends, it calls `Stop`, as `Run` does today, so the drain timeout (`WorkerStopTimeout`) still applies. Reason: in SDK v1.48.0, `Run` after `Start` panics when a fatal error stopped the worker first. Later changes must keep this. A failure of the own DescribeNamespace call in step 1 of the `pm-worker` readiness, for example for a namespace that does not exist, which today prints `pm-worker: Temporal worker stopped after a fatal runtime error` (`cmd/pm-worker/main.go:185-189`), now prints `pm-worker: Temporal namespace is unavailable`. A failure of the DescribeNamespace call inside the SDK `Worker.Start` keeps its current text. The other error texts do not change.
- The worker signal is weaker than "worker poll readiness" in the Temporal reference. A worker whose polls the Temporal service refuses still sends the signal, because the SDK retries most poll errors. An end-to-end scenario, in which a worker takes and completes work, proves polling (issue #72, part b). The signal does not.
- `pm-server` still starts its relay before the listener and the admission probe. A `pm-server` that never becomes ready runs its relay until its start fails or its starter stops it. This record does not change that.
- The record adds no dependency.
- Each main package holds its own descriptor code, so the two copies can drift. Control: the planned scenarios that check the signal content and the single signal run both executables.

## Decisions that this record touches

| Decision | Effect |
| --- | --- |
| [ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery) operator lifecycle | Kept. The starter owns the process lifecycle. No `pm` command starts these processes or waits for them. |
| [ADR475-01](../architecture/pm-server/architecture/ADRS.md#adr475-01--one-remote-stack) one remote stack | Kept. |
| [ADR475-02](../architecture/pm-server/architecture/ADRS.md#adr475-02--gin-adapter) Gin adapter | Kept. The flag is in the Cobra command of each executable. The server hook is a plain function. No framework type enters domain or workflow code. |
| [0001](0001-cli-configuration.md) configuration | Kept. 0001 rejects repeated and malformed values of client controls. This record applies the same rule to this flag of `pm-server` and `pm-worker`. The operator configuration does not change. |
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
- A process sends its signal in the same way on a descriptor of 256 or more.

The acceptance lane waits for the ready signal of each process that it starts, with one blocking read and the start limit of the suite. It makes no repeated observation, and it reads, or sends to a file, the standard output and standard error of each process. No scenario can prove this. A review check of these points at the lane start is one of the planned additions to the acceptance criteria of issue #72. Those additions are not accepted yet.

A process can start a query child while it still holds the descriptor. At the base commit this window may not be reachable yet (see the repository searches in Evidence). This record keeps the window for later wiring. The query child starts for a query or a sync activity. The pollers of `pm-worker` run before its signal and can take a sync activity. `pm-server` serves requests from its first Accept, and a full pipe that the starter does not read keeps the write and the close waiting while the process serves queries. In this window two layers keep the descriptor out of the child:

1. Close-on-exec on the handle. The exec of the child closes the descriptor.
2. The supervisor of the query child, from the separate hardening change outside issue #72 (see Consequences). Before it starts the child, it lists the open descriptors of its process in the descriptor directory `/dev/fd`, and it declares to the child each listed descriptor of 3 or more that has no close-on-exec. The child accepts a declared descriptor of any number from 3 up, closes each one, and refuses to run without the declaration. The child makes its report of remaining descriptors from the same kind of listing. A listing counts only when it reads the whole directory with no error, also no error after a partial result, and when it contains the open descriptor of the listing itself. If the supervisor gets any other listing, it does not start the child. If the child gets any other listing, the child does not run. Neither falls back to a fixed range. Principle: act on what exists. So this layer covers every descriptor number, and the flag contract does not depend on an internal choice of the query child.

No scenario can prove the first layer. The query child reports the descriptors that it inherited only to its supervisor, and no public API response or command output shows that report. So a scenario through the real binaries cannot see whether a child inherited the descriptor. This record does not add a public view of that internal report. The design review of the start code must check these points:

- The process refuses a descriptor that has close-on-exec set at the check, and then marks the handle close-on-exec, before it opens its configuration and before any code that can start a child process.
- The process keeps that one handle and makes no copy of the descriptor without close-on-exec.
- The process closes the handle after the write on every path, including a failed write, and never uses the number `n` again.
- The declare-and-close path of the query child stays in place. From the separate hardening change that adds layer 2: the supervisor and the child list the open descriptors by name in the descriptor directory, with no fixed range, skip a listed descriptor that is closed at the check, and stop on any listing error, also one after a partial result, and on a listing that does not contain the open descriptor of the listing itself.
- The own GetSystemInfo and DescribeNamespace calls of `pm-worker` carry no retry options, accept only a success answer, and choose the failure text by the call, not by the status code. No planned scenario makes the Temporal service answer "not implemented", so the design review checks that case.

The single-signal scenario shows that the process closes the descriptor after the signal while the process still runs.

Go developer tests prove three internal facts that no observer outside the process can see. They run in CI (owner decision of 2026-11-15). Their label is "proven by Go tests in CI, not acceptance proof":

- `pm-server` writes the signal only after the admission probe passed. One test uses a probe that fails, and one uses a probe that passes.
- A full or unread pipe does not stop `pm-server` from serving or `pm-worker` from working.
- The handle has close-on-exec after the check.

## Evidence

- Read, not run: the start code of both executables, the server lifecycle and relay composition, the worker configuration, the Temporal Go SDK v1.48.0 worker start and poll code, and the Go `os/signal` and `os/exec` documentation for go1.27.1.
- Run: a local spike on Darwin with go1.26.5. A child process wrote one line to an inherited descriptor and closed it. The starter read the line and then end of file. A child that exited early gave end of file with no data. A write after the starter closed its read end returned EPIPE, and the child continued and exited with code 0. Without close-on-exec, a grandchild held the descriptor, and end of file came about 3 seconds late, when the grandchild exited. With close-on-exec, end of file came at once.
- Not run: the spike on Linux or with go1.27.1, any Gherkin scenario, and a measurement of the Temporal server long-poll default.
- Run: an offline `pm-server` built from the current main. A repeated `--config` printed `pm-server: invalid argument "b" for "--config" flag: --config may be supplied exactly once`, and `--config` with no value printed `pm-server: flag needs an argument: --config`. Both exited with code 1. This shows the flag parser texts that the refusals above use.
- Read: the Temporal Go SDK v1.48.0 calls GetSystemInfo in `ensureInitialized` and `loadCapabilities`, and DescribeNamespace in `loadNamespaceData` (`internal/internal_workflow_client.go`). Both continue on a "not implemented" answer. `cmd/pm-worker/main.go`, `internal/temporal/worker_client.go` and the relay client options in `internal/server/server.go` set no SDK logger. At the base commit the supervisor and the child check only descriptors 3 to 255 (see the searches below).
- Read, the bounds of the start steps: the admission probe of `pm-server` runs with `ProbeTimeout` (`internal/server/lifecycle.go`). The Temporal dial of `pm-worker` (`cmd/pm-worker/main.go:129` calls `DialWorkerClient`) runs with the configured dial timeout, at most 5 minutes (`internal/temporal/worker_client.go`). The SDK dial calls GetSystemInfo once, with a 5-second default timeout, and keeps the answer (SDK `internal/client.go:1605-1606`, `internal/internal_workflow_client.go:1658-1668` and `:1680-1681`). The SDK sets that default when the client options give no `GetSystemInfoTimeout` (SDK `internal/client.go:1655-1656`), and a repository search finds no `GetSystemInfoTimeout` in the Go files of pm-cli. `Worker.Start` uses the kept answer and calls DescribeNamespace (SDK `internal/internal_worker.go:1393`, `:1397` and `:1403`), with a 10-second RPC timeout when its context has no deadline (`internal/internal_utils.go`).
- Read: the SDK adds retry options to a call only when the context of the call holds retry configuration (SDK `internal/grpc_dialer.go:151-162`, `internal/common/retry/interceptor.go:114-116`). The retry interceptor of go-grpc-middleware v2.3.2, the version in `go.mod` (line 100), retries no call by default (`interceptors/retry/options.go:22-24`). `WorkflowService()` gives that same gRPC client (SDK `internal/internal_workflow_client.go:1643-1644`). So a direct call with a plain context carries no retry options, and these interceptors do not retry it.
- Read: the configuration requires only that `probe_timeout` is more than 0 (`internal/config/server.go:145-149`), and that `dial_timeout` is more than 0 and not more than 5 minutes (`internal/config/worker.go:105`). The admission probe failure stops the start before the server serves (`internal/server/lifecycle.go:121-124`). `pm-server` opens its store within `probe_timeout` (`internal/server/server.go:161`, `internal/app/runtime.go:103`), runs OIDC discovery within a fixed 15 seconds (`internal/app/runtime.go:122`, `internal/auth/discovery.go:40`) and runs its admission probe within `probe_timeout` (`internal/server/lifecycle.go:116`). `pm-worker` opens its store within `dial_timeout` (`internal/app/worker_runtime.go:97`) and dials Temporal within `dial_timeout` (`internal/temporal/worker_client.go:419`). Outside Go files, only the configuration schema names `probe_timeout` (`git grep -n -F 'probe_timeout' 05ad74c -- ':!*.go'` gives `schemas/server-config.schema.json:42`). Go files and operator configurations outside the repository were not searched.
- Read, the crash safety of the work that can run before ready:
  - The relay claims a pending outbox command with a lease of at most 5 minutes and a fence. A command whose lease has ended can be claimed again, and each claim raises the fence (`internal/store/migrations/postgres/0009_workflow_reconciliation.sql:165`, `:176-184` and `:190-193`). The command stays pending until an acknowledgement that requires the exact current claim: its owner, its fence and a lease that has not ended (`internal/store/outbox.go:212-222`). The relay records the binding and acknowledges the claim in one transaction: `CommitWorkflowBinding` writes the binding record (`internal/store/workflows.go:257`) and calls `AcknowledgeOutbox` (`internal/store/workflows.go:260`), and the relay calls it inside one scoped transaction (`internal/temporal/outbox.go:419-420`).
  - Before a start, the relay looks up the stable workflow ID and reconciles an execution that exists (`internal/temporal/outbox.go:226-229`). It writes a no-dispatch witness only for the current claim of a run that has no binding observation and no dispatch (`internal/store/workflows.go:166-205`). It starts with that workflow ID, conflict policy FAIL and reuse policy REJECT_DUPLICATE, and on an already-started answer it looks up and reconciles a matching execution (`internal/temporal/outbox.go:247-283`). When the evidence does not match, when the witness is refused, or when the started execution is not found, the hand-off ends as repair required or unsafe to restart with no new start (`internal/temporal/outbox.go:230-233`, `:240-243` and `:271-280`).
  - Activities have a schedule-to-close and a start-to-close timeout, and a heartbeat timeout when the caller asks for one, with no retry policy set in these options (`internal/workflows/options.go:30-37`). So the default retry policy of the Temporal service applies, which has unlimited attempts unless the dynamic configuration of the service overrides it (SDK `internal/activity.go:151-161`). Before a provider send, the worker records the dispatch attempt as possibly sent (`internal/app/worker.go:793-803`), inside the transaction of `prepareWorkerUnit` (`internal/app/worker.go:522-531` and `:1438`, `internal/store/workers.go:78`). That transaction commits before `prepareWorkerUnit` returns at `internal/app/worker.go:457`, and only then does the worker call `handler.Execute` (`internal/app/worker.go:487`). In reconcile mode it calls `handler.Reconcile` in place of `handler.Execute` (`internal/app/worker.go:484-485`). An attempt that finds a recorded attempt reconciles it, or fails with a conflict when it is a fresh send, and records no second attempt (`internal/app/worker.go:804-812` and `:821-824`).
  - Each of these transactions is its own. `WithinTransaction` acquires a connection and begins a new transaction (`internal/store/transaction.go:74` and `:90`), reads no outer transaction, and commits before it returns (`internal/store/transaction.go:129`), or rolls back on an error (`internal/store/transaction.go:122-127`). `WithinScope` and `WithinWorkerScope` use it (`internal/store/scope.go:32`, `internal/store/workers.go:78`).
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
  - A child called `os.OpenRoot` on a FIFO that had no writer, with a 2-second timer. The open did not return, the timer ended the child with exit code 1, and the starter got end of file with no data after 2.01 seconds.
  - A child with a 2-second timer blocked in the open of a FIFO that had no writer. The starter made one blocking read with no deadline and got end of file with no data after 2.00 seconds. The child exited with code 1.
  - The starter stopped the same kind of child with SIGSTOP at once after the start. For 4 seconds the starter got no data and no end of file. After SIGCONT the child ran its own 2-second timer, and the starter got end of file 2.01 seconds later. The child exited with code 1.
- Run, reported by a reviewer of this record and not repeated by the author: at the base commit, `pm-server` and `pm-worker` with a valid offline configuration and a FIFO as `--secrets-directory` waited 72.85 and 39.92 seconds, until the FIFO was opened for writing. Then each exited with code 1 and its secrets text (`operator secret directory is unavailable`, `worker secrets are unavailable`).

- Read and run, for the start limit of the acceptance suite (amendment of 2026-11-15):
  - The default `probe_timeout` is 5 seconds and the default `drain_timeout` of `pm-server` is 30 seconds (`internal/config/server.go:38-39` at `c59aa20`). OIDC discovery has a fixed 15 seconds (`internal/auth/discovery.go:40`).
  - The lane of PR #261 at `4be05ea` sets the `pm-worker` `dial_timeout` and `drain_timeout` to 5 seconds (`tests/acceptance/lane_test.go:49-50`) and the `pm-server` `drain_timeout` to 5 seconds (`:305`). Its server section sets no `probe_timeout` (`:302-306`), so the default applies. It stops a process with SIGTERM, a stop limit of 45 seconds and then SIGKILL (`:37`, `:392-410`). After SIGKILL it waits for the exit with no limit (`:405`).
  - Both executables cancel their run context on SIGTERM, interrupt and SIGHUP (`cmd/pm-server/main.go:22`, `cmd/pm-worker/main.go:27` at `c59aa20`).
  - Run in CI: bdd run 43217335128, job 131179803052, at `4be05ea`, line 140 of the job log: `PASS 54.2s github.com/polymetrics-ai/pm-cli/tests/acceptance`. bdd run 41193915946, job 124516127588, at `652de58`, line 140 of the job log: `PASS 58.3s github.com/polymetrics-ai/pm-cli/tests/acceptance`. Each log shows only this package total. At these commits the report script prints one line for each package (`scripts/ci/go-test-report.py:45-46` at `4be05ea`), and the lane prints no timing. So the time from a process start to the ready observation of the lane is not in the logs.
  - Read in the source, not in the logs: at `4be05ea` the package builds `pm` (`tests/acceptance/main_test.go:59-89`). The lane start builds `pm-server` and `pm-worker` (`lane_test.go:128`, `:132`), migrates the store (`:138`), and starts and waits for each process (`:172-186`). The feature files hold three scenarios each: `tests/acceptance/features/cli_offline.feature` and `tests/acceptance/features/lane.feature`, at `4be05ea` and at `652de58`.
  - Reported, not checked again in this amendment: the Go `os/exec` documentation says that `Cmd.Wait` also waits for the copy of a standard output or standard error that is not an `*os.File`. So the exit observation of the stop sequence must not depend on that copy.
  - Not run: a measurement of the duration of one start.
- Not run: the second spike on Linux, because no container runtime was available on the host. On Linux, `/dev/fd` is expected to name the open descriptors of the process through `/proc/self/fd` (not checked).
- Run: `pm-server --config --secrets-directory x` printed `pm-server: unknown command "x" for "pm-server"` and exited with code 1. The parser took `--secrets-directory` as the value of `--config`.
- Not checked: that the Temporal service keeps a task in its queue until a poller takes it. The Temporal server source was not read.
- Not checked: the facts about the systemd notification protocol (a socket path in the environment, with a length limit), and whether a Temporal authorizer requires a DescribeTaskQueue permission.
- Not checked: the bounds of `server.Open` (the store, the identity provider discovery and the artifact resources), of the worker runtime open, and of any other call inside `Worker.Start`. Also not checked: any retry inside the gRPC library itself. The start limit bounds the wait of the starter for the whole start, so the result that the starter reports does not depend on these facts.
- Not checked: whether native libraries in the query child hold descriptors without close-on-exec.
- Not checked: that each descriptor that the process, the Go runtime or a package initializer opens before the `--ready-fd` check has close-on-exec set. The check of the flag depends on it.
- Not checked: whether an image or a Compose starter can pass an inherited descriptor.

## Revisit trigger

- Packaging needs a service-manager protocol, for example systemd notification, or an image or a Compose starter cannot pass an inherited descriptor.
- An operator needs a signal that the Temporal service lists or answered the worker poll, or the worker gets a per-process identity.
- A supported target has no inherited descriptors.
- A starter needs a signal for a later state change, for example drain or the loss of a dependency.
- A Temporal SDK upgrade changes what `Worker.Start` checks before it returns, or changes the contract of `Start` and `Run`.
- A flag-parser upgrade changes a refusal text.
- A supported system has no descriptor directory `/dev/fd`, or its listing leaves out an open descriptor.
- A Temporal SDK or go-grpc-middleware upgrade changes how retry options apply to a direct call.

- A supported starter cannot set its own start limit, or Packaging (M12) needs a start limit that PM owns.
