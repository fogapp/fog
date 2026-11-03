---
category: Status
---
The same app state as a 6px dot, for lists: sidebar rows, tiles, the command palette. Colour alone carries the state — nothing glows: a running app is a beacon dot, unhealthy is amber, `deploying` pulses beacon; `new` (needs setup), `missing` and anything stopped are a plain `fog-600` dot. The `title` attribute carries the word. Place it first in a `flex items-center gap-2` row.

## Examples

```jsx
<div className="flex items-center gap-2 text-sm text-ink">
  <StateDot app={{ state: "running" }} /> Jellyfin
</div>
```
