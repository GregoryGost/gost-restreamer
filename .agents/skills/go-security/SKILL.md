---
name: go-security
description: "Use for Go security engineering under a fixed project risk/disclosure profile: code boundaries, dependencies, network and input safety."
---

# Go Security — declared project controls

Use verified Go security engineering practices proportionately to the application's **preselected** context. Do not recompute or override its risk classification on each task.

## Required Dependencies

- `go-core`

Apply `.codex/rules/go_security.md` only when independently active.

## Workflow

1. Read the Go Application Context And Data Disclosure Policy and Go Security Profile in `CODEX_PROJECT.md`. This configuration is selected once when the skill is integrated. If an applicable field is absent or demonstrably inconsistent with real deployment, report the exact gap and seek a profile decision rather than assuming a new default.
2. Examine the complete relevant code, tests, module and toolchain versions, trust boundaries and authorized operation.
3. Identify actual input/authorization, SQL, filesystem, OS-command, TLS, crypto or dependency weaknesses only when relevant; separate proven defects, accepted risks and optional hardening.
4. Respect the shared logging choice: `full`, `masked` or `selective` with per-category rules for application-owned sinks. A deliberate full-value local diagnostic policy is valid. Separate transfer to agent reports, PRs, CI and third-party services requires independent authorization.
5. Apply the smallest approved change, then verify against project-declared tests. Never install scanners, rotate credentials, add network listeners or change the policy as an incidental operation.

## Reference routing

- `references/project-risk-and-log-policy.md` — once-only profile and disclosure boundaries.
- `references/go-security-patterns.md` — conditional Go code Good/Bad patterns.
- `references/vulnerability-review-and-testing.md` — vulnerability review, tool limitations and tests.
- `references/official-sources.md` — Go official sources.

## Optional coordination

`go-observability`, `go-http-client`, `go-huma-v2` and `go-testing` are independent overlays when selected. The single target-project disclosure policy controls both security review and observability implementation.

