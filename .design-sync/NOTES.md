# design-sync notes for fog

fog is an application, not a component library: no Storybook, no `dist/`, no
package entry. The sync builds one from the app's own source. Everything the
converter needs that the app doesn't provide lives under `.design-sync/` and
is produced by `cfg.buildCmd` (`sh .design-sync/prebuild.sh`) before every
build:

- `frontend/src/design-system.ts` is the export barrel (`cfg.entry`). Most
  components are default exports, and a synthesized `export *` entry drops
  those, so the barrel is hand-maintained. **Adding a component to
  `frontend/src/components/` means adding it to the barrel AND to
  `cfg.componentSrcMap`** (there is no `.d.ts` export list to discover from
  otherwise), and writing a `.design-sync/docs/<Name>.md` (category + usage).
- `frontend/package.json` `"types"` points at `.ds-types/src/design-system.d.ts`,
  a gitignored tree `prebuild.sh` emits with `tsc --emitDeclarationOnly`. Without
  it every `<Name>Props` came out as `[key: string]: unknown`. The field has no
  effect on the app (private package).
- `frontend/.ds-styles.css` (`cfg.cssEntry`, gitignored) is compiled by
  `.design-sync/build-css.mjs` with the Tailwind v4 compiler the Vite plugin
  uses, over `frontend/src` plus the safelist in `.design-sync/safelist.css`.
  The Vite build's own CSS is content-scanned (only classes the app uses) and
  lands under a hashed name outside `frontend/`, so it can't be the entry.
  Add to the safelist when the design agent needs a utility the app never used.
- `cfg.dtsPropsFor` hand-writes props for components whose inferred types
  referenced app-only types (`AppStatus`, `MenuItem`, `TabItem<T>`, API
  records) or lost inherited HTML attributes (Button, Field). Re-check these
  when those components' props change.

Previews:

- The card harness paints a white page; fog's components assume the dark
  `<body>` ground from `index.css`. Every authored preview wraps its cells in
  `.design-sync/preview-lib/surface.tsx` (`bg-fog-950 text-fog-100`). That
  helper is NOT a component preview; previews import it relatively.
- `previewExamples` only reads `export const X = ...`; previews here use
  `export function X()` on purpose, so the Surface wrapper never leaks into
  `.prompt.md`. Usage examples come from `.design-sync/docs/<Name>.md`.
- Fixed-position components (Modal, ContextMenu) render in `cardMode: single`
  inside a sized page block (`h-96` / `h-80`): the card's `translateZ(0)` makes
  that block the containing block for `fixed`, so the overlay covers the block.
- `Layout`, `Sidebar`, `CommandPalette` need react-router;
  everything react-query-bound needs a QueryClient and live fogd. No
  `cfg.provider` is set on purpose (it would tell the design agent to wrap
  every tree in a router), so those 15 ship as floor cards.
- `Tabs` previews use query-routed mode (no `to`) so no Router is needed.
- Group precedence: a doc's `category` only applies to components whose
  source-dir group is `general`; `PeerShareComposer` lives in
  `components/files/` and therefore stays in the `files` group even though its
  doc says App-wired.

Known render warns: none (validate is clean; 15 floor cards are unauthored,
not warnings).

Fonts: fog self-hosts Plus Jakarta Sans (variable 200–800, body at 450) and
IBM Plex Mono (400/500) from `frontend/public/fonts/` via
`frontend/src/fonts.css`; `cfg.extraFonts: ["src/fonts.css"]` ships them to
`fonts/`. The build log's "dead @font-face block(s) dropped" is the compiled
sheet's copies being replaced by the shipped ones — expected. (Plex Sans was
the reading face from 2026-09-02 until the refresh on 2026-10-29.)

Identity: two hues (beacon `accent`, amber `warn`) since 2026-09-02. The
refresh of 2026-10-29 (docs/design/fog-identity.md) moved text onto three
tokens (`text-ink`, `text-ink-muted`, `text-ink-faint`) — `text-fog-*` is
gone from source and from the safelist — collapsed the structure tokens to
two line strengths, set every control at 32px, retired the section motif
(label · tick · hairline) for sentence-case section titles, dropped the
glows and `animate-breathe`, and added lucide-react icons. `caps-label`,
`field-label`, `group-label`, `subhead`, `unit`, `edge-accent` and the
`text-cell/hint/label/title/heading/figure` steps are `@utility`/`@theme`
names in `frontend/src/index.css` — the design agent gets them through the
safelist and the conventions header. A retheme never invalidates grades on
its own: re-run `package-capture.mjs --force` so previews are recaptured and
regraded, or the old grades carry forward.

## Re-sync risks

- The README token list is generated from `_ds_bundle.css` and carries
  `--tw-*` internals alongside the real fog tokens (`--color-fog-*`,
  `--color-accent`, `--color-accent-hover`, `--color-code`); the conventions
  header names the real ones. (Until 2026-09-03 it was also dominated by
  `--xy-*`: FederationMap imported xyflow's stylesheet. That component and
  the dependency are gone — the Federation page draws its own SVG map.)
- `dtsPropsFor` bodies are inlined copies: a change to `AppStatus`,
  `MenuItem`, `TabItem`, `ButtonProps` or Field's input attrs in source must be
  mirrored in `.design-sync/config.json`.
- `safelist.css` is a hand-picked utility set; utilities outside it don't
  exist in designs even though they exist in Tailwind.
- Toolchain: node 24, tailwindcss 4.3.x (`@source inline` needs ≥4.1),
  playwright 1.62.1 pinned in `.ds-sync/` to match the cached chromium-1234.
- The `.d.ts` tree and stylesheet are gitignored build outputs: on a fresh
  clone run `sh .design-sync/prebuild.sh` before the converter (buildCmd).

## Re-sync invocation (recorded 2026-09-02)

- Fetch the anchor (`DesignSync get_file _ds_sync.json` → `.design-sync/.cache/remote-sync.json`),
  run `sh .design-sync/prebuild.sh`, then from the repo root:
  `node .ds-sync/resync.mjs --config .design-sync/config.json --node-modules frontend/node_modules --out ./ds-bundle --remote .design-sync/.cache/remote-sync.json`
  (the entry comes from `cfg.entry`; no `--entry` needed). `finalize_plan` needs an
  absolute `localDir` (`/…/fog/ds-bundle`) — a relative one resolves against the
  wrong cwd from inside the tool.
- A new primitive with no authored preview trips `[RENDER_BLANK]` on its floor
  card (Toggle did, 2026-09-02): author `.design-sync/previews/<Name>.tsx`, re-run
  the driver, grade the sheet, re-run once more for the receipt.
- A prop added to an already-authored component (Tabs `inline`, 2026-09-03)
  does not show up as `changed`: grades key on the preview `.tsx` and the
  preview-affecting config, and a defaulted prop leaves the render hash alone.
  Add a cell for the new prop to `.design-sync/previews/<Name>.tsx` (so the
  design agent can see it) and the driver regrades just that component.
- `list_files` on the project also returns the Claude Design app's own files
  (`design_handoff_*/`, the page `.html` exports at the root,
  `_ds_manifest.json`, `_adherence.oxlintrc.json`, `tweaks-panel.jsx`). They
  are not sync output; never put them in a plan's `deletes`.
