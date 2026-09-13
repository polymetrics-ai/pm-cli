# Admission wire and limit bindings — F01 remains open

Status: essential specification blocker. This reference records existing authority and the minimum missing decision. It does not amend the accepted API or choose production defaults.

The preserved acceptance fixes HTTP 202 after commit and HTTP 409 for the same scoped idempotency key with changed input. CLIENT-PARITY's shared error rule requires RFC 9457 `application/problem+json`, a safe code, incident ID, scope-safe detail and retry disposition. The selected parity rows preserve current authorization, optimistic control revision and scoped mutation idempotency. PLAN requires finite pending count and age caps; PostgreSQL failure never accepts work. These statements are sufficient for those assertions, but do not define every refusal response.

The complete canonical PLAN, CONTRACTS and CLIENT-PARITY were read. A targeted search of those sources and the setup-only PM checkout found no accepted table binding admission refusals to exact HTTP status, safe code and retry disposition. `CONTRACTS.json` has `synthetic_server_config.limits.admission_backlog_runs = 1000` and `admission_backlog_max_age = "1h"`. Those are explicitly synthetic values. They are not approved production defaults or a complete scope/boundary contract.

Before the admission pilot is specification-ready, its product/API approver must supply or approve:

| Required binding | Minimum decision |
| --- | --- |
| Refusal catalog | Exact status, safe code and retry disposition for unavailable handler, incompatible foundation, wrong workspace/connection, expired/revoked grant, stale schema/descriptor, backlog count/age limit, unavailable PostgreSQL, unavailable resume and stale control revision. Reuse an accepted existing entry where one exists; identify its version and path. |
| Limit semantics | Configuration scope; counted pending states; count and age threshold comparison including equality; age clock/reference; transactional reservation behavior under concurrent requests; whether zero disables admission or is invalid. Preserve existing finite bounds and tenant/run/window/account obligations. |
| Fixture binding | Explicit small synthetic limit values and below/equal/above/age cases derived from that accepted rule, separately labeled from production defaults. |

S06–S08 and S11/S13 cannot assert exact wire results until these bindings exist. Other examples retain their existing assertions. The proposed tests must run through the public Gin route, observe zero unauthorized acceptance/provider sends, and retain the original store and app commands. An author or reviewer must not resolve this gap by guessing status codes.

Bounded authority: the admission pilot's `contracts.json` contains exact selected parity rows and original acceptance. The staged canonical CONTRACTS and CLIENT-PARITY retain the synthetic config and shared error rule. Source digests are recorded in the snapshot and input manifest; the canonical sources were not edited.
