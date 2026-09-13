# Architecture decisions475

Proposed/Recommended. These16 decisions belong to the single implementation plan. All application falsifiers remain UNEXECUTED.

## ADR475-01 — One remote stack

Context: Accepted jobs must outlive clients and work across users/hosts.

Considered: One PG/Temporal/S3 stack; Local embedded product plus later hosted rewrite.

Choice: PostgreSQL+Temporal+workers+shared objects in every deployment.

Evidence: TEMP-SRC-001, OC02, EX06 in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Adds operational components and maintenance; avoids two durability architectures.

Supersedes: SQLite/local custom queue removed.

Revisit trigger: Source-backed chosen stack cannot meet qualified recovery/budget constraints.

First milestone: M0.

## ADR475-02 — Gin adapter

Context: 473 used standard HTTP simplicity without deep Gin study.

Considered: Gin; Chi; Echo; net/http.

Choice: Gin with strict schema/error/proxy/context policy; domain uses context.Context.

Evidence: TECH475-001, TECH475-003, TECH475-004 in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Framework context/binding defaults need explicit adaptation; cohesive route tooling is the benefit.

Supersedes: Standard library/Chi viable, no unsupported inferiority claim.

Revisit trigger: Strict boundary or maintainability tests cannot be satisfied without disproportionate patching.

First milestone: M3.

## ADR475-03 — PG application truth

Context: Two durable systems can disagree after either handoff reply is lost.

Considered: PG app truth plus Temporal orchestration; Temporal history as all business state; Custom PG queue.

Choice: pm_app owns accepted intent, effects, grants, manifests and events.

Evidence: 475-TEMPORAL in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Outbox/reconciler complexity is necessary; no cross-system atomicity claim.

Supersedes: Temporal owns orchestration, not provider effect facts.

Revisit trigger: Cross-store recovery loses accepted work/effect identity.

First milestone: M4.

## ADR475-04 — Tenant authority

Context: Multiple workspaces and pooled connections make missing filters consequential.

Considered: Application authorization plus RLS; Application-only checks.

Choice: Application authorization plus scoped RLS/non-owner roles and explicit workspace.

Evidence: 475-TEMPORAL in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Policies/roles/transaction cleanup add complexity and do not protect against fully compromised trusted PG credentials.

Supersedes: No global current-workspace or app-only filter dependence.

Revisit trigger: Any cross-resource/pool-reuse negative case leaks or mutates foreign scope.

First milestone: M2.

## ADR475-05 — Durable grant

Context: Short login sessions and long accepted jobs have different lifetimes.

Considered: Durable execution grant; Recheck original login session every send.

Choice: Session checked at API acceptance; independent run grant checked at effect boundaries.

Evidence: BRIEF475, 475-TEMPORAL in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Requires explicit grant revocation/expiry and reconciliation-only authority, but ordinary logout is safe.

Supersedes: Logout/expiry does not cancel accepted work.

Revisit trigger: Expiry stops accepted job or explicit grant revocation permits a new mutation.

First milestone: M1 contract, M4 admission; M9 explicit control integration.

## ADR475-06 — Provider I/O ownership

Context: Inline provider execution could create another signer/rate/receipt path.

Considered: Worker-only provider I/O; Bounded API provider execution with same guards.

Choice: All provider requests/discovery run in admitted worker activities.

Evidence: EX18, EX22, BRIEF475 in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Direct reads incur durable admission latency; one execution path is easier to audit.

Supersedes: API inline reads are PM metadata only; admin encrypts ingress only.

Revisit trigger: API obtains provider signer/executes request outside admitted worker path.

First milestone: M6.

## ADR475-07 — Source fidelity

Context: Historical projected schemas invented nullability and discarded response variants.

Considered: Pinned typed compiler with sparse amendments; Provider SDK per connector; Loose runtime spec interpretation.

Choice: One pinned source/ref closure -> typed operations -> generated safe projections.

Evidence: RI-0167, EX11, EX14, EX24 in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Compiler fidelity requires real fixtures and explicit unsupported shapes; generated code remains disposable.

Supersedes: No generated provider-specific Go SDK or source/proof runtime reader.

Revisit trigger: Null/media/status/location/requiredness changes without valid source rule.

First milestone: M6.

## ADR475-08 — Actual capability registry

Context: Historical helper libraries/descriptors lacked product consumers.

Considered: One actual handler composition; Atlas/descriptor as availability authority.

Choice: One executable composition with real activity consumers; compiler/value/proof foundations compose at their actual owners.

Evidence: RI-0125 in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Requires end-to-end reachability and phase wiring tests; avoids false implementation claims.

Supersedes: Atlas and descriptors alone cannot claim implementation.

Revisit trigger: Removing actual handler leaves capability reported implemented.

First milestone: M4.

## ADR475-09 — Member effect truth

Context: Batch transport and member outcomes are different; response loss is ambiguous.

Considered: Durable member/effect ledger; Batch success counts; Blind activity retry.

Choice: Persist dispatch/member intent and capacity before I/O, settle exact correlated outcomes.

Evidence: RI-0009, EX31 in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: More state and readback cost; preserves partial success and honest unknowns.

Supersedes: Aggregate counts or workflow success are insufficient.

Revisit trigger: Response loss/retry creates duplicate or erases unknown effect.

First milestone: M8.

## ADR475-10 — Temporal schedules

Context: Calendar semantics, overlap and out-of-order edits are provider-specific to the scheduler.

Considered: Temporal Schedule and adopted workflow; PM cron trigger; Short launcher starting independent run.

Choice: Temporal trigger owns occurrence; effect-free scheduled workflow first admits then adopts same execution.

Evidence: 475-TEMPORAL in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Low-level CAS adapter and occurrence admission add code; no competing scheduler.

Supersedes: No short launcher breaking overlap, duplicate StartRun or PM cron.

Revisit trigger: Concurrent mutation/trigger/DST/overlap case admits duplicate or wrong occurrence.

First milestone: M9.

## ADR475-11 — Immutable shared bytes

Context: Multiple worker hosts cannot use one host rename as byte publication truth.

Considered: S3 immutable attempts plus PG manifest; Host/shared writable files; PG bytea for all data.

Choice: Unique S3 attempt objects then PG manifest/checkpoint commit.

Evidence: 475-STORAGE in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Object service plus orphan/GC/integrity handling; portable bytes and bounded control DB.

Supersedes: No host rename or conditional-PUT atomicity assumption.

Revisit trigger: Partial/corrupt bytes publish or a second host cannot read registered data.

First milestone: M7.

## ADR475-12 — Analytical child

Context: Untrusted analytical data/query bounds need an actual process boundary.

Considered: Restricted DuckDB child; Shared writable DuckDB; Additional distributed query service.

Choice: Only restricted pm-query/analytics links DuckDB native driver.

Evidence: 475-STORAGE in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Native helper packaging and scratch copying cost; contains query failures/secrets.

Supersedes: No shared writable native file or arbitrary SQL security assumption.

Revisit trigger: Query escapes admitted files/secrets/resource bounds.

First milestone: M7.

## ADR475-13 — Public client parity

Context: Future UI must exercise the same product use cases as CLI.

Considered: One public OpenAPI3.1 with two clients; Independent UI backend; Go-only API client.

Choice: One OpenAPI3.1 Go/browser contract; separate admin ingress and route-specific auth.

Evidence IDs: OC35, TECH475-043, TECH475-044, BRIEF475; source ledger distinguishes primary source inspection from proposed PM behavior.

Tradeoffs and consequences: Requires actual-browser auth tests and constrained generator schemas; no full UI now.

Supersedes: Full UI deferred; browser harness real, not Node-only cookie proof.

Revisit trigger: Equivalent use cases differ in permissions, inputs, effects or errors.

First milestone: M5.

## ADR475-14 — Durable events

Context: SSE reconnect cannot infer commit order from ID allocation or live notifications.

Considered: Per-run locked sequence and PG replay; Global allocation cursor; Live bus only.

Choice: Per-run locked sequence with state/event TX and scoped snapshot/gap replay.

Evidence: OC22, OC23, OC38 in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Run-row serialization and retention snapshots cost writes; prevents skipped late commits.

Supersedes: Global sequence allocation or live bus is not commit-order durability.

Revisit trigger: Delayed first transaction causes reconnect cursor to skip committed event.

First milestone: M4.

## ADR475-15 — Operator lifecycle and recovery

Context: Operator must know which services/data PM owns and what restore can safely resume.

Considered: Explicit owned stack launcher; Client/HTTP autostart; Implicit managed-service lifecycle.

Choice: Explicit full stack launcher, ownership-aware schema/init, quarantine restore and data retention.

Evidence: OC33, EX06, 475-STORAGE, 475-TEMPORAL in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Coordinated maintenance/backup drills are required; one-host is not HA.

Supersedes: No CLI autostart, managed-service mutation or implicit purge.

Revisit trigger: Upgrade/restore reissues uncertain external effect or loses required state.

First milestone: M12.

## ADR475-16 — Proof scope

Context: New binaries cannot inherit old connector proof counts.

Considered: Exact case receipts and independent oracles; Batch certification/global CURRENT; Credential presence as proof.

Choice: Connector/lane/operation/behavior/case evidence with exact component/scope/currentness.

Evidence: RI-0233, RI-0125, BRIEF475 in SOURCE-LEDGER.json; exact primary links/ranges in the framework, storage and Temporal dossiers.

Tradeoffs and consequences: Live validation can be tier/account blocked; valid independent passes are preserved with provenance.

Supersedes: No batch authority, stale pass resurrection or inherited certification.

Revisit trigger: Wrong effect/bytes/rows or invalidated old pass appears certified.

First milestone: M11.
