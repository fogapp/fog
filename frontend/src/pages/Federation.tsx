// Connect: which other fogs this one trusts, what they hold for each
// other, and whether any of it is broken right now. One picture, then one
// table; the two directions of sharing kept apart; trouble visible without
// opening anything — amber in the subheader pill, amber in its row, dashed
// on the map. Liveness here is real, never inferred: every figure is
// something fogd measured on its own dials.
//
// The window header already names the page, so it opens the way the app
// page does: one compact subheader — the tab strip on the left; the
// trouble pills, this fog's identity and the two actions on the right —
// then the body.
//
// The shell holds the shared reads, the selection (in the URL, so the
// sidebar's peer list and a shared link land on the same peer), the
// modals and the keys; each tab lives in ./federation.

import { useEffect, useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { FileFedShare, FileFedSubscription } from "../api";
import { LoadError, Loading } from "../components/Async";
import Button from "../components/Button";
import CopyButton from "../components/CopyButton";
import Subheader, { Note } from "../components/Subheader";
import { useTabParam } from "../components/Tabs";
import PeerShareComposer from "../components/files/PeerShareComposer";
import { plural } from "../lib/format";
import { FED_TABS, FED_TAB_LABELS, paths } from "../lib/paths";
import { GUTTER, Key, Pill } from "./app/cockpit";
import { ActivityTab, ledgerTone } from "./federation/Activity";
import { ConfigTab } from "./federation/Config";
import { InvitesTab } from "./federation/Invites";
import { PeerDetail, PeersTab, type PeerFacts } from "./federation/Peers";
import { SharingTab } from "./federation/Sharing";
import { AboutModal, PeerSettingsModal, UnpairedModal } from "./federation/PeerSettings";
import { SubscriptionModal } from "./federation/Subscription";
import { InviteModal, JoinModal, RemoveModal } from "./federation/modals";
import { peerState, useAllSubscriptions, useLedger, useOffers } from "./federation/data";
import { federationQuery, peerSharesQuery } from "../queries";
import { useHotkeys } from "../lib/useHotkeys";

type FedModal =
  | null
  | { kind: "invite" }
  | { kind: "join" }
  | { kind: "about" }
  | { kind: "share"; peerId?: number; share?: FileFedShare }
  | { kind: "remove"; peerId: number }
  | { kind: "settings"; peerId: number }
  | { kind: "unpaired"; peerId: number }
  | { kind: "subscription"; sub: FileFedSubscription };

const DAY = 24 * 3600 * 1000;

export default function Federation() {
  const [tab] = useTabParam(FED_TABS, { keepParams: true });
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const selected = Number(params.get("peer")) || null;
  const [modal, setModal] = useState<FedModal>(null);

  const info = useQuery({ ...federationQuery, refetchInterval: 10_000 });
  const shares = useQuery({ ...peerSharesQuery, refetchInterval: 30_000 });
  const subs = useAllSubscriptions();
  const ledger = useLedger();

  const peers = useMemo(() => info.data?.peers ?? [], [info.data]);
  const shareList = useMemo(() => shares.data?.shares ?? [], [shares.data]);
  const subList = useMemo(() => subs.data?.subscriptions ?? [], [subs.data]);
  const facts: PeerFacts[] = useMemo(
    () => peers.map((peer) => ({
      peer,
      holds: shareList.filter((s) => s.peerId === peer.id),
      shares: subList.filter((s) => s.peerId === peer.id),
    })),
    [peers, shareList, subList],
  );
  const { offers, pending: offersPending } = useOffers(peers, subList);

  const select = (id: number | null) => {
    const next = new URLSearchParams(params);
    if (id === null) next.delete("peer");
    else next.set("peer", String(id));
    setParams(next, { replace: true });
  };
  const current = facts.find((f) => f.peer.id === selected) ?? null;
  // This fog's own side — identity, address, reachability — is the Config
  // tab; the map's centre node, the subheader identity and the "can't dial"
  // line all lead there.
  const openConfig = () => navigate(paths.federationView("config", selected ? { peer: String(selected) } : undefined));
  // A selection that no longer names a peer — removed, or a stale link —
  // clears itself once the list is known.
  useEffect(() => {
    if (selected !== null && info.data && !info.data.peers.some((p) => p.id === selected)) select(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected, info.data]);

  // I invites, Esc clears the selection; none while a dialog is open — the
  // dialog owns Esc.
  useHotkeys(
    {
      i: () => setModal({ kind: "invite" }),
      Escape: () => select(null),
    },
    { disabled: !!modal },
  );

  if (!info.data) {
    return (
      <div className={`${GUTTER} py-6`}>
        {info.isError ? (
          <LoadError label="Couldn't load Connect." error={info.error} onRetry={() => info.refetch()} />
        ) : (
          <Loading label="Loading Connect…" />
        )}
      </div>
    );
  }

  const data = info.data;
  const identity = data.identity;
  const unreachable = peers.filter((p) => peerState(p) === "unreachable" || peerState(p) === "off").length;
  const unpaired = peers.filter((p) => peerState(p) === "unpaired").length;
  const entries = ledger.data?.entries ?? [];
  const recentWarns = entries.filter((e) => ledgerTone(e) === "warn" && Date.now() - new Date(e.at).getTime() < DAY).length;
  const showDetail = tab === "peers" && current !== null;
  const stale = info.isError;

  const tabItems = FED_TABS.map((t) => ({
    id: t,
    label: FED_TAB_LABELS[t],
    to: paths.federationView(t, selected ? { peer: String(selected) } : undefined),
    ...(t === "invites" && data.invites.length > 0 ? { count: data.invites.length } : {}),
    ...(t === "sharing" && offers.length > 0 ? { count: offers.length, hot: true } : {}),
    ...(t === "activity" && recentWarns > 0 ? { count: recentWarns, hot: true } : {}),
  }));

  return (
    <div className="flex min-h-[calc(100vh-3.5rem)] flex-col">
      <Subheader
        label="Connect views"
        items={tabItems}
        active={tab}
        right={
          <>
            {unreachable > 0 && <Pill tone="warn">{plural(unreachable, "peer")} unreachable</Pill>}
            {unpaired > 0 && <Pill tone="warn">{unpaired === 1 ? "1 peer unpaired you" : `${unpaired} peers unpaired you`}</Pill>}
            <span className="inline-flex items-center gap-2">
              <button
                type="button"
                onClick={openConfig}
                title="This fog — its identity, address and reachability"
                className="font-mono text-hint text-ink-muted transition-colors hover:text-ink"
              >
                {identity.name}{identity.fingerprint && <> · {identity.fingerprint}</>}
              </button>
              {identity.fingerprintFull && <CopyButton text={`${identity.name} ${identity.fingerprintFull}`} />}
            </span>
            <Button size="sm" variant="ghost" onClick={() => setModal({ kind: "join" })}>Join with a code</Button>
            <Button size="sm" variant="primary" onClick={() => setModal({ kind: "invite" })}>Invite a peer <Key>I</Key></Button>
          </>
        }
        notes={
          <>
            {!data.reachability.reachable && (
              <Note tone="warn">
                <button type="button" onClick={openConfig} className="hover:underline">Peers can't dial this fog — what to do</button>
              </Note>
            )}
            {stale && <Note>Can't reach fogd — link figures may be out of date.</Note>}
          </>
        }
      />

      {/* body */}
      <div className={`grid flex-1 ${showDetail ? "lg:grid-cols-[minmax(0,1fr)_340px]" : ""}`}>
        <div className={`min-w-0 overflow-x-hidden ${GUTTER} py-[26px] pb-10`}>
          {tab === "peers" && (
            <PeersTab info={data} facts={facts} selected={selected} onSelect={select} onSelf={openConfig} />
          )}
          {tab === "sharing" && (
            <SharingTab
              peers={peers.filter((p) => !p.unpairedAt)}
              shares={shareList}
              subs={subList}
              offers={offers}
              offersPending={offersPending}
              onShare={() => setModal({ kind: "share", peerId: selected ?? undefined })}
              onEditShare={(share) => setModal({ kind: "share", share })}
              onOpenSubscription={(sub) => setModal({ kind: "subscription", sub })}
            />
          )}
          {tab === "invites" && (
            <InvitesTab invites={data.invites} onInvite={() => setModal({ kind: "invite" })} onAbout={() => setModal({ kind: "about" })} />
          )}
          {tab === "activity" && (
            <ActivityTab entries={entries} loading={ledger.isLoading} error={ledger.error} onRetry={() => ledger.refetch()} />
          )}
          {tab === "config" && <ConfigTab info={data} />}
        </div>
        {showDetail && current && (
          <PeerDetail
            facts={current}
            onShare={() => setModal({ kind: "share", peerId: current.peer.id })}
            onSettings={() => setModal({ kind: "settings", peerId: current.peer.id })}
            onRemove={() => setModal({ kind: "remove", peerId: current.peer.id })}
            onUnpaired={() => setModal({ kind: "unpaired", peerId: current.peer.id })}
          />
        )}
      </div>

      {modal?.kind === "invite" && <InviteModal onClose={() => setModal(null)} />}
      {modal?.kind === "join" && <JoinModal onClose={() => setModal(null)} />}
      {modal?.kind === "about" && <AboutModal onClose={() => setModal(null)} />}
      {modal?.kind === "share" && (
        <PeerShareComposer peerId={modal.peerId} share={modal.share} onClose={() => setModal(null)} />
      )}
      {modal?.kind === "remove" && (() => {
        const f = facts.find((x) => x.peer.id === modal.peerId);
        return f ? (
          <RemoveModal peer={f.peer} holds={f.holds.length} shares={f.shares.length} onClose={() => setModal(null)}
            onRemoved={() => { setModal(null); select(null); }} />
        ) : null;
      })()}
      {modal?.kind === "settings" && (() => {
        const p = peers.find((x) => x.id === modal.peerId);
        return p ? <PeerSettingsModal peer={p} onClose={() => setModal(null)} /> : null;
      })()}
      {modal?.kind === "unpaired" && (() => {
        const p = peers.find((x) => x.id === modal.peerId);
        return p ? <UnpairedModal peer={p} onClose={() => setModal(null)} onRemoved={() => { setModal(null); select(null); }} /> : null;
      })()}
      {modal?.kind === "subscription" && (
        <SubscriptionModal sub={subList.find((s) => s.id === modal.sub.id) ?? modal.sub}
          peer={peers.find((p) => p.id === modal.sub.peerId)} onClose={() => setModal(null)} />
      )}
    </div>
  );
}
