---
name: apple-hig-design
description: "Use Apple HIG, WWDC design guidance, and Apple Design case studies for UI design and review when explicitly active. Distinguish native platform guidance from web adaptations for layout, shapes, controls, navigation, accessibility, typography, color, and motion."
---

# Apple HIG Design

Use Apple's design guidance to make concrete interface decisions and explain their rationale.
This is one skill with references loaded by topic, rather than a requirement to load the entire HIG.

## Required Dependencies

None.

## Authority and applicability

- Follow repository bootstrap and request routing. In target projects use this skill only when
  explicitly listed in `CODEX_PROJECT.md` under `Active Skills` and relevant to the task.
  A frontend stack, an Apple device, metadata, or a task mentioning HIG is not an activation signal.
- Read `CODEX_PROJECT.md` and applicable approved product/design documents before making decisions.
  Preserve the current request, approved design system, product behavior, component library, and
  existing design tokens. HIG is a design reference, not a replacement for that contract.
  If applicable accessibility requirements conflict with the approved design, report the conflict
  and propose a scoped resolution; do not silently change the design or claim compliance.
- Identify the actual surface: native Apple app, website, or spatial experience. A website viewed on
  an iPhone is still a website; SwiftUI, UIKit, and AppKit behavior does not become its implementation contract.
  For other non-Apple surfaces, use only transferable principles with appropriate platform-specific verification.
- Distinguish **Apple guidance**, **platform-specific guidance**, and **web/project adaptation** when the
  distinction affects the decision. Preserve qualifiers such as "consider," "prefer," and "when appropriate."
- Use current HIG guidance for present platform behavior. Older WWDC sessions explain reasoning;
  case studies illustrate decisions made by particular teams and are not universal requirements.
- A recommendation does not authorize a redesign, new feature, dependency, integration, asset import,
  or data processing change. Stay within request routing and any applicable architecture gate.

## Choose references

| Task | Read |
| --- | --- |
| Principles, layout, radii, spacing, type, color, materials, icons, motion | [Foundations](references/foundations.md) |
| Actions, forms, search, navigation, dialogs, tables, loading, errors, help, input | [Patterns and components](references/patterns-and-components.md) |
| Platform differences, native metrics, Apple technologies, translation to the web | [Platform and web boundaries](references/platforms-and-web.md) |
| Any indexed HIG subject, including specialized components and technologies | [HIG topic index](references/hig-index.md) |
| WWDC explanations, design process, prototyping, writing, case studies | [Extended Apple sources](references/extended-sources.md) |

Read the relevant recommendation section, then open the linked primary source when the task depends on
exact wording, dimensions, platform/version availability, a conflicting recommendation, or a topic that
is only indexed. Read its complete relevant section and platform considerations, not just a search snippet.
Do not treat an indexed link as a locally synthesized recommendation.

## Work through a design decision

1. Establish the user's task, current design contract, supported inputs, density, and relevant states.
2. Select the applicable HIG topic and target-platform section. Trace component-specific guidance before
   applying a more general statement; check exceptions and related patterns.
3. Convert the recommendation into an observable decision: hierarchy, grouping, control semantics,
   geometry, readable text, recovery, or feedback. If a material departure from the approved design
   is outside the authorized scope, present it as a proposal and obtain authorization before changing it.
4. For web work, use existing project components and independently active styling overlays. Preserve HTML semantics,
   keyboard access, browser behavior, and the project's applicable accessibility baseline. Use the
   web-specific standards and patterns in the platform reference; native metrics are contextual
   references, not automatic CSS values. Preserve task-critical data density in professional interfaces.
5. Check counterexamples: large text, long localized labels, narrow and wide layouts, dense data,
   keyboard-only operation, touch without hover, reduced motion, and unavailable/error states as relevant.
6. For authorized implementation or validation, use the project's declared checks within the selected
   request mode. Report the decision, supporting source, observable evidence, and any remaining gap.
   Documentation-only work does not require launching the app; do not claim unperformed validation.

## Source coverage and freshness

The supplied archive records a source-map baseline of **2026-10-25**: 158 topic pages across the six
HIG sections, plus 26 selected WWDC sessions and 44 articles linked from Apple Design's update
directory. These imported catalogs preserve that baseline; their live availability and every
recommendation have not been independently rechecked as part of this import.
The references synthesize cross-cutting and common interface guidance; specialized topics stay available
through the index and must be read when applicable. This is not a claim to reproduce every Apple article,
all historical videos, or every paragraph of the HIG.

For "latest" requests, OS migrations, changed components, or outdated/conflicting guidance, consult
[Apple Design updates](https://developer.apple.com/design/whats-new/) and the current topic page.
Verify substantive changes before updating a recommendation and its checked date. If a live source is
unavailable, identify the gap and avoid inventing a platform requirement.

Apple documentation may offer a **View Markdown** link when its HTML requires JavaScript. Use the
official alternative or the official video transcript. Keep paraphrases, links, and task-relevant notes;
do not vendor Apple's manuals, screenshots, fonts, or design kits into the repository.

## Optional coordination

The optional rule `apple_design.md` has a one-way dependency on this skill. The rule is not a prerequisite for using the skill. Framework, styling, accessibility tooling, and browser automation remain project-selected overlays.

When independently active and relevant, coordinate with the `apple_design.md` rule, `ui-ux-review`
for review evidence, `playwright-ui-checks-mcp` for browser inspection, and the project's CSS, CSS
animation, Tailwind, SCSS, Vue, and SVG skills for implementation details. Their activation and
required dependencies are resolved independently; this skill does not enable them.
