# CSS Browser And Architecture Review

Use this reference together with `patterns-and-review.md` for CSS architecture and browser compatibility review.

## File Ownership

Use existing project conventions first. For a new plain-CSS structure, keep files by responsibility:

- entry point: imports and layer order;
- tokens: custom properties and reusable design values;
- base: reset and element defaults;
- layout: containers, page shells, grids, stacks, and section spacing;
- components: reusable component classes;
- utilities: narrow reusable helpers;
- overrides: documented integration fixes.

## Selector Rules

- Prefer class selectors for component styling.
- Avoid ID selectors for styling.
- Keep descendant chains short.
- Use `:where()` for low-specificity wrappers.
- Use `:is()` to group selector alternatives without duplicating rule bodies.
- Use `:has()` only when browser policy supports it or when the selector is progressive enhancement.
- Use `@scope` only when browser policy supports it or when scoped rules are optional enhancement.
- Use native nesting only when supported by the project browser policy or build pipeline.
- Review the resulting selector, not just the nested source.

## Browser Review Areas

Check these areas when touched:

- form controls and native UI elements;
- scrollbars and overflow;
- mobile viewport height;
- font rendering and line-height;
- flex and grid overflow;
- logical properties and writing modes;
- hover and pointer media queries;
- forced-colors preferences;
- color-scheme behavior.

## Modern CSS Feature Guardrails

Use a baseline-first approach for:

- cascade layers;
- `@scope`;
- `@property` registered custom properties;
- container queries;
- native nesting;
- parent-aware selectors;
- subgrid;
- new viewport units;
- modern color functions;
- anchor positioning.

If the feature is not covered by project browser policy, add a fallback or use `@supports` when possible.

## Feature-Specific Review

- `@layer`: define layer order in one entry point and avoid scattering layer order across files.
- `@scope`: verify scoping roots and limits; do not use it to hide unclear component boundaries.
- `@property`: declare syntax, inheritance, and initial value for typed custom properties.
- Container queries: keep a baseline layout outside the container query.
- Anchor positioning: keep a baseline absolute/fixed position before anchor positioning rules.
- `:has()`: avoid expensive broad selectors and prefer component-local scope.
- Native nesting: keep nesting shallow and review compiled specificity.

## Vue SFC Review

When Vue is active:

- keep local component CSS in SFC style blocks;
- use `scoped` only when component-local scoping is intended;
- use CSS Modules only when the project convention already uses them;
- use CSS `v-bind()` only when component state must drive a CSS value;
- keep shared tokens, base rules, layout primitives, and shared utilities in shared CSS files;
- use deep/global selectors narrowly and document the reason.
