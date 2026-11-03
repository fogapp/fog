// The Trash: what was deleted, where it came from, and the two ways out —
// back where it was, or gone for good. Entries leave on their own after
// thirty days, and the page says so.

import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { get, type TrashItem } from "../../api";
import { LoadError, Loading, errorText, phaseOf } from "../../components/Async";
import Button from "../../components/Button";
import FileIcon from "../../components/FileIcon";
import PageHeader from "../../components/PageHeader";
import { categoryOf } from "../../lib/fileKinds";
import { formatBytes, timeAgo } from "../../lib/format";
import { paths } from "../../lib/paths";
import { filesTrashKey, useFilesActions } from "../../lib/useFilesActions";
import Toast, { type ToastState } from "./Toast";
import { displayName, FileName } from "../../lib/displayName";

export default function Trash() {
  const trash = useQuery({ queryKey: filesTrashKey, queryFn: () => get<{ items: TrashItem[] }>("/api/files/trash") });
  const act = useFilesActions();
  const [toast, setToast] = useState<ToastState | null>(null);
  const [armed, setArmed] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const items = trash.data?.items;
  const phase = phaseOf(trash, items?.length);
  const bytes = items?.reduce((s, it) => s + it.size, 0) ?? 0;

  const run = async (key: string, fn: () => Promise<void>, ok: string) => {
    setBusy(key);
    try {
      await fn();
      setToast({ message: ok });
    } catch (e) {
      setToast({ message: errorText(e, "That did not work"), tone: "error" });
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title="Trash"
        description={`Deleted files wait here for 30 days, then go for good. ${items && items.length > 0 ? `${items.length} item${items.length === 1 ? "" : "s"} · ${formatBytes(bytes)}` : ""}`}
        actions={
          <>
            <Link to={paths.files()} className="rounded-md border border-edge px-3 py-1.5 text-sm font-medium text-ink hover:bg-fog-800">
              Back to Drive
            </Link>
            {phase === "ready" && (
              armed ? (
                <Button variant="danger" disabled={busy !== null} onClick={() => run("empty", async () => { await act.emptyTrash(); setArmed(false); }, "Trash emptied")}>
                  Really delete everything
                </Button>
              ) : (
                <Button variant="danger" onClick={() => setArmed(true)}>Empty trash…</Button>
              )
            )}
          </>
        }
      />

      {phase === "loading" && <Loading label="Reading the trash…" />}
      {phase === "error" && <LoadError label="Could not read the trash." error={trash.error} onRetry={() => trash.refetch()} />}
      {phase === "empty" && (
        <div className="flex flex-col items-center gap-2 py-16 text-center">
          <FileIcon category="folder" className="h-14 w-14 opacity-50" />
          <p className="text-sm text-ink-muted">The trash is empty.</p>
        </div>
      )}
      {phase === "ready" && items && (
        <div className="overflow-hidden rounded-lg border border-hair">
          <div className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_6rem_7rem_auto] gap-3 border-b border-hair bg-fog-950 px-3 py-1.5 caps-label">
            <span>Name</span><span>Was in</span><span className="text-right">Size</span><span>Deleted</span><span />
          </div>
          {items.map((it) => {
            const dir = it.path.includes("/") ? it.path.slice(0, it.path.lastIndexOf("/")) : "";
            return (
              <div key={it.id} className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_6rem_7rem_auto] items-center gap-3 border-b border-divider px-3 py-2 text-sm last:border-0">
                <span className="flex min-w-0 items-center gap-2.5 text-ink">
                  <FileIcon category={categoryOf(it)} />
                  <span className="truncate"><FileName name={it.name} /></span>
                </span>
                <Link to={paths.files(dir)} className="truncate font-mono text-xs text-ink-muted hover:text-ink hover:underline" title={dir || "/"}>
                  {dir || "/"}
                </Link>
                <span className="text-right tabular-nums text-ink-muted">{formatBytes(it.size)}</span>
                <span className="text-ink-muted" title={new Date(it.trashedAt).toLocaleString()}>{timeAgo(it.trashedAt)}</span>
                <span className="flex gap-1">
                  <Button size="sm" disabled={busy !== null} onClick={() => run(it.id, async () => { await act.putBack([it.id]); }, `Restored ${displayName(it.name)}`)}>
                    Put back
                  </Button>
                  <Button size="sm" variant="ghost" disabled={busy !== null} onClick={() => run(it.id, () => act.purge(it.id), `Deleted ${displayName(it.name)} for good`)}>
                    Delete
                  </Button>
                </span>
              </div>
            );
          })}
        </div>
      )}
      <Toast toast={toast} onDone={() => setToast(null)} />
    </div>
  );
}
