---
category: Status
---
A spinning ring that takes its size and colour from Tailwind classes: `size` is a `h-* w-*` pair (default `h-3 w-3`), `tone` a `border-*` colour (default `border-fog-300`). Use it inline beside text; for a whole loading line use `Loading` instead.

## Examples

```jsx
<span className="flex items-center gap-2 text-sm text-ink-muted"><Spinner /> Pulling image</span>
<Spinner size="h-8 w-8" tone="border-fog-500" />
```
