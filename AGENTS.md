# Agent Bootstrap — gost-restreamer

## Mandatory Request Routing

Before any task-specific tool call, edit, MCP operation, external-system call or final answer:

1. Read and apply `.codex/rules/request_routing.md`.
2. Resolve the current user request, allowed surfaces and side effects.
3. Read this file and `CODEX_PROJECT.md`. This repository consumes agent-projects-tools in target-project mode.
4. Read every applicable active rule/skill entrypoint before using it. Validate declared hard dependencies through `.codex/rules/material_dependencies.md`, including the complete transitive closure.
5. Act only within the user-authorized scope and all applicable constraints.

All materials selected in `CODEX_PROJECT.md` are active by default, including before application code exists. Do not ask the user to select those layers again. Applicability still depends on the task; activation does not authorize unrelated work, dependency installation or external mutations. Missing runtime versions are verification gaps, not disabled profiles. Never omit or disable request routing.

## Evidence And Critical Review

- Base conclusions on verified facts. Do not invent project behavior, versions, requirements, execution results or approvals.
- Use search only to locate candidate files. Before drawing conclusions, manually read each relevant file in full, including relevant consumers and configuration; snippets and pattern matches are insufficient.
- Study relevant files in detail. Distinguish confirmed source behavior, observed execution and unresolved facts.
- After exploring an idea or implementation, actively try to refute it: inspect counterexamples, failure paths and simpler alternatives. Resolve supported objections before recommending or accepting a solution.
- Think deeply and double-check conclusions for every user command.
- Always locate and read the root `AGENTS.md`; reread it, request routing and the applicable project profile after context compression or resumption.
- Use macOS-compatible terminal commands in the development workspace. Deployment commands target Debian only when the task explicitly concerns that environment.

## Subagents For Executable Code And Tests

Read `.codex/rules/subagent_coordination.md` before any delegation. It is mandatory bootstrap support, together with the selected `.codex/agents/*.toml` files and the Agent Roles And Model Selection policy in `CODEX_PROJECT.md`.

- Delegate authorized executable application/test-code work only. Documentation-only, comment-only, docstring-only and instruction-only tasks stay with the primary.
- Select relevant roles; do not launch all roles automatically. Every assignment uses a fresh no-history thread (`fork_turns: "none"` on verified MultiAgentV2), never a completed thread or `followup_task`.
- The primary explicitly sets a scope-specific Architecture Gate: `NOT_REQUIRED`, or `PENDING` when both architects are appointed. Closed gates block affected downstream planning, code/test changes and acceptance for the primary and subagents.
- Both architects must independently endorse the identical canonical final candidate; only the primary can approve the versioned Architecture Handoff. Preserve independent proposals and evidence-based peer critique.
- Screen data-impacting work for the selected `database_architect`. A required assessment must be `CLEAR` for the current candidate/baseline before affected downstream work. Track `db_assessment_status` separately from Architecture Gate.
- Verify a safe pause and partial writes when gates close. Distinguish implementation defects from proven defects in the design.
- Assign disjoint write ownership, preserve unrelated work and verify a stable snapshot for final checks. Required independent testing/review and project checks cannot be waived by passing self-tests.
- Use `local-ephemeral` handoff persistence as declared in `CODEX_PROJECT.md`: only the primary saves verified completed reports under `.agent-handoffs/<task_scope_id>/` when the current request permits those writes, following `.codex/rules/subagent_coordination.md`. Otherwise keep reports in-session. No automatic task notes, Obsidian writes, worktrees, commits or hooks.
- After compression recover current authorization, scope, explicit gate decision, applicable handoff/assessment, active operations and verified evidence before resuming.

## Project Sources And Boundaries

`CODEX_PROJECT.md` declares the selected stack, language policy, roles, commands and runtime verification requirements. `gost-restreamer-architecture.md` is the architecture specification, not evidence of completed implementation.

Do not introduce unrelated layers or silently substitute unavailable roles/models. Report missing dependencies, unsupported versions and contradictory evidence. Do not install packages, mutate user databases or contact live streaming destinations merely to validate agent instructions.

Keep identifiers and contractual values unchanged; apply the declared Russian prose policy to project-authored comments and documentation. Preserve imported portable instructions in their original language.
