// The upload tray: one line of truth for the whole batch (how many, how
// far, how fast), a row per file with pause/resume/cancel, and the one
// question an upload can ask — a name is taken — answered here for one
// file or for all of them.

import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Spinner } from "../../components/Async";
import { formatBytes } from "../../lib/format";
import { filesKey } from "../../lib/useFiles";
import { cancel, clearFinished, onUploadLanded, pause, resolveConflict, resume, summary, useUploads, type UploadItem } from "../../lib/uploads";

export default function UploadTray() {
  const items = useUploads();
  const qc = useQueryClient();
  const [open, setOpen] = useState(true);

  // Every landed file changes a folder; the listing re-reads it.
  useEffect(() => onUploadLanded((dir) => qc.invalidateQueries({ queryKey: [...filesKey, "ls", dir] })), [qc]);

  if (items.length === 0) return null;
  const s = summary(items);
  const pct = s.total > 0 ? Math.round((s.sent / s.total) * 100) : 100;
  const conflicts = items.filter((it) => it.status === "conflict");
  const label =
    s.active > 0
      ? `Uploading ${s.active} file${s.active === 1 ? "" : "s"} · ${pct}%${s.rate > 0 ? ` · ${formatBytes(s.rate)}/s` : ""}`
      : `${s.done} uploaded${s.failed ? `, ${s.failed} failed` : ""}`;

  return (
    <div className="fixed bottom-4 right-4 z-30 w-96 overflow-hidden rounded-lg border border-veil-edge bg-fog-900 text-xs shadow-2xl shadow-black/40">
      <div className="flex items-center gap-2 border-b border-hair px-3 py-2">
        {s.active > 0 && <Spinner />}
        <span className="min-w-0 flex-1 truncate font-medium text-ink">{label}</span>
        <button onClick={() => setOpen((o) => !o)} className="text-ink-muted hover:text-ink" title={open ? "Collapse" : "Expand"}>
          {open ? "▾" : "▸"}
        </button>
        {s.active === 0 && (
          <button onClick={clearFinished} className="text-ink-muted hover:text-ink" title="Clear">✕</button>
        )}
      </div>
      {s.active > 0 && (
        <div className="h-1 bg-fog-800" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
          <div className="h-full bg-accent transition-[width]" style={{ width: `${pct}%` }} />
        </div>
      )}
      {open && (
        <ul className="max-h-72 divide-y divide-divider/60 overflow-y-auto">
          {conflicts.length > 0 && (
            <li className="space-y-1.5 bg-warn/10 px-3 py-2 text-warn">
              <p>
                {conflicts.length === 1 ? `“${conflicts[0].file.name}” already exists.` : `${conflicts.length} files already exist.`}
              </p>
              <div className="flex flex-wrap gap-1">
                <button onClick={() => resolveConflict(conflicts[0].key, "rename", true)} className="rounded border border-warn/30 px-2 py-0.5 font-medium hover:bg-warn/10">Keep both</button>
                <button onClick={() => resolveConflict(conflicts[0].key, "replace", true)} className="rounded border border-warn/30 px-2 py-0.5 font-medium hover:bg-warn/10">Replace</button>
                <button onClick={() => resolveConflict(conflicts[0].key, "skip", true)} className="rounded border border-warn/30 px-2 py-0.5 font-medium hover:bg-warn/10">Skip</button>
                <span className="self-center text-warn">applies to all</span>
              </div>
            </li>
          )}
          {items.map((it) => (
            <Row key={it.key} it={it} />
          ))}
        </ul>
      )}
    </div>
  );
}

function Row({ it }: { it: UploadItem }) {
  const pct = it.size > 0 ? Math.round((it.offset / it.size) * 100) : 100;
  return (
    <li className="flex items-center gap-2 px-3 py-1.5">
      <div className="min-w-0 flex-1">
        <div className="flex items-baseline gap-2">
          <span className="min-w-0 flex-1 truncate text-ink" title={it.dest}>{it.file.name}</span>
          <span className="shrink-0 text-ink-muted">
            {it.status === "uploading" && `${pct}%`}
            {it.status === "queued" && "waiting"}
            {it.status === "paused" && "paused"}
            {it.status === "done" && <span className="text-accent">done</span>}
            {it.status === "skipped" && "skipped"}
            {it.status === "cancelled" && "cancelled"}
            {it.status === "conflict" && <span className="text-warn">name taken</span>}
            {it.status === "error" && <span className="text-warn">failed</span>}
          </span>
        </div>
        {(it.status === "uploading" || it.status === "paused") && (
          <div className="mt-1 h-1 overflow-hidden rounded-full bg-fog-800">
            <div className={`h-full ${it.status === "paused" ? "bg-fog-500" : "bg-accent"}`} style={{ width: `${pct}%` }} />
          </div>
        )}
        {it.status === "error" && <p className="truncate text-warn" title={it.error}>{it.error}</p>}
      </div>
      <div className="flex shrink-0 gap-1 text-ink-muted">
        {it.status === "uploading" && <button onClick={() => pause(it.key)} className="hover:text-ink" title="Pause">⏸</button>}
        {(it.status === "paused" || it.status === "error") && <button onClick={() => resume(it.key)} className="hover:text-ink" title="Resume">▶</button>}
        {(it.status === "uploading" || it.status === "queued" || it.status === "paused" || it.status === "error" || it.status === "conflict") && (
          <button onClick={() => cancel(it.key)} className="hover:text-ink" title="Cancel">✕</button>
        )}
      </div>
    </li>
  );
}
