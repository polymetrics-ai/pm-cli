# Skill provenance

The [manifest](skill-provenance.json) is the portable source of truth for this setup's four original project skills and selected external references. [Routing](skills.md) explains when to use them, and the [issue/spec tracker](../agents/issue-tracker.md) supplies accepted task intake. The schema is `pm.skill-provenance/v1`; this is a setup inventory, not a product contract, runtime registry, or proof ledger.

## Source and reference closure

Project skills are original repository-owned MIT instructions. Their hashes identify the current delivered bytes. Every project entry names the active root `AGENTS.md`, `docs/development/skills.md`, and `docs/agents/issue-tracker.md` as repository-relative required references; no private file path is required.

External records reuse the retained setup candidate's inspected source provenance. Their strategy is `use-installed-no-copy`: an available host installation may be opened in place, but no payload is vendored into PMCLI. Each selected source records an official HTTPS origin or explicit local-only status, an immutable revision when qualified or exact file hashes otherwise, license/notice status, requiredness, selected file closure, PM overrides, and official HTTPS fallbacks. Cross-skill links do not recursively select a catalog.

Samber's selected closure matches its recorded commit. The pinned Matt Pocock AI Hero entrypoint and root license match the retained inspected bytes. The writing installation is identified by local hashes; its earlier upstream commit observation is disclosed without claiming current byte equality. Local `go-engineering` licensing remains unverified, so redistribution is forbidden. PMCLI's retired Firstmate/no-mistakes delivery workflow is not a selected external source or project prerequisite. There are zero third-party copy operations.

Official fallback URLs are documentation entrypoints, not dependency locks. Inspect APIs against the immutable versions selected by the owning task. A successful documentation request establishes access only. If an optional skill is absent, use its listed primary documentation and report that absence; do not install or update it implicitly.

## Structural consumer contract

Reject duplicate JSON keys and unknown keys. The root has exactly `schema_version`, `project_skills`, `external_sources`, and `official_fallbacks`; all three arrays are nonempty. Project membership is exactly:

- `.agents/skills/pm-go/SKILL.md` — the sole Go router;
- `.agents/skills/pm-connector-contracts/SKILL.md`;
- `.agents/skills/pm-durable-effects/SKILL.md`;
- `.agents/skills/pm-release-recovery/SKILL.md`.

Project entries have `name`, `path`, `sha256`, `origin`, `license`, `references`, and `overrides`. Check unique names and paths, frontmatter names, exact membership, current byte hashes, and confined existing references. `origin` is `repository-original`; `license` is `MIT`.

External entries have `id`, `url`, `revision`, `license`, `notice`, `strategy`, `required_for`, `files`, and `overrides`. Source IDs are unique. Strategy is `use-installed-no-copy`; requiredness is `optional-reference` or `authorized-review`. Each nonempty file list contains unique source-relative `path`, lowercase SHA-256, and `role` (`entrypoint`, `reference`, `license`, `notice`, or `metadata`). A null revision means local hashes only, never a verified upstream commit. A null URL means no qualified public origin. Unverified licensing is explicit.

Fallback entries have unique `id` and HTTPS `url`. Strings are nonempty except explicitly nullable URL/revision fields. Hashes are 64 lowercase hexadecimal characters. Paths are canonical relative files without absolute roots, backslashes, empty/dot/parent components, or symlinks. Reject duplicate array values. The manifest does not hash itself.

Structural checks read local project files and external record shape; they do not require external installations or fetch sources. Missing or extra project entries, stale hashes, escaping references, or an empty inventory must fail. No model login, provider account, private controller, or global installation is a setup-check prerequisite.

## Qualification limits

File, hash, and link checks establish structure. Actual discovery requires a fresh authorized worker in the candidate checkout. Independent Standards and Spec review requires complete accepted issue inputs and a nonempty diff against a pinned base, merge base, and head. Operational route metadata remains outside repository documentation.

The exact AI Hero source is `mattpocock/skills`, commit `3cca18b368ae95cdbdebbff572ccafa662551015`, path `skills/engineering/code-review/SKILL.md`. Keep Standards and Spec axes independent and distinguish documented standards from heuristic smells. A missing Spec is not a pass. Source presence cannot claim review, CI, merge, application behavior, provider proof, or release readiness.
