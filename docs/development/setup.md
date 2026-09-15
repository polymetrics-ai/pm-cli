# Developer setup

This checkout contains the accepted PM Server documentation baseline and the combined issue #2/#3 foundation. PR #47 landed the reviewed context on `main` at `d8e84e1fc78561173feac8abfd64ce669f09f373`, and BOOT-1 readback verified the approved staging-to-landed relationship. The bounded Darwin foundation checks and fresh-worker discovery below do not substitute for independent review, exact-head Linux CI, or merge records. A working build/toolchain foundation does not implement PM services, client commands, generated bundles, connectors, provider effects, or certification.

## What is available now

The repository contains a digest-pinned bootstrap, application and tools Go modules, native DuckDB probe, dependency-policy CLI, and Make and Bazel targets. Use them from the repository root; do not replace them with whichever compiler or build tool happens to be on the ambient `PATH`.

### Resolve and run pinned tools

`tools/bootstrap.py` is the public entry point for the locked Go, LLVM/Clang, Bazelisk/Bazel, and Syft assets in `build/toolchains.lock.json`:

```sh
python3 tools/bootstrap.py print-env
python3 tools/bootstrap.py exec go -- version
python3 tools/bootstrap.py exec clang -- --version
python3 tools/bootstrap.py exec bazelisk -- --batch version
```

The literal `--` before tool arguments is required. On a cache miss the bootstrap downloads only locked assets for the selected host platform, verifies each locked SHA-256, and installs them into a persistent local cache. The default is `.cache/pm-tools`; set `PM_TOOLS_CACHE` to another persistent directory when needed. The bootstrap rejects caches under `/tmp` and `/private/tmp`. When the default repository-local tool cache is used, Bazel's output-user root is placed in the platform user cache because Bazel does not permit its repository-contents cache inside the source workspace.

Bootstrap environment changes apply only to the process it executes. In particular, the pinned Go directory is prepended only to that child process's `PATH`; the parent shell, global `PATH`, and globally installed tools are not changed. Go and Bazel execution force `GOTOOLCHAIN=local` and `GOWORK=off`, so an ambient workspace or Go's automatic toolchain selection cannot replace the locked inputs.

On macOS, cgo-enabled Go, Clang, and Bazel execution require an Apple macOS SDK discoverable through the fixed system command:

```sh
/usr/bin/xcrun --sdk macosx --show-sdk-path
```

Select Xcode or the Xcode Command Line Tools so that command returns an available absolute SDK directory. The bootstrap refuses native execution when it cannot resolve that prerequisite; it passes the resulting SDK and deployment target only to the child process.

Linux development-native execution additionally requires the reviewed [`build/native-environment.json`](../../build/native-environment.json) profile. The current profile captures the existing Linux amd64 CI sysroot, including OS identity, headers, GNU runtime libraries and loader. Its fingerprints bind logical paths, entry types, symlink chains and file bytes; they are not raw-file payload checksums. Capture alone does not qualify native execution.

Bootstrap verifies that profile before native execution, selects the locked Clang/`ld.lld`/`llvm-ar` source, requires its compiler-rt startup/builtin objects when no GCC installation is selected, and derives local linker scripts for the admitted GNU runtime SONAMEs. Bazel receives the verified LLVM archiver rather than requiring host binutils. Added cached tool roles are authenticated against the locked archive before their admission record changes; cached SDK files are not replaced. Bootstrap installs nothing into the host sysroot. The profile digest participates in compile and link options so a newly reviewed sysroot identity cannot reuse stale C objects. DuckDB application archives remain pinned separately by `go.sum` and the generated Bazel dependency repositories.

Linux header search excludes implicit host locations: only the selected Clang builtin headers and profiled system include directories are enabled. Selected trees bind intermediate symlink selectors and refuse targets outside the declared trees or files. Two existing LLVM 18 shared-library targets are included only to close symlinks in the selected system-library tree; they are not the selected compiler.

Unadmitted native flag overrides and mismatched native `GOOS`/`GOARCH` requests fail rather than falling back. A missing or changed profile requires a deliberate new capture and review; CI never refreshes it automatically. Linux targets without an admitted matching profile remain unqualified. This is bound-host development evidence, not a hermetic build or release qualification; immutable build-image digests remain mandatory before image-based or release-native builds under the [accepted TD27 amendment](https://github.com/polymetrics-ai/pm-cli/issues/3#issuecomment-5687998290).

### Build and test entry points

These Make targets are source-present:

| Command | Boundary exercised |
| --- | --- |
| `make build` | Builds the cgo native probe and the isolated dependency-policy, oapi-codegen, Staticcheck, and govulncheck binaries with pinned Go |
| `make test` | Runs the resolved-dependency policy test and the native DuckDB behavioral test |
| `make vet` | Vets the application module with cgo and the tools module without cgo |
| `make boundaries` | Checks native-linkage and module ownership boundaries |
| `make bazel-build` | Runs `bazelisk --batch build //...` through the bootstrap |
| `make bazel-test` | Runs `bazelisk --batch test //...` with test-result caching disabled |

The bounded Bazel timing probe is also source-present:

```sh
make benchmark
```

It asks Bazel for the actual output base, creates an isolated temporary output/action cache alongside it and outside the source workspace, and measures one cold-output-cache build followed by one warm build of the same `//...` targets and output base. Tool and repository-download caches may already be warm, and one sample is not a statistical speedup claim.

Run the isolated native qualification, including its negative controls, with:

```sh
python3 scripts/ci/check-native.py
```

It builds and runs the native probe in an owned temporary directory under `.cache`. A separate C/C++ fixture must then compile and return its expected value while ambient `CC` and `CXX` point to failing executables, proving that child processes retain the pinned compilers. The qualification reuses resolved modules with network lookup disabled and requires normal build-path refusal for cgo disabled and for an incompatible 32-bit native target. On Linux, the latter requests an unqualified Go target and must fail host-profile preflight; it does not claim that a cross-target compiler ran. The macOS control retains its explicit incompatible C target.

Run the current-platform external-tool and selected Bazel-module inventories with both Go graph checks using a disposable, uncommitted evidence directory:

```sh
python3 scripts/ci/check-dependencies.py --evidence-dir .cache/foundation-sboms
```

That orchestrator resolves the five selected tool installs, expands Bazel to its actual install base, and resolves the two locked Gazelle/rules_go source repositories through Bazel's canonical repository metadata. It checks their exact archive URL/integrity pins, scans only those selected source targets rather than a whole historical cache, creates package and per-file SHA-256 CycloneDX inventories, invokes the dependency-policy CLI, and reruns `TestResolvedDependencyPolicy` against those SBOMs. Each selected Bazel module requires recognized-package coverage; file-only evidence is insufficient. File inventory describes installed bytes, not proof that a package catalog recognized every dependency or that every source component executed.

The optional availability report remains:

```sh
sh scripts/dev/doctor.sh
```

It reports ambient tool versions and index status without installing dependencies, changing global configuration, generating source, building PM, running tests, or contacting providers. Missing optional tools are reported explicitly; an installed tool whose version/status command fails makes the report fail. Exit zero means the report completed, not that the application is validated.

### Go tool and module pin maintenance

The `tool` block in `tools/go.mod` names three executable packages: oapi-codegen, govulncheck, and Staticcheck. Their modules and two deliberately selected supporting modules form the five exact tools-graph entries in `tools/versions.json`: `github.com/getkin/kin-openapi`, `github.com/oapi-codegen/oapi-codegen/v2`, `go.yaml.in/yaml/v3`, `golang.org/x/vuln`, and `honnef.co/go/tools`. Go may annotate these explicit `require` entries `// indirect` because ordinary non-tool packages do not import them. That annotation does not turn a reviewed root pin into an incidental transitive selection.

Conversely, a version chosen transitively by minimal version selection is not an approved pin merely because it appears in `go list -m all`. The policy requires every selected manifest entry at the exact version and as a direct main-module graph edge; it separately examines the full graph for forbidden identities.

Do not run a blind `go mod tidy`: the foundation intentionally pins approved modules whose later consumers do not exist yet. Tidy can remove those requirements, including deliberately selected tools support modules. Inspect a proposed tidy without applying it:

```sh
python3 tools/bootstrap.py exec go -- mod tidy -diff
python3 tools/bootstrap.py exec go -- -C tools mod tidy -diff
```

These read-only previews return nonzero when they propose changes. Apply only reviewed changes for an authorized pin update, retaining all still-approved explicit requirements. After updating the affected module and selection manifest, retain full-graph module and `go.mod` sums:

```sh
python3 tools/bootstrap.py exec go -- mod download all
python3 tools/bootstrap.py exec go -- -C tools mod download all
```

Use the command for each affected module. Keep its graph selections, checksums, `tools/versions.json`, affected Bazel locks, external-asset lock when applicable, and `THIRD-PARTY-NOTICES.md` synchronized. `go mod download all` supplies full-graph sums; it does not approve a version or refresh manifests and notices by itself. See [testing](testing.md) for the bounded local observations and their unqualified scope.

## Active instructions and workflow

Fresh workers start with the root [AGENTS.md](../../AGENTS.md), [issue/spec lookup](../agents/issue-tracker.md), short [architecture overview](../architecture/overview.md), [decision index](../architecture/decisions.md), [domain glossary](../agents/domain.md), [testing](testing.md), [release](release.md), [skill routing](skills.md), [skill provenance](skill-provenance.md), the active [configuration ADR](../decisions/0001-cli-configuration.md), and the assigned accepted PM Server contracts.

Use the sole [PM Go router](../../.agents/skills/pm-go/SKILL.md) for Go work. Add [connector contracts](../../.agents/skills/pm-connector-contracts/SKILL.md), [durable effects](../../.agents/skills/pm-durable-effects/SKILL.md), or [release and recovery](../../.agents/skills/pm-release-recovery/SKILL.md) only for its named behavior boundary. Files under `docs/architecture/pm-server/instructions/` are synchronized inactive projections, not a second instruction tree. Historical issue drafts, private setup/controller runbooks, transition evidence, and operator configuration are excluded from fresh-worker intake.

OMP is the sole coordinator and integration owner. The single [engineering and review policy](../architecture/pm-server/policy/ENGINEERING-REVIEW.md) owns validation, independent review, and delivery. Agent model/account routing is supplied separately by the operator and is not application documentation. No controller installation, private home path, model login, or host qualification is a general application prerequisite.

The doctor command above and the CodeGraph commands below are real developer-tool commands for this checkout. They do not activate application work. Every `pm`, server, worker, stack, generation, proof, build, test, and provider command described only in planning remains future until its owner creates and exercises the actual path.

## Repository skills and source provenance

The [machine-readable manifest](skill-provenance.json) records exactly four repository-original project skills, their current byte hashes, required repository-relative references, and selected external source closure. External references use `use-installed-no-copy`; [the provenance guide](skill-provenance.md) records licenses, immutable revisions or file hashes, PM overrides, and official HTTPS fallbacks.

Optional references may be absent on a host. Use the official fallback and report the missing discovery rather than installing a catalog or rewriting global configuration. No third-party skill payload is copied, and no skill file, model login, source URL, or structure-only setup check is evidence that PM behavior or delivery passed.

## CodeGraph identity and prerequisites

The tool used here is [`@colbymchenry/codegraph`](https://github.com/colbymchenry/codegraph), version **1.5.0**, under the MIT license. Its npm launcher selects a platform bundle with a bundled Node runtime and parser. The npm launcher itself needs Node on PATH; the standalone distribution bundles the runtime. Go structural indexing did not require `go.mod`, a Go build, a database service, or a container in the fixture.

Do not substitute another project named CodeGraph. If it is absent, bounded `rg` and direct reads remain sufficient to contribute. Installation is an explicit developer choice: review the official distribution, exact version, supported platform, and provenance first. Do not silently install or upgrade to latest. This setup did not independently verify release attestations; installed package metadata is provenance evidence, not an attestation result.

The installed npm shim can download a missing platform bundle automatically. The commands below set `CODEGRAPH_NO_DOWNLOAD=1` so a missing prerequisite fails visibly. `CODEGRAPH_TELEMETRY=0` disables telemetry for that invocation without altering global preferences. Neither setting changes PM behavior.

## Initialize this checkout explicitly

The current candidate's exact checkout has already been initialized manually; it was not indexed through a workspace parent. For another checkout without an index, inspect its root and `codegraph.json`, then run from that checkout:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph init .
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph status --json .
```

`init` creates `.codegraph/` and builds the initial graph in the same command. Version 1.5.0 has no CLI init-only flag; `--index` is a deprecated compatibility option. Keep explicit manual synchronization and the repository's exclusions if initialization offers Git hooks or ignored-child inclusion. Do not use `--force` on a workspace parent or home, and do not install hooks, start a daemon, or invoke an updater as a setup shortcut.

## Refresh and inspect real code

Use the intended checkout root explicitly:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph sync .
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph status --json .
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph explore "<real symbol names>" --path . --max-files 5
```

Replace the angle-bracket text with symbols that actually exist. After changing exclusions, language mappings, or extraction versions, rebuild explicitly:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph index . --quiet
```

Inspect `projectPath`, `indexPath`, `lastIndexed`, languages, counts, `worktreeMismatch`, and the nested index and extraction versions. A successful explicit sync followed by inspection of a known changed symbol is stronger evidence than a timestamp or pending-change count. The current gate used explicit sync; it did not rely on a hook, daemon, or watcher.

The root `codegraph.json` and `.gitignore` exclude generated runtime data, generated website/manual projections, proof output, caches, and local execution state. CodeGraph 1.5.0's `exclude` also applies to tracked files. Treat a malformed configuration warning as incomplete setup because the tool can continue after ignoring the file.

## CodeGraph CLI versus development MCP

CodeGraph's MCP exposure is optional developer navigation tooling. This read-only mode prints version 1.5.0's agent configuration snippet without changing global configuration:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph install --print-config codex
```

It reports a `codegraph serve --mcp` command for the developer to review through the agent's own configuration process. The current candidate gate used the CLI and did not install a hook, configure MCP, or test watcher behavior. Do not manually launch a second service or make MCP an application prerequisite. PM itself neither implements nor depends on MCP.

## Current CodeGraph observation

CodeGraph 1.5.0 was manually initialized and explicitly synchronized at the exact candidate checkout, with no worktree mismatch. After source edits, explicit sync made the changed source available to exploration. An exploration of `CheckDependencies` resolved the implementation in `tools/dependency_policy.go`, its real CLI call from `tools/cmd/pm-dependency-check/main.go`, and the associated `tools/dependency_policy_test.go`; source inspection identified `TestResolvedDependencyPolicy` as the focused graph-policy test.

This establishes useful static navigation for the observed source. It does not establish complete dependency closure, runtime dispatch, cgo behavior, provider behavior, or complete test selection. No Git hook, persistent watcher, or daemon supplied freshness, so synchronize explicitly before relying on an edited symbol.

## Retained fixture limits

The earlier disposable external fixture contained `TestRun → Run → Normalize` across three Go files. CodeGraph indexed three files, seven nodes, and six edges, returned the call flow and source, and excluded three noise files. After an edit introduced `Bump`, explicit sync reported one changed file and exploration returned `Run → Normalize → Bump`; a full reindex also completed. No fixture was added to this repository and no Go test was executed.

Two limits remain relevant:

- `affected normalize.go --path . --json` returned an empty test list although exploration exposed the known test call chain; adding `--filter '**/*_test.go'` also returned no tests. Neither result can authorize skipping tests.
- In that non-Git fixture, `status` reported zero pending changes immediately after an edit, while the following explicit sync found it. Do not infer freshness from the pending count alone or assume identical behavior in every Git checkout.

Use file and package ownership, known consumers, declarations, and causal risk to select checks. A graph does not fully describe runtime JSON selection, scripts, reflection, dynamic dispatch, cgo, or provider behavior, and graph reachability alone cannot preserve old proof receipts.
