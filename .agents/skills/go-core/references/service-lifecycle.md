# Go application lifecycle and operational ownership

This is a `go-core` reference for long-running services and local commands. Do not create a new mandatory service framework or apply a server lifecycle to every library/CLI.

## Service contracts chosen by project

Before implementing, read `CODEX_PROJECT.md` for application role, process ownership, startup/health policy, OS/platform, service manager, shutdown deadline and external system access. Startup, daemonization, signal handling, dependency checks and health routes are **project decisions**, not automatic requirements for a Go module.

## Startup and state

- The composition root constructs shared dependencies once: config, logger, HTTP server/client, database pool, NATS connection, cache and optional telemetry providers.
- Validate necessary config early, but distinguish transient dependency failure from invalid local configuration. Do not require all remote services to be up during startup unless the project contract demands it.
- Avoid package `init()` for mutable global network connections, DB migrations or external side effects. Keep owned init failures observable and resources cleaned on partial startup.
- For public services, distinguish liveness (process health) and readiness (can handle intended work) when configured. A local CLI needs neither by default.
- Do not automatically expose `/health`, `/metrics`, `/debug/pprof` or other listeners; introducing a network endpoint is a separate project decision.

## Shutdown ordering

1. Stop accepting new work at owning boundaries (e.g. scheduler, HTTP server, queue consumer), according to the project policy.
2. Signal owned background workers to cancel, wait for bounded completion and decide drain vs abort per component.
3. Finish or roll back owned transactions, release resources **after** their users stop. Close/drain shared NATS/SQL/HTTP resources once at composition root, not per request.
4. Flush any telemetry that the project actually enabled with a bounded independent shutdown context; do not allow a stuck exporter to prevent exit forever.
5. Report partial drain, dropped work and close errors; don't silently report successful graceful shutdown when the deadline expired.

**BAD** — using the signal-canceled request context for cleanup: `http.Server.Shutdown(ctx)` may immediately fail with `context.Canceled`.

**GOOD — bounded independent cleanup context** (Go 1.16+; abbreviated fragment after `server` has been created and started):

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
defer stop()

<-ctx.Done()

shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
defer cancel()

if err := server.Shutdown(shutdownCtx); err != nil {
    // Report and follow the project's fallback policy.
    return err
}
```

Imports: `context`, `os`, `os/signal`, `time`. Snippet is a **fragment** inside a function returning `error`, not a complete `main`. Add only signals supported by the target OS and declared orchestration. `stop()` unregisters notification; call when no longer needed.

`http.Server.Shutdown` closes listeners and waits for ordinary active requests, but **does not** wait for hijacked connections (such as WebSockets); protocol-specific work needs explicit termination. Server `ListenAndServe` returning `http.ErrServerClosed` during expected shutdown is not an unexpected crash. Account for server goroutine completion before process exit.

## Tests and operational review

- Verify partial-startup cleanup, startup errors, signal/cancellation paths and shutdown order using controlled listeners and in-memory/disposable dependencies.
- Check missing readiness transitions, worker leaks, unobserved panic/error exits, pending work left behind, double close and repeated start/stop if supported.
- Do not perform real signal kills, network publishing, process management or production service restarts under a documentation task.
- See `go-core/references/errors-and-lifecycle.md` for general context/resource ownership and `go-testing/references/leak-regression.md` for lifecycle regression tests.

Official sources: https://pkg.go.dev/os/signal ; https://pkg.go.dev/net/http#Server.Shutdown ; https://pkg.go.dev/context

