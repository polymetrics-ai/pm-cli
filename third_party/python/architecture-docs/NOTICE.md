# Architecture documentation CI dependencies

These Linux x64 / Python 3.12 wheels are test-only inputs. The workflow verifies each SHA-256 then extracts them only into job-owned disposable storage. They are not application runtime dependencies and are never installed into root-owned runner tooling.

| Package | Version | License | Purpose | SHA-256 |
| --- | --- | --- | --- | --- |
| attrs | 25.4.0 | MIT | jsonschema object model dependency | `adcf7e2a1fb3b36ac48d97835bb6d8ade15b8dcce26aba8bf1d14847b57a3373` |
| jsonschema | 4.25.1 | MIT | Draft 2020-12 architecture schema validation | `3fba0169e345c7175110351d456342c364814cfcf3b964ba4587f22915230a63` |
| jsonschema-specifications | 2025.9.1 | MIT | Draft 2020-12 metaschema registry | `98802fee3a11ee76ecaca44429fda8a41bff98b00a0f2838151b113f210cc6fe` |
| referencing | 0.37.0 | MIT | JSON Schema reference resolution | `381329a9f99628c9069361716891d34ad94af76e461dcb0335825aecc7692231` |
| rpds-py | 0.30.0 | MIT | referencing persistent data structures | `47f236970bccb2233267d89173d3ad2703cd36a0e2a6e92d0560d333871a3d23` |
| typing-extensions | 4.15.0 | PSF-2.0 | Python 3.12 conditional dependency of referencing | `f0fa19c6845758ab08074a0cfa8b7aecb71c999ca73d62883bc25cc018c4e548` |

Source URLs and complete pinned wheel identities are in `dependency-manifest.json`; package license files remain embedded in the corresponding wheel distributions.
