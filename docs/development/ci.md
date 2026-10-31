# CI events and owner-account merge

## What runs where

| Where | What runs | Gate |
| --- | --- | --- |
| `pre-commit` hook | `gofmt`, `go vet`, Staticcheck and `go test` on the changed packages that need no stack; `gitleaks` on staged changes when it is installed | A failure stops the commit |
| `commit-msg` hook | [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/) check of the subject line | A failure stops the commit |
| `pre-push` hook | Refuses a push to `main`; `go build ./...`, `go vet ./...` and `go test` of every package that needs no stack | A failure stops the push |
| CI ([bdd.yml](../../.github/workflows/bdd.yml)) | The Gherkin acceptance suite in `tests/acceptance` | The only required check, `bdd` |

Install the hooks once per clone with `make hooks`. The hooks are developer checks. Go tests are not acceptance proof (OD-05); only the Gherkin suite is.

## The `bdd` check

The `bdd` workflow runs on every push to any branch of this repository by the owner account. A pull request shows the result of its head commit, because a status belongs to a commit. Pull request events do not start CI, so a fork pull request never reaches the self-hosted runners. Only collaborators can push branches or open pull requests.

The job checks out the exact pushed SHA and runs [check-bdd.sh](../../scripts/ci/check-bdd.sh) in the pinned job image. The script starts the job-owned dependency stack and runs `go test ./tests/acceptance/...`. Strict godog fails the run on an undefined or pending step. A failed suite is never retried.

A new push to a working branch cancels the older run for that branch. Runs on `main` are never cancelled. A force-push is admitted on a working branch and refused on `main`.

Missing prerequisites, test failures and cleanup failures remain failures.

[stack-packages.txt](../../scripts/ci/stack-packages.txt) lists the packages that need the dependency stack. The hooks skip them. [check-foundation.sh](../../scripts/ci/check-foundation.sh) still runs every package, including the stack packages, for a full local check.

## Agent branches

Agents push `agent/<issue>-<slug>` branches with git only and hold no GitHub API token. [agent-pr.yml](../../.github/workflows/agent-pr.yml) manages pull requests when `PM BDD` starts or finishes. It runs from the default branch on a GitHub-hosted runner. It never checks out agent code, downloads artifacts or runs pushed content.

The workflow creates a draft pull request while checks are pending. It marks the pull request ready for owner review only after the latest `PM BDD` run succeeds for the exact current branch revision. The `bdd` job in that run's latest attempt must also succeed. New work, reruns and failed checks return the pull request to draft when their workflow events arrive. Each event reconciles the current branch state, even if GitHub combines pending events. Delayed events cannot promote an older revision. A current revision without a BDD run stays draft. The workflow checks the branch again after promotion and restores draft if the head or checks changed.

This policy manages all open pull requests from this repository's `agent/**` branches, including an owner-created draft. Branches such as `codex/**` need manual readiness changes. Readiness signals that the current BDD gate passed. Successful CI does not prove that every task obligation is complete. The workflow has no separate task completion receipt. Readiness does not replace independent review or authorize a merge. Only the owner merges, manually and by squash. The workflow never merges or enables auto-merge. The owner must check the current revision and checks before merging.

The workflow needs `actions: read`, `contents: read` and `pull-requests: write`. It accepts only owner-authored push runs from this repository's `.github/workflows/bdd.yml`. It becomes active after this workflow change reaches the default branch. Existing completed runs do not replay automatically.

## Pinned job image and native environment

Every shard runs inside one job image built from
[build/ci/Dockerfile](../../build/ci/Dockerfile): a digest-pinned Ubuntu 24.04
base, packages from one fixed `snapshot.ubuntu.com` timestamp, the Docker CLI,
actionlint, ShellCheck and the hash-pinned setup-check Python packages. Go,
Clang, Bazel, Compose, Node and Chromium still come from `tools/bootstrap.py`
and the locked archives in `build/toolchains.lock.json`, cached on the runner.

[in-ci-image.sh](../../scripts/ci/in-ci-image.sh) tags the image
`localhost/pmcli-ci:<hash>`, where the hash covers the Dockerfile and
`build/ci/requirements.txt`. It builds the image once per runner host when that
tag is missing; no registry is used. It then starts one container with:

- the workspace, `RUNNER_TEMP` and the cache directory mounted at their host
  paths, because the stack fixture's bind-mount sources are resolved by the host
  engine;
- the rootless Podman socket mounted at `/var/run/docker.sock`;
- host networking, so the fixture's loopback-published ports are reachable;
- memory, CPU and process limits (`PMCLI_JOB_MEMORY`, `PMCLI_JOB_CPUS`,
  `PMCLI_JOB_PIDS`). Containers run under Podman's cgroup, not the runner
  unit's, so the unit's limits do not cover them.

Do not use the workflow `container:` key. It moves the workspace to `/__w`, and
the fixture's bind mounts then name paths that do not exist on the host.

The image is the native environment.
[build/native-environment.json](../../build/native-environment.json) is captured
inside it, and bootstrap verifies it before every native build. Host package
upgrades no longer cause drift. In a local test, two independent no-cache builds
of the image produced byte-identical profiles. To change the image, edit the Dockerfile (for
example a new snapshot timestamp), then recapture and review the profile in the
same PR:

```sh
sh scripts/ci/in-ci-image.sh run capture -- \
  python3 build/ci/capture-native-profile.py > build/native-environment.json
```

CI never refreshes the profile. A build that differs from the committed profile
fails at `bootstrap: native environment drift`.

## Operate a runner without sudo

The runner hosts are owner-controlled Ubuntu 24.04 machines. A runner runs as an
unprivileged user (for example `ghrunner`) under `systemd --user` with linger,
and uses rootless Podman. Rootless Docker is not used: Ubuntu's AppArmor policy
allows user namespaces for `rootlesskit` only at `/usr/bin/rootlesskit`, which
needs a root package install. Root was needed once, to create the user with its
`/etc/subuid` and `/etc/subgid` ranges and to install `podman`, `uidmap` and
`catatonit` (the `--init` process; a Podman recommendation).

As the runner user:

1. Enable linger for yourself: `loginctl enable-linger` (polkit allows this).
2. Start the Podman API socket: `systemctl --user enable --now podman.socket`.
   Check `podman info --format '{{.Host.Security.Rootless}}'` prints `true`.
3. Unpack the GitHub Actions runner in your home directory and check its
   published SHA-256.
4. Create a registration token with a repo-admin credential, on a machine that
   jobs cannot reach:
   `gh api -X POST repos/polymetrics-ai/pm-cli/actions/runners/registration-token`.
   Read it from stdin; do not store it on the host.
5. Register an ephemeral runner with the label `pm-cli-vps`, for example
   `./config.sh --unattended --ephemeral --url https://github.com/polymetrics-ai/pm-cli --name pmcli-<host>-<n> --labels pm-cli-vps`.
   GitHub adds `self-hosted`, `Linux` and `X64`. Do not reuse the retired
   `pm-cli-qual-20260911-r8` label.
6. Set the runner `.env`:
   - `PMCLI_RUNNER_UID=<your numeric uid>`
   - `XDG_RUNTIME_DIR=/run/user/<your numeric uid>`
   - `ACTIONS_RUNNER_HOOK_JOB_STARTED=<home>/pmcli-hooks/runner-admission.sh`
   - optional: `PMCLI_CACHE_DIR` (default `~/.cache/pmcli`, never under `/tmp`),
     `PMCLI_JOB_MEMORY` (default `8g`), `PMCLI_JOB_CPUS` (default `4`),
     `PMCLI_ENGINE_SOCKET` (default `$XDG_RUNTIME_DIR/podman/podman.sock`).
7. Run the runner from a `systemd --user` unit with `Restart=always`. An
   ephemeral runner exits after one job; the supervisor must register it again
   before the next job. Each registration needs a new token, so the token source
   is part of the runner setup and must not be readable by jobs.

The first job on a host builds the image and downloads the locked tools
(about 2.4 GB, mostly LLVM) into the cache. Later jobs reuse both. Concurrent
jobs on one host share the cache; bootstrap serializes installs with file locks.
Use at least two runner slots per host so that three shards can run at once.

If a fixture cleanup fails, PostgreSQL data owned by a mapped UID can stay in
`RUNNER_TEMP`. Remove it as the runner user with `podman unshare rm -rf PATH`;
no root is needed.

## Trust boundary without root

The old runner had a root-owned admission hook, a UID-scoped nftables egress
filter and a system unit sandbox. A sudo-less runner has none of these. The
listener, hook, job container and every Podman container run as one host UID,
and the Podman socket gives a job full control of that user's containers.

Mitigations:

- Only events from `karthik-sivadas` on this repository run: open same-repo PRs
  against `main` and non-forced pushes to `main`. Both the workflow `if:` guards
  and the admission hook check this before any step runs. Keep fork PR workflows
  disabled.
- Workflows have `contents: read`, use no secrets and check out with
  `persist-credentials: false`.
- Runners are ephemeral, so a stolen runner registration is good for one job.
- The host should run only the pm-cli runner (OD-11).

Residual risk: an admitted job can change user-owned files for later jobs (the
hook, runner binaries, the cache and the image), and it has unrestricted
network egress. Only reviewed owner code is admitted, but a compromised test
dependency runs with these rights. A full fix needs a root-created second
account or a host egress rule.

## Runner admission hook

The reviewed [wrapper](../../scripts/ci/runner-admission.sh) and
[policy](../../scripts/ci/runner_admission.py) are copied together into the
runner user's hook directory, outside the checkout, and named by
`ACTIONS_RUNNER_HOOK_JOB_STARTED`. They run with `/usr/bin/python3`.

Admission requires that the hook runs as the UID named by `PMCLI_RUNNER_UID`
(never root), and checks the repository, original and rerun actor, payload
sender, workflow path, event ref and GitHub's server-defined `GITHUB_SHA`. The
direct workflow's `GITHUB_WORKFLOW_SHA` must equal `GITHUB_SHA`; checkout
consumes that exact SHA. Admission makes no live REST lookup. Only `bdd.yml` on a push event is admitted. The ref must be a branch (`refs/heads/...`), equal the event ref and point at `GITHUB_SHA`. A deleted ref and a force-push to `main` are refused. Pull request events, other actors, refs, events and workflows are refused. Input size and execution
time are bounded. Refusal returns nonzero to fail only the job; it never stops
the listener, the Podman service or another job.

Before installation, run the focused suite:

```sh
python3 -I -B scripts/ci/test_runner_admission.py
actionlint -config-file scripts/ci/actionlint.yaml .github/workflows/*.yml
shellcheck --shell=sh scripts/ci/runner-admission.sh
```

Synthetic admission is not live workflow proof; exercise a real push job after
each hook update.

## Retire the old runner

The `pm-cli-qual-20260911-r8` runner on polymetrics-origin takes no new jobs:
no workflow names its label. Retire it only after `main` is green on the new
runners and `foundation` is required.

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
to require the `foundation` check against
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
