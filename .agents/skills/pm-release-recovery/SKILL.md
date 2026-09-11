---
name: pm-release-recovery
description: Preserve PM packaging, paired and standalone compatibility, same-stack deployment, migrations, shared-artifact backup and quarantine recovery. Use for release or lifecycle changes and their qualification; not routine connector or stateless application work.
license: MIT
---

# Release and recovery

## Required context

Read [repository instructions](../../../AGENTS.md) and [skill routing](../../../docs/development/skills.md); use `pm-go` for Go changes. Obtain the accepted release/task specification, component/file ownership, compatibility matrix, database migration sequence, deployment ownership and recovery targets. Verify actual artifacts and version pins before making compatibility claims. Missing accepted input is an explicit affected-scope gap, not permission to choose a new architecture.

Keep F06 context-import/manifest and README first-ownership reconciliation with its existing owner. This skill does not approve a context import, activate old instruction drafts or resolve that decision.

## Procedure

1. **Inventory release boundaries.** Identify actual client, server, worker, query helper, generated API/bundle/schema/handler versions and artifact hashes. Support paired, client-only and backend-only packaging through the same Go/Gin/PostgreSQL/Temporal/shared-object architecture. Development uses that same stack. Standalone packaging does not create an embedded local queue or alternative durability mode.
2. **Check public compatibility.** Use the sole public information contract at `/v1/info`: protocol major and required features, minimal safe metadata and defined additive behavior. Exercise supported old/new client/server combinations and refusal paths from the accepted matrix. CLI and browser retain the same API semantics; administrative ingress stays separate. Do not infer compatibility from matching marketing versions or successful startup.
3. **Check operator ownership.** Ordinary remote client calls never auto-start a backend. Stack lifecycle is an explicit operator action. Distinguish bundled services from provisioned managed dependencies; never mutate managed-service lifecycle/schema without the applicable authority. Preserve PM PG, Temporal persistence/visibility, object metadata and customer connector database boundaries. No task implicitly authorizes destructive cleanup or purge.
4. **Check native and shared-artifact packaging.** Keep DuckDB/native analytics confined to the restricted query child; client/API/worker do not link the analytical engine. Qualify actual OS/architecture/toolchain/native combinations and child resource, input, network and secret isolation. Verify complete immutable shared objects, digest/length/generation and scoped PG manifests; host-local scratch and rename are not durable multi-host storage.
5. **Plan upgrade and rollback limits.** Follow supported PostgreSQL and Temporal upgrade sequences, separately owned schema migrations and worker history/version compatibility. Expand schemas compatibly before old/new workers coexist; test replay and in-flight work across the approved transition. Document irreversible steps and supported rollback boundaries. An arbitrary Down migration, binary downgrade or restoring only PM PG is not a general rollback plan.
6. **Close the backup set.** Record consistent backup provenance and restore order for PM application PG, Temporal persistence and visibility state, shared-object metadata/filer and object bytes, required encryption keys/configuration and retention references. Keep ownership, digests, component/schema versions, epoch and recovery window explicit. Verify retained artifacts remain reachable; a database dump alone is not a complete PM backup.
7. **Restore under quarantine.** Disable effect egress and scheduling/admission as specified before restored state can run. Establish the new recovery epoch and invalidate old execution authority. Reconcile post-backup provider effects and dispatch uncertainty using independent evidence before releasing eligible work. Restoring storage does not undo provider writes; unknown effects cannot be blindly replayed. Preserve restricted reconciliation authority and explicit release decisions.
8. **Measure recovery and release evidence.** Run authorized same-stack restore/upgrade/rollback/replica checks against owned fixtures, including missing objects/keys, interrupted migration, old histories, stale grants and post-backup effects. Record measured RPO/RTO, loss window, component identities and unproven conditions. Stated RPO 24h/RTO 4h are qualification targets, not measured guarantees; stricter tiers and logical CDC remain deferred unless separately accepted.

## Expected evidence

Produce a release/compatibility matrix, exact artifact manifest, ownership-aware migration/runbook sequence, complete backup/restore inventory, quarantine and effect-reconciliation evidence, measured recovery outcomes and known rollback limits. Separate structure/build/unit checks from actual persistence, browser, cross-host, provider and recovery evidence. Tool availability or a passing health endpoint cannot certify release readiness.

For an authorized delivery workflow, preserve the reviewed target-base SHA, merge base, candidate/final head, independent Standards and Spec reports, fixes and final CI identities. The assigned delivery pipeline owns fixes/push/CI and never gains automatic merge authority. Do not execute delivery merely because this procedure mentions it.

## Boundaries

Use `pm-durable-effects` for run/effect/checkpoint transitions and `pm-connector-contracts` for exact connector proof reuse. Do not invent unsupported version pairs, a second product mode, product MCP, backup completeness, recovery success, provider-write reversibility or setup/import approval.
