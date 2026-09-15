# Architecture overview

PM is one remote-first application for declared connectors and reliable data movement. The `pm` CLI and future browser are thin clients of the same versioned public API. They never become alternate execution engines. This guide is a portable map to the canonical PM Server contracts; it does not replace them or claim that planned components are implemented.

## Runtime shape

```text
pm CLI / browser
  -> public OpenAPI 3.1 -> Gin HTTP adapter -> server application services
  -> PostgreSQL accepted intent, authorization, outbox, effects, events, and manifests
  -> Temporal workflows and schedules -> admitted worker activities
  -> provider HTTP / PostgreSQL / binary / metadata I/O
  -> immutable shared objects, referenced by PostgreSQL manifests
```

A client may submit or observe work and then disconnect. Accepted durable work belongs to PM Server, not to the client process. Operator-managed stack lifecycle is explicit; an ordinary client invocation never starts PostgreSQL, Temporal, object storage, API, or workers.

## Ownership at each boundary

| Boundary | Owns | Must not own |
| --- | --- | --- |
| CLI and browser | Explicit workspace/connection selectors, typed request input, bounded wait and presentation controls | Provider credentials, authorization, durable state, direct provider I/O, backend lifecycle |
| Gin HTTP adapter | Strict wire adaptation, route-specific authentication transport, request context propagation | Domain policy, workflow logic, provider calls |
| Server application services | Authoritative validation, authorization, admission, capability composition, grants, plans, runs, and schedules | Hidden local fallbacks or proof-driven runtime gates |
| PostgreSQL | Application intent, scoped identity and authorization facts, idempotency, outbox, effects, safe events, schema selections, artifact manifests, and checkpoints | Temporal internal history, shared object bytes, customer PostgreSQL connector state |
| Temporal | Deterministic workflow/activity orchestration, timers, schedules, and workflow history | Provider credentials, bulk data, provider I/O, final business-effect truth |
| Worker activities | Admitted provider HTTP, SQL, binary, and metadata I/O; signer resolution after current grant checks | Unadmitted effects or client-visible secrets |
| Shared object storage | Immutable datasets and artifacts addressed through verified digest/length/generation | Application intent, authorization, or checkpoint authority |
| Restricted query child | Bounded analytics over admitted read-only files | Network access, provider/storage credentials, shared writable DuckDB state |

Framework types stop at adapters. Cobra stays in the CLI adapter and Gin stays in the HTTP adapter. Domain, application, and workflow contracts use ordinary typed values and `context.Context` where applicable.

## State and effect truth

PostgreSQL acceptance and outbox state precede durable handoff to Temporal. The systems do not share a transaction, so reconciliation must preserve disagreement explicitly. A workflow retry, lease, fence, process exit, or desired-state readback cannot prove whether an external write committed. Persist exact dispatch and member intent before sending; keep an ambiguous outcome unknown until the operation's declared sound occurrence witness identifies that exact intent.

Immutable business bytes are published to shared object storage and become authoritative only through a PostgreSQL manifest. Worker-local files are disposable scratch. An object upload that is not registered is an orphan, not published progress; a checkpoint never advances ahead of the exact acknowledged contiguous prefix and its manifest.

## Source to executable capability

Pinned provider sources and source-linked amendments own provider facts. The compiler must preserve operation identity, field location and encoding, requiredness, response variants, unions, arrays, timestamps, and absent/null/false/zero/empty distinctions. It emits immutable connector bundles and client/catalog projections. Runtime availability comes from an actual registered handler with a compatible foundation closure—not from source presence, a descriptor, Atlas entry, proof receipt, or review result.

The initial inventory covers GitHub, PostgreSQL, Gong, Pylon, and HubSpot across seven independent lanes: `direct_read`, `direct_write`, `etl`, `reverse_etl`, `binary_download`, `binary_upload`, and `sync_transport`. Inventory is not implementation, parity, or certification.

## Canonical navigation

- [Implementation plan](pm-server/architecture/PLAN.md) and [decision records](pm-server/architecture/ADRS.md)
- [Normative contracts](pm-server/architecture/CONTRACTS.json), [foundations](pm-server/architecture/FOUNDATIONS.json), and [foundation mappings](pm-server/architecture/FOUNDATION-MAPPINGS.json)
- [State ownership](pm-server/architecture/STATE-OWNERSHIP.json) and [client parity](pm-server/architecture/CLIENT-PARITY.json)
- [Technology selections](pm-server/architecture/TECHNOLOGY.json) and [architecture delta](pm-server/architecture/ARCHITECTURE-DELTA.json)
- [File ownership](pm-server/implementation/FILE-OWNERSHIP.json), [milestones](pm-server/implementation/MILESTONES.json), and [validation matrix](pm-server/implementation/VALIDATION-MATRIX.json)
- [Domain glossary](../agents/domain.md), [decision index](decisions.md), and [configuration ADR](../decisions/0001-cli-configuration.md)

When a summary and a canonical record differ, the accepted issue and canonical record control. A planned path, command, target, or test remains future until its owner creates and exercises it.
