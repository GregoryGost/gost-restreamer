# Apple Platform Guidance and Web Boundaries

HIG recommendations belong to a particular environment. Read the target platform's page and the component's
platform considerations before transferring a recommendation. The topic map is in [hig-index.md](hig-index.md).

## Native platform entrypoints

| Surface | Consult for |
| --- | --- |
| [iOS](https://developer.apple.com/design/human-interface-guidelines/designing-for-ios) | Touch-first phone layout, platform navigation, system behaviors |
| [iPadOS](https://developer.apple.com/design/human-interface-guidelines/designing-for-ipados) | Resizable windows, multitasking, mixed touch/pointer/keyboard inputs |
| [macOS](https://developer.apple.com/design/human-interface-guidelines/designing-for-macos) | Dense desktop tools, menus, windows, pointer and keyboard conventions |
| [tvOS](https://developer.apple.com/design/human-interface-guidelines/designing-for-tvos) | Distant viewing, remote navigation, focus and overscan considerations |
| [visionOS](https://developer.apple.com/design/human-interface-guidelines/designing-for-visionos) | Gaze/gesture interaction, comfort, depth, windows, volumes, and immersion |
| [watchOS](https://developer.apple.com/design/human-interface-guidelines/designing-for-watchos) | Glanceable wrist interactions and compact device-specific controls |
| [Games](https://developer.apple.com/design/human-interface-guidelines/designing-for-games) | Game-specific interaction, legibility, controls, and platform adaptation |
| [iPhone Duo](https://developer.apple.com/design/human-interface-guidelines/designing-for-iphone-duo) | Device poses and adaptive dual-display layouts |

This table routes to platform guidance; it is not a substitute for those pages or an instruction to support
additional devices. The actual interface and input environment, rather than the subject matter of a product,
determine which guidance is applicable.

## Metrics need their original context

- Native points, image pixels, CSS pixels, display scale, and spatial angular sizing are different concepts.
- HIG's platform accessibility table and an individual component's hit-region advice can have different scopes.
  Check both rather than selecting a number in isolation or calling one value Apple's universal minimum.
- A hit target can exceed the visible glyph. Target spacing matters as well as target size.
- Native font-size tables and Dynamic Type categories do not define a website's typography scale.
- Safe areas depend on the actual display/window and surrounding controls, rather than a copied phone inset.

Sources: [Accessibility](https://developer.apple.com/design/human-interface-guidelines/accessibility),
[Buttons](https://developer.apple.com/design/human-interface-guidelines/buttons),
[Typography](https://developer.apple.com/design/human-interface-guidelines/typography),
[Images](https://developer.apple.com/design/human-interface-guidelines/images),
[Layout](https://developer.apple.com/design/human-interface-guidelines/layout).

## Native appearance and APIs

Prefer system window frames and controls in native apps; reproducing a frame also creates obligations
to match its behavior and changing state. This does not prohibit styling a web modal through the project's
components or justify imitating macOS window controls on a website.

Source: [Windows](https://developer.apple.com/design/human-interface-guidelines/windows).

Standard native components can adopt updated shapes and materials through the platform frameworks.
When adapting to Liquid Glass, inspect custom backgrounds, sheet edges, control spacing, and safe areas.
SwiftUI, UIKit, and AppKit APIs are native implementation choices, not dependencies for a frontend application.

Source: [Adopting Liquid Glass](https://developer.apple.com/documentation/technologyoverviews/adopting-liquid-glass).

## Apply the reasoning to a website

The following are **web adaptations**, not verbatim Apple implementation requirements:

| Apple design concern | Web decision to evaluate |
| --- | --- |
| Familiar controls | Native HTML semantics and existing accessible project primitives |
| Type enlargement | Browser text sizing/zoom and content that wraps without hiding essential information |
| Meaningful colors | Semantic CSS tokens with adequate contrast in actual rendered states |
| Concentric shapes | Deliberate relationships among approved radii, spacing, borders, and nesting |
| Multiple input methods | Keyboard, touch, pointer, visible focus, and non-hover access |
| Clear modality | An appropriate dialog pattern, focus handling, dismissal, and restoration |
| Reduced motion | A usable reduced-motion presentation that still communicates results |
| Materials and hierarchy | Project-approved surfaces and readable separation between controls and content |

Use the independently active styling and frontend overlays for browser compatibility and implementation.
Check browser support before using a modern CSS feature; similarity to an Apple screenshot proves neither
support nor fidelity.

Use the project's web accessibility baseline and authoritative web sources:
[WCAG 2.2](https://www.w3.org/TR/WCAG22/) (a normative standard; apply the applicable version, level,
criteria and exceptions) and [WAI-ARIA Authoring Practices](https://www.w3.org/WAI/ARIA/apg/)
(informative implementation patterns, not a conformance standard).
HIG review alone is not a WCAG audit. Prefer native HTML where it expresses the required behavior;
do not copy native platform focus exceptions into browser components. Surface conflicts with applicable
accessibility requirements and the approved design instead of weakening either or claiming conformance.

## Technology-specific guidance

The HIG technology section covers Apple Pay, purchases, authentication, Siri, AI, health, cars, maps,
sharing, cloud storage, and other services. Read the exact technology page when the feature is requested
and supported by the project. Check current implementation documentation separately if APIs are needed.

A catalog entry is not activation, permission to collect data, approval to add a service, or a recommendation
that every product needs the feature. Preserve the project's existing privacy and integration boundaries.

## Updating recommendations

For a platform migration, compare the current HIG, its change log, the relevant adoption guide, and recent
WWDC material. Keep older videos for enduring explanations, but resolve obsolete visuals and API details
against current sources. Case studies describe product-specific tradeoffs; they do not override current HIG.

Start with [Apple Design updates](https://developer.apple.com/design/whats-new/) and
[Apple Design Resources](https://developer.apple.com/design/resources/). Design kits aid inspection;
their availability does not authorize importing their assets or changing the product's design system.
