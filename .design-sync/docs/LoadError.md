---
category: Status
---
A fetch that never landed, as a veil with an amber dot and label — needs you, not an emergency — the raw error text in mono when `error` is an Error or string, and a small "Retry" button when `onRetry` is given. Distinct from an empty result. Give it a bounded width (`max-w-md`).

## Examples

```jsx
<LoadError label="Couldn't load apps." error={err} onRetry={refetch} />
```
