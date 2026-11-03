# Go metrics and distributed tracing boundaries

## Choose the actually configured telemetry stack

An application can use structured `slog` logs with **no** OpenTelemetry. The optional Go Observability Profile selects any enabled OTel libraries, exporters, destinations, sampling, provider lifecycle and permission to provision telemetry services. Don't install an SDK merely because the skill is active.

Read versions from `go.mod` and the selected exporters; verify upstream docs for those versions. Trace, metric and log signal API maturity may differ across repositories/releases. Do not assume a current example or extension works for every configured version.

## Trace and context ownership

- Identify the real operation boundary: inbound HTTP, outbound HTTP, background task, queue consumer, retry attempt or scheduled job. Use spans where their identity and parent relationship are meaningful.
- Propagate context through call stacks. If a worker detaches from a request, define an intentional new lifecycle and span link/relationship; don't pass canceled request contexts into long-lived tasks by habit.
- Avoid multiple overlapping instrumentations for the same HTTP client/server boundary: select one owning middleware/instrumentation layer.
- Do not invent business success based on an HTTP span success or a transport ACK. Distinguish transport latency, queue wait, downstream latency and business outcome.
- Errors, span attributes and body/header capture obey the **shared** project disclosure policy and destination authorization. A `full` application log policy does not automatically authorize copying full tokens into an external trace exporter.

## Metric dimensions and performance

- Define units (seconds vs milliseconds), monotonic counters vs up/down measurements and histogram boundaries based on the stated question.
- Avoid using unbounded keys such as UUID, raw token, request ID, URL query strings or customer input as metric label values. This is primarily a cardinality/cost issue even if full-value logging is allowed.
- Use low-cardinality dimensions that preserve the needed distinction: route template, method, bounded outcome, service component and operation type when useful.
- Document sampling, aggregation windows, dropped datapoints and how absent measurements affect interpretation. A zero is not automatically equivalent to a missing observation.

## Lifecycle and ownership

The composition root owns meter/tracer providers, any exporters and their `Shutdown`/`ForceFlush` lifecycle when supported by the selected SDK. Flush with a bounded separate shutdown context after request/workers stop; report error or dropped telemetry without blocking shutdown indefinitely.

A local CLI might only need `slog` on stderr and no long-lived metrics provider. Any collector, HTTP endpoint or background exporter requires separate project selection and execution authorization. Never expose `net/http/pprof` as a public side effect of enabling observability.

