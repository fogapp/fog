// The deploy tray: a bottom-left panel, on every page, while any app is
// deploying, updating, or rolling back — one row per app with its stage and
// how far along it is. Fog doesn't know a deploy's percentage; it knows how
// long this app's last few deploys took, so the bar is that clock, held
// short of the end until the deploy really finishes. An app with no history
// (or an update still snapshotting, before its deploy starts) gets a
// sweeping bar and its stage alone. A finished row says how it ended;
// successes clear themselves, failures wait to be dismissed.

import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { get, type DeployStatusResponse, type LiveDeploysResponse } from "../api";
import { Spinner } from "./Async";
import { appsKey } from "../lib/useApps";
import { deployStatusKey } from "../lib/useAppData";

const liveDeploysKey = ["deploys", "live"] as const;

// How long a finished success stays on screen before it clears itself.
const successLinger = 8_000;

type Ended = { app: string; status: "succeeded" | "failed" | "cancelled" | "unknown"; at: number };

export default function DeployTray() {
  const qc = useQueryClient();
  const { data, dataUpdatedAt } = useQuery({
    queryKey: liveDeploysKey,
    queryFn: () => get<LiveDeploysResponse>("/api/deploys"),
    refetchInterval: (q) => ((q.state.data?.deploys.length ?? 0) > 0 ? 1500 : 5000),
  });
  const live = data?.deploys ?? [];
  const [ended, setEnded] = useState<Ended[]>([]);
  const [open, setOpen] = useState(true);
  const now = useNow(live.length > 0);

  // An app that leaves the list has finished: ask how it went, and refresh
  // what the rest of the UI shows about it.
  const seen = useRef<Set<string>>(new Set());
  useEffect(() => {
    if (!data) return;
    const names = new Set(data.deploys.map((d) => d.app));
    const gone = [...seen.current].filter((n) => !names.has(n));
    seen.current = names;
    // An app deploying again drops the row saying how its last one ended.
    setEnded((prev) => (prev.some((e) => names.has(e.app)) ? prev.filter((e) => !names.has(e.app)) : prev));
    if (gone.length === 0) return;
    qc.invalidateQueries({ queryKey: appsKey });
    for (const app of gone) {
      qc.invalidateQueries({ queryKey: deployStatusKey(app) });
      get<DeployStatusResponse>(`/api/apps/${app}/deploy-status`)
        .then((s) => statusOf(s))
        .catch(() => "unknown" as const)
        .then((status) => setEnded((prev) => [...prev.filter((e) => e.app !== app), { app, status, at: Date.now() }]));
    }
  }, [data, qc]);

  // Successes clear themselves.
  useEffect(() => {
    const ok = ended.filter((e) => e.status === "succeeded");
    if (ok.length === 0) return;
    const next = Math.min(...ok.map((e) => e.at)) + successLinger - Date.now();
    const t = setTimeout(() => setEnded((prev) => prev.filter((e) => e.status !== "succeeded" || Date.now() - e.at < successLinger)), Math.max(next, 0));
    return () => clearTimeout(t);
  }, [ended]);

  if (live.length === 0 && ended.length === 0) return null;
  const sinceFetch = Math.max(0, now - dataUpdatedAt);
  const failed = ended.filter((e) => e.status === "failed").length;
  const title =
    live.length > 0
      ? `Deploying ${live.length === 1 ? live[0].app : `${live.length} apps`}`
      : failed > 0
        ? `${failed === 1 ? "A deploy" : `${failed} deploys`} failed`
        : "Deployed";

  return (
    <aside
      aria-label="Deploys"
      className="fixed bottom-4 left-4 z-30 w-80 max-w-[calc(100vw-2rem)] overflow-hidden rounded-lg border border-veil-edge bg-fog-900 text-xs shadow-2xl shadow-black/40 lg:left-[calc(218px+1rem)]"
    >
      <div className="flex items-center gap-2 border-b border-hair px-3 py-2">
        {live.length > 0 && <Spinner />}
        <span className="min-w-0 flex-1 truncate font-medium text-ink" aria-live="polite">{title}</span>
        <button onClick={() => setOpen((o) => !o)} className="text-ink-muted hover:text-ink" title={open ? "Collapse" : "Expand"}>
          {open ? "▾" : "▸"}
        </button>
        {live.length === 0 && (
          <button onClick={() => setEnded([])} className="text-ink-muted hover:text-ink" title="Clear">✕</button>
        )}
      </div>
      {open && (
        <ul className="max-h-72 divide-y divide-divider/60 overflow-y-auto">
          {live.map((d) => {
            const p = progress(d.elapsedMs, d.typicalMs, sinceFetch);
            return (
              <li key={d.app} className="px-3 py-2">
                <div className="flex items-baseline gap-2">
                  <Link to={`/apps/${d.app}`} className="min-w-0 flex-1 truncate font-medium text-ink hover:text-ink">{d.app}</Link>
                  {p && <span className="shrink-0 text-ink-muted">{p.note}</span>}
                </div>
                <div
                  className="relative mt-1.5 h-1 overflow-hidden rounded-full bg-fog-800"
                  role="progressbar"
                  aria-label={`${d.app}: ${d.stage}`}
                  aria-valuemin={0}
                  aria-valuemax={100}
                  aria-valuenow={p?.pct}
                >
                  {p ? (
                    <div className="h-full rounded-full bg-accent transition-[width] duration-500 ease-linear" style={{ width: `${p.pct}%` }} />
                  ) : (
                    <div className="absolute inset-y-0 w-2/5 animate-sweep rounded-full bg-accent" />
                  )}
                </div>
                <p className="mt-1 truncate text-ink-muted" title={d.stage}>{d.stage}</p>
              </li>
            );
          })}
          {ended.map((e) => (
            <li key={e.app} className="flex items-baseline gap-2 px-3 py-2">
              <Link to={`/apps/${e.app}`} className="min-w-0 flex-1 truncate font-medium text-ink hover:text-ink">{e.app}</Link>
              <span className={`shrink-0 ${e.status === "succeeded" ? "text-accent" : e.status === "unknown" ? "text-ink-muted" : "text-warn"}`}>
                {ENDED_LABELS[e.status]}
              </span>
              <button onClick={() => setEnded((prev) => prev.filter((x) => x.app !== e.app))} className="text-ink-muted hover:text-ink" title="Dismiss">✕</button>
            </li>
          ))}
        </ul>
      )}
    </aside>
  );
}

const ENDED_LABELS: Record<Ended["status"], string> = {
  succeeded: "deployed",
  failed: "failed",
  cancelled: "cancelled",
  unknown: "finished",
};

function statusOf(s: DeployStatusResponse): Ended["status"] {
  const last = s.last;
  if (!last) return "unknown";
  if (last.status === "succeeded") return "succeeded";
  if (last.status === "failed") return /cancelled by /.test(last.logTail) ? "cancelled" : "failed";
  return "unknown";
}

// The bar's figure: elapsed against the app's usual deploy time, capped at
// 95% so a bar never reads as done while the deploy still runs. Null when
// there's nothing to measure against.
function progress(elapsedMs: number | undefined, typicalMs: number | undefined, sinceFetch: number) {
  if (!elapsedMs || !typicalMs) return null;
  const elapsed = elapsedMs + sinceFetch;
  const pct = Math.min(95, Math.max(2, (elapsed / typicalMs) * 100));
  const left = typicalMs - elapsed;
  const note = left > 0 ? `~${duration(left)} left` : "longer than usual";
  return { pct: Math.round(pct), note };
}

function duration(ms: number): string {
  const s = Math.ceil(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.round(s / 60);
  return `${m}m`;
}

// A clock that ticks while something is live, so the bar moves between polls.
function useNow(ticking: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!ticking) return;
    const t = setInterval(() => setNow(Date.now()), 500);
    return () => clearInterval(t);
  }, [ticking]);
  return now;
}
