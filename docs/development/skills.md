# Focused skill routing

Use the repository's original skills and selected installed references on demand. The accepted task and [repository instructions](../../AGENTS.md) define authority. Skill advice does not create product requirements, implementation permission or evidence that a tool/connector works.

## Project procedures

| Changed behavior | Entry point |
| --- | --- |
| Go design, implementation, debugging or review | [pm-go](../../.agents/skills/pm-go/SKILL.md), the sole project Go router |
| Connector source/value/schema fidelity, pagination, generated surfaces or seven-lane proof | [pm-connector-contracts](../../.agents/skills/pm-connector-contracts/SKILL.md) |
| Admission/outbox, Temporal handoff, duplicate delivery, grants, worksets, effects or checkpoints | [pm-durable-effects](../../.agents/skills/pm-durable-effects/SKILL.md) |
| Paired/standalone packaging, compatibility, migrations, shared-artifact backup or quarantine recovery | [pm-release-recovery](../../.agents/skills/pm-release-recovery/SKILL.md) |

The domain skills are bounded procedures, not additional Go routers. Load only relevant sections and obtain the accepted specification through the current task. Missing private planning files are not implicit prerequisites. Unresolved admission/source/projection/context-import decisions stay explicit until their owner records acceptance.

## Technical references

| Work | Installed reference when available | Official fallback |
| --- | --- | --- |
| Go ownership, values, errors and bounded execution | `go-engineering`; fundamentals, advanced, production or agentic-ETL reference only as needed | [Go documentation](https://go.dev/doc/) |
| Cobra command tree, flags, help and completion | `cc-skills-golang:golang-spf13-cobra` | [Cobra guide](https://github.com/spf13/cobra/blob/main/site/content/user_guide.md) |
| Causal tests, fault cases and parser fuzzing | `cc-skills-golang:golang-testing` | [Go tests](https://go.dev/doc/tutorial/add-a-test), [Go fuzzing](https://go.dev/doc/security/fuzz/) |
| Cancellation and bounded concurrency | `cc-skills-golang:golang-context`; `golang-concurrency` for actual concurrency changes | [context](https://pkg.go.dev/context) |
| Gin and public API/schema generation | No dedicated Gin skill selected; use official docs | [Gin](https://gin-gonic.com/en/docs/), [OpenAPI 3.1](https://spec.openapis.org/oas/v3.1.0.html), [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen) |
| PostgreSQL transactions, pools and transport | `cc-skills-golang:golang-database` | [PostgreSQL](https://www.postgresql.org/docs/current/), [pgx](https://pkg.go.dev/github.com/jackc/pgx/v5) |
| Temporal Go workflow/activity/replay behavior | No dedicated Temporal Go skill selected; use official docs | [Temporal Go](https://docs.temporal.io/develop/go), [testing](https://docs.temporal.io/develop/go/testing-suite) |
| Authentication, secrets and transport security | `cc-skills-golang:golang-security` | [Go security](https://go.dev/doc/security/) |
| Safe logs, metrics and traces | `cc-skills-golang:golang-observability` | [OpenTelemetry Go](https://opentelemetry.io/docs/languages/go/) |
| CI and tool/module pins | `cc-skills-golang:golang-continuous-integration` for authorized CI work | [GitHub Actions security](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions), [Go modules](https://go.dev/ref/mod) |
| Behavior examples and acceptance mapping | Standard Go tests by default; no BDD framework skill selected | [Example mapping](https://cucumber.io/docs/bdd/example-mapping/) |
| Reader-facing prose | `nbj-write-clearly` plus its guide | [Google developer style](https://developers.google.com/style) |
| Authorized independent Standards/Spec review | Exact Matt Pocock AI Hero `code-review` source in provenance | Accepted review specification; source absence cannot produce a claimed review pass |
| Authorized fixes and delivery validation | Installed `no-mistakes` under the task's delivery policy | Report missing delivery capability honestly; never invent a successful pipeline run |

Discover installed names through the current host's catalog; never hard-code a contributor's home path. A community namespace may be displayed differently across hosts: match source and entrypoint. Optional absence uses official documentation without installation. Context7 may retrieve public docs if already available; it does not justify global MCP changes and is not a product MCP integration.

## PM overrides

Keep the remote-first Go/Gin/PostgreSQL/Temporal/shared-object stack, thin CLI/browser parity, five connectors, seven lanes and deferred logical CDC. Community examples cannot replace accepted contracts.

Do not recursively load `golang-how-to`, copy a catalog, auto-install `@latest`, spawn agents without task authority, activate hosted AI-review jobs or auto-merge. Use exact pins and only the owning setup task's runner/check policy. A tool version check or empty CI job inventory is not validation.

The generic database skill excludes RLS and schema creation. PM retains explicit application authorization plus scoped RLS and reviewed migrations. The inspected Swagger skill centers handler annotations; it is not selected because PM's public OpenAPI 3.1 is authoritative. Do not infer a second schema from handlers or substitute Temporal Python tests for Go workflow and real-stack evidence.

Follow [decision 0001](../decisions/0001-cli-configuration.md): Cobra plus strict typed JSON, Viper deferred, fresh trees and offline help. Preserve typed presence and server-owned schema/configuration. Generic connection pooling/retry examples cannot override PM's physical provider-attempt and uncertain-effect rules.

Choose focused shared causal tests, relevant race/fuzz checks and coherent integration/release gates. Do not manufacture application tests for reversible setup prose or impose provider wrappers, blanket fake-plus-live pairing or arbitrary coverage targets. Observability recipes do not authorize analytics export or public debug endpoints.

## Issue and specification lookup

[PM issue/spec lookup](../agents/issue-tracker.md) supplies the repository-owned integration expected by the selected AI Hero review skill. It identifies the private repository, supported gh-axi reads and the accepted issue and scope-specific immutable input envelope. The authorized early skills/CI setup slice does not require full PM context import. Both independent review axes remain required. This integration adds no triage, issue-write or product-dispatch authority. Runtime discovery and accepted-spec readback still need qualification on the authorized candidate.

## Provenance and installation

[Provenance policy](skill-provenance.md) and the [machine-readable manifest](skill-provenance.json) record original skill membership/hashes, selected external references and their required closure, licenses/notices, immutable refs or local hashes, PM overrides and fallbacks. No third-party payload is copied. Keep working installations. Any future missing-source installation requires its authorized setup scope and a reviewed exact source/closure; no floating or whole-catalog installation.

Structural CI validates repository membership, hashes, references and manifest shape without external installed files, credentials, model login or network access. Actual fresh-worker discovery and independent review are separate development observations; neither is claimed merely because the files exist.
