# Go security rules

Activate only via an exact `go_security.md` Active Rules entry, `go-security` Active Stack Profiles entry or a complete Go Security Profile naming the `go_security.md + go-security` pair. A mere topic mention is not activation.

## Required skills

- `go-core`
- `go-security`

## Fixed target-project policy

- Read the already selected `CODEX_PROJECT.md` Go Application Context And Data Disclosure Policy plus Go Security Profile. These are decided **once during integration** rather than reclassified for every task.
- Use the declared application role, exposure, trusted actors, assets, allowed risks, controls and category-based logging settings. Missing or materially contradictory information is a profile issue to report for owner review, not permission to invent or silently replace a policy.
- The owner may explicitly select `full` unredacted values for designated application-owned local log fields. Do not silently alter them to `masked`; both security and observability use one shared policy. This selection does not authorize disclosure to other destinations such as PR comments or external exporters.
- Identify real code and deployment boundaries and evidence-backed defects. A personal local tool does not automatically require public authentication or a third-party security suite.
- Follow request authorization for any write, dependency update, permission change, credential rotation or tool installation. `govulncheck` is optional, version-verified and not automatically installed.
- Use the Go security skill's focused references for code patterns, dependency scans and verification.

