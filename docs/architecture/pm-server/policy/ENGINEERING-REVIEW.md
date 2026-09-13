# Shared engineering and review policy

Proposed documentation import; this policy neither grants implementation nor publication authority. [IMPORT-MANIFEST](../IMPORT-MANIFEST.json) owns the candidate state, granted import/publication authority, and remaining verification/merge conditions. This policy consolidates the existing PM instruction draft and REVIEW-DELIVERY-POLICY without activating either. Source digests are in the import manifest.

PM has one remote-first Go/Gin/PostgreSQL/Temporal/shared-artifact architecture. The CLI and browser call the same versioned API. Server application services own authorization and durable state; worker activities own provider I/O and credential resolution. Domain code accepts context.Context and typed contracts; framework contexts stay in adapters. Source descriptors and the Foundation Atlas cannot establish executable reachability or provider proof. The five initial connectors and seven lane inventories do not imply complete provider parity.

Read the accepted issue and its explicitly linked versioned contracts. Preserve first-owned files and later integration rights. Where CodeGraph is present, use it first to locate code; otherwise use normal source tools. Use the project pm-go router and relevant installed references only. No task may silently install tools, add libraries or change accepted architecture.

Create and execute the causal failing test before production edits for changed behavior; implement the scoped change and retain the resulting evidence. Create tests this issue owns rather than treating their absence as a prerequisite failure. Missing merged prerequisite tests are discrepancies. Use focused standard Go tests and table subtests by default. Add integration, concurrency, security, recovery and regression checks where the accepted validation map requires them. Compilation, whitespace checks, missing/zero tests, unexpected skips or mocks labeled live cannot satisfy behavioral acceptance. Documentation uses content, link and instruction-discovery checks. All planned PM checks in this preparation are UNEXECUTED.

Verify the authorized implementation and independent reviewer environments before relying on their results.
Implementation and independent review are distinct roles; an implementer cannot supply its own independent review.
Record the actual role and authentication evidence without credentials.
Application engineering instructions are model-neutral; task-specific operational routing is supplied separately and must not be copied into this context.
Preparation does not install, restart or change shared tooling, and does not qualify future implementation environments.

Use the pinned [AI Hero code-review reference](https://github.com/mattpocock/skills/blob/3cca18b368ae95cdbdebbff572ccafa662551015/skills/engineering/code-review/SKILL.md), MIT, for the explicitly requested independent two-axis review. Standards receives PM application instructions and linked standards; Spec receives the accepted issue and immutable context. Keep reports separate. Missing independent reviewers, inaccessible spec, an empty applicable code diff or an unapproved comparison base blocks that future code review. Reviewing these issue specifications is a separate read-only activity, not a code-diff review.

Record the accepted comparison scope, target revision at intake, resolved merge-base and commit list, and actual reviewed candidate head.
No-mistakes remains the sole implementation fixer, pusher and CI owner once its required execution route is qualified.
Do not invent integration keys or change its shared daemon.
Changed heads require review of the resulting diff.
Record standards/spec digests, findings, dispositions and final-head evidence.
AI approval does not replace required human review.
Automatic merge stays off; confirm separately approved landing before cleanup.

The documentation-publication administrative task is the sole exception to requiring a previously committed context at intake: its reviewed local manifest/diff supplies that context. Once a candidate exists, record its real commit. Dependent implementation requires the approved merged context and verified access. Publication approval, instruction activation, implementation start, live account/effect scope and merge remain distinct user-owned gates. No comment archive or private fleet state belongs in PM documentation.
