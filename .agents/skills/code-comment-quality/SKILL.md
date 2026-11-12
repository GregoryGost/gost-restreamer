---
name: code-comment-quality
description: "Use for human-readable, evidence-based code comments and docstrings with complete contracts, clear rationale, and maintainability review across languages."
---

# Code Comment Quality

## Required Dependencies

- `.codex/rules/code_comment_quality.md`

This is an intentional **mutual rule ↔ skill dependency**: the rule requires this skill, and the skill requires the rule. Both must be independently active through the target project's `CODEX_PROJECT.md`, with `material_dependencies.md` validation. Presence of this package or a comment-related request does not activate the pair.

## Purpose and boundaries

Use this skill to decide **what technical information must be documented**, **where**, and **to what depth**, then explain it in language meant for a human developer. This is not a prose-language checker, formatter, new documentation generator, mandatory API-doc overhaul, or authorization to modify unrelated source.

- Follow the active rule's change-scope and evidence boundaries, existing project convention, and the active language/framework skill for syntax and official doc tags.
- For grammatical correctness and target-language consistency, use `comment-language-audit` only if it is independently active.
- Do not copy the examples into a real project verbatim. Each worked example is illustrative; its claims must be independently checked against the actual target implementation.

## Task-specific reference routing

Read the relevant sections of **both** references when new or revised documentation is material to the assigned code or review. Do not load every example for an obvious one-line change.

| Need | Reference sections |
| --- | --- |
| Choose documentation location and depth | `references/documentation-patterns.md`: levels and coverage triage; `references/examples-and-antipatterns.md`: 1–3 and 15 (human-readable wording) |
| API and error/side-effect contracts | Patterns: contracts/boundaries; Examples: 3, 8, 9 |
| State machine, ordering, concurrency | Patterns: states and concurrency; Examples: 4–6 |
| Async lifecycle, network integration, retries | Patterns: lifecycle/integrations; Examples: 6–8 |
| Resource ownership, architectural trade-offs | Patterns: ownership and decisions; Examples: 2, 9–11 |
| Test intent, misleading/stale/verbose comments | Patterns: validation and maintenance; Examples: 1, 12–14 |
| Human-readable prose versus agent-oriented jargon | Patterns: human-reader check; Examples: 15 |

For a genuinely complex component with multiple relevant dimensions, inspect more than one worked contrast. The references teach reasoning patterns, not required comment layouts.

## Authoring workflow

1. **Inspect actual behavior.** Read the complete affected function/type and relevant callers, state owners, tests, configuration, and applicable external contracts. Search snippets alone are not proof.
2. **Map the knowledge gap.** Identify what an experienced new maintainer could misunderstand: external contract, exceptional path, terminal state, race, ownership rule, timeout, ordering constraint, or a non-obvious trade-off. Mark categories that do not apply; do not fill empty slots with invented explanations.
3. **Choose level and examples.** Use module/type docs for shared architecture, public-doc conventions for caller-facing contracts, and local comments for fragile implementation steps. Read the appropriate contrastive example(s) to calibrate completeness.
4. **Draft for a human reader.** Explain the actual situation in ordinary, precise sentences: name the actor or event, what happens, and why it matters. Add necessary conditions, constraints, and consequences; use exact technical terms where useful, but do not replace an explanation with labels such as "ownership boundary" or "invariant enforcement". Avoid chatbot-style framing, repetitive stock phrases, process instructions, and mechanical section templates inside source comments. A diagram or multi-paragraph comment is appropriate when it genuinely helps. Never present hypotheses as guarantees or invent design history.
5. **Cross-check the implementation.** Trace every assertion to current code, tests, project specification, or verified API documentation. Compare normal, edge, failure, cancellation, and concurrent paths as applicable. Amend the code or comment only within the authorized scope; otherwise raise the discrepancy.
6. **Read as an independent maintainer.** Ask whether an unsafe but superficially reasonable edit is still possible because a material rule is implicit. Then read the comment aloud as if explaining the code to a teammate: would a real developer understand the causal story without translating abstract buzzwords? Rewrite stiff, machine-like or overly formal prose while keeping every necessary technical fact. Remove redundant narration. Preserve project language, formatting, and doc tooling.

## Review and handoff

- Review **missing explanations as well as existing text**, concentrating on new/changed behavior and directly impacted documentation. Treat prose that hides its meaning behind unexplained jargon or agent-oriented structure as a readability problem only when it creates a real comprehension cost; do not reject valid specialist terminology on stylistic grounds.
- Classify each finding as missing contract, insufficient rationale, missing state/ordering/ownership invariant, stale/inaccurate statement, or redundant/misleading narration.
- Give file/line, the verified missing or false claim, concrete maintenance risk, and the smallest source-adjacent correction. If behavior is unknowable from available evidence, report uncertainty rather than write a speculative comment.
- A comment-only PR is not a code implementation; do not delegate it to subagents when the active project delegation contract prohibits comment-only tasks.
- Do not require fixed counts, templates on every method, repository-wide rewrites, new tooling, or tests solely for comments. Respect any existing mandatory doc tooling when independently active.
