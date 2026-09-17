# PM475: implementation plan for one remote-first PM architecture

Status: **Proposed/Recommended; implementation remains paused.** Source and document inspection support the choices below. Every application command, test, installation, provider scenario and performance target in this plan is **UNEXECUTED**. Synthetic examples demonstrate contracts; they are not receipts. PM code is MIT by settled instruction.

## 1. Executive decision

Build `pm-cli` as one remotely accessible application from its first usable release. The `pm` command and a future browser interface submit the same typed requests to a persistent `pm-server`. PostgreSQL records who is allowed to do what, the exact accepted work, its approvals, results and data references. Temporal coordinates that work through `pm-worker` processes. Shared object storage holds immutable datasets and artifacts. Closing a terminal or browser leaves accepted work running.

This gives a clear answer to “where did my job go?” A client can reconnect from another machine, select the same explicit workspace, and see the accepted run and its actual outcomes. Multiple people can use different workspaces concurrently without changing a global active workspace. Adding API or worker replicas does not change the product architecture or move authoritative state to a particular host.

**Proposed/Recommended.** Select Go, Gin, PostgreSQL, Temporal, an S3 artifact contract with bundled SeaweedFS, and a restricted DuckDB analytical child. Use the same stack in development, on one server, or across multiple servers. Compose is the explicit operator launcher for a small installation; managed dependencies are another provisioning arrangement for those same interfaces. There is no SQLite, private local queue, embedded scheduler or host-files product variant. API and CLI processes never start their own dependencies.

The connector model remains intentionally ordinary. Pinned provider data compiles into typed operations and safe command descriptors. Shared foundations implement request encoding, field validation, pagination, authentication, effects, durable transport and data publication. Sparse source-linked JSON supplies relationships or behavior that a provider specification does not express. No provider-name branches or one service/Go adapter per HTTP connector. A real native PostgreSQL protocol adapter is justified because SQL is a different transport, not a disguised HTTP exception.

The initial five connectors are **GitHub, PostgreSQL, Gong, Pylon and HubSpot**. Every one receives a complete inventory across `direct_read`, `direct_write`, `etl`, `reverse_etl`, `binary_download`, `binary_upload` and `sync_transport`. Inventory completeness does not claim all operations work. The first vertical slice is an actual remote GitHub read; it is followed by shared data/PostgreSQL and approved GitHub writes/reverse ETL before expanding the five. Each supported cell earns fresh evidence through the new client/API/worker path.

The rewrite removes duplicated generation/proposal/review authorities, provider-specific test wrappers and local deployment assumptions. It cannot remove provider permissions, rate limits, paid tiers, required fields, stateful prerequisites, missing schemas or ambiguous write outcomes. Temporal replay also cannot decide whether a provider accepted a request whose response was lost. Those engineering obligations remain explicit and testable.

## 2. Baseline, corpus and coverage

**Observed/Verified — read-only Git observation, 2026-09-10 22:57:17–18 UTC.** The existing new repository at `[historical-local-reference:46622f0f7ed7]` was clean `main`, HEAD `563adfd28b80101737ad41298aaa3af839eeacf6`, with setup only and no application implementation. It has `.codegraph`; no indexing was performed. The old reference `[historical-local-reference:6bd74ee50c10]` was clean `main`, HEAD `813f457a925f7ee3fe3bea101a43e445992c8552`, without `.codegraph`. The registered scout was clean at `81a6ad5b4acde773ebc486419e7dbbc811af27c9`. Exact observations are in `LOCAL-BASELINE.json`. No repository creation, source edit, commit, build or service startup occurred in this research.

**Observed — corpus metadata, not whole-corpus review.** A shallow inventory across both workspace data roots found 879 directories and 2,911 candidate immediate Markdown/JSON artifacts. Thirty-eight named inputs were byte-revalidated; a hash proves the bytes, not their meaning. `RESEARCH-INVENTORY.json` distinguishes unread names, prior474 selected-section review, current475 ranges, file hashes and exclusions. The previous474 inventory had a narrower 372-artifact scope and 27 selected-section reviews. None of these counts is a test count or product completeness score.

foundation audit is a newer **proposal**, not implemented or approved behavior: 20 foundation contracts, 32 requirement rows, 413 proposed paths, 12 milestones and 84 unexecuted application checks. remote design retains its valid semantic invariants while replacing its state and deployment assumptions. The complete prior473 plan and research,472 OpenCode/executor source dossiers,467 foundation/dynamic-schema design and471 mode/transport contract remain selectively reused evidence. Artifact numbering never establishes approval.

Current475 directly revisited the consequential source paths recorded in `REINSPECTION-475.json`: OpenCode transport/lifetime, worker fetch/shutdown, durable events versus public SSE, generated SDK repairs; executor dialect parsing, selected response extraction, parameter/null handling, stored bindings and compile-before-transaction refresh. Other pinned OC/EX entries retain their explicitly named prior source-inspection scope. Historical connector reports supply causal failure examples; their quoted tests were not rerun and their old defects are not asserted to persist on current main.

**Proposed/Recommended coverage boundary.** This is sufficient source-grounded architecture planning, not another census of all old tests or provider endpoints. Remaining unreviewed historical artifacts stay visible. During implementation, an affected operation must acquire its exact source/reference closure and account requirements before its command can be certified. Existing dirty paused worktrees and old proof records remain untouched and cannot certify new binaries.

## 3. Research, source findings and contradictions

**Observed — historical reports, selected ranges.** The batch-dispatch report distinguishes a transport batch from per-member settlement: the old Ruby path counted submitted member IDs and returned error values that did not trigger its declared retry policy. The pinned Twenty example established an ordered all-success result contract, not a documented mixed 27-success/1-error response. The new foundation therefore supports correlated outcomes but never invents a provider's partial-success shape. `RI-0009:22–107`, especially26–30,49–55,68–84, is report evidence; no old code was executed here.

The nullable-scalar report found 37 projected nullable fields unsupported by retained provider schemas. Twilio `CreateAddress.City` was a concrete plain-string counterexample. Passing tests over a local projected schema had not proved provider nullability. Preserve raw schema, normalized schema and exact field origin, and block a conflicting binding rather than adding a foundation to justify invented facts. `RI-0167:15–41,87–111` supports the lesson. Pylon's current documented-null/schema-string disagreement gets the same explicit treatment.

The polling/apply topology report describes useful isolated libraries without production consumers. A descriptor and a green helper test did not make ETL or destination apply executable. `RI-0125:195–236` drives the new actual-handler reachability gate. The Gong report showed provider-specific date validation leaking into a generic runner and also documented that the old connector parser bypassed Cobra flag binding. `RI-0093:69–145` drives real generated command trees and reusable source-declared constraints. These observations are historical, not permission to transplant the old wrappers or CI machinery.

Dynamic-schema research distinguishes format version from content fingerprint, partial candidate metadata from a complete account snapshot, and a `dynamic_schema` boolean from executable discovery. Database research distinguishes polling from log-based CDC, entity identity from cursor identity and complete checkpoint state from a last-seen token. Zoom's complete provider inventory with only three executable streams demonstrates why inventory, implementation and certification must stay separate. The exact reused paths, hashes, ranges, dispositions and causal tests are recorded in the source and mapping companions.

**Observed/Verified — pinned OpenCode implementation.** At `b7ca4f91d9222dddb8487e689d3274a732999fd9`, TUI code chooses worker RPC/in-process fetch or a network listener, and its terminal cleanup stops the owned worker. That is useful transport separation but unsuitable accepted-job lifetime for PM. The worker invokes the same request handler; this does not mean every invocation traverses a network server. [TUI235–306](https://github.com/anomalyco/opencode/blob/b7ca4f91d9222dddb8487e689d3274a732999fd9/packages/opencode/src/cli/cmd/tui.ts#L235), [worker23–80](https://github.com/anomalyco/opencode/blob/b7ca4f91d9222dddb8487e689d3274a732999fd9/packages/opencode/src/cli/tui/worker.ts#L23).

Its durable event transaction/projectors and internal replay are useful knowledge. Its public event handler sets no SSE ID and uses an unbounded live queue; that public path is not a durable reconnect contract merely because internal events have sequence numbers. PM adopts transactional state/event publication and explicitly adds bounded public replay/gap behavior. Optional Basic authentication, query tokens, directory-based tenancy, client-owned process shutdown and raw SDK generation repairs are reference-only or rejected. [Durable events236–415](https://github.com/anomalyco/opencode/blob/b7ca4f91d9222dddb8487e689d3274a732999fd9/packages/core/src/event.ts#L236), [public events12–62](https://github.com/anomalyco/opencode/blob/b7ca4f91d9222dddb8487e689d3274a732999fd9/packages/opencode/src/server/routes/instance/httpapi/handlers/event.ts#L12), [SDK build74–85](https://github.com/anomalyco/opencode/blob/b7ca4f91d9222dddb8487e689d3274a732999fd9/packages/sdk/js/script/build.ts#L74).

**Observed/Verified — pinned executor implementation.** At `eaa1f3a57ffff88aede8e83783ea7ed4471aec1f`, operation bindings are persisted separately from large source blobs; refresh compiles outside a database transaction before swapping references. Adopt those ownership boundaries. Its `startsWith("3.")` check is not a full standards validator; selected-response extraction and dropped null query values are specific limitations. PM retains every response variant and location/type semantics. Code-execution invocation, fixed local identity and daemon autostart do not transfer. [Parser155–215](https://github.com/UsefulSoftwareCo/executor/blob/eaa1f3a57ffff88aede8e83783ea7ed4471aec1f/packages/plugins/openapi/src/sdk/parse.ts#L155), [response308–355](https://github.com/UsefulSoftwareCo/executor/blob/eaa1f3a57ffff88aede8e83783ea7ed4471aec1f/packages/plugins/openapi/src/sdk/extract.ts#L308), [store12–57](https://github.com/UsefulSoftwareCo/executor/blob/eaa1f3a57ffff88aede8e83783ea7ed4471aec1f/packages/plugins/openapi/src/sdk/store.ts#L12), [refresh1000–1060](https://github.com/UsefulSoftwareCo/executor/blob/eaa1f3a57ffff88aede8e83783ea7ed4471aec1f/packages/plugins/openapi/src/sdk/plugin.ts#L1000).

**Resolved contradictions.** Temporal's actual calendar uses restricted day-of-month **AND** day-of-week, skips missing daylight-saving wall times and emits both repeated fall-back instants. This supersedes473/474 cron OR/earlier-fold assumptions. The SDK schedule-update convenience helper omits the conflict token; PM uses the lower-level token-bearing request. PostgreSQL16.15 is selected within the documented tested major16; a newer major's existence is not compatibility evidence. Root branch HEAD pins are kept separate from actual Temporal release commits. Image digests have been inspected, but images and schemas have not been executed. Initial backup targets are RPO24h/RTO4h;5min/60min is a future operational tier, not a competing promise. [Temporal schedule semantics160–220](https://github.com/temporalio/sdk-go/blob/b7c242c6894df088a57a85b33d0586e908da8b93/internal/schedule_client.go#L160), [schedule update258–305](https://github.com/temporalio/sdk-go/blob/b7c242c6894df088a57a85b33d0586e908da8b93/internal/internal_schedule_client.go#L258).

**Accepted product/source decisions — implementation still unexecuted.** [F01 and AUTH-MUTATION-PARITY](https://github.com/polymetrics-ai/pm-cli/issues/14#issuecomment-5670702388) fix the exact admission error/retry table, finite pending profiles/reservations, bounded same-key recovery/retention and six revision-safe access mutations. [F02/F03](https://github.com/polymetrics-ai/pm-cli/issues/27#issuecomment-5669069268) fix complete action results by default, optional shared typed field selection/filtering and the exact GitHub source/license identity. [The PostgreSQL demo boundary](https://github.com/polymetrics-ai/pm-cli/issues/28#issuecomment-5669099222) and [mapped insert-or-update](https://github.com/polymetrics-ai/pm-cli/issues/34#issuecomment-5669559504) fix official protocol/SQL/metadata authority, bounded keyed-upsert semantics and the post-M8 N13-PG stage. Acceptance settles product meaning only; source locks, application code, fixtures, generated artifacts and all proof remain with their existing owners.

The accepted GitHub source is `github/rest-api-description` commit `0a144e030abbe358000f0d32874e197b59a8f7ed`, path `descriptions/api.github.com/api.github.com.json`, operation `issues/list-for-repo`, pointer `#/paths/~1repos~1{owner}~1{repo}~1issues/get`, blob `ed76b1371e381b078e50c8a698b890911031aeb2`, 12,939,686 bytes and SHA-256 `7c3a4bfa71e850a32992882392db3cf315dcdeca46ba5a62da963dbea6a589a2`. Its same-commit MIT `LICENSE.md` is Copyright (c) 2020 GitHub, blob `b50625eb63949013cae604b1cadd42cfa1eaf825`, 1,063 bytes and SHA-256 `3243761cbac07e6d169a5a2f4e7c25cc544da85248e735df74c3672e055cc87b`. The raw 12.9 MB file is intentionally not copied here. Issue #27 still owns source.lock/reference closure/compiler/inventory/proof. PostgreSQL instead uses pinned official native protocol, SQL and metadata documentation for the selected version; no OpenAPI is assumed.

## 4. Reset and delta from473/474

**Proposed/Recommended.** Replace the architecture at its owners, not by appending a remote deployment chapter. `ARCHITECTURE-DELTA.json` and the file crosswalk enumerate retired paths and replacements. These are future changes in the new repository; no historical file is deleted now.

| Previous assumption |475 decision | Consequence |
|---|---|---|
| Embedded SQLite application state | PostgreSQL `pm_app` from first usable release | Scoped transactions, RLS, migrations, pooled-connection cleanup and backup required early |
| Custom worker queue and scheduler | Temporal workflows/activities/schedules | PG outbox bridges accepted intent; no PM cron authority |
| Host-local keys and file locks | Operator wrapping-key provisioning plus PG deployment/grant/fence state | Replicas share authority without sharing plaintext keys in client state |
| Host WAL/rename as published artifact truth | Immutable S3 objects plus transactional PG manifests/checkpoints | Orphans are collectible; storage and PG are not one transaction |
| Shared/local DuckDB file | Restricted child per query/materialization over immutable objects | Worker scratch is disposable; only analytics child imports native driver |
| Standard HTTP handler chosen without deep Gin study | Gin with strict PM adapter policy | Domain receives ordinary context; generated binding/error defaults overridden |
| Multiuser/UI/distribution deferred | Tenant/workspace model and shared Go/browser API now | Full UI remains later, but API parity and negative scope tests are initial obligations |
| Login session used as ongoing job authority | Durable accepted execution grant | Logout/expiry does not stop job; explicit grant revocation stops future effects |
| Local server service/autostart helpers | Explicit operator stack launcher | Remote clients need no backend; install/stop respects bundled versus managed ownership |
| Old cron OR and first DST fold | Temporal calendar AND and both folds | Preview, occurrence keys and tests use the selected scheduler's semantics |

The source/value/field semantics, action readback, bounded batches, approval completeness, all seven lanes and exact proof ownership remain. Atlas IDs stay stable where the concept remains stable; contract versions change where ownership or interface changes. No old certification, review verdict, global current selector or batch authority becomes a dependency of ordinary execution.

## 5. Technology selection, alternatives and costs

**Proposed/Recommended.** Pin Go1.27.1 and Cobra1.10.2 for the client, Gin1.12.0 for the server adapter, pgx5.11.0 for PostgreSQL, Temporal Server1.31.2/Go SDK1.48.0, oapi-codegen2.8.0, openapi-typescript7.13.0/openapi-fetch0.17.0, the selected JSON Schema2020-12 validator, and the existing source-normalization tool pins. Use separate application and tools module graphs. Exact complete dependency sums, native archives and transitive policy checks are M0 obligations, not implied by this list. Development-native CI binds the existing host environment under the accepted TD27 amendment below; immutable build-image digests remain mandatory before image-based or release-native builds.

| ID / selected component | Version / location | Purpose, constraint and primary evidence |
|---|---|---|
| TD01 Go | 1.27.1 / all Go processes/tools | Standard HTTP/TLS, exact JSON wrappers, math/big, crypto/rand and AES-GCM envelope encryption; test/fuzz/synctest, vet and race. PM encryption rotates below per-key random-nonce usage bound; allowlisted redaction required. No default provider replay assumption. [BSD-3-Clause](https://go.dev/doc/devel/release#go1.27.0) |
| TD02 Gin | 1.12.0 / httpapi | Route grouping and strict generated adapter. Override trust-proxy/binding/recovery; Request.Context only beyond adapter. [MIT](https://github.com/gin-gonic/gin/tree/73726dc606796a025971fe451f0aa6f1b9b847f6) |
| TD03 github.com/oapi-codegen/oapi-codegen/v2 | v2.8.0 / tools | Generate Go DTO/client and strict Gin from authored public OpenAPI3.1. Test admitted3.1 subset; duplicate/allOf-loss and full schema validation remain PM obligations. Separate tools module graph. [Apache-2.0](https://github.com/oapi-codegen/oapi-codegen/tree/de2d8b2b0afb287198554eb305bb0d2687d26a85) |
| TD04 github.com/oapi-codegen/runtime | v1.7.0 / api/httpapi | Runtime support for generated Go API code. Generated helpers do not replace permission/schema validation. [Apache-2.0](https://github.com/oapi-codegen/runtime/tree/25e2d353ccbf722e1d991ff71beb9502a421d665) |
| TD05 github.com/getkin/kin-openapi | v0.149.0 / compile/tools | OpenAPI2/3 parsing, explicit Swagger conversion and source-origin references. Closed captured-resource loader; custom ReadFromURIFunc must reject unknown refs itself; reject unsupported dialects/shapes. [MIT](https://github.com/getkin/kin-openapi/tree/1a812b4b73ede7fa295c63a5c89d1ca7250dcc07) |
| TD06 github.com/santhosh-tekuri/jsonschema/v6 | v6.0.3 / contract/schema/httpapi | Canonical JSON Schema2020-12 and admitted provider schema validation. Pin draft and format/content assertion policy; bounded recursion/number fidelity; no network loader. [Apache-2.0](https://github.com/santhosh-tekuri/jsonschema/tree/b0fc661f4939578bc429f408c18202573dbafcc4) |
| TD07 go.yaml.in/yaml/v3 | v3.0.5 / compile/tools | YAML AST for provider/API source loading with line/pointer provenance. Reject duplicate/non-string keys, unsupported tags and excessive aliases before normalization. [MIT/Apache-2.0 file-specific](https://github.com/yaml/go-yaml/tree/e16c7af9361b241fa02d91582fb59ce4954d8afc) |
| TD08 github.com/spf13/cobra | v1.10.2 / cli | Real static/dynamic command tree, help/completion and presence-aware flags. Strict encoding/json config instead of Viper; explicit workspace and reserved aliases. [Apache-2.0](https://github.com/spf13/cobra/tree/88b30ab89da2d0d0abb153818746c5a2d30eccec) |
| TD09 github.com/jackc/pgx/v5 | v5.11.0 / store/runtime | Separate PM PG pools and customer PostgreSQL typed native transport/COPY. Different DB/roles/connection identities; codecs/timezones/query mode and ambiguity tested. [MIT](https://github.com/jackc/pgx/tree/5e583fa7aabfa88b796292f849fc9d7d75ac159d) |
| TD10 PostgreSQL | 16.15 / operator/store | Canonical pm_app; separately owned Temporal persistence/visibility and filer metadata databases. Chosen documented tested major; exact patch/image/schema/roles integration still required. [PostgreSQL License](https://github.com/postgres/postgres/tree/7d3e000c5961a544302072058a1184e9a588837b) |
| TD11 github.com/pressly/goose/v3 | v3.28.0 / store/migration job | Embedded ordered PostgreSQL SQL migrations only. Disable env interpolation; isolated migrator, no automatic destructive Down. PM never migrates Temporal/filer tables. [MIT](https://github.com/pressly/goose/tree/43d2d9c819ed6c9ba2b67a86bdf9fc08562495b7) |
| TD12 Temporal Server / Go SDK / API Go | 1.31.2 / 1.48.0 / 1.63.4 / temporal/workflows/activities | Durable orchestration, timers, schedules, versioned workers and handoff APIs. Server commit19a774302c613da9adc4436ab14278ccdca8e0a5; API commit60e39586a91fbf101a8763293ad55dae4fbb9f49. Determinism, history, CAS, retention and explicit policy required. [MIT](https://github.com/temporalio/sdk-go/tree/b7c242c6894df088a57a85b33d0586e908da8b93) |
| TD13 github.com/duckdb/duckdb-go/v2 | v2.10505.0 / analytics child only | DuckDB1.5.5 query and Parquet materialization. Native cgo only in pm-query; no shared writable file; process/AST/secret/file bounds. [MIT](https://github.com/duckdb/duckdb-go/tree/36040984c722e0f174b880eb7d77e238b3d8bc61) |
| TD14 SeaweedFS / AWS S3 SDK | 4.46 / service/s3 v1.113.0 / objects/operator | One S3 immutable artifact contract; PostgreSQL filer metadata. SDK commitb189f382f4924bc6c948c9942e17c547553faf0d pins rootSDK1.47.0/Smithy1.28.1. Pool/listing, multipart and digest behavior qualified; unique attempt keys. [Apache-2.0](https://github.com/seaweedfs/seaweedfs/tree/d997fba1575583a89cf0cc50dc0150642286c86d) |
| TD15 github.com/coreos/go-oidc/v3 | v3.21.0 / auth | Configured issuer verification for human login. Issuer/audience/signature/expiry/nonce/state and configured JWKS trust; OIDC identity is not PM resource permission. [Apache-2.0](https://github.com/coreos/go-oidc/tree/c914bd380327a5a3a81403774d1a5d5b73772ce7) |
| TD16 golang.org/x/oauth2 | v0.37.0 / auth/credentials | Code+PKCE/device helper and server-side provider OAuth refresh. PG refresh CAS/rotation persists separately from in-memory TokenSource; lost rotating refresh may need reauthorization. [BSD-3-Clause](https://go.googlesource.com/oauth2/+/c624b89dadc3221560b7345c090bbe69e90808ee) |
| TD17 github.com/zalando/go-keyring | v0.2.8 / CLI only | OS storage for PM login/session handles. No server wrapping-key authority here. Precheck fixed encoded size before macOS Set; no plaintext fallback. [MIT](https://github.com/zalando/go-keyring/tree/a8cdfe320cc8bc0534c895a648dc9214715a9da5) |
| TD18 golang.org/x/sync | v0.23.0 / worker/application concurrency | Bounded errgroup/cancellation for ordinary Go work, never workflow goroutines. Temporal workflows use SDK primitives; SetLimit not changed while active. [BSD-3-Clause](https://go.googlesource.com/sync/+/f75267d8412fc1dfd12b343644a7ea46e4d9c85d) |
| TD19 golang.org/x/time | v0.16.0 / optional extra process cap | Local rate cap as defense in depth only. PG account permits own cross-replica quota; no custom fair scheduler authority. [BSD-3-Clause](https://go.googlesource.com/time/+/fb013b3d305a26f5ef5350a6eaffc7c87a200383) |
| TD20 go.opentelemetry.io/otel | v1.46.0 / server/worker observability | Traces/metrics1.46.0 with SDK/metric/OTLP-HTTP exporter modules at1.46.0; slog logs. otelhttp0.71.0; bounded lossy telemetry, no secrets or high-cardinality IDs; durable PG audit separate. [Apache-2.0](https://github.com/open-telemetry/opentelemetry-go/tree/58db4c898f5b5594f8ba78f156475bf48486e2f2) |
| TD21 github.com/testcontainers/testcontainers-go | v0.44.0 / isolated integration tooling | Disposable exact-image PG/protocol fixture when owning an isolated test resource is useful. Same selected PG/stack behavior; no alternate product backend; explicit container runtime and cleanup ownership. [MIT](https://github.com/testcontainers/testcontainers-go/tree/007bd6b1407fe3f2ab9487479e8bd841e16a1c97) |
| TD22 honnef.co/go/tools | v0.8.1 / quality tooling | Staticcheck0.8.1 with gofmt/vet/race/fuzz. Pinned tools graph and scoped checks; not correctness proof alone. [MIT](https://github.com/dominikh/go-tools/tree/1285a6a5ec1e0ebb658f49e82b6c566a878cc3cb) |
| TD23 golang.org/x/vuln | v1.8.0 / quality tooling | govulncheck1.8.0 on resolved Go graph. Record advisory DB timestamp; native/image vulnerabilities need separate scanning. [BSD-3-Clause](https://go.googlesource.com/vuln/+/709015412431dd2b5b28a53c06c70bc02d49074c) |
| TD24 Compose / Caddy | 5.5.1 / 2.11.4 / operator | Explicit stack supervisor/launcher and TLS ingress. Caddy commite2eee6a7fce366321294c9c2a79f3146891dcbdf; managed ownership respected; Compose one host not HA. [Apache-2.0](https://github.com/docker/compose/tree/5f94fb0aa42a2cd1248c6e6c7fafb87546b9c8de) |
| TD25 openapi-typescript / openapi-fetch | 7.13.0 / 0.17.0 / browser SDK | Types and typed fetch wrapper from same public3.1 document. Server validation remains authoritative; cookie/CSRF/same-origin/AbortSignal and exact-number encodings. [MIT](https://github.com/openapi-ts/openapi-typescript/tree/0cc7ee77d28359c7901d9cd3b5733b70a050ea49) |
| TD26 Playwright / Node | @playwright/test1.63.0 / Node24 LTS / isolated browser test tooling | Actual browser auth/permission/parity harness. Exact Node patch and browser binaries/checksums lock atM0/M5 before use; no installs executed here. [Apache-2.0 / Node notices](https://github.com/microsoft/playwright/releases/tag/v1.63.0) |
| TD27 LLVM/Clang | 21.1.8 / native query helper build | C compiler baseline for qualified pm-query Linux targets. For development CI, pin and verify the actual compiler/linker, target sysroot headers/libraries, native archive and host OS/architecture through a reviewed immutable manifest; refuse identity drift before execution. An immutable build-image digest remains mandatory before image-based or release-native builds. This binds the existing native host environment, not a hermetic build image; no native cross-build assumption. [Accepted #3 amendment](https://github.com/polymetrics-ai/pm-cli/issues/3#issuecomment-5687998290). [Apache-2.0 with LLVM exceptions](https://github.com/llvm/llvm-project/releases/tag/llvmorg-21.1.8) |
| TD28 Syft / Trivy / cosign | 1.51.1 / 0.74.0 / 3.1.3 / release/security tooling | SBOM, native/image scanning and signed paired archives/images. Also official Trivy0.74.0 and cosign3.1.3 release pins from473; lock assets/advisory timestamp. Custom standard-library assembler replaces rejected packaging framework. [Apache-2.0](https://github.com/anchore/syft/releases/tag/v1.51.1) |

**Why Gin was not selected in473.** The earlier rationale was standard-library contract simplicity; its research explicitly said Gin/Fiber were not deeply evaluated. It did not establish that Gin was unsafe, incapable or unscalable. The changed remote application requirements justify the current comparison. Select Gin for cohesive route groups/middleware and a maintained generated strict adapter. This is a maintainability choice, not a benchmark-derived performance superiority claim. Chi5.3.2 remains the strongest smaller alternative; Echo5.3.1 has a coherent central error/serializer model, and standard `net/http` remains technically credible. We choose one adapter so the codebase does not carry several equivalent framework paths. [Gin source](https://github.com/gin-gonic/gin/tree/73726dc606796a025971fe451f0aa6f1b9b847f6), [Chi mux50–91](https://github.com/go-chi/chi/blob/38939062c5df4d3e8814aad1a488983112627ced/mux.go#L50), [Echo64–88](https://github.com/labstack/echo/blob/ed8bbe4b6cbf519766c99e492b9cc427404b3719/echo.go#L64).

**Observed source limits.** Gin trusts proxies by default; configure `SetTrustedProxies(nil)` unless exact ingress proxies are supplied. Default JSON binding does not enable exact-number or unknown-field rejection. Default recovery can expose request/panic data. Use custom allowlisted logging/recovery, bounded strict decoding, duplicate/trailing-member checks, full server schema validation and safe RFC9457 callbacks. Never retain a pooled Gin context in an activity or domain object: pass `Request.Context()`. SSE gets context-aware bounded streaming with per-write deadlines, not a short generic request timeout. [Gin202–240/443–454](https://github.com/gin-gonic/gin/blob/73726dc606796a025971fe451f0aa6f1b9b847f6/gin.go#L202), [binding16–55](https://github.com/gin-gonic/gin/blob/73726dc606796a025971fe451f0aa6f1b9b847f6/binding/json.go#L16), [recovery52–105](https://github.com/gin-gonic/gin/blob/73726dc606796a025971fe451f0aa6f1b9b847f6/recovery.go#L52).

The pinned generator supports selected OpenAPI3.1 shapes; use the same authored3.1 document for both clients and the strict Gin adapter, without a3.0 public bridge. Its generated checks are partial, and overlapping/unsupported schema constructs still need explicit fixtures or rejection. Provider specifications remain independent compiler inputs rather than thousands of generated provider-specific Go SDKs. [oapi-codegen README1347/2303–2307](https://github.com/oapi-codegen/oapi-codegen/blob/de2d8b2b0afb287198554eb305bb0d2687d26a85/README.md#L2303), [strict Gin template](https://github.com/oapi-codegen/oapi-codegen/blob/de2d8b2b0afb287198554eb305bb0d2687d26a85/pkg/codegen/templates/strict/strict-gin.tmpl#L1).

**PostgreSQL and Temporal cost.** Their benefits are shared transactions/authorization state and durable orchestration/timers/recovery. Their costs are explicit services, schema lifecycles, database I/O, history/visibility growth, versioned workers, monitoring and coordinated backups. Temporal requires separate persistence and visibility stores, with selected schema versions1.19 and1.14. PostgreSQL16.15 is a conservative patched-major selection based on tested-major documentation; exact selected-stack integration remains unexecuted. A shared PG instance saves infrastructure but correlates outages and resource contention. PM never writes Temporal internal tables. [Temporal persistence documentation](https://github.com/temporalio/documentation/blob/bc5c548f28d0bc0b72e31976e2865563f40a85fc/docs/encyclopedia/temporal-service/persistence.mdx#L21), [schema constants](https://github.com/temporalio/temporal/blob/19a774302c613da9adc4436ab14278ccdca8e0a5/schema/postgresql/v12/version.go#L5).

**Storage selection.** Bundle SeaweedFS4.46 behind one S3 adapter using AWS SDK service/s3 v1.113.0. Managed S3 must qualify the same behavior. Seaweed has a PostgreSQL filer store, but its source warns of nested listing deadlock with bounded pools; M0 must qualify and configure this before readiness claims. Unique immutable attempt keys avoid depending on unproven concurrent conditional-PUT atomicity. DuckDB1.5.5/duckdb-go2.10505.0 runs only in the analytical child; settings are defense in depth, not a sandbox for arbitrary SQL. DuckLake, ClickHouse or Trino are deferred until measured analytical concurrency/compaction needs justify another data-system boundary. [Seaweed postgres2 notes36–65](https://github.com/seaweedfs/seaweedfs/blob/d997fba1575583a89cf0cc50dc0150642286c86d/weed/filer/postgres2/postgres2_store.go#L36), [DuckDB security](https://duckdb.org/docs/current/operations_manual/securing_duckdb/overview).

PM's MIT choice is settled. Preserve third-party notices for MIT/BSD/Apache components and exact native distributions. The old packaging alternative rejected for an unwanted transitive dependency is not imported. Use the small repository-owned release assembler with pinned checksums, SBOM and signing tools. No source inspection here proves the combined build graph, native target or image is secure or operationally qualified.

## 6. Remote architecture and dependency direction

**Proposed/Recommended.** The dependency path is:

```text
pm CLI / future browser
  -> public OpenAPI3.1 -> Gin adapter -> application admission
  -> PostgreSQL RunSpec + grant + budgets + outbox + audit/events
  -> Temporal workflow -> registered worker activities
  -> shared HTTP / PostgreSQL / schema / sync / binary foundations
  -> provider systems and immutable S3 artifacts
  -> PG effect/member receipts + dataset manifest/checkpoint

warehouse query/materialization -> restricted pm-query child -> immutable objects
operator pm-stack -> explicit PostgreSQL / Temporal / object / API / worker services
```

`internal/contract` is pure values and interfaces. `internal/api` is public DTO/client behavior. `internal/httpapi` contains Gin and endpoint adaptation. `internal/app` contains use cases and the executable capability composition. `internal/store` owns PM PostgreSQL repositories. `internal/workflows` contains deterministic orchestration only; `internal/activities` invokes application/foundation services; `internal/temporal` owns handoff and projection adapters. Connector-specific data stays under `connectors/<name>`; compiled bundles are immutable generated data.

All provider I/O, including direct reads and metadata discovery that contacts a provider, runs in admitted worker activities. API synchronous reads serve PM-owned catalog/schema/run/identity metadata only. A direct read returns202 with a durable run; `pm` may wait and render that run's one-page result. Provider writes without approval produce a plan; an approval flag by itself never authorizes effects. API administrator credential ingress encrypts new values but is not given the worker's signer-resolution capability.

The API and worker use one executable capability composition; an actual function registration determines implementation availability. API code projects availability and performs admission, while only worker code receives execution dependencies. Compiler foundations compose in `pmgen`; pure values compose at serializers; proof foundations compose in `pmproof`; they are not invented Temporal handlers. Missing runtime wiring blocks only dependent cells. Atlas/proof status is never a runtime authority.

The file manifest distinguishes final allowed dependencies from imports needed when a file is first created. M3 server composition cannot import an M4 Temporal adapter before it exists. Pure ports are introduced first; later milestones explicitly update composition files to wire their new foundations. Before that wiring a feature is unexposed or unavailable, with no in-memory/local substitute engine. The CLI similarly gains provider catalog wiring at M6 after its M5 session/workspace shell.

A single shared object service is authoritative for bytes. Every worker can resolve a PG manifest and open the same objects from another host. Scratch and analytical native files are private to an attempt. Query children receive only admitted read-only files and typed expressions, never provider/storage credentials. There is no shared writable DuckDB file, arbitrary shell/code endpoint, generic HTTP write or unrestricted PM database SQL route.


## 7. Foundations and source-to-capability mapping

**Canonical foundation plan.** Retain FND-01 through FND-20 as stable design identities and apply the accepted F01/F02/F03/AUTH/PostgreSQL clarifications below. A version names a behavioral contract, not a release batch. `FOUNDATIONS.json` names the actual compiler, serializer, runtime, proof or operator composition; it is not another runtime registry. Every implementation and evidence status remains planned/unexecuted.

| ID / version | Foundation / owner | Interface and actual composition | First / ready phase |
|---|---|---|---|
| FND-01@1.0.0 | Source capture/provenance / compile | Exact repository/commit/path/pointer/blob/byte/SHA/license identity → immutable pinned source lock and complete reference closure; `internal/compile/source.go:CaptureSource` in the `cmd/pmgen` pipeline. Accepted GitHub raw bytes are not duplicated in docs; PostgreSQL uses official protocol/SQL/metadata sources. | M6 / M6 |
| FND-02@1.0.0 | Parsing/normalization / compile | SourceSet -> typed operations and diagnostics; internal/compile/normalize.go:Normalize and internal/compile/requirements.go:CheckRequirements | M6 / M6 |
| FND-03@1.0.0 | Typed values/serialization / contract | `internal/contract/value.go:Value` through runtime input, Parquet/analytics, API and client serializers preserves complete action data by default, nested objects/arrays, absent/null/value, false/zero/empty and exact identifiers/numbers; typed output selection does not change provider request meaning. | M1 / M1 |
| FND-04@1.1.0 | Operation identity/command projection / connectors | Operation ID -> safe command descriptor; CLI/UI project same public catalog; internal/connectors/registry.go:LoadBundle and internal/compile/commands.go:ProjectCommands | M6 / M6 |
| FND-05@2.0.0 | Authentication/secret resolution / credentials | RunGrant+ConnectionRef -> scoped signer only at physical effect boundary; internal/credentials/resolve.go:ResolveForEffect injected only into worker execution dependencies; vault admin ingress separate | M3 / M4 |
| FND-06@1.0.0 | HTTP transport / runtime | TypedOperation+Input -> one bounded HTTP attempt; internal/app/capabilities.go:RegisterHTTP binding runtime/http.go executor; activities/execute.go consumes actual handler | M6 / M6 |
| FND-07@1.0.0 | SQL transport/metadata / runtime | Official-source-derived typed PostgreSQL metadata/read and bounded keyed/mapped-column plans → quoted identifiers, bound values and a parameterized native request; `internal/app/capabilities.go:RegisterSQL` binds `runtime/postgres.go`, consumed by `activities/execute.go`. No OpenAPI assumption or arbitrary SQL. | M7 / M7 |
| FND-08@1.0.0 | Pagination/bounded queries / runtime | Declared provider navigation/effective request size plus total record cap and typed filter/selection → truthful page/continuation, unread tail, completeness and fetched/returned/emitted counts through `runtime/pagination.go:NewPaginator`, consumed by `sync/direct.go` and `sync/etl.go`. An empty filtered page is not exhaustion, and one direct-read page is not durable extraction/checkpoint incrementality. | M6 / M6 |
| FND-09@1.1.0 | Dynamic schemas / schema | Connection/account/source pin -> complete immutable metadata snapshot; internal/schema/discover.go:Discover and effective.go:BuildEffectiveSchema called by registered discovery activities | M7 / M7 |
| FND-10@2.0.0 | Retry/rate/backpressure / runtime | Finite deployment/workspace pending-admission profiles govern API acceptance; a scoped account permit, deadline and known replay disposition govern one physical provider attempt through `app/account_permits.go:AcquirePermit` and `runtime/admission.go:AdmitPhysicalCall`, with provider/run retry timing owned durably by the workflow/runtime. The separate client-to-API recovery loop is at most three same-key sends in 30 seconds and never invents commit certainty. | M4 / M4 |
| FND-11@1.1.0 | Binary transfer / runtime | Declared byte operation+ArtifactRef -> bounded verified transfer; internal/app/capabilities.go:RegisterBinary binding runtime/binary.go and objects/store.go | M11 / M11 |
| FND-12@2.0.0 | Extraction/checkpointing / sync | Page source -> verified publication and contiguous checkpoint; internal/app/capabilities.go:RegisterETL binding sync/etl.go and checkpoint.go | M7 / M7 |
| FND-13@2.0.0 | Warehouse publication/query / warehouse | Authorized object snapshot -> exact Parquet/query result and PG manifest; internal/warehouse/manifest.go:PublishManifest and child.go:ExecuteQueryChild; cmd/pm-query composes analytics only | M7 / M7 |
| FND-14@2.0.0 | Destination apply/member settlement / sync | Approved exact action/member set → independently correlated per-member outcomes and readback through `RegisterApply`, `sync/write.go`, `dispatch.go` and `settlement.go`. PostgreSQL mapped upsert assigns only changed approved writable columns; equality settles unchanged with zero assignments. Unknown COMMIT remains charged until a declared sound occurrence witness matches the exact intent; desired rows alone are insufficient, and actual dispatch history remains auditable. | M8 / M8 |
| FND-15@2.0.0 | Reverse mappings/worksets / sync | Saved mapping plus immutable snapshot and verified stable unique-key constraint → immutable desired workset and approved effects through `RegisterReverse`, `sync/reverse.go` and `workset.go`; reject missing/ambiguous/duplicate keys and preserve unmapped fields. Any key/account/generation reset is an explicit approved migration retaining the old mapping and dispatch history for audit. | M8 / M8 |
| FND-16@2.0.0 | Runs/tasks/recovery / app | Atomic PG idempotency/RunSpec/grant/outbox/pending reservation before 202 → durable Temporal coordination through `app/admission.go:AcceptRun`, `temporal/outbox.go:StartOrReconcile` and registered workflow/activity interfaces. Exact replay reserves/dispatches nothing; unresolved receipt guards never age out. Issue #14 proves the actual route and controlled durable execution-start transition; actual provider-worker integration remains issue #16. | M4 / M4 |
| FND-17@2.0.0 | Approvals/authorization / app | Exact sealed action set+current grant -> bounded execution authorization; internal/app/approvals.go:ApprovePlan and revocation.go:RecheckRunGrant before dispatch | M8 / M8 |
| FND-18@2.0.0 | Flows/schedules / temporal | PG desired revision -> Temporal schedule; effect-free occurrence admission adopts execution; internal/temporal/schedules.go:ApplyScheduleCAS; workflows/scheduled.go:PMScheduledRunWorkflow | M9 / M9 |
| FND-19@1.1.0 | Evidence/conformance / proof | Case plus independent oracle → sanitized immutable receipt comparing complete/selected source results, truthful pagination and exact destination readback through `proof/runner.go:ExecuteCase` and `oracle.go:AssertExpected` across the actual public client/API/worker path; no self-certified descriptor. | M6 / M6 |
| FND-20@2.0.0 | Instance lifecycle/distribution / server | Strict deployment config -> independently managed API/worker readiness, never dependency autospawn; scripts/pm-stack operator composition and internal/server/server.go:Serve, cmd/pm-worker main | M0 / M12 |

Foundation-wide REQ-FND rows define contracts and deliberately have no lane eligibility. Operation-specific rows own that decision; other seven-lane rows describe conditional behavioral impact. Their foundation lists are design impact sets, not a runtime AND gate. The compiler derives an exact capability closure: select HTTP or native SQL, add pagination only for a paginated collection, ETL publication/checkpoint contracts only for ETL, and approval/apply contracts for effects. Compiler, proof and complete distribution qualification never gate an otherwise runtime-valid operation. A static HTTP collection does not require PostgreSQL transport or unrelated dynamic metadata. First phase introduces a contract; ready phase denotes its complete planned qualification, not proof of present implementation.

Every admitted operation has an eight-part trace: source identity; typed transport/fields; behavior/lane dimensions; exact foundation requirements/registration; ownership/state; projection/admission; durability/security/recovery; and causal evidence/files/milestone. `FOUNDATION-MAPPINGS.json` retains the 41 original design requirements and adds `REQ-ADMISSION-F01` and `REQ-AUTH-MUTATION`, for 43 total. This remains a design requirement set, not a provider-operation parity census.

The compiler performs a bounded deterministic pipeline. First, validate the source envelope, dialect, license and captured reference closure; network references are never resolved during ordinary compilation. Second, preserve every operation and every request/response media/status variant with exact field origins. Swagger2 conversion is an explicit tested normalization, not a version-string edit. Third, preserve missing/null/empty/default and exact numeric/temporal meaning, including unions and array constraints. Fourth, resolve sparse source-linked semantic amendments: record extraction, pagination, returned IDs, safe retry witnesses, resource relationships and lane eligibility. Amendments cannot add arbitrary code, origins, credentials or HTTP bodies that bypass typed contracts.

Fifth, produce stable semantic operation IDs separately from friendly names. Namespace provider flags by their location when necessary, and deterministically alias conflicts with reserved PM controls. Sixth, check required foundation contracts against the actual installed handler set. Unknown semantics block the affected binding/cell with an Atlas gap; source inventory still includes it. Seventh, emit immutable bundles, safe command descriptors, seven lane inventories, shared schemas, help/manual projections and case manifests. Eighth, validate the complete generated closure and reproducible bytes before embedding it in the paired release.

Source captures and attribution are authoring data. Runtime bundles contain only execution facts and safe source revision identifiers, not private research citations or old receipts. Command help may show unavailable capabilities and precise reasons; ordinary execution never reads a proof catalog, review ledger or global selector. Regeneration has one entry script; derived bundles/help/lane inventories are never hand-authored in parallel.

Accepted source authority does not collapse source capture into documentation. The GitHub identity above is complete verified capture evidence, while issue #27 must still first-create source.lock, recursive closure, NOTICE, compiler outputs and honest lane inventories. The historical moving `WEB-GITHUB` page is no longer implementation authority. Issue #28 must pin complete relevant official PostgreSQL protocol, SQL and metadata definitions/attribution for the selected supported version; PostgreSQL has no assumed OpenAPI input. Source inspection and credential presence remain neither handler reachability nor provider proof.

Accepted F02 output controls belong to the shared typed operation/result surface, not a GitHub parser. With controls absent, the complete action record and every source entry flow through worker, server, Go client, CLI and browser. A typed record filter distinguishes absent/null/unlike types without coercion and runs against the unprojected record before typed field selection. Malformed controls fail shared preflight before provider I/O. Provider request filters remain separate. Page size, total record cap, unread tail, continuation, provider completeness and fetched/returned/emitted counts stay independently truthful.

**Extension rule.** If a supported operation needs only parameter/schema/response facts, add declaration data. If several operations require a missing encoding or semantic mechanism, extend a compatible existing foundation through its declared seam. If that changes behavior incompatibly, version the contract and add a causal regression. Introduce a new foundation only when the semantic mechanism has a distinct owner and reusable interface. Add a bounded native adapter only for a genuinely different protocol such as PostgreSQL. A connector name is not a reason for an adapter, service, Go test package or runtime branch.

## 8. API, worker, CLI configuration and stack deployment

**Proposed/Recommended.** Ship three public deployment choices that package the same architecture: client-only `pm`; backend-only stack containing API/worker/query components; full distribution containing both. Linux amd64/arm64 are backend image qualification targets. The CLI also targets the previously selected macOS architectures; actual native and signing checks must pass before any target is advertised. Windows is deferred. Only the query helper imports DuckDB/cgo; client and API/worker composition must remain free of native analytical linkage by import-boundary tests.

A configured operator-owned OIDC issuer is required for human login; PM does not silently provision an identity provider or ship a default password. Offline installation means preloaded verified software assets; remote provider and identity operations still require their configured endpoints. The query helper receives only scoped local files and a typed analytical request, with a cleared environment, closed inherited descriptors, no provider/object credentials, disabled extension/network access, and enforced process/container limits. It fails closed where those isolation controls cannot be established.

`pm-stack` is an explicit operator launcher, not an ordinary provider command. Its proposed actions are `check`, `configure`, `up`, `migrate`, `status`, `drain`, `backup` and `restore`. A deployment lock protects provisioning of that named installation only. API/CLI exit never starts/stops a private PostgreSQL, Temporal or object service. Operator stop/uninstall acts only on owned bundled components and preserves volumes/keys/backups; managed dependencies are not initialized destructively, stopped or deleted.

Use Compose5.5.1, Caddy2.11.4, PostgreSQL16.15, Temporal server/admin-tools1.31.2 and SeaweedFS4.46 at the inspected image-index and per-platform digests in `tech/IMAGE-PINS.json`. Digest observations establish identity, not startup/readiness/security. PM image hashes do not exist yet and must not be invented. M0 qualifies dependency configuration; M12 qualifies native paired releases and offline image archives. No floating `latest` tag or silent package download in release/install scripts.

| Selected dependency | Observed OCI index digest | Execution status |
|---|---|---|
| docker.io/library/postgres:16.15 | sha256:f1c3376c26f2609ab9f29f71f824103fe2fcd8ee0346485cb6122a4f93df6f94 | Registry identity inspected; native/schema/readiness checks UNEXECUTED |
| docker.io/temporalio/server:1.31.2 | sha256:b5ecdb8282bededae2a10c36e8d862e27d0bc2d247fc73c5416025997ab4a1da | Registry identity inspected; native/schema/readiness checks UNEXECUTED |
| docker.io/temporalio/admin-tools:1.31.2 | sha256:dbc5fcd6ee8f0f4d808bf765af9a87dea9d8a283abfdcfbd2fc148496ba66107 | Registry identity inspected; native/schema/readiness checks UNEXECUTED |
| docker.io/chrislusf/seaweedfs:4.46 | sha256:08d516132314207d10c8e37cbffc1f32b147d870169688734cc61c6231625b62 | Registry identity inspected; native/schema/readiness checks UNEXECUTED |
| docker.io/library/caddy:2.11.4 | sha256:13ba145cba2f3e28fa801994876e4c086d1b95d5aa2a520a734765ffb6b12017 | Registry identity inspected; native/schema/readiness checks UNEXECUTED |

One small PostgreSQL instance may host `pm_app`, `temporal`, `temporal_visibility` and `pm_objects_meta`, each with distinct owner/migrator/runtime principals. PM migrations touch only `pm_app`; Temporal schema jobs own persistence/visibility; Seaweed owns filer metadata. A customer PostgreSQL connector uses separate explicit connection/credential identity and cannot implicitly bind to infrastructure databases. Separate PG instances are an operational isolation choice, not another product mode.

The operator starts dependency services, runs their explicit schema/init jobs, verifies schema and authenticated readiness, provisions the first administrator/wrapping-key references, then starts API and workers. A listening socket is insufficient readiness. PostgreSQL schema compatibility, secret-key availability, Temporal namespace/worker registration and object integrity access are checked. No default public DB/Temporal/filer ports or embedded development passwords. Local development uses the same stack and explicit secrets through operator configuration.

**Synthetic configuration sketch — accepted admission values, illustrative field spelling, no real endpoints or secrets:**

```json
{
  "schema_version": "1",
  "deployment_id": "synthetic-deployment-475",
  "public_origin": "https://pm.example.invalid",
  "postgres": {
    "ownership": "bundled",
    "database": "pm_app",
    "endpoint_ref": "secret://infra/pm-dsn"
  },
  "temporal": {
    "ownership": "bundled",
    "endpoint": "temporal:7233",
    "namespace": "pm-production",
    "client_identity_ref": "secret://infra/temporal-mtls",
    "retention": "720h",
    "task_queues": [
      "pm-control",
      "pm-read",
      "pm-effect",
      "pm-binary-query"
    ]
  },
  "artifacts": {
    "ownership": "bundled",
    "driver": "s3",
    "endpoint": "https://objects.internal.invalid",
    "bucket": "pm-artifacts",
    "credential_ref": "secret://infra/object-access",
    "attempt_key_policy": "unique_immutable"
  },
  "identity": {
    "issuer": "https://identity.example.invalid",
    "client_id": "pm-server",
    "client_secret_ref": "secret://infra/oidc"
  },
  "encryption": {
    "wrapping_key_ref": "secret://infra/pm-wrap-v1"
  },
  "admission": {
    "profile": "development",
    "resolved_limits": {
      "global_pending": 32,
      "workspace_pending": 8,
      "workspace_max_pending_age": "2m"
    },
    "field_spelling_owner": "server configuration owner; reject aliases/unknown keys"
  },
  "limits": {
    "workspace_active_runs": 4,
    "workflow_payload_bytes": 65536,
    "workflow_fanout": 32,
    "run_lifetime": "24h",
    "event_buffer": 128,
    "query_children": 1,
    "query_child_memory_bytes": 4294967296,
    "query_child_deadline": "5m",
    "request_bytes": 1048576,
    "metadata_deadline": "5s"
  },
  "label": "SYNTHETIC non-secret illustration using the accepted development admission profile"
}
```

Accepted production admission profiles are development `32/8/2m` and standard `256/32/10m` for deployment pending, workspace pending and workspace oldest-pending age. Generated local development selects development; every deployment explicitly selects one profile. Typed overrides resolve once through the configuration owner, remain finite/strictly positive, require workspace count no larger than deployment count and apply across replicas through one durable authority. `3/2/30s` is test-only. Historical `1000/1h` is not a production default. `limits.run_lifetime` is a separate required finite operator value; the synthetic `24h` shown above is an example, not a production default, and query or metadata deadlines are never reused for it. Exact external config field spelling remains the configuration owner's output; no aliases or second settings reader are permitted.

Configuration rejects unknown/duplicate/wrong-type or obsolete keys, zero/negative/unbounded admission values, workspace count above deployment count, invalid ownership, unsupported versions, unsafe origins and impossible budgets. `deploymentOwnership` is bundled/managed per dependency; there is no local/distributed switch. A managed S3 service must pass the same integrity/multipart/access contract as bundled Seaweed. The filer configuration explicitly enables PostgreSQL and TLS; copied defaults are unsuitable. If qualification materially fails, replace the selected service across deployments through a reviewed artifact-contract change, never a hidden file fallback.

Caddy provides public TLS ingress; the API trusts only explicitly configured proxies and origins. Browser cookies and CLI bearer sessions have different transport protections. Internal service identities/mTLS and network policy keep Temporal/admin/storage endpoints private. A single-host stack is remote-accessible, not highly available. Multi-host operation requires durable PG/object infrastructure and compatible API/worker versions; it does not require Kubernetes.

## 9. Identity, permissions, credentials, API events and state authority

**Canonical identity boundary.** Organization is the tenant boundary; workspace is the invocation/resource boundary. A human can belong to several workspaces. Service accounts are organization-owned automation identities with explicit workspace, connection, operation and expiry limits. Optional delegation is at most one bounded hop initially and cannot exceed the parent grant. Persist requested actor, effective actor and executing worker separately.

Connections belong to a workspace. Different connection aliases may point to the same provider account, so authentication refresh/cooldown/rate coordination uses a canonical provider-account witness rather than raw credentials or process-local keys. Entity identity, action occurrence, dispatch envelope and physical attempt remain distinct. A legitimate second action on one entity gets a new effect occurrence; an exact retry retains the intended effect and payload identity.

| Identity | Purpose | Invariant |
|---|---|---|
| org_id | Tenant/organization authorization and billing boundary | Opaque stable ID; workspace belongs to exactly one org |
| user_id | Human identity independent of workspace | One human may hold multiple workspace memberships; identity provider subject is issuer-scoped |
| membership_id | Principal-to-workspace role and resource grants | Versioned policy epoch, expiry/revocation; role alone not permission to every connection |
| service_account_id | Authorized automation identity | Org-owned, explicit workspace/connection/operation scopes, expiry and separate credential rotation |
| delegation_id | Optional bounded authorized-agent delegation | At most one explicit delegation hop initially; cannot exceed parent grant; immutable requested_actor/effective_actor attribution |
| workspace_id | Per-invocation resource boundary | Mandatory in every workspace route and CLI effect invocation; no mutable global current workspace |
| connection_id | Workspace-owned provider configuration handle | Aliases may share provider account but grants remain separate |
| provider_account_id | Canonical provider account/instance witness | Shared distributed auth/rate coordination identity; safe nonreversible internal key, never raw credential |
| credential_ref | Opaque reference to server-owned ciphertext version | No raw value/locator in CLI/UI public schema/history |
| profile_id | Logical synchronization policy identity | Immutable revisions; stable creation ledger survives nonidentity revisions |
| plan_id | Sealed intended actions and workset | Exact schema/account/mapping/actions/budgets; If-Match for editable candidates, immutable accepted snapshot |
| flow_id | Explicit typed DAG identity | Revisioned graph; no inferred dependencies |
| run_id | PM durable accepted intent | Stable across Temporal retries and ContinueAsNew; source of public run identity |
| workflow_id | Temporal orchestration identity | API-origin uses pm/run/<opaque UUID>; schedule-origin adopts its actual Temporal execution after effect-free admission. Both map to stable PM run identity; never start a second workflow for admitted schedule origin. |
| temporal_run_id | Temporal execution-chain member | Changes across retries/ContinueAsNew; mapped as orchestration observations |
| entity_key | Logical provider entity identity | Source/destination account/resource/key + explicit creation generation |
| effect_key | Intended action occurrence identity | Entity key + explicit logical action occurrence/revision where semantically required; exact retry retains key |
| dispatch_id | One exact physical request envelope | Ordered member IDs, payload hash/ref, attempt and pre-I/O intent |
| member_id | Independently attributable input/effect member | Never infer result from aggregate counts or response array position without declared contract |
| artifact_id | Registered immutable business bytes | Workspace scope, digest/length/media/object version; no arbitrary paths or client storage authority |

Use application authorization **plus PostgreSQL RLS**, rather than app-only filters. RLS is defense against omitted filters, not client authentication or protection from a fully compromised trusted PG principal that can choose its own scope. Non-owner API/worker roles have no superuser/BYPASSRLS/DDL/TRUNCATE/role privileges. Scoped tables use USING and WITH CHECK policies, composite tenant/workspace foreign/unique keys and safe errors. Verified scope is installed through parameterized transaction-local `set_config(...,true)`. Every path commits/rolls back before returning a pooled connection; cleanup failure destroys that connection. Missing/inherited scope is rejected. Owners normally bypass RLS, so isolated owner/migrator identities and FORCE policies where appropriate are deliberate. [PostgreSQL RLS2376–2434](https://github.com/postgres/postgres/blob/7d3e000c5961a544302072058a1184e9a588837b/doc/src/sgml/ddl.sgml#L2376), [SET LOCAL46–66](https://github.com/postgres/postgres/blob/7d3e000c5961a544302072058a1184e9a588837b/doc/src/sgml/ref/set.sgml#L46).

Login-session validity is checked at API acceptance and subsequent control actions. Accepted jobs use a durable execution grant with its own expiry and policy epochs. Ordinary logout, browser closure or short-lived session expiry does **not** revoke that grant. Explicit principal/run-grant revocation, expired delegation, removed membership, revoked connection permission or invalidated approval stops future dispatch. A request already sent cannot be unsent. Restricted reconciliation-only authority may perform declared safe readbacks for already-dispatched effects after write permission is revoked; it cannot issue a new mutation. Test both logout survival and explicit revocation.

Provider secrets enter only authenticated server administration with suitable scope and step-up/CSRF protections. The server encrypts them under an operator-controlled wrapping-key reference and returns status/version only. Worker signer resolution checks the run/account/grant immediately before physical I/O. The CLI stores PM session handles in the OS store, not provider credentials. Browser sessions use HttpOnly Secure cookies; no provider secret or browser refresh token in localStorage. Temporal histories, heartbeat details, search attributes, identifiers, logs, errors and ordinary artifacts contain only safe opaque refs and bounded counts/reason codes. Encryption/data converters are defense in depth, not an excuse to put plaintext or sensitive metadata in histories.

The public API uses `/v1/info` as its only compatibility-info route, OpenAPI3.1, typed bodies, scoped opaque cursors and RFC9457 errors. `/v1/info` exposes minimal public version/instance/features. Device initiation is bounded and unauthenticated; exchange requires one-time approved proof, expiry and poll cadence. Browser OIDC callback creates a cookie session and is a distinct authentication transport, not device-token JSON. After authentication both clients share application use cases. Provider-secret admin schema is excluded from ordinary generated clients.

**Accepted F01 admission wire contract.** Authentication and scope-safe authorization run before idempotency/replay disclosure. Safe RFC9457 results are exhaustive for this boundary:

| Condition | HTTP | Safe code | Safe retry action |
|---|---:|---|---|
| Authentication missing/invalid | 401 | `authentication_required` | `refresh_authorization` |
| Resource absent or scope-hidden | 404 | `resource_not_found` | `correct_scope` |
| Required grant unavailable | 403 | `grant_unavailable` | `refresh_authorization` |
| Registered capability unavailable | 503 | `capability_unavailable` | `after_capability_change` |
| Foundation incompatible | 409 | `foundation_incompatible` | `after_capability_change` |
| Specification stale | 409 | `specification_stale` | `refresh_specification` |
| Workspace pending count full | 429 | `workspace_admission_count_limit` | `after_backlog_progress` |
| Workspace oldest pending age at/over limit | 429 | `workspace_admission_age_limit` | `after_backlog_progress` |
| Deployment pending count full | 503 | `deployment_admission_overload` | `after_backlog_progress` |
| Control store unavailable before known commit | 503 | `control_store_unavailable` | `same_key_after_dependency_recovery` |
| Admission commit outcome unknown | 503 | `admission_outcome_unknown` | `same_key_reconcile_only` |
| Resume unavailable | 409 | `resume_unavailable` | `refresh_run_state` |
| Required control revision missing | 428 | `control_revision_required` | `refresh_control_revision` |
| Control revision mismatch | 412 | `control_revision_mismatch` | `refresh_control_revision` |
| Committed key reused with changed canonical input | 409 | `idempotency_conflict` | `new_key_for_new_intent` |
| Malformed/unsupported typed request | 400 | `invalid_request` | `correct_input` |
| Safe retry response expired after known resolution | 410 | `retry_receipt_expired` | `inspect_existing_result` |

If either workspace refusal coincides with deployment overload for the same new admission, the workspace-specific 429 wins: evaluate workspace pending count first, nonempty oldest workspace pending-reservation age second and deployment pending count last. Development is `global=32/workspace=8/workspace-age=2m`; standard is `256/32/10m`. The generated local setup selects development. A deployment explicitly selects one profile; finite positive typed overrides resolve once through the configuration owner, require workspace count no larger than global count and are shared across replicas. `3/2/30s` is test-only; `1000/1h` is not a production default.

One serialized PostgreSQL transaction evaluates post-reservation `<=` pending-count caps and oldest immutable `accepted_at` age using the authoritative PostgreSQL clock; age refuses at `>=`, including equality, while an empty pending set has no age refusal. It commits idempotency, RunSpec/grant/run, audit/event, outbox and pending reservation atomically before 202. Pending reservations are distinct from immutable per-run/window/account execution budgets and persist through queued, blocked, reconciling, awaiting-approval and Temporal acknowledgement until the controlled durable execution-start transition or a known terminal pre-start outcome. Retry never resets age, admission pressure never deletes accepted work, and issue #16 later proves actual provider-worker integration. Exact authorized replay returns the original run without a second reservation, dispatch or capacity check; uncertain commit reconciles only the same key.

CLI/browser clients persist a secret-free key/intent fingerprint before the first client-to-API send and make at most three such sends inside one 30-second deadline with 250ms then 1s backoff. A server `Retry-After` may delay only within the remaining client deadline; provider/run retry timing and provider `Retry-After` handling are independent. Clients never mint a replacement key after uncertainty, auto-rebase 409/412/428 or auto-resubmit 410. Full safe responses remain at least seven days after known resolution; unresolved receipts/digests/guards never expire. Resolved responses compact atomically to a secret-free key/digest/outcome-reference refusal guard for the immutable namespace lifetime: exact old intent returns 410 when the full response is gone, changed intent remains 409, and workspace deletion cannot recycle the namespace.

**Accepted AUTH-MUTATION-PARITY.** Only `CP-memberships-grant/revoke`, `CP-service-accounts-create/revoke` and `CP-delegations-create/revoke` change. Before replay disclosure, require current authentication and workspace/resource authorization. A new mutation requires `If-Match` and a key scoped by workspace, principal, operation and target; the canonical digest includes the original expected revision. Missing/stale revision is 428/412 with zero effects. Exact same-key/input replay is resolved before new-revision comparison and returns 200 with explicit replay plus the original safe result/revision and no mutation; changed input is 409. Effect, collection/resource revision and receipt commit atomically; already-revoked is unchanged, not a second effect. Original grant/create succeeds with 201, revoke/unchanged with 200. Service-account and delegation grants retain explicit connection/operation/workspace/time ceilings, and actor/effective actor remain distinct and auditable. Only original successful service-account creation may emit one-time PM automation credential material; replay/read/cache/receipt/log/error omit it, and loss requires explicit authorized revoke/new create. The three GET rows remain scoped reads. No provider credential or extra review gate is added.

**Canonical state ownership and disagreement resolution:**

| ID / field | Authority / owner | Guard and disagreement recovery |
|---|---|---|
| S01 — Organizations, users, workspace memberships, service accounts, delegation grants, sessions and durable run grants | PM PostgreSQL / auth | Session validity governs acceptance/control, while the persisted execution grant and current effect policy govern dispatch independently of logout/session expiry. Current auth/scope precede disclosure for the six named mutations, whose effect/revision/scoped receipt commit atomically; exact replay returns prior safe metadata and changed intent conflicts. Service/delegation grants retain connection/operation/workspace/time ceilings and distinct auditable actor/effective actor. Explicit principal/run-grant/membership/service/connection/approval revocation stops future effects; immutable attribution remains and narrower read-only reconciliation may continue without mutation authority. One-time service-account credential exists only in original successful creation; loss requires explicit revoke/new create. |
| S02 — Connection, provider-account witness and credential versions/ciphertext | PM PostgreSQL plus operator-owned wrapping-key service / credentials | Scope and account revalidated before signer resolution; CAS refresh version and distributed lock/lease Refresh loss may require reauthorization; alias connections share account coordination identity |
| S03 — Immutable RunSpec, complete action set, profile/flow/workset/schema bindings, submission key/digest, pending reservation and retained outcome guard | PM PostgreSQL / app/store | One serialized transaction uses PostgreSQL time to evaluate post-reservation workspace/deployment pending-count caps and nonempty oldest pending-reservation `accepted_at` age at `>=` its cap, applying workspace count then workspace age before deployment overload when predicates coincide; it then commits idempotency, RunSpec/grant/run, audit/event, reservation and outbox before 202. Pending reservations are distinct from active/run-window/account budgets. Exact replay reserves/dispatches nothing; release occurs only at the controlled durable execution-start transition or terminal pre-start failure, never Temporal acknowledgement. Workflow outage leaves bounded pending handoff visible; unresolved guards never expire and resolved responses compact only after at least seven days. |
| S04 — Workflow/activity schedule, durable timers, retry state and workflow history | Temporal / workflows/activities | Deterministic workflows; all external I/O in activities; bounded metadata reads may remain synchronous in API Workflow retry/replay cannot rewrite PM effect truth; missing/expired history is explicit repair condition |
| S05 — Physical dispatch intent, member identities, attempts, effects and exact acknowledgement/readback | PM PostgreSQL / sync/store | Persist exact dispatch/member intent and atomically reserve response/member/byte capacity before I/O; settle per member with CAS/fence. Fences stop stale local commits but cannot unsend a physical request. PostgreSQL mapped upsert seals stable key/constraint, approved mapped values and changed columns; equal values assign nothing and unmapped fields/unrelated rows survive. A confirmed rollback can permit safely re-admitted work but is not proof no physical dispatch occurred. Possible COMMIT stays unknown/charged until a declared sound occurrence witness matches the exact intent; desired rows alone are insufficient, blind retry is forbidden and actual dispatch history remains auditable. |
| S06 — Run state, task/member counters, results and terminal effect classification | PM PostgreSQL / app/store | Reducer gives unknown/reconciling precedence; blocked task reason and cancel_requested are separate Temporal completion without final PM projection remains projection_pending; reconcile from PM receipts, never infer success from workflow closed |
| S07 — Admission outbox, workflow start binding and schedule mutation desired/applied revision | PM PostgreSQL / temporal/store | API-origin stable StartRun outbox; schedule-origin EnsureWorkflowBinding outbox. Schedule updates use Describe conflict token plus persistent exact-payload RequestId; rebase changes dispatch identity. No atomic PG/Temporal transaction claim; retry same start, distinguish running/closed/absent/history-retained; never restart closed effect scope blindly |
| S08 — Schedule desired configuration, grants, revisions and occurrence admission ledger | PM PostgreSQL / app/store | Temporal schedule workflow first admits occurrence with PG RunSpec/budget/audit/binding-outbox. Uniqueness is schedule ID plus nominal UTC instant; adopt actual execution, never duplicate StartRun. Explicit 1h catchup, SKIP across actual run, DOM/DOW AND and both DST folds. PM does not run a competing cron loop; stale trigger revision is blocked explicitly rather than rebound to latest flow |
| S09 — Provider-account concurrency/rate reservations across aliases and worker replicas | PM PostgreSQL / store/app | Atomic account permit/token reservation, short expiry and durable retry eligibility; scope-derived canonical account identity A per-process limiter is only an additional cap; queue limits do not imply global account limits; unknown effects retain relevant budgets |
| S10 — Immutable artifact bytes, Parquet segments and sealed worksets | Shared object storage / objects | Complete bounded upload, verify hash/length/version, then PG registers object/manifest revision and cursor Object success before PG failure leaves an orphan; PG never references partial bytes; no filesystem rename/cross-storage atomicity fiction |
| S11 — Dataset manifest revisions, exact segment sets, snapshots and cursors | PM PostgreSQL / warehouse/store | Manifest/cursor/ack transaction under scope and CAS/fence; empty publication gets generation witness Compaction publishes new immutable objects then CAS manifest; GC uses snapshot/plan/proof/backup reachability and retention |
| S12 — Ephemeral query compute, download buffers and worker scratch | Worker process only / warehouse/runtime | Bound memory/disk/rows/time; reconstruct from registered object snapshot Worker death discards scratch, not accepted intent or committed data; retries use same snapshot |
| S13 — Safe event history, audit and SSE sequence/snapshot | PM PostgreSQL / store/httpapi | State/effect/receipt/audit/event commit together; scoped cursor, retention gap and consistent snapshot Any API replica can replay; local bus loss cannot lose history; client disappearance does not cancel run |
| S14 — Catalog bundle and foundation implementation availability | Immutable release bundle plus actual worker handler registrations / app/connectors | One actual handler registry; version closure and real reachability gates; Atlas/proof not runtime authorities Missing worker capability blocks dependent cell; descriptor alone cannot advertise implemented execution |
| S15 — Discovered account schemas and active snapshot selection | PM PostgreSQL / schema | Candidate completeness/content fingerprint/account scope, If-Match promotion and immutable accepted bindings Relevant drift blocks new effects; partial capture cannot replace complete; unrelated fields do not invalidate unused bindings |
| S16 — Build, deployment and environment configuration | Versioned strict operator configuration and release manifest / config/packaging | One explicit development/standard pending-admission profile plus finite positive typed overrides resolves through one registry; workspace ≤ global and replicas share the durable deployment budget. Reject unknown aliases, `1000/1h` and test-only `3/2/30s` as production defaults. The operator explicitly starts/provisions owned dependencies and separately started API/workers; API/CLI never autostarts private PG/Temporal/object services. Missing dependencies produce precise readiness/degraded admission state, with no hidden SQLite/in-memory/local fallback. |
| S17 — Backup recovery epoch, restore manifest and dispatch quarantine | PM PostgreSQL plus coordinated Temporal/object/key backups / operations | Restore components at explicit consistent checkpoints; verify references and retain uncertain effect intents Stale restoration can lose evidence of real effects: quarantine post-backup dispatch window and reconcile before writes, never claim rollback undid providers |

PM PostgreSQL owns accepted intent and effect truth. Temporal owns workflow/activity/timer/history state. The API exposes a timestamped orchestration observation beside PM effect state rather than pretending they are one field. `cancel_requested`, observed workflow cancellation, blocked reason and unknown-effect count are orthogonal. Workflow success cannot create successful member receipts; workflow failure cannot erase a provider effect. Unknown effects prevent a clean success/failure/cancellation claim until exact reconciliation resolves them.

**Synthetic public shapes, complete for their stated purpose:**

```json
{
  "task": {
    "task_id": "task_demo_1", "run_id": "run_demo_1",
    "org_id": "org_demo", "workspace_id": "ws_demo",
    "operation_id": "github.issues.create", "lane": "direct_write",
    "behavior_id": "create_issue_with_readback", "input_refs": ["artifact_input_1"],
    "schema_refs": ["schema_demo_1"], "state": "reconciling",
    "effect_summary": {"succeeded": 0, "unknown": 1}, "revision": 4
  },
  "event": {
    "org_id": "org_demo", "workspace_id": "ws_demo", "run_id": "run_demo_1",
    "sequence": 9, "state_revision": 4, "type": "task.reconciling",
    "safe_payload": {"task_id": "task_demo_1", "reason": "response_lost"},
    "committed_at": "2026-09-11T00:00:00Z"
  },
  "credential_public": {"connection_id": "conn_demo", "credential_version": 3, "status": "configured"},
  "synthetic": true
}
```

Each run's event sequence is allocated while locking that run row in the same transaction that changes state and inserts the event. A global sequence allocation alone does not establish commit order. SSE `id` encodes authorized run/scope/sequence; reconnection to another API replica queries committed rows after that sequence. A retention gap returns an explicit snapshot/cursor recovery response. Snapshot state revision and cursor are read consistently. Notifications only wake readers; they are not the durable event authority. Bound each consumer buffer, heartbeat and write deadline. V30 delays or rolls back the first of two writers and proves exact replay after another replica reconnects.

## 10. Command/UI parity and worked source mappings

**Canonical client projection; commands remain unimplemented.** `pm` is not another policy engine. One safe descriptor produces Cobra, generated Go/browser types, help/manual/completion and the same server DTOs. Provider request fields retain exact path/query/header/body mappings; output record filters are separate shared typed controls. Reserved PM controls include explicit workspace/connection/server profile, paging/record cap, output/wait/deadline, idempotency, approval and plan. False/zero/empty/presence remain exact; unknown/duplicate controls fail before submission and the server repeats validation. No client supplies provider credentials or direct-provider recovery.

With no result controls, every client receives complete action data and every source entry. Explicit typed field selection preserves selected nested values/presence; typed filtering distinguishes absent/null/unlike values and evaluates before selection. Pagination reports actual wire size, total record cap, unread tail, continuation, provider completeness and fetched/returned/emitted counts. Clients persist a secret-free key/intent before the first client-to-API mutation send, use at most three same-key sends in 30 seconds with 250ms/1s backoff, and never mint a replacement key after uncertainty, auto-rebase stale control revisions or auto-resubmit 410. A server `Retry-After` must fit the remaining client deadline; provider/run retry scheduling is independent.

The matrix is the canonical planned use-case surface, including accepted F01/F02/AUTH rows. Paths and syntax are not commands claimed available today. Browser and CLI invoke the same use case; a full UI remains outside this plan. Local server profiles store server selection and display/session handles only, never workspace/provider authority.

| ID / endpoint | CLI and browser action | Permission / contract | Phase / checks |
|---|---|---|---|
| CP01 GET /v1/info | pm server profiles check <profile>; UI: Check server compatibility | Public minimal instance/version/features only; none → ServerInfo | M3; V79 |
| CP02 POST /v1/sessions/device | pm sessions login --server-profile <profile>; UI: Login using redirect/cookie flow | Bounded unauthenticated device initiation; DeviceStart → DeviceChallenge | M3; V15, R07 |
| CP03 POST /v1/sessions/device/exchange | pm sessions login (approved challenge exchange); UI: Complete browser login | One-time approved device proof; DeviceProof → PMSession (client OS store only) | M3; V15, R07 |
| CP04 GET /v1/sessions/me | pm sessions inspect; UI: View current identity | Authenticated PM session; none → Actor and memberships | M3; R07 |
| CP05 DELETE /v1/sessions/current | pm sessions logout; UI: Logout | Current session; none → 204; accepted run grants unaffected | M3; R23 |
| CP06 POST /v1/sessions/browser | pm sessions login opens browser; UI: Sign in | OIDC callback/code/state/nonce+PKCE and exact origin; BrowserLogin → HttpOnly Secure SameSite session cookie | M3; R07 |
| CP07 GET /v1/workspaces | pm workspaces list; UI: Workspace picker | Membership in returned workspaces; cursor, page_size → WorkspacePage | M3; R03, R15 |
| CP08 GET /v1/workspaces/{workspace_id} | pm workspaces inspect <workspace>; UI: Workspace details | workspace.read; path workspace → Workspace | M3; R04 |
| CP09 GET /v1/workspaces/{workspace_id}/connectors | pm connectors list --workspace <workspace>; UI: Connector catalog | catalog.read; cursor, filters → ConnectorPage | M6; R14, R04 |
| CP10 GET /v1/workspaces/{workspace_id}/connectors/{connector_id} | pm connectors inspect <connector> --workspace <workspace>; UI: Connector capabilities | catalog.read; connector ID → Seven lane inventory and exact reasons | M6; V22, V66 |
| CP11 GET /v1/workspaces/{workspace_id}/commands | pm commands list --workspace <workspace> --connection <connection>; UI: Available actions | catalog.read and connection.read; connection ID, operation filter → SafeCommandPage | M6; V20, V23, R14 |
| CP12 GET /v1/workspaces/{workspace_id}/commands/{operation_id} | pm <connector> <resource> <action> --help --workspace <workspace>; UI: Action help/typed form | catalog.read; operation ID and connection → CommandDescriptor with schema/capability revision | M6; V23, V55 |
| CP-CONN-list GET /v1/workspaces/{workspace_id}/connections | pm connections list --workspace <workspace>; UI: List connection | connection.read; cursor → ConnectionPage | M3; R04, R14, V16 |
| CP-CONN-create POST /v1/workspaces/{workspace_id}/connections | pm connections create --workspace <workspace>; UI: Create connection | connection.manage; SafeConnectionInput → Connection | M3; R04, R14, V16 |
| CP-CONN-inspect GET /v1/workspaces/{workspace_id}/connections/{connection_id} | pm connections inspect --workspace <workspace>; UI: Inspect connection | connection.read; none → Connection | M3; R04, R14, V16 |
| CP-CONN-update PATCH /v1/workspaces/{workspace_id}/connections/{connection_id} | pm connections update --workspace <workspace>; UI: Update connection | connection.manage; SafeConnectionPatch+If-Match → ConnectionRevision | M3; R04, R14, V16 |
| CP-CONN-delete DELETE /v1/workspaces/{workspace_id}/connections/{connection_id} | pm connections delete --workspace <workspace>; UI: Delete connection | connection.manage; If-Match → Deletion/tombstone; active run policy explicit | M3; R04, R14, V16 |
| CP13 POST /v1/workspaces/{workspace_id}/connections/{connection_id}/discovery | pm schema discover --workspace <workspace> --connection <connection>; UI: Refresh account metadata | connection.discover; DiscoveryScope, expected prior revision → 202 RunRef | M10; V36, V73, R14 |
| CP14 GET /v1/workspaces/{workspace_id}/connections/{connection_id}/schema | pm schema inspect --workspace <workspace> --connection <connection>; UI: View effective schema | schema.read; resource ID, snapshot ID → EffectiveSchema and completeness/source revision | M7; V65, V73 |
| CP15 POST /v1/workspaces/{workspace_id}/invocations | pm <connector> <resource> <action> --workspace <workspace> --connection <connection> <typed provider flags>; UI: Submit typed action | operation.invoke plus operation-specific permission; typed Invocation may include optional shared output field selection/filter. An unapproved write returns a typed PlanRef awaiting approval; an approved write or admitted provider read returns 202 RunRef. Eventual ActionResult is complete by default or explicitly selected, with exact nested/presence/source membership/pagination. Exact replay returns the original plan/run with no new reservation/dispatch; changed input is 409. No provider I/O runs inline in API. | M6; V26, R09, R14, P_direct_read |
| CP16 POST /v1/workspaces/{workspace_id}/queries | pm query --workspace <workspace> --dataset <dataset> --expression-file <typed-query.json>; UI: Run bounded dataset query | dataset.query; TypedQuery over authorized immutable dataset snapshot → 202 RunRef and eventual ResultArtifact | M7; V37, R18 |
| CP-etl-plan POST /v1/workspaces/{workspace_id}/etl-plans | pm etl plan --workspace <workspace> --plan <plan>; UI: Plan etl | plan.manage; TypedPlanInput → PlanRevision or 202 planning RunRef | M8; V40, V42, R14 |
| CP-etl-inspect GET /v1/workspaces/{workspace_id}/etl-plans/{plan_id} | pm etl inspect --workspace <workspace> --plan <plan>; UI: Inspect etl | plan.read; none → SealedPlan | M8; V40, V42, R14 |
| CP-etl-preview POST /v1/workspaces/{workspace_id}/etl-plans/{plan_id}/preview | pm etl preview --workspace <workspace> --plan <plan>; UI: Preview etl | plan.manage; Expected plan revision → Preview with exact rows/actions/caps | M8; V40, V42, R14 |
| CP-etl-compatibility GET /v1/workspaces/{workspace_id}/etl-plans/{plan_id}/compatibility | pm etl compatibility --workspace <workspace> --plan <plan>; UI: Compatibility etl | plan.read; none → Typed requirement results and blockers | M8; V40, V42, R14 |
| CP-etl-approve POST /v1/workspaces/{workspace_id}/etl-plans/{plan_id}/approvals | pm etl approve --workspace <workspace> --plan <plan>; UI: Approve etl | plan.approve; Exact plan/actions/workset hash, expiry/caps → ApprovalRef | M8; V40, V42, R14 |
| CP-etl-execute POST /v1/workspaces/{workspace_id}/etl-plans/{plan_id}/runs | pm etl execute --workspace <workspace> --plan <plan>; UI: Execute etl | plan.execute; Exact approval/plan refs and scoped idempotency key → 202 RunRef | M8; V40, V42, R14 |
| CP-reverse-etl-plan POST /v1/workspaces/{workspace_id}/reverse-plans | pm reverse-etl plan --workspace <workspace> --plan <plan>; UI: Plan reverse-etl | plan.manage; TypedPlanInput → PlanRevision or202 planning RunRef | M8; V40, V42, R14 |
| CP-reverse-etl-inspect GET /v1/workspaces/{workspace_id}/reverse-plans/{plan_id} | pm reverse-etl inspect --workspace <workspace> --plan <plan>; UI: Inspect reverse-etl | plan.read; none → SealedPlan | M8; V40, V42, R14 |
| CP-reverse-etl-preview POST /v1/workspaces/{workspace_id}/reverse-plans/{plan_id}/preview | pm reverse-etl preview --workspace <workspace> --plan <plan>; UI: Preview reverse-etl | plan.manage; Expected plan revision → Preview with exact rows/actions/caps | M8; V40, V42, R14 |
| CP-reverse-etl-compatibility GET /v1/workspaces/{workspace_id}/reverse-plans/{plan_id}/compatibility | pm reverse-etl compatibility --workspace <workspace> --plan <plan>; UI: Compatibility reverse-etl | plan.read; none → Typed requirement results and blockers | M8; V40, V42, R14 |
| CP-reverse-etl-approve POST /v1/workspaces/{workspace_id}/reverse-plans/{plan_id}/approvals | pm reverse-etl approve --workspace <workspace> --plan <plan>; UI: Approve reverse-etl | plan.approve; Exact plan/actions/workset hash, expiry/caps → ApprovalRef | M8; V40, V42, R14 |
| CP-reverse-etl-execute POST /v1/workspaces/{workspace_id}/reverse-plans/{plan_id}/runs | pm reverse-etl execute --workspace <workspace> --plan <plan>; UI: Execute reverse-etl | plan.execute; Exact approval/plan refs and idempotency key → 202 RunRef | M8; V40, V42, R14 |
| CP-profiles-list GET /v1/workspaces/{workspace_id}/profiles | pm profiles list --workspace <workspace>; UI: List profiles | profiles.read; cursor → ProfilesPage | M8; R14, R04 |
| CP-profiles-create POST /v1/workspaces/{workspace_id}/profiles | pm profiles create --workspace <workspace>; UI: Create profiles | profiles.manage; Typed definition → ProfilesRevision | M8; R14, R04 |
| CP-profiles-inspect GET /v1/workspaces/{workspace_id}/profiles/{id} | pm profiles inspect --workspace <workspace>; UI: Inspect profiles | profiles.read; revision selector → ProfilesRevision | M8; R14, R04 |
| CP-profiles-update PATCH /v1/workspaces/{workspace_id}/profiles/{id} | pm profiles update --workspace <workspace>; UI: Update profiles | profiles.manage; If-Match + typed patch → New immutable revision | M8; R14, R04 |
| CP-profiles-delete DELETE /v1/workspaces/{workspace_id}/profiles/{id} | pm profiles delete --workspace <workspace>; UI: Delete profiles | profiles.manage; If-Match → Tombstone and dependent admission policy | M8; R14, R04 |
| CP-flows-list GET /v1/workspaces/{workspace_id}/flows | pm flows list --workspace <workspace>; UI: List flows | flows.read; cursor → FlowsPage | M9; R14, R04 |
| CP-flows-create POST /v1/workspaces/{workspace_id}/flows | pm flows create --workspace <workspace>; UI: Create flows | flows.manage; Typed definition → FlowsRevision | M9; R14, R04 |
| CP-flows-inspect GET /v1/workspaces/{workspace_id}/flows/{id} | pm flows inspect --workspace <workspace>; UI: Inspect flows | flows.read; revision selector → FlowsRevision | M9; R14, R04 |
| CP-flows-update PATCH /v1/workspaces/{workspace_id}/flows/{id} | pm flows update --workspace <workspace>; UI: Update flows | flows.manage; If-Match + typed patch → New immutable revision | M9; R14, R04 |
| CP-flows-delete DELETE /v1/workspaces/{workspace_id}/flows/{id} | pm flows delete --workspace <workspace>; UI: Delete flows | flows.manage; If-Match → Tombstone and dependent admission policy | M9; R14, R04 |
| CP-schedules-list GET /v1/workspaces/{workspace_id}/schedules | pm schedules list --workspace <workspace>; UI: List schedules | schedules.read; cursor → SchedulesPage | M9; R14, R04 |
| CP-schedules-create POST /v1/workspaces/{workspace_id}/schedules | pm schedules create --workspace <workspace>; UI: Create schedules | schedules.manage; Typed definition → SchedulesRevision | M9; R14, R04 |
| CP-schedules-inspect GET /v1/workspaces/{workspace_id}/schedules/{id} | pm schedules inspect --workspace <workspace>; UI: Inspect schedules | schedules.read; revision selector → SchedulesRevision | M9; R14, R04 |
| CP-schedules-update PATCH /v1/workspaces/{workspace_id}/schedules/{id} | pm schedules update --workspace <workspace>; UI: Update schedules | schedules.manage; If-Match + typed patch → New immutable revision | M9; R14, R04 |
| CP-schedules-delete DELETE /v1/workspaces/{workspace_id}/schedules/{id} | pm schedules delete --workspace <workspace>; UI: Delete schedules | schedules.manage; If-Match → Tombstone and dependent admission policy | M9; R14, R04 |
| CP-schedule-preview POST /v1/workspaces/{workspace_id}/schedules/{schedule_id}/preview | pm schedules preview <schedule> --workspace <workspace>; UI: Preview schedule | schedules.manage; Revision; bounded time horizon for preview; approved run policy for trigger → Preview UTC instants or desired/applied revision or202 RunRef | M9; V45, R19, R20 |
| CP-schedule-trigger POST /v1/workspaces/{workspace_id}/schedules/{schedule_id}/runs | pm schedules trigger <schedule> --workspace <workspace>; UI: Trigger schedule | schedules.manage; Revision; bounded time horizon for preview; approved run policy for trigger → Preview UTC instants or desired/applied revision or202 RunRef | M9; V45, R19, R20 |
| CP-schedule-pause POST /v1/workspaces/{workspace_id}/schedules/{schedule_id}/pause | pm schedules pause <schedule> --workspace <workspace>; UI: Pause schedule | schedules.manage; Revision; bounded time horizon for preview; approved run policy for trigger → Preview UTC instants or desired/applied revision or202 RunRef | M9; V45, R19, R20 |
| CP-schedule-resume POST /v1/workspaces/{workspace_id}/schedules/{schedule_id}/resume | pm schedules resume <schedule> --workspace <workspace>; UI: Resume schedule | schedules.manage; Revision; bounded time horizon for preview; approved run policy for trigger → Preview UTC instants or desired/applied revision or202 RunRef | M9; V45, R19, R20 |
| CP-schedule-status GET /v1/workspaces/{workspace_id}/schedules/{schedule_id}/status | pm schedules status <schedule> --workspace <workspace>; UI: Status schedule | schedules.read; Revision; bounded time horizon for preview; approved run policy for trigger → Preview UTC instants or desired/applied revision or202 RunRef | M9; V45, R19, R20 |
| CP-flow-trigger POST /v1/workspaces/{workspace_id}/flows/{flow_id}/runs | pm flows run <flow> --workspace <workspace>; UI: Run flow | flow.execute; Immutable flow revision and approvals → 202 RunRef | M9; V44, R14 |
| CP-runs-list GET /v1/workspaces/{workspace_id}/runs | pm runs list <run> --workspace <workspace>; UI: List run | run.read; cursor and filters → RunPage | M4; R09, R10, V30, R14 |
| CP-runs-inspect GET /v1/workspaces/{workspace_id}/runs/{run_id} | pm runs inspect <run> --workspace <workspace>; UI: Inspect run | run.read; none → Run with effect state and orchestration observation | M4; R09, R10, V30, R14 |
| CP-runs-cancel POST /v1/workspaces/{workspace_id}/runs/{run_id}/cancel | pm runs cancel <run> --workspace <workspace>; UI: Cancel run | run.control; Expected run/control revision → Durable cancel command | M4; R09, R10, V30, R14 |
| CP-runs-resume POST /v1/workspaces/{workspace_id}/runs/{run_id}/resume | pm runs resume <run> --workspace <workspace>; UI: Resume run | run.control; Explicit recovery generation/approval → RunRef, exact unresolved effects retained | M4; R09, R10, V30, R14 |
| CP-runs-result GET /v1/workspaces/{workspace_id}/runs/{run_id}/result | pm runs result <run> --workspace <workspace>; UI: Result run | run.read; none → Result refs with truthful state | M4; R09, R10, V30, R14 |
| CP-runs-tasks GET /v1/workspaces/{workspace_id}/runs/{run_id}/tasks | pm runs tasks <run> --workspace <workspace>; UI: Tasks run | run.read; cursor → TaskPage and per-member outcomes | M4; R09, R10, V30, R14 |
| CP-runs-watch GET /v1/workspaces/{workspace_id}/runs/{run_id}/events | pm runs watch <run> --workspace <workspace>; UI: Watch run | run.read; Last-Event-ID/cursor → SSE with durable IDs, heartbeat and explicit gap | M4; R09, R10, V30, R14 |
| CP-task GET /v1/workspaces/{workspace_id}/tasks/{task_id} | pm tasks inspect <task> --workspace <workspace>; UI: Task details | run.read; task ID → Task and exact member dispositions | M4; R14, R04 |
| CP-artifact-upload POST /v1/workspaces/{workspace_id}/artifacts | pm artifacts upload --workspace <workspace>; UI: Upload artifact | artifact.write; Typed metadata + bounded byte stream → ArtifactRef | M7; R16, R17, V31 |
| CP-artifact-inspect GET /v1/workspaces/{workspace_id}/artifacts/{artifact_id} | pm artifacts inspect --workspace <workspace>; UI: Inspect artifact | artifact.read; none → Authorized digest/length/media | M7; R16, R17, V31 |
| CP-artifact-download GET /v1/workspaces/{workspace_id}/artifacts/{artifact_id}/content | pm artifacts download --workspace <workspace>; UI: Download artifact | artifact.read; Range → Bounded bytes and digest metadata | M7; R16, R17, V31 |
| CP-revoke POST /v1/workspaces/{workspace_id}/runs/{run_id}/grant/revoke | pm runs revoke-grant <run> --workspace <workspace>; UI: Revoke future effect permission | run.revoke; Expected grant revision and reason → Revoked grant revision; no rollback claim | M9; R23 |
| CP-admin-secret POST /admin/v1/workspaces/{workspace_id}/connections/{connection_id}/credentials | No ordinary CLI command; use authenticated server admin page; UI: Enter/rotate provider credential in isolated admin UI | connection.credentials.manage plus step-up authentication; Secret ingress only; sensitive fields excluded from public schema → Opaque version/status only, no readback | M3; V16, V17, R07 |
| CP-memberships-list GET /v1/workspaces/{workspace_id}/memberships | pm access memberships list --workspace <workspace>; UI: List memberships | workspace.access.read; scoped cursor → safe membership metadata plus current collection/resource revision; cursor/ETag is read-only, no one-time secret | M3; R04, R08, R14 |
| CP-memberships-grant POST /v1/workspaces/{workspace_id}/memberships | pm access memberships grant --workspace <workspace>; UI: Grant membership | workspace.access.manage; typed grant + collection `If-Match` + workspace/principal/operation/target key → original 201 safe identity/revision; exact authorized replay 200 with explicit prior safe result; changed intent 409 | M3; R04, R08, R14 |
| CP-memberships-revoke POST /v1/workspaces/{workspace_id}/memberships/{id}/revoke | pm access memberships revoke --workspace <workspace>; UI: Revoke membership | workspace.access.manage; typed revoke + target `If-Match` + scoped key → revoke/already-revoked 200; exact authorized replay 200 with explicit original safe result; no second effect | M3; R04, R08, R14 |
| CP-service-accounts-list GET /v1/workspaces/{workspace_id}/service-accounts | pm access service-accounts list --workspace <workspace>; UI: List service accounts | workspace.access.read; scoped cursor → safe identity/grant metadata plus current revision; no automation credential material | M3; R04, R08, R14 |
| CP-service-accounts-create POST /v1/workspaces/{workspace_id}/service-accounts | pm access service-accounts create --workspace <workspace>; UI: Create service account | workspace.access.manage; typed create + collection `If-Match` + scoped key → original 201 safe identity/revision and one-time PM automation credential; replay 200 omits credential and returns explicit original safe result; loss requires revoke/new create | M3; R04, R08, R14 |
| CP-service-accounts-revoke POST /v1/workspaces/{workspace_id}/service-accounts/{id}/revoke | pm access service-accounts revoke --workspace <workspace>; UI: Revoke service account | workspace.access.manage; typed revoke + target `If-Match` + scoped key → revoke/already-revoked 200; exact authorized replay 200 with original safe result; no credential material | M3; R04, R08, R14 |
| CP-delegations-list GET /v1/workspaces/{workspace_id}/delegations | pm access delegations list --workspace <workspace>; UI: List delegations | workspace.access.read; scoped cursor → safe delegation metadata plus current revision; read cursor is not mutation idempotency | M3; R04, R08, R14 |
| CP-delegations-create POST /v1/workspaces/{workspace_id}/delegations | pm access delegations create --workspace <workspace>; UI: Create delegation | workspace.access.manage; typed create + collection `If-Match` + scoped key → original 201 safe identity/revision; exact authorized replay 200 with explicit original safe result; changed intent 409 | M3; R04, R08, R14 |
| CP-delegations-revoke POST /v1/workspaces/{workspace_id}/delegations/{id}/revoke | pm access delegations revoke --workspace <workspace>; UI: Revoke delegation | workspace.access.manage; typed revoke + target `If-Match` + scoped key → revoke/already-revoked 200; exact authorized replay 200 with original safe result and no second effect | M3; R04, R08, R14 |

**Worked GitHub slices.** The accepted M6 read is pinned `issues/list-for-repo`: with no controls it returns complete mixed issue/pull-request records, and explicit shared selection/filtering preserves exact values and truthful continuation. Independent repository, numeric `id` and repository-scoped `number` remain oracle inputs, not a six-field output contract. The JSON below is a separate synthetic later-write illustration from the historical moving `WEB-GITHUB` page; it neither supplies the accepted F03 read source nor activates create in M6. Any later create owner must pin its own exact source/eligibility. Closing an issue is an update, not hard delete.

```json
{
  "synthetic": true,
  "connector_id": "github",
  "source": {
    "id": "WEB-GITHUB", "url": "https://docs.github.com/en/rest/issues/issues",
    "capture_sha256": "76453eb5d99f8596bf72b440f3a09b008b63a6a23e275830f65e536d769e1256",
    "operation_pointer": "create-an-issue", "schema_ref": "captured-full-create-issue-schema"
  },
  "operation": {
    "id": "github.issues.create", "method": "POST",
    "path": "/repos/{owner}/{repo}/issues", "request_media": "application/json",
    "case_fields": [
      {"id":"path.owner","type":"string","required":true,"wire":"owner"},
      {"id":"path.repo","type":"string","required":true,"wire":"repo"},
      {"id":"body.title","type":"string","required":true,"wire":"title"},
      {"id":"body.body","type":"string","required":false,"wire":"body"}
    ],
    "pm_controls": ["workspace","connection","approval","idempotency-key"],
    "command": ["github","issues","create"],
    "requires": ["FND-03@1.0.0","FND-06@1.0.0","FND-14@2.0.0","FND-17@2.0.0"]
  },
  "scenario": {
    "id":"issue-create-readback", "lane":"direct_write",
    "prerequisites":{"repository":"explicit disposable authorized repository"},
    "steps":[
      {"id":"plan","action":"create sealed plan","input":{"title":"PM proof demo","body":"Owned proof resource"}},
      {"id":"approve","action":"approve exact plan/action hash"},
      {"id":"create","operation":"github.issues.create","capture":{"issue_number":"response.number"}},
      {"id":"readback","operation":"github.issues.get","bind":{"issue_number":"steps.create.issue_number"}},
      {"id":"cleanup","operation":"github.issues.update","bind":{"issue_number":"steps.create.issue_number"},"input":{"state":"closed"}}
    ],
    "assertions":["create status201","returned number present and exact","readback title/body equal independent input","cleanup state closed"]
  }
}
```

A proposed invocation is `pm github issues create --workspace ws_demo --connection conn_demo --owner example --repo disposable --title 'PM proof demo' --body 'Owned proof resource'`. Without a sealed approval it returns a plan reference. After explicit approval, the same immutable plan is admitted with an idempotency key and the client can wait for its run. The fake/live scenario runner captures the actual returned number, supplies it to readback and cleanup, and records every sanitized command/request/response. Cleanup uses only the resource it created. It cannot infer that an existing title belongs to this run.

```json
{
  "synthetic": true, "certifies": false, "status": "unproven",
  "connector_id":"github", "lane":"direct_write",
  "operation_id":"github.issues.create", "behavior_id":"create_issue_with_readback",
  "case_id":"issue-create-readback", "run_id":null,
  "pm_client_hash":null, "pm_api_hash":null, "pm_worker_hash":null,
  "source_hashes":["76453eb5d99f8596bf72b440f3a09b008b63a6a23e275830f65e536d769e1256"],
  "command":["pm","github","issues","create","--workspace","ws_demo","--connection","conn_demo","--owner","example","--repo","disposable","--title","PM proof demo","--body","Owned proof resource"],
  "request":null, "response":null,
  "assertions":[{"expected":"readback title equals PM proof demo","observed":null,"passed":null}],
  "effect":"not observed", "started_at":null, "finished_at":null,
  "provenance":{"kind":"planning example","reuse":null}
}
```

This sketch is intentionally unproven: no new binary or response exists. A real emitted proof replaces the null observations with actual sanitized evidence and derives pass/fail from the independent assertions. It includes full scope, operation/schema/handler/assertion hashes and all executing component hashes. One successful create does not certify list pagination, attachments or every write on the connector.

**The other four worked mappings retain their source-specific meaning:**

| Connector/fact | Typed mapping and projection | Foundation/state/validation consequence |
|---|---|---|
| PostgreSQL `numeric`, `timestamptz`, `timestamp without time zone`, schema-qualified tables | Numeric coefficient/scale remain exact; instant and local timestamp are different types. Identifiers come from admitted catalog; values are parameters. `pm postgres ... --workspace ... --connection ...` uses saved native connection, never infrastructure DSN. | FND-03/07/09/13; scoped PG metadata snapshot and immutable output manifest; V34/V35 and exact round-trip rows. Native SQL TLS cannot downgrade. |
| Gong `POST /v2/calls/extensive` and nested `filter` | POST is a read here. Date filters bind to the documented nested body. `calls[].metaData.id` is a string, including large numeric-looking IDs. PM record cap is not an invented provider page-size field. | FND-03/06/08/12; unread page tail retained behind server continuation; V67 asserts no invented wire `limit` and no dropped remainder. |
| Pylon custom-field request/response shapes | Response field map and request field array are different encodings; metadata slug/options drive typed mapping. `parent_account_id` documented-null versus string-schema disagreement stays an affected-binding conflict. | FND-02/03/09/14; account-scoped complete metadata and explicit source amendment/fidelity gate; V65/V73. No fabricated nullable union. |
| HubSpot property metadata | `type` is logical data type; `fieldType` is display metadata. Computed/read-only fields are not writable. Unique value does not imply required/non-null/upsert. `--property internal_name=value` or typed property JSON maps dynamic fields. | FND-03/09/14/15; immutable effective schema and accepted mapping/workset; drift invalidates only actual field dependencies; V36/V65/V73. |

Primary documentation: [PostgreSQL columns](https://www.postgresql.org/docs/18/infoschema-columns.html), [Gong extensive read](https://help.gong.io/apidocs/retrieve-detailed-call-data-by-various-filters-v2callsextensive-2), [Pylon accounts](https://docs.usepylon.com/pylon-docs/developer/api/api-reference/accounts.md), [HubSpot properties](https://developers.hubspot.com/docs/api-reference/legacy/crm/properties/guide). PostgreSQL provider schema references and infrastructure engine qualification are distinct. These facts are pinned in `WEB-GITHUB`, `WEB-PG`, `WEB-GONG`, `WEB-PYLON` and `WEB-HUBSPOT` source records, with the source-specific pointers/hashes in the mapping ledger. Their account authorization, write outcomes and complete operation coverage remain unproven until the relevant connector scripts execute.

For reverse ETL, save a typed mapping from an immutable warehouse snapshot to destination fields, preview exact row cardinality and the complete physical action set, then approve that snapshot/hash/caps before execution. Two source rows must yield exactly two intended outcomes using returned IDs and independent field readbacks; correct counts with wrong values fail. Persist logical create mappings across nonidentity profile revisions. New action occurrences differ from retries. The four planned composition paths are GitHub→warehouse→PostgreSQL, PostgreSQL→warehouse→GitHub, GitHub→warehouse→GitHub and PostgreSQL→warehouse→PostgreSQL; none inherits old proof.


## 11. Complete proposed file manifest and ownership

**Planning ownership projection, not an execution receipt.** CREATE identifies first implementation ownership after authorization; an existing file receives a bounded update rather than blind overwrite. GENERATE means disposable output of its named generator. DEFER remains outside initial release. The tree includes source-present foundation/M3 candidate files and unimplemented later work; presence does not establish passing behavior, review, CI or release completion. The old repository remains frozen reference.

`FILE-OWNERSHIP.json` is the exact file-level companion: path, preserved group ID, assignment owner, actual Go package, process location, purpose, interface subset, first milestone, allowed dependency direction, later wiring milestones and validation IDs. Group responsibility allocates work; it does not mean every file implements every interface in its group. The tree below lists every proposed path once, including generated and deferred files.

```text
pm-cli/
├── .github/
│   └── workflows/
│       ├── check.yml [M0 CREATE; F01]
│       └── release.yml [M12 CREATE; F40]
├── AGENTS.md [M0 CREATE; F02]
├── LICENSE [M12 CREATE; R15]
├── Makefile [M0 CREATE; F01]
├── THIRD-PARTY-NOTICES.md [M0 CREATE; F01]
├── api/
│   ├── admin.openapi.yaml [M3 CREATE; F12]
│   ├── controls.json [M1 CREATE; F04]
│   ├── generated.sha256 [M1 GENERATE; F07]
│   ├── oapi-codegen.yaml [M1 CREATE; F04]
│   ├── pm.openapi.json [M1 GENERATE; F07]
│   └── pm.openapi.yaml [M1 CREATE; F04]
├── build/
│   └── toolchains.lock.json [M0 CREATE; F01]
├── cmd/
│   ├── pm-query/
│   │   └── main.go [M7 CREATE; R12]
│   ├── pm-server/
│   │   ├── BUILD.bazel [M3 CREATE; F11]
│   │   └── main.go [M3 CREATE; F11]
│   ├── pm-worker/
│   │   └── main.go [M4 CREATE; R07]
│   ├── pm/
│   │   └── main.go [M5 CREATE; F15]
│   ├── pmgen/
│   │   └── main.go [M6 CREATE; F16]
│   └── pmproof/
│       └── main.go [M6 CREATE; F25]
├── codegraph.json [M0 CREATE; F02]
├── connectors/
│   ├── github/
│   │   ├── NOTICE [M6 CREATE; C_github]
│   │   ├── lanes/
│   │   │   ├── binary_download.json [M6 GENERATE; B_github]
│   │   │   ├── binary_upload.json [M6 GENERATE; B_github]
│   │   │   ├── direct_read.json [M6 GENERATE; B_github]
│   │   │   ├── direct_write.json [M6 GENERATE; B_github]
│   │   │   ├── etl.json [M6 GENERATE; B_github]
│   │   │   ├── reverse_etl.json [M6 GENERATE; B_github]
│   │   │   └── sync_transport.json [M6 GENERATE; B_github]
│   │   ├── scenarios/
│   │   │   ├── composition.json [M11 CREATE; N13]
│   │   │   └── lifecycle.json [M6 CREATE; C_github]
│   │   ├── scripts/
│   │   │   ├── binary_download.sh [M6 GENERATE; S_github]
│   │   │   ├── binary_upload.sh [M6 GENERATE; S_github]
│   │   │   ├── direct_read.sh [M6 GENERATE; S_github]
│   │   │   ├── direct_write.sh [M6 GENERATE; S_github]
│   │   │   ├── etl.sh [M6 GENERATE; S_github]
│   │   │   ├── reverse_etl.sh [M6 GENERATE; S_github]
│   │   │   └── sync_transport.sh [M6 GENERATE; S_github]
│   │   ├── semantics.json [M6 CREATE; C_github]
│   │   └── source.lock.json [M6 CREATE; C_github]
│   ├── gong/
│   │   ├── NOTICE [M11 CREATE; C_gong]
│   │   ├── lanes/
│   │   │   ├── binary_download.json [M11 GENERATE; B_gong]
│   │   │   ├── binary_upload.json [M11 GENERATE; B_gong]
│   │   │   ├── direct_read.json [M11 GENERATE; B_gong]
│   │   │   ├── direct_write.json [M11 GENERATE; B_gong]
│   │   │   ├── etl.json [M11 GENERATE; B_gong]
│   │   │   ├── reverse_etl.json [M11 GENERATE; B_gong]
│   │   │   └── sync_transport.json [M11 GENERATE; B_gong]
│   │   ├── scenarios/
│   │   │   └── lifecycle.json [M11 CREATE; C_gong]
│   │   ├── scripts/
│   │   │   ├── binary_download.sh [M11 GENERATE; S_gong]
│   │   │   ├── binary_upload.sh [M11 GENERATE; S_gong]
│   │   │   ├── direct_read.sh [M11 GENERATE; S_gong]
│   │   │   ├── direct_write.sh [M11 GENERATE; S_gong]
│   │   │   ├── etl.sh [M11 GENERATE; S_gong]
│   │   │   ├── reverse_etl.sh [M11 GENERATE; S_gong]
│   │   │   └── sync_transport.sh [M11 GENERATE; S_gong]
│   │   ├── semantics.json [M11 CREATE; C_gong]
│   │   └── source.lock.json [M11 CREATE; C_gong]
│   ├── hubspot/
│   │   ├── NOTICE [M11 CREATE; C_hubspot]
│   │   ├── lanes/
│   │   │   ├── binary_download.json [M11 GENERATE; B_hubspot]
│   │   │   ├── binary_upload.json [M11 GENERATE; B_hubspot]
│   │   │   ├── direct_read.json [M11 GENERATE; B_hubspot]
│   │   │   ├── direct_write.json [M11 GENERATE; B_hubspot]
│   │   │   ├── etl.json [M11 GENERATE; B_hubspot]
│   │   │   ├── reverse_etl.json [M11 GENERATE; B_hubspot]
│   │   │   └── sync_transport.json [M11 GENERATE; B_hubspot]
│   │   ├── scenarios/
│   │   │   └── lifecycle.json [M11 CREATE; C_hubspot]
│   │   ├── scripts/
│   │   │   ├── binary_download.sh [M11 GENERATE; S_hubspot]
│   │   │   ├── binary_upload.sh [M11 GENERATE; S_hubspot]
│   │   │   ├── direct_read.sh [M11 GENERATE; S_hubspot]
│   │   │   ├── direct_write.sh [M11 GENERATE; S_hubspot]
│   │   │   ├── etl.sh [M11 GENERATE; S_hubspot]
│   │   │   ├── reverse_etl.sh [M11 GENERATE; S_hubspot]
│   │   │   └── sync_transport.sh [M11 GENERATE; S_hubspot]
│   │   ├── semantics.json [M11 CREATE; C_hubspot]
│   │   └── source.lock.json [M11 CREATE; C_hubspot]
│   ├── postgres/
│   │   ├── NOTICE [M7 CREATE; C_postgres]
│   │   ├── lanes/
│   │   │   ├── binary_download.json [M7 GENERATE; B_postgres]
│   │   │   ├── binary_upload.json [M7 GENERATE; B_postgres]
│   │   │   ├── direct_read.json [M7 GENERATE; B_postgres]
│   │   │   ├── direct_write.json [M7 GENERATE; B_postgres]
│   │   │   ├── etl.json [M7 GENERATE; B_postgres]
│   │   │   ├── reverse_etl.json [M7 GENERATE; B_postgres]
│   │   │   └── sync_transport.json [M7 GENERATE; B_postgres]
│   │   ├── scenarios/
│   │   │   ├── composition.json [post-M8 CREATE for N13-PG after #28–#34; M11 UPDATE for remaining N13; issue #41 first owner]
│   │   │   └── lifecycle.json [M7 CREATE by C_postgres/#28; M8 serialized UPDATE by #34 for mapped upsert]
│   │   ├── scripts/
│   │   │   ├── binary_download.sh [M7 GENERATE; S_postgres]
│   │   │   ├── binary_upload.sh [M7 GENERATE; S_postgres]
│   │   │   ├── direct_read.sh [M7 GENERATE; S_postgres]
│   │   │   ├── direct_write.sh [M7 GENERATE; S_postgres]
│   │   │   ├── etl.sh [M7 GENERATE; S_postgres]
│   │   │   ├── reverse_etl.sh [M7 GENERATE; S_postgres]
│   │   │   └── sync_transport.sh [M7 GENERATE; S_postgres]
│   │   ├── semantics.json [M7 CREATE by C_postgres/#28; M8 serialized UPDATE by #34 for mapped upsert]
│   │   └── source.lock.json [M7 CREATE; C_postgres]
│   └── pylon/
│       ├── NOTICE [M11 CREATE; C_pylon]
│       ├── lanes/
│       │   ├── binary_download.json [M11 GENERATE; B_pylon]
│       │   ├── binary_upload.json [M11 GENERATE; B_pylon]
│       │   ├── direct_read.json [M11 GENERATE; B_pylon]
│       │   ├── direct_write.json [M11 GENERATE; B_pylon]
│       │   ├── etl.json [M11 GENERATE; B_pylon]
│       │   ├── reverse_etl.json [M11 GENERATE; B_pylon]
│       │   └── sync_transport.json [M11 GENERATE; B_pylon]
│       ├── scenarios/
│       │   └── lifecycle.json [M11 CREATE; C_pylon]
│       ├── scripts/
│       │   ├── binary_download.sh [M11 GENERATE; S_pylon]
│       │   ├── binary_upload.sh [M11 GENERATE; S_pylon]
│       │   ├── direct_read.sh [M11 GENERATE; S_pylon]
│       │   ├── direct_write.sh [M11 GENERATE; S_pylon]
│       │   ├── etl.sh [M11 GENERATE; S_pylon]
│       │   ├── reverse_etl.sh [M11 GENERATE; S_pylon]
│       │   └── sync_transport.sh [M11 GENERATE; S_pylon]
│       ├── semantics.json [M11 CREATE; C_pylon]
│       └── source.lock.json [M11 CREATE; C_pylon]
├── deploy/
│   ├── backup.schema.json [M12 CREATE; R15]
│   ├── caddy/
│   │   └── Caddyfile [M0 CREATE; R02]
│   ├── compose.managed.yaml [M0 CREATE; R02]
│   ├── compose.yaml [M0 CREATE; R02]
│   ├── images.lock.json [M0 CREATE; R02]
│   ├── postgres/
│   │   └── bootstrap.sql [M0 CREATE; R02]
│   ├── seaweed/
│   │   ├── filer.toml.tmpl [M0 CREATE; R02]
│   │   ├── s3.json.tmpl [M0 CREATE; R02]
│   │   └── security.toml.tmpl [M0 CREATE; R02]
│   ├── server.example.json [M0 CREATE; R01]
│   └── temporal/
│       ├── config.yaml.tmpl [M0 CREATE; R02]
│       ├── dynamicconfig.yaml [M0 CREATE; R02]
│       └── schema-init.sh [M0 CREATE; R02]
├── docs/
│   ├── architecture/
│   │   ├── decisions.md [M0 CREATE; F03]
│   │   ├── overview.md [M0 CREATE; F03]
│   │   └── scale-triggers.md [M13 CREATE; F41]
│   ├── cli/
│   │   ├── commands.json [M11 GENERATE; F42]
│   │   └── manual.md [M11 GENERATE; F42]
│   ├── decisions/
│   │   └── 0001-cli-configuration.md [M0 CREATE; F02]
│   ├── development/
│   │   ├── release.md [M0 CREATE; F03]
│   │   ├── setup.md [M0 CREATE; F02]
│   │   └── testing.md [M0 CREATE; F03]
│   ├── foundations/
│   │   ├── README.md [M6 CREATE; F19]
│   │   ├── atlas.json [M6 CREATE; F19]
│   │   ├── requirements.json [M6 CREATE; N11]
│   │   └── traceability.schema.json [M6 CREATE; N11]
│   └── server/
│       ├── artifacts.md [M12 CREATE; R15]
│       ├── backup.md [M12 CREATE; F40]
│       ├── install.md [M12 CREATE; N12]
│       ├── operations.md [M12 CREATE; R15]
│       ├── pairing.md [M12 CREATE; N10]
│       ├── remote.md [M13 CREATE; F41]
│       ├── restore.md [M12 CREATE; R15]
│       ├── security.md [M12 CREATE; F40]
│       ├── standalone.md [M12 CREATE; N10]
│       ├── temporal.md [M12 CREATE; R15]
│       ├── tenancy.md [M12 CREATE; R15]
│       ├── uninstall.md [M12 CREATE; N10]
│       └── upgrade.md [M12 CREATE; N10]
├── go.mod [M0 CREATE; F01]
├── go.sum [M0 CREATE; F01]
├── internal/
│   ├── activities/
│   │   ├── admission.go [M4 CREATE; R07]
│   │   ├── effect_crash_test.go [M4 CREATE; T_R13]
│   │   ├── execute.go [M4 CREATE; R07]
│   │   ├── options.go [M4 CREATE; R07]
│   │   ├── project.go [M4 CREATE; R07]
│   │   ├── reconcile.go [M4 CREATE; R07]
│   │   └── register.go [M4 CREATE; R07]
│   ├── analytics/
│   │   ├── engine.go [M7 CREATE; R18]
│   │   ├── limits.go [M7 CREATE; R18]
│   │   ├── parquet.go [M7 CREATE; R18]
│   │   └── values.go [M7 CREATE; R18]
│   ├── api/
│   │   ├── catalog.go [M6 CREATE; F18a]
│   │   ├── client.gen.go [M1 GENERATE; F07]
│   │   ├── client.go [M5 CREATE; F14]
│   │   ├── controls.gen.go [M1 GENERATE; F07]
│   │   ├── events.go [M5 CREATE; F22]
│   │   ├── events_test.go [M4 CREATE; T_V30]
│   │   ├── output.go [M5 CREATE; F14]
│   │   ├── profile.go [M5 CREATE; F14]
│   │   ├── protocol_test.go [M3 CREATE; T_V05]
│   │   ├── session.go [M5 CREATE; F14]
│   │   ├── session_test.go [M5 CREATE; T_V18]
│   │   ├── types.gen.go [M1 GENERATE; F07]
│   │   └── version_test.go [M3 CREATE; T_V79]
│   ├── app/
│   │   ├── BUILD.bazel [M3 CREATE; F11]
│   │   ├── account_permits.go [M4 CREATE; R14a]
│   │   ├── action_approval_test.go [M8 CREATE; T_V71]
│   │   ├── admission.go [M4 CREATE; F21]
│   │   ├── admission_limits.go [M9 CREATE; F34]
│   │   ├── admission_limits_test.go [M9 CREATE; T_V46]
│   │   ├── approvals.go [M8 CREATE; F31]
│   │   ├── cancel.go [M4 CREATE; F21]
│   │   ├── cancel_test.go [M9 CREATE; T_V47]
│   │   ├── capabilities.go [M4 CREATE; N02]
│   │   ├── capabilities_test.go [M4 CREATE; T_V66]
│   │   ├── catalog.go [M6 CREATE; F18c]
│   │   ├── connections.go [M3 CREATE; F12]
│   │   ├── credentials.go [M3 CREATE; N08]
│   │   ├── delegations.go [M3 CREATE; R20]
│   │   ├── flows.go [M9 CREATE; F34]
│   │   ├── flows_test.go [M9 CREATE; T_V44]
│   │   ├── lifecycle_test.go [M4 CREATE; T_V12]
│   │   ├── reconcile.go [M8 CREATE; F31]
│   │   ├── reconcile_test.go [M8 CREATE; T_V43]
│   │   ├── resume.go [M8 CREATE; F31]
│   │   ├── revocation.go [M4 CREATE; R14b]
│   │   ├── revocation_test.go [M9 CREATE; T_R23]
│   │   ├── runs.go [M4 CREATE; F21]
│   │   ├── runtime.go [M3 CREATE; F11]
│   │   ├── schedules.go [M9 CREATE; R14]
│   │   ├── service_accounts.go [M3 CREATE; R20]
│   │   └── workspaces.go [M3 CREATE; R20]
│   ├── auth/
│   │   ├── BUILD.bazel [M3 CREATE; F12]
│   │   ├── authorize.go [M3 CREATE; F12]
│   │   ├── authorize_test.go [M3 CREATE; T_V14]
│   │   ├── browser.go [M3 CREATE; R05]
│   │   ├── browser_test.go [M3 CREATE; T_R07]
│   │   ├── credential_ingress_test.go [M3 CREATE; T_V16]
│   │   ├── device.go [M3 CREATE; R05]
│   │   ├── discovery.go [M3 CREATE; R05]
│   │   ├── grants.go [M3 CREATE; R05]
│   │   ├── identity.go [M3 CREATE; R05]
│   │   ├── oidc.go [M3 CREATE; R05]
│   │   ├── receipt_test.go [M3 CREATE; T_R08]
│   │   ├── revocation.go [M3 CREATE; R05]
│   │   ├── service.go [M3 CREATE; R05]
│   │   ├── service_test.go [M3 CREATE; T_R08]
│   │   ├── sessions.go [M3 CREATE; F12]
│   │   ├── sessions_test.go [M3 CREATE; T_V15]
│   │   └── test_helpers_test.go [M3 CREATE; T_V14]
│   ├── cli/
│   │   ├── access.go [M5 CREATE; R21]
│   │   ├── artifacts.go [M5 CREATE; R10]
│   │   ├── boundary_test.go [M5 CREATE; T_V53]
│   │   ├── config.go [M5 CREATE; F15]
│   │   ├── config_test.go [M5 CREATE; T_V19]
│   │   ├── connections.go [M5 CREATE; R10]
│   │   ├── dynamic.go [M6 CREATE; F18b]
│   │   ├── dynamic_test.go [M6 CREATE; T_V20]
│   │   ├── flows.go [M9 CREATE; F36b]
│   │   ├── help.go [M6 CREATE; F18b]
│   │   ├── plans.go [M8 CREATE; F33b]
│   │   ├── profiles.go [M8 CREATE; F33b]
│   │   ├── render.go [M5 CREATE; F15]
│   │   ├── root.go [M5 CREATE; F15]
│   │   ├── runs.go [M5 CREATE; F22b]
│   │   ├── schedules.go [M9 CREATE; F36b]
│   │   ├── schema.go [M7 CREATE; F29b]
│   │   ├── selectors.go [M5 CREATE; F15]
│   │   ├── sessions.go [M5 CREATE; R10]
│   │   ├── tasks.go [M5 CREATE; R10]
│   │   ├── tree_test.go [M6 CREATE; T_V23]
│   │   ├── warehouse.go [M7 CREATE; F29b]
│   │   ├── workspaces.go [M5 CREATE; R10]
│   │   └── workspaces_test.go [M5 CREATE; T_R15]
│   ├── compile/
│   │   ├── amendment.go [M6 CREATE; N03]
│   │   ├── amendment_test.go [M6 CREATE; T_V84]
│   │   ├── bundle_test.go [M6 CREATE; T_V22]
│   │   ├── commands.go [M6 CREATE; F16]
│   │   ├── compatibility.go [M6 CREATE; F16]
│   │   ├── dialect_test.go [M6 CREATE; T_V21]
│   │   ├── dialects.go [M6 CREATE; F16]
│   │   ├── emit.go [M6 CREATE; F16]
│   │   ├── fidelity.go [M6 CREATE; N03]
│   │   ├── fidelity_test.go [M6 CREATE; T_V65]
│   │   ├── normalize.go [M6 CREATE; F16]
│   │   ├── refs.go [M6 CREATE; F16]
│   │   ├── requirements.go [M6 CREATE; N03]
│   │   ├── semantics.go [M6 CREATE; F16]
│   │   ├── sensitivity.go [M6 CREATE; F16]
│   │   ├── sensitivity_test.go [M6 CREATE; T_V55]
│   │   └── source.go [M6 CREATE; F16]
│   ├── config/
│   │   ├── config.go [M0 CREATE; R01]
│   │   ├── server.go [M3 CREATE; F11]
│   │   ├── server_test.go [M3 CREATE; T_V13]
│   │   └── validate.go [M0 CREATE; R01]
│   ├── connectors/
│   │   ├── bundle.go [M6 CREATE; F17]
│   │   ├── catalog.go [M6 CREATE; F17]
│   │   ├── data/
│   │   │   ├── github.bundle.json [M6 GENERATE; B_github]
│   │   │   ├── gong.bundle.json [M11 GENERATE; B_gong]
│   │   │   ├── hubspot.bundle.json [M11 GENERATE; B_hubspot]
│   │   │   ├── postgres.bundle.json [M7 GENERATE; B_postgres]
│   │   │   └── pylon.bundle.json [M11 GENERATE; B_pylon]
│   │   ├── registry.go [M6 CREATE; F17]
│   │   └── revision_test.go [M6 CREATE; T_V24]
│   ├── contract/
│   │   ├── approval.go [M1 CREATE; F06]
│   │   ├── capability.go [M1 CREATE; F05]
│   │   ├── command.go [M1 CREATE; F05]
│   │   ├── compatibility.go [M1 CREATE; F05]
│   │   ├── credential.go [M1 CREATE; F06]
│   │   ├── digest.go [M1 CREATE; F05]
│   │   ├── digest_test.go [M1 CREATE; T_V03]
│   │   ├── dispatch.go [M1 CREATE; N01]
│   │   ├── effect.go [M1 CREATE; F06]
│   │   ├── error.go [M1 CREATE; F05]
│   │   ├── event.go [M1 CREATE; F06]
│   │   ├── field_binding.go [M1 CREATE; N01]
│   │   ├── flow.go [M1 CREATE; F06]
│   │   ├── foundation.go [M1 CREATE; N01]
│   │   ├── identity.go [M1 CREATE; F05]
│   │   ├── object.go [M1 CREATE; R03]
│   │   ├── operation.go [M1 CREATE; F05]
│   │   ├── policy.go [M1 CREATE; R03]
│   │   ├── principal.go [M1 CREATE; R03]
│   │   ├── profile.go [M1 CREATE; F05]
│   │   ├── run.go [M1 CREATE; F06]
│   │   ├── schedule.go [M1 CREATE; F06]
│   │   ├── schema.go [M1 CREATE; F05]
│   │   ├── schema_test.go [M1 CREATE; T_V01]
│   │   ├── settlement.go [M1 CREATE; N01]
│   │   ├── state_test.go [M1 CREATE; T_V04]
│   │   ├── task.go [M1 CREATE; F06]
│   │   ├── tenant.go [M1 CREATE; R03]
│   │   ├── value.go [M1 CREATE; F05]
│   │   ├── value_test.go [M1 CREATE; T_V02]
│   │   └── workflow.go [M1 CREATE; R03]
│   ├── credentials/
│   │   ├── BUILD.bazel [M3 CREATE; F13]
│   │   ├── boundary_test.go [M3 CREATE; T_V16]
│   │   ├── redact.go [M3 CREATE; F13]
│   │   ├── refresh.go [M3 CREATE; F13]
│   │   ├── refresh_test.go [M3 CREATE; T_V17]
│   │   ├── resolve.go [M3 CREATE; F13]
│   │   ├── test_helpers_test.go [M3 CREATE; T_V17]
│   │   └── vault.go [M3 CREATE; F13]
│   ├── httpapi/
│   │   ├── admin.gen.go [M3 GENERATE; F12g]
│   │   ├── admin.go [M3 CREATE; N08]
│   │   ├── admin_service.go [M3 CREATE; N08]
│   │   ├── admin_test.go [M3 CREATE; T_V16]
│   │   ├── approvals.go [M8 CREATE; F33]
│   │   ├── artifacts.go [M4 CREATE; F23]
│   │   ├── events.go [M4 CREATE; F23]
│   │   ├── flows.go [M9 CREATE; F36]
│   │   ├── identity.go [M3 CREATE; R20]
│   │   ├── middleware.go [M3 CREATE; R05]
│   │   ├── plans.go [M8 CREATE; F33]
│   │   ├── profiles.go [M8 CREATE; F33]
│   │   ├── protocol_test.go [M3 CREATE; T_R06]
│   │   ├── routes.go [M3 CREATE; F11]
│   │   ├── runs.go [M4 CREATE; F21b]
│   │   ├── schedules.go [M9 CREATE; F36]
│   │   ├── schema.go [M7 CREATE; F29]
│   │   ├── server.gen.go [M1 GENERATE; F07]
│   │   ├── service.go [M3 CREATE; R05]
│   │   ├── service_config.go [M3 CREATE; R05]
│   │   ├── sessions.go [M3 CREATE; R05]
│   │   ├── strict_test.go [M3 CREATE; T_R06]
│   │   └── warehouse.go [M7 CREATE; F29]
│   ├── objects/
│   │   ├── gc.go [M7 CREATE; R11]
│   │   ├── publication_test.go [M7 CREATE; T_R16]
│   │   ├── reservations.go [M7 CREATE; R11]
│   │   ├── s3.go [M7 CREATE; R11]
│   │   ├── scope_gc_test.go [M7 CREATE; T_R17]
│   │   ├── store.go [M7 CREATE; R11]
│   │   └── verify.go [M7 CREATE; R11]
│   ├── proof/
│   │   ├── currentness.go [M6 CREATE; F25]
│   │   ├── currentness_test.go [M6 CREATE; T_V33]
│   │   ├── inventory.go [M6 CREATE; F25]
│   │   ├── oracle.go [M6 CREATE; F25]
│   │   ├── oracle_test.go [M6 CREATE; T_V32]
│   │   ├── receipt.go [M6 CREATE; F25]
│   │   └── runner.go [M6 CREATE; F25]
│   ├── runtime/
│   │   ├── admission.go [M6 CREATE; N04]
│   │   ├── admission_test.go [M6 CREATE; T_V74]
│   │   ├── binary.go [M11 CREATE; F37]
│   │   ├── binary_test.go [M11 CREATE; T_V31]
│   │   ├── egress.go [M6 CREATE; F20]
│   │   ├── egress_test.go [M6 CREATE; T_V27]
│   │   ├── http.go [M6 CREATE; F20]
│   │   ├── http_test.go [M6 CREATE; T_V26]
│   │   ├── inputs.go [M6 CREATE; F20]
│   │   ├── limits.go [M6 CREATE; N04]
│   │   ├── limits_test.go [M6 CREATE; T_V67]
│   │   ├── pagination.go [M6 CREATE; F20]
│   │   ├── pagination_test.go [M6 CREATE; T_V29]
│   │   ├── postgres.go [M7 CREATE; F26]
│   │   ├── postgres_cdc.go [D1 DEFER; F37d]
│   │   ├── postgres_cdc_test.go [D1 DEFER; T_V48]
│   │   ├── response.go [M6 CREATE; F20]
│   │   ├── retry.go [M6 CREATE; F20]
│   │   ├── retry_test.go [M6 CREATE; T_V28]
│   │   ├── sqltls.go [M7 CREATE; F26]
│   │   ├── sqltls_test.go [M7 CREATE; T_V35]
│   │   ├── sqltypes.go [M7 CREATE; F26]
│   │   └── sqltypes_test.go [M7 CREATE; T_V34]
│   ├── schema/
│   │   ├── completeness.go [M7 CREATE; N05]
│   │   ├── discover.go [M7 CREATE; F27]
│   │   ├── drift.go [M7 CREATE; N05]
│   │   ├── drift_test.go [M7 CREATE; T_V73]
│   │   ├── effective.go [M7 CREATE; F27]
│   │   ├── predicates.go [M7 CREATE; F27]
│   │   ├── predicates_test.go [M7 CREATE; T_V25]
│   │   ├── property_forms.go [M10 CREATE; R19]
│   │   ├── refresh.go [M10 CREATE; R19]
│   │   ├── snapshot.go [M7 CREATE; F27]
│   │   └── snapshot_test.go [M7 CREATE; T_V36]
│   ├── server/
│   │   ├── BUILD.bazel [M3 CREATE; F11]
│   │   ├── browser.go [M3 CREATE; F11]
│   │   ├── browser_assets/
│   │   │   ├── callback.html [M3 CREATE; R05]
│   │   │   ├── callback.js [M3 CREATE; R05]
│   │   │   ├── credentials.html [M3 CREATE; R05]
│   │   │   ├── credentials.js [M3 CREATE; R05]
│   │   │   ├── device.html [M3 CREATE; R05]
│   │   │   ├── device.js [M3 CREATE; R05]
│   │   │   ├── login.html [M3 CREATE; F12]
│   │   │   ├── login.js [M3 CREATE; F12]
│   │   │   └── pm.css [M3 CREATE; R05]
│   │   ├── delegations_integration_test.go [M3 CREATE; T_R08]
│   │   ├── health.go [M3 CREATE; F11]
│   │   ├── integration_fixture_test.go [M3 CREATE; T_R07]
│   │   ├── integration_test.go [M3 CREATE; T_R07]
│   │   ├── lifecycle.go [M3 CREATE; F11]
│   │   ├── lifecycle_test.go [M3 CREATE; T_V13]
│   │   ├── observability.go [M12 CREATE; F39]
│   │   ├── observability_test.go [M12 CREATE; T_V50]
│   │   ├── secrets.go [M3 CREATE; F11]
│   │   ├── secrets_test.go [M3 CREATE; T_V13]
│   │   ├── server.go [M3 CREATE; F11]
│   │   └── service_accounts_integration_test.go [M3 CREATE; T_R08]
│   ├── store/
│   │   ├── account_permits_test.go [M2 CREATE; T_R05]
│   │   ├── admission_test.go [M2 CREATE; T_V09]
│   │   ├── approvals.go [M8 CREATE; F32]
│   │   ├── artifact_test.go [M2 CREATE; T_V08]
│   │   ├── artifacts.go [M2 CREATE; F08]
│   │   ├── audit.go [M2 CREATE; F09]
│   │   ├── auth.go [M3 CREATE; R05]
│   │   ├── budgets.go [M2 CREATE; R04]
│   │   ├── checkpoint_identity_test.go [M7 CREATE; T_V83]
│   │   ├── checkpoints.go [M2 CREATE; R04]
│   │   ├── commands.go [M4 CREATE; R08]
│   │   ├── connections.go [M2 CREATE; R04]
│   │   ├── credential_lifecycle.go [M3 CREATE; F13]
│   │   ├── db.go [M2 CREATE; F08]
│   │   ├── dispatches.go [M2 CREATE; N07]
│   │   ├── event_test.go [M2 CREATE; T_V11]
│   │   ├── events.go [M2 CREATE; F09]
│   │   ├── flows.go [M9 CREATE; F35]
│   │   ├── identities.go [M2 CREATE; R04]
│   │   ├── identity_ledger.go [M8 CREATE; F32]
│   │   ├── identity_mutations.go [M3 CREATE; R20]
│   │   ├── lease_test.go [M2 CREATE; T_V10]
│   │   ├── leases.go [M2 CREATE; F09]
│   │   ├── manifests.go [M2 CREATE; R04]
│   │   ├── memberships.go [M2 CREATE; R04]
│   │   ├── migrate.go [M2 CREATE; F08]
│   │   ├── migrate_test.go [M2 CREATE; T_V06]
│   │   ├── migrations/
│   │   │   └── postgres/
│   │   │       ├── 0001_control.sql [M2 CREATE; F10]
│   │   │       ├── 0002_tenancy_rls.sql [M2 CREATE; R04]
│   │   │       ├── 0003_identity.sql [M3 CREATE; R05]
│   │   │       ├── 0004_credential_lifecycle.sql [M3 CREATE; F13]
│   │   │       ├── 0005_workflow_outbox.sql [M4 CREATE; R08]
│   │   │       ├── 0006_warehouse_schema.sql [M7 CREATE; R17]
│   │   │       ├── 0007_profiles_approvals.sql [M8 CREATE; F32]
│   │   │       ├── 0008_schedules_flows.sql [M9 CREATE; F35]
│   │   │       └── 0009_dynamic_schema.sql [M10 CREATE; R19]
│   │   ├── occurrences.go [M9 CREATE; R13]
│   │   ├── outbox.go [M2 CREATE; R04]
│   │   ├── profiles.go [M8 CREATE; F32]
│   │   ├── provider_accounts.go [M2 CREATE; R04]
│   │   ├── receipts.go [M2 CREATE; F09]
│   │   ├── rls_test.go [M2 CREATE; T_R03]
│   │   ├── runs.go [M2 CREATE; F09]
│   │   ├── schedule_mutations.go [M9 CREATE; R13]
│   │   ├── schedules.go [M9 CREATE; F35]
│   │   ├── scope.go [M2 CREATE; F08]
│   │   ├── scope_test.go [M2 CREATE; T_V07]
│   │   ├── secrets.go [M2 CREATE; R04]
│   │   ├── settlements.go [M2 CREATE; N07]
│   │   ├── tasks.go [M2 CREATE; F09]
│   │   ├── tenant_keys_test.go [M2 CREATE; T_R04]
│   │   ├── transaction.go [M2 CREATE; F08]
│   │   └── workflows.go [M4 CREATE; R08]
│   ├── sync/
│   │   ├── absence_test.go [M8 CREATE; T_V72]
│   │   ├── ack_test.go [M7 CREATE; T_V39]
│   │   ├── approval_test.go [M8 CREATE; T_V40]
│   │   ├── binary.go [M11 CREATE; F38]
│   │   ├── capacity.go [M8 CREATE; N06]
│   │   ├── capacity_test.go [M8 CREATE; T_V70]
│   │   ├── checkpoint.go [M7 CREATE; F28b]
│   │   ├── direct.go [M6 CREATE; F24]
│   │   ├── dispatch.go [M8 CREATE; N06]
│   │   ├── dispatch_test.go [M8 CREATE; T_V69]
│   │   ├── etl.go [M7 CREATE; F28b]
│   │   ├── identity_test.go [M8 CREATE; T_V41]
│   │   ├── modes.go [M11 CREATE; F38]
│   │   ├── modes_test.go [M11 CREATE; T_V49]
│   │   ├── readback.go [M8 CREATE; F30]
│   │   ├── readback_test.go [M8 CREATE; T_V42]
│   │   ├── result_retention_test.go [M8 CREATE; T_V81]
│   │   ├── reverse.go [M8 CREATE; F30]
│   │   ├── settlement.go [M8 CREATE; N06]
│   │   ├── settlement_test.go [M8 CREATE; T_V68]
│   │   ├── transport.go [M11 CREATE; F38]
│   │   ├── workset.go [M8 CREATE; F30]
│   │   └── write.go [M8 CREATE; F30]
│   ├── temporal/
│   │   ├── bindings.go [M4 CREATE; R08]
│   │   ├── client.go [M4 CREATE; R08]
│   │   ├── commands.go [M4 CREATE; R08]
│   │   ├── deployment.go [M9 CREATE; R13]
│   │   ├── occurrence_test.go [M9 CREATE; T_R20]
│   │   ├── outbox.go [M4 CREATE; R08]
│   │   ├── outbox_test.go [M4 CREATE; T_R09]
│   │   ├── projection.go [M4 CREATE; R08]
│   │   ├── projection_test.go [M4 CREATE; T_R10]
│   │   ├── replay_test.go [M9 CREATE; T_R21]
│   │   ├── schedule_cas_test.go [M9 CREATE; T_R19]
│   │   ├── schedule_test.go [M9 CREATE; T_V45]
│   │   └── schedules.go [M9 CREATE; R13]
│   ├── warehouse/
│   │   ├── child.go [M7 CREATE; R12]
│   │   ├── child_test.go [M7 CREATE; T_R18]
│   │   ├── compaction.go [M7 CREATE; F28]
│   │   ├── manifest.go [M7 CREATE; F28]
│   │   ├── object_layout.go [M7 CREATE; F28]
│   │   ├── parquet.go [M7 CREATE; F28]
│   │   ├── publish_test.go [M7 CREATE; T_V38]
│   │   ├── query.go [M7 CREATE; F28]
│   │   ├── query_protocol.go [M7 CREATE; R12]
│   │   ├── query_test.go [M7 CREATE; T_V37]
│   │   ├── scratch.go [M7 CREATE; R12]
│   │   └── segments_test.go [M7 CREATE; T_V57]
│   └── workflows/
│       ├── commands.go [M4 CREATE; R06]
│       ├── continue.go [M4 CREATE; R06]
│       ├── continue_test.go [M4 CREATE; T_R11]
│       ├── options.go [M4 CREATE; R06]
│       ├── payload_test.go [M4 CREATE; T_R12]
│       ├── replay.go [M9 CREATE; R13]
│       ├── run.go [M4 CREATE; R06]
│       └── scheduled.go [M4 CREATE; R06]
├── packaging/
│   ├── Dockerfile.query [M7 CREATE; R12]
│   ├── Dockerfile.server [M12 CREATE; N10]
│   ├── Dockerfile.worker [M12 CREATE; R15]
│   ├── config/
│   │   └── server.example.json [M0 CREATE; N09]
│   ├── install-manifest.schema.json [M0 CREATE; N09]
│   └── release-pair.schema.json [M0 CREATE; N09]
├── schemas/
│   ├── atlas.schema.json [M1 CREATE; F04]
│   ├── bundle.schema.json [M1 CREATE; F04]
│   ├── field-binding.schema.json [M1 CREATE; N01]
│   ├── foundation.schema.json [M1 CREATE; N01]
│   ├── identity.schema.json [M1 CREATE; R03]
│   ├── proof.schema.json [M1 CREATE; F04]
│   ├── scenario.schema.json [M1 CREATE; F04]
│   ├── semantics.schema.json [M1 CREATE; F04]
│   ├── server-config.schema.json [M0 CREATE; R01]
│   ├── source.schema.json [M1 CREATE; F04]
│   └── workflow-ref.schema.json [M1 CREATE; R03]
├── scripts/
│   ├── backup-stack.sh [M12 CREATE; R15]
│   ├── check-boundaries.sh [M0 CREATE; F01]
│   ├── generate.sh [M1 CREATE; F04]
│   ├── install.sh [M0 CREATE; N09]
│   ├── pm-stack [M0 CREATE; R02]
│   ├── prove.sh [M6 CREATE; F25]
│   ├── release.sh [M12 CREATE; F40]
│   ├── restore-stack.sh [M12 CREATE; R15]
│   ├── test-changed.sh [M0 CREATE; F01]
│   ├── uninstall.sh [M12 CREATE; N10]
│   ├── upgrade.sh [M12 CREATE; N10]
│   └── verify-release.sh [M12 CREATE; R15]
├── sdk/
│   └── browser/
│       ├── client.ts [M5 CREATE; R09]
│       ├── package-lock.json [M5 CREATE; R09]
│       ├── package.json [M5 CREATE; R09]
│       ├── schema.d.ts [M5 GENERATE; R09]
│       └── tsconfig.json [M5 CREATE; R09]
├── tests/
│   ├── client-parity/
│   │   ├── browser.ts [M5 CREATE; R09]
│   │   ├── fixtures.json [M5 CREATE; R09]
│   │   ├── index.html [M5 CREATE; R22]
│   │   ├── package-lock.json [M5 CREATE; R22]
│   │   ├── package.json [M5 CREATE; R22]
│   │   ├── parity_test.go [M5 CREATE; R09]
│   │   └── playwright.config.ts [M5 CREATE; R22]
│   ├── install/
│   │   ├── backup_test.go [M12 CREATE; T_V51]
│   │   ├── deployment_test.go [M13 CREATE; T_V54]
│   │   ├── install_test.go [M3 CREATE; T_V75]
│   │   ├── managed_test.go [M12 CREATE; T_R26]
│   │   ├── native_images_test.go [M12 CREATE; T_R25]
│   │   ├── ownership_test.go [M3 CREATE; T_V76]
│   │   ├── release_test.go [M12 CREATE; T_V82]
│   │   ├── restore_epoch_test.go [M13 CREATE; T_V52]
│   │   ├── uninstall_test.go [M12 CREATE; T_V78]
│   │   └── upgrade_test.go [M12 CREATE; T_V77]
│   └── stack/
│       ├── config_test.go [M0 CREATE; T_R01]
│       ├── fairness_test.go [M9 CREATE; T_R22]
│       ├── load_test.go [M13 CREATE; R16]
│       ├── outages_test.go [M13 CREATE; R16]
│       ├── replicas_test.go [M13 CREATE; R16]
│       ├── restore_effects_test.go [M13 CREATE; R16]
│       └── storage_qualification_test.go [M0 CREATE; T_R02]
└── tools/
    ├── api_admin_generation.go [M3 CREATE; F12g]
    ├── atlas/
    │   └── main.go [M6 CREATE; N11]
    ├── dependency_policy_test.go [M0 CREATE; T_V56]
    ├── go.mod [M0 CREATE; F01]
    ├── go.sum [M0 CREATE; F01]
    ├── release/
    │   └── main.go [M12 CREATE; N12]
    ├── traceability_test.go [M6 CREATE; T_V80]
    └── versions.json [M0 CREATE; F01]
```

The package count follows real process and state boundaries. `analytics` is separate to isolate native execution; `workflows` is separate to enforce deterministic code; `httpapi` isolates Gin from application services; `api` prevents client imports from pulling server execution into `pm`. The remaining foundations are small owner packages rather than provider packages. Connector JSON, lane inventories and generated shell wrappers are per connector; runtime logic is shared.

There is one authored public API document, one generated runtime bundle per connector and one actual handler composition. A source lock references captured bytes rather than embedding duplicated giant catalogs. Seven generated lane files enable direct connector/lane navigation. Minimal scenarios contain only relationships, prerequisites and independent expected effects that the source cannot derive. The common proof runner owns execution, retries and receipt formatting. No global CURRENT, proposal generation, release-batch proof authority or duplicate runtime reader is introduced.

Migration ordering follows implementation dependency:0001 control,0002 tenancy/RLS,0003 workflow/outbox,0004 warehouse/schema,0005 profiles/approvals,0006 schedules/flows,0007 dynamic metadata. Separate Temporal and filer schema jobs never enter that migration chain. A file created earlier can receive later wiring edits: server/routes/app capability/worker registration are explicitly revisited at the relevant M4/M6/M7/M8/M9/M10/M11 boundaries. A foundation absent at an intermediate milestone is unavailable, not implemented by an alternate stub product engine.

## 12. Pseudocode, transactions and recovery flows

**Proposed/Recommended.** These are algorithm contracts, not implementation code. `TX` means a scoped PM PostgreSQL transaction. No TX spans provider/network/object/Temporal I/O. Each flow names its public boundary, durable output, error behavior and foundation owners. Client checks improve UX; server checks determine authority.

### PF01 — equivalent client invocation

Input: generated CLI flags or browser typed form, explicit workspace/connection/operation revision/authentication, typed provider inputs and optional shared output selection/filter. Both clients build the same Invocation DTO and preflight presence/type/path conflicts before any request; the server repeats exact source/handler/schema/resource-permission and typed-control validation before provider I/O. A write missing approval produces a sealed PlanRef; an approved write or read proceeds to PF12 durable admission and yields the same 202 RunRef for equivalent requests. No output controls means complete records; explicit controls produce truthful selection/filter/pagination metadata. For mutations, the client persists a secret-free scoped key/canonical intent before its first client-to-API send, retries only that key after transport/5xx/503/504 uncertainty, makes at most three sends in 30 seconds with 250ms then 1s delay, accepts server Retry-After only within the remaining deadline, and never auto-rebases 412, resubmits 410, mints a replacement key or calls a provider directly; provider/run retry scheduling is independent. Client wait/cancel context stops viewing only unless an explicit run-control API is called. Owners: FND-03/04/10/16/17; `internal/cli/dynamic.go`, `internal/httpapi/runs.go`, `internal/app/admission.go`; R14/V26.

### PF02 — human/automation authentication and provider resolution

Input: browser OIDC callback, approved device proof or bounded PM service identity. Validate route-specific origin/CSRF/state/nonce/issuer/audience/expiry; map to immutable actor and current grants. Service-account/delegation grants retain explicit connection/operation/workspace/time ceilings, cannot exceed the delegator and preserve distinct auditable actor/effective actor. All six access mutations authenticate and authorize before idempotency disclosure, require scoped key plus collection/target `If-Match` (missing 428, stale 412), return original create/grant 201 or revoke/already-revoked 200, and expose an exact authorized replay as 200 with explicit original safe result/revision. Only the original successful service-account create response carries its one-time PM automation credential; replay/read/log/cache/receipt/error paths omit it, and loss requires revoke/new create. At API acceptance persist a separate execution grant rather than the login token; before every physical dispatch recheck that grant, delegation/membership/connection/approval epoch and account witness, then obtain a scoped signer from encrypted server state. Failure produces a safe auth/permission/reauthorization result with zero new dispatch. Raw credentials never cross the public API, Temporal payload or logging, and read-only reconciliation uses a narrower grant without replacement/compensating-write authority. Owners: FND-05/17; auth/credentials/store; R07/R08/R23/V16.

### PF03 — source compilation and catalog invalidation

Input: source lock, captured reference map, schema dialect and sparse semantic amendments. For GitHub M6 `issues/list-for-repo`, the accepted authority is `github/rest-api-description` commit `0a144e030abbe358000f0d32874e197b59a8f7ed`, file `descriptions/api.github.com/api.github.com.json`, pointer `#/paths/~1repos~1{owner}~1{repo}~1issues/get`, blob `ed76b1371e381b078e50c8a698b890911031aeb2`, 12,939,686 bytes, SHA-256 `7c3a4bfa71e850a32992882392db3cf315dcdeca46ba5a62da963dbea6a589a2`; its MIT `LICENSE.md` is blob `b50625eb63949013cae604b1cadd42cfa1eaf825`, 1,063 bytes, SHA-256 `3243761cbac07e6d169a5a2f4e7c25cc544da85248e735df74c3672e055cc87b`, Copyright (c) 2020 GitHub. Parse under byte/depth/reference budgets; retain raw hash/pointer and every source operation/field/media/status, normalize types without coercion and join source fidelity before enabling a field. Derive stable operation identities, command/flag aliases, lane requirements and minimal scenario bindings; check actual handler/version closure and emit deterministic bundles/help/seven inventories. A changed source/semantic/schema hash creates a new immutable revision: accepted runs keep pinned bindings, mutable plans get affected-dependency incompatibility, and an unknown shape blocks only its cell while recording one Atlas gap. Raw upstream bytes remain external; the repository stores source locks, complete reference closure/capture evidence, generated output and NOTICE. Owners: FND-01/02/03/04/09; V21/V22/V24/V65/V84.

### PF04 — direct read and truthful pagination

Input: admitted read operation, provider navigation plus optional shared typed field selection/filter, declared page size and PM record cap. Reject malformed path/type/presence or conflicting raw/PM navigation controls before provider I/O. Reserve account/request/byte budgets, persist the physical attempt and send exactly one declared provider page request. Decode the matching response variant and all source entries—including GitHub pull-request entries returned by the issues operation—then filter on unprojected records before selection; preserve selected nested values and absent/null/value distinctions. Distinguish actual provider request page size from PM cap: if a page crosses the cap, emit only the allowed prefix and retain unread tail under a scoped continuation. An empty filtered page is not exhaustion while source continuation exists. Persist a durable one-page result artifact and metadata reporting effective sent size, fetched/returned/emitted counts, cap, unread tail, usable next navigation and provider completeness/reason. One page or absence of a cursor makes no CDC or durable extraction/checkpoint-completeness claim. Owners: FND-06/08/10/16; runtime/sync/direct; V29/V67/P_direct_read.

### PF05 — ETL publication before checkpoint

Input: immutable extraction schema/source generation, current checkpoint revision and authorized dataset. Worker reads a bounded page and materializes bounded Parquet through the restricted child, retaining presence/type metadata. Reserve an object attempt, upload to a unique key, verify length/digest, then TX lock the relevant checkpoint/manifest row and compare the complete prior identity/fence. Commit object reference, manifest version, page receipt, exact acknowledged prefix, next continuation and event together. Upload-success/TX-failure leaves an orphan; TX-success/reply-loss resumes from the committed receipt. No cursor advances over missing rows, missing members or unverified bytes. Owners: FND-12/13/16; objects/warehouse/store; V38/V39/V83/R16.

### PF06 — saved reverse ETL and member settlement

Input: saved mapping/profile revision, immutable source dataset/schema, exact desired workset, destination account and full action plan. Preview validates required/read-only/key/null/type rules and counts every physical primary/secondary action, including readback; approval seals workset/action/schema/account/budget hashes. PostgreSQL mapped upsert is eligible only with a stable key, usable unique/primary constraint and unambiguous writable mapping; reject missing/ambiguous keys, duplicate source keys and unsupported mappings before database I/O. For each independent key, insert if absent; if present update only approved mapped columns whose values differ; exact equality is unchanged with zero assignments. Preserve unmapped/unrelated columns and rows; never infer delete, DDL, raw SQL or a customer-owned effect ledger. Any key/account/generation reset is an explicit approved migration retaining the old mapping and dispatch history. TX records stable entity/effect/dispatch/member identities and atomically reserves request, encoded-byte, result/readback/artifact capacity before the exact envelope is sent under the current grant. Correlate every result by a documented key/order and exact cardinality, independently read back declared fields/effects, settle members independently and advance only safe contiguous progress. A confirmed rollback can permit safely re-admitted work but does not prove no physical dispatch; an unknown COMMIT remains unknown and charged until a declared sound occurrence witness matches the exact sealed intent. Desired rows alone are insufficient, blind retry is forbidden and actual dispatch history remains auditable. Owners: FND-14/15/17, C_postgres #28 then serialized #34 semantics/lifecycle update; M8 V40/V41/V42/V43/P_reverse_etl.

### PF07 — Temporal schedule mutation and occurrence

Input: immutable flow/profile revision, approved recurring policy, timezone/calendar, overlap/catchup, expected schedule revision. TX checks If-Match and writes desired revision/audit/outbox. Dispatcher serializes that schedule, describes Temporal, sends current conflict token plus persisted RequestId for the exact payload, verifies applied revision and acknowledges. Changed rebase payload gets a new linked dispatch ID; old tombstones cannot resurrect schedules.

Temporal starts `PMScheduledRunWorkflow` with safe schedule references. Its first activity verifies schedule provenance and TX admits unique `(schedule_id, nominal_UTC_instant)`, RunSpec/grant/budgets/audit and binding-outbox **before effects**. It adopts the already-running execution, never inserts a second StartRun. Then the same coordinator stays open through actual work so overlap SKIP covers the full run. Denied occurrences are visible. DOM/DOW AND, both DST folds, missing-time skip, explicit1h catchup and finite preview/backfill are tested. Owners: FND-16/18; temporal/workflows/activities; V45/R19/R20.

### PF08 — remote acceptance without local acknowledgement

Input: persisted exact dispatch/member intent whose provider request may have been accepted before process death or timeout. The retrying activity first loads the ledger. A committed receipt returns its outcome without another request. A definitely unsent attempt can be re-admitted. A possibly sent request becomes unknown; use the same documented provider idempotency key within its retention horizon or an exact-ID/field reconciliation witness. Otherwise preserve unknown and require explicit disposition. A new Temporal attempt or local fence is never proof of no provider effect. Owners: FND-10/14/16; sync/activities/app reconciliation; V28/V43/R13.

### PF09 — API/worker restart and projection repair

Input: PG accepted runs/outbox/effect receipts plus Temporal execution observations. A new API replica serves PG state immediately when dependencies permit. A worker replays deterministic coordination and loads durable effect state before each unit. Open workflow with failed final projection retries its idempotent projection activity. Closed retained history can repair only orchestration observation; it cannot run a missing final activity after closure. Never-bound/no-dispatch NotFound may safely retry stable start under a current claim. Previously bound/expired/restored/uncertain NotFound becomes orchestration-unknown and reconciles PM effects; no automatic blind restart. Explicit authorized recovery gets a new orchestration generation while preserving effect IDs. Owners: FND-16; temporal/app/store; R10/R13.

### PF10 — SSE and UI reconnect

Input: authenticated run ID and prior scoped SSE cursor. TX read a consistent run snapshot/revision and per-run sequence. Stream committed events strictly after that sequence; allocate new sequence under the run-row lock in the state/event transaction. Bounded notifications wake a PG query; they are not replay storage. On disconnect, close only the subscriber. On reconnect to any replica, replay/deduplicate; expired cursor returns an explicit snapshot-required gap. A slow reader hits bounded buffer/write timeout and resumes from its last committed cursor. Owners: FND-16/19; store/events and httpapi/events; V30, including delayed/rolled-back writer ordering.

### PF11 — stack startup and upgrade

Input: one strict config, ownership flags, immutable image/tool versions and secret references. Operator validates budgets/identity and starts owned dependencies; dependency schema jobs finish before API/worker admission. Probe exact schemas/private auth/object readiness, not open ports. Upgrade quiesces new admission/schedules as required, retains versioned workers for open runs, expands PM schema compatibly, applies Temporal's supported schema/server sequence and replay-tests histories before promoting new workers. Failure preserves volumes/old images and refuses unsupported downgrade. Managed services are never lifecycle-owned by the launcher. Owners: FND-20; deploy/scripts/server; R01/R25/R26/V77.

### PF12 — PM admission and workflow handoff

Input: validated immutable execution request, current authority, canonical request fingerprint and idempotency key scoped by tenant/workspace/principal/operation/target. Authenticate/authorize before replay disclosure and apply refusal precedence exactly: workspace pending-count 429 first, nonempty workspace oldest-pending-reservation age 429 second when its oldest immutable `accepted_at` is `>=` its cap, and deployment pending-count overload 503 only after both workspace predicates pass when they coincide. Using PostgreSQL time in one transaction, lock/check scope+key: same fingerprint returns the original run/current safe status with no reservation, dispatch or capacity check; changed fingerprint is 409. Reserve workspace/deployment pending admission, require every post-reservation count `<=` its cap, and refuse a nonempty workspace pending-reservation population when its oldest immutable `accepted_at` age is `>=` its cap; an empty pending-reservation set has no age refusal. `development` is global 32/workspace 8/2m, `standard` is 256/32/10m and 3/2/30s is test-only. Pending reservations are separate from tenant/run/window/account execution budgets. Persist key/fingerprint, immutable RunSpec, execution grant, run, all budgets, pending reservation, audit/event and StartRun outbox atomically before 202. PG refusal yields no accepted response; unknown commit reconciles only the same key, and a committed row survives reply loss. Dispatcher retries durable outbox delivery; pending capacity releases only at the controlled durable execution-start transition or known terminal pre-start failure, never Temporal acknowledgement/pre-start presence. Issue #14 proves the actual route and controlled transition; issue #16 later proves actual provider-worker integration. Response replay records remain at least seven days; unresolved outcomes never expire and namespace-lifetime terminal/generation guards prevent late duplicate effects. Owners: FND-08/10/16/17; V05/V09/V24/V26/V32/V34/R09/R10.

### PF13 — dynamic metadata drift

Input: connection/account/source identity and current complete metadata revision. Discovery runs as an admitted provider activity, gathering all required pages under bounds; a partial/error candidate remains separate. Compute content fingerprint including field types, required/null/read-only/options/keys, not merely format version. Compare field-level dependencies: changed relevant field invalidates editable plan compatibility; unrelated changes do not. Accepted plans retain immutable schema/workset but must pass current safety predicates before physical writes. Never silently rebind accepted inputs to latest metadata. Owner: FND-09/17; schema/store/app; V36/V65/V73.

### PF14 — partial physical batch

Input: sealed ordered member set and provider-declared request/response correlation contract. Dispatch intent is durable before sending the action-owned array/object envelope. For a documented mixed outcome response of28 inputs with27 correlated successes and one rejection, persist the27 and retry only the remaining member if independently safe. For a provider that returns only ordered all-success records, a short or uncorrelated response leaves affected members unknown rather than fabricating mixed statuses. A count alone proves nothing. Capacity is checked before send so an expanded locator/readback cannot make the outcome unrecordable. Owner: FND-14; sync/dispatch/settlement/capacity; V68–V70/V81.

### PF15 — concurrent workspaces and shared provider account

Input: two independent clients with memberships in several workspaces, plus two connection aliases for one provider account. Every API route/CLI invocation carries explicit workspace; catalog caches bind server/principal/workspace/connection/schema revision. PG composite keys and RLS deny cross-bindings even for identical names/IDs. Worker claims contain references, then re-enter scoped transactions; a payload tenant ID alone is not authority. Account permits coordinate across aliases/replicas/queues while resource grants remain connection-specific. Queue fairness is complementary and explicitly enabled. Owner: FND-05/10/16/17; auth/store/app; R03–R05/R15/R22/R27.

### PF16 — revocation during accepted execution

Input: an active accepted run and an explicit policy/grant revocation or routine login-session expiry. Ordinary session expiry/logout affects future API actions only; current run grant remains valid. Explicit run/principal/delegation/membership/connection/approval revocation increments policy epoch and commits audit/control outbox. Before each new physical call, worker checks current execution grant and stops unsent work when invalid. Already-dispatched effects remain possibly real; scoped read-only reconciliation may continue, but cannot create replacement/compensating writes without new authorization. Cancellation/termination is not rollback. Owner: FND-05/16/17; app/revocation and activities; R23/V47/V74.

**Temporal execution discipline — proposed with inspected API support.** Workflows use deterministic SDK time/timers/channels/activities, not wall clocks/network/PG calls. Keep encoded workflow/control payloads≤64KiB and error summaries≤1KiB, use at most32 in-flight bounded units per run, and ContinueAsNew at the server suggestion or2,000 events/2MiB, whichever comes first. Drain buffered commands/handlers before rollover and retain the original deadline in PG. These are proposed PM caps, not measured throughput. [Workflow restrictions](https://github.com/temporalio/sdk-go/blob/b7c242c6894df088a57a85b33d0586e908da8b93/workflow/doc.go#L71), [ContinueAsNew guidance](https://github.com/temporalio/documentation/blob/bc5c548f28d0bc0b72e31976e2865563f40a85fc/docs/develop/go/workflows/continue-as-new.mdx#L63).

The initial provider HTTP adapter uses HTTP/1.1 fresh connections (DisableKeepAlives=true, ForceAttemptHTTP2=false, non-nil empty TLSNextProto map), disables redirects and does not synthesize replayable GetBody/idempotency headers for unsafe writes. This costs connection/TLS latency; HTTP2-only operations stay unsupported until a separately qualified single-attempt transport exists. V28/V74 must prove no hidden transport resend before pooling or HTTP2 is enabled. [Pinned Go transport retry frontier](https://github.com/golang/go/blob/862c888e612ac346c7c4d99c9392bdfd265f33b0/src/net/http/transport.go#L89). Temporal owns safe logical-activity retry timing. The provider transport performs one physical request per recorded attempt; there is no nested unbounded retry multiplier. Set explicit ScheduleToClose, StartToClose and heartbeat timeouts; transport deadline is shorter than the activity attempt. Heartbeats carry safe progress refs and permit cooperative cancellation. Retry-After becomes shared account cooldown plus a durable timer. Unknown effects enter reconciliation instead of ordinary retry. Updates/signals carry persisted command IDs; workflow validators cannot perform PG authorization, so activities revalidate before effect. Versioned worker deployments retain old workers for pinned open workflows and replay captured safe histories before promotion. [Activity options100–161](https://github.com/temporalio/sdk-go/blob/b7c242c6894df088a57a85b33d0586e908da8b93/internal/activity.go#L100), [worker deployment69–95](https://github.com/temporalio/sdk-go/blob/b7c242c6894df088a57a85b33d0586e908da8b93/internal/worker.go#L69).

## 13. Dependency milestones and future implementation ownership

**Proposed/Recommended.** Execute fourteen coherent milestones, not an unbounded review loop. Their order puts identity/durability before asynchronous product claims and shared semantic foundations before connector breadth. `MILESTONES.json` lists exact files, interfaces, schemas/migrations, prerequisite IDs, future commands, expected evidence, recovery and non-goals for each. Relative effort is architectural sizing; no fixed completion date or measured time saving is claimed.

| Milestone / prerequisites | Objective / exact file groups | Exit evidence / risk |
|---|---|---|
| M0 Qualify the single stack and build boundary; requires implementation authorization | Freeze MIT module/dependency/image pins; strict config and explicit operator launcher; qualify Seaweed postgres2 pool/listing and selected PG/Temporal schema image pair before product claims. Groups: F01, F02, F03, T_V56, N09, R01, R02, T_R01, T_R02 | R01, R02, V56. Medium; infrastructure/native qualification is the early falsifier. Non-goal: No provider operations, user data or public usability claim. |
| M1 Define versioned contracts and tenant identity; requires M0 | Author OpenAPI 3.1 and strict generated shapes for identity/run/grant, complete-by-default ActionResult with typed selection/filter, F01 admission/error/recovery and AUTH mutation/replay contracts, plus seven-lane schemas. Groups: F04, F05, F06, F07, T_V01, T_V02, T_V03, T_V04, T_V05, N01, R03 | V01–V05. Medium; presence, nested values, error precedence and replay status are contract invariants. Non-goal: No accepted asynchronous run until durable storage/handoff exist. |
| M2 Build PostgreSQL state, RLS and secret storage; requires M1 | Scoped non-owner pools, migrations 0001/0002, composite identity keys, outbox/budget/receipt/event repositories and encrypted credential metadata. T_V08/T_V09 create staged store primitives; their actual artifact/admission behavior gates M7/M4. Groups: F08, F09, F10, T_V06, T_V07, T_V08, T_V09, T_V10, T_V11, N07, R04, T_R03, T_R04, T_R05 | V06, V07, V10, V11, R03–R05. Large; tenancy/transaction boundaries precede feature breadth. Non-goal: No client DB/Temporal access or generic customer SQL on PM pool. |
| M3 Serve authenticated API and administration; requires M2 | Gin strict validation/error/context/proxy rules; identity/device/browser/service flows; exact membership/service-account/delegation `If-Match`, scoped-key and replay responses; one-time automation credential boundary; explicit connection/operation/workspace/time grant ceilings, distinct auditable actor/effective actor and isolated provider-secret administration. Groups: F11, F12, F13, T_V13, T_V14, T_V15, T_V16, T_V17, F12g, N08, R05, T_R06, T_R07, T_R08, R20 | V13–V17, R06–R08. Large; auth-before-replay, least-privilege ceilings and credential absence from every replay/read/log/error path are mandatory. Non-goal: No provider secret in ordinary SDK and no full UI. |
| M4 Make accepted runs durable across PG and Temporal; requires M3 | Implement F01 in PG: canonical scope/fingerprint, ordered workspace-count/workspace-age/deployment-overload admission refusals, atomic pending reservations plus separate RunSpec/grant/run/budgets/audit/event/outbox before 202, controlled durable execution-start release, server receipt retention and namespace-lifetime duplicate guards; then workflow binding, command/control outbox, projection repair, durable events and truthful unknown effects. Issue #14 proves the actual route and controlled start transition; actual provider-worker integration remains #16. Groups: F21, F21b, F23, T_V12, T_V66, N02, R06, R07, R08, T_R09, T_R10, T_R11, T_R12, T_R13, R14a, R14b | V09, R09–R13 plus packet-scoped F01 cases. Largest early slice; admission and dual-write crash windows are critical. Non-goal: No reliability claim from an activity stub, Temporal acknowledgement or pre-start presence. |
| M5 Deliver thin CLI and browser API parity; requires M4 | Explicit workspace commands, PM session OS storage, profile negotiation, generated browser wrapper, equivalent use-case harness and #19/#21 bounded same-key client-to-API recovery independent of provider/run retry scheduling. Groups: F14, F15, F22, F22b, T_V18, T_V19, T_V53, R09, R10, T_R15, R21, R22 | V18, V19, V53, R14, R15. Medium; strict flag presence, cache scope, three-send/30-second recovery and no replacement-key/rebase behavior are regression seams. Non-goal: No full UI design/build; harness proves shared application behavior. |
| M6 Compile accepted source authority and prove the first GitHub read; requires M5 | Capture the accepted GitHub commit/blob/license bytes, emit lock/reference/NOTICE and deterministic bundle, compile exact `issues/list-for-repo`, implement complete-by-default F02 selection/filter/pagination, then exercise one real read through API/worker. Groups: F16, F17, F18a, F18b, F18c, F19, F20, F24, F25, C_github, B_github, S_github, T_V20, T_V21, T_V22, T_V23, T_V24, T_V26, T_V27, T_V28, T_V29, T_V32, T_V33, T_V55, T_V65, T_V67, T_V74, T_V80, T_V84, N03, N04, N11 | V20–V24, V26–V29, V32–V33, V55, V65, V67, V80, V84, P_direct_read plus packet-scoped F02/F03 cases. Large; accepted support is one exact operation, not blanket GitHub certification. |
| M7 Publish shared data and establish PostgreSQL connector foundations; requires M6 | Unique S3 objects, PG manifest/checkpoint transaction, immutable Parquet/restricted query child, plus #28 PostgreSQL protocol/SQL/type/metadata/TLS/read source authority and initial lifecycle/semantics. Groups: F26, F27, F28, F28b, F29, F29b, C_postgres, B_postgres, S_postgres, T_V25, T_V34, T_V35, T_V36, T_V37, T_V38, T_V39, T_V57, T_V73, T_V83, N05, R11, R12, T_R16, T_R17, T_R18, R17, R18 | V08, V25, V34–V39, V57, V66, V73, V83, R16–R18, P_etl. Large; connector identity, field fidelity and publication crash windows dominate. Non-goal: no saved `CP-etl-*` routes or mapped upsert yet; CDC deferred. |
| M8 Implement saved ETL/reverse/direct effects and PostgreSQL mapped upsert; requires M7 | Move all `CP-etl-*` routes here with sealed worksets/mappings, approvals, exact effect/member identities, capacity and ambiguity recovery. Serialize #34 updates to #28-owned PostgreSQL lifecycle/semantics: stable-key constrained insert/update-only-changed behavior, unchanged zero assignments, independent readback, no delete/DDL/raw SQL/ledger. Create N13-PG composition only after #28–#34. Groups: F30, F31, F32, F33, F33b, T_V40, T_V41, T_V42, T_V43, T_V68, T_V69, T_V70, T_V71, T_V72, T_V81, N06, N13-PG | V40–V43, V68–V72, V81, P_direct_write, P_reverse_etl plus packet-scoped PG cases. Largest semantic slice; unsupported/ambiguous mappings block before I/O. |
| M9 Add Temporal schedules, controlled recovery and fairness; requires M8 | Typed DAGs, desired/applied schedule revision CAS, occurrence adoption, explicit timezone/misfire semantics, durable controls, worker versions and distributed account permits. Groups: F34, F35, F36, F36b, T_V44, T_V45, T_V46, T_V47, R13, R14, T_R19, T_R20, T_R21, T_R22, T_R23 | V44, V45, V46, V47, R19, R20, R21, R22, R23. Large; schedule overlap/retention/replay and revocation semantics need separate faults. Non-goal: No PM cron scheduler, queue-per-user security claim or per-process global rate claim. |
| M10 Activate account-specific schema refresh and property forms; requires M9 | Complete scoped metadata refresh, drift dependency analysis and immutable accepted bindings; HubSpot/Pylon field shapes and Gong safe form projection. Groups: R19 | V65, V73. Medium to large; account-tier metadata and Pylon source contradiction remain visible. Non-goal: Do not coerce unproven nullability or replace a complete catalog with a partial candidate. |
| M11 Activate remaining connector/lane obligations and update compositions; requires M10 | Finish five-connector declarations and exact seven-lane inventories; activate binary/sync only with proved semantics; update the existing PostgreSQL N13-PG composition for remaining #41 obligations and create the other source-linked lifecycle/composition scenarios. Groups: F37, F38, C_pylon, B_pylon, S_pylon, C_hubspot, B_hubspot, S_hubspot, C_gong, B_gong, S_gong, F42, T_V31, T_V49, N13 | V31, V49, V74, P_direct_read, P_direct_write, P_etl, P_reverse_etl, P_binary_download, P_binary_upload, P_sync_transport. Large/account dependent; no universal parity. Non-goal: early N13-PG does not close V74, P_sync_transport or full #41. |
| M12 Package, upgrade and rehearse recovery; requires M11 | Full/client-only/backend-only packages, immutable image pair, native query helper targets, operator backups/restores, managed ownership, offline installation and uninstall retention. Groups: F39, F40, T_V50, T_V51, T_V77, T_V78, T_V82, N10, N12, R15, T_R25, T_R26 | V50, V51, V75, V76, V77, V78, V82, R24, R25, R26. Large; initial RPO24h/RTO4h targets require measured drills. Non-goal: No HA guarantee from Compose and no automatic purge or schema downgrade. |
| M13 Prove replicas, bounded load and release readiness; requires M12 | Two hosts without shared filesystem, tenant/provider-account limits, outages, historical replay, restored external-effect quarantine and coherent release suite. Groups: F41, T_V52, T_V54, R16 | V52, V54, R27, R28, R29, R30. Large; load envelope is measured here, not promised beforehand. Non-goal: No fixed throughput/time savings or strict operational tier claimed without measurements. |

### M0 implementation order

Dependencies: TD01, TD02, TD10, TD12, TD14, TD24, TD27, TD28.

1. Lock application/tools/native/image dependency identities and MIT notices; establish pure CLI/API/worker versus native analytical-child import boundary.
2. Implement strict config and operator ownership validation, then Compose dependency topology/private endpoints and explicit schema/init jobs.
3. Qualify PostgreSQL16.15/Temporal schema pair and Seaweed postgres2 pool/list/multipart behavior using controlled integration resources.
4. Record actual resource/readiness failures and block unsupported target/backend before any application usability claim.

Later wiring edits: None beyond files first owned here.

### M1 implementation order

Dependencies: TD01, TD03, TD04, TD06, TD07, TD25.

1. Define org/workspace/actor, durable execution grant, operation/value/plan/effect/task/event/workflow/object refs plus complete-by-default ActionResult and typed selection/filter.
2. Author one public OpenAPI 3.1 and separate admin schema; encode exact F01 status/code/precedence/profile/reservation/recovery and six AUTH mutation/replay contracts.
3. Generate strict Gin/Go/browser shapes; reject duplicate/allOf/null/precision loss and malformed result controls with zero provider I/O.
4. Freeze semantic IDs, schema versions, exact number/presence/nesting rules and seven-lane inventory schema before downstream imports.

Later wiring edits: None beyond files first owned here.

### M2 implementation order

Dependencies: TD09, TD10, TD11, TD01, TD21.

1. Create PM role separation and migrations0001/0002 with composite tenant keys and RLS USING/WITH CHECK.
2. Implement scoped pgx transactions/pool cleanup, identity/connection/credential metadata repositories and safe constraint errors.
3. Implement immutable RunSpec/outbox/budget/effect/event repositories and per-account permits; no network calls inside transactions.
4. Fault migration/rollback/cancellation/pool reuse and cross-resource writes, then prove atomic acceptance and receipt/event commit primitives.

Later wiring edits: None beyond files first owned here.

### M3 implementation order

Dependencies: TD02, TD04, TD06, TD15, TD16, TD01.

1. Compose only available Gin/config/store/auth services; Temporal/provider runtime wiring is explicitly deferred to its later phase.
2. Implement OIDC/browser and device/service identity paths with issuer/state/nonce/PKCE/CSRF and one-time exchange rules.
3. Implement membership/delegation/service-account reads and six mutations: current authorization before replay, collection/target `If-Match`, scoped idempotency, exact 201/200 results and one-time service credential only on original create; keep provider-secret ingress isolated under operator wrapping-key refs.
4. Enforce strict decode/proxy/recovery/problem behavior; prove denials and replay/read/log/cache/receipt/error paths perform zero provider I/O and expose no credential.

Later wiring edits: internal/httpapi/server.gen.go

### M4 implementation order

Dependencies: TD12, TD09, TD10, TD01, TD18.

1. Implement F01 in PostgreSQL: canonical request/scope rows, post-reservation deployment/workspace pending counts, oldest immutable `accepted_at` age, immutable RunSpec/grant/run plus separate run/window/account budgets, audit/event, pending reservation and StartRun outbox atomically before 202; keep development/standard profiles and test-only limits distinct.
2. Register deterministic run/scheduled coordinator and actual activity interfaces; prove issue #14 through the actual route and a controlled durable execution-start transition, while provider capabilities and actual provider-worker integration remain unavailable until issue #16 and their later foundation.
3. Implement durable start/binding and command outboxes, projection repair and retained/absent-history branches; release pending admission only at the controlled start transition or known terminal pre-start failure, never Temporal acknowledgement/pre-start presence, and add exact replay, seven-day safe-response retention, unresolved-outcome non-expiry and namespace-lifetime guards.
4. Allocate per-run event sequence under the run-row lock; exercise reply loss, PG rollback/unknown commit, Temporal outage, delayed controlled start, retention and PG/Temporal crash windows with controlled activities.
5. Wire current grant checks/account permits before any later provider activity; do not tie execution to login-session expiry.

Later wiring edits: cmd/pm-server/main.go, internal/httpapi/routes.go, internal/server/health.go, internal/server/lifecycle.go, internal/server/server.go

### M5 implementation order

Dependencies: TD08, TD03, TD04, TD17, TD25, TD26.

1. Implement strict PM profile/session settings, OS token storage and explicit workspace/member/service-account commands over generated Go client.
2. Implement run/task/event/artifact views and structured exit semantics; close viewing independently from durable run cancellation.
3. Generate browser client/types and isolated Playwright harness with actual cookie/origin/CSRF context.
4. Submit shared fixtures through CLI and browser to the same stack, compare authorization/idempotency/errors/RunSpecs and validate concurrent workspace selection.
5. Implement #19/#21 client recovery with at most three same-key client-to-API sends in 30 seconds, 250ms/1s backoff, server Retry-After only within the remaining client deadline, no stale-revision rebase, 410 resubmit or replacement key, independent of provider/run retry scheduling.

Later wiring edits: None beyond files first owned here.

### M6 implementation order

Dependencies: TD05, TD06, TD07, TD08, TD01, TD12.

1. Within the grouped Stage C candidate, dependency edges order implementation and proof rather than requiring one PR or merge per issue: first implement issue #22's shared compiler over admitted inputs, including pinned ref/dialect normalization, recursive reference closure, exact field/response fidelity, typed source requirements and bounded sparse semantic amendments with no silent coercion. Read-only official-source research may proceed independently but does not admit connector input and is not implementation or proof.
2. Then implement issue #23's shared catalog and command projection over #22 outputs: stable operation IDs, deterministic PM/provider flag aliases, safe CLI/browser descriptors and real catalog/CLI wiring.
3. Then implement issue #24's derived Atlas and conformance checks from the shared compiler/catalog declarations, rejecting missing consumers, version mismatches and broken traceability without creating another runtime authority.
4. Then implement issue #25's shared HTTP/pagination runtime and physical-attempt admission with controlled transport retry/redirect behavior; apply typed selection/filtering to unprojected records, preserve complete-default results and truthful actual-page-size/cap/unread-tail/continuation/count metadata, and reject malformed controls before physical I/O.
5. Then implement issue #26's common proof runner, independent oracle, currentness semantics and generated lane wrappers over #25 and the existing client watch path, before any connector-specific proof consumes them.
6. Only after issues #22 through #26 are implemented, issue #27 creates the admitted pinned connector inputs: write source.lock/ref/NOTICE evidence from the accepted `github/rest-api-description` commit/path/blob/SHA/length/license identity, capture the complete reference closure without copying the 12.9 MB source into canonical docs, and generate the exact `issues/list-for-repo` operation with all response fields/source entries, its runtime bundle, CLI/browser descriptors and all seven lane inventories.
7. Using the already-implemented #25 runtime and #26 common proof, run the exact GitHub direct-read case through client/API/Temporal/worker against the independent oracle: exercise mixed issue/pull-request pages, complete-default results, nested/presence/exact values, explicit shared typed selection/filtering, malformed-control zero-send, empty-filtered-page continuation, effective sent wire size, unread tails and truthful page/continuation/fetched/returned/emitted counts.

Later wiring edits: internal/activities/register.go, internal/app/admission.go, internal/app/cancel.go, internal/app/capabilities.go, internal/app/capabilities_test.go, internal/app/connections.go, internal/app/delegations.go, internal/app/lifecycle_test.go, internal/app/runs.go, internal/app/service_accounts.go, internal/app/workspaces.go, internal/httpapi/routes.go, internal/server/server.go

### M7 implementation order

Dependencies: TD09, TD10, TD13, TD14, TD27, TD12.

1. Add migration 0004 warehouse/schema state and implement #28-owned PostgreSQL connector foundations from official protocol/SQL/type/metadata authority, with separate customer identity and versioned source lock.
2. Create PostgreSQL lifecycle/semantics for metadata/TLS/read and the reusable bounded keyed/mapped-column transport only; do not activate mapped upsert, generic SQL or saved `CP-etl-*` plan routes in M7.
3. Implement S3 reservations/unique attempt upload/integrity verification, then PG manifest/checkpoint publication, reference-aware GC and restricted query child while preserving exact values/presence.
4. Wire foundation-level ETL page/ack/continuation and complete metadata snapshots; prove P_etl, independent connector-versus-infrastructure pools, type fidelity, cross-host rows and every object/PG publication crash window.

Later wiring edits: internal/activities/register.go, internal/app/capabilities.go, internal/httpapi/routes.go, internal/server/server.go, internal/sync/direct.go

### M8 implementation order

Dependencies: TD09, TD10, TD12, TD06, TD14.

1. Add migration 0005 profile/approval/entity/effect state and immutable workset/mapping revisions; move all saved ETL route ownership from M7 to M8/F33.
2. Implement preview and complete primary/secondary approval hashes plus request/result/readback capacity; reject PostgreSQL missing/ambiguous/duplicate keys and invalid mappings before I/O.
3. Serialize #34 updates to #28-owned PostgreSQL lifecycle/semantics; implement insert-absent, update-only-changed approved mapped columns, unchanged zero assignments, preservation of unmapped columns and COMMIT-unknown independent readback.
4. Register direct/ETL/reverse handlers, prove returned-ID and 28-member partial/unknown cases, then create the N13-PG composition artifact after #28–#34 without claiming full #41.

Later wiring edits: internal/activities/register.go, internal/app/capabilities.go, internal/httpapi/routes.go, internal/server/server.go

### M9 implementation order

Dependencies: TD12, TD09, TD10, TD18, TD19.

1. Add migration0006 typed flows/schedules/occurrence and mutation outbox; expose CRUD/revisions/control routes.
2. Implement low-level Temporal schedule conflict-token CAS with persistent exact-payload RequestId and stale tombstone defense.
3. Admit automatic occurrences in their effect-free existing workflow and preserve overlap across full execution; test calendar AND/gap/both folds/catchup.
4. Enable and test worker deployment versioning/replay/ContinueAsNew commands, explicit grant revocation and measured account fairness across replicas.

Later wiring edits: internal/activities/register.go, internal/app/account_permits.go, internal/app/capabilities.go, internal/app/revocation.go, internal/httpapi/routes.go, internal/server/server.go

### M10 implementation order

Dependencies: TD05, TD06, TD07, TD12.

1. Add migration0007 account metadata refresh bookkeeping and dynamic property form projection.
2. Run bounded complete provider discovery through admitted activities; retain partial candidates without active-snapshot replacement.
3. Compute content/field dependency drift and preserve immutable accepted plan bindings while rechecking current safety predicates.
4. Resolve or explicitly block Pylon nullability conflict, Gong POST/read form and HubSpot logical/read-only/unique field cases without provider Go branches.

Later wiring edits: internal/activities/register.go, internal/app/capabilities.go, internal/httpapi/routes.go, internal/server/server.go

### M11 implementation order

Dependencies: TD05, TD06, TD12, TD14.

1. Capture/attribute and compile remaining connector sources; generate exact lane inventories without widening the accepted GitHub or PostgreSQL source scopes.
2. Wire shared binary and sync-transport handlers only for supported shapes with real bytes, delivery, acknowledgement and checkpoint semantics.
3. Update the existing PostgreSQL N13-PG composition for the remaining #41 obligations; add the other minimal source-linked lifecycle/composition scenarios.
4. Run selected connector/lane scripts independently in retry/all/unimplemented modes; retain live/fake/blocked distinctions and do not use early N13-PG as V74/P_sync_transport/full-#41 evidence.

Later wiring edits: internal/activities/register.go, internal/app/capabilities.go, internal/httpapi/routes.go, internal/server/server.go

### M12 implementation order

Dependencies: TD24, TD27, TD28, TD01, TD11, TD12.

1. Build paired API/worker/query OCI components and client/full/backend archives from immutable locks; verify native target linkage and SBOM/signatures.
2. Implement explicit offline/managed install and owner-aware drain/upgrade/uninstall retention commands.
3. Use owner-supported coordinated PM/Temporal/filer/object/key backup at named cutoff and quarantined restore with new epoch.
4. Replay active-version histories and interrupt migration/upgrade/restore; measure initial24h/4h recovery targets on declared hardware.

Later wiring edits: None beyond files first owned here.

### M13 implementation order

Dependencies: TD12, TD14, TD20, TD21, TD22, TD23, TD26, TD28.

1. Run two API/worker hosts with no shared filesystem and scope/permit/replay fault fixtures.
2. Measure declared noisy-neighbor load envelope, query/process/object/PG/history bounds and dependency-outage backlog behavior.
3. Restore an interval with externally successful but locally missing effect; prove quarantine and no blind duplicate mutation.
4. Run coherent required suite plus current five-connector proof/status and native packaging gates; publish only measured outcomes and explicit blocked cells.

Later wiring edits: None beyond files first owned here.

One future milestone owner serializes shared OpenAPI, contract, schema and migration changes. After those stabilize, implementation workers may own disjoint groups such as CLI projection, one shared runtime foundation or connector declarations/scenarios; no overlapping file ownership. A second connector should be declaration-only when existing semantics suffice. PostgreSQL is an intentional native protocol milestone, so Gong/HubSpot/Pylon provide the later HTTP declaration-extension check rather than pretending SQL proves that property.

Each slice follows baseline→causal RED/fault→smallest shared implementation→focused tests→same-stack integration→generated/diff/source/handler audit→actual receipt→milestone gate. Generic foundation regression and exact connector proof are different evidence. Review coherent code changes once with follow-up fixes; do not require a new speculative review per operation. No implementation agents, issues or changes start from this planning task.

## 14. Validation, proof and release gates

**Canonical planned evidence matrix.** Retain all 84 prior causal checks with corrected owners/semantics and 30 remote checks. The 114 rows are future tests/lane proofs, not completed validation. Accepted F01/F02/F03/AUTH/PostgreSQL contracts are assigned to existing checks and packet-scoped acceptance rather than padded with new global IDs. A foundation pass proves only that behavior; live certification requires actual account-scoped observations.

| ID / phase | Exact file / symbol | Causal negative control and expected observation |
|---|---|---|
| V01 / M1 | internal/contract/schema_test.go:TestStrictContractShape | Reject duplicate, wrong-case, unknown or secret public fields and unsupported versions; accept valid shapes. Nullable/union/presence/allOf generator fixtures preserve semantics or fail explicitly. [UNEXECUTED] |
| V02 / M1 | internal/contract/value_test.go:TestPresencePrecisionAndMedia | Absent, null, false, zero, empty values, nested objects/arrays, exact large integers/identifiers and fixed decimals remain distinct through API/worker/result/client encoding; selection preserves chosen nested values without flattening/fabricated null, and incompatible media is refused. [UNEXECUTED] |
| V03 / M1 | internal/contract/digest_test.go:TestStableDigestAndIdentity | Key order does not change a canonical digest; changed semantics does. Raw source hashes are separate, and logical entity identity ignores attempt revisions. [UNEXECUTED] |
| V04 / M1 | internal/contract/state_test.go:TestAllowedTransitions | Timeout never turns an unknown effect into failed/cancelled. Preserve attempt receipts; task blocked_reason with uncertainty keeps aggregate unknown/reconciling. Reconciliation can become blocked only when all effects are known. [UNEXECUTED] |
| V05 / M1 | internal/api/protocol_test.go:TestWireVersionAndEnvelope | Reject unsupported protocol major/required features, malformed RFC 9457 errors, incomplete F01 status/code/retry DTOs and lossy ActionResult/control shapes; tolerate safe additive response fields. A 202/accepted response is not terminal success. [UNEXECUTED] |
| V06 / M2 | internal/store/migrate_test.go:TestMigrationRecovery | Interrupted migration or unsupported downgrade fails safely; reopening sees only a valid committed schema. [UNEXECUTED] |
| V07 / M2 | internal/store/scope_test.go:TestStructuralScopeIsolation | Equal IDs/names in different tenant/workspace/connection scopes cannot collide; composite keys, RLS USING/WITH CHECK and artifact authorization deny cross-resource reads/writes. [UNEXECUTED] |
| V08 / M2 | internal/store/artifact_test.go:TestArtifactCrashWindows | Upload partial/complete bytes then fail before/after PG reference commit. No published reference points to partial bytes; unique orphan attempts are retained until safe collection. [UNEXECUTED] |
| V09 / M4 | internal/store/admission_test.go:TestDurableAcceptedBefore202 | Crash after the atomic idempotency/RunSpec/grant/run/audit/event/outbox/pending-reservation commit but before 202, then resubmit the same authorized key: return the same run with no second reservation, dispatch or capacity check. Precommit refusal yields no 202; changed input is 409; unknown commit reconciles only the same key. Final-slot/count/age/concurrency boundaries use post-reservation `<=`, immutable PG-clock age `>=` and no age refusal for an empty pending set. [UNEXECUTED] |
| V10 / M2 | internal/store/lease_test.go:TestLeaseFenceAndClaimCAS | Two claims produce one owner; a stale worker cannot commit. Losing a fence never erases a possibly dispatched provider effect. [UNEXECUTED] |
| V11 / M2 | internal/store/event_test.go:TestAtomicStateReceiptOutbox | Injected failure cannot commit state without its receipt/audit/event; ordered history contains only committed transitions. [UNEXECUTED] |
| V12 / M4 | internal/app/lifecycle_test.go:TestAcceptedRunSurvivesClientExit | Two remote clients submit/watch; closing either, logout or login-session expiry preserves accepted work. API/worker death resumes through PG outbox and Temporal; explicit run-grant revocation separately stops new effects. [UNEXECUTED] |
| V13 / M3 | internal/config/server_test.go:TestTypedServerStartup | Resolve exactly one development (32 pending, 8/2m) or standard (256 pending, 32/10m) profile and finite positive overrides with workspace ≤ deployment caps; reject zero/negative/unknown/obsolete keys, default secrets, unsupported schema, incompatible dependency identity and impossible budgets. Replicas share one durable deployment budget; 3/2/30s is test-only and 1000/1h is never a default. API/CLI never forks PG/Temporal/object dependencies, and readiness distinguishes admission from dispatch degradation. [UNEXECUTED] |
| V14 / M3 | internal/auth/authorize_test.go:TestScopeAndInboundPolicy | Wrong tenant/workspace/connection/run/event/artifact is rejected; loopback is not exempt. Remote TLS and exact Origin/Host/CSRF policy are enforced. [UNEXECUTED] |
| V15 / M3 | internal/auth/sessions_test.go:TestDeviceBootstrapProofs | Bootstrap needs no existing PM session but remains bounded: wrong, expired, reused or unapproved proof never issues one; approved one-time proof succeeds. [UNEXECUTED] |
| V16 / M3 | internal/credentials/boundary_test.go:TestNoProviderSecretCrossesClientBoundary | Provider-secret canaries in vault, refresh, headers, response bodies and errors never enter CLI arguments, requests, caches, logs, catalog or proof. Safe server authentication still works. [UNEXECUTED] |
| V17 / M3 | internal/credentials/refresh_test.go:TestScopedRefreshLeaseAndRedaction | Concurrent refresh has one owner; account changes invalidate witnesses. Rotation failure leaks no secret and never weakens scope. [UNEXECUTED] |
| V18 / M5 | internal/api/session_test.go:TestPMTokenOSStorage | Store PM sessions only; missing OS store fails or uses explicitly selected memory-only mode. Precheck fixed-size token/key encoded command below4096 bytes before Set; oversize starts no subprocess. No plaintext fallback. [UNEXECUTED] |
| V19 / M5 | internal/cli/config_test.go:TestControlsPresenceAndPrecedence | Unset flags do not override environment/settings; explicit false survives. Reject duplicate, empty, unknown or wrong-type controls and conflicting JSON/output selections. Settings cannot supply provider credentials. [UNEXECUTED] |
| V20 / M6 | internal/cli/dynamic_test.go:TestSelectorTreeAndOfflineHelp | Selectors before/after -- and duplicate flags cannot select one account then invoke another. Offline help needs no execution config/secrets; use one fresh Cobra tree. [UNEXECUTED] |
| V21 / M6 | internal/compile/dialect_test.go:TestDialectAndReferenceBoundary | Normalize admitted Swagger/OpenAPI dialects with conversion provenance. Reject missing/unsafe references and unsupported shapes. Enforce YAML duplicate/tag/alias budgets, closed custom loader, serialization/media and explicit allOf handling. [UNEXECUTED] |
| V22 / M6 | internal/compile/bundle_test.go:TestDeterministicCompleteInventory | Every source operation appears in all seven inventories; required fields/status/media are not dropped. Exact bundle bytes reproduce from identical inputs. [UNEXECUTED] |
| V23 / M6 | internal/cli/tree_test.go:TestStableNamesFlagsHelp | Location conflicts and reserved PM aliases bind exactly; renames retain stable identity. Help/manual/completion project the same command tree. [UNEXECUTED] |
| V24 / M6 | internal/connectors/revision_test.go:TestAtomicRevisionAndCacheScope | Preserve pinned run meaning/invalidate affected plans; reject wrong server/principal/scope caches and GitHub identities that differ from accepted commit/path/pointer/blob/SHA/license or have incomplete reference closure. [UNEXECUTED] |
| V25 / M7 | internal/schema/predicates_test.go:TestTypedRequirementsAndAtlasGaps | Unknown metadata/runtime contracts cannot pass compatibility; invalid overlays never execute expressions. Deduplicate the same gap while unrelated cells continue. [UNEXECUTED] |
| V26 / M6 | internal/runtime/http_test.go:TestTypedRequestAndResponseVariants | Assert exact path/query/header/cookie/body/style/media/status; distinguish201/202/204/default responses. Missing required input causes zero provider I/O. [UNEXECUTED] |
| V27 / M6 | internal/runtime/egress_test.go:TestDestinationDNSRedirectAuthAudience | Reject metadata/private-address redirects and DNS rebinding; never forward auth across audiences. Explicitly approved private DB policy still works. [UNEXECUTED] |
| V28 / M6 | internal/runtime/retry_test.go:TestRetryRequiresKnownReplaySafety | A provider effect before timeout yields unknown without blind replay. Known no-effect or declared idempotent retries are bounded and respect Retry-After. [UNEXECUTED] |
| V29 / M6 | internal/runtime/pagination_test.go:TestReturnedCountsAndCompleteness | Mixed/filtered known larger source pages prove exact page count, actual wire/sent page size and navigation, source continuation, unread tail and fetched/returned/emitted counts. Provider total/completeness stays distinct; unsent size or zero emitted matches never implies exhaustion, and raw/PM navigation conflicts fail. [UNEXECUTED] |
| V30 / M4 | internal/api/events_test.go:TestReplayGapSnapshotAndTerminalRender | Per-run sequence allocated under same run-row lock as state/event TX. Delay or roll back first of two writers, commit second only after lock release, reconnect to another API replica: exact committed event replay with no skip, scoped snapshot/cursor on retention gap, bounded slow consumer. [UNEXECUTED] |
| V31 / M11 | internal/runtime/binary_test.go:TestStreamIntegrityAndBounds | Truncation, wrong hash, HTML redirect or oversized bytes cannot publish success; valid byte count/hash/media pass with bounded buffers. [UNEXECUTED] |
| V32 / M6 | internal/proof/oracle_test.go:TestFalseSuccessAndIndependentCases | HTTP 200, credential presence, count-only success, a six-field subset or the wrong record/effect cannot prove the action. Independently compare complete mixed issue/PR records, nested/presence/exact values, selection/filter output and destination readback; preserve independent passes plus owned cleanup diagnostics when another case fails. [UNEXECUTED] |
| V33 / M6 | internal/proof/currentness_test.go:TestNoStaleProofResurrection | A new relevant failure or invalidation prevents old-pass resurrection. Unchanged independent passes retain original timestamps/build/source provenance. [UNEXECUTED] |
| V34 / M7 | internal/runtime/sqltypes_test.go:TestLosslessParameterizedSQL | Quoted qualified identifiers/bound values resist injection and numeric/integer/decimal/time/null round-trip exactly. The reusable transport admits only bounded keyed/mapped-column plans needed by declared operations, never public DSN secrets or arbitrary SQL. [UNEXECUTED] |
| V35 / M7 | internal/runtime/sqltls_test.go:TestStrictSQLTLS | Wrong hostname/CA cannot cause TLS downgrade; the intended strict connection works. Permissions remain operation-specific. [UNEXECUTED] |
| V36 / M7 | internal/schema/snapshot_test.go:TestDynamicFieldDriftBeforeWrite | Relevant enum/read-only/type/account drift invalidates fields/plans; unchanged metadata stays usable. Partial discovery never replaces a complete snapshot. [UNEXECUTED] |
| V37 / M7 | internal/warehouse/query_test.go:TestQueryReturnsRowsAndRejectsWrites | Owned relation/saved-query parameters return exact rows/types. Raw SQL, file functions, external extensions, unknown identifiers and cross-scope paths fail before query. [UNEXECUTED] |
| V38 / M7 | internal/warehouse/publish_test.go:TestRowsBeforeCheckpoint | Kill worker before object completion, after verified object publication and during PG manifest/cursor TX. Cursor never advances ahead of immutable registered rows; second host reads exact rows without shared filesystem. [UNEXECUTED] |
| V39 / M7 | internal/sync/ack_test.go:TestPartialAckContiguousCursor | Lost, partial, out-of-order or stale acknowledgement cannot advance over a gap; only the committed exact prefix advances checkpoint. [UNEXECUTED] |
| V40 / M8 | internal/sync/approval_test.go:TestSealedWorksetAndRecurringCaps | Seal destination/account, stable key/verified constraint, writable mappings/values, relevant schema and complete effects/cardinality. Any mapping/row/schema/account change invalidates approval; missing/ambiguous/duplicate keys fail before I/O. Reserve per-run/window capacities atomically, and unknown effects retain capacity/budget. [UNEXECUTED] |
| V41 / M8 | internal/sync/identity_test.go:TestStableCreateMappingAcrossRevisions | A stable source key maps to one destination entity across worksets/nonidentity revisions: absent inserts once, approved changed mapped values update it, and exact/equal repeat creates nothing and performs zero assignments. Any key/account/generation reset requires an explicit approved migration preserving the old mapping and dispatch history for audit. [UNEXECUTED] |
| V42 / M8 | internal/sync/readback_test.go:TestExactRowsAndReadback | Capture returned IDs and independently read exact destination keys, typed mapped values and preseeded unmapped sentinels. Two source rows must produce exactly the two intended effects; counts/SQL exit zero or correct mapped values with altered unmapped fields fail. Unrelated rows and per-row failure/unknown remain exact and distinct. [UNEXECUTED] |
| V43 / M8 | internal/app/reconcile_test.go:TestCrashAfterRemoteAccept | For a generic provider, persist exact intent, allow possible remote acceptance, then crash before acknowledgement: restart reconciles through a declared occurrence witness or remains unknown, never blindly recreates or claims zero effect. The PostgreSQL case distinguishes confirmed rollback from possible COMMIT, keeps possible COMMIT unknown/charged, requires a declared sound witness of the exact intent, rejects desired rows alone as occurrence proof and preserves actual dispatch history. [UNEXECUTED] |
| V44 / M9 | internal/app/flows_test.go:TestExplicitDAGAndIndependentFailures | Reject cycles, missing or multiple producers. Preserve query rows as artifacts; a failed branch blocks only descendants while independent work completes. [UNEXECUTED] |
| V45 / M9 | internal/temporal/schedule_test.go:TestUniqueOccurrenceDSTMisfire | Temporal schedule DOM/DOW AND, UTC/IANA timezone, DST gap skip and BOTH fall folds; repeated admission produces one run per schedule/UTC instant. Explicit1h catchup and overlap SKIP cover complete run lifetime. [UNEXECUTED] |
| V46 / M9 | internal/app/admission_limits_test.go:TestAccountFairnessAndBackpressure | Two worker replicas and connection aliases share PG provider-account permits; explicitly enabled Temporal fairness and bounded units avoid starvation. Unknown dispatched work retains budget; queue limit alone cannot satisfy test. [UNEXECUTED] |
| V47 / M9 | internal/app/cancel_test.go:TestCancelGracefulDrainTruth | Cancel stops new claims. Known completed subsets stay partial; uncertain dispatched effects remain unknown/reconciling after drain. No implicit rollback. [UNEXECUTED] |
| V48 / D1 | internal/runtime/postgres_cdc_test.go:TestSnapshotWALHandoff | Deferred CDC: snapshot/LSN duplicates, gaps or retention loss produce exact deduped rows or explicit blockage. Polling cannot claim log-based CDC. [UNEXECUTED] |
| V49 / M11 | internal/sync/modes_test.go:TestIndependentSemanticAxes | Cursor, POST or full read does not imply CDC/upsert/delete. Independently admit matching keys, null/presence, history, ordering and delivery semantics. [UNEXECUTED] |
| V50 / M12 | internal/server/observability_test.go:TestSafeAuditAndMetricCardinality | No secrets/row values/unbounded IDs in metric labels. Trace/receipt correlation remains safe; audit integrity/currentness faults are visible and telemetry outage does not block commits. [UNEXECUTED] |
| V51 / M12 | tests/install/backup_test.go:TestConsistentBackupRestore | Quiesced coordinated PM/Temporal/filer/object/key backup restores into isolated quarantine; missing object/key or incomplete backup fails. Restored uncertain interval never blindly replays external writes. [UNEXECUTED] |
| V52 / M13 | tests/install/restore_epoch_test.go:TestRestoreEpochFencesOldWorkers | Restore/new deployment epoch prevents old workers committing or obtaining new dispatch grants. Previously sent effects remain unknown; operator releases only reconciled scope. [UNEXECUTED] |
| V53 / M5 | internal/cli/boundary_test.go:TestThinBinaryDependencyBoundary | Import/dependency checks keep server credentials, SQL/DuckDB/control store out of CLI. Client exit cannot stop the persistent server. [UNEXECUTED] |
| V54 / M13 | tests/install/deployment_test.go:TestOneStackDeploymentParity | Dev, one host, managed endpoints and multiple API/worker hosts run same PG/Temporal/object architecture. No local storage/queue mode; remote CLI needs no backend and no shared filesystem. [UNEXECUTED] |
| P_direct_read / M6 | internal/proof/runner.go:ExecuteCase | Through the actual PM client/API/worker and pinned `issues/list-for-repo`, independently assert exact provider records, repository/id/number, complete mixed issue/PR entries, nested/presence/exact values, typed selection/filter, malformed-control zero-send, exact page counts/wire size/next navigation, unread tail, continuation and truthful counts/completeness. [UNEXECUTED] |
| P_direct_write / M8 | internal/proof/runner.go:ExecuteCase | Actual declared write plus independent exact expected field/value readback using the returned ID. PostgreSQL extends the generic lane only with accepted keyed mapped insert-or-update, unmapped/unrelated-row preservation and rollback-versus-possible-COMMIT handling that requires an exact occurrence witness rather than desired rows alone. Original GitHub lane proof remains independently required. [UNEXECUTED] |
| P_etl / M7 | internal/proof/runner.go:ExecuteCase | Actual persisted warehouse rows, exact typed values, truthful selection/filter/pagination and interruption/checkpoint recovery. N13-PG later sources rows from an owned PostgreSQL relation through the same registered path, never a demo executor. [UNEXECUTED] |
| P_reverse_etl / M8 | internal/proof/runner.go:ExecuteCase | Saved mapping plus sealed key/constraint/cardinality/approval produces exact per-row provider effects and readback; PostgreSQL insert, changed-mapped update and equal repeat preserve stable identity/unmapped values. Failure and unknown remain distinct; possible COMMIT stays charged and is never blindly retried or resolved from desired rows alone. [UNEXECUTED] |
| P_binary_download / M11 | internal/proof/runner.go:ExecuteCase | Actualbytes/digest/length/contentmetadata; JSONURL is insufficient [UNEXECUTED] |
| P_binary_upload / M11 | internal/proof/runner.go:ExecuteCase | Observeduploadedbytes plus providerobject/mediaoutcome/retrievalwhenavailable [UNEXECUTED] |
| P_sync_transport / M11 | internal/proof/runner.go:ExecuteCase | Actual staged source → delivery → durable acknowledgement → contiguous cursor with lost acknowledgement/restart; early N13-PG is not this proof. [UNEXECUTED] |
| V55 / M6 | internal/compile/sensitivity_test.go:TestSecretOperationsNeverProjectToCLI | Spec containing API-key header, OAuth bearer, password body, create-token operation and token response: no public CLI credential flags/body route/raw output; safe ordinary business fields remain. Unknown sensitivity blocks operation until classified. [UNEXECUTED] |
| V56 / M0 | tools/dependency_policy_test.go:TestResolvedDependencyPolicy | Resolve both module graphs and external build-tool SBOMs; injected forbidden transitive dependency fails even if not imported by pm. Selected graph without forbidden families passes. GoReleaser2.18.1 is rejected from selected tools. [UNEXECUTED] |
| V57 / M7 | internal/warehouse/segments_test.go:TestBoundedSegmentsManifestAndGC | Bounded immutable object segments; compaction CAS prevents competing publication; query snapshots, plans, proofs, backups and reservations retain referenced objects. LIST never establishes dataset membership. [UNEXECUTED] |
| V65 / M6 | internal/compile/fidelity_test.go:TestFieldSourceFidelity | Raw source nullable/required/style facts disagree with normalized fields: preserve both and block the affected binding; Twilio historical City and current Pylon parent-account contradiction are negative fixtures. [UNEXECUTED] |
| V66 / M4 | internal/app/capabilities_test.go:TestRegisteredHandlerReachability | Remove actual worker handler wiring while preserving descriptor: compatibility blocks affected capability and no dispatch occurs. API admission and HTTP/SQL activity execution use one shared executable registry. [UNEXECUTED] |
| V67 / M6 | internal/runtime/limits_test.go:TestRecordLimitIsNotPageSize | Provider page size is not total record cap. Gong POST read has no invented wire `limit`; local truncation retains unread rows behind a scoped continuation, filtering cannot hide source continuation or claim EOF, and provider request parameters remain distinct from output filters. [UNEXECUTED] |
| V68 / M8 | internal/sync/settlement_test.go:TestMemberSettlement | 28 inputs, 27 correlated successes, one rejection; only retry the one when independently safe. A short uncorrelated array yields unknown members, never inferred success. [UNEXECUTED] |
| V69 / M8 | internal/sync/dispatch_test.go:TestEffectAndEntityIdentity | Two legitimate occurrences on one entity differ in effect keys; exact replay retains key and payload. A non-idempotent create remains unknown after dispatch loss. [UNEXECUTED] |
| V70 / M8 | internal/sync/capacity_test.go:TestPreDispatchCapacity | Action/readback/member/encoded-byte capacities are intersected before I/O; escaped locator expansion cannot make a successful dispatch unrecordable. [UNEXECUTED] |
| V71 / M8 | internal/app/action_approval_test.go:TestCompleteActionApproval | Primary update plus tombstone delete both appear in approval hash; hidden or changed secondary action gives zero provider calls. [UNEXECUTED] |
| V72 / M8 | internal/sync/absence_test.go:TestAlreadyAbsentAndEmptyPublication | Confirmed already-absent deletion settles unchanged; empty replacement has a durable generation witness; restart never repeats a completed publication. [UNEXECUTED] |
| V73 / M7 | internal/schema/drift_test.go:TestScopedCompleteSchema | Two accounts with same object name cannot share metadata; partial discovery never replaces a complete active snapshot; unrelated-field change does not invalidate unaffected profiles. [UNEXECUTED] |
| V74 / M11 | internal/runtime/admission_test.go:TestAllPhysicalCallsAdmitted | Every HTTP/binary request, retry and SQL Begin/COPY/commit checks current durable grant/policy/budgets/context; login-session expiry alone does not revoke a run grant. Early N13-PG evidence does not close this global gate. [UNEXECUTED] |
| V75 / M3 | tests/install/install_test.go:TestBundledAndStandaloneInstall | Full package and backend-only package explicitly launch identical PG/Temporal/object/API/worker stack. Client-only package contacts it remotely and never autostarts services; private dependency endpoints and role separation verified. [UNEXECUTED] |
| V76 / M3 | tests/install/ownership_test.go:TestPersistentSharedServer | Two clients and API replicas share PG authority; CLI exit preserves accepted run, no private autospawn or current-workspace switch. Wrong deployment identity rejected. [UNEXECUTED] |
| V77 / M12 | tests/install/upgrade_test.go:TestUpgradeAndRestore | Drain accepted work safely, capture coordinated backup, expand PG schema, replay pinned workflow histories and stage compatible API/worker images. Interrupted upgrade preserves data; incompatible downgrade refused. [UNEXECUTED] |
| V78 / M12 | tests/install/uninstall_test.go:TestUninstallPreservesData | Remove only selected owned package/stack registration; volumes, keys, backups and managed services remain. Stop/uninstall never purges or stops independently managed dependencies. [UNEXECUTED] |
| V79 / M3 | internal/api/version_test.go:TestNegotiatedCompatibility | Current and prior minor clients negotiate API1 and feature set; incompatible major or instance ID mismatches fail before submission. [UNEXECUTED] |
| V80 / M6 | tools/traceability_test.go:TestTraceabilityClosure | Reject dangling foundation or contract versions, ownerless files, missing handler consumers, stale generated artifacts, duplicate semantic mechanisms and implementation claims without exact evidence. [UNEXECUTED] |
| V81 / M8 | internal/sync/result_retention_test.go:TestPostEffectResultRetention | Provider succeeds then receipt/storage fails: preserve dispatch identity and sanitized outcome if persistable; otherwise durable pre-dispatch intent remains unknown; never fabricate failure of remote effect. [UNEXECUTED] |
| V82 / M12 | tests/install/release_test.go:TestReleasePairAndOfflineInstall | Verify signed component/image lock, SBOM, archive hashes and native target linkage. Offline full stack uses supplied verified OCI images and explicit operator runtime prerequisite; no hidden package download. [UNEXECUTED] |
| V83 / M7 | internal/store/checkpoint_identity_test.go:TestCompleteCheckpointIdentity | Continuation presence/kind/bytes, source generation, schema and partitions all participate in canonical equality; continuation-only change defeats stale CAS. [UNEXECUTED] |
| V84 / M6 | internal/compile/amendment_test.go:TestNoSecretOrPolicyOverlayEscape | Source-linked amendments cannot expand origins, credentials, arbitrary executable code or unsupported capability; source changes require explicit revision diff. [UNEXECUTED] |
| R01 / M0 | tests/stack/config_test.go:TestStrictStackConfiguration | Unknown keys, duplicate keys, unowned dependency lifecycle or absent secret refs fail before service launch; valid bundled and managed endpoint configs select identical architecture. [UNEXECUTED] |
| R02 / M0 | tests/stack/storage_qualification_test.go:TestSelectedStorageQualification | Seaweed postgres2 nested listing under bounded pool saturates without deadlock; two gateways, multipart interruption, checksum mismatch and cross-worker read pass exact S3 contract. Failure blocks selected service qualification. [UNEXECUTED] |
| R03 / M2 | internal/store/rls_test.go:TestRLSAndPoolCleanup | Missing scope, wrong workspace, pooled connection reuse, rollback/error/cancellation and cross-table insert all deny; runtime roles cannot BYPASSRLS/DDL/TRUNCATE. Correct scope succeeds. [UNEXECUTED] |
| R04 / M2 | internal/store/tenant_keys_test.go:TestCompositeTenantReferences | Equal resource IDs/idempotency keys in two tenants cannot cross-bind identity mutation, plan, approval, run, task, event, artifact or connection. Safe errors/replay metadata hide foreign existence; workspace deletion never recycles an immutable key namespace. [UNEXECUTED] |
| R05 / M2 | internal/store/account_permits_test.go:TestSharedAccountPermits | Concurrent aliases across replicas reserve no more than global configured capacity; PG loss stops new permits, unknown sends stay charged, stale expiry checked at physical send. [UNEXECUTED] |
| R06 / M3 | internal/httpapi/strict_test.go:TestGinStrictBoundary | Gin defaults are overridden: malformed/duplicate/trailing/unknown JSON, unsafe proxy headers, raw panic/error and unexpected origins fail safely; large exact numbers survive; request cancellation reaches standard context. [UNEXECUTED] |
| R07 / M3 | internal/auth/browser_test.go:TestCookieCSRFAndDeviceSessions | Browser same-origin HttpOnly session plus CSRF and exact origins; device flow one-time/expiry checks; bearer credentials not accepted from query strings; provider secret never enters public SDK. [UNEXECUTED] |
| R08 / M3 | internal/auth/service_test.go:TestServiceAndDelegationScope | Service-account/delegation grants cannot exceed explicit connection/operation/workspace/time ceilings, and actor/effective actor remain distinct and auditable. Across the six access POSTs, current auth precedes replay; missing/stale `If-Match` is 428/412 with zero effect; equal concurrent requests commit once; exact replay is 200 with original safe result/revision; one-time service credential appears only on original 201. [UNEXECUTED] |
| R09 / M4 | internal/temporal/outbox_test.go:TestBothStartHandoffWindows | Crash after PG commit/before start and after Temporal accept/before outbox ack; stable identity produces one logical execution; changed idempotent payload conflicts and202 never precedes commit. [UNEXECUTED] |
| R10 / M4 | internal/temporal/projection_test.go:TestClosedAbsentProjectionRepair | Open final-projection activity retry, closed retained-history repair, never-bound/no-dispatch NotFound and prior-bound/expired-history NotFound are separate branches; none infers effect success or blindly restarts uncertain writes. [UNEXECUTED] |
| R11 / M4 | internal/workflows/continue_test.go:TestCommandsAcrossContinueAsNew | Duplicate/lost approval or cancellation notifications, buffered signals and updates crossing rollover retain command dedupe and immutable deadline; no orphan waiter or repeated effect. [UNEXECUTED] |
| R12 / M4 | internal/workflows/payload_test.go:TestHistoryAndPayloadBounds | Secret/row/cursor/body canaries never enter workflow arguments/results/heartbeats/search/errors.64KiB payload, bounded fanout and early history rollover are enforced with safe refs only. [UNEXECUTED] |
| R13 / M4 | internal/activities/effect_crash_test.go:TestWorkerDeathAfterEffect | Provider accepts then activity/worker dies before PM ack; retry consults same dispatch/member identity and reconciles or stays unknown, without duplicate mutation. [UNEXECUTED] |
| R14 / M5 | tests/client-parity/parity_test.go:TestCLIAndBrowserEquivalentUseCases | The same authorized principal/workspace/typed input/idempotency key in generated Go CLI and browser yields the same endpoint, authorization, RunSpec, complete/default or selected ActionResult, typed filter/pagination, F01 error/retry, AUTH replay and eventual effect. No CLI-only parser/policy, replacement key, stale-revision rebase or public admin-secret route. [UNEXECUTED] |
| R15 / M5 | internal/cli/workspaces_test.go:TestConcurrentExplicitWorkspaces | Concurrent shell invocations use separate explicit workspaces with same profile/session; no mutation of current-workspace and no cache cross-talk. Missing required workspace fails locally and server also denies. [UNEXECUTED] |
| R16 / M7 | internal/objects/publication_test.go:TestUniqueAttemptPublication | Upload success/PG failure leaves orphan only; repeated upload uses new immutable key; lost PG reply reads receipt. Published digest/length corruption blocks read and cursor progress. [UNEXECUTED] |
| R17 / M7 | internal/objects/scope_gc_test.go:TestScopedArtifactRetention | Wrong workspace cannot stat/range/download; active run/manifest/backup/proof/reservation references prevent collection; leaked partial upload expires only after safe grace. [UNEXECUTED] |
| R18 / M7 | internal/warehouse/child_test.go:TestQueryProcessIsolation | Disallowed AST/file/extension/network requests and oversized memory/temp output cannot escape bounded child; child has no provider/storage secrets. Kill child preserves source manifest and effect state. [UNEXECUTED] |
| R19 / M9 | internal/temporal/schedule_cas_test.go:TestScheduleMutationCAS | Concurrent If-Match edits, lost reply, rebased payload and out-of-order tombstone delivery cannot resurrect an old revision. Describe token and exact dispatch RequestId distinguish retry from rebase. [UNEXECUTED] |
| R20 / M9 | internal/temporal/occurrence_test.go:TestScheduledAdmissionAndOverlap | Temporal starts effect-free scheduled workflow, first activity admits occurrence once then adopts existing execution. Denied grant sends no effects; duplicate trigger/revision collision does not create another run; overlap spans actual run. [UNEXECUTED] |
| R21 / M9 | internal/temporal/replay_test.go:TestDeploymentReplayAndRetirement | Histories at approval/retry/unknown/rollover replay under proposed version; old worker retained for pinned open runs; unsafe command change fails replay and rollout. [UNEXECUTED] |
| R22 / M9 | tests/stack/fairness_test.go:TestReplicaFairnessAndCooldown | Explicit matching fairness plus PG tenant/account admission sustains bounded progress for quiet workspace during noisy neighbor load; queue-specific limit cannot bypass shared provider cooldown. [UNEXECUTED] |
| R23 / M9 | internal/app/revocation_test.go:TestSessionExpiryVersusRunRevocation | Logout/short-lived session expiry leaves accepted grant usable. Explicit principal/run/delegation/membership/connection/approval revocation stops future sends; restricted read-only reconciliation handles already-dispatched unknowns without new write permission. [UNEXECUTED] |
| R24 / M12 | tests/install/backup_test.go:TestCoordinatedBackupContract | Backup records cutoff and unresolved effects, quiesces writers, captures PM/Temporal/filer/object/key owners and verifies manifest. Independent inconsistent snapshots fail eligibility. [UNEXECUTED] |
| R25 / M12 | tests/install/native_images_test.go:TestPinnedNativeImagesAndSchemas | Actual Linux amd64/arm64 images start selected PG16.15/Temporal1.31.2 schemas1.19/1.14 with distinct roles; DuckDB native query helper and paired release pass native smoke. Registry digest alone cannot satisfy. [UNEXECUTED] |
| R26 / M12 | tests/install/managed_test.go:TestManagedProvisioningOwnership | Same API/worker binaries work with qualified managed endpoints; launcher never initializes/stops/deletes unowned dependencies or exposes default public database ports. [UNEXECUTED] |
| R27 / M13 | tests/stack/replicas_test.go:TestReplicaScope | Two API and worker hosts with no shared filesystem preserve authorization, event replay, one effect identity and immediate registered object visibility. [UNEXECUTED] |
| R28 / M13 | tests/stack/load_test.go:TestBoundedLoad | Declared100 clients/10 workspaces/20 admitted jobs/2 analytic tasks fixture records p95 admission, memory, queue/history/PG/object growth; caps reject excess and no starvation; no promised throughput inferred. [UNEXECUTED] |
| R29 / M13 | tests/stack/restore_effects_test.go:TestRestoreEffectQuarantine | Restored state omits externally successful request: old worker fenced, outbound effects disabled and uncertain interval reconciled before operator release; no replay-created duplicate. [UNEXECUTED] |
| R30 / M13 | tests/stack/outages_test.go:TestDependencyOutages | PG outage rejects new acceptance; Temporal outage retains bounded outbox pending state then caps admission; storage outage stops publication without moving checkpoint; recovery drains same intent. [UNEXECUTED] |

Seven lane proofs have distinct outcomes. Direct read asserts exact records/body and pagination against known larger data. Direct write verifies a real effect plus exact readback via returned IDs. ETL queries actual published warehouse rows and checks restart/checkpoint behavior. Reverse ETL uses saved mapping, immutable row count/workset, preview/approval and exact per-row effects. Binary download proves bytes, length/digest/media and corruption handling, not a JSON URL. Binary upload proves observed uploaded bytes and resulting object/content, with retrieval when supported. `sync_transport` means staged source→delivery→durable member acknowledgement→contiguous source checkpoint, including lost acknowledgements and restart; it is not a synonym for CDC, webhook or every sync strategy.

A connector's common proof algorithm inventories its full compiled surface once, builds/reuses one paired component set, then plans all selected cases and their prerequisites. Cases are independent except for explicit DAG dependencies. Each command/request/response/assertion checkpoint is persisted even if another case fails. Setup binds actual returned IDs; cleanup remains armed for owned resources. A case's status comes from independent expected observations, never credential presence or exit0 alone. Record fake/live evidence distinctly; paid/admin/inaccessible cases may use contract-faithful fakes for local behavior but stay blocked/unproven for live certification.

`--mode retry` is the default: rerun failed/unproven/actually invalidated cases and preserve valid independent passes. `--mode all` physically reruns selected executable cases. `--mode unimplemented` inventories missing handlers/semantics and retries cells whose foundations became available; it cannot turn an unsupported cell into a successful command. One standard schema emits seven connector-owned reports backed by immutable case receipts. There is no batch CURRENT selector. Atomic report replacement follows complete validated receipt assembly; partial runs retain completed case checkpoints.

Currentness keys include source/operation/schema/assertion/handler/affected implementation closure, account scope, lane/behavior/case and executing component identity. Unrelated documentation does not invalidate execution. Unknown dependency closure conservatively invalidates the selected affected scope; it is not assumed safe. A newer relevant failure or invalidation prevents resurrection of an older pass. Reused evidence retains its original physical run time/component hashes and explicit reuse provenance; a new build does not acquire old certification counts as if freshly tested. Cached proof compatibility and new physical proof are separately displayed.

Keep meaningful generic tests in compiler/contract/CLI/storage/auth/runtime/schema/sync/objects/warehouse/workflow/proof packages. Fault fixtures are justified by causal risk: malformed source, invented nullability, secret leakage, redirects/DNS rebinding, streaming corruption, retry replay, partial writes, stale proof, scope collision, post-effect storage failure, missing handler and schedule/replica recovery. Do not add hundreds of identical per-provider `_test.go` wrappers. Minimal connector scenario JSON and generated lane scripts own provider-specific expectations.

CI uses pinned toolchains/dependency/image hashes, format/static/schema checks, generated-output parity, import/source-boundary checks and targeted affected tests. `go test -c` is compile-only and must not be reported as executed tests. Go build cache and unchanged unit-test results may be reused; fresh physical proof uses a fresh process/`-count=1` where applicable and records actual execution. `scripts/test-changed.sh` maps file groups to owning package/tests plus shared contract consumers; unknown changes expand the gate conservatively. Cold builds are reserved for toolchain/native/cache-sensitive changes and coherent release milestones, not every edit.

Use an isolated Playwright1.63.0/Node24 LTS test-tool package for actual browser cookie/origin/CSRF behavior over the same stack and generated SDK. Node fetch alone is insufficient browser evidence. [Official Playwright1.63.0 release](https://github.com/microsoft/playwright/releases/tag/v1.63.0). The minimal harness is not a full UI or duplicated connector test suite. Provider-live scripts are opt-in with server-owned test-account credentials; no normal CI fork receives provider secrets. The coherent initial release gate runs the required full suite, five-connector inventory and current scoped live/fake distinction, native image/install checks, replica/outage tests and a recovery drill. All remain UNEXECUTED here.


## 15. Security, operations, scale and recovery

**Proposed/Recommended.** Treat tenant/resource authorization, provider effect safety and infrastructure identity as separate boundaries. RLS protects scoped PM rows; application policy protects use cases; worker execution grants protect physical provider calls; private service credentials protect PG/Temporal/object infrastructure. A Temporal task queue or shared namespace is not a tenant security boundary. Use a small queue set by workload/isolation class and one namespace per deployment/trust environment, not one per user. Do not expose Temporal UI/admin APIs publicly as a substitute for PM administration.

Enforce resource budgets at admission and each bounded physical unit: tenant active runs, account request/concurrency/cooldowns, bytes, rows, response/member capacity, object reservations, worker slots, analytical memory/temp/time and event buffers. The selected Temporal matching fairness setting defaults off, so explicitly configure and test it. Worker activity rates are per process and task-queue rates are per queue; neither is a global provider-account limit. PG transactional permits span aliases/replicas/queues. If unavailable, return a next-eligible time and use a Temporal timer rather than occupying an activity slot waiting indefinitely. Unknown dispatched work is conservatively charged. Fences prevent stale local commits, not remote effects or arbitrary-pause timing. [SDK worker149–196](https://github.com/temporalio/sdk-go/blob/b7c242c6894df088a57a85b33d0586e908da8b93/internal/worker.go#L149), [fairness default](https://github.com/temporalio/temporal/blob/19a774302c613da9adc4436ab14278ccdca8e0a5/common/dynamicconfig/constants.go#L1419).

Provider origin/method/media and redirect audiences come from admitted source/connection policy. No credential forwarding across unapproved origins, metadata/private-network redirects or unrestricted DNS targets. Explicit private PostgreSQL connectivity can be admitted with its own network/TLS policy and must not silently downgrade strict TLS. All values/paths/identifiers are typed and bounded. Analytical children accept only typed queries over registered relations, disable extension/network access, receive no secrets and run with OS/container limits; DuckDB settings alone do not sandbox arbitrary SQL.

PostgreSQL outage rejects new durable acceptance. Temporal outage can leave already-accepted outbox work pending while PG is healthy, but new admission stops at a finite backlog/age cap. Object outage prevents publication and new transfers without advancing cursors. A missing key blocks credential resolution and sensitive admission without exposing ciphertext or weakening policy. Liveness means the process responds; readiness states which authoritative dependencies and admission paths are available. Telemetry loss cannot rewrite successful business state or block a required transactional receipt.

**Proposed evaluation budget, UNMEASURED.** Begin Linux qualification at8vCPU/16GiB RAM/100GiB SSD, with32GiB preferable for meaningful analytical concurrency. Reserve memory for PG, Temporal, object metadata/gateways, API/workers, OS and each4GiB analytical child; child concurrency must fit actual/cgroup capacity. Default one analytical child is conservative until measured. Exercise100 clients,10 workspaces,20 admitted jobs and up to two analytic tasks on a suitably sized fixture. Record p95 admission/dispatch latency, worker/child memory, queue fairness, PG connection waits/WAL/visibility growth, history rollover, object bytes and restore duration. These are qualification workloads, not advertised minimum sizing or throughput guarantees.

One-host Compose has a maintenance/outage domain, not HA. Scale workers independently, then API replicas as load/availability requires. Higher availability needs qualified PG failover, object replication/quorum, Temporal topology and operational ownership. Visibility-index churn and retained histories require disk/maintenance budgets. None of those changes creates a local/remote product fork.

**Initial recovery target, UNMEASURED:** RPO≤24h and RTO≤4h for a small deployment with daily coordinated tested backup and a preprovisioned restore host. A5min/60min tier requires measured WAL/object retention, more automation and operational budget; it is deferred. Back up PM application DB, both Temporal DBs, filer metadata, immutable referenced bytes, configuration/version provenance and recoverable encryption keys. Independent online snapshots are not automatically one consistent cross-system point.

The baseline backup quiesces new admission/schedules, drains or records unresolved activities/effects, stops writers at a named cutoff and uses each owner's supported backup tools. Produce an integrity manifest and preserve referenced object versions/keys. Restore into a quarantined deployment with outbound provider effects disabled and a new fencing epoch; invalidate old worker grants. Compare PM accepted intent/effect receipts/manifests with Temporal bindings and the uncertain backup interval before resuming. Even consistent PG/Temporal restore cannot erase writes already accepted by a provider. Idempotency/readback horizons may be insufficient; unresolved effects stay unknown until operator disposition.

Temporal upgrades follow supported adjacent-minor progression, schema sequencing and compatible worker rollout; they are not arbitrary image swaps. Retain old workers for pinned open executions, replay histories at waits/retry/unknown/rollover, and use tested ContinueAsNew migration. An irreversible schema cannot be downgraded in place. Interrupted upgrades preserve volumes/images/backups. Uninstall retains data and managed dependencies by default; purge is a separate explicit operation, never incidental cleanup. [Temporal upgrade guidance27–77](https://github.com/temporalio/documentation/blob/bc5c548f28d0bc0b72e31976e2865563f40a85fc/docs/production-deployment/self-hosted-guide/upgrade-server.mdx#L27).

## 16. Selective reuse and rollout

**Proposed/Recommended.** Transfer knowledge by default; transfer source data only after provenance/license/schema checks. Copy small code only after it has a clear dependency boundary, exact behavior and causal admission test. This research does not identify a foundational module whose wholesale copy is safer than a clean contract-sized implementation. Existing source locks/authentic provider captures may be imported as pinned DATA with attribution; generated runtime bundles are regenerated from them.

| Existing evidence/artifact | Disposition | New owner and admission |
|---|---|---|
|473/474 typed values, mode axes, approval/workset/effect identities | Adapt contracts; replace local ownership | contract/app/sync/store; exact value, partial-member and approval regressions |
| Old connector source locks/provider captures and notices | Selective data copy after licensing/hash/reference closure | connectors/<name>/source.lock.json and source capture references; compile fidelity gate |
| Old generated runtime catalogs, flags and implemented counts | Reference-only; regenerate | compile/connectors; complete source inventory and actual handler reachability |
| Historical polling/database/warehouse reports | Preserve semantic lessons, not old topology | runtime/schema/warehouse/sync; cursor, TLS, row and acknowledged-prefix tests |
| OpenCode API/SDK and durable event design | Adapt knowledge | public API/browser clients and PG event sequence; reject client-owned lifetime/auth shortcuts |
| Executor source/binding/refresh design | Adapt knowledge | captured sources, compiled operation bundle and atomic revision switch; reject flattened/lossy semantics and code execution |
| Old connector-specific Go wrappers | Omit wholesale | Minimal scenario JSON, seven scripts and one shared proof runner |
| Old receipts/review verdicts/batch proof authorities | Reference-only test-design evidence | Fresh component/scope/case proof; no certification inheritance |
| Old local SQLite/cron/host-file/service machinery | Replace | PG/Temporal/S3/operator stack with explicit migration/restore ownership |
| Home state, `.env`, private histories, agent operational ledgers and old CI policy | Omit | No target path or runtime dependency |

The exact path crosswalk is in `FILE-OWNERSHIP.json`; source hashes/ranges and adopt/adapt/reject/defer dispositions are in `SOURCE-LEDGER.json`. Provider citations and old research remain authoring provenance, not runtime gates. Licensing attribution is checked before any data/code transfer. Old work stays preserved; no bulk deletion or “cleanup” of another worker's changes is part of this plan.

After implementation authorization, use the existing clean `pm-cli` setup repository and create only the milestone's application files. Revalidate any changed baseline before work. Establish the full remote stack and identity/durable acceptance first, prove a GitHub read, add shared warehouse/PostgreSQL, then safe writes/reverse ETL and schedules before broad connector activation. Release only when exact selected cells are honestly classified. A second HTTP connector requiring only declarations is an extensibility proof; a genuinely missing shared semantic contract is implemented once and does not block unrelated cells.

## 17. Risks, deferred scope and falsifiers

**Proposed/Recommended.** The largest risks are semantic correctness and operations, not the number of command names. The early falsifiers are selected-stack compatibility, Seaweed PostgreSQL filer saturation, source/generator fidelity, tenant isolation, durable admission and provider ambiguity. A failed falsifier changes the affected design through a concrete evidence-backed decision; it does not authorize a hidden alternate backend or manufactured parity.

| Risk or unknown | Falsifier / expected response |
|---|---|
| Seaweed PostgreSQL filer pool/listing deadlock | M0 saturation/multipart/multi-gateway test fails; block storage qualification and replace/fix one backend contract across deployments |
| PG16.15/Temporal1.31.2/image/schema pair incompatibility | Real schema/init/restart/visibility test fails; correct selected pin/roles before asynchronous release |
| Gin/generated3.1 binding loses type or leaks errors | Causal strict/number/union/error fixtures fail; adapt validator/generator at API boundary, preserve public semantics |
| Provider response loss lacks safe idempotency/readback | Exact effect stays unknown; block automatic retry and affected live certification |
| Source-field contradiction or incomplete metadata | Affected field/operation remains blocked; preserve conflicting evidence, never silently coerce |
| Delayed/stale worker or restore can repeat external effects | Effect-ledger/generation/restore fault test fails; stop release until safe reconciliation boundary is proven |
| Browser/CLI diverge in permissions or execution | Shared Playwright/Go use-case fixture differs; repair public contract/adapters before UI parity claim |
| Noisy tenant/account exhausts shared resources | Declared multi-replica load starves another scope or exceeds bounds; tune/partition infrastructure and admission, then remeasure |
| Native analytic helper violates sandbox/resource assumptions | File/network/extension/secret or memory fault escapes limits; tighten process boundary before query release |
| Backup target cannot be met | Publish measured recovery envelope; operational owner chooses resources/retention before stricter commitment |

Deferred scope is explicit: full PM UI, Windows release, PostgreSQL log-based CDC, broader connector inventory beyond the initial five, advanced distributed query/catalog services, direct signed artifact transfers, and stricter HA/RPO/RTO tiers. Seven lane inventories and explicit unsupported/not-applicable/blocked cells are still required. A binary URL is not byte-lane support, polling is not CDC, a POST read is not a write, and one passing case is not complete provider parity.

## 18. Handoff and remaining decisions

**Handoff.** The engineer receives this standalone 18-section plan plus source/evidence inventory, 20 versioned foundations, 43 traceability requirements, complete file ownership/crosswalk, 76 API/client use-case rows, state/identity/contracts, 14 milestones, 114 planned validation rows, 21 architecture decisions and artifact-local schemas. The F01/F02/F03/AUTH/PostgreSQL product decisions are accepted contract inputs; all implementation and proof remain absent.

The settled boundaries are not approval questions: remote-first single stack, multiuser/workspaces, shared CLI/UI public API, server-owned execution/provider secrets, initial five, seven lanes, PM MIT and preserved old work. The remaining consequential decisions are **implementation start**, **operational owner/budget and deployment trust boundary**, and **which test accounts/tiers/effect scopes may be used for live proof**. The plan uses labeled small-deployment assumptions so optional choices do not stop research. No fixed delivery date, performance saving or live certification is promised.

A connector is done only when all source operations appear in all seven inventories with exact availability reasons, supported operations have typed executable handler closure, dynamic/account requirements are explicit, command/help/form projections agree, relevant generic regressions pass and connector-owned scripts produce current independent observations for the claimed scope. Paid/admin/inaccessible operations stay visibly blocked or unproven; they do not disappear from the denominator.

The initial release is done when the required milestones and coherent release gates pass through actual CLI/browser/API/PG/Temporal/worker/object components; all five connectors have truthful inventories and scoped evidence; accepted work survives client/API/worker loss; tenant/credential/effect boundaries hold; packaging/native targets, upgrade and quarantine restore are measured; and the operator can understand every unsupported or uncertain outcome. Nothing in this planning task starts that implementation.
