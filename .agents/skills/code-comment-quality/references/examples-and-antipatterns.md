# Worked Contrasts: What a Maintainer Actually Needs to Know

**Illustrative examples, not executable project code or universally correct contracts.** Assume the stated behavior exists in each hypothetical component; before using a similar comment, verify it against the target's complete source, tests, callers and dependency contracts. The snippets use several languages only to expose different engineering situations. Translate natural-language prose to the project's selected language and use its doc-comment syntax. Each lesson concerns **semantic information**, not length. The comments should sound like something a developer would explain to a teammate, not like an agent's summary or scoring rubric. Technical terms are welcome where useful, but unexplained abstractions are not.

## 1. Obvious behavior: no comment is better (TypeScript)

**Unhelpful:**

~~~ts
// Increase the retry counter by one.
retryCount += 1
~~~

**Better:**

~~~ts
retryCount += 1
~~~

**Why:** the comment contributes no additional fact. An invented rationale would be worse. A short function can be sufficiently self-documenting.

## 2. Module role and shared invariants (Python)

**Insufficient:**

~~~python
"""Event delivery helpers."""
~~~

**Stronger when this describes the actual module:**

~~~python
"""Coordinate ordered delivery of stored events to subscribers.

The store owns event identity and persistence; this module owns per-subscriber
delivery cursors. A cursor advances only after the subscriber acknowledges an
event, so a disconnected subscriber may receive the last unacknowledged event
again. Consumers must therefore tolerate duplicate delivery.

This module never deletes events and cannot ensure that the receiver processes each event only once.
"""
~~~

**Why:** this establishes ownership, important observable behavior, and a constraint across multiple functions. It is not merely a longer title.

## 3. Public API: exceptional and boundary contract (Python)

**Insufficient:**

~~~python
def read_page(limit: int, cursor: str | None) -> Page:
    """Read events."""
    ...
~~~

**Stronger for a hypothetical API that enforces these rules:**

~~~python
def read_page(limit: int, cursor: str | None) -> Page:
    """Return up to `limit` events after `cursor` in ascending ID order.

    A missing cursor starts at the oldest retained event. A valid cursor is
    exclusive: the event identified by it is not repeated in this page.

    Args:
        limit: Number of events to request, in the inclusive range 1..500.
        cursor: Previously returned cursor, or None for the first page.

    Returns:
        Page with events and a next cursor; a missing next cursor means
        no additional retained events were available at read time.

    Raises:
        ValueError: If limit or cursor format is invalid.
    """
    ...
~~~

**Why:** inputs, pagination semantics, boundary values, and errors are actionable to callers. Do not promise stable snapshots or atomic reads unless the implementation provides them.

## 4. State machine: description versus lifecycle (Go)

**Insufficient:** `// Task state: pending, running, done, cancelled.`

**Better, but still incomplete:** `// Tasks start pending and end done or cancelled.`

**Stronger for a fictional task scheduler:**

~~~go
// Task lifecycle:
//
//     pending --> running --> done
//        |           |
//        +-----------+-----> cancelled
//
// Only the scheduler starts a pending task. A worker can finish just as
// another goroutine requests cancellation. Both updates use the same lock,
// so the task ends as either done or cancelled, never both.
//
// After completion or cancellation, callbacks can read the result, but
// cannot restart the task or change its final state.
~~~

**Why:** the diagram describes reachable transitions, and the prose names the actor, race, winner, and terminal invariant. Verify that the implementation really enforces a single terminal transition.

## 5. Concurrency: why a guard matters (Go)

**Insufficient:**

~~~go
// Lock to avoid races.
mu.Lock()
defer mu.Unlock()
~~~

**Stronger when these facts are proven:**

~~~go
// A lease can be renewed while an expiry callback from its previous
// version is still running. We check the version under the lock so the
// old callback cannot cancel the renewed lease.
mu.Lock()
defer mu.Unlock()
if expiringVersion != lease.version {
    return
}
lease.expired = true
~~~

**Why:** “avoid races” describes an abstract goal, while the improved version exposes the actual race and the protected invariant. It must agree with the code's lock coverage.

## 6. Qt/C++ asynchronous lifetime and cancellation

**Insufficient:**

~~~cpp
// Clean up the network response.
reply->deleteLater();
~~~

**Stronger for a finished handler with a `QPointer<Controller> controller`:**

~~~cpp
// The controller may have been destroyed before the reply finishes.
// QPointer becomes null when that happens, so check it before passing
// the data on. Read the reply before scheduling its deletion; do not
// keep the reply after this handler returns.
if (controller) {
    controller->accept(reply->readAll());
}
reply->deleteLater();
~~~

**Why:** the comment says what can go wrong during shutdown and why the pointer check and cleanup order matter. This is illustrative: the full handler must still be checked for correct reply and receiver lifetimes.

## 7. HTTP retry and ambiguous completion (TypeScript)

**Insufficient:** `// Retry the request when the connection fails.`

**Stronger for an implementation that retries only declared-safe operations:**

~~~ts
// The connection can fail after the server has already saved the change.
// Sending the same request again might apply it twice. Retry only operations
// that the endpoint explicitly allows us to repeat; otherwise the caller
// must check what happened before trying again.
~~~

**Why:** this covers the causality, scope, and consequence of retries. “All POSTs are unsafe” or “GET is always harmless” would be an unjustified universal claim.

## 8. Cancellation and partial side effects (C# XML documentation)

**Insufficient:**

~~~csharp
/// <summary>Sends the batch.</summary>
Task SendAsync(Batch batch, CancellationToken cancellationToken);
~~~

**Stronger for a client that can stop locally after server acceptance:**

~~~csharp
/// <summary>Submits a batch and waits for the server acknowledgement.</summary>
/// <param name="batch">The batch to submit; its contents are not modified.</param>
/// <param name="cancellationToken">
/// Requests cancellation of local waiting and transport activity.
/// </param>
/// <remarks>
/// Cancellation does not roll back a batch already accepted by the server.
/// If the acknowledgement is lost, the caller must check whether the
/// server accepted the batch before sending it again.
/// </remarks>
Task SendAsync(Batch batch, CancellationToken cancellationToken);
~~~

**Why:** this distinguishes local cancellation from remote rollback. Do not invent exception types or idempotency rules missing from the real contract.

## 9. Owned versus borrowed data (C++)

**Insufficient:** `// Store bytes for later.`

**Stronger for a hypothetical non-owning input API:**

~~~cpp
// The parser borrows [data, data + size) only for this call and never saves
// the pointer. The caller retains ownership and may release or reuse the
// buffer immediately after parse() returns. Any deferred parsing must first
// copy the bytes into parser-owned storage.
~~~

**Why:** ownership and lifetime are otherwise hard to infer from a raw-pointer signature. Do not make this guarantee if the implementation queues work that refers to the original memory.

## 10. Justifying a technical decision without fictional history (Go)

**Insufficient:** `// Use two queues because it is faster.`

**Stronger if the actual synchronization contract confirms it:**

~~~go
// Timer callbacks have their own limited queue. Ordinary work waiting
// in another queue cannot prevent an expired timer from being queued.
// This does not guarantee that its callback runs immediately or exactly
// in the order timers expired.
~~~

**Why:** it describes the precise purpose and the limit of the guarantee. Avoid claims about benchmarks, speedups, or decisions by previous authors without evidence.

## 11. Ordering and persistence boundary (SQL)

**Insufficient:** `-- Insert a row before sending the notification.`

**Stronger for an actual transactional outbox:**

~~~sql
-- Store the event in the same transaction as the business change.
-- Publishing it before commit could tell other services about a change
-- that is later rolled back. The sender may retry and deliver the event
-- twice, so receivers must detect events they have already handled.
~~~

**Why:** this ties the ordering to a real failure mode and explains the delivery consequence. Only use when the schema/dispatcher behavior supports the outbox contract.

## 12. Test intent and regression protection (Python)

**Insufficient:** `# Call cancel and assert that state is cancelled.`

**Stronger where the test actually exercises the race:**

~~~python
# A timeout from an old lease may arrive after that lease is renewed.
# The old timeout must not cancel the renewed lease or send a second
# cancellation notification.
~~~

**Why:** it explains which defect the test detects. Ensure synchronization in the test actually controls this interleaving; a comment cannot make a nondeterministic test reliable.

## 13. Stale or speculative explanation: correct or remove

**Misleading after implementation changes:**

~~~ts
// Exactly once: successful sends are never retried.
await transport.send(payload)
~~~

**Better:** confirm what `send` and the surrounding caller actually guarantee. If retries or ambiguous outcomes are possible, replace the false promise with a verified account of commit/replay semantics. If no non-obvious invariant remains, **remove the comment** rather than replacing it with another guess.

**Why:** elaborate but false documentation is worse than a missing comment.

## 14. Excess narration versus one high-value constraint

**Overdocumented:**

~~~go
// Read the flag.
enabled := config.Enabled
// Check the flag.
if enabled {
    // Start the worker.
    startWorker()
}
~~~

**Better where a real startup invariant applies:**

~~~go
routing := buildRoutingTable(config)
publishRoutingTable(routing)
// Workers must start after publication of the complete routing table;
// starting earlier would allow a worker to read partially updated routes.
if config.Enabled {
    startWorker()
}
~~~

**Why:** one fact explaining an ordering dependency can replace a series of obvious comments. If startup ordering is not enforced here, that improved comment would also be misleading.

---

## 15. Human explanation instead of an agent-style abstraction (TypeScript)

**Formally technical, but difficult to read:**

~~~ts
// Enforce request exclusivity invariant to prevent stale asynchronous
// response propagation across overlapping execution boundaries.
if (requestInFlight) return
~~~

**Clearer, when this guard really prevents overlapping requests:**

~~~ts
// Don't start a second request while the previous one is still running.
// Otherwise the older response could arrive last and overwrite newer data.
if (requestInFlight) return
~~~

**Why:** both comments point toward the same risk, but the second names the real situation and consequence. The goal is not to avoid the terms "async", "invariant" or "race" everywhere. Use them when they genuinely help the intended developers, and explain an unfamiliar mechanism rather than decorating it with jargon. Do not paste an instruction such as "Review the contract" or a generated What/Why/Risk template into source comments.

---

## Calibrate by consequence, not size

A single sentence may completely explain a local invariant. A public boundary with cancellation, replay, persistence, and concurrent state can need several paragraphs plus a transition table. A long comment is not evidence of quality: ask which supported fact would be lost if each sentence were removed, and which important fact is still missing. Do not retrofit every example into one component.
