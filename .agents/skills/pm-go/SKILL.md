---
name: pm-go
description: Route PM CLI Go design, implementation, debugging and review to the owning contract and focused engineering references. Use for Go work; domain skills add procedures without replacing this router or authorizing implementation.
license: MIT
---

# PM Go task router

Read [repository instructions](../../../AGENTS.md), [skill routing](../../../docs/development/skills.md), and the current task's accepted specification. Resolve these links from this skill directory. If a required behavior is unspecified or its accepted source is unavailable, record the affected gap before changing that behavior. Do not guess private reference paths.

1. Identify the behavior to preserve, owner, consumers and authorized files. Distinguish planning contracts from observed implementation.
2. Load installed `go-engineering` and only its relevant reference, then the focused samber entry in the routing table. Use official documentation when an optional reference is absent.
3. Add `pm-connector-contracts` for source/schema/lane fidelity, `pm-durable-effects` for durable admission/effects/checkpoints, or `pm-release-recovery` for packaging/compatibility/restore. Load only procedures touched by this change.
4. Use CodeGraph first when this checkout is indexed; follow the documented bounded fallback otherwise.
5. Trace the actual composition and consumers. Plan focused causal checks before production edits, implement only authorized work, and record actual outcomes and remaining gaps.

This is the sole project Go router. Do not load a competing `golang-how-to` dispatcher, recursively load a catalog, install tools automatically, or copy unlicensed local instructions. An installed skill is a reference, not evidence of PM capability.

## PM rules over community defaults

- Keep the remote-first Go/Gin/PostgreSQL/Temporal/shared-object stack and thin CLI/browser parity. No local queue, product MCP, provider-specific Go wrapper architecture or second scheduler.
- Keep Cobra in CLI, Gin in the HTTP adapter, and typed shared validation/authorization at the application boundary. Pass `context.Context` beyond adapters; durable execution grants outlive login sessions and client waits.
- Use small consumer-owned interfaces, explicit presence/types/errors and bounded I/O. Do not turn compiler/proof inputs into runtime gates.
- Use approved PostgreSQL schema/migrations and RLS even when a generic database skill excludes them. Quote identifiers and bind values; distinguish PM storage from the customer PostgreSQL connector.
- Standard Go networking advice does not authorize pooled or hidden retries of unsafe provider writes. Follow the admitted physical-attempt and reconciliation contract.
- Preserve OpenAPI 3.1 as the public schema source. Do not replace it with handler annotations, Swagger 2 output, or a second browser backend.
- Pin dependencies/tools when authorized. Do not adopt `@latest`, automatic audit agents, auto-merge, global configuration, provider accounts or mandatory hosted review services from examples.
- Follow decision 0001: Cobra plus strict typed JSON, Viper deferred, offline help, no blanket environment binding.
- Choose shared behavioral tests from ownership and risk. Use race checks for changed concurrency and bounded fuzzing for parser risks. Full suites belong to coherent integration/release gates, not every edit. Prose/setup checks do not run the application.
- Telemetry must be scoped, redacted and bounded. Generic tracing, analytics, profiling or dashboard recipes do not authorize new services, public debug endpoints or data export.

Use `nbj-write-clearly` for prose when available. Report changed behavior, focused evidence and unproven claims. Skill instructions cannot release an implementation pause or bypass issue-first delivery.
