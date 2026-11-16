# 0007: Schema discovery and selection in PM PostgreSQL

Date: 2026-11-16. Status: proposed. This record does not authorize application work. Every table, route, command and check below is a planned contract until the owning implementation creates and runs it. Issue: #285, a sub-issue of #72.

## Context

A connection reads typed records only through a selected, complete metadata snapshot of the resource that it reads. At `main` `c59aa20`, PM keeps those snapshots and the selection in files, in two separate places:

- pm-server reads the selection for the schema route from a server-owned metadata root, in one directory for each organization, workspace and connection (`internal/httpapi/schema.go:118-125`, `internal/httpapi/service_config.go:28-31`, `internal/schema/snapshot.go:367-390`).
- pm-worker reads the selection for a SQL read from the state directory that the operator declares for each SQL connection (`internal/app/sql_composition.go:238`, `:406`; `internal/config/provider_sql.go:77`).
- In each directory, `config.json` holds the one selected `schema_ref`, a lock file serializes a change, and each snapshot is a `<digest>.json` file of at most 4 MiB with at most 4,096 fields (`internal/schema/snapshot.go:22-32`).

Nothing makes the two directories hold the same selection, so pm-server can show one selection while pm-worker reads with another. No product path makes a discovery or a selection: `schema.Discover` (`internal/schema/discover.go:57`) and `Store.Promote` (`internal/schema/snapshot.go:649`) have only test callers.

The texts in force already place this state in PM PostgreSQL:

- `AGENTS.md`: "PostgreSQL owns application intent, authorization, idempotency, effect facts, metadata selection, artifact manifests, and checkpoints."
- The architecture plan, row S15: "Discovered account schemas and active snapshot selection | PM PostgreSQL / schema", with "If-Match promotion and immutable accepted bindings".
- The architecture plan, FND-09: a complete immutable metadata snapshot, built by `Discover`, "called by registered discovery activities".

## Decision

PM stores each discovered metadata snapshot and each schema selection in PM PostgreSQL, and both pm-server and pm-worker read them only from there.

- A discovery is an admitted, durable run. pm-worker runs the connector's metadata operation (for PostgreSQL, `relation/metadata`) in a worker activity, normalizes the result with `schema.Discover`, and stores the result as an immutable snapshot row, keyed by its content digest within its binding. Provider I/O stays in the worker activity, as `AGENTS.md` requires.
- A selection is a separate, authorized act through the public API. It names the snapshot to select and the selection that the caller read, and it succeeds only when that expected selection is still current (compare and set, as `Store.Promote` does today). A partial or failed discovery never replaces a complete selection. Each selection writes an audit record.
- pm-server reads the selection for the schema route, and pm-worker reads it for a read, from the same rows, under the same tenant scope.
- The binding, the completeness rule, the 4 MiB and 4,096-field bounds, and the drift rules of `internal/schema` stay as they are. Only the storage moves.
- The server metadata root and the selection role of `provider_sql.state_directory` end. No file under them holds a snapshot or a selection. A remaining use of a state directory is scratch only.
- Planned contract examples: two tables, one for snapshots and one for selections, with row-level security like the other scoped tables; one route that starts a discovery and one route that selects, both under `/v1/workspaces/{workspace_id}/connections/{connection_id}/schema`; and `pm schema discover` and `pm schema select` next to the existing `pm schema show`. Names are final in the implementing slices.

| Option | Assessment for PM |
| --- | --- |
| Snapshots and selection in PM PostgreSQL | Selected. One source for both processes. The compare and set, the tenant scope and the audit record are one transaction. It follows `AGENTS.md` and the architecture plan. |
| Snapshot documents in the object store, with a PostgreSQL manifest and the selection row in PostgreSQL | Rejected for now. The object store is for immutable shared business bytes. A snapshot is bounded metadata (at most 4 MiB), so the object store adds a publication step and a second failure point for no gain. Reconsider it if snapshots must outgrow the bound. |
| Keep the file stores, and give pm-server and pm-worker one shared directory | Rejected. It contradicts "PostgreSQL owns ... metadata selection" and the rule that worker-local files are disposable scratch. It needs storage shared between hosts, and today the two processes use two separate roots. |
| Keep the file stores as they are | Rejected. pm-server and pm-worker can disagree on the selection, and no Gherkin scenario can select a schema through a product path (owner decision of 2026-11-16: "Build product commands first"). |

## Consequences

- Easier: one selection for both processes; a transactional compare and set; tenant scope by the existing row-level security; an audit record for each selection; PostgreSQL backup and restore cover the selection.
- Harder:
  - A migration adds two tables, their row-level security and their grants: the worker role writes snapshots, and the runtime role reads snapshots and writes selections.
  - A snapshot document of up to 4 MiB lives in a PostgreSQL row.
  - The discovery becomes a durable write path of the worker.
  - The public API gains a discovery route and a selection route (an additive change to `api/pm.openapi.yaml`).
  - The server metadata root setting and the selection role of `provider_sql.state_directory` go away. The operator documentation changes.
  - The tests that build file stores change.
- Compatibility: PM has no released package. This record assumes that no deployment holds a file selection that it must keep, so it plans no import of file selections. The owner confirms that assumption when accepting this record.
- New dependencies: none.
- Owners who must change: `internal/schema` (the store behind the existing contract), `internal/store` and its migrations, `internal/app` (the discovery activity, the selection service and the SQL read), `internal/httpapi` (the routes), `api/pm.openapi.yaml`, `internal/cli` (`pm schema discover`, `pm schema select`), `internal/config` and `docs/development/setup.md`.

## Evidence

Read only, at `main` `c59aa20`. Nothing was run for this record.

- The two file roots: `internal/httpapi/schema.go:118-125`, `internal/httpapi/service_config.go:28-31`, `internal/app/sql_composition.go:238` and `:406`, `internal/config/provider_sql.go:77`.
- The file layout and bounds: `internal/schema/snapshot.go:22-32`, `:367-390`, `:439-485`, `:649`.
- No product caller of `Discover` or `Promote`: `git grep` finds only test callers.
- The accepted placement: `AGENTS.md` ("PostgreSQL owns ... metadata selection"); the architecture plan rows FND-09 and S15.

## Supersedes

Nothing accepted. The file stores at `c59aa20` are an interim implementation, not a recorded decision. This record keeps and applies these decisions:

- [ADR475-03](../architecture/pm-server/architecture/ADRS.md#adr475-03--pg-application-truth) (application truth in PostgreSQL): kept. The selection becomes PostgreSQL state.
- [ADR475-04](../architecture/pm-server/architecture/ADRS.md#adr475-04--tenant-authority) (tenant authority): kept. The new tables use the same scope and row-level security.
- [ADR475-06](../architecture/pm-server/architecture/ADRS.md#adr475-06--provider-io-ownership) (provider I/O): kept. The discovery runs in an admitted worker activity.
- [ADR475-13](../architecture/pm-server/architecture/ADRS.md#adr475-13--public-client-parity) (client parity): kept. The new routes are part of the one public OpenAPI 3.1 contract, and `pm` uses them.
- The architecture plan rows FND-09 and S15, and the `AGENTS.md` ownership rule: kept and applied.

## Revisit trigger

- A snapshot must be larger than the 4 MiB bound or hold more than 4,096 fields.
- A reader of the selection must work where PM PostgreSQL is not reachable.

## Future checks

None of these exists yet.
- The #285 scenarios: an authorized user discovers and selects the schema of a table, and `pm schema show` reports it; a selection of a relation that does not exist is refused and keeps the earlier selection.
- The #72 worker read (W1b): pm-worker reads a PostgreSQL table through the selection that pm-server reports.
