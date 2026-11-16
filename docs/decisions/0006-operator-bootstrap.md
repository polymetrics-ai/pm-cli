# 0006: Operator bootstrap of the first workspace and admin

Date: 2026-11-16. Status: proposed. This record does not authorize application work. Every command, flag, role, text and check below is a planned contract until the owning implementation creates and runs it. Issue: #284, a sub-issue of #72.

## Context

A user logs in to PM through an OpenID Connect provider. PM maps the provider's issuer and subject to a PM principal through a login binding, one row for each deployment, issuer and subject (`internal/store/auth.go:348-375`, called at `internal/auth/oidc.go:225-227`; the table is `pm.auth_bootstrap_identities`, `internal/store/migrations/postgres/0003_identity.sql:109-122`, with row-level security on the deployment, `:368-380`).

At `main` `c59aa20`, no product path creates the first of these:
- a workspace (`store.RegisterWorkspace`, `internal/store/identities.go:27`);
- a user identity (`store.RegisterIdentity`, `internal/store/identities.go:159`);
- its login binding (no writer outside tests);
- its first admin membership (`store.RegisterMembership`, `internal/store/memberships.go:41`);
- a provider account (`store.RegisterProviderAccount`, `internal/store/provider_accounts.go:32`).

Each has only test callers. `pm memberships grant` and `pm connections create` exist, but they need an existing workspace and an authorized user. So a new deployment has no first admin, and no product path can make one.

The owner decided on 2026-11-16 that acceptance scenarios must not write these rows directly: "Build product commands first". The operator owns the stack lifecycle, and a client invocation never starts or owns a backend ([ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery), `AGENTS.md`).

The stack database has two roles today: `pm_app_migrator`, which owns the database, and `pm_app_runtime`, which the server uses (`deploy/postgres/bootstrap.sql:13-14`, `:45-64`).

## Decision

PM adds an operator subcommand, `pm-server bootstrap`. It creates, in one PostgreSQL transaction:
- the first workspace of an organization;
- the first user identity;
- that identity's login binding (issuer and subject) with the right to approve devices;
- the identity's first admin membership;
- the provider accounts that the input lists;
- one audit record that names the operator bootstrap as the actor.

Authority and scope:
- **Operator authority is a dedicated database credential.** A new role, `pm_app_bootstrap`, may insert only these rows and read only what the scope check needs. Its credential lives only in the operator's secrets directory for the bootstrap. The running pm-server never holds it.
- **Scope.** One bootstrap for each deployment and organization. The deployment comes from the server configuration. The login binding table is already scoped by deployment.
- **Once.** Any second bootstrap of the same scope is refused, also when its input is the same as the first input. The reasons:
  - The bootstrap is one transaction, so a failed bootstrap leaves no partial state, and a retry after a failure starts clean.
  - After a lost or uncertain commit, a retry tells the operator the truth: a refusal means that the first bootstrap committed, and a success means that it did not.
  - A clear "once" needs no stored copy of the first input and no rule for when two inputs are the same.
  - An operator who lost the output can read the result through the product after login.

The planned interface:
- `pm-server bootstrap --config <file> --secrets-directory <dir> --input <file>`.
- The input file holds the organization, the workspace ID and name, the admin's issuer and subject, and the provider accounts (connector ID and display name). It holds no secret, and no secret is in an argument.
- Success: exit code 0. One JSON document on stdout names the workspace, the admin identity, the admin membership and each provider account. It holds no secret.
- Each refusal: exit code 1, nothing on stdout, nothing written, and exactly one stderr line:
  - `pm-server: bootstrap scope is already bootstrapped`;
  - `pm-server: bootstrap workspace name is required`;
  - `pm-server: bootstrap admin subject is required`;
  - `pm-server: bootstrap provider account names an undeclared connector`;
  - `pm-server: bootstrap requires operator authority` (the secrets directory holds no bootstrap credential, or the credential is refused).

The admin membership holds the right to manage memberships and connections. Its resource grants list no connection at the start. An admin who later creates a connection adds it to a membership through `pm memberships update` (#284, slice 284c), under the existing grant model. That update is outside this record.

| Option | Assessment for PM |
| --- | --- |
| `pm-server bootstrap` with a dedicated bootstrap role | Selected. No network route. The authority is a credential that only the operator holds and the running server never holds. The role can create only the bootstrap rows. |
| `pm-server bootstrap` with the `pm_app_migrator` credential | Rejected. That role owns the database. The most privileged credential would serve a routine first-run act, against least privilege. |
| A bootstrap section in the server configuration, applied at each start | Rejected. Authority grants would live in configuration, every start would apply them again, and no operator act would mark the bootstrap. A removed section has no defined meaning. |
| An HTTP bootstrap route with a one-time token | Rejected. It adds a network attack surface for the most privileged act in PM, and the token needs its own issue, storage and expiry. |
| One bootstrap for each deployment (all organizations together) | Not chosen, and kept as the stricter alternative. It limits a leaked bootstrap credential to one bootstrap in total. Plan v2 (O3) and the criteria chose one bootstrap for each deployment and organization. Reconsider it if the security review asks for it. |

## Consequences

- Easier: a new deployment gets its first admin through a product path. Acceptance scenarios use that path, and no test writes PM rows.
- Harder:
  - A migration adds the role `pm_app_bootstrap` and its grants. `deploy/postgres/bootstrap.sql` and the stack fixture create its login from a new password file.
  - The operator keeps one more credential.
  - `docs/development/setup.md` states the operator duties: run the bootstrap once, then remove the bootstrap credential from the secrets directory.
- Risk: a leaked bootstrap credential can bootstrap a new organization in the same deployment, because the scope is the deployment and the organization. The operator duty above limits this, and the stricter alternative above removes it.
- New dependencies: none.
- Owners who must change: `cmd/pm-server` (the subcommand), `internal/app` (the bootstrap service), `internal/store` and its migrations (the role, the grants, the scope check), `deploy/postgres/bootstrap.sql`, the stack fixture (`tools/cmd/pm-stack-fixture`), `docs/development/setup.md`.

## Evidence

Read only, at `main` `c59aa20`. Nothing was run for this record.

- The login binding: `internal/store/auth.go:348-375`, `internal/auth/oidc.go:225-227`, `internal/store/migrations/postgres/0003_identity.sql:109-122`, `:368-380`.
- No product writer: the `Register*` functions in `internal/store` and the login binding table have only test callers (`git grep`).
- The roles: `deploy/postgres/bootstrap.sql:13-14`, `:45-64`.

## Supersedes

Nothing accepted. This record keeps and applies these decisions:

- [ADR475-03](../architecture/pm-server/architecture/ADRS.md#adr475-03--pg-application-truth) (application truth in PostgreSQL): kept. The bootstrap writes PostgreSQL rows in one transaction.
- [ADR475-04](../architecture/pm-server/architecture/ADRS.md#adr475-04--tenant-authority) (tenant authority): kept. The rows carry the organization and workspace scope, and the login binding keeps its deployment scope.
- [ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery) (operator lifecycle): kept. The operator runs the bootstrap. No client command starts it.

## Revisit trigger

- The security review asks for the stricter scope of one bootstrap for each deployment.
- PM adds a product path that creates more organizations, workspaces or admins.
- An operator needs to repeat a bootstrap, for example after a restore.

## Future checks

None of these exists yet. The #284 scenarios (`tests/acceptance/features/operator_bootstrap.feature`, criteria for #72 and #284):
- K1: the operator bootstraps a deployment, the observer reads the rows and one audit record, and the bootstrapped user logs in and lists the workspace.
- K2: a second bootstrap of the same scope is refused, also with the same input, and writes nothing.
- K3: an empty workspace name, an empty admin subject, or a provider account for an undeclared connector is refused, and writes nothing.
- K4: a bootstrap without the bootstrap credential is refused, and writes nothing.
- K5: another deployment on the same stack bootstraps its own scope.
