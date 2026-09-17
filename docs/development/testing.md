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

The store checks run against the selected owned PostgreSQL fixture, including transaction faults, two-connection visibility, reopen/replay, competing claims, stale fences, scoped cleanup and metadata publication. R03 observes physical backend identity and empty scope after reuse, discards uncertain cleanup connections, and rejects an otherwise valid runtime role with a stray `TRUNCATE` grant on a PM table. R04 checks tenant-local composite bindings and retained namespaces; checkpoint publication also requires the complete prior identity, current writer fence and atomic manifest advancement. R05 exercises shared account capacity across aliases/replicas, unknown-charge retention and fenced outbox claims. The API generator guard and these store transactions alone do not establish application authorization, a physical-send gate or a durable HTTP 202 boundary. The authenticated integration below separately exercises current M3 application authorization; dispatch and the durable HTTP 202 boundary remain unavailable.

The all-target Bazel command selects `//internal/store:store_test`, `//internal/server:server_test` and `//tests/stack:stack_test`. All three declare exclusive execution because their fault and application cases mutate shared fixture schema, roles or scoped state. Manual runs require the explicit fixture inputs documented in setup. The foundation CI job provisions its [job-owned dependency fixture](setup.md#job-owned-ci-dependency-fixture) on the existing Docker engine and preserves an ownership manifest for unconditional cleanup. Missing prerequisites, failed cleanup and skipped physical checks are not successful validation. Local fixture passes do not substitute for actual candidate CI.

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

The wrapper supplies fresh synthetic stack credentials and trusted test certificates to the child, then cleans only its owned resources. `TestGinStrictBoundary` covers exact typed binding and refusal, trusted forwarding provenance, sanitized error/panic handling, and cancellation reaching the standard request context. The `internal/server` cases use a real PostgreSQL store plus a signed loopback HTTPS OIDC provider and trusted edge. They exercise browser and device sessions, logout and one-time exchange, workspace concealment, and registered membership, service-account and delegation mutations with preconditions, exact replay, changed-intent conflict and current authorization before replay disclosure. Service-account proof checks one-time credential use, omission from subsequent responses, revocation and explicit replacement; delegation proof retains distinct actors and bounded parent scopes. Private credential cases cover denial, step-up, encryption, replay and non-readback with an independent database observer. Readiness and invocation submission remain unavailable while dispatch is unwired. `TestAuthenticatedApplicationDrainPreservesAcceptedRun` commits a synthetic pre-existing run through the real store, closes the composed API, and verifies the unchanged record and admission identity through an independent pool; it is not HTTP admission or Temporal execution proof. `TestBoundedLifecycleDrain` separately covers real socket, request-cancellation and subscriber-release bounds.

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
