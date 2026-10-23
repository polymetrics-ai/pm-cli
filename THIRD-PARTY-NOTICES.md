# Third-party notices

This file records the third-party source identities selected by the build/toolchain foundation and the complete Go module build lists resolved from the two checked-in module roots. PM-owned source is MIT; that project license does not alter any third-party terms. A declaration, checksum, or notice is provenance only and does not claim that a binary, native target, service, image, or release was built or qualified.

## Recorded dependency scope

- Module-listing environment: Go 1.27.1 with `CGO_ENABLED=0`, `GOWORK=off`, and `GOFLAGS=-mod=readonly`.
- Application graph: 247 resolved third-party modules; `go.sum` SHA-256 `d0fc9dd7c14f008aaed4b3d9a3f8311b3acfeeab34709e4f133bbc0a36aa3f6a`.
- Isolated tools/generator graph: 65 resolved third-party modules; `tools/go.sum` SHA-256 `7c8cfcd3e5d3149e9e8ce2b09003dec1ede617494a9f19f8e8cc90c3f3fa3be0`.
- Union: 299 exact module path/version identities; 13 identities occur in both graphs.
- The module table below names the license material present in the downloaded source module. Full upstream license and NOTICE files must accompany any distribution that contains the corresponding code. This compact inventory is not a substitute for those texts.
- `github.com/iris-contrib/schema@v0.0.6` places its BSD-3-Clause terms in `README.md#License`. `github.com/nexus-rpc/nexus-proto-annotations@v0.1.0` omits the repository MIT file from its module zip; the upstream repository license must be retained separately. The downloaded `github.com/chzyer/logex@v1.1.10` module contains no license or NOTICE file and no README license statement, so its distribution license remains unresolved.

## Selected Go source identities

`tools/versions.json` is the machine-readable selected-direct-module policy for the application, generators, quality tools, and fixture tooling. The resolved lists contain every selected pin, including DuckDB Go `v2.10505.0`, bindings `v0.10505.0`, oapi-codegen `v2.8.0`, Staticcheck `v0.8.1`, govulncheck `v1.8.0`, and the fixture's direct `golang.org/x/sys@v0.48.0` dependency. The latter was already present in the resolved tools graph and license inventory; its descriptor-relative filesystem use now makes it an explicit selected root. The only selected forbidden identity is exactly `github.com/goreleaser/goreleaser/v2@v2.18.1`; no broader GoReleaser family policy is asserted.

## Native and external source notices

| Component | Exact identity | License / notice | Qualification status |
| --- | --- | --- | --- |
| Go | 1.27.1; official source archive SHA-256 `4e408abae126d916b6164627193f2c54f0e3ca1312d693b86db45f862ab238b1`; Darwin arm64 tool archive SHA-256 `ee215d57e0ec269c60cc9ceca68e6bda321ba9ee5afe24f4b0988703c2d87d12` | BSD-3-Clause | The pinned Darwin arm64 tool executed for module resolution and successful build, test, and vet boundaries; Linux locks remain unexecuted and no release qualification is claimed |
| DuckDB Go driver | `github.com/duckdb/duckdb-go/v2@v2.10505.0`, source archive SHA-256 `afa622810aa5cb65e4acafcbb6d95db9936e98329c743d9d9eff8ce521af470c` | MIT | Resolved and exercised through the Darwin arm64 native child; other targets remain unexecuted |
| DuckDB Go bindings | `github.com/duckdb/duckdb-go-bindings@v0.10505.0`, origin `5f98009483427ea94061fdd5e39515d129d11b00` | MIT | The Darwin arm64 native probe passed with the locked binding and native asset; an omitted embedded build-info sum remains explicitly coupled to the separately verified module lock |
| DuckDB core/static libraries | DuckDB 1.5.5; Linux amd64 `deb47c5300f3c99725e84cdb14d214c3b12bbd748b613b1698b938c894cb68eb`; Linux arm64 `ea6a34cb49ec2db5ed23d9e8311237c53c32abf9cdbf5dd608c4176c3dd8bfeb`; macOS amd64 `a27d36fa1247a3ffa1692e7aa0bf4ea4d1e0ee51da7c4df7a5db5217357b1b4d`; macOS arm64 `d79ec66b8a4054b866faada82e9e31f859a713c555b3f1c4b71c4a43d3273e9c` | MIT; preserve DuckDB and bundled extension notices | The Darwin arm64 SQL and C API probe passed at 1.5.5, including exact `±9007199254740993` values and a two-row Parquet read; every other platform remains unexecuted and no release or service qualification is claimed |
| LLVM/Clang | 21.1.8; Linux x86_64 `b3b7f2801d15d50736acea3c73982994d025b01c2f035b91ae3b49d1b575732b`; Linux arm64 `65ce0b329514e5643407db2d02a5bd34bf33d159055dafa82825c8385bd01993`; macOS arm64 `b95bdd32a33a81ee4d40363aaeb26728a26783fcef26a4d80f65457433ea4669` | Apache-2.0 WITH LLVM-exception; preserve archive notices | The pinned Darwin arm64 compiler executed for the native build/probe; cgo-disabled and explicit incompatible i386 C-target builds refused as intended; Linux archives remain unexecuted |
| Syft | 1.51.1; Darwin arm64 `ac063af3b9874769deb7ea1e6d76841e68f9e3bb50cd654226fc977de65532c1`; Linux amd64 `8fcb33017a0dc1058298c923c436d19dfa68ae93968e0b423248542e3afb9fc3`; Linux arm64 `a7fd2b784e6664acd44719270574f6cd8c6864fc2b1700bf9099bd1cccda7d7f` | Apache-2.0 (`LICENSE` in the inspected Darwin arm64 archive; no separate NOTICE file) | The locked Darwin arm64 scanner executed in the successful expanded dependency-policy inventory; Linux archives remain unexecuted, and this is not a vulnerability scan or release qualification |
| Bazel / Bazelisk | Bazel 9.2.0 / Bazelisk 1.29.0; Darwin arm64 binary SHA-256 `dd466352a3e4d3581b8898740ee1ff208866ccbe25f8d367c5dcb950219587e6` / `cee851f726789227d5561004e9904a52be45c3efb56f8b38b6993d6adbaa0409` | Apache-2.0 for Bazel/Bazelisk; preserve the embedded-runtime material described below | The locked pair executed on Darwin arm64; normal Bazel build/test boundaries passed with two real tests executed without test-result-cache reuse; other platforms remain unexecuted and no release qualification is claimed |
| rules_go / Gazelle | 0.63.0 / 0.54.0 | Apache-2.0 | The locked Bzlmod inputs executed in the successful Darwin arm64 Bazel build/test boundaries; other targets remain unexecuted |
| Docker Compose | 5.5.1; Darwin arm64 `998735c9b6fe68a4f05895e6ea73d71ad06f9fc7046383ad89e47346781b6af5`; Linux amd64 `db1889184726840f75c4f9c001048430d4f25b3be3cb084d3ddd762bc0aed576`; Linux arm64 `732e3a84c1a0f67256ce80bc2598a24546b10ca05f9faa97efceb1171ece2ef7` | [Apache-2.0](https://github.com/docker/compose/blob/v5.5.1/LICENSE); retain the binary's component notices and inventory | The project-local Darwin arm64 executable reported 5.5.1 after locked-asset verification; Linux clients and dependency-stack behavior remain unqualified by this observation |

The inspected Bazel 9.2.0 Darwin arm64 install base identifies its server build label as 9.2.0. `A-server.jar` carries `META-INF/LICENSE` (Apache-2.0), `META-INF/LICENSE.txt` (Checker Framework MIT), `META-INF/NOTICE` (Apache Commons Compress), and `META-INF/NOTICE.txt` (Apache Tomcat). The bundled Azul Zulu OpenJDK 25.0.2 (`Zulu 25.32+17`) is not covered by Bazel's Apache-2.0 terms: its `readme.txt` identifies certain OpenJDK portions as GPLv2 with the Classpath Exception. Preserve its OpenJDK `ASSEMBLY_EXCEPTION`, Azul `DISCLAIMER`, `readme.txt`, and complete per-module `legal/**` tree, including every component-specific third-party text.

The observed Darwin arm64 dependency-policy run used the selected Syft 1.51.1 scanner and accepted an expanded Bazel install-base CycloneDX inventory containing 52 recognized packages and 435 hashed installed files. It retained the unversioned `A-server` package record and the separate hashed `A-server.jar` file without inventing a package version. These are dependency-policy observations, not a vulnerability scan, SBOM replacement, release qualification, or service qualification.

`build/toolchains.lock.json` is the machine-readable source for platform URLs, hashes, attestations, Bzlmod integrity values, and image index digests. External CycloneDX/SPDX evidence is mandatory for a full dependency-policy result; this notice file is not an SBOM.

The profile in `build/native-environment.json` fingerprints the Linux amd64 sysroot of the CI job image (`build/ci/Dockerfile`); it does not redistribute its files or confer one license on the whole tree. The observed Ubuntu packages include `libc6-dev` **2.39-0ubuntu8.9** and `libgcc-s1`/`libstdc++6` **14.2.0-4ubuntu2~24.04.1**. Preserve their distribution copyright files when distributing corresponding material: `/usr/share/doc/libc6-dev/copyright`, `/usr/share/doc/libgcc-s1/copyright`, and `/usr/share/doc/libstdc++6/copyright`. The inspected libc notice assigns most library code LGPL-2.1-or-later and identifies additional terms; the GCC notice assigns libgcc and libstdc++ GPL-3.0-or-later with GCC Runtime Library Exception 3.1. These are package-specific notices, not blanket clearance for every profiled host file. The selected LLVM archive owns compiler-rt notices separately. Sysroot capture is not native execution, immutable-image qualification, or release qualification.

Locked source archive/binary hashes and installed-content hashes emitted by an SBOM scanner cover different bytes. The dependency checker reports them separately and never treats an extracted-file digest as the source-archive digest.

## Native platform labels

- `linux-amd64` and `linux-arm64`: intended backend native targets; **UNEXECUTED** until the actual DuckDB child probe passes with the exact locked compiler, binding, and native asset on that target.
- `darwin-arm64`: development probe target; **EXECUTED** with pinned Go 1.27.1 and LLVM/Clang 21.1.8. The native DuckDB SQL/C API probe and normal Bazel build/test boundaries passed; cgo-disabled and explicit incompatible i386 C-target builds refused. This is not an advertised backend release target and does not qualify a release or service.
- `darwin-amd64`: development probe source target; **UNEXECUTED** and not an advertised backend release target.
- Windows: **DEFERRED / UNSUPPORTED** by this foundation even though the upstream binding publishes a Windows archive.
- FreeBSD and every other unlisted OS/architecture: **UNSUPPORTED**; no native cross-build or compatibility claim.

## Upstream NOTICE and bundled runtime files that require preservation

- Selected Syft 1.51.1 Darwin arm64 archive: `LICENSE` (the inspected archive contains no separate NOTICE file)
- Selected Bazel 9.2.0 Darwin arm64 runtime: `A-server.jar` entries `META-INF/LICENSE`, `META-INF/LICENSE.txt`, `META-INF/NOTICE`, and `META-INF/NOTICE.txt`; embedded JDK `ASSEMBLY_EXCEPTION`, `DISCLAIMER`, `readme.txt`, and `legal/**`
- `github.com/apache/arrow-go/v18@v18.5.1`: `NOTICE.txt`
- `github.com/aws/aws-sdk-go-v2@v1.47.0`: `NOTICE.txt`
- `github.com/aws/smithy-go@v1.28.1`: `NOTICE`
- `github.com/coreos/go-oidc/v3@v3.21.0`: `NOTICE`
- `github.com/elastic/go-sysinfo@v1.15.5`: `NOTICE.txt`
- `github.com/elastic/go-windows@v1.0.2`: `NOTICE.txt`
- `github.com/go-openapi/jsonpointer@v0.22.5`: `NOTICE`
- `github.com/go-openapi/jsonpointer@v0.23.1`: `NOTICE`
- `github.com/go-openapi/testify/v2@v2.4.2`: `NOTICE`, `docs/doc-site/project/NOTICE.md`, `internal/difflib/NOTICE`
- `github.com/kataras/iris/v12@v12.2.11`: `NOTICE`
- `github.com/moby/patternmatcher@v0.6.1`: `NOTICE`
- `github.com/oasdiff/yaml3@v0.0.14`: `NOTICE`
- `github.com/prometheus/procfs@v0.22.0`: `NOTICE`
- `github.com/vmware-labs/yaml-jsonpath@v0.3.2`: `NOTICE`
- `github.com/ydb-platform/ydb-go-genproto@v0.0.0-20260810123728-f0c151ab31b9`: `NOTICE`
- `github.com/ydb-platform/ydb-go-sdk/v3@v3.151.1`: `NOTICE`
- `go.yaml.in/yaml/v3@v3.0.5`: `NOTICE`
- `google.golang.org/grpc@v1.83.2`: `NOTICE.txt`
- `gopkg.in/yaml.v2@v2.4.0`: `NOTICE`
- `gopkg.in/yaml.v3@v3.0.1`: `NOTICE`

## Complete resolved Go module inventory

| Module | Scope | License material observed | Upstream version record |
| --- | --- | --- | --- |
| `cloud.google.com/go/compute/metadata@v0.3.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/cloud.google.com/go/compute/metadata@v0.3.0) |
| `dario.cat/mergo@v1.0.2` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/dario.cat/mergo@v1.0.2) |
| `filippo.io/edwards25519@v1.2.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/filippo.io/edwards25519@v1.2.0) |
| `github.com/Azure/azure-sdk-for-go/sdk/azcore@v1.23.1` | application | MIT (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azcore@v1.23.1) |
| `github.com/Azure/azure-sdk-for-go/sdk/azidentity@v1.14.1` | application | MIT (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azidentity@v1.14.1) |
| `github.com/Azure/azure-sdk-for-go/sdk/internal@v1.12.0` | application | MIT (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/internal@v1.12.0) |
| `github.com/Azure/go-ansiterm@v0.0.0-20250102033503-faa5f7b0171c` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/Azure/go-ansiterm@v0.0.0-20250102033503-faa5f7b0171c) |
| `github.com/AzureAD/microsoft-authentication-library-for-go@v1.9.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/AzureAD/microsoft-authentication-library-for-go@v1.9.0) |
| `github.com/BurntSushi/toml@v1.3.2` | application | MIT (`COPYING`) | [source](https://pkg.go.dev/github.com/BurntSushi/toml@v1.3.2) |
| `github.com/BurntSushi/toml@v1.4.1-0.20240526193622-a339e1f7089c` | tools | MIT (`COPYING`) | [source](https://pkg.go.dev/github.com/BurntSushi/toml@v1.4.1-0.20240526193622-a339e1f7089c) |
| `github.com/ClickHouse/ch-go@v0.74.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/ClickHouse/ch-go@v0.74.0) |
| `github.com/ClickHouse/clickhouse-go/v2@v2.48.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/ClickHouse/clickhouse-go/v2@v2.48.0) |
| `github.com/CloudyKit/fastprinter@v0.0.0-20200109182630-33d98a066a53` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/CloudyKit/fastprinter@v0.0.0-20200109182630-33d98a066a53) |
| `github.com/CloudyKit/jet/v6@v6.2.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/CloudyKit/jet/v6@v6.2.0) |
| `github.com/Joker/jade@v1.1.3` | application | BSD-3-Clause (`LICENSE.md`) | [source](https://pkg.go.dev/github.com/Joker/jade@v1.1.3) |
| `github.com/Microsoft/go-winio@v0.6.2` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/Microsoft/go-winio@v0.6.2) |
| `github.com/Shopify/goreferrer@v0.0.0-20220729165902-8cddb4f5de06` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/Shopify/goreferrer@v0.0.0-20220729165902-8cddb4f5de06) |
| `github.com/andybalholm/brotli@v1.2.3` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/andybalholm/brotli@v1.2.3) |
| `github.com/antlr4-go/antlr/v4@v4.13.1` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/antlr4-go/antlr/v4@v4.13.1) |
| `github.com/apache/arrow-go/v18@v18.5.1` | application | Apache-2.0 AND BSD-3-Clause (`LICENSE.txt, NOTICE.txt`) | [source](https://pkg.go.dev/github.com/apache/arrow-go/v18@v18.5.1) |
| `github.com/apapsch/go-jsonmerge/v2@v2.0.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/apapsch/go-jsonmerge/v2@v2.0.0) |
| `github.com/aws/aws-sdk-go-v2@v1.47.0` | application | Apache-2.0 (`LICENSE.txt, NOTICE.txt`) | [source](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2@v1.47.0) |
| `github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream@v1.7.20` | application | Apache-2.0 (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream@v1.7.20) |
| `github.com/aws/aws-sdk-go-v2/internal/configsources@v1.5.3` | application | Apache-2.0 (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/internal/configsources@v1.5.3) |
| `github.com/aws/aws-sdk-go-v2/internal/endpoints/v2@v2.8.3` | application | Apache-2.0 (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/internal/endpoints/v2@v2.8.3) |
| `github.com/aws/aws-sdk-go-v2/internal/v4a@v1.5.3` | application | Apache-2.0 (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/internal/v4a@v1.5.3) |
| `github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding@v1.13.19` | application | Apache-2.0 (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding@v1.13.19) |
| `github.com/aws/aws-sdk-go-v2/service/internal/checksum@v1.11.3` | application | Apache-2.0 (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/internal/checksum@v1.11.3) |
| `github.com/aws/aws-sdk-go-v2/service/internal/presigned-url@v1.14.3` | application | Apache-2.0 (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/internal/presigned-url@v1.14.3) |
| `github.com/aws/aws-sdk-go-v2/service/internal/s3shared@v1.20.3` | application | Apache-2.0 (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/internal/s3shared@v1.20.3) |
| `github.com/aws/aws-sdk-go-v2/service/s3@v1.113.0` | application | Apache-2.0 (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/s3@v1.113.0) |
| `github.com/aws/smithy-go@v1.28.1` | application | Apache-2.0 (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/aws/smithy-go@v1.28.1) |
| `github.com/aymerick/douceur@v0.2.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/aymerick/douceur@v0.2.0) |
| `github.com/blackwell-systems/gcf-go@v1.2.2` | tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/blackwell-systems/gcf-go@v1.2.2) |
| `github.com/bytedance/gopkg@v0.1.3` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/bytedance/gopkg@v0.1.3) |
| `github.com/bytedance/sonic@v1.15.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/bytedance/sonic@v1.15.0) |
| `github.com/bytedance/sonic/loader@v0.5.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/bytedance/sonic/loader@v0.5.0) |
| `github.com/cenkalti/backoff/v4@v4.3.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/cenkalti/backoff/v4@v4.3.0) |
| `github.com/cenkalti/backoff/v5@v5.0.3` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/cenkalti/backoff/v5@v5.0.3) |
| `github.com/cespare/xxhash/v2@v2.3.0` | application | MIT (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/cespare/xxhash/v2@v2.3.0) |
| `github.com/chzyer/logex@v1.1.10` | tools | **UNRESOLVED** (no license or NOTICE material in the downloaded module) | [source](https://pkg.go.dev/github.com/chzyer/logex@v1.1.10) |
| `github.com/chzyer/readline@v0.0.0-20180603132655-2972be24d48e` | tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/chzyer/readline@v0.0.0-20180603132655-2972be24d48e) |
| `github.com/chzyer/test@v0.0.0-20180213035817-a1ea475d72b1` | tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/chzyer/test@v0.0.0-20180213035817-a1ea475d72b1) |
| `github.com/cloudwego/base64x@v0.1.6` | application | Apache-2.0 (`LICENSE, LICENSE-APACHE`) | [source](https://pkg.go.dev/github.com/cloudwego/base64x@v0.1.6) |
| `github.com/cloudwego/iasm@v0.2.0` | application | Apache-2.0 (`LICENSE-APACHE`) | [source](https://pkg.go.dev/github.com/cloudwego/iasm@v0.2.0) |
| `github.com/coder/websocket@v1.8.15` | application | ISC (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/coder/websocket@v1.8.15) |
| `github.com/containerd/errdefs@v1.0.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/containerd/errdefs@v1.0.0) |
| `github.com/containerd/errdefs/pkg@v0.3.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/containerd/errdefs/pkg@v0.3.0) |
| `github.com/containerd/log@v0.1.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/containerd/log@v0.1.0) |
| `github.com/containerd/platforms@v0.2.1` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/containerd/platforms@v0.2.1) |
| `github.com/coreos/go-oidc/v3@v3.21.0` | application | Apache-2.0 (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/coreos/go-oidc/v3@v3.21.0) |
| `github.com/cpuguy83/dockercfg@v0.3.2` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/cpuguy83/dockercfg@v0.3.2) |
| `github.com/cpuguy83/go-md2man/v2@v2.0.6` | application | MIT (`LICENSE.md`) | [source](https://pkg.go.dev/github.com/cpuguy83/go-md2man/v2@v2.0.6) |
| `github.com/danieljoos/wincred@v1.2.3` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/danieljoos/wincred@v1.2.3) |
| `github.com/davecgh/go-spew@v1.1.2-0.20180830191138-d8f796af33cc` | application, tools | ISC (`LICENSE`) | [source](https://pkg.go.dev/github.com/davecgh/go-spew@v1.1.2-0.20180830191138-d8f796af33cc) |
| `github.com/distribution/reference@v0.6.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/distribution/reference@v0.6.0) |
| `github.com/dlclark/regexp2@v1.11.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/dlclark/regexp2@v1.11.0) |
| `github.com/dlclark/regexp2@v1.11.4` | tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/dlclark/regexp2@v1.11.4) |
| `github.com/docker/go-connections@v0.8.1` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/docker/go-connections@v0.8.1) |
| `github.com/docker/go-units@v0.5.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/docker/go-units@v0.5.0) |
| `github.com/dprotaso/go-yit@v0.0.0-20220510233725-9ba8df137936` | tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/dprotaso/go-yit@v0.0.0-20220510233725-9ba8df137936) |
| `github.com/duckdb/duckdb-go-bindings@v0.10505.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/duckdb/duckdb-go-bindings@v0.10505.0) |
| `github.com/duckdb/duckdb-go-bindings/lib/darwin-amd64@v0.10505.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/duckdb/duckdb-go-bindings/lib/darwin-amd64@v0.10505.0) |
| `github.com/duckdb/duckdb-go-bindings/lib/darwin-arm64@v0.10505.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/duckdb/duckdb-go-bindings/lib/darwin-arm64@v0.10505.0) |
| `github.com/duckdb/duckdb-go-bindings/lib/linux-amd64@v0.10505.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/duckdb/duckdb-go-bindings/lib/linux-amd64@v0.10505.0) |
| `github.com/duckdb/duckdb-go-bindings/lib/linux-arm64@v0.10505.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/duckdb/duckdb-go-bindings/lib/linux-arm64@v0.10505.0) |
| `github.com/duckdb/duckdb-go-bindings/lib/windows-amd64@v0.10505.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/duckdb/duckdb-go-bindings/lib/windows-amd64@v0.10505.0) |
| `github.com/duckdb/duckdb-go/v2@v2.10505.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/duckdb/duckdb-go/v2@v2.10505.0) |
| `github.com/dustin/go-humanize@v1.0.1` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/dustin/go-humanize@v1.0.1) |
| `github.com/ebitengine/purego@v0.10.1` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/ebitengine/purego@v0.10.1) |
| `github.com/elastic/go-sysinfo@v1.15.5` | application | Apache-2.0 (`LICENSE.txt, NOTICE.txt`) | [source](https://pkg.go.dev/github.com/elastic/go-sysinfo@v1.15.5) |
| `github.com/elastic/go-windows@v1.0.2` | application | Apache-2.0 (`LICENSE.txt, NOTICE.txt`) | [source](https://pkg.go.dev/github.com/elastic/go-windows@v1.0.2) |
| `github.com/facebookgo/clock@v0.0.0-20150410010913-600d898af40a` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/facebookgo/clock@v0.0.0-20150410010913-600d898af40a) |
| `github.com/fatih/structs@v1.1.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/fatih/structs@v1.1.0) |
| `github.com/felixge/httpsnoop@v1.1.0` | application | MIT (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/felixge/httpsnoop@v1.1.0) |
| `github.com/flosch/pongo2/v4@v4.0.2` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/flosch/pongo2/v4@v4.0.2) |
| `github.com/fsnotify/fsnotify@v1.4.9` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/fsnotify/fsnotify@v1.4.9) |
| `github.com/gabriel-vasile/mimetype@v1.4.12` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/gabriel-vasile/mimetype@v1.4.12) |
| `github.com/getkin/kin-openapi@v0.149.0` | application, tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/getkin/kin-openapi@v0.149.0) |
| `github.com/gin-contrib/sse@v1.1.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/gin-contrib/sse@v1.1.0) |
| `github.com/gin-gonic/gin@v1.12.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/gin-gonic/gin@v1.12.0) |
| `github.com/go-faster/city@v1.0.1` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-faster/city@v1.0.1) |
| `github.com/go-faster/errors@v0.8.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-faster/errors@v0.8.0) |
| `github.com/go-jose/go-jose/v4@v4.1.4` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-jose/go-jose/v4@v4.1.4) |
| `github.com/go-logr/logr@v1.4.4` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-logr/logr@v1.4.4) |
| `github.com/go-logr/stdr@v1.2.2` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-logr/stdr@v1.2.2) |
| `github.com/go-ole/go-ole@v1.3.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-ole/go-ole@v1.3.0) |
| `github.com/go-openapi/jsonpointer@v0.22.5` | application | Apache-2.0 (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/go-openapi/jsonpointer@v0.22.5) |
| `github.com/go-openapi/jsonpointer@v0.23.1` | tools | Apache-2.0 (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/go-openapi/jsonpointer@v0.23.1) |
| `github.com/go-openapi/swag/jsonname@v0.25.5` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-openapi/swag/jsonname@v0.25.5) |
| `github.com/go-openapi/swag/jsonname@v0.26.0` | tools | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-openapi/swag/jsonname@v0.26.0) |
| `github.com/go-openapi/testify/v2@v2.4.2` | tools | Apache-2.0 AND BSD-3-Clause AND ISC AND MIT (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/go-openapi/testify/v2@v2.4.2) |
| `github.com/go-playground/locales@v0.14.1` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-playground/locales@v0.14.1) |
| `github.com/go-playground/universal-translator@v0.18.1` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-playground/universal-translator@v0.18.1) |
| `github.com/go-playground/validator/v10@v10.30.1` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-playground/validator/v10@v10.30.1) |
| `github.com/go-sql-driver/mysql@v1.10.0` | application | MPL-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-sql-driver/mysql@v1.10.0) |
| `github.com/go-task/slim-sprig@v0.0.0-20210107165309-348f09dbbbc0` | tools | MIT (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/go-task/slim-sprig@v0.0.0-20210107165309-348f09dbbbc0) |
| `github.com/go-viper/mapstructure/v2@v2.5.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/go-viper/mapstructure/v2@v2.5.0) |
| `github.com/goccy/go-json@v0.10.5` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/goccy/go-json@v0.10.5) |
| `github.com/goccy/go-yaml@v1.19.2` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/goccy/go-yaml@v1.19.2) |
| `github.com/godbus/dbus/v5@v5.2.2` | application | BSD-2-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/godbus/dbus/v5@v5.2.2) |
| `github.com/gogo/protobuf@v1.3.2` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/gogo/protobuf@v1.3.2) |
| `github.com/golang-jwt/jwt/v4@v4.5.2` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/golang-jwt/jwt/v4@v4.5.2) |
| `github.com/golang-jwt/jwt/v5@v5.3.1` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/golang-jwt/jwt/v5@v5.3.1) |
| `github.com/golang-sql/civil@v0.0.0-20220223132316-b832511892a9` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/golang-sql/civil@v0.0.0-20220223132316-b832511892a9) |
| `github.com/golang-sql/sqlexp@v0.1.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/golang-sql/sqlexp@v0.1.0) |
| `github.com/golang/mock@v1.6.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/golang/mock@v1.6.0) |
| `github.com/golang/protobuf@v1.5.2` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/golang/protobuf@v1.5.2) |
| `github.com/golang/snappy@v0.0.4` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/golang/snappy@v0.0.4) |
| `github.com/gomarkdown/markdown@v0.0.0-20240328165702-4d01890c35c0` | application | BSD-2-Clause (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/gomarkdown/markdown@v0.0.0-20240328165702-4d01890c35c0) |
| `github.com/google/flatbuffers@v25.12.19+incompatible` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/google/flatbuffers@v25.12.19+incompatible) |
| `github.com/google/go-cmdtest@v0.4.1-0.20220921163831-55ab3332a786` | tools | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/google/go-cmdtest@v0.4.1-0.20220921163831-55ab3332a786) |
| `github.com/google/go-cmp@v0.7.0` | application, tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/google/go-cmp@v0.7.0) |
| `github.com/google/pprof@v0.0.0-20210407192527-94a9f03dee38` | tools | Apache-2.0 AND BSD-3-Clause (`LICENSE, third_party/*/LICENSE`) | [source](https://pkg.go.dev/github.com/google/pprof@v0.0.0-20210407192527-94a9f03dee38) |
| `github.com/google/renameio@v0.1.0` | tools | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/google/renameio@v0.1.0) |
| `github.com/google/uuid@v1.6.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/google/uuid@v1.6.0) |
| `github.com/gorilla/css@v1.0.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/gorilla/css@v1.0.0) |
| `github.com/gorilla/mux@v1.8.0` | application, tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/gorilla/mux@v1.8.0) |
| `github.com/grpc-ecosystem/go-grpc-middleware/v2@v2.3.2` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/grpc-ecosystem/go-grpc-middleware/v2@v2.3.2) |
| `github.com/grpc-ecosystem/grpc-gateway/v2@v2.30.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/grpc-ecosystem/grpc-gateway/v2@v2.30.0) |
| `github.com/hpcloud/tail@v1.0.0` | tools | MIT (`LICENSE.txt, ratelimiter/Licence`) | [source](https://pkg.go.dev/github.com/hpcloud/tail@v1.0.0) |
| `github.com/ianlancetaylor/demangle@v0.0.0-20200824232613-28f6c0f3b639` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/ianlancetaylor/demangle@v0.0.0-20200824232613-28f6c0f3b639) |
| `github.com/inconshreveable/mousetrap@v1.1.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/inconshreveable/mousetrap@v1.1.0) |
| `github.com/iris-contrib/schema@v0.0.6` | application | BSD-3-Clause (`README.md#License`) | [source](https://pkg.go.dev/github.com/iris-contrib/schema@v0.0.6) |
| `github.com/jackc/pgpassfile@v1.0.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/jackc/pgpassfile@v1.0.0) |
| `github.com/jackc/pgservicefile@v0.0.0-20240606120523-5a60cdf6a761` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/jackc/pgservicefile@v0.0.0-20240606120523-5a60cdf6a761) |
| `github.com/jackc/pgx/v5@v5.11.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/jackc/pgx/v5@v5.11.0) |
| `github.com/jackc/puddle/v2@v2.2.2` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/jackc/puddle/v2@v2.2.2) |
| `github.com/joho/godotenv@v1.5.1` | application | MIT (`LICENCE`) | [source](https://pkg.go.dev/github.com/joho/godotenv@v1.5.1) |
| `github.com/jonboulle/clockwork@v0.5.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/jonboulle/clockwork@v0.5.0) |
| `github.com/josharian/intern@v1.0.0` | application | MIT (`LICENSE.md`) | [source](https://pkg.go.dev/github.com/josharian/intern@v1.0.0) |
| `github.com/json-iterator/go@v1.1.12` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/json-iterator/go@v1.1.12) |
| `github.com/kataras/blocks@v0.0.8` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/kataras/blocks@v0.0.8) |
| `github.com/kataras/golog@v0.1.11` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/kataras/golog@v0.1.11) |
| `github.com/kataras/iris/v12@v12.2.11` | application | BSD-3-Clause (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/kataras/iris/v12@v12.2.11) |
| `github.com/kataras/pio@v0.0.13` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/kataras/pio@v0.0.13) |
| `github.com/kataras/sitemap@v0.0.6` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/kataras/sitemap@v0.0.6) |
| `github.com/kataras/tunnel@v0.0.4` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/kataras/tunnel@v0.0.4) |
| `github.com/klauspost/compress@v1.19.2` | application | Apache-2.0 AND BSD-3-Clause AND MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/klauspost/compress@v1.19.2) |
| `github.com/klauspost/cpuid/v2@v2.3.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/klauspost/cpuid/v2@v2.3.0) |
| `github.com/kr/pretty@v0.3.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/kr/pretty@v0.3.0) |
| `github.com/kr/pretty@v0.3.1` | tools | MIT (`License`) | [source](https://pkg.go.dev/github.com/kr/pretty@v0.3.1) |
| `github.com/kr/pty@v1.1.1` | tools | MIT (`License`) | [source](https://pkg.go.dev/github.com/kr/pty@v1.1.1) |
| `github.com/kr/text@v0.2.0` | application, tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/kr/text@v0.2.0) |
| `github.com/kylelemons/godebug@v1.1.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/kylelemons/godebug@v1.1.0) |
| `github.com/labstack/echo/v4@v4.15.1` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/labstack/echo/v4@v4.15.1) |
| `github.com/labstack/gommon@v0.4.2` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/labstack/gommon@v0.4.2) |
| `github.com/leodido/go-urn@v1.4.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/leodido/go-urn@v1.4.0) |
| `github.com/lufia/plan9stats@v0.0.0-20260330125221-c963978e514e` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/lufia/plan9stats@v0.0.0-20260330125221-c963978e514e) |
| `github.com/magiconair/properties@v1.8.10` | application | BSD-2-Clause (`LICENSE.md`) | [source](https://pkg.go.dev/github.com/magiconair/properties@v1.8.10) |
| `github.com/mailgun/raymond/v2@v2.0.48` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/mailgun/raymond/v2@v2.0.48) |
| `github.com/mailru/easyjson@v0.7.7` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/mailru/easyjson@v0.7.7) |
| `github.com/mattn/go-colorable@v0.1.14` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/mattn/go-colorable@v0.1.14) |
| `github.com/mattn/go-isatty@v0.0.24` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/mattn/go-isatty@v0.0.24) |
| `github.com/mfridman/interpolate@v0.0.2` | application | MIT (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/mfridman/interpolate@v0.0.2) |
| `github.com/mfridman/xflag@v0.1.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/mfridman/xflag@v0.1.0) |
| `github.com/microcosm-cc/bluemonday@v1.0.26` | application | BSD-3-Clause (`LICENSE.md`) | [source](https://pkg.go.dev/github.com/microcosm-cc/bluemonday@v1.0.26) |
| `github.com/microsoft/go-mssqldb@v1.11.0` | application | BSD-3-Clause (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/microsoft/go-mssqldb@v1.11.0) |
| `github.com/moby/docker-image-spec@v1.3.1` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/moby/docker-image-spec@v1.3.1) |
| `github.com/moby/go-archive@v0.2.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/moby/go-archive@v0.2.0) |
| `github.com/moby/moby/api@v1.55.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/moby/moby/api@v1.55.0) |
| `github.com/moby/moby/client@v0.5.1` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/moby/moby/client@v0.5.1) |
| `github.com/moby/patternmatcher@v0.6.1` | application | Apache-2.0 (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/moby/patternmatcher@v0.6.1) |
| `github.com/moby/sys/sequential@v0.7.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/moby/sys/sequential@v0.7.0) |
| `github.com/moby/sys/user@v0.4.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/moby/sys/user@v0.4.0) |
| `github.com/moby/sys/userns@v0.1.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/moby/sys/userns@v0.1.0) |
| `github.com/moby/term@v0.5.2` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/moby/term@v0.5.2) |
| `github.com/modern-go/concurrent@v0.0.0-20180306012644-bacd9c7ef1dd` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/modern-go/concurrent@v0.0.0-20180306012644-bacd9c7ef1dd) |
| `github.com/modern-go/reflect2@v1.0.2` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/modern-go/reflect2@v1.0.2) |
| `github.com/ncruces/go-strftime@v1.0.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/ncruces/go-strftime@v1.0.0) |
| `github.com/nexus-rpc/nexus-proto-annotations@v0.1.0` | application | MIT (`upstream LICENSE (omitted from v0.1.0 module zip)`) | [source](https://pkg.go.dev/github.com/nexus-rpc/nexus-proto-annotations@v0.1.0) |
| `github.com/nexus-rpc/sdk-go@v0.7.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/nexus-rpc/sdk-go@v0.7.0) |
| `github.com/nxadm/tail@v1.4.8` | tools | MIT (`LICENSE, ratelimiter/Licence`) | [source](https://pkg.go.dev/github.com/nxadm/tail@v1.4.8) |
| `github.com/oapi-codegen/nullable@v1.1.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/oapi-codegen/nullable@v1.1.0) |
| `github.com/oapi-codegen/oapi-codegen/v2@v2.8.0` | tools | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/oapi-codegen/oapi-codegen/v2@v2.8.0) |
| `github.com/oapi-codegen/runtime@v1.7.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/oapi-codegen/runtime@v1.7.0) |
| `github.com/oasdiff/yaml@v0.1.1` | application, tools | BSD-3-Clause AND MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/oasdiff/yaml@v0.1.1) |
| `github.com/oasdiff/yaml3@v0.0.14` | application, tools | Apache-2.0 AND MIT (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/oasdiff/yaml3@v0.0.14) |
| `github.com/onsi/ginkgo@v1.16.4` | tools | MIT (`LICENSE, reporters/stenographer/support/*/LICENSE`) | [source](https://pkg.go.dev/github.com/onsi/ginkgo@v1.16.4) |
| `github.com/onsi/ginkgo/v2@v2.1.3` | tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/onsi/ginkgo/v2@v2.1.3) |
| `github.com/onsi/gomega@v1.19.0` | tools | MIT (`LICENSE, matchers/support/goraph/MIT.LICENSE`) | [source](https://pkg.go.dev/github.com/onsi/gomega@v1.19.0) |
| `github.com/opencontainers/go-digest@v1.0.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/opencontainers/go-digest@v1.0.0) |
| `github.com/opencontainers/image-spec@v1.1.1` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/opencontainers/image-spec@v1.1.1) |
| `github.com/paulmach/orb@v0.13.0` | application | MIT (`LICENSE.md`) | [source](https://pkg.go.dev/github.com/paulmach/orb@v0.13.0) |
| `github.com/pelletier/go-toml/v2@v2.2.4` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/pelletier/go-toml/v2@v2.2.4) |
| `github.com/pierrec/lz4/v4@v4.1.29` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/pierrec/lz4/v4@v4.1.29) |
| `github.com/pkg/browser@v0.0.0-20240102092130-5ac0b6a4141c` | application | BSD-2-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/pkg/browser@v0.0.0-20240102092130-5ac0b6a4141c) |
| `github.com/pmezard/go-difflib@v1.0.1-0.20181226105442-5d4384ee4fb2` | application, tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/pmezard/go-difflib@v1.0.1-0.20181226105442-5d4384ee4fb2) |
| `github.com/power-devops/perfstat@v0.0.0-20240221224432-82ca36839d55` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/power-devops/perfstat@v0.0.0-20240221224432-82ca36839d55) |
| `github.com/pressly/goose/v3@v3.28.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/pressly/goose/v3@v3.28.0) |
| `github.com/prometheus/procfs@v0.22.0` | application | Apache-2.0 (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/prometheus/procfs@v0.22.0) |
| `github.com/quic-go/qpack@v0.6.0` | application | MIT (`LICENSE.md`) | [source](https://pkg.go.dev/github.com/quic-go/qpack@v0.6.0) |
| `github.com/quic-go/quic-go@v0.59.1` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/quic-go/quic-go@v0.59.1) |
| `github.com/remyoudompheng/bigfft@v0.0.0-20230129092748-24d4a6f8daec` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/remyoudompheng/bigfft@v0.0.0-20230129092748-24d4a6f8daec) |
| `github.com/robfig/cron@v1.2.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/robfig/cron@v1.2.0) |
| `github.com/rogpeppe/go-internal@v1.11.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/rogpeppe/go-internal@v1.11.0) |
| `github.com/rogpeppe/go-internal@v1.14.1` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/rogpeppe/go-internal@v1.14.1) |
| `github.com/russross/blackfriday/v2@v2.1.0` | application | BSD-2-Clause (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/russross/blackfriday/v2@v2.1.0) |
| `github.com/santhosh-tekuri/jsonschema/v6@v6.0.3` | application, tools | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/santhosh-tekuri/jsonschema/v6@v6.0.3) |
| `github.com/schollz/closestmatch@v2.1.0+incompatible` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/schollz/closestmatch@v2.1.0+incompatible) |
| `github.com/segmentio/asm@v1.2.1` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/segmentio/asm@v1.2.1) |
| `github.com/sergi/go-diff@v1.1.0` | tools | Apache-2.0 AND MIT (`APACHE-LICENSE-2.0, LICENSE`) | [source](https://pkg.go.dev/github.com/sergi/go-diff@v1.1.0) |
| `github.com/sethvargo/go-retry@v0.4.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/sethvargo/go-retry@v0.4.0) |
| `github.com/shirou/gopsutil/v4@v4.26.6` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/shirou/gopsutil/v4@v4.26.6) |
| `github.com/shopspring/decimal@v1.4.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/shopspring/decimal@v1.4.0) |
| `github.com/sirupsen/logrus@v1.9.4` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/sirupsen/logrus@v1.9.4) |
| `github.com/speakeasy-api/jsonpath@v0.6.3` | tools | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/speakeasy-api/jsonpath@v0.6.3) |
| `github.com/speakeasy-api/openapi@v1.24.0` | tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/speakeasy-api/openapi@v1.24.0) |
| `github.com/spf13/cobra@v1.10.2` | application | Apache-2.0 (`LICENSE.txt`) | [source](https://pkg.go.dev/github.com/spf13/cobra@v1.10.2) |
| `github.com/spf13/pflag@v1.0.9` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/spf13/pflag@v1.0.9) |
| `github.com/stretchr/objx@v0.1.0` | tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/stretchr/objx@v0.1.0) |
| `github.com/stretchr/objx@v0.5.3` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/stretchr/objx@v0.5.3) |
| `github.com/stretchr/testify@v1.11.1` | tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/stretchr/testify@v1.11.1) |
| `github.com/stretchr/testify@v1.12.1` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/stretchr/testify@v1.12.1) |
| `github.com/tdewolff/minify/v2@v2.20.19` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/tdewolff/minify/v2@v2.20.19) |
| `github.com/tdewolff/parse/v2@v2.7.12` | application | MIT (`LICENSE.md`) | [source](https://pkg.go.dev/github.com/tdewolff/parse/v2@v2.7.12) |
| `github.com/testcontainers/testcontainers-go@v0.44.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/testcontainers/testcontainers-go@v0.44.0) |
| `github.com/tklauser/go-sysconf@v0.4.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/tklauser/go-sysconf@v0.4.0) |
| `github.com/tklauser/numcpus@v0.12.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/tklauser/numcpus@v0.12.0) |
| `github.com/tursodatabase/libsql-client-go@v0.0.0-20260528064733-9d5d30a29a60` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/tursodatabase/libsql-client-go@v0.0.0-20260528064733-9d5d30a29a60) |
| `github.com/twitchyliquid64/golang-asm@v0.15.1` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/twitchyliquid64/golang-asm@v0.15.1) |
| `github.com/ugorji/go/codec@v1.3.1` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/ugorji/go/codec@v1.3.1) |
| `github.com/valyala/bytebufferpool@v1.0.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/valyala/bytebufferpool@v1.0.0) |
| `github.com/valyala/fasttemplate@v1.2.2` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/valyala/fasttemplate@v1.2.2) |
| `github.com/vertica/vertica-sql-go@v1.3.8` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/github.com/vertica/vertica-sql-go@v1.3.8) |
| `github.com/vmihailenco/msgpack/v5@v5.4.1` | application | BSD-2-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/vmihailenco/msgpack/v5@v5.4.1) |
| `github.com/vmihailenco/tagparser/v2@v2.0.0` | application | BSD-2-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/vmihailenco/tagparser/v2@v2.0.0) |
| `github.com/vmware-labs/yaml-jsonpath@v0.3.2` | tools | Apache-2.0 (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/vmware-labs/yaml-jsonpath@v0.3.2) |
| `github.com/ydb-platform/ydb-go-genproto@v0.0.0-20260810123728-f0c151ab31b9` | application | Apache-2.0 (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/ydb-platform/ydb-go-genproto@v0.0.0-20260810123728-f0c151ab31b9) |
| `github.com/ydb-platform/ydb-go-sdk/v3@v3.151.1` | application | Apache-2.0 (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/github.com/ydb-platform/ydb-go-sdk/v3@v3.151.1) |
| `github.com/yosssi/ace@v0.0.5` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/yosssi/ace@v0.0.5) |
| `github.com/yuin/goldmark@v1.4.13` | tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/yuin/goldmark@v1.4.13) |
| `github.com/yusufpapurcu/wmi@v1.2.4` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/yusufpapurcu/wmi@v1.2.4) |
| `github.com/zalando/go-keyring@v0.2.8` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/github.com/zalando/go-keyring@v0.2.8) |
| `github.com/zeebo/xxh3@v1.1.0` | application | BSD-2-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/zeebo/xxh3@v1.1.0) |
| `github.com/ziutek/mymysql@v1.5.4` | application | BSD-2-Clause (`LICENSE`) | [source](https://pkg.go.dev/github.com/ziutek/mymysql@v1.5.4) |
| `go.mongodb.org/mongo-driver/v2@v2.5.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/go.mongodb.org/mongo-driver/v2@v2.5.0) |
| `go.opentelemetry.io/auto/sdk@v1.2.1` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/auto/sdk@v1.2.1) |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.71.0` | application | Apache-2.0 AND BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.71.0) |
| `go.opentelemetry.io/otel@v1.46.0` | application | Apache-2.0 AND BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/otel@v1.46.0) |
| `go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp@v1.46.0` | application | Apache-2.0 AND BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp@v1.46.0) |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace@v1.46.0` | application | Apache-2.0 AND BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlptrace@v1.46.0) |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.46.0` | application | Apache-2.0 AND BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.46.0) |
| `go.opentelemetry.io/otel/metric@v1.46.0` | application | Apache-2.0 AND BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/otel/metric@v1.46.0) |
| `go.opentelemetry.io/otel/metric/x@v0.68.0` | application | Apache-2.0 AND BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/otel/metric/x@v0.68.0) |
| `go.opentelemetry.io/otel/sdk@v1.46.0` | application | Apache-2.0 AND BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/otel/sdk@v1.46.0) |
| `go.opentelemetry.io/otel/sdk/metric@v1.46.0` | application | Apache-2.0 AND BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/otel/sdk/metric@v1.46.0) |
| `go.opentelemetry.io/otel/trace@v1.46.0` | application | Apache-2.0 AND BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/otel/trace@v1.46.0) |
| `go.opentelemetry.io/proto/otlp@v1.11.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/go.opentelemetry.io/proto/otlp@v1.11.0) |
| `go.temporal.io/api@v1.63.4` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/go.temporal.io/api@v1.63.4) |
| `go.temporal.io/sdk@v1.48.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/go.temporal.io/sdk@v1.48.0) |
| `go.uber.org/goleak@v1.3.0` | application | MIT (`LICENSE`) | [source](https://pkg.go.dev/go.uber.org/goleak@v1.3.0) |
| `go.uber.org/mock@v0.6.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/go.uber.org/mock@v0.6.0) |
| `go.uber.org/multierr@v1.11.0` | application | MIT (`LICENSE.txt`) | [source](https://pkg.go.dev/go.uber.org/multierr@v1.11.0) |
| `go.yaml.in/yaml/v3@v3.0.5` | application, tools | Apache-2.0 AND MIT (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/go.yaml.in/yaml/v3@v3.0.5) |
| `golang.org/x/arch@v0.22.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/arch@v0.22.0) |
| `golang.org/x/crypto@v0.0.0-20200622213623-75b288015ac9` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/crypto@v0.0.0-20200622213623-75b288015ac9) |
| `golang.org/x/crypto@v0.55.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/crypto@v0.55.0) |
| `golang.org/x/exp@v0.0.0-20231110203233-9a3e6036ecaa` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/exp@v0.0.0-20231110203233-9a3e6036ecaa) |
| `golang.org/x/exp@v0.0.0-20260824195058-e88cd73687aa` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/exp@v0.0.0-20260824195058-e88cd73687aa) |
| `golang.org/x/exp/typeparams@v0.0.0-20231108232855-2478ac86f678` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/exp/typeparams@v0.0.0-20231108232855-2478ac86f678) |
| `golang.org/x/mod@v0.32.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/mod@v0.32.0) |
| `golang.org/x/mod@v0.41.0` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/mod@v0.41.0) |
| `golang.org/x/net@v0.58.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/net@v0.58.0) |
| `golang.org/x/net@v0.59.0` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/net@v0.59.0) |
| `golang.org/x/oauth2@v0.37.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/oauth2@v0.37.0) |
| `golang.org/x/sync@v0.23.0` | application, tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/sync@v0.23.0) |
| `golang.org/x/sys@v0.47.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/sys@v0.47.0) |
| `golang.org/x/sys@v0.48.0` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/sys@v0.48.0) |
| `golang.org/x/telemetry@v0.0.0-20260116145544-c6413dc483f5` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/telemetry@v0.0.0-20260116145544-c6413dc483f5) |
| `golang.org/x/telemetry@v0.0.0-20260908163034-4bcc4b2ee518` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/telemetry@v0.0.0-20260908163034-4bcc4b2ee518) |
| `golang.org/x/term@v0.0.0-20210927222741-03fcf44c2211` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/term@v0.0.0-20210927222741-03fcf44c2211) |
| `golang.org/x/text@v0.40.0` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/text@v0.40.0) |
| `golang.org/x/text@v0.41.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/text@v0.41.0) |
| `golang.org/x/time@v0.16.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/time@v0.16.0) |
| `golang.org/x/tools@v0.41.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/tools@v0.41.0) |
| `golang.org/x/tools@v0.50.0` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/tools@v0.50.0) |
| `golang.org/x/tools/go/expect@v0.1.1-deprecated` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/tools/go/expect@v0.1.1-deprecated) |
| `golang.org/x/tools/go/packages/packagestest@v0.1.1-deprecated` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/tools/go/packages/packagestest@v0.1.1-deprecated) |
| `golang.org/x/vuln@v1.8.0` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/vuln@v1.8.0) |
| `golang.org/x/xerrors@v0.0.0-20200804184101-5ec99f83aff1` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/xerrors@v0.0.0-20200804184101-5ec99f83aff1) |
| `golang.org/x/xerrors@v0.0.0-20240903120638-7835f813f4da` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/golang.org/x/xerrors@v0.0.0-20240903120638-7835f813f4da) |
| `google.golang.org/genproto/googleapis/api@v0.0.0-20260819154853-08b0e4226688` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/google.golang.org/genproto/googleapis/api@v0.0.0-20260819154853-08b0e4226688) |
| `google.golang.org/genproto/googleapis/rpc@v0.0.0-20260831171406-18b4a7587f8a` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/google.golang.org/genproto/googleapis/rpc@v0.0.0-20260831171406-18b4a7587f8a) |
| `google.golang.org/grpc@v1.83.2` | application | Apache-2.0 (`LICENSE, NOTICE.txt`) | [source](https://pkg.go.dev/google.golang.org/grpc@v1.83.2) |
| `google.golang.org/protobuf@v1.26.0` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/google.golang.org/protobuf@v1.26.0) |
| `google.golang.org/protobuf@v1.36.12` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/google.golang.org/protobuf@v1.36.12) |
| `gopkg.in/check.v1@v1.0.0-20201130134442-10cb98267c6c` | application, tools | BSD-2-Clause (`LICENSE`) | [source](https://pkg.go.dev/gopkg.in/check.v1@v1.0.0-20201130134442-10cb98267c6c) |
| `gopkg.in/fsnotify.v1@v1.4.7` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/gopkg.in/fsnotify.v1@v1.4.7) |
| `gopkg.in/ini.v1@v1.67.0` | application | Apache-2.0 (`LICENSE`) | [source](https://pkg.go.dev/gopkg.in/ini.v1@v1.67.0) |
| `gopkg.in/tomb.v1@v1.0.0-20141024135613-dd632973f1e7` | tools | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/gopkg.in/tomb.v1@v1.0.0-20141024135613-dd632973f1e7) |
| `gopkg.in/yaml.v2@v2.4.0` | tools | Apache-2.0 AND MIT (`LICENSE, LICENSE.libyaml, NOTICE`) | [source](https://pkg.go.dev/gopkg.in/yaml.v2@v2.4.0) |
| `gopkg.in/yaml.v3@v3.0.1` | application, tools | Apache-2.0 AND MIT (`LICENSE, NOTICE`) | [source](https://pkg.go.dev/gopkg.in/yaml.v3@v3.0.1) |
| `honnef.co/go/tools@v0.8.1` | tools | MIT (`LICENSE`) | [source](https://pkg.go.dev/honnef.co/go/tools@v0.8.1) |
| `howett.net/plist@v1.0.1` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/howett.net/plist@v1.0.1) |
| `modernc.org/libc@v1.75.6` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/modernc.org/libc@v1.75.6) |
| `modernc.org/mathutil@v1.7.1` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/modernc.org/mathutil@v1.7.1) |
| `modernc.org/memory@v1.12.1` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/modernc.org/memory@v1.12.1) |
| `modernc.org/sqlite@v1.57.0` | application | BSD-3-Clause (`LICENSE`) | [source](https://pkg.go.dev/modernc.org/sqlite@v1.57.0) |

## License text locations

Except for `github.com/nexus-rpc/nexus-proto-annotations@v0.1.0`'s omitted upstream license and `github.com/chzyer/logex@v1.1.10`'s unresolved license, the exact module archives selected by `go.sum` and `tools/go.sum` contain the license files named in the inventory. Canonical license texts are also available from the SPDX license list for [Apache-2.0](https://spdx.org/licenses/Apache-2.0.html), [BSD-2-Clause](https://spdx.org/licenses/BSD-2-Clause.html), [BSD-3-Clause](https://spdx.org/licenses/BSD-3-Clause.html), [ISC](https://spdx.org/licenses/ISC.html), [MIT](https://spdx.org/licenses/MIT.html), and [MPL-2.0](https://spdx.org/licenses/MPL-2.0.html). Preserve component-specific copyright, NOTICE, exception, and file-level terms from the locked sources; a generic license link does not replace them.
