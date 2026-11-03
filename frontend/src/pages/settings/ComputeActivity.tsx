// Settings › Compute › Activity: what apps asked of the machines — each
// request to fog · Inference and each video run — with the machine it went
// to and how it ended, newest first. A row appears as fog takes the work
// and settles when it ends, so what's in flight shows too; the first page
// is polled while the page is open. The filters are asked of fogd, so
// "Show older" reads further back through the narrowed log, not the loaded
// rows.

import { useState } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { get, type ComputeActivity as Row } from "../../api";
import { LoadError, Loading } from "../../components/Async";
import Button from "../../components/Button";
import { Select } from "../../components/Field";
import Toggle from "../../components/Toggle";
import { Section } from "../../components/Section";
import { Dot } from "../app/cockpit";

const PAGE = 100;

export default function ComputeActivity({ apps, machines }: { apps: string[]; machines: string[] }) {
  const [app, setApp] = useState("");
  const [kind, setKind] = useState("");
  const [machine, setMachine] = useState("");
  const [problems, setProblems] = useState(false);

  const filter = new URLSearchParams();
  if (app) filter.set("app", app);
  if (kind) filter.set("kind", kind);
  if (machine) filter.set("machine", machine);
  if (problems) filter.set("problems", "1");

  const log = useInfiniteQuery({
    queryKey: ["compute-activity", filter.toString()],
    queryFn: ({ pageParam }) => {
      const q = new URLSearchParams(filter);
      if (pageParam) q.set("before", String(pageParam));
      return get<Row[]>(`/api/compute/activity?${q}`);
    },
    initialPageParam: 0,
    // A short page is the end of the log.
    getNextPageParam: (last) => (last.length < PAGE ? undefined : last[last.length - 1].id),
    refetchInterval: 5000,
  });
  const rows = log.data?.pages.flat() ?? [];
  const narrowed = filter.toString() !== "";

  return (
    <Section
      id="activity"
      title="Activity"
      description="Every request an app made to fog · Inference and every video run it started, the machine fog sent it to, and how it ended. Kept for 30 days."
    >
      <div className="space-y-3">
        <div className="flex flex-wrap items-center gap-3">
          <Select value={app} onChange={(e) => setApp(e.target.value)} aria-label="App">
            <option value="">Every app</option>
            {apps.map((a) => <option key={a} value={a}>{a}</option>)}
          </Select>
          <Select value={kind} onChange={(e) => setKind(e.target.value)} aria-label="Kind of work">
            <option value="">Inference and video</option>
            <option value="inference">Inference</option>
            <option value="video">Video</option>
          </Select>
          <Select value={machine} onChange={(e) => setMachine(e.target.value)} aria-label="Machine">
            <option value="">Every machine</option>
            {machines.map((m) => <option key={m} value={m}>{m}</option>)}
          </Select>
          <Toggle label="Only what went wrong" checked={problems} onChange={setProblems} />
        </div>

        {log.isPending && <Loading label="Loading activity…" />}
        {log.isError && <LoadError label="Couldn't load compute activity." error={log.error} onRetry={() => log.refetch()} />}
        {log.data && (
          <div className="overflow-x-auto rounded-lg border border-veil-edge bg-veil">
            <table className="w-full text-left text-cell">
              <thead>
                <tr className="caps-label border-b border-hair-strong">
                  <th className="px-4 py-2.5 font-medium">When</th>
                  <th className="px-3 py-2.5 font-medium">App</th>
                  <th className="px-3 py-2.5 font-medium">What</th>
                  <th className="px-3 py-2.5 font-medium">Machine</th>
                  <th className="px-3 py-2.5 font-medium">Outcome</th>
                  <th className="px-4 py-2.5 text-right font-medium">Took</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => <ActivityRow key={r.id} row={r} />)}
                {rows.length === 0 && (
                  <tr>
                    <td colSpan={6} className="px-4 py-4 text-ink-muted">
                      {narrowed ? "Nothing matches — clear the filters." : "Nothing yet. Apps' requests and runs appear here as fog takes them."}
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        )}
        {log.hasNextPage && (
          <Button onClick={() => log.fetchNextPage()} disabled={log.isFetchingNextPage}>
            {log.isFetchingNextPage ? "Loading…" : "Show older"}
          </Button>
        )}
      </div>
    </Section>
  );
}

function ActivityRow({ row: r }: { row: Row }) {
  const running = r.outcome === "running";
  return (
    <tr className="border-b border-divider align-top last:border-0">
      <td className="whitespace-nowrap px-4 py-2.5 font-mono text-hint text-ink-muted" title={new Date(r.at).toLocaleString()}>{when(r.at)}</td>
      <td className="whitespace-nowrap px-3 py-2.5 text-ink">{r.app}</td>
      <td className="px-3 py-2.5">
        <span className="whitespace-nowrap font-mono text-hint text-ink">{what(r)}</span>
        <Facts row={r} />
      </td>
      <td className="whitespace-nowrap px-3 py-2.5 text-ink">{r.machine || <span className="text-ink-muted">—</span>}</td>
      <td className="px-3 py-2.5">
        <span className="flex items-center gap-2">
          {running ? <Dot tone="ok" /> : <Dot tone={bad(r.outcome) ? "warn" : "off"} />}
          <span className={bad(r.outcome) ? "text-warn" : r.outcome === "ok" ? "text-ok" : "text-ink-muted"}>{outcomeText(r)}</span>
        </span>
        {r.message && <p className="mt-1 max-w-md break-words text-hint text-ink-muted">{r.message}</p>}
      </td>
      <td className="whitespace-nowrap px-4 py-2.5 text-right font-mono text-hint tabular-nums text-ink-muted">
        {running ? <Elapsed since={r.at} /> : took(r)}
      </td>
    </tr>
  );
}

// Facts is what fog learned of an inference answer: time to its first
// byte, and its tokens — said to be a count of chunks when the model
// server reported no usage.
function Facts({ row: r }: { row: Row }) {
  if (r.kind !== "inference" || r.outcome === "running" || r.outcome === "refused") return null;
  const parts = [
    r.ttftMs ? `first byte ${duration(r.ttftMs)}` : "",
    r.tokensIn ? `${r.tokensIn.toLocaleString()} in` : "",
    r.tokensOut ? `${r.tokensEstimated ? "~" : ""}${r.tokensOut.toLocaleString()} out` : "",
  ].filter(Boolean);
  if (parts.length === 0) return null;
  return (
    <p className="mt-0.5 text-hint text-ink-muted" title={r.tokensEstimated ? "The model server reported no usage; fog counted the chunks it streamed." : undefined}>
      {parts.join(" · ")}
    </p>
  );
}

function what(r: Row): string {
  if (r.kind === "inference") return r.model ? `${r.model} · ${r.what}` : r.what || "inference";
  return `${r.pool.replace(/^video\//, "")} · ${r.what || "video"}`;
}

const bad = (outcome: string) => outcome === "failed" || outcome === "lost" || outcome === "refused";

function outcomeText(r: Row): string {
  switch (r.outcome) {
    case "ok":
      return r.kind === "inference" ? "answered" : "finished";
    case "failed":
      return r.status != null ? `failed (${r.status})` : "failed";
    case "displaced":
      return "made room for a viewer";
    case "stopped":
      return "stopped";
    default:
      return r.outcome;
  }
}

// took is how long finished work ran: nothing for a refusal, which never
// ran, nor for work fogd lost track of across a restart, whose end it
// never saw.
function took(r: Row): string {
  if (r.outcome === "refused" || (r.outcome === "lost" && r.durationMs === 0)) return "";
  return duration(r.durationMs);
}

// when is a row's start: the time alone today, with the date before that.
function when(iso: string): string {
  const d = new Date(iso);
  const today = d.toDateString() === new Date().toDateString();
  return d.toLocaleString(undefined, today
    ? { hour: "numeric", minute: "2-digit", second: "2-digit" }
    : { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
}

function duration(ms: number): string {
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  const m = Math.floor(ms / 60_000);
  return `${m}m ${Math.round((ms % 60_000) / 1000)}s`;
}

// Elapsed is a running row's age, as of the last poll's render.
function Elapsed({ since }: { since: string }) {
  return <span className="text-accent">{duration(Math.max(Date.now() - new Date(since).getTime(), 0))}</span>;
}
