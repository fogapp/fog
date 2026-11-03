// The Inbox's Feedback tab: what people wrote from the header's bug button,
// on this fog or on a peer that sends its reports here. A report stays open
// until someone closes it; closing is "dealt with", not deletion, and the
// Closed list keeps them. The pane shows the words first and the dossier
// under them — a photograph of what fog knew when the words were written.

import { useState, type ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, get, post, type FeedbackDossier, type FeedbackItem, type LogRecord } from "../../api";
import { LoadError, Loading, StaleNote, errorText, phaseOf } from "../../components/Async";
import Button from "../../components/Button";
import CopyButton from "../../components/CopyButton";
import type { TrailEvent } from "../../lib/clientTrail";
import { formatBytes, formatWhen, timeAgo, uaSummary, versionLabel } from "../../lib/format";
import { useFlash } from "../../lib/useFlash";
import { feedbackKey, inboxSummaryKey } from "../../lib/useInbox";
import { DetailPane, EmptyList, InboxRow, InboxSplit, NoSelection, Tag } from "./Split";

type Show = "open" | "closed";

export default function Feedback() {
  const qc = useQueryClient();
  const [error, flash] = useFlash();
  const [params, setParams] = useSearchParams();
  // ?state=, not ?show= — the Inbox reads a stray ?show= as an old Issues link.
  const show: Show = params.get("state") === "closed" ? "closed" : "open";
  const selectedId = params.get("sel") ? Number(params.get("sel")) : null;

  const setParam = (key: string, value: string | null) => {
    const next = new URLSearchParams(params);
    if (value === null) next.delete(key);
    else next.set(key, value);
    if (key === "state") next.delete("sel");
    setParams(next, { replace: true });
  };

  const list = useQuery({
    queryKey: [...feedbackKey, show],
    queryFn: () => get<{ feedback: FeedbackItem[] }>(`/api/feedback?status=${show}`),
    refetchInterval: 60_000,
  });
  const items = list.data?.feedback ?? [];
  const phase = phaseOf(list, list.data?.feedback.length);

  const refresh = () =>
    Promise.all([qc.invalidateQueries({ queryKey: feedbackKey }), qc.invalidateQueries({ queryKey: inboxSummaryKey })]);
  const setStatus = useMutation({
    mutationFn: ({ id, status }: { id: number; status: Show }) => post<FeedbackItem>(`/api/feedback/${id}/status`, { status }),
    // The report leaves this list once the lists agree it has moved.
    onSuccess: async () => {
      await refresh();
      setParam("sel", null);
    },
    onError: (e) => flash(errorText(e, "Couldn't update the report")),
  });
  const remove = useMutation({
    mutationFn: (id: number) => del(`/api/feedback/${id}`),
    onSuccess: async () => {
      await refresh();
      setParam("sel", null);
    },
    onError: (e) => flash(errorText(e, "Couldn't delete the report")),
  });

  return (
    <InboxSplit
      selected={selectedId !== null}
      list={
        <>
          <div className="flex items-center gap-1 border-b border-divider px-4 py-2">
            {(["open", "closed"] as const).map((s) => (
              <button
                key={s}
                onClick={() => setParam("state", s === "open" ? null : s)}
                aria-pressed={show === s}
                className={`rounded-md px-2 py-0.5 text-xs ${show === s ? "bg-fog-800 text-ink" : "text-ink-muted hover:text-ink"}`}
              >
                {s === "open" ? "Open" : "Closed"}
              </button>
            ))}
          </div>
          {error && <p className="px-4 py-2 text-sm text-warn">{error}</p>}
          {phase === "loading" ? (
            <div className="p-4"><Loading label="Loading feedback…" /></div>
          ) : phase === "error" ? (
            <div className="p-4"><LoadError label="Couldn't load feedback." error={list.error} onRetry={() => list.refetch()} /></div>
          ) : phase === "empty" ? (
            <EmptyList
              title={show === "open" ? "No open feedback." : "Nothing closed yet."}
              hint={show === "open" ? "The bug button in the header files a report here, with what fog knew at that moment." : "Closed reports stay here."}
            />
          ) : (
            items.map((f) => (
              <InboxRow
                key={f.id}
                selected={f.id === selectedId}
                onClick={() => setParam("sel", String(f.id))}
                title={firstLine(f.body)}
                when={timeAgo(f.createdAt)}
                tag={f.origin ? <Tag tone="violet">{f.origin}</Tag> : f.author ? <Tag>{f.author}</Tag> : undefined}
                subtitle={f.page || undefined}
              />
            ))
          )}
          {list.isError && list.data && <div className="p-4"><StaleNote what="this list" /></div>}
        </>
      }
      detail={
        selectedId !== null ? (
          <DetailPane onBack={() => setParam("sel", null)}>
            <Report
              key={selectedId}
              id={selectedId}
              busy={setStatus.isPending || remove.isPending}
              onStatus={(status) => setStatus.mutate({ id: selectedId, status })}
              onDelete={() => remove.mutate(selectedId)}
            />
          </DetailPane>
        ) : (
          <NoSelection label="Pick a report to read it." />
        )
      }
    />
  );
}

function firstLine(s: string): string {
  return s.trim().split("\n")[0];
}

function Report({ id, busy, onStatus, onDelete }: {
  id: number;
  busy: boolean;
  onStatus: (s: Show) => void;
  onDelete: () => void;
}) {
  const [confirming, setConfirming] = useState(false);
  const q = useQuery({
    queryKey: [...feedbackKey, "item", id],
    queryFn: () => get<FeedbackItem>(`/api/feedback/${id}`),
    // While a send is still waiting, keep asking: the receipt is the point.
    // Once it has landed somewhere and is still open, keep an ear out for
    // the peer's word coming back.
    refetchInterval: (query) => {
      const f = query.state.data;
      if (f?.deliveries?.some((d) => d.state === "queued")) return 15_000;
      if (f?.status === "open" && f.deliveries?.some((d) => d.state === "delivered")) return 60_000;
      return false;
    },
  });
  if (q.isLoading) return <Loading label="Loading the report…" />;
  if (!q.data) return <LoadError label="Couldn't load this report." error={q.error} onRetry={() => q.refetch()} />;
  const f = q.data;

  let dossier: FeedbackDossier | null = null;
  let unreadable = false;
  if (f.dossier) {
    try {
      dossier = JSON.parse(f.dossier) as FeedbackDossier;
    } catch {
      unreadable = true;
    }
  }
  const pageIsLocal = !f.peerId && f.page.startsWith("/");

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <p className="flex flex-wrap items-center gap-2 text-xs text-ink-muted">
          {f.origin ? <Tag tone="violet">{f.origin}</Tag> : null}
          {f.author && <span className="text-ink-muted">{f.author}</span>}
          <span title={formatWhen(f.createdAt)}>{timeAgo(f.createdAt)}</span>
          {f.page && (
            <>
              <span aria-hidden>·</span>
              {pageIsLocal ? (
                <Link to={f.page} className="font-mono hover:text-ink">{f.page}</Link>
              ) : (
                <span className="font-mono">{f.page}</span>
              )}
            </>
          )}
          {f.fogVersion && <><span aria-hidden>·</span><span>fog {versionLabel(f.fogVersion)}</span></>}
          {f.statusBy && (
            <>
              <span aria-hidden>·</span>
              <span className={f.status === "closed" ? "text-accent" : "text-warn"} title={f.statusAt ? formatWhen(f.statusAt) : undefined}>
                {f.status === "closed" ? "resolved" : "reopened"} by {f.statusBy}{f.statusAt ? ` ${timeAgo(f.statusAt)}` : ""}
              </span>
            </>
          )}
        </p>
        <div className="flex shrink-0 items-center gap-2">
          <CopyButton text={reportText(f, dossier)} />
          <Button size="sm" disabled={busy} onClick={() => onStatus(f.status === "open" ? "closed" : "open")}>
            {f.status === "open" ? "Close report" : "Reopen"}
          </Button>
          {confirming ? (
            <>
              <Button size="sm" variant="danger" disabled={busy} onClick={onDelete}>Delete it</Button>
              <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>Keep</Button>
            </>
          ) : (
            <Button size="sm" variant="ghost" onClick={() => setConfirming(true)}>Delete…</Button>
          )}
        </div>
      </div>

      <p className="whitespace-pre-wrap break-words text-base leading-relaxed text-ink">{f.body}</p>

      {f.deliveries && f.deliveries.length > 0 && (
        <section className="space-y-1.5">
          <h3 className="subhead">Sent to</h3>
          {f.deliveries.map((d) => (
            <p key={d.peerId} className="text-hint text-ink-muted">
              <span className="text-ink">{d.peer}</span>
              {" — "}
              {d.mode === "full" ? "with the dossier" : "the words and page only"}
              {" · "}
              {d.state === "delivered" ? (
                <span className="text-accent">delivered {d.deliveredAt ? timeAgo(d.deliveredAt) : ""}</span>
              ) : d.state === "refused" ? (
                <span className="text-warn">refused: {d.lastError}</span>
              ) : (
                <span>
                  not delivered yet{d.attempts > 0 ? ` after ${d.attempts} ${d.attempts === 1 ? "try" : "tries"}` : ""}
                  {d.lastError && <span className="block break-words text-ink-muted">Last try: {d.lastError}</span>}
                </span>
              )}
            </p>
          ))}
        </section>
      )}

      {dossier ? (
        <DossierView d={dossier} />
      ) : unreadable ? (
        <p className="text-hint text-warn">The dossier arrived but couldn't be read.</p>
      ) : (
        <p className="text-hint text-ink-muted">
          {f.origin ? `${f.origin} sends the words and page only, so no dossier came with this.` : "No dossier was attached."}
        </p>
      )}
    </div>
  );
}

// The whole report as text, for pasting into an issue tracker.
function reportText(f: FeedbackItem, d: FeedbackDossier | null): string {
  const head = [f.body, "", `— ${f.author || "someone"}${f.origin ? ` at ${f.origin}` : ""}, ${f.createdAt}`];
  if (f.page) head.push(`Page: ${f.page}`);
  if (f.fogVersion) head.push(`Fog: ${f.fogVersion}`);
  return d ? `${head.join("\n")}\n\nDossier:\n${JSON.stringify(d, null, 2)}` : head.join("\n");
}

function DossierView({ d }: { d: FeedbackDossier }) {
  const c = d.client;
  const failed = c ? c.requests.filter((r) => /→ (failed|[45]\d\d)/.test(r.message)) : [];
  return (
    <section className="space-y-3">
      <h3 className="subhead">What fog knew · {formatWhen(d.capturedAt)}</h3>

      {d.gaps && d.gaps.length > 0 && (
        <div className="rounded-md border border-warn/30 bg-warn/5 px-3 py-2 text-hint text-warn">
          Not attached:
          <ul className="mt-1 list-disc pl-5">{d.gaps.map((g) => <li key={g} className="break-words">{g}</li>)}</ul>
        </div>
      )}

      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-hint">
        <Fact k="Page">{d.page.path}{d.page.search}</Fact>
        <Fact k="Fog">
          {versionLabel(d.fog.version)} · {d.fog.os}
          {d.fog.startedAt && ` · up since ${formatWhen(d.fog.startedAt)}`}
        </Fact>
        {(d.fog.serverName || d.fog.hostname) && <Fact k="Server">{[d.fog.serverName, d.fog.hostname].filter(Boolean).join(" · ")}</Fact>}
        {d.host && (
          <Fact k="Host">
            CPU {Math.round(d.host.cpuPct)}% · load {d.host.load1.toFixed(2)} · memory {formatBytes(d.host.memUsed)} of {formatBytes(d.host.memTotal)} · disk{" "}
            {formatBytes(d.host.diskUsed)} of {formatBytes(d.host.diskTotal)}
          </Fact>
        )}
        <Fact k="fogd">{d.fog.goroutines} goroutines · {formatBytes(d.fog.heapBytes)} heap</Fact>
        {c && (
          <Fact k="Browser">
            {uaSummary(c.userAgent)} · {c.viewport.width}×{c.viewport.height} @{c.viewport.pixelRatio}x · {c.timeZone} · {c.theme}
            {!c.online && " · offline"}
          </Fact>
        )}
      </dl>

      {c && (
        <Section title="Browser" count={c.errors.length + failed.length} hot={c.errors.length + failed.length > 0} open={c.errors.length + failed.length > 0}>
          <Events title="Errors" events={c.errors} empty="No errors since the page loaded." />
          <Events title="Requests" events={c.requests} empty="No requests recorded." />
          <Events title="Pages visited" events={c.navigation} empty="Only this page." />
        </Section>
      )}

      {d.app && (
        <Section title={`App · ${d.app.name}`} open>
          <p className="text-hint text-ink-muted">
            wants {d.app.desiredState} · revision {d.app.currentRev}
            {d.app.deployedCommit && <> · commit <span className="font-mono">{d.app.deployedCommit.slice(0, 12)}</span></>}
          </p>
          {d.app.containers.length > 0 && (
            <ul className="space-y-0.5 text-hint">
              {d.app.containers.map((ct) => (
                <li key={ct.name} className="font-mono text-ink-muted">
                  {ct.name} <span className={ct.state === "running" ? "text-accent" : "text-warn"}>{ct.state}</span>{" "}
                  <span className="text-ink-muted">{ct.status} · {ct.image}</span>
                </li>
              ))}
            </ul>
          )}
          {d.app.deploys.map((dep) => (
            <details key={dep.id}>
              <summary className="cursor-pointer text-hint text-ink-muted hover:text-ink">
                Deploy #{dep.id} · rev {dep.rev} · <span className={dep.status === "failed" ? "text-warn" : ""}>{dep.status}</span> · {timeAgo(dep.startedAt)}
                {dep.actor && ` by ${dep.actor}`}
              </summary>
              {dep.log && <Pre>{dep.log}</Pre>}
            </details>
          ))}
          {d.app.logs && (
            <details>
              <summary className="cursor-pointer text-hint text-ink-muted hover:text-ink">Container logs</summary>
              <Pre>{d.app.logs}</Pre>
            </details>
          )}
        </Section>
      )}

      <Section title="fogd warnings" count={d.warnings.length} hot={d.warnings.length > 0} open={d.warnings.length > 0}>
        <Logs records={d.warnings} empty="No warnings in fogd's recent log." />
      </Section>
      <Section title="fogd log" count={d.logs.length}>
        <Logs records={d.logs} empty="fogd's log was empty." />
      </Section>

      <Section title="Open errors" count={d.errors.length} hot={d.errors.length > 0}>
        {d.errors.length === 0 ? <Empty>No open errors.</Empty> : (
          <ul className="space-y-1 text-hint">
            {d.errors.map((e, i) => (
              <li key={i} className="text-ink-muted">
                <span className="text-ink-muted">{e.origin ? `${e.origin} · ` : ""}{e.app}</span> {e.title}{" "}
                <span className="text-ink-muted">× {e.events} · {e.status} · {timeAgo(e.lastSeen)}</span>
              </li>
            ))}
          </ul>
        )}
      </Section>
      <Section title="Open issues" count={d.issues.length}>
        {d.issues.length === 0 ? <Empty>No open issues.</Empty> : (
          <ul className="space-y-1 text-hint">
            {d.issues.map((iss, i) => (
              <li key={i} className="text-ink-muted">
                <span className={iss.severity === "critical" ? "text-warn" : "text-ink-muted"}>{iss.severity}</span>{" "}
                {iss.app && <span className="text-ink-muted">{iss.app} · </span>}
                {iss.title}
              </li>
            ))}
          </ul>
        )}
      </Section>
      <Section title="Notices" count={d.notices.length}>
        {d.notices.length === 0 ? <Empty>No notices waiting.</Empty> : (
          <ul className="space-y-1 text-hint text-ink-muted">
            {d.notices.map((n, i) => <li key={i}>{n.app && <span className="text-ink-muted">{n.app} · </span>}{n.title}</li>)}
          </ul>
        )}
      </Section>
      <Section title="Recent activity" count={d.audit.length}>
        {d.audit.length === 0 ? <Empty>Nothing recorded.</Empty> : (
          <ul className="space-y-0.5 font-mono text-[11px]">
            {d.audit.map((a) => (
              <li key={a.id} className="break-words text-ink-muted">
                <span className="text-ink-muted">{clock(a.at)}</span> {a.actor || "fog"} <span className="text-ink">{a.action}</span> {a.subject}
              </li>
            ))}
          </ul>
        )}
      </Section>
      <Section title="Peers" count={d.peers.length}>
        {d.peers.length === 0 ? <Empty>Not paired with anyone.</Empty> : (
          <ul className="space-y-0.5 text-hint text-ink-muted">
            {d.peers.map((p) => (
              <li key={p.name}>
                {p.name}{" "}
                <span className="text-ink-muted">
                  {p.fogVersion && `fog ${versionLabel(p.fogVersion)} · `}
                  {p.unpairedAt ? "unpaired this fog" : p.lastSeenAt ? `seen ${timeAgo(p.lastSeenAt)}` : "never seen"}
                </span>
              </li>
            ))}
          </ul>
        )}
      </Section>
    </section>
  );
}

function Fact({ k, children }: { k: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-ink-muted">{k}</dt>
      <dd className="min-w-0 break-words text-ink-muted">{children}</dd>
    </>
  );
}

function Section({ title, count, hot, open, children }: {
  title: string;
  count?: number;
  hot?: boolean;
  open?: boolean;
  children: ReactNode;
}) {
  return (
    <details open={open} className="rounded-md border border-hair">
      <summary className="flex cursor-pointer items-center gap-2 px-3 py-2 text-sm text-ink hover:text-ink">
        {title}
        {count !== undefined && <span className={`text-xs tabular-nums ${hot ? "text-warn" : "text-ink-muted"}`}>{count}</span>}
      </summary>
      <div className="space-y-3 border-t border-hair px-3 py-3">{children}</div>
    </details>
  );
}

function Empty({ children }: { children: ReactNode }) {
  return <p className="text-hint text-ink-muted">{children}</p>;
}

function Pre({ children }: { children: ReactNode }) {
  return (
    <pre className="mt-2 max-h-80 overflow-auto whitespace-pre-wrap break-all rounded-md border border-hair bg-code p-2 text-[11px] text-ink-muted">
      {children}
    </pre>
  );
}

function clock(iso: string): string {
  const t = new Date(iso);
  return Number.isNaN(t.getTime()) ? iso : t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

function Logs({ records, empty }: { records: LogRecord[]; empty: string }) {
  if (records.length === 0) return <Empty>{empty}</Empty>;
  return (
    <div className="max-h-96 space-y-0.5 overflow-auto font-mono text-[11px]">
      {records.map((r, i) => (
        <div key={i} className="break-words">
          <span className="text-ink-muted">{clock(r.time)}</span>{" "}
          <span className={r.level === "WARN" || r.level === "ERROR" ? "text-warn" : "text-ink-muted"}>{r.level}</span>{" "}
          <span className="text-ink">{r.msg}</span> <span className="text-ink-muted">{r.attrs}</span>
        </div>
      ))}
    </div>
  );
}

function Events({ title, events, empty }: { title: string; events: TrailEvent[]; empty: string }) {
  return (
    <div>
      <h4 className="mb-1 text-hint text-ink-muted">{title}</h4>
      {events.length === 0 ? <Empty>{empty}</Empty> : (
        <ul className="space-y-0.5 font-mono text-[11px]">
          {[...events].reverse().map((e, i) => (
            <li key={i} className="break-words">
              <span className="text-ink-muted">{clock(e.at)}</span>{" "}
              <span className={e.kind === "nav" || /→ [23]\d\d/.test(e.message) ? "text-ink-muted" : "text-warn"}>{e.message}</span>
              {e.detail && (
                <details className="ml-4">
                  <summary className="cursor-pointer text-ink-muted hover:text-ink">detail</summary>
                  <pre className="whitespace-pre-wrap break-all text-ink-muted">{e.detail}</pre>
                </details>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
