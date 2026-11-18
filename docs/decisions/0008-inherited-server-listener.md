# 0008: pm-server can take an inherited listener from its starter

Date: 2026-11-18. Status: proposed. This record does not authorize application work. Every flag, text and check below is a planned contract until the owning implementation creates and runs it. Issue: #72.

## Context

`pm-server` binds its configured listener address (`server.listen`) itself. At `f35eee7` the start order is: validate the configuration (`cmd/pm-server/main.go:112`), open the application (`:116`), bind `server.listen` with `net.Listen("tcp", …)` (`:129`), then serve (`:135`). As ADR 0005 defines, `pm-server` is ready when its admission probe passed, its state is serving, and its HTTP accept loop calls Accept for the first time; it then sends the ready line through `--ready-fd`. No incoming connection is needed for that. So the bind comes after the configuration and the application open, and before serving and the ready signal. Any bind error ends the start with exit code 1 and the line `pm-server: configured server listener is unavailable`.

`server.listen` is a literal IP address and a port: the configuration accepts only what `netip.ParseAddrPort` accepts, with a port other than 0 and no zone (`internal/config/server.go:109-113` at `f35eee7`). It accepts no host name.

A starter that must know the address before the process starts has no safe way to choose it. The acceptance harness is such a starter: it writes the address into the configuration and puts its own TLS edge in front of the server. It binds `127.0.0.1:0`, reads the port, closes the socket, and gives `pm-server` the address (`reserveLoopbackAddress`, `tests/acceptance/deployment_test.go`). Between that close and the bind of `pm-server`, any other socket can take the port: the harness's own edge, which binds `127.0.0.1:0` immediately after, a client connection, or another process. The source proves this gap.

Run 44223051987 shows a bind failure that agrees with this gap: `pm-server` stopped after 61 ms with `configured server listener is unavailable`, and one scenario failed. The log does not show the bind error or the socket that held the port, so the gap is the probable cause of that run, not an observed one.

A starter cannot close this gap from outside the process without a retry, a sleep or a poll. The test-wait rule allows none of them.

ADR 0005 already gives a pattern for a descriptor that a starter passes to the process: `--ready-fd`, with strict value rules and a check of the descriptor before use.

## Decision

`pm-server` accepts `--listen-fd <n>`. The value names an inherited descriptor of a TCP socket that the starter has already bound and set to listen at the configured `server.listen`. With the flag, the process does not bind `server.listen`. It takes that socket as its listener, after the checks below. Without the flag, the process binds `server.listen` itself, exactly as today.

There is no fallback. When `--listen-fd` is present and any check fails, the process refuses and stops. It never binds `server.listen` in place of an unusable inherited socket.

### The value

The number rules of `--ready-fd` apply (ADR 0005, "Request"): only ASCII decimal digits, no leading zero, it fits in a 32-bit signed `int`, it is 3 or more, and the flag may appear once. When both flags are present, they must name different descriptors.

The refusal text never repeats the raw value, unlike the `--ready-fd` refusals of ADR 0005, which repeat it. A rejected value need not be a number, and a command line can carry private text by mistake (pm-cli `AGENTS.md`: keep secrets out of command arguments and errors). So the process reads the raw values while the flags are parsed, without a check that can fail, and checks them after parsing, with fixed texts. The flag parser's own error wrapper therefore never sees a failing `--listen-fd` value.

### The checks of the descriptor

The process checks the descriptor after the `--config` and `--secrets-directory` checks and after the configuration is validated, and before it opens the application. The descriptor must:

1. be open, and not have the close-on-exec flag set at the check. As in ADR 0005, a descriptor with that flag set belongs to the process itself;
2. be a socket of type stream in the family of the configured address: `AF_INET` for an IPv4 address, `AF_INET6` for an IPv6 address;
3. use the TCP protocol;
4. listen for connections;
5. have a local address equal to the configured `server.listen`. Equality means: the same family, the same IP address and the same port. The configured address has no zone, so a socket address with a scope or zone does not equal it. An IPv4 address in an `AF_INET6` socket (an IPv4-mapped address) does not equal an IPv4 configured address. A wildcard address (`0.0.0.0` or `::`) equals only the same wildcard.

### Ownership of the listener

- **The starter** owns the socket from its bind until the process has served one request on it. It keeps one copy open for that whole time, so the port stays held through the handover. It protects every copy that it keeps from inheritance by any other child (close-on-exec), and passes the socket only to this process, as the inherited descriptor `n`. It closes every copy that it owns when the launch fails, when the start fails, and at its normal stop.
- **The process** owns its inherited descriptor from the check. If a check fails, it closes the descriptor and stops. After the checks it sets close-on-exec on the descriptor before any code that can start a child process, makes its listener from it, and closes every other handle of the same socket that the conversion leaves, so that it keeps exactly one. It closes the listener when serving ends, also on a start failure after the checks.
- The configuration stays the one source of the server address. The flag only lets the process use a socket that is already bound at that address.
- `pm-worker` has no listener and gets no such flag.

### Refusals

Each refusal: exit code 1, nothing served, and exactly one stderr line after `pm-server: `. No line contains the raw flag value or the address.

| Order | Case | Text after `pm-server: ` |
| --- | --- | --- |
| 1, while the flags are read | `--listen-fd` with no value | `flag needs an argument: --listen-fd` (the text of the flag parser, which contains no value) |
| 2, after the flags are read | `--listen-fd` given more than once | `--listen-fd may be supplied exactly once` |
| 2, after the flags are read | A value that breaks the number rules above | `--listen-fd requires only the digits 0-9, with no leading zero, for a number from 3 to the int maximum` |
| 2, after the flags are read | `--listen-fd` and `--ready-fd` name the same descriptor | `--listen-fd and --ready-fd require different descriptors` |
| 3, after the configuration is validated | A descriptor that fails any check of "The checks of the descriptor" | `--listen-fd requires an inherited listener at the configured server address` |

### The ready signal

The ready contract of ADR 0005 does not change. The process sends its ready line through `--ready-fd`, never through the listener. With `--listen-fd`, the process serves on the inherited listener, and the readiness boundary of ADR 0005 stays exactly as it is: the admission probe passed, the state is serving, and the HTTP accept loop calls Accept for the first time, now on the inherited listener. No incoming connection is needed before the ready line.

### The acceptance harness

The harness binds `127.0.0.1:0` and keeps the listener. It writes that exact address into the configuration, and it starts `pm-server` with `--listen-fd`, through the starter, as it passes the ready descriptor. It keeps its copy open until `pm-server` has sent its ready line and has served a request, then closes it. No pick-and-release step remains.

### Options

| Option | Result |
| --- | --- |
| An inherited listener, `--listen-fd` | Selected. No gap: the port stays held from the starter's bind until `pm-server` serves on it. It follows the number rules of ADR 0005. A service manager can also use it to start `pm-server` with a socket that it holds. |
| `pm-server` binds port 0 and reports its address on the ready line | Rejected. It changes the ready line of ADR 0005, which scenario A1 checks word for word, and every reader of that line. |
| Choose the port outside the range for client connections, and start the edge after `pm-server` | Rejected as the fix. It makes the gap smaller but does not close it: another process can still bind the port. |
| Start `pm-server` again after a bind failure | Rejected. The test-wait rule allows no retry. |

## Consequences

- Easier: a starter can give `pm-server` a known address with no race. A service manager can start `pm-server` with a socket that it holds.
- Harder:
  - one more operator flag, its checks and fixed refusal texts in `cmd/pm-server`;
  - the starter passes a second inherited descriptor and owns its copies as above;
  - `docs/development/setup.md` describes the flag.
- Risk: a starter can pass the wrong socket. The checks refuse a descriptor that is not a TCP listener of the configured family at the configured address. A starter that passes a listener at the configured address that it does not intend to share is outside this record, as for any process that a starter controls.
- New dependencies: none.
- Owners who must change: `cmd/pm-server` (the flag, the checks, the ownership), `tests/starter` (a second inherited descriptor), the acceptance harness in `tests/acceptance`, `docs/development/setup.md`.

## Decisions that this record touches

- ADR 0005 (process ready signal): this record uses its number rules and its close-on-exec rule. It differs in one point on purpose: its refusal texts never repeat the raw value. It changes nothing in ADR 0005.
- Decision 0001 (CLI configuration): a malformed flag value is refused, not corrected.
- pm-cli `AGENTS.md` (checked at `f35eee7`): secrets stay out of command arguments and errors (the fixed texts); Gherkin scenarios that run the real binaries are the only behaviour proof (the scenarios below); missing wiring is unavailable, never a fallback (no bind of `server.listen` after a failed `--listen-fd` check); Cobra stays in the command adapter (`cmd/pm-server`).

## Planned proof

**Gherkin scenarios that run the real binaries** (the criteria owner sets the final text; QA writes the RED):
1. **`pm-server` serves on a listener that its starter holds.** The starter binds an address, keeps its copy open, writes the address into the configuration and starts `pm-server` with `--listen-fd`. In this order: the starter receives the ready line through `--ready-fd`; then a `pm` command through the lane succeeds; the starter's copy stays open through both. An implementation that closed the inherited socket and bound the address again would fail here, because the starter's open copy holds the port. On the code before this record, the scenario fails at the start with the flag parser's unknown-flag refusal.
2. **Without `--listen-fd`, a held address stops the start.** The starter holds the configured address and starts `pm-server` without `--listen-fd`. `pm-server` stops with exit code 1 and `configured server listener is unavailable`. This is the bind failure of the old start path, shown deterministically, and it passes on the code before and after this record. It is distinct from the unknown-flag refusal of scenario 1.
3. **A refused descriptor.** The starter passes a listener at another address with `--listen-fd`. `pm-server` stops with exit code 1 and `--listen-fd requires an inherited listener at the configured server address`, and nothing listens at the configured address.

**Existing coverage that must stay green:** a start without `--listen-fd` that serves (the developer check of the existing bind path, and every stack scenario until the harness change), and the ready line through `--ready-fd` (scenario A1, `ready_signal.feature:14`).

**Developer checks, not acceptance proof:**
- each refusal in the table, with a private-text sentinel as the raw value, and a check that the sentinel appears in no output;
- a regular file, a datagram socket, a stream socket in the wrong family, a stream socket that is not TCP (a Unix stream socket), a stream socket that does not listen, a listener at another address or port, close-on-exec set, the same number as `--ready-fd`;
- the ownership: after the start, the process holds exactly one handle of the socket, with close-on-exec set; after a refused check and after serving ends, the process holds none;
- in the harness: on the old code, a second listener on the reserved address makes `pm-server` stop with `configured server listener is unavailable`; with the flag, while the harness holds the listener, a second bind of that address fails with `EADDRINUSE`, and `pm-server` serves and sends its ready line through `--ready-fd`.

## Evidence

- Run 44223051987 (#402 at `8c1eaa2`): `pm-server start: end of file before the ready line, after 61.315818ms`, and `stderr="pm-server: configured server listener is unavailable\n"`, exit 1, in `A device login start answer states the configured poll interval: 1 seconds` (`features/device_poll_interval.feature:57`). The same scenario passed in runs 44223960871 and 44215806219. The log shows no bind error and no competing socket.
- `cmd/pm-server/main.go` at `f35eee7`: `config.Validate` (`:112`), `server.Open` (`:116`), `net.Listen("tcp", …)` and the refusal line (`:129-131`), `application.Serve` (`:135`). The same bind and line are at `:119-121` at `8c1eaa2`.
- `internal/config/server.go:109-113` at `f35eee7`: `server.listen` is a `netip.AddrPort` with a port other than 0 and no zone.
- `tests/acceptance/deployment_test.go` at `8c1eaa2`: `reserveLoopbackAddress` binds `127.0.0.1:0`, closes the socket, and returns the address; `deployment.open()` then starts the edge, which binds `127.0.0.1:0` again.
- ADR 0005, "Request" and "Refusals": the number rules, the close-on-exec rule, and that the `--ready-fd` refusals repeat the given value. The `--ready-fd` descriptor checks in `cmd/pm-server/main.go:215-233` at `f35eee7`.

## Revisit trigger

- `pm-server` gets more than one listener.
- `server.listen` starts to accept a host name or a zone.
- PM supports a platform where a starter cannot pass an inherited descriptor.
