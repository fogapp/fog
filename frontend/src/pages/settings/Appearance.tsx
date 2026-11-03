// Settings › Preferences › Appearance: how fog looks to the person at this
// keyboard — a mode (follow the OS, or pin light or dark) and a palette for
// each mode, so the bright-desk fog and the bedroom fog can each be their
// own colour; those are browser preferences and touch nothing on the
// server. Then the one server-wide choice about looks: how much of fog the
// household sees at all.

import type { ReactNode } from "react";
import { Spinner } from "../../components/Async";
import { Section } from "../../components/Section";
import Segmented from "../../components/Segmented";
import { useSettingToggle } from "../../lib/useSettingToggle";
import { useTheme, type Theme, type ThemePref } from "../../theme";
import { THEMES, type ThemeId } from "../../themes.generated";
import { SettingsPage } from "./Frame";

export default function Appearance() {
  const { pref, theme, palettes, setTheme, setPalette } = useTheme();
  return (
    <SettingsPage>
      <ModeSection pref={pref} theme={theme} setTheme={setTheme} />
      <PaletteSection mode="light" showing={theme === "light"} selected={palettes.light} onPick={(id) => setPalette("light", id)} />
      <PaletteSection mode="dark" showing={theme === "dark"} selected={palettes.dark} onPick={(id) => setPalette("dark", id)} />
      <InterfaceSection />
    </SettingsPage>
  );
}

const MODES: { id: ThemePref; label: string; hint: string }[] = [
  { id: "system", label: "System", hint: "Follows the operating system, and changes with it." },
  { id: "light", label: "Light", hint: "Always the light theme." },
  { id: "dark", label: "Dark", hint: "Always the dark theme." },
];

function ModeSection({ pref, theme, setTheme }: { pref: ThemePref; theme: Theme; setTheme: (p: ThemePref) => void }) {
  return (
    <Section
      id="mode"
      title="Mode"
      description="Remembered by this browser only. The avatar menu carries the same switch as a shortcut."
    >
      <div className="flex flex-wrap items-center gap-3">
        <Segmented label="Mode" options={MODES} value={pref} onChange={setTheme} />
        <span className="text-sm text-ink-muted">
          {pref === "system" ? `The system is ${theme} right now.` : MODES.find((m) => m.id === pref)!.hint}
        </span>
      </div>
    </Section>
  );
}

// One mode's palettes. The section that is on screen right now says
// so, because picking a palette for the other mode changes nothing visible
// until that mode comes round — a silent click would read as broken.
function PaletteSection({
  mode,
  showing,
  selected,
  onPick,
}: {
  mode: Theme;
  showing: boolean;
  selected: ThemeId;
  onPick: (id: ThemeId) => void;
}) {
  const current = THEMES.find((t) => t.id === selected)!;
  return (
    <Section
      id={mode}
      title={`${mode === "light" ? "Light" : "Dark"} theme`}
      description={
        showing ? (
          <>Showing now. <span className="text-ink">{current.name}</span> — {current.blurb}</>
        ) : (
          <>Applies whenever fog is in {mode} mode. Currently <span className="text-ink">{current.name}</span>.</>
        )
      }
      actions={showing ? <span className="text-hint font-medium text-accent">Showing now</span> : undefined}
    >
      <div role="radiogroup" aria-label={`${mode} palette`} className="grid grid-cols-2 gap-x-4 gap-y-5 sm:grid-cols-3 lg:grid-cols-4">
        {THEMES.map((t) => (
          <Swatch key={t.id} mode={mode} id={t.id} name={t.name} selected={t.id === selected} onPick={() => onPick(t.id)} />
        ))}
      </div>
    </Section>
  );
}

// A palette rendered in miniature with the page's own utilities: the
// swatch carries data-theme and data-palette, so every token inside it
// resolves to that palette rather than the page's. What it shows is a
// screen — sidebar with its active row, a section title over a panel of two
// rows, a primary and a quiet button, an amber dot — not a strip of colours.
function Swatch({ mode, id, name, selected, onPick }: { mode: Theme; id: ThemeId; name: string; selected: boolean; onPick: () => void }) {
  return (
    // The swatch is the control — a picture of the palette, not a labelled
    // button — so it is the one hand-drawn button on a Settings page.
    // eslint-disable-next-line no-restricted-syntax
    <button type="button" role="radio" aria-checked={selected} aria-label={name} onClick={onPick} className="group block w-full text-left">
      <Preview mode={mode} id={id} selected={selected} />
      <span className="mt-1.5 flex items-center gap-1.5 text-cell">
        <span className={selected ? "font-medium text-ink" : "text-ink-muted group-hover:text-ink"}>{name}</span>
        {selected && (
          <span aria-hidden className="h-1.5 w-1.5 rounded-full bg-accent" />
        )}
      </span>
    </button>
  );
}

function Preview({ mode, id, selected }: { mode: Theme; id: ThemeId; selected: boolean }) {
  return (
    <span
      data-theme={mode}
      data-palette={id}
      aria-hidden
      className={`block overflow-hidden rounded-md border ${
        selected ? "border-accent ring-2 ring-accent/30" : "border-edge group-hover:border-edge-hover"
      }`}
    >
      <span className="flex h-[4.5rem] w-full bg-fog-950 font-sans text-ink">
        {/* Sidebar: the wordmark's beacon, three items, one of them active. */}
        <span className="flex w-9 shrink-0 flex-col gap-1.5 border-r border-hair px-1.5 pt-2">
          <span className="mb-0.5 h-1.5 w-1.5 rounded-full bg-accent" />
          <Bar w="w-4" tone="bg-fog-500" />
          <span className="-mx-0.5 flex h-2.5 items-center rounded-[2px] bg-accent/15 px-0.5">
            <Bar w="w-5" tone="bg-accent" />
          </span>
          <Bar w="w-3.5" tone="bg-fog-500" />
        </span>
        {/* Page: a section title, a panel of two rows, a control row. */}
        <span className="flex min-w-0 flex-1 flex-col gap-1.5 px-2 pt-2">
          <Bar w="w-6" tone="bg-ink" />
          <span className="flex flex-col gap-1 rounded-[3px] border border-hair bg-veil px-1 py-1">
            <span className="flex items-center gap-1.5">
              <Bar w="w-8" tone="bg-ink-muted" />
              <Bar w="w-3" tone="bg-ink-faint" />
            </span>
            <span className="flex items-center gap-1.5">
              <Bar w="w-6" tone="bg-ink-muted" />
              <span className="h-1 w-1 rounded-full bg-warn" />
              <Bar w="w-4" tone="bg-ink-faint" />
            </span>
          </span>
          <span className="mt-auto mb-2 flex items-center gap-1">
            <span className="h-2.5 w-6 rounded-[3px] bg-accent" />
            <span className="h-2.5 w-5 rounded-[3px] border border-edge bg-veil" />
          </span>
        </span>
      </span>
    </span>
  );
}

function Bar({ w, tone }: { w: string; tone: string }): ReactNode {
  return <span className={`block h-1 rounded-full ${w} ${tone}`} />;
}

// The Basic/Advanced switch. It applies on click — a mode change should
// take effect the moment it's chosen, not sit behind a Save button — and
// the highlighted side always reflects what the server has stored. The
// avatar menu carries the same switch as a shortcut; this is where it is
// explained.
const UI_MODES = [
  { id: "basic", label: "Basic" },
  { id: "advanced", label: "Advanced" },
] as const;

function InterfaceSection() {
  // The highlight only moves once the stored setting came back.
  const setting = useSettingToggle("ui_mode", { errorLabel: "Couldn't switch modes" });
  const { settings, error } = setting;

  // No claim about the current mode until the settings actually arrive.
  const mode = setting.value === undefined ? undefined : setting.value === "basic" ? "basic" : "advanced";

  return (
    <Section
      id="interface"
      title="Interface mode"
      description="For everyone on this server, not just this browser. Basic mode reduces fog to an app list with stop, restart, update, and open — for a household that just uses the apps. Everything else comes back from the avatar menu → Switch to advanced mode."
    >
      <div className="flex items-center gap-3">
        <Segmented label="Interface mode" options={UI_MODES} value={mode} onChange={setting.set} disabled={setting.isPending} />
        {!mode && !settings.isError && <Spinner tone="border-fog-500" />}
        {!mode && settings.isError && <span className="text-sm text-warn">current mode unavailable</span>}
      </div>
      {error && <p className="text-sm text-warn">{error}</p>}
    </Section>
  );
}
