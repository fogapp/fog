---
category: Forms
---
A `Field` for a value too long for one line — a public key, a certificate. Monospace, labelled, with an optional `hint`.

## Examples

```jsx
<TextArea label="Public key" rows={3} placeholder="ssh-ed25519 AAAAC3Nz… you@laptop" hint="The contents of the .pub file — never the private key." />
```
