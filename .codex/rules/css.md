# CSS rules

Apply this rule when `CODEX_PROJECT.md` declares plain CSS active or when the task directly changes CSS files, CSS Modules, global styles, component styles, or Vue SFC style blocks.

This rule is standalone and does not require another styling overlay to be present.

## Required skills

Use together with:

- `css-expert`.

## Source of truth

Before changing CSS:

1. Read `CODEX_PROJECT.md`.
2. Check CSS entry points, browser support policy, CSS build pipeline, PostCSS/autoprefixer policy, CSS Modules policy, formatter/linter policy, and validation commands.
3. Inspect existing CSS architecture: tokens, base/reset, layout, components, utilities, overrides, CSS Modules, and local component styles.
4. If Vue is active, inspect affected `.vue` style blocks, SFC style scope, CSS `v-bind()` usage, deep/global selector usage, and the project policy for local vs shared styles.
5. Load `css-expert/references/patterns-and-review.md` and `css-expert/references/browser-and-architecture.md` when adding or changing architecture, cascade layers, custom properties, responsive rules, browser fallbacks, form-control styles, scrollbars, modern selectors, container queries, nesting, or Vue SFC style blocks.
6. Follow project-declared commands.

## Constraints

- Use plain CSS for project-owned styling, shared design-token wiring, global base styles, layout primitives, component styles, and narrow utilities.
- Prefer existing CSS custom properties, tokens, layers, layout primitives, component classes, and utility classes before adding new ones.
- Use CSS custom properties for runtime theme values and reusable design tokens.
- Keep selectors low-specificity; prefer class selectors and `:where()` for intentionally zero-specificity wrappers.
- Avoid IDs for styling, deep descendant chains, and broad element selectors outside reset/base layers.
- Keep nesting shallow and avoid nesting that hides compiled selector specificity.
- Use `@supports` for progressive enhancement when a modern feature needs a fallback.
- Use the CSS `transform` property and transform functions such as `translate()`, `rotate()`, `scale()`, and `skew()` normally when they fit the styling requirement and browser support policy.
- Do not assume build-time CSS transformations, transpilation, polyfills, or automatic vendor prefixing unless the project build pipeline explicitly provides them.
- Do not hand-write vendor prefixes unless project policy or browser support requirements demand it.
- Treat form controls, scrollbars, viewport units, font rendering, and mobile browser behavior as cross-browser risk areas.
- Check `@layer`, `@scope`, `@property`, container queries, anchor positioning, `:has()`, `:is()`, and native nesting against browser support policy before relying on them.
- Preserve focus-visible, disabled, hover, active, forced-colors, and color-scheme behavior where relevant.
- In Vue SFCs, use local style blocks only for component-owned styling; keep reusable tokens, base styles, layout primitives, and shared utilities in project-declared shared CSS files.

## Vue usage when active

- Use CSS in Vue only when the `vue3-typescript-vite` profile and CSS profile are both active, or when an existing Vue SFC already uses local CSS.
- Put local component-only styles in `.vue` style blocks.
- Use `scoped` only when component-local scoping is intended.
- Use CSS Modules only when the project convention already uses them or the task explicitly asks for module-scoped class names.
- Use Vue SFC `v-bind()` in CSS only for dynamic component state that must drive CSS values.
- Use deep/global selectors narrowly and document why local scoping is not enough.
