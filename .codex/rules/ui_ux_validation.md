# UI/UX validation rules

Apply this rule when `CODEX_PROJECT.md` declares UI/UX validation active or when the task changes visible UI, user flow, navigation, accessibility-sensitive markup, layout stability, responsive behavior, or interaction states.

This rule is framework-agnostic. Framework, styling, motion, and browser automation details are handled by active overlays.

## Required skills

Use together with:

- `ui-ux-review`.

Use related skills only when active:

- `playwright-ui-checks-mcp` for local browser validation and evidence gathering;
- `css-expert` when plain CSS, CSS Modules, global styles, component styles, or Vue SFC style blocks affect UI/UX;
- `css-animation-expert` when CSS motion, transitions, animations, or Vue transition CSS affect UI/UX;
- `tailwind-expert` when Tailwind utilities affect UI/UX;
- `scss-expert` when SCSS affects UI/UX.

## Source of truth

Before validating UI/UX:

1. Read `CODEX_PROJECT.md`.
2. Check active frontend framework, styling overlays, UI validation tools, routes, target browsers, artifact policy, accessibility policy, and design-token policy.
3. Inspect changed UI code and related style/config files.
4. Load `ui-ux-review/references/patterns-and-review.md` for visible UI review checks.
5. Use project-declared validation commands or MCP workflows.

## General checks

- Layout remains stable across declared breakpoints or viewport policy.
- Focus, hover, active, disabled, loading, empty, success, and error states are handled when relevant.
- UI text and accessible names follow project language policy.
- Interactive elements are keyboard reachable where applicable.
- Color, spacing, density, and hierarchy use active project tokens or styling conventions.
- Active styling-overlay checks apply only when those overlays are active.
- Motion and animation checks apply only when CSS animation or motion is active or directly touched.
- Browser evidence is collected when project policy requires it.
- Remaining validation gaps are stated explicitly.
