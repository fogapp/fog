---
category: Layout
---
How a group opens: the `title` in sentence case at 16px / 600, an optional 16px line `icon` before it (a lucide-react component), an optional one-line `description` under it, and anything for the far end of the line (`right`: a count or state in `text-hint text-ink-muted`, or the group's buttons). No rule, no tick, no card — the space above the next title separates groups. Use it for **every** group on every screen; put the body `mt-4` below. `Section` renders through it, so reach for `SectionHead` directly only where a group is not a `<section id>` block — a table's title, a column in a two-column body.

## Examples

```jsx
<SectionHead title="Apps" right={<span className="text-hint text-ink-muted">6 running</span>} />
<SectionHead title="Peers" />
<SectionHead title="Disks" right={<Button size="sm">Add disk</Button>} />
```
