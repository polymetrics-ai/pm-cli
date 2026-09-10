# 0001: Cobra commands with strict typed configuration

Date: 2026-09-10. Status: selected for setup by the coordinating owner; application implementation is still paused. No Go module or application code was created by this setup. This document refines the clean implementation plan's CLI/configuration contract. It does not approve the whole application plan, revise historical evidence, or count as a completed implementation check.

## Decision

Use **Cobra for the complete command tree** and a small **typed JSON resolver for PM control settings**. **Do not add Viper initially.** Keep configuration code inside the existing `cli`, `contract`, and `store` ownership boundaries; do not create a twelfth configuration package.

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

Only PM controls participate in settings precedence. Do not load provider fields, API URLs, auth material, `schema_ref`, approval flags, worksets, or certification status through a generic settings map. Do not search `.env`, merge arbitrary working-directory files, or accept a generic HTTP/body override. Credential environment/stdin ingress remains a distinct resolver, not part of the PM-control environment map.

The proposed state layout adds one small non-secret settings document:

```text
<pm-state>/settings.json
<pm-state>/connections/<connection-id>/config.json
<pm-state>/connections/<connection-id>/schema/snapshots/<snapshot-id>.json
```

The connection's `config.json` remains the sole owner of `schema_ref`. Adding `settings.json` does not introduce a connection catalog, second active-schema selector, or proof authority.

## Exact initial settings contract

These names and commands are **proposed**, not installed PM syntax. Register the PM flags in `internal/contract/flags.go` and use its deterministic provider aliases. Provider wire mappings stay unchanged.

| Control | Explicit flag | Named environment | Settings key | Default and empty rule |
| --- | --- | --- | --- | --- |
| State root | `--state-dir` | `PM_STATE_DIR` | Not permitted in settings | New versioned PM state directory; explicit empty is an error |
| Connection selection | `--connection` | `PM_CONNECTION` | `defaults.connection` | Unset unless selected; explicit empty is an error |
| Output format | `--output` | `PM_OUTPUT` | `defaults.output` | `text`; only `text`/`json`; explicit empty is an error |
| Per-request timeout | `--request-timeout` | `PM_REQUEST_TIMEOUT` | `defaults.request_timeout` | `30s`; positive duration at most `10m`; explicit empty is an error |

The timeout is a bounded request policy, not an overall ETL job deadline or permission to retry. Longer provider operations should use their declared asynchronous/polling behavior or a reviewed bound change. Unset connection is valid for offline help and namespaces; operations needing a connection must report a usage/configuration error before provider I/O.

`--json` is an existing planned PM convenience flag: bare `--json` or `--json=true` means explicit `output=json`; `--json=false` means explicit `output=text`. An absent `--json` contributes no value. When `--output` is also explicit, require agreement: refuse `--json --output text` and `--json=false --output json`; accept the two agreeing pairs. Explicit false therefore overrides an environment/file JSON preference rather than disappearing as a zero value. Reject repeated scalar controls and invalid boolean spellings. Approval/plan flags remain explicit workflow inputs and have no environment or settings fallback.

Synthetic complete settings example:

```json
{
  "schema_version": 1,
  "defaults": {
    "connection": "demo-hubspot",
    "output": "text",
    "request_timeout": "30s"
  }
}
```

The schema requires `schema_version` and `defaults`; each listed default field is optional. Reject null, unknown keys, duplicate keys, wrong case, wrong JSON types, multiple trailing JSON values, and unsupported schema versions. No coercion from a number or boolean to a string. The connection ID uses the shared safe identifier rules. Duration parsing is an explicit field decoder, not arbitrary coercion.

## Resolution algorithm and interfaces

1. Snapshot the explicitly allowed environment variables once through an injected lookup. Perform the bounded PM selector pass over reserved state/connection selectors only. Preserve the original argument list for final validation; reject repeated scalar selectors and respect `--`. This pass does not resolve general controls or provider fields.
2. For help/version without an execution action, construct the offline tree without loading secrets or requiring a readable connection. Selected local metadata may enrich help if valid, with a truthful missing/stale message otherwise. Execution settings are not a prerequisite for help/version.
3. For execution, resolve state root from explicit selector, snapshotted named environment, then the platform's new versioned default. Read only that root's `settings.json`; the file cannot redirect the state root. Missing implicit settings means defaults; unreadable or malformed existing settings is an error. Enforce bounded reads, exact keys, duplicate rejection, and types.
4. Bootstrap **only the connection selection** from explicit selector, snapshotted environment, settings, then default. Load and validate that connection and its local effective metadata. Retain the selected state/connection identity and revisions. No provider or credential I/O occurs.
5. Build the effective Cobra tree from the bundle and selected local schema. Let Cobra perform the final parse of the original arguments, providing actual `Flag.Changed` presence for PM controls and typed provider bindings. This selector pass is not a second provider parser and does not manufacture changed-flag state.
6. Now resolve all approved controls by **explicit changed flag > present snapshotted environment > present settings field > default**, enforcing the `--json`/`--output` consistency rules. Presence is separate from value; unrelated variables have no effect. Verify final state/connection selection exactly agrees with bootstrap before any provider I/O. Refuse disagreement instead of reparsing against another account. Freeze the resolved values.
7. Shared preflight validates the typed call for CLI and non-CLI consumers. Pass immutable value objects to runtime. Relevant connection/config/schema revisions join the saved plan/approval scope; changing only presentation does not change provider input or certification.

Proposed symbols:

```text
contract.ControlSettings / ResolvedControls / ControlOrigin / ContextSelection
contract.ConnectionConfig / ConnectionRevision
cli.BootstrapSelection(selectors, settings, envSnapshot) -> ContextSelection, error
cli.ResolveControls(changedFlags, settings, envSnapshot) -> ResolvedControls, error
cli.ResolveConnection(selection, connectionStore) -> ConnectionConfig, error
cli.ParseSelectors(argv) -> Selectors, error
cli.BuildEffectiveTree(bundle, effectiveSchema, selection) -> command tree, error
store.ReadJSON(rootRelativePath, maxBytes) -> bounded bytes, error
```

`ControlOrigin` records only the field's source kind, not secret values or the whole environment. The resolved value object must not retain mutable source maps or a live config getter. Connection snapshot selection is read once and revision-bound; write freshness remains the shared schema/approval contract. Persistence uses `store` atomic publication, never Cobra or a configuration library's convenience writer.

If Viper is adopted later, keep one invocation-local `viper.New()` strictly inside this resolver, bind the named fields explicitly, validate the raw JSON before Viper sees it, reject case/key collisions, disable weak coercion, define empty handling, and produce the same immutable value object. No global Viper, repeated runtime `Get`, hot reload, or `WriteConfig` shortcut is allowed.

## Planned causal checks

These tests do not exist or pass yet. Implement them as shared foundation tests when their owning files are created. Existing plan tests should be extended rather than duplicated with provider wrappers.

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
| `TestConnectionConfigCannotOverrideSchemaScope`, `internal/schema/resolve_test.go` | Settings inject another account's schema selection → connection-owned `schema_ref` and context remain authoritative |
| `TestApprovalBindsResolvedConnectionRevision`, `internal/sync/approval_test.go` | Saved write executes after relevant connection/config change → approval rejected before provider I/O |
| `TestConfigReadBoundsAndConfinement`, `internal/store/read_test.go` | Oversized/traversing/symlinked read crosses the owned boundary → bounded refusal without leaking bytes |
| Existing `TestHelpNamespaceAndManualShareTree`, `internal/cli/tree_test.go` | Control alias appears in execution but not docs/completion → every surface projects the same tree |

Choose targeted package checks as these files appear. Preserve ordinary build/test caches; use fresh execution only for its stated purpose. No library-wrapper suite, provider-specific Go test file, or mandatory live provider request is needed to settle this configuration decision.
