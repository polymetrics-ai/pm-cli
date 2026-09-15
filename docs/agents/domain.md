# PM domain glossary

This glossary gives fresh workers stable navigation terms. It summarizes, but does not replace, the canonical [architecture overview](../architecture/overview.md), [contracts](../architecture/pm-server/architecture/CONTRACTS.json), [state ownership](../architecture/pm-server/architecture/STATE-OWNERSHIP.json), and accepted issue retrieved through the [issue/spec tracker](issue-tracker.md).

## People, scope, and clients

- **Organization / tenant** — Top-level authorization and billing boundary. Tenant identity is explicit and never inferred from a resource name.
- **Workspace** — Collaboration and policy scope inside one organization. Every resource access rechecks workspace and resource authorization; there is no mutable global current workspace.
- **Principal / actor / effective actor** — Authenticated identity, recorded initiator, and delegated execution identity. Delegation never erases either identity or expand its grant.
- **PM session** — Server-issued client authentication material held only through the qualified client mechanism. It is distinct from a provider credential and from a durable execution grant.
- **CLI / browser** — Thin clients of the same public OpenAPI 3.1 use cases. They select explicit context, submit typed requests, and render server results; they do not execute providers or own durable jobs.

## Connector and schema terms

- **Connector** — Declared integration with one provider source and runtime capabilities. Initial connectors are GitHub, PostgreSQL, Gong, Pylon, and HubSpot.
- **Connection** — Server-owned configuration and account binding for a connector, including opaque credential references and revisions. Client profiles do not own it.
- **Operation** — Stable typed provider action derived from admitted source facts. Friendly command names are projections and do not replace operation identity.
- **Lane** — One of `direct_read`, `direct_write`, `etl`, `reverse_etl`, `binary_download`, `binary_upload`, or `sync_transport`. Inventorying a lane is not implementing or certifying it.
- **Source lock / reference closure** — Immutable source identity, captured bytes, recursive references, hashes, and attribution selected by the owning issue. A documentation link alone is not a source lock.
- **Bundle** — Deterministic immutable runtime execution facts generated from admitted source and amendments. It is not hand-edited and does not carry private research or proof authority.
- **Schema snapshot** — Complete immutable account- and permission-scoped metadata candidate selected under PostgreSQL authority. Partial discovery cannot replace a complete active snapshot.
- **Foundation** — Versioned reusable semantic mechanism with a named owner and composition seam. Foundation mappings express design impact; actual runtime capability uses only the exact closure required by an operation.
- **Atlas gap** — Explicit authoring/compatibility record for missing semantics. It blocks the affected cell but is not a runtime registry or provider proof.
- **Capability** — Operation plus compatible foundation versions and an actual registered handler. A descriptor or bundle without executable composition is unavailable.

## Durable work and effects

- **RunSpec / run** — Immutable accepted intent and its durable identity, followed by mutable observed state. PostgreSQL records both before acceptance is returned.
- **Task / action / member** — Orchestration unit, planned provider effect, and individual element within a physical batch. Batch transport success never substitutes for exact member settlement.
- **Durable execution grant** — Accepted server authority for bounded work. It has its own revision and lifetime; logout, session expiry, or client disconnect does not cancel it.
- **Idempotency key and fingerprint** — Scoped client intent identity and canonical admitted request meaning. Same key and fingerprint replays the original identity; changed meaning conflicts. They do not prove a provider write's outcome.
- **Entity identity / effect identity / dispatch / attempt** — The target resource, one logical occurrence, the persisted physical-send intent, and one bounded execution try. Keeping them distinct prevents accidental duplicate effects.
- **Unknown effect** — A possibly committed external write without sufficient occurrence evidence. It remains unknown and charged until the declared sound witness identifies the exact intent; desired state alone is insufficient.
- **Workset / plan / approval** — Immutable desired members and actions, their reviewed preview, and bounded authority sealed to exact inputs, schema/account/config revisions, cardinality, expiry, and budgets. Relevant changes invalidate approval before new effects.
- **Schedule occurrence** — A unique nominal UTC occurrence admitted by the effect-free Temporal schedule workflow. Temporal owns timing; PostgreSQL owns admitted run and effect truth.

## Data, evidence, and recovery

- **Artifact / shared object** — Immutable bytes in the approved object-store contract, identified by digest, length, and backend generation. Worker-local files are scratch.
- **Manifest / checkpoint** — PostgreSQL-owned reference to an exact immutable object set and the exact acknowledged contiguous source position. Publication and checkpoint advancement commit together.
- **Receipt / proof** — Sanitized evidence for one exact case, component/source closure, scope, and execution time with an independent oracle. It is not admission authority and cannot certify broader cells.
- **Implemented / certified** — Implemented means all required exact-scope cases pass through the real PM composition. Certified additionally requires the accepted real-provider evidence for that exact scope. Source presence, inventory, build success, credentials, or AI review alone proves neither.
- **Recovery epoch / quarantine** — New authority and effect-egress boundary established after restore. Restored storage does not undo provider writes; uncertain post-backup effects are reconciled before eligible work is released.

Use the [decision index](../architecture/decisions.md) for rationale, [testing guide](../development/testing.md) for evidence scope, and [release guide](../development/release.md) for packaging and recovery boundaries.
