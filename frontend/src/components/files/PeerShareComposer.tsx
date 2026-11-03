// The composer for sharing a folder with a peer: the folder, the peer, and
// the lens — scope, detail, from-when, mode, apps — with the impact
// sentence written live under it, so what is being promised is read
// before it is saved. Editing an existing share opens the same form.
// Saving grants files.share to the peer if it was not already.

import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { get, post, put, type FileFedShare, type FileNode, type FilesListing } from "../../api";
import { Loading, errorText } from "../Async";
import Button from "../Button";
import Modal from "../Modal";
import FileIcon from "../FileIcon";
import { peerLabel } from "../../lib/format";
import { DETAIL_LABELS, draftFromShare, emptyDraft, escalations, impactSentence, KIND_LABELS, type LensDraft, peerSharesRootKey, SHARE_DETAILS, SHARE_KINDS, type ShareDetail, type ShareKind } from "../../lib/filesFed";
import { filesKey } from "../../lib/useFiles";
import { federationQuery } from "../../queries";
import { control } from "../Field";

const input = `${control} w-full`;
const sel = control;

export default function PeerShareComposer({
  share,
  peerId,
  path,
  onClose,
}: {
  share?: FileFedShare; // editing
  peerId?: number; // preselected peer (from the Connect page)
  path?: string; // preselected folder (from the Drive page)
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const fedInfo = useQuery(federationQuery);
  const peers = (fedInfo.data?.peers ?? []).filter((p) => !p.unpairedAt);
  const [d, setD] = useState<LensDraft>(() => (share ? draftFromShare(share) : emptyDraft(peerId ?? 0, path ?? "")));
  const [picking, setPicking] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!share && d.peerId === 0 && peers.length > 0) setD((x) => ({ ...x, peerId: peers[0].id }));
  }, [peers, share, d.peerId]);
  const peer = peers.find((p) => p.id === d.peerId);
  const peerName = peer ? peerLabel(peer) : "the peer";
  const granted = !!peer?.capabilities.some((c) => c.capability === "files.share" && c.direction === "grant");

  const save = useMutation({
    mutationFn: async () => {
      if (!granted && peer) {
        await put(`/api/federation/peers/${peer.id}`, { capabilities: [{ capability: "files.share", direction: "grant", enabled: true, config: {} }] });
      }
      const body = { ...d, since: d.since ? new Date(d.since).toISOString() : "" };
      return share ? put<FileFedShare>(`/api/files/peer-shares/${share.id}`, body) : post<FileFedShare>("/api/files/peer-shares", body);
    },
    onSuccess: async () => {
      await Promise.all([
        qc.invalidateQueries({ queryKey: federationQuery.queryKey }),
        qc.invalidateQueries({ queryKey: peerSharesRootKey }),
        qc.invalidateQueries({ queryKey: filesKey }),
      ]);
      onClose();
    },
    onError: (e) => setError(errorText(e, "Could not save the share")),
  });

  const set = <K extends keyof LensDraft>(k: K, v: LensDraft[K]) => setD((x) => ({ ...x, [k]: v }));
  const toggleKind = (k: ShareKind) => {
    const f = `kind:${k}`;
    set("filters", d.filters.includes(f) ? d.filters.filter((x) => x !== f) : [...d.filters, f]);
  };
  const globs = d.filters.filter((f) => f.startsWith("glob:")).map((f) => f.slice(5)).join(", ");
  const setGlobs = (v: string) => {
    const kinds = d.filters.filter((f) => f.startsWith("kind:"));
    const gl = v.split(",").map((x) => x.trim()).filter(Boolean).map((g) => `glob:${g}`);
    set("filters", [...kinds, ...gl]);
  };
  const sentence = useMemo(() => impactSentence(d, peerName), [d, peerName]);
  const canSave = !!peer && !!d.path && !save.isPending && (d.scope !== "picked" || d.picks.length > 0) && (d.scope !== "filtered" || d.filters.length > 0);

  return (
    <Modal title={share ? `Sharing ${share.title} with ${peerName}` : "Share a folder with a peer"} onClose={onClose} width="max-w-2xl">
      <div className="space-y-4 text-sm">
        {fedInfo.isPending ? (
          <Loading label="Loading peers…" />
        ) : peers.length === 0 ? (
          <p className="text-ink-muted">No peers yet — pair one on the Connect page first.</p>
        ) : (
          <>
            <div className="grid grid-cols-2 gap-3">
              <label className="block">
                <span className="mb-1 block text-xs font-medium text-ink-muted">Peer</span>
                <select value={d.peerId} disabled={!!share} onChange={(e) => set("peerId", Number(e.target.value))} className={`${sel} w-full`}>
                  {peers.map((p) => (
                    <option key={p.id} value={p.id}>{peerLabel(p)}</option>
                  ))}
                </select>
              </label>
              <label className="block">
                <span className="mb-1 block text-xs font-medium text-ink-muted">Title (what they see)</span>
                <input value={d.title} onChange={(e) => set("title", e.target.value)} className={input} />
              </label>
            </div>
            <div>
              <span className="mb-1 block text-xs font-medium text-ink-muted">Folder</span>
              <div className="flex items-center gap-2">
                <code className="flex-1 truncate rounded-md border border-hair bg-fog-950 px-2.5 py-1.5 font-mono text-xs text-ink">{d.path || "/"}</code>
                {!share && <Button size="sm" onClick={() => setPicking(true)}>Choose…</Button>}
              </div>
              <p className="mt-1 text-xs text-ink-muted">They never see this path — only the title.</p>
            </div>
            {picking && <FolderPicker initial={d.path} onPick={(p) => { set("path", p); if (!d.title || d.title === emptyDraft(0, d.path).title) set("title", emptyDraft(0, p).title); setPicking(false); }} onClose={() => setPicking(false)} />}

            <fieldset className="space-y-2">
              <legend className="text-xs font-medium text-ink-muted">Which entries</legend>
              <div className="flex flex-wrap gap-3 text-ink">
                <label className="flex items-center gap-1.5"><input type="radio" checked={d.scope === "all"} onChange={() => set("scope", "all")} /> the whole folder</label>
                <label className="flex items-center gap-1.5"><input type="radio" checked={d.scope === "filtered"} onChange={() => set("scope", "filtered")} /> only some kinds</label>
                <label className="flex items-center gap-1.5"><input type="radio" checked={d.scope === "picked"} onChange={() => set("scope", "picked")} /> hand-picked entries</label>
              </div>
              {d.scope === "filtered" && (
                <div className="space-y-2 rounded-md border border-hair p-2">
                  <div className="flex flex-wrap gap-3 text-ink">
                    {SHARE_KINDS.map((k) => (
                      <label key={k} className="flex items-center gap-1.5">
                        <input type="checkbox" checked={d.filters.includes(`kind:${k}`)} onChange={() => toggleKind(k)} /> {KIND_LABELS[k]}
                      </label>
                    ))}
                  </div>
                  <input value={globs} onChange={(e) => setGlobs(e.target.value)} placeholder="or patterns, e.g. *.pdf, report-*" className={input} />
                </div>
              )}
              {d.scope === "picked" && <PickList folder={d.path} picks={d.picks} onChange={(p) => set("picks", p)} />}
            </fieldset>

            <div className="grid grid-cols-2 gap-3">
              <label className="block">
                <span className="mb-1 block text-xs font-medium text-ink-muted">How much of each</span>
                <select value={d.detail} onChange={(e) => set("detail", e.target.value as ShareDetail)} className={`${sel} w-full`}>
                  {SHARE_DETAILS.map((x) => (
                    <option key={x} value={x}>{DETAIL_LABELS[x]}</option>
                  ))}
                </select>
              </label>
              <label className="block">
                <span className="mb-1 block text-xs font-medium text-ink-muted">From when</span>
                <input type="date" value={d.since} onChange={(e) => set("since", e.target.value)} className={input} />
                <span className="mt-1 block text-xs text-ink-muted">Empty: the whole history.</span>
              </label>
            </div>
            <div className="flex flex-wrap gap-4 text-ink">
              <label className="flex items-center gap-1.5">
                <input type="checkbox" checked={d.apps === "allowed"} onChange={(e) => set("apps", e.target.checked ? "allowed" : "denied")} />
                May be mounted for apps on {peerName}'s fog
              </label>
              <label className="flex items-center gap-1.5">
                <input type="checkbox" checked={d.mode === "contribute"} onChange={(e) => { set("mode", e.target.checked ? "contribute" : "read"); if (e.target.checked) set("scope", "all"); }} />
                They may add files (a drop box)
              </label>
              {d.mode === "contribute" && (
                <label className="flex items-center gap-1.5">
                  Up to
                  <input
                    type="number"
                    min={0}
                    step={1}
                    value={d.contributeQuotaBytes ? Math.round(d.contributeQuotaBytes / 1e6) : ""}
                    onChange={(e) => set("contributeQuotaBytes", e.target.value ? Math.round(Number(e.target.value) * 1e6) : 0)}
                    className={`${input} w-24`}
                    placeholder="∞"
                  />
                  MB of their files (0 = no limit)
                </label>
              )}
            </div>

            <div className="rounded-lg border border-edge bg-fog-950 p-3">
              <p className="subhead">What this promises</p>
              <p className="mt-1 text-ink">{sentence}</p>
              {escalations(d).map((e) => (
                <p key={e} className="mt-1 text-xs text-warn">⚠ {e}</p>
              ))}
              {!granted && peer && <p className="mt-1 text-xs text-ink-muted">Saving also grants {peerName} files.share on this instance.</p>}
            </div>
            {error && <p className="text-warn">{error}</p>}
            <div className="flex justify-end gap-2">
              <Button variant="ghost" onClick={onClose}>Cancel</Button>
              <Button variant="primary" onClick={() => save.mutate()} disabled={!canSave}>
                {save.isPending ? "Saving…" : share ? "Save lens" : "Share"}
              </Button>
            </div>
          </>
        )}
      </div>
    </Modal>
  );
}

// A picker over the Files tree (folders only), root-relative.
function FolderPicker({ initial, onPick, onClose }: { initial: string; onPick: (p: string) => void; onClose: () => void }) {
  const [path, setPath] = useState(initial);
  const listing = useQuery({
    queryKey: ["files", "picker", path],
    queryFn: () => get<FilesListing>(`/api/files/ls?path=${encodeURIComponent(path)}&limit=500`),
  });
  const dirs = (listing.data?.entries ?? []).filter((e) => e.kind === "dir" && !e.peer);
  const crumbs = path ? path.split("/") : [];
  return (
    <Modal title="Choose a folder" onClose={onClose}>
      <div className="space-y-2 text-sm">
        <div className="flex flex-wrap items-center gap-1 font-mono text-xs">
          <button onClick={() => setPath("")} className="text-ink-muted hover:text-ink">Drive</button>
          {crumbs.map((c, i) => (
            <span key={i} className="flex items-center gap-1">
              <span className="text-ink-faint">/</span>
              <button onClick={() => setPath(crumbs.slice(0, i + 1).join("/"))} className="text-ink-muted hover:text-ink">{c}</button>
            </span>
          ))}
        </div>
        <div className="max-h-64 overflow-y-auto rounded-md border border-hair">
          {listing.isPending ? (
            <Loading label="Reading…" className="p-2" />
          ) : dirs.length === 0 ? (
            <p className="p-2 text-xs text-ink-muted">No folders here.</p>
          ) : (
            dirs.map((e) => (
              <button key={e.path} onDoubleClick={() => setPath(e.path)} onClick={() => setPath(e.path)} className="flex w-full items-center gap-2 px-2 py-1 text-left hover:bg-fog-800">
                <FileIcon category="folder" className="h-4 w-4" />
                <span className="truncate">{e.name}</span>
                {e.sharedWith && e.sharedWith.length > 0 && <span className="ml-auto text-[10px] text-ink-muted">→ {e.sharedWith.join(", ")}</span>}
              </button>
            ))
          )}
        </div>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={!path} onClick={() => onPick(path)}>Use {path ? path.slice(path.lastIndexOf("/") + 1) : "…"}</Button>
        </div>
      </div>
    </Modal>
  );
}

// Hand-picked entries inside the folder: check what crosses.
function PickList({ folder, picks, onChange }: { folder: string; picks: string[]; onChange: (p: string[]) => void }) {
  const listing = useQuery({
    queryKey: ["files", "picker", folder],
    queryFn: () => get<FilesListing>(`/api/files/ls?path=${encodeURIComponent(folder)}&limit=500`),
    enabled: !!folder,
  });
  const entries: FileNode[] = listing.data?.entries ?? [];
  const toggle = (rel: string) => onChange(picks.includes(rel) ? picks.filter((p) => p !== rel) : [...picks, rel]);
  return (
    <div className="max-h-48 space-y-0.5 overflow-y-auto rounded-md border border-hair p-2">
      {listing.isPending ? (
        <Loading label="Reading the folder…" />
      ) : entries.length === 0 ? (
        <p className="text-xs text-ink-muted">The folder is empty.</p>
      ) : (
        entries.map((e) => {
          const rel = e.path.slice(folder.length + 1);
          return (
            <label key={e.path} className="flex items-center gap-2 text-ink">
              <input type="checkbox" checked={picks.includes(rel)} onChange={() => toggle(rel)} disabled={e.kept} />
              <FileIcon category={e.kind === "dir" ? "folder" : "file"} className="h-4 w-4" />
              <span className={`truncate ${e.kept ? "text-ink-muted line-through" : ""}`}>{e.name}</span>
              {e.kept && <span className="text-[10px] text-ink-muted">kept on this box</span>}
            </label>
          );
        })
      )}
      <p className="pt-1 text-xs text-ink-muted">Top-level entries only; a checked folder shares its whole subtree.</p>
    </div>
  );
}

