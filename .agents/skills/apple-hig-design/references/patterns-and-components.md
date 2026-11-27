# Apple Interaction Patterns and Components

Apply these recommendations within the existing product contract. Additional components and specialized
patterns are discoverable through [the HIG index](hig-index.md); open their current pages before using them.

## Actions and control choice

Make the primary action recognizable without making every action equally prominent. Similar choices
should use coherent sizing; emphasis comes from style. Custom buttons need a visible pressed state and
enough surrounding space. Prefer text when an icon's purpose is unclear. Distinguish primary, cancel,
and destructive intent; check context-specific alert rules before choosing a default action.

Source: [Buttons](https://developer.apple.com/design/human-interface-guidelines/buttons).

Use a toggle for opposing states, with an unambiguous subject and a state indicator beyond color.
Source: [Toggles](https://developer.apple.com/design/human-interface-guidelines/toggles).

Use a stepper for small incremental adjustments; combine it with direct entry when large changes matter.
Make the affected value evident. Product rules define the permitted range and increment.
Source: [Steppers](https://developer.apple.com/design/human-interface-guidelines/steppers).

For menus, context menus, pickers, segmented controls, sliders, and combo boxes, select the component by
the task and selection model. Read the specific topic before treating its appearance or behavior as interchangeable.

## Forms and data entry

Ask only for information needed by the task. Prefer appropriate choices and existing permitted information
over repetitive entry. Allow useful paste and other supported inputs. Explain required data and give
timely, actionable validation feedback. Avoid interpreting suggestions to gather information as permission
to request additional private data or introduce a new integration.

Source: [Entering data](https://developer.apple.com/design/human-interface-guidelines/entering-data).

Give fields clear labels and hints, sensible size, consistent grouping, and logical keyboard order.
A disappearing placeholder must not leave the field's purpose ambiguous. Choose a suitable keyboard/input
type, account for locale when formatting numbers, and validate at a time appropriate to the value.

**Web adaptation:** associate labels and errors programmatically; preserve an editable value during
intermediate typing. Do not change numeric parsing, limits, or persistence without checking the product contract.

Source: [Text fields](https://developer.apple.com/design/human-interface-guidelines/text-fields).

## Search and navigation

Keep important search easy to find, make its scope explicit, and make refinements understandable.
Search history and suggestions need contextual privacy consideration; they are optional capabilities.

Source: [Searching](https://developer.apple.com/design/human-interface-guidelines/searching).

Respond as people type where feasible, show relevant results clearly, and make filters/tokens usable.
HIG recommends a broad initial scope, but that is not authority to replace an approved product-specific
empty state, category selection model, matching algorithm, ranking, or debounce behavior.

Source: [Search fields](https://developer.apple.com/design/human-interface-guidelines/search-fields).

**Web adaptation:** display typing immediately even when result computation is debounced. Distinguish
initial, loading, empty-result, and failed states. Preserve existing scope semantics and browser navigation.
Implement links for navigation and buttons for actions; these are web semantics, not SwiftUI APIs.

When working on sidebars, tab bars, tab views, toolbars, or path controls, consult the exact HIG component
and platform. Do not move a site's navigation to an iOS-style tab bar merely because current iOS uses one.

## Dialogs, sheets, popovers, and alerts

Use modality for a clear, scoped benefit. Give the task an understandable title and an obvious exit.
Avoid unnecessary nested modal tasks and protect unsaved work when dismissal can cause unexpected loss.

Source: [Modality](https://developer.apple.com/design/human-interface-guidelines/modality).

A native sheet can be modal or nonmodal depending on platform and purpose. Do not assume every sheet
blocks its parent. Keep its task focused and its completion/cancel behavior clear.
Source: [Sheets](https://developer.apple.com/design/human-interface-guidelines/sheets).

Anchor transient popover content to its origin, avoid obscuring essential context, and keep multi-selection
content open while choices are being made. Dismissing a nonmodal editor automatically must not silently
discard work. A tooltip, popover, menu, and modal dialog have different interaction contracts.

Source: [Popovers](https://developer.apple.com/design/human-interface-guidelines/popovers).

Reserve alerts for important actionable interruptions. Ordinary information and common undoable actions
usually need less intrusive feedback. Name the actual consequence in action labels; provide a safe way
to cancel a destructive choice. Exact native button ordering and default behavior depend on platform/context.

Source: [Alerts](https://developer.apple.com/design/human-interface-guidelines/alerts).

**Web adaptation:** use the project's dialog primitive and applicable web accessibility pattern. Verify
focus entry, containment where the dialog is modal, an accessible title, dismissal, and appropriate focus
restoration. The project's approved Escape/outside-click behavior and unsaved-work contract remain
authoritative within applicable accessibility requirements; report conflicts rather than silently
changing behavior. A native sheet's gesture behavior is not automatically a browser dialog requirement.

## Lists, collections, and dense information

Use lists/tables for scanning text and comparing attributes; use collections where visual content is central.
Keep row content readable and selection feedback consistent. Give multicolumn data meaningful headers.

Sources: [Lists and tables](https://developer.apple.com/design/human-interface-guidelines/lists-and-tables),
[Collections](https://developer.apple.com/design/human-interface-guidelines/collections).

For hierarchical data, keep the hierarchy in the primary column and make expansion discoverable.
Preserve useful expansion context when the product calls for it. Do not invent sorting, editing, or
persistent expansion just because a native outline view supports those features.

Source: [Outline views](https://developer.apple.com/design/human-interface-guidelines/outline-views).

**Web adaptation:** preserve table headers, row relationships, readable names, and accessible expansion state.
Use existing numerical alignment and density tokens. Keep the main task operable as text and viewport size change.
For administrative, monitoring, and other professional interfaces, preserve task-critical information density,
comparison and scanning; do not replace tables with cards or hide essential values solely for visual minimalism.

Use charts when they help people see a relationship, trend, or comparison, rather than merely displaying
numbers decoratively. Keep a clear takeaway, labels, and accessible alternatives; tables can be better
when exact values are the main need. A visual summary does not replace accessible data descriptions.

Source: [Charting data](https://developer.apple.com/design/human-interface-guidelines/charting-data).

## Feedback, loading, recovery, and help

Keep status near what it describes. Match interruption to significance; explain why an unavailable action
cannot run. Important confirmation is useful, but routine success does not need repeated alerts.

Source: [Feedback](https://developer.apple.com/design/human-interface-guidelines/feedback).

Show useful structure promptly and let unrelated work continue where possible. Use determinate progress
only when progress can be estimated meaningfully; otherwise indicate ongoing work without fabricated precision.
Keep placeholders visually distinct from completed data.

Source: [Loading](https://developer.apple.com/design/human-interface-guidelines/loading).

Predictable undo/redo supports exploration. If the feature exists or is requested, explain what will be
reversed and make its result apparent. Do not silently add an undo history or change persistence policy.

Source: [Undo and redo](https://developer.apple.com/design/human-interface-guidelines/undo-and-redo).

Prefer short contextual help and learning through interaction. Keep optional onboarding skippable and
available later where useful. Help must use the same terms and input language as the actual interface.

Sources: [Offering help](https://developer.apple.com/design/human-interface-guidelines/offering-help),
[Onboarding](https://developer.apple.com/design/human-interface-guidelines/onboarding).

## Keyboard, pointer, and touch

Respect familiar shortcuts and platform gestures. Let people complete important tasks using alternative
inputs. Custom gestures should supplement discoverable standard controls, rather than becoming the only route.
Focus, selection, hover, and activation are distinct states; do not move focus unexpectedly as results update.

Sources: [Keyboards](https://developer.apple.com/design/human-interface-guidelines/keyboards),
[Gestures](https://developer.apple.com/design/human-interface-guidelines/gestures),
[Pointing devices](https://developer.apple.com/design/human-interface-guidelines/pointing-devices),
[Focus and selection](https://developer.apple.com/design/human-interface-guidelines/focus-and-selection).

**Web adaptation:** preserve keyboard access to buttons and all other applicable controls. Native iPadOS
guidance about leaving control navigation to Full Keyboard Access does not apply to HTML focusability.
Provide focus/touch alternatives for hover-revealed information and actions.

## Specialized patterns

For collaboration, accounts, notifications, files, full screen, multitasking, media, haptics, printing,
ratings, settings, workouts, widgets, and system experiences, consult their HIG index entries when the
task actually includes them. Their presence in the source map does not add them to the product scope.

## Review evidence

For the changed task, verify the relevant combination of:

- clear action and information hierarchy, consistent controls, and justified geometry;
- initial, empty, loading, success, error, unavailable, and destructive states;
- typing, selection, cancellation, focus restoration, and recovery behavior;
- pointer, keyboard, touch without hover, large text, long labels, and reduced motion;
- project-required desktop/wide/narrow layouts, readable data, and stable content boundaries.

Use independently active UI review and browser-validation workflows within current authorization.
Record actual evidence and gaps; do not present this design checklist as proof of accessibility conformance.
