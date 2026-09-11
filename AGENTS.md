# AGENTS.md

## Project status and authority

PM CLI is a remote-first Go client and backend for declared connectors and reliable data movement.
This repository is at the preimplementation setup stage.
The architecture below is the current planning direction, not existing code.
Do not claim a package, command, test, connector, or certification exists before verifying it.
Start application implementation only when the current task authorizes it.
Preserve other contributors' uncommitted work; edit only the assigned scope.
Keep old repositories as references; do not import their working state or certification.
Follow the user's current scope and this repository's applicable instructions.
When Firstmate manages a task, use its assigned inbox, status, and handoff contract.
Other contributors need no Firstmate installation or home directory.

## Start with the relevant context

Read [developer setup](docs/development/setup.md) for actual tool availability and commands.
Read [skill routing](docs/development/skills.md) and load only the task's useful references.
Read [issue/spec lookup](docs/agents/issue-tracker.md) before retrieving review inputs.
Use the accepted issue and immutable inputs required by its scope; lookup does not authorize publication or implementation.
Use the local [PM Go router](.agents/skills/pm-go/SKILL.md) for Go work.
Use [connector contracts](.agents/skills/pm-connector-contracts/SKILL.md),
[durable effects](.agents/skills/pm-durable-effects/SKILL.md), or
[release and recovery](.agents/skills/pm-release-recovery/SKILL.md) for their specific boundaries.
These domain procedures complement the sole Go router; they do not define another architecture.
Read [CLI configuration decision](docs/decisions/0001-cli-configuration.md) before CLI/config work.
Check the current task and accepted specification before changing a shared contract.
The task must supply any accepted contract that is not yet in this checkout.
Do not require private home files or silently treat proposals as accepted requirements.
Unresolved admission, source/projection and context-import decisions remain with their owners.
Record the behavior to preserve, files owned, and focused validation before production edits.
Distinguish retained requirements, observed behavior, defects, and proposed changes.
Do not recreate old planning histories, review ledgers, or operational policy in this repo.

## CodeGraph navigation

Check whether this repository root has a `.codegraph/` directory.
When indexed, use CodeGraph before searching for or locating code.
Prefer the exposed `codegraph_explore` MCP tool with this checkout's explicit project path.
The CLI fallback is `codegraph explore "<symbols or question>" --path .` from this root.
Read its source and call flow; do not reread identical bodies through another tool.
Use `codegraph sync .` after edits or a checkout change before relying on graph relationships.
Use `codegraph status --json .` to inspect the project path, version, and index state.
Zero pending changes alone does not prove freshness; see the observed limitation in setup.
After ignore/extraction changes, use the explicit full index command documented in setup.
Never index a workspace parent, home directory, or another contributor's checkout.
Without an index or a working tool, use bounded `rg --files`, `rg`, and direct reads.
Do not install tools, mutate global MCP config, or start a background watcher to fix navigation.
An empty repository has no meaningful Go graph; do not add dummy application source.
Treat `affected` as an additional clue, never as the complete set of required tests.
It missed a known Go test in the setup fixture, including with an explicit Go filter.
Generated JSON, reflection, interface dispatch, cgo, and scripts need independent reasoning.
CodeGraph output cannot certify behavior or prove dependency closure for receipt reuse.

## Architecture and ownership

Preserve one remote-first Go/Gin/PostgreSQL/Temporal/shared-object architecture.
Development, paired distribution and standalone client/backend use the same stack.
Keep the CLI and future browser thin clients of the same public OpenAPI 3.1 contract.
Use explicit workspace and connection scope; never a mutable global current workspace.
Ordinary client calls never auto-start a backend; lifecycle is an explicit operator action.
Do not add a local queue, PM cron scheduler, product MCP or provider-specific Go wrappers.

Use the accepted file/ownership map when introducing packages; do not infer a fixed count.
Keep contract values and small consumer-owned ports free of framework dependencies.
CLI owns Cobra and generated-client composition; httpapi owns Gin and strict HTTP adaptation.
Shared application preflight owns authorization, admission and actual capability composition.
PostgreSQL owns PM intent, grants, effect facts, metadata selection and publication manifests.
Temporal owns orchestration and schedule timing, with explicit outbox/reconciliation boundaries.
Deterministic workflows orchestrate activities; admitted activities own all provider I/O.
API inline execution is limited to PM-owned metadata; admin ingress encrypts provider secrets.
Shared objects hold immutable business bytes; scratch files never establish durable truth.
Only the restricted query child links DuckDB/native analytics.
Compiler and proof are authoring/validation owners, never provider runtime or admission gates.
Keep PM storage separate from the customer PostgreSQL connector.
Do not pass Gin/Cobra types beyond their adapters; use context.Context and typed values.
Introduce a package only when ownership or a real dependency boundary requires it.

## Declaration and runtime boundary

Provider source captures and source references own provider facts and provenance.
Canonical connector JSON owns typed operations, fields, and their requiredness.
The normal generator/build path must create every command supported by shared capabilities.
Generate the runtime bundle; never hand-edit generated execution facts.
Runtime execution reads the generated bundle only.
Source citations, Atlas entries, proof, and review state are authoring inputs, not runtime gates.
Keep exactly one runtime bundle per connector and regenerate it from admitted inputs.
Make generation deterministic; reject duplicate keys, unknown keys, and ambiguous bindings.
Preserve absent, null, false, zero, empty, integer precision, and declared array behavior.
Do not drop a required field to make a command appear executable.
Add minimal source-linked scenario overlays for real resource prerequisites and effects.
Use actual returned resource identifiers in dependent steps; never invent lifecycle IDs.
Do not assume every operation has a create/read/update/delete lifecycle.
Keep unavailable semantics explicit in a shared Foundation Atlas gap.
Deduplicate a gap by reusable behavior and extend its declared seam once.
An unsupported shape must not suppress unrelated supported connector cells.

## CLI and configuration

Use one Cobra command tree for PM namespaces and generated connector commands.
Use a fresh tree per invocation/test, instance-owned state, `RunE`, and injected I/O.
Cobra and its flag types stay in `internal/cli`; shared preflight validates every caller.
Do not reintroduce a second hand-written provider argument parser.
The bounded selector pass selects safe client context; final parsing uses the same tree.
CLI-side metadata projections aid binding/help; server authorization and schema validation are authoritative.
Help, completion, bare namespaces, and generated manuals use that executable tree.
Bare namespaces render useful help and succeed; unknown actions return usage errors.
Help/version must work offline without secrets, provider access, or valid connection config.
Keep stdout for requested results and stderr for diagnostics; emit stable structured errors.
Use Cobra with explicit typed JSON configuration; Viper is deferred by decision 0001.
Resolve approved PM controls once: explicit flag, then named environment, file, default.
Track presence separately from zero values; never let an untouched flag default override a file.
Reject malformed, unknown, duplicate, or wrongly cased configuration keys.
Allow only explicitly named environment bindings, with documented empty-value behavior.
Do not read `.env`, search arbitrary working directories, or enable config hot reload.
Local profiles contain only server identity/URL, PM session handle and display preferences.
Provider configuration and credentials remain server-owned; workspace is explicit per invocation.
Pass immutable typed resolved values to foundations; do not read config getters during execution.
Use one reserved PM-control registry and deterministic aliases for conflicting provider flags.
Keep provider wire names exact; PM aliases must not rewrite provider semantics.
Provider parameters cannot come from PM settings or arbitrary environment-variable expansion.
Connection settings, credentials, source definitions, metadata, plans, and proof are separate objects.
Settings must never supply approval, credentials, provider body fields, or certification claims.

## Dynamic fields and account context

Static operation schemas describe the provider contract, not an account's custom fields.
Discover dynamic fields through declared metadata capabilities, never inferred sample-row guesses.
Bind snapshots to server/principal/workspace/connector/connection/account/permissions/resource/source scope.
Store immutable snapshot references and the sole selected identity under PostgreSQL authority.
Client caches are safe projections; do not add a local authoritative selector or global CURRENT.
Failed or partial discovery cannot replace the last complete valid snapshot.
Build typed dynamic flags from the selected snapshot and retain provider identifier spelling.
Requiredness, writability, types, enums, associations, and precision must survive normalization.
Help can use local snapshots and must explain when metadata is missing or stale.
Accepted runs pin immutable schema evidence.
Before new effects, revalidate used field constraints through the accepted freshness witness.
Changed relevant schema, connection identity, or permissions invalidate the saved plan/approval.
Do not silently remap fields or replay a write whose effect is unknown.

## Data movement and safety

Direct read returns one page with truthful records, completeness, and navigation metadata.
Pagination derives from the declared strategy and the page size actually sent on the wire.
Direct write validates source-backed effects through readback when available/required.
Otherwise use the declared independent observable assertion; exit zero alone is insufficient.
ETL persists actual rows through verified immutable shared objects and PostgreSQL manifests.
Direct read is not ETL. Publish only complete digest/length/generation-verified object attempts.
Advance exact acknowledged checkpoint prefixes with their manifests in one PG transaction.
Host-local WAL, scratch files and rename cannot provide shared durable publication.
Keep artifact and warehouse scope structural; reject unsafe or colliding identifiers.
DuckDB is confined to the restricted query child; qualify its cgo/C tooling there.
Native PostgreSQL is an initial connector, with shared SQL protocol/TLS/value handling.
Quote qualified identifiers safely, bind values, preserve exact types, and use transactions.
Do not expose arbitrary SQL write, shell execution, or generic HTTP write as a connector escape hatch.
Admission commits immutable intent, scoped idempotency, durable grant and outbox before HTTP 202.
Exact duplicates retain run identity; changed fingerprints conflict. No PG/Temporal atomicity is assumed.
Reverse ETL follows saved plan, preview, scoped approval, and execution with sealed row cardinality.
Changes to mappings, worksets, relevant config, or schema invalidate approvals.
Transport modes retain their actual delivery, deduplication, history, checkpoint, and cursor semantics.
Binary lanes validate streamed bytes, metadata, and observable upload/download effects.
Bound buffers and concurrency; propagate cancellation and deadlines; close owned resources.
Persist dispatch/member intent and outcome capacity before one physical send.
Retries require declared replay safety; unknown write outcomes stay explicit.
Neither fencing nor workflow success proves exactly-once provider writes.
Keep effect/member truth separate from cancel intent and Temporal observations.
Login-session expiry/logout or a client wait ending does not cancel an accepted durable grant.
Explicit revocation stops new effects; already-sent effects may require restricted read reconciliation.
Respect provider rate limits, account scope, permissions, and resource prerequisites.
Resolve provider signers only in admitted worker activities after grant checks.
Separate authenticated administrative secret ingress from ordinary CLI/browser public APIs.
Never print, commit, inspect for convenience, or copy real secret values into examples or receipts.
Strip authorization on unsafe redirects and sanitize command, request, response, and error evidence.

## Connector-owned proof

Initial connectors are GitHub, PostgreSQL, Gong, Pylon, and HubSpot.
The previously planned ten-connector destination remains later scope, not deleted functionality.
Every connector inventories all seven lanes explicitly:
`direct_read`, `direct_write`, `etl`, `reverse_etl`, `binary_download`, `binary_upload`, `sync_transport`.
Keep seven lane JSON files and seven thin connector scripts using one common runner.
No connector-specific Go wrappers that merely repeat shared tests or assert process exit zero.
Scenario cases belong to connector + operation + lane + behavior, never to a release batch.
Inventory the full surface, then compile/reuse one PM binary and plan dependent cases.
Capture actual PM client/API/worker hashes, query-helper hash if used, source/bundle/schema/handler revisions,
sanitized command/request/response, assertions, effects, scope and physical execution timestamps.
Preserve independent case results and cleanup diagnostics when another case fails.
Write immutable attempt receipts and seven derived reports atomically; avoid global selectors.
Default reruns select failed, unproven, and actually invalidated cases; support explicit all.
Never revive an older pass after a newer relevant failure for the same scope and evidence mode.
Reuse requires known compatible dependencies; unknown closure invalidates conservatively.
A reused pass retains its original execution time and receipt; it is not new physical proof.
Executable means shared preflight can bind and execute the declared capability.
Implemented requires all required exact-scope cases to pass through actual PM, fake or live.
Certified additionally requires real-provider validation for that exact scope and current inputs.
Foundation tests, source JSON, credentials, and synthetic examples never certify a connector.
Label contract-faithful fake evidence separately; do not require fake plus live for every success.
Keep unsupported, not applicable, blocked, failed, and unproven distinct with explicit reasons.
One passing command cannot stand for a lane, connector, or account tier.

## Focused engineering validation

Use meaningful shared behavioral regression tests, with causal RED then GREEN evidence for changes.
RED must fail for the intended defect; GREEN must observe the corrected behavior.
Generator, CLI, proof, storage, and oracle fault tests are shared foundation tests too.
Choose checks from changed ownership and known consumers, with graph output as supplemental input.
Do not test reversible prose/setup edits by building or running the application.
Format changed Go files; run targeted package tests and appropriate static checks after Go changes.
Use race checks for changed concurrency, bounded fuzzing for parser risks, and explicit fault cases.
Broaden checks only for dependency reach, new failures, or a coherent release milestone.
Keep normal Go build/test caches; do not flush caches as a ritual.
Use `-count=1` only where a fresh test execution is required, and record cached results honestly.
`go test -c` compiles a test binary; it does not run the tests.
Run the full suite at the agreed integration/release gate, not after every small change.
Live provider and database checks are opt-in and require explicit owned fixtures and cleanup.
Missing tools, absent credentials, skips, timeouts, and partial runs are not successful validation.
Do not invent passing counts or run old-repository tests to validate this new implementation.

## Dependencies, documentation, and delivery

Prefer the standard library when it meets the actual contract.
Pin the chosen Go patch, modules, native dependencies, and tools when their files are introduced.
Qualify paired/standalone compatibility, migrations, complete backups and quarantined restore
through the release/recovery contract; restoring storage cannot undo provider effects.
Verify uncertain APIs and versions against official sources; never install floating latest by habit.
No module, build targets, CI jobs, or app scaffolding is authorized by this setup document alone.
Use `nbj-write-clearly` for prose when available; preserve technical facts and modality.
Label future commands and synthetic examples explicitly; keep runnable setup instructions accurate.
Update help, command reference, and website projections with relevant surface changes when they exist.
Review coherent code changes; automated evidence validation is not a new per-operation review pipeline.
Report what changed, why, what ran, its outcome, and what remains unproven.
Do not promise speedups or completion dates without supporting measurements.
