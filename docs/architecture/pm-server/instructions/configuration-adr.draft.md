# Inactive instruction proposal

This file does not activate an application instruction or install a skill. Intended target is recorded in INSTRUCTION-TARGETS.json. Relative paths in the draft are evaluated only at that proposed target.

# 0001: Cobra and strict client/server configuration boundaries

Status: proposed remote-server amendment; apply only through approved bootstrap issue. The inspected repository is setup-only. This decision does not certify runtime behavior.

Retain Cobra and strict typed configuration; do not add Viper initially. Supersede this ADR's former local state root, local connection/schema ownership, CLI credential ingress and CLI-composed runtime. The authoritative remote-server architecture is `docs/architecture/pm-server/architecture/PLAN.md`, with CONTRACTS.json and ADRS.md beside it.

The thin CLI may resolve explicit server profile, workspace/connection selectors and presentation settings. Use flags > explicitly allowed snapshotted PM environment > chosen non-secret client profile > defaults, preserving absent/null/empty distinctions. No arbitrary working-directory .env search, generic HTTP/body override or provider-credential resolver exists in the CLI. Concurrent invocations receive explicit workspace context; mutable global workspace selection is not authority. A scoped server-issued PM session is stored using the qualified client session mechanism.

The server owns one strict typed configuration schema across bundled and operator-managed endpoints: PostgreSQL, Temporal, artifact storage, identity, server-side secret references and resource budgets. Unknown/duplicate/wrong-case keys and invalid types fail before startup. There is no local/distributed switch that selects another product engine. Installation components and operator startup are distinct.

CLI help projects a server-owned versioned catalog, with a scoped revision-keyed cache for usability. Stale/missing schemas are explicit; authoritative admission and authorization always run on the server. The client cannot choose provider policy by editing descriptors. Vendor inputs and PM controls have separate namespaces and immutable semantic IDs. No credential values in help/output/cache.

Cobra stays inside the CLI adapter. Server configuration belongs to internal/config; authoritative use cases belong to internal/app. Framework types do not cross domain boundaries. API generation/contract packages own shared DTOs, not provider execution policy.

Planned tests are UNEXECUTED: strict precedence/presence and malformed config, server-profile/workspace concurrency isolation, reserved-control/provider collision, stale descriptor rejection, secret-free output, matching CLI/UI admission, and no alternate local backend. Concrete commands are maintained by the approved build/development issue, not asserted here before code exists.

Revisit Viper only for an accepted additional configuration-source requirement when a measured comparison preserves strictness and reduces maintained complexity. Do not adopt weak typing, global mutable config or hot reload by default.
