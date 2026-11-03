// The right-hand pane: everything known about what is selected. One item
// gets its facts (kind, size, dates, digest, where its duplicates are);
// several get a tally; nothing selected describes the folder itself. Every
// fact waits for the stat call behind it — the list row is a summary, the
// pane is the record.

import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { get, type FileNode, type FileShare } from "../../api";
import { Loading, LoadError } from "../../components/Async";
import CopyButton from "../../components/CopyButton";
import FileIcon from "../../components/FileIcon";
import PlayBadge from "../../components/PlayBadge";
import { categoryOf, kindLabel } from "../../lib/fileKinds";
import { formatBytes } from "../../lib/format";
import { paths } from "../../lib/paths";
import { rawUrl, thumbUrl, useFileDetail, zipUrl } from "../../lib/useFiles";
import { filesSharesKey, useFilesActions } from "../../lib/useFilesActions";
import { timeAgo } from "../../lib/format";
import { displayName, FileName } from "../../lib/displayName";

export default function DetailPane({
  folder,
  hidden = 0,
  selection,
  at,
  onOpen,
  onQuickLook,
  onToggleExclude,
  onToggleKeep,
  onShare,
  onToggleOffline,
  onReject,
  rejectArmed,
  onArmReject,
  onDuplicate,
}: {
  folder: FileNode | undefined;
  // Entries the listing leaves out of this folder — the reserved folders at
  // the root, which are tabs of their own. The pane describes what the list
  // shows, so a root that lists three things must not say four.
  hidden?: number;
  selection: FileNode[];
  // Browsing a snapshot: facts come from the row, bytes from the snapshot.
  at?: number;
  onOpen: (n: FileNode) => void;
  onQuickLook: (n: FileNode) => void;
  onToggleExclude?: (n: FileNode, excluded: boolean) => void;
  onToggleKeep?: (n: FileNode, keep: boolean) => void;
  onShare?: (n: FileNode) => void;
  onToggleOffline?: (n: FileNode, keep: boolean) => void;
  onReject?: (n: FileNode) => void;
  rejectArmed?: string | null;
  onArmReject?: (path: string | null) => void;
  onDuplicate?: () => void;
}) {
  if (selection.length > 1) return <MultiDetail selection={selection} at={at} />;
  const n = selection[0] ?? folder;
  if (!n) return null;
  return <SingleDetail n={n} isFolderItself={selection.length === 0} hidden={selection.length === 0 ? hidden : 0} at={at} onOpen={onOpen} onQuickLook={onQuickLook} onToggleExclude={onToggleExclude} onToggleKeep={onToggleKeep} onShare={onShare} onToggleOffline={onToggleOffline} onReject={onReject} rejectArmed={rejectArmed} onArmReject={onArmReject} onDuplicate={onDuplicate} />;
}

function Row({ label, children, mono }: { label: string; children: React.ReactNode; mono?: boolean }) {
  return (
    <div className="grid grid-cols-[5rem_minmax(0,1fr)] gap-2 text-xs">
      <dt className="text-ink-muted">{label}</dt>
      <dd className={`min-w-0 break-words text-ink ${mono ? "font-mono" : ""}`}>{children}</dd>
    </div>
  );
}

// A video in the pane: its poster frame with a play mark, and one click
// turns that same spot into the player. The poster is the server's
// thumbnail where it has one (a snapshot has no thumbnail cache, and a box
// without ffmpeg makes none), the video glyph where it does not — either
// way the click plays. The player is dropped when the selection moves on,
// so a film does not keep sounding under the next file.
function VideoPoster({ n, at, noThumb, onNoThumb }: { n: FileNode; at?: number; noThumb: boolean; onNoThumb: () => void }) {
  const [playing, setPlaying] = useState(false);
  useEffect(() => setPlaying(false), [n.path, at]);
  const poster = at || noThumb ? undefined : thumbUrl(n.path, 320);
  if (playing) {
    return (
      <video
        key={`${at ?? 0}:${n.path}`}
        src={rawUrl(n.path, false, at)}
        poster={poster}
        controls
        autoPlay
        playsInline
        className="max-h-40 w-full rounded-md bg-black object-contain"
      />
    );
  }
  return (
    <button
      type="button"
      onClick={() => setPlaying(true)}
      title="Play"
      aria-label={`Play ${displayName(n.name)}`}
      className="group relative flex max-w-full items-center justify-center rounded-md focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
    >
      {poster ? (
        <img src={poster} alt="" onError={onNoThumb} className="max-h-40 max-w-full rounded-md object-contain" />
      ) : (
        <FileIcon category="video" className="h-16 w-16" />
      )}
      <span className="pointer-events-none absolute inset-0 flex items-center justify-center">
        <PlayBadge className="h-10 w-10" />
      </span>
    </button>
  );
}

function SingleDetail({ n, isFolderItself, hidden, at, onOpen, onQuickLook, onToggleExclude, onToggleKeep, onShare, onToggleOffline, onReject, rejectArmed, onArmReject, onDuplicate }: {
  n: FileNode; isFolderItself: boolean; hidden: number; at?: number; onOpen: (n: FileNode) => void; onQuickLook: (n: FileNode) => void;
  onToggleExclude?: (n: FileNode, excluded: boolean) => void;
  onToggleKeep?: (n: FileNode, keep: boolean) => void;
  onShare?: (n: FileNode) => void;
  onToggleOffline?: (n: FileNode, keep: boolean) => void;
  onReject?: (n: FileNode) => void;
  rejectArmed?: string | null;
  onArmReject?: (path: string | null) => void;
  onDuplicate?: () => void;
}) {
  const detail = useFileDetail(at ? null : n.path);
  const d = detail.data ?? n;
  const cat = categoryOf(d);
  const isDir = d.kind === "dir";
  const shown = Math.max(0, (d.entries ?? 0) - hidden);
  const visual = (cat === "image" || cat === "video") && d.kind === "file";
  const [noThumb, setNoThumb] = useState(false);
  useEffect(() => setNoThumb(false), [d.path]);
  return (
    <div className="space-y-4">
      <div className="flex flex-col items-center gap-3 pt-2 text-center">
        {cat === "video" && visual ? (
          <VideoPoster n={d} at={at} noThumb={noThumb} onNoThumb={() => setNoThumb(true)} />
        ) : visual && !noThumb ? (
          <img
            src={at ? rawUrl(d.path, false, at) : thumbUrl(d.path, 320)}
            alt=""
            onError={() => setNoThumb(true)}
            className="max-h-40 max-w-full rounded-md object-contain"
          />
        ) : (
          <FileIcon category={cat} className="h-16 w-16" />
        )}
        <div className="min-w-0 max-w-full">
          <p className="break-words text-sm font-semibold text-ink">{d.path === "" ? "Drive" : <FileName name={d.name} />}</p>
          <p className="text-xs text-ink-muted">
            {kindLabel(d)}
            {isDir ? ` · ${shown} item${shown === 1 ? "" : "s"}` : ` · ${formatBytes(d.size)}`}
          </p>
        </div>
      </div>

      {!isFolderItself && (
        <div className="flex flex-wrap justify-center gap-2">
          {isDir ? (
            <button onClick={() => onOpen(d)} className="rounded-md bg-accent px-3 py-1.5 text-xs font-semibold text-accent-on hover:bg-accent-hover">Open</button>
          ) : (
            <button onClick={() => onQuickLook(d)} className="rounded-md bg-accent px-3 py-1.5 text-xs font-semibold text-accent-on hover:bg-accent-hover">Quick Look</button>
          )}
          {(!isDir || !at) && (
            <a href={isDir ? zipUrl([d.path]) : rawUrl(d.path, true, at)} className="rounded-md border border-edge px-3 py-1.5 text-xs font-medium text-ink hover:bg-fog-800">
              Download{isDir ? " zip" : ""}
            </a>
          )}
        </div>
      )}

      {d.role === "apps" && (
        <p className="border-t border-hair pt-3 text-xs text-ink-muted">
          Fog keeps a folder here for every app that stores its own data in Drive — Paperless, Immich,
          Jellyfin — made when the app is installed. What is inside belongs to that app and travels in
          its backups; the badge on each folder says which app that is. Your own libraries live in
          Content instead.
        </p>
      )}
      {d.role === "content" && (
        <p className="border-t border-hair pt-3 text-xs text-ink-muted">
          Your own libraries — Photos, Movies, TV, Music, Books, Documents. Drop files into one and any
          app pointed at it sees them, so several apps can read the same folder. These are covered by
          Drive's nightly snapshot rather than by any one app's backups.
        </p>
      )}
      {d.role !== "apps" && ((d.mounts && d.mounts.length > 0) || (d.mountedBy && d.mountedBy.length > 0)) && (
        <div className="space-y-1 border-t border-hair pt-3 text-xs text-accent">
          {(d.mounts ?? []).map((m) => (
            <p key={m.app + m.mount}>
              <Link to={paths.app(m.app)} className="font-medium underline hover:text-accent">{m.app}</Link>
              {" "}sees this folder as <span className="font-mono">{m.mount}</span>{m.readOnly ? " (read-only)" : ""}.
            </p>
          ))}
          {(d.mountedBy ?? []).filter((a) => !(d.mounts ?? []).some((m) => m.app === a)).map((a) => (
            <p key={a}>
              <Link to={paths.app(a)} className="font-medium underline hover:text-accent">{a}</Link> mounts a folder inside.
            </p>
          ))}
          <p className="text-ink-muted">It stays where it is while an app depends on it.</p>
        </div>
      )}

      {d.peer && (
        <div className="space-y-1.5 border-t border-hair pt-3 text-xs">
          <p className={d.peer.rejected ? "text-warn" : d.peer.stub ? "text-ink-muted" : "text-accent"}>{peerSentence(d)}</p>
          {!at && d.peer.kind === "contribution" && (onReject || onDuplicate) && (
            rejectArmed === d.path && onReject ? (
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="text-warn">
                  {d.peer.stub ? "Remove the listing?" : `Reject it? It goes away here for good and ${d.peer.peer} is told; their own copy is untouched.`}
                </span>
                <button type="button" onClick={() => onReject(d)} className="rounded-md border border-warn/45 px-2 py-1 font-medium text-warn hover:bg-warn/10">
                  {d.peer.stub ? "Remove" : "Reject"}
                </button>
                <button type="button" onClick={() => onArmReject?.(null)} className="text-ink-muted hover:text-ink">Keep</button>
              </div>
            ) : (
              <div className="flex flex-wrap gap-1.5">
                {onDuplicate && !d.peer.stub && (
                  <button type="button" onClick={onDuplicate} className="rounded-md border border-edge px-2 py-1 font-medium text-ink hover:bg-fog-800">
                    Make a copy
                  </button>
                )}
                {onReject && (
                  <button type="button" onClick={() => onArmReject?.(d.path)} className="rounded-md border border-warn/30 px-2 py-1 font-medium text-warn hover:bg-warn/10">
                    {d.peer.stub ? "Remove listing…" : "Reject…"}
                  </button>
                )}
              </div>
            )
          )}
        </div>
      )}

      {!at && d.peer?.kind === "replica" && d.peer.detail === "previews" && (d.kind === "file" || d.kind === "dir") && (
        <div className="space-y-1.5 text-xs">
          {d.kind === "file" && (
            <p className={d.dehydrated ? "text-accent" : "text-ink-muted"}>
              {d.dehydrated
                ? `Online-only: fetched from ${d.peer.peer} when opened.`
                : d.peer.pinned
                  ? "Kept offline on this box."
                  : `On this box since it was last opened; ${d.peer.peer}'s next change lists it again.`}
            </p>
          )}
          {d.kind === "dir" && <p className="text-ink-muted">Previews: files here are fetched when opened.</p>}
          {onToggleOffline && !d.peer.revoked && (
            <div className="flex flex-wrap gap-1.5">
              {!(d.kind === "file" && d.peer.pinned) && (
                <button type="button" onClick={() => onToggleOffline(d, true)} className="rounded-md border border-edge px-2 py-1 font-medium text-ink hover:bg-fog-800">
                  {d.kind === "dir" ? "Keep all offline" : "Keep offline"}
                </button>
              )}
              {!(d.kind === "file" && d.dehydrated) && (
                <button type="button" onClick={() => onToggleOffline(d, false)} className="rounded-md border border-edge px-2 py-1 font-medium text-ink hover:bg-fog-800">
                  Free up space
                </button>
              )}
            </div>
          )}
        </div>
      )}

      {!at && d.path !== "" && !d.peer && (onToggleKeep || onShare) && (
        <div className="space-y-1.5 border-t border-hair pt-3 text-xs">
          {d.kept ? (
            <p className="text-warn">
              Kept on this box{d.keptBy ? <> — with <span className="font-mono">{d.keptBy}</span></> : ""}. Never leaves through any share.
            </p>
          ) : d.sharedWith && d.sharedWith.length > 0 ? (
            <p className="text-accent">
              Shared with {d.sharedWith.join(", ")}.{" "}
              <Link to={paths.federation} className="underline">Connect</Link>
            </p>
          ) : (
            <p className="text-ink-muted">Not shared with any peer.</p>
          )}
          <div className="flex flex-wrap gap-1.5">
            {onShare && d.kind === "dir" && !d.kept && (
              <button type="button" onClick={() => onShare(d)} className="rounded-md border border-edge px-2 py-1 font-medium text-ink hover:bg-fog-800">
                Share with a peer…
              </button>
            )}
            {onToggleKeep && !d.keptBy && (
              <button type="button" onClick={() => onToggleKeep(d, !d.kept)} className="rounded-md border border-edge px-2 py-1 font-medium text-ink hover:bg-fog-800">
                {d.kept ? "Allow sharing again" : "Keep on this box"}
              </button>
            )}
          </div>
        </div>
      )}

      {!at && d.path !== "" && !isFolderItself && <Shares key={d.path} path={d.path} />}

      {!at && d.peer?.kind === "replica" && (
        <p className="border-t border-hair pt-3 text-xs text-ink-muted">
          {d.peer.backup
            ? "Backed up with your own files — “back up my copy” is on for this share on the Connect page."
            : `Not backed up: ${d.peer.peer} holds the original. Turn on “back up my copy” for this share on the Connect page to include it.`}
        </p>
      )}

      {!at && onToggleExclude && d.path !== "" && d.peer?.kind !== "replica" && (
        <div className="space-y-1 border-t border-hair pt-3 text-xs">
          {d.excluded ? (
            <p className="text-warn">
              Not backed up{d.excludedBy ? <> — excluded with <span className="font-mono">{d.excludedBy}</span></> : ""}.
            </p>
          ) : (
            <p className="text-ink-muted">Included in the nightly files backup.</p>
          )}
          {!d.excludedBy && (
            <button
              type="button"
              onClick={() => onToggleExclude(d, !d.excluded)}
              className="rounded-md border border-edge px-2 py-1 font-medium text-ink hover:bg-fog-800"
            >
              {d.excluded ? "Include in backups" : "Exclude from backups"}
            </button>
          )}
        </div>
      )}

      {detail.isError && !detail.data && <LoadError label="Could not read the details." error={detail.error} onRetry={() => detail.refetch()} />}
      <dl className="space-y-1.5 border-t border-hair pt-3">
        {isDir ? (
          <Row label="Size">
            {formatBytes(d.size)}
            {d.walkedAt ? (
              <span className="text-ink-muted"> as of {new Date(d.walkedAt).toLocaleString()}</span>
            ) : (
              <span className="text-ink-muted"> (not yet fully counted)</span>
            )}
          </Row>
        ) : (
          <Row label="Size">{d.size.toLocaleString()} bytes</Row>
        )}
        <Row label="Modified">{new Date(d.modified).toLocaleString()}</Row>
        {d.mime && <Row label="Type" mono>{d.mime}</Row>}
        {detail.data?.width && detail.data?.height ? (
          <Row label="Pixels">{detail.data.width.toLocaleString()} × {detail.data.height.toLocaleString()}</Row>
        ) : null}
        {d.mode && <Row label="Mode" mono>{d.mode}</Row>}
        <Row label="Path" mono>{d.path || "/"}</Row>
        {d.kind === "file" && (
          <Row label="SHA-256" mono>
            {d.sha256 ? (
              <span className="inline-flex max-w-full items-center gap-1">
                <span className="truncate" title={d.sha256}>{d.sha256.slice(0, 16)}…</span>
                <CopyButton text={d.sha256} />
              </span>
            ) : detail.isPending && !at ? (
              <Loading label="" />
            ) : at ? (
              <span className="text-ink-muted">in the snapshot manifest</span>
            ) : (
              <span className="text-ink-muted">not yet computed</span>
            )}
          </Row>
        )}
      </dl>

      {detail.data?.duplicates && detail.data.duplicates.length > 0 && (
        <div className="space-y-1 border-t border-hair pt-3">
          <p className="text-xs font-medium text-warn">Identical to</p>
          <ul className="space-y-0.5">
            {detail.data.duplicates.map((p) => {
              const dir = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
              return (
                <li key={p} className="truncate font-mono text-[11px] text-ink-muted">
                  <Link to={paths.files(dir)} className="hover:text-ink hover:underline" title={displayName(p)}><FileName name={p} /></Link>
                </li>
              );
            })}
          </ul>
        </div>
      )}
    </div>
  );
}

function MultiDetail({ selection, at }: { selection: FileNode[]; at?: number }) {
  const files = selection.filter((n) => n.kind === "file");
  const dirs = selection.filter((n) => n.kind === "dir");
  const bytes = selection.reduce((s, n) => s + n.size, 0);
  return (
    <div className="space-y-4">
      <div className="flex flex-col items-center gap-3 pt-2 text-center">
        <div className="flex -space-x-6">
          {selection.slice(0, 3).map((n) => (
            <FileIcon key={n.path} category={categoryOf(n)} className="h-14 w-14 drop-shadow" />
          ))}
        </div>
        <div>
          <p className="text-sm font-semibold text-ink">{selection.length} items</p>
          <p className="text-xs text-ink-muted">
            {dirs.length > 0 && `${dirs.length} folder${dirs.length === 1 ? "" : "s"}`}
            {dirs.length > 0 && files.length > 0 && ", "}
            {files.length > 0 && `${files.length} file${files.length === 1 ? "" : "s"}`}
            {" · "}
            {formatBytes(bytes)}
          </p>
        </div>
      </div>
      {!at && (
        <div className="flex justify-center">
          <a href={zipUrl(selection.map((n) => n.path))} className="rounded-md border border-edge px-3 py-1.5 text-xs font-medium text-ink hover:bg-fog-800">
            Download zip
          </a>
        </div>
      )}
    </div>
  );
}

// Share links for one entry: the ones that exist (with how often they
// were opened), and a way to make another. A new link's address is shown
// once, right here; a revoked one stops working at once.
// Keyed by path where it is rendered, so another file starts it fresh.
function Shares({ path }: { path: string }) {
  const act = useFilesActions();
  const shares = useQuery({ queryKey: filesSharesKey(path), queryFn: () => get<{ shares: FileShare[] }>(`/api/files/shares?path=${encodeURIComponent(path)}`) });
  const [open, setOpen] = useState(false);
  const [days, setDays] = useState(7);
  const [password, setPassword] = useState("");
  const [made, setMade] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const create = async () => {
    setBusy(true);
    try {
      const res = await act.createShare(path, days, password);
      setMade(res.url);
      setPassword("");
      setOpen(false);
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : "Could not create the link");
    } finally {
      setBusy(false);
    }
  };

  const list = shares.data?.shares ?? [];
  return (
    <div className="space-y-2 border-t border-hair pt-3 text-xs">
      <div className="flex items-center justify-between">
        <p className="font-medium text-ink-muted">Share links</p>
        <button type="button" onClick={() => setOpen((o) => !o)} className="rounded-md border border-edge px-2 py-0.5 text-ink hover:bg-fog-800">
          {open ? "Cancel" : "New link"}
        </button>
      </div>
      {made && (
        <div className="space-y-1 rounded-md border border-accent/30 bg-accent/10 p-2">
          <p className="text-accent">Link created — anyone with it can open {list.length > 0 ? "this" : "it"}.</p>
          <p className="flex items-center gap-1 font-mono text-[11px] text-ink"><span className="truncate">{made}</span><CopyButton text={made} /></p>
        </div>
      )}
      {open && (
        <div className="space-y-2 rounded-md border border-veil-edge bg-veil p-2">
          <label className="flex items-center gap-2 text-ink-muted">
            Expires
            <select value={days} onChange={(e) => setDays(Number(e.target.value))} className="rounded border border-edge bg-veil px-1.5 py-0.5 text-ink">
              <option value={1}>in a day</option>
              <option value={7}>in a week</option>
              <option value={30}>in a month</option>
              <option value={0}>never</option>
            </select>
          </label>
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="Password (optional)"
            className="w-full rounded border border-edge bg-veil px-2 py-1 text-ink placeholder-ink-muted outline-none focus:border-accent"
          />
          <button type="button" onClick={create} disabled={busy} className="rounded-md bg-accent px-2.5 py-1 font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
            {busy ? "Creating…" : "Create link"}
          </button>
          {error && <p className="text-warn">{error}</p>}
        </div>
      )}
      {shares.isPending ? (
        <Loading label="" />
      ) : list.length === 0 ? (
        !open && !made && <p className="text-ink-muted">Not shared.</p>
      ) : (
        <ul className="space-y-1">
          {list.map((sh) => (
            <li key={sh.id} className="flex items-center gap-2 text-ink-muted">
              <span className="min-w-0 flex-1 truncate">
                {sh.hasPassword ? "🔒 " : ""}
                {sh.expiresAt ? `expires ${new Date(sh.expiresAt).toLocaleDateString()}` : "never expires"} · opened {sh.hits}×
                {sh.lastUsedAt ? `, last ${timeAgo(sh.lastUsedAt)}` : ""}
              </span>
              <button
                type="button"
                onClick={() => {
                  setMade(null);
                  act.revokeShare(sh.id, path);
                }}
                className="text-ink-muted hover:text-warn"
              >
                Revoke
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// One sentence on whose an entry is and what that means here.
function peerSentence(d: FileNode): string {
  const m = d.peer!;
  if (m.kind === "contribution") {
    return m.stub
      ? `Added by ${m.peer}, who has since unpaired. The bytes left with them; only this listing remains.`
      : `Added by ${m.peer}. Theirs to edit — make a copy for one of your own, or reject it to send it back.`;
  }
  if (m.mine) {
    return m.rejected
      ? `${m.peer} did not keep this. It is still yours here; edit it and it goes up again, or remove it.`
      : `Yours, in ${m.peer}'s ${d.kind === "dir" ? "folder" : "drop box"}. Edit or remove it here and ${m.peer}'s copy follows.`;
  }
  if (m.author) {
    return m.stub
      ? `Added by ${m.author} to ${m.peer}'s folder. Their bytes reach you only over your own peering with them.`
      : `Added by ${m.author} to ${m.peer}'s folder, fetched from ${m.author} directly.`;
  }
  const base = `A copy of ${m.peer}'s share${m.revoked ? " — no longer shared by them" : ", refreshed on every sync"}.`;
  if (m.mode === "contribute" && !m.revoked) return `${base} A drop box: add files of your own here and they go to ${m.peer}; their entries stay theirs.`;
  return `${base} Read-only here; nothing changed here reaches ${m.peer}.`;
}
