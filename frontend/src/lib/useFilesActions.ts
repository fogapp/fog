// The Files verbs from the page's side: each call patches every cached
// listing it can predict the outcome of (a rename, a move out, a trash)
// before the request lands, then invalidates so the server's answer wins.
// A verb that fails rolls its patch back by invalidating too — the cache
// simply re-reads the truth.

import { useQueryClient, type InfiniteData, type QueryClient } from "@tanstack/react-query";
import {
  ApiError, del, post,
  type FileNode, type FileShare, type FilesConflict, type FilesConflictPolicy, type FilesJob, type FilesListing, type FilesMoveResult, type TrashItem,
} from "../api";
import { filesKey, snapshotsKey } from "./useFiles";
import { parentOf } from "./drivePath";

export const filesTrashKey = [...filesKey, "trash"] as const;
export const filesSharesKey = (path: string) => [...filesKey, "shares", path] as const;
export const filesJobsKey = [...filesKey, "jobs"] as const;

type Pages = InfiniteData<FilesListing, string>;

// patchListing rewrites the cached pages of one folder's listings (every
// sort) through fn. Totals follow the entry count delta.
function patchListing(qc: QueryClient, dir: string, fn: (entries: FileNode[]) => FileNode[]) {
  qc.setQueriesData<Pages>({ queryKey: [...filesKey, "ls", dir] }, (old) => {
    if (!old) return old;
    const before = old.pages.reduce((n, p) => n + p.entries.length, 0);
    const pages = old.pages.map((p) => ({ ...p, entries: fn(p.entries) }));
    const after = pages.reduce((n, p) => n + p.entries.length, 0);
    return { ...old, pages: pages.map((p, i) => (i === 0 ? { ...p, total: Math.max(0, p.total + (after - before)) } : p)) };
  });
}

export function conflictOf(e: unknown): FilesConflict | null {
  if (e instanceof ApiError && e.status === 409) {
    const c = (e.body as { conflict?: FilesConflict } | undefined)?.conflict;
    if (c) return c;
  }
  return null;
}

export function useFilesActions() {
  const qc = useQueryClient();
  const settle = () => qc.invalidateQueries({ queryKey: filesKey });

  return {
    async mkdir(path: string): Promise<FileNode> {
      try {
        const n = await post<FileNode>("/api/files/mkdir", { path });
        patchListing(qc, parentOf(path), (es) => (es.some((e) => e.path === n.path) ? es : [n, ...es]));
        return n;
      } finally {
        settle();
      }
    },

    async rename(path: string, name: string): Promise<FileNode> {
      const dir = parentOf(path);
      const next = dir ? `${dir}/${name}` : name;
      patchListing(qc, dir, (es) => es.map((e) => (e.path === path ? { ...e, path: next, name } : e)));
      try {
        return await post<FileNode>("/api/files/rename", { path, name });
      } finally {
        settle();
      }
    },

    async move(paths: string[], to: string, onConflict: FilesConflictPolicy = ""): Promise<FilesMoveResult> {
      const set = new Set(paths);
      for (const dir of new Set(paths.map(parentOf))) {
        if (dir !== to) patchListing(qc, dir, (es) => es.filter((e) => !set.has(e.path)));
      }
      try {
        return await post<FilesMoveResult>("/api/files/move", { paths, to, onConflict });
      } finally {
        settle();
      }
    },

    async copy(paths: string[], to: string, onConflict: FilesConflictPolicy = ""): Promise<FilesJob> {
      try {
        return await post<FilesJob>("/api/files/copy", { paths, to, onConflict });
      } finally {
        qc.invalidateQueries({ queryKey: filesJobsKey });
      }
    },

    async trash(paths: string[]): Promise<TrashItem[]> {
      const set = new Set(paths);
      for (const dir of new Set(paths.map(parentOf))) {
        patchListing(qc, dir, (es) => es.filter((e) => !set.has(e.path)));
      }
      try {
        const res = await post<{ items: TrashItem[] }>("/api/files/trash", { paths });
        return res.items;
      } finally {
        settle();
      }
    },

    async putBack(ids: string[]): Promise<string[]> {
      try {
        const res = await post<{ paths: string[] }>("/api/files/trash/restore", { ids });
        return res.paths;
      } finally {
        settle();
      }
    },

    async purge(id: string): Promise<void> {
      try {
        await del(`/api/files/trash/${id}`);
      } finally {
        qc.invalidateQueries({ queryKey: filesTrashKey });
      }
    },

    async emptyTrash(): Promise<number> {
      try {
        const res = await del<{ purged: number }>("/api/files/trash");
        return res.purged;
      } finally {
        qc.invalidateQueries({ queryKey: filesTrashKey });
      }
    },

    async setExcluded(path: string, excluded: boolean): Promise<void> {
      const dir = parentOf(path);
      patchListing(qc, dir, (es) => es.map((e) => (e.path === path ? { ...e, excluded, excludedBy: undefined } : e)));
      try {
        await post("/api/files/excludes", { path, excluded });
      } finally {
        settle();
      }
    },

    // Turn a peer's contribution away at this origin (or remove a stub).
    async reject(path: string): Promise<void> {
      const dir = parentOf(path);
      patchListing(qc, dir, (es) => es.filter((e) => e.path !== path));
      try {
        await post("/api/files/contributions/reject", { path });
      } finally {
        settle();
      }
    },

    // Keep a peer's entry offline (fetching it) or free its space again.
    async setOffline(path: string, keep: boolean): Promise<FileNode> {
      const dir = parentOf(path);
      try {
        const d = await post<FileNode>("/api/files/hydrate", { path, keep });
        patchListing(qc, dir, (es) => es.map((e) => (e.path === path ? { ...e, ...d } : e)));
        return d;
      } finally {
        settle();
      }
    },

    async setKept(path: string, keep: boolean): Promise<void> {
      const dir = parentOf(path);
      patchListing(qc, dir, (es) => es.map((e) => (e.path === path ? { ...e, kept: keep, keptBy: undefined } : e)));
      try {
        await post("/api/files/keep", { path, keep });
      } finally {
        settle();
      }
    },

    async snapshotNow(): Promise<FilesJob> {
      try {
        return await post<FilesJob>("/api/files/snapshots");
      } finally {
        qc.invalidateQueries({ queryKey: snapshotsKey });
        qc.invalidateQueries({ queryKey: filesJobsKey });
      }
    },

    async createShare(path: string, days: number, password: string): Promise<{ share: FileShare; url: string }> {
      try {
        return await post<{ share: FileShare; url: string }>("/api/files/shares", { path, days, password });
      } finally {
        qc.invalidateQueries({ queryKey: filesSharesKey(path) });
      }
    },

    async revokeShare(id: number, path: string): Promise<void> {
      try {
        await del(`/api/files/shares/${id}`);
      } finally {
        qc.invalidateQueries({ queryKey: filesSharesKey(path) });
      }
    },

    async restore(at: number, paths: string[], to: string | undefined, onConflict: FilesConflictPolicy = ""): Promise<FilesJob> {
      try {
        return await post<FilesJob>(`/api/files/snapshots/${at}/restore`, { paths, to, onConflict });
      } finally {
        qc.invalidateQueries({ queryKey: filesJobsKey });
      }
    },
  };
}
