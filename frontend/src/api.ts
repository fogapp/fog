// Thin typed fetch layer. All endpoints are same-origin JSON.

import type {
  AppRecord,
  AppRevision,
  AppSpec,
  CapacityApp,
  CapacityHost,
  ContainerFactsT,
  DeployRecord,
  Disk,
  ErrorGroup,
  FedPeer,
  FederationFacts,
  FileFedShare,
  FileNode,
  Fit,
  GitInfo,
  Issue,
  Job,
  MetricSample,
  NasShare,
  UpdateCheckResult,
} from "./api.gen";

// The API's response types are generated from the Go structs that encode
// them (internal/tools/tsgen). What stays here are request bodies, the
// envelopes handlers build from maps, and types the UI refines.
export type * from "./api.gen";

export class ApiError extends Error {
  status: number;
  // The decoded error body, for endpoints that say more than a message (a
  // 409 from Files carries the entry in the way).
  body?: unknown;
  constructor(status: number, message: string, body?: unknown) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  // A FormData body is multipart: the browser sets that Content-Type, with
  // its boundary, only when nothing else has.
  const json = !(init?.body instanceof FormData);
  const res = await fetch(path, {
    headers: json ? { "Content-Type": "application/json" } : undefined,
    credentials: "same-origin",
    ...init,
  });
  if (!res.ok) {
    let message = res.statusText;
    let body: unknown;
    try {
      body = await res.json();
      const e = (body as { error?: string }).error;
      if (e) message = e;
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, message, body);
  }
  return res.json() as Promise<T>;
}

export const get = <T,>(path: string) => request<T>(path);
export const post = <T,>(path: string, body?: unknown) =>
  request<T>(path, { method: "POST", body: body ? JSON.stringify(body) : undefined });
export const put = <T,>(path: string, body: unknown) =>
  request<T>(path, { method: "PUT", body: JSON.stringify(body) });
// Multipart upload (backup artifacts): request leaves the Content-Type to
// the browser, which sets the multipart boundary itself.
export const postForm = <T,>(path: string, form: FormData) =>
  request<T>(path, { method: "POST", body: form });

// ---- types ----

export interface SystemMetrics {
  intervalSeconds: number;
  samples: MetricSample[];
}

// Settings (GET /api/settings) is generated from fogd's editableSettings.

// ---- offsite (docs/locker-contract.md) ----

export type OffsiteJob = Job;

export interface RepoDocContent {
  file: string;
  content: string;
  truncated: boolean;
}

export const del = <T,>(path: string) => request<T>(path, { method: "DELETE" });
export const patch = <T,>(path: string, body: unknown) =>
  request<T>(path, { method: "PATCH", body: JSON.stringify(body) });

// ---- app types ----

// ---- error tracking (Flare) ----

// ---- configuration issues ----

export interface IssuesResponse {
  issues: Issue[];
  lastScanAt: string; // "" before the first sweep
}

export interface IssuesSummary {
  critical: number;
  warning: number;
  info: number;
  lastScanAt: string;
}

// ---- federation ----

// What POST /api/federation/peers/{id}/recheck answers: one dial, made now,
// reported as it went. unpaired distinguishes "they refuse to know us" from
// "we couldn't reach them", which is the whole question the operator has.
export interface FedRecheck {
  peer: FedPeer;
  ok: boolean;
  unpaired?: boolean;
  error?: string;
}

// FedInvitePreview decodes a pasted invite (no network, no secret spent) so the
// joiner sees who and where before pairing.
export interface FedInvitePreview {
  name: string;
  url: string;
}

// FedPeerMirror is the impersonation view: what one peer receives from us,
// stated in the peer's own voice.
export interface FedPeerMirror {
  as: string;
  from: string;
  url?: string;
  capabilities: { capability: string; label: string; granted: boolean }[];
  directory: { app: string; title: string; url: string }[];
  sentence: string;
}

interface ErrorEventSummary {
  id: number;
  groupId: number;
  eventId: string;
  receivedAt: string;
  release: string;
  environment: string;
  transaction: string;
}

export interface ErrorGroupDetail {
  group: ErrorGroup;
  events: ErrorEventSummary[];
  // For peer groups only: the release THIS instance runs of the same
  // catalog app, when it runs one — the "is their fix already shipped
  // here?" comparison.
  localRelease?: string;
}

// The stored Sentry-schema payload, as far as the stacktrace viewer reads it.
export interface SentryFrame {
  filename?: string;
  abs_path?: string;
  function?: string;
  lineno?: number;
  in_app?: boolean;
  context_line?: string;
  pre_context?: string[];
  post_context?: string[];
}

export interface SentryException {
  type?: string;
  value?: string;
  mechanism?: { type?: string; handled?: boolean };
  stacktrace?: { frames?: SentryFrame[] };
}

interface ErrorEventPayload {
  event_id?: string;
  level?: string;
  transaction?: string;
  release?: string;
  environment?: string;
  server_name?: string;
  message?: string | { formatted?: string };
  exception?: { values?: SentryException[] } | SentryException[];
  request?: { url?: string; method?: string; [k: string]: unknown };
  [k: string]: unknown;
}

export interface ErrorEventDetail {
  id: number;
  eventId: string;
  receivedAt: string;
  release: string;
  environment: string;
  transaction: string;
  payload: ErrorEventPayload;
}

// The shared Postgres cluster: which major its data directory is, which
// major this fog ships, and the last/live upgrade between the two.
export interface PostgresStatus {
  cluster: {
    major: number;
    targetMajor: number;
    layout: "legacy" | "nested" | "";
    fresh: boolean;
    upgradeAvailable: boolean;
    dataBytes: number;
    error?: string;
    retired: { path: string; sizeBytes: number }[];
  };
  upgrade: {
    running: boolean;
    done: boolean;
    error?: string;
    rolledBack: boolean;
    stage?: string;
    from?: number;
    to?: number;
    lines: string[];
    startedAt?: string;
    finishedAt?: string;
  };
}

export interface UpdateCheckResponse {
  ok: boolean;
  checked: number;
  updatesAvailable: number;
  failed: number;
  results: UpdateCheckResult[];
}

// What POST /api/system/backup-artifacts/inspect reports about an uploaded
// fog-backup/1 artifact: which app produced it, when, from what code, and
// which env values its data was encrypted with.
export interface ArtifactPreview {
  app: string;
  format: string;
  createdAt: string;
  gitSha: string;
  schemaVersion: string;
  components: { name: string; path: string; kind: string; bytes: number; sha256: string }[];
  totalBytes: number;
  // env key -> "sha256:<first 12 hex>". The value itself never travels — this
  // is enough to check a pasted secret against the artifact and nothing more.
  secrets: Record<string, string>;
  // Set when the artifact is bound to a hostname; restoring it elsewhere needs
  // explicit confirmation.
  identityHost: string;
  excluded: string[];
}

export interface AppDetail {
  app: AppRecord;
  spec: AppSpec;
  latestRev: number;
  secretKeys: string[];
  revisions: AppRevision[];
  deploys: DeployRecord[];
  git?: GitInfo;
  federation?: FederationFacts;
  // Imported and not yet reviewed: no deploy touches it until the operator
  // confirms its review (pages/app/ImportReview.tsx).
  reviewPending?: boolean;
}

// GET /api/capacity: the machine, the apps on it, and how each catalog
// entry would fit. The verdicts are computed server-side so every view
// agrees; the UI only puts words to them.
export type FitTone = "ok" | "tight" | "over";

export interface Capacity {
  host: CapacityHost;
  measuredAt: number; // unix seconds of the memory reading; 0 = none yet
  apps: CapacityApp[];
  fits: Record<string, Fit>;
}

export interface NasResponse {
  shares: NasShare[];
  helpers: { nfs: boolean; cifs: boolean };
  /** Why fogd can't mount shares on this host at all (an unprivileged container); empty when it can. */
  mountsBlocked: string;
}

// ---- Settings › Storage › Disks & shares: every filesystem fog puts bytes on ----

export interface DisksResponse {
  disks: Disk[];
  helpers: { nfs: boolean; cifs: boolean };
  /** Why fogd can't mount shares on this host at all (an unprivileged container); empty when it can. */
  mountsBlocked: string;
}

// ---- Files: fog's own root ----

// How much of a peer share a box keeps: everything, previews, or the listing.
export type FileFedDetail = FileFedShare["detail"];

export type FilesConflictPolicy = "" | "rename" | "replace" | "skip";

// A 409 from a Files verb: the entry already at the destination.
export interface FilesConflict {
  path: string;
  existing: FileNode;
}

export interface FilesMoveResult {
  moved: string[];
  skipped?: string[];
  job?: FilesJob;
}

export type FilesJob = Job;

// Where *.<domain> points and what stands between the house and the
// internet. Mirrors dnssync.Status; times are RFC 3339, absent when unset.
export interface ReachStatus {
  target: "lan" | "wan";
  domain?: string;
  lanIp?: string;
  publicIp?: string;
  publicIpError?: string;
  desiredIp?: string;
  recordIp?: string;
  checkedAt?: string;
  confirmedAt?: string;
  lastAction?: "created" | "updated" | "ok";
  lastError?: string;
  failing: boolean;
  nat?: "none" | "nat" | "cgnat" | "unknown";
  gateway?: string;
  portMap: {
    enabled: boolean;
    method?: "natpmp" | "upnp";
    gateway?: string;
    externalIp?: string;
    opened?: number[];
    failed?: Record<string, string>;
    permanent?: boolean;
    expiresAt?: string;
    checkedAt?: string;
    error?: string;
  };
  beacon?: { at: string; ip: string; public: boolean; publicIp?: string };
  // Proof on record was obtained under a different public address.
  beaconStale?: boolean;
  // From-inside self-test: a TLS handshake with fog.<domain> at the public
  // address. "ok" proves the forward and the router's NAT loopback.
  hairpin?: "ok" | "failed";
  hairpinError?: string;
  hairpinCheckedAt?: string;
  // The handshake reached a certificate that isn't fog's: the names on
  // it. Loopback works; another device (usually the router's own admin
  // page) holds 443.
  hairpinAnswerer?: string;
}

export interface ReachResponse {
  status: ReachStatus;
  beaconUrl?: string;
}

export interface DependenciesStatus {
  docker: { available: boolean; imagesBytes?: number; reclaimableBytes?: number };
  postgres: ContainerFactsT;
  proxy: ContainerFactsT;
}

// ---- fleet views (Settings › Storage › Backups, Settings › Server › Updates) ----

// ---- feedback (the header's bug button; Inbox › Feedback) ----
