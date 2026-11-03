// The Drive page's data layer: query keys in one place (a mutation that
// invalidates the prefix reaches every listing), the paged listing, and the
// URL builders for bytes.

import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { get, type FileDetail, type FileNode, type FileSnapshot, type FilesBackupStatus, type FilesListing, type FilesSort, type FilesStatus } from "../api";

export const filesKey = ["files"] as const;
const filesStatusKey = [...filesKey, "status"] as const;
const filesListKey = (path: string, sort: FilesSort, desc: boolean) =>
  [...filesKey, "ls", path, sort, desc] as const;
const snapshotListKey = (at: number, path: string, sort: FilesSort, desc: boolean) =>
  [...filesKey, "snapshot", at, path, sort, desc] as const;
export const snapshotsKey = [...filesKey, "snapshots"] as const;
export const filesStatKey = (path: string) => [...filesKey, "stat", path] as const;
const filesSearchKey = (q: string, under: string) => [...filesKey, "search", under, q] as const;

const seg = (p: string) => p.split("/").map(encodeURIComponent).join("/");

// Bytes of one file; dl asks for a download rather than an inline view;
// at reads the file out of a snapshot instead of the live tree.
export const rawUrl = (path: string, dl = false, at?: number) =>
  `${at ? `/api/files/snapshots/${at}/raw/` : "/api/files/raw/"}${seg(path)}${dl ? "?dl=1" : ""}`;
// A cached JPEG thumbnail (96, 320 or 1024 wide); 404 means "use the icon".
export const thumbUrl = (path: string, w: number) => `/api/files/thumb/${seg(path)}?w=${w}`;
// A streamed zip of any selection.
export const zipUrl = (paths: string[]) => `/api/files/zip?${paths.map((p) => `path=${encodeURIComponent(p)}`).join("&")}`;

const PAGE = 400;

export function useFilesListing(path: string, sort: FilesSort, desc: boolean, enabled = true, at?: number) {
  const base = at ? `/api/files/snapshots/${at}/ls` : "/api/files/ls";
  return useInfiniteQuery({
    queryKey: at ? snapshotListKey(at, path, sort, desc) : filesListKey(path, sort, desc),
    queryFn: ({ pageParam }) =>
      get<FilesListing>(
        `${base}?path=${encodeURIComponent(path)}&sort=${sort}&dir=${desc ? "desc" : "asc"}&limit=${PAGE}${
          pageParam ? `&cursor=${encodeURIComponent(pageParam)}` : ""
        }`,
      ),
    initialPageParam: "",
    getNextPageParam: (last) => last.next ?? undefined,
    // The directory may be changing under other tools; a poll keeps the
    // page honest, and the server answers an unchanged folder with a 304.
    // A snapshot never changes.
    refetchInterval: at ? false : 15_000,
    enabled,
  });
}

export function useSnapshots(enabled = true) {
  return useQuery({
    queryKey: snapshotsKey,
    queryFn: () => get<{ snapshots: FileSnapshot[]; status: FilesBackupStatus }>("/api/files/snapshots"),
    refetchInterval: (q) => (q.state.data?.status.running ? 3000 : 60_000),
    enabled,
  });
}

export function useFilesStatus(opts?: { refetchInterval?: number }) {
  return useQuery({
    queryKey: filesStatusKey,
    queryFn: () => get<FilesStatus>("/api/files/status"),
    refetchInterval: opts?.refetchInterval,
    staleTime: 10_000,
  });
}

export function useFileDetail(path: string | null) {
  return useQuery({
    queryKey: filesStatKey(path ?? ""),
    queryFn: () => get<FileDetail>(`/api/files/stat?path=${encodeURIComponent(path ?? "")}`),
    enabled: path !== null,
  });
}

export function useFilesSearch(q: string, under: string) {
  return useQuery({
    queryKey: filesSearchKey(q, under),
    queryFn: () =>
      get<{ entries: FileNode[] }>(`/api/files/search?q=${encodeURIComponent(q)}&under=${encodeURIComponent(under)}&limit=200`),
    enabled: q.trim().length > 0,
    placeholderData: (prev) => prev,
  });
}

// The flat entry list a paged listing resolves to.
export function flattenListing(pages: FilesListing[] | undefined): FileNode[] {
  if (!pages) return [];
  const out: FileNode[] = [];
  for (const p of pages) out.push(...p.entries);
  return out;
}
