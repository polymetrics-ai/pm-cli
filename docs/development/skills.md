# Focused skill routing

Use the repository's four original project skills and selected installed references on demand. The accepted task, [repository instructions](../../AGENTS.md), and [issue/spec tracker](../agents/issue-tracker.md) define authority. Skill advice does not create product requirements, implementation permission, or evidence that a tool, service, or connector works.

## Project procedures

| Changed behavior | Entry point |
| --- | --- |
| Go design, implementation, debugging, or review | [pm-go](../../.agents/skills/pm-go/SKILL.md), the sole project Go router |
| Connector source/value/schema fidelity, pagination, generated surfaces, or seven-lane proof | [pm-connector-contracts](../../.agents/skills/pm-connector-contracts/SKILL.md) |
| Admission/outbox, Temporal handoff, duplicate delivery, grants, worksets, effects, or checkpoints | [pm-durable-effects](../../.agents/skills/pm-durable-effects/SKILL.md) |
| Paired/standalone packaging, compatibility, migrations, shared-artifact backup, or quarantine recovery | [pm-release-recovery](../../.agents/skills/pm-release-recovery/SKILL.md) |

The three domain skills are bounded procedures, not additional Go routers. Start Go work with `pm-go`, then load only the procedure touched by the accepted scope. Missing private planning files are not implicit prerequisites. Canonical PM Server contracts and accepted issue amendments override community examples.

## Server-first authority

PM has one Go/Gin/PostgreSQL/Temporal/shared-object architecture. CLI and browser are presentation and transport clients of the same API. PM Server owns authentication, authorization, provider configuration and credentials, admission, execution grants, schemas, runs, schedules, and authoritative state. Deterministic Temporal workflows orchestrate; admitted worker activities own every provider HTTP, SQL, binary, and metadata call. No skill may reintroduce a local engine, client credential resolver, product MCP, alternate scheduler, client-started backend, or fallback implementation.

## Technical references

| Work | Installed reference when available | Official fallback |
| --- | --- | --- |
| Go ownership, values, errors, and bounded execution | `go-engineering`; only the relevant reference | [Go documentation](https://go.dev/doc/) |
| Cobra command tree, flags, help, and completion | `cc-skills-golang:golang-spf13-cobra` | [Cobra guide](https://github.com/spf13/cobra/blob/main/site/content/user_guide.md) |
| Causal tests, fault cases, and parser fuzzing | `cc-skills-golang:golang-testing` | [Go tests](https://go.dev/doc/tutorial/add-a-test), [Go fuzzing](https://go.dev/doc/security/fuzz/) |
| Error, parsing, and input boundaries | `cc-skills-golang:golang-error-handling`, `cc-skills-golang:golang-safety` | [Go documentation](https://go.dev/doc/) |
| Cancellation and bounded concurrency | `cc-skills-golang:golang-context`; `golang-concurrency` only for actual concurrency changes | [context](https://pkg.go.dev/context) |
| Authentication, secrets, redirects, or transport | `cc-skills-golang:golang-security` | [Go security](https://go.dev/doc/security/) |
| PostgreSQL transactions, pools, and transport | `cc-skills-golang:golang-database` | [PostgreSQL](https://www.postgresql.org/docs/current/), [pgx](https://pkg.go.dev/github.com/jackc/pgx/v5) |
| Temporal workflow, activity, and replay behavior | No selected project skill; use primary docs | [Temporal Go](https://docs.temporal.io/develop/go), [testing](https://docs.temporal.io/develop/go/testing-suite) |
| Gin and public OpenAPI 3.1 adapters | No selected project skill; use primary docs | [Gin](https://gin-gonic.com/en/docs/), [OpenAPI 3.1](https://spec.openapis.org/oas/v3.1.0.html), [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen) |
| Dependencies, native query child, or authorized CI | `cc-skills-golang:golang-dependency-management`; `golang-continuous-integration` only for owned CI work | [Go modules](https://go.dev/ref/mod), [GitHub Actions security](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions) |
| Safe logs, metrics, and traces | `cc-skills-golang:golang-observability` | [OpenTelemetry Go](https://opentelemetry.io/docs/languages/go/) |
| Reader-facing prose | `nbj-write-clearly` when available | [Google developer style](https://developers.google.com/style) |
| Authorized independent Standards/Spec review | Exact Matt Pocock AI Hero `code-review` source recorded in provenance | Accepted review specification; source availability alone is not a review pass |
| A future approved Viper evaluation | `cc-skills-golang:golang-spf13-viper` | Only after [decision 0001](../decisions/0001-cli-configuration.md)'s reconsideration condition is met |

Resolve installed names through the current host's catalog. Open the selected `SKILL.md`, then only the references needed for the change. Namespaces may render differently across hosts; match source and entrypoint rather than a contributor's home path. If an optional skill is absent, use the linked primary documentation and report that absence. Do not install, update, or globally configure tools unless the task explicitly owns that work.

CodeGraph or Context7 may assist navigation when already approved and available; neither is a PM product dependency. Follow [developer setup](setup.md), use the checkout's explicit path, and make no global MCP or watcher change as a workaround.

## PM overrides for generic guidance

- Do not recursively load `golang-how-to`, vendor a catalog, auto-install `@latest`, spawn agents without task authority, activate hosted review jobs, or auto-merge.
- Keep thin-client parity, five initial connectors, seven independent lane inventories, and accepted scope through issues #29–#34 plus only issue #41's bounded `pg-wh-pg` composition. Inventory is not provider parity.
- Generic database guidance cannot remove PostgreSQL RLS, reviewed migrations, explicit PM authorization, exact-value handling, or separation between PM storage and a customer PostgreSQL connector.
- Generic retry or Temporal guidance cannot replay an ambiguous external write. Persist physical intent, preserve unknown outcomes, and require the declared occurrence witness for that exact intent.
- Preserve OpenAPI 3.1 as the public schema source; do not derive a competing contract from handler annotations or create a second browser backend.
- Follow decision 0001: Cobra plus strict typed presence-aware client/server configuration, no blanket environment binding, offline help, and no Viper initially.
- Choose focused behavioral checks from ownership, consumers, and risk. Broad suites and actual CI belong to the coherent integration boundary. Reversible prose changes do not need invented application tests.
- Do not copy provider data, credentials, private paths, external skill payloads, or historical delivery-controller policy into the repository.

## Review and provenance

The single [engineering and review policy](../architecture/pm-server/policy/ENGINEERING-REVIEW.md) owns review and delivery. OMP is the sole coordinator and integration owner; task-specific model/account routing remains operator-owned outside application documentation. Before authorized review, the issue tracker must supply the pinned base, merge base, head, complete accepted bodies/comments for every grouped issue, immutable context digests, and applicable standards.

[Provenance policy](skill-provenance.md) and the [machine-readable manifest](skill-provenance.json) record exact project membership and current hashes, selected installed-no-copy source closure, licenses/notices, PM overrides, and official HTTPS fallbacks. No third-party payload is copied. File/hash checks establish structure only; fresh-worker discovery, independent review, application behavior, CI, and merge require their own actual evidence.
