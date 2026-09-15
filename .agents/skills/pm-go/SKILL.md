---
name: pm-go
description: Route PM Go design, implementation, debugging, and review to the accepted server-first contracts and focused engineering references. Use for Go work; this router does not authorize implementation.
---

# PM Go task router

Read [repository instructions](../../../AGENTS.md), [skill routing](../../../docs/development/skills.md), [issue/spec lookup](../../../docs/agents/issue-tracker.md), and the current task's accepted design. Resolve these links from this skill directory. The task must identify the accepted issue and owned scope; never substitute a private path, old draft, or remembered architecture.

1. Identify the behavior to preserve, owning package or packet, immediate consumers, authorized files, and dependency order.
2. Distinguish accepted planning contracts from observed implementation. Treat every planned command, symbol, test, and capability as future until it exists and has been exercised.
3. Load `go-engineering` when available and only its relevant reference, then the focused `cc-skills-golang` entry named in the routing table. Use the listed official source when an optional skill is absent.
4. Use the existing CodeGraph first when this checkout is indexed, with its explicit project path. Use bounded source search and direct reads when it is absent or incomplete; graph output never selects or waives tests.
5. Trace the real composition and consumers. Plan the focused causal check before production edits, implement only authorized work, and record actual outcomes and remaining gaps.

This is the sole project Go router. Do not install or load a competing umbrella dispatcher, recursively load a catalog, invent a skill, copy unlicensed instructions, install dependencies, or spawn reviewers unless the task authorizes it.

## PM rules over community defaults

- Preserve the one server-first Go/Gin/PostgreSQL/Temporal/shared-object architecture. CLI and browser are thin clients of the same public API; no local queue, embedded engine, product MCP, second scheduler, or client-started backend exists.
- Keep Cobra in the CLI adapter and Gin in the HTTP adapter. Server application services own authoritative validation, authorization, admission, and capability composition. Pass `context.Context` and typed values beyond adapters; do not pass framework contexts into domain or workflow code.
- PM Server owns provider configuration, credentials, identity/authentication, durable grants, schemas, runs, schedules, and state. Server-issued PM sessions differ from provider credentials, which never enter clients.
- Keep Temporal workflow code deterministic. All provider HTTP, SQL, binary, and metadata I/O belongs in admitted worker activities. PostgreSQL intent/outbox commits before durable handoff; reconciliation owns the non-atomic PostgreSQL/Temporal boundary.
- Store shared business bytes as immutable objects with PostgreSQL manifests. Worker scratch is disposable, and DuckDB/native analytics stays in the restricted query child.
- Preserve exact source semantics, typed presence, identifiers and numbers. Do not add provider-name branches, silent coercion, generic HTTP/SQL escapes, proof-driven runtime gates, or a fallback capability when wiring is absent.
- Persist physical dispatch/member intent before sending. Standard retry advice does not authorize replay of a possibly committed provider write. Unknown remains explicit until a declared sound occurrence witness identifies the exact intent; desired state alone is insufficient.
- Preserve the initial five-connector/seven-lane inventory without treating it as parity. Current product sequencing continues through issues #29–#34 and only issue #41's bounded `pg-wh-pg` slice; broader #41 remains open.
- Follow [decision 0001](../../../docs/decisions/0001-cli-configuration.md): Cobra, strict typed presence-aware configuration, a thin client profile, server-owned authoritative configuration, and no Viper initially.
- Choose tests from observable behavior, ownership, consumers, and risk. Use relevant concurrency, fault, security, integration, or bounded fuzz checks only when the changed contract requires them. Broad suites belong at the coherent integration gate, not every edit; prose does not need an invented application RED.

Follow the single [engineering and review policy](../../../docs/architecture/pm-server/policy/ENGINEERING-REVIEW.md). OMP is the project coordinator and integration owner; the operator supplies task-specific model/account routing outside these application instructions. Use the unchanged upstream `code-review` skill only when review is authorized and the issue tracker envelope is complete. No skill can release an implementation pause, establish a landed BOOT baseline, or claim application evidence.
