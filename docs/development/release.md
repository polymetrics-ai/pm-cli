# Release and recovery guide

PM releases one architecture in three packaging arrangements: client-only `pm`, a backend-only API/worker/query stack, and a full distribution containing both. Development uses the same Go/Gin/PostgreSQL/Temporal/shared-object boundaries. Standalone packaging never means an embedded queue, local state engine, client-started backend, or alternate durability mode.

The canonical [implementation plan](../architecture/pm-server/architecture/PLAN.md), [milestones](../architecture/pm-server/implementation/MILESTONES.json), [technology selections](../architecture/pm-server/architecture/TECHNOLOGY.json), and accepted release issue own exact versions, files, checks, and supported targets. This short guide is navigation, not a release runbook or readiness claim.

## Package and compatibility boundary

A release identifies every client, API, worker, restricted query helper, public schema, generated bundle, handler registry, migration, container image, native archive, and tool input by immutable version and digest. The public compatibility route is `/v1/info`; compatible clients and servers must satisfy its protocol-major and required-feature contract. Matching marketing versions or a passing health endpoint is insufficient.

Only the restricted query helper may link DuckDB/cgo/native analytics. Client, API, and ordinary worker dependency graphs must remain free of that native linkage. Advertise an operating-system or architecture target only after the actual artifact, linker/runtime boundary, and selected native inputs pass on that target.

Preserve PM's MIT licensing choice and complete third-party notices for the resolved dependency and distributed-asset graph. Release assembly uses exact locks, checksums, SBOMs, scans, and signatures selected by the owning issue. A recorded source URL, module declaration, image index digest, or successful download is provenance—not a qualified release artifact.

## Operator lifecycle

An operator explicitly checks, configures, provisions, migrates, starts, drains, backs up, restores, and observes a named installation. Ordinary CLI and browser calls never perform those lifecycle actions. Bundled dependencies may be owned by that installation; managed PostgreSQL, Temporal, object storage, identity, or ingress remain independently owned and must not be initialized, stopped, migrated, deleted, or purged without their own authority.

PM application PostgreSQL, Temporal persistence and visibility, object-store metadata, shared object bytes, and customer PostgreSQL connector databases have separate owners and identities. PM migrations touch only PM application tables. Listening ports are not readiness: validate exact schema, authentication, service identity, worker registration, and object integrity through the accepted checks.

## Upgrade, backup, and restore

Use expand-compatible PM schema changes while supported old and new processes coexist. Follow the selected PostgreSQL and Temporal upgrade sequences, retain compatible versioned workers for open histories, and replay-test affected histories before promotion. An arbitrary Down migration or binary downgrade is not a rollback strategy.

A complete backup coordinates PM application PostgreSQL, Temporal persistence and visibility, object metadata and bytes, required key/configuration material, version identities, and retention references at a named cutoff. Restore into quarantine with new recovery authority: stop new admission, schedules, and effect egress as required; reject stale workers; verify object references; and reconcile the post-backup dispatch window before releasing writes. Storage restore cannot undo provider effects, and an unknown external write cannot be blindly replayed.

Initial RPO 24h and RTO 4h values are qualification targets until measured by the owning recovery drill. A database dump, copied volume, source-only fixture, or successful startup cannot claim backup completeness or recovery success.

## Release evidence and delivery

Follow [testing and evidence guidance](testing.md) and the single [engineering and review policy](../architecture/pm-server/policy/ENGINEERING-REVIEW.md). Preserve the accepted base, merge base, reviewed/final heads, exact artifact manifest, focused and same-stack results, independent Standards and Spec reports, genuine final-head CI, and explicit unresolved targets. OMP owns integration; merge requires separate approval.

The current bootstrap and bounded build/toolchain source do not implement or qualify packaging, services, migrations, native targets, upgrade, backup, restore, or provider behavior. Record only what the actual candidate exercises.
