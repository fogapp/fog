---
category: Layout
---
One block of a settings page: a `<section id>` whose header is `SectionHead` (the `title` in sentence case, an optional `icon`, and `actions` at the end of the line), an optional one-line `description` under it, and the controls as `children` `mt-4` below. No card and no rule around it — the space between sections is the structure. The `id` is what a link from anywhere in fog lands on, so make it a stable slug. Stack sections with `space-y-10`, under the page's `PageHeader`.

## Examples

```jsx
<Section id="offsite" title="Offsite copy" description="A second copy of every kit, in a bucket you control." actions={<Button size="sm">Run now</Button>}>
  <p className="text-sm text-ink-muted">Last copy finished 02:14 today · 41 GB in the bucket.</p>
</Section>
```
