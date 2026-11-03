// Settings › Storage › Disks & shares: where the bytes are. Every
// filesystem fog puts bytes on — the system disk and every NAS share beside
// it — each with how full it is and what depends on it.

import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  del, post, put,
  type Disk, type DiskSegment, type NasShare,
} from "../../api";
import Button from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import Field, { Select } from "../../components/Field";
import { RowList } from "../../components/SettingRow";
import { SettingsPage } from "./Frame";
import { LoadError, Loading, errorText, phaseOf } from "../../components/Async";
import { formatBytes } from "../../lib/format";
import { paths } from "../../lib/paths";
import { disksQuery, nasQuery } from "../../queries";

// Every filesystem fog puts bytes on, as one list: the system disk first,
// then each network share, then anything else a volume points at. The same
// reading for each — used of total, free, and what depends on it — so a
// NAS is a disk like any other, and the "+" at the head of the list is how
// another one arrives. Shares are mounted on the host by fogd, not fstab:
// enabled ones come back when the server restarts fogd.
const EMPTY_SHARE = {
  name: "", proto: "nfs" as "nfs" | "cifs", server: "", remote: "",
  mountPoint: "", options: "", username: "", password: "",
};

export default function Disks() {
  const [ask, dialog] = useConfirm();
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  // Name of the share being edited; the add form doubles as the edit form.
  // Editing exists so a share can follow its NAS to a new address without
  // delete-and-recreate (which would also drop the stored password).
  const [editing, setEditing] = useState<string | null>(null);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [form, setForm] = useState(EMPTY_SHARE);
  const [error, setError] = useState("");

  const disks = useQuery({
    ...disksQuery,
    refetchInterval: 10_000,
  });
  // The install pane and the path picker read shares through /api/nas, so
  // a change here refreshes both readings.
  const refresh = () =>
    Promise.all([qc.invalidateQueries({ queryKey: disksQuery.queryKey }), qc.invalidateQueries({ queryKey: nasQuery.queryKey })]);
  const closeForm = () => {
    setForm(EMPTY_SHARE);
    setAdding(false);
    setEditing(null);
  };

  const add = useMutation({
    mutationFn: () => post<{ ok: boolean; error?: string }>("/api/nas", form),
    onSuccess: async (res) => {
      await refresh();
      closeForm();
      // Stored but the mount failed — the error sits by the list, where
      // the row's Mount button retries.
      setError(res.ok ? "" : (res.error ?? "Mount failed — the share was saved; retry from the list above."));
    },
    onError: (e) => setError(errorText(e, "Adding the share failed")),
  });
  // Mount state is the whole point of this list, so every action waits for
  // the refreshed truth before its button stops saying "Mounting…".
  const mount = useMutation({
    mutationFn: (name: string) => post(`/api/nas/${name}/mount`),
    onSuccess: () => { setError(""); return refresh(); },
    onError: (e) => { setError(errorText(e, "Mount failed")); return refresh(); },
  });
  const unmount = useMutation({
    mutationFn: (name: string) => post(`/api/nas/${name}/unmount`),
    onSuccess: () => { setError(""); return refresh(); },
    onError: (e) => { setError(errorText(e, "Unmount failed")); return refresh(); },
  });
  const update = useMutation({
    mutationFn: (name: string) => put<{ ok: boolean; error?: string }>(`/api/nas/${name}`, form),
    onSuccess: async (res) => {
      await refresh();
      closeForm();
      setError(res.ok ? "" : (res.error ?? "Mount failed — the change was saved; retry from the list above."));
    },
    onError: (e) => setError(errorText(e, "Saving the share failed")),
  });
  const remove = useMutation({
    mutationFn: (v: { name: string; force?: boolean }) => del(`/api/nas/${v.name}${v.force ? "?force=1" : ""}`),
    onSuccess: () => { setError(""); return refresh(); },
    onError: (e, v) => {
      // An unreachable server fails the unmount that guards deletion. The
      // share can still go — after an explicit ok to detach the dead mount.
      if (!v.force) {
        void ask({
          title: `Force-remove share ${v.name}?`,
          body: <><p className="text-warn">{errorText(e, "Remove failed")}</p><p>Force-detach the mount and remove the share anyway? Nothing on the NAS is touched.</p></>,
          action: "Force-remove",
          danger: true,
        }).then((ok) => {
          if (ok) remove.mutate({ name: v.name, force: true });
          else {
            setError(errorText(e, "Remove failed"));
            void refresh();
          }
        });
        return;
      }
      setError(errorText(e, "Remove failed"));
      return refresh();
    },
  });
  const startAdd = () => {
    setForm(EMPTY_SHARE);
    setEditing(null);
    setAdding(true);
    setShowAdvanced(false);
    setError("");
  };
  const startEdit = (sh: NasShare) => {
    setForm({
      name: sh.name, proto: sh.proto, server: sh.server, remote: sh.remote,
      mountPoint: sh.mountPoint, options: sh.options ?? "", username: sh.username ?? "", password: "",
    });
    setEditing(sh.name);
    setAdding(false);
    setShowAdvanced(Boolean(sh.options));
    setError("");
  };
  const confirmRemove = async (d: Disk) => {
    const depends = [
      d.uses.drive && "the Drive",
      d.uses.backups && "the backup directory",
      ...d.uses.apps,
    ].filter(Boolean);
    const ok = await ask({
      title: `Remove share ${d.name}?`,
      body: (
        <>
          <p>It is unmounted first; nothing on the NAS is touched.</p>
          {depends.length > 0 && (
            <p className="text-warn">{depends.join(", ")} point{depends.length === 1 ? "s" : ""} at this share and will lose the path.</p>
          )}
        </>
      ),
      action: "Remove share",
      danger: true,
    });
    if (ok) {
      remove.mutate({ name: d.name });
    }
  };

  const list = disks.data?.disks ?? [];
  const phase = phaseOf(disks, list.length);
  const set = (k: keyof typeof EMPTY_SHARE) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm({ ...form, [k]: e.target.value });
  const helperMissing = disks.data && !disks.data.helpers[form.proto];
  const formOpen = adding || editing !== null;

  return (
    <SettingsPage
      description="Every filesystem fog writes to, and what each one holds. Network shares are mounted by fogd and come back whenever it restarts."
      actions={!formOpen && <Button onClick={startAdd}>Add network share</Button>}
    >
      {phase === "loading" && <Loading label="Reading the disks…" />}
      {phase === "error" && (
        <LoadError label="Couldn't read the disks." error={disks.error} onRetry={() => disks.refetch()} />
      )}
      {list.length > 0 && (
        <RowList>
          {list.map((d) => (
            <DiskRow
              key={d.id}
              disk={d}
              busy={
                mount.isPending && mount.variables === d.name ? "Mounting…"
                : unmount.isPending && unmount.variables === d.name ? "Unmounting…"
                : remove.isPending && remove.variables?.name === d.name ? "Removing…"
                : undefined
              }
              disabled={mount.isPending || unmount.isPending || remove.isPending}
              onEdit={d.share ? () => startEdit(d.share!) : undefined}
              onMount={() => mount.mutate(d.name)}
              onUnmount={() => unmount.mutate(d.name)}
              onRemove={() => void confirmRemove(d)}
            />
          ))}
        </RowList>
      )}

      {formOpen && (
        <div className="space-y-4 rounded-lg border border-veil-edge bg-veil p-4">
          {editing ? (
            <p className="text-xs text-ink-muted">
              Editing <span className="font-medium text-ink">{editing}</span> — the stored password is kept
              unless you type a new one. Saving remounts the share at the new address.
            </p>
          ) : (
            <p className="text-xs text-ink-muted">
              A new network share, mounted on this server by fogd and remounted whenever it restarts.
            </p>
          )}
          <div className="grid grid-cols-2 gap-4">
            <Select label="Protocol" value={form.proto} onChange={(e) => setForm({ ...form, proto: e.target.value as "nfs" | "cifs" })}>
              <option value="nfs">NFS</option>
              <option value="cifs">SMB / CIFS</option>
            </Select>
            <Field label="Name" value={form.name} onChange={set("name")} placeholder="media" disabled={Boolean(editing)}
              hint={`Mounts at ${form.mountPoint || `/mnt/nas/${form.name || "<name>"}`}`} />
          </div>
          <div className="grid grid-cols-2 gap-4">
            <Field label="Server" value={form.server} onChange={set("server")} placeholder="nas.lan or 192.168.1.50" />
            <Field label={form.proto === "nfs" ? "Export path" : "Share name"} value={form.remote} onChange={set("remote")}
              placeholder={form.proto === "nfs" ? "/volume1/media" : "media"} />
          </div>
          {form.proto === "cifs" && (
            <div className="grid grid-cols-2 gap-4">
              <Field label="Username" value={form.username} onChange={set("username")} placeholder="leave empty for guest access" />
              <Field label="Password" type="password" value={form.password} onChange={set("password")}
                placeholder="stored encrypted" />
            </div>
          )}
          <div>
            <Button variant="ghost" size="sm" onClick={() => setShowAdvanced(!showAdvanced)} aria-expanded={showAdvanced}>
              {showAdvanced ? "Hide advanced" : "Advanced"}
            </Button>
          </div>
          {showAdvanced && (
            <div className="grid grid-cols-2 gap-4">
              <Field label="Mount point" value={form.mountPoint} onChange={set("mountPoint")}
                placeholder={`/mnt/nas/${form.name || "<name>"}`} />
              <Field label="Mount options" value={form.options} onChange={set("options")}
                placeholder={form.proto === "nfs" ? "e.g. ro,soft" : "e.g. vers=3.0,ro"} />
            </div>
          )}
          {disks.data?.mountsBlocked && (
            <p className="text-xs text-warn">{disks.data.mountsBlocked}</p>
          )}
          {helperMissing && !disks.data?.mountsBlocked && (
            <p className="text-xs text-ink-muted">
              The {form.proto === "cifs" ? "cifs-utils" : "nfs-common"} package isn't installed yet — fog installs
              it automatically on first mount, so this one takes a little longer.
            </p>
          )}
          <div className="flex items-center gap-3">
            {editing ? (
              <Button variant="primary" onClick={() => update.mutate(editing)} disabled={update.isPending || !form.server || !form.remote}>
                {update.isPending ? "Saving…" : "Save & mount"}
              </Button>
            ) : (
              <Button variant="primary" onClick={() => add.mutate()} disabled={add.isPending || !form.name || !form.server || !form.remote}>
                {add.isPending ? "Mounting…" : "Add & mount"}
              </Button>
            )}
            <Button onClick={() => { closeForm(); setError(""); }}>Cancel</Button>
          </div>
        </div>
      )}

      {error && <p className="break-words text-sm text-warn">{error}</p>}
      {dialog}
    </SettingsPage>
  );
}

// One disk: what it is, how full it is, and what depends on it. A share
// also carries its own actions — mount, unmount, edit, remove — because
// the row is where its state is read, so the row is where it's changed.
function DiskRow({ disk: d, busy, disabled, onEdit, onMount, onUnmount, onRemove }: {
  disk: Disk;
  // The action in flight on this row, as its button's label.
  busy?: string;
  disabled: boolean;
  onEdit?: () => void;
  onMount: () => void;
  onUnmount: () => void;
  onRemove: () => void;
}) {
  const total = d.totalBytes ?? 0;
  const used = d.usedBytes ?? 0;
  const free = d.freeBytes ?? 0;
  const known = total > 0;
  const low = known && free < total * 0.1;
  const pct = known ? Math.min(100, Math.round((used / total) * 100)) : 0;
  const sh = d.share;
  const kind = d.kind === "local" ? "Local" : d.kind === "other" ? "Mounted" : sh?.proto === "cifs" ? "SMB" : "NFS";
  const remote = sh ? (sh.proto === "cifs" ? `//${sh.server}/${sh.remote}` : `${sh.server}:${sh.remote}`) : "";
  const uses = d.uses;
  const nothing = !uses.fog && !uses.drive && !uses.backups && uses.apps.length === 0;

  return (
    <li>
      <div className="space-y-2.5 px-4 py-3">
      <div className="flex items-center gap-3">
        <div className="min-w-0 flex-1">
          <p className="text-sm font-medium text-ink">
            {d.kind === "local" ? "System disk" : d.name}
            <span className="ml-2 rounded-full border border-hair px-2 py-px text-[11px] font-medium text-ink-muted">{kind}</span>
          </p>
          <p className="truncate font-mono text-xs text-ink-muted">
            {sh ? `${remote} → ${d.mountPoint}` : d.mountPoint}
          </p>
        </div>
        {sh && (
          <span className={`shrink-0 text-xs font-medium ${d.stale ? "text-warn" : d.mounted ? "text-accent" : "text-ink-muted"}`}>
            {d.stale ? "⚠ not responding" : d.mounted ? "✓ mounted" : "○ not mounted"}
          </span>
        )}
        {sh && (
          <>
            <Button size="sm" onClick={onEdit} disabled={disabled}>Edit</Button>
            {d.mounted ? (
              <Button size="sm" onClick={onUnmount} disabled={disabled}>{busy === "Unmounting…" ? busy : "Unmount"}</Button>
            ) : (
              <Button size="sm" onClick={onMount} disabled={disabled}>{busy === "Mounting…" ? busy : "Mount"}</Button>
            )}
            <Button size="sm" variant="danger" onClick={onRemove} disabled={disabled}>{busy === "Removing…" ? busy : "Remove"}</Button>
          </>
        )}
      </div>

      {known ? (
        <div className="space-y-1.5">
          <div className="flex items-baseline justify-between text-sm tabular-nums">
            <span className="text-ink">{formatBytes(used)} used of {formatBytes(total)}</span>
            <span className={low ? "font-medium text-warn" : "text-ink-muted"}>{low ? "⚠ " : ""}{formatBytes(free)} free</span>
          </div>
          <UsageBar total={total} used={used} pct={pct} low={low} segments={d.usage?.segments} />
        </div>
      ) : (
        // No figures is no reading — never an empty bar pretending to be one.
        <p className="text-xs text-ink-muted">
          {d.stale ? "No reading — the share isn't answering." : !d.mounted ? "No reading until it is mounted." : "No reading yet."}
        </p>
      )}

      <div className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs">
        {nothing ? (
          <span className="text-ink-muted">Nothing on fog uses this disk yet.</span>
        ) : (
          <>
            <span className="mr-0.5 text-ink-muted">Holds</span>
            {uses.fog && <UsePill tone="fog" title="Fog's data directory: its database, builds, and every app's own data">Fog data</UsePill>}
            {uses.drive && <UsePill tone="fog" to={paths.settings("storage/drive")} title="The Drive's files root is on this disk">Drive</UsePill>}
            {uses.backups && <UsePill tone="fog" to={paths.settings("storage/backups")} title="Local backups are written to this disk">Backups</UsePill>}
            {uses.apps.map((a) => (
              <UsePill key={a} tone="app" to={paths.app(a)} title={`${a} has a volume on this disk`}>{a}</UsePill>
            ))}
          </>
        )}
      </div>
      {d.lastError && (!d.mounted || d.stale) && (
        <p className="break-words font-mono text-xs text-warn">{d.lastError}</p>
      )}
      </div>
    </li>
  );
}

// One fixed colour per category — the same in every row and every theme,
// so "Apps" is the same blue on the NAS as on the system disk.
const SEGMENT_FILL: Record<DiskSegment["key"], string> = {
  apps: "bg-seg-apps",
  drive: "bg-seg-drive",
  backups: "bg-seg-backups",
  postgres: "bg-seg-postgres",
  fog: "bg-seg-fog",
  docker: "bg-seg-docker",
  other: "bg-seg-other",
};

// The legend line for one segment. Docker is the one category that can
// say how much of itself is dead weight — images no container runs,
// build cache nothing uses — so its entry carries that figure.
function segmentTitle(seg: DiskSegment): string {
  const base = `${seg.label} · ${formatBytes(seg.bytes)}`;
  return seg.reclaimable ? `${base} · ${formatBytes(seg.reclaimable)} reclaimable` : base;
}

// The used space as a stacked bar with a legend under it. Without a
// sizing pass yet there is nothing to split, so the bar is the plain used
// fill — a reading, not a guess — and the legend waits. Low space keeps
// its amber, on the whole fill: the warning is about the disk, not a
// category.
function UsageBar({ total, used, pct, low, segments }: {
  total: number;
  used: number;
  pct: number;
  low: boolean;
  segments?: DiskSegment[];
}) {
  const split = !low && segments && segments.length > 0;
  return (
    <div className="space-y-1.5">
      <div
        className="flex h-2 gap-px overflow-hidden rounded-full bg-fog-800"
        role="progressbar"
        aria-valuenow={pct}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={`${formatBytes(used)} used of ${formatBytes(total)}`}
      >
        {split ? (
          segments.map((seg) => (
            <div
              key={seg.key}
              title={segmentTitle(seg)}
              className={`h-full ${SEGMENT_FILL[seg.key]}`}
              style={{ width: `${(seg.bytes / total) * 100}%` }}
            />
          ))
        ) : (
          <div className={`h-full ${low ? "bg-warn" : "bg-accent"}`} style={{ width: `${pct}%` }} />
        )}
      </div>
      {segments && segments.length > 0 && (
        <ul className="flex flex-wrap gap-x-3 gap-y-0.5 text-[11px] tabular-nums text-ink-muted">
          {segments.map((seg) => (
            <li key={seg.key} className="flex items-center gap-1.5" title={segmentTitle(seg)}>
              <span aria-hidden className={`h-1.5 w-1.5 shrink-0 rounded-full ${SEGMENT_FILL[seg.key]}`} />
              <span className="text-ink-muted">{seg.label}</span>
              <span>{formatBytes(seg.bytes)}</span>
              {seg.reclaimable ? <span className="text-ink-faint">{formatBytes(seg.reclaimable)} reclaimable</span> : null}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// A thing that depends on a disk: one of fog's own features (beacon-tinted)
// or an app (outlined), each a link to where it is set.
function UsePill({ tone, to, href, title, children }: {
  tone: "fog" | "app";
  to?: string;
  href?: string;
  title?: string;
  children: React.ReactNode;
}) {
  // Fog's own uses sit on a quiet fill; an app's is an outline, the way an
  // app is named elsewhere. Neither is the accent — nothing here is active.
  const cls = `rounded-full px-2 py-0.5 font-medium ${
    tone === "fog"
      ? "bg-hover text-ink hover:bg-fog-800"
      : "border border-hair-strong text-ink hover:border-edge-hover"
  }`;
  if (to) return <Link to={to} title={title} className={cls}>{children}</Link>;
  if (href) return <a href={href} title={title} className={cls}>{children}</a>;
  return <span title={title} className={cls}>{children}</span>;
}

