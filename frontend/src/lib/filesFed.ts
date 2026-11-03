// The client mirror of the files-across-peers lens: the vocabulary, the
// impact sentence rendered live in the composer (the server re-derives the
// true one on save), and the query keys.

import type { FileFedShare } from "../api";

const SHARE_MODES = ["read", "contribute"] as const;
const SHARE_SCOPES = ["all", "picked", "filtered"] as const;
export const SHARE_DETAILS = ["full", "previews", "listing"] as const;
export const SHARE_KINDS = ["image", "video", "audio", "document", "archive", "text"] as const;
type ShareMode = (typeof SHARE_MODES)[number];
type ShareScope = (typeof SHARE_SCOPES)[number];
export type ShareDetail = (typeof SHARE_DETAILS)[number];
export type ShareKind = (typeof SHARE_KINDS)[number];

export const DETAIL_LABELS: Record<ShareDetail, string> = {
  full: "Full copies",
  previews: "Previews — names, sizes and thumbnails; a file is fetched when opened",
  listing: "Listing — names, sizes and dates only",
};

export const KIND_LABELS: Record<ShareKind, string> = {
  image: "photos", video: "videos", audio: "audio", document: "documents", archive: "archives", text: "text files",
};

// Every peer's shares sit under this prefix, so invalidating it refreshes all.
export const peerSharesRootKey = ["files", "peer-shares"] as const;
export const peerSharesKey = (peerId?: number) => [...peerSharesRootKey, peerId ?? 0] as const;

export interface LensDraft {
  peerId: number;
  path: string;
  title: string;
  mode: ShareMode;
  scope: ShareScope;
  detail: ShareDetail;
  since: string; // "" or YYYY-MM-DD
  apps: "allowed" | "denied";
  contributeQuotaBytes: number; // 0 = unlimited; only meaningful in contribute mode
  picks: string[];
  filters: string[];
}

export function draftFromShare(sh: FileFedShare): LensDraft {
  return {
    peerId: sh.peerId,
    path: sh.path,
    title: sh.title,
    mode: sh.mode,
    scope: sh.scope,
    detail: sh.detail,
    since: sh.since ? sh.since.slice(0, 10) : "",
    apps: sh.apps,
    contributeQuotaBytes: sh.contributeQuotaBytes ?? 0,
    picks: sh.picks ?? [],
    filters: sh.filters ?? [],
  };
}

export function emptyDraft(peerId: number, path: string): LensDraft {
  const title = path.includes("/") ? path.slice(path.lastIndexOf("/") + 1) : path || "Drive";
  return { peerId, path, title, mode: "read", scope: "all", detail: "full", since: "", apps: "allowed", contributeQuotaBytes: 0, picks: [], filters: [] };
}

// The sentence, as the server will write it (modulo counts it knows and we
// do not). Kept in step with filesfed.Impact.
export function impactSentence(d: LensDraft, peer: string, keptCount = 0): string {
  let what = "the files";
  if (d.scope === "picked") what = `${d.picks.length} hand-picked entr${d.picks.length === 1 ? "y" : "ies"}`;
  else if (d.scope === "filtered") what = `only ${describeFilters(d.filters)}`;
  const detail = {
    full: "full copies",
    previews: "previews (names, sizes and thumbnails; a file is fetched when opened)",
    listing: "a listing (names, sizes and dates only)",
  }[d.detail];
  let s = `${peer} sees ${what} in ${d.title || "this folder"} as ${detail}`;
  if (d.since) s += `, going forward from ${new Date(d.since).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" })}`;
  s += d.apps === "allowed" ? ` — and may mount them for apps on ${peer}'s fog.` : " — for people, not for apps there.";
  s += d.mode === "contribute" ? " They can add files of their own; each side keeps its own." : " They cannot add files.";
  if (keptCount > 0) s += ` Never shared: ${keptCount} entr${keptCount === 1 ? "y" : "ies"} marked keep on this box.`;
  return s;
}

function describeFilters(filters: string[]): string {
  return filters
    .map((f) => {
      const [kind, arg] = f.split(/:(.*)/s);
      return kind === "kind" ? KIND_LABELS[arg as ShareKind] ?? arg : `entries matching ${arg}`;
    })
    .join(" and ");
}

// What a wider-than-default choice newly discloses — the escalation callouts.
export function escalations(d: LensDraft): string[] {
  const out: string[] = [];
  if (d.mode === "contribute") out.push("The peer can put files into this folder. Theirs stay theirs; yours stay yours.");
  if (d.apps === "allowed") out.push("Apps on the peer's fog (an indexer, a media server) may be given this folder read-only.");
  if (d.scope === "all" && !d.since) out.push("Everything in the folder, past and future, crosses — unless marked keep on this box.");
  return out;
}
