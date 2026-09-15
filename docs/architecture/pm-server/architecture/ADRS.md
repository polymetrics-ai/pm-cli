# Architecture decisions475

These 21 decisions belong to one implementation plan. ADR475-17 through ADR475-21 are accepted product/source contracts; every application falsifier, generated artifact and provider proof remains UNEXECUTED.

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

## ADR475-17 — Complete action result surface

Context: A fixed display subset and hidden pull-request filter lose source data and make result/pagination truth ambiguous.

Considered: Complete data by default plus shared typed optional selection/filtering; fixed six-field default; provider-specific or proof-only projection.

Choice: Accepted F02 returns complete action data by default. Shared typed optional field selection and record filtering preserve nesting, absent/null/value, exact values, every source entry and truthful pagination.

Evidence: PM-ISSUE-27-ACCEPTED and GITHUB-REST-SPEC-ACCEPTED in SOURCE-LEDGER.json; V02, V29, V32, V67 and P_direct_read remain UNEXECUTED.

Tradeoffs and consequences: Payloads can be larger; callers may explicitly select/filter while server and every generated client preserve source membership, continuation and exact values.

Supersedes: The six-field GitHub default and hidden `pull_request` exclusion are rejected, not compatibility behavior.

Revisit trigger: An absent selector omits valid source data, filtering coerces types/runs after selection, or an empty filtered page is reported exhausted.

First milestone: M1 contract; M6 first GitHub consumer.

## ADR475-18 — Finite durable admission and recovery

Context: Unbounded or synthetic-only limits and replacement keys after uncertain commit can overload a deployment or create duplicate logical runs.

Considered: Finite deployment/workspace reservations with same-key recovery; historical 1000/1h sample as default; unlimited aliases or client-generated replacement keys.

Choice: Accepted F01 uses the exact refusal table, development 32/8/2m and standard 256/32/10m, atomic PostgreSQL pending reservations and workspace-count/workspace-age-before-deployment refusal precedence. Its bounded three-send/30-second policy is client-to-API recovery only and never constrains provider/run-owned `Retry-After` or durable retry scheduling. Issue #14 proves the actual route and a controlled durable execution-start transition; issue #16 later proves actual-worker integration, while #19/#21 own client recovery.

Evidence: PM-DECISION-F01-AUTH and PM-ISSUE-14-ACCEPTED in SOURCE-LEDGER.json; V09, R09, R10 and R14 remain UNEXECUTED.

Tradeoffs and consequences: Overload refuses new work while retaining accepted work; clients keep secret-free intent journals and expose stable recovery guidance.

Supersedes: Historical 1000 pending/1h and synthetic 3/2/30s are not production defaults; incomplete refusal bindings are closed.

Revisit trigger: 202 precedes known commit, a replica multiplies budget, replay reserves again, age removes accepted work, or a used/unresolved key executes fresh intent.

First milestone: M4.

## ADR475-19 — Revision-safe access mutation replay

Context: Six M3 access POST rows incorrectly inherited read cursor/ETag idempotency and could duplicate mutations or reissue one-time credential material.

Considered: Scoped mutation receipt plus `If-Match`; read cursor/ETag as idempotency; automatic stale-revision rebase or secret replay.

Choice: Accepted AUTH-MUTATION-PARITY requires current authorization, `If-Match` and workspace/principal/operation/target-scoped keys. It preserves explicit connection/operation/workspace/time ceilings on service/delegation grants and distinct auditable actor/effective-actor identities. Original grant/create is 201, revoke/unchanged is 200, and exact replay is 200 with the original safe result/revision.

Evidence: PM-DECISION-F01-AUTH, PM-ISSUE-8-ACCEPTED and PM-ISSUE-12-ACCEPTED in SOURCE-LEDGER.json; V14, R08 and R14 remain UNEXECUTED.

Tradeoffs and consequences: Clients retain keys and expected revisions; a lost one-time service-account credential requires explicit revoke/new create instead of replay recovery.

Supersedes: Read-style mutation idempotency metadata, automatic revision rebasing and replay secret recovery are rejected.

Revisit trigger: Replay bypasses current authorization, a grant exceeds its ceiling, actor attribution collapses, a refused revision mutates, equal concurrent requests create two effects, or credential material appears outside the original successful create.

First milestone: M3.

## ADR475-20 — Connector source authority

Context: A moving documentation page or assumed OpenAPI cannot provide immutable schema fidelity for GitHub or native PostgreSQL.

Considered: Pinned official source/license and provider-native documentation; moving docs pages; invented or assumed PostgreSQL OpenAPI.

Choice: Accept the exact GitHub REST source/license identity and require version-pinned official PostgreSQL protocol, SQL and metadata sources. The bounded docs tree records identities without copying 12.9 MB; source.lock remains with each first owner.

Evidence: GITHUB-REST-SPEC-ACCEPTED, GITHUB-REST-LICENSE-ACCEPTED and PM-ISSUE-28-ACCEPTED in SOURCE-LEDGER.json; V20, V21 and V24 remain UNEXECUTED.

Tradeoffs and consequences: Source owners must retain complete relevant definitions, attribution and reference closure before compilation; recorded identity alone does not certify a connector.

Supersedes: WEB-GITHUB and WEB-PG remain historical observations, not accepted immutable implementation inputs.

Revisit trigger: Generated meaning uses another revision/hash, incomplete closure, a moving page or fabricated PostgreSQL OpenAPI.

First milestone: M6 GitHub; M7 PostgreSQL.

## ADR475-21 — Bounded PostgreSQL mapped upsert and staged composition

Context: Reusable SQL transport did not itself define safe destination identity, changed-column effects or when the narrow `pg-wh-pg` demonstration could be composed.

Considered: Approved keyed mapped insert-or-update through common settlement; arbitrary SQL/blanket replacement; demo executor; waiting for full all-five M11 before any PostgreSQL composition.

Choice: Issue #34 adds one bounded mapped insert-or-update through existing approval/settlement, requires an explicit approved migration for any key/account/generation reset and retains the old mapping for audit. Issue #28 first-creates source/semantics/lifecycle, #34 later updates those declarations, and issue #41 first-creates only N13-PG after #28–#34 before updating the same composition file for full N13 at M11.

Evidence: PM-ISSUE-28-ACCEPTED, PM-ISSUE-34-ACCEPTED and PM-ISSUE-41-ACCEPTED in SOURCE-LEDGER.json; V34, V40, V41, V42, V43, P_etl and P_reverse_etl remain UNEXECUTED.

Tradeoffs and consequences: Stable-key/constraint metadata, changed-column calculation, independent unmapped-field readback, a declared sound occurrence witness for unknown COMMIT and serialized file integration are required. Desired rows alone do not identify which occurrence committed; broader issue #41 obligations stay open.

Supersedes: Stop-at-#28 staging and M11-only first creation of PostgreSQL composition are superseded only for `pg-wh-pg`.

Revisit trigger: A missing/ambiguous key reaches I/O, unchanged columns are assigned, unmapped fields change, key/account/generation identity resets without approved migration, confirmed rollback is treated as definitely unsent, unknown COMMIT is inferred from desired rows or blindly retried, or N13-PG is counted as full V74/P_sync_transport/five-by-seven proof.

First milestone: M8 after M7/M8 prerequisites.
