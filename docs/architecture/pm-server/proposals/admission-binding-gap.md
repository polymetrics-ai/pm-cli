# Admission wire and limit binding — accepted decision record

Status: RESOLVED BY ACCEPTED PRODUCT DECISION; IMPLEMENTATION AND ALL APPLICATION TESTS UNEXECUTED.

The former specification blocker is closed by [F01-v1 and AUTH-MUTATION-PARITY-v1](https://github.com/polymetrics-ai/pm-cli/issues/14#issuecomment-5670702388). The exact accepted public body is 16,655 bytes with SHA-256 `608eddb50248b75a16df55911528fce20836df669645b0b5f300450d0120d6c0`. This retained file records why the proposal is no longer open; [CONTRACTS](../architecture/CONTRACTS.json) and [CLIENT-PARITY](../architecture/CLIENT-PARITY.json) are normative.

The accepted refusal catalog binds authentication, hidden/missing resource, unavailable grant/capability/foundation, stale specification, workspace/deployment pending limits, unavailable/unknown control-store outcome, resume, control revision, idempotency conflict, invalid request and expired retry receipt to exact HTTP status, safe code and recovery action. Authentication and scope-safe authorization precede idempotency disclosure. Refusal order is workspace pending-count 429 first, nonempty oldest workspace pending-reservation age 429 second and deployment pending-count overload 503 last when predicates coincide; either workspace refusal therefore wins over deployment overload. The server never guesses `Retry-After` or reveals inaccessible resource existence.

The accepted finite profiles are:

| Profile | Deployment pending | Workspace pending | Workspace maximum pending age |
| --- | ---: | ---: | ---: |
| development | 32 | 8 | 2 minutes |
| standard | 256 | 32 | 10 minutes |

Generated local development selects `development`; deployment configuration explicitly selects one profile. Typed overrides resolve once through the single configuration owner, stay finite and strictly positive, require workspace count no larger than deployment count, reject obsolete/unknown aliases and apply across all replicas through one durable reservation authority. Synthetic `3/2/30s` values are tests only. Historical `1000/1h` is not a production default.

A pending reservation commits atomically with idempotency, RunSpec/grant/run, separate execution budgets, audit/event and outbox and remains through queued, blocked, reconciling, awaiting-approval and Temporal acknowledgement until the controlled durable execution-start transition or a known terminal pre-start outcome. Post-reservation count must be `<=` each cap. Oldest pending age uses immutable `accepted_at` and the authoritative PostgreSQL clock and refuses a nonempty pending set at `>=` the cap, including equality; an empty set has no age refusal. Retry does not reset age; accepted work is never discarded by admission pressure. Issue #14 proves the actual route and controlled transition, while issue #16 later proves actual-worker-start integration.

An exact currently authorized replay returns the original acceptance identity with no new reservation, dispatch or capacity decision. Changed canonical intent under a committed key is 409. Unknown commit reconciles only with the same key. CLI/browser clients persist a secret-free key/intent fingerprint before the first client-to-API send and make at most three such sends inside one 30-second recovery deadline with 250 ms then 1 second backoff. A server `Retry-After` may delay only within the remaining client deadline; provider/run-owned retry timing is independent. They never mint a replacement key after uncertainty, auto-rebase stale control revisions or auto-resubmit 410.

Full safe retry responses remain at least seven days after known resolution. Unresolved receipts/digests/guards never expire by time. Afterwards a secret-free key/digest/outcome-reference refusal guard survives for the immutable workspace/key namespace lifetime: exact old intent returns 410 with a scope-safe reference when available, changed intent remains 409, and workspace deletion cannot recycle the namespace.

Issue #8 owns public DTO/error projection, issue #9 durable primitives, issue #14 the actual admission route and controlled execution-start transition, issue #12 the six identity mutations and issue #17 its existing grant/budget work. Issue #16 later owns actual-worker-start integration and issues #19/#21 later own CLI/browser recovery; those consumers are not backward prerequisites. This decision adds no per-checklist review gates and transfers no implementation ownership.