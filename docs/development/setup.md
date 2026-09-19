# Developer setup

This checkout contains the accepted PM Server documentation baseline, the combined issue #2/#3 foundation, and the authenticated `pm-server` and bounded `pm-worker` candidates. PR #47 landed the reviewed context on `main` at `d8e84e1fc78561173feac8abfd64ce669f09f373`, and BOOT-1 readback verified the approved staging-to-landed relationship. Local application checks below do not substitute for independent review, exact-head Linux CI, or merge records. The server composes authentication, encrypted credential ingress, durable admission, scoped run controls, and bounded PostgreSQL-to-Temporal handoff. The separate worker registers deterministic workflows and authorized activities. Production provider handlers remain unavailable; these foundations do not certify provider execution or a release.

## What is available now

The repository contains a digest-pinned bootstrap, application and tools Go modules, native DuckDB probe, dependency-policy CLI, authenticated `pm-server` entry point and minimal same-origin browser surface, standalone `pm-worker`, thin `pm` client, `pmgen` source compiler, and Make and Bazel targets. Use them from the repository root; do not replace them with whichever compiler or build tool happens to be on the ambient `PATH`.

### Resolve and run pinned tools

`tools/bootstrap.py` is the public entry point for the locked Go, LLVM/Clang, Bazelisk/Bazel, Syft, Docker Compose, Node and Chromium assets in `build/toolchains.lock.json`. npm comes from the same verified Node archive:

```sh
python3 tools/bootstrap.py print-env
python3 tools/bootstrap.py exec go -- version
python3 tools/bootstrap.py exec clang -- --version
python3 tools/bootstrap.py exec bazelisk -- --batch version
python3 tools/bootstrap.py exec docker-compose -- version --short
python3 tools/bootstrap.py exec node -- --version
python3 tools/bootstrap.py exec npm -- --version
python3 tools/bootstrap.py exec chromium -- --version
python3 tools/bootstrap.py print-path chromium
```

The literal `--` before tool arguments is required. On a cache miss the bootstrap downloads only locked assets for the selected host platform, verifies each locked SHA-256, and installs them into a persistent local cache. The default is `.cache/pm-tools`; set `PM_TOOLS_CACHE` to another persistent directory when needed. The bootstrap rejects caches under `/tmp` and `/private/tmp`. When the default repository-local tool cache is used, Bazel's output-user root is placed in the platform user cache because Bazel does not permit its repository-contents cache inside the source workspace.

Bootstrap environment changes apply only to the process it executes. In particular, the pinned Go directory is prepended only to that child process's `PATH`; the parent shell, global `PATH`, and globally installed tools are not changed. Go and Bazel execution force `GOTOOLCHAIN=local` and `GOWORK=off`, so an ambient workspace or Go's automatic toolchain selection cannot replace the locked inputs. For Bazel test and coverage commands, bootstrap asks the pinned Go tool for its configured `GOCACHE` and `GOMODCACHE` before Bazel changes the test `HOME`, then passes only those two values into the test environment. The sandbox `HOME` remains fresh, valid user-selected cache locations retain their Go semantics, and other Bazel commands do not perform the cache query.

The Compose 5.5.1 executable stays in the project tool cache; it does not replace an installed Docker CLI plugin, install an engine, start Docker Desktop, or change a Docker context. Stack lifecycle actions require explicit operator authority and an owned project. A successful Compose version check is not dependency-stack readiness or CI evidence.

Node 24.21.0 includes npm 11.19.0. Both `exec node` and `exec npm` put that Node directory first on the child process's `PATH`; npm executes its verified archive entrypoint through the same Node binary. An ambient Node executable cannot replace the selected child runtime. `print-path node` and `print-path chromium` resolve and verify one tool without launching it.

The Chromium 153.0.8010.12 assets are the official headless-shell distribution used for browser qualification, pinned separately for Darwin arm64, Linux amd64 and Linux arm64. This retains the existing refusal of ZIP symlinks; bootstrap does not install Linux browser dependencies or change a host service. `exec go` does not fetch browser tools. Explicit `print-env` resolves the complete selected-tool inventory, including Node and Chromium, for environment and SBOM evidence. Tool startup alone does not prove browser/API parity or another platform's runtime.

On macOS, cgo-enabled Go, Clang, and Bazel execution require an Apple macOS SDK discoverable through the fixed system command:

```sh
/usr/bin/xcrun --sdk macosx --show-sdk-path
```

Select Xcode or the Xcode Command Line Tools so that command returns an available absolute SDK directory. The bootstrap refuses native execution when it cannot resolve that prerequisite; it passes the resulting SDK and deployment target only to the child process.

Linux development-native execution additionally requires the reviewed [`build/native-environment.json`](../../build/native-environment.json) profile. The current profile captures the existing Linux amd64 CI sysroot, including OS identity, headers, GNU runtime libraries and loader. Its fingerprints bind logical paths, entry types, symlink chains and file bytes; they are not raw-file payload checksums. Capture alone does not qualify native execution.

Bootstrap verifies that profile before native execution, selects the locked Clang/`ld.lld`/`llvm-ar` source, requires its compiler-rt startup/builtin objects when no GCC installation is selected, and derives local linker scripts for the admitted GNU runtime SONAMEs. Bazel receives the verified LLVM archiver rather than requiring host binutils. Added cached tool roles are authenticated against the locked archive before their admission record changes; cached SDK files are not replaced. Bootstrap installs nothing into the host sysroot. The profile digest participates in compile and link options so a newly reviewed sysroot identity cannot reuse stale C objects. DuckDB application archives remain pinned separately by `go.sum` and the generated Bazel dependency repositories.

Bazel's tracked `CC_TOOLCHAIN_NAME` binds the selected native environment. This invalidates cached compiler configuration when a selected tool changes, including `AR`, which the pinned `rules_cc` reads without declaring as a repository dependency.

Linux header search excludes implicit host locations: only the selected Clang builtin headers and profiled system include directories are enabled. Selected trees bind intermediate symlink selectors and refuse targets outside the declared trees or files. Two existing LLVM 18 shared-library targets are included only to close symlinks in the selected system-library tree; they are not the selected compiler.

Unadmitted native flag overrides and mismatched native `GOOS`/`GOARCH` requests fail rather than falling back. A missing or changed profile requires a deliberate new capture and review; CI never refreshes it automatically. Linux targets without an admitted matching profile remain unqualified. This is bound-host development evidence, not a hermetic build or release qualification; immutable build-image digests remain mandatory before image-based or release-native builds under the [accepted TD27 amendment](https://github.com/polymetrics-ai/pm-cli/issues/3#issuecomment-5687998290).

### Build and test entry points

These Make targets are source-present:

| Command | Boundary exercised |
| --- | --- |
| `make build` | Builds `.cache/bin/pm-server`, `.cache/bin/pm-worker`, `.cache/bin/pm`, `.cache/bin/pmgen`, the cgo native probe, and the isolated dependency-policy, oapi-codegen, Staticcheck, and govulncheck binaries with pinned Go |
| `make test` | Runs the resolved-dependency policy test and the native DuckDB behavioral test |
| `make vet` | Vets the application module with cgo and the tools module without cgo |
| `make boundaries` | Checks native-linkage and module ownership boundaries |
| `make bazel-build` | Runs `bazelisk --batch build //...` through the bootstrap |
| `make bazel-test` | Runs `bazelisk --batch test //...` with test-result caching disabled |

Bazel uses two action jobs, two loading/analysis threads, two filesystem-checking threads, and a JVM processor count of two. Analysis and execution run separately: Bazel 9.2's Skymeld mode creates a fixed 200-thread symlink pool independent of `--jobs`, which exceeds the available headroom under the existing runner's 256-task limit.

The bounded Bazel timing probe is also source-present:

```sh
make benchmark
```

It asks Bazel for the actual output base, creates an isolated temporary output/action cache alongside it and outside the source workspace, and measures one cold-output-cache build followed by one warm build of the same `//...` targets and output base. Tool and repository-download caches may already be warm, and one sample is not a statistical speedup claim.

Run the isolated native qualification, including its negative controls, with:

```sh
python3 scripts/ci/check-native.py
```

It builds and runs the native probe in an owned temporary directory under `.cache`. A separate C/C++ fixture must then compile and return its expected value while ambient `CC` and `CXX` point to failing executables, proving that child processes retain the pinned compilers. The qualification reuses resolved modules with network lookup disabled and requires normal build-path refusal for cgo disabled and for an incompatible 32-bit native target. On Linux, the latter requests an unqualified Go target and must fail host-profile preflight; it does not claim that a cross-target compiler ran. The macOS control retains its explicit incompatible C target.

Run the current-platform external-tool and selected Bazel-module inventories with both Go graph checks using a disposable, uncommitted evidence directory:

```sh
python3 scripts/ci/check-dependencies.py --evidence-dir .cache/foundation-sboms
```

That orchestrator resolves the current-platform selected tool installs, expands Bazel to its actual install base, and resolves the two locked Gazelle/rules_go source repositories through Bazel's canonical repository metadata. It checks their exact archive URL/integrity pins, scans only those selected source targets rather than a whole historical cache, creates package and per-file SHA-256 CycloneDX inventories, invokes the dependency-policy CLI, and reruns `TestResolvedDependencyPolicy` against those SBOMs. Each selected Bazel module requires recognized-package coverage; file-only evidence is insufficient. File inventory describes installed bytes, not proof that a package catalog recognized every dependency or that every source component executed.

The optional availability report remains:

```sh
sh scripts/dev/doctor.sh
```

It reports ambient tool versions and index status without installing dependencies, changing global configuration, generating source, building PM, running tests, or contacting providers. Missing optional tools are reported explicitly; an installed tool whose version/status command fails makes the report fail. Exit zero means the report completed, not that the application is validated.

### Configuration and offline installation

The pure server-configuration boundary is now source-present in `internal/config`: `Load` rejects malformed, duplicate, unknown, wrongly cased, and lossy Unicode input; `Validate` resolves one immutable PostgreSQL/Temporal/S3 configuration and checks every requested lifecycle action before an operator can mutate a dependency. Input is bounded to 1 MiB and 128 levels. Configuration carries secret references, not resolved secret values. OIDC client identifiers remain exact opaque values within the [OAuth character contract](https://www.rfc-editor.org/rfc/rfc6749#appendix-A.1), not PM identifier names.

The [server schema](../../schemas/server-config.schema.json), [bundled example](../../deploy/server.example.json), and [managed example](../../packaging/config/server.example.json) describe the same backend architecture. The examples contain placeholder references, not provisioned credentials or working endpoints. Admission selects `development` (32 global pending, 8 workspace pending, 2-minute workspace age) or `standard` (256, 32, 10 minutes); explicit finite positive overrides retain presence, and workspace pending cannot exceed global pending.

`limits.run_lifetime` is a required positive duration with no implicit production default. Admission captures it with the resolved resource limits in an immutable run-budget snapshot. Query and metadata deadlines do not supply the run lifetime; the explicit durations in checked-in examples are synthetic configuration values.

`identity.step_up_maximum_age` bounds fresh OIDC authentication for private credential writes. Omission selects `5m`; explicit values must be between `1s` and `15m`, inclusive. Null, numeric seconds and out-of-range durations are rejected. The freshness witness is signed OIDC `auth_time`, not the PM session's creation or expiry time.

`artifacts.execution` is an optional-complete operator block. Omitting it leaves artifact-byte execution unavailable even when the base `artifacts` fields are sufficient to check the selected S3 dependency; PM never infers a byte backend or execution authority from those fields. When present, the block requires all eight controls: `region`, `max_object_bytes`, `max_in_flight`, `request_timeout`, `organization_reserved_bytes`, `workspace_reserved_bytes`, `organization_reserved_attempts`, and `workspace_reserved_attempts`. The checked-in deployment and packaging examples intentionally omit the block.

The region is retained exactly and must contain 1–64 lowercase ASCII letters, digits, or hyphens. Every byte and attempt value is positive and no greater than 9,223,372,036,854,775,807. `max_object_bytes` must be smaller than that signed maximum to leave room for the verification sentinel byte. `max_in_flight` is additionally capped at 65,536, its product with `max_object_bytes` must fit the signed bound, and `request_timeout` is positive and at most five minutes. One object fits the workspace retained-byte reservation, which fits the organization retained-byte reservation; workspace retained attempts fit the organization retained-attempt reservation. These are scoped retained artifact reservations and backend I/O controls, not aliases for `limits.request_bytes`, workflow payload/input limits, run budgets, or admission controls. A valid block makes this policy available to process composition; it does not by itself install or certify a backend.

Run the pure configuration and packaging-contract checks without starting services:

```sh
python3 tools/bootstrap.py exec go -- test -mod=readonly ./tests/stack -run '^(TestStrictStackConfiguration|TestOIDCClientIdentifierRemainsOpaque|TestPackagingPathSchemas|TestInstallerRejectsDestinationFileDirectoryConflict)$' -count=1 -timeout 10m
scripts/install.sh --help
```

The offline installer consumes an operator-supplied payload described by the [release-pair schema](../../packaging/release-pair.schema.json) and [install-manifest schema](../../packaging/install-manifest.schema.json). `--check` verifies the plan without creating its destination. Installation checks release identity, artifact roles, target platform, digests, modes, and confined nonconflicting paths; it refuses different existing files and never starts services. This is not release authenticity verification, an upgrade protocol, real-stack readiness, or completed M12 installation/recovery proof.

### Authenticated PM Server entry point

`make build` emits the real server binary at `.cache/bin/pm-server` through the pinned Go wrapper. Its help and version paths are offline: they do not read configuration or secrets, contact PostgreSQL or the identity provider, or start dependencies.

```sh
make build
.cache/bin/pm-server --help
.cache/bin/pm-server --version
```

The local build reports the source version `development`; that value is not a release identity or attestation. Serving requires each explicit path flag exactly once:

```sh
.cache/bin/pm-server \
  --config /absolute/path/to/operator-server.json \
  --secrets-directory /absolute/path/to/operator-secret-bundles
```

Those paths are placeholders. The configuration path must name one complete strict typed server document; no environment merge, directory search or alternate configuration source fills missing fields. The checked-in examples contain synthetic endpoints and secret references and are not runnable production configurations. Do not pass an example unchanged or place raw secret values in it.

The separately selected secret directory is the only filesystem root for secret resolution. A `secret://authority/path` reference maps to the confined bundle directory `authority/path` below that root. Startup reads the PostgreSQL endpoint reference's `pm-app-endpoint` and the identity reference's `oidc-client-secret`; credential ingress loads the encryption reference's `wrapping-key` when needed. Each must be a nonempty bounded regular file, and the wrapping key must contain exactly 32 raw bytes. A declared optional `identity.ca_ref` adds one such `ca.pem` read from that reference: every entry in it must then be a CA certificate, and the OIDC client trusts exactly those roots, while an undeclared reference keeps the process system root store. The relay lazily reads `ca.pem`, `client.pem`, and `client-key.pem` from the configured Temporal identity reference when durable work requires a connection. An unavailable Temporal service or identity cannot erase committed admission. Artifact references remain declarations for a component this process does not open. Secret values are never flags, public configuration, or diagnostics.

The process observes existing dependencies and never provisions or migrates them. Startup requires a compatible migrated PostgreSQL application store and reachable configured HTTPS OIDC discovery. The OIDC client must register the exact callback formed by appending `/v1/sessions/browser/callback` to `public_origin`. The bounded relay selects the explicitly configured `pm-control` task queue, not the first queue in a list. It starts or reconciles stable workflow identities and repairs orchestration observations; it does not execute provider effects or register a production workflow worker. Shutdown stops the relay and closes its client before closing the application pool; it never cancels or terminates durable workflows.

The executable opens a plain HTTP listener and accepts no certificate or key flags. Authenticated API and browser traffic must be terminated by an HTTPS edge whose backend address is explicitly admitted by `server.trusted_proxies`. `public_origin` must be HTTPS, the request host must match it, and forwarded HTTPS/host/client fields are accepted only from that trusted peer. Untrusted or ambiguous forwarding is rejected. Plain loopback HTTP is limited to unauthenticated `GET` requests for `/v1/info`, `/health/live`, and `/health/ready`; it never admits cookies or bearer credentials.

The minimal same-origin pages are `/login` for configured OIDC sign-in or reauthentication, `/login/device` for approving or denying a displayed device challenge, and `/admin/credentials` for the private write-only credential ingress. Browser authentication uses the Secure host-only `__Host-pm_oidc`, `__Host-pm_session`, and `__Host-pm_csrf` cookies with `Path=/` and no `Domain`; the OIDC and session cookies are HttpOnly, while the same-origin CSRF cookie is deliberately readable for the matching request header. The credential page clears its password field, does not place the value in the URL or browser storage, and sends it only to the server-side private endpoint. The server encrypts the provider credential under the operator wrapping key and returns opaque status/version metadata, never credential readback.

An unconfirmed credential response is not a refusal. The page treats only HTTP 401/403 as authoritative authorization refusals; other unsuccessful responses and transport loss leave the outcome unknown and lock further submissions for that page lifetime. Check the connection state before reloading. The page neither retains the secret for retry nor silently submits a new request key after an ambiguous result.

`/health/live` can report the process live, but `/health/ready` remains HTTP 503 with dispatch false even when the PostgreSQL admission probe succeeds. The production handler registry is empty, so invocation submission returns unavailable capability rather than fabricating accepted work. The [admission checks](testing.md#authenticated-admission-and-run-controls-m4) register an explicit fixture capability; the [handoff checks](testing.md#temporal-handoff-and-projection-m4) exercise real PostgreSQL and Temporal. Neither makes this process a ready provider-execution backend.

### Scoped event streams and artifact boundaries

`GET /v1/workspaces/{workspace_id}/runs/{run_id}/events` serves authenticated SSE. Both `run.read` and `event.read` must authorize the parent run before event disclosure, and polling rechecks current authority. Foreign scopes and runs remain concealed. The stream begins with an authoritative `snapshot`; committed `run_event` frames follow in durable sequence order. Sequences, state revisions and safe-payload counts are decimal strings, including values beyond JavaScript's exact integer range.

Reconnect with the opaque `Last-Event-ID` from a committed event. When retained history has a gap, use the recovery snapshot's signed `cursor` rather than inventing a sequence. Cursor signatures bind deployment, organization, workspace and run; current actor authority is checked separately. Signing lazily derives a separate key from the configured 32-byte wrapping key and deployment identity; replicas serving the same deployment must use the same key. Missing signing capability returns explicit unavailability, not unsigned cursors.

The subscription queue is capped at the smaller of the configured event-buffer limit and 1,000 messages. Each frame has the configured server write deadline. On an underlying write timeout, the raw transport closes before TLS shutdown can retain a blocked handler. The event handler cancels and joins its producer before returning. Disconnect and server drain end the subscription without cancelling the accepted run. These boundaries have [focused physical checks](testing.md#scoped-events-and-artifact-refusal-m4), including the composed server's signing and drain path; they do not make the provider registry ready.

Artifact upload, inspection and download routes enforce scoped authorization before consulting the optional byte backend. Without both a complete `artifacts.execution` block and an installed backend, authorized requests return HTTP 503 and fabricate neither bytes nor metadata. Upload remains a bodyless capability probe, and the shared protocol boundary rejects a raw upload body with HTTP 400; configuring execution does not create a public arbitrary-upload API. A composed read backend verifies the registered object's generation, length and SHA-256 before returning metadata or any full/range bytes. Public metadata contains only the artifact ID, SHA-256, byte length and media type. Downloads use `Content-Disposition: attachment` and `X-Content-Type-Options: nosniff`; provider-controlled media must not become an active PM-origin document.

### Thin PM client

`make build` emits `.cache/bin/pm`. Its fresh Cobra tree owns server profiles, PM sessions, explicit workspace selection, connection/access mutations, and task/run/artifact views. Help and version work without valid settings, credentials or a server. Profiles contain nonsecret HTTPS server identity and session references, never a mutable current workspace or provider credentials.

Fresh server-qualified command discovery also saves a private metadata-only documentation projection beside `profiles.json`, under `catalog-projections/`. With explicit `--server-profile`, `--workspace`, and `--connection` selectors, root/generated help, `help <command>`, `manual`, and `completion` can use that projection without reading execution settings or PM sessions or contacting a server. The output identifies the cached catalog revision and principal and labels the metadata stale and non-authoritative. Missing, corrupt/unsafe, or scope-mismatched metadata is reported while static documentation remains available.

The projection retains the server-instance, principal, workspace, connection, and catalog-revision binding; it is not an execution-authority cache. A generated command invocation still performs fresh server/session/catalog qualification. Generated flags consume their declared value before the bounded selector pass interprets help or PM controls, so literal provider values such as `--help`, `-h`, `--`, and `--workspace` retain their meaning.

PM sessions use the OS credential store or explicitly selected process-local memory; there is no plaintext fallback. Mutations without an explicit request key allocate a new 192-bit key for a new logical submission. An explicit key selects same-key recovery; an equal payload does not silently select an earlier intent. The existing recovery store persists secret-free scoped key/intent fingerprints before sending and permits at most three same-key sends within 30 seconds, with bounded 250 ms/1 s waits. A later uncertain send cannot be treated as a definite earlier refusal. Conflict, expiry and precondition refusals do not trigger rebasing or a replacement key.

The client uses the generated public API and the canonical manual artifact boundaries. Current artifact-capability refusals come from the authenticated server; they are not fabricated client stubs. Catalog-backed commands, explicit watch/cancel behavior, physical API/client/worker replacement and same-stack CLI/browser parity have [focused local checks](testing.md#thin-client-and-source-compiler-checks-m5m6). These are synthetic-provider checks on Darwin arm64, not GitHub provider execution, Linux browser qualification or candidate CI. The production provider registry remains unavailable until its declared capabilities are implemented and registered.

An available catalog command accepts `--result-controls` as one strict JSON object containing shared typed `selection`, `filter`, `total_record_cap` and `continuation` controls. For example, `--result-controls '{"selection":{"paths":[["id"]]}}'` requests the `id` path without rewriting provider inputs. Filtering precedes selection; omitted controls preserve complete action data. A positive `total_record_cap` limits emitted records per invocation after filtering, not across the whole continuation chain.

Resume uses a typed token, for example `--result-controls '{"continuation":{"presence":"value","type":"string","text":"TOKEN_FROM_RESULT"}}'`. Only a present, nonempty token permits omitted required provider inputs at the client boundary. Supplied inputs remain validated; the server restores omitted original inputs and sticky selection, filter and cap controls, refuses explicit conflicts or caller-supplied navigation, and applies full source preflight before a provider send. A typed absent token does not enable partial requiredness.

Duplicate, wrongly cased, unknown, raw-null or invalid controls fail before invocation. The caller's original controls and input presence participate in durable request identity, so changing them cannot reuse an earlier intent's request key. A provider field named `result-controls` receives a deterministic safe alias from the same PM-control registry used by the CLI.

`pm artifacts inspect ARTIFACT_ID` renders verified public metadata. `pm artifacts download ARTIFACT_ID` writes exact raw bytes to stdout, even with `--output json` or `--json`; it adds neither an envelope nor a newline. An optional `--range` selects one closed, open-ended or suffix HTTP bytes range. Problems remain on stderr. A failed or cancelled download can leave a streamed prefix, so a nonzero exit must not be treated as a complete artifact. The client keeps instance/session qualification, bounded streaming and exact length checks; full-object downloads also verify SHA-256.

### Standalone bounded PM worker

`make build` also emits `.cache/bin/pm-worker`. Its `--help` and `--version` paths are offline. Startup uses the same two explicit path flags as the server:

```sh
.cache/bin/pm-worker \
  --config /absolute/path/to/operator-worker.json \
  --secrets-directory /absolute/path/to/operator-secret-bundles
```

These are placeholder paths, not a runnable deployment. The complete server configuration document must include the optional `worker` block defined in [the published schema](../../schemas/server-config.schema.json). API-only documents may omit it; `pm-worker` refuses an absent, partial, or null block. All six worker fields are explicit: `task_queue`, `dial_timeout`, `drain_timeout`, `max_concurrent_activities`, `max_concurrent_workflow_tasks`, and `activity_timeouts`. No worker defaults are inferred.

`task_queue` must be `pm-control` and appear in `temporal.task_queues`. Dial and drain timeouts must be positive and at most five minutes; each concurrency bound must be 1–65,536. The nested timeouts require `schedule_to_close > start_to_close > 0`, with positive `heartbeat` and `transport` both below `start_to_close`. Schedule-to-close cannot exceed `limits.run_lifetime`. The Go validator enforces these relational constraints in addition to the JSON Schema's closed shape.

The worker opens the configured application PostgreSQL endpoint and authenticated Temporal client from the confined operator secret bundles. It uses `pm-app-endpoint` and Temporal `ca.pem`, `client.pem`, and `client-key.pem`; it does not provision dependencies, migrate the store, or perform OIDC discovery. Workflow and activity polling share one SDK client. Shutdown stops the worker before closing the client and application resources, without cancelling accepted durable runs.

At each new physical-unit boundary, the worker checks the accepted durable grant against current connection, principal and grant authority. Login-session expiry alone does not revoke accepted work. PostgreSQL account permits are shared across worker instances and connection aliases; authorization checks database-time expiry before committing a new attempt. An expired reservation is not renewed and dispatched in the same acquisition. Workspace admission budgets remain independent. These are the bounded [grant and permit checks](testing.md#current-grants-and-shared-account-permits-m4), not a measured fairness guarantee.

The production provider registry and operation resolver remain unavailable. Registered orchestration is not permission to execute an undeclared provider operation, and schedule admission remains unavailable without its admitted authority. The [worker checks](testing.md#bounded-workflow-and-worker-execution-m4) exercise controlled capabilities through real PostgreSQL and TLS Temporal, including the actual compiled worker's polling and graceful stop; they are not connector certification.

A configured worker also owns the shared artifact publisher. Result capacity joins the lease/physical-attempt preparation transaction before the execution callback; a capacity refusal rolls back preparation and makes no provider call. A returned, valid `ActionResult` is encoded without losing typed values or presence, uploaded under a unique immutable key, and independently verified. Its artifact publication and the worker settlement receipt commit together. Run views expose only the receipt's scoped, connection-bound public result reference. Failed byte publication does not replace independently known member outcomes; unused unbound reservations are released, while bound/uncertain uploads remain charged. This shared result path does not activate the default provider registry or durable retry scheduling.

Control-schema migration 16 retains existing artifact registrations as public and adds an immutable, connection-bound continuation purpose. Migration 17 requires an explicit purpose on new reservations and registrations, binds publication receipts to that purpose, and retains canonical producer lineage in the existing worker settlement receipt. Public metadata, content, authorization and run-result projections conceal private continuation artifacts. Downgrade refuses retained state that the preceding schema cannot represent.

### Durable HTTP continuation

`app.NewContinuationService` stores the encoded continuation in the shared object backend and opens verified bytes after worker replacement. Its opaque token selects a private artifact, not a public result. The canonical settlement receipt identifies the producing run; the envelope binds its original inputs, sticky controls, operation revision, scope, connection, selected origin, credential reference and provider-account witness. Admission and worker execution reject mismatched authority rather than trusting token-derived claims. PostgreSQL authority transactions close before object-store reads.

The worker reserves both public result and private continuation capacity before provider I/O. Private envelopes cannot exceed the smaller of the configured object limit and the strict contract reader's 16 MiB limit. Publication and settlement commit the exact private and public references together. Bound or uncertain uploads remain charged.

A resumed invocation may return a retained unread tail without a provider call, including terminal tails whose source operation requires navigation. That verified local result releases its still-reserved account permit by exact owner/fence CAS; it creates neither a dispatched permit nor a physical-attempt observation. A resumed provider page instead requires fresh physical-call authority. Tokens rotate through the current producing run, while public pagination retains truthful counts, unread tails and source exhaustion. The [durable continuation checks](testing.md#durable-http-continuation) exercise this shared path; the production registry and live connector qualification remain separate obligations.

### Explicit provider HTTP policy

The optional `provider_http` server-configuration block is complete or absent. It requires `max_request_bytes`, `max_response_bytes`, `max_dns_answers`, `max_attempts`, `max_retry_delay` (a positive duration string), and a nonempty `destinations` array. Each destination names exact `org_id`, `workspace_id`, `connection_id`, and HTTPS `origin` values. Duplicate scoped bindings, unknown/null fields and invalid bounds are rejected. There are no wildcard destinations, inferred first origins, environment bindings or production numeric defaults; API request and artifact storage limits are not substitutes for provider HTTP limits.

For an operation with source-owned HTTP facts, admission selects the exact connection policy after current authorization and same-key replay resolution. The selected origin must match exactly one admitted origin, including its base path. Missing or unlisted authority refuses new admission. Only the selected policy enters the immutable run-budget snapshot, and workers receive that sealed value rather than re-reading mutable configuration. An existing accepted intent can still be recovered with its original key after policy removal; that removal prevents new logical submissions.

Control-schema migration 15 extends the existing closed run-budget document without changing absent-policy digests or adding a policy table. Downgrade refuses while any HTTP execution budget is retained. These controls and their admission checks do not activate the still-unavailable production provider registry.

`app.NewHTTPDirectReadRegistration` composes the shared one-attempt HTTP runtime with direct-read result validation, the configured continuation service and the common worker artifact publisher. It supports one independently attributable read member, uses only the worker's current physical-call authority and sealed policy, and supplies no write or reconciliation callback. A received cooldown is committed with settlement across connection aliases, even when the response body is unusable. The transport never resends in-process: the durable worker owns any admitted retry, retaining the exact previous attempt and outcome. This constructor alone does not activate the default provider registry.

### Durable HTTP retries

PostgreSQL retains each dispatch, physical attempt, outcome and settlement receipt. An eligible retry atomically prepares one child dispatch for the same logical effect and immutable input, with the next ordinal within the sealed attempt budget. It also expires the exact parent lease without resetting its fence; the next execution must claim a fresh lease, account permit, physical witness and publication capacity. Source revision, current account/grant authority, cancellation and the original deadline are rechecked before sending.

Definitive no-send evidence may permit another attempt. Received responses require the declared retry status and unconditional source replay safety; unknown sends require unconditional source replay safety. Conditional or missing replay authority never authorizes a guessed resend. Known parent rejection remains in its immutable receipt while the logical retry is deferred. A genuinely unknown parent permit stays charged even if a separate safe retry succeeds.

Temporal owns eligibility timers and bounded recovery polling; it does not extend the original deadline. A present malformed, excessive or beyond-deadline `Retry-After` suppresses automatic retry. Qualified unit references select one exact dispatch; retained unqualified references still mean the initial dispatch. Versioned deadline projection preserves recorded workflow commands. The payload converter admits only its registered constant SDK change ID and typed version in addition to safe-reference contracts, not arbitrary scalar data.

[Migration 18](../../internal/store/migrations/postgres/0018_worker_retries.sql) preserves legacy root ordinals and refuses downgrade while retry lineage remains. Native scoped foreign keys validate child membership at commit after application scope cleanup. Cancellation or deadline closure can derive known rejection from the canonical parent receipt without inventing a child attempt; unknown outcomes remain unknown. The [retry checks](testing.md#durable-http-retries) cover these shared foundations, not default registry activation or live connector certification.

### Explicit dependency operator and physical qualification

`scripts/pm-stack` invokes the selected Go toolchain and the dependency operator in `internal/stack`. It is not PM CLI auto-start and does not create a PM API or worker. `check` verifies actual PostgreSQL database/role/schema identities, the authenticated Temporal namespace, and authenticated S3 bucket access. `up` initializes and starts selected bundled dependencies; `migrate` serializes their schema tooling; `drain` stops only selected bundled services. Managed dependencies remain externally owned: lifecycle preflight rejects attempts to mutate or stop them.

Compose's implicit `.env` loading and inherited `COMPOSE_ENV_FILES` are disabled. They cannot supply a second configuration source or override this boundary through `COMPOSE_DISABLE_ENV_FILE=false`.

Bootstrap grants the application runtime only `SELECT` on its readiness marker, revokes database `CREATE` and `TEMPORARY` privileges from both the runtime and `PUBLIC`, and removes table/sequence default grants for future application objects. Store migrations own their exact per-table privileges; repeating dependency initialization must not restore broader application grants.

The digest-pinned [Compose layout](../../deploy/compose.yaml) is a bounded, non-production qualification layout, not production sizing or availability guidance. It uses separate PostgreSQL application, Temporal persistence, Temporal visibility and object-metadata databases; Temporal's official schema tool; Seaweed `postgres2`; two TLS S3 gateways; and Caddy. The filer pool ceiling of two, four concurrent listing clients, and seeded object names are synthetic qualification inputs, not production defaults. Seaweed grows one volume at a time and reserves capacity for both its default metadata collection and the configured artifact bucket.

Use an explicitly owned disposable fixture with fresh test-only credentials and trusted test certificates. Configuration examples alone do not provision these inputs. `PM_STACK_SECRETS_DIR` names the confined secret-bundle root; `PM_STACK_STATE_DIR` names owned generated state and data; `PM_STACK_PROJECT` identifies the unique Compose project. The named PostgreSQL, Temporal, S3-primary, S3-secondary and public-port overrides must agree with the configured endpoints. Every published infrastructure port binds to `127.0.0.1`; service CPU, memory and process counts are bounded. All service data mounts stay under owned state. Select the intended container engine explicitly; the operator does not install or start a host engine.

The selected `pm-stack-fixture` output adds an explicit development-only `artifacts.execution` block to its generated `server.json`: region `us-east-1`, 16,777,216 maximum object bytes, four in-flight object operations, a 30-second request timeout, 1,073,741,824 organization and 268,435,456 workspace retained bytes, and 4,096 organization and 1,024 workspace retained attempts. These are synthetic fixture controls for bounded integration and cleanup checks, not certified production sizing, availability guidance, or proof that a process composed the backend.

With that fixture prepared, these source-present entry points operate on its configuration. The absolute path below is a placeholder, not a supplied fixture:

```sh
scripts/pm-stack check --config /absolute/path/to/owned-test-server.json
scripts/pm-stack up --config /absolute/path/to/owned-test-server.json
scripts/pm-stack migrate --config /absolute/path/to/owned-test-server.json
scripts/pm-stack drain --dependency all --config /absolute/path/to/owned-test-server.json
```

Physical tests additionally require `PM_STACK_TEST_CONFIG`, `PM_STACK_TEST_S3_PRIMARY_ENDPOINT`, and `PM_STACK_TEST_S3_SECONDARY_ENDPOINT`, while retaining the fixture's secret, state, project and port settings. Missing inputs fail; they are not skips. Run against the owned fixture only:

```sh
python3 tools/bootstrap.py exec go -- test -race -mod=readonly ./tests/stack -run '^TestSelected(Storage|PostgresTemporal)Qualification$' -count=1 -timeout 10m
```

The storage root exercises bounded concurrent nested listing, both gateways and a separate reader process, interrupted multipart publication, and rejection of corrupted or truncated bytes. The database/Temporal root exercises concurrent initialization, exact schema versions and incompatible-version refusal with restoration, denied roles, missing client authentication, unsafe port publication, and managed-stop refusal. Authenticated readiness alone is not byte-transfer proof.

The Bazel target `//tests/stack:stack_test` includes these physical checks. Its `external` and `no-remote` tags prevent cached test success and remote execution without disabling sandboxed compilation. The [test rule](../../tests/stack/BUILD.bazel) explicitly inherits its named fixture, engine and tool settings from the invoking environment. Export those settings, including `PM_STACK_ROOT` set to this checkout; do not substitute an unowned stack. Results from this dependency-stack target do not establish runner CI, authenticated PM API/worker behavior, connector certification, or later installation and recovery proof.

### Job-owned CI dependency fixture

The [foundation job](../../scripts/ci/check-foundation.sh) builds the dependency operator and [fixture generator](../../tools/cmd/pm-stack-fixture/main.go), then runs the complete Bazel test set through [the fixture wrapper](../../scripts/ci/with-stack-fixture.py). It requires an existing local Docker Unix socket, a Linux engine and the already-cached pinned Compose executable. Local Docker Desktop development uses the same wrapper. Missing prerequisites fail; the wrapper does not install or reconfigure an engine, register a runner, or borrow another project's services.

The stack inspects the selected Docker engine before lifecycle actions. Ordinary engines use the operator's UID/GID; a rootless engine owned by that operator uses container `0:0`, which [maps to the unprivileged host owner](https://docs.docker.com/engine/security/rootless/uid-gid-mapping/), not host root. Private bind-mounted credentials keep their existing permissions. SeaweedFS invokes its binary directly so the image entrypoint cannot change that identity or chown the data; its FIPS runtime setting is retained. Rootful `userns-remap` is refused until an explicit ownership mapping is implemented. The fixture records and rechecks the engine's rootless identity during cleanup.

`with-stack-fixture.py --manifest ABSOLUTE_PATH -- COMMAND...` creates a unique private project, bounded containers, loopback-only published ports, fresh synthetic credentials and test certificates. It uses the admitted image pins, removes inherited Docker/Compose overrides, isolates Docker authentication, and passes the generated fixture references to the requested command. No real provider credentials are needed.

The manifest is `$RUNNER_TEMP/pmcli-foundation-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json`. Its exclusive, durable pre-mutation record binds the job attempt, checkout, daemon, project and owned paths; later records retain resource identities. The workflow's `always()` step invokes `with-stack-fixture.py --cleanup ABSOLUTE_PATH`. Cleanup revalidates ownership, removes only the owned resources and synthetic data, and leaves a cleaned receipt that cannot authorize deletion of a replacement directory or recycled resource. Startup, child-command and cleanup failures remain nonzero.

The physical stack, store, Temporal, and authenticated server targets declare [`exclusive` execution](https://bazel.build/reference/be/common-definitions#common.tags): their schema, role, history, and application cases run serially within a Bazel invocation, including direct Bazel and Make entry points. Other tests retain normal scheduling. Separate manual test invocations must not share a fixture concurrently.

The wrapper's lifecycle checks exercise successful physical tests, child failure, normal cancellation, abrupt termination, partial startup, mismatched-job refusal and cleaned-receipt reuse. Those lifecycle observations are local development results, not final-head runner CI, runner-network qualification, production recovery or application/provider proof; a child command establishes only the exact behavior it asserts.

### Typed source and value contracts

`internal/contract` now provides strict operation, foundation and field-binding boundaries, exact semantic digests, scoped entity keys, and typed values. Values keep absent, null, false, zero, empty containers, exact integer/decimal text and scale, nested data, and provider property spelling distinct. Instant normalization preserves the exact fractional value when converting to UTC; local wall timestamps remain a separate type. Unsupported input is refused rather than silently coerced.

Action results retain complete data unless the caller requests shared typed controls. Filtering evaluates the unprojected record before nested field selection. Filtered emission counts change without rewriting source counts, effective wire limits, continuation, unread tails, provider totals or source exhaustion. Container inputs and returned views cannot mutate retained values.

The [foundation schema](../../schemas/foundation.schema.json), [field-binding schema](../../schemas/field-binding.schema.json), [identity schema](../../schemas/identity.schema.json), and [workflow-reference schema](../../schemas/workflow-ref.schema.json) execute with the pinned JSON Schema validator and a closed external loader in the focused tests. Shared Go validation additionally checks duplicate keys, exactly cased metadata names, exact references and semantic constraints; schema validation alone does not establish those guarantees.

Identity and transition contracts retain explicit organization/workspace scope, requested/effective actors, bounded one-hop delegation, and grant expiry/policy epochs independent of login sessions. `ObjectRef` is the single Go representation of the eight-field immutable artifact descriptor; an explicit zero byte length differs from an absent length. `NewRunRecord` owns copies of caller-supplied intent and workflow-history containers.

Timeouts retain effect uncertainty and attempt receipts. Unknown effects keep run/task aggregates reconciling rather than failed, cancelled, succeeded or blocked. Policy blockage becomes a blocked state only when every effect is known. API workflow bindings use `pm/run/<opaque UUID>`; Temporal chain observations do not replace the stable PM run identity or grant reference.

```sh
python3 tools/bootstrap.py exec go -- test -mod=readonly ./internal/contract -count=1 -timeout 10m
```

These are pure contract checks, not generated API readiness, authentication/RLS enforcement, durable lifecycle or Temporal execution, registered artifact-byte verification, connector implementation or provider certification.

### Source compiler

`make build` also emits `.cache/bin/pmgen`. `pmgen compile` consumes an explicit source lock, captured source/license bytes and optional source-linked semantics or revision diffs; it writes the generated execution bundle, fidelity report and optional command manifest. Required path arguments are listed in `cmd/pmgen/main.go`. It does not fetch missing references, install runtime handlers or certify a connector.

The compiler retains request/status/media variants, explicit no-content responses, all seven lane dispositions, exact field presence/defaults and source pointers. Defaults are not applied implicitly. Shapes the shared contract cannot represent receive explicit diagnostics rather than lossy projection. The retained Pylon `Account.parent_account_id` counterexample still lacks matching captured bytes: no reproduction or full issue #22 closure is claimed.

The same runtime bundle contains the compiler's exact command projection and a separate non-executable inventory of unavailable operations. A source-linked unsupported shape does not suppress an unrelated admitted operation. Source integrity, fidelity disagreement, malformed declarations and invalid capability requirements still refuse publication. The optional command sidecar is an authoring artifact, not a second runtime input; the report retains unavailable operations' full source/request/status/media facts.

### Generated public API boundary

The ordinary public API is authored once in [OpenAPI 3.1](../../api/pm.openapi.yaml), with exact PM controls in [controls.json](../../api/controls.json). The pinned generator produces the public Go DTOs/client, the Gin boundary, canonical OpenAPI JSON and their digest manifest. Edit the authoring inputs or generator, never the generated files.

```sh
make generate
make check-generated
python3 tools/bootstrap.py exec go -- test -race -mod=readonly ./internal/api -count=1 -timeout 5m
```

`check-generated` compares every owned output byte against fresh generation and is included in `make check` and the foundation CI command sequence. `TestGeneratedContractBoundary` exercises independent output directories, stale and semantically lossy output, unsupported source projection and generator failure propagation. Required, nullable and composed-schema constraints must survive; no proof-only DTO belongs in the ordinary API.

`TestWireVersionAndEnvelope` covers protocol/required-feature refusal, safe additive responses, distinct admission/error/replay envelopes and exact shared values/selection/filter/pagination. `RegisterPMStrictHandlers` installs the protocol guard before caller middleware and typed binding. Ordinary metadata and replay reject secret-bearing fields; provider-owned data field names remain exact, including a legitimate field named `password`. The sole one-time PM credential response is separate from replay.

The SSE and three artifact operations remain in canonical OpenAPI, exact controls and generated named DTOs. Their streaming/manual handlers are deliberately excluded from the ordinary generated client and strict handler interface; the same route registrar applies the shared protocol and authentication boundaries.

Generated-router and protocol checks alone do not prove authentication, durable admission or provider execution. The HTTP package now also contains physical fixture cases; run those through the [owned-fixture wrapper](testing.md#scoped-events-and-artifact-refusal-m4), not as an unconfigured standalone package test.

### PostgreSQL control store

`internal/store` owns the embedded [control](../../internal/store/migrations/postgres/0001_control.sql) and [tenancy](../../internal/store/migrations/postgres/0002_tenancy_rls.sql) migrations and their ordered extensions, bounded runtime pools, transaction-local scope, durable admission/outbox, immutable input/budget/grant snapshots, receipts/events/audit, task leases/fences, dispatch/member outcomes and manifest/checkpoint metadata CAS. The current application marker is `pm.control/v18`; authenticated worker state extends the existing store rather than creating another queue. Workflow handoff adds immutable command intents and append-only reconciliation witnesses. Its bounded migrator-owned claim function returns scoped routes; runtime callers re-enter scoped transactions. The runtime role cannot create schemas/tables/temporary objects, truncate PM tables, delete history, assume the migrator role, or broaden its authority. Operator reinitialization must preserve these store-owned grants.

Migration 0011 backfills each run's durable event watermark across retained scopes. Event allocation continues beyond that watermark even when retained rows have been removed. Its temporary migrator-only policies are transaction-bound and removed before commit; runtime visibility is not widened. Downgrade refuses a watermark that the retained rows cannot represent, rather than rewinding future event identities.

Migration 0012 binds each newly registered artifact descriptor to one trusted, scoped connection in the same atomic insert. The binding is immutable on idempotent replay, retained unbound rows fail closed at public authorization, and downgrade refuses to erase any populated authority binding.

Use only an explicitly owned disposable PostgreSQL fixture. The tests require the selected application's `pm-app-endpoint` and `pm-app-migrator-password` references under `PM_STORE_TEST_SECRETS_DIR`; they do not skip when those inputs are missing. With the dependency fixture above already prepared:

```sh
export PM_STORE_TEST_SECRETS_DIR="$PM_STACK_SECRETS_DIR/operator/postgres"
python3 tools/bootstrap.py exec go -- test -race -mod=readonly ./internal/store -count=1 -timeout 5m
python3 tools/bootstrap.py exec bazelisk -- test //internal/store:store_test --nocache_test_results --test_output=errors
```

The Bazel target inherits only the named fixture directory and resource controls. Tests use isolated scopes and ordered, scope-bound cleanup rather than dropping the application schema. They exercise interrupted migration rollback and downgrade refusal, equal-ID composite references, immutable workspace namespaces, physical connection reuse and discard after cleanup failure, competing claims and stale fences, atomic state/receipt/audit/event/outbox, admission replay after reopen, and complete-identity manifest/checkpoint publication.

Connections, memberships, encrypted credential versions and checkpoints are workspace-scoped. Provider-account coordination is organization-owned: aliases across workspaces and worker replicas share one PostgreSQL capacity reservation, not a per-process counter. A store permit requires a current owner/fence and unexpired authority before it can enter the dispatched state. Dispatched or unknown work stays charged after expiry; no expiry alone proves that a request was unsent. Workspace tombstones retain their immutable key namespaces.

The foundational store checks establish PostgreSQL-owned behavior. `TestDurableAcceptedBefore202` additionally exercises the authenticated admission route and independently observes committed records before HTTP 202, as described in the [M4 checks](testing.md#authenticated-admission-and-run-controls-m4). `TestArtifactCrashWindows` observes unpublished metadata, not object upload/readback. R03–R05 cover scoped storage, composite references and durable permit/CAS boundaries, not the physical provider send. Ciphertext metadata round-tripping does not qualify a wrapping-key service or signer. No store result proves a provider effect, resolution of an unknown external outcome, or Temporal execution.

### Go tool and module pin maintenance

The `tool` block in `tools/go.mod` names three executable packages: oapi-codegen, govulncheck, and Staticcheck. Their modules, two deliberately selected generator dependencies, and the fixture's descriptor-relative filesystem dependency form the six exact tools-graph entries in `tools/versions.json`: `github.com/getkin/kin-openapi`, `github.com/oapi-codegen/oapi-codegen/v2`, `go.yaml.in/yaml/v3`, `golang.org/x/sys`, `golang.org/x/vuln`, and `honnef.co/go/tools`. Go may annotate selected tool-only `require` entries `// indirect` because ordinary packages do not import them. That annotation does not turn a reviewed root pin into an incidental transitive selection. The fixture imports the pinned `golang.org/x/sys/unix` directly; the policy declares that direct dependency explicitly.

Conversely, a version chosen transitively by minimal version selection is not an approved pin merely because it appears in `go list -m all`. The policy requires every selected manifest entry at the exact version and as a direct main-module graph edge; it separately examines the full graph for forbidden identities.

Do not run a blind `go mod tidy`: the foundation intentionally pins approved modules whose later consumers do not exist yet. Tidy can remove those requirements, including deliberately selected tools support modules. Inspect a proposed tidy without applying it:

```sh
python3 tools/bootstrap.py exec go -- mod tidy -diff
python3 tools/bootstrap.py exec go -- -C tools mod tidy -diff
```

These read-only previews return nonzero when they propose changes. Apply only reviewed changes for an authorized pin update, retaining all still-approved explicit requirements. After updating the affected module and selection manifest, retain full-graph module and `go.mod` sums:

```sh
python3 tools/bootstrap.py exec go -- mod download all
python3 tools/bootstrap.py exec go -- -C tools mod download all
```

Use the command for each affected module. Keep its graph selections, checksums, `tools/versions.json`, affected Bazel locks, external-asset lock when applicable, and `THIRD-PARTY-NOTICES.md` synchronized. `go mod download all` supplies full-graph sums; it does not approve a version or refresh manifests and notices by itself. See [testing](testing.md) for the bounded local observations and their unqualified scope.

## Active instructions and workflow

Fresh workers start with the root [AGENTS.md](../../AGENTS.md), [issue/spec lookup](../agents/issue-tracker.md), short [architecture overview](../architecture/overview.md), [decision index](../architecture/decisions.md), [domain glossary](../agents/domain.md), [testing](testing.md), [release](release.md), [skill routing](skills.md), [skill provenance](skill-provenance.md), the active [configuration ADR](../decisions/0001-cli-configuration.md), and the assigned accepted PM Server contracts.

Use the sole [PM Go router](../../.agents/skills/pm-go/SKILL.md) for Go work. Add [connector contracts](../../.agents/skills/pm-connector-contracts/SKILL.md), [durable effects](../../.agents/skills/pm-durable-effects/SKILL.md), or [release and recovery](../../.agents/skills/pm-release-recovery/SKILL.md) only for its named behavior boundary. Files under `docs/architecture/pm-server/instructions/` are synchronized inactive projections, not a second instruction tree. Historical issue drafts, private setup/controller runbooks, transition evidence, and operator configuration are excluded from fresh-worker intake.

OMP is the sole coordinator and integration owner. The single [engineering and review policy](../architecture/pm-server/policy/ENGINEERING-REVIEW.md) owns validation, independent review, and delivery. Agent model/account routing is supplied separately by the operator and is not application documentation. No controller installation, private home path, model login, or host qualification is a general application prerequisite.

The doctor command above and the CodeGraph commands below are real developer-tool commands for this checkout. They do not activate application work. Every `pm`, server, worker, stack, generation, proof, build, test, and provider command described only in planning remains future until its owner creates and exercises the actual path.

## Repository skills and source provenance

The [machine-readable manifest](skill-provenance.json) records exactly four repository-original project skills, their current byte hashes, required repository-relative references, and selected external source closure. External references use `use-installed-no-copy`; [the provenance guide](skill-provenance.md) records licenses, immutable revisions or file hashes, PM overrides, and official HTTPS fallbacks.

Optional references may be absent on a host. Use the official fallback and report the missing discovery rather than installing a catalog or rewriting global configuration. No third-party skill payload is copied, and no skill file, model login, source URL, or structure-only setup check is evidence that PM behavior or delivery passed.

## CodeGraph identity and prerequisites

The tool used here is [`@colbymchenry/codegraph`](https://github.com/colbymchenry/codegraph), version **1.5.0**, under the MIT license. Its npm launcher selects a platform bundle with a bundled Node runtime and parser. The npm launcher itself needs Node on PATH; the standalone distribution bundles the runtime. Go structural indexing did not require `go.mod`, a Go build, a database service, or a container in the fixture.

Do not substitute another project named CodeGraph. If it is absent, bounded `rg` and direct reads remain sufficient to contribute. Installation is an explicit developer choice: review the official distribution, exact version, supported platform, and provenance first. Do not silently install or upgrade to latest. This setup did not independently verify release attestations; installed package metadata is provenance evidence, not an attestation result.

The installed npm shim can download a missing platform bundle automatically. The commands below set `CODEGRAPH_NO_DOWNLOAD=1` so a missing prerequisite fails visibly. `CODEGRAPH_TELEMETRY=0` disables telemetry for that invocation without altering global preferences. Neither setting changes PM behavior.

## Initialize this checkout explicitly

The current candidate's exact checkout has already been initialized manually; it was not indexed through a workspace parent. For another checkout without an index, inspect its root and `codegraph.json`, then run from that checkout:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph init .
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph status --json .
```

`init` creates `.codegraph/` and builds the initial graph in the same command. Version 1.5.0 has no CLI init-only flag; `--index` is a deprecated compatibility option. Keep explicit manual synchronization and the repository's exclusions if initialization offers Git hooks or ignored-child inclusion. Do not use `--force` on a workspace parent or home, and do not install hooks, start a daemon, or invoke an updater as a setup shortcut.

## Refresh and inspect real code

Use the intended checkout root explicitly:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph sync .
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph status --json .
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph explore "<real symbol names>" --path . --max-files 5
```

Replace the angle-bracket text with symbols that actually exist. After changing exclusions, language mappings, or extraction versions, rebuild explicitly:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph index . --quiet
```

Inspect `projectPath`, `indexPath`, `lastIndexed`, languages, counts, `worktreeMismatch`, and the nested index and extraction versions. A successful explicit sync followed by inspection of a known changed symbol is stronger evidence than a timestamp or pending-change count. The current gate used explicit sync; it did not rely on a hook, daemon, or watcher.

The root `codegraph.json` and `.gitignore` exclude generated runtime data, generated website/manual projections, proof output, caches, and local execution state. CodeGraph 1.5.0's `exclude` also applies to tracked files. Treat a malformed configuration warning as incomplete setup because the tool can continue after ignoring the file.

## CodeGraph CLI versus development MCP

CodeGraph's MCP exposure is optional developer navigation tooling. This read-only mode prints version 1.5.0's agent configuration snippet without changing global configuration:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph install --print-config codex
```

It reports a `codegraph serve --mcp` command for the developer to review through the agent's own configuration process. The current candidate gate used the CLI and did not install a hook, configure MCP, or test watcher behavior. Do not manually launch a second service or make MCP an application prerequisite. PM itself neither implements nor depends on MCP.

## Current CodeGraph observation

CodeGraph 1.5.0 was manually initialized and explicitly synchronized at the exact candidate checkout, with no worktree mismatch. After source edits, explicit sync made the changed source available to exploration. An exploration of `CheckDependencies` resolved the implementation in `tools/dependency_policy.go`, its real CLI call from `tools/cmd/pm-dependency-check/main.go`, and the associated `tools/dependency_policy_test.go`; source inspection identified `TestResolvedDependencyPolicy` as the focused graph-policy test.

This establishes useful static navigation for the observed source. It does not establish complete dependency closure, runtime dispatch, cgo behavior, provider behavior, or complete test selection. No Git hook, persistent watcher, or daemon supplied freshness, so synchronize explicitly before relying on an edited symbol.

## Retained fixture limits

The earlier disposable external fixture contained `TestRun → Run → Normalize` across three Go files. CodeGraph indexed three files, seven nodes, and six edges, returned the call flow and source, and excluded three noise files. After an edit introduced `Bump`, explicit sync reported one changed file and exploration returned `Run → Normalize → Bump`; a full reindex also completed. No fixture was added to this repository and no Go test was executed.

Two limits remain relevant:

- `affected normalize.go --path . --json` returned an empty test list although exploration exposed the known test call chain; adding `--filter '**/*_test.go'` also returned no tests. Neither result can authorize skipping tests.
- In that non-Git fixture, `status` reported zero pending changes immediately after an edit, while the following explicit sync found it. Do not infer freshness from the pending count alone or assume identical behavior in every Git checkout.

Use file and package ownership, known consumers, declarations, and causal risk to select checks. A graph does not fully describe runtime JSON selection, scripts, reflection, dynamic dispatch, cgo, or provider behavior, and graph reachability alone cannot preserve old proof receipts.
