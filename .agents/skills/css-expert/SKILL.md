---
name: css-expert
description: "Use for CSS architecture, browser compatibility, responsive styling, accessibility states, and Vue SFC styles."
---

# CSS Expert

Use this skill when plain CSS is active in `CODEX_PROJECT.md` or when the task directly touches CSS files, CSS Modules, global styles, component styles, or Vue SFC style blocks.

This skill is standalone and does not require another styling overlay to be present.

Apply `.codex/rules/css.md` together with this skill.

Load references when needed:

- `references/patterns-and-review.md` for CSS good/bad patterns, cross-browser guardrails, and review prompts.
- `references/browser-and-architecture.md` for modern CSS feature guardrails and architecture review.
- `references/official-sources.md` for official CSS, browser-compatibility, accessibility, and Vue SFC style references.

## Workflow

1. Read `CODEX_PROJECT.md`.
2. Confirm CSS entry points, browser support policy, CSS build pipeline, PostCSS/autoprefixer policy, CSS Modules policy, formatter/linter policy, and validation commands.
3. Inspect existing CSS structure before editing: tokens, base/reset, layout, components, utilities, overrides, CSS Modules, and Vue SFC style blocks when present.
4. Identify the smallest styling layer that solves the task: token, base rule, layout rule, component rule, state rule, utility rule, or local SFC style.
5. Compare the intended change against `references/patterns-and-review.md` and `references/browser-and-architecture.md`.
6. Check browser support and fallback requirements for modern features, especially `@layer`, `@scope`, `@property`, container queries, nesting, `:has()`, subgrid, viewport units, color functions, anchor positioning, form-control styling, and scrollbars.
7. Preserve responsive behavior, accessibility states, color-scheme behavior, and forced-colors behavior when relevant.
8. Run or report project-declared validation commands when applicable.

## Guardrails

- Prefer project-owned CSS architecture over ad hoc global rules.
- Use CSS custom properties for runtime design tokens and theme values.
- Use cascade layers only when the project already uses them or the change introduces a clear layer contract.
- Keep selectors low-specificity and predictable; avoid IDs for styling.
- Keep nesting shallow and intentional.
- Do not assume CSS polyfills or prefixes exist; check the project build pipeline and browser support policy.
- Use feature queries and progressive enhancement for optional modern features.
- Prefer logical properties when supported by the project browser policy.
- Avoid styling that relies on identical form-control, scrollbar, font-rendering, or viewport behavior across all browsers.
- In Vue projects, use CSS in SFC style blocks according to project policy and keep reusable styles in project-declared shared CSS files.

## Review Checklist

- [ ] CSS profile, entry points, and browser support policy were checked.
- [ ] CSS build-pipeline assumptions were verified instead of guessed.
- [ ] Styling layer is correct: token, base, layout, component, utility, override, CSS Module, or SFC-local style.
- [ ] Selectors remain low-specificity and maintainable.
- [ ] Modern CSS features have browser support or fallback strategy.
- [ ] Responsive, theme, and accessibility states remain coherent.
- [ ] Vue SFC style scope was checked when `.vue` files are affected.
- [ ] No unrelated styling layer was changed.
