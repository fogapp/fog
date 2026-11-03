// The Sharing tab keeps the two directions apart — what this fog sends out
// and what comes in — because which way a folder flows is the most common
// confusion on a federation screen. A third group holds what a peer has
// offered and this box has not yet taken up.

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { del, post, type FedPeer, type FileFedDetail, type FileFedShare, type FileFedSubscription } from "../../api";
import { errorText } from "../../components/Async";
import Button from "../../components/Button";
import { formatBytes, peerLabel, plural } from "../../lib/format";
import { paths } from "../../lib/paths";
import { filesKey } from "../../lib/useFiles";
import { Dot, Group, Head, Row, Table, Veil } from "../app/cockpit";
import { DETAIL_LABEL, MODE_LABEL, peerState, peerTone, subState, type Offer } from "./data";
import { federationQuery, subscriptionsQuery } from "../../queries";
import { peerSharesRootKey } from "../../lib/filesFed";

const OUT = "minmax(160px,1.5fr) 1.1fr 130px 120px 140px";
const IN = "minmax(150px,1.2fr) 1.5fr 110px 1fr";

export function SharingTab({ peers, shares, subs, offers, offersPending, onShare, onEditShare, onOpenSubscription }: {
  peers: FedPeer[];
  shares: FileFedShare[];
  subs: FileFedSubscription[];
  offers: Offer[];
  offersPending: boolean;
  onShare: () => void;
  onEditShare: (share: FileFedShare) => void;
  onOpenSubscription: (sub: FileFedSubscription) => void;
}) {
  const qc = useQueryClient();
  const [revoking, setRevoking] = useState<FileFedShare | null>(null);
  const [error, setError] = useState("");
  const refresh = () =>
    Promise.all([
      qc.invalidateQueries({ queryKey: peerSharesRootKey }),
      qc.invalidateQueries({ queryKey: federationQuery.queryKey }),
      qc.invalidateQueries({ queryKey: subscriptionsQuery.queryKey }),
      qc.invalidateQueries({ queryKey: filesKey }),
    ]);
  const revoke = useMutation({
    mutationFn: (id: number) => del(`/api/files/peer-shares/${id}`),
    onSuccess: async () => { setRevoking(null); setError(""); await refresh(); },
    onError: (e) => setError(errorText(e, "Couldn't stop sharing")),
  });
  const subscribe = useMutation({
    mutationFn: (o: { peerId: number; remoteShareId: number; detail: FileFedDetail }) =>
      post<FileFedSubscription>("/api/files/subscriptions", o),
    onSuccess: async () => { setError(""); await refresh(); },
    onError: (e) => setError(errorText(e, "Couldn't subscribe")),
  });
  const peerOf = (id: number) => peers.find((p) => p.id === id);
  const live = subs.filter((s) => s.status !== "revoked" || true);

  return (
    <div>
      <Group title="What you send out" first right={<Button size="sm" onClick={onShare} disabled={peers.length === 0}>Share a folder…</Button>}>
        {shares.length === 0 ? (
          <p className="text-hint text-ink-muted">
            {peers.length === 0 ? "Pair a peer first; then any Drive folder can be shared with them." : "No folders leave this fog yet."}
          </p>
        ) : (
          <Table>
            <Head cols={OUT}><span>Folder</span><span>Peer</span><span>Mode</span><span>Detail</span><span /></Head>
            {shares.map((sh) => {
              const p = peerOf(sh.peerId);
              return (
                <Row key={sh.id} cols={OUT} className="group">
                  <span className="truncate font-mono text-ink" title={sh.path}>{sh.title}</span>
                  <span className="truncate text-ink-muted">{p ? peerLabel(p) : sh.peer}</span>
                  <span className={sh.mode === "contribute" ? "text-warn" : "text-ink-muted"}>{MODE_LABEL[sh.mode]}</span>
                  <span className="text-ink-muted">{DETAIL_LABEL[sh.detail]}</span>
                  <span className="flex justify-end gap-1 opacity-0 transition-opacity duration-[120ms] focus-within:opacity-100 group-hover:opacity-100">
                    <Button size="sm" variant="ghost" onClick={() => onEditShare(sh)}>Edit</Button>
                    <Button size="sm" variant="ghost" onClick={() => setRevoking(sh)}>Revoke</Button>
                  </span>
                </Row>
              );
            })}
          </Table>
        )}
        {revoking && (
          <Veil className="mt-3 flex flex-wrap items-center justify-between gap-3 px-[18px] py-3">
            <span className="text-cell text-warn">
              Stop sharing <span className="font-mono">{revoking.title}</span> with {revoking.peer}? Their copy stays until they remove it.
            </span>
            <span className="inline-flex gap-2">
              <Button size="sm" variant="ghost" onClick={() => setRevoking(null)}>Keep</Button>
              <Button size="sm" variant="danger" onClick={() => revoke.mutate(revoking.id)} disabled={revoke.isPending}>
                {revoke.isPending ? "Stopping…" : "Stop sharing"}
              </Button>
            </span>
          </Veil>
        )}
      </Group>

      <Group title="What comes in" right={<span className="text-hint text-ink-muted">{plural(live.length, "subscription")}</span>}>
        {live.length === 0 ? (
          <p className="text-hint text-ink-muted">Nothing from a peer is held here yet.</p>
        ) : (
          <Table>
            <Head cols={IN}><span>Peer</span><span>Folder</span><span>Size</span><span className="text-right">State</span></Head>
            {live.map((s) => {
              const p = peerOf(s.peerId);
              const st = subState(s);
              const trouble = st.tone === "warn" || (p ? peerState(p) !== "ok" : false);
              return (
                <Row key={s.id} cols={IN} tone={trouble ? "warn" : undefined} onClick={() => onOpenSubscription(s)}>
                  <span className="inline-flex min-w-0 items-center gap-[9px] text-ink">
                    {p && <Dot tone={peerTone(p)} />}
                    <span className="truncate">{p ? peerLabel(p) : s.peer}</span>
                  </span>
                  <span className="truncate font-mono text-ink-muted" title={`Drive / ${s.folder}`}>{s.title}</span>
                  <span className="font-mono text-ink-muted">{s.bytes > 0 ? formatBytes(s.bytes) : "—"}</span>
                  <span className={`text-right text-hint ${st.tone === "warn" ? "text-warn" : st.tone === "ok" ? "text-accent" : "text-ink-muted"}`}>
                    {p && peerState(p) !== "ok" && st.tone !== "warn" ? "unreachable" : st.text}
                  </span>
                </Row>
              );
            })}
          </Table>
        )}
      </Group>

      <Group title="Waiting for you" right={<span className="text-hint text-ink-muted">{offers.length === 0 ? (offersPending ? "reading catalogs…" : "no offers") : plural(offers.length, "offer")}</span>}>
        {offers.length === 0 ? (
          <p className="text-hint text-ink-muted">
            When a peer shares a folder with this fog, it appears here until you take it up.
          </p>
        ) : (
          <div className="space-y-2">
            {offers.map((o) => (
              <Veil key={`${o.peer.id}-${o.id}`} className="flex flex-wrap items-center justify-between gap-4 px-[18px] py-4">
                <div className="min-w-0">
                  <div className="text-cell text-ink">
                    <span className="text-ink-muted">{peerLabel(o.peer)}</span> offers <span className="font-mono">{o.title}</span>
                  </div>
                  <div className="mt-[5px] text-hint text-ink-muted">
                    {formatBytes(o.bytes)} · {plural(o.files, "file")} · {MODE_LABEL[o.mode].toLowerCase()} · {DETAIL_LABEL[o.detail].toLowerCase()} · lands in{" "}
                    <Link to={paths.files()} className="font-mono hover:text-ink">Drive / Peers / {peerLabel(o.peer)}</Link>
                  </div>
                  <div className="mt-1 text-hint text-ink-muted">{o.impact}</div>
                </div>
                <span className="inline-flex flex-wrap gap-2">
                  {o.detail !== "listing" && (
                    <Button size="sm" variant={o.detail === "full" ? "ghost" : "primary"}
                      onClick={() => subscribe.mutate({ peerId: o.peer.id, remoteShareId: o.id, detail: "previews" })} disabled={subscribe.isPending}>
                      Previews only
                    </Button>
                  )}
                  {o.detail === "full" && (
                    <Button size="sm" variant="primary"
                      onClick={() => subscribe.mutate({ peerId: o.peer.id, remoteShareId: o.id, detail: "full" })} disabled={subscribe.isPending}>
                      Keep a copy
                    </Button>
                  )}
                  {o.detail === "listing" && (
                    <Button size="sm" variant="primary"
                      onClick={() => subscribe.mutate({ peerId: o.peer.id, remoteShareId: o.id, detail: "listing" })} disabled={subscribe.isPending}>
                      Take the listing
                    </Button>
                  )}
                </span>
              </Veil>
            ))}
          </div>
        )}
      </Group>
      {error && <p className="mt-4 text-hint text-warn">{error}</p>}
    </div>
  );
}
