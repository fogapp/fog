---
category: Overlays
---
The page dims and one panel takes over: a fixed full-viewport backdrop (`bg-black/60`) with a fog-900 dialog, a `title` bar with ✕, and a scrolling body for `children`. It closes three ways, all through `onClose`: the ✕, the backdrop, Escape. `width` is a Tailwind max-width class (default `max-w-lg`; `max-w-md` for confirmations). Put the dialog's buttons in a `flex justify-end gap-2` row at the bottom of the body. Render it only while open; it has no `open` prop.

## Examples

```jsx
{confirming && (
  <Modal title="Remove Jellyfin?" onClose={() => setConfirming(false)} width="max-w-md">
    <div className="space-y-4">
      <p className="text-sm text-ink-muted">The container and its config are removed. Media on the NAS stays where it is.</p>
      <div className="flex justify-end gap-2">
        <Button variant="ghost" onClick={() => setConfirming(false)}>Keep it</Button>
        <Button variant="danger" onClick={remove}>Remove app</Button>
      </div>
    </div>
  </Modal>
)}
```
