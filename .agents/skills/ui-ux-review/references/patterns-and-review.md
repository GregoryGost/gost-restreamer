# UI/UX Review Patterns And Review

Use this reference when reviewing visible UI behavior, layout, accessibility-sensitive markup, and interaction states.

## Good: Active Overlay Routing

When a visible change touches styling, route detailed checks to the active styling overlay:

- CSS files or CSS Modules: `css-expert`;
- CSS transitions or motion: `css-animation-expert`;
- Tailwind utilities: `tailwind-expert`;
- SCSS modules or SFC SCSS: `scss-expert`.

Why this is good:

- UI/UX review stays framework-agnostic.
- Styling-specific rules remain in their own skills.
- The agent avoids duplicating CSS/Tailwind/SCSS rules here.

## Bad: Hardcoding One Styling Tool

Do not assume Tailwind, SCSS, or plain CSS just because visible UI changed.

Problems:

- The wrong overlay can be applied.
- Project-specific styling policy can be bypassed.
- Review misses active CSS or motion rules.

## Good: State Checklist By User Outcome

Check states that users can observe:

- default;
- hover and active when pointer interaction exists;
- focus-visible for keyboard use;
- disabled when an action is unavailable;
- loading while data or action is pending;
- empty when no data exists;
- error when action or data loading fails;
- success when confirmation matters.

## Bad: Screenshot-Only Validation

A screenshot can document visible output, but it should not be the only validation signal.

Problems:

- Keyboard reachability can be missed.
- Accessible names and semantic behavior can be missed.
- Console/network issues can remain hidden.

## Good: Browser Evidence Report

When browser validation is active, report:

- route checked;
- wait target;
- visible result summary;
- accessibility snapshot summary when available;
- console errors;
- failed network requests;
- screenshot or artifact references;
- remaining gaps.

## Review Questions

- What visible user outcome changed?
- Which framework and styling overlays are active?
- Are interaction states handled where relevant?
- Are accessible names and semantic roles adequate?
- Does layout remain stable across declared breakpoints?
- Are color, spacing, and hierarchy aligned with active project tokens?
- Is browser validation required by project policy?
- What could not be validated and why?
