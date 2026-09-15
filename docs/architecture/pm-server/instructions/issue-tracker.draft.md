# Synchronized inactive projection: `docs/agents/issue-tracker.md`

This file mirrors the intended issue/spec tracker for provenance and comparison. It is not an active instruction entrypoint, is not fresh-worker intake, and does not establish a landed BOOT baseline. Relative links in the projected body resolve from the target path named in `INSTRUCTION-TARGETS.json`.

# Issue and specification tracker

PM tracks accepted work in the private GitHub repository [`polymetrics-ai/pm-cli`](https://github.com/polymetrics-ai/pm-cli). Use the actual issue identity supplied by the authorized task. This guide defines read-only intake and traceability; it does not authorize triage, issue mutation, publication, implementation, push, merge, or provider/host access.

The single [engineering and review policy](../architecture/pm-server/policy/ENGINEERING-REVIEW.md) owns validation, independent review, and delivery. Repository instructions and the accepted issue set own product scope. OMP is the sole coordinator and integration owner; model/account routing is supplied separately by the operator and is not issue content.

## Resolve the exact issue

Consult current `gh-axi` help, then use read-only lookup against the exact repository and number. These are current issue-intake command templates, not PM application commands:

```sh
gh-axi issue view --help
gh-axi issue view <actual-issue-number> -R polymetrics-ai/pm-cli --full --comments
gh-axi api GET /repos/polymetrics-ai/pm-cli/issues/<actual-issue-number> --jq '{number,id,html_url,title,state,updated_at,pull_request}'
```

Verify repository identity, privacy/default branch, issue number, database ID, URL, title, stable task/case identity, state, and `updated_at`. GitHub shares issue and pull-request numbering; reject a result with a `pull_request` member when an issue specification is required. If the task supplies only a stable identity, search every relevant paginated issue page and require one unique exact match. A title, milestone position, branch name, old draft, or remembered number is not identity.

Read the complete issue body and every comment or amendment included by the accepted task. Preserve comment IDs, authorship, creation/update order, and explicit acceptance provenance. Later unrelated comments do not silently expand scope. Read merged dependency issues or accepted decision comments when the issue or task incorporates them.

## Prove content completeness

`gh-axi issue view <N> --full --comments` is an intake aid, not proof that a long body or comment is complete. On 2026-09-15, long issue output still clamped, and a direct API body projection exposed `truncated: true` with `original_length`. Always inspect wrapper/API truncation metadata and expected length before hashing or reviewing.

When any required body or comment is truncated, use supported read-only `gh-axi api` projections to recover its decoded bytes in ordered chunks of at most 2,000 characters. Preserve chunk order and offsets, require continuous coverage through the reported original length, and retrieve paginated comments separately. Base64-encode bounded chunks where needed so display rendering cannot alter or clamp their bytes. Reassemble outside product state, decode exactly once, and verify the complete UTF-8 byte length and SHA-256 against the approved fingerprint. Do not hash a rendered preview, truncated `.body`, wrapper envelope, JSON quoting, Base64 text, or reordered comments. Do not add a repository renderer or recovery script for this lookup.

The September 15 intake recovered complete issues #2, #3, and #46 through this bounded route; #2 and #46 matched their approved SHA-256 fingerprints. That observation does not make a later mutable issue read complete: repeat truncation detection and compare the accepted content identity for the actual review intake.

For each required issue/comment, record outside product runtime state:

- repository, issue number/database ID/URL, retrieval time, `updated_at`, decoded byte length, and exact decoded SHA-256;
- each accepted comment's database ID/URL, position, decoded byte length/hash, and the decision that incorporates it;
- any body/comment revision relative to the task's approved fingerprint, reconciled before review rather than silently accepted;
- the exact immutable architecture/source inputs required by that issue, using full commit IDs and per-file SHA-256 values; keep Git blob SHA, source SHA-256, sanitized-predecessor SHA-256, and transformed-output SHA-256 as distinct identities.

If complete accepted bytes or an approved digest cannot be obtained, report the exact gap. Do not replace the specification with a local task summary, private archive, historical issue draft, or prior chat.

## Bind a candidate to accepted review inputs

Before independent review begins, the coordinator must supply one complete review envelope so reviewers do not need to ask for routine context:

- target repository and default branch;
- accepted comparison base, target-branch SHA recorded at intake, actual merge base, included commits, candidate head, and nonempty diff identity;
- the full decoded body and accepted comments/amendments, identities, byte lengths, and hashes for **every** issue grouped into the candidate, not only the first or closing issue;
- the immutable accepted context commit, required paths and per-file digests for the affected scope, plus source/decision provenance where required;
- applicable repository instructions, ownership records, and standards;
- explicit included and excluded scope, dependencies, first-owner/later-update rights, validation evidence, and unresolved gaps.

The Spec reviewer receives all originating grouped issues and accepted amendments. The Standards reviewer receives the applicable PM instructions and standards. Both receive the same accepted base/head/diff identity. Use the upstream `code-review` skill unchanged when the shared policy authorizes review. A changed head requires coverage of the resulting diff. Missing issue bytes, accepted comments, immutable context, base, or applicable diff blocks a claimed review pass.

A local candidate, task brief, worker report, review result, successful documentation check, or retrievable unmerged commit is not an approved landed baseline. Dependent application work requires the accepted default-branch context revision identified by its task.

## Issue linkage and stacked changes

For a pull request targeting the default branch, use separate linkage lines for each issue:

```text
Closes #N
Refs #M
```

Use `Closes #N` only when that candidate fully satisfies issue #N and is intended to close it on default-branch merge. Use `Refs #N` for partial work, setup contributions, administrative amendments, or any issue whose obligations remain. A grouped candidate may include several separate `Closes` and/or `Refs` lines; never hide multiple issues in a range or treat one issue as the specification for the others.

A stacked pull request that targets another feature branch must use `Refs #N`, record its actual base/parent PR and dependency, and identify which later default-branch candidate will carry closure after the full issue is satisfied. Keep that tracking current when a stack is rebased or regrouped. Do not put premature closure language on an intermediate stack member.

This September 15 instruction cleanup is bounded issue #2 work with approved workflow amendments associated with #3 and #46; it closes none of them. Full BOOT requires the approved #46 publication to land, followed by BOOT-1 verification of that exact immutable default-branch revision. The cleanup candidate is not that landed baseline, does not complete BOOT-1–5, and does not authorize application implementation or CI/merge claims.

## Fresh-worker intake boundary

Fresh workers use the active root `AGENTS.md`, this tracker, `docs/development/{setup.md,skills.md}`, `.agents/skills/pm-go/SKILL.md`, the active configuration ADR, the accepted issue envelope, and the canonical PM Server contracts. Files under `docs/architecture/pm-server/instructions/` are synchronized inactive projections only. Historical issue drafts, private setup/controller runbooks, transition evidence, transcripts, and operator configuration are excluded from instruction intake without deleting their retained evidence bytes.

Under this guide, do not create, edit, comment on, label, assign, close, or publish issues; do not push or merge; and do not dispatch product work. Escalate only a genuinely missing accepted input or material unresolved product/security decision through the current task owner.
