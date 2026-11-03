# Go security patterns under the selected trust boundaries

## 1. SQL parameterization

**BAD** when `name` is untrusted input (fragment, compilable only with surrounding symbols):

```go
row := db.QueryRowContext(ctx, "SELECT id FROM users WHERE name = '"+name+"'")
```

**GOOD** for a SQLite/MySQL driver that accepts `?` placeholders (fragment):

```go
row := db.QueryRowContext(ctx, "SELECT id FROM users WHERE name = ?", name)
```

PostgreSQL drivers commonly use `$1` rather than `?`. Check dialect and actual client API. A static trusted query is not automatically injection.

## 2. Filesystem and OS-command boundaries

**BAD** for untrusted input: concatenating a user-selected filesystem path or shell command into an unchecked path/executable invocation. **GOOD**: resolve the declared directory authority and use `filepath.Rel` or platform-appropriate mechanisms, with symlink/race protections where the threat model requires; invoke `exec.CommandContext` with controlled executable and separate arguments rather than interpolating a shell string.

A project-owned local shell utility may intentionally allow arbitrary commands; that is an explicit capability, not automatically a defect.

## 3. Network and cryptography

- Verify actual URL, redirect, proxy, DNS, IP and TLS settings before claiming SSRF, insecure transport or untrusted redirect. Avoid naive string-prefix origin checks.
- Prefer `crypto/rand` where unpredictable values are a real cryptographic requirement. Password storage, encryption and signing need separate verified contracts.
- Huma's public OpenAPI security declarations are not authorization enforcement; only inspect/modify the Huma layer if independently active and in scope.
- Follow the selected `full`/`masked`/`selective` application-logging policy; identifying a logging preference is not automatically a security defect.

## Review standard

Name the untrusted origin, actual reachable data flow, project-required invariant, exploit preconditions and concrete effect. Provide counterevidence and the smallest authorized fix. An incident claim without exposure evidence remains a hypothesis.

