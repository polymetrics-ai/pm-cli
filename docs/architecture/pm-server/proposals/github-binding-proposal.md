# GitHub read binding proposal — approval required

Status: PROPOSED, NOT ACCEPTED. F02 remains a product decision. F03 now has a retrievable immutable candidate, but adoption and future compiler capture remain unapproved. These proposals do not change the canonical architecture or certify a provider.

## Recommended projection

Preserve every entry returned by the repository issues operation, including entries with `pull_request`. Retain that source discriminator and its absent/value distinction; do not silently filter or rename an issue ID into a pull-request ID. Treat `number` as repository-scoped and `title` as display data, never an identity key. Preserve source pagination over the returned collection. This recommendation preserves the source operation's membership and avoids introducing filtering semantics.

For this pilot, propose the required selected fields `id`, `number`, `repository_url`, `url`, `title` and `state`. All six occur in the pinned issue schema's required set. Preserve `pull_request` when present; it is optional at the issue level. Validate its object shape and required nullable URL properties against the pinned schema when present. Preserve unknown additional source fields according to the existing compiler contract; this six-field selection does not redefine the entire upstream required set or allow invalid source records.

The independent fixture must bind expected numeric `id`, repository-scoped `number`, repository identity and resource URL before the response is generated. The existing connection/request identity and admitted destination policy constrain repository and URL checks; no source URL becomes independent authority merely because it appeared in the response. Seed an ordinary issue and a mixed entry separately. Missing/wrong-type selected fields, wrong identity and wrong repository each require their own named case. Exact new projection behavior remains blocked until the approver accepts or replaces this proposal.

The minimum F02 decision is: accept this collection-preserving projection and six-field selection, or provide the replacement membership/filtering rule, required field set and pagination consequence. No implementation owner may silently decide it while writing a fixture.

## Immutable source candidate

Use the official `github/rest-api-description` revision `0a144e030abbe358000f0d32874e197b59a8f7ed`, path `descriptions/api.github.com/api.github.com.json`, operation `issues/list-for-repo`, pointer `#/paths/~1repos~1{owner}~1{repo}~1issues/get`. [The pinned upstream file](https://github.com/github/rest-api-description/blob/0a144e030abbe358000f0d32874e197b59a8f7ed/descriptions/api.github.com/api.github.com.json) is retrievable. Its Git blob is `ed76b1371e381b078e50c8a698b890911031aeb2` and its upstream size is 12,939,686 bytes.

The adjacent [source facts](github-operation-closure.json) contain the complete selected GET operation, selected identity/discriminator field schemas, referenced parameter definitions and a complete recursive reference-node inventory. Inventory nodes summarize type/required fields; this bounded review capture is **not a complete compiler input**. Every pointer resolves within the pinned upstream JSON. Its license is MIT; the exact [LICENSE.md](github-source-LICENSE.md) is included. No full upstream checkout or schema archive was imported.

Read-only `gh-axi api` requests verified the commit, file/blob metadata, operation/ref inventory and license. No PM service, provider account, connector compilation or proof ran. One oversized wrapper result was rejected; the successful capture reassembled bounded base64 slices, checked their total length and parsed the result. The source facts record their normalized digest. Future approved source.lock creation must capture and hash the complete immutable input and preserve notices; no full-input SHA-256 is invented here.

The minimum F03 decision is acceptance of this exact upstream candidate and MIT notice for the declared operation. The later connector owner still owns source.lock creation, full-byte retrieval/hash verification, reference resolution, compiler fidelity and causal tests. WEB-GITHUB remains a historical moving-page observation, not the authority for this candidate.
