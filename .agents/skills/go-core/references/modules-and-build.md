# Go modules, toolchains and reproducible builds

This reference describes version-aware module maintenance and binary build practices; it does not authorize dependency upgrades, network downloads or modifications to `go.mod`/`go.sum`.

## Module/workspace source of truth

- Read `CODEX_PROJECT.md`, `go.mod` and `go.work` when present, `go.sum`, CI configuration, active build tags, OS/architecture targets, CGO and existing vendor policy.
- `go` directive specifies minimum module language/runtime requirements; `toolchain` is a toolchain selection hint. The executing Go command can select/download toolchains depending on `GOTOOLCHAIN` and runtime config; verify actual `go version` and local policy before commands.
- `go.work` is an explicit workspace that can change module resolution and which modules are considered main; inspect `GOWORK` and `go env GOWORK`. Do not automatically commit, modify or remove an existing workspace file.
- Understand Minimal Version Selection and selected dependency graph. `go mod why` and `go list -m` answer different questions and can have cache/network effects.

## Read-oriented and mutating commands

| Tool | Meaning / important caveat |
| --- | --- |
| `go mod tidy -diff` | Shows proposed module/sum changes without writing **those files**, when supported by selected Go; can still load packages or use caches/network |
| `go mod tidy` | **Changes** `go.mod` and `go.sum`; never run as a read-only check |
| `go mod verify` | Compares cached module files against recorded download state; **not** a vulnerability scanner and may read module metadata/download go.mod or alter cache/sums per selected Go |
| `go mod why -m` | Explains why a selected module is needed, within current module context |
| `go list -m all` | Inspects selected graph but may perform module resolution/downloads |
| `go get` / `go work use` | Mutates module/workspace state; requires authorized dependency/workspace change |

Use `GOPRIVATE` / `GONOSUMDB` / `GOPROXY` per project policy; never weaken checksum verification or leak private module paths through unapproved external services merely to obtain a successful build.

## Build, generate and cross-platform targets

- Use `go build` with project-declared output paths, tags and target build matrix. Build commands create artifacts and may download dependencies/toolchains even if no source files change.
- `GOOS`/`GOARCH` and `CGO_ENABLED` affect availability of native dependencies; cross-compiling a CGO project may require an appropriate C cross-compiler. Do not assume one binary runs on every architecture or silently turn CGO off.
- `//go:build` constraints select files; verify linux/windows differences, test tags and APIs under each declared target. A tag change can alter behavior, not merely compilation.
- `go generate` runs arbitrary declared commands and may edit source or create files; inspect directives and side effects before running. Generated files require authoritative generator input.
- `-trimpath`, `-buildvcs`, `-ldflags`, vendor mode, reproducible inputs and version stamping are project choices. Do not claim bit-for-bit reproducibility without comparing actual artifacts and environment.

## Good / Bad decisions

**BAD:** run `go mod tidy` during a review-only task and then report no files changed.

**GOOD:** inspect the actual module state and, where selected toolchain supports it and execution is allowed, show `go mod tidy -diff` output. Apply `tidy` only when user authorization covers `go.mod`/`go.sum` edits.

**BAD:** force `GOTOOLCHAIN=auto` or update the `go` directive for a local failed command without checking project policy.

**GOOD:** report actual selected toolchain vs module/CI requirements; request a bounded toolchain update if required.

## Sources

- Module reference: https://go.dev/ref/mod
- Toolchain selection: https://go.dev/doc/toolchain
- Go command and build flags: https://pkg.go.dev/cmd/go
- Build constraints: https://pkg.go.dev/cmd/go#hdr-Build_constraints
- Go release notes: https://go.dev/doc/devel/release

