---
name: playwright-ui-checks-mcp
description: "Use for local UI validation through Playwright MCP with navigation, accessibility, screenshots, console/network evidence, and reports."
---

# Playwright UI Checks MCP

Use this skill when `CODEX_PROJECT.md` declares Playwright MCP or browser UI validation active, or when the user explicitly asks for Playwright-based UI checks.

This skill is for local UI validation and evidence gathering. It is not a persistent Playwright Test E2E suite.

Apply `.codex/rules/playwright_ui_validation.md` together with this skill.

## Workflow

1. Read `CODEX_PROJECT.md`.
2. Confirm MCP server name, dev server URL, target routes, wait targets, and artifact path.
3. Confirm the web app and required backend/MCP services are available, or report what is unavailable.
4. Navigate only to routes relevant to the changed behavior.
5. Wait for a specific element or state.
6. Capture an accessibility snapshot when available.
7. Capture screenshots when visible UI changed or the task asks for evidence.
8. Summarize console errors and failed network requests when available.
9. After evidence collection, use the exposed MCP browser-close operation only for the session exclusively owned by this check; never close another agent's or user's browser.
10. Stop a dev server started for this check only if it remains exclusively controlled, using its approved shutdown path, and verify observable completion. Leave borrowed servers and MCP-client-managed processes running.
11. Report what was validated, what remains unvalidated, and whether owned resources were released or cleanup remains unverified.

## Guardrails

- Use project-declared URL and routes; do not hardcode local defaults.
- Use project-declared artifact paths.
- Do not retry indefinitely; report unavailable apps, failed services, or missing wait targets.
- Do not treat screenshots as the only validation signal.
- Do not replace persistent E2E tests with MCP screenshots.
- Do not create project files or test artifacts unless the project declares that workflow.
- Browser close and MCP-server shutdown are distinct. If a close operation is unavailable or ownership is ambiguous, report the remaining resource instead of guessing a PID or killing by port.

## Report Format

Include route checked, wait target, visible result summary, accessibility snapshot summary when available, console/network errors, screenshots or artifact references when available, remaining validation gaps, and owned-resource cleanup status.
