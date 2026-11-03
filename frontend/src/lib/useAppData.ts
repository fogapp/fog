// The app page's queries: the shell's detail and deploy status (shared by
// the advanced and basic pages), the container's lifecycle facts, fog's own
// activity on the app, its volumes as Drive sees them, its backups and
// where copies of them live. Keys live here so a mutation on one tab
// reaches the figure on another.

import { useCallback, useEffect, useRef, useState } from "react";
import { useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  get,
  type AppContainer, type AppDetail, type AuditEntry, type BackupRecord,
  type DeployStatusResponse, type FileDetail, type OffsiteRowsResponse,
  type RepoDoc, type RepoDocContent, type Volume,
} from "../api";
import { appsKey } from "./useApps";
import { placementOf, type Placement, type PlacementContext } from "./volumePlacement";
import { filesStatKey, useFilesStatus } from "./useFiles";
import { backupsStatusQuery, catalogQuery, nasQuery, subscriptionsQuery } from "../queries";

export const appKey = (name: string) => ["app", name] as const;
export const deployStatusKey = (name: string) => ["deploy-status", name] as const;
const containerKey = (name: string) => ["app", name, "container"] as const;
const appAuditKey = (name: string) => ["audit", "app", name] as const;
const docsKey = (name: string) => ["app", name, "docs"] as const;
export const appVersionsKey = (name: string) => ["app", name, "versions"] as const;
export const backupsKey = (name: string) => ["backups", name] as const;
export const offsiteRowsKey = (name: string) => ["offsite-rows", name] as const;

// How a wait for a deploy ended: it finished, it outlasted the patience
// below, or the page went away first.
export type DeployWait = "done" | "timeout" | "gone";

interface Waiter {
  started: boolean;
  appears: number;
  gaveUp: number;
  resolve: (w: DeployWait) => void;
}

// A deploy row exists a beat before the live tracker registers it.
const appearsWithin = 30_000;
const giveUpAfter = 15 * 60_000;

// The app shell's shared state: detail, deploy status, and the refresh the
// actions await. There is exactly one deploy-status poll per page — a
// caller that needs to wait for a deploy (update, rollback) asks
// awaitDeploy rather than polling on its own.
export function useAppDetail(name: string) {
  const qc = useQueryClient();
  const detail = useQuery({
    queryKey: appKey(name),
    queryFn: () => get<AppDetail>(`/api/apps/${name}`),
  });
  const waiters = useRef<Waiter[]>([]);
  const [awaiting, setAwaiting] = useState(false);
  const deployStatus = useQuery({
    queryKey: deployStatusKey(name),
    queryFn: () => get<DeployStatusResponse>(`/api/apps/${name}/deploy-status`),
    // Poll fast while a deploy is live, or while someone waits for one to
    // show up, so progress appears immediately.
    refetchInterval: (q) => {
      const d = q.state.data;
      return awaiting || d?.current.running || d?.last?.status === "running" ? 1500 : 5000;
    },
  });

  // Returns a promise on purpose: mutations await it, so a button reports a
  // result only after the queries behind that result have actually refetched.
  const refresh = useCallback(
    () =>
      Promise.all([
        qc.invalidateQueries({ queryKey: appKey(name) }),
        qc.invalidateQueries({ queryKey: appsKey }),
        qc.invalidateQueries({ queryKey: deployStatusKey(name) }),
        qc.invalidateQueries({ queryKey: containerKey(name) }),
        qc.invalidateQueries({ queryKey: appAuditKey(name) }),
        qc.invalidateQueries({ queryKey: appVersionsKey(name) }),
      ]),
    [qc, name],
  );

  // When a deploy finishes, refresh everything so the page flips state.
  const running = deployStatus.data?.current.running ?? false;
  const wasDeploying = useRef(false);
  useEffect(() => {
    if (wasDeploying.current && !running) {
      qc.invalidateQueries({ queryKey: appKey(name) });
      qc.invalidateQueries({ queryKey: appsKey });
      qc.invalidateQueries({ queryKey: deployStatusKey(name) });
      qc.invalidateQueries({ queryKey: containerKey(name) });
    }
    wasDeploying.current = running;
  }, [running, name, qc]);

  // Every poll settles the waiters — a failed one too: a restarting app can
  // drop a request, which is not an answer, but the clock still runs. Wait
  // for the deploy to appear before waiting for it to finish, or "not
  // running" on the first poll reads as "already done".
  const { data, dataUpdatedAt, errorUpdatedAt } = deployStatus;
  useEffect(() => {
    if (waiters.current.length === 0) return;
    const now = Date.now();
    const live = data && dataUpdatedAt >= errorUpdatedAt
      ? data.current.running || data.last?.status === "running"
      : undefined;
    waiters.current = waiters.current.filter((w) => {
      if (live) w.started = true;
      else if (live === false && (w.started || now > w.appears)) {
        w.resolve("done");
        return false;
      }
      if (now > w.gaveUp) {
        w.resolve("timeout");
        return false;
      }
      return true;
    });
    if (waiters.current.length === 0) setAwaiting(false);
  }, [data, dataUpdatedAt, errorUpdatedAt]);

  // A navigation away ends every wait rather than leaving it polling.
  useEffect(() => () => {
    for (const w of waiters.current) w.resolve("gone");
    waiters.current = [];
  }, []);

  const awaitDeploy = useCallback(
    () =>
      new Promise<DeployWait>((resolve) => {
        const now = Date.now();
        waiters.current.push({ started: false, appears: now + appearsWithin, gaveUp: now + giveUpAfter, resolve });
        setAwaiting(true);
      }),
    [],
  );

  return { detail, deployStatus, deploying: running, refresh, awaitDeploy };
}

export function useAppContainer(name: string, enabled = true) {
  return useQuery({
    queryKey: containerKey(name),
    queryFn: () => get<AppContainer>(`/api/apps/${encodeURIComponent(name)}/container`),
    refetchInterval: 10_000,
    enabled,
  });
}

export function useAppAudit(name: string) {
  return useQuery({
    queryKey: appAuditKey(name),
    queryFn: () => get<AuditEntry[]>(`/api/audit?app=${encodeURIComponent(name)}`),
    refetchInterval: 30_000,
  });
}

export function useAppDocs(name: string, enabled = true) {
  return useQuery({
    queryKey: docsKey(name),
    queryFn: () => get<RepoDoc[]>(`/api/apps/${encodeURIComponent(name)}/docs`),
    enabled,
  });
}

export function useAppDoc(name: string, file: string | undefined) {
  return useQuery({
    queryKey: [...docsKey(name), file ?? ""],
    queryFn: () => get<RepoDocContent>(`/api/apps/${encodeURIComponent(name)}/docs/${encodeURIComponent(file ?? "")}`),
    enabled: !!file,
  });
}

export function useAppBackups(name: string) {
  return useQuery({
    queryKey: backupsKey(name),
    queryFn: () => get<BackupRecord[]>(`/api/apps/${encodeURIComponent(name)}/backups`),
  });
}

export function useAppOffsite(name: string) {
  return useQuery({
    queryKey: offsiteRowsKey(name),
    queryFn: () => get<OffsiteRowsResponse>(`/api/apps/${encodeURIComponent(name)}/offsite`),
    refetchInterval: 30_000,
  });
}

export function useBackupsStatus() {
  return useQuery({
    ...backupsStatusQuery,
    refetchInterval: 60_000,
  });
}

export function useCatalogEntry(name: string) {
  const catalog = useQuery(catalogQuery);
  return { known: !!catalog.data, entry: catalog.data?.entries.find((e) => e.id === name) };
}

// One volume of the app as the page reasons about it: the spec's line,
// where it lives, and — for a Drive folder — what Drive knows about it.
export interface AppVolume {
  volume: Volume;
  // Which service mounts it: "" for the main container, else the sidecar.
  service: string;
  placement: Placement;
  // The Drive-relative folder, for files and peer placements.
  rel?: string;
  stat?: FileDetail;
  // stat's phase: pending until Drive answers; "missing" is Drive saying the
  // folder is not there (a 404), which is a finding of its own.
  statPhase: "pending" | "ready" | "missing" | "error" | "n/a";
}

export function useAppVolumes(detail: AppDetail | undefined) {
  const filesStatus = useFilesStatus();
  const filesRoot = filesStatus.data?.ok ? filesStatus.data.root : "";
  const nas = useQuery(nasQuery);
  const subs = useQuery({
    ...subscriptionsQuery,
    enabled: !!filesRoot,
  });
  const ctx: PlacementContext = {
    filesRoot,
    shares: nas.data?.shares ?? [],
    peerSubs: (subs.data?.subscriptions ?? []).filter((s) => s.status !== "revoked"),
  };
  const lines: { volume: Volume; service: string }[] = [
    ...(detail?.spec.volumes ?? []).map((v) => ({ volume: v, service: "" })),
    ...(detail?.spec.aux ?? []).flatMap((a) => (a.volumes ?? []).map((v) => ({ volume: v, service: a.name }))),
  ];
  const placed = lines.map((l) => {
    const placement = placementOf(l.volume, ctx);
    const rel = placement.kind === "files" || placement.kind === "peer" ? placement.rel : undefined;
    return { ...l, placement, rel };
  });
  const stats = useQueries({
    queries: placed.map((p) => ({
      queryKey: filesStatKey(p.rel ?? ""),
      queryFn: () => get<FileDetail>(`/api/files/stat?path=${encodeURIComponent(p.rel ?? "")}`),
      enabled: !!filesRoot && p.rel !== undefined,
      retry: false,
      refetchInterval: 60_000,
    })),
  });
  const volumes: AppVolume[] = placed.map((p, i) => {
    const q = stats[i];
    const statPhase: AppVolume["statPhase"] =
      p.rel === undefined ? "n/a"
      : q.data ? "ready"
      : q.isError ? (isNotFound(q.error) ? "missing" : "error")
      : "pending";
    return { ...p, stat: q.data, statPhase };
  });
  return { volumes, filesStatus, filesRoot, ctx };
}

function isNotFound(e: unknown): boolean {
  return typeof e === "object" && e !== null && "status" in e && (e as { status: number }).status === 404;
}
