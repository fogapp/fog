// Shared: everything on this box that a peer can reach, gathered in one
// place. The tree says where a folder lives; this says who else can see it,
// through which lens, and how much has actually crossed. Every row opens
// the real folder — nothing here is a copy or a second home for a file.

import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, type FileFedShare } from "../../api";
import { LoadError, Loading, errorText } from "../../components/Async";
import Button, { buttonClass } from "../../components/Button";
import FileIcon from "../../components/FileIcon";
import { DRIVE_GUTTER } from "./gutter";
import PeerShareComposer from "../../components/files/PeerShareComposer";
import { formatBytes, plural, timeAgo } from "../../lib/format";
import { paths } from "../../lib/paths";
import { filesKey } from "../../lib/useFiles";
import { federationQuery, peerSharesQuery } from "../../queries";
import { peerSharesRootKey } from "../../lib/filesFed";
import { baseOf, parentOf } from "../../lib/drivePath";
import { displayName } from "../../lib/displayName";

const link = "text-ink-muted hover:text-ink";

// A hand-picked entry opens where it actually lives: its folder, with the
// entry selected. Picks are named relative to the shared folder and may be
// a file or a folder, so selecting in the parent is the one link that fits
// both.
function pickHref(folder: string, pick: string): string {
  const rel = folder ? `${folder}/${pick}` : pick;
  return `${paths.files(parentOf(rel))}?select=${encodeURIComponent(baseOf(rel))}`;
}

export default function SharedView() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const shares = useQuery({
    ...peerSharesQuery,
  });
  const [editing, setEditing] = useState<FileFedShare | null>(null);
  const [confirming, setConfirming] = useState<number | null>(null);
  const [error, setError] = useState("");
  const stop = useMutation({
    mutationFn: (id: number) => del(`/api/files/peer-shares/${id}`),
    onSuccess: async () => {
      setConfirming(null);
      setError("");
      await Promise.all([
        qc.invalidateQueries({ queryKey: peerSharesRootKey }),
        qc.invalidateQueries({ queryKey: federationQuery.queryKey }),
        qc.invalidateQueries({ queryKey: filesKey }),
      ]);
    },
    onError: (e) => setError(errorText(e, "Could not stop sharing")),
  });

  if (shares.isPending) return <div className="flex-1 p-4"><Loading label="Reading what is shared…" /></div>;
  if (shares.isError) {
    return (
      <div className="flex-1 p-4">
        <LoadError label="Could not read what is shared." error={shares.error} onRetry={() => shares.refetch()} />
      </div>
    );
  }

  // Folder first, then peer: one folder shared twice reads as two lenses in
  // a row rather than two rows a screen apart.
  const list = [...(shares.data?.shares ?? [])].sort(
    (a, b) => a.path.localeCompare(b.path) || a.peer.localeCompare(b.peer),
  );

  if (list.length === 0) return <SharedEmpty />;

  const peers = new Set(list.map((sh) => sh.peer));

  return (
    <>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <ul className="divide-y divide-divider/70">
          {list.map((sh) => {
            const picks = sh.scope === "picked" ? sh.picks ?? [] : [];
            return (
              <li key={sh.id} className={`flex items-start gap-2.5 py-2.5 ${DRIVE_GUTTER}`}>
                <FileIcon category="folder" className="mt-0.5 h-5 w-5 shrink-0 opacity-80" />
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-baseline gap-x-2">
                    <Link to={paths.files(sh.path)} className="truncate text-sm font-medium text-ink hover:underline">
                      {sh.title}
                    </Link>
                    <span className="truncate font-mono text-[11px] text-ink-muted" title={displayName(sh.path)}>
                      {sh.path ? `Drive/${displayName(sh.path)}` : "Drive"}
                    </span>
                  </div>
                  <p className="mt-0.5 text-xs text-ink-muted">{sh.impact}</p>
                  {picks.length > 0 && (
                    <div className="mt-1 flex flex-wrap gap-1">
                      {picks.map((p) => (
                        <button
                          key={p}
                          type="button"
                          onClick={() => navigate(pickHref(sh.path, p))}
                          className="rounded border border-veil-edge bg-veil px-1.5 py-0.5 font-mono text-[11px] text-ink-muted hover:border-fog-600 hover:text-ink"
                        >
                          {p}
                        </button>
                      ))}
                    </div>
                  )}
                  <div className="mt-1.5 flex flex-wrap items-center gap-2 text-xs">
                    <Link to={paths.files(sh.path)} className={link}>Open folder</Link>
                    <button type="button" onClick={() => setEditing(sh)} className={link}>Edit lens</button>
                    {confirming === sh.id ? (
                      <>
                        <span className="text-warn">Stop sharing? Their copy stays until they remove it.</span>
                        <Button size="sm" variant="danger" onClick={() => stop.mutate(sh.id)} disabled={stop.isPending}>Stop</Button>
                        <button type="button" onClick={() => setConfirming(null)} className="text-ink-muted hover:text-ink">Keep</button>
                      </>
                    ) : (
                      <button type="button" onClick={() => setConfirming(sh.id)} className="text-warn hover:text-warn">Stop sharing</button>
                    )}
                  </div>
                </div>
                <div className="shrink-0 space-y-0.5 text-right text-[11px] text-ink-muted">
                  <div className={sh.peer ? "text-accent" : "text-warn"}>{sh.peer || "peer no longer paired"}</div>
                  <div>{plural(sh.files, "file")} · {formatBytes(sh.bytes)}</div>
                  <div>
                    {sh.bytesServed > 0
                      ? `${formatBytes(sh.bytesServed)} fetched${sh.lastFetchedAt ? `, last ${timeAgo(sh.lastFetchedAt)}` : ""}`
                      : "not fetched yet"}
                  </div>
                </div>
              </li>
            );
          })}
        </ul>
        {error && <p className={`py-2 text-xs text-warn ${DRIVE_GUTTER}`}>{error}</p>}
        {editing && <PeerShareComposer share={editing} onClose={() => setEditing(null)} />}
      </div>
      <div className={`flex shrink-0 items-center gap-3 border-t border-hair py-1.5 text-[11px] text-ink-muted ${DRIVE_GUTTER}`}>
        <span>
          {plural(list.length, "share")} · {plural(peers.size, "peer")}
        </span>
        <span className="ml-auto hidden sm:inline">a row is one folder through one lens · the same folder can go to more than one peer</span>
      </div>
    </>
  );
}

function SharedEmpty() {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-3 p-8 text-center">
      <FileIcon category="folder" className="h-16 w-16 opacity-60" />
      <p className="text-sm text-ink-muted">Nothing is shared with a peer.</p>
      <p className="max-w-md text-xs text-ink-muted">
        Pick a folder in Files and choose <strong className="font-medium text-ink-muted">Share with a peer…</strong> in the pane beside it. What you
        share shows up here, with the lens it goes out through — and anything marked keep on this box never crosses, however wide the lens.
      </p>
      <Link to={paths.federation} className={buttonClass()}>
        Connect
      </Link>
    </div>
  );
}
