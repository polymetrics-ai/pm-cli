# Architecture decision index

This is a portable index, not a second decision log. The complete accepted rationale, alternatives, evidence, and revisit triggers remain in the canonical [PM Server ADRs](pm-server/architecture/ADRS.md), [technology selections](pm-server/architecture/TECHNOLOGY.json), and [architecture delta](pm-server/architecture/ARCHITECTURE-DELTA.json). A selected planning decision is not implementation evidence.

## Core decisions

| Decision | Selected boundary | Canonical record |
| --- | --- | --- |
| One remote stack | One Go/Gin/PostgreSQL/Temporal/shared-object architecture in development and deployment; no embedded/local execution mode | [ADR475-01](pm-server/architecture/ADRS.md#adr475-01--one-remote-stack) |
| HTTP and CLI adapters | Gin owns strict HTTP adaptation; Cobra owns the thin CLI tree. Framework types do not cross into domain or workflow contracts | [ADR475-02](pm-server/architecture/ADRS.md#adr475-02--gin-adapter), [configuration ADR](../decisions/0001-cli-configuration.md) |
| Application truth | PostgreSQL owns accepted intent, authorization, idempotency, effects, manifests, and safe event history | [ADR475-03](pm-server/architecture/ADRS.md#adr475-03--pg-application-truth) |
| Tenant authority | Explicit tenant/workspace/resource scope plus transaction-scoped RLS; no global current workspace | [ADR475-04](pm-server/architecture/ADRS.md#adr475-04--tenant-authority) |
| Durable execution authority | Accepted execution grants outlive login sessions and client waits; explicit revocation stops new effects at the defined boundary | [ADR475-05](pm-server/architecture/ADRS.md#adr475-05--durable-grant) |
| Provider I/O | All provider HTTP, SQL, binary, and metadata calls occur in admitted worker activities after current grant checks | [ADR475-06](pm-server/architecture/ADRS.md#adr475-06--provider-io-ownership) |
| Source fidelity | Pinned source/reference closure and exact typed semantics precede generated bundles; source, descriptors, and proof are not runtime authority | [ADR475-07](pm-server/architecture/ADRS.md#adr475-07--source-fidelity), [ADR475-20](pm-server/architecture/ADRS.md#adr475-20--connector-source-authority) |
| Executable capability | One actual handler composition determines availability; missing wiring is unavailable, never a fallback engine | [ADR475-08](pm-server/architecture/ADRS.md#adr475-08--actual-capability-registry) |
| External effects | Entity, dispatch, attempt, and member outcome identities remain distinct; ambiguous writes stay unknown without a sound occurrence witness | [ADR475-09](pm-server/architecture/ADRS.md#adr475-09--member-effect-truth), [ADR475-21](pm-server/architecture/ADRS.md#adr475-21--bounded-postgresql-mapped-upsert-and-staged-composition) |
| Schedules | Temporal owns calendar execution; an effect-free occurrence first admits the durable PM run; there is no PM cron loop | [ADR475-10](pm-server/architecture/ADRS.md#adr475-10--temporal-schedules) |
| Shared bytes and analytics | Immutable shared objects plus PostgreSQL manifests own business bytes; DuckDB/native analytics is confined to a restricted child | [ADR475-11](pm-server/architecture/ADRS.md#adr475-11--immutable-shared-bytes), [ADR475-12](pm-server/architecture/ADRS.md#adr475-12--analytical-child) |
| Client parity | CLI and browser derive from one public OpenAPI 3.1 contract and share server-side authorization and behavior | [ADR475-13](pm-server/architecture/ADRS.md#adr475-13--public-client-parity) |
| Durable events | PostgreSQL transactionally owns safe sequence/history; live notification loss cannot erase replayable committed events | [ADR475-14](pm-server/architecture/ADRS.md#adr475-14--durable-events) |
| Lifecycle and recovery | Operators explicitly own stack lifecycle, coordinated backup, compatibility, and quarantined restore; storage restore does not undo provider effects | [ADR475-15](pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery) |

## Applying a decision

Start with the accepted issue and its owned files, then read the linked canonical record and [file ownership map](pm-server/implementation/FILE-OWNERSHIP.json). Preserve first-owner and later-update boundaries. If an implementation requires changing a selected decision, do not silently fork it in code or another guide: identify the conflicting contract and return the decision to its named owner.

Use [testing guidance](../development/testing.md) for evidence boundaries and [release guidance](../development/release.md) for packaging and recovery qualification. Neither document adds another architecture or review policy.
