## Building with Fog

Fog is a quiet operator UI for a home server. Its identity is a few rules applied everywhere: three text tones, two line strengths, one control height, hierarchy from weight rather than size, and one accent kept for what is primary or active. Every component is a Tailwind v4 composition over the tokens in `styles.css`; there is no provider, no theme object, no CSS-in-JS. Load `styles.css` and the components are styled. You write layout glue with the same utility classes the components use.

### The page ground

`styles.css` gives `<body>` the ground: `bg-fog-950 text-ink antialiased`, at the 450 body weight. If your root is not `<body>`, put `bg-fog-950 text-ink antialiased` on it. Everything is flat: no gradients and no shadows on cards, rows, buttons, badges, nav items or bars (only floating menus, popovers and dialogs carry a shadow). Light mode is the default and dark is `data-theme="dark"` on `<html>`; both re-point the same tokens, so never hard-code a hex colour.

### Colour: three text tones, two lines, two hues

| Role | Classes |
|---|---|
| Page ground | `bg-fog-950` (the body already has it) |
| A panel | `bg-veil border border-hair rounded-lg` — white over a light ground, a lifted translucent step over a dark one |
| Solid surface, only for menus, popovers, dialogs | `bg-fog-900 border border-hair` |
| Panel edges, hairlines, row dividers | `border-hair`, `divide-y divide-divider` (all the same `line` strength) |
| The edge of a control | `border-edge`, hover `border-edge-hover` (the `line-strong` strength) |
| Hover row, quiet fill | `bg-hover` |
| Headings, body text, values | `text-ink` |
| Descriptions, hints, navigation, meta | `text-ink-muted` |
| Placeholders, units, disabled | `text-ink-faint` |
| The beacon — primary, active, focused, alive | `text-accent`, `bg-accent text-accent-on` (never white on a beacon fill), an active row or tab `bg-accent/10 text-accent` |
| Amber — needs you | `text-warn`; a destructive action is `border border-warn/45 text-warn`, never red |
| Code, logs, keys | `bg-code font-mono text-xs text-ink` inside `border border-hair rounded` |

The fog scale (`fog-950 → fog-50`) is for fills and surfaces — tracks, the toggle, an avatar — never for text.

**The accent is spent sparingly.** It marks the one primary action, the active nav row or tab, the focus ring, and live state. Section titles, sparklines, tags and category chips are not the accent.

**Status is beacon or amber, nothing else.** Running / ok / alive: `bg-accent/10 border border-accent/30 text-accent`. Degraded / warn: `bg-warn/10 border border-warn/30 text-warn`. Stopped / neutral: `bg-hover border border-hair text-ink-muted`. Unknown: `border border-hair text-ink-faint`. Dots are `h-1.5 w-1.5 rounded-full`: live `bg-accent`, warn `bg-warn`, off/unknown `bg-fog-600`. Nothing glows. There is no green, no red, no blue.

### Type: Plus Jakarta Sans reads, Plex Mono is a spice

`styles.css` ships Plus Jakarta Sans (variable 200–800; body is 450) and IBM Plex Mono (400/500). Hierarchy comes from weight:

| Role | Classes |
|---|---|
| Page title | `text-title font-bold` (22px / 700) — `PageHeader` does it |
| Section title | `text-heading font-semibold` (16px / 600), sentence case — `SectionHead` does it |
| Heading inside a pane or panel | `subhead` (14px / 600) |
| Body | `text-sm` (14px / 1.6) |
| Table cells | `text-cell` (13.5px) |
| Hints, a right-hand fact on a section line | `text-hint text-ink-muted` (13px) |
| A form control's label | `field-label` (13.5px / 500, sentence case) |
| Quiet heading over a group of rows (menus, the sidebar) | `group-label` (12px / 500, ink-faint) |
| Stat-tile label, table column header | `caps-label` (12px / 500, uppercase) — **the only uppercase in the system** |
| Headline figure | `text-figure` (42px) / `text-figure-sm` (32px) with `font-medium text-ink`; unit in `<span className="unit">` |

`font-mono` is permitted **only** for literal machine strings someone could paste into a terminal: paths, image tags, hostnames and IPs, timestamps in logs, log lines, code, and the value of an input that takes machine syntax (`Field` is mono for that reason). Never for labels, buttons, pills, nav items, hints, prose, or figures. Copy is sentence case everywhere, pills and badges included: "Prune cache", "Add disk", "Resolved".

### Shape and rhythm

**One control height.** Buttons, inputs, selects, the segmented track and the header search are all 32px (`h-8`); a compact action in a dense row is 24px (`Button size="sm"`, `h-6`). Text in a control is 14px / 500. Use `Button`, `Field`, `Select`, `FilterInput` and `Segmented` rather than hand-drawing one; where something must look like a control but can't be one, the `control` class string (from `Field`) or `buttonClass()` (from `Button`) gives the same box.

Radii, strictly: `rounded-xl` dialogs and window-like cards only · `rounded-lg` panels · `rounded-md` buttons, inputs, nav rows, highlighted table rows · `rounded-full` pills, dots, bars. A hairline never gets a radius. Spacing: `gap-2` between buttons, `space-y-4` inside a block, `space-y-12` between page sections, table rows `py-2.5 pl-3.5 pr-2.5`. Forms `max-w-sm`, prose `max-w-lg`, dialogs `max-w-lg`.

### Icons

16px line icons from `lucide-react`, at the control's text colour: one before each sidebar row, optionally before a tab (`TabItem.icon`) or a section title (`SectionHead icon`), and inside a button before its label. An icon names what a row is; it is never decoration on its own.

### Composition rules

- **A group opens with `SectionHead`**: the title in sentence case (16/600), an optional icon before it, an optional one-line `description` under it, and the group's actions — or a right-hand fact in `text-hint text-ink-muted` — at the far end of the line. No rule, no tick, no card around it; the space above the next title separates groups. `Section` renders it for you. A page's status is a `StatusLine` under its `PageHeader`; a list of preferences is `SettingRow`s in a `SettingRows` panel.
- Start a page with `PageHeader`; put `Tabs variant="underline"` in its children, page actions in `actions`, a `StateBadge` in `badge`.
- Metric tiles are not cards: divide cells with `border-l border-hair` on a plain flex row, `px-6 py-5`, `caps-label` on top, `text-figure` below.
- Tables: header row `caps-label border-b border-hair`, cells `text-cell`, row dividers `border-divider`, numbers right-aligned, hover `hover:bg-hover`; a selected row is `bg-accent/10 edge-accent rounded-md`, a warn row `bg-warn/10`.
- Never state a fact before its data exists: show `Loading` (with a label naming what loads), `LoadError` (with retry) or `StateBadge known={false}`, and `StaleNote` under numbers that stopped refreshing.
- Right-align action rows: `flex justify-end gap-2`, `ghost` then `secondary` then `primary`. One `primary` per view.
- `Modal` and `ContextMenu` are rendered only while open (no `open` prop) and close through `onClose`.
- Components in the App-wired group read the fog API and cannot be composed in a design; build those screens from the primitives.

### Where the truth lives

`styles.css` imports `_ds_bundle.css`, the compiled Tailwind sheet: only classes present there exist, and it carries the fog scale, the ink and line tokens, the beacon and amber tokens, the named utilities above, and a safelist of layout utilities (flex/grid, gap/p/m 0–16, w/h/size, max-w, text sizes, rounded, border). Read `components/<group>/<Name>/<Name>.prompt.md` for each component's usage and `<Name>.d.ts` for its props.

### One idiomatic screen

```jsx
const { PageHeader, StateBadge, Button, Tabs, Section, SectionHead, Field, StatusLine } = window.Fog;

<div className="min-h-screen bg-fog-950 p-8 text-ink antialiased">
  <PageHeader
    title="jellyfin"
    description="Media server · jellyfin/jellyfin:10.10.3"
    badge={<StateBadge status={{ state: "running" }} known error={false} />}
    actions={<><Button>Restart</Button><Button variant="primary">Open</Button></>}
  >
    <Tabs variant="underline" label="App sections" active="settings" onChange={() => {}}
      items={[{ id: "overview", label: "Overview" }, { id: "logs", label: "Logs" }, { id: "settings", label: "Settings" }]} />
  </PageHeader>
  <div className="mt-8 space-y-12">
    <div className="flex border-b border-hair">
      <div className="flex-1 px-6 py-5"><p className="caps-label">CPU</p><p className="mt-2.5 text-figure font-medium text-ink">12<span className="unit">%</span></p></div>
      <div className="flex-1 border-l border-hair px-6 py-5"><p className="caps-label">Memory</p><p className="mt-2.5 text-figure font-medium text-ink">5.8<span className="unit">/ 32 GB</span></p></div>
    </div>
    <Section id="hostname" title="Hostname" description="What this app answers to on your network.">
      <div className="max-w-sm space-y-4">
        <Field label="Hostname" defaultValue="media" hint="Reachable as media.fog.lan." />
        <div className="flex justify-end gap-2"><Button variant="ghost">Cancel</Button><Button variant="primary">Save</Button></div>
      </div>
    </Section>
    <Section id="backups" title="Backups">
      <StatusLine tone="ok">Nightly at 02:00 · last kit 41 GB</StatusLine>
    </Section>
    <div>
      <SectionHead title="Peers" right={<span className="text-hint text-ink-muted">2 of 3 reachable</span>} />
      <ul className="mt-4 divide-y divide-divider rounded-lg border border-hair bg-veil px-4 text-cell">
        <li className="flex items-center gap-2 py-2.5"><span className="h-1.5 w-1.5 rounded-full bg-accent" />studio-mini<span className="ml-auto text-ink-muted">12 ms</span></li>
        <li className="flex items-center gap-2 py-2.5"><span className="h-1.5 w-1.5 rounded-full bg-fog-600" />cabin-01<span className="ml-auto text-ink-muted">Unreachable</span></li>
      </ul>
    </div>
  </div>
</div>
```
