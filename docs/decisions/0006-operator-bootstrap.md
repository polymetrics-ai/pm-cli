# 0006: Operator bootstrap of the first workspace and admin

Date: 2026-11-16. Status: accepted 2026-11-16, with two corrections on 2026-11-18 (see "Corrections"). This record does not authorize application work. Every command, flag, role, text and check below is a planned contract until the owning implementation creates and runs it. Issue: #284, a sub-issue of #72.

## Context

A user logs in to PM through an OpenID Connect provider. PM maps the provider's issuer and subject to a PM principal through a login binding, one row for each deployment, issuer and subject (`internal/store/auth.go:348-375`, called at `internal/auth/oidc.go:225-227`; the table is `pm.auth_bootstrap_identities`, `internal/store/migrations/postgres/0003_identity.sql:109-122`, with row-level security on the deployment, `:368-380`). One login binding names exactly one organization and one principal. So in one deployment, one issuer and subject can belong to only one organization.

At `main` `c59aa20`, no product path creates the first of these:
- a workspace (`store.RegisterWorkspace`, `internal/store/identities.go:27`);
- a user identity (`store.RegisterIdentity`, `internal/store/identities.go:159`);
- its login binding (no writer outside tests);
- its first admin membership (`store.RegisterMembership`, `internal/store/memberships.go:41`);
- a provider account (`store.RegisterProviderAccount`, `internal/store/provider_accounts.go:32`).

Each has only test callers. `pm memberships grant` and `pm connections create` exist, but they need an existing workspace and an authorized user. So a new deployment has no first admin, and no product path can make one.

The owner decided on 2026-11-16 that acceptance scenarios must not write these rows directly: "Build product commands first". The operator owns the stack lifecycle, and a client invocation never starts or owns a backend ([ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery), `AGENTS.md`).

The stack database has two roles today: `pm_app_migrator`, which owns the database, and `pm_app_runtime`, which the server uses. The privileged deployment bootstrap, `deploy/postgres/bootstrap.sql`, creates both roles from password files, each with `NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION` (`:11-41`), and it grants database access (`:45-64`). So `pm_app_migrator` cannot create a role. Each migration runs as `pm_app_migrator` and first checks that `pm_app_runtime` exists and has no prohibited authority (`internal/store/migrations/postgres/0003_identity.sql:8-20`).

## Decision

PM adds an operator subcommand, `pm-server bootstrap`. It creates, in one PostgreSQL transaction:
- the first workspace of an organization;
- the first user identity;
- that identity's login binding (issuer and subject) with the right to approve devices;
- the identity's first admin membership;
- the provider accounts that the input lists;
- one bootstrap record for the deployment and the organization;
- one audit record that names the operator bootstrap as the actor.

### Operator authority

Operator authority is a dedicated database credential for a new role, `pm_app_bootstrap`. Its credential lives only in the operator's secrets directory for the bootstrap. The running pm-server never holds it.

- **The role.** The privileged deployment bootstrap (`deploy/postgres/bootstrap.sql`) creates `pm_app_bootstrap` from a new password file, with the same attributes as the other roles: `LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION`. It grants `CONNECT` on `pm_app` to the role and revokes `CREATE` and `TEMPORARY` from it, as it does for `pm_app_runtime`. No migration creates a role.
- **The migration.** A new migration runs as `pm_app_migrator`, as each migration does. It first checks, in the form of `0003_identity.sql:8-20`:
  - the role `pm_app_bootstrap` exists;
  - it has no superuser, `BYPASSRLS`, create-database, create-role or replication authority;
  - it is not a member of `pm_app_migrator` or `pm_app_runtime`, and neither of those is a member of it.

  If a check fails, the migration fails and changes nothing. Then it applies the scoped grants.
- **The grants.** `pm_app_bootstrap` gets no grant on any table. Its only write path is one function that `pm_app_migrator` owns and that runs with the owner's rights (`SECURITY DEFINER`). The function has a fixed search path (`pg_catalog, pg_temp`), and every name in it is schema-qualified. Only `pm_app_bootstrap` may execute it; `PUBLIC` may not. The role keeps `CONNECT` on `pm_app` and `USAGE` on the schema. The function:
  - checks its inputs (length, no control characters);
  - refuses an organization that already has any row: a bootstrap record, a workspace, an identity, a membership or a provider account. Only a new organization can be bootstrapped;
  - refuses an admin issuer and subject that already have a login binding in the deployment (see "An existing login binding");
  - inserts the rows above, the bootstrap record and the audit record in one call, and returns only the IDs that it created;
  - stores the SHA-256 of the canonical input in the bootstrap record (see "Once").

### Scope

- **One bootstrap for each deployment and organization.** The deployment comes from the server configuration. The organization comes from the input.
- **Once.** The bootstrap record has the primary key (deployment, organization). Any second bootstrap of the same scope is refused, also when its input is the same as the first input. Two concurrent bootstraps of one scope cannot both commit, because the primary key refuses the second insert. The reasons for "once":
  - The bootstrap is one transaction, so a failed bootstrap leaves no partial state, and a retry after a failure starts clean.
  - After a lost or uncertain commit, a retry tells the operator what happened. A success means that no bootstrap of this scope had committed. A refusal means only that the scope is taken: a second operator, or a person with a leaked credential, can take the scope with another input. So the refusal also says whether the SHA-256 of the input that the bootstrap record holds equals the SHA-256 of this input. Equal means that a bootstrap with this exact input committed. Not equal means that another input took the scope. The exact refusal lines are set with the implementation (#302) and the scenario owners.
  - A clear "once" needs no stored copy of the first input, only its SHA-256, and no rule for when two inputs are the same.
  - An operator who lost the output can read the result through the product after login.
- **A second organization in the same deployment.** A bootstrap of another organization in the same deployment succeeds. It inserts only the rows of the new organization. The rows of each earlier organization stay unchanged: the function writes only the rows of a new organization, and the role has no other write authority.
- **An existing login binding.** A login binding is the key (deployment, issuer, subject), and it names one organization. The bootstrap only inserts a new login binding. It never changes, enables again or reuses an existing one. If the input's admin issuer and subject already have a login binding in this deployment, for any organization and enabled or not, the bootstrap is refused and writes nothing. The existing binding, its organization and its principal stay unchanged. So a bootstrap of a new organization cannot move an existing user's login to that organization. The grants hold this rule too: the role has no `UPDATE` or `DELETE` on the login binding table.

### The planned interface

- `pm-server bootstrap --config <file> --secrets-directory <dir> --input <file>`.
- The input file holds the organization, the workspace ID and name, the admin's issuer and subject, and the provider accounts (connector ID and display name). It holds no secret, and no secret is in an argument.
- Success: exit code 0. One JSON document on stdout names the workspace, the admin identity, the admin membership and each provider account. It holds no secret.
- Each refusal: exit code 1, nothing on stdout, nothing written, and exactly one stderr line:
  - `pm-server: bootstrap scope is already bootstrapped`;
  - `pm-server: bootstrap admin login is already bound`;
  - `pm-server: bootstrap workspace name is required`;
  - `pm-server: bootstrap admin subject is required`;
  - `pm-server: bootstrap provider account names an undeclared connector`;
  - `pm-server: bootstrap requires operator authority` (the secrets directory holds no bootstrap credential, or the credential is refused).

The admin membership holds the right to manage memberships and connections. Its resource grants list no connection at the start. An admin who later creates a connection adds it to a membership through `pm memberships update` (#284, slice 284c), under the existing grant model. That update is outside this record.

### Options

| Option | Assessment for PM |
| --- | --- |
| `pm-server bootstrap` with a dedicated bootstrap role | Selected. No network route. The authority is a credential that only the operator holds and the running server never holds. The role can only call the bootstrap function, which writes the rows of a new organization. |
| `pm-server bootstrap` with the `pm_app_migrator` credential | Rejected. That role owns the database. The most privileged credential would serve a routine first-run act, against least privilege. |
| A migration that creates the role `pm_app_bootstrap` | Rejected. The migration runs as `pm_app_migrator`, which has `NOCREATEROLE` (`deploy/postgres/bootstrap.sql:30-39`). To give it create-role authority would widen the most privileged application credential. |
| A bootstrap section in the server configuration, applied at each start | Rejected. Authority grants would live in configuration, every start would apply them again, and no operator act would mark the bootstrap. A removed section has no defined meaning. |
| An HTTP bootstrap route with a one-time token | Rejected. It adds a network attack surface for the most privileged act in PM, and the token needs its own issue, storage and expiry. |
| A bootstrap that reuses an existing login binding for a new organization | Rejected. One binding names one organization (`0003_identity.sql:109-122`). A reuse would move the user's login out of the earlier organization. |
| One bootstrap for each deployment (all organizations together) | Not chosen, and kept as the stricter alternative. It limits a leaked bootstrap credential to one bootstrap in total. Plan v2 (O3) and the criteria chose one bootstrap for each deployment and organization. Reconsider it if the security review asks for it. |

## Consequences

- Easier: a new deployment gets its first admin through a product path. Acceptance scenarios use that path, and no test writes PM rows.
- Harder:
  - `deploy/postgres/bootstrap.sql` and the stack fixture create the role `pm_app_bootstrap` from a new password file. An existing deployment must run its privileged deployment bootstrap again before the new migration, or the migration fails at its role check.
  - A migration checks the role, adds the bootstrap record table and the bootstrap function, and grants the role only the right to execute that function.
  - The operator keeps one more credential.
  - `docs/development/setup.md` states the operator duties: run the bootstrap once for each organization, then remove the bootstrap credential from the secrets directory.
- Risk: a leaked bootstrap credential can bootstrap a new organization in the same deployment, because the scope is the deployment and the organization. To use that organization, the holder also needs a login at the deployment's identity provider. The credential cannot add authority to an existing organization or change a login binding: its only authority is the function, which refuses an organization that has any row and an admin login that is already bound. The operator duty above limits this risk, and the stricter alternative above removes it.
- New dependencies: none.
- Owners who must change: `cmd/pm-server` (the subcommand), `internal/app` (the bootstrap service), `internal/store` and its migrations (the role check, the grants, the policies, the bootstrap record and the scope checks), `deploy/postgres/bootstrap.sql` (the role), the stack fixture (`tools/cmd/pm-stack-fixture`), `docs/development/setup.md`.

## Evidence

Read only, at `main` `c59aa20`. Nothing was run for this record.

- The login binding: `internal/store/auth.go:348-375`, `internal/auth/oidc.go:225-227`, `internal/store/migrations/postgres/0003_identity.sql:109-122`, `:368-380`.
- No product writer: the `Register*` functions in `internal/store` and the login binding table have only test callers (`git grep`).
- The roles and their attributes: `deploy/postgres/bootstrap.sql:11-41`, `:45-64`.
- The migration role checks: `internal/store/migrations/postgres/0003_identity.sql:8-20`, and the same form in `0001`, `0002`, `0004` and `0005`.

## Supersedes

Nothing accepted. This record keeps and applies these decisions:

- [ADR475-03](../architecture/pm-server/architecture/ADRS.md#adr475-03--pg-application-truth) (application truth in PostgreSQL): kept. The bootstrap writes PostgreSQL rows in one transaction.
- [ADR475-04](../architecture/pm-server/architecture/ADRS.md#adr475-04--tenant-authority) (tenant authority): kept. The rows carry the organization and workspace scope, and the login binding keeps its deployment scope.
- [ADR475-15](../architecture/pm-server/architecture/ADRS.md#adr475-15--operator-lifecycle-and-recovery) (operator lifecycle): kept. The operator runs the bootstrap. No client command starts it.

## Revisit trigger

- The security review asks for the stricter scope of one bootstrap for each deployment.
- PM adds a product path that creates more organizations, workspaces or admins.
- An operator needs to repeat a bootstrap, for example after a restore.
- One user must be an admin in more than one organization of one deployment.

## Future checks

None of these exists yet. The #284 scenarios (`tests/acceptance/features/operator_bootstrap.feature`, criteria for #72 and #284):
- K1: the operator bootstraps a deployment, the observer reads the rows and one audit record, and the bootstrapped user logs in and lists the workspace.
- K2: a second bootstrap of the same scope is refused, also with the same input. It writes nothing, and the rows of the first bootstrap do not change.
- K3: an empty workspace name, an empty admin subject, or a provider account for an undeclared connector is refused, and writes nothing.
- K4: a bootstrap without the bootstrap credential is refused, and writes nothing.
- K5: another deployment on the same stack bootstraps its own scope.
- K6: in the same deployment, a bootstrap of a second organization succeeds. The observer reads that the rows and the audit record of the first organization did not change.
- K7: in the same deployment, a bootstrap of a second organization whose admin issuer and subject already have a login binding is refused with `pm-server: bootstrap admin login is already bound`. It writes nothing, and the existing binding, its organization and its principal do not change.
- A developer check of the migration: it fails when `pm_app_bootstrap` is absent or has a prohibited authority.
- Developer checks of the bootstrap authority, each run with the bootstrap credential: a direct insert into each table of the rows above is refused; the role cannot read the login bindings; the function refuses an organization that already has rows, also one without a bootstrap record; the function refuses an admin login that is already bound; no role other than `pm_app_bootstrap` may execute the function, and its search path is fixed. A retry of a taken scope reports the same input or another input, and writes nothing.

## Corrections

The owner accepted this record on 2026-11-16. The code review of 2026-11-18 found two statements that claimed more than the planned design held. These corrections make the record true. They narrow the bootstrap authority and do not widen it. Each is a named change in the owner's sign-off packet.

1. **Existing organizations (review finding F1).** The planned grants allowed direct inserts that were limited only by the organization that the client names. So a leaked credential could add an admin to an existing organization, and the risk statement was not true. The correction replaces the table grants with one function that refuses an organization that has rows ("The grants"), and states the remaining risk exactly ("Consequences"). It also removes the read of every login binding. The implementation is in #302.
2. **A retry after an uncertain commit (review finding F2).** The record said that a refusal on retry proves that the first bootstrap committed. A refusal proves only that the scope is taken. The correction says so, and adds the SHA-256 of the input as a witness of the same input ("Scope"). The implementation is in #302.
