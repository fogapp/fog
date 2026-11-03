// The machine's memory as one bar: what the installed apps are measured to
// be using, what the rest of the system takes, what the app in question
// would add — at rest, and the further reach when busy — and what would be
// left. Flat tints on a flat track, in the identity's two hues: the apps
// and the system in greys, the candidate in the beacon, trouble in amber.
// A legend under it names each band; the bar itself carries no text.

import type { FitTone } from "../api";
import { formatBytes } from "../lib/format";

export interface Band {
  label: string;
  bytes: number;
  kind: "apps" | "other" | "add" | "peak";
}

const FILL: Record<Band["kind"], string> = {
  apps: "bg-fog-400",
  other: "bg-fog-600",
  add: "bg-accent",
  peak: "bg-accent/35",
};

export default function CapacityBar({
  total,
  bands,
  tone = "ok",
  free = "free",
}: {
  total: number;
  bands: Band[];
  // Amber replaces the beacon for the candidate's bands when the fit is
  // tight or over — the bar shows the same verdict the words do.
  tone?: FitTone;
  free?: string;
}) {
  if (total <= 0) return null;
  const shown = bands.filter((b) => b.bytes > 0);
  const used = shown.reduce((n, b) => n + b.bytes, 0);
  const left = Math.max(0, total - used);
  const fill = (b: Band) =>
    tone !== "ok" && (b.kind === "add" || b.kind === "peak")
      ? b.kind === "add" ? "bg-warn" : "bg-warn/35"
      : FILL[b.kind];
  return (
    <div className="space-y-1.5">
      <div className="flex h-2.5 w-full overflow-hidden rounded-full bg-fog-800" role="img" aria-label={legendText(shown, left, free)}>
        {shown.map((b) => (
          <span
            key={b.label}
            className={`${fill(b)} h-full shrink-0 border-r border-fog-950/60 last:border-0`}
            style={{ width: `${Math.min(100, (b.bytes / total) * 100)}%` }}
          />
        ))}
      </div>
      <ul className="flex flex-wrap gap-x-4 gap-y-1 text-hint text-ink-muted">
        {shown.map((b) => (
          <li key={b.label} className="flex items-center gap-1.5">
            <span aria-hidden className={`inline-block h-2 w-2 rounded-full ${fill(b)}`} />
            <span>{b.label}</span>
            <span className="tabular-nums text-ink-muted">{formatBytes(b.bytes)}</span>
          </li>
        ))}
        <li className="flex items-center gap-1.5">
          <span aria-hidden className="inline-block h-2 w-2 rounded-full border border-veil-edge bg-fog-800" />
          <span>{free}</span>
          <span className="tabular-nums text-ink-muted">{formatBytes(left)}</span>
        </li>
      </ul>
    </div>
  );
}

function legendText(bands: Band[], left: number, free: string): string {
  return [...bands.map((b) => `${b.label} ${formatBytes(b.bytes)}`), `${free} ${formatBytes(left)}`].join(", ");
}
