---
category: Layout
---
One tab strip for peer views of the same thing. `pills` (default) for a strip inside a page, `underline` for the strip under a `PageHeader`. Each item is `{ id, label, count?, hot? }`; counts render as a `Badge`. Two routing modes: query-routed tabs leave `to` unset and use `active` + `onChange` (works anywhere); segment-routed tabs set `to` and render react-router links, which need a Router around them. `label` is the nav's aria-label. Set `inline` when the strip sits in a row that already draws its own hairline (the app page's subheader): it then brings no bottom border or padding of its own. Filters are not tabs.

## Examples

```jsx
<Tabs label="Inbox" active={tab} onChange={setTab} items={[
  { id: "all", label: "All", count: 9 },
  { id: "errors", label: "Errors", count: 2, hot: true },
  { id: "done", label: "Done" },
]} />
```

```jsx
<Tabs variant="underline" label="App sections" active="logs" onChange={setTab} items={[{ id: "overview", label: "Overview" }, { id: "logs", label: "Logs" }]} />
```
