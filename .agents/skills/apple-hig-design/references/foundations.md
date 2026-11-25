# Apple Design Foundations

These are paraphrased recommendations. Platform qualifications and web adaptations are identified below.
Use the linked source for its complete context; consult the HIG index for specialized subjects.

## Human goals and hierarchy

Apple's principles give useful questions for evaluating a design:

| Principle | Design question |
| --- | --- |
| Purpose | Does this feature help people accomplish the product's main task? |
| Agency | Can people choose their path, understand consequences, and recover? |
| Responsibility | Are data use, limitations, and risks communicated honestly? |
| Familiarity | Do recognizable controls behave as people expect? |
| Flexibility | Does the experience accommodate different abilities, inputs, and contexts? |
| Simplicity | Does each element earn its space, with enough context to act? |
| Craft | Are wording, geometry, responsiveness, and behavior consistently finished? |
| Delight | Does the experience create the intended feeling without obstructing work? |

Simplicity does not mean removing necessary labels, explanations, or controls.
Use these questions to weigh tradeoffs, rather than treating every principle as an absolute prohibition.

Source: [Design principles](https://developer.apple.com/design/human-interface-guidelines/design-principles).

## Layout and adaptation

- Make important content easy to locate through position, alignment, spacing, and clear grouping.
- Reveal secondary detail progressively while keeping the main task understandable.
- Adapt to available space, large text, orientation, and localized content without losing context.
- Separate controls from the content they affect and respect applicable safe areas.

**Web adaptation:** respond to the actual content area and browser zoom. Avoid selecting layouts solely
by a device name. A responsive change should preserve the task and reading order; visual rearrangement
must not produce a contradictory DOM or keyboard order. Use the project's declared breakpoints and tokens.
For professional and data-intensive interfaces, preserve meaningful simultaneous comparisons, scanning
efficiency, and the approved density; do not hide information just to make the screen look minimal.

Source: [Layout](https://developer.apple.com/design/human-interface-guidelines/layout).

## Corners, shapes, and concentricity

Apple describes three shape families: a fixed corner radius, a capsule with radius half its height
(for a horizontal capsule whose width is at least its height), and a concentric shape whose radius
derives from its container and inset.
For nearby, equally inset rounded corners sharing a center, the simple relationship is:

```text
inner radius = outer radius - inset
```

For example, outer radius 24 and inset 8 imply inner radius 16. These example values are not Apple tokens.
Check the actual visible boundaries, including borders. This simple relation assumes circular arcs and a
nonnegative resulting radius; it is not a formula for every continuous or elliptical corner. Do not
apply this subtraction indiscriminately to distant, independently positioned controls or uneven insets;
use an appropriate fixed/fallback shape.

Apple favors capsules with additional edge space on phones and container-concentric shapes near window
edges on iPad and Mac in the cited design-system session. Dense Mac interfaces still use small rounded
rectangles. A component that also appears independently needs a fallback radius. These relationships
do not prescribe one universal radius.

Source: [Get to know the new design system, 3:03–6:16](https://developer.apple.com/videos/play/wwdc2025/356/?time=183).

**Native detail:** modern AppKit can derive a view's corner configuration from its container and a minimum
radius. Use the current native API rather than copying a measured screenshot radius.
Source: [Modernize your AppKit app, 15:41–16:59](https://developer.apple.com/videos/play/wwdc2026/289/?time=941).

**Web adaptation:** express approved relationships through existing radius and spacing tokens. Inspect
nested thumbnails, cards, dialogs, buttons near corners, and clipping at multiple sizes. CSS rounding is
an implementation choice; the concentricity principle does not require a particular CSS feature or library.

## Typography

Keep text legible at its actual size and viewing distance. Use a small, consistent set of text roles;
distinguish hierarchy through weight, size, and spacing. Avoid fragile thin weights for small text.
Preserve that hierarchy as text grows, and let containers wrap, grow, or stack before important text clips.

**Native guidance:** built-in text styles and Dynamic Type provide coordinated scaling. Custom fonts need
equivalent accessibility behavior. Native point-size tables are platform-specific.

**Web adaptation:** preserve project fonts, support browser text enlargement, and verify supported writing
systems and long localized labels. A system font stack may use installed platform fonts; this is not
permission to embed Apple fonts.

Source: [Typography](https://developer.apple.com/design/human-interface-guidelines/typography).

## Color and appearance

Use color consistently by meaning. Combine it with text, shape, or symbols for state and status.
Reserve strong accents for information and actions that need emphasis. Check contrast on the actual
background, including images, translucency, and interaction states. Native semantic colors adapt to
appearance and contrast settings; copying their current numeric values loses that behavior.

**Web adaptation:** reuse semantic project tokens and evaluate rendered combinations, including disabled,
hover, focus, selected, and error states. Apply the relevant web contrast criteria and exceptions; do not
assume every state has the same normative threshold. Do not introduce a new palette from an Apple screenshot.

Source: [Color](https://developer.apple.com/design/human-interface-guidelines/color).

A dark appearance needs its own contrast and elevation decisions, rather than an automatic inversion
of a light palette. Apple generally recommends respecting native appearance preferences, with contextual
exceptions. A project's approved dark-only web design is not automatically replaced by that native default.

Source: [Dark Mode](https://developer.apple.com/design/human-interface-guidelines/dark-mode).

## Materials and depth

Apple distinguishes the Liquid Glass control/navigation layer from the content layer. Use glass sparingly
for appropriate functional elements; applying it everywhere weakens hierarchy. Standard content materials
have a different role. Transparency must preserve legibility over the backgrounds it actually encounters.

**Web adaptation:** retain the approved material language. Separation can come from opaque surfaces,
spacing, borders, and elevation. Adding blur, refraction, or glass to a website is a separate design choice.

Source: [Materials](https://developer.apple.com/design/human-interface-guidelines/materials).

## Icons, images, and brand

Use recognizable metaphors, consistent icon weight/detail, and optical alignment. Pair ambiguous icons
with clear wording or accessible descriptions. An icon's visible size and its activation area are different.
Keep the established icon system unless the task authorizes a change.

Source: [Icons](https://developer.apple.com/design/human-interface-guidelines/icons).

Check resolution, cropping, aspect ratio, and visibility on real backgrounds. Apple asset-catalog formats
and scale suffixes concern native delivery; they do not replace a web project's image pipeline.

Source: [Images](https://developer.apple.com/design/human-interface-guidelines/images).

Express brand through content, tone, selective color, and appropriate typography while keeping familiar
control behavior. Repeated logos and excessive accent color compete with useful content.

Source: [Branding](https://developer.apple.com/design/human-interface-guidelines/branding).

App icons and SF Symbols have their own platform specifications. Read those topics when relevant;
do not extrapolate app-icon masks to window radii or replace the project's SVG library automatically.

## Accessibility, inclusion, language, and privacy

Design for visual, hearing, motor, speech, and cognitive differences. Provide alternatives to information
communicated by a single sense; allow text enlargement, sufficient target spacing, and alternative input.
Avoid time-limited messages that disappear before people can process or reach them.

**Web adaptation:** preserve semantic controls, accessible names, keyboard operation, visible focus,
and text enlargement. Native focus-system exceptions do not justify removing browser focusability.
Use applicable web accessibility standards for a conformance claim, rather than native metrics alone.
See [platform and web boundaries](platforms-and-web.md).

Source: [Accessibility](https://developer.apple.com/design/human-interface-guidelines/accessibility).

Use respectful plain language and test assumptions about culture, ability, and prior knowledge.
Source: [Inclusion](https://developer.apple.com/design/human-interface-guidelines/inclusion).

Write purposeful headings, action labels, useful empty states, and errors that explain recovery without
blame. Keep terminology consistent. Localize language patterns rather than transferring English title
capitalization mechanically to other languages. Do not rewrite verified domain terminology merely
to make it shorter.

Source: [Writing](https://developer.apple.com/design/human-interface-guidelines/writing).

Minimize requested data and explain its use in context. Permission UI must reflect the actual feature
and platform. A design recommendation does not authorize new data collection, authentication, or tracking.

Source: [Privacy](https://developer.apple.com/design/human-interface-guidelines/privacy).

For localization or RTL work, read the current language-specific topic; layout mirroring is not a rule
to flip every image, icon, number, or word. Follow verified meaning and the project's supported locales.

## Motion

Use short, purposeful motion to explain relationships, state, or feedback. Keep repeated interactions
responsive and allow interruption where practical. Avoid motion that delays work or communicates meaning
without an alternative. Reduce automatic, repetitive, or disorienting movement for people who request it.

**Web adaptation:** honor the project's reduced-motion policy and preserve feedback when motion is reduced.
Do not add a motion library or fixed animation duration based solely on an Apple example.

Source: [Motion](https://developer.apple.com/design/human-interface-guidelines/motion).
