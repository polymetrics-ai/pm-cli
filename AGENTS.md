# AGENTS.md

## Authority and status

PM (Polymetrics) is a remote-first Go application for declared connectors and reliable data movement. The `pm` CLI and future browser are thin clients of PM Server; they are not alternate execution engines.

Within this candidate, [the PM Server plan](docs/architecture/pm-server/architecture/PLAN.md), its adjacent contracts, and the assigned accepted issue are the current product source. Application code, PM commands, generated bundles, tests, provider effects, and certification remain unimplemented until an authorized issue creates and exercises them. A local candidate is not a landed BOOT baseline. Full BOOT still requires the approved issue #46 publication to land and BOOT-1 to verify that immutable revision.

Implement only the explicitly authorized issue in its owned files. Preserve other work, first ownership, later integration rights, issue/case identities, and unresolved obligations. OMP is the sole project coordinator and integration owner. Agent model/account routing is operator-owned outside application documentation; do not encode a route here or make another delivery controller, private runbook, or host setup a worker prerequisite.

## Start with authoritative context

1. Read [issue/spec lookup](docs/agents/issue-tracker.md), the complete assigned issue and accepted comments, and every accepted grouped-issue input supplied by the task.
2. Read the relevant canonical design and ownership records under `docs/architecture/pm-server/`, especially `architecture/PLAN.md` and `implementation/FILE-OWNERSHIP.json`.
3. Read [developer setup](docs/development/setup.md), [skill routing](docs/development/skills.md), and the sole local [PM Go router](.agents/skills/pm-go/SKILL.md) when doing Go work.
4. Follow the single [engineering and review policy](docs/architecture/pm-server/policy/ENGINEERING-REVIEW.md) for validation, review, and delivery.

Files under `docs/architecture/pm-server/instructions/` are synchronized, inactive projections of the actual entrypoints above. Do not load them as a second instruction tree. Historical issue drafts, private setup runbooks, controller records, transcripts, and operator paths are evidence, not fresh-worker instruction intake. Do not require them or copy them into the repository.

## Client, server, and worker boundary

- CLI and browser project the same versioned public API, submit explicitly scoped requests, and render catalog, result, and event data. They do not execute provider calls, ETL, reverse ETL, schedules, or durable jobs.
- PM Server owns identity and authentication, PM sessions, authorization, workspace and connection configuration, provider credentials and refresh, admission, durable execution grants, schemas, runs, schedules, audit, and authoritative state.
- Clients may retain only a scoped PM session through the qualified client mechanism. Raw provider credentials never enter client arguments, profiles, caches, browser storage, output, or logs.
- PostgreSQL owns application intent, authorization, idempotency, effect facts, metadata selection, artifact manifests, and checkpoints. PM application tables, Temporal persistence, object-store metadata, and a customer's PostgreSQL connector have distinct owners and identities.
- Temporal owns durable orchestration and schedule timing. Workflow code is deterministic and carries bounded references, not secrets or bulk data. All provider HTTP, SQL, binary, and metadata I/O runs in admitted worker activities.
- PostgreSQL acceptance/outbox state precedes durable handoff. Reconciliation handles the non-atomic PostgreSQL/Temporal boundary; neither a workflow retry nor a lease proves exactly-once provider effects.
- Immutable shared business bytes belong in the approved object-store contract and are committed through PostgreSQL manifests. Worker-local files are disposable scratch. DuckDB/native analytics remain confined to the bounded query child.
- Operator stack lifecycle is explicit. A client invocation never starts, substitutes, or owns a local backend. Development and deployment use the same server-first architecture.
- PM neither implements nor depends on MCP. Approved developer navigation or documentation tools are outside the product dependency graph.

## Contracts and fidelity

Use the accepted file and package ownership map; do not infer a fixed package count. Keep Cobra in the CLI adapter and Gin in the HTTP adapter. Framework contexts and types do not enter domain or workflow contracts. Server application services own authoritative validation, authorization, admission, and capability composition. Missing wiring is unavailable, never an in-memory or local-engine fallback.

Pinned source captures and source references own provider facts. The compiler must preserve operation identity, field location and encoding, requiredness, unions, arrays, timestamps/time zones, exact identifiers and numbers, and absent/null/false/zero/empty distinctions. Generated bundles and client/catalog projections derive from admitted source; do not hand-edit execution facts, add provider-name branches, or turn Atlas/proof/review records into runtime gates.

The initial connectors remain GitHub, PostgreSQL, Gong, Pylon, and HubSpot, each inventoried across `direct_read`, `direct_write`, `etl`, `reverse_etl`, `binary_download`, `binary_upload`, and `sync_transport`. Inventory is not implementation or certification. Preserve the authorized product sequence through issues #29–#34 and only issue #41's narrow `pg-wh-pg` composition after #28–#34; broader #41 and later connector scope remain open.

## Authorization and durable effects

Authorize tenant, workspace, connection, operation, run, artifact, event, and proof scope at every boundary. An explicit selector is not authorization. Recheck current grants before new physical effects and retain immutable accepted intent, actor attribution, revisions, budgets, and idempotency identity.

Keep secrets out of command arguments, public descriptors, workflow history, artifacts, logs, errors, and receipts. Resolve provider signers only inside an admitted worker activity after current grant checks. Enforce bounded I/O, cancellation, deadlines, redirect/origin policy, SQL/file restrictions, and resource limits.

Persist dispatch and member intent before sending. Retry only when the declared operation and observed attempt state make replay safe. Preserve an ambiguous or possibly committed outcome as unknown until a declared sound occurrence witness identifies that exact intent; desired-row readback alone is not such a witness. Never blind-retry an uncertain write. PostgreSQL mapped upsert must use a verified stable key, bind typed values, update only changed approved writable columns, make no assignment when unchanged, preserve unmapped fields, and expose no delete, DDL, shell, generic HTTP write, or unrestricted SQL escape.

## Navigation, implementation, and claims

If this checkout has `.codegraph/`, use the existing CodeGraph with this checkout's explicit path before broad source search. Inspect the returned source and callers; synchronize only when the task authorizes changing the local index. Do not create an index, install tools, change global MCP configuration, or start a watcher as a workaround. If the graph is absent or incomplete, use bounded repository search and direct reads. Graph reachability does not select or waive tests.

Before production edits, identify the behavior, owner, consumers, and focused causal validation. Use the task's authorized checks and record actual results. All PM command lines, package paths, test names, deployment actions, and validation counts in planning documents are future until the owning implementation creates and runs them. Missing tools, skipped checks, local documentation success, an AI review, or a generated descriptor is not application proof.

Review coherent dependency-ordered candidates rather than each todo. The issue tracker supplies the accepted base, complete grouped issue/comment inputs, and candidate identity; the shared policy owns independent review and delivery. Report exact changes, executed evidence, and remaining unproven scope without claiming CI, merge, BOOT completion, provider execution, or certification that did not occur.
