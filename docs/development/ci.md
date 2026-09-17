# CI events and human-only merge

## Test before and after merge

The foundation and documentation workflows run for trusted, open, same-repository
pull requests targeting `main`. They also run on each non-forced push to `main`,
including a human merge. Checkout uses the event's exact `github.sha`: the PR
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
actor, payload sender, workflow path, event ref and commit identity. Only the
two reviewed workflows, open same-repository PRs against main, and non-deleted,
non-forced owner pushes to main are supported. Forks, closed or merged PR
metadata edits, other actors, refs, events and workflows are refused. Input size
and execution time are bounded. Refusal returns nonzero to fail only the job;
it never stops the listener, Docker daemon or another job.

Before installation, run the focused suite:

```sh
python3 -I -B scripts/ci/test_runner_admission.py
actionlint -config-file scripts/ci/actionlint.yaml .github/workflows/check.yml .github/workflows/architecture-documentation.yml
shellcheck --shell=sh scripts/ci/runner-admission.sh
```

For an authorized host update: verify runner identity and idle state; preserve
the current root-owned wrapper and its hash; install the reviewed policy first,
then atomically replace the wrapper; compare installed hashes and ownership.
Retain the existing confinement and Docker/network setup. Verify accepted and
rejected synthetic events under the runner identity, then a real PR job. A
rollback restores the exact saved files only after stopping new admissions
through the runner's normal service lifecycle, not UID-wide signals. Do not
automatically restore a known listener-killing hook.

## Merge authority and enforcement prerequisites

Only the human `karthik-sivadas` may merge. Agents may test, review, push feature
branches and prepare PRs; they must not merge, enable auto-merge or use an
administrator bypass. This overrides older operational merge authorizations.

On an eligible private-repository organization plan, configure main protection
to require the `foundation` and `architecture-documentation` checks against
the current base, require a PR and resolved review conversations, restrict
updates to `karthik-sivadas`, include administrators, and prohibit force-pushes
and deletion. Keep auto-merge off. Do not require the sole author's own approval:
independent AI review and the human merge decision are separate controls.

Private repositories on GitHub Free for organizations cannot enforce that
branch-protection policy. A paid organization plan or separately authorized
visibility change is required; documentation is not enforcement. An agent using
the human's GitHub credential is indistinguishable from that human to GitHub.
Strong human-only enforcement additionally needs a separate restricted agent
identity without permission to update main. Do not revoke shared credentials,
change billing or make the repository public as an implicit CI repair.

See GitHub's [protected branch documentation](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches)
and [runner hook behavior](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/run-scripts).
