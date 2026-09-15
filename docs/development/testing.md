# Testing and evidence

PM tests observable contracts at their owners and consumers. The canonical [validation matrix](../architecture/pm-server/implementation/VALIDATION-MATRIX.json), accepted issue, and [file ownership map](../architecture/pm-server/implementation/FILE-OWNERSHIP.json) select the required checks. The shared [engineering and review policy](../architecture/pm-server/policy/ENGINEERING-REVIEW.md) owns integration and review sequencing; this guide does not create a second gate.

## Choose the causal check

For changed behavior, identify the failure oracle before production edits. Exercise a causal RED that fails for the intended defect, make the smallest owned correction, then exercise GREEN and refactor without weakening the oracle. Use standard Go tests and table subtests by default. Add concurrency, race, bounded fuzz, security, persistence, browser, same-stack, recovery, or provider checks only where the changed contract and validation row require them.

Tests must observe behavior, boundaries, invariants, transitions, precedence, and real errors. A test that only echoes a mock, checks a copied field, asserts source text, compiles without execution, or proves that a fixture is nonempty is not behavioral evidence. Reversible documentation work uses content, link, provenance, and instruction-discovery checks rather than an invented application RED.

## Keep evidence scopes distinct

| Observation | What it can establish | What it cannot establish |
| --- | --- | --- |
| Source, schema, or descriptor inspection | Exact reviewed bytes and declared structure | Handler reachability, runtime behavior, or provider effects |
| Build or compilation | Selected source compiles for the exercised target | Tests ran, native portability, or service readiness |
| Unit or Temporal SDK test | The exact in-process or replay property asserted | Real PostgreSQL/Temporal/object persistence or cross-host recovery |
| Controlled compatible fake | Contract behavior under the recorded fixture | Live account entitlement, provider semantics, or certification |
| Same-stack integration | The exercised component boundary and failure oracle | A different image, architecture, account, or untested failure mode |
| Live provider case | The exact operation/account/source/binary scope and time observed | An entire connector, lane, account tier, or future build |
| Documentation/provenance check | Portable links, hashes, membership, and honest status | PM application behavior or full BOOT by itself |
| CI result | The recorded workflow, commit, runner, and jobs actually exercised | A later head or obligations absent from that workflow |

Keep fake and live evidence separately labeled. Preserve exact component, source, schema, fixture/account, and candidate identities. A newer relevant failure prevents reuse of an older pass; unknown dependency closure invalidates conservatively. Skipped, missing, timed-out, zero-selected, compile-only, or different-head work is not a pass.

## Record an actual run

For every claimed check, retain the exact command or action, selected test/case IDs, repository and commit/head, environment and architecture, relevant dependency/fixture identities, start and finish time, exit state, skips, and sanitized output or receipt location. Never record secrets, raw provider credentials, private operator routing, or mutable local paths as product evidence.

Run focused checks before integration. Broad suites and genuine CI run once at the coherent candidate boundary, with affected checks and targeted review after material fixes. Independent Standards and Spec review is separate from testing and cannot turn a missing check into green.

## Current foundation boundary

The issue #3 foundation commands are now source-present. From the repository root, use the pinned entry points:

```sh
make build test vet boundaries
python3 scripts/ci/check-native.py
make bazel-build bazel-test
python3 scripts/ci/check-dependencies.py --evidence-dir .cache/foundation-sboms
make benchmark
```

`make test` runs `TestResolvedDependencyPolicy`, including both real module graphs and its fail-closed synthetic SBOM controls. Without `PM_SBOMS`, that invocation does not claim full external-tool inventory qualification. `check-dependencies.py` generates current-platform inventories, invokes the real `pm-dependency-check` CLI, supplies those SBOMs through `PM_SBOMS`, and reruns the named test. The command is no longer planned or unavailable.

### Bounded Darwin development observations

The following results were observed on 2026-09-15 for the foundation candidate on Darwin arm64. They are local development evidence, not Linux or final-head CI:

- The pinned Go 1.27.1 and LLVM/Clang 21.1.8 were selected. `make build test vet boundaries` passed.
- The native probe reported DuckDB **v1.5.5** through both SQL and the C API, preserved exact `-9007199254740993` and `9007199254740993` values, and read back two rows from an owner-only temporary Parquet file. The separate native controls required build refusal with cgo disabled and with the explicit incompatible i386 C target.
- `make bazel-build bazel-test` passed with Bazel 9.2.0, rules_go 0.63.0, and Gazelle 0.54.0. With Bazel test-result caching disabled, exactly `//build/nativeprobe:nativeprobe_test` and `//tools:tools_test` executed and passed. A Gazelle `-mode=diff` check emitted no changes.
- The built oapi-codegen v2.8.0, Staticcheck 2026.2.1 (module v0.8.1), and govulncheck v1.8.0 binaries executed their version commands. A govulncheck version command is not a vulnerability scan.
- The application and isolated tools MVS graphs resolved 247 and 65 non-main modules, respectively. All five selected tools-module entries were explicit `go.mod` graph pins even where Go marked their `require` lines `// indirect` because they are tool dependencies.

The expanded full-inventory command passed in 17.07 seconds, including the policy CLI and a fresh `TestResolvedDependencyPolicy`. It scanned all five selected installs and, for Bazel, the actual expanded install base:

| Selected install | Recognized packages | SHA-256-inventoried files | Bounded note |
| --- | ---: | ---: | --- |
| Bazel | 52 | 435 | Expanded install base, including its bundled runtime |
| Bazelisk | 0 | 2 | File inventory retained despite no recognized package |
| LLVM | 0 | 9,609 | File inventory retained despite no recognized package |
| Go | 158 | 15,640 | Nineteen unversioned source-root package records retained |
| Syft | 276 | 5 | Package and file inventory reported separately |

These counts describe what the scanner recognized and hashed in those exact installs. They do not claim that recognized-package discovery is complete. Unversioned records are not dropped to obtain a pass, and an unversioned component matching a forbidden identity is rejected conservatively.

A focused review correction also inventoried the two locked Bazel module source trees selected by the current output base. Gazelle 0.54.0 yielded **159 recognized packages and 1,997 SHA-256-inventoried files**; rules_go 0.63.0 yielded **43 recognized packages and 1,064 SHA-256-inventoried files**. The corrected policy CLI passed with those two inventories and the five unchanged retained install inventories above, followed by a fresh `TestResolvedDependencyPolicy` pass. The pre-fix CLI accepted the five-install set without either Bazel module inventory; the corrected CLI refused that same omission. Source-tree inventories include source fixtures and release tooling, not a claim that every discovered component executed.

The same focused regression run loaded the Linux amd64 and arm64 native imports with `go list -mod=readonly -deps`. Both failed before their explicit native leaf requirements were added and passed afterward. This proves readonly import closure, not Linux compilation or execution.

The benchmark completed one cold-output/action-cache build in **47.086722667 seconds** and one warm build using the same output base in **5.195518208 seconds**. Tool and repository-download caches were already warm. This is one sample of each state, not a statistical speedup claim.

### Still unqualified

The Darwin observations do not qualify a Linux target, release artifact, backend, service process, or deployment. Independent Standards and Spec review, exact-head Linux CI, and merge require their own records. Full BOOT completion must bind those results rather than infer success from this local checkpoint.

This foundation creates no PM server, worker, client, connector, provider execution, service stack, or demo, and qualifies none of them. Source locks, successful downloads, module resolution, SBOM inventory, build/test results, and developer-tool navigation cannot substitute for the later product, same-stack, provider, recovery, release, and two-real-demo obligations owned by their accepted issues.
