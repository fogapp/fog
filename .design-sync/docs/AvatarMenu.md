---
category: App-wired
---
The account menu in the top bar: username, theme choice (system/light/dark), sign out.

This component belongs to the running fog app: it fetches from the fog API (react-query) and/or reads the router, so it cannot be composed in a design on its own; it is included so the API contract is complete. Build the equivalent screen from the primitives (`PageHeader`, `Section`, `Field`, `Button`, `StateBadge`, `Tabs`…) instead.
