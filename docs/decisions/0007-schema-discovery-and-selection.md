# 0007: Schema discovery and selection in PM PostgreSQL

Date: 2026-11-16. Status: accepted 2026-11-16. This record does not authorize application work. Every table, route, command and check below is a planned contract until the owning implementation creates and runs it. Issue: #285, a sub-issue of #72.

## Context

A connection reads typed records only through a selected, complete metadata snapshot of the resource that it reads. At `main` `c59aa20`, PM keeps those snapshots and the selection in files, in two separate places:

- pm-server reads the selection for the schema route from a server-owned metadata root, in one directory for each organization, workspace and connection (`internal/httpapi/schema.go:118-125`, `internal/httpapi/service_config.go:28-31`, `internal/schema/snapshot.go:367-390`).
- pm-worker reads the selection for a SQL read from the state directory that the operator declares for each SQL connection (`internal/app/sql_composition.go:238`, `:406`; `internal/config/provider_sql.go:77`).
- In each directory, `config.json` holds the one selected `schema_ref` of the whole connection, a lock file serializes a change, and each snapshot is a `<digest>.json` file of at most 4 MiB with at most 4,096 fields (`internal/schema/snapshot.go:22-32`, `:466`).

Nothing makes the two directories hold the same selection, so pm-server can show one selection while pm-worker reads with another. No product path makes a discovery or a selection: `schema.Discover` (`internal/schema/discover.go:57`) and `Store.Promote` (`internal/schema/snapshot.go:649`) have only test callers.

Two more facts at `c59aa20` limit the design:

- The SQL path binds a relation through its selected snapshot (`internal/app/sql_composition.go:337`, `bind` at `:377`, the selected snapshot at `:406-424`). It then checks a metadata observation against that bound relation (`:737-740`). A relation that has no selection cannot pass `bind`, so the first discovery of a relation cannot use this path.
- Every `pm` mutation needs `--revision`, and `pm` refuses a missing one before it sends a request (`internal/cli/connections.go:343-345`). The server accepts only a positive revision (`internal/httpapi/service.go:467-476`, `internal/app/workspaces.go:564-571`). The schema route returns no revision (`api/pm.openapi.yaml:3879-3901`).

The texts in force already place this state in PM PostgreSQL:

- `AGENTS.md`: "PostgreSQL owns application intent, authorization, idempotency, effect facts, metadata selection, artifact manifests, and checkpoints."
- The architecture plan, row S15: "Discovered account schemas and active snapshot selection | PM PostgreSQL / schema", with "If-Match promotion and immutable accepted bindings".
- The architecture plan, FND-09: a complete immutable metadata snapshot, built by `Discover`, "called by registered discovery activities".

## Decision

PM stores each discovered metadata snapshot and each schema selection in PM PostgreSQL. pm-server and pm-worker read them only from there. The first discovery of a relation has its own path, which needs no selection.

### Storage

- A snapshot row is immutable. Its key is its content digest (`schema_ref`) within its binding: organization, workspace, connection, provider account, permission revision, resource and source revision.
- A selection row names one complete snapshot for one connection and resource. This changes the key: at `c59aa20`, one file store holds one selection for the whole connection. With this change, each declared relation of a connection has its own selection.
- The binding, the completeness rule, the 4 MiB and 4,096-field bounds and the drift rules of `internal/schema` stay as they are. A read whose binding is not the binding of the selected snapshot is refused, as `Store.LoadFor` refuses it today (`internal/schema/snapshot.go:613`).
- Admission records the selected `schema_ref` in the accepted run intent. pm-worker loads that exact snapshot by its digest. A later selection does not change an accepted run.
- pm-server reads the selection for the schema route, and admission reads it for a read, from the same rows, under the same tenant scope.
- The server metadata root and the selection role of `provider_sql.state_directory` end. No file under them holds a snapshot or a selection. A remaining use of a state directory is scratch only.
- Planned contract examples: two tables, one for snapshots and one for selections, with row-level security like the other scoped tables. One route starts a discovery and one route selects, both under `/v1/workspaces/{workspace_id}/connections/{connection_id}/schema`. Table and route names are final in the implementing slices.

### The selection revision

- PM keeps one selection revision for each connection and resource. The revision is 1 while no selection exists. Each successful selection adds 1, in the same transaction as the compare and set and the audit record. The revision is never 0. This follows the existing collection revisions (`pm.identity_collection_revisions`, `DEFAULT 1` and `CHECK (... > 0)`, `internal/store/migrations/postgres/0003_identity.sql:288-297`).
- How a caller reads it:
  - The schema route returns it. `ConnectionSchemaInspection` (`api/pm.openapi.yaml:3879-3901`) gets a new required member, `selection_revision`, an integer of at least 1. The route also sets the `ETag` header to the same value (`setETag`, `internal/httpapi/service.go:478`).
  - With no selection, the route returns `selected: null` and `selection_revision: 1`. It returns `selected: null` for no selection today (`internal/httpapi/schema.go:67-78`).
  - `pm schema show` prints `selection_revision` in its text output and its JSON output.
- The first selection: the caller reads revision 1 from `pm schema show`, then runs `pm schema select ... --revision 1`. There is no zero value and no special value.
- The select route uses the existing mutation control with no change (`parseMutationControl`, `internal/httpapi/service.go:448-465`): the `Idempotency-Key` header, the protocol `expected_revision` and `If-Match`, each with the same revision. The server checks, in this order:
  1. the permission that manages the connection (`connection.manage`, as `internal/app/connections.go:239`);
  2. the mutation control (`validateMutationControl`, `internal/app/workspaces.go:564-573`);
  3. the idempotency receipt: a replay of the same key and intent returns the stored result;
  4. the precondition (`requireFreshMutationPrecondition`, `:575-584`);
  5. the candidate: it is a complete snapshot of this connection and resource;
  6. the compare and set of the revision. The new revision, the selection and one audit record are written together, or nothing is written.
- A missing precondition:
  - `pm schema select` without `--revision` is a local usage refusal. `pm` sends no request, writes `error: --revision is required` on stderr and exits with code 2. Every `pm` mutation does this today (`internal/cli/connections.go:343-345`, `internal/cli/root.go:29-33`).
  - A fresh request to the select route, with a valid protocol `expected_revision` and no `If-Match`, gets HTTP 428 `control_revision_required` (`internal/app/workspaces.go:575-584`). The route accepts a request without `If-Match` (`internal/httpapi/service.go:448-464`), so the replay check of step 3 comes first: a request whose key and intent match a stored receipt gets the stored result, also without `If-Match`. The connection update service uses the same order today (`internal/app/connections.go:273-282`). `pm` cannot send a request without `If-Match`. Only another client of the public API, or a developer check of the route, can observe it.

### First discovery

The first discovery binds the relation from the operator's declaration, not from a selected snapshot. The selected-schema check stays on the read path.

- The discovery activity binds the relation from the declared `relation_bindings` of the SQL connection (`internal/config/provider_sql.go:86-105`): the binding name, the schema, the table and the snapshot binding. It needs no selected snapshot.
- The declared relation scope: admission refuses a resource that no declared relation binding of the connection names, with `schema_relation_unknown`, before any run starts. The worker checks the declaration again before any provider I/O.
- It runs only the connector's declared metadata operation for that relation. For PostgreSQL, that is `relation/metadata`, statement kind `metadata` (`internal/contract/operation.go:31`). The declared schema and table are bound values, never SQL text. It runs no other statement.
- Bounded I/O: it uses the declared bounds of the connection, the same bounds as a read: the connect, statement and total durations, the row and byte limits and the attempt ceiling (`ProviderSQLBounds`, `internal/config/provider_sql.go:55-62`, applied by `limits`, `internal/app/sql_composition.go:442`). Cancellation and the deadline stop it.
- Authorization: admission checks `connection.manage` on the connection. The worker activity checks the current grants again before the provider I/O. It resolves the provider credential only inside the activity, as `AGENTS.md` requires.
- It checks that the observation describes the declared schema and table. `schema.Discover` then normalizes it and applies the completeness rule and the bounds. The activity stores the result as a candidate snapshot. A partial or failed discovery stores no complete candidate, and no discovery changes a selection.
- The selected-schema check (`VerifySQLRelation` against the bound relation, `internal/app/sql_composition.go:737-740`) stays on the read path. The first discovery does not use it.

### Commands and refusals

`pm` maps each server problem to an exit code (`internal/api/output.go:109-125`) and writes one stderr line, `error: <problem code>: <detail> (incident <id>)` (`internal/api/output.go:99`). A scenario checks the exit code and the problem code.

| Command (planned, except `pm schema show`) | What it does |
| --- | --- |
| `pm schema discover --connection <id> --resource <resource>`, with `--wait` to wait for the result | Starts a discovery run of one declared relation. With `--wait`, exit code 0 when the run stored one complete candidate. It never selects. |
| `pm schema select --connection <id> --resource <resource> --schema-ref <digest> --revision <selection revision>` | Selects one complete candidate of the relation. `--revision` is the `selection_revision` that `pm schema show` reported, which is 1 before the first selection. It uses the existing mutation control (`--revision`, as `pm connections` does). |
| `pm schema show` | Exists at `c59aa20` (`internal/cli/root.go:136`, `internal/cli/schema.go`). After this change it reads the selection from PostgreSQL and prints `selection_revision`. It never discovers or selects. |

| Refusal | HTTP status | Problem code or `pm` line | `pm` exit code |
| --- | --- | --- | --- |
| The resource is not a declared relation of the connection, or it has no complete candidate with that `schema_ref` | 404 | `schema_relation_unknown` (new) | 3 |
| The expected revision is not the current selection revision (a stale `--revision`) | 412 | `control_revision_mismatch` (exists, `internal/app/workspaces.go:630-636`) | 4 |
| `pm schema select` without `--revision` | none: `pm` sends no request | `error: --revision is required` (exists, `internal/cli/connections.go:343-345`) | 2 |
| A fresh request to the select route with a valid protocol revision and no `If-Match` (`pm` cannot send it; a replay of a stored receipt gets the stored result first) | 428 | `control_revision_required` (exists, `internal/app/workspaces.go:575-584`) | not applicable |
| A read of a relation that has no selection | 409 | `schema_selection_absent` (new). Admission refuses it before any run, and the worker checks it again. At `c59aa20` `internal/app/sql_composition.go:419-424` refuses it with no problem code. | 4 |

A refusal writes no selection, no revision and no audit record, and it does not change an existing selection.

### Options

| Option | Assessment for PM |
| --- | --- |
| Snapshots and selection in PM PostgreSQL | Selected. One source for both processes. The compare and set, the tenant scope and the audit record are one transaction. It follows `AGENTS.md` and the architecture plan. |
| Snapshot documents in the object store, with a PostgreSQL manifest and the selection row in PostgreSQL | Rejected for now. The object store is for immutable shared business bytes. A snapshot is bounded metadata (at most 4 MiB), so the object store adds a publication step and a second failure point for no gain. Reconsider it if snapshots must outgrow the bound. |
| Keep the file stores, and give pm-server and pm-worker one shared directory | Rejected. It contradicts "PostgreSQL owns ... metadata selection" and the rule that worker-local files are disposable scratch. It needs storage shared between hosts, and today the two processes use two separate roots. |
| Keep the file stores as they are | Rejected. pm-server and pm-worker can disagree on the selection, and no Gherkin scenario can select a schema through a product path (owner decision of 2026-11-16: "Build product commands first"). |
| `--revision 0` for "no earlier selection" | Rejected. The existing control refuses 0 (`internal/httpapi/service.go:467-476`, `internal/app/workspaces.go:564-571`). A selection revision that starts at 1 keeps the existing control with no change. |
| The first discovery through the existing SQL `bind` path | Rejected. `bind` needs a selected snapshot (`internal/app/sql_composition.go:406-424`), so a relation with no selection can never get its first one. |

## Consequences

- Easier: one selection for both processes; a transactional compare and set; tenant scope by the existing row-level security; an audit record for each selection; PostgreSQL backup and restore cover the selection.
- Harder:
  - A migration adds two tables, their row-level security and their grants to the runtime role (`pm_app_runtime`): it reads and inserts snapshots, and it reads and changes selections, only in its tenant scope.
  - A snapshot document of up to 4 MiB lives in a PostgreSQL row.
  - The discovery becomes a durable write path of the worker, with its own relation binding apart from `bind`.
  - The selection key changes from the connection to the connection and the resource.
  - The public API gains a discovery route and a selection route. The schema route gains the required `selection_revision` member and the `ETag` header. PM has no released package, so no released client depends on the old response.
  - `pm schema show` prints one more member.
  - The server metadata root setting and the selection role of `provider_sql.state_directory` go away. The operator documentation changes.
  - The tests that build file stores change.
- Compatibility: PM has no released package. This record assumes that no deployment holds a file selection that it must keep, so it plans no import of file selections. The owner confirms that assumption when accepting this record.
- New dependencies: none.
- Owners who must change, by planned slice of #285:
  - The discovery slice (285b): the snapshot table, its migration and grants (`internal/store`); the discovery admission and route (`internal/app`, `internal/httpapi`, `api/pm.openapi.yaml`); the discovery activity and its declared-relation binding (`internal/app`); `pm schema discover` (`internal/cli`).
  - The selection slice (285c): the selection table, the selection revision, its migration and grants (`internal/store`); the select route, `selection_revision` and the `ETag` (`internal/app`, `internal/httpapi`, `api/pm.openapi.yaml`); `pm schema select` and the `pm schema show` output (`internal/cli`); admission and the SQL read from PostgreSQL in place of the file stores (`internal/app`, `internal/schema`, `internal/config`); `docs/development/setup.md`.

## Evidence

Read only, at `main` `c59aa20`. Nothing was run for this record.

- The two file roots: `internal/httpapi/schema.go:118-125`, `internal/httpapi/service_config.go:28-31`, `internal/app/sql_composition.go:238` and `:406`, `internal/config/provider_sql.go:77`.
- The file layout, one selection for each connection, and the bounds: `internal/schema/snapshot.go:22-32`, `:367-390`, `:439-485`, `:613`, `:628-644`, `:649`.
- No product caller of `Discover` or `Promote`: `git grep` finds only test callers.
- The SQL bind and the metadata check: `internal/app/sql_composition.go:337`, `:377`, `:406-424`, `:442`, `:737-740`. The declared relation bindings and bounds: `internal/config/provider_sql.go:55-62`, `:86-105`.
- The revision control: `internal/cli/connections.go:343-345`, `internal/cli/root.go:29-33`, `internal/httpapi/service.go:448-476`, `internal/app/workspaces.go:564-584`, `internal/store/migrations/postgres/0003_identity.sql:288-297`. The schema response: `api/pm.openapi.yaml:3879-3901`, `internal/httpapi/schema.go:67-78`.
- The accepted placement: `AGENTS.md` ("PostgreSQL owns ... metadata selection"); the architecture plan rows FND-09 and S15.

## Supersedes

Nothing accepted. The file stores at `c59aa20` are an interim implementation, not a recorded decision. This record keeps and applies these decisions:

- [ADR475-03](../architecture/pm-server/architecture/ADRS.md#adr475-03--pg-application-truth) (application truth in PostgreSQL): kept. The selection becomes PostgreSQL state.
- [ADR475-04](../architecture/pm-server/architecture/ADRS.md#adr475-04--tenant-authority) (tenant authority): kept. The new tables use the same scope and row-level security.
- [ADR475-06](../architecture/pm-server/architecture/ADRS.md#adr475-06--provider-io-ownership) (provider I/O): kept. The discovery runs in an admitted worker activity.
- [ADR475-13](../architecture/pm-server/architecture/ADRS.md#adr475-13--public-client-parity) (client parity): kept. The new routes and the new member are part of the one public OpenAPI 3.1 contract, and `pm` uses them.
- The architecture plan rows FND-09 and S15, and the `AGENTS.md` ownership rule: kept and applied.

## Revisit trigger

- A snapshot must be larger than the 4 MiB bound or hold more than 4,096 fields.
- A reader of the selection must work where PM PostgreSQL is not reachable.
- A connector needs a discovery that its declared relation bindings cannot name.

## Future checks

None of these exists yet.
- The #285 scenarios:
  - The first discovery of a declared relation with no selection stores one complete candidate. `pm schema show` still reports no selection at `selection_revision` 1.
  - The first selection at revision 1 succeeds. `pm schema show` reports the candidate and revision 2, and one selection audit record exists.
  - A replacement of candidate A by a complete candidate B at the current revision succeeds and gives the next revision. A later read uses B.
  - A stale `--revision` is refused with `control_revision_mismatch`. The selection, the revision and the audit records do not change.
  - `pm schema select` without `--revision` exits with code 2 and sends no request.
  - A resource that the connection does not declare is refused with `schema_relation_unknown`, for a discovery and for a selection, and nothing is written.
- Developer checks of the select route: a fresh request with a valid protocol revision and no `If-Match` gets 428 `control_revision_required`; a replay of a stored receipt without `If-Match` gets the stored result.
- The #72 worker read (W1b): pm-worker reads a PostgreSQL table through the selection that pm-server reports.
