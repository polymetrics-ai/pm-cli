# Synchronized inactive projection: `docs/development/skills.md`

This file mirrors the intended skill-routing document for provenance and comparison. It is not an active instruction entrypoint, is not fresh-worker intake, and does not establish a landed BOOT baseline. Relative links in the projected body resolve from the target path named in `INSTRUCTION-TARGETS.json`.

# Focused skill routing

The repository contains one focused project router, [pm-go](../../.agents/skills/pm-go/SKILL.md), and uses installed technical references only when a task needs them. Do not add another project router, vendor a skill catalog, or treat skill availability as implementation authority. [Repository instructions](../../AGENTS.md), the accepted issue, and the canonical PM Server contracts own scope and behavior.

## Server-first authority

The current architecture is one Go/Gin/PostgreSQL/Temporal/shared-object application. CLI and browser are presentation/transport clients of the same API. PM Server owns authentication, authorization, provider configuration and credentials, admission, execution grants, schemas, runs, schedules, and authoritative state. Deterministic Temporal workflows orchestrate; admitted worker activities own every provider HTTP, SQL, binary, and metadata call. No community example may reintroduce a local engine, client credential resolver, product MCP, alternate scheduler, or fallback implementation.

## Select by changed behavior

| Work | Load when available | Focus or official fallback |
| --- | --- | --- |
| Any Go design, implementation, debugging, or review | `go-engineering`; only the relevant reference | Ownership, small interfaces, explicit types/errors/presence, bounded execution; [Go documentation](https://go.dev/doc/) |
| Cobra commands, flags, help, and completion | `cc-skills-golang:golang-cli`, `cc-skills-golang:golang-spf13-cobra` | Fresh trees, typed flags, injected I/O, thin API client; [Cobra guide](https://github.com/spf13/cobra/blob/main/site/content/user_guide.md) |
| Foundation tests or fault investigation | `cc-skills-golang:golang-testing` | Causal observable checks and isolated fixtures; [Go testing](https://go.dev/doc/tutorial/add-a-test) and [fuzzing](https://go.dev/doc/security/fuzz/) |
| Error, parsing, and input boundaries | `cc-skills-golang:golang-error-handling`, `cc-skills-golang:golang-safety` | Stable error identity, strict refusal, exact presence, resource bounds |
| Authentication, secrets, redirects, or transport | `cc-skills-golang:golang-security` | Scoped server/worker safeguards; [Go security](https://go.dev/doc/security/) |
| Cancellation or parallel execution | `cc-skills-golang:golang-context`; `golang-concurrency` only for concurrency changes | Ownership, cancellation, bounded workers, relevant race checks; [context](https://pkg.go.dev/context) |
| PostgreSQL, warehouse, or transactions | `cc-skills-golang:golang-database` | Parameterized transactions, RLS-aware PM storage, separate customer-connector identity, exact values; [PostgreSQL](https://www.postgresql.org/docs/current/) and [pgx](https://pkg.go.dev/github.com/jackc/pgx/v5) |
| Temporal workflow/activity behavior | No selected project skill; use primary docs | Determinism, activities, replay, durable handoff; [Temporal Go](https://docs.temporal.io/develop/go) and [testing](https://docs.temporal.io/develop/go/testing-suite) |
| Gin or public OpenAPI 3.1 adapters | No selected project skill; use primary docs | Strict adapter behavior and one public schema; [Gin](https://gin-gonic.com/en/docs/) and [OpenAPI 3.1](https://spec.openapis.org/oas/v3.1.0.html) |
| Dependencies, native query child, or CI | `cc-skills-golang:golang-dependency-management`; CI reference only for authorized CI work | Exact pins, confined native boundary, actual candidate checks; [Go modules](https://go.dev/ref/mod) |
| Reader-facing prose | `nbj-write-clearly` when available | Result first, stable terms, exact technical modality; [Google developer style](https://developers.google.com/style) |
| A future approved Viper evaluation | `cc-skills-golang:golang-spf13-viper` | Only after [decision 0001](../decisions/0001-cli-configuration.md)'s reconsideration condition is met |

Resolve names through the current host's skill catalog. Open the selected `SKILL.md`, then only the references needed for the change. Namespaces may render differently across hosts; match source and entrypoint rather than a contributor's home path. An optional skill's absence is not a blocker: use the linked primary documentation and record the fallback. Do not install, update, or globally configure tools unless the task explicitly owns that work.

Use CodeGraph first only when this checkout already has a usable index, following [developer setup](setup.md). Context7 or an agent-exposed CodeGraph MCP may assist development when already approved and available; neither becomes a PM product dependency or authorizes global MCP changes.

## PM overrides for generic guidance

- The [PM Go router](../../.agents/skills/pm-go/SKILL.md) is the sole focused router. Do not recursively invoke `golang-how-to` or create connector/effect/release routers.
- Keep server-first ownership, thin-client parity, five initial connectors, seven lane inventories, and the accepted scope through #34 plus only #41's bounded `pg-wh-pg` composition. Inventory is not provider parity.
- Generic database guidance cannot remove PostgreSQL RLS, migrations, explicit PM authorization, exact-value handling, or the separation between PM storage and a customer PostgreSQL connector.
- Generic retry or Temporal guidance cannot replay an ambiguous external write. Persist physical intent, preserve unknown outcomes, and require the declared occurrence witness for that exact intent.
- Preserve OpenAPI 3.1 as the public schema source; do not derive a competing contract from handler annotations or create a second browser backend.
- Follow decision 0001: Cobra plus strict typed presence-aware client/server configuration, no blanket environment binding, offline help, and no Viper initially.
- Do not adopt `@latest`, automatic tool installation, global configuration, provider accounts, hosted review jobs, auto-merge, broad test loops, provider-specific wrapper tests, or arbitrary coverage targets from examples.
- Choose focused behavioral checks from ownership, consumers, and risk. Broad suites and actual CI belong to the coherent integration boundary. Reversible prose changes do not need invented application tests.

## Review and delivery route

The single [engineering and review policy](../architecture/pm-server/policy/ENGINEERING-REVIEW.md) owns review and delivery rules. OMP is the sole coordinator and integration owner. Task-specific model/account routing is supplied by the operator and remains outside application documentation.

When independent review is authorized, use the installed upstream `code-review` skill unchanged. Before dispatch, [issue/spec lookup](../agents/issue-tracker.md) must supply the accepted comparison base, candidate head, complete bodies and accepted comments for every grouped issue, immutable context digests, and applicable standards. Do not create a second review policy or make a private controller/runbook a prerequisite.

## Provenance and copying policy

| Reference | Setup provenance | Transfer decision |
| --- | --- | --- |
| Local `go-engineering` | Installed skill and focused references were inspected during setup; no license was established for redistribution | Use in place when available. Do not copy it into this repository. |
| `samber/cc-skills-golang` | Inspected local checkout `466ea6dfd4aecb5c19caf29e7595e752c66c1a5d`, MIT | Use selected installed entries by name; no upstream payload is copied. |
| `daniel-p-green/nbj-write-clearly` | Inspected source commit `56e3a27f920bcd772f7b03fa476b5cfb9268332c`, with its MIT/CC BY attribution boundary | Use in place when available; no payload is copied. |
| Project `pm-go` | Original repository router | Maintain one portable local router. |
| Matt Pocock AI Hero `code-review` | Upstream source pinned by the shared review policy, MIT | Use the installed upstream skill unchanged when authorized; do not fork it into the project. |

If future work needs a vendored reference, its authorized task must inspect the exact source closure and preserve licenses, notices, source commit/path, target path, hashes, and modifications. This instruction cleanup performs no third-party copy and adds no provenance scaffold, skill payload, CI script, or application code.
