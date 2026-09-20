# GitHub read binding — accepted decision record

Status: F02/F03 ACCEPTED; SOURCE LOCK, COMPILER OUTPUT, APPLICATION IMPLEMENTATION AND PROVIDER PROOF UNEXECUTED.

The former proposal is resolved by the accepted [F02/F03 decision](https://github.com/polymetrics-ai/pm-cli/issues/27#issuecomment-5669069268). [CONTRACTS](../architecture/CONTRACTS.json), [FOUNDATION-MAPPINGS](../architecture/FOUNDATION-MAPPINGS.json) and the issue #27 record in [PACKET-CONTRACTS](../implementation/PACKET-CONTRACTS.json) are normative.

## Accepted result contract

Return complete action data by default. The former `id`, `number`, `repository_url`, `url`, `title`, `state` selection is not a default projection and does not reduce the provider schema. Independent expected `id`, repository-scoped `number` and repository identity remain required oracle assertions, but they are not the whole result.

Optional caller-selected fields and record filtering use one shared typed request/result contract across server, generated Go client, CLI and browser. Selecting a nested field preserves the nested value without flattening. Filtering evaluates the complete unprojected record before selection, distinguishes absent from null and unlike JSON types without coercion, and fails malformed/unsupported controls before provider I/O. Provider request parameters such as `state` remain distinct from output-record filters. No provider-specific parser, proof-only selector or hidden filter is allowed.

Preserve every returned entry, including issue-list entries with `pull_request`; retain that field's absent/value semantics and all nested objects/arrays. Preserve absent, null, false, zero, empty values and exact integer/decimal/identifier values. Per-request provider page size differs from a caller total record cap. Retain actual wire limit, source continuation and unread page tail and distinguish provider total/completeness from fetched, returned and emitted counts. An empty filtered page is not source exhaustion.

## Accepted immutable source

Use the official [`github/rest-api-description`](https://github.com/github/rest-api-description) revision `0a144e030abbe358000f0d32874e197b59a8f7ed`, path `descriptions/api.github.com/api.github.com.json`, operation `issues/list-for-repo`, pointer `#/paths/~1repos~1{owner}~1{repo}~1issues/get`. The complete 12,939,686-byte source was verified against Git blob `ed76b1371e381b078e50c8a698b890911031aeb2` and SHA-256 `7c3a4bfa71e850a32992882392db3cf315dcdeca46ba5a62da963dbea6a589a2`.

The same-commit [`LICENSE.md`](https://github.com/github/rest-api-description/blob/0a144e030abbe358000f0d32874e197b59a8f7ed/LICENSE.md) is MIT, Copyright (c) 2020 GitHub, Git blob `b50625eb63949013cae604b1cadd42cfa1eaf825`, 1,063 bytes and SHA-256 `3243761cbac07e6d169a5a2f4e7c25cc544da85248e735df74c3672e055cc87b`. Its exact text is retained in [github-source-LICENSE](github-source-LICENSE.md).

The [operation closure](github-operation-closure.json) retains bounded normalized facts and the accepted full-source identity. The raw 12.9 MB source is intentionally not duplicated in this docs tree, and the closure is not a compiler input. Issue #27 still first-owns `source.lock`, complete recursive reference capture, NOTICE, compiler fidelity/generated inventories, authored lifecycle cases, common-runner controlled proof and separately authorized live provider proof. Only exact `direct_read` is activated in M6; later GitHub lanes and the aggregate composition remain with their existing owners.