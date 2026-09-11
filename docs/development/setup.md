# Developer setup

This repository is being prepared before application implementation. Go packages, the PM binary, build targets, and connector proof are still planned. Installing navigation tools or creating an index does not implement them.

## What is available now

From the repository root, run the included availability report:

```sh
sh scripts/dev/doctor.sh
```

It prints tool versions and index status. It does not install dependencies, change global configuration, generate source, build PM, run tests, or contact providers. Missing optional tools are reported explicitly; exit zero means the report completed, not that the application is validated. An installed tool that fails its version/status command makes the report fail.

The setup investigation on 2026-09-10 verified CodeGraph 1.5.0. The private candidate doctor observed Go1.26.5 darwin/arm64 and golangci-lint2.12.2; govulncheck was absent. After installation, the setup owner ran doctor in the actual clone: it completed successfully, but that process's PATH selected **Go1.25.6 darwin/amd64**. An explicit native Go binary on the same machine reports **Go1.26.5 darwin/arm64**. Doctor success therefore does not establish that the intended compiler or architecture is selected.

These observations describe one machine and execution context, not minimum supported versions or the project's eventual toolchain lock. Run doctor in the actual checkout and build context. At M0, explicitly select and pin the Go executable/toolchain and verify its version and architecture. Qualify cgo, the C compiler and DuckDB together for the restricted query child; the client/API/worker must not acquire the analytical engine dependency. Record the resolved executable identity as well as the version. Do not upgrade tools or rewrite global PATH merely to make setup appear ready; no version choice is made here. C/DuckDB build readiness remains unverified.

No `make test`, PM smoke, schema-generation target, or Go-module setup command is advertised as available yet. The new module and pinned toolchain belong to the later implementation task.

## Repository skills and setup evidence

Use [skill routing](skills.md) and the repository's four original skill entrypoints.
[Skill provenance](skill-provenance.md) explains the nonempty [portable inventory](skill-provenance.json).
External references are optional except when an authorized review/delivery task explicitly needs its tool.
Use existing installations by source/name; official fallbacks cover absent optional references.
Do not install a catalog, rewrite global MCP configuration or require a model login in CI.
A successful public Context7 query proves documentation access only; use ordinary official sources if unavailable.

This setup does not create the application stack, import private context, or resolve product decisions.
Future implementation retains PostgreSQL, Temporal and shared objects in development as well as deployment.
Structural setup checks cover files, hashes, confined links and declared schemas.
Actual agent discovery, independent Standards/Spec review and delivery-tool qualification are separate observations.
An empty runner check inventory is not CI success. Runner setup and activation require their owning task.
The published tracking specification and explicit task authority must precede repository implementation.

## CodeGraph identity and prerequisites

The tool used here is [`@colbymchenry/codegraph`](https://github.com/colbymchenry/codegraph), version **1.5.0**, under the MIT license. Its npm launcher selects a platform bundle with a bundled Node runtime and parser. The npm launcher itself needs Node on PATH; the standalone distribution bundles the runtime. Go structural indexing did not require `go.mod`, a Go build, a database service, or a container in the fixture.

Do not substitute another project named CodeGraph. If it is absent, bounded `rg` and direct reads remain sufficient to contribute. Installation is an explicit developer choice: review the official distribution, exact version, supported platform, and provenance first. Do not silently install or upgrade to latest. This setup did not independently verify release attestations; installed package metadata is provenance evidence, not an attestation result.

The installed npm shim can download a missing platform bundle automatically. The commands below set `CODEGRAPH_NO_DOWNLOAD=1` so a missing prerequisite fails visibly. `CODEGRAPH_TELEMETRY=0` disables telemetry for that invocation without altering global preferences. Neither setting changes PM behavior.

## Initialize this checkout explicitly

Inspect the root and the included `codegraph.json` before indexing. It excludes generated runtime data, generated website/manual projections, proof output, and local execution state. `.gitignore` also excludes the local graph. Declarations, foundation source, shared tests, and connector scripts remain visible. Excluding generated JSON from navigation does not exclude it from builds or validation.

For a checkout without an index, run:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph init .
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph status --json .
```

`init` creates `.codegraph/` and builds the initial graph in the same command. Version 1.5.0 has no CLI init-only flag; `--index` is a deprecated compatibility option. An empty repository can therefore be initialized successfully while containing no Go symbols. Report that honestly and repeat the real-source check below after implementation begins. Do not insert dummy PM code just to produce a graph.

Do not use `--force` to index a workspace parent or home. Do not invoke `install`, `uninit`, daemon controls, or an updater as part of a setup check. If `init` offers Git hooks or inclusion of ignored child repositories, keep explicit manual sync and the existing exclusions; those mutations are not required here.

## Refresh and inspect real code

From the intended checkout root, use:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph sync .
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph status --json .
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph explore "<real symbol names>" --path . --max-files 5
```

Replace the angle-bracket text with symbols that actually exist. After changing exclusions, language mappings, or extraction versions, rebuild the graph explicitly:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph index . --quiet
```

Inspect `projectPath`, `indexPath`, `lastIndexed`, `languages`, counts, `worktreeMismatch`, and the nested `index` state/version fields. A version mismatch or incomplete extraction needs resolution before relying on relationships. A successful explicit sync plus inspection of a known changed symbol is stronger evidence than timestamp or pending-count checks alone.

The MCP server can watch and synchronize files while a supported agent session is running. The fixture deliberately disabled watching and daemon use, so watcher behavior was not tested. CLI initialization alone is not evidence of a persistent watcher. Local source documents `CODEGRAPH_NO_WATCH=1` as an opt-out; initialization with it displayed a hook/manual-sync prompt. Do not bake this prompt-producing setting into unattended initialization.

The authoritative [configuration documentation](https://github.com/colbymchenry/codegraph#configuration) identifies root `codegraph.json` and `.gitignore` as exclusion controls. In version 1.5.0, `exclude` also excludes tracked files; `.gitignore` alone is not sufficient for tracked generated content. Treat a malformed config warning as incomplete setup because the tool can continue after ignoring that file.

## CLI versus MCP

The setup owner already has `codegraph_explore` exposed through MCP. No global reinstall is required. Use its explicit project path argument for this checkout. For another agent installation, this read-only command prints the installed tool's configuration snippet:

```sh
CODEGRAPH_TELEMETRY=0 CODEGRAPH_NO_DOWNLOAD=1 codegraph install --print-config codex
```

It reports `command = "codegraph"` with `args = ["serve", "--mcp"]`. A developer can review and add the snippet through their agent's own MCP configuration process. The agent starts the MCP server; do not manually launch or manage a second service. The investigation tested snippet generation, not a new MCP session or global configuration changes.

## Observed Go indexing limits

A disposable external fixture contained `TestRun → Run → Normalize` across three `.go` files. CodeGraph indexed three files, seven nodes, and six edges, returned the call flow and source, and excluded three noise files through `codegraph.json` and `.gitignore`. After an edit introduced `Bump`, explicit sync reported one changed file and exploration returned `Run → Normalize → Bump`. A full reindex also completed. No fixture was added to this repository and no Go tests were executed.

Two observations constrain its use:

- `affected normalize.go --path . --json` returned an empty test list, although the graph exposed the known test call chain. Adding `--filter '**/*_test.go'` also returned an empty list. Neither result can authorize skipping tests.
- In this non-Git fixture, `status` reported zero pending changes immediately after the edit; the following explicit sync found the edit. Do not infer that a graph is fresh from that count alone. This does not establish identical behavior in a Git checkout.

Use file/package ownership, known consumers, declarations, and the change's causal risk to select tests. Graphs do not fully describe runtime JSON selection, scripts, reflection, dynamic dispatch, cgo, or provider behavior. Never use graph reachability alone to preserve old proof receipts.

## Gate after real Go source exists

The implementation owner should record these observations once meaningful source has been added:

1. Doctor reports the pinned Go/tool prerequisites; validate the C/DuckDB build separately.
2. Initialize or sync this checkout and verify a nonzero Go graph at this exact path.
3. Explore a real cross-file caller/callee pair and confirm source and relationships.
4. Edit an owned real symbol, sync, and confirm the updated relationship/source.
5. Confirm generated bundle/proof/runtime paths stay excluded from navigation.
6. Select and run the implementation's focused behavioral checks independently of `affected`.

These are future gates. The setup packet's fixture result cannot be substituted for them, and none establishes connector implementation or live certification.
