<!-- The spec the frontend implements; frontend/src/index.css is the token
     layer and frontend/scripts/themes.mjs generates every palette. This
     version is the refresh of 2026-10-29. The "Instrument × Fogbank"
     handoff it replaces (2026-09-02) is in git history: `git log --
     docs/design/fog-identity.md`. -->

# Fog identity

Fog is a quiet operator UI for a home server. It is recognisable not by a
flourish but by a few rules applied everywhere, with nothing left as a
one-off:

1. **Three text tones.** Ink, muted, faint. Nothing else is text.
2. **Two line strengths.** One for panels, hairlines and dividers; one for the
   edge of a control.
3. **One control height.** Every button, field, select, segmented track and
   search box is 32px; a compact action in a dense row is 24px.
4. **Hierarchy from weight, not size.** Titles are small and heavy; there is
   no display type outside headline figures.
5. **One accent, spent sparingly.** It marks the primary action, what is
   active, the focus ring, and what is alive.

And three carried over from the identity before it:

6. **Flat.** No gradients anywhere, and no shadows except on floating layers
   (menus, popovers, dialogs).
7. **Two hues.** The palette's beacon and amber. Amber is "needs you"; there
   is no red, because nothing in a house is an emergency.
8. **Monospace is a spice.** Only for literal machine strings.

The refresh took its discipline from OpenRouter's interface (measured from
openrouter.ai on 2026-10-29: two text tones, one 8% hairline, every control
32px with 14px/500 text, 20px/600 titles, a 16px line icon on every label, one
saturated accent used in about three places per screen). The identity — the
palettes, the beacon, amber, the flat ground, mono as a spice — is Fog's own.

## Tokens

Everything a screen needs is a token in `index.css` or a named utility, so a
page never carries a literal colour. Every palette and mode re-points the same
names; no class changes between light and dark.

### Text

| Token | Class | For |
|---|---|---|
| `--color-ink` | `text-ink` | Headings, body, values, anything read first |
| `--color-ink-muted` | `text-ink-muted` | Descriptions, hints, navigation, meta, table headers |
| `--color-ink-faint` | `text-ink-faint` | Placeholders, units, separators, disabled |

The three are generated per palette by `themes.mjs` as shares of the way from
the ground to ink (muted 0.66 light / 0.64 dark, faint 0.45), keeping the hue
and chroma of the ramp stop they replace so a palette's greys keep its tint.
The audit holds ink ≥ 7:1, muted ≥ 4.5:1 on the ground and on `fog-900`, and
faint ≥ 2.5:1; a palette that misses is never written.

### Surfaces and structure

| Token | Class | For |
|---|---|---|
| `--color-fog-950` | `bg-fog-950` | The ground (the body has it) |
| `--color-veil` | `bg-veil` | A panel: white over a light ground, a lifted translucent step over a dark one |
| `--color-fog-900` | `bg-fog-900` | Solid surface for menus, popovers, dialogs |
| `--color-code` | `bg-code` | The screen within a page: code, logs, keys, anything quoted verbatim |
| `--color-line` | `border-hair`, `border-divider`, `border-veil-edge`, `border-hair-strong` | Panel edges, hairlines, row dividers |
| `--color-line-strong` | `border-edge`, `border-edge-strong` | The edge of a control |
| `--color-edge-hover` | `hover:border-edge-hover` | A control's edge under the pointer |
| `--color-hover` | `bg-hover` | Hover rows, quiet fills, a neutral pill |

The role names (`hair`, `divider`, `edge`…) stay in markup because they say
what a line is for; `themes.css` resolves each to `line` or `line-strong`, so
the values can't drift apart. Light: line is black at 8%, line-strong 14%.
Dark: white at 8% and 15%.

The fog ramp (`fog-50 … fog-950`) is for fills and surfaces only — tracks, the
toggle, an avatar, an off dot (`bg-fog-600`). It is never text.

### Colour

| Token | For |
|---|---|
| `--color-accent` | The beacon: the primary action (`bg-accent text-accent-on`), the active nav row or tab (`bg-accent/10 text-accent`), focus, live state, links |
| `--color-accent-hover` | A beacon fill under the pointer |
| `--color-accent-on` | The only text colour on a beacon fill |
| `--color-warn` | Amber: needs you. Destructive actions are `border-warn/45 text-warn`, never filled |
| `--color-seg-*` | The usage bar on a disk row, one fixed colour per category: the one place more than two hues appear, because a stacked bar is unreadable in one |

Not the accent: section titles, sparklines and charts (they draw in
`ink-muted`), tags and category chips, the avatar, the store's Install action.
A view has one `primary` button.

Status is beacon or amber, nothing else. Running / ok: `bg-accent/10
border-accent/30 text-accent`. Degraded / warn: `bg-warn/10 border-warn/30
text-warn`. Stopped / neutral: `bg-hover border-veil-edge text-ink-muted`.
Unknown: `border-veil-edge text-ink-faint`. Dots are 6px `rounded-full`: live
`bg-accent`, warn `bg-warn`, off `bg-fog-600`. Nothing glows.

## Type

Plus Jakarta Sans does all the reading; IBM Plex Mono is kept for machine
strings. Both are self-hosted (OFL) in `frontend/public/fonts/` and ship
inside the binary — fog never loads a font from a CDN.

| Role | Spec | Class |
|---|---|---|
| Page title | 22px / 700, −1.5% | `text-title font-bold` (`PageHeader`) |
| Page title inside a frame (a Settings page) | 20px / 700 | `PageHeader size="md"` |
| Section title | 16px / 600, sentence case | `text-heading font-semibold` (`SectionHead`) |
| Heading in a pane or panel | 14px / 600 | `subhead` |
| Body | 14px / 450, line-height 1.6 | `text-sm` (the default) |
| Table cell | 13.5px | `text-cell` |
| Hint, a right-hand fact on a section line | 13px, muted | `text-hint text-ink-muted` |
| Control text | 14px / 500 | (Button, Field, Select do it) |
| Form label | 13.5px / 500, sentence case | `field-label` |
| Quiet group heading (sidebar, menus, palette) | 12px / 500, faint | `group-label` |
| Stat-tile label, table column header | 12px / 500, uppercase, +2% | `caps-label` |
| Headline figure | 42px / 32px, 500, −2.5%, tabular | `text-figure` / `text-figure-sm`, unit in `unit` |
| Machine string | Plex Mono 12–13.5px | `font-mono` |

`font-weight: normal` is 450 (`--font-weight-normal`). `caps-label` is the only
uppercase in the system; everything else — buttons, pills, badges, status
words, section titles — is sentence case. Digits are tabular everywhere.

Monospace is permitted only for what someone could paste into a terminal:
paths, image tags and versions, hostnames and IPs, log timestamps, log lines,
code, and the value of a field that takes machine syntax (`Field` is mono).

## Shape and rhythm

**Controls.** 32px (`h-8`), 6px radius, 14px / 500. Compact is 24px (`h-6`,
12px), for an inline action in a dense row (`Button size="sm"`). `Button`,
`Field`, `Select`, `TextArea`, `FilterInput` and `Segmented` all hold it. A
control drawn by hand uses the `control` / `controlArea` class strings from
`Field`, or `buttonClass(variant, size)` from `Button`, so it is the same box.
`Segmented` is a 32px track with 24px options; the chosen one is lifted onto
the surface in the beacon.

**Radii.** `rounded-xl` (12) for dialogs and window-like cards (sign-in,
command palette, the inbox popover) · `rounded-lg` (8) panels · `rounded-md`
(6) controls, nav rows, highlighted table rows · `rounded-full` pills, dots,
bars. A hairline never gets a radius.

**Spacing.** `gap-2` between buttons, `space-y-4` inside a block, `space-y-12`
between page sections, table rows `py-2.5 pl-3.5 pr-2.5`. Forms `max-w-sm`,
prose `max-w-lg`.

## Icons

16px line icons from `lucide-react` (ISC), imported per icon. One sits before
each sidebar row; a tab (`TabItem.icon`) or section title (`SectionHead
icon`) may carry one; a button may lead with one (`[&>svg]:size-4` is built
into `Button`). Icons take the colour of the text beside them, at the muted
tone in navigation. An icon names what a row is — never decoration alone.

## Patterns

- **Shell.** A 218px sidebar on the ground beside the page, split by a
  hairline. The header (56px) carries the breadcrumb — the page in ink at
  600, the levels above it as muted links — then the search box that opens
  the command palette (⌘K), feedback, the inbox bell and a neutral avatar.
- **Sidebar.** Rows are 32px with an icon (an app's row has its state dot in
  the icon's place); the active row is `bg-accent/10 text-accent`. Group
  headings (Apps, Connect) are `group-label` with a › or + saying what a
  click does. Sub-rows (Drive pins, Settings areas) indent to the parent's
  words.
- **Section head.** Title in sentence case, optional icon before it,
  optional one-line description under it, actions or a quiet right-hand fact
  at the end of the line. No rule, no tick, no card; body `mt-4`.
- **Panels.** `bg-veil border-veil-edge rounded-lg`. Lists of rows in a panel
  divide with `divide-divider`. Metric tiles are not panels: cells on a plain
  row divided by `border-l border-hair`, `caps-label` over a figure.
- **Tables.** Header `caps-label border-b border-hair`, cells `text-cell`,
  numbers right-aligned, dividers `border-divider`, hover `bg-hover`; a
  selected row is `bg-accent/10 edge-accent rounded-md`, a warn row
  `bg-warn/10`.
- **Tabs.** Under a page header, the underline strip: the active tab is ink
  over a beacon underline. Inside a page, pills: the active one
  `bg-accent/10 text-accent`.
- **Focus.** Every focusable control shows a 1px beacon outline and a 3px
  ring at 13%. Never removed without a replacement.
- **Never state a fact before its data exists.** `Loading` (labelled),
  `LoadError` (with retry), `StateBadge known={false}`, and `StaleNote`
  under numbers that stopped refreshing.

## Palettes

Sixteen palettes, each in a light and a dark variant, chosen per mode on
Settings › Preferences › Appearance and remembered by the browser
(`fog.palette.light`, `fog.palette.dark`; the document carries `data-palette`
beside `data-theme`). Out of the box fog is light and wears Graphite; the dark
slot defaults to Fog. Every palette is the same eleven-stop ladder measured in
OKLCH, re-hued and given its own beacon; amber is amber in all of them, and
the structure tokens are per mode, not per palette. They are generated —
`frontend/scripts/themes.mjs` holds the seeds and the contrast floors, `npm
run themes` writes `src/themes.css` and `src/themes.generated.ts` — so a new
palette is one seed line.

| id | ramp hue | beacon |
|---|---|---|
| fog | blue-grey (the reference ramp; default in dark) | teal |
| graphite | neutral (default in light) | steel blue |
| ink | neutral, true black / paper white | electric blue |
| charcoal | hearth: warm neutral on a raised `#1f1e1e` dark ground | teal |
| flint | hearth | cornflower |
| sage | hearth | sage |
| heather | hearth | heather |
| slate | indigo-grey | frost blue |
| ocean | deep blue | cyan |
| mint | teal-grey | mint |
| forest | green-black | leaf |
| olive | olive drab | chartreuse |
| ember | warm charcoal | coral |
| dune | sepia sand | gold |
| plum | aubergine grey | lavender |
| rose | dusty rose grey | rose |

The light ground is near-white (Graphite `#f9f9f9`) so white panels lift off
it by their hairline alone. In light mode the beacon darkens to hold contrast
as text, with white on its fill.

## The mark

The house-and-cloud glyph (`Logo`), painted in the beacon through a mask so it
follows every palette, beside "fog" in Plus Jakarta Sans 600. The favicon and
PWA icons still carry the older glyph.
