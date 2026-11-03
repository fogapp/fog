---
category: Status
---
An app's container state as a sentence-case pill: beacon "Running", amber "Unhealthy" (running with `healthy: false`), neutral for anything stopped (`Exited`, `Dead`…). Two honest interim states: `known={false}` shows a spinner "Checking…" until the server has answered once, and `error` shows amber "State unknown" when the status fetch failed. Never leave a blank where the badge will go; render the badge with `known={false}` instead. Rides beside a page title in `PageHeader`'s `badge` slot.

## Examples

```jsx
<StateBadge status={{ state: "running" }} known error={false} />
<StateBadge status={{ state: "running", healthy: false }} known error={false} />
<StateBadge known={false} error={false} />
```
