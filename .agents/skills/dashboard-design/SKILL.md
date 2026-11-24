---
name: dashboard-design
description: "Design and visually review the gost-restreamer chat Dashboard. Use when changing its layout, typography, or visual hierarchy."
---

# Chat Dashboard Design

Use [Apple's design guidance and its application to the Dashboard](references/apple-hig.md) when choosing the layout, typography, or visual hierarchy. The page's behavioral requirements are defined in `chat-architecture.md`.

- Keep the conversation at the center of the Dashboard. Group filters, source statuses, and controls so users can find them without losing their place in the feed. Show system statuses separately from chat messages.
- Make the next available action easy to find when a source needs attention. Give each status a text label that communicates its meaning without relying on color.
- Use size and weight to distinguish headings, body text, and metadata. Use a system font stack. Check that long names and messages wrap and that larger text leaves controls visible.
- Review the feed and settings in both narrow and wide browser windows. Verify that incoming messages preserve the user's position during manual scrolling and that the new-message count is easy to find next to the feed.
