# Synchronized inactive projection: `docs/decisions/0001-cli-configuration.md`

This file mirrors the intended configuration ADR for provenance and comparison. It is not an active instruction entrypoint, is not fresh-worker intake, and does not establish a landed BOOT baseline. Relative links in the projected body resolve from the target path named in `INSTRUCTION-TARGETS.json`.

# 0001: Cobra and strict client/server configuration boundaries

Date: 2026-09-10. Server-first reconciliation: 2026-09-15. Status: selected planning decision; application implementation and its checks remain unexecuted. This ADR preserves the accepted Cobra, typed-presence, and no-Viper choices while superseding the earlier local-engine, local connection/schema, and CLI credential assumptions. It neither establishes a landed BOOT baseline nor authorizes application work.

## Decision

Use **Cobra for the complete thin-CLI command tree**. Use small, strict, typed, presence-aware resolvers for approved non-secret client controls and for operator-owned PM Server configuration. **Do not add Viper initially.** PM Server remains the sole authoritative engine; a client profile or flag can select context but cannot create configuration, credentials, authorization, schema, approval, execution, or proof authority.

| Option | Assessment for PM |
| --- | --- |
| Standard `flag` plus typed configuration | Avoids Cobra, but would require a second implementation for nested commands, help, completion, and generated command projection. |
| Cobra plus strict typed resolvers | Selected. One fresh command tree, explicit values and presence, and separate client/server configuration owners. |
| Cobra plus Viper | Deferred. Current requirements do not justify Viper's source-merging, case, empty-value, weak-typing, and runtime-access adaptation surface. |
| Client-selectable local or distributed engine | Rejected. PM has one server-first product architecture in development and deployment; client invocation never selects or starts another backend. |

Cobra supplies nested commands, local/persistent flags, argument validation, help, completion, and documentation projection. It does not replace shared server preflight. Keep Cobra types inside the CLI adapter, use `RunE` with injected streams, and build a fresh tree per invocation. Do not use package-global mutable commands or a root authentication hook that makes offline help require a session. [Official Cobra guide](https://github.com/spf13/cobra/blob/main/site/content/user_guide.md)

## Ownership boundary

| Owner | Configuration it may hold | Configuration it must not own |
| --- | --- | --- |
| CLI client profile | Server URL and instance identity, scoped PM session handle through the qualified OS store, and explicitly approved presentation defaults | Provider credentials, provider endpoint/body overrides, global active workspace, connection/schema authority, approvals, execution policy, durable run state |
| CLI invocation | Explicit workspace/connection selectors, operation inputs, output selection, and bounded client wait/request controls | Authentication or authorization decisions, provider retry policy, durable-run deadline, raw secrets, local backend lifecycle |
| Browser | Same public API inputs and display controls as the CLI, using its qualified PM session transport | Provider secrets, filesystem profile, alternate API semantics, direct provider calls |
| PM Server operator configuration | PostgreSQL, Temporal, artifact storage, identity, wrapping-key/secret references, service identities, origins, deployment ownership, and finite resource/admission budgets | Raw secrets in public descriptors, a local/distributed engine switch, mutable client-selected policy |
| PM Server application state | Workspace/connection configuration, encrypted provider credentials and versions, authoritative schema selection, grants, plans, runs, schedules, effects, manifests, and checkpoints | Client files as authority, Temporal internal-table ownership, customer-connector identity conflated with PM infrastructure |

Cobra remains in `internal/cli`; Gin remains in the HTTP adapter. Pure public/domain values cross those adapters. Authoritative configuration parsing belongs to the server configuration owner, and authoritative validation/authorization/admission belongs to server application services. Temporal workflow code receives immutable typed references and contains no environment, filesystem, network, SQL, or secret lookup. Admitted worker activities resolve provider signers and perform provider I/O.

## Strict controls and presence

Only explicitly approved PM client controls participate in client precedence. For each future accepted control, resolve:

1. an explicitly changed CLI flag;
2. an explicitly named, present environment binding captured once;
3. a present field in the selected non-secret client profile;
4. the specified default.

Presence is independent of the value. False, zero, and empty must not disappear as defaults; an explicitly present empty value follows that field's declared rule. Reject repeated scalar controls, malformed values, wrong case, duplicate or unknown JSON keys, forbidden null, wrong JSON types, trailing JSON values, unsupported schema versions, and source conflicts. Do not weakly coerce numbers or booleans to strings. Snapshot allowed environment variables once and do not expose a live getter to execution.

Exact profile paths, keys, environment names, wire bindings, and defaults belong to their accepted implementation issue. They are not created by this ADR. There is no `--state-dir` local engine, local connection directory, client-owned `schema_ref`, arbitrary `.env` search, arbitrary working-directory configuration discovery, generic provider URL/body override, hot reload, or blanket environment binding.

The future planned `--json` convenience alias retains explicit presence: bare `--json` or `--json=true` means `output=json`; `--json=false` means `output=text`; absence contributes no value. If a future `--output` flag is also explicit, the two must agree. These spellings are planned contract examples until the owning implementation creates and tests them; they are not commands available in this setup-only checkout.

Provider parameters, PM controls, server operator configuration, connection configuration, credentials, dynamic schemas, plans/worksets, approvals, and proof records are distinct contracts. A generic settings map must never bridge them. Provider wire names and exact semantics survive client projection; deterministic PM aliases cannot rewrite a provider field.

## Thin-client resolution model

The following is the selected future behavior, not an implemented algorithm:

1. Capture only explicitly allowed environment bindings. Parse a bounded selector set for approved client context while preserving the original argument list and respecting `--`; this is not a second provider parser.
2. Build help/version and bare namespaces without secrets, provider access, or a valid execution session. A scoped revision-keyed cache of server-owned safe catalog/schema projections may enrich help, but missing or stale data must be explicit.
3. Read only the approved non-secret client profile through a bounded, confined mechanism. A missing implicit profile may use specified defaults; a corrupt existing profile fails. The profile cannot establish a global current workspace or local connection/schema authority.
4. Build a fresh Cobra tree from generated public catalog projections and final-parse the original arguments. Preserve actual `Flag.Changed` state and exact typed provider bindings.
5. Resolve controls by the presence-aware precedence above and require selector/final-parse agreement. Freeze immutable typed request values.
6. Submit through the generated public API client with explicit workspace and connection scope. PM Server revalidates identity, permission, schema revision, approval, budgets, and actual registered capability before durable admission. Provider I/O runs only in admitted worker activities.
7. A client wait or login session ending does not cancel an accepted durable grant. Explicit control actions use the server's revision and authorization contracts.

Client catalog caches are usability projections, not admission or provider-policy inputs. Stale or missing revisions must never be silently accepted. Help/output/cache contains no credential value. The server and both generated clients derive public types from the same OpenAPI 3.1 contract; handlers do not create a competing schema.

## Strict server configuration

PM Server uses one versioned, strict typed configuration contract across bundled and operator-managed dependencies. Reject unknown, duplicate, obsolete, or wrongly cased keys; invalid types; unsafe endpoints/origins; unsupported versions; invalid ownership; missing required secret references; and zero, negative, or unbounded finite-budget fields before startup. Preserve absent/null/empty distinctions according to each field's accepted rule.

PostgreSQL application storage, Temporal persistence/visibility, object-store metadata, and customer PostgreSQL connections have separate database/role/credential identities. PM migrations touch only PM application state. A managed service must satisfy the same interface and evidence contract as its bundled counterpart; deployment ownership does not select a different PM engine. Secret references resolve at the owned server/worker boundary and raw values never enter public configuration, workflow history, client caches, or diagnostics.

## Why Viper remains deferred

Viper's precedence, case-insensitive keys, default empty-environment behavior, weak decoding defaults, live environment reads, and mutable/global usage patterns would require explicit adaptation. This is not a claim that Viper is unsuitable generally. Reconsider it only when an accepted requirement adds configuration sources and a measured comparison reduces maintained code while preserving every strictness and ownership rule above. Pin and inspect the chosen version then.

If later adopted, keep one invocation-local `viper.New()` inside the relevant resolver, bind every supported key explicitly, validate raw input before Viper sees it, reject case/key collisions, disable weak coercion, define empty handling, and emit the same immutable typed value. No global Viper, repeated runtime `Get`, hot reload, `AutomaticEnv`, or convenience writer may become authority. [Official Viper README](https://github.com/spf13/viper/blob/master/README.md)

## Future causal checks

No check below exists or passes because of this ADR. The owning implementation must place it under the accepted file/packet ownership and exercise the actual client/server boundary:

- precedence and explicit false/zero/empty presence, including alias agreement;
- strict bounded JSON, allowed environment names, duplicate/wrong-case/unknown-key refusal, and corrupt-existing-profile failure;
- concurrent invocation isolation with explicit workspace/connection scope and no mutable global selection;
- offline help/version despite missing sessions or broken execution configuration;
- exact provider-control namespace separation and server rejection of stale/forged catalog or schema revisions;
- no provider credential in client arguments, profile, cache, browser storage, output, logs, workflow history, or receipts;
- strict server dependency/ownership/budget configuration and no alternate local backend;
- CLI/browser parity through the same generated API types and server preflight;
- accepted run/approval binding to current connection, schema, grant, and configuration revisions;
- confined profile reads and immutable resolved client values.

Use focused checks as their owners create the relevant files. No provider-specific Go wrapper, blanket live call, or application test invented for this documentation change is required.
