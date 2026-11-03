// Configuration issues (the Inbox's Issues and Resolved tabs). Fog
// re-derives what should be true against what is true every sweep; the
// discrepancies land here. Open notices lead the Issues list — steps only
// a person can take, which stay until dismissed (see Notices.tsx). Issues holds what is still open; Resolved holds
// what a human acknowledged and what stopped reproducing on its own — one
// archive rather than the two sub-tabs this used to carry, which split a
// short list into two shorter ones for no gain.

import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { get, post, type Issue, type IssuesResponse, type IssuesSummary } from "../../api";
import { LoadError, Loading, StaleNote, errorText, phaseOf } from "../../components/Async";
import Button from "../../components/Button";
import { timeAgo } from "../../lib/format";
import { inboxSummaryKey } from "../../lib/useInbox";
import { NoticeBadge, NoticeDetail, noticeSel, useDismissNotice, useNotices } from "./Notices";
import { DetailPane, EmptyList, InboxRow, InboxSplit, NoSelection, Tag } from "./Split";
import { displayName } from "../../lib/displayName";

const CATEGORY_LABELS: Record<string, string> = {
  database: "Database",
  backups: "Backups",
  network: "Network",
  storage: "Storage",
  runtime: "Runtime",
  host: "Fog host",
};

const SEVERITY_RANK: Record<string, number> = { critical: 0, warning: 1, info: 2 };

function SeverityBadge({ issue }: { issue: Issue }) {
  if (issue.status === "resolved") {
    return (
      <span className="rounded-full bg-accent/10 px-2 py-px text-[11px] font-medium text-accent">
        Resolved
      </span>
    );
  }
  if (issue.status === "acked") {
    return (
      <span className="rounded-full border border-hair px-2 py-px text-[11px] font-medium text-ink-muted">
        Acknowledged
      </span>
    );
  }
  const tones: Record<string, string> = {
    critical: "border-warn/40 bg-warn/10 text-warn",
    warning: "border-transparent bg-warn/10 text-warn",
    info: "border-hair text-ink-muted",
  };
  return (
    <span className={`rounded-full border px-2 py-px text-[11px] font-medium capitalize ${tones[issue.severity]}`}>
      {issue.severity}
    </span>
  );
}

export default function Issues({ archive }: { archive?: boolean }) {
  const qc = useQueryClient();
  const [params, setParams] = useSearchParams();
  const [actionError, setActionError] = useState("");

  // "open" and "done" are the two halves of the whole set: the tab you are
  // on decides which, and there is no third state to filter to.
  const status = archive ? "done" : "open";
  const q = useQuery({
    queryKey: ["issues", status],
    queryFn: () => get<IssuesResponse>(`/api/issues?status=${status}`),
    refetchInterval: 15000,
  });

  const refresh = () =>
    Promise.all([
      qc.invalidateQueries({ queryKey: ["issues"] }),
      qc.invalidateQueries({ queryKey: ["notices"] }),
      qc.invalidateQueries({ queryKey: inboxSummaryKey }),
    ]);

  const act = useMutation({
    mutationFn: ({ id, action }: { id: number; action: "ack" | "unack" }) =>
      post(`/api/issues/${id}/${action}`),
    onSuccess: () => {
      setActionError("");
      return refresh();
    },
    onError: (e) => setActionError(errorText(e, "Action failed")),
  });
  // Fixes fog can apply itself: mount what an app cannot see of the apps it
  // uses, or restart one holding a dead handle on a folder.
  const fix = useMutation({
    mutationFn: ({ kind, app }: { kind: string; app: string }) =>
      kind === "app-unwired"
        ? post(`/api/apps/${encodeURIComponent(app)}/wire`)
        : post(`/api/apps/${encodeURIComponent(app)}/actions`, { action: "restart" }),
    onSuccess: () => {
      setActionError("");
      return refresh();
    },
    onError: (e) => setActionError(errorText(e, "Fix failed")),
  });

  // "Scanning…" holds until the sweep's own timestamp advances past where it
  // stood when the button was pressed — the POST returning only means the
  // sweep started, and refreshing on a timer risks presenting pre-scan data
  // as the scan's result.
  const [scanning, setScanning] = useState(false);
  const gone = useRef(false);
  useEffect(() => {
    gone.current = false;
    return () => { gone.current = true; };
  }, []);

  const scan = useMutation({
    mutationFn: async () => {
      const baseline = q.data?.lastScanAt ?? "";
      await post("/api/issues/scan");
      return baseline;
    },
    onSuccess: (baseline) => {
      setActionError("");
      setScanning(true);
      void watchScan(baseline);
    },
    onError: (e) => setActionError(errorText(e, "Scan failed")),
  });

  async function watchScan(baseline: string) {
    const deadline = Date.now() + 60_000;
    for (;;) {
      await new Promise((r) => setTimeout(r, 1500));
      if (gone.current) return;
      try {
        const s = await get<IssuesSummary>("/api/issues/summary");
        if (gone.current) return;
        if (s.lastScanAt && s.lastScanAt !== baseline) break;
      } catch {
        // A dropped poll isn't an answer; keep asking until the deadline.
      }
      if (Date.now() > deadline) {
        setActionError("The scan hasn't reported finishing after a minute — it may still be running. The list shows the last recorded sweep.");
        break;
      }
    }
    await refresh();
    if (!gone.current) setScanning(false);
  }

  const issues = [...(q.data?.issues ?? [])].sort(
    (a, b) => SEVERITY_RANK[a.severity] - SEVERITY_RANK[b.severity] || b.lastSeen.localeCompare(a.lastSeen),
  );
  // Notices only on the open list: a dismissed notice is gone, not archived.
  const noticesQ = useNotices(!archive);
  const notices = archive ? [] : noticesQ.data?.notices ?? [];
  const issuesPhase = phaseOf(q, q.data?.issues.length);
  // Empty means empty of both; a notice alone is a list worth showing.
  const phase = issuesPhase === "empty" && notices.length > 0 ? "ready" : issuesPhase;

  const sel = params.get("sel");
  const selectedId = sel && /^\d+$/.test(sel) ? Number(sel) : null;
  // Acknowledging moves an issue to the other tab, and dismissing removes a
  // notice, so an id can outlive its presence in this list — the pane
  // empties rather than showing a ghost.
  const current = issues.find((i) => i.id === selectedId) ?? null;
  const currentNotice = notices.find((n) => noticeSel(n.id) === sel) ?? null;
  const select = (id: string | number | null) => {
    const next = new URLSearchParams(params);
    if (id === null) next.delete("sel");
    else next.set("sel", String(id));
    setParams(next, { replace: true });
  };
  const dismiss = useDismissNotice(
    () => select(null),
    (e) => setActionError(errorText(e, "Couldn't dismiss the notice")),
  );

  return (
    <InboxSplit
      selected={!!current || !!currentNotice}
      list={
        <>
          {!archive && (
            <div className="flex items-center justify-between gap-3 border-b border-divider px-4 py-2">
              <span className="text-xs text-ink-muted">
                {q.data ? (q.data.lastScanAt ? `Scanned ${timeAgo(q.data.lastScanAt)}` : "No scan yet") : ""}
              </span>
              <Button size="sm" onClick={() => scan.mutate()} disabled={scan.isPending || scanning}>
                {scan.isPending || scanning ? "Scanning…" : "Scan now"}
              </Button>
            </div>
          )}
          {actionError && <p className="px-4 py-2 text-sm text-warn">{actionError}</p>}
          {!archive && noticesQ.isError && (
            <p className="px-4 py-2 text-sm text-warn">{errorText(noticesQ.error, "Couldn't load notices")} — issues below are still current.</p>
          )}

          {phase === "loading" ? (
            <div className="p-4">
              <Loading label="Loading issues…" />
            </div>
          ) : phase === "error" ? (
            <div className="p-4">
              <LoadError label="Couldn't load issues." error={q.error} onRetry={() => q.refetch()} />
            </div>
          ) : phase === "empty" ? (
            <EmptyList
              title={archive ? "Nothing here yet." : "No configuration issues. 🎉"}
              hint={
                archive
                  ? "Issues you acknowledge, and issues that stop reproducing, are kept here as history."
                  : "Fog scans hourly; anything it finds lands here before it becomes a failure, along with any step only you can take."
              }
            />
          ) : (
            <>
            {notices.map((n) => (
              <InboxRow
                key={noticeSel(n.id)}
                selected={noticeSel(n.id) === sel}
                onClick={() => select(noticeSel(n.id))}
                badge={<NoticeBadge />}
                title={displayName(n.title)}
                when={timeAgo(n.createdAt)}
                tag={n.app ? <Tag>{n.app}</Tag> : undefined}
                subtitle={n.body ? displayName(n.body.split("\n")[0]) : undefined}
              />
            ))}
            {issues.map((i) => (
              <InboxRow
                key={i.id}
                selected={i.id === selectedId}
                onClick={() => select(i.id)}
                badge={<SeverityBadge issue={i} />}
                title={i.title}
                when={timeAgo(i.lastSeen)}
                tag={i.appName ? <Tag>{i.appName}</Tag> : undefined}
                subtitle={CATEGORY_LABELS[i.category || "host"] ?? i.category}
              />
            ))}
            </>
          )}
          {q.isError && q.data && (
            <div className="p-4">
              <StaleNote what="this list" />
            </div>
          )}
        </>
      }
      detail={
        currentNotice ? (
          <NoticeDetail notice={currentNotice} dismiss={dismiss} onBack={() => select(null)} />
        ) : current ? (
          <DetailPane onBack={() => select(null)}>
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <SeverityBadge issue={current} />
                  <span className="text-hint text-ink-muted">
                    {CATEGORY_LABELS[current.category || "host"] ?? current.category}
                  </span>
                  {current.appName && <Tag>{current.appName}</Tag>}
                </div>
                <h2 className="mt-2 break-words text-lg font-semibold text-ink">{current.title}</h2>
              </div>
              {current.status === "open" && (
                <Button size="sm" onClick={() => act.mutate({ id: current.id, action: "ack" })} disabled={act.isPending}>
                  Acknowledge
                </Button>
              )}
              {current.status === "acked" && (
                <Button size="sm" onClick={() => act.mutate({ id: current.id, action: "unack" })} disabled={act.isPending}>
                  Reopen
                </Button>
              )}
            </div>

            {current.detail && <p className="mt-4 text-sm text-ink-muted">{current.detail}</p>}

            {current.fix && (
              <div className="mt-4 rounded-md border border-accent/30 bg-accent/10 px-3 py-2">
                <p className="subhead text-accent">Fix</p>
                <p className="mt-1 whitespace-pre-line break-words text-sm text-accent">{current.fix}</p>
                {current.appName && (current.kind === "app-unwired" || current.kind === "app-mount-stale") && current.status !== "resolved" && (
                  <Button
                    size="sm"
                    className="mt-2"
                    onClick={() => fix.mutate({ kind: current.kind, app: current.appName! })}
                    disabled={fix.isPending}
                  >
                    {current.kind === "app-unwired" ? "Mount them" : `Restart ${current.appName}`}
                  </Button>
                )}
                {current.fixUrl && (
                  <Link
                    to={current.fixUrl}
                    className="mt-1.5 inline-block text-sm font-medium text-accent underline underline-offset-2 hover:text-accent"
                  >
                    Go to the fix →
                  </Link>
                )}
              </div>
            )}

            <dl className="mt-5 space-y-1 border-t border-hair pt-4 text-xs text-ink-muted">
              <Fact label="First seen" value={timeAgo(current.firstSeen)} />
              <Fact label="Last seen" value={timeAgo(current.lastSeen)} />
              {current.ackedAt && <Fact label="Acknowledged" value={timeAgo(current.ackedAt)} />}
              {current.resolvedAt && <Fact label="Resolved" value={timeAgo(current.resolvedAt)} />}
              <Fact label="Check" value={current.kind} />
            </dl>
          </DetailPane>
        ) : (
          <NoSelection label={archive ? "Pick something to see its history." : "Pick an issue to see what fog found."} />
        )
      }
    />
  );
}

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex gap-2">
      <dt className="w-28 shrink-0">{label}</dt>
      <dd className="min-w-0 break-words text-ink-muted">{value}</dd>
    </div>
  );
}
