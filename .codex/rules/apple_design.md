# Apple design guidance rules

Apply this rule only when `CODEX_PROJECT.md` lists `apple_design.md` in `Active Rules`
and the task concerns interface design, visible UI behavior, interaction, or UI/UX review.
Unrelated domain logic, infrastructure, tooling, and documentation do not trigger it.
Design documentation is in scope when the task concerns interface design.

## Required skills

- `apple-hig-design`.

Validate this dependency through `.codex/rules/material_dependencies.md` before applying the rule.
The skill must exist and be independently listed in `Active Skills`; this rule does not activate
missing or inactive materials.

## Source of truth

1. Resolve the current request and allowed surfaces through `.codex/rules/request_routing.md`.
2. Read `CODEX_PROJECT.md` and the applicable approved design/product documents.
3. Read `.agents/skills/apple-hig-design/SKILL.md` and only its task-relevant references.
4. Use the current official Apple topic for exact metrics, platform/version details, conflicts,
   or recommendations that are only indexed locally.

The current user request and project design contract retain their declared priority. Apple guidance
supports decisions within that contract; it does not silently replace agreed layouts, labels, tokens,
search semantics, density, imagery, privacy behavior, or interaction flows. If a recommendation conflicts
with the approved design, preserve the contract or propose an authorized change. If the conflict involves
applicable accessibility requirements, report it and propose a scoped resolution without weakening
those requirements or silently changing the design. Do not claim conformance without evidence.

## Applying the guidance

- Distinguish general Apple principles, native platform recommendations, and web/project adaptations.
- For corners and nested shapes, evaluate radius, inset, border, and container relationships together.
  Do not invent a universal Apple radius or apply a capsule shape to every control.
- Check hierarchy, typography, contrast, control semantics, accessible input, feedback, and recovery
  for the changed scenario. Preserve necessary information density and comparison capabilities in
  professional or data-intensive interfaces. Read specialized source topics when the task calls for them.
- Treat native point sizes, focus systems, SwiftUI/UIKit/AppKit APIs, and Liquid Glass as contextual.
  They are not automatic CSS tokens, browser behavior, or a requirement to restyle the website.
- For web work, apply the project's applicable accessibility baseline and web standards, including
  applicable WCAG criteria; use WAI-ARIA Authoring Practices as implementation guidance.
  HIG does not replace web standards or the evidence required for a conformance claim.
- Attribute material recommendations to the relevant Apple source. Label adapted choices and preserve
  conditional wording. Case studies and older WWDC examples are not current universal requirements.
- A design recommendation does not authorize new features, dependencies, assets, accounts, analytics,
  external services, or changes to data processing and persistence. It cannot override routing or
  the architecture gate.
- For an authorized UI change, use the project's existing components, independently active styling
  overlays, and declared UI/browser validation within the permitted request scope. Documentation-only
  work does not require a runtime UI validation session.

## Optional coordination

When independently active and relevant, coordinate with `.codex/rules/ui_ux_validation.md`,
`.codex/rules/css.md`, `.codex/rules/css_animation.md`, `.codex/rules/tailwind_css.md`,
`.codex/rules/scss_styling.md`, and `.codex/rules/vue_svg_graphics.md`.
These remain responsible for implementation and validation mechanics. Their activation and dependencies
are checked separately; this rule adds design reasoning rather than another styling pipeline.
