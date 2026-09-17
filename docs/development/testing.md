# Testing and evidence

PM tests observable contracts at their owners and consumers. The canonical [validation matrix](../architecture/pm-server/implementation/VALIDATION-MATRIX.json), accepted issue, and [file ownership map](../architecture/pm-server/implementation/FILE-OWNERSHIP.json) select the required checks. The shared [engineering and review policy](../architecture/pm-server/policy/ENGINEERING-REVIEW.md) owns integration and review sequencing; this guide does not create a second gate.

## Choose the causal check

For changed behavior, identify the failure oracle before production edits. Exercise a causal RED that fails for the intended defect, make the smallest owned correction, then exercise GREEN and refactor without weakening the oracle. Use standard Go tests and table subtests by default. Add concurrency, race, bounded fuzz, security, persistence, browser, same-stack, recovery, or provider checks only where the changed contract and validation row require them.

Tests must observe behavior, boundaries, invariants, transitions, precedence, and real errors. A test that only echoes a mock, checks a copied field, asserts source text, compiles without execution, or proves that a fixture is nonempty is not behavioral evidence. Reversible documentation work uses content, link, provenance, and instruction-discovery checks rather than an invented application RED.

## Keep evidence scopes distinct

| Observation | What it can establish | What it cannot establish |
| --- | --- | --- |
| Source, schema, or descriptor inspection | Exact reviewed bytes and declared structure | Handler reachability, runtime behavior, or provider effects |
| Build or compilation | Selected source compiles for the exercised target | Tests ran, native portability, or service readiness |
| Unit or Temporal SDK test | The exact in-process or replay property asserted | Real PostgreSQL/Temporal/object persistence or cross-host recovery |
| Controlled compatible fake | Contract behavior under the recorded fixture | Live account entitlement, provider semantics, or certification |
| Same-stack integration | The exercised component boundary and failure oracle | A different image, architecture, account, or untested failure mode |
| Live provider case | The exact operation/account/source/binary scope and time observed | An entire connector, lane, account tier, or future build |
| Documentation/provenance check | Portable links, hashes, membership, and honest status | PM application behavior or full BOOT by itself |
| CI result | The recorded workflow, commit, runner, and jobs actually exercised | A later head or obligations absent from that workflow |

Keep fake and live evidence separately labeled. Preserve exact component, source, schema, fixture/account, and candidate identities. A newer relevant failure prevents reuse of an older pass; unknown dependency closure invalidates conservatively. Skipped, missing, timed-out, zero-selected, compile-only, or different-head work is not a pass.

## Record an actual run

For every claimed check, retain the exact command or action, selected test/case IDs, repository and commit/head, environment and architecture, relevant dependency/fixture identities, start and finish time, exit state, skips, and sanitized output or receipt location. Never record secrets, raw provider credentials, private operator routing, or mutable local paths as product evidence.

Run focused checks before integration. Broad suites and genuine CI run once at the coherent candidate boundary, with affected checks and targeted review after material fixes. Independent Standards and Spec review is separate from testing and cannot turn a missing check into green.

## Build and toolchain entry points

The [CI event and merge policy](ci.md) defines exact-revision PR checks,
post-merge main checks, safe runner admission and owner-account merge authority.

The issue #3 foundation commands and current build target are source-present. From the repository root, use the pinned entry points below. The all-target Bazel test command also selects the physical stack, control-store and authenticated-server checks: prepare and export their [explicitly owned fixture inputs](setup.md#explicit-dependency-operator-and-physical-qualification) first. Missing inputs fail rather than skip.

```sh
make build test vet boundaries
python3 scripts/ci/check-native.py
make bazel-build bazel-test
python3 scripts/ci/check-dependencies.py --evidence-dir .cache/foundation-sboms
make benchmark
```

`make test` includes `TestResolvedDependencyPolicy`, which checks both real module graphs and its fail-closed synthetic SBOM controls. Without `PM_SBOMS`, that invocation does not claim full external-tool inventory qualification. `check-dependencies.py` generates current-platform inventories, invokes the real `pm-dependency-check` CLI, supplies those SBOMs through `PM_SBOMS`, and reruns the named test. The command is no longer planned or unavailable.

### API generation and PostgreSQL control-store checks

The [generated public API and control-store entry points](setup.md#generated-public-api-boundary) are source-present. Keep generation, compilation, protocol behavior and actual storage execution distinct. Compilation does not replace the executed strict HTTP-adapter or authenticated same-stack checks below.

`TestGeneratedContractBoundary` constructs semantically different required/nullable/`allOf` outputs, observes their differences with the pinned JSON Schema consumer and requires qualification against the original source to refuse them. Its independent-directory, stale-output and generator-exit checks remain separate. Temporary emitted-DTO and Gin-router consumers exercise generated code without exercising the application and authentication handlers tested separately below.

The foundational store checks run against the selected owned PostgreSQL fixture, including transaction faults, two-connection visibility, reopen/replay, competing claims, stale fences, scoped cleanup and metadata publication. R03 observes physical backend identity and empty scope after reuse, discards uncertain cleanup connections, and rejects an otherwise valid runtime role with a stray `TRUNCATE` grant on a PM table. R04 checks tenant-local composite bindings and retained namespaces; checkpoint publication also requires the complete prior identity, current writer fence and atomic manifest advancement. R05 exercises shared account capacity across aliases/replicas, unknown-charge retention and fenced outbox claims. Those checks alone do not establish application authorization or a physical-send gate. The M3 checks below separately exercise current application authorization; the M4 checks exercise the durable HTTP 202 boundary and Temporal handoff.

The all-target Bazel command selects `//internal/store:store_test`, `//internal/temporal:temporal_test`, `//internal/server:server_test` and `//tests/stack:stack_test`. All four declare exclusive execution because their fault and application cases mutate shared fixture schema, roles, history or scoped state. Manual runs require the explicit fixture inputs documented in setup. The foundation CI job provisions its [job-owned dependency fixture](setup.md#job-owned-ci-dependency-fixture) on the existing Docker engine and preserves an ownership manifest for unconditional cleanup. Missing prerequisites, failed cleanup and skipped physical checks are not successful validation. Local fixture passes do not substitute for actual candidate CI.

### Authenticated PM Server M3 checks

The focused same-stack command uses the existing ownership-scoped fixture wrapper. It requires an already available supported Docker CLI and Unix-socket engine; it does not install, start or reconfigure either. Prepare the two existing fixture helpers through pinned Go and make the locked Compose executable available:

```sh
mkdir -p .cache/bin
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o .cache/bin/pm-stack ./cmd/pm-stack
CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- -C tools build -o ../.cache/bin/pm-stack-fixture ./cmd/pm-stack-fixture
python3 tools/bootstrap.py exec docker-compose -- version --short
```

For a local run outside GitHub Actions, give the wrapper a fresh absolute temporary root and its required numeric manifest identity:

```sh
RUNNER_TEMP="$(mktemp -d)"
export RUNNER_TEMP
python3 scripts/ci/with-stack-fixture.py \
  --manifest "$RUNNER_TEMP/pmcli-foundation-0-1.json" \
  -- python3 tools/bootstrap.py exec go -- test -race -mod=readonly \
  ./internal/httpapi ./internal/server -count=1 -timeout 10m
```

The wrapper supplies fresh synthetic stack credentials and trusted test certificates to the child, then cleans only its owned resources. `TestGinStrictBoundary` covers exact typed binding and refusal, trusted forwarding provenance, sanitized error/panic handling, and cancellation reaching the standard request context. The `internal/server` cases use a real PostgreSQL store plus a signed loopback HTTPS OIDC provider and trusted edge. They exercise browser and device sessions, logout and one-time exchange, workspace concealment, and registered membership, service-account and delegation mutations with preconditions, exact replay, changed-intent conflict and current authorization before replay disclosure. Service-account proof checks one-time credential use, omission from subsequent responses, revocation and explicit replacement; delegation proof retains distinct actors and bounded parent scopes. Private credential cases cover denial, step-up, encryption, replay and non-readback with an independent database observer. Production readiness and invocation submission remain unavailable without registered execution handlers and a worker. `TestAuthenticatedApplicationDrainPreservesAcceptedRun` commits a synthetic pre-existing run through the real store, closes the composed API, and verifies the unchanged record and admission identity through an independent pool; it is not HTTP admission or Temporal execution proof. `TestBoundedLifecycleDrain` separately covers real socket, request-cancellation and subscriber-release bounds.

These tests do not skip when the store secret inputs are absent or malformed; outside the wrapper they fail. Fixture startup, the child command and ownership-scoped cleanup must all succeed. Leave `PM_SERVER_BROWSER_PROBE_READY_FILE`, `PM_SERVER_BROWSER_PROBE_STOP_FILE` and `PM_SERVER_BROWSER_PROBE_TIMEOUT` unset for the standard check: it uses Go HTTP clients and does not launch Chrome. A separately exercised local browser, the synthetic OIDC provider and the disposable dependency stack do not establish a live identity/provider certification, Temporal execution, artifact/provider effects, production readiness or final-head CI. No current M3 CI result is claimed; a runner without its approved Docker CLI and isolated daemon prerequisite cannot turn the missing run into a skip or pass.

### Authenticated admission and run controls M4

After building the two fixture helpers above, run the public admission and scoped-control cases against one disposable TLS/Gin/PostgreSQL fixture:

```sh
RUNNER_TEMP="$(mktemp -d)"
export RUNNER_TEMP
python3 scripts/ci/with-stack-fixture.py \
  --manifest "$RUNNER_TEMP/pmcli-foundation-0-1.json" \
  -- python3 tools/bootstrap.py exec go -- test -p 1 -race -mod=readonly \
  ./internal/app ./internal/store \
  -run '^(TestInitialAdmissionAndRegistry|TestDurableAcceptedBefore202)$' \
  -count=1 -timeout 5m
```

The combined local check passed all 44 required issue #14 cases and the added expired-cancellation receipt regression on 2026-09-17, with no skips and ownership-scoped fixture cleanup. The cases exercise committed-before-202 acceptance, canonical typed-intent conflicts, lost responses and an independently observed dropped COMMIT reply, current authorization before replay, immutable run inputs/budgets, pending capacity and age limits, scoped run/task views, durable cancellation, and retry-receipt compaction. Expired cancellation receipts reference the admitted run, not the cancellation command. The unavailable resume capability returns a refusal through the actual resume route.

These are local shared-foundation results with synthetic credentials and an explicitly registered fixture capability. They do not establish Temporal handoff, worker/provider execution, live connector certification, PR CI or merge. The current production registry remains empty; tests do not register a dummy production handler.

### Temporal handoff and projection M4

After preparing the same fixture helpers, run issue #15's two physical roots and the role/lifecycle checks:

```sh
RUNNER_TEMP="$(mktemp -d)"
export RUNNER_TEMP
python3 scripts/ci/with-stack-fixture.py \
  --manifest "$RUNNER_TEMP/pmcli-foundation-0-1.json" \
  -- python3 tools/bootstrap.py exec go -- test -p 1 -race -mod=readonly \
  ./internal/temporal ./internal/store ./internal/server \
  -run '^(TestBothStartHandoffWindows|TestClosedAbsentProjectionRepair|TestWorkflowRouteRoleBoundary|TestApplicationCloseStopsRelayBeforeReturning|TestConfiguredControlTaskQueueSelectsNamedMember)$' \
  -count=1 -timeout 10m
```

All ten physical cases passed locally on Darwin arm64 on 2026-09-17, with race checks, no skips, and ownership-scoped fixture cleanup:

| Root and case | Independent observation |
| --- | --- |
| R09 E01 C01 | A subprocess exits after committed admission/claim but before start; PostgreSQL remains unacknowledged, Temporal is actually absent, and recovery starts the stable identity. |
| R09 E01 C02 | A subprocess exits after real Temporal acceptance but before binding/acknowledgement; another client observes that execution, and recovery binds the same run. |
| R09 E01 C03 | An exact AlreadyStarted race reconciles one execution and one durable binding. |
| R09 E01 C04 | Retained start input with an unknown JSON member is rejected without adoption, disclosure, termination, or effects. |
| R09 E01 C05 | A terminal PostgreSQL run cannot start an absent workflow. |
| R09 E01 C06 | Schedule-origin handoff adopts the existing execution without another Execute call. |
| R10 E02 C01 | A real update drives an activity through transient failure and PostgreSQL commit; a lost completed response and repeated stable UpdateID do not rerun the activity. |
| R10 E02 C02 | Closed retained history repairs orchestration condition/time only, without changing business outcomes, effects, or pending capacity. |
| R10 E02 C03 | Real NotFound for a never-bound run records a scoped no-dispatch witness. |
| R10 E02 C04 | After an actual returned execution is bound and deleted through the public running-execution API in the owned fixture, exact Describe and History NotFound establish the prerequisite. Repair preserves its PostgreSQL observation and uncertainty without restart. This models missing retained history, not elapsed retention time. |

The role check exercises the existing outbox's bounded routing authority and denies broader runtime mutation. The lifecycle checks cover relay cancellation/wait ordering and the named control queue. Fresh starts are verified through retained start history because a successful SDK start response need not contain a first-execution ID. Open-workflow final projection belongs to the idempotent update/activity; the relay must not consume its projection receipt first.

These are shared-foundation results, not issue #16's production workflow/worker implementation, provider effects, connector certification, current PR CI, or merge. No production handler is fabricated to make readiness succeed.

### Bounded workflow and worker execution M4

Rebuild the fixture helpers from the current source as shown above before using the wrapper: it executes the prebuilt `.cache/bin/pm-stack` and `.cache/bin/pm-stack-fixture` binaries. Run issue #16's physical roots serially in one owned fixture:

```sh
RUNNER_TEMP="$(mktemp -d)"
export RUNNER_TEMP
python3 scripts/ci/with-stack-fixture.py \
  --manifest "$RUNNER_TEMP/pmcli-foundation-0-1.json" \
  -- python3 tools/bootstrap.py exec go -- test -p 1 -race -mod=readonly \
  ./internal/workflows ./internal/activities \
  -run '^(TestCommandsAcrossContinueAsNew|TestHistoryAndPayloadBounds|TestWorkerDeathAfterEffect|TestKnownEffectTruthIsIndependentOfResultPublicationAndCallbackError|TestCancelBeforeProviderSendProjectsKnownNoEffect)$' \
  -count=1 -timeout 10m
```

All fourteen required cases and the additional effect/cancellation regressions passed locally on Darwin arm64 on 2026-09-17 with the race detector and no skips:

| Root and case | Independent observation |
| --- | --- |
| R11 C01 | A committed cancel intent is recovered even when its notification is lost. |
| R11 C02 | Signal and stable-ID update delivery across a real Continue-As-New chain apply one durable command. |
| R11 C03 | A genuinely started, buffered control activity drains before rollover; successor identity and the original deadline are preserved. |
| R11 C04 | A genuine server suggestion rolls the production workflow below its local history limits without losing the command. |
| R12 C01–C02 | Unknown/raw payloads and forbidden diagnostic channels are rejected before sensitive serialization or server-state leakage. |
| R12 C03–C04 | Exact safe-reference payloads of 65,535 and 65,536 bytes are accepted; 65,537 bytes are refused. |
| R12 C05 | Both 1,024- and 1,025-byte diagnostic inputs produce bounded summaries without canaries in Temporal channels. |
| R12 C06 | Thirty-two active activities prevent the thirty-third unit from starting. |
| R12 C07–C08 | Real history rolls at the 2,000-event boundary, or earlier when the 2 MiB byte boundary is reached. |
| R12 C09 | Actual activity options and transport behavior enforce the declared 20s/10s/2s/5s timeout hierarchy. |
| R13 | A provider acceptance followed by worker-process death preserves the same physical intent as unknown; reconciliation does not resend it. |

The rollover-suggestion case uses a fixture-only dynamic configuration constrained to the job's auxiliary `-rollover` namespace, with a 64-event suggestion threshold. Production dynamic configuration and the ordinary namespace's thresholds are unchanged. Cleanup follows the exact first-execution-fenced workflow chain; it does not terminate a namespace or queue.

The extra regressions preserve valid member-effect truth despite unavailable result publication or a callback error, and project a prepared pre-send cancellation as known no effect without inventing attempts or member outcomes. A negative-control overlay that treated publication/callback failure as unknown made both known-effect cases fail; the candidate passed them. The pre-send cancellation case likewise failed before its projection correction and passed afterward.

A separate disposable smoke driver launched the rebuilt `.cache/bin/pm-worker`, independently observed its actual workflow and activity pollers over TLS, and verified exit zero after SIGTERM. `TestWorkerConfigurationSchemaBoundary` additionally failed when the published schema rejected a Go-valid worker block, then passed after the closed schema projection was corrected. Neither a compiled test binary nor a worker help command substitutes for these physical checks.

These are controlled shared-foundation results, not live-provider certification, issue #17/#18 completion, final-head PR CI, or merge. The command above excludes the disposable compiled-binary smoke driver; it runs the retained behavioral roots and regressions.

### Current grants and shared account permits M4

After rebuilding the same two fixture helpers, run issue #17's retained application roots in one owned fixture:

```sh
RUNNER_TEMP="$(mktemp -d)"
export RUNNER_TEMP
python3 scripts/ci/with-stack-fixture.py \
  --manifest "$RUNNER_TEMP/pmcli-foundation-0-1.json" \
  -- python3 tools/bootstrap.py exec go -- test -p 1 -race -mod=readonly \
  ./internal/app \
  -run '^(TestInitialGrantRevocation|TestSharedAccountAdmission)$' \
  -count=1 -timeout 10m
```

All six E01 cases, including the three explicit-revocation variations, passed locally on Darwin arm64 on 2026-09-17 with the race detector and no skips:

| Case | Independent observation |
| --- | --- |
| C01 | Expiring the CLI login session does not revoke an accepted durable execution grant. |
| C02 | Explicit connection, principal or run-grant revocation prevents the next controlled physical send; immutable accepted attribution is preserved. |
| C03 | Missing execution handlers refuse admission without creating an accepted run or starting a callback. |
| C04 | Two application worker instances share the PostgreSQL account limit across connection aliases; exhausting one workspace's admission budget does not consume another's. |
| C05 | An expired permit cannot create a durable attempt or invoke the physical callback. |
| C06 | Healthy current authority reaches the callback only after its authorized permit and attempt are committed. |

C05 failed against the preceding production implementation with one callback and one durable attempt after expiry. The corrected boundary produced neither. The other five cases already passed as characterization; they are not claimed as newly introduced behavior. The explicit run-grant variation exercises `app.RevokeFutureEffects`, not only its storage primitive.

These checks use real PostgreSQL transactions, authenticated Temporal admission and actual workflow identities. The shared-capacity case uses two `WorkerService` instances, not two OS worker processes. Controlled callbacks are not live-provider certification, measured fairness, issue #18 proof, final-candidate CI, or merge.

### Scoped events and artifact refusal M4

Rebuild the two fixture helpers from current source, then run issue #18's event/artifact routes, retained-data migration and composed-server drain case:

```sh
RUNNER_TEMP="$(mktemp -d)"
export RUNNER_TEMP
python3 scripts/ci/with-stack-fixture.py \
  --manifest "$RUNNER_TEMP/pmcli-foundation-0-1.json" \
  -- python3 tools/bootstrap.py exec go -- test -p 1 -race -mod=readonly \
  ./internal/httpapi ./internal/store ./internal/server \
  -run '^(TestScopedDurableEventRoutes|TestScopedUnavailableArtifactRoutes|TestEventWatermarkMigrationRetainedData|TestIdentityArraysUseCanonicalByteOrder|TestAuthenticatedApplicationDrainPreservesAcceptedRun)$' \
  -count=1 -timeout 10m
```

The current slow-subscriber and composed-drain cases passed locally on Darwin arm64 with the race detector and no skips. The wrapper completed ownership-scoped fixture cleanup. Final-head Linux CI remains a separate qualification.

| Boundary | Independent observation |
| --- | --- |
| R10 E01 C01, C08 | Foreign scopes/runs are concealed; neither `run.read` nor `event.read` alone permits event disclosure. |
| R10 E01 C02 | A retained-history gap returns terminal run truth and a recovery cursor; later events preserve sequence and exact decimal counters without replacing that truth. |
| R10 E01 C03 | Before the client closes, the timed-out request is canceled and the real HTTP handler count reaches zero; handler return also joins its producer. The 100 ms write deadline, two-event queue and original two-second observation bound remain unchanged. An unexpired-deadline control retains one live handler. Reconnect starts at the last observed committed cursor. |
| R10 E01 C04 | Disconnect and server drain release subscriptions without durable run cancellation. |
| R10 E01 C05–C06 | Tampered and foreign cursors are refused; restart/cross-replica replay follows committed event order. |
| R10 E01 C07 | Missing signing capability returns unavailability before reading event rows. |
| R16 E02 C01–C03 | Unauthorized artifacts remain concealed; an absent backend fabricates neither bytes nor metadata; raw upload bodies fail at the protocol boundary. |
| R16 E02 C04 | Inspection and download conceal artifacts whose stored connection is restricted or revoked, legacy unbound artifacts and sibling-workspace artifacts. Callers cannot supply replacement connection authority. |
| Migration 0011 | Retained v10 events in two scopes backfill and continue their sequences; downgrade refuses a removed-tail gap without changing v11. |
| Migration 0012 | New artifact bindings are scoped and immutable; downgrade refuses retained authority without discarding its binding. |
| Composed server | Real browser authentication opens a signed SSE snapshot through the configured server; drain ends the stream while an independent database observer sees the accepted run unchanged. |

Disabling only the per-frame write deadline makes the slow-reader resource oracle fail before the client timeout, with an uncanceled request and one active handler. The same case passes with the production deadline; a separate unexpired-deadline control remains live throughout the observation window. The permission and migration probes failed for missing `event.read` authorization, RLS-hidden backfill, and unsafe downgrade before their fixes, then passed. The cancellation API check accepts the existing HTTP 202 contract; this is not a new cancellation behavior.

Earlier local passes did not establish Linux resource release. [Linux diagnostic run 35260552029](https://github.com/polymetrics-ai/pm-cli/actions/runs/35260552029), at PR50 head `de141d78`, observed a roughly 100 ms socket write timeout and request cancellation, but TLS close-notify retained the handler until about 3.23 seconds. Cancellation alone therefore cannot qualify this boundary. The shared listener policy closes the timed-out raw connection beneath TLS, and the event handler cancels and joins its producer. The fixture counts actual handler entry and return rather than inferring release from `Context.Done()` or client EOF. Final Linux verification must bind the corrected candidate, not the diagnostic head or an earlier local pass.

Review regressions also exercise expired identity receipts before compaction and credential-free loopback discovery through composed middleware. In a managed Chromium session, the actual credential-page assets were exercised with synthetic HTTP 500/408 responses, HTTP 403, success and transport loss. Ambiguous outcomes retain the page lock and cannot mint a second request key; authorization refusals and success release it. The field clears and browser storage remains unused. This browser fault probe does not claim a live backend commit.

The corrected `auth`, `store`, `app`, `httpapi` and `server` suites passed together with the race detector, no skips and ownership-scoped cleanup. Before the shared authorization fix, restricted, revoked and unbound artifact cases returned HTTP 503 instead of concealed 404, and caller-supplied authority was accepted. The same cases pass after deriving connection authority from persisted metadata; the immutable-binding and downgrade tests pass independently.

Canonical identity arrays use PostgreSQL `COLLATE "C"` to match Go's bytewise grant intersection. A real-PostgreSQL regression accepts the canonical mixed-case order under both C and English caller collations and rejects reversed byte order; it failed before the shared constraint fix and passes afterward. The original Linux auth failure exposed only a `PgError` type, so no historical SQLSTATE or particular constraint name is claimed.

These are controlled local HTTP/TLS/PostgreSQL checks. They do not establish live provider effects, artifact-byte support, final-head Linux CI, cleanup of another job's fixtures, or merge.

### Thin-client and source-compiler checks M5/M6

After rebuilding the existing fixture helpers, use the same owned wrapper for the client, API, compiler and shared-contract targets:

```sh
RUNNER_TEMP="$(mktemp -d)"
export RUNNER_TEMP
python3 scripts/ci/with-stack-fixture.py \
  --manifest "$RUNNER_TEMP/pmcli-foundation-0-1.json" \
  -- python3 tools/bootstrap.py exec bazelisk -- --batch test \
  //internal/api:api_test //internal/cli:cli_test \
  //internal/compile:compile_test //internal/contract:contract_test \
  --test_output=errors --cache_test_results=no
```

The workspace test starts two real child processes through the product CLI entry point, with one shared profile/session identity and a request barrier proving concurrent use of separate workspace selectors. Its controlled TLS peer retains the real synthetic PM credential in the parent-owned fixture transport. It does not install an OS credential or export provider secrets. The dependency-boundary test resolves the actual `cmd/pm` graph, checks a disposable forbidden-import overlay, and verifies that the authenticated server still responds after the client process exits.

Run the focused race checks through a fresh owned fixture as well:

```sh
python3 tools/bootstrap.py exec go -- test -p 1 -race -mod=readonly \
  ./internal/cli ./internal/app \
  -run '^(TestConcurrentExplicitWorkspaces|TestThinBinaryDependencyBoundary|TestControlsPresenceAndPrecedence|TestRegisteredHandlerReachability)$' \
  -count=1 -timeout 10m
```

Compiler checks cover captured-reference closure, deterministic inventories, exact default/presence fidelity, sensitivity and amendment refusal. Actual `pmgen` smoke execution additionally checks emitted bundle acceptance by the published JSON Schema, byte-identical repeated outputs, and no output publication after capture mismatch. These are local shared-foundation checks, not full Pylon reproduction, GitHub execution, browser parity, live-provider certification or candidate CI.

### Bounded Darwin development observations

The following results were observed on 2026-09-15 for the foundation candidate on Darwin arm64. They are local development evidence, not Linux or final-head CI:

- The pinned Go 1.27.1 and LLVM/Clang 21.1.8 were selected. `make build test vet boundaries` passed.
- The native probe reported DuckDB **v1.5.5** through both SQL and the C API, preserved exact `-9007199254740993` and `9007199254740993` values, and read back two rows from an owner-only temporary Parquet file. The separate native controls required build refusal with cgo disabled and with the explicit incompatible i386 C target.
- `make bazel-build bazel-test` passed with Bazel 9.2.0, rules_go 0.63.0, and Gazelle 0.54.0. With Bazel test-result caching disabled, exactly `//build/nativeprobe:nativeprobe_test` and `//tools:tools_test` executed and passed. A Gazelle `-mode=diff` check emitted no changes.
- The built oapi-codegen v2.8.0, Staticcheck 2026.2.1 (module v0.8.1), and govulncheck v1.8.0 binaries executed their version commands. A govulncheck version command is not a vulnerability scan.
- The application and isolated tools MVS graphs resolved 247 and 65 non-main modules, respectively. All five selected tools-module entries were explicit `go.mod` graph pins even where Go marked their `require` lines `// indirect` because they are tool dependencies.

The expanded full-inventory command passed in 17.07 seconds, including the policy CLI and a fresh `TestResolvedDependencyPolicy`. It scanned all five selected installs and, for Bazel, the actual expanded install base:

| Selected install | Recognized packages | SHA-256-inventoried files | Bounded note |
| --- | ---: | ---: | --- |
| Bazel | 52 | 435 | Expanded install base, including its bundled runtime |
| Bazelisk | 0 | 2 | File inventory retained despite no recognized package |
| LLVM | 0 | 9,609 | File inventory retained despite no recognized package |
| Go | 158 | 15,640 | Nineteen unversioned source-root package records retained |
| Syft | 276 | 5 | Package and file inventory reported separately |

These counts describe what the scanner recognized and hashed in those exact installs. They do not claim that recognized-package discovery is complete. Unversioned records are not dropped to obtain a pass, and an unversioned component matching a forbidden identity is rejected conservatively.

A focused review correction also inventoried the two locked Bazel module source trees selected by the current output base. Gazelle 0.54.0 yielded **159 recognized packages and 1,997 SHA-256-inventoried files**; rules_go 0.63.0 yielded **43 recognized packages and 1,064 SHA-256-inventoried files**. The corrected policy CLI passed with those two inventories and the five unchanged retained install inventories above, followed by a fresh `TestResolvedDependencyPolicy` pass. The pre-fix CLI accepted the five-install set without either Bazel module inventory; the corrected CLI refused that same omission. Source-tree inventories include source fixtures and release tooling, not a claim that every discovered component executed.

The same focused regression run loaded the Linux amd64 and arm64 native imports with `go list -mod=readonly -deps`. Both failed before their explicit native leaf requirements were added and passed afterward. This proves readonly import closure, not Linux compilation or execution.

The benchmark completed one cold-output/action-cache build in **47.086722667 seconds** and one warm build using the same output base in **5.195518208 seconds**. Tool and repository-download caches were already warm. This is one sample of each state, not a statistical speedup claim.

### Limits of the Darwin build observations

The Darwin observations do not qualify a Linux target, release artifact, backend, service process, or deployment. Independent Standards and Spec review, exact-head Linux CI, and merge require their own records. Full BOOT completion must bind those results rather than infer success from this local checkpoint.

The historical foundation build/toolchain evidence in this Darwin subsection establishes no additional PM server, worker, client, connector, provider execution, or demo behavior. The selected dependency operator and its independently exercised storage/database checks have a separate [fixture boundary](setup.md#explicit-dependency-operator-and-physical-qualification); they do not complete provider-workflow obligations. Source locks, successful downloads, module resolution, SBOM inventory, and developer-tool navigation cannot replace the actual product, same-stack, provider, recovery, release, and two-real-demo checks owned by their accepted issues.
