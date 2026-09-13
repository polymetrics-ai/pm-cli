# Inactive application-instruction proposal

Intended target: AGENTS.md, only after separate activation approval. Relative project links in the proposed body are resolved at that target.

<!-- firstmate:maintained-by-project -->
# PM application instructions

## Scope and status

PM means Polymetrics. This repository owns the `pm` client, PM Server and worker implementation. It does not own an agent fleet. Implement the assigned, explicitly authorized issue in its isolated checkout; ordinary contributors do not need FirstMate or Herdr.

The remote-server design is **proposed until the context-publication issue records its approved digest and immutable revision**. The inspected baseline contains setup only. Do not describe planned files or tests as implemented. The canonical design is `docs/architecture/pm-server/architecture/PLAN.md`, with ADRS.md, CONTRACTS.json and the implementation records alongside it. The bootstrap issue must make these links real before dependent work is Ready.

Maintain this file as a concise application entry point when conventions change. Keep detailed policy in the linked authoritative documents. A same-repository CLAUDE.md, if needed, is only an `@AGENTS.md` pointer.

## Product boundary

- PMCLI and the future UI call the same authorized, versioned server use cases. Clients parse/project commands and schemas, submit requests and render results/events. They do not execute providers, ETL, reverse ETL or schedules.
- The server owns identity validation, session issuance/revocation, workspace/connection authorization, provider configuration and secrets, refresh, execution, durable state, schemas, runs and schedules. A client may retain a scoped PM session via a server-owned browser/device flow; it must never collect, persist, forward or print raw provider credentials.
- One remote-first Go/Gin/PostgreSQL/Temporal application model serves development and production. PostgreSQL application tables, Temporal-managed databases and a customer's PostgreSQL connector have separate identities and owners. PM does not write Temporal's internal tables.
- PM workers execute provider I/O in activities. Temporal workflow code is deterministic and carries scoped references, not secrets or bulk data. PostgreSQL intent/outbox commit precedes durable acceptance; reconciliation handles non-atomic handoff and unknown external effects.
- Shared immutable artifact bytes belong to approved object storage. DuckDB runs behind the bounded analytical child boundary. Worker-local files are disposable scratch, not shared truth.
- The full PM distribution includes compatible CLI and server components; server-only installation remains supported. Stack provisioning is deliberate operator work. A remote-client invocation never starts another backend.
- PM does not implement or depend on MCP. Explicitly approved development tools such as Context7 are outside the product dependency graph. PM's own code is MIT; retain required third-party notices.

## Dependency direction and foundations

Composition roots wire dependencies; framework request contexts never enter domain/workflow contracts. `internal/cli` depends on public contracts/client transport and presentation. `internal/httpapi` adapts Gin to server application services. Application admission and activities use registered foundations; CLI code cannot import execution, credentials, store or analytics. Workflows cannot perform HTTP/SQL I/O. Native analytical imports stay in their qualified child/module boundary. Follow FILE-OWNERSHIP.json and the assigned issue; serialize shared contract or migration changes.

Connector definitions carry pinned source facts and source-linked semantic amendments. The compiler normalizes them, checks typed requirements against the actual registered runtime implementations, and generates operation/command/case descriptors. Do not add connector-name branches, provider flags, routes, fallback policy or schema coercion to shared code. A native SQL/metadata adapter requires an explicit bounded contract and evidence. Foundation Atlas tables are generated design/impact views, not a second runtime or proof registry.

Preserve field location, encoding, requiredness, exact numbers, declared unions, arrays, keys, timestamps/time zones and absent/null/empty/default distinctions. Trace disagreements to retained source facts before adding runtime behavior. Reject or explicitly approve lossy mappings. Dynamic schemas are workspace/connection/account-scoped, content-fingerprinted, complete-or-explicitly-incomplete snapshots pinned into plans/runs. Drift invalidates affected bindings.

Initial connectors: GitHub, PostgreSQL, Gong, Pylon and HubSpot. Canonical lanes: `direct_read`, `direct_write`, `etl`, `reverse_etl`, `binary_download`, `binary_upload`, `sync_transport`. A source HTTP operation is not automatically a sync profile, idempotent write, complete snapshot, CDC feed or binary transfer. Preserve independent sync dimensions and explicit unsupported/unknown/conditional reasons.

## Security and durable correctness

Authorize tenant/workspace/connection scope on every API, activity, query, cache, artifact, event and proof path. Agents use scoped identities; an explicit workspace selector is not authorization. Avoid mutable global client workspace selection for concurrent invocations. Use revisions for edits and immutable accepted specifications. Recheck relevant grants before new effect units; preserve accepted-run audit attribution and revocation semantics.

Keep secrets out of command arguments, public descriptors, workflow histories, logs and receipts. Resolve/refresh only in the server execution boundary. Enforce SSRF/redirect/origin policy, SQL/file/code restrictions and resource limits. Use scoped idempotency keys, durable member outcomes, safe retry classes, provider effect reconciliation and fencing. Workflow retries or local leases do not prove exactly-once effects. Preserve unknown outcomes rather than replay unsafe writes.

Publish artifact bytes durably before committing their manifest/cursor. Advance an ETL checkpoint only after durable publication. Reverse ETL records exact workset identities and per-member success/rejection/unknown outcomes, with action-specific readback when required. Cancellation, watch interruption and server shutdown have distinct meanings. Apply expand-compatible migrations, explicit rollback limits, backup and restore gates from the issue.

## Source navigation and skills

If `.codegraph/` exists, use the existing CodeGraph before searching or reading implementation code. Do not create an index without approval. A missing graph result is not a negative test result. Follow `.agents/skills/pm-go/SKILL.md` and `docs/development/skills.md` for on-demand Go skills, with official documentation fallbacks when unavailable. Do not import unrelated global configuration.

## Implementation, verification and delivery

Follow the single shared [engineering/review policy](docs/architecture/pm-server/policy/ENGINEERING-REVIEW.md). This link resolves from the proposed root AGENTS.md target, not this inactive staging location. The documentation-publication exception uses its reviewed local manifest/diff until a real candidate exists. Dependent implementation requires approved committed context. No archive comments.
