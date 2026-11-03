---
category: Forms
---
A choice among a few, in the same box as a `Field` but in the reading face — options are words ("SMB / CIFS", "member"), not machine strings. With `label` (and optional `hint`) it stands in a form; without one it sits inline in a row of controls and is named by `aria-label`.

## Examples

```jsx
<Select label="Protocol" value="nfs" onChange={() => {}}>
  <option value="nfs">NFS</option>
  <option value="cifs">SMB / CIFS</option>
</Select>
```
