// Copies and moves that take time, in the corner: what is running, how
// far along, and a Cancel. Polled at one second while anything runs and
// left alone otherwise; finished jobs linger long enough to be read.

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { get, post, type FilesJob } from "../../api";
import { Spinner } from "../../components/Async";
import { formatBytes } from "../../lib/format";
import { filesKey } from "../../lib/useFiles";
import { filesJobsKey } from "../../lib/useFilesActions";
import { useEffect, useRef, useState } from "react";
import { displayName } from "../../lib/displayName";

const SHOW_KINDS = new Set(["copy", "root-move", "restore", "zip"]);

export default function JobsTray() {
  const qc = useQueryClient();
  const jobs = useQuery({
    queryKey: filesJobsKey,
    queryFn: () => get<{ jobs: FilesJob[] }>("/api/files/jobs"),
    refetchInterval: (q) => (q.state.data?.jobs.some((j) => j.state === "running") ? 1000 : 15_000),
  });
  const [dismissed, setDismissed] = useState<Set<string>>(() => new Set());
  // When a job finishes, the folders it touched changed: refetch listings.
  const seenRunning = useRef<Set<string>>(new Set());
  useEffect(() => {
    const list = jobs.data?.jobs ?? [];
    let finished = false;
    for (const j of list) {
      if (j.state === "running") seenRunning.current.add(j.id);
      else if (seenRunning.current.has(j.id)) {
        seenRunning.current.delete(j.id);
        finished = true;
      }
    }
    if (finished) qc.invalidateQueries({ queryKey: filesKey });
  }, [jobs.data, qc]);

  const shown = (jobs.data?.jobs ?? []).filter((j) => SHOW_KINDS.has(j.kind) && !dismissed.has(j.id) && recent(j)).slice(0, 5);
  if (shown.length === 0) return null;

  return (
    <div className="fixed bottom-4 right-4 z-30 w-80 space-y-2">
      {shown.map((j) => {
        const pct = j.total > 0 ? Math.min(100, Math.round((j.done / j.total) * 100)) : null;
        return (
          <div key={j.id} className="space-y-1.5 rounded-lg border border-veil-edge bg-fog-900 p-3 text-xs shadow-2xl shadow-black/40">
            <div className="flex items-center gap-2">
              {j.state === "running" && <Spinner />}
              <span className={`min-w-0 flex-1 truncate font-medium ${j.state === "failed" ? "text-warn" : "text-ink"}`}>{displayName(j.label)}</span>
              {j.state === "running" ? (
                <button onClick={() => post(`/api/files/jobs/${j.id}/cancel`)} className="text-ink-muted hover:text-ink">Cancel</button>
              ) : (
                <button onClick={() => setDismissed((d) => new Set(d).add(j.id))} className="text-ink-muted hover:text-ink" title="Dismiss">✕</button>
              )}
            </div>
            {j.state === "running" && (
              <>
                {pct !== null && (
                  <div className="h-1.5 overflow-hidden rounded-full bg-fog-800" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
                    <div className="h-full bg-accent" style={{ width: `${pct}%` }} />
                  </div>
                )}
                <p className="truncate text-ink-muted">
                  {j.current ? displayName(j.current) : "working…"}
                  {j.total > 0 && ` · ${formatBytes(j.done)} of ${formatBytes(j.total)}`}
                </p>
              </>
            )}
            {j.state === "done" && <p className="text-accent">Done{j.items > 0 ? ` · ${j.items} file${j.items === 1 ? "" : "s"}` : ""}</p>}
            {j.state === "failed" && <p className="break-words text-warn">{j.error}</p>}
            {j.state === "cancelled" && <p className="text-ink-muted">Cancelled</p>}
          </div>
        );
      })}
    </div>
  );
}

// Finished jobs older than a minute are history, not a tray.
function recent(j: FilesJob): boolean {
  if (j.state === "running") return true;
  const t = j.finishedAt ? new Date(j.finishedAt).getTime() : 0;
  return Date.now() - t < 60_000;
}
