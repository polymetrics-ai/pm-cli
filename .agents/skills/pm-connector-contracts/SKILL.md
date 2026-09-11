---
name: pm-connector-contracts
description: Preserve PM connector source, typed values, schema and seven-lane contracts when changing compiler lowering, generated commands, metadata, pagination or connector proof. Use for these boundaries, not routine unrelated Go edits or deployment work.
license: MIT
---

# Connector contracts

## Required context

Read [repository instructions](../../../AGENTS.md) and [skill routing](../../../docs/development/skills.md). For Go changes start with `pm-go`. Obtain the current task's accepted contract, affected connector source revision/reference closure, ownership mapping and exact acceptance examples. For account metadata or proof reuse, also obtain the scoped schema/dependency evidence. These are task inputs; no private planning file is an implicit repository dependency.

Keep unresolved F02 projection/required-field choices and F03 upstream-source adoption explicit until their owner records acceptance. Retrieval of an official source is not adoption, full compiler capture or provider proof. Work on unaffected accepted behavior can continue.

## Procedure

1. **Trace facts to their owner.** Record source URL, immutable revision, captured byte hash, license/notice, dialect and exact source pointers. Close recursive references with explicit resolution and conflict handling. Reject duplicate/unknown keys and ambiguous bindings. Keep sparse amendments source-linked; do not invent fields, lifecycle steps, generic URL/body overrides or one adapter per provider. Generated runtime JSON is disposable output; runtime reads the admitted bundle, not source documents, proof receipts or an Atlas.
2. **Preserve typed semantics.** Inspect every affected request location and status/media response variant. Retain required lists, wire names, stable field identity, nullability, defaults, style/explode/allowReserved, sensitivity, unions and declared arrays. Distinguish absent, null, false, zero and empty. Preserve integer/decimal precision and scale across Go/browser boundaries; do not coerce through float64. Keep instant and local timestamp meanings distinct. Unsupported shapes create an exact affected-cell gap; never silently drop a required field or suppress unrelated supported cells.
3. **Check executable composition and client parity.** Follow the generated operation into an actual registered activity handler. A descriptor alone cannot claim implementation. CLI and browser use the same public OpenAPI 3.1, typed inputs, authorization and error semantics. Server preflight remains authoritative. All provider I/O, including reads/discovery, runs in admitted worker activities. Offline help can use safe local projections and disclose missing/stale metadata.
4. **Check page and transport semantics.** Direct read returns one truthful page with completeness and navigation. Derive navigation from declared strategy and the page size actually sent, separately from a PM total cap. Preserve scoped continuation and unread tails; reject conflicting navigation before I/O. Keep source strategy, write/deletion/matching/null policy, cadence, delivery, order, deduplication, state and binary behavior independent. A page cursor or polling checkpoint does not establish CDC.
5. **Check account schema authority.** Static schemas do not describe account-specific custom fields. Discover only through declared metadata capabilities; no sample-row inference. Bind immutable snapshots to server/principal/workspace/connection/account/resource/source/permission scope. PostgreSQL owns selected snapshot identity. Failed/partial discovery cannot replace the complete selection. Preserve field spelling, requiredness, writability, types, associations and precision. Accepted runs pin schema evidence; relevant used-field drift invalidates new effects and plan approval. Do not silently choose latest or remap fields.
6. **Maintain the complete inventory.** Initial connectors are GitHub, PostgreSQL, Gong, Pylon and HubSpot. Each explicitly inventories `direct_read`, `direct_write`, `etl`, `reverse_etl`, `binary_download`, `binary_upload` and `sync_transport`. Keep seven lane JSON files and seven thin scripts through the common runner when those files are implemented. Keep unsupported, not applicable, blocked, failed and unproven distinct. Deferred PostgreSQL logical CDC stays deferred.
7. **Choose observable checks.** Link each changed rule to a shared causal regression and exact connector + operation + lane + behavior + case. Preserve returned IDs and owned cleanup. Compare independent expected fields, rows, effects or byte digests with actual observations. Source JSON, exit zero, mocks and descriptor reachability alone cannot certify a connector.

## Expected evidence

Report source/ref closure and license disposition; before/after semantic mapping; affected owners/cells; actual handler reachability; focused RED/GREEN results when code changes; and remaining unsupported or blocked scope. Connector proof records actual PM client/API/worker hashes, query-helper hash if used, source/bundle/schema/handler/assertion revisions, scoped inputs, sanitized command/request/response, independent assertions, effects and physical execution times.

Keep fake and live evidence separate. Reuse requires known compatible dependency closure and retains the original receipt/time. A newer relevant failure prevents resurrecting an older pass; unknown closure invalidates conservatively. One successful command never certifies a lane, connector or account tier.

## Boundaries

Use `pm-durable-effects` for write grants, duplicate delivery, worksets and checkpoint publication; use `pm-release-recovery` for release/restore compatibility. This procedure does not approve source adoption, product choices, provider access, new connectors, CDC, runtime proof gates or application implementation.
