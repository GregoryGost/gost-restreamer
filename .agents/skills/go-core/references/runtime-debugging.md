# Go terminal debugging — Delve and focused runtime investigation

This reference covers **terminal-only** debugging of Go applications and tests using the already available `dlv` command (Delve). It is optional, not a reason to install tools, require an IDE, start a debug server or create a new skill. Apply it only when `go-core` is active and the current user authorization permits execution of the chosen local program/test and its actual side effects.

## 1. Establish the failing behavior and permitted surface

1. Record the expected/observed behavior, first failure, relevant log/stack trace, source/test revision and a reproducible input or exact test. Trace the relevant source and test first; do not assume breakpoints are always necessary.
2. Read `CODEX_PROJECT.md`, `go.mod`/`go.work`, build tags, `go version` and the declared test/build commands. If Delve is already present, check `dlv version` and `dlv help debug` / `dlv help test`. Delve and Go must be compatible on the actual OS/architecture; preserve the debugger's Go-version compatibility check. Do **not** install/upgrade Delve, change Go toolchain, change ptrace/codesigning/sandbox privileges or enable CGO just to debug.
3. Identify what starting the target/test would do: file/database writes, network calls, message publications, hardware/process effects, credentials, temp binaries, ports and spawned processes. Only execute within the current authorization and verified isolated environment. A command called `debug`, `test` or `trace` is **not** read-only.
4. Check that the terminal can keep a session alive **and** accept subsequent stdin commands. Start interactive Delve only when the terminal/tool can receive its prompt and send a bounded sequence of instructions. If this is unavailable, use the noninteractive fallback below; do not leave a debugger waiting indefinitely.
5. Prefer one target package and one deterministic failing test; do not start the whole system, production service or full end-to-end suite for a narrow unit bug.

## 2. Delve entrypoints and exact argument boundary

These are command **templates** (replace package paths and test names with verified project values). The commands deliberately omit any automatic installation, debugger server and privileged attach:

```sh
go version
dlv version
dlv help test

# Debug a locally authorized main package:
dlv debug ./cmd/service

# Debug a single Go test; arguments after -- belong to the test binary:
dlv test ./internal/service -- -test.run "^TestExample$" -test.count=1 -test.parallel=1

# For an approved prebuilt local binary:
dlv exec ./path/to/debug-binary
```

- `dlv debug [package]` and `dlv test [package]` build debug-oriented binaries with optimizations disabled. The package must actually contain an executable main or runnable tests, respectively.
- `--` separates Delve flags from **target-program/test-binary** arguments. For application flags: `dlv debug ./cmd/service -- -config approved-config`, but only after inspecting config and side effects. The `-test.run` flag belongs **after** `--` for `dlv test`. `-test.parallel=1` limits parallel tests but cannot eliminate goroutines started within the test.
- `dlv exec` needs an existing binary with usable debug information. Optimized or stripped builds may hide variables or distort stepping. When a new debug build is authorized, use project-declared build commands (for example Go `-gcflags="all=-N -l"` on supported versions) and write temporary outputs outside the repository.
- Delve can create `__debug_bin` or other temporary outputs. Use an approved temporary directory and documented `--output` option when needed, inspect artifacts before/after, and clean up only artifacts owned by the diagnostic session. A debugger may also keep per-user command history; avoid printing secrets.
- The snippets do **not** assert that a particular package or binary exists in a target project.

## 3. Terminal session (illustrative commands at the Delve prompt)

After the debugger starts, select a verified line/function. Commands below are separate **interactive** input, not shell commands or a script that is automatically run:

```text
(dlv) break main.main
(dlv) continue
(dlv) next
(dlv) step
(dlv) stack
(dlv) goroutines
(dlv) locals
(dlv) args
(dlv) print variableName
(dlv) exit
```

Use `break package.Function` or `break file.go:line` only when the symbol/line is verified; `main.main` is an example, not a universal breakpoint. `continue`, `next` and `step` execute real program code, not just observations. `stack`, `goroutines`, `locals`, `args` and `print` reveal runtime state, potentially including secrets; redact reports. Conditional breakpoints can help narrow a known failure, but verify expressions and overhead. Do not call application functions or mutate state through `call`, `set`, `jump`, `restart` or debugger scripts by default: they can have side effects and need an explicitly authorized diagnostic purpose.

Use a small hypothesis-driven sequence: set the first relevant breakpoint, reproduce, inspect precise values and goroutine/stack state, test a competing explanation and stop. Preserve the first failure and debugger observations as evidence, not as proof that a code change is correct.

## 4. Terminal without persistent stdin / TTY

- A terminal which merely streams output is **not** sufficient for interactive `dlv`. Check that the agent's execution tool can send input to the **same living session** before starting.
- Prefer an authorized focused `go test -run "^TestExample$" -count=1 -v ./internal/service` and existing errors/logs/stack traces. Use `go test -race` only for suspected data races in an approved environment; use `pprof`/`go tool trace` only for the appropriate approved profiling question.
- Delve provides `--init` for a command file, but do not use it as an automatic workaround for missing stdin. Scripts can run/modify the target or wait forever; permit only when a reviewed, bounded input script, termination behavior, temporary output policy and execution permissions are already established.
- Do not add temporary source breakpoints, debug print statements, `runtime.Goexit` or permanent flags merely to compensate for a noninteractive terminal. If diagnosis remains blocked, report the unavailable input channel and the minimum safe next step.

## 5. Existing processes and special environments

- `dlv attach <pid>` affects an **already running process** and may stop or kill it depending on exit behavior. Attaching to a production/shared service, modifying kernel debugging settings, changing container privileges or using elevated/sudo execution requires separate explicit approval and proven target identity. It is **not** a default local-debug method.
- Do not start `--headless`, open remote debugger listeners, publish debugger ports or connect to third-party machines as part of routine terminal debugging.
- Build tags, target architecture, CGO, inlined functions, stripped symbols, race instrumentation and process scheduling affect the debugger's visibility. A debugging build can behave differently from a release binary; validate the **normal build** after fixing the defect.
- A debugger is not an alternative to the source-specific rules for database, Huma, NATS or cache infrastructure: only use compatible active overlays when the target actually touches those systems.

## 6. Close the diagnostic loop

1. Record the root-cause hypothesis with exact location, observed state, conditions, and any falsifying evidence. Mark unknowns; do not claim a root cause because one debugging session appeared plausible.
2. Exit and account for debugger-owned processes, files/binaries and temporary artifacts; do not delete or terminate unrelated processes.
3. For an authorized implementation change, apply the smallest correction and rerun the **normal focused test without Delve**, then the project-declared broader checks. The test engineer's independent acceptance and the primary's Validation Gate are separate from debugger output.
4. Report actual debugger availability, Go/Delve versions, terminal support, executed commands, side effects, changes in the observed behavior, reproducibility and any checks not run.

## Official references

- Go diagnostics overview: https://go.dev/doc/diagnostics
- Delve getting started: https://github.com/go-delve/delve/blob/master/Documentation/cli/getting_started.md
- Delve CLI commands: https://github.com/go-delve/delve/blob/master/Documentation/cli/README.md
- Delve `debug` / `test` / `exec` help: https://github.com/go-delve/delve/tree/master/Documentation/usage
- Go test flags: https://pkg.go.dev/cmd/go#hdr-Testing_flags

Always check the selected Delve and Go versions. The linked upstream guides evolve; an installed older `dlv` can have different flags or supported platforms.

