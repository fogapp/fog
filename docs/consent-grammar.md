# The consent grammar

Every federated share is a **lens**: a promise about who sees what, in how
much detail, and what never crosses. The lens is the same shape wherever
sharing happens — fog's file shares, hub's calendars and chronicles, a book
asked of a peer — so a person reads the same kind of promise everywhere. This
is the vocabulary; `clients/fogfed` (Ruby, `Fogfed::Lens`) and `internal/fed`
(Go, `fed.Lens*`) hold the one implementation — both the axis terms and their
plain-English surface — and the conformance vectors prove the two agree term
for term, so `mode`, `scope`, and `provenance` cannot drift between fog, an
app, and the gem. (`detail` is the exception below, and stays local.)

## The four axes

- **mode** — `read` | `contribute`. Contribute lets the peer add items of
  their own; each side keeps its own copy.
- **scope** — `all` | `filtered` | `picked`. What of a collection crosses:
  everything, a rule (a kind or a glob), or a hand-picked set.
- **detail** — how much of each item. Named levels (`full`, `previews`,
  `listing` for files; field-group presets for records), or a custom set.
- **since** — a cutoff, or nil for the whole history. A "going forward" share.

## Invariants

- **One origin.** A share only ever exposes content this instance owns.
  Nothing received from a peer is re-shared onward — no relay, no boost.
- **The floor.** Some things never cross, per kind, and are named in the
  sentence ("Never shared: attendee data"). The floor is constitutional, not
  a setting.

## Provenance (hub)

Words carried across a peering are marked:

- **own** — written by a local member.
- **firsthand** — the author's own instance handed them to us over our direct
  edge, in the clear.
- **withheld** — a stub: we know a contribution exists here and nothing more.

A share sends in the clear only what its own people wrote; everything else is
a stub, filled only over the author's own peering. Words leave with the
relationship.

## The impact sentence

The lens read back in plain English, assembled the same way everywhere
(`Fogfed::Impact.sentence` / `fed.ImpactSentence`, proven identical by the
conformance vectors):

    <subject> sees <scope> — <sees>.
    [ They do not see <hidden>.]
    [ Never shared: <floor>.]
    [ <addendum>.]

> dave sees all 42 events of Family — titles and times. They do not see
> locations. Never shared: attendee data. They can also comment back.

Callers compose the content phrases (files read differently from events); the
grammar that binds them is shared, so the promise always reads the same shape.
