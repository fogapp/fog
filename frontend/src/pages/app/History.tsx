// The history tab: past deploys with their logs, and config revisions to
// roll back to. Two different questions — "what happened to this app" and
// "what has it been configured as" — so they're two tabs rather than two
// tables you scroll past each other to reach.

import { Fragment, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { post, type AppDetail as AppDetailT } from "../../api";
import { errorText } from "../../components/Async";
import Tabs, { useTabParam } from "../../components/Tabs";

const TABS = ["deploys", "revisions"] as const;

export function History({ name, detail, onAction }: { name: string; detail: AppDetailT; onAction: () => Promise<unknown> }) {
  const [tab, setTab] = useTabParam(TABS);
  const [error, setError] = useState("");
  const [openLog, setOpenLog] = useState<number | null>(null);
  const rollback = useMutation({
    mutationFn: (rev: number) => post(`/api/apps/${name}/actions`, { action: "rollback", rev }),
    onSuccess: onAction,
    onError: (e) => setError(errorText(e, "Rollback failed")),
  });

  return (
    <div className="space-y-4">
      <Tabs
        label="History view"
        active={tab}
        onChange={setTab}
        items={[
          { id: "deploys", label: "Deploys", count: detail.deploys.length },
          { id: "revisions", label: "Config revisions", count: detail.revisions.length },
        ]}
      />

      {error && <p className="text-sm text-warn">{error}</p>}

      {tab === "deploys" ? (
        detail.deploys.length === 0 ? (
          <Empty>No deploys yet.</Empty>
        ) : (
          <table className="w-full text-left text-cell">
            <thead>
              <tr className="caps-label border-b border-hair-strong">
                <th className="py-2 pr-3 font-medium">Started</th>
                <th className="py-2 pr-3 font-medium">Revision</th>
                <th className="py-2 pr-3 font-medium">Result</th>
                <th className="py-2 pr-3 font-medium">By</th>
                <th className="py-2" />
              </tr>
            </thead>
            <tbody>
              {detail.deploys.map((d) => (
                <Fragment key={d.id}>
                  <tr className="border-b border-divider">
                    <td className="py-2 pr-3 text-ink-muted">{new Date(d.startedAt).toLocaleString()}</td>
                    <td className="py-2 pr-3 text-ink-muted">rev {d.rev}</td>
                    <td className={`py-2 pr-3 ${d.status === "succeeded" ? "text-accent" : d.status === "failed" ? "text-warn" : "text-warn"}`}>
                      {d.status}
                    </td>
                    <td className="py-2 pr-3 text-ink-muted">{d.actor}</td>
                    <td className="py-2 text-right">
                      {d.log && (
                        <button onClick={() => setOpenLog(openLog === d.id ? null : d.id)} className="text-xs text-ink-muted hover:text-ink">
                          {openLog === d.id ? "hide log" : "log"}
                        </button>
                      )}
                    </td>
                  </tr>
                  {openLog === d.id && (
                    <tr>
                      <td colSpan={5}>
                        {d.logTruncated && (
                          <p className="mt-1 text-xs text-ink-muted">Showing the end of this log; earlier output is trimmed.</p>
                        )}
                        <pre className="my-1 max-h-48 overflow-auto rounded border border-hair bg-code p-2 text-xs text-ink-muted">{d.log}</pre>
                      </td>
                    </tr>
                  )}
                </Fragment>
              ))}
            </tbody>
          </table>
        )
      ) : detail.revisions.length === 0 ? (
        <Empty>No config revisions yet — the first save of this app's config starts the list.</Empty>
      ) : (
        <table className="w-full text-left text-cell">
          <thead>
            <tr className="caps-label border-b border-hair-strong">
              <th className="py-2 pr-3 font-medium">Revision</th>
              <th className="py-2 pr-3 font-medium">Saved</th>
              <th className="py-2 pr-3 font-medium">Comment</th>
              <th className="py-2 pr-3 font-medium">By</th>
              <th className="py-2" />
            </tr>
          </thead>
          <tbody>
            {detail.revisions.map((r) => (
              <tr key={r.id} className="border-b border-divider">
                <td className="py-2 pr-3 text-ink-muted">
                  rev {r.rev}
                  {r.rev === detail.app.currentRev && (
                    <span className="ml-2 rounded-full bg-fog-700 px-1.5 py-0.5 text-[10px] font-medium text-ink">current</span>
                  )}
                </td>
                <td className="py-2 pr-3 text-ink-muted">{new Date(r.createdAt).toLocaleString()}</td>
                <td className="py-2 pr-3 text-ink-muted">{r.comment || "—"}</td>
                <td className="py-2 pr-3 text-ink-muted">{r.actor}</td>
                <td className="py-2 text-right">
                  {r.rev !== detail.app.currentRev && (
                    <button onClick={() => rollback.mutate(r.rev)} disabled={rollback.isPending}
                      className="text-xs text-ink-muted hover:text-ink disabled:opacity-50">
                      {rollback.isPending && rollback.variables === r.rev ? "deploying…" : "deploy this"}
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-dashed border-edge p-8 text-center text-sm text-ink-muted">
      {children}
    </div>
  );
}
