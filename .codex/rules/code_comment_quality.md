# Code Comment Quality

Apply this cross-language rule only when it is explicitly active in the target project's `CODEX_PROJECT.md`. It applies to authorized creation, modification, and review of source code, tests, code comments, docstrings, and API documentation embedded in source. It does not authorize unrelated documentation edits or a repository-wide comment rewrite.

## Required skills

- `code-comment-quality`

This rule and `code-comment-quality` intentionally declare a **mutual rule ↔ skill dependency**. Both must be independently active; validate the pair with `.codex/rules/material_dependencies.md` before use. This rule does not activate the skill by reference.

## Goal: semantic coverage, not comment density

Documentation is sufficient when an informed maintainer can recover the **material behavior, constraints, and rationale** for the affected component from the code and its relevant nearby documentation, without guessing at the original author's intent.

- Document material knowledge that is difficult or unsafe to reconstruct from names and ordinary control flow alone.
- Cover relevant **purpose and role**, **observable contract**, **state transitions**, **invariants**, **algorithmic reasoning**, **ownership and lifecycle**, **concurrency**, **integration boundaries**, **failure behavior**, and **surprising side effects**. These are *applicability categories*, not mandatory sections in every comment.
- Explain the reasoning behind non-obvious choices and the specific failure or constraint they address. For a complex mechanism, document enough **how** to make the **why** and correctness conditions understandable.
- Prefer the right level: module/type documentation for overarching models, API documentation for caller-facing guarantees, and comments adjacent to delicate operations for local timing or ordering constraints.
- Use tables, small state-transition descriptions, examples, or multi-paragraph explanations when the mechanism warrants them. Do not impose a minimum length, comment percentage, or a rule that every function needs a comment.
- Omit comments that merely narrate obvious code. A complex subsystem may legitimately require several detailed explanations while a simple helper needs none.

## Human maintainer first

Write comments **for another developer who must understand, debug, or change this code**, not for an AI agent, an automated reviewer, or a documentation checklist. The content should sound like a colleague explaining a concrete behavior, not a generated analysis report.

- Prefer natural, direct sentences with identifiable actors and actions: what can happen, under which condition, why the code acts this way, and what would break if it changed. Concrete explanations beat labels such as "enforce lifecycle invariant" when those labels alone do not explain the behavior.
- Use accurate technical terms, identifiers, protocol names, and project vocabulary when they make an explanation **more precise**. Do not ban necessary terminology or oversimplify expert concepts. Briefly explain an unfamiliar term in context when the intended project's developers would otherwise have to guess.
- Avoid artificial "AI-style" prose: generic padding, repeated "responsible for" descriptions, abstract buzzword chains, rigid What/Why/How or Risk/Invariant/Decision sections, and self-assessment or instructions addressed to agents. A project-required documentation format (for example, `@param` or XML documentation tags) and a genuinely useful state diagram are exceptions, not defects.
- Make the prose readable in the project's chosen natural language. Do not copy internal task analysis, prompts, reviewer checklists, or English-language constructions into source comments. Use ordinary, grammatically natural language rather than a literal translation of reference examples.
- Explain a real mechanism at the detail necessary for safe maintenance; do not trade away important conditions or guarantees merely to shorten the text.

## Evidence, accuracy, and upkeep

- Ground each factual comment in the real implementation, callers, tests, project contracts, or verified platform/API behavior. Do not invent design history, alternative approaches that were never considered, concurrency guarantees, or external-service behavior.
- Distinguish guarantees from implementation details, assumptions, contingencies, and examples. When rationale is unknown, investigate within authorization or report the gap; do not manufacture an explanation.
- Keep comments synchronized with changed conditions, error cases, control flow, API semantics, and invariants. Remove or correct misleading documentation in the affected scope.
- Respect the project-selected natural language and language-specific syntax (Go doc comments, Python docstrings, C++/Qt documentation, JSDoc/TSDoc, XML docs, etc.). The separately active `comment-language-audit` checks prose language; it is neither a substitute for nor an automatic dependency of this pair.
- Avoid ticket IDs, internal planning labels, and temporary implementation-history commentary except where they are genuine product contracts; preserve `source_code_hygiene.md` when active.

## Change-scope boundary

For an authorized code change, assess new and materially affected existing comments in owned files and their directly affected contracts. Fix inaccurate or missing comments there as part of the same code change. Do **not** perform unrelated documentation-only maintenance, refactor code to satisfy a comment template, add new runtime behavior, or sweep untouched files. A separate comment-only audit requires its own explicit authorization.

## Reviewer gate

For the affected scope, ask:

1. Is a critical contract, precondition, failure case, state transition, ordering/ownership rule, or non-obvious decision undocumented?
2. Can the reader tell *why* a delicate mechanism exists, not merely *what* each statement does?
3. Could a maintainer make a plausible but unsafe change because an important constraint is missing or inaccurate?
4. Does each substantive assertion match observable code, project contracts, and validated dependencies?
5. Is the explanation positioned at the level where its consumers need it, with no repetitive narration?
6. Could another project developer understand the comment as ordinary, direct technical prose, without reconstructing the meaning of opaque labels, agent instructions, or artificial template phrases?

Report a **material omission, stale statement, or false guarantee** with exact file/line, evidence, consequence, and the smallest needed documentation correction. Do not file findings based on stylistic preference, raw comment count, or the fact that an example is longer than the project's comment. Use the skill's applicable patterns and contrastive examples before judging completeness.
