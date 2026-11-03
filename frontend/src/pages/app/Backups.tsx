// The backups half of the Drive tab: one chronological list of every
// snapshot fog knows about — taken here or held only by a locker — with
// three doors above it: take one, restore one, and the schedule that makes
// them.

import { useRef, useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  del, post, postForm, put,
  type AppDetail as AppDetailT, type BackupRecord, type OffsiteJob, type OffsiteJobStarted, type OffsiteRow,
} from "../../api";
import { LoadError, Loading, Spinner, errorText, phaseOf } from "../../components/Async";
import Button from "../../components/Button";
import Field, { control } from "../../components/Field";
import Modal from "../../components/Modal";
import Toggle from "../../components/Toggle";
import { fleetSnapshotsBeforeUpdate } from "../../lib/backupPolicy";
import { formatBytes } from "../../lib/format";
import { backupsKey, offsiteRowsKey, useAppBackups, useAppOffsite } from "../../lib/useAppData";
import { appsKey } from "../../lib/useApps";
import { useFlash } from "../../lib/useFlash";
import { useJob } from "../../lib/useJob";
import { settingsQuery } from "../../queries";
import { IconButton, glyph } from "./Actions";
import SettingsLink from "../../components/SettingsLink";
import { Group, Head, Row, Table, whenShort } from "./cockpit";

// One restore point, whoever holds it. The local row and the lockers'
// copies of the same snapshot share a stamp, so they fold into one line;
// a snapshot only a locker holds is a line too, fetchable, and once it is
// fetched it is the same line with a local row behind it.
interface Point {
  key: string;
  createdAt: string;
  kind: string;
  sizeBytes: number;
  verified: boolean;
  // The lockers holding a copy.
  targets: string[];
  // The copy on this server; absent when only a locker has it.
  backup?: BackupRecord;
  // Where to fetch it from when it is not here.
  remote?: OffsiteRow;
}

// How many restore points the list shows before "show all": the recent past
// is what an operator reaches for; the rest is history.
const RECENT = 5;

const stampOf = (path: string) => path.split("/").filter(Boolean).pop() ?? "";

function foldPoints(backups: BackupRecord[], rows: OffsiteRow[]): Point[] {
  const byStamp = new Map<string, OffsiteRow[]>();
  for (const r of rows) byStamp.set(r.stamp, [...(byStamp.get(r.stamp) ?? []), r]);
  const out: Point[] = backups.map((b) => {
    const stamp = stampOf(b.path);
    const cloud = byStamp.get(stamp) ?? [];
    byStamp.delete(stamp);
    return {
      key: stamp || `b:${b.id}`,
      createdAt: b.createdAt,
      kind: b.kind,
      sizeBytes: b.sizeBytes,
      verified: Boolean(b.verifiedAt) || cloud.some((r) => r.verifiedAt),
      targets: cloud.map((r) => r.target),
      backup: b,
    };
  });
  for (const [stamp, cloud] of byStamp) {
    const remote = cloud.find((r) => !r.local);
    if (!remote) continue;
    out.push({
      key: stamp,
      createdAt: remote.createdAt,
      kind: remote.kind,
      sizeBytes: remote.sizeBytes,
      verified: cloud.some((r) => r.verifiedAt),
      targets: cloud.map((r) => r.target),
      remote,
    });
  }
  return out.sort((a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime());
}

// Fetching a snapshot back from a locker: a job fog runs, polled until it
// settles, after which the lists re-read so the row shows up as local.
function useFetchBack(name: string) {
  const qc = useQueryClient();
  const [jobId, setJobId] = useState<string | null>(null);
  const [error, setError] = useState("");
  const start = useMutation({
    mutationFn: (r: OffsiteRow) => post<OffsiteJobStarted>("/api/offsite/fetch", { target: r.target, app: r.app, stamp: r.stamp }),
    onSuccess: (res) => { setError(""); setJobId(res.job.id); },
    onError: (e) => setError(errorText(e, "Fetch failed to start")),
  });
  const status = useJob<OffsiteJob>("offsite", jobId, {
    interval: 2000,
    onDone: () => {
      qc.invalidateQueries({ queryKey: backupsKey(name) });
      qc.invalidateQueries({ queryKey: offsiteRowsKey(name) });
    },
  });
  const job = status.data;
  const running = start.isPending || job?.state === "running";
  const note: ReactNode = error
    ? <span className="text-warn">{error}</span>
    : jobId && job
      ? job.state === "running" ? <span className="text-accent">Fetching {job.current || "…"}</span>
        : job.state === "done" ? <span className="text-accent">Fetched — it is on this server now.</span>
        : <span className="text-warn">{job.error || job.state}</span>
      : null;
  return { start: (r: OffsiteRow) => start.mutate(r), running, note };
}

type FetchBack = ReturnType<typeof useFetchBack>;

// restorePending is undefined until its data lands — the banner it drives
// says nothing until then rather than guessing.
export function Backups({ name, detail, restorePending, onSettings }: {
  name: string; detail: AppDetailT; restorePending?: boolean; onSettings: () => void;
}) {
  const qc = useQueryClient();
  const delegated = detail.spec.backup?.mode === "delegated";
  const [error, setError] = useState("");
  const [selected, setSelected] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  // The restore door and the point it opens on: the toolbar opens it on the
  // selection, a row's cloud on that row's snapshot.
  const [restoreOn, setRestoreOn] = useState<{ initial: string | null } | null>(null);
  const [showAll, setShowAll] = useState(false);
  const backups = useAppBackups(name);
  const offsite = useAppOffsite(name);
  const fetchBack = useFetchBack(name);
  const create = useMutation({
    mutationFn: () => post(`/api/apps/${name}/backups`),
    // Awaited: the spinner gives way only once the new row is on screen.
    onSuccess: () => {
      setError("");
      return qc.invalidateQueries({ queryKey: backupsKey(name) });
    },
    onError: (e) => setError(errorText(e, "Backup failed")),
  });
  const remove = useMutation({
    mutationFn: (id: number) => del(`/api/backups/${id}`),
    onSuccess: () => {
      setConfirmDelete(false);
      setSelected(null);
      setError("");
      return qc.invalidateQueries({ queryKey: backupsKey(name) });
    },
    onError: (e) => setError(errorText(e, "Delete failed")),
  });
  // Calling off a staged restore: fog disarms the marker and starts the app
  // back up. The banner is driven by the app status poll, so let it re-read
  // rather than asserting here that the restore is gone.
  const cancelRestore = useMutation({
    mutationFn: () => post(`/api/apps/${name}/restore/cancel`),
    onSuccess: () => { setError(""); return qc.invalidateQueries({ queryKey: appsKey }); },
    onError: (e) => setError(errorText(e, "Couldn't cancel the restore")),
  });

  const phase = phaseOf(backups, backups.data?.length);
  const rows = offsite.data?.rows ?? [];
  const points = backups.data ? foldPoints(backups.data, rows) : [];
  // The list is the recent past; the rest is one click away. Folding it back
  // up drops a selection that is no longer on screen.
  const shown = showAll ? points : points.slice(0, RECENT);
  const sel = points.find((p) => p.key === selected);
  const cols = "minmax(0,1fr) auto 64px 36px";
  const canRestore = points.length > 0 || delegated;

  return (
    <Group
      title="Backups"
      first
      right={
        <span className="inline-flex divide-x divide-hair rounded-md border border-edge">
          <IconButton label="Back up now" title={create.isPending ? "Snapshotting…" : "Back up now"} onClick={() => create.mutate()} disabled={create.isPending} edge="start">
            {create.isPending ? <Spinner size="h-3.5 w-3.5" tone="border-fog-300" /> : <BackupGlyph />}
          </IconButton>
          <IconButton label="Restore" title={canRestore ? "Restore…" : "Nothing to restore yet"} onClick={() => setRestoreOn({ initial: selected })} disabled={!canRestore}>
            <RestoreGlyph />
          </IconButton>
          <IconButton label="Settings" title="Schedule" onClick={onSettings} align="end" edge="end">
            <SettingsGlyph />
          </IconButton>
        </span>
      }
    >
      {restorePending && (
        <div className="mb-3 rounded-lg border border-warn/30 bg-warn/10 px-4 py-3">
          <p className="text-cell text-warn">
            A restore is staged in {name}'s volumes. It's either still running, or the app can't consume it — in which
            case it retries on every boot and never comes up. Cancelling disarms it and starts {name} on the data it has now.
          </p>
          <Button size="sm" variant="danger" className="mt-2.5" onClick={() => cancelRestore.mutate()} disabled={cancelRestore.isPending}>
            {cancelRestore.isPending ? "Cancelling…" : "Cancel restore"}
          </Button>
        </div>
      )}
      {error && <p className="mb-2 text-hint text-warn">{error}</p>}
      {create.isPending && <p className="mb-2 text-hint text-accent">Snapshotting…</p>}
      {phase === "loading" && <Loading label="Loading backups…" />}
      {phase === "error" && <LoadError label="Couldn't load this app's backups." error={backups.error} onRetry={() => backups.refetch()} />}
      {phase !== "loading" && phase !== "error" && points.length === 0 && (
        <p className="text-hint text-ink-muted">No backups yet — the first snapshot starts the list.</p>
      )}
      {points.length > 0 && (
        <Table>
          <Head cols={cols}><span>Taken</span><span>Kind</span><span className="text-right">Size</span><span /></Head>
          {shown.map((p) => {
            const on = selected === p.key;
            return (
              <Row key={p.key} cols={cols} tone={on ? "sel" : undefined} onClick={() => { setSelected(on ? null : p.key); setConfirmDelete(false); }}>
                <span className={p.backup ? "text-ink" : "text-ink-muted"}>
                  {whenShort(p.createdAt)}
                </span>
                <span className="text-ink-muted">{p.kind}</span>
                {/* What the snapshot takes on this server's disk. A copy only a
                    locker holds takes none here, so its size reads dimmed. */}
                <span className={`text-right tabular-nums ${p.backup ? "text-ink-muted" : "text-ink-faint"}`}
                  title={p.backup ? "On this server's disk" : `Not on this server — only in ${p.targets.join(", ")}`}>
                  {formatBytes(p.sizeBytes)}
                </span>
                <span className="inline-flex items-center justify-end gap-1.5">
                  {p.verified && <VerifiedMark point={p} />}
                  {p.targets.length > 0 && <CloudMark point={p} onRestore={() => setRestoreOn({ initial: p.key })} />}
                </span>
              </Row>
            );
          })}
        </Table>
      )}
      {points.length > RECENT && (
        <button
          type="button"
          className="mt-2.5 text-hint text-accent hover:text-accent-hover"
          onClick={() => {
            if (showAll && selected && !points.slice(0, RECENT).some((p) => p.key === selected)) setSelected(null);
            setShowAll(!showAll);
          }}
        >
          {showAll ? "Show fewer" : `Show all ${points.length} backups`}
        </button>
      )}

      {sel && (
        <div className="mt-3 flex flex-wrap items-center gap-2 rounded-lg border border-veil-edge bg-veil px-4 py-2.5">
          <span className="mr-auto text-hint text-ink-muted">
            {whenShort(sel.createdAt)} · {formatBytes(sel.sizeBytes)} · {sel.kind}
            {sel.backup ? "" : ` · only in ${sel.targets.join(", ")}`}
          </span>
          {sel.backup ? (
            <>
              <a href={`/api/backups/${sel.backup.id}/download`}
                title={sel.backup.format ? "Single verified artifact (.fogpack.tar) — restorable here or by upload" : "Snapshot bundle (.snapshot.tar): volume tarballs + database dumps — for off-box safekeeping"}
                className="inline-flex items-center rounded-md px-2.5 py-1 text-xs font-medium text-ink-muted hover:bg-hover hover:text-ink">
                Download
              </a>
              {confirmDelete ? (
                <>
                  <Button size="sm" variant="danger" onClick={() => remove.mutate(sel.backup!.id)} disabled={remove.isPending}>{remove.isPending ? "Deleting…" : "Confirm delete"}</Button>
                  <Button size="sm" variant="ghost" onClick={() => setConfirmDelete(false)}>Keep it</Button>
                </>
              ) : (
                <Button size="sm" variant="ghost" onClick={() => setConfirmDelete(true)}>Delete</Button>
              )}
            </>
          ) : (
            <Button size="sm" onClick={() => fetchBack.start(sel.remote!)} disabled={fetchBack.running}>
              {fetchBack.running ? "Fetching…" : "Fetch to this server"}
            </Button>
          )}
        </div>
      )}
      {fetchBack.note && <p className="mt-2 text-hint">{fetchBack.note}</p>}

      {restoreOn && (
        <RestoreModal name={name} detail={detail} points={points} initial={restoreOn.initial} fetchBack={fetchBack} onClose={() => setRestoreOn(null)} />
      )}
    </Group>
  );
}

// The restore door. Pick a restore point — the one selected in the list to
// begin with — and confirm; or, for apps that speak fog-backup/1, hand over
// an artifact file and let the app restore it on boot.
function RestoreModal({ name, detail, points, initial, fetchBack, onClose }: {
  name: string; detail: AppDetailT; points: Point[]; initial: string | null; fetchBack: FetchBack; onClose: () => void;
}) {
  const qc = useQueryClient();
  const delegated = detail.spec.backup?.mode === "delegated";
  const [key, setKey] = useState(initial ?? points[0]?.key ?? "");
  const [confirm, setConfirm] = useState(false);
  const [confirmIdentity, setConfirmIdentity] = useState(false);
  // A server rebuilt without its recovery kit no longer holds the secrets
  // its apps' data was written with. Restoring anyway is a decision: the app
  // takes the data and clears what those secrets encrypted.
  const [forgetSecrets, setForgetSecrets] = useState(false);
  const [error, setError] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);
  const chosen = points.find((p) => p.key === key);
  const restore = useMutation({
    mutationFn: (id: number) => post(`/api/backups/${id}/restore`, forgetSecrets ? { forgetSecrets } : undefined),
    onSuccess: async () => { await qc.invalidateQueries({ queryKey: appsKey }); onClose(); },
    onError: (e) => setError(errorText(e, "Restore failed")),
  });
  // Restore a foreign artifact: upload it, then hand fog the server-side
  // path. Stop → stage → boot; the app's own guard performs the restore.
  const restoreUpload = useMutation({
    mutationFn: async (file: File) => {
      const form = new FormData();
      form.append("artifact", file);
      const { path } = await postForm<{ path: string }>("/api/system/backup-uploads", form);
      return post(`/api/apps/${name}/restore`, { path, confirmIdentity, forgetSecrets });
    },
    onSuccess: async () => { await qc.invalidateQueries({ queryKey: appsKey }); onClose(); },
    onError: (e) => setError(errorText(e, "Restore failed")),
  });
  const busy = restore.isPending || restoreUpload.isPending;
  const selectCls = `${control} w-full`;

  return (
    <Modal title={`Restore ${name}`} onClose={onClose}>
      <div className="space-y-4">
        {points.length > 0 && (
          <label className="block">
            <span className="field-label mb-1.5 block">Restore point</span>
            <select value={key} onChange={(e) => { setKey(e.target.value); setConfirm(false); }} className={selectCls}>
              {points.map((p) => (
                <option key={p.key} value={p.key}>
                  {whenShort(p.createdAt)} · {p.kind} · {formatBytes(p.sizeBytes)}{p.verified ? " · verified" : ""}{p.backup ? "" : ` · in ${p.targets.join(", ")} only`}
                </option>
              ))}
            </select>
            {chosen && (
              <span className="mt-1.5 block text-hint text-ink-muted">
                {chosen.backup
                  ? `Fog stops ${name}, replaces its data with this snapshot, and starts it again. What ${name} holds now is overwritten.`
                  : `This snapshot is only in ${chosen.targets.join(", ")}. Fetch it to this server first; it then restores like any other.`}
              </span>
            )}
          </label>
        )}
        {chosen && !chosen.backup && (
          <div className="flex flex-wrap items-center gap-3">
            <Button size="sm" onClick={() => fetchBack.start(chosen.remote!)} disabled={fetchBack.running}>
              {fetchBack.running ? "Fetching…" : `Fetch from ${chosen.remote!.target}`}
            </Button>
            {fetchBack.note && <span className="text-hint">{fetchBack.note}</span>}
          </div>
        )}
        {delegated && (
          <label className="flex items-start gap-2 text-xs text-ink-muted">
            <input type="checkbox" className="mt-0.5" checked={forgetSecrets} onChange={(e) => setForgetSecrets(e.target.checked)} />
            <span>
              This server no longer has the secrets this backup was written with (rebuilt without its recovery kit) —
              restore anyway. {name} keeps its data and clears what those secrets encrypted: saved passwords, API keys
              and tokens need entering again.
            </span>
          </label>
        )}
        {chosen?.backup && (
          <div className="flex flex-wrap items-center gap-2">
            {confirm ? (
              <>
                <Button size="sm" variant="danger" onClick={() => restore.mutate(chosen.backup!.id)} disabled={busy}>
                  {restore.isPending ? "Restoring…" : "Confirm restore — overwrites current data"}
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setConfirm(false)} disabled={busy}>Cancel</Button>
              </>
            ) : (
              <Button size="sm" variant="danger" onClick={() => setConfirm(true)} disabled={busy}>Restore this point…</Button>
            )}
          </div>
        )}

        {/* Artifact upload is a fog-backup/1 door: the app performs the restore
            itself on boot. It is only offered where it can work. */}
        {delegated && (
          <div className={points.length > 0 ? "border-t border-hair pt-4" : ""}>
            <h3 className="text-cell font-medium text-ink">{points.length > 0 ? "Or restore from an artifact file" : "Restore from an artifact file"}</h3>
            <p className="mt-1 text-hint text-ink-muted">
              Upload a .fogpack.tar this app produced (here or on another server). Fog stops the app, stages the
              artifact, and the app restores it on boot — current data is overwritten.
            </p>
            <div className="mt-3 flex flex-wrap items-center gap-3">
              <input ref={fileRef} type="file" accept=".tar,application/x-tar"
                className="text-sm text-ink-muted file:mr-3 file:rounded file:border file:border-edge file:bg-fog-800 file:px-3 file:py-1.5 file:text-sm file:text-ink" />
              <label className="flex items-center gap-2 text-xs text-ink-muted">
                <input type="checkbox" checked={confirmIdentity} onChange={(e) => setConfirmIdentity(e.target.checked)} />
                artifact came from a different hostname — restore anyway
              </label>
              <Button size="sm" variant="danger" disabled={busy}
                onClick={() => {
                  const file = fileRef.current?.files?.[0];
                  if (!file) { setError("Choose an artifact file first"); return; }
                  restoreUpload.mutate(file);
                }}>
                {restoreUpload.isPending ? "Uploading & restoring…" : "Upload & restore"}
              </Button>
            </div>
          </div>
        )}
        {!delegated && detail.spec.backup?.enabled && (
          <p className="text-hint text-ink-muted">
            Volume snapshots restore from the list; a downloaded .snapshot.tar is for off-box safekeeping, not for uploading back.
          </p>
        )}
        {error && <p className="text-hint text-warn">{error}</p>}
      </div>
    </Modal>
  );
}

// The marks beside a kind: a tick when a test restore has proven the
// snapshot, a cloud when a locker holds a copy. Each says what it means on
// hover — fog's own tooltip, as elsewhere, rather than the browser's title,
// which arrives late and looks like someone else's. The cloud is also a
// door: it opens the restore dialog on that snapshot.
function Mark({ label, lines, onClick, tone, children }: {
  label: string; lines: string[]; onClick?: () => void; tone: string; children: ReactNode;
}) {
  return (
    <span className="group/mark relative inline-flex">
      {onClick ? (
        <button
          type="button"
          aria-label={label}
          onClick={(e) => { e.stopPropagation(); onClick(); }}
          className={`-m-1 inline-flex rounded p-1 transition-colors hover:text-ink ${tone}`}
        >
          {children}
        </button>
      ) : (
        <span role="img" aria-label={label} className={`inline-flex ${tone}`}>{children}</span>
      )}
      <span
        role="tooltip"
        className="pointer-events-none absolute right-0 top-full z-20 mt-1.5 w-max max-w-[16rem] whitespace-normal rounded-md border border-veil-edge bg-fog-900 px-2 py-1.5 text-left text-xs leading-snug opacity-0 shadow-lg shadow-black/40 transition-opacity duration-150 group-hover/mark:opacity-100 group-focus-within/mark:opacity-100"
      >
        {lines.map((line, i) => (
          <span key={i} className={`block ${i === 0 ? "text-ink" : "mt-0.5 text-ink-muted"}`}>{line}</span>
        ))}
      </span>
    </span>
  );
}

function VerifiedMark({ point }: { point: Point }) {
  const at = point.backup?.verifiedAt;
  return (
    <Mark
      label="Test-restore verified"
      tone="text-accent"
      lines={[
        at ? `Test-restore verified ${new Date(at).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" })}` : "Test-restore verified",
        "Fog restored this snapshot on its own and the restore came back clean.",
      ]}
    >
      <svg {...glyph} width={12} height={12} strokeWidth={2.25}><path d="m5 12 4 4L19 6" /></svg>
    </Mark>
  );
}

function CloudMark({ point, onRestore }: { point: Point; onRestore: () => void }) {
  const where = point.targets.join(", ");
  return (
    <Mark
      label={`Restore from the copy in ${where}`}
      tone="text-ink-muted"
      onClick={onRestore}
      lines={[
        `Off this server, in ${where}`,
        point.backup
          ? "A copy is also on this server, so it can restore straight from here."
          : "Only the locker has it — fog fetches it back before it can restore.",
        `${formatBytes(point.sizeBytes)} · ${point.kind} · click to restore from it`,
      ]}
    >
      <svg {...glyph} width={13} height={13}><path d="M17.5 19H9a7 7 0 1 1 6.71-9h1.79a4.5 4.5 0 1 1 0 9Z" /></svg>
    </Mark>
  );
}

const BackupGlyph = () => (
  <svg {...glyph}><path d="M12 2v8" /><path d="m16 6-4 4-4-4" /><rect width="20" height="8" x="2" y="14" rx="2" /><path d="M6 18h.01" /><path d="M10 18h.01" /></svg>
);
const RestoreGlyph = () => (
  <svg {...glyph}><path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8" /><path d="M3 3v5h5" /><path d="M12 7v5l4 2" /></svg>
);
const SettingsGlyph = () => (
  <svg {...glyph}>
    <path d="M21 4h-7M10 4H3M21 12h-9M8 12H3M21 20h-5M12 20H3" />
    <path d="M14 2v4M8 10v4M16 18v4" />
  </svg>
);

// ---- schedule ----

// The spec fields that decide whether nightly and pre-update snapshots
// happen, plus the global defaults they fall back to. Saving writes a new revision without
// deploying — the scheduler reads the latest spec, so the change is live
// the moment it lands.
// An app's beforeUpdate as the form holds it: "" follows the global
// default, "on"/"off" override it.
type BeforeUpdateChoice = "" | "on" | "off";
const beforeUpdateChoice = (v?: boolean): BeforeUpdateChoice => (v === undefined ? "" : v ? "on" : "off");

export function Schedule({ name, detail, onSaved }: { name: string; detail: AppDetailT; onSaved: () => Promise<unknown> }) {
  // Seeded from the spec; re-seeded during render if a save elsewhere moves
  // the revision, so the form never shows a stale spec.
  const seed = () => ({
    enabled: detail.spec.backup?.enabled ?? false,
    beforeUpdate: beforeUpdateChoice(detail.spec.backup?.beforeUpdate),
    hour: detail.spec.backup?.hourUTC == null ? "" : String(detail.spec.backup.hourUTC),
    mode: detail.spec.backup?.mode === "delegated" ? "delegated" : "volumes",
    timeout: detail.spec.backup?.timeout ?? "",
    rev: detail.latestRev,
  });
  const [form, setForm] = useState(seed);
  if (form.rev !== detail.latestRev) setForm(seed());
  const [error, setError] = useState("");
  const [saved, flashSaved] = useFlash();
  const settings = useQuery(settingsQuery);

  const globalRaw = settings.data?.backup_hour_utc ?? "";
  const globalHour = /^\d{1,2}$/.test(globalRaw.trim()) && Number(globalRaw) <= 23 ? Number(globalRaw) : null;
  const hourNum = form.hour.trim() === "" ? null : parseInt(form.hour, 10);
  const hourValid = hourNum === null || (!Number.isNaN(hourNum) && hourNum >= 0 && hourNum <= 23);
  // The exact condition the "backup-unscheduled" issue fires on, shown here
  // before saving so the fix and the problem meet on the same screen.
  const unscheduled = form.enabled && hourNum === null && globalHour === null && !!settings.data;

  const save = useMutation({
    mutationFn: () => {
      const backup = form.enabled || form.mode === "delegated" || form.timeout.trim() || hourNum !== null
        ? {
            enabled: form.enabled,
            beforeUpdate: form.beforeUpdate === "" ? undefined : form.beforeUpdate === "on",
            mode: form.mode === "delegated" ? "delegated" : undefined,
            timeout: form.timeout.trim() || undefined,
            hourUTC: hourNum,
          }
        : null;
      return put(`/api/apps/${name}`, { spec: { ...detail.spec, backup }, secrets: {}, comment: "Backup schedule", deploy: false });
    },
    onSuccess: async () => {
      setError("");
      await onSaved();
      flashSaved("Saved — the scheduler picks this up immediately");
    },
    onError: (e) => setError(errorText(e, "Save failed")),
  });

  const fmtHour = (h: number) => `${String(h).padStart(2, "0")}:00 UTC`;
  const dirty =
    form.enabled !== (detail.spec.backup?.enabled ?? false) ||
    form.beforeUpdate !== beforeUpdateChoice(detail.spec.backup?.beforeUpdate) ||
    form.hour.trim() !== (detail.spec.backup?.hourUTC == null ? "" : String(detail.spec.backup.hourUTC)) ||
    form.mode !== (detail.spec.backup?.mode === "delegated" ? "delegated" : "volumes") ||
    form.timeout.trim() !== (detail.spec.backup?.timeout ?? "");
  // Delegated backups are an HTTP conversation — without a health check or
  // route the server refuses the save, so say why before the round trip.
  const unreachable = form.mode === "delegated" && !detail.spec.health && !(detail.spec.routes?.length);

  return (
    <div className="space-y-4">
      <div><Toggle checked={form.enabled} onChange={(on) => setForm((f) => ({ ...f, enabled: on }))} label={`Include ${name} in nightly backups`} className="text-cell text-ink" /></div>
      {form.enabled && (
        <>
          <label className="block">
            <span className="field-label mb-1.5 block">Snapshot before updates</span>
            <select value={form.beforeUpdate}
              onChange={(e) => setForm((f) => ({ ...f, beforeUpdate: e.target.value as BeforeUpdateChoice }))}
              className={`${control} w-full`}>
              <option value="">Global default ({fleetSnapshotsBeforeUpdate(settings.data) ? "snapshot" : "don't snapshot"})</option>
              <option value="on">Always — snapshot before every update and rollback</option>
              <option value="off">Never — update and roll back without a snapshot</option>
            </select>
            <span className="mt-1.5 block text-hint text-ink-muted">
              The global default is set in{" "}
              <SettingsLink to="storage/backups" anchor="policy" />.
              One update can still skip its snapshot from the Update dialog.
            </span>
          </label>
          <Field label="Backup hour (UTC)" type="number" min={0} max={23} value={form.hour}
            onChange={(e) => setForm((f) => ({ ...f, hour: e.target.value }))}
            placeholder={globalHour !== null ? `empty = global default (${fmtHour(globalHour)})` : "empty = global default (not set)"}
            aria-invalid={!hourValid}
            hint={
              !hourValid ? "The backup hour must be between 0 and 23."
              : hourNum !== null ? `Snapshots run daily at ${fmtHour(hourNum)}, overriding the global default.`
              : globalHour !== null ? `Following the global default: daily at ${fmtHour(globalHour)}.`
              : "No hour set here or in the global settings."
            } />
          {unscheduled && (
            <div className="rounded-lg border border-warn/30 bg-warn/10 p-4">
              <h3 className="text-cell font-medium text-warn">Enabled, but no snapshots will run</h3>
              <p className="mt-1 text-hint text-warn">
                Neither this app nor the global settings define a backup hour, so the nightly scheduler skips {name}.
                Set an hour above, or set a default for every app in{" "}
                <SettingsLink to="storage/backups" anchor="policy" />.
              </p>
            </div>
          )}
          <label className="block">
            <span className="field-label mb-1.5 block">Mode</span>
            <select value={form.mode}
              onChange={(e) => setForm((f) => ({ ...f, mode: e.target.value as "volumes" | "delegated" }))}
              className={`${control} w-full`}>
              <option value="volumes">Volume snapshots — fog tars volumes and dumps databases</option>
              <option value="delegated">Delegated (fog-backup/1) — the app produces a verified artifact</option>
            </select>
            <span className="mt-1.5 block text-hint text-ink-muted">
              Delegated backups get periodic test restores, but only work when the app implements the fog-backup/1
              contract — fog's own apps do; most third-party images don't. Volume snapshots work for any app.
            </span>
          </label>
          {form.mode === "delegated" && (
            <Field label="Backup timeout" value={form.timeout}
              onChange={(e) => setForm((f) => ({ ...f, timeout: e.target.value }))}
              placeholder="30m (default)"
              hint="Bounds one delegated backup end to end, as a Go duration — e.g. 45m." />
          )}
          {unreachable && (
            <p className="text-hint text-warn">
              Delegated backups are an HTTP conversation with the app, so {name} needs a health check or a route before
              this can be saved — add one on Config.
            </p>
          )}
        </>
      )}
      {error && <p className="text-hint text-warn">{error}</p>}
      {saved && <p className="text-hint font-medium text-accent">✓ {saved}</p>}
      {dirty && (
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => setForm(seed())} disabled={save.isPending}>Cancel</Button>
          <Button variant="primary" onClick={() => save.mutate()} disabled={save.isPending || !hourValid || unreachable}>
            {save.isPending ? "Saving…" : "Save schedule"}
          </Button>
        </div>
      )}
    </div>
  );
}
