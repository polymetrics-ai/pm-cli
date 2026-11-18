# CI events and owner merge

## What runs where

| Where | What runs | Gate |
| --- | --- | --- |
| `pre-commit` hook | Required Gitleaks scan of staged changes. `gofmt`, `go vet`, Staticcheck and `go test` on changed packages that need no stack. `check-setup.py` when a staged path is under `scripts/`, `.github/`, `.agents/`, `tools/`, `build/ci/` or `docs/development/`, or is `AGENTS.md`. A change that only adds Go files runs no check from `check-docs.sh` in this hook, unless a Go file is under `tools/`, `scripts/` or `build/ci/`. Such a file selects `setup` | A failure stops the commit |
| `commit-msg` hook | [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/) check of the subject line | A failure stops the commit |
| `pre-push` hook | Refuses a push to `main`. Scans outgoing history with Gitleaks. Runs `go build ./...`, `go vet ./...` and `go test` of every package that needs no stack. Runs [check-docs.sh](../../scripts/ci/check-docs.sh), the `checks` job, one time for each push, without the `unit` check, because the hook ran those packages already. On macOS, it runs these six checks on the host | A failure stops the push |
| CI ([bdd.yml](../../.github/workflows/bdd.yml)) | Job `bdd`: the stack run of the Go developer tests, then the Gherkin acceptance suite in `tests/acceptance` on a fresh stack. Job `checks`: the static checks and the unit run of the Go developer tests in `check-docs.sh` | `bdd` is a required check. `checks` becomes a required check only by an owner decision |

Install the scanner once per user. Install the hooks once per clone:

```sh
python3 -I -B scripts/dev/secret_scan.py install
make hooks
```

The hooks are developer checks. Go tests are not acceptance proof (OD-05). Only the Gherkin suite is acceptance proof. The `checks` job runs static checks and Go developer tests, not behavior tests. The stack run in the `bdd` job also runs Go developer tests. A pass of a Go test run in CI is not acceptance proof of PM behavior.

On Linux, the `pre-push` hook runs inside the pinned job image through `sh scripts/ci/in-ci-image.sh dev -- COMMAND...`. The `pre-commit` hook selects its checks on the host first. It enters the image only when the staged change has a Go file or a path that a check above names. Other commits run only the secret scan, on the host, and start no container. On Linux, bootstrap accepts only the native environment of that image (see [Pinned job image and native environment](#pinned-job-image-and-native-environment)). Developer entry checks the host scanner's checksum and mounts the binary read-only at `/opt/pmcli-gitleaks`. A hook that enters the container scans inside the container. The scanner mount does not change the image or its native profile. Generic developer commands do not scan the index. On macOS, the hooks run on the host (see [macOS hosts](#macos-hosts)). The `commit-msg` hook always runs on the host.

The hooks treat `PMCLI_IN_IMAGE=1` as "already inside the image" only when `/run/.containerenv` exists. Podman creates that file in each container. On a host, the hooks ignore the variable and enter the image.

The first hook run on a Linux host builds the image and downloads the locked tools into the cache (`PMCLI_CACHE_DIR`, default `~/.cache/pmcli`). Later runs reuse both. The host needs rootless Podman (`PMCLI_CONTAINER_ENGINE`, default `podman`). The developer container mounts the checkout, the cache and the Git common directory of a linked worktree at their host paths. It uses the default network and has no engine socket, so a hook cannot start the dependency stack. It uses the same user and the same `PMCLI_JOB_*` limits as a CI job.

### macOS hosts

On macOS, the hooks run on the host. The `pre-push` hook, and a `pre-commit` hook that runs `check-docs.sh`, need these items:

- Python 3.11 or later that imports `tomllib`, `yaml`, `markdown_it` and `mdurl`. [requirements.txt](../../build/ci/requirements.txt) pins the versions of PyYAML, markdown-it-py and mdurl that CI uses. `tomllib` is in the standard library from Python 3.11. `test_secret_scan.py` imports it.
- ShellCheck and actionlint on `PATH`, for `check-setup.py`.
- The Gitleaks scanner (see [Required developer secret scans](#required-developer-secret-scans)).
- An Apple macOS SDK for cgo, as [developer setup](setup.md) describes.

The hooks select the Python interpreter for `check-docs.sh`. They try `python3`, `python3.13`, `python3.12` and `python3.11` on `PATH`, in that order. They select the first one that is Python 3.11 or later and imports the four modules. The hook prints the version of that interpreter and gives its name to `check-docs.sh` in `PMCLI_PYTHON`. If no interpreter qualifies, the hook prints the prerequisite and stops the commit or push. The hook does not skip the `python` check. Other Python commands still use the first `python3` on `PATH`. Examples are the secret scan, `generate.sh` and `check-boundaries.sh`. CI ignores `PMCLI_PYTHON` and uses `python3`.

The macOS hooks skip no check. The `pre-push` hook runs the unit packages and the other six checks of the CI `checks` job, and it prints no skip notice.

## Required developer secret scans

[secret_scan.py](../../scripts/dev/secret_scan.py) pins the official Gitleaks 8.30.1 archives and binaries by SHA-256. It supports Linux x86_64 and Apple Silicon macOS. The installer writes `~/.local/bin/gitleaks`. Each scan checks the binary checksum before execution. `PMCLI_GITLEAKS` can select another path, but that binary must match the platform's pin. No scan downloads tools automatically. An unavailable scanner, wrong checksum, scan error or finding blocks the Git operation.

The commit scan reads the Git index, including `GIT_INDEX_FILE` and the temporary index from `git commit -a`. It scans full changed blobs through a private temporary snapshot. Git attributes and diff drivers cannot replace those bytes. An unstaged edit does not replace the staged content. Direct hook execution inside a developer container uses the same required scan. `PMCLI_IN_IMAGE=1` does not disable scanning.

The push scan checks each outgoing range from the remote commit to the local commit. A new branch or unavailable remote commit causes a scan of all history reachable from the local commit. This includes secrets removed before the branch tip. The scan includes merge diffs and disables text conversion, external diff drivers and replacement objects. Shallow history blocks a push until you fetch the missing history. Grafted history also blocks a push. Ref deletion needs no content scan. Each scan has a ten-minute timeout.

The helper uses the pinned default detection rules and fully redacted diagnostics. Its [reviewed configuration](../../scripts/dev/gitleaks.toml) has exact exceptions for known nonsecret values. Each exception requires the rule, exact file and exact value to match. These values are S3 object names, a run identifier, an explicit documentation placeholder and a public GitHub documentation sample. No exception excludes an entire file or directory.

Local `.gitleaks.toml`, environment configuration, `.gitleaksignore` and `gitleaks:allow` comments cannot suppress findings. Investigate each finding before changing the reviewed configuration. Do not add broad exceptions for tests or captured documents.

Run the developer checks with the real scanner:

```sh
python3 -I -B scripts/dev/test_secret_scan.py
```

These checks use temporary Git repositories, actual hooks and an invalid synthetic token. They check index selection, missing or changed scanners, scan errors and outgoing history. They are separate from the required Gherkin CI gate.

Secret detection is pattern-based and cannot prove that content has no credentials. Existing history can block a new branch. Review suspected false positives individually. A user who controls the clone can change or disable hooks. GitHub secret scanning and push protection require separate repository settings. If a real credential was published, revoke or rotate it before any history repair.

## The `bdd` check

The `bdd` workflow runs on every push to any branch of this repository by the owner account. A pull request shows the result of its head commit, because a status belongs to a commit. Pull request events do not start CI, so a fork pull request never reaches the self-hosted runners. Only collaborators can push branches or open pull requests.

The job checks out the exact pushed SHA and runs [check-bdd.sh](../../scripts/ci/check-bdd.sh) in the pinned job image. The script runs two Go test runs. Each run gets its own job-owned dependency stack:

1. The stack run: the Go developer tests of the stack packages. The script starts a stack and runs `go test -json -p 1 -count=1 -timeout 15m` of these packages. The packages share one schema, its roles and one Temporal history, so they run one package at a time.
2. The Gherkin run: the script starts a fresh stack and runs `go test -json -p 1 -count=1 -timeout 40m ./tests/acceptance/...`. No stack-run test runs on this stack. Strict godog fails the run on an undefined or pending step. A failed suite is never retried. Before the stack run, the script takes the intended set of the Gherkin run from `go list ./tests/acceptance/...`. It uses the same pattern and the same environment as the run. The report checks the set with `--expect`. If that selection fails or selects no package, the Gherkin run does not run, the log says so, and the job fails.

The Gherkin run also runs when the stack run fails, and the stack run does not stop for a Gherkin failure. The job fails when either run fails. A stack run command that ends on a signal (exit 129, 130 or 143) is a stack run failure. The log says so, and the receipt check and the Gherkin run still run. A job cancellation is different. The container init passes the signal to `check-bdd.sh`, which sets no trap, and `sh` in the CI image (dash) stops the script at once. So a cancelled script does not reach the Gherkin run. The log shows two fixture starts, one for each run. The script compares the toolchain identity before the first run and after the last run.

The stack run is a developer check. It is not acceptance proof of PM behavior. A failed test fails the run. A test that calls `t.Skip` also fails the run, and the log and the job summary name it. Before the stack run, the script builds `.cache/bin/pm-api-generate` from the candidate and unsets `PM_API_GENERATE`, because the `internal/api` tests use the generator at that default path. Each step line of the log shows the seconds since the script started, so the log shows the order of the phases.

### Time budget of the `bdd` job

The frozen step limit is 90 minutes, and the frozen job limit is 100 minutes. `check-bdd.sh` keeps 3 minutes of the step for the start and stop of the job container. So its own work ends within 87 minutes of its start, also on the failure path. The owner decided on 2026-11-17 that the stack go test command stops one hour after the start. It stopped after 20 minutes before, and a cold build with the Keycloak start could use up that time. `go-test-report.py --deadline` stops the go test command of each run at an absolute time, and the run still reports. The fixture start before that command and the fixture cleanup after it have their own phase deadlines (see [Fixture phase deadlines](#fixture-phase-deadlines)). `--timeout` also stops each go test command after its own start. The limit is 3590 seconds for the stack run and 1140 seconds (19 minutes) for the Gherkin run. Each limit plus the 60-second stop grace (see below) ends before the 61-minute command limit of `with-stack-fixture.py`. So the reporter, not the wrapper, ends the run. The stack `--timeout` can stop the stack command before its deadline only when that command starts less than 10 s after the script. The tool builds and the stack fixture start come before that command. The `-timeout 15m` of the stack run does not change. It limits one package test binary. A stopped run gets `SIGTERM`, then `SIGKILL` after 30 seconds, and exit status 124. When the reporter itself receives `SIGTERM`, `SIGINT` or `SIGHUP`, it passes the signal to the process group of the command. It waits up to 30 seconds for the command to stop, then sends `SIGKILL`. It prints `the reporter received SIG... and stopped the command` and exits with 128 plus the signal number. This also happens when the command exits 0. When a command ends, the reporter stops each process that is left in its process group. It sends that `SIGKILL` before it reaps the command. Until the reap, no new process or process group can get the ID of the command. After that `SIGKILL`, the reporter sends no more signals to the group. A signal that the reporter receives later is only recorded, and it still fails the run. It waits for the end of the command with a pidfd (Linux) or a kqueue process filter (macOS), which do not reap it. The system can refuse a signal to the process group (permission denied). Then a test process can still run. So the run fails, and the log names the refused signal. On Linux, and so in CI, each refusal fails the run. macOS also refuses a signal to a group whose only member is the command after it has exited and before the reporter reaps it. So only on macOS, which runs the developer hooks, a refusal does not always fail the run. It does not fail the run when the command has exited and `ps` shows no live member of the group.

After the last `SIGKILL` and before the reap, the reporter also lists the live members of the group. On Linux it reads `/proc`, and on other systems it reads `ps`. Linux reports success for a group signal when one member gets it. So a live member that the reporter may not signal (permission denied) did not get the `SIGKILL`. Such a member fails the run with a fixed message, and so does a member list that the reporter cannot read. The message names no process and no command line. Each wait after the last `SIGKILL` has a limit of 30 seconds, because the system can refuse that `SIGKILL`. A command that has not ended by then is not reaped, so its ID stays reserved, and the run fails with a cleanup message.

So a stopped go test command can end up to 60 s after its stop. That is 30 s of `SIGTERM` grace and a bounded wait of up to 30 s after the `SIGKILL`. This is the 60 s stop grace of the budget. The member list and the report writing come in addition. On Linux, the member list reads `/proc`, and nothing limits that read. On other systems, `ps` has a limit of 10 s, and its collection after a kill has a limit of 5 s. Nothing limits the report writing.

The table gives each part of the run step on the script clock, with #358's limits and the allowances of #370. P is the end of the prelude (row 1).

| Row | Part of the run step | Limit | Enforced |
| --- | --- | --- | --- |
| 0 | Image build before the script, only when the image is missing | none | no. This is E4 exception 2. A cold build takes about 16 minutes |
| 1 | Prelude: image check, tool builds, `go list`, package selection | none. The budget assumes 8 minutes (480 s) or less | no |
| 2 | Stack fixture start | 720 s phase deadline, plus up to 10 s after a `SIGKILL` (730 s allowance) | yes |
| 3 | Stack `go test` | stops at +3600 s, then 60 s stop grace | yes |
| 4 | Stack fixture cleanup | 360 s phase deadline, plus up to 10 s after a `SIGKILL` (370 s allowance) | yes |
| 5 | Receipt check, with no Docker call | 15 s finish allowance. It also holds the member list and the report writing of the stack run | no. The receipt check and the member list take less than 1 s. The report writing is not measured |
| 6 | Gherkin fixture start | 720 s phase deadline, plus up to 10 s after a `SIGKILL` (730 s allowance) | yes |
| 7 | Gherkin `go test` | stops at +4775 s or after 19 minutes, then 60 s stop grace | yes |
| 8 | Gherkin fixture cleanup | 360 s phase deadline, plus up to 10 s after a `SIGKILL` (370 s allowance) | yes |
| 9 | Finish: toolchain identity, summary | 15 s finish allowance. It also holds the member list and the report writing of the Gherkin run | no. The finish and the member list take less than 1 s. The report writing is not measured |
| — | Workflow cleanup step: container stop, cleanup container start, `--cleanup` | 6-minute step limit. `--cleanup` has a 300 s phase deadline, plus up to 10 s after a `SIGKILL` | yes, for `--cleanup` |

Rows 2 to 8 have a limit. The arithmetic, with rows 5 and 9 at their full 15 s finish allowance:

- Row 2 ends by P + 730.
- Row 3 stops at the later of +3600 and the end of row 2. It ends 60 s later: max(3600, P + 730) + 60.
- Rows 4, 5 and 6 add 370, 15 and 730. So row 6 ends by max(3600, P + 730) + 1175 = max(+4775 s, P + 1905 s).
- Row 7 stops at the later of +4775 and the end of row 6, and ends 60 s later. Row 8 adds 370.
- So row 8 ends by max(+5205 s, P + 2335 s). The 2335 s are 730 + 60 + 370 + 15 + 730 + 60 + 370.
- Row 9 adds 15 s. So the script ends by max(+5220 s, P + 2350 s). With P of 2870 s or less, this is +5220 s, the budget. The budget adds 3 minutes for the job container (5400 s, the 90-minute step).

Before #370, the budget used a 30 s stop grace, the bare 720 s and 360 s phase deadlines and a 60 s finish allowance. With the real stop and phase times, its longest path ended at +5310 s, 90 s after the end of the step.

Rows 0, 1, 5 and 9 have no enforced limit. The member list and the report writing of each run also have no enforced limit. Together with row 5 or row 9, they must fit the 15 s finish allowance. E4 exception 2 stays: a cold image build, or a long prelude together with phases at their deadlines, can pass the step limit. Then the Gherkin run may not report, and the frozen cleanup step still removes the live fixture (see [Cleanup after an abrupt stop](#cleanup-after-an-abrupt-stop)). The design (v3 §4.5) gave max(+2760 s, P + 2160 s) for the old +1200 s stop, without the two stop graces.

The stack go test command stops 3600 s (60 minutes) after the start. The Gherkin go test command stops 4775 s after the start. On the longest path, the stack run ends by +3660 s and its cleanup by +4030 s. The receipt check ends by +4045 s and the Gherkin fixture start by +4775 s. The Gherkin run ends by +4835 s, its cleanup by +5205 s and the toolchain check by +5220 s. So the script ends 87 minutes after the start in the worst case. With the 3 minutes for the job container, the step ends by 90 minutes. With the 6-minute cleanup step and 4 minutes for the checkout and the runner, the job ends by 100 minutes.

The script keeps no reserve of Gherkin test time (`gherkin_minimum` is 0). On one path, the Gherkin run can get no test time. On that path, the stack run reaches its deadline. Then the stack cleanup uses its full 370 s, and the Gherkin fixture start uses its full 730 s. The Gherkin run then reports that it stopped at its deadline, and the job fails. E4 is narrowed on that path. On a normal run, the Gherkin go test command keeps its own 19-minute limit (`--timeout 1140`).

#### Fixture phase deadlines

[with-stack-fixture.py](../../scripts/ci/with-stack-fixture.py) has one deadline for each whole fixture phase, on a monotonic clock:

- The fixture start: 720 s, from the private fixture directory to the running fixture.
- The fixture cleanup in the run step: 360 s.
- The fixture cleanup of `--cleanup` in the workflow cleanup step: 300 s. The 6-minute step also stops the job container (`stop -t 10`) and starts the cleanup container. The other 60 s are for these, or 50 s when `--cleanup` ends 10 s after its deadline.

Each Docker call and helper call in a phase gets the smaller of its own limit and the time that is left in the phase. Examples of own limits are 30 s for one Docker call and 600 s for `pm-stack up`. The stop grace of a stopped call is also limited to the time that is left. After the `SIGKILL`, the wrapper waits up to 10 s (`PROCESS_TERM_GRACE`) for the end of the call. The phase does not limit that wait. So a phase can end up to 10 s after its deadline. The start ends by 730 s, the cleanup in the run step by 370 s and `--cleanup` by 310 s. A phase that reaches its deadline fails with `the fixture <phase> phase reached its deadline (<limit> s)`. A call that reaches its own limit prints `<stage> timed out: the call reached its own limit of <limit> s`. So the log tells the two causes apart. After a start deadline, no command runs on that fixture. The manifest stays for the workflow cleanup step. After a cleanup deadline, its phase is `cleanup_failed` with the code `cleanup-phase-deadline`.

The wrapper stops the process group of a call that reached its limit. It sends `SIGTERM` to the group and waits for the exit of the call process without a reap. Then it sends `SIGKILL` to the group, and only then it reaps the call process. It uses the same pidfd or kqueue method as the reporter. So no new process or process group can get the ID before the last group signal. A call process that something already reaped is refused, with no signal.

E4 exception 4 of #278 is slow Docker calls that each end in time. The phase deadlines end it for rows 2, 4, 6 and 8 and for `--cleanup`. Each phase now ends by its deadline, plus up to 10 s to stop a call after its `SIGKILL`.

The phase deadlines replace the breakdown below, from before issue #286. The figures are conditional command allowances, not deadlines. Each sum holds only when each call ends just inside its own limit. It also needs the resource counts that the table states.

One resource discovery makes 13 list calls and up to 12 inspect calls: up to 25 calls, or 750 s. The inspect calls are for the stack containers, the cleanup helper and the network.

| Fixture start part | Allowance |
| --- | --- |
| Daemon check | 30 s |
| Generator | 60 s |
| Discovery before the start, on an empty project: 13 calls | 390 s |
| `pm-stack up` | 600 s |
| Discovery after the start: 22 calls | 660 s |
| Total | 1,740 s (29 minutes) |

| Fixture cleanup part | Allowance |
| --- | --- |
| Daemon check | 30 s |
| Five discoveries | 750, 750, 420, 390 and 390 s |
| Helper stop and removal | 60 s |
| Drain | 120 s |
| Container stop, and up to 10 container removals | 75 s and 300 s |
| Network removal | 30 s |
| PostgreSQL cleanup helper: create 30 s, inspect 30 s, start 60 s, remove 30 s | 150 s |
| Total | 3,465 s (57 minutes 45 seconds) |

The log has one `starting a fresh job-owned stack` line before each fixture start. A cold image build happens in the same step before `check-bdd.sh` starts, and this budget does not include it. On that path, the worst case can pass the step limit, and the Gherkin run may not report. The frozen cleanup step then still removes the live fixture (see the next section).

### Cleanup after an abrupt stop

The frozen cleanup step of `bdd.yml` stops the job container and runs `with-stack-fixture.py --cleanup` on one manifest path: `$RUNNER_TEMP/pmcli-bdd-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json`. The wrapper writes its manifest at that path before it lets Docker start anything. So each live fixture is always under that path, and the cleanup step removes it after an abrupt stop. The stack run uses the path first. When its fixture is clean, the manifest stays as a receipt in the phase `cleaned`. `check-bdd.sh` checks the receipt with `with-stack-fixture.py --check-receipt`, which only reads the manifest, checks the phase `cleaned` and makes no Docker call. It then moves the receipt to `pmcli-bdd-<run>-<attempt>.stack-run-receipt.json`. The rename is in one directory, so the path holds either the cleaned receipt or no file, and no fixture is live. The Gherkin run then writes a new manifest at the same path.

If the stack fixture is not clean, the receipt check fails, and the manifest stays at the path for the cleanup step. `check-bdd.sh` does not repeat the cleanup. The Gherkin run does not run, the log says so with the reason, and the job fails. Both runs report only on the other paths.

The `--cleanup` of the cleanup step has its own 300 s phase deadline (see [Fixture phase deadlines](#fixture-phase-deadlines)). This is the end state when it reaches that deadline:

- `--cleanup` exits 1. The cleanup step fails, and so the job fails.
- The wrapper sets the manifest phase to `cleanup_failed`, with the diagnostic code `cleanup-phase-deadline`. If that manifest write also fails, the manifest keeps its earlier phase. Another cleanup failure also gives the phase `cleanup_failed`, with its own code.
- The fixture can stay on the runner host: its containers, its network, its volumes and its private folder (`pm-stack-job-*` in `RUNNER_TEMP`). No later step of the job removes them.
- If the 6-minute step limit stops the step first, the manifest keeps the phase that it had. The job also fails.

GitHub documents that the runner empties `RUNNER_TEMP` at the start and at the end of each job. So the manifest can be gone after the job. Then the owner, or an operator with host access, acts in this order:

1. If the manifest is still there, run the cleanup from a shell on the runner host. Use the same CI-image entry as the workflow cleanup step:

   ```sh
   sh scripts/ci/in-ci-image.sh run bdd-cleanup -- python3 scripts/ci/with-stack-fixture.py \
     --cleanup "$RUNNER_TEMP/pmcli-bdd-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json"
   ```

   A direct `with-stack-fixture.py --cleanup` on the host does not work. The wrapper uses the socket `/var/run/docker.sock`, which exists only in the job container. It also checks the manifest against `RUNNER_TEMP` and the run identity. The command has these prerequisites:

   - It runs as the original runner user.
   - It runs from the checkout at the repository root that the manifest records, at the same commit. This root is inside `GITHUB_WORKSPACE`. The wrapper refuses a manifest that records another root, and it runs the operator `pm-stack` from that root.
   - The operator `.cache/bin/pm-stack` of that commit is in that checkout. When the checkout of the job still holds it, do not build it again. Else build it through the CI image, with the same run variables set:

     ```sh
     sh scripts/ci/in-ci-image.sh run bdd-build -- sh -c \
       'GOCACHE="$PMCLI_CACHE_DIR/go-build" GOMODCACHE="$PMCLI_CACHE_DIR/go-mod" CGO_ENABLED=0 python3 tools/bootstrap.py exec go -- build -o .cache/bin/pm-stack ./cmd/pm-stack'
     ```

     This is the `pm-stack` build of `check-bdd.sh`, with the same Go caches. When stack containers stay, the cleanup runs `pm-stack drain --dependency all`. Without the operator, the cleanup records `operator-drain`, and the fixture can stay. Then step 2 applies.
   - `GITHUB_RUN_ID`, `GITHUB_RUN_ATTEMPT`, `GITHUB_WORKSPACE` and `RUNNER_TEMP` have the values of that run. `RUNNER_TEMP` is the existing folder that holds the manifest.
   - The Podman socket of the runner user is available: `PMCLI_ENGINE_SOCKET`, else `$XDG_RUNTIME_DIR/podman/podman.sock`, else `/run/user/<uid>/podman/podman.sock`.
   - `PMCLI_CACHE_DIR` has the value from the runner `.env`, or it is unset when the runner uses the default under `HOME`. Then the cleanup container finds the pinned Compose in the same cache.
2. If the manifest is gone, or a prerequisite is not available, find the fixture by its project label (`pmci-*`) in a host health read. The owner decides on the removal, and the operator then removes the fixture by hand.

[go-test-report.py](../../scripts/ci/go-test-report.py) `--scenarios` lists each scenario with its result and the godog totals (`N scenarios (...)`, `N steps (...)`). It does this also when the suite passes. The list is at the end of the `bdd` job log and in the job summary on the workflow run page. A passing run fails when the godog scenario total is missing or differs from the number of scenarios in the test stream.

The Gherkin run also fails in these cases (issue #350). The log and the job summary name each cause:

- godog reports 0 scenarios: `0 scenarios ran`.
- The godog step total shows skipped steps: `the godog step total has <N> skipped steps`.
- A Go test or subtest of the Gherkin package calls `t.Skip`. The Gherkin run has `--fail-on-skip`. The log prints `skipped test fails this run: <package> <test>`.

Only these totals and skip events fail the run. When the step total shows skipped steps, the log and the summary also name the cause:

- `skipped step: <scenario> / <step>` for each skipped step. godog v0.16.0 prints a skipped step in cyan, and the reporter finds it by that color.
- `skipped scenario: <scenario>` for a scenario whose steps are all skipped.

godog also prints the template steps of a Scenario Outline in cyan, whatever their result. So the reporter takes no names from an outline. A skipped step of a Scenario Outline, or of a Background after the first scenario, fails the run only through the step total. The reporter does not name such a step.

For the stack run and the unit run, `go-test-report.py --title` adds a separate section to the job summary. The section has the Go test totals, each package with its result and time, each failed or skipped test and each problem. The Gherkin run has no `--title`, so its scenario section lists each test with no result, each skipped test and each problem. The godog totals stay in that section. `--expect` gives the import paths that the run selected from `go list` before the run. The log and the summary give the size of this intended set and the number of package results. Each one must have exactly one package result. A missing, duplicated or unexpected package result, an empty stream or a line that is not a JSON event fails the run. A test or subtest can start (a `run` event) and have no result (no `pass`, `fail` or `skip` event). Such a test also fails the run, also in a package that passes. An example is a test whose goroutine panics: the test binary exits, and the panic output belongs to that test. The log prints the test as `FAIL (no result)` with its output, the problem list names it, and the job summary lists it. A test that pauses and continues, and then gets a result, is not such a test. The two runs use no `-run` and no build-tag filter. With `--expect`, a guard accepts only the options `-json`, `-p`, `-count`, `-timeout` and `-race`. The first four are the options of the CI commands. `-race` is in the list for the stack run of #312, and today no CI command uses it. The guard refuses each other option before the command runs. Examples are `-exec`, `-overlay` and `-toolexec`, which can change what runs (crrao N1), and the test filters `-run`, `-skip`, `-list`, `-short`, `-failfast` and `-tags`. It reads the forms `-name`, `--name` and `-test.name`, with `=value` or, in the command, with the value as the next word. It also refuses `-args`. It splits `GOFLAGS` at the characters that go splits it at (space, tab, newline, carriage return). go reads a quoted `GOFLAGS` field without its quotes, so the guard refuses a `GOFLAGS` value with a quote character. It also refuses a `GOFLAGS` field that is not an option, and a `GOFLAGS` option with a value that has no `=value` form, which go also refuses. It does not read a `GOFLAGS` value from the go environment file (`go env -w`). The command and `GOFLAGS` together must set the test count exactly once, to 1 (`-count=1`). A nonzero exit of the command, of the fixture wrapper or of its cleanup fails the run. The `bdd` job summary keeps this section apart from the Gherkin section with the godog totals. With `--fail-on-skip`, a skipped test fails the run. A package with no test files shows as `SKIP (no test files)`. That is not a test skip, and it does not fail the run.

A new push to a working branch cancels the older run for that branch. Runs on `main` are never cancelled. A force-push is admitted on a working branch and refused on `main`.

Missing prerequisites, test failures and cleanup failures remain failures.

### Go test package selection

[stack-packages.txt](../../scripts/ci/stack-packages.txt) is the one list of the packages that need the dependency stack. [go-packages.sh](../../scripts/ci/go-packages.sh) holds the selection rule. The `checks` job, the `bdd` job and the hooks read the list only through it. Each root-module package that `go list ./...` prints goes to one place:

| Place | Packages |
| --- | --- |
| Unit run (`checks` job) | Every package that is not in the stack list and not excluded |
| Stack run (`bdd` job) | Every stack list entry except `tests/acceptance` and the excluded packages |
| Gherkin run (`bdd` job) | `tests/acceptance` |
| Exclusion list | `tests/client-parity`. It also runs the Playwright browser tests, which need a browser as well as the stack. Issue #274 runs it in CI. The `bdd` job log prints this name and reason |

The rule selects the packages, so a new package goes to the unit run with no list edit. A package that needs the stack fails in the unit run until you add it to the stack list. The stack tests fail, and do not skip, without their fixture inputs. A stack list entry that `go list ./...` does not print fails the `bdd` job, and the log names the path. The hooks skip the stack packages and the excluded packages.

[check-foundation.sh](../../scripts/ci/check-foundation.sh) has its own package lists. No workflow runs it. The shards need a GitHub Actions environment. A developer cannot run them locally as they are.

## The `checks` check

The `checks` job is in the same workflow as `bdd`. It has the same admission condition, runner labels and pinned checkout. It runs in parallel with `bdd` and does not wait for it. It runs [check-docs.sh](../../scripts/ci/check-docs.sh) in the pinned job image. The script first builds `.cache/bin/pm-api-generate` and `pmgen` from the candidate with the pinned toolchain. Then it runs these seven checks:

- `setup`: `check-setup.py` (required files, links, skills, ShellCheck, actionlint and the frozen `bdd.yml`).
- `python`: `test_setup.py`, `test_runner_admission.py`, `test_stack_fixture.py`, `tools/test_bootstrap.py` and `test_secret_scan.py`.
- `generate`: `generate.sh --check` for the API and the connectors.
- `gofmt`: `gofmt -l` on every tracked Go file.
- `boundaries`: `check-boundaries.sh` (the native import boundary and the store dependency rule).
- `tools`: `go test` for the tools module.
- `unit`: the unit run. `go test -json -count=1 -timeout 10m` of every root-module package outside the stack list and the exclusion list (see [Go test package selection](#go-test-package-selection)). `go-test-report.py --timeout 600` stops the run after 10 minutes and still reports. A failed or skipped test fails the check, and so does a missing or duplicated package result. The log and the job summary list each package with its result and time. These tests are developer checks, not acceptance proof. The `pre-push` hook does not select `unit`, because it runs the same packages itself (`pm_push_checks` in `scripts/dev/hook-lib.sh`).

In CI, the script installs the pinned, checksummed Gitleaks binary into the job cache when the cache has no binary. Run all checks locally with `sh scripts/ci/in-ci-image.sh dev -- sh scripts/ci/check-docs.sh`. To run some checks, give their names, for example `check-docs.sh setup boundaries`. `check-docs.sh --list` prints the check names. The hooks read the names from it. Browser generation, stack-package Go tests, SBOM and licence reports, Bazel, image pins, `go vet`, Staticcheck and govulncheck are not in this job. The stack-package Go tests run in the `bdd` job.

Issue #64 removed the `atlas` and `docs` checks. A Go-only change needs no ownership record and no new Foundation Atlas projection. The store dependency rule moved from Atlas to `check-boundaries.sh`, and it stays a blocking rule. No non-test package under `internal/store` may depend on `internal/app`, `internal/auth` or their subpackages, directly or through another package. For a violation, the check writes `Store depends on a consumer package: <store package> -> <consumer package>` and exits with code 1.

`tools/atlas`, `scripts/validate_architecture_docs.py` and `scripts/test_validate_architecture_docs.py` stay as history. No CI job, git hook, Makefile target or local script runs the validator, its test or the Atlas check. The `tools` tests in `tools/traceability_test.go` build Atlas and run it only against a synthetic fixture in a temporary directory, not against the repository. Issue #268 removed the `make test-docs` target, so `make check` and `scripts/test-changed.sh` do not select it. The `governance` shard of `scripts/ci/check-foundation.sh` keeps its Bazel build and test, `check-dependencies.py` and `benchmark.py`.

Run by hand, the validator and its test fail on the live tree. Issue #64 changed two frozen files under `docs/architecture/pm-server/`: its `README.md` and its engineering and review policy. These files no longer match their digests in `IMPORT-MANIFEST.json`. Run by hand, the Atlas check fails for a Go file that has no ownership record.

## Agent branches

Agents push `agent/<issue>-<slug>` branches with git only and hold no GitHub API token. [agent-pr.yml](../../.github/workflows/agent-pr.yml) manages pull requests when `PM BDD` starts or finishes. It runs from the default branch on a GitHub-hosted runner. It never checks out agent code, downloads artifacts or runs pushed content.

The workflow creates a draft pull request while checks are pending. It marks the pull request ready for owner review only after the latest `PM BDD` run succeeds for the exact current branch revision. The `bdd` and `checks` jobs in that run's latest attempt must both succeed for that revision. New work, reruns and failed checks return the pull request to draft when their workflow events arrive. Each event reconciles the current branch state, even if GitHub combines pending events. Delayed events cannot promote an older revision. A current revision without a BDD run stays draft. The workflow checks the branch again after promotion and restores draft if the head or checks changed.

When this rule is on `main`, `agent-pr.yml` keeps a pull request in draft if the `PM BDD` run for its head has no `checks` job. A branch that started before this rule must merge `main` first.

This policy manages all open pull requests from this repository's `agent/**` branches, including an owner-created draft. Branches such as `codex/**` need manual readiness changes. Readiness signals that the current `bdd` and `checks` gates passed. Successful CI does not prove that every task obligation is complete. The workflow has no separate task completion receipt. Readiness does not replace independent review or authorize a merge. Only the owner merges, manually and by squash. The workflow never merges or enables auto-merge. The owner must check the current revision and checks before merging.

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

Only the owner merges, by squash. An agent does not merge and does not use the
owner's GitHub account to merge. Missing, skipped, stale, different-head or
failed required checks do not qualify a pull request for merge. Do not use an
administrator bypass or force a merge. Keep automatic merge off.

On an eligible private-repository organization plan, configure main protection
to require the `foundation` check against
the current base, require a PR and resolved review conversations, restrict
updates to `karthik-sivadas`, include administrators, and prohibit force-pushes
and deletion. Keep auto-merge off. Do not require the sole author's own approval:
independent AI review and the authorized merge decision are separate controls.

Private repositories on GitHub Free for organizations cannot enforce that
branch-protection policy. A paid organization plan or separately authorized
visibility change is required. Documentation is not enforcement. GitHub cannot
separate the owner from another user of the owner's credential. Do not revoke
shared credentials, change billing or make the repository public as an implicit
CI repair.

See GitHub's [protected branch documentation](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches)
[default variables](https://docs.github.com/en/actions/reference/workflows-and-actions/variables),
[pull-request event identity](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request),
[pull-request mergeability behavior](https://docs.github.com/en/rest/pulls/pulls#get-a-pull-request),
[REST API breaking changes](https://docs.github.com/en/rest/about-the-rest-api/breaking-changes?apiVersion=2026-03-10),
and [runner hook behavior](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/run-scripts).
