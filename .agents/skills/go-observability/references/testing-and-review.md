# Observability tests and evidence-based review

## Verification matrix

| Feature | Smallest useful evidence |
| --- | --- |
| Structured logs | Capture actual handler output; assert event, level, stable keys and exact expected values under the configured `full`/`masked`/`selective` policy |
| Request correlation | Two related operations share approved correlation context; unrelated operations do not inherit it |
| Trace propagation | Verify parent/child/links across selected boundaries using an approved in-memory exporter; avoid a required external collector |
| Metric instruments | Verify type, unit, bounded dimensions and real measurement semantics, not just presence of a counter |
| Retry/error handling | One request's retries generate accurately named attempts; final outcome/error is not counted twice |
| Flush/shutdown | Owned providers stop/flush with a bounded timeout, and observed errors are surfaced |
| Disclosure | Test independently the application log sink, enabled external exporters and agent output policy |

## Review process

1. Compare current implementation with the already selected `CODEX_PROJECT.md` fields; do not impose an alternative risk classification.
2. Trace the source of each attribute, cardinality and recipient across handlers, interceptors and exporters.
3. Try to falsify "observed exactly once" by identifying retries, concurrent workers or dual middleware.
4. Run only project-declared and authorized checks; in-memory tests are preferred when no external telemetry service is declared.
5. Separate compiler behavior, semantic event correctness, privacy contract, operational overhead and style. Report exact files, reproduction and what remains unverified.

**BAD** — a histogram labeled "milliseconds" but recorded with a raw `time.Duration` integer in nanoseconds. **GOOD** — normalize into the declared instrument unit and test a known duration. Misleading units are correctness defects; absence of metrics in an intentionally minimal CLI is not.

**BAD** — auto-start a collector or expose debug routes to satisfy a skill checklist. **GOOD** — treat such infrastructure as opt-in configuration and separate mutation authorization.

