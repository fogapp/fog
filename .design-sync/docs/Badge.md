---
category: Status
---
The count pill: how many of something are waiting. `hot` turns it amber for counts someone should look at (open errors, a critical issue); the quiet tone is for counts that are just counts. A count of 0 renders nothing, and 100+ reads "99+". `label` becomes the aria-label ("4 unread"). Put it after the word it counts inside a `flex items-center gap-2`, or at the right end of a nav row with `justify-between`.

## Examples

```jsx
<span className="flex items-center gap-2 text-sm text-ink-muted">Errors <Badge count={12} hot label="errors" /></span>
```

```jsx
<div className="flex items-center justify-between rounded-md bg-fog-900 px-2 py-1.5 text-sm text-ink">
  Inbox <Badge count={4} hot label="unread" />
</div>
```
