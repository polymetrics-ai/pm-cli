# PM Server architecture and implementation context

This bounded tree projects the accepted PM Server product contracts and still-unexecuted implementation plan. [IMPORT-MANIFEST](IMPORT-MANIFEST.json) is the compact target/source digest inventory. Each `files[]` record distinguishes its source digest, any explicitly recorded sanitized-predecessor digest, and current transformed-target digest; `files[]` does **not** store per-file byte counts. Byte lengths cited in source or decision evidence are separate facts, not an implied manifest field.

## Publication and BOOT state

The accepted-contract publication correction, including its digest-field disclosure follow-up, is locally committed at `3516a84aef9eb9a880408e6b5ab56ab911276429` on the retained publication branch. [PR #47](https://github.com/polymetrics-ai/pm-cli/pull/47) remains recorded as open and unmerged against `main`; its last observed remote head `fb80b101a28d573194cf13c293e7ba86e381cdaa` predates the local accepted-contract candidate. Publication-specific local checks and independent review do not establish remote CI, push, merge, or a default-branch baseline.

The September 15 issue #2 instruction cleanup is a separate candidate, not a mutation of publication PR #47. It activates and reconciles the existing target entrypoints early, with approved workflow amendments associated with issues #3 and #46, but closes none of those issues. Full BOOT still requires the approved #46 publication to land, BOOT-1 to retrieve and verify that exact immutable default-branch revision, and the remaining BOOT-2–5 observations. No application implementation, generated connector bundle, passing application test, provider execution, live-effect proof, full CI, merge, or BOOT completion is claimed.

OMP is the sole project coordinator and integration owner under the September 15 workflow. Agent model/account routing remains operator-owned and outside application documents. The single [engineering and review policy](policy/ENGINEERING-REVIEW.md) owns validation, independent review, and delivery; no private controller/runbook is an application prerequisite.

## Active worker entrypoints

Within this candidate, fresh workers use these actual target files:

- repository [AGENTS.md](../../../AGENTS.md);
- [developer setup](../../development/setup.md) and [focused skill routing](../../development/skills.md);
- the sole local [PM Go router](../../../.agents/skills/pm-go/SKILL.md);
- [CLI and server configuration ADR](../../decisions/0001-cli-configuration.md);
- [issue and specification tracker](../../agents/issue-tracker.md);
- the shared [engineering and review policy](policy/ENGINEERING-REVIEW.md).

Files under `instructions/` are synchronized non-active projections for provenance and comparison. They are not a second instruction tree or fresh-worker intake. Historical issue drafts, private setup/controller runbooks, transition evidence, transcripts, operator paths, and credentials remain excluded without deleting their retained evidence bytes. [INSTRUCTION-TARGETS](instructions/INSTRUCTION-TARGETS.json) maps each projection to its authoritative target.

## Canonical product source

Start with [PLAN](architecture/PLAN.md) and [ADRS](architecture/ADRS.md). The normative machine-readable obligations are [CONTRACTS](architecture/CONTRACTS.json), [FOUNDATIONS](architecture/FOUNDATIONS.json), [FOUNDATION-MAPPINGS](architecture/FOUNDATION-MAPPINGS.json), [STATE-OWNERSHIP](architecture/STATE-OWNERSHIP.json), and [CLIENT-PARITY](architecture/CLIENT-PARITY.json). [TECHNOLOGY](architecture/TECHNOLOGY.json) and [ARCHITECTURE-DELTA](architecture/ARCHITECTURE-DELTA.json) preserve dependency selections and decision qualifications.

The current source keeps the CLI and browser thin. PM Server owns authentication, authorization, provider credentials, admission, execution, schemas, runs, schedules, and durable state. Deterministic Temporal workflows orchestrate; admitted worker activities perform all provider HTTP, SQL, binary, and metadata I/O. PostgreSQL owns accepted intent, grants, outbox, effect facts, manifests, and checkpoints across the explicit non-atomic Temporal handoff. Shared immutable objects own business bytes; worker scratch is disposable and DuckDB/native analytics remains in the restricted query child. PM has no product MCP or local fallback engine.

The accepted amendments projected into the canonical documents include:

- [F01 and AUTH-MUTATION-PARITY](https://github.com/polymetrics-ai/pm-cli/issues/14#issuecomment-5670702388): exhaustive admission errors/retry dispositions, finite pending-admission profiles and reservations, bounded same-key client recovery, retained receipt guards, and revision-safe idempotency for the six exact M3 access mutation rows.
- [F02 and F03](https://github.com/polymetrics-ai/pm-cli/issues/27#issuecomment-5669069268): complete action data by default, optional shared typed field selection and record filtering, exact nested/presence/number/pagination behavior, and the accepted immutable GitHub REST source/license identity.
- [PostgreSQL demonstration boundary](https://github.com/polymetrics-ai/pm-cli/issues/28#issuecomment-5669099222) and [mapped insert-or-update](https://github.com/polymetrics-ai/pm-cli/issues/34#issuecomment-5669559504): official protocol/SQL/metadata source authority, bounded stable-key mapped upsert, changed-approved-columns-only assignments, unchanged repeats with no assignments, unmapped-field preservation, exact types, safe checkpoint/retry behavior, and independent readback. An unknown COMMIT requires a declared sound occurrence witness for the exact intent; desired-row state alone is insufficient.

[MILESTONES](implementation/MILESTONES.json), [FILE-OWNERSHIP](implementation/FILE-OWNERSHIP.json), [VALIDATION-MATRIX](implementation/VALIDATION-MATRIX.json), and [PACKET-CONTRACTS](implementation/PACKET-CONTRACTS.json) retain all 44 original packets and their first-owner/later-update boundaries. Current product scope continues through issues #29–#34 and only issue #41's narrow post-#28–#34 `N13-PG` / `pg-wh-pg` composition, including its wrong-value and ambiguous-outcome variants. Broader #41, other compositions, the all-five/seven-lane denominator, and later connector obligations remain open. Coherent dependency order does not require one pull request or review per issue.

The former admission and GitHub proposal files are retained as resolved decision records, not open proposals. [SOURCE-ACCESS](SOURCE-ACCESS.md) explains why the verified 12,939,686-byte GitHub source is identified but not copied, why issue #27 still owns `source.lock`, complete recursive capture, attribution, compiler output, and proof, and why issue #28 must capture exact official PostgreSQL protocol/SQL/metadata inputs. [SOURCE-LEDGER](reference/SOURCE-LEDGER.json) preserves original source identities separately from sanitized and transformed outputs without publishing local operator paths.

Technical dossiers under `reference/remote-design/tech/` remain subordinate source observations. An accepted decision, recorded hash, local candidate, or inactive projection is not implementation evidence. Every planned PM command, package, test, deployment action, connector capability, and provider validation remains future until its existing owner creates and exercises the actual path.
