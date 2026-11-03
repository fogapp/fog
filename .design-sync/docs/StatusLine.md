---
category: Status
---
A status in one line: a dot and the answer. A Settings page wears one under its title (`PageHeader` children), so "is this set up, and to what?" is answered before any control. `ok` (a beacon dot) is a working setup, `warn` (amber) something half-done or erroring, `idle` (a plain grey dot, the default) nothing configured, which is a normal state, not a fault.

## Examples

```jsx
<StatusLine tone="ok">Mounted from 192.168.1.111/media · 3.2 TB free</StatusLine>
<StatusLine>Not set up</StatusLine>
```
