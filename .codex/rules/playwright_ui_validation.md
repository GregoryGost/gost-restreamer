# Playwright UI validation rules

Apply this rule only when `CODEX_PROJECT.md` declares Playwright MCP or browser-based UI validation active, or when the task directly asks for Playwright-based UI checks.

This rule covers local UI validation and evidence gathering through Playwright MCP. It does not define a persistent Playwright Test E2E suite.

## Required skills

Use together with:

- `playwright-ui-checks-mcp`.

## Source of truth

Before running UI validation:

1. Read `CODEX_PROJECT.md`.
2. Check the active dev server URL, required startup commands, MCP server name, artifact path, target routes, and wait targets.
3. Confirm that the web app and required backend/MCP services are running or report what is unavailable.
4. Use the smallest route set that validates the changed behavior.

## Browser and server lifecycle

- Before a check, distinguish the browser/session used through Playwright MCP, an existing or task-started dev server, and the MCP server itself. A reachable browser or MCP server is not automatically owned by this agent.
- After collecting evidence, close a browser/session owned exclusively by this check through the currently exposed Playwright MCP close tool (for example `browser_close`, when available). Preserve a shared/borrowed browser session and report if exclusive ownership or a safe close operation cannot be established.
- Stop a dev server only if this check started it, still controls it exclusively, and the project's supported shutdown path is available; never terminate an existing/shared server by port or process name.
- A browser close does not terminate the client-managed MCP server. Do not kill/reconfigure the MCP host or infer its shutdown from a completed tool call. Verify observable cleanup and report any unresolved owned resources, including after errors or interruption.

## Constraints

- Do not run Playwright MCP checks against hardcoded URLs.
- Use browser snapshot and screenshot checks when validating UI changes.
- Summarize console errors and failed network requests when available.
- Use specific wait targets instead of blind long waits.
- Do not create or rely on project-local artifact paths unless they are declared by the project.
- Do not replace persistent E2E suites with screenshots or one-off MCP checks.
- Report unavailable apps, routes, backends, MCP servers, or wait targets as validation gaps.
