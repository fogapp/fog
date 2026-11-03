---
category: Actions
---
A small "Copy" button that sits beside the thing it copies (a key, a URL, a command) and flips to "Copied ✓" only after a copy that actually worked. Pair it with a `<code>` block on `bg-code` in a `flex items-center gap-2` row; the code gets `min-w-0 flex-1 truncate`.

## Examples

```jsx
<div className="flex items-center gap-2">
  <code className="min-w-0 flex-1 truncate rounded border border-fog-800 bg-code px-2 py-1.5 font-mono text-xs text-ink">{url}</code>
  <CopyButton text={url} />
</div>
```
