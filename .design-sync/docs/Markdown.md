---
category: Content
---
Renders a Markdown string the way fog shows app notes: headings collapse to bold lines, links are beacon, inline code and fenced blocks sit on the `bg-code` surface, lists and blockquotes are styled. Raw HTML is never injected. Pass the Markdown as the single string child and give it a bounded width.

## Examples

```jsx
<div className="max-w-lg">
  <Markdown>{notes}</Markdown>
</div>
```
