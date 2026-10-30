# AGENTS.md

## Authority and status

PM (Polymetrics) is a remote-first Go application for declared connectors and reliable data movement. The `pm` CLI and future browser are thin clients of PM Server; they are not alternate execution engines.

The accepted PM Server context published through PR #47 is landed on `main` at `d8e84e1fc78561173feac8abfd64ce669f09f373`. BOOT-1 readback authenticated the approved original staging and the exact reviewed tree that landed; see the [context index](docs/architecture/pm-server/README.md) for its evidence boundary. [Developer setup](docs/development/setup.md) records bounded local Darwin foundation checks and fresh-worker discovery. Independent candidate review, genuine final-head CI, and required merge authorization remain separate obligations; local observations alone cannot establish full BOOT or a merge.

Only behavior present in source and actually exercised may be reported. A bounded build/toolchain foundation does not implement PM application services, client commands, generated bundles, connectors, provider effects, packaging, or certification.

Implement only the explicitly authorized issue in its owned files. Preserve other work, first ownership, later integration rights, issue/case identities, and unresolved obligations. The owner is the sole integration owner and the only person who merges. Agent model/account routing is operator-owned outside application documentation; do not encode a route here or make another delivery controller, private runbook, or host setup a worker prerequisite.

## Start with authoritative context

1. Read [issue/spec lookup](docs/agents/issue-tracker.md), the complete assigned issue and accepted comments, and every accepted grouped-issue input supplied by the task.
2. Read the short [architecture overview](docs/architecture/overview.md), [decision index](docs/architecture/decisions.md), and [domain glossary](docs/agents/domain.md), then the relevant canonical records under `docs/architecture/pm-server/`, especially `architecture/PLAN.md` and `implementation/FILE-OWNERSHIP.json`.
3. Read [developer setup](docs/development/setup.md), [testing](docs/development/testing.md), [release](docs/development/release.md), [skill routing](docs/development/skills.md), and [skill provenance](docs/development/skill-provenance.md).
4. For Go work, start with the sole local [PM Go router](.agents/skills/pm-go/SKILL.md). Add only the bounded procedure the behavior needs: [connector contracts](.agents/skills/pm-connector-contracts/SKILL.md), [durable effects](.agents/skills/pm-durable-effects/SKILL.md), or [release and recovery](.agents/skills/pm-release-recovery/SKILL.md).
5. Follow the single [engineering and review policy](docs/architecture/pm-server/policy/ENGINEERING-REVIEW.md) for validation, review, and delivery.
6. Before you write any prose, load the [plain-English skill](.agents/skills/asd-ste100/SKILL.md). See [Writing](#writing).

Files under `docs/architecture/pm-server/instructions/` are synchronized, inactive projections of their mapped actual entrypoints. Do not load them as a second instruction tree. Historical issue drafts, private setup runbooks, controller records, transcripts, and operator paths are evidence, not fresh-worker instruction intake. Do not require them or copy them into the repository.

## Writing

Write all prose in Simplified Technical English (ASD-STE100). Load the [plain-English skill](.agents/skills/asd-ste100/SKILL.md) before you write or change:

- documentation, `AGENTS.md` and skills;
- issue, pull request and review text;
- commit messages;
- user-visible help, error and log text.

Keep every fact, requirement strength (must, should, may), hedge and exact identifier. Do not add a cause, guarantee or action that the source does not state. Do not apply the skill to code, generated files or quoted provider text.

## Commits and pull requests

- Write every commit message in [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/) format: `type(scope): description`.
  - Types: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `build`, `ci`, `chore` and `revert`.
  - The scope is optional. Use the area that changed, for example `cli`, `server`, `worker`, `gong`, `postgres`, `ci` or `skills`.
  - Write the description in the imperative mood and in lower case. Use 72 characters or fewer and no final period.
  - Mark a breaking change with `!` after the type or scope, or with a `BREAKING CHANGE:` footer.
- Use the same format for the pull request title. A squash merge uses the title as the commit subject on `main`.
- Push only to your own branch. Do not push to `main`, do not force-push a shared branch, and do not merge. Only the owner merges, by squash.
- Agent branches are named `agent/<issue>-<slug>`. A push opens a draft pull request automatically.

## Hooks and CI

- Run `make hooks` once in each clone. The hooks run `gofmt`, `go vet`, the Go developer tests and the commit message check. Do not skip them: never use `--no-verify`.
- CI runs only the Gherkin acceptance suite in `tests/acceptance` (the required `bdd` check), on our own runners. Go tests are developer checks and never count as acceptance evidence. Gherkin scenarios that run the real binaries are the only proof.
- See [CI and hooks](docs/development/ci.md).

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

Review coherent dependency-ordered candidates rather than each todo. The issue tracker supplies the accepted base, complete grouped issue/comment inputs, and candidate identity; the shared policy owns independent review and delivery. Distinguish the landed PR #47 baseline and its CI from evidence for the current candidate. Report exact changes, executed evidence, and remaining unproven scope without claiming current-candidate CI, merge, full BOOT, provider execution, or certification that did not occur.
