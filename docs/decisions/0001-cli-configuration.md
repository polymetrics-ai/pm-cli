# 0001: Cobra commands with strict typed configuration

Date: 2026-09-10. Status: selected for setup by the coordinating owner; application implementation is still paused. No Go module or application code was created by this setup. This document retains the Cobra/strict-JSON choice while correcting the earlier local-state assumptions for the remote-first plan. It does not approve the whole application plan, revise historical evidence, or count as a completed implementation check.

## Decision

Use **Cobra for the complete command tree** and a small **typed JSON resolver for PM control settings**. **Do not add Viper initially.** Keep CLI parsing/resolution inside its adapter with pure shared value types and server-owned authoritative state. Use the accepted ownership map; the earlier eleven-package count is obsolete.

| Option | Assessment for PM |
| --- | --- |
| Standard `flag` plus typed config | Avoids Cobra, but PM would own additional command-tree, help, completion, and generated-surface behavior. That duplicates work the plan already assigns to Cobra. |
| Cobra plus typed config | Recommended. One command tree plus an explicit small settings contract; no broad configuration registry across unrelated artifacts. |
| Cobra plus Viper | Useful for richer configuration-source requirements, but current PM settings do not justify an extra source-merging layer and its adaptation rules. Defer it. |

Cobra supplies nested commands, local/persistent flags, argument validation, help, completion, and documentation support. It does not replace runtime preflight. Keep Cobra types in the CLI adapter and route errors through `RunE` with injected streams. Build a fresh tree per invocation; do not use package-level mutable command instances or root authentication hooks that make offline help require credentials. [Official Cobra guide](https://github.com/spf13/cobra/blob/main/site/content/user_guide.md)

## Why Viper is deferred

Viper supports multiple formats and configuration sources. Its precedence puts explicit `Set` above flags, then environment, file, external stores, and defaults. Keys are case-insensitive; complex overrides replace values rather than deep-merging them. Empty environment values fall through by default, and environment reads are not cached. Its maintainers discourage global singletons and require synchronization for concurrent reads/writes. Those behaviors would need deliberate adaptation for PM. [Official Viper README](https://github.com/spf13/viper/blob/master/README.md)

Source inspection adds two cautions: default decoding enables weak typing, so unknown-field rejection alone does not guarantee strict types. Also, unmarshalling enumerates known keys; `AutomaticEnv` should not be assumed to enumerate every intended struct field. Bind each supported key explicitly if Viper is later adopted. The inspected source has an experimental struct-binding branch; PM must not depend on that implicitly. The source's watcher handles parent-directory events and atomic renames, so an older skill's categorical “atomic saves do not reload” advice is not a reliable current API fact. [Viper source](https://raw.githubusercontent.com/spf13/viper/master/viper.go), [maintainer issue on environment unmarshalling](https://github.com/spf13/viper/issues/761)

This is a PM design judgment, not a claim that Viper is unsuitable for CLIs generally. Reconsider it when an accepted user requirement needs additional file formats or configuration sources and a bounded implementation comparison demonstrates less maintained adapter code while preserving every test below. Merely having flags plus environment variables is not enough. Pin and inspect the selected version at that time; this setup recommends no unverified Cobra/Viper release number.

## Separate the contracts

PM controls select execution context and presentation. Provider parameters describe a declared operation. Saved connection configuration describes an endpoint/account/transport and refers to credentials. Account metadata describes dynamic fields. Plans, approval, source declarations, and proof have separate owners and revision rules.

Only PM controls participate in settings precedence. Do not load provider fields, API URLs, auth material, `schema_ref`, approval flags, worksets, or certification status through a generic settings map. Do not search `.env`, merge arbitrary working-directory files, or accept a generic HTTP/body override. Provider credentials enter through separate authenticated administrative ingress and resolve only in admitted worker activities. CLI profiles may hold a PM session handle in the OS store, never provider secrets.

The earlier local connection files and `config.json.schema_ref` selection are superseded.
PostgreSQL owns connection configuration and immutable schema selection. A local client profile contains only server URL/instance identity, PM session handle and display preferences. Workspace and connection scope are explicit for each invocation; a profile cannot establish a global active workspace.

## Controls and unresolved bindings

Cobra and strict typed JSON remain selected. The earlier local `--state-dir`/connection-directory algorithm and synthetic settings schema are historical proposals, not the remote server contract. Exact new settings keys, environment names, wire bindings and defaults must come from their owning accepted specification. This setup does not invent replacements. A client request/wait timeout never becomes a durable run deadline or cancellation request.

`--json` is an existing planned PM convenience flag: bare `--json` or `--json=true` means explicit `output=json`; `--json=false` means explicit `output=text`. An absent `--json` contributes no value. When `--output` is also explicit, require agreement: refuse `--json --output text` and `--json=false --output json`; accept the two agreeing pairs. Explicit false therefore overrides an environment/file JSON preference rather than disappearing as a zero value. Reject repeated scalar controls and invalid boolean spellings. Approval/plan flags remain explicit workflow inputs and have no environment or settings fallback.

For any approved settings schema, reject null where forbidden, unknown or duplicate keys, wrong case/types, trailing JSON values and unsupported versions. Do not coerce numbers or booleans to strings. Decode duration fields explicitly and preserve presence independently of zero values. These strictness requirements apply without activating the superseded local state schema.

## Resolution algorithm and interfaces

1. Snapshot explicitly allowed environment bindings through injected lookup. Run a bounded selector pass for approved client context only; preserve the original arguments and respect `--`. This is not a second provider parser.
2. Construct help/version and bare namespaces offline without secrets or execution configuration. Safe cached catalog/schema projections may enrich help with truthful missing/stale qualifications.
3. Load only the approved non-secret client profile/settings document using confined bounded reads. An absent implicit file may use specified defaults; corrupt existing settings must fail. Do not create a local connection/schema authority.
4. Build a fresh Cobra tree from generated safe projections and selected context. Final-parse the original arguments with real changed-flag presence and typed provider bindings.
5. Resolve approved controls by explicit changed flag, named present environment, present settings field, then specified default. Enforce alias consistency and exact agreement with bootstrap context before submission; freeze typed values.
6. Submit through the generated public API client with explicit workspace/connection scope. Server shared preflight checks permission, current schema and capability before admission. All provider I/O runs in admitted activities.
7. Bind relevant connection/config/schema evidence to the accepted run and sealed approval on the server. Presentation changes do not alter provider inputs. Request cancellation stops viewing/submission as defined, not an already accepted durable grant.

No proposed local `ResolveConnection` or `store.ReadJSON` symbol creates application ownership here. Future files and signatures follow the owning packet. Provenance fields record safe origin kinds, never environment dumps or secret values.

If Viper is adopted later, keep one invocation-local `viper.New()` strictly inside this resolver, bind the named fields explicitly, validate the raw JSON before Viper sees it, reject case/key collisions, disable weak coercion, define empty handling, and produce the same immutable value object. No global Viper, repeated runtime `Get`, hot reload, or `WriteConfig` shortcut is allowed.

## Planned causal checks

These tests do not exist or pass yet. Names below are behavioral proposals, not a new file allocation. Reconcile exact file ownership with the accepted task before implementing shared foundation tests. Existing plan tests should be extended rather than duplicated with provider wrappers.

| Proposed test and file | RED trigger → GREEN observation |
| --- | --- |
| `TestControlPrecedenceAndPresence`, `internal/cli/config_test.go` | An unchanged flag default overrides file/env or false disappears → explicit presence wins; `--json=false` selects text and inconsistent explicit aliases fail |
| `TestControlEnvironmentAllowlistAndEmpty`, same file | Empty/unknown env changes execution or falls through silently → allowed empty errors; unrelated variables have no effect |
| `TestStrictSettingsJSON`, same file | Duplicate/wrong-case/unknown keys or coercible types are admitted → bounded strict decoder rejects before I/O |
| `TestSettingsReadPolicy`, same file | Corrupt existing settings silently becomes defaults → only absent implicit settings falls back |
| `TestControlProviderIsolation`, same file | A provider field or approval value is read from settings → refused, while reserved alias retains exact wire binding |
| `TestResolvedControlsAreImmutable`, same file | Source map/env mutation changes a running call → resolved values remain stable and invocation instances are isolated |
| `TestSelectorsAndFinalParseAgree`, `internal/cli/dynamic_test.go` | Repeated/after-`--`/value-shaped selectors select one account but execute another → deterministic refusal or one consistent selected account |
| `TestOfflineHelpIgnoresExecutionConfig`, `internal/cli/tree_test.go` | Broken connection config or missing secret prevents help → namespace/help/version succeed offline; real execution still fails correctly |
| Shared server schema-scope check, file assigned by the schema owner | A client setting injects another account's snapshot → scoped PG selection and server preflight reject it |
| Shared approval revision check, file assigned by the effect owner | Saved write executes after relevant connection/config/schema change → server approval rejected before new effects |
| Client profile read-bounds check, file assigned by the CLI owner | Oversized/traversing/symlinked read crosses the owned boundary → bounded refusal without leaking bytes |
| Existing `TestHelpNamespaceAndManualShareTree`, `internal/cli/tree_test.go` | Control alias appears in execution but not docs/completion → every surface projects the same tree |

Choose targeted package checks as these files appear. Preserve ordinary build/test caches; use fresh execution only for its stated purpose. No library-wrapper suite, provider-specific Go test file, or mandatory live provider request is needed to settle this configuration decision.
