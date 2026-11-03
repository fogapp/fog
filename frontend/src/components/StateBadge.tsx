import type { AppStatus } from "../api";
import { Spinner } from "./Async";

// One app's container state as a pill. Shared by the header, the app page,
// and basic mode so the same app never reads two different ways depending on
// where you're looking at it.
//
// Two hues only: the beacon for alive, amber for "needs you". A stopped app
// is neutral — off, not wrong. Labels are sentence case.

const pill = "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-[3px] text-xs font-medium";
const pillTone = {
  ok: "border-accent/30 bg-accent/10 text-accent",
  warn: "border-warn/30 bg-warn/10 text-warn",
  neutral: "border-veil-edge bg-hover text-ink-muted",
  unknown: "border-veil-edge text-ink-faint",
} as const;

function sentence(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : s;
}

export default function StateBadge({ status, known, error }: { status?: AppStatus; known: boolean; error: boolean }) {
  // No state yet is its own badge — an app whose container is running must
  // never sit next to a blank space that later fills in with "running".
  if (!status) {
    if (error) return <span className={`${pill} ${pillTone.warn}`}>State unknown</span>;
    if (!known) {
      return (
        <span className={`${pill} ${pillTone.unknown}`}>
          <Spinner size="h-2.5 w-2.5" tone="border-fog-500" />
          Checking…
        </span>
      );
    }
    return null;
  }
  const tone =
    status.state === "running" ? (status.healthy === false ? pillTone.warn : pillTone.ok) : pillTone.neutral;
  const label = status.state === "running" && status.healthy === false ? "Unhealthy" : sentence(status.state);
  return <span className={`${pill} ${tone}`}>{label}</span>;
}

// The same state as a dot, for lists: sidebar rows, tiles, the palette.
// 6px. Colour alone carries the state: the beacon for running, amber for
// unhealthy, a quiet dot for off.
export function StateDot({ app }: { app: AppStatus }) {
  const [cls, title] = app.deploying
    ? ["animate-pulse bg-accent", "deploying"]
    : app.state === "running"
      ? app.healthy === false
        ? ["bg-warn", "unhealthy"]
        : ["bg-accent", "running"]
      : app.state === "new" || app.state === "missing"
        ? ["bg-fog-600", app.state === "new" ? "needs setup" : "missing"]
        : ["bg-fog-600", app.state];
  return <span title={title} className={`h-1.5 w-1.5 shrink-0 rounded-full ${cls}`} />;
}
