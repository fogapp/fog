// The app page's small vocabulary: the pieces the cockpit layout is
// composed from, shared by the tabs and the standing column so a row, a
// pill or a key chip reads the same wherever it sits. Nothing here fetches;
// these are the shapes, the data comes from the tab that renders them.

import type { ReactNode } from "react";
import { SectionHead } from "../../components/Section";

// The page gutter is the shell's; the bands this page draws run edge to
// edge and carry it themselves. Re-exported so the tabs keep one import.
export { GUTTER } from "../../components/Layout";

// A group: a section head, then its body. Groups stack with a 36px gap
// (a column can ask for more), the first flush with the top of its column.
// fill stretches the group down the rest of a flex column, for a pane that
// scrolls inside the frame rather than with the page.
export function Group({ title, right, first, gap = "mt-9", fill, children }: {
  title: ReactNode; right?: ReactNode; first?: boolean; gap?: string; fill?: boolean; children: ReactNode;
}) {
  return (
    <section className={`${first ? "" : gap} ${fill ? "flex min-h-0 flex-1 flex-col" : ""}`}>
      <SectionHead title={title} right={right} />
      <div className={`mt-3 ${fill ? "flex min-h-0 flex-1 flex-col" : ""}`}>{children}</div>
    </section>
  );
}

// A veil: the panel surface — white over a light ground, a lifted step over a dark one.
export function Veil({ children, className = "" }: { children: ReactNode; className?: string }) {
  return <div className={`rounded-lg border border-veil-edge bg-veil ${className}`}>{children}</div>;
}

export type Tone = "ok" | "warn" | "off" | "neutral";

const pillTone: Record<Tone, string> = {
  ok: "border-accent/30 bg-accent/10 text-accent",
  warn: "border-warn/30 bg-warn/10 text-warn",
  off: "border-veil-edge text-ink-faint",
  neutral: "border-veil-edge text-ink-muted",
};

export function Pill({ tone = "neutral", children, title }: { tone?: Tone; children: ReactNode; title?: string }) {
  return (
    <span title={title} className={`whitespace-nowrap rounded-full border px-2.5 py-[3px] text-xs ${pillTone[tone]}`}>
      {children}
    </span>
  );
}

// A state dot: the beacon when live, amber when it needs you, a quiet dot
// when off. Colour alone carries it — no dot glows.
export function Dot({ tone = "ok", title }: { tone?: Tone; title?: string }) {
  const cls =
    tone === "warn" ? "bg-warn"
    : tone === "off" || tone === "neutral" ? "bg-fog-600"
    : "bg-accent";
  return <span title={title} aria-hidden={!title} className={`inline-block h-1.5 w-1.5 shrink-0 rounded-full ${cls}`} />;
}

// A key chip: the shortcut that fires the control it sits in.
export function Key({ children }: { children: ReactNode }) {
  return (
    <kbd className="rounded border border-veil-edge bg-hover px-1.5 py-px font-sans text-xs text-ink-muted">
      {children}
    </kbd>
  );
}

// A 3px bar: how full something is, amber when it needs you.
export function Bar({ pct, tone }: { pct: number; tone?: "warn" }) {
  const w = Math.max(0, Math.min(100, pct));
  return (
    <div className="h-[3px] overflow-hidden rounded-full bg-fog-800">
      <span
        className={`block h-[3px] rounded-full transition-[width] duration-150 ${tone === "warn" ? "bg-warn" : "bg-fog-200"}`}
        style={{ width: `${w}%` }}
      />
    </div>
  );
}

// The table pieces: a small uppercase head over a strong hairline, then rows
// on dividers. cols is a grid-template-columns value; the table pulls
// itself out to the group's edge so the row tint can bleed past the text.
export function Table({ children }: { children: ReactNode }) {
  return <div className="-ml-3.5 -mr-2.5 mt-3">{children}</div>;
}

export function Head({ cols, children }: { cols: string; children: ReactNode }) {
  return (
    <div className="caps-label grid gap-3.5 border-b border-hair-strong pb-2.5 pl-3.5 pr-2.5" style={{ gridTemplateColumns: cols }}>
      {children}
    </div>
  );
}

// A clickable row is a div wearing a button's clothes, not a <button>: rows
// carry their own controls, and a button inside a button is not HTML. It
// still answers Enter and Space, so the keyboard loses nothing.
export function Row({ cols, tone, onClick, className = "", children }: {
  cols: string; tone?: "warn" | "sel"; onClick?: () => void; className?: string; children: ReactNode;
}) {
  const tint = tone === "warn" ? "rounded-md bg-warn/10" : tone === "sel" ? "rounded-md bg-accent/10 edge-accent" : "";
  return (
    <div
      role={onClick ? "button" : undefined}
      tabIndex={onClick ? 0 : undefined}
      onClick={onClick}
      onKeyDown={onClick && ((e) => {
        if (e.target !== e.currentTarget) return;
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClick();
        }
      })}
      className={`grid w-full items-center gap-3.5 border-b border-divider py-2.5 pl-3.5 pr-2.5 text-left text-cell transition-colors ${
        onClick ? "cursor-pointer hover:bg-hover" : ""
      } ${tint} ${className}`}
      style={{ gridTemplateColumns: cols }}
    >
      {children}
    </div>
  );
}

// A label / value / action line inside a veil.
export function Kv({ label, children, action }: { label: ReactNode; children: ReactNode; action?: ReactNode }) {
  return (
    <div className="grid items-center gap-3.5" style={{ gridTemplateColumns: "minmax(0,170px) minmax(0,1fr) auto" }}>
      <span className="text-cell text-ink-muted">{label}</span>
      <span className="min-w-0 break-all font-mono text-cell text-ink">{children}</span>
      <span>{action}</span>
    </div>
  );
}

// The quiet link that sits in a group's right slot: "Open", "Open logs".
export function SlotLink({ children, onClick, to, external }: { children: ReactNode; onClick?: () => void; to?: string; external?: boolean }) {
  const cls = "text-hint font-medium text-accent hover:text-accent-hover";
  if (to) return <a href={to} className={cls} {...(external ? { target: "_blank", rel: "noreferrer" } : {})}>{children}</a>;
  return (
    <button type="button" onClick={onClick} className={cls}>
      {children}
    </button>
  );
}

// Clock times on this page are 24-hour: they sit in mono columns beside
// machine strings, and "PM" is a word where a figure belongs.
const H23 = { hourCycle: "h23" as const };

function clock(d: Date): string {
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", ...H23 });
}

export function hhmmss(d: Date): string {
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", ...H23 });
}

// A time that is today reads as a clock time; older ones carry their day.
export function whenShort(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const now = new Date();
  const time = clock(d);
  if (d.toDateString() === now.toDateString()) return `${time} today`;
  const y = new Date(now);
  y.setDate(now.getDate() - 1);
  if (d.toDateString() === y.toDateString()) return `${time} yesterday`;
  return `${time} ${d.toLocaleDateString([], { day: "numeric", month: "short" })}`;
}
