---
name: pm-go
description: Route PM CLI Go tasks to focused installed engineering references and the repository's architecture and testing contracts. Use for Go design, implementation, debugging, or review; not as permission to start implementation.
---

# PM Go task router

Read `../../../docs/development/skills.md` for the routing table and fallbacks.
Resolve that path from this skill's directory, not from the shell's working directory.
Read the repository's `AGENTS.md` and the task's relevant design first.

1. Identify the changed behavior, owner package, and immediate consumers.
2. If available, load `go-engineering` and only its relevant reference.
3. Select the targeted `cc-skills-golang` entry from the routing table.
4. Treat those community skills as technical references under PM's scope and rules.
5. Use CodeGraph first when this checkout is indexed, with the documented fallback.
6. Write the focused causal validation plan before production edits.
7. Implement only authorized work, then record actual observations and remaining gaps.

This is the sole project router. Do not install a competing umbrella dispatcher.
Do not recursively load every cross-linked skill or an entire library catalog.
An absent optional skill is not a blocker: use the linked official references and state the fallback.
Do not invent a skill file or copy an unlicensed local instruction set.

PM overrides broad community defaults that suggest:

- Running `go test ./...` or the race suite after every change.
- Installing `gotests`, test libraries, linters, or `@latest` dependencies automatically.
- Spawning audit/reviewer agents without task authorization.
- Adding Viper, global initialization, hot reload, or root-wide auth to every command.
- Generating provider-specific Go test wrappers or blindly refreshing golden outputs.

Use small consumer-owned interfaces, typed data, explicit errors and presence, and bounded I/O.
Keep Cobra in CLI and keep shared validation independent of the command parser.
Choose tests for causal behavior; tests through actual PM own connector proof.
Use the installed `nbj-write-clearly` for prose only when the task includes documentation.
No skill overrides the current task's implementation pause or publication boundary.
