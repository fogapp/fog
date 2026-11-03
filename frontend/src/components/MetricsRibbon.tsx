import { useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { get, type MetricSample, type SystemMetrics } from "../api";
import { StaleNote, errorText } from "./Async";
import Button from "./Button";
import { formatBytes } from "../lib/format";

// The host vitals strip: CPU, memory, disk, and load as one connected,
// borderless row pinned directly under the header — cells divided by
// hairlines, not cards; a label, a headline figure with its unit, and a
// sparkline of the sampler's retained hour. It draws only samples that
// exist: a fresh server says "collecting", never an invented flat line.
// <main> has no padding of its own, so the strip already runs edge to edge
// and only its contents need the page gutter.

type Point = { t: number; v: number };

export default function MetricsRibbon() {
  const metrics = useQuery({
    queryKey: ["system-metrics"],
    queryFn: () => get<SystemMetrics>("/api/system/metrics"),
    refetchInterval: 10000,
  });

  const samples: MetricSample[] = metrics.data?.samples ?? [];
  const last = samples.at(-1);

  if (metrics.isError && !metrics.data) {
    return (
      <section className="flex items-center gap-3 border-b border-hair px-4 py-3 sm:px-6 lg:px-8">
        <p className="text-sm text-ink-muted">
          Couldn't load host metrics. <span className="font-mono text-xs text-ink-muted">{errorText(metrics.error, "")}</span>
        </p>
        <Button size="sm" onClick={() => metrics.refetch()}>
          Retry
        </Button>
      </section>
    );
  }

  const spanMin =
    samples.length >= 2 ? Math.max(1, Math.round((samples[samples.length - 1].t - samples[0].t) / 60)) : 0;
  const caption = !metrics.data
    ? "measuring…"
    : samples.length < 2
      ? "collecting first samples…"
      : spanMin >= 60
        ? "last hour"
        : `last ${spanMin} min`;

  const freeDisk = last ? last.diskTotal - last.diskUsed : 0;
  const lowDisk = !!last && last.diskTotal > 0 && freeDisk < last.diskTotal * 0.1;

  return (
    <section className="relative border-b border-hair">
      <span className="pointer-events-none absolute right-3 top-2 z-10 text-[10px] text-ink-muted">
        {caption}
        {metrics.isError && metrics.data ? " · may be stale" : ""}
      </span>
      <div className="overflow-x-auto">
        <div className="flex">
          <MetricCell
            label="CPU"
            figure={last ? split(`${last.cpuPct.toFixed(0)}%`) : undefined}
            sub={last ? "all cores" : undefined}
            points={samples.map((s) => ({ t: s.t, v: s.cpuPct }))}
            max={100}
            format={(v) => `${v.toFixed(1)}%`}
          />
          <MetricCell
            label="Memory"
            figure={last ? split(formatBytes(last.memUsed), last.memTotal ? ` / ${formatBytes(last.memTotal)}` : "") : undefined}
            points={samples.map((s) => ({ t: s.t, v: s.memUsed }))}
            max={last?.memTotal}
            format={formatBytes}
          />
          <MetricCell
            label="Disk"
            figure={last?.diskTotal ? split(formatBytes(last.diskUsed), ` / ${formatBytes(last.diskTotal)}`) : undefined}
            sub={
              last?.diskTotal
                ? lowDisk
                  ? `only ${formatBytes(freeDisk)} free`
                  : `${formatBytes(freeDisk)} free`
                : undefined
            }
            subTone={lowDisk ? "warn" : undefined}
            points={samples.map((s) => ({ t: s.t, v: s.diskUsed }))}
            max={last?.diskTotal}
            format={formatBytes}
          />
          <MetricCell
            label="Load"
            figure={last ? split(last.load1.toFixed(2)) : undefined}
            sub={last ? "1-minute average" : undefined}
            points={samples.map((s) => ({ t: s.t, v: s.load1 }))}
            format={(v) => v.toFixed(2)}
          />
        </div>
      </div>
      {metrics.isError && metrics.data && (
        <div className="px-4 pb-2 sm:px-6 lg:px-8">
          <StaleNote what="these host figures" />
        </div>
      )}
    </section>
  );
}

// A headline figure is the number and, at 0.4em beside it, its unit: "12"
// and "%", "5.8" and "/ 32 GB". Formatters return one string, so the unit is
// whatever trails the last digit, plus an optional "of total" suffix.
type Figure = { value: string; unit: string };
function split(s: string, suffix = ""): Figure {
  const m = s.match(/^(.*?\d)\s*([^\d]*)$/);
  return m ? { value: m[1], unit: `${m[2]}${suffix}`.trim() } : { value: s, unit: suffix.trim() };
}

function MetricCell({ label, figure, sub, subTone, points, max, format }: {
  label: string;
  figure?: Figure; // undefined until a sample exists — the cell says so
  sub?: string;
  subTone?: "warn";
  points: Point[];
  max?: number; // fixed top of scale; defaults to the data max with headroom
  format: (v: number) => string;
}) {
  const [hover, setHover] = useState<number | null>(null);
  const hovered = hover != null ? points[hover] : null;

  return (
    <div className="relative h-36 min-w-44 flex-1 overflow-hidden border-l border-hair first:border-l-0">
      <div className="pointer-events-none relative z-10 px-4 pt-4 sm:px-6 lg:px-8">
        <p className="caps-label">{label}</p>
        <p className="mt-2.5 text-figure font-medium text-ink">
          {figure ? (
            <>
              {figure.value}
              {figure.unit && <span className="unit">{figure.unit}</span>}
            </>
          ) : (
            <span className="text-xs font-normal tracking-normal text-ink-muted">waiting for first sample…</span>
          )}
        </p>
        {sub && (
          <p className={`mt-2 text-hint ${subTone === "warn" ? "text-warn" : "text-ink-muted"}`}>
            {sub}
          </p>
        )}
      </div>

      {points.length >= 2 && (
        <Sparkline points={points} max={max} format={format} label={label} hover={hover} onHover={setHover} />
      )}

      {hovered && (
        <div
          className="pointer-events-none absolute z-20 -translate-x-1/2 rounded-md border border-veil-edge bg-fog-900 px-2 py-1 text-center shadow-lg shadow-black/40"
          style={{
            left: `clamp(3.5rem, ${(hover! / (points.length - 1)) * 100}%, calc(100% - 3.5rem))`,
            bottom: "2.75rem",
          }}
        >
          <p className="text-xs font-medium tabular-nums text-ink">{format(hovered.v)}</p>
          <p className="text-[10px] tabular-nums text-ink-muted">
            {new Date(hovered.t * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
          </p>
        </div>
      )}
    </div>
  );
}

// Hand-rolled sparkline: a muted ink line over a faint flat wash — the
// accent is kept for what is active, and nothing in the system is shaded.
// The viewBox stretches to fill; vector-effect keeps the stroke and
// crosshair at true pixel width.
// The hover dot is an HTML overlay so the stretch can't distort it.
function Sparkline({ points, max, format, label, hover, onHover }: {
  points: Point[];
  max?: number;
  format: (v: number) => string;
  label: string;
  hover: number | null;
  onHover: (i: number | null) => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const n = points.length;

  const dataMax = Math.max(...points.map((p) => p.v));
  const top = max && max > 0 ? max : dataMax > 0 ? dataMax * 1.25 : 1;

  const W = 100;
  const H = 100;
  const xy = points.map((p, i): [number, number] => [
    (i / (n - 1)) * W,
    H - Math.min(p.v / top, 1) * (H - 8),
  ]);
  const line = xy.map(([x, y], i) => `${i === 0 ? "M" : "L"}${x.toFixed(2)},${y.toFixed(2)}`).join("");
  const area = `${line}L${W},${H}L0,${H}Z`;

  function locate(e: React.PointerEvent) {
    const rect = ref.current!.getBoundingClientRect();
    const frac = Math.min(Math.max((e.clientX - rect.left) / rect.width, 0), 1);
    onHover(Math.round(frac * (n - 1)));
  }

  return (
    <div
      ref={ref}
      className="absolute inset-x-0 bottom-0 h-10 cursor-crosshair"
      onPointerMove={locate}
      onPointerLeave={() => onHover(null)}
      onFocus={() => onHover(n - 1)}
      onBlur={() => onHover(null)}
      onKeyDown={(e) => {
        if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
        e.preventDefault();
        const cur = hover ?? n - 1;
        onHover(Math.min(Math.max(cur + (e.key === "ArrowRight" ? 1 : -1), 0), n - 1));
      }}
      tabIndex={0}
      role="img"
      aria-label={`${label} over time, currently ${format(points[n - 1].v)}`}
    >
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="h-full w-full">
        <path d={area} fill="var(--color-ink)" fillOpacity="0.04" />
        <path
          d={line}
          fill="none"
          stroke="var(--color-ink-muted)"
          strokeWidth="1.5"
          strokeLinejoin="round"
          strokeLinecap="round"
          vectorEffect="non-scaling-stroke"
        />
        {hover != null && (
          <line
            x1={xy[hover][0]}
            x2={xy[hover][0]}
            y1="0"
            y2={H}
            stroke="var(--color-fog-400)"
            strokeWidth="1"
            vectorEffect="non-scaling-stroke"
          />
        )}
      </svg>
      {hover != null && (
        <span
          className="absolute h-1.5 w-1.5 -translate-x-1/2 -translate-y-1/2 rounded-full bg-ink"
          style={{ left: `${xy[hover][0]}%`, top: `${xy[hover][1]}%` }}
        />
      )}
    </div>
  );
}
