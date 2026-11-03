---
category: Actions
---
The one button. `secondary` (the default) is what most rows carry; `primary` is the single committing action on a page (Deploy, Save, Connect); `info` is the beacon-tinted action that pairs with the "update available" pill; `danger` (alias `destructive`) is an amber outline for destructive verbs — nothing in a house is an emergency, so there is no red; `ghost` is for Cancel/Discard beside a primary. Two sizes: `md` for pages and dialogs, `sm` inside tables and section headers. It is a real `<button>`, so `onClick`, `disabled`, `type` and `title` pass through; `disabled` dims it to 40% and blocks the pointer. Copy is sentence case: "Prune cache", never "PRUNE CACHE".

Group actions right-aligned with `flex justify-end gap-2`, primary last.

## Examples

```jsx
<div className="flex justify-end gap-2">
  <Button variant="ghost">Discard</Button>
  <Button>Preview</Button>
  <Button variant="primary">Publish</Button>
</div>
```

```jsx
<Button variant="danger" size="sm" onClick={remove}>Remove app</Button>
<Button variant="primary" disabled>Deploying…</Button>
```
