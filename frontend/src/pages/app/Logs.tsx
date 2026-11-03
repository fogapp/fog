// The logs tab: the container's output and fogd's own actions on the app,
// interleaved by time. The interleaving is the point — it is how you see
// that a crash followed an update. Container lines are quoted as they
// came; fog does not read them. The severity pills are text filters over
// what has arrived, not a classification fog is claiming to have made.

import { memo, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Loading, errorText } from "../../components/Async";
import Toggle from "../../components/Toggle";
import { useAppAudit } from "../../lib/useAppData";
import { Group, hhmmss } from "./cockpit";
import { control } from "../../components/Field";

type StreamState = "connecting" | "streaming" | "ended" | "failed";
type Severity = "all" | "warn" | "error";
const SEVERITIES: { id: Severity; label: string }[] = [
  { id: "all", label: "All" },
  { id: "warn", label: "Warnings" },
  { id: "error", label: "Errors" },
];

interface Line {
  // Stable across renders: a container line's arrival number, an audit
  // entry's id. Keys built from list positions would shift as the window
  // trims its head, and every row would re-render on every chunk.
  key: string;
  t?: Date;
  source: "container" | "fogd";
  sev: "info" | "warn" | "error";
  text: string;
}

const TAIL = 300;
const KEEP = 2000;

export function Logs({ name }: { name: string }) {
  const [params, setParams] = useSearchParams();
  const raw = params.get("sev");
  const sev: Severity = raw === "warn" || raw === "error" ? raw : "all";
  const setSev = (s: Severity) => {
    const next = new URLSearchParams(params);
    if (s === "all") next.delete("sev");
    else next.set("sev", s);
    setParams(next, { replace: true });
  };
  const [lines, setLines] = useState<Line[]>([]);
  const [stream, setStream] = useState<StreamState>("connecting");
  const [streamError, setStreamError] = useState("");
  const [following, setFollowing] = useState(true);
  const [filter, setFilter] = useState("");
  const boxRef = useRef<HTMLDivElement>(null);
  // Autoscroll only holds while the reader is at the bottom: scrolling up to
  // read must not be yanked back down by the next line of a chatty app.
  const atBottomRef = useRef(true);
  const audit = useAppAudit(name);

  useEffect(() => {
    if (!following) {
      setStream("ended");
      return;
    }
    const ctrl = new AbortController();
    setLines([]);
    setStreamError("");
    setStream("connecting");
    // A chatty app sends many chunks a frame; they are held here and land
    // in one state update per animation frame, so the list re-renders at
    // most once a frame however fast the lines come.
    let pending: Line[] = [];
    let frame = 0;
    let seq = 0;
    const flush = () => {
      frame = 0;
      if (pending.length === 0) return;
      const batch = pending;
      pending = [];
      setLines((prev) => [...prev.slice(-KEEP), ...batch]);
    };
    (async () => {
      try {
        const res = await fetch(`/api/apps/${encodeURIComponent(name)}/logs?follow=1&tail=${TAIL}&timestamps=1`, { signal: ctrl.signal });
        if (!res.ok || !res.body) {
          setStreamError(`The log stream returned HTTP ${res.status}.`);
          setStream("failed");
          return;
        }
        setStream("streaming");
        const reader = res.body.getReader();
        const dec = new TextDecoder();
        let buf = "";
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          const parts = buf.split("\n");
          buf = parts.pop() ?? "";
          for (const part of parts) pending.push(parseContainerLine(part, `c${seq++}`));
          if (!frame && pending.length) frame = requestAnimationFrame(flush);
        }
        if (frame) cancelAnimationFrame(frame);
        flush();
        setStream("ended");
      } catch (e) {
        if (ctrl.signal.aborted) return; // switched off or navigated away
        setStreamError(errorText(e, "The connection dropped."));
        setStream("failed");
      }
    })();
    return () => {
      ctrl.abort();
      if (frame) cancelAnimationFrame(frame);
    };
  }, [name, following]);

  // fogd's entries join the container's within the window the container
  // lines cover; earlier ones would sit above a log that starts later and
  // read as if the app had been silent then.
  // Sorted once per audit read, not once per chunk of container output.
  const fogLines = useMemo(
    () =>
      (audit.data ?? [])
        .map((e): Line => ({
          key: `f${e.id}`,
          t: new Date(e.at),
          source: "fogd",
          sev: /fail|error|cancel|delete|oom/.test(e.action) && !/errors\.resolve/.test(e.action) ? "warn" : "info",
          text: `${e.action}${e.detail ? ` ${e.detail}` : ""} · ${e.actor}`,
        }))
        .filter((l) => l.t && !Number.isNaN(l.t.getTime()))
        .sort((a, b) => a.t!.getTime() - b.t!.getTime()),
    [audit.data],
  );
  const merged = useMemo(() => {
    const first = lines.find((l) => l.t)?.t;
    const fog = first ? fogLines.filter((l) => l.t! >= first) : fogLines;
    return interleave(lines, fog);
  }, [lines, fogLines]);

  const needle = filter.trim().toLowerCase();
  const shown = useMemo(
    () => merged.filter((l) => (sev === "all" ? true : sev === "warn" ? l.sev !== "info" : l.sev === "error")
      && (!needle || l.text.toLowerCase().includes(needle))),
    [merged, sev, needle],
  );

  useEffect(() => {
    if (following && atBottomRef.current) boxRef.current?.scrollTo(0, boxRef.current.scrollHeight);
  }, [shown, following]);

  // An empty pane has several meanings and gets a different line for each.
  const placeholder =
    (sev !== "all" || needle) && merged.length > 0
      ? `Nothing ${sev !== "all" ? `at ${sev === "warn" ? "warning or error" : "error"} level` : ""}${needle ? ` matching "${filter.trim()}"` : ""} in the ${merged.length.toLocaleString()} lines received so far.`
      : stream === "connecting"
        ? "Connecting to the log stream…"
        : stream === "failed"
          ? `Couldn't read the logs. ${streamError}`
          : stream === "streaming"
            ? `Connected — ${name} hasn't logged anything yet.`
            : "The log stream ended. Turn Follow back on to reconnect.";

  return (
    <Group
      title="Log"
      first
      fill
      right={
        <span className="inline-flex flex-wrap items-center gap-3.5">
          <span className="inline-flex gap-1.5" role="group" aria-label="Severity">
            {SEVERITIES.map((s) => (
              <button
                key={s.id}
                type="button"
                onClick={() => setSev(s.id)}
                aria-pressed={sev === s.id}
                className={`rounded-full border px-2.5 py-[3px] text-xs transition-colors ${
                  sev === s.id ? "border-accent/30 bg-accent/10 text-accent" : "border-veil-edge text-ink-muted hover:text-ink"
                }`}
              >
                {s.label}
              </button>
            ))}
          </span>
          <Toggle checked={following} onChange={setFollowing} label="Follow" />
        </span>
      }
    >
      <div className="mb-2 flex flex-wrap items-center gap-3">
        <div className="relative min-w-48 flex-1">
          <input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            onKeyDown={(e) => e.key === "Escape" && setFilter("")}
            placeholder="Filter lines…"
            aria-label="Filter log lines"
            className={`${control} w-full pr-7 font-mono`}
          />
          {filter && (
            <button onClick={() => setFilter("")} aria-label="Clear filter"
              className="absolute right-1 top-1/2 -translate-y-1/2 px-1.5 text-ink-muted hover:text-ink">
              ✕
            </button>
          )}
        </div>
        {(needle || sev !== "all") && (
          <span className="shrink-0 text-xs tabular-nums text-ink-muted">
            {shown.length.toLocaleString()} of {merged.length.toLocaleString()}
          </span>
        )}
        {following && stream === "connecting" && <Loading label="connecting…" />}
        {stream === "failed" && <span className="text-xs text-warn">stream failed</span>}
      </div>
      <div
        ref={boxRef}
        onScroll={() => {
          const el = boxRef.current;
          if (el) atBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
        }}
        className="min-h-60 flex-1 overflow-auto rounded border border-veil-edge bg-code p-4 font-mono text-xs leading-[2] text-ink"
      >
        {shown.length === 0 ? (
          <div className="text-ink-muted">{placeholder}</div>
        ) : (
          shown.map((l) => <LogRow key={l.key} line={l} needle={needle} />)
        )}
        {following && stream === "streaming" && (
          <div className="mt-1.5 text-ink-faint">
            ▸ following · <span className="text-ink-muted">{shown.length.toLocaleString()}</span> lines
          </div>
        )}
      </div>
      <div className="mt-2.5 text-hint text-ink-muted">
        Last {TAIL} lines on open, then live ·{" "}
        <a href={`/api/apps/${encodeURIComponent(name)}/logs?tail=5000&timestamps=1`} target="_blank" rel="noreferrer" className="text-accent hover:text-accent-hover">
          open the last 5,000 as text
        </a>
      </div>
    </Group>
  );
}

// One line of the pane. Memoised on the line object and the filter, so a
// new chunk mounts its own rows and leaves the 2,000 above it alone. The
// rows wrap, so their heights vary — which rules out fixed-height
// windowing; content-visibility lets the browser skip laying out the ones
// scrolled out of view instead.
const LogRow = memo(function LogRow({ line: l, needle }: { line: Line; needle: string }) {
  return (
    <div
      className="grid gap-3"
      style={{ gridTemplateColumns: "66px 44px minmax(0,1fr)", contentVisibility: "auto", containIntrinsicSize: "auto 24px" }}
    >
      <span className="text-ink-muted">{l.t ? hhmmss(l.t) : ""}</span>
      <span className={l.sev === "info" ? "text-ink-muted" : "text-warn"}>{l.sev}</span>
      <span className="min-w-0 whitespace-pre-wrap break-words">
        <span className={l.source === "fogd" ? "text-accent/80" : "text-ink-muted"}>{l.source}:</span>{" "}
        {needle ? mark(l.text, needle) : l.text}
      </span>
    </div>
  );
});

// Container lines in arrival order, with fogd's (already sorted) entries
// slotted in before the first container line stamped later than each. A
// container line wins a tie, and one with no stamp stays where it arrived.
function interleave(lines: Line[], fog: Line[]): Line[] {
  if (fog.length === 0) return lines;
  const out: Line[] = [];
  let j = 0;
  for (const l of lines) {
    if (l.t) while (j < fog.length && fog[j].t! < l.t) out.push(fog[j++]);
    out.push(l);
  }
  while (j < fog.length) out.push(fog[j++]);
  return out;
}

// Docker prefixes each line with an RFC3339Nano stamp and a space when
// asked for timestamps. A line without one (an app that writes bare
// carriage returns, say) keeps its text and no time.
function parseContainerLine(raw: string, key: string): Line {
  const m = raw.match(/^(\d{4}-\d{2}-\d{2}T[^ ]+) ?(.*)$/s);
  const t = m ? new Date(m[1]) : undefined;
  const text = m ? m[2] : raw;
  const lower = text.toLowerCase();
  const sev: Line["sev"] = /\b(error|fatal|panic|exception)\b/.test(lower) ? "error" : /\bwarn(ing)?\b/.test(lower) ? "warn" : "info";
  return { key, t: t && !Number.isNaN(t.getTime()) ? t : undefined, source: "container", sev, text };
}

// The match, lit up where it sits. Plain indexOf rather than a RegExp: the
// filter is whatever you typed, and a stray ( or * in a log line's text
// shouldn't turn into syntax — or an error.
function mark(line: string, needle: string) {
  const out: React.ReactNode[] = [];
  const lower = line.toLowerCase();
  let i = 0;
  for (;;) {
    const at = lower.indexOf(needle, i);
    if (at === -1) {
      out.push(line.slice(i));
      return out;
    }
    if (at > i) out.push(line.slice(i, at));
    out.push(
      <mark key={at} className="rounded-sm bg-warn/10 text-inherit">
        {line.slice(at, at + needle.length)}
      </mark>,
    );
    i = at + needle.length;
  }
}
