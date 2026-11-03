---
category: Overlays
---
A right-click menu at the pointer: `x`/`y` are viewport coordinates (from the contextmenu event), `items` the verbs that apply, `onClose` fires after a choice, Escape, or a click away. Items are `{ label, onSelect, shortcut?, disabled?, danger? }` or `{ separator: true }`. It never runs off the viewport and arrow keys walk the enabled items. Render it only while open.

## Examples

```jsx
{menu && (
  <ContextMenu x={menu.x} y={menu.y} onClose={() => setMenu(null)} items={[
    { label: "Open", shortcut: "↵", onSelect: open },
    { label: "Download", onSelect: download },
    { separator: true },
    { label: "Rename", shortcut: "F2", onSelect: rename },
    { label: "Share with a peer", disabled: true },
    { separator: true },
    { label: "Move to trash", danger: true, shortcut: "⌫", onSelect: trash },
  ]} />
)}
```
