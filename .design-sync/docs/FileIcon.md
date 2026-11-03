---
category: Content
---
One inline SVG glyph per file `category`, each in its own tone: `folder` and `apps` are filled beacon folders; `image`, `video`, `audio`, `pdf`, `text`, `code`, `archive`, `document` are outlined pages with a mark; `link` is a chain; `file` the plain page. `className` sets the size (default `h-5 w-5`). Use it first in a file row.

## Examples

```jsx
<div className="flex items-center gap-3 py-2 text-sm">
  <FileIcon category="pdf" />
  <span className="flex-1 text-ink">lease-2026.pdf</span>
  <span className="text-xs text-ink-muted">1.1 MB</span>
</div>
```
