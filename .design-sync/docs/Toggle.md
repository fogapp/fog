---
category: Forms
---
A switch for a setting that is on or off right now — "Follow" on a log, "Reveal" on a masked value. It is a state, not a button: the track tints to the beacon at 30% when on and the knob becomes the beacon. Give it a `label` naming what it controls, in sentence case.

## Examples

```jsx
<Toggle checked={follow} onChange={setFollow} label="Follow" />
```
