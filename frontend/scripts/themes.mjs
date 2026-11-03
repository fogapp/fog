// Generates src/themes.css and src/themes.generated.ts: the twelve palettes
// an operator can pick from on Settings › Preferences › Appearance, each in a light and a
// dark variant. Run with `npm run themes` after editing the seeds below.
//
// Every palette is the same ladder — the eleven lightness stops the default
// "fog" ramp uses, measured in OKLCH — with its own hue and a chroma
// multiplier, plus its own beacon. Amber stays amber in all of them: "needs
// you" is one colour across the whole system. Text is three tones derived
// from each palette's ramp (ink, muted, faint), and structure is two line
// strengths per mode, so a page can't reach for a fourth grey or a sixth
// border.

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { hexToOklch, oklchToHex, contrast } from "./oklch.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const STOPS = [50, 100, 200, 300, 400, 500, 600, 700, 800, 900, 950];

// The default palette, verbatim from the identity spec. Everything else is
// derived from its lightness ladder.
const FOG = {
  dark: {
    ramp: ["#f2f7fb", "#dce3ea", "#cfd8e2", "#b6c2ce", "#93a1b1", "#8a98a9", "#5a6878", "#2c3742", "#212a34", "#171e26", "#0d1116"],
    accent: "#7ee7d5", accentHover: "#98efe0", accentOn: "#08211d", warn: "#e6a23c", code: "#0a0e12",
  },
  light: {
    ramp: ["#0d1116", "#1c242d", "#2b3540", "#3a4654", "#4a5766", "#5a6878", "#8593a3", "#cfd6de", "#e3e8ee", "#ffffff", "#f8f9fb"],
    accent: "#1f8f7c", accentHover: "#17786a", accentOn: "#ffffff", warn: "#a76a12", code: "#f5f7f9",
  },
};

// Per-mode structure, in every palette. Two line strengths do all the
// drawing: line for panel edges, hairlines and row dividers, line-strong for
// the edge of a control. The role names a page uses (hair, divider, edge…)
// say what a line is for and resolve to one of the two, so the roles stay
// readable in the markup while the values can't drift apart. The veil is
// the surface a panel sits on: a lifted translucent step over a dark
// ground, plain white over a light one.
const LINES = {
  dark: { line: "#ffffff14", "line-strong": "#ffffff26" },
  light: { line: "#00000014", "line-strong": "#00000024" },
};
const ROLES = {
  "veil-edge": "line", hair: "line", "hair-strong": "line", divider: "line",
  edge: "line-strong", "edge-strong": "line-strong",
};
const STRUCTURE = {
  dark: { veil: "#ffffff09", "edge-hover": "#ffffff38", hover: "#ffffff08" },
  light: { veil: "#ffffff", "edge-hover": "#00000038", hover: "#00000008" },
};

// The three text tones, as shares of the way from the ground to ink: muted
// carries descriptions, hints, navigation and meta; faint is for
// placeholders, units and what is disabled.
const TONES = { dark: { muted: 0.64, faint: 0.45 }, light: { muted: 0.66, faint: 0.45 } };

// The lightness is the share; the hue and chroma come from the ramp stop
// the tone replaces (fog-400 for muted, fog-600 for faint), so a palette's
// greys keep its tint.
function tone(ink, ground, like, share) {
  const a = hexToOklch(ink);
  const g = hexToOklch(ground);
  const ref = hexToOklch(like);
  return oklchToHex({ L: g.L + (a.L - g.L) * share, C: ref.C, h: ref.h });
}

function inks(p, mode) {
  const [ink, ground] = [p.ramp[0], p.ramp[10]];
  return {
    ink,
    muted: tone(ink, ground, p.ramp[4], TONES[mode].muted),
    faint: tone(ink, ground, p.ramp[6], TONES[mode].faint),
  };
}

// The hearth ladder: a raised dark, for rooms where fog's own ground reads as
// a hole. The ground is #1f1e1e — a warm, barely-there charcoal, about where
// fog keeps its raised panels — and the surfaces above it keep fog's steps.
// The text stops lift with it so the faint ones still hold off the ground.
// Several palettes share it and differ only by beacon.
const HEARTH = {
  hue: 17, chroma: 0.12,
  surfaces: { dark: [0.383, 0.335, 0.287, 0.236] },
  textLift: { dark: 0.03 },
};

// hue: the ramp's hue. chroma: multiplier on fog's (already faint) chroma;
// 0 is neutral. accent: the beacon's hue and chroma. surfaces: explicit
// lightness for the 700/800/900/950 stops per mode, for palettes that want
// a ground of their own; textLift nudges the text stops.
const SEEDS = [
  { id: "fog", name: "Fog", blurb: "Blue-grey fog with the teal beacon. The default in dark." },
  { id: "graphite", name: "Graphite", blurb: "Neutral greys, a steel-blue beacon. The default in light.", hue: 250, chroma: 0, accent: { hue: 245, chroma: 0.11 } },
  { id: "ink", name: "Ink", blurb: "True black and paper white. The highest contrast.", hue: 250, chroma: 0, accent: { hue: 262, chroma: 0.13 },
    surfaces: { dark: [0.27, 0.22, 0.17, 0], light: [0.9, 0.95, 0.975, 1] }, textLift: { dark: 0.03, light: -0.06 } },
  { id: "charcoal", name: "Charcoal", blurb: "A softer dark on warm charcoal, the teal beacon.", ...HEARTH, accent: { hue: 180, chroma: 0.1 } },
  { id: "flint", name: "Flint", blurb: "Warm charcoal, a cornflower beacon.", ...HEARTH, accent: { hue: 258, chroma: 0.1 } },
  { id: "sage", name: "Sage", blurb: "Warm charcoal, a sage beacon.", ...HEARTH, accent: { hue: 140, chroma: 0.08 } },
  { id: "heather", name: "Heather", blurb: "Warm charcoal, a heather beacon.", ...HEARTH, accent: { hue: 305, chroma: 0.09 } },
  { id: "slate", name: "Slate", blurb: "Indigo-grey, a frost-blue beacon.", hue: 268, chroma: 1.9, accent: { hue: 232, chroma: 0.11 } },
  { id: "ocean", name: "Ocean", blurb: "Deep blue, a cyan beacon.", hue: 238, chroma: 2.6, accent: { hue: 205, chroma: 0.12 } },
  { id: "mint", name: "Mint", blurb: "Cool teal-grey, a mint beacon.", hue: 185, chroma: 1.7, accent: { hue: 160, chroma: 0.11 } },
  { id: "forest", name: "Forest", blurb: "Green-black, a leaf beacon.", hue: 152, chroma: 1.9, accent: { hue: 138, chroma: 0.12 } },
  { id: "olive", name: "Olive", blurb: "Olive drab, a chartreuse beacon.", hue: 112, chroma: 1.7, accent: { hue: 105, chroma: 0.12 } },
  { id: "ember", name: "Ember", blurb: "Warm charcoal, a coral beacon.", hue: 38, chroma: 1.5, accent: { hue: 28, chroma: 0.13 } },
  { id: "dune", name: "Dune", blurb: "Sepia sand, a gold beacon.", hue: 72, chroma: 1.6, accent: { hue: 55, chroma: 0.12 } },
  { id: "plum", name: "Plum", blurb: "Aubergine grey, a lavender beacon.", hue: 312, chroma: 1.5, accent: { hue: 298, chroma: 0.12 } },
  { id: "rose", name: "Rose", blurb: "Dusty rose grey, a rose beacon.", hue: 358, chroma: 1.5, accent: { hue: 350, chroma: 0.12 } },
];

const ladder = { dark: FOG.dark.ramp.map(hexToOklch), light: FOG.light.ramp.map(hexToOklch) };
const beacon = {
  dark: { accent: hexToOklch(FOG.dark.accent), hover: hexToOklch(FOG.dark.accentHover), on: hexToOklch(FOG.dark.accentOn) },
  light: { accent: hexToOklch(FOG.light.accent), hover: hexToOklch(FOG.light.accentHover) },
};

function derive(seed, mode) {
  if (seed.id === "fog") return FOG[mode];
  const lift = seed.textLift?.[mode] ?? 0;
  const surfaces = seed.surfaces?.[mode];
  const ramp = ladder[mode].map((o, i) => {
    const s = STOPS[i] >= 700 ? i - 7 : -1; // 700→0 … 950→3
    let L = s >= 0 && surfaces ? surfaces[s] : o.L + (s >= 0 ? 0 : lift);
    L = Math.min(1, Math.max(0, L));
    return oklchToHex({ L, C: o.C * seed.chroma, h: seed.hue });
  });
  const b = beacon[mode];
  const C = seed.accent.chroma;
  const accent = oklchToHex({ L: b.accent.L, C, h: seed.accent.hue });
  const accentHover = oklchToHex({ L: b.hover.L, C: C * 0.85, h: seed.accent.hue });
  const accentOn = mode === "dark" ? oklchToHex({ L: beacon.dark.on.L, C: beacon.dark.on.C, h: seed.accent.hue }) : "#ffffff";
  const ground = hexToOklch(ramp[10]);
  const code = mode === "dark"
    ? oklchToHex({ L: Math.max(0, ground.L - 0.015), C: ground.C, h: seed.hue })
    : oklchToHex({ L: 0.975, C: ground.C * 0.6, h: seed.hue });
  return { ramp, accent, accentHover, accentOn, warn: FOG[mode].warn, code };
}

function block(selector, p, mode, withStructure) {
  const lines = [`${selector} {`];
  if (withStructure) lines.push(`  color-scheme: ${mode};`);
  p.ramp.forEach((hex, i) => lines.push(`  --color-fog-${STOPS[i]}: ${hex};`));
  const t = inks(p, mode);
  lines.push(`  --color-ink: ${t.ink};`, `  --color-ink-muted: ${t.muted};`, `  --color-ink-faint: ${t.faint};`);
  lines.push(`  --color-accent: ${p.accent};`, `  --color-accent-hover: ${p.accentHover};`, `  --color-accent-on: ${p.accentOn};`);
  lines.push(`  --color-warn: ${p.warn};`, `  --color-code: ${p.code};`);
  if (withStructure) {
    for (const [k, v] of Object.entries(LINES[mode])) lines.push(`  --color-${k}: ${v};`);
    for (const [k, v] of Object.entries(ROLES)) lines.push(`  --color-${k}: var(--color-${v});`);
    for (const [k, v] of Object.entries(STRUCTURE[mode])) lines.push(`  --color-${k}: ${v};`);
  }
  lines.push("}");
  return lines.join("\n");
}

// The floors a palette must clear, on its own ground. Body text is AAA,
// labels and the beacon-as-text AA-large. The beacon's own label is held
// to where the identity's default light beacon already sits (white on
// #1f8f7c is 3.98): no palette may be worse than fog.
function audit(id, mode, p) {
  const g = p.ramp[10];
  const t = inks(p, mode);
  const checks = [
    ["ink on ground", contrast(t.ink, g), 7],
    ["ink-muted on ground", contrast(t.muted, g), 4.5],
    ["ink-muted on fog-900", contrast(t.muted, p.ramp[9]), 4.5],
    ["ink-faint on ground", contrast(t.faint, g), 2.5],
    ["fog-100 on ground", contrast(p.ramp[1], g), 7],
    ["fog-200 on ground", contrast(p.ramp[2], g), 4.5],
    ["fog-300 on ground", contrast(p.ramp[3], g), 4.5],
    ["fog-400 on ground", contrast(p.ramp[4], g), 3],
    ["fog-500 on ground", contrast(p.ramp[5], g), 3],
    ["accent on ground", contrast(p.accent, g), 3],
    ["accent-on on accent", contrast(p.accentOn, p.accent), 3.9],
    ["warn on ground", contrast(p.warn, g), 3],
    ["fog-100 on fog-900", contrast(p.ramp[1], p.ramp[9]), 4.5],
  ];
  const failures = checks.filter(([, v, min]) => v < min);
  for (const [what, v, min] of failures) console.error(`FAIL ${id}/${mode}: ${what} = ${v.toFixed(2)} (< ${min})`);
  return failures.length;
}

let failures = 0;
const css = [
  "/* Generated by scripts/themes.mjs — edit the seeds there, not this file.",
  "   Selectors carry no element, so a swatch can be a <div data-theme",
  "   data-palette> and render its palette with the page's own utilities. */",
  "",
  block('[data-theme="dark"]', FOG.dark, "dark", true),
  "",
  block('[data-theme="light"]', FOG.light, "light", true),
];
const meta = [];
for (const seed of SEEDS) {
  for (const mode of ["dark", "light"]) {
    const p = derive(seed, mode);
    failures += audit(seed.id, mode, p);
    if (seed.id !== "fog") css.push("", block(`[data-theme="${mode}"][data-palette="${seed.id}"]`, p, mode, false));
  }
  meta.push(`  { id: "${seed.id}", name: "${seed.name}", blurb: ${JSON.stringify(seed.blurb)} },`);
}
if (failures) {
  console.error(`${failures} contrast floor(s) missed; nothing written.`);
  process.exit(1);
}
writeFileSync(join(here, "../src/themes.css"), css.join("\n") + "\n");
writeFileSync(
  join(here, "../src/themes.generated.ts"),
  [
    "// Generated by scripts/themes.mjs — edit the seeds there, not this file.",
    "// The palettes on Settings › Preferences › Appearance, in display order. Their colours",
    "// live in themes.css; this is only the vocabulary.",
    "",
    "export const THEMES = [",
    ...meta,
    "] as const;",
    "",
    "export type ThemeId = (typeof THEMES)[number][\"id\"];",
    "",
    "export function isThemeId(v: unknown): v is ThemeId {",
    "  return typeof v === \"string\" && THEMES.some((t) => t.id === v);",
    "}",
    "",
  ].join("\n"),
);
console.log(`wrote ${SEEDS.length} palettes × 2 modes; all contrast floors met.`);
