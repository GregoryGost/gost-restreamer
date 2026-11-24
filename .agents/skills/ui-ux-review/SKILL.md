---
name: ui-ux-review
description: "Use for UI/UX review of responsive layout, interaction states, accessibility, visual stability, and browser evidence."
---

# UI/UX Review

Use this skill when a task changes visible UI, user flows, responsive behavior, accessibility-sensitive markup, layout stability, or interaction states.

Apply `.codex/rules/ui_ux_validation.md` together with this skill.

Load references when needed:

- `references/patterns-and-review.md` for UI/UX review patterns and validation prompts.
- `references/official-sources.md` for accessibility, browser evidence, and UI review sources.

## Workflow

1. Read `CODEX_PROJECT.md`.
2. Confirm active framework, styling overlays, validation commands, browser targets, routes, and artifact policy.
3. Inspect changed UI code, related style/config files, and route/user-flow context.
4. Check layout, hierarchy, interaction states, accessibility-sensitive behavior, responsive behavior, and visible error/loading/empty states.
5. Apply active styling overlay checks only when those profiles are active: CSS, CSS animation, Tailwind, SCSS, or project-specific styling.
6. Use `playwright-ui-checks-mcp` when browser validation or visual evidence is active.
7. Report validation gaps when the app, route, backend, MCP server, or wait target is unavailable.

## Review Checklist

- [ ] Visible UI change is covered by a UI/UX review.
- [ ] Responsive behavior is checked for declared breakpoints or viewport policy.
- [ ] Focus, hover, active, disabled, loading, empty, success, and error states are considered when relevant.
- [ ] Semantic markup and accessible names are considered.
- [ ] Color, spacing, hierarchy, and density follow active design tokens or project styling conventions.
- [ ] Active styling overlays were applied only when active.
- [ ] Browser validation evidence is captured when project policy requires it.
- [ ] Remaining validation gaps are explicit.
