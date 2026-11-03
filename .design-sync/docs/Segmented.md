---
category: Forms
---
One of a few, side by side — Light · Dark · System, Basic · Advanced. A radio group drawn as joined buttons with the chosen one lit. `value` undefined means the stored choice has not arrived: nothing is lit and nothing can be chosen.

## Examples

```jsx
<Segmented label="Mode" value="dark" onChange={() => {}}
  options={[{ id: "system", label: "System" }, { id: "light", label: "Light" }, { id: "dark", label: "Dark" }]} />
```
