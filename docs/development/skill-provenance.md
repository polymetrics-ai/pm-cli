# Skill provenance

The [manifest](skill-provenance.json) is the portable source of truth for this setup's four original project skills and selected external references. [Routing](skills.md) explains when to use them. The schema is `pm.skill-provenance/v1`; this is a setup inventory, not a product contract or proof registry.

## Source and reference closure

Project skills are original repository-owned MIT instructions. Their hashes identify the delivered bytes. Repository-relative required references must resolve in the checkout; no private file path is required.

External skills use existing installations without copying their payloads. Each selected source records its HTTPS URL or local-only status, immutable revision when qualified, license/notice, strategy, requiredness, file hashes and PM overrides. The listed files close selected entrypoints' in-package Markdown references and linked assets, plus license/notice metadata. Cross-skill links do not recursively select a catalog. Host UI metadata, where listed, is separately hashed and is not review-policy authority.

Samber's selected closure matches its recorded commit. The pinned Matt Pocock AI Hero entrypoint and root license match the installed bytes. The writing installation is identified by local hashes; a prior upstream commit observation is disclosed without claiming new byte equality. Local go-engineering and no-mistakes licensing remains unverified: use in place, with no redistribution. There are zero third-party copy operations.

Official fallback URLs are documentation entrypoints, not version locks. Inspect APIs against the task's chosen immutable dependency versions when implementing. A successful docs request establishes access only.

## Structural consumer contract

Reject duplicate JSON keys and unknown keys. The root has exactly `schema_version`, `project_skills`, `external_sources` and `official_fallbacks`. All three arrays are nonempty; project membership is exactly the four skill files in this setup.

Project entries have `name`, `path`, `sha256`, `origin`, `license`, `references` and `overrides`. Check unique names/paths, frontmatter names, actual membership, byte hashes and confined existing references. `origin` is `repository-original`; `license` is `MIT`.

External entries have `id`, `url`, `revision`, `license`, `notice`, `strategy`, `required_for`, `files` and `overrides`. Source IDs are unique. Strategy is `use-installed-no-copy`; requiredness is `optional-reference`, `authorized-review` or `authorized-delivery`. Each nonempty file list contains unique source-relative `path`, lowercase SHA-256 and `role` (`entrypoint`, `reference`, `license`, `notice` or `metadata`). A null revision means local hashes only, never a verified upstream commit. A null URL means no qualified public origin. Unverified licensing is explicit.

Fallback entries have unique `id` and HTTPS `url`. Strings are nonempty except explicitly nullable URL/revision fields. Hashes are 64 lowercase hex characters. Paths are canonical relative files without absolute roots, backslashes, empty/dot/parent components or symlinks. Reject duplicate array values. The manifest does not hash itself.

CI checks local project files and external record shape/completeness; it does not require external installations or fetch sources. Missing/extra project entries, stale hashes, escaping references or an empty inventory must fail. No model login, provider account or global installation is a CI prerequisite.

## Qualification limits

File/hash/link checks establish structure. Actual discovery requires a fresh authorized worker in the candidate checkout. Independent Standards and Spec review requires an accepted accessible specification and a nonempty diff against the recorded target SHA/merge base. Verify the requested runtime from actual native turn metadata, separately from login status and launch configuration.

The exact AI Hero source is `mattpocock/skills`, commit `3cca18b368ae95cdbdebbff572ccafa662551015`, path `skills/engineering/code-review/SKILL.md`. Keep its axes independent and distinguish documented standards from heuristic smells. PM's policy blocks readiness for a missing Spec; a skipped axis is not a pass. The repository-owned [issue/spec lookup](../agents/issue-tracker.md) supplies its issue-tracker integration. Actual complete accepted-spec readback and fresh-worker discovery remain bootstrap qualification.

The authorized no-mistakes pipeline owns fixes/push/CI. Record final head, both review reports and real check identities; never infer success from tool installation, empty checks or an earlier head. Its integration is unqualified until actually exercised under the accepted delivery task. No automatic merge authority is added.

## Original issue-tracker integration

The PM-owned tracker guide was authored after inspecting selected guidance from `mattpocock/skills` at commit `3cca18b368ae95cdbdebbff572ccafa662551015`. No template or skill payload was copied or installed.

| Inspected source | SHA-256 |
| --- | --- |
| [Setup entrypoint](https://github.com/mattpocock/skills/blob/3cca18b368ae95cdbdebbff572ccafa662551015/skills/engineering/setup-matt-pocock-skills/SKILL.md) | `2bcd89e97777cdb705914424e39c97d5db524c8eb4eafac8120778a07774f0ec` |
| [GitHub tracker guidance](https://github.com/mattpocock/skills/blob/3cca18b368ae95cdbdebbff572ccafa662551015/skills/engineering/setup-matt-pocock-skills/issue-tracker-github.md) | `afd6852a80185217bd28aa5cbe456bef1e85be25be7bd1fba382d5b8ee428325` |
| [Setup documentation](https://github.com/mattpocock/skills/blob/3cca18b368ae95cdbdebbff572ccafa662551015/docs/engineering/setup-matt-pocock-skills.md) | `4a4129edb23d9ba1491e2ef9ccba964ebc3d85830b013c089f0b8b4dd58ebbe6` |

The same source-root MIT license, copyright 2026 Matt Pocock, has SHA-256 `0e7ac423bf2c6e223b7c5b156f8cf72da49d748e56a1641402c31f22ad07dbb5`. These are inspected authoring sources, not additional installed skills or runtime reference requirements. The selected setup entrypoint's GitLab/local tracker, triage-label and domain-layout branches are outside this integration; no claimed installation omits them.

PM replaces implicit repository inference and general issue-writing/wayfinding recipes with explicit repository identity, bounded read commands, accepted revision evidence and both required review axes. The authorized early skills/CI setup slice uses published setup-owning tracking issues and exact immutable bounded inputs; full PM context import is a separate readiness gate, and no F06/README or product authority is added. Existing task authority permits this narrow original integration; it does not apply the broad upstream setup workflow or transfer any product/context ownership.
