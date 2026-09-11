# Issue tracker: PM CLI

PM tracks accepted work in the private GitHub repository [polymetrics-ai/pm-cli](https://github.com/polymetrics-ai/pm-cli). Use its actual issue identity and the supported `gh-axi` reads below. Repository instructions and the current task govern authority. This guide configures issue/spec lookup for the selected AI Hero code-review skill; it does not enable a triage or publishing workflow.

## Read the assigned issue

Start with the actual issue URL or number supplied by the authorized task. For the early skills/CI setup slice, collect each published setup-owning tracking issue required by that slice. Verify the repository, issue type, title and stable task identity. GitHub shares issue and change-request numbering; reject a response containing `pull_request` when an issue specification is required. Never infer issue numbers from milestone order or a prior deleted item.

For the early skills/CI setup slice, read both [instruction and skill setup](https://github.com/polymetrics-ai/pm-cli/issues/2) (`pm-bootstrap-context-instructions`) and [CI and toolchain setup](https://github.com/polymetrics-ai/pm-cli/issues/3) (`pm-stack-toolchain-qualification`). This slice contributes to their setup acceptance rules and closes neither issue. Full BOOT, context/F06 and V56/TOOLCHAIN-QUAL obligations remain open; publication alone does not authorize their implementation.

The following are read-only command templates. Replace angle-bracket values with the actual task inputs; they are not future identity predictions.

```sh
gh-axi api GET /repos/polymetrics-ai/pm-cli --jq '{full_name,private,default_branch}'
gh-axi issue view <actual-issue-number> -R polymetrics-ai/pm-cli --full --comments
gh-axi api GET /repos/polymetrics-ai/pm-cli/issues/<actual-issue-number> --jq '{number,id,html_url,title,updated_at,pull_request}'
```

If the task supplies a stable identifier but no resolved issue identity, inspect all relevant pages and require a unique exact match. Listing metadata is discovery, not a complete specification:

```sh
gh-axi api GET '/repos/polymetrics-ai/pm-cli/issues?state=all&per_page=100' --paginate --jq '[.[] | select(.pull_request == null) | {number,html_url,title,updated_at}]'
```

Read the complete body and every decision/comment explicitly incorporated by the accepted task. `issue view --full --comments` is the supported full-body path. Check for truncation; API-rendered strings can be clamped even when the underlying GitHub response is complete. Do not hash a truncated display or a wrapper envelope as issue content. If necessary, use the supported API `--jq` projection to emit bounded decoded chunks and verify complete ordered reconstruction. An inaccessible or incomplete required spec blocks review readiness; do not replace it with a guessed summary.

## Bind review to accepted inputs

Before review, record an evidence envelope outside product runtime state:

- Actual repository identity, full issue URL/number/database ID, retrieval time, `updated_at`, exact decoded body hash and the accepted revision/decision provenance. An update timestamp alone is not content identity.
- The exact accessible immutable inputs required by the accepted scope. For the currently authorized early skills/CI setup slice, bind its published setup-owning tracking issues, current scope/authority evidence and bounded setup candidate/input closure by full commit IDs or verified byte digests. Importing the full PM architecture is not a prerequisite for this narrow setup review.
- For later product work or a separately authorized full-context review, record the accepted PM context commit, required file paths and byte hashes. Resolve complete contents at full commit IDs; mutable branch links do not establish accepted context.
- Any accepted comments/amendments, with actual identities and hashes. A later unrelated comment is not automatic scope expansion. If the body changes, reconcile it with the accepted task before reviewing against new requirements.
- The target branch SHA recorded at task intake, actual merge base, commit list, candidate head and nonempty diff digest. Keep target and candidate identities separate.
- Standards/spec input digests, reviewer identities and actual runtime evidence, independent reports and findings/dispositions. Any changed candidate head needs review coverage for the resulting head.

When the accepted scope includes commit-pinned repository context, resolve that actual commit and then fetch each required file through the supported API. These commands do not require creating or importing context for the early setup slice:

```sh
gh-axi api GET /repos/polymetrics-ai/pm-cli/commits/<accepted-context-commit> --jq '{sha}'
gh-axi api GET '/repos/polymetrics-ai/pm-cli/contents/<accepted-context-path>?ref=<accepted-context-commit>' --jq '{path,sha,size,type}'
```

The second command verifies metadata only. Obtain complete decoded content separately; compare its SHA-256 with the accepted evidence and retain the Git blob SHA as a different identity. Do not decode a truncated Base64 string. There is no implicit private home path, alternate issue store or unaccepted archive fallback. The currently authorized early skills/CI setup review uses its published setup-owning tracking issues plus the exact accessible immutable setup candidate/input closure and current scope evidence. Repository implementation still requires its explicit task promotion. This bounded setup authority does not approve an administrative context import, resolve F06, transfer README ownership or dispatch product work. Full PM context/spec readiness remains separate.

Missing, ambiguous, deleted or unaccepted issue/context inputs remain explicit gaps. A closed issue, successful worker report or retrievable unmerged commit does not prove an accepted landing.

## Required review and delivery boundaries

Use the exact AI Hero source identified in [skill provenance](../development/skill-provenance.md), with [PM routing](../development/skills.md) and the candidate's applicable [repository instructions](../../AGENTS.md). Before an authorized review, resolve the recorded comparison base and require a nonempty applicable diff. The accepted intake-SHA comparison policy does not require a new policy question.

Run both required native Codex gpt-6-astra/xhigh axes independently: Standards receives PM standards and the original skill's heuristic baseline; Spec receives the accepted issue and the immutable inputs required by its scope. Verify actual turn metadata, not only launch configuration. Keep their reports separate and distinguish heuristic smells from explicit standards. An unavailable reviewer or missing Spec is not a pass.

The assigned no-mistakes delivery owner handles fixes/push/CI when that work is authorized. Reading this guide neither initializes a repository nor starts a pipeline. The CLI driver is not the pipeline backend, and doctor success does not establish the model used by a future worker. Preserve final-head review and real CI evidence; no automatic merge.

Do not create, edit, comment on, label, assign, close or publish issues under this lookup guide. Do not dispatch product work, treat change requests as a triage queue, create a map/child-ticket system, import context, activate broad upstream setup or copy skill catalogs. Publication and implementation follow the current task's explicit ownership and gates.
