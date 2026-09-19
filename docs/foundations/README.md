# Foundation Atlas

The Foundation Atlas is a deterministic authoring projection of the accepted planning, ownership, validation, packet, and Go declaration sources. It is not a runtime registry, an admission gate, or evidence that a connector, operation, lane, handler, or provider is ready.

## Generated projections

- `atlas.json` lists all versioned foundations, their design dependencies and consumers, mapped requirements, exact file owners, observed source declarations, and explicit gaps.
- `requirements.json` lists all current requirement-to-foundation impact mappings, registration declarations, file owners, planned status, and retained evidence limits.
- `traceability.schema.json` defines the closed shape of both projections.

The generator consumes these canonical inputs:

- `docs/architecture/pm-server/architecture/FOUNDATIONS.json`
- `docs/architecture/pm-server/architecture/FOUNDATION-MAPPINGS.json`
- `docs/architecture/pm-server/implementation/FILE-OWNERSHIP.json`
- `docs/architecture/pm-server/implementation/VALIDATION-MATRIX.json`
- `docs/architecture/pm-server/implementation/PACKET-CONTRACTS.json`

Run it from the repository root through the pinned bootstrap tools:

```sh
python3 tools/bootstrap.py exec go -- -C tools run ./atlas --root ..
python3 tools/bootstrap.py exec go -- -C tools run ./atlas --root .. --check
```

The first command validates the complete source closure before atomically replacing each projection. The second derives both documents in memory and compares their exact bytes without writing. The normal command requires all 20 foundations, 43 requirements, and 44 canonical packets. Count overrides are reserved for V80's isolated destructive fixtures and cannot qualify the project map.

The focused conformance entrypoints are:

```sh
python3 tools/bootstrap.py exec go -- -C tools test . -run '^TestTraceabilityClosure$' -count=1 -timeout 5m
python3 tools/bootstrap.py exec bazelisk -- --batch test //tools:tools_test --test_output=errors --cache_test_results=no
```

The Bazel test receives the prebuilt `//tools/atlas:atlas` executable as declared runfile data; it does not compile Atlas through an ambient checkout or Go executable. The ordinary tools-module command keeps the pinned Go path available for non-Bazel development.

## State and authority

The projection keeps three independent facts visible:

- `implementation_state` is copied from the owning foundation declaration: `planned`, `unavailable`, or `implemented`.
- `evidence_state` is copied as `unexecuted`; this authoring tool refuses pass labels because no proof-owner receipt contract is part of its input closure.
- `capability_state` is always `not_established_by_atlas`; executable availability still requires the actual application/activity registry and compatible operation closure.

Observed `path:symbol` declarations report only source presence. They do not prove that a process composes or reaches the declaration. Missing declarations on planned or unavailable foundations remain explicit gaps; an `implemented` claim with a missing declaration is rejected. Requirement mappings remain impact unions rather than an unconditional runtime AND over every listed foundation.

`ownership_package` and `ownership_process` retain the aggregate FILE-OWNERSHIP responsibility; `assignment_owner`, `go_package`, and `packet_process` retain the packet's per-file declarations. They are deliberately separate. In particular, a durable-admission packet may own a store file without turning it into package `app` or authorizing a store-to-app/auth dependency cycle.

V80 also rejects unresolved foundation versions, missing consumers, ownerless Go files, conflicting file-owner crosswalks, duplicate semantic authorities, stale projection bytes, and pass claims without an exact receipt. Packet-only accepted file amendments may extend the older file-ownership map, but overlapping declarations must retain the same group identity. No unknown path is silently ignored.
