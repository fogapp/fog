// What Drive is showing: the folder's listing (or a name search under it),
// with the Apps and Content folders lifted out of the root into their own
// tabs, and what kind of place this is — live, a snapshot, or a copy of a
// peer's share — which decides whether anything here can change.

import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { get, type TrashItem } from "../../api";
import { flattenListing, useFilesListing, useFilesSearch, useFilesStatus, useSnapshots } from "../../lib/useFiles";
import { filesTrashKey } from "../../lib/useFilesActions";
import { APPS_DIR, CONTENT_DIR } from "../../lib/volumePlacement";
import { useViewPrefs } from "./viewPrefs";

// rel is that folder, or inside it.
function under(rel: string, dir: string): boolean {
  return rel === dir || rel.startsWith(dir + "/");
}

export function useDriveListing(rel: string, at: number | undefined, sharedView: boolean) {
  const timeTravel = at !== undefined;
  const status = useFilesStatus({ refetchInterval: 30_000 });
  const prefs = useViewPrefs(rel);
  const [q, setQ] = useState("");
  useEffect(() => setQ(""), [rel, at]);
  const usable = status.data?.ok !== false;
  // Shared lists lenses, not a folder: no listing is fetched for it.
  const listing = useFilesListing(rel, prefs.sort, prefs.desc, usable && !sharedView, at);
  const searching = q.trim().length > 0 && !timeTravel;
  const search = useFilesSearch(q.trim(), rel);
  const snapshots = useSnapshots(timeTravel);
  const snapshot = snapshots.data?.snapshots.find((s) => s.id === at);
  const trashCount = useQuery({
    queryKey: filesTrashKey,
    queryFn: () => get<{ items: TrashItem[] }>("/api/files/trash"),
    staleTime: 30_000,
  });

  // Apps and Content are tabs, not entries: the two folders fog gives a
  // meaning to sit beside your files rather than in among them, so the root
  // listing hides both and the count forgets them. Search still reaches
  // inside — a hit there opens the tab the folder lives on.
  const inApps = under(rel, APPS_DIR);
  const inContent = under(rel, CONTENT_DIR);
  const { entries, hidden } = useMemo(() => {
    const list = searching ? search.data?.entries ?? [] : flattenListing(listing.data?.pages);
    if (searching || rel !== "") return { entries: list, hidden: 0 };
    const kept = list.filter((n) => n.role !== "apps" && n.role !== "content");
    return { entries: kept, hidden: list.length - kept.length };
  }, [searching, search.data, listing.data, rel]);
  const folder = listing.data?.pages[0]?.dir;
  // Inside a copy of a peer's share: every verb is off; the pane says why.
  // A drop box (contribute mode) takes new files and lets us change our
  // own; the peer's entries stay theirs.
  const replica = !!folder?.peer && folder.peer.kind === "replica";
  const dropBox = replica && folder?.peer?.mode === "contribute" && !folder?.peer?.revoked;
  const readOnly = timeTravel || (replica && !dropBox);
  const total = searching ? entries.length : Math.max(0, (listing.data?.pages[0]?.total ?? 0) - hidden);
  const trashItems = trashCount.data?.items.length ?? 0;

  return {
    status, prefs, q, setQ, listing, search, searching, snapshot,
    inApps, inContent, entries, hidden, folder, replica, dropBox, readOnly, total, trashItems,
  };
}
