---
category: App-wired
---
The full app spec form (source, deploy, postgres, env, secrets, volumes, routes, ports, runtime, notes, files).

This component belongs to the running fog app: it fetches from the fog API (react-query) and/or reads the router, so it cannot be composed in a design on its own; it is included so the API contract is complete. Build the equivalent screen from the primitives (`PageHeader`, `Section`, `Field`, `Button`, `StateBadge`, `Tabs`…) instead.
