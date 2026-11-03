// The errors tab: Flare's triage surface for one app. A group list with
// status filters, and a detail view with the stacktrace and lifecycle
// actions. The fleet-wide inbox (/errors) reuses the badges from here.

import { useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  get, post,
  type ErrorEventDetail, type ErrorGroup, type ErrorGroupDetail,
  type SentryException, type SentryFrame,
} from "../../api";
import { LoadError, Loading, errorText, phaseOf } from "../../components/Async";
import Button from "../../components/Button";
import { timeAgo } from "../../lib/format";
import { useDismissable } from "../../lib/useDismissable";
import { useFlash } from "../../lib/useFlash";
import { appsKey } from "../../lib/useApps";
import { inboxSummaryKey } from "../../lib/useInbox";
import { appErrorsKey, errorGroupKey, fleetErrorsKey } from "../../queries";
import { control } from "../../components/Field";

const FILTERS = ["all", "open", "regressed", "resolved", "ignored"] as const;

export function Errors({ name }: { name: string }) {
  // A ?g= param (from the fleet inbox) deep-links straight into a group.
  const [params, setParams] = useSearchParams();
  const selected = params.get("g") ? Number(params.get("g")) : null;
  const select = (id: number | null) =>
    setParams(id === null ? {} : { g: String(id) }, { replace: true });

  if (selected !== null && !Number.isNaN(selected)) {
    return <GroupDetail base={`/api/apps/${name}/errors`} id={selected} onBack={() => select(null)} />;
  }
  return <GroupList name={name} onSelect={select} />;
}

function GroupList({ name, onSelect }: { name: string; onSelect: (id: number) => void }) {
  // Open is where triage starts, so it's where the page starts. "all" is one
  // click away and still says which set you're looking at.
  const [filter, setFilter] = useState<(typeof FILTERS)[number]>("open");
  const [q, setQ] = useState("");
  const [sel, setSel] = useState<ReadonlySet<number>>(new Set());
  const [note, flash] = useFlash();
  const groups = useQuery({
    queryKey: [...appErrorsKey, name, filter],
    queryFn: () =>
      get<ErrorGroup[]>(`/api/apps/${name}/errors${filter === "all" ? "" : `?status=${filter}`}`),
    refetchInterval: 15000,
  });

  const all = groups.data;
  const rows = useMemo(() => search(all ?? [], q, (g) => [g.title, g.culprit, g.errorClass]), [all, q]);
  // Selection is intersected with what's on screen, so a group that gets
  // filtered away — or resolved out of this list — can't be acted on from
  // behind a filter that no longer shows it.
  const picked = rows.filter((g) => sel.has(g.id));
  const phase = phaseOf(groups, all?.length);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-1.5">
        {FILTERS.map((f) => (
          <button
            key={f}
            onClick={() => setFilter(f)}
            className={`rounded-full px-3 py-1 text-xs font-medium transition-colors ${
              filter === f ? "bg-fog-700 text-ink" : "text-ink-muted hover:bg-hover hover:text-ink"
            }`}
          >
            {f}
          </button>
        ))}
      </div>

      {phase === "loading" ? (
        <Loading label="Loading errors…" />
      ) : phase === "error" ? (
        <LoadError label="Couldn't load errors." error={groups.error} onRetry={() => groups.refetch()} />
      ) : phase === "empty" ? (
        <div className="rounded-lg border border-dashed border-edge p-8 text-center">
          <p className="text-ink-muted">
            {filter === "all" ? "No errors reported." : `No ${filter} errors.`}
          </p>
          {filter === "all" && (
            <p className="mt-1 text-sm text-ink-muted">
              Reports arrive automatically once the app includes the flare gem (or any Sentry SDK
              pointed at its FLARE_DSN).
            </p>
          )}
        </div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-veil-edge bg-veil">
          <TableToolbar q={q} onQ={setQ} shown={rows.length} total={all!.length} note={note} />
          {picked.length > 0 && (
            <BulkBar
              ids={picked.map((g) => g.id)}
              urlFor={(id) => `/api/apps/${name}/errors/${id}`}
              onClear={() => setSel(new Set())}
              onResult={flash}
            />
          )}
          {rows.length === 0 ? (
            <p className="px-4 py-8 text-center text-sm text-ink-muted">
              No {filter === "all" ? "" : `${filter} `}errors match “{q.trim()}”.
            </p>
          ) : (
            <table className="w-full table-fixed text-left text-cell">
              <thead>
                <tr className="caps-label border-b border-hair-strong">
                  <th className="w-10 py-2.5 pl-4">
                    <SelectAll rows={rows} sel={sel} onChange={setSel} />
                  </th>
                  <th className="py-2.5 pl-3 pr-3 font-medium">Error</th>
                  <th className="w-44 px-3 py-2.5 font-medium">Status</th>
                  <th className="w-20 px-3 py-2.5 text-right font-medium">Events</th>
                  <th className="w-32 px-4 py-2.5 text-right font-medium">Last seen</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((g) => (
                  <tr
                    key={g.id}
                    onClick={() => onSelect(g.id)}
                    className={`cursor-pointer border-b border-divider last:border-0 ${
                      sel.has(g.id) ? "bg-fog-800/50" : "hover:bg-hover"
                    }`}
                  >
                    <td className="py-2.5 pl-4" onClick={(e) => e.stopPropagation()}>
                      <RowCheck id={g.id} sel={sel} onChange={setSel} />
                    </td>
                    <td className="overflow-hidden py-2.5 pl-3 pr-3">
                      <p className="truncate font-medium text-ink">{g.title}</p>
                      <p className="mt-0.5 truncate text-xs text-ink-muted">{g.culprit || g.errorClass}</p>
                    </td>
                    <td className="px-3 py-2.5">
                      <GroupBadges group={g} />
                    </td>
                    <td className="px-3 py-2.5 text-right tabular-nums text-ink-muted">{g.eventsCount}×</td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-right text-xs text-ink-muted">
                      {timeAgo(g.lastSeen)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </div>
  );
}

// ---- pieces both error tables use ----

/** Case-insensitive substring match across the fields a row is searched by. */
export function search<T>(rows: T[], q: string, fields: (row: T) => (string | undefined)[]): T[] {
  const needle = q.trim().toLowerCase();
  if (!needle) return rows;
  return rows.filter((r) => fields(r).some((f) => f?.toLowerCase().includes(needle)));
}

// The strip above the rows: what you're searching, and — while a search is
// narrowing things — how much of the list you are actually looking at.
export function TableToolbar({ q, onQ, shown, total, note, children }: {
  q: string; onQ: (v: string) => void; shown: number; total: number; note?: string; children?: React.ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2 border-b border-hair px-3 py-2">
      {children}
      <div className="relative min-w-48 flex-1">
        <input
          value={q}
          onChange={(e) => onQ(e.target.value)}
          onKeyDown={(e) => e.key === "Escape" && onQ("")}
          placeholder="Search errors…"
          aria-label="Search errors"
          className={`${control} w-full pr-7`}
        />
        {q && (
          <button onClick={() => onQ("")} aria-label="Clear search"
            className="absolute right-1 top-1/2 -translate-y-1/2 px-1.5 text-ink-muted hover:text-ink">
            ✕
          </button>
        )}
      </div>
      {q.trim() && (
        <span className="shrink-0 text-xs tabular-nums text-ink-muted">
          {shown} of {total}
        </span>
      )}
      {note && (
        <span className={`shrink-0 text-xs ${note.includes("failed") ? "text-warn" : "text-accent"}`}>
          {note}
        </span>
      )}
    </div>
  );
}

export function RowCheck({ id, sel, onChange }: {
  id: number; sel: ReadonlySet<number>; onChange: (s: ReadonlySet<number>) => void;
}) {
  return (
    <input
      type="checkbox"
      checked={sel.has(id)}
      aria-label="Select this error"
      onChange={(e) => {
        const next = new Set(sel);
        if (e.target.checked) next.add(id);
        else next.delete(id);
        onChange(next);
      }}
    />
  );
}

export function SelectAll<T extends { id: number }>({ rows, sel, onChange }: {
  rows: T[]; sel: ReadonlySet<number>; onChange: (s: ReadonlySet<number>) => void;
}) {
  const picked = rows.filter((r) => sel.has(r.id)).length;
  return (
    <input
      type="checkbox"
      aria-label="Select all errors"
      checked={picked > 0 && picked === rows.length}
      // Some-but-not-all is its own state: the box must not read as "none
      // selected" while three rows are.
      ref={(el) => { if (el) el.indeterminate = picked > 0 && picked < rows.length; }}
      onChange={(e) => onChange(e.target.checked ? new Set(rows.map((r) => r.id)) : new Set())}
    />
  );
}

const BULK = [
  { action: "resolve", label: "Resolve", done: "resolved", body: { nextDeploy: false } },
  { action: "ignore", label: "Ignore", done: "ignored", body: {} },
  { action: "unresolve", label: "Reopen", done: "reopened", body: {} },
] as const;

// Flare has no bulk endpoint — each group is its own POST — so the bar counts
// what actually came back rather than announcing a clean sweep and leaving
// you to discover otherwise. A partial failure keeps the selection so the
// same click can be repeated on what's left.
export function BulkBar({ ids, urlFor, actions, onClear, onResult }: {
  ids: number[];
  urlFor: (id: number) => string;
  actions?: (typeof BULK)[number]["action"][];
  onClear: () => void;
  onResult: (note: string) => void;
}) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState("");

  async function run(action: string, body: unknown, done: string) {
    setBusy(action);
    onResult("");
    const results = await Promise.allSettled(ids.map((id) => post(`${urlFor(id)}/${action}`, body)));
    const failed = results.filter((r) => r.status === "rejected").length;
    await Promise.all([
      qc.invalidateQueries({ queryKey: appErrorsKey }),
      qc.invalidateQueries({ queryKey: fleetErrorsKey }),
      qc.invalidateQueries({ queryKey: errorGroupKey }),
      qc.invalidateQueries({ queryKey: appsKey }),
      qc.invalidateQueries({ queryKey: inboxSummaryKey }),
    ]);
    setBusy("");
    if (failed === 0) {
      onResult(`${ids.length} ${done}`);
      onClear();
    } else {
      onResult(`${ids.length - failed} of ${ids.length} ${done} — ${failed} failed`);
    }
  }

  return (
    <div className="flex flex-wrap items-center gap-2 border-b border-hair bg-fog-800/40 px-3 py-2">
      <span className="text-xs font-medium text-ink">{ids.length} selected</span>
      {BULK.filter((b) => !actions || actions.includes(b.action)).map((b) => (
        <Button key={b.action} size="sm" onClick={() => run(b.action, b.body, b.done)} disabled={!!busy}>
          {busy === b.action ? "…" : b.label}
        </Button>
      ))}
      <button onClick={onClear} className="text-xs text-ink-muted hover:text-ink">
        Clear
      </button>
    </div>
  );
}

// Status + severity as pills. Regressed gets the loudest treatment: it's the
// state that means "you thought this was fixed".
export function GroupBadges({ group }: { group: ErrorGroup }) {
  const status: Record<string, string> = {
    open: "bg-warn/10 text-warn",
    regressed: "bg-warn/10 text-ink-muted ring-1 ring-warn/40",
    resolved: "bg-accent/10 text-accent",
    ignored: "bg-fog-700/50 text-ink-muted",
  };
  return (
    <span className="flex flex-wrap items-center gap-1">
      <span className={`rounded-full px-2 py-0.5 text-[10px] font-medium ${status[group.status]}`}>
        {group.status}
      </span>
      {group.unhandled && (
        <span className="rounded-full bg-warn/10 px-2 py-0.5 text-[10px] font-medium text-warn">
          crash
        </span>
      )}
    </span>
  );
}

// GroupDetail is shared between the per-app tab (base=/api/apps/x/errors)
// and the fleet inbox (base=/api/errors — the only route to peer-relayed
// groups, whose apps have no local namespace).
// onBack draws the "all errors" link. The Inbox's split pane has the list
// beside it and needs no such link, so it leaves it off.
export function GroupDetail({ base, id, onBack }: { base: string; id: number; onBack?: () => void }) {
  const qc = useQueryClient();
  const [error, setError] = useState("");
  const [eventId, setEventId] = useState<number | null>(null);
  const [ignoreMenu, setIgnoreMenu] = useState(false);
  const ignoreRef = useRef<HTMLDivElement>(null);
  useDismissable(ignoreRef, ignoreMenu, () => setIgnoreMenu(false));

  const detail = useQuery({
    queryKey: [...errorGroupKey, base, id],
    queryFn: () => get<ErrorGroupDetail>(`${base}/${id}`),
    refetchInterval: 15000,
  });

  // Refresh everything that quotes this group — the list, the badges, the
  // notice count — and only report success once they have.
  const refresh = () =>
    Promise.all([
      qc.invalidateQueries({ queryKey: appErrorsKey }),
      qc.invalidateQueries({ queryKey: errorGroupKey }),
      qc.invalidateQueries({ queryKey: fleetErrorsKey }),
      qc.invalidateQueries({ queryKey: appsKey }),
      qc.invalidateQueries({ queryKey: inboxSummaryKey }),
    ]);
  const act = useMutation({
    mutationFn: ({ action, body }: { action: string; body?: unknown }) =>
      post(`${base}/${id}/${action}`, body ?? {}),
    onSuccess: () => {
      setError("");
      setIgnoreMenu(false);
      return refresh();
    },
    onError: (e) => setError(errorText(e, "Action failed")),
  });

  if (!detail.data) {
    return detail.isError ? (
      <LoadError label="Couldn't load this error." error={detail.error} onRetry={() => detail.refetch()} />
    ) : (
      <Loading label="Loading error…" />
    );
  }
  const g = detail.data.group;
  const events = detail.data.events;
  const currentEventID = eventId ?? events[0]?.id ?? null;
  const busy = act.isPending;

  return (
    <div className="space-y-5">
      {onBack && (
        <button onClick={onBack} className="text-sm text-ink-muted hover:text-ink">
          ← All errors
        </button>
      )}

      <div className="space-y-2">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <h2 className="break-words text-lg font-semibold text-ink">{g.title}</h2>
            <p className="mt-0.5 text-sm text-ink-muted">{g.culprit || g.errorClass}</p>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            {(g.status === "open" || g.status === "regressed") && (
              <>
                <Button size="sm" onClick={() => act.mutate({ action: "resolve", body: { nextDeploy: true } })} disabled={busy}>
                  Resolve in next deploy
                </Button>
                <Button size="sm" onClick={() => act.mutate({ action: "resolve", body: { nextDeploy: false } })} disabled={busy}>
                  Resolve
                </Button>
                <div ref={ignoreRef} className="relative">
                  <Button size="sm" onClick={() => setIgnoreMenu(!ignoreMenu)} disabled={busy}>
                    Ignore ▾
                  </Button>
                  {ignoreMenu && (
                    <div className="absolute right-0 z-20 mt-1 w-48 overflow-hidden rounded-lg border border-veil-edge bg-fog-900 py-1 shadow-xl shadow-black/40">
                      {[
                        { label: "Forever", body: {} },
                        { label: "For 24 hours", body: { forHours: 24 } },
                        { label: "For 7 days", body: { forHours: 168 } },
                        { label: "Until 100 more events", body: { untilCount: g.eventsCount + 100 } },
                      ].map((o) => (
                        <button
                          key={o.label}
                          onClick={() => act.mutate({ action: "ignore", body: o.body })}
                          className="block w-full px-3 py-1.5 text-left text-sm text-ink hover:bg-fog-800 hover:text-ink"
                        >
                          {o.label}
                        </button>
                      ))}
                    </div>
                  )}
                </div>
              </>
            )}
            {(g.status === "resolved" || g.status === "ignored") && (
              <Button size="sm" onClick={() => act.mutate({ action: "unresolve" })} disabled={busy}>
                Reopen
              </Button>
            )}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-ink-muted">
          <GroupBadges group={g} />
          <span className="tabular-nums text-ink-muted">{g.eventsCount}× total</span>
          <span>first {timeAgo(g.firstSeen)}</span>
          <span>last {timeAgo(g.lastSeen)}</span>
          {g.lastRelease && <span className="font-mono">on {g.lastRelease.slice(0, 12)}</span>}
          {detail.data?.localRelease && g.lastRelease && (
            <span className="font-mono">
              {detail.data.localRelease.startsWith(g.lastRelease) || g.lastRelease.startsWith(detail.data.localRelease)
                ? "same release as yours"
                : `yours runs ${detail.data.localRelease.slice(0, 12)}`}
            </span>
          )}
          {g.status === "resolved" && g.resolvedRelease && (
            <span className="font-mono">resolved as of {g.resolvedRelease.slice(0, 12)}</span>
          )}
          {g.status === "ignored" && g.ignoredUntil && <span>ignored until {new Date(g.ignoredUntil).toLocaleString()}</span>}
          {g.status === "ignored" && (g.ignoreUntilCount ?? 0) > 0 && (
            <span>ignored until {g.ignoreUntilCount} events</span>
          )}
        </div>
        {error && <p className="text-sm text-warn">{error}</p>}
      </div>

      {events.length > 1 && (
        <div className="flex flex-wrap items-center gap-2 text-xs">
          <span className="text-ink-muted">
            {events.length} stored occurrence{events.length === 1 ? "" : "s"}:
          </span>
          <select
            value={currentEventID ?? undefined}
            onChange={(e) => setEventId(Number(e.target.value))}
            className={control}
          >
            {events.map((e) => (
              <option key={e.id} value={e.id}>
                {new Date(e.receivedAt).toLocaleString()}
                {e.release ? ` · ${e.release.slice(0, 12)}` : ""}
              </option>
            ))}
          </select>
        </div>
      )}

      {currentEventID !== null ? (
        <EventView base={base} groupId={id} eventId={currentEventID} />
      ) : (
        <p className="text-sm text-ink-muted">
          No stored occurrences — payloads for this group have aged out; the counters above are
          still complete.
        </p>
      )}
    </div>
  );
}

function EventView({ base, groupId, eventId }: { base: string; groupId: number; eventId: number }) {
  const ev = useQuery({
    queryKey: ["error-event", base, eventId],
    queryFn: () => get<ErrorEventDetail>(`${base}/${groupId}/events/${eventId}`),
    staleTime: Infinity, // stored payloads never change
  });
  if (!ev.data) {
    return ev.isError ? (
      <LoadError label="Couldn't load this occurrence." error={ev.error} onRetry={() => ev.refetch()} />
    ) : (
      <Loading label="Loading occurrence…" />
    );
  }
  const p = ev.data.payload;
  const chain: SentryException[] = Array.isArray(p.exception) ? p.exception : (p.exception?.values ?? []);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-ink-muted">
        <span>{new Date(ev.data.receivedAt).toLocaleString()}</span>
        {ev.data.environment && <span>{ev.data.environment}</span>}
        {ev.data.release && <span className="font-mono">{ev.data.release.slice(0, 12)}</span>}
        {p.server_name && <span>{String(p.server_name)}</span>}
        {p.request?.method && p.request?.url && (
          <span className="font-mono">
            {String(p.request.method)} {String(p.request.url)}
          </span>
        )}
      </div>

      {chain.length === 0 ? (
        <p className="rounded-lg border border-veil-edge bg-veil p-3 font-mono text-sm text-ink-muted">
          {typeof p.message === "string" ? p.message : p.message?.formatted ?? "(no exception data)"}
        </p>
      ) : (
        // Root cause first in the payload; the crash — the interesting end —
        // is shown first here.
        [...chain].reverse().map((ex, i) => (
          <ExceptionView key={i} ex={ex} nested={i > 0} />
        ))
      )}
    </div>
  );
}

function ExceptionView({ ex, nested }: { ex: SentryException; nested: boolean }) {
  const frames = ex.stacktrace?.frames ?? [];
  return (
    <div className="overflow-hidden rounded-lg border border-veil-edge bg-veil">
      <div className="border-b border-hair px-4 py-2.5">
        <p className="font-mono text-sm text-ink">
          {nested && <span className="text-ink-muted">caused by </span>}
          <span className="font-semibold">{ex.type ?? "Error"}</span>
          {ex.value && <span className="text-ink-muted">: {ex.value}</span>}
        </p>
      </div>
      {frames.length > 0 && <StackTrace frames={frames} />}
    </div>
  );
}

// Frames arrive oldest-call-first; display newest-first with library frames
// folded, the way every tracker renders it. Folded frames stay one click
// away — hiding them entirely would misrepresent the stack.
function StackTrace({ frames }: { frames: SentryFrame[] }) {
  const display = [...frames].reverse();
  const rows: { frame: SentryFrame; idx: number }[][] = [];
  // Partition into alternating runs of in-app and library frames.
  for (const [idx, frame] of display.entries()) {
    const last = rows[rows.length - 1];
    const inApp = frame.in_app === true;
    if (last && (last[0].frame.in_app === true) === inApp) last.push({ frame, idx });
    else rows.push([{ frame, idx }]);
  }
  return (
    <div className="divide-y divide-divider/60 font-mono text-xs">
      {rows.map((run, i) =>
        run[0].frame.in_app === true ? (
          run.map(({ frame, idx }) => <FrameView key={idx} frame={frame} />)
        ) : (
          <FoldedFrames key={`lib-${i}`} frames={run.map((r) => r.frame)} />
        ),
      )}
    </div>
  );
}

function FoldedFrames({ frames }: { frames: SentryFrame[] }) {
  const [open, setOpen] = useState(false);
  return (
    <div>
      <button
        onClick={() => setOpen(!open)}
        className="block w-full px-4 py-1.5 text-left text-ink-muted hover:bg-hover hover:text-ink-muted"
      >
        {open ? "▾" : "▸"} {frames.length} library frame{frames.length === 1 ? "" : "s"}
      </button>
      {open && frames.map((f, i) => <FrameView key={i} frame={f} dim />)}
    </div>
  );
}

function FrameView({ frame, dim }: { frame: SentryFrame; dim?: boolean }) {
  const [open, setOpen] = useState(false);
  const hasContext = !!frame.context_line;
  const file = frame.filename || frame.abs_path || "?";
  return (
    <div className={dim ? "opacity-60" : ""}>
      <button
        onClick={() => hasContext && setOpen(!open)}
        className={`block w-full px-4 py-1.5 text-left ${hasContext ? "hover:bg-hover" : "cursor-default"}`}
      >
        <span className="text-ink">{file}</span>
        {frame.lineno !== undefined && <span className="text-ink-muted">:{frame.lineno}</span>}
        {frame.function && (
          <span className="text-ink-muted">
            {" "}in <span className="text-ink-muted">{frame.function}</span>
          </span>
        )}
        {hasContext && <span className="ml-1.5 text-ink-muted">{open ? "▾" : "▸"}</span>}
      </button>
      {open && hasContext && <SourceContext frame={frame} />}
    </div>
  );
}

function SourceContext({ frame }: { frame: SentryFrame }) {
  const pre = frame.pre_context ?? [];
  const post = frame.post_context ?? [];
  const start = (frame.lineno ?? 0) - pre.length;
  const lines = [
    ...pre.map((l, i) => ({ no: start + i, text: l, hot: false })),
    { no: frame.lineno ?? 0, text: frame.context_line ?? "", hot: true },
    ...post.map((l, i) => ({ no: (frame.lineno ?? 0) + 1 + i, text: l, hot: false })),
  ];
  return (
    <pre className="overflow-x-auto border-y border-hair bg-code py-1 text-[11px] leading-5">
      {lines.map((l) => (
        <div key={l.no} className={l.hot ? "bg-warn/10 text-warn" : "text-ink-muted"}>
          <span className="inline-block w-12 select-none pr-3 text-right text-ink-muted">{l.no}</span>
          {l.text}
        </div>
      ))}
    </pre>
  );
}
