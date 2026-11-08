// The standing column: what needs you, when anything does, then the facts
// about the app that are always true, beside whatever the Overview tab is
// showing. Where the code lives, version and the update
// waiting on it, the address it answers on, the container as Docker describes it, the volumes as
// Drive holds them, who else holds a copy, and what the app depends on. Stop and Uninstall live under the
// subheader's More…. Rendered on Overview only; every other tab takes the
// full width.

import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { AppContainer, AppDetail as AppDetailT, AppStatus, DeployStatusResponse, FilesStatus } from "../../api";
import Button from "../../components/Button";
import { formatBytes, peerLabel } from "../../lib/format";
import { paths } from "../../lib/paths";
import { useAppOffsite, useCatalogEntry, type AppVolume } from "../../lib/useAppData";
import { federationQuery } from "../../queries";
import { placementLabel } from "../../lib/volumePlacement";
import type { AppActions, AppModal } from "./Actions";
import { AddressGroup } from "./Address";
import { NeedsGroup, useNeeds } from "./Overview";
import { SourceGroup, useSourceLink } from "./Source";
import { Bar, Dot, Group, Key, Pill, SlotLink, whenShort } from "./cockpit";

// The column's groups breathe more than the left column's: each is a few
// lines, and at the left column's 36px they read as one list.
const GAP = "mt-14";

export function StandingColumn({ name, detail, status, statusKnown, container, containerError, deployStatus, volumes, filesStatus, actions, onOpen, onGoDrive, onGoFed, onGoLogs, onCancelRestore, onSaved }: {
  name: string;
  detail: AppDetailT;
  status?: AppStatus;
  statusKnown: boolean;
  container?: AppContainer;
  containerError: boolean;
  deployStatus?: DeployStatusResponse;
  volumes: AppVolume[];
  filesStatus?: FilesStatus;
  actions: AppActions;
  onOpen: (m: AppModal) => void;
  onGoDrive: () => void;
  onGoFed: () => void;
  onGoLogs: () => void;
  onCancelRestore: () => void;
  onSaved: () => Promise<unknown>;
}) {
  const needs = useNeeds({
    name, detail, status, container, deployStatus, volumes, filesStatus,
    restorePending: status?.restorePending, onGoLogs, onGoDrive, onCancelRestore,
  });
  return (
    <aside className="border-t border-hair px-4 py-[26px] pb-10 sm:px-6 lg:border-l lg:border-t-0 lg:px-[26px]">
      <NeedsGroup needs={needs} gap={GAP} />
      <SourceGroup name={name} detail={detail} gap={GAP} first={needs.length === 0} />
      <Version name={name} detail={detail} status={status} statusKnown={statusKnown} container={container} actions={actions} onOpen={onOpen}
        first={needs.length === 0} />
      <AddressGroup name={name} detail={detail} status={status} gap={GAP} onSaved={onSaved} />
      <ContainerGroup detail={detail} status={status} container={container} containerError={containerError} />
      <DriveGroup volumes={volumes} filesStatus={filesStatus} onGoDrive={onGoDrive} />
      <FederationGroup name={name} volumes={volumes} onGoFed={onGoFed} />
      <DependsOn name={name} detail={detail} volumes={volumes} filesStatus={filesStatus} />
    </aside>
  );
}

// first: whether Version may open the column — only when nothing above it
// (Needs you, Source) is showing.
function Version({ name, detail, status, statusKnown, container, actions, onOpen, first }: {
  name: string; detail: AppDetailT; status?: AppStatus; statusKnown: boolean; container?: AppContainer;
  actions: AppActions; onOpen: (m: AppModal) => void; first: boolean;
}) {
  const isGit = detail.spec.source.type === "git";
  const ref = isGit
    ? (detail.app.currentRev ? `rev ${detail.app.currentRev}` : "not deployed")
    : tagOf(container?.image || status?.image || detail.spec.source.image || "");
  const policy = detail.spec.updatePolicy || "manual";
  const available = statusKnown && !!status?.updateAvailable;
  const { shown: sourceShown } = useSourceLink(name, detail);
  return (
    <Group title="Version" first={first && !sourceShown} gap={GAP} right={available ? <Pill tone="ok">Update</Pill> : undefined}>
      <div className="font-mono text-cell text-ink">{ref}</div>
      <div className={`mt-1 text-hint ${available ? "text-accent" : "text-ink-muted"}`}>
        {!statusKnown
          ? "checking for updates…"
          : available
            ? <>{status?.updateTo ? <><span className="font-mono">{status.updateTo}</span> available</> : "an update is available"}</>
            : isGit ? "latest push deployed as far as fog knows" : `nothing newer known · ${policy} updates`}
      </div>
      <div className="mt-3 flex flex-wrap gap-2">
        {available && (
          <Button size="sm" variant="primary" onClick={() => onOpen("update")} disabled={actions.updating || !actions.ready}>
            Update <Key>U</Key>
          </Button>
        )}
        <Button size="sm" variant={available ? "ghost" : "secondary"} onClick={actions.check} disabled={actions.checking || actions.updating}>
          {actions.checking ? "Checking…" : "Check now"}
        </Button>
        <Link to={paths.app(name, "updates")} className="inline-flex items-center px-1 text-xs text-ink-muted hover:text-ink">Updates</Link>
      </div>
    </Group>
  );
}

function tagOf(image: string): string {
  if (!image) return "—";
  const at = image.indexOf("@");
  const base = at >= 0 ? image.slice(0, at) : image;
  const slash = base.lastIndexOf("/");
  const colon = base.lastIndexOf(":");
  return colon > slash ? base.slice(colon + 1) : "latest";
}

// The container, one fact per row: label on the left, value on the right.
function ContainerGroup({ detail, status, container, containerError }: {
  detail: AppDetailT; status?: AppStatus; container?: AppContainer; containerError: boolean;
}) {
  const facts = containerFacts(detail, status, container, containerError);
  return (
    <Group title="Container" gap={GAP}>
      <div>
        {facts.map((f) => (
          <div key={f.label} className="flex justify-between gap-2.5 border-b border-divider py-2">
            <span className="shrink-0 text-hint text-ink-muted">{f.label}</span>
            <span className={`min-w-0 break-words text-right text-hint ${f.dim ? "text-ink-muted" : "text-ink"} ${f.mono ? "font-mono" : ""}`}>{f.value}</span>
          </div>
        ))}
      </div>
    </Group>
  );
}

// Four facts, short: what runs, whether fog checks it, what Docker does
// when it dies, and how often it has. The exit code joins only when there
// is one worth reading. The rest — image ID, memory limit — is Config's.
function containerFacts(detail: AppDetailT, status: AppStatus | undefined, c: AppContainer | undefined, error: boolean) {
  const pending = !c ? (error ? "unavailable" : "checking…") : null;
  const isGit = detail.spec.source.type === "git";
  const out: { label: string; value: string; mono?: boolean; dim?: boolean }[] = [];
  out.push({
    label: isGit ? "Revision" : "Image",
    value: isGit
      ? (detail.app.currentRev ? `rev ${detail.app.currentRev}` : "—")
      : c?.image || status?.image || detail.spec.source.image || "—",
    mono: true,
  });
  out.push({
    label: "Health check",
    value: detail.spec.health ? `${detail.spec.health.path}${c ? ` · ${Math.round(c.healthEverySeconds)} s` : ""}` : "none",
    mono: !!detail.spec.health,
    dim: !detail.spec.health,
  });
  out.push({ label: "Restart policy", value: pending ?? (c?.found ? policyLabel(c.restartPolicy) : "—"), dim: !!pending });
  out.push({
    label: "Restarts",
    value: pending ?? (c?.found ? (c.restartCount === 0 ? "none" : `${c.restartCount}`) : "—"),
    dim: !!pending || c?.restartCount === 0,
  });
  if (c?.found && c.finishedAt && (c.exitCode !== 0 || c.oomKilled)) {
    out.push({ label: "Last exit", value: `code ${c.exitCode}${c.oomKilled ? " · out of memory" : ""} · ${whenShort(c.finishedAt)}`, mono: true });
  }
  return out;
}

function policyLabel(p?: string): string {
  switch (p) {
    case "unless-stopped": return "Unless stopped";
    case "always": return "Always";
    case "on-failure": return "On failure";
    case "no": case "": case undefined: return "Never";
    default: return p;
  }
}

// The volumes as Drive holds them. Snapshots are the Drive tab's story;
// the folded summary that used to sit here was one bright line too many.
function DriveGroup({ volumes, filesStatus, onGoDrive }: {
  volumes: AppVolume[]; filesStatus?: FilesStatus; onGoDrive: () => void;
}) {
  const total = filesStatus?.totalBytes;
  const low = !!total && filesStatus?.freeBytes !== undefined && filesStatus.freeBytes < total * 0.1;

  return (
    <Group title="Drive" gap={GAP} right={<SlotLink onClick={onGoDrive}>Open</SlotLink>}>
      {volumes.length === 0 ? (
        <p className="text-hint text-ink-muted">No volumes — this app keeps nothing on disk.</p>
      ) : (
        <div>
          {volumes.map((v) => {
            const size = v.stat?.size;
            const pct = size !== undefined && total ? (size / total) * 100 : 0;
            const label =
              v.statPhase === "ready" && size !== undefined ? formatBytes(size)
              : v.statPhase === "pending" ? "measuring…"
              : v.statPhase === "missing" ? "folder missing"
              : v.statPhase === "error" ? "size unknown"
              : v.placement.kind === "managed" ? "Fog-managed"
              : v.placement.kind === "nas" ? "NAS"
              : "host path";
            const warn = v.statPhase === "missing" || (low && v.rel !== undefined);
            return (
              <div key={`${v.service}:${v.volume.name}`} className="mb-3">
                <div className="flex justify-between gap-2.5">
                  <span className="min-w-0 truncate font-mono text-hint text-ink" title={placementLabel(v.placement)}>
                    {v.volume.mount}{v.service && <span className="text-ink-muted"> · {v.service}</span>}
                  </span>
                  <span className={`shrink-0 font-mono text-hint ${warn ? "text-warn" : "text-ink-muted"}`}>{label}</span>
                </div>
                {v.rel !== undefined && <div className="mt-[7px]"><Bar pct={pct} tone={low ? "warn" : undefined} /></div>}
              </div>
            );
          })}
        </div>
      )}
    </Group>
  );
}

// Who else holds this app's data: peers a Drive folder is shared with, the
// peer a copied folder comes from, and the lockers its snapshots are in.
// Fog has no per-app failover, so this is the whole list.
export function useCopies(name: string, volumes: AppVolume[]) {
  const fed = useQuery({ ...federationQuery, refetchInterval: 10_000 });
  const offsite = useAppOffsite(name);
  const peers = fed.data?.peers ?? [];
  const copies: { key: string; label: string; role: string; online?: boolean }[] = [];
  const seen = new Set<string>();
  for (const v of volumes) {
    for (const p of v.stat?.sharedWith ?? []) {
      const key = `share:${p}`;
      if (seen.has(key)) continue;
      seen.add(key);
      const peer = peers.find((x) => x.name === p || peerLabel(x) === p);
      copies.push({ key, label: p, role: `Holds ${v.volume.mount}`, online: peer?.online });
    }
    if (v.placement.kind === "peer") {
      const p = v.placement.sub.peer ?? "peer";
      const key = `source:${p}`;
      if (!seen.has(key)) {
        seen.add(key);
        const peer = peers.find((x) => x.name === p || peerLabel(x) === p);
        copies.push({ key, label: p, role: `Source of ${v.volume.mount}`, online: peer?.online });
      }
    }
  }
  for (const r of offsite.data?.rows ?? []) {
    const key = `locker:${r.target}`;
    if (seen.has(key)) continue;
    seen.add(key);
    copies.push({ key, label: r.target, role: "Locker", online: true });
  }
  // Known once both sources have answered (or failed): a list that is
  // still filling in must not be read as "nobody".
  const known = (!!fed.data || fed.isError) && (!!offsite.data || offsite.isError)
    && volumes.every((v) => v.statPhase !== "pending");
  return { copies, known, peers, fed, offsite };
}

function FederationGroup({ name, volumes, onGoFed }: { name: string; volumes: AppVolume[]; onGoFed: () => void }) {
  const { copies, known } = useCopies(name, volumes);
  return (
    <Group title="Connect" gap={GAP} right={<SlotLink onClick={onGoFed}>Open</SlotLink>}>
      {copies.length === 0 ? (
        <p className="text-hint text-ink-muted">{known ? "No other machine holds a copy." : "Looking for copies…"}</p>
      ) : (
        <div className="flex flex-col gap-[9px]">
          {copies.map((c) => (
            <div key={c.key} className="flex items-center justify-between gap-2.5">
              <span className="inline-flex min-w-0 items-center gap-2 text-cell text-ink">
                <Dot tone={c.online === false ? "off" : "ok"} />
                <span className="truncate">{c.label}</span>
              </span>
              <span className="shrink-0 text-xs text-ink-muted">{c.role}</span>
            </div>
          ))}
        </div>
      )}
    </Group>
  );
}

function DependsOn({ name, detail, volumes, filesStatus }: {
  name: string; detail: AppDetailT; volumes: AppVolume[]; filesStatus?: FilesStatus;
}) {
  const { entry, known } = useCatalogEntry(name);
  const lines: { key: string; node: React.ReactNode }[] = [];
  const onDrive = volumes.some((v) => v.rel !== undefined);
  if (onDrive) {
    lines.push({
      key: "drive",
      node: filesStatus?.ok
        ? <>Drive on <span className="font-mono text-ink">{filesStatus.fsMount || "/"}</span>{filesStatus.freeBytes !== undefined && <> · {formatBytes(filesStatus.freeBytes)} free</>}</>
        : filesStatus ? <>Drive · <span className="text-warn">{filesStatus.error || "not usable"}</span></> : <>Drive · checking…</>,
    });
  }
  for (const v of volumes) {
    if (v.placement.kind === "nas") {
      const s = v.placement.share;
      lines.push({
        key: `nas:${s.name}`,
        node: <>NAS share <span className="text-ink">{s.name}</span> · {s.mounted ? (s.stale ? <span className="text-warn">stale</span> : "mounted") : <span className="text-warn">not mounted</span>}</>,
      });
    }
  }
  if (detail.spec.needs?.postgres) {
    lines.push({ key: "pg", node: <>Managed Postgres · <span className="font-mono text-ink">{detail.spec.needs.postgres.databases.join(", ")}</span></> });
  }
  for (const a of detail.spec.aux ?? []) {
    lines.push({ key: `aux:${a.name}`, node: <>Sidecar <span className="font-mono text-ink">{a.name}</span> · {a.image}</> });
  }
  for (const c of entry?.companions ?? []) {
    lines.push({ key: `comp:${c.id}`, node: <>Works with <Link to={paths.app(c.id)} className="text-ink hover:text-ink">{c.id}</Link></> });
  }
  // Nothing real to say — every app is on the fog network — means no group,
  // once the catalog has answered about companions.
  if (lines.length === 0 && known) return null;
  return (
    <Group title="Depends on" gap={GAP}>
      <div className="text-hint leading-[1.8] text-ink-muted">
        {lines.map((l) => <div key={l.key}>{l.node}</div>)}
        {!known && lines.length === 0 && <div className="text-ink-faint">checking the catalog…</div>}
      </div>
    </Group>
  );
}
