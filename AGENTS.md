# AGENTS.md

## Project status and authority

PM CLI is a new Go CLI for declared connectors and reliable local data movement.
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
Use the local [PM Go router](.agents/skills/pm-go/SKILL.md) for Go work.
Read [CLI configuration decision](docs/decisions/0001-cli-configuration.md) before CLI/config work.
Check the current task and relevant design before changing a shared contract.
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

Keep three thin binary entry points: `cmd/pm`, `cmd/pmgen`, and `cmd/pmproof`.
The initial production design has eleven internal packages, with these owners:
`contract`: typed boundary data, schemas, enums, identifiers, and reserved controls.
`compile`: strict source validation, lowering, deterministic generation, and capability mapping.
`bundle`: the one embedded execution JSON reader and safe connector metadata listing.
`cli`: the command tree, typed argument binding, control configuration, help, and output.
`credentials`: secret ingress, isolated credential storage, leases, and redaction.
`runtime`: shared HTTP/SQL execution, auth, encoding, pagination, and observations.
`schema`: account metadata discovery coordination, normalization, snapshots, and effective schemas.
`warehouse`: owned local tables, WAL, materialization, and local queries.
`sync`: saved plans, worksets, approval, ETL, reverse ETL, and transport state.
`proof`: case planning, actual-PM execution, assertions, receipts, and reports.
`store`: confined paths, locking, durable reads/writes, and atomic publication.
Use small interfaces at consumers; CLI composes concrete implementations.
Foundation runtime code must not import CLI, compiler, or proof packages.
Keep shared authorization and metadata interfaces in `contract` to avoid package cycles.
Keep provider-name switches and one-Go-adapter-per-connector designs out of the foundations.
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
The bounded selector pass selects local state/connection metadata; final parsing uses the same tree.
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
Pass immutable typed resolved values to foundations; do not read config getters during execution.
Use one reserved PM-control registry and deterministic aliases for conflicting provider flags.
Keep provider wire names exact; PM aliases must not rewrite provider semantics.
Provider parameters cannot come from PM settings or arbitrary environment-variable expansion.
Connection settings, credentials, source definitions, metadata, plans, and proof are separate objects.
Settings must never supply approval, credentials, provider body fields, or certification claims.

## Dynamic fields and account context

Static operation schemas describe the provider contract, not an account's custom fields.
Discover dynamic fields through declared metadata capabilities, never inferred sample-row guesses.
Bind a snapshot to connector, connection, account, permissions, resource, and source revision.
Store immutable snapshots under the connection's owned state directory.
The connection's `config.json` owns the sole `schema_ref` selection.
Do not add a second metadata catalog, global CURRENT, or mirrored active selector.
Failed or partial discovery cannot replace the last complete valid snapshot.
Build typed dynamic flags from the selected snapshot and retain provider identifier spelling.
Requiredness, writability, types, enums, associations, and precision must survive normalization.
Help can use local snapshots and must explain when metadata is missing or stale.
Before a write, revalidate the used field constraints through the planned freshness witness.
Changed relevant schema, connection identity, or permissions invalidate the saved plan/approval.
Do not silently remap fields or replay a write whose effect is unknown.

## Data movement and safety

Direct read returns one page with truthful records, completeness, and navigation metadata.
Pagination derives from the declared strategy and the page size actually sent on the wire.
Direct write validates source-backed effects through readback when available/required.
Otherwise use the declared independent observable assertion; exit zero alone is insufficient.
ETL persists actual rows in an owned warehouse; direct read is not ETL.
Keep local JSONL WAL appendable and durable; materialize one atomic Parquet table file.
Namespace warehouse paths structurally by workspace, connector, and connection identity.
Reject unsafe path components and ambiguous legacy layouts; never normalize collisions away.
DuckDB is the planned embedded query/Parquet engine; its build requires cgo/C tooling.
Native PostgreSQL is an initial connector, with shared SQL protocol/TLS/value handling.
Quote qualified identifiers safely, bind values, preserve exact types, and use transactions.
Do not expose arbitrary SQL write, shell execution, or generic HTTP write as a connector escape hatch.
Reverse ETL follows saved plan, preview, scoped approval, and execution with sealed row cardinality.
Changes to mappings, worksets, relevant config, or schema invalidate approvals.
Transport modes retain their actual delivery, deduplication, history, checkpoint, and cursor semantics.
Binary lanes validate streamed bytes, metadata, and observable upload/download effects.
Bound buffers and concurrency; propagate cancellation and deadlines; close owned resources.
Retries require declared replay safety; unknown write outcomes stay explicit.
Respect provider rate limits, account scope, permissions, and resource prerequisites.
Resolve secrets only at the execution boundary from approved secret ingress or references.
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
Capture actual PM version/hash, sanitized command/request/response, assertions, effects, and timestamps.
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
Verify uncertain APIs and versions against official sources; never install floating latest by habit.
No module, build targets, CI jobs, or app scaffolding is authorized by this setup document alone.
Use `nbj-write-clearly` for prose when available; preserve technical facts and modality.
Label future commands and synthetic examples explicitly; keep runnable setup instructions accurate.
Update help, command reference, and website projections with relevant surface changes when they exist.
Review coherent code changes; automated evidence validation is not a new per-operation review pipeline.
Report what changed, why, what ran, its outcome, and what remains unproven.
Do not promise speedups or completion dates without supporting measurements.
