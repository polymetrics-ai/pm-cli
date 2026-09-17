# CI events and owner-account merge

## Test before and after merge

The foundation and documentation workflows run for trusted, open, same-repository
pull requests targeting `main`. They also run on each non-forced push to `main`,
including an authorized merge. Checkout uses the event's exact `github.sha`: the PR
merge-test revision before merge and the landed commit afterward. PR issue-link
validation applies only to PR events; main pushes still validate documentation.

New commits cancel obsolete runs for the same PR. Main runs have separate
SHA-keyed concurrency groups and are not cancelled by PR activity or later main
commits. A main failure must stay visible and receive a focused repair PR; it
does not authorize an agent to merge or waive a test.

The foundation suite retains all Bazel targets, its job-owned dependency fixture
and unconditional ownership-scoped cleanup. Missing prerequisites, test failures
and cleanup failures remain failures.

## Dedicated runner admission

The reviewed [wrapper](../../scripts/ci/runner-admission.sh) and
[policy](../../scripts/ci/runner_admission.py) are installed by the operator
outside the writable checkout, owned by root. The existing runner hook points
to that wrapper. Workflow code cannot promote a changed policy into the host.

Admission requires the expected runner UID, repository, original and rerun
actor, payload sender, workflow path, event ref and GitHub's server-defined
`GITHUB_SHA`. The direct workflow's `GITHUB_WORKFLOW_SHA` must equal
`GITHUB_SHA`; checkout consumes that exact SHA. For PR jobs,
`GITHUB_SHA` and `GITHUB_REF` are the merge-test
identity source of truth; the immutable event's nullable or stale
`pull_request.merge_commit_sha` is not required or compared, and admission makes
no live REST lookup. Only the two reviewed workflows, open same-repository PRs
against main, and non-deleted, non-forced owner pushes to main are supported.
Forks, closed or merged PR metadata edits, other actors, refs, events and
workflows are refused. Input size and execution time are bounded. Refusal
returns nonzero to fail only the job; it never stops the listener, Docker daemon
or another job.

Before installation, run the focused suite:

```sh
python3 -I -B scripts/ci/test_runner_admission.py
actionlint -config-file scripts/ci/actionlint.yaml .github/workflows/check.yml .github/workflows/architecture-documentation.yml
shellcheck --shell=sh scripts/ci/runner-admission.sh
```

For an authorized host update: verify runner identity and idle state; preserve
the current root-owned wrapper and its hash; install the reviewed policy first
as root:root mode 0644, then atomically replace the executable wrapper as
root:root mode 0755; compare installed hashes, ownership and modes.
Retain the existing confinement and Docker/network setup. Verify accepted and
rejected synthetic events under the runner identity and independently confirm
that the listener PID, service restart count and Docker process survive refusal,
then exercise a real PR job. Synthetic admission is not live workflow proof. A
rollback restores the exact saved files only after stopping new admissions
through the runner's normal service lifecycle, not UID-wide signals. Do not
automatically restore a known listener-killing hook.

## Merge authority and enforcement prerequisites

Only the GitHub account `karthik-sivadas` may merge. The account owner explicitly
authorizes agents to merge using this account when the PR's declared scope is
correctly implemented, its agreed BDD/acceptance tests pass, independent review
findings are resolved, and genuine final-candidate CI is green. No human-only
click or additional routine approval is required. Missing, skipped, stale,
different-head or failed required checks do not qualify. Do not use an
administrator bypass or force a merge. Keep automatic merge off; each merge
requires this explicit verification. This supersedes the earlier human-only
interpretation.

On an eligible private-repository organization plan, configure main protection
to require the `foundation` and `architecture-documentation` checks against
the current base, require a PR and resolved review conversations, restrict
updates to `karthik-sivadas`, include administrators, and prohibit force-pushes
and deletion. Keep auto-merge off. Do not require the sole author's own approval:
independent AI review and the authorized merge decision are separate controls.

Private repositories on GitHub Free for organizations cannot enforce that
branch-protection policy. A paid organization plan or separately authorized
visibility change is required; documentation is not enforcement. GitHub sees an
agent using the owner's credential as the same account, which matches this
account-based policy. Do not revoke shared credentials, change billing or make
the repository public as an implicit CI repair.

See GitHub's [protected branch documentation](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches)
[default variables](https://docs.github.com/en/actions/reference/workflows-and-actions/variables),
[pull-request event identity](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request),
[pull-request mergeability behavior](https://docs.github.com/en/rest/pulls/pulls#get-a-pull-request),
[REST API breaking changes](https://docs.github.com/en/rest/about-the-rest-api/breaking-changes?apiVersion=2026-03-10),
and [runner hook behavior](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/run-scripts).
