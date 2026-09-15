# PM Server architecture and implementation context

This bounded tree projects the accepted PM Server product contracts and still-unexecuted implementation plan. [IMPORT-MANIFEST](IMPORT-MANIFEST.json) is the compact target/source digest inventory. Each `files[]` record distinguishes its source digest, any explicitly recorded sanitized-predecessor digest, and current transformed-target digest; `files[]` does **not** store per-file byte counts. Byte lengths cited in source or decision evidence are separate facts, not an implied manifest field.

## Publication and BOOT state

The reviewed accepted-contract publication tree at `3516a84aef9eb9a880408e6b5ab56ab911276429` was merged normally by [PR #47](https://github.com/polymetrics-ai/pm-cli/pull/47) into `main` at `d8e84e1fc78561173feac8abfd64ce669f09f373`, whose parents are `563adfd28b80101737ad41298aaa3af839eeacf6` and `3516a84aef9eb9a880408e6b5ab56ab911276429`. [CI run 34959292721](https://github.com/polymetrics-ai/pm-cli/actions/runs/34959292721) succeeded on runner 2 for that publication baseline. Those facts do not pre-certify this later combined candidate.

BOOT-1 readback passed: approved original outer SHA-256 `ec1b366dcebad2ac2fa1c58c68d4cde54fa0ead571392d189c4498cce4de7656` authenticated all 43 original staged paths, accepted review/merge authenticated the amended descendant, and the landed tree matched the reviewed `3516a84aef9eb9a880408e6b5ab56ab911276429` tree with all 42 landed-baseline payload hashes plus its compact manifest verified. The historical outer authenticates original staging; it is not the digest of later transformed manifest bytes. Issue #2/#3 now has bounded local Darwin foundation checks and fresh-worker discovery evidence, described in [developer setup](../../development/setup.md). Those observations do not replace independent Standards/Spec review, genuine final-head CI, or required merge authorization. Remaining BOOT completion must bind those actual results. No PM service, connector, provider effect, or backend release qualification follows from documentation or local foundation checks.

OMP is the sole project coordinator and integration owner under the September 15 workflow. Agent model/account routing remains operator-owned and outside application documents. The single [engineering and review policy](policy/ENGINEERING-REVIEW.md) owns validation, independent review, and delivery; no private controller/runbook is an application prerequisite.

## Active worker entrypoints

Within this candidate, fresh workers use these actual target files:

- repository [AGENTS.md](../../../AGENTS.md);
- short [architecture overview](../../architecture/overview.md), [decision index](../../architecture/decisions.md), and [domain glossary](../../agents/domain.md);
- [developer setup](../../development/setup.md), [testing](../../development/testing.md), [release](../../development/release.md), [focused skill routing](../../development/skills.md), and [skill provenance](../../development/skill-provenance.md);
- the sole local [PM Go router](../../../.agents/skills/pm-go/SKILL.md) plus the bounded [connector-contract](../../../.agents/skills/pm-connector-contracts/SKILL.md), [durable-effects](../../../.agents/skills/pm-durable-effects/SKILL.md), and [release-recovery](../../../.agents/skills/pm-release-recovery/SKILL.md) procedures when their triggers apply;
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
