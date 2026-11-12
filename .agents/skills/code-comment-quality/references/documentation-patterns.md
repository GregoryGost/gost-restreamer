# Documentation Patterns: Technical Coverage Without Narration

These patterns are language-independent. Adapt syntax, naming, terminology, target natural language, and doc conventions to the active project. **Completeness is measured against real responsibilities and risks, never line count.**

## 1. Locate the explanation at the right level

| Level | Document when relevant | Do not |
| --- | --- | --- |
| File/package/module | The component's responsibility, entry points, external boundaries, shared invariants or protocol assumptions | Restate the import list or every public symbol |
| Class/type/subsystem | Ownership, lifecycle, cooperation between components, states, thread affinity, shared state | Explain each field twice when its name suffices |
| Public API | Inputs/preconditions, outputs, mutation/side effects, errors, cancellation, idempotency, guarantees, surprising edge cases | Promise behavior not established by code or contract |
| Complex private method | Algorithm's stages, correctness condition, invariants and reason for non-obvious branches | Translate code statements line by line |
| Delicate operation | Why the exact order, guard, lock, retry limit, or cleanup boundary matters | Record temporary design discussion or ticket history |
| Test | The scenario, important setup, asserted invariant and regression the test protects when not evident from its name | Assert the implementation algorithm in prose rather than expected behavior |

Avoid scattering one global contract over many methods: describe it once near its authoritative owner and cross-reference existing established names where useful.

## 2. Contract completeness: focus on caller-visible distinctions

Ask which aspects could change the correct use of the API:

- Null/empty values, valid ranges, units, defaults, sentinel values, and who owns or mutates arguments.
- Meaning and stability of return values; partial results; ordering; cardinality; persistence.
- Error types, cancellation and timeout semantics, early exits, and whether work may already have committed.
- Resource lifetime, thread/callback affinity, backpressure, and retry/idempotency guarantees.
- Version-/platform-specific assumptions only when verified for the actual project.

Document **only** applicable items. An internal helper need not restate a fully established public contract.

## 3. State machines and long-running coordination

When behavior depends on states, show the **reachable model**, not only a list of constants:

1. Identify the owner of state and whether state is local, shared, persisted, or externally observed.
2. Specify triggering events and allowed transitions, including failure and interruption paths where they matter.
3. State terminal states, forbidden transitions, and invariants that guard correctness.
4. Explain timing/race windows and the mechanism ensuring an old event cannot affect a new operation.
5. Distinguish a conceptual diagram from actual runtime values if there are intermediate/transient states.

Use an ASCII transition table/diagram when it is clearer than prose, as in complex schedulers; do not copy a particular upstream implementation's states into an unrelated project.

## 4. Concurrency, async lifecycle, and resource ownership

A useful explanation connects a specific hazard to the protection used:

- **Concurrency:** Who may access state concurrently? Which lock, atomic operation or event-loop confinement establishes the guarantee? What must be done under the lock? What race would omission cause?
- **Async:** Who schedules and cancels work? Can a callback arrive during teardown? What does cancellation guarantee, and what *doesn't* it guarantee?
- **Ownership:** Who allocates, borrows and releases a resource? Can a borrowed view outlive the owner? What happens on exceptional exits?
- **Ordering:** Why must A precede B? Is it a publication barrier, transaction/commit boundary, or protocol constraint?
- **Failure:** What can be retried safely, how can an ambiguous remote outcome arise, and which resource remains live after failure?

Do not claim a particular memory-order, cancellation guarantee, or event-loop behavior unless the target's implementation or verified platform contract supports it.

## 5. Integration and non-obvious choices

Document the verified boundary, such as:

- What the dependency is responsible for versus what this component must enforce.
- An external protocol or serialization assumption, including units and compatibility.
- Why an apparently simpler option violates a real requirement or invariant.
- What happens on a transient fault, a partial response, or an already-committed remote write.

A comment can explain a *known technical constraint* without inventing historical intent: say “This order prevents ...” only when the causal relation is verified. Do not claim “We chose ... over ... after testing” without actual evidence.

## 6. Deciding depth and reviewing coverage

Use an evidence-based triage:

- **No new comment:** names, types and short control flow already communicate the necessary meaning.
- **One or two sentences:** one localized invariant or non-obvious reason needs explanation.
- **Contract block:** several caller-visible cases, return/error rules, or lifecycle guarantees need to be kept together.
- **Subsystem explanation:** cross-method state transitions, concurrency and failure handling cannot be understood safely from local comments alone.

The longest category is not automatically the best. Expand until the *material knowledge gap* is closed, then stop.

During review, build a short mental map of the affected component's **contracts / invariants / unusual mechanisms / failure paths**. Compare it with the existing explanations. Report omissions only when they carry a concrete comprehension or change-safety risk; distinguish real defects from formatting preferences. Validate each statement against source and contracts, not against another agent's prose.

## 7. A developer should understand it on first reading

The patterns and review questions in this document are **instructions for the author**, not headings or wording to paste into actual source comments.

- Write like a colleague explaining a concrete problem: "The old response can arrive last and replace newer data" rather than "preserve asynchronous ordering invariants".
- Prefer specific nouns and verbs over stacked abstractions. Name the worker, request, lock, database row, or callback when the code makes it identifiable.
- Give context for a specialist term if a teammate working on this project may not know it; preserve established terms such as `commit`, `mutex`, `QPointer`, `context.Context` and exact API names when they are the clearest description.
- Keep paragraphs and lists only when they improve navigation. Do not stamp every comment with "Context / Contract / Invariants / Risks", introduce your own grading scale, address an AI assistant, or embed a reviewer task list in production source.
- Don't turn source comments into beginner tutorials for standard language features. Readers are developers, not necessarily nontechnical users. The desired result is clarity **without losing precision**.
- In the project's selected language, use phrases a teammate would naturally write; translate meaning, not the English syntax of examples.

**Human-reader check:** after reading the comment alone, can a developer say what might go wrong and why the code prevents it, in plain technical language? If not, simplify the *wording* or add the missing concrete cause, rather than deleting necessary facts.

## 8. Maintenance and change discipline

A behavior change may invalidate a comment that is physically outside the modified lines. Inspect directly dependent comments and examples within the permitted scope. Correct or remove stale promises. Do not initiate a repository-wide documentation campaign, generate speculative explanations, or alter executable behavior merely to make a comment sound complete.

Reference inspiration can come from high-quality open-source code, but **extract transferable reasoning patterns rather than importing a particular class's documentation layout, implementation-specific transitions, or code text**.
