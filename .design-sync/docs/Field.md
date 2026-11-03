---
category: Forms
---
A labelled text input at the control height (32px), on the panel surface with the control edge, monospace because field values are machine strings (a hostname, a cron line, a path); on focus the edge turns beacon with a soft ring. `label` sits above in sentence case (`field-label`), `hint` (text or JSX) below in `text-ink-muted`; pass `aria-invalid` to turn the edge and the hint amber. Every other prop goes to the `<input>`: `value`/`defaultValue`, `placeholder`, `type`, `disabled`, `required`, `onChange`. Stack fields with `space-y-4` inside a `max-w-sm`, and finish a form with a right-aligned `Button` row.

## Examples

```jsx
<div className="max-w-sm space-y-4">
  <Field label="Hostname" defaultValue="media" hint="Reachable as media.fog.lan once the proxy picks it up." />
  <Field label="Port" type="number" defaultValue="8096" required />
  <div className="flex justify-end gap-2">
    <Button variant="ghost">Cancel</Button>
    <Button variant="primary">Save</Button>
  </div>
</div>
```
