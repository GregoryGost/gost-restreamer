# Go language-server analysis with gopls

Use this reference under the already active `go-core` skill when exploring Go
symbols and dependencies, implementing Go changes, or checking project-required
diagnostics. It complements the compiler, `go vet`, declared linters and tests:
none is a complete substitute for another.

## 1. Resolve the project-selected integration

Read `CODEX_PROJECT.md` for the selected gopls mode, executable/version source,
workspace roots, Go build environment, enabled analyzers, diagnostic scope,
severity/acceptance policy and authorized execution side effects. The owner
selects these settings once when adopting the profile; do not invent new
analyzers or reclassify each task. Verify the **actual** installed `gopls`
version, exposed tools/flags and compatibility with the effective Go toolchain.

Available modes (choose only the project's declared and currently accessible one):

| Mode | Behavior and limitations |
| --- | --- |
| Built-in MCP, detached | Official experimental `gopls mcp` over stdio; headless LSP session, sees **saved files only**. Requires an already configured MCP client and gopls v0.20+ with matching support. |
| Built-in MCP, attached | Shares an existing LSP session, including unsaved buffer state; requires separately approved server/listener configuration and verified session identity. Do not automatically start `gopls serve -mcp.listen=...` or expose a port. |
| Existing LSP client | Uses available, verified client operations and workspace configuration; no assumption that the agent can access the VS Code Problems pane or unsaved buffers. |
| CLI fallback | An already available and authorized `gopls check <actual Go file>` can retrieve diagnostics; validate version-specific help, scope, analyzer settings, output and exit status. It is not guaranteed to match all IDE diagnostics. |
| Disabled/unavailable | Use existing authorized `go vet`, project linters and tests, but report gopls diagnostics as **NOT VERIFIED** when they are required. Do not claim parity or a clean workspace. |

No automatic `go install`, toolchain upgrade, MCP registration, editor
configuration, local daemon, network listener, or new third-party wrapper. Even
a nominally read-oriented gopls operation can read the workspace, invoke the
`go` command, download modules, write caches/configuration, and emit opted-in
Go telemetry. Check the applicable request/sandbox/network and credential
boundaries before invoking it. MCP `ReadOnlyHint` does not eliminate these effects.

## 2. Use semantic Go tools deliberately

When official built-in MCP tools are **actually advertised by the connected
server**, prefer focused operations:

- `go_workspace` to establish the Go workspace and build context;
- `go_search` and `go_symbol_references` to locate symbols and their usages;
- `go_file_context` and `go_package_api` to understand cross-file and package
  APIs without relying only on text search;
- `go_diagnostics` to collect parse/build diagnostics for the workspace and
  additional analysis findings for **specific active files**.

In the documented MCP interface, `go_diagnostics` accepts an optional `files`
array of **absolute paths**. Without it, the workspace check need not run
the deeper analyzers that produce IDE hints for the modified files. Supply the
actual changed Go files (and affected files when justified), using the
argument schema **discovered at runtime**. Names, arguments, defaults and
features may change by gopls version; do not fabricate a successful tool call.

A connected tool such as `go_rename_symbol` may return proposed edits. Treat
applying edits, code actions or bulk fixes as **separate source modifications**
subject to the original user request, path ownership, project conventions
and the active Architecture/Validation Gate. Never auto-apply `source.fixAll`
or rename code solely because an assistant tool offers it. `go_vulncheck`
is a distinct vulnerability scan with its own scope and potential side
effects, not a mandatory effect of ordinary diagnostics.

For CLI fallback, an illustrative request is:

```sh
gopls version
gopls help check
gopls check ./internal/example/example.go
```

These are **templates** for an already installed executable and a verified
project file, not universal requirements. Prefer the selected project command;
do not rely solely on the CLI's process exit code to decide whether a
severity-4 Hint or other diagnostic should block acceptance.

## 3. Implement, inspect and re-check

1. Establish the current Go workspace, build tags, CGO/GOOS/GOARCH, `go.work`,
   module/toolchain, settings and diagnostics baseline. Preserve pre-existing
   findings separately from those introduced or exposed by the current edit.
2. Before a nontrivial edit, use semantic references/context tools where
   they reduce uncertainty about callers, interfaces or package ownership.
   A symbol search or MCP summary is a locator, **not** a substitute for
   reading complete relevant source and contracts.
3. After editing, ensure the analysis sees the **same source snapshot** that
   is to be reviewed. In detached MCP/CLI, save changed files first. If
   packages/files/build configuration changed and results seem stale, refresh
   the verified workspace/session as supported, not by blindly trusting a cache.
4. Obtain `go_diagnostics` for the workspace **with changed-file paths**
   when using MCP, or the declared LSP/CLI equivalent. Add impacted files
   when the edit can cause breakage elsewhere. Record source/analyzer, code,
   severity, file/range, finding text and relevant build context.
5. Investigate new or changed actionable findings against real code and
   requirements. Fix confirmed defects within the authorized paths; apply
   project-selected blocking severity/analyzer rules. Nonblocking hints are
   considered, not automatically implemented. Suppression, analyzer
   disabling, broad formatting/refactoring or expanded dependencies are not
   workarounds to obtain an empty list.
6. Re-run affected diagnostics on the **final** source snapshot, together
   with approved formatting, `go vet`, project linters, build and tests.
   Re-analysis after a code correction invalidates older diagnostic results.
   Neither gopls nor a green test alone proves functional correctness.

## 4. Validation and handoff

Report the selected tool/mode and version, whether it was reachable, actual
scope/files, relevant settings, exact invocation or discovered MCP tool,
snapshot identity, findings (new versus pre-existing), disposition under the
declared severity policy and any checks not executed. The primary must not
call the project's **required** gopls diagnostic gate `PASS` when it was
unavailable, stale, partial, or not run. If gopls is not an active project
requirement, do not invent a blocking dependency; use other active checks.

Detached MCP cannot guarantee identical results to VS Code: editor buffers,
analyzer configuration, Go toolchain, environment, build tags, workspace roots
and version all influence diagnostics. Verify differences explicitly instead
of assuming one side is wrong.

## Official references

- Built-in experimental MCP modes, prerequisites and side effects:
  https://go.dev/gopls/features/mcp
- Diagnostics, analysis findings, IDE and CLI behavior:
  https://go.dev/gopls/features/diagnostics
- Language server settings and configurable analyzers:
  https://go.dev/gopls/settings
- Official gopls repository and tool implementations:
  https://github.com/golang/tools/tree/master/gopls
- Go analysis framework (also used by go vet):
  https://pkg.go.dev/golang.org/x/tools/go/analysis
