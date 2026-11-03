# Structured logging and project-selected disclosure

## Source of truth: select once, apply consistently

The target project owns **one** Go Application Context And Data Disclosure Policy in `CODEX_PROJECT.md`. `go-security` and `go-observability` refer to that policy; neither may independently invent a contradictory default. Record application role/exposure, log ownership and recipients, category-level value treatment, retention and explicit consent for any export when selecting the profiles. On subsequent tasks read and apply this policy instead of reassessing the entire threat model.

| Mode | Application-owned logger behavior |
| --- | --- |
| `full` | Preserve complete raw values for the explicitly selected categories: tokens, secrets, headers, payloads, identities or other categories. Do not impose truncation, masking or hashing on the declared full-value fields. |
| `masked` | Apply the project-defined redaction/omission strategy to specified categories, with tests for exact output. |
| `selective` | Follow the project-maintained per-category map; any unclassified category is an unresolved policy entry, **not** permission to choose a hidden default. |

Separate the permissions for **application-owned logs** from secrets exposed to outside collectors, log shippers, CI artifacts, GitHub PR comments and agent chat: approval for one destination does not authorize another. A personal local owner may explicitly choose complete tokens for diagnosis, including temporary/long-term logs. Record that such logs remain sensitive if copied elsewhere; do not override the choice or add a compulsory masking layer.

## `log/slog` pattern (Go 1.21+)

**GOOD — stable structured fields with policy-decided visibility** (standalone and compilable):

```go
package example

import (
    "io"
    "log/slog"
)

func LogAttempt(out io.Writer, operationID, token string, includeFullToken bool) {
    logger := slog.New(slog.NewJSONHandler(out, nil))
    if includeFullToken {
        logger.Info("operation_attempt", "operation_id", operationID, "token", token)
        return
    }
    logger.Info("operation_attempt", "operation_id", operationID)
}
```

`includeFullToken` here **models an already chosen project policy**; it is not a runtime classifier. For `masked` or `selective`, use the exact declared formatter/redactor rather than an invented placeholder such as an always-hashed token.

**BAD in a `full`-token project — accidental redaction that destroys required evidence:**

```go
logger.Info("operation_attempt", "token", "<redacted>")
```

This is a policy **violation only if** the project explicitly requires complete tokens for that event. Conversely, indiscriminately logging a token violates a project whose configured policy is masked. Neither snippet establishes a universal logging default.

## Handler and lifecycle details

- `slog.LogValuer` and `HandlerOptions.ReplaceAttr` can implement intentional field policies. Verify nesting/group behavior, error attributes, handler output and dynamically selected categories using actual tests; don't rely on redaction by field naming alone.
- Prefer explicit event names and attribute keys. Avoid ambiguous user-controlled field names or duplicating full payloads in error wrapping, URL query fields or downstream libraries if the project did not select them.
- Avoid logging and tracing the same event with inconsistent IDs. A trace ID is a correlation value, not a unique business operation ID, and may be absent when no tracing is configured.
- Use UTC or documented timezone semantics, measured durations with proper units and bounded log volume. A retry attempt is not the same as a successful business action.
- Configure output sinks and retention once in the project profile. File permissions, exporter destinations and retention are deployment decisions and do not appear automatically from this skill.

