---
category: Forms
---
One setting as a row: `label` and `hint` on the left, the control (`children`: a `Toggle`, a short `Field`, a choice) on the right; on a phone the control drops under its words. `error` shows under the words when the last change didn't take. Rows stack inside `SettingRows`, a veil with a hairline between each, so a page of preferences reads as one list.

## Examples

```jsx
<SettingRows>
  <SettingRow label="A peer's fleet breaks" hint="Push when a peer that sends its errors here reports one.">
    <Toggle checked onChange={() => {}} aria-label="Push when a peer's fleet breaks" />
  </SettingRow>
  <SettingRow label="Snapshot before updates" hint="Applies to apps with backups on.">
    <Toggle checked={false} onChange={() => {}} aria-label="Snapshot before updates" />
  </SettingRow>
</SettingRows>
```
