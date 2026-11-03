import { useSyncExternalStore } from "react";
import { isThemeId, type ThemeId } from "./themes.generated";

// Appearance is a per-browser preference, not server state: fog is
// single-operator, but one operator can sit at a bright desk and a dark
// bedroom on the same account. "system" is one of the choices and stays
// live — it follows the OS the moment the OS changes, without a reload.
//
// Two things are remembered: which mode (system / light / dark), and which
// palette each mode wears. The document carries both as data attributes,
// so CSS needs one pair of selectors and never has to reason about
// "system" itself. Out of the box fog is light and wears Graphite; the
// dark slot defaults to Fog. The boot scripts in index.html repeat these
// defaults, so change them there too.

export type ThemePref = "system" | "light" | "dark";
export type Theme = "light" | "dark";
export type Palettes = Record<Theme, ThemeId>;

const KEY = "fog.theme";
const paletteKey = (t: Theme) => `fog.palette.${t}`;
const DEFAULT_PREF: ThemePref = "light";
const DEFAULT_PALETTE: Palettes = { light: "graphite", dark: "fog" };
const query = () => window.matchMedia("(prefers-color-scheme: light)");

function readPref(): ThemePref {
  try {
    const v = localStorage.getItem(KEY);
    return v === "light" || v === "dark" || v === "system" ? v : DEFAULT_PREF;
  } catch {
    return DEFAULT_PREF;
  }
}

function readPalette(t: Theme): ThemeId {
  try {
    const v = localStorage.getItem(paletteKey(t));
    return isThemeId(v) ? v : DEFAULT_PALETTE[t];
  } catch {
    return DEFAULT_PALETTE[t];
  }
}

function resolve(pref: ThemePref): Theme {
  if (pref !== "system") return pref;
  return query().matches ? "light" : "dark";
}

// The browser chrome's colour follows the ground, whatever palette it is —
// read back from the stylesheet rather than kept as a second table here.
function paintChrome(theme: Theme) {
  const meta = document.querySelector('meta[name="theme-color"]');
  if (!meta) return;
  const ground = getComputedStyle(document.documentElement).getPropertyValue("--color-fog-950").trim();
  meta.setAttribute("content", ground || (theme === "light" ? "#f9f9f9" : "#0d1116"));
}

function paint(theme: Theme, palette: ThemeId) {
  document.documentElement.dataset.theme = theme;
  document.documentElement.dataset.palette = palette;
  // Next frame: in development the stylesheet may still be on its way in
  // when this module first runs, and the ground would read back empty.
  requestAnimationFrame(() => paintChrome(theme));
}

const listeners = new Set<() => void>();
let pref = readPref();
let palettes: Palettes = { light: readPalette("light"), dark: readPalette("dark") };

function announce() {
  paint(resolve(pref), palettes[resolve(pref)]);
  for (const l of listeners) l();
}

function setTheme(next: ThemePref) {
  pref = next;
  try {
    localStorage.setItem(KEY, next);
  } catch {
    // Private-mode browsers still get the theme, just not the memory of it.
  }
  announce();
}

function setPalette(mode: Theme, id: ThemeId) {
  palettes = { ...palettes, [mode]: id };
  try {
    localStorage.setItem(paletteKey(mode), id);
  } catch {
    // As above.
  }
  announce();
}

function subscribe(onChange: () => void) {
  listeners.add(onChange);
  const mq = query();
  // Only matters while the preference is "system", but staying subscribed is
  // cheaper than tearing the listener down every time the choice changes.
  const onSystem = () => announce();
  // Another tab changing the preference should not leave this one stale.
  const onStorage = (e: StorageEvent) => {
    if (e.key === null || e.key === KEY || e.key.startsWith("fog.palette.")) {
      pref = readPref();
      palettes = { light: readPalette("light"), dark: readPalette("dark") };
      announce();
    }
  };
  mq.addEventListener("change", onSystem);
  window.addEventListener("storage", onStorage);
  return () => {
    listeners.delete(onChange);
    mq.removeEventListener("change", onSystem);
    window.removeEventListener("storage", onStorage);
  };
}

// The boot script in index.html painted before first paint from the same
// storage; this repeats it so the theme-color meta reflects the palette too.
paint(resolve(pref), palettes[resolve(pref)]);

export function useTheme(): {
  pref: ThemePref;
  theme: Theme;
  palettes: Palettes;
  setTheme: (p: ThemePref) => void;
  setPalette: (mode: Theme, id: ThemeId) => void;
} {
  const snapshot = useSyncExternalStore(
    subscribe,
    () => `${pref}:${resolve(pref)}:${palettes.light}:${palettes.dark}`,
    () => `${DEFAULT_PREF}:${DEFAULT_PREF}:${DEFAULT_PALETTE.light}:${DEFAULT_PALETTE.dark}`,
  );
  const [p, t, light, dark] = snapshot.split(":") as [ThemePref, Theme, ThemeId, ThemeId];
  return { pref: p, theme: t, palettes: { light, dark }, setTheme, setPalette };
}
