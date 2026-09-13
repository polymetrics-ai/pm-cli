# Inactive instruction proposal

This file does not activate an application instruction or install a skill. Intended target is recorded in INSTRUCTION-TARGETS.json. Relative paths in the draft are evaluated only at that proposed target.

# Focused skill routing

The repository contains one small original router, [pm-go](../../.agents/skills/pm-go/SKILL.md), and uses installed skills on demand. Do not copy a whole skill catalog into the repository. The project's `AGENTS.md` and current task define scope, testing, and architecture; community examples provide technical help within those boundaries.

## Remote-server authority

The approved issue-pinned remote-server plan owns Go/Gin/PostgreSQL/Temporal/shared-artifact choices. CLI is presentation/transport; server services and activities own execution, secrets, authorization and state. Use server identity/session, tenant isolation, SQL transaction, workflow replay and cross-worker artifact guidance as relevant. Do not reuse obsolete local-engine examples as PM architecture.

## Select by changed behavior

| Work | Load when available | Focus |
| --- | --- | --- |
| Any Go design or implementation | `go-engineering`; its relevant reference only | Ownership, small interfaces, explicit types, errors, bounded execution |
| CLI commands, flags, help, completion | `cc-skills-golang:golang-cli`, `cc-skills-golang:golang-spf13-cobra` | One executable tree, hooks, typed flags, injected I/O, fresh command instances |
| Foundation tests or fault investigation | `cc-skills-golang:golang-testing` | Causal assertions, isolation, useful fixtures; PM's focused-test policy overrides the umbrella defaults |
| Error contracts and input boundaries | `cc-skills-golang:golang-error-handling`, `cc-skills-golang:golang-safety` | Error identity, preflight refusal, presence, resource bounds |
| Secrets, auth, redirects, or transport | `cc-skills-golang:golang-security` | Secret handling and scoped transport safeguards |
| Cancellation or parallel execution | `cc-skills-golang:golang-context`; `golang-concurrency` only for concurrency changes | Ownership, cancellation, bounded workers, meaningful race checks |
| SQL, warehouse, or transactions | `cc-skills-golang:golang-database` | Transactions, cancellation, lossless values, qualified identifiers |
| Dependencies, native build, or release | `cc-skills-golang:golang-dependency-management`; CI skill only for actual CI work | Exact pins and relevant verification |
| User/developer prose | `nbj-write-clearly` | Result first, active language, stable terms, facts and modality preserved |
| A future approved Viper adoption | `cc-skills-golang:golang-spf13-viper` | Only after decision 0001's reconsideration criterion is met |

Resolve a name through the agent's current skill catalog. Open its `SKILL.md`, then only needed relative references; announce first use according to the host's communication rules. Different hosts may display the same community entry as `samber/cc-skills-golang@golang-cli` or without its namespace. Match the source and skill name, not a hard-coded home path. A skill being installed does not mean every task loads it.

The `go-engineering` fundamentals reference is useful for ordinary work; advanced, production, and agentic-ETL references are conditional. Load Temporal Go SDK primary guidance for the approved workflow/activity boundary and replay tests. Temporal is an explicit architecture dependency; GraphQL, Kubernetes and dependency-injection frameworks are not implied by a skill catalog.

## Conflicting defaults and fallback

PM's local router intentionally replaces a second always-active `golang-how-to` dispatcher. Do not recursively invoke that dispatcher after this one. Its routing knowledge was consulted during setup; its automatic AGENTS configuration mode is not part of PM setup.

The inspected community testing skill suggests automatic `gotests` installation, audit subagents, and broad suite commands. PM does not adopt those defaults. Use the current task's agent policy, standard Go tests where sufficient, causal shared regression checks, and a full suite only at the coherent integration/release gate. No mandatory provider wrappers, blanket fake-and-live pairing, coverage target, or repeated full-suite loop is introduced.

The inspected Cobra skill uses `@latest` installation examples and suggests configuration pairing with Viper. PM pins dependencies when authorized and follows [decision 0001](../decisions/0001-cli-configuration.md). Do not apply the Viper skill's hot-reload or blanket environment-binding examples. In particular, its warning that atomic editor saves necessarily escape file watching conflicts with the inspected current Viper source, which watches the parent directory to handle renames. Verify uncertain APIs against primary sources instead of treating skill prose as implementation evidence.

If an optional skill is unavailable, report that fact and use the local contract plus the relevant official reference: [Go documentation](https://go.dev/doc/), [Go modules](https://go.dev/ref/mod), [Go security](https://go.dev/doc/security/), or [Cobra user guide](https://github.com/spf13/cobra/blob/main/site/content/user_guide.md). For prose, follow the [Google developer style guide](https://developers.google.com/style): put the reader's purpose first, preserve exact code and error text, and label proposed behavior. Skill absence alone is not a reason to stop authorized work.

## Provenance and copying policy

| Reference | Setup provenance | Transfer decision |
| --- | --- | --- |
| Local `go-engineering` | Installed skill and `references/fundamentals.md` inspected on 2026-09-10; no license file found in its skill root | Use in place when available. Do not redistribute without established permission. |
| `samber/cc-skills-golang` | Local checkout HEAD `466ea6dfd4aecb5c19caf29e7595e752c66c1a5d`; root MIT license, copyright Samuel Berthe, 2026 | On-demand reference by name. No upstream skill files copied. |
| `daniel-p-green/nbj-write-clearly` | Installed six-file skill from commit `56e3a27f920bcd772f7b03fa476b5cfb9268332c`, source `.agents/skills/nbj-write-clearly` | Use installed skill. Original material MIT; its NOTICE attributes Google style-guide material under CC BY 4.0. No copy in this repo. |
| Project `pm-go` router | Authored for this repository's task and architecture | One portable local instruction entry; no third-party source pasted. |

If a future contributor needs a portable vendored reference, select the exact skill first, inspect its relative-reference closure, and preserve its license and notices. Record source commit, source path, target path, byte hashes, and modifications. Do not assume a single copied `SKILL.md` carries its referenced files. This setup has **zero third-party COPY operations**; no licenses or source payloads are silently omitted from a copied skill.
