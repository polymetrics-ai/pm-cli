# 0008: pm-server can take an inherited listener from its starter

Date: 2026-11-18. Status: proposed. This record does not authorize application work. Every flag, text and check below is a planned contract until the owning implementation creates and runs it. Issue: #72.

## Context

`pm-server` binds its configured listener address itself (`server.listen`). It binds the address only after its whole start: it reads the configuration and opens the application first (`cmd/pm-server/main.go:102-121` at `8c1eaa2`). When the bind fails, the process stops with exit code 1 and the line `pm-server: configured server listener is unavailable` (`:119-121`).

A starter that must know the address before the process starts has no safe way to choose it. The acceptance harness is such a starter. It writes the address into the configuration, and it puts its own TLS edge in front of the server. Today it binds `127.0.0.1:0`, reads the port, closes the socket, and gives `pm-server` the address (`reserveLoopbackAddress`, `tests/acceptance/deployment_test.go`). Between that close and the bind of `pm-server`, any other socket can take the port: the harness's own edge, which also binds `127.0.0.1:0` immediately after, a client connection, or another process on the runner. Run 44223051987 failed this way. `pm-server` stopped after 61 ms with the line above, and one scenario failed.

A starter cannot close this gap from outside the process without a retry, a sleep or a poll. The test-wait rule allows none of them.

ADR 0005 already gives the pattern for a descriptor that a starter passes to the process: `--ready-fd`, with strict value rules and a check of the descriptor before use.

## Decision

`pm-server` accepts `--listen-fd <n>`. The value names an inherited descriptor of a TCP socket that the starter has already bound and set to listen. With the flag, the process does not bind `server.listen`. It takes that socket as its listener, after the checks below. Without the flag, the process binds `server.listen` itself, as today.

- **The value.** The rules of `--ready-fd` apply (ADR 0005, "Request"): only ASCII decimal digits, no leading zero, it fits in a 32-bit signed `int`, it is 3 or more, and the flag may appear once. When both flags are present, they must name different descriptors.
- **The checks of the descriptor.** The process checks the descriptor after the `--config` and `--secrets-directory` checks, and before it opens the application. The descriptor must:
  - be open, and not have the close-on-exec flag set when the process checks it. As in ADR 0005, a descriptor with that flag set belongs to the process itself;
  - be a stream socket that listens for connections;
  - have a local address that equals the configured `server.listen`, compared after the same address parsing that the server uses for `server.listen`.
- **After the checks.** The process sets close-on-exec on the descriptor, so that no child process holds it, and serves on it. The configuration stays the one source of the server address. The flag only lets the process use a socket that is already bound at that address.
- **`pm-worker`.** It has no listener, so it gets no such flag.

### Refusals

Each refusal: exit code 1, nothing served, and exactly one stderr line after `pm-server: `.

| Order | Case | Text after `pm-server: ` |
| --- | --- | --- |
| 1, while the flags are read | `--listen-fd` with no value | `flag needs an argument: --listen-fd` (the text of the flag parser) |
| 1, while the flags are read | A second `--listen-fd` | `invalid argument "<value>" for "--listen-fd" flag: --listen-fd may be supplied exactly once` |
| 1, while the flags are read | A value that breaks the value rules above | `invalid argument "<value>" for "--listen-fd" flag: --listen-fd requires only the digits 0-9, with no leading zero, for a number from 3 to the int maximum` |
| 2, after the flags are read | `--listen-fd` and `--ready-fd` name the same descriptor | `--listen-fd and --ready-fd require different descriptors` |
| 3, after the `--config` and `--secrets-directory` checks | A descriptor that is not open, has close-on-exec set, is not a listening stream socket, or has another local address than `server.listen` | `--listen-fd requires an inherited listener at the configured server address` |

The value of the flag is a number, not a secret. The address is not printed.

### The acceptance harness

The harness binds `127.0.0.1:0` and keeps the listener. It writes that exact address into the configuration, and it starts `pm-server` with `--listen-fd`. The starter passes the socket as an inherited descriptor, as it passes the ready descriptor. After the start, the harness closes its own copy; the child keeps its copy. No pick-and-release step remains.

### Options

| Option | Result |
| --- | --- |
| An inherited listener, `--listen-fd` | Selected. No gap: the port stays held from the bind to the first accept. It follows the descriptor rules of ADR 0005. A service manager can also use it to start `pm-server` with a socket that it holds. |
| `pm-server` binds port 0 and reports its address on the ready line | Rejected. It changes the ready line of ADR 0005, which scenario A1 checks word for word, and every reader of that line. |
| Choose the port outside the range for client connections, and start the edge after `pm-server` | Rejected as the fix. It makes the gap smaller but does not close it: another process can still bind the port. |
| Start `pm-server` again after a bind failure | Rejected. The test-wait rule allows no retry. |

## Consequences

- Easier: a starter can give `pm-server` a known address with no race. A service manager can start `pm-server` with a socket that it holds.
- Harder:
  - one more operator flag and its checks in `cmd/pm-server`;
  - the starter passes a second inherited descriptor;
  - `docs/development/setup.md` describes the flag.
- Risk: a starter can pass the wrong socket. The checks refuse a descriptor that is not a listening stream socket or is not at the configured address. A starter that passes a listening socket at the configured address that it does not intend to share is outside this record, as for any process that a starter controls.
- New dependencies: none.
- Owners who must change: `cmd/pm-server` (the flag and its checks), `tests/starter` (a second inherited descriptor), the acceptance harness in `tests/acceptance`, `docs/development/setup.md`.

## Decisions that this record touches

- ADR 0005 (process ready signal): this record uses its descriptor rules and changes nothing in it.
- Decision 0001 (CLI configuration): a malformed flag value is refused, not corrected.

## Planned proof

- Developer checks of `cmd/pm-server`: each refusal in the table; a descriptor that is a regular file, a datagram socket, a stream socket that does not listen, or a listener at another address; a descriptor with close-on-exec set; the same number as `--ready-fd`.
- The race check, in the harness: on the old code, the harness reserves the address, a second listener binds it, and `pm-server` stops with `configured server listener is unavailable`. With the flag, while the harness holds the listener, a second bind of that address fails with `EADDRINUSE`, and `pm-server` starts and sends its ready line on the held socket.
- Gherkin: every stack scenario starts `pm-server` through the harness, so every stack run uses the flag. A refusal scenario for a wrong descriptor is a candidate for the criteria owner. The race itself has no Gherkin proof, because a scenario cannot force another socket onto the port without timing.

## Evidence

- Run 44223051987 (#402 at `8c1eaa2`): `pm-server start: end of file before the ready line, after 61.315818ms`, and `stderr="pm-server: configured server listener is unavailable\n"`, exit 1, in the scenario `A device login start answer states the configured poll interval: 1 seconds` (`features/device_poll_interval.feature:57`). The same scenario passed in runs 44223960871 and 44215806219.
- `cmd/pm-server/main.go:119-121` at `8c1eaa2`: the bind of `server.listen` and the refusal line.
- `tests/acceptance/deployment_test.go` at `8c1eaa2`: `reserveLoopbackAddress` binds `127.0.0.1:0`, closes the socket, and returns the address; `deployment.open()` then starts the edge, which binds `127.0.0.1:0` again.
- ADR 0005, "Request" and "Refusals": the value rules and the close-on-exec rule of `--ready-fd`. The descriptor checks of `--ready-fd` in `cmd/pm-server/main.go:215-233` at `f35eee7`.

## Revisit trigger

- `pm-server` gets more than one listener.
- PM supports a platform where a starter cannot pass an inherited descriptor.
