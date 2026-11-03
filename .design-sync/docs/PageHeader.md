---
category: Layout
---
The top of every page: `title` (an `h1`), one line of `description` under it, a `badge` slot beside the title (a `StateBadge`, a saved tick), page-wide `actions` on the right (Buttons), and `children` for what belongs under the header, usually a `Tabs` strip in the `underline` variant. Wraps on narrow widths. Use it instead of a bare heading.

## Examples

```jsx
<PageHeader
  title="Jellyfin"
  description="Media server · jellyfin/jellyfin:10.10.3"
  badge={<StateBadge status={{ state: "running" }} known error={false} />}
  actions={<><Button>Restart</Button><Button variant="primary">Open</Button></>}
>
  <Tabs variant="underline" label="App sections" active="logs" onChange={setTab} items={[{ id: "overview", label: "Overview" }, { id: "logs", label: "Logs" }]} />
</PageHeader>
```
