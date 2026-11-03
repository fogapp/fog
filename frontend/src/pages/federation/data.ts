// The Federation page's reads and the words it uses for a peer's state.
// One query per fact source — the federation document, every outbound
// share, every inbound subscription, the ledger — shared by the tabs, the
// detail column and the sidebar so a count can never disagree with the
// table it summarises.

import { useQueries, useQuery } from "@tanstack/react-query";
import {
  get,
  type FedPeer,
  type FederationLedgerEntry,
  type FileFedCatalogEntry,
  type FileFedShare,
  type FileFedSubscription,
} from "../../api";
import { timeAgo } from "../../lib/format";
import type { Tone } from "../app/cockpit";
import { subscriptionsQuery } from "../../queries";

const ledgerKey = ["federation", "ledger"] as const;

export function useAllSubscriptions() {
  return useQuery({
    ...subscriptionsQuery,
    refetchInterval: 30_000,
  });
}

export function useLedger(limit = 60) {
  return useQuery({
    queryKey: [...ledgerKey, limit],
    queryFn: () => get<{ entries: FederationLedgerEntry[] }>(`/api/federation/ledger?limit=${limit}`),
    refetchInterval: 30_000,
  });
}

// An offer: a folder a peer shares that this box holds no subscription to.
// Catalogs are read from each peer that grants file shares and can be
// dialled; a peer that can't be read simply contributes no offers.
export interface Offer extends FileFedCatalogEntry {
  peer: FedPeer;
}

export function useOffers(peers: FedPeer[], subs: FileFedSubscription[]) {
  const readable = peers.filter((p) => !p.unpairedAt && !!p.url && p.remoteGrants.includes("files.share"));
  const catalogs = useQueries({
    queries: readable.map((p) => ({
      queryKey: ["federation", "files-catalog", p.id],
      queryFn: () => get<{ shares: FileFedCatalogEntry[] }>(`/api/federation/peers/${p.id}/files`),
      retry: false,
      refetchInterval: 60_000,
    })),
  });
  const offers: Offer[] = [];
  catalogs.forEach((q, i) => {
    const peer = readable[i];
    for (const c of q.data?.shares ?? []) {
      if (!subs.some((s) => s.peerId === peer.id && s.remoteShareId === c.id)) offers.push({ ...c, peer });
    }
  });
  return { offers, pending: catalogs.some((q) => q.isPending) };
}

// A peer's state, in the order trouble wins: unpaired by them, unreachable
// from here, quiet, or answering.
export type PeerState = "ok" | "off" | "unreachable" | "unpaired";

export function peerState(p: FedPeer): PeerState {
  if (p.unpairedAt) return "unpaired";
  if (p.dialError) return "unreachable";
  return p.online ? "ok" : "off";
}

export function peerTone(p: FedPeer): Tone {
  const s = peerState(p);
  return s === "ok" ? "ok" : s === "off" ? "off" : "warn";
}

export function stateWord(p: FedPeer): string {
  switch (peerState(p)) {
    case "unpaired": return "unpaired by them";
    case "unreachable": return "unreachable from here";
    case "off": return "offline";
    default: return "online";
  }
}

// A round trip under a millisecond is a real measurement, not a zero.
export function rttText(ms: number): string {
  return ms < 1 ? "<1" : String(ms);
}

// When the peer was last heard from, as the Seen column says it. "Now" is
// the last minute; "online" (a heartbeat within six minutes) is not the
// same claim, so an older exchange keeps its age even while the dot is lit.
export function seenText(p: FedPeer): string {
  if (!p.lastSeenAt) return "Never";
  const age = (Date.now() - new Date(p.lastSeenAt).getTime()) / 1000;
  if (age < 60) return "Now";
  return timeAgo(p.lastSeenAt);
}

export const MODE_LABEL: Record<FileFedShare["mode"], string> = { read: "Read only", contribute: "Can add files" };
export const DETAIL_LABEL: Record<FileFedShare["detail"], string> = { full: "Full copy", previews: "Previews", listing: "Listing" };

// A subscription's state as one phrase and a tone.
export function subState(s: FileFedSubscription): { text: string; tone: "ok" | "warn" | "quiet" } {
  switch (s.status) {
    case "active":
      return { text: s.lastSyncedAt ? `synced ${timeAgo(s.lastSyncedAt)}` : "first sync pending", tone: "ok" };
    case "error":
      return { text: "sync failing", tone: "warn" };
    case "paused":
      return { text: "paused", tone: "quiet" };
    case "revoked":
      return { text: "stopped by them", tone: "warn" };
    case "offer_changed":
      return { text: "terms changed", tone: "warn" };
    default:
      return { text: s.status, tone: "quiet" };
  }
}
