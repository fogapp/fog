// The upload manager: a queue of files, a pool of four in flight, eight
// megabyte chunks, retries with backoff, pause/resume/cancel, and a memory
// of unfinished sessions so the same file dropped again after a reload
// continues from the server's offset. Module state with a subscriber list
// (the theme store's pattern) so the tray and the page read one truth and
// an upload outlives the component that started it.

import { useSyncExternalStore } from "react";
import { ApiError, type FileNode, type FilesConflict, type FilesConflictPolicy } from "../api";

type UploadStatus = "queued" | "uploading" | "paused" | "done" | "error" | "conflict" | "skipped" | "cancelled";

export interface UploadItem {
  key: string; // client id
  file: File;
  dest: string; // root-relative destination path (with name)
  dir: string; // the folder it lands in
  size: number;
  status: UploadStatus;
  offset: number;
  sessionId?: string;
  error?: string;
  conflict?: FilesConflict;
  policy: FilesConflictPolicy;
  startedAt?: number;
  rate?: number; // bytes/s, smoothed
  node?: FileNode;
}

const CHUNK = 8 * 1024 * 1024;
const POOL = 4;
const RETRIES = 6;
const STORE_KEY = "fog.files.uploads";

let items: UploadItem[] = [];
const listeners = new Set<() => void>();
const controllers = new Map<string, AbortController>();
let running = 0;
let defaultPolicy: FilesConflictPolicy = "";
// Listeners that want to know a folder changed (the page invalidates).
const landed = new Set<(dir: string, node: FileNode) => void>();

function emit() {
  items = [...items];
  for (const l of listeners) l();
}

function patch(key: string, p: Partial<UploadItem>) {
  const i = items.findIndex((it) => it.key === key);
  if (i < 0) return;
  items[i] = { ...items[i], ...p };
  emit();
}

function get(key: string): UploadItem | undefined {
  return items.find((it) => it.key === key);
}

// ---- resume memory ----

interface Remembered {
  sessionId: string;
  dest: string;
}
function fileKey(f: File, dest: string): string {
  return `${dest}|${f.size}|${f.lastModified}`;
}
function remembered(): Record<string, Remembered> {
  try {
    return JSON.parse(localStorage.getItem(STORE_KEY) ?? "{}") as Record<string, Remembered>;
  } catch {
    return {};
  }
}
function remember(k: string, r: Remembered | null) {
  try {
    const all = remembered();
    if (r) all[k] = r;
    else delete all[k];
    localStorage.setItem(STORE_KEY, JSON.stringify(all));
  } catch {
    /* private mode */
  }
}

// ---- public API ----

export function useUploads(): UploadItem[] {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => items,
    () => items,
  );
}

export function onUploadLanded(fn: (dir: string, node: FileNode) => void): () => void {
  landed.add(fn);
  return () => landed.delete(fn);
}

// enqueue adds files to the queue. relPath is the path inside dir (a
// folder drop keeps its structure); a bare file's relPath is its name.
export function enqueue(dir: string, files: { file: File; relPath: string }[]) {
  const seen = new Set(items.map((it) => it.key));
  for (const { file, relPath } of files) {
    const dest = dir ? `${dir}/${relPath}` : relPath;
    const key = fileKey(file, dest);
    if (seen.has(key) && get(key)?.status !== "done") continue;
    items = items.filter((it) => it.key !== key);
    items.push({
      key, file, dest, size: file.size, status: "queued", offset: 0, policy: defaultPolicy,
      dir: dest.includes("/") ? dest.slice(0, dest.lastIndexOf("/")) : "",
    });
  }
  emit();
  pump();
}

export function pause(key: string) {
  const it = get(key);
  if (!it || (it.status !== "uploading" && it.status !== "queued")) return;
  controllers.get(key)?.abort();
  patch(key, { status: "paused" });
}

export function resume(key: string) {
  const it = get(key);
  if (!it || (it.status !== "paused" && it.status !== "error")) return;
  patch(key, { status: "queued", error: undefined });
  pump();
}

export function cancel(key: string) {
  const it = get(key);
  if (!it) return;
  controllers.get(key)?.abort();
  if (it.sessionId) fetch(`/api/files/uploads/${it.sessionId}`, { method: "DELETE", credentials: "same-origin" }).catch(() => {});
  remember(key, null);
  patch(key, { status: "cancelled" });
}

// resolveConflict answers a "name taken" for one item — or, with all set,
// for every item waiting on the same question and every one to come.
export function resolveConflict(key: string, policy: Exclude<FilesConflictPolicy, "">, all: boolean) {
  if (all) {
    defaultPolicy = policy;
    for (const it of items) if (it.status === "conflict") patch(it.key, { status: "queued", policy, conflict: undefined });
    for (const it of items) if (it.status === "queued") patch(it.key, { policy });
  } else {
    patch(key, { status: "queued", policy, conflict: undefined });
  }
  pump();
}

export function clearFinished() {
  items = items.filter((it) => !["done", "cancelled", "skipped", "error"].includes(it.status));
  if (items.length === 0) defaultPolicy = "";
  emit();
}

export function summary(list: UploadItem[]) {
  const active = list.filter((it) => it.status === "uploading" || it.status === "queued" || it.status === "paused" || it.status === "conflict");
  const done = list.filter((it) => it.status === "done").length;
  const total = active.reduce((s, it) => s + it.size, 0);
  const sent = active.reduce((s, it) => s + it.offset, 0);
  const rate = active.reduce((s, it) => s + (it.rate ?? 0), 0);
  return { active: active.length, done, total, sent, rate, failed: list.filter((it) => it.status === "error").length };
}

// ---- the pump ----

function pump() {
  while (running < POOL) {
    const next = items.find((it) => it.status === "queued");
    if (!next) return;
    running++;
    run(next.key).finally(() => {
      running--;
      pump();
    });
  }
}

async function json<T>(res: Response): Promise<T> {
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, (body as { error?: string }).error ?? res.statusText, body);
  return body as T;
}

async function run(key: string) {
  const it = get(key);
  if (!it || it.status !== "queued") return;
  const ac = new AbortController();
  controllers.set(key, ac);
  patch(key, { status: "uploading", startedAt: Date.now(), error: undefined });
  try {
    // A session: remembered from before, or new.
    let sessionId = it.sessionId;
    let offset = 0;
    if (!sessionId) {
      const r = remembered()[key];
      if (r && r.dest === it.dest) {
        const res = await fetch(`/api/files/uploads/${r.sessionId}`, { credentials: "same-origin", signal: ac.signal });
        if (res.ok) {
          const u = await json<{ id: string; offset: number; size: number }>(res);
          if (u.size === it.size) {
            sessionId = u.id;
            offset = u.offset;
          }
        }
      }
    } else {
      const res = await fetch(`/api/files/uploads/${sessionId}`, { credentials: "same-origin", signal: ac.signal });
      if (res.ok) offset = (await json<{ offset: number }>(res)).offset;
      else sessionId = undefined;
    }
    if (!sessionId) {
      const res = await fetch("/api/files/uploads", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        signal: ac.signal,
        body: JSON.stringify({
          path: it.dest,
          size: it.size,
          mtime: new Date(it.file.lastModified).toISOString(),
          onConflict: it.policy,
        }),
      });
      if (res.status === 409) {
        const body = (await res.json().catch(() => ({}))) as { conflict?: FilesConflict };
        patch(key, { status: "conflict", conflict: body.conflict });
        return;
      }
      const u = await json<{ id: string; offset: number; skipped?: boolean }>(res);
      if (u.skipped) {
        patch(key, { status: "skipped" });
        return;
      }
      sessionId = u.id;
      offset = u.offset;
      remember(key, { sessionId, dest: it.dest });
    }
    patch(key, { sessionId, offset });

    // Chunks, with the server's offset as the truth on any disagreement.
    let attempts = 0;
    let lastTick = Date.now();
    let lastOffset = offset;
    while (offset < it.size) {
      const end = Math.min(offset + CHUNK, it.size);
      try {
        const res = await fetch(`/api/files/uploads/${sessionId}`, {
          method: "PATCH",
          credentials: "same-origin",
          headers: { "Content-Type": "application/octet-stream", "Upload-Offset": String(offset) },
          body: it.file.slice(offset, end),
          signal: ac.signal,
        });
        if (res.status === 409) {
          const body = (await res.json().catch(() => ({}))) as { offset?: number };
          if (typeof body.offset === "number") {
            offset = body.offset;
            continue;
          }
        }
        const u = await json<{ offset: number }>(res);
        offset = u.offset;
        attempts = 0;
        const now = Date.now();
        const dt = (now - lastTick) / 1000;
        const cur = get(key);
        const inst = dt > 0 ? (offset - lastOffset) / dt : 0;
        const rate = cur?.rate ? cur.rate * 0.7 + inst * 0.3 : inst;
        lastTick = now;
        lastOffset = offset;
        patch(key, { offset, rate });
      } catch (e) {
        if (ac.signal.aborted) return;
        if (e instanceof ApiError && e.status >= 400 && e.status < 500 && e.status !== 408 && e.status !== 429) throw e;
        if (++attempts > RETRIES) throw e;
        await new Promise((r) => setTimeout(r, Math.min(30_000, 500 * 2 ** attempts)));
      }
    }

    const res = await fetch(`/api/files/uploads/${sessionId}/complete`, { method: "POST", credentials: "same-origin", signal: ac.signal });
    if (res.status === 409) {
      patch(key, { status: "skipped" });
      remember(key, null);
      return;
    }
    const node = await json<FileNode>(res);
    remember(key, null);
    patch(key, { status: "done", offset: it.size, node, rate: 0 });
    for (const fn of landed) fn(it.dir, node);
  } catch (e) {
    if (ac.signal.aborted) return;
    patch(key, { status: "error", error: e instanceof Error ? e.message : "Upload failed", rate: 0 });
  } finally {
    controllers.delete(key);
  }
}

// ---- what was dropped ----

// filesFromDataTransfer reads a drop, descending into folders where the
// browser allows (Chrome, Safari, Firefox all expose webkitGetAsEntry) and
// keeping each file's path inside the dropped folder.
export async function filesFromDataTransfer(dt: DataTransfer): Promise<{ file: File; relPath: string }[]> {
  const out: { file: File; relPath: string }[] = [];
  const entries: FileSystemEntry[] = [];
  for (const item of Array.from(dt.items ?? [])) {
    const entry = item.webkitGetAsEntry?.();
    if (entry) entries.push(entry);
  }
  if (entries.length === 0) {
    for (const f of Array.from(dt.files)) out.push({ file: f, relPath: f.name });
    return out;
  }
  const walk = async (entry: FileSystemEntry, prefix: string): Promise<void> => {
    if (entry.isFile) {
      const f = await new Promise<File>((res, rej) => (entry as FileSystemFileEntry).file(res, rej));
      out.push({ file: f, relPath: prefix + entry.name });
    } else if (entry.isDirectory) {
      const reader = (entry as FileSystemDirectoryEntry).createReader();
      // readEntries returns in batches; keep reading until an empty one.
      for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((res, rej) => reader.readEntries(res, rej));
        if (batch.length === 0) break;
        for (const e of batch) await walk(e, prefix + entry.name + "/");
      }
    }
  };
  for (const e of entries) await walk(e, "");
  return out;
}

// filesFromInput reads a file input, honouring webkitRelativePath for a
// folder picker.
export function filesFromInput(list: FileList): { file: File; relPath: string }[] {
  return Array.from(list).map((f) => ({ file: f, relPath: f.webkitRelativePath || f.name }));
}
