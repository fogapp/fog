import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { AppSpec, AuxService, Connection, NasShare } from "../api";
import AppFolderChoices from "./AppFolderChoices";
import Field, { control, controlArea } from "./Field";
import PathBrowser from "./PathBrowser";
import { useFilesStatus } from "../lib/useFiles";
import { validateGitRepo } from "../lib/gitUrl";
import { APPS_DIR, CONTENT_DIR, applyPlacement, defaultContentFolder, driveAreaOf, AREA_LABELS, placementOf, type Placement, type PlacementContext } from "../lib/volumePlacement";
import { nasQuery, subscriptionsQuery } from "../queries";
import { buttonClass } from "./Button";
import { ackKey, cleanDriveRel, cleanHostPath, driveVerdict, hostAccessVerdict } from "../lib/hostAccess";

// Editable working copy: secrets are edited as key/value here; values are
// sent separately from the spec (the spec only carries secret names).
export interface SpecDraft {
  sourceType: "image" | "git";
  image: string;
  repo: string;
  branch: string;
  dockerfile: string;
  buildArgs: { k: string; v: string }[];
  env: { k: string; v: string }[];
  secrets: { k: string; v: string; existing: boolean }[];
  // follows rides along untouched: wiring sets it, and a save from this form
  // must not quietly drop it.
  volumes: { name: string; mount: string; files: string; host: string; owner: string; readOnly: boolean; follows?: string }[];
  files: { name: string; mount: string; content: string }[];
  routes: { host: string; port: string; noBuffer: boolean; auth: boolean; lanOnly: boolean }[];
  ports: { port: string; hostPort: string; protocol: "tcp" | "udp" }[];
  processes: { name: string; command: string }[];
  // Sidecar services pass through the editor untouched — they're part of
  // the app's template (its own database, broker), not a per-field edit.
  aux: AuxService[];
  release: string;
  pgDatabases: string; // comma-separated
  pgExtensions: string;
  pgInject: { k: string; v: string }[];
  healthPath: string;
  memoryLimit: string;
  notes: string;
  updatePolicy: "manual" | "notify" | "auto";
  backupEnabled: boolean;
  // undefined follows the global default (Settings › Storage › Backups).
  backupBeforeUpdate?: boolean;
  // Hour is edited as a string ("" = follow the global default). Mode and
  // timeout have no editor yet — they ride along so a save can't strip them.
  backupHourUTC: string;
  backupMode: string;
  backupTimeout: string;
  // What this app may read from fog (docs/events-contract.md), as a comma
  // list: "events" and other apps' names. The connection it offers others
  // rides along untouched, like aux — it belongs to the app's template.
  uses: string;
  // The other apps it may call on the network, as a comma list
  // (AppSpec.reach): the operator's approval, one app at a time.
  reach: string;
  // Keep CAP_NET_RAW (AppSpec.acknowledgeRawSockets), which fog drops.
  acknowledgeRawSockets?: boolean;
  connection?: Connection;
  // The app's network passes through the form too: it is what seals an app,
  // and a save from this form must not quietly unseal it.
  network?: AppSpec["network"];
  sealedPeer?: boolean;
  // Host paths the operator acknowledged hand the app the machine
  // (lib/hostAccess). Each volume row toggles its own; a sidecar's ride
  // along untouched, like the sidecar.
  acknowledgeHostAccess: string[];
}

export function emptyDraft(): SpecDraft {
  return {
    sourceType: "image", image: "", repo: "", branch: "", dockerfile: "", buildArgs: [],
    env: [], secrets: [], volumes: [], files: [], routes: [], ports: [], processes: [], aux: [], release: "",
    pgDatabases: "", pgExtensions: "", pgInject: [], healthPath: "", memoryLimit: "", notes: "",
    updatePolicy: "manual", backupEnabled: false, backupBeforeUpdate: undefined,
    backupHourUTC: "", backupMode: "", backupTimeout: "", uses: "", reach: "", acknowledgeHostAccess: [],
  };
}

export function draftFromSpec(spec: AppSpec, secretKeys: string[]): SpecDraft {
  return {
    sourceType: spec.source.type,
    image: spec.source.image ?? "",
    repo: spec.source.repo ?? "",
    branch: spec.source.branch ?? "",
    dockerfile: spec.source.dockerfile ?? "",
    buildArgs: Object.entries(spec.source.buildArgs ?? {}).map(([k, v]) => ({ k, v })),
    env: Object.entries(spec.env ?? {}).map(([k, v]) => ({ k, v })),
    secrets: secretKeys.map((k) => ({ k, v: "", existing: true })),
    volumes: (spec.volumes ?? []).map((v) => ({ name: v.name, mount: v.mount, files: v.files ?? "", host: v.host ?? "", owner: v.owner ?? "", readOnly: !!v.readOnly, follows: v.follows })),
    files: (spec.files ?? []).map((f) => ({ ...f })),
    routes: (spec.routes ?? []).map((r) => ({ host: r.host ?? "", port: String(r.port), noBuffer: !!r.noBuffer, auth: !!r.auth, lanOnly: !!r.lanOnly })),
    ports: (spec.ports ?? []).map((p) => ({ port: String(p.port), hostPort: p.hostPort ? String(p.hostPort) : "", protocol: p.protocol ?? "tcp" })),
    processes: (spec.processes ?? []).map((p) => ({ name: p.name, command: p.command.join(" ") })),
    aux: spec.aux ?? [],
    release: (spec.release ?? []).join(" "),
    pgDatabases: (spec.needs?.postgres?.databases ?? []).join(", "),
    pgExtensions: (spec.needs?.postgres?.extensions ?? []).join(", "),
    pgInject: Object.entries(spec.needs?.postgres?.inject ?? {}).map(([k, v]) => ({ k, v })),
    healthPath: spec.health?.path ?? "",
    memoryLimit: spec.memoryLimit ?? "",
    notes: spec.notes ?? "",
    updatePolicy: spec.updatePolicy ?? "manual",
    backupEnabled: spec.backup?.enabled ?? false,
    backupBeforeUpdate: spec.backup?.beforeUpdate,
    backupHourUTC: spec.backup?.hourUTC == null ? "" : String(spec.backup.hourUTC),
    backupMode: spec.backup?.mode ?? "",
    backupTimeout: spec.backup?.timeout ?? "",
    uses: (spec.uses ?? []).join(", "),
    reach: (spec.reach ?? []).join(", "),
    acknowledgeRawSockets: spec.acknowledgeRawSockets,
    connection: spec.connection,
    network: spec.network,
    sealedPeer: spec.sealedPeer,
    acknowledgeHostAccess: spec.acknowledgeHostAccess ?? [],
  };
}

const splitList = (s: string) => s.split(",").map((x) => x.trim()).filter(Boolean);
const splitCmd = (s: string) => s.trim().split(/\s+/).filter(Boolean);

export function draftToPayload(d: SpecDraft): { spec: AppSpec; secrets: Record<string, string> } {
  const dbs = splitList(d.pgDatabases);
  const spec: AppSpec = {
    source:
      d.sourceType === "git"
        ? {
            type: "git", repo: d.repo.trim(), branch: d.branch.trim() || undefined,
            dockerfile: d.dockerfile.trim() || undefined,
            buildArgs: Object.fromEntries(d.buildArgs.filter((e) => e.k.trim()).map((e) => [e.k.trim(), e.v])),
          }
        : { type: "image", image: d.image.trim() },
    env: Object.fromEntries(d.env.filter((e) => e.k.trim()).map((e) => [e.k.trim(), e.v])),
    secrets: d.secrets.filter((s) => s.k.trim()).map((s) => s.k.trim()),
    volumes: d.volumes
      .filter((v) => v.name.trim() && v.mount.trim())
      .map((v) => ({
        name: v.name.trim(), mount: v.mount.trim(),
        files: v.files.trim().replace(/^\/+|\/+$/g, "") || undefined,
        host: v.host.trim() || undefined, owner: v.owner.trim() || undefined, readOnly: v.readOnly || undefined,
        follows: v.follows || undefined,
      })),
    files: d.files.filter((f) => f.name.trim() && f.mount.trim()),
    routes: d.routes
      .filter((r) => r.port.trim())
      .map((r) => ({ host: r.host.trim() || undefined, port: parseInt(r.port, 10), noBuffer: r.noBuffer || undefined, auth: r.auth || undefined, lanOnly: r.lanOnly || undefined })),
    ports: d.ports
      .filter((p) => p.port.trim())
      .map((p) => ({
        port: parseInt(p.port, 10),
        hostPort: p.hostPort.trim() ? parseInt(p.hostPort, 10) : undefined,
        protocol: p.protocol === "tcp" ? undefined : p.protocol,
      })),
    processes: d.processes
      .filter((p) => p.name.trim() && p.command.trim())
      .map((p) => ({ name: p.name.trim(), command: splitCmd(p.command) })),
    aux: d.aux.length ? d.aux : undefined,
    release: d.release.trim() ? splitCmd(d.release) : undefined,
    needs: dbs.length
      ? {
          postgres: {
            databases: dbs,
            extensions: splitList(d.pgExtensions),
            inject: Object.fromEntries(d.pgInject.filter((e) => e.k.trim()).map((e) => [e.k.trim(), e.v.trim()])),
          },
        }
      : undefined,
    notes: d.notes.trim() || undefined,
    health: d.healthPath.trim() ? { path: d.healthPath.trim() } : undefined,
    memoryLimit: d.memoryLimit.trim() || undefined,
    updatePolicy: d.updatePolicy === "manual" ? undefined : d.updatePolicy,
    // Mode/timeout survive a disable so re-enabling doesn't silently demote a
    // delegated (fog-backup/1) app to volume snapshots.
    backup: d.backupEnabled || d.backupMode || d.backupTimeout || d.backupHourUTC.trim()
      ? {
          enabled: d.backupEnabled,
          beforeUpdate: d.backupBeforeUpdate,
          mode: d.backupMode || undefined,
          timeout: d.backupTimeout || undefined,
          hourUTC: d.backupHourUTC.trim() && !Number.isNaN(parseInt(d.backupHourUTC, 10))
            ? parseInt(d.backupHourUTC, 10)
            : undefined,
        }
      : undefined,
    uses: splitList(d.uses).length ? splitList(d.uses) : undefined,
    reach: splitList(d.reach).length ? splitList(d.reach) : undefined,
    acknowledgeRawSockets: d.acknowledgeRawSockets || undefined,
    connection: d.connection,
    network: d.network,
    sealedPeer: d.sealedPeer,
    acknowledgeHostAccess: boundAcknowledgements(d),
  };
  // Only send secret values that were entered (blank = keep existing).
  const secrets: Record<string, string> = {};
  for (const s of d.secrets) {
    if (s.k.trim() && (s.v !== "" || !s.existing)) secrets[s.k.trim()] = s.v;
  }
  return { spec, secrets };
}

// boundAcknowledgements keeps the acknowledgements a volume still binds:
// the server refuses one that names a path nothing mounts, so a removed or
// re-pointed volume takes its acknowledgement with it.
function boundAcknowledgements(d: SpecDraft): string[] | undefined {
  const vols = [...d.volumes, ...d.aux.flatMap((a) => a.volumes ?? [])];
  const bound = new Set([
    ...vols.map((v) => v.host ?? "").filter((h) => h.trim()).map(cleanHostPath),
    ...vols.map((v) => v.files ?? "").filter((f) => f.trim()).map(cleanDriveRel),
  ]);
  const out = d.acknowledgeHostAccess.filter((a) => bound.has(ackKey(a)));
  return out.length ? out : undefined;
}

// volumeAckKey is what a volume's acknowledgement is spelled as: its host
// path, or its Drive folder (no leading slash); "" for neither.
const volumeAckKey = (v: VolumeDraft) => (v.host.trim() ? cleanHostPath(v.host) : v.files.trim() ? cleanDriveRel(v.files) : "");

const rowInput = control;

function AddBtn({ onClick, label }: { onClick: () => void; label: string }) {
  return (
    <button type="button" onClick={onClick} className="text-xs font-medium text-ink-muted hover:text-ink">
      + {label}
    </button>
  );
}

function RemoveBtn({ onClick }: { onClick: () => void }) {
  return (
    <button type="button" onClick={onClick} className="px-1 text-ink-muted hover:text-warn" title="Remove">
      ✕
    </button>
  );
}

type VolumeDraft = SpecDraft["volumes"][number];

// One volume's storage location: a picker over the places a folder can be
// — Fog-managed, a folder in Files (the Apps folder is where libraries
// go), a peer's copy inside Files, a NAS share, a host path — so putting
// an app's data somewhere is a selection plus a folder name, not a
// hand-typed absolute path. lib/volumePlacement is the shared reading of
// what the spec says and how a choice is written back.
function VolumeRow({ v, shares, app, acknowledged, onAcknowledge, onChange, onRemove }: {
  v: VolumeDraft; shares: NasShare[];
  acknowledged: boolean; onAcknowledge: (yes: boolean) => void;
  // The app this spec belongs to; "" where it has no name yet (a door that
  // creates one). It names the folder in Apps that is this app's own.
  app: string;
  onChange: (v: VolumeDraft) => void; onRemove: () => void;
}) {
  // forceCustom keeps "Custom host path" selected while its input is empty
  // or still matches a share prefix the user is editing away from.
  const [forceCustom, setForceCustom] = useState(false);
  // Which browse dialog is open: the whole host filesystem (custom path) or
  // a share's subtree (subpath). Browsing beats typing — the picked path is
  // one that actually exists, with ownership and mount facts shown en route.
  const [browsing, setBrowsing] = useState<"" | "host" | "share" | "files">("");
  // Files (fog's own tree) is the third place a volume can point: a folder
  // the operator can see, upload to, and back up from the Drive page.
  const filesStatus = useFilesStatus();
  const filesRoot = filesStatus.data?.ok ? filesStatus.data.root : "";
  // A peer's share, copied into Files, is the fourth place: read-only in
  // the container, and only when its lens allows apps and the copy is full.
  const subsQ = useQuery({
    ...subscriptionsQuery,
    enabled: !!filesRoot,
  });
  const peerSubs = (subsQ.data?.subscriptions ?? []).filter((s) => s.status !== "revoked");
  const ctx: PlacementContext = { filesRoot, shares, peerSubs };
  const placement = placementOf(v, ctx);
  const mode = forceCustom ? "custom"
    : placement.kind === "managed" ? "managed"
    : placement.kind === "nas" ? `nas:${placement.share.name}`
    : placement.kind === "peer" ? `peer:${placement.sub.id}`
    : placement.kind === "files" ? "files"
    : "custom";
  const peerSub = placement.kind === "peer" ? placement.sub : undefined;
  const selected = placement.kind === "nas" && !forceCustom ? placement.share : undefined;
  const subpath = placement.kind === "nas" ? placement.sub : placement.kind === "files" ? placement.rel : "";
  const place = (p: Placement) => onChange(applyPlacement(v, p));

  const browseBtn = (kind: "host" | "share" | "files") => (
    <button type="button" onClick={() => setBrowsing(kind)}
      className={buttonClass()}
      title="Browse the server's filesystem">
      Browse…
    </button>
  );

  const nameMountOwner = (
    <>
      <input className={`${rowInput} w-28`} placeholder="name" value={v.name}
        onChange={(ev) => onChange({ ...v, name: ev.target.value })} />
      <input className={`${rowInput} flex-1`} placeholder="/config (container path)" value={v.mount}
        onChange={(ev) => onChange({ ...v, mount: ev.target.value })} />
    </>
  );

  // Full-filesystem browser for the custom host path; the share and Files
  // browsers are scoped by starting inside them (selecting elsewhere is
  // still honest — the mode re-derives from whatever path comes back).
  const filesAbs = placement.kind === "files" && filesRoot ? `${filesRoot}/${placement.rel}` : filesRoot;
  const browser = browsing !== "" && (
    <PathBrowser
      title={browsing === "share" ? "Choose a folder on the share" : browsing === "files" ? "Choose a folder in Drive" : "Choose a host folder"}
      initial={browsing === "share" ? (v.host || selected?.mountPoint) : browsing === "files" ? filesAbs : v.host}
      defaultOwner={v.owner}
      onSelect={(p) => { place(placementOf({ host: p }, ctx)); setForceCustom(false); setBrowsing(""); }}
      onClose={() => setBrowsing("")} />
  );

  // A Drive folder by files: is judged as the server does; one that follows
  // another app's volume is the server's to judge, which knows its shares.
  const verdict = v.host.trim()
    ? hostAccessVerdict(v.host, v.readOnly, { shares: shares.map((s) => s.mountPoint), filesRoot, app })
    : { refused: "", needsAck: v.files.trim() && !v.follows ? driveVerdict(v.files, app) : "" };
  const shown = v.host.trim() ? cleanHostPath(v.host) : `Drive › ${cleanDriveRel(v.files)}`;
  const hostAccess = verdict.refused ? (
    <div className="rounded-md border border-err/30 bg-err/10 p-2 text-xs text-err">
      {shown} is {verdict.refused}. Fog never mounts it into an app.
    </div>
  ) : verdict.needsAck ? (
    <div className="rounded-md border border-warn/30 bg-warn/10 p-2 text-xs text-warn">
      <p>{shown} is {verdict.needsAck}. The app can read and change everything in it.</p>
      <label className="mt-1 flex items-center gap-1.5">
        <input type="checkbox" checked={acknowledged} onChange={(ev) => onAcknowledge(ev.target.checked)} />
        Let this app mount {shown}
      </label>
    </div>
  ) : null;

  if (shares.length === 0 && !filesRoot) {
    return (
      <div className="space-y-1.5">
      <div className="flex gap-2">
        {nameMountOwner}
        <input className={`${rowInput} flex-1`} placeholder="host path (optional)" value={v.host}
          onChange={(ev) => place({ kind: "host", path: ev.target.value })} />
        {browseBtn("host")}
        <input className={`${rowInput} w-24`} placeholder="uid:gid" value={v.owner}
          onChange={(ev) => onChange({ ...v, owner: ev.target.value })} />
        <RemoveBtn onClick={onRemove} />
        {browser}
      </div>
      {hostAccess}
      </div>
    );
  }

  return (
    <div className="space-y-1.5 rounded-md border border-hair p-2">
      <div className="flex gap-2">
        {nameMountOwner}
        <input className={`${rowInput} w-24`} placeholder="uid:gid" value={v.owner}
          onChange={(ev) => onChange({ ...v, owner: ev.target.value })} />
        <RemoveBtn onClick={onRemove} />
      </div>
      <div className="flex items-center gap-2">
        <select
          value={mode}
          onChange={(ev) => {
            const m = ev.target.value;
            if (m === "managed") {
              setForceCustom(false);
              place({ kind: "managed" });
            } else if (m === "custom") {
              setForceCustom(true);
            } else if (m === "files") {
              setForceCustom(false);
              place({ kind: "files", rel: defaultContentFolder(v.name) });
            } else if (m.startsWith("peer:")) {
              const s = peerSubs.find((x) => String(x.id) === m.slice(5));
              setForceCustom(false);
              if (s) place({ kind: "peer", rel: s.folder, sub: s });
            } else {
              const sh = shares.find((s) => s.name === m.slice(4));
              setForceCustom(false);
              if (sh) place({ kind: "nas", share: sh, sub: "" });
            }
          }}
          className={`${rowInput} w-56 shrink-0`}>
          <option value="managed">Fog-managed (backed up)</option>
          {filesRoot && <option value="files">Drive folder</option>}
          {shares.map((s) => (
            <option key={s.name} value={`nas:${s.name}`}>NAS: {s.name}</option>
          ))}
          {peerSubs.length > 0 && (
            <optgroup label="Shared by a peer">
              {peerSubs.map((s) => (
                <option key={s.id} value={`peer:${s.id}`} disabled={!s.appsAllowed || s.detailWanted !== "full"}>
                  {s.peer ?? "peer"}: {s.title}
                  {!s.appsAllowed ? " — for people, not apps" : s.detailWanted !== "full" ? " — keep a full copy first" : ""}
                </option>
              ))}
            </optgroup>
          )}
          <option value="custom">Custom host path</option>
        </select>
        {peerSub && (
          <span className="shrink-0 text-xs text-accent" title={`A copy of ${peerSub.peer ?? "the peer"}'s share; mounted read-only, refreshed on every sync.`}>
            read-only · {peerSub.peer ?? "peer"}'s copy{peerSub.detailWanted !== "full" ? " · keep a full copy to mount" : ""}
          </span>
        )}
        {mode === "files" && (
          <>
            <span className="shrink-0 font-mono text-xs text-ink-muted">Drive/</span>
            <input className={`${rowInput} flex-1`} placeholder={`${CONTENT_DIR}/…`} value={subpath}
              onChange={(ev) => place({ kind: "files", rel: ev.target.value })} />
            {browseBtn("files")}
            <span className="shrink-0 text-xs text-ink-muted" title={`Created on deploy if it isn't there yet. Only a folder under ${APPS_DIR} that belongs to this app travels in its backups; anything else is covered by the nightly Drive snapshot.`}>
              {AREA_LABELS[driveAreaOf(subpath)].toLowerCase()}
            </span>
          </>
        )}
        {selected && (
          <>
            <span className="shrink-0 font-mono text-xs text-ink-muted">{selected.mountPoint}/</span>
            <input className={`${rowInput} flex-1`} placeholder="folder on the share (optional)" value={subpath}
              onChange={(ev) => place({ kind: "nas", share: selected, sub: ev.target.value.replace(/^\/+/, "") })} />
            {selected.mounted && browseBtn("share")}
            {!selected.mounted && (
              <span className="shrink-0 text-xs text-warn" title="Fog mounts it automatically when this app deploys or starts.">
                not mounted — mounts on deploy
              </span>
            )}
          </>
        )}
        {mode === "custom" && (
          <>
            <input className={`${rowInput} flex-1`} placeholder="/absolute/host/path" value={v.host}
              onChange={(ev) => place({ kind: "host", path: ev.target.value })} />
            {browseBtn("host")}
          </>
        )}
        {mode === "managed" && (
          <span className="text-xs text-ink-muted">stored under Fog's data dir · included in this app's backups</span>
        )}
        {mode !== "managed" && !peerSub && (
          <label className="flex shrink-0 items-center gap-1 text-xs text-ink-muted" title="Mount it :ro — the app reads, never writes">
            <input type="checkbox" checked={v.readOnly} onChange={(ev) => onChange({ ...v, readOnly: ev.target.checked })} /> read-only
          </label>
        )}
      </div>
      {/* The folders this app could sensibly use: its own in Apps, and the
          library folder the volume already names. Typing a path still works
          — this is the same choice without one. */}
      {mode === "files" && (
        <AppFolderChoices app={app} rel={subpath} onPick={(rel) => place({ kind: "files", rel })} />
      )}
      {hostAccess}
      {browser}
    </div>
  );
}

function Section({ title, hint, children, action }: { title: string; hint?: string; children: React.ReactNode; action?: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-hair p-4">
      <div className="mb-2 flex items-center justify-between">
        <h3 className="text-sm font-semibold text-ink">{title}</h3>
        {action}
      </div>
      {hint && <p className="mb-3 text-xs text-ink-muted">{hint}</p>}
      <div className="space-y-2">{children}</div>
    </div>
  );
}

// Each group of the spec edits on its own. The config page opens one at a
// time, inline under the values it changes; the new-app form stacks all of
// them into one long form. Both mount the same controls, so a field can't
// drift between where you create an app and where you change it.
export interface EditorProps {
  draft: SpecDraft;
  set: (patch: Partial<SpecDraft>) => void;
}

function Hint({ children }: { children: React.ReactNode }) {
  return <p className="text-xs text-ink-muted">{children}</p>;
}

export function SourceEditor({ draft, set }: EditorProps) {
  return (
    <div className="space-y-3">
      <div className="flex gap-2">
        {(["image", "git"] as const).map((t) => (
          <button key={t} type="button" onClick={() => set({ sourceType: t })}
            className={`rounded-md px-3 py-1.5 text-sm font-medium ${
              draft.sourceType === t ? "bg-accent text-accent-on" : "border border-edge text-ink-muted hover:bg-fog-800"
            }`}>
            {t === "image" ? "Container image" : "Git repository"}
          </button>
        ))}
      </div>

      {draft.sourceType === "image" ? (
        <Field label="Image" value={draft.image} onChange={(e) => set({ image: e.target.value })}
          placeholder="lscr.io/linuxserver/sonarr:latest" />
      ) : (
        <>
          <div>
            <Field label="Repository (SSH)" value={draft.repo} onChange={(e) => set({ repo: e.target.value })}
              placeholder="ssh://git@git.example.com/you/app.git"
              hint="Fog generates a deploy key on create — add it to the repo, then add the webhook for push-to-deploy." />
            {validateGitRepo(draft.repo) && (
              <p className="mt-1 text-xs text-warn">{validateGitRepo(draft.repo)}</p>
            )}
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Branch" value={draft.branch} onChange={(e) => set({ branch: e.target.value })} placeholder="main" />
            <Field label="Dockerfile (optional)" value={draft.dockerfile} onChange={(e) => set({ dockerfile: e.target.value })} placeholder="Dockerfile" />
          </div>
          <div className="space-y-2">
            <Hint>Build args — values may use {"{{commit}}"} for the git SHA (e.g. GIT_REV).</Hint>
            {draft.buildArgs.map((e, i) => (
              <div key={i} className="flex gap-2">
                <input className={`${rowInput} w-48`} placeholder="GIT_REV" value={e.k}
                  onChange={(ev) => set({ buildArgs: draft.buildArgs.map((x, j) => (j === i ? { ...x, k: ev.target.value } : x)) })} />
                <input className={`${rowInput} flex-1`} placeholder="{{commit}}" value={e.v}
                  onChange={(ev) => set({ buildArgs: draft.buildArgs.map((x, j) => (j === i ? { ...x, v: ev.target.value } : x)) })} />
                <RemoveBtn onClick={() => set({ buildArgs: draft.buildArgs.filter((_, j) => j !== i) })} />
              </div>
            ))}
            <AddBtn label="build arg" onClick={() => set({ buildArgs: [...draft.buildArgs, { k: "", v: "" }] })} />
          </div>
        </>
      )}
    </div>
  );
}

export function DeployEditor({ draft, set }: EditorProps) {
  return (
    <div className="space-y-3">
      <Field label="Release command (optional)" value={draft.release} onChange={(e) => set({ release: e.target.value })}
        placeholder="bundle exec rails db:migrate"
        hint="Runs once per deploy from the new image, before processes switch over." />
      <div className="space-y-2">
        <Hint>Worker processes — extra long-running processes sharing the app's image and env (e.g. a job worker).</Hint>
        {draft.processes.map((p, i) => (
          <div key={i} className="flex gap-2">
            <input className={`${rowInput} w-32`} placeholder="worker" value={p.name}
              onChange={(ev) => set({ processes: draft.processes.map((x, j) => (j === i ? { ...x, name: ev.target.value } : x)) })} />
            <input className={`${rowInput} flex-1`} placeholder="bundle exec ruby bin/jobs" value={p.command}
              onChange={(ev) => set({ processes: draft.processes.map((x, j) => (j === i ? { ...x, command: ev.target.value } : x)) })} />
            <RemoveBtn onClick={() => set({ processes: draft.processes.filter((_, j) => j !== i) })} />
          </div>
        ))}
        <AddBtn label="process" onClick={() => set({ processes: [...draft.processes, { name: "", command: "" }] })} />
      </div>
    </div>
  );
}

export function PostgresEditor({ draft, set }: EditorProps) {
  return (
    <div className="space-y-2">
      <Hint>Fog provisions role, databases, and extensions, then injects connection env vars.</Hint>
      <div className="grid gap-2 sm:grid-cols-2">
        <input className={rowInput} placeholder="databases (comma-sep)" value={draft.pgDatabases}
          onChange={(e) => set({ pgDatabases: e.target.value })} />
        <input className={rowInput} placeholder="extensions: citext, pg_trgm" value={draft.pgExtensions}
          onChange={(e) => set({ pgExtensions: e.target.value })} />
      </div>
      {draft.pgDatabases.trim() && (
        <div className="space-y-2">
          <Hint>Inject env var → field (host, port, user, password, dbname)</Hint>
          {draft.pgInject.map((e, i) => (
            <div key={i} className="flex gap-2">
              <input className={`${rowInput} w-56`} placeholder="DATABASE_HOST" value={e.k}
                onChange={(ev) => set({ pgInject: draft.pgInject.map((x, j) => (j === i ? { ...x, k: ev.target.value } : x)) })} />
              <input className={`${rowInput} w-32`} placeholder="host" value={e.v}
                onChange={(ev) => set({ pgInject: draft.pgInject.map((x, j) => (j === i ? { ...x, v: ev.target.value } : x)) })} />
              <RemoveBtn onClick={() => set({ pgInject: draft.pgInject.filter((_, j) => j !== i) })} />
            </div>
          ))}
          <AddBtn label="mapping" onClick={() => set({ pgInject: [...draft.pgInject, { k: "", v: "" }] })} />
        </div>
      )}
    </div>
  );
}

export function EnvEditor({ draft, set }: EditorProps) {
  return (
    <div className="space-y-2">
      {draft.env.map((e, i) => (
        <div key={i} className="flex gap-2">
          <input className={`${rowInput} w-48`} placeholder="KEY" value={e.k}
            onChange={(ev) => set({ env: draft.env.map((x, j) => (j === i ? { ...x, k: ev.target.value } : x)) })} />
          <input className={`${rowInput} flex-1`} placeholder="value" value={e.v}
            onChange={(ev) => set({ env: draft.env.map((x, j) => (j === i ? { ...x, v: ev.target.value } : x)) })} />
          <RemoveBtn onClick={() => set({ env: draft.env.filter((_, j) => j !== i) })} />
        </div>
      ))}
      <AddBtn label="variable" onClick={() => set({ env: [...draft.env, { k: "", v: "" }] })} />
    </div>
  );
}

export function SecretsEditor({ draft, set }: EditorProps) {
  return (
    <div className="space-y-2">
      <Hint>Encrypted at rest; injected as env vars at deploy. Leave the value blank to keep an existing secret.</Hint>
      {draft.secrets.map((s, i) => (
        <div key={i} className="flex gap-2">
          <input className={`${rowInput} w-48`} placeholder="KEY" value={s.k} disabled={s.existing}
            onChange={(ev) => set({ secrets: draft.secrets.map((x, j) => (j === i ? { ...x, k: ev.target.value } : x)) })} />
          <input className={`${rowInput} flex-1`} type="password" placeholder={s.existing ? "•••••• (unchanged)" : "value"} value={s.v}
            onChange={(ev) => set({ secrets: draft.secrets.map((x, j) => (j === i ? { ...x, v: ev.target.value } : x)) })} />
          <RemoveBtn onClick={() => set({ secrets: draft.secrets.filter((_, j) => j !== i) })} />
        </div>
      ))}
      <AddBtn label="secret" onClick={() => set({ secrets: [...draft.secrets, { k: "", v: "", existing: false }] })} />
    </div>
  );
}

// What this app may read from Fog itself. The other apps it is handed are
// in the same uses list, but they are connected and disconnected on the
// Integrations tab, so they are shown here and kept as they are.
export function AccessEditor({ draft, set }: EditorProps) {
  const c = draft.connection;
  const uses = draft.uses.split(",").map((u) => u.trim()).filter(Boolean);
  const grants = [
    { id: "events", label: "Read the household event stream", title: "Every app's events as they happen: what a notifier or automation reads" },
    { id: "home", label: "Read the home view", title: "Every app, its state, the Inbox summary and the server's vitals: what a start page shows" },
    { id: "drive", label: "Search Drive for you", title: "File and folder names, paths, sizes and dates in Drive, while you are using the app: what a start page's search box finds. Never file contents, other apps' folders or peers' files" },
    { id: "chat", label: "Post into chat", title: "Post messages into the rooms fog's bot is in, #fog among them, shown as this app's" },
  ];
  const connectedApps = uses.filter((u) => !grants.some((g) => g.id === u));
  const toggle = (id: string, on: boolean) =>
    set({ uses: (on ? [...uses, id] : uses.filter((u) => u !== id)).join(", ") });
  return (
    <div className="space-y-2">
      <Hint>What this app may read from Fog, or do through it. Other apps are connected on its Integrations tab.</Hint>
      {grants.map((g) => (
        <label key={g.id} className="flex items-center gap-2 text-sm text-ink-muted" title={g.title}>
          <input type="checkbox" checked={uses.includes(g.id)} onChange={(ev) => toggle(g.id, ev.target.checked)} />
          {g.label}
        </label>
      ))}
      {connectedApps.length > 0 && (
        <p className="text-sm text-ink-muted">Connected to {connectedApps.join(", ")}.</p>
      )}
      <label className="flex items-center gap-2 text-sm text-ink-muted"
        title="Fog takes raw sockets away from every app: with them an app can forge addresses on its network. Ping works without. A DHCP server or a network scanner needs them.">
        <input type="checkbox" checked={!!draft.acknowledgeRawSockets}
          onChange={(ev) => set({ acknowledgeRawSockets: ev.target.checked })} />
        Allow raw sockets (a DHCP server, a network scanner)
      </label>
      <label className="block text-sm text-ink-muted"
        title="Apps share no network. This app reaches another only if it is named here (or connected on the Integrations tab) — never just because a setting mentions it.">
        Apps it may call on the network
        <input className="mt-1 w-full rounded-md border border-hair bg-transparent px-2 py-1 font-mono text-sm text-ink"
          placeholder="e.g. ntfy, gotenberg" value={draft.reach} onChange={(ev) => set({ reach: ev.target.value })} />
      </label>
      {c && (
        <p className="text-sm text-ink-muted">
          Offers a connection on port <span className="font-mono">{c.port}{c.path ?? ""}</span>
          {c.secret && <> with the <span className="font-mono">{c.secret}</span> secret as its credential</>}.
        </p>
      )}
    </div>
  );
}

export function VolumesEditor({ draft, set, app = "" }: EditorProps & { app?: string }) {
  const nas = useQuery(nasQuery);
  const shares = nas.data?.shares ?? [];
  return (
    <div className="space-y-2">
      <Hint>
        Fog-managed storage lives under Fog's data dir and is backed up with the app. A Drive folder is one you
        can browse and drop files into: this app's own data belongs under Drive › Apps ›{" "}
        {app || "<app>"}, which travels in its backups, and a library several apps read belongs under
        Drive › Content, which the nightly Drive snapshot covers instead.
        {shares.length > 0 && " NAS storage lands on a mounted share (Settings → Storage → Disks & shares); Fog mounts it on deploy if needed."}
      </Hint>
      {draft.volumes.map((v, i) => (
        <VolumeRow key={i} v={v} shares={shares} app={app}
          acknowledged={!!volumeAckKey(v) && draft.acknowledgeHostAccess.some((a) => ackKey(a) === volumeAckKey(v))}
          onAcknowledge={(yes) => {
            const k = volumeAckKey(v);
            const rest = draft.acknowledgeHostAccess.filter((a) => ackKey(a) !== k);
            set({ acknowledgeHostAccess: yes && k ? [...rest, k] : rest });
          }}
          onChange={(nv) => set({ volumes: draft.volumes.map((x, j) => (j === i ? nv : x)) })}
          onRemove={() => set({ volumes: draft.volumes.filter((_, j) => j !== i) })} />
      ))}
      <AddBtn label="volume" onClick={() => set({ volumes: [...draft.volumes, { name: "", mount: "", files: "", host: "", owner: "", readOnly: false }] })} />
    </div>
  );
}

export function RoutesEditor({ draft, set }: EditorProps) {
  return (
    <div className="space-y-2">
      <Hint>
        Hostname defaults to &lt;app&gt;.&lt;base domain&gt;. A bare label like 'claw' becomes claw.&lt;base domain&gt; and
        follows it if it ever changes; a dotted name is used as-is. 'No buffering' is for media/streaming endpoints.
      </Hint>
      {draft.routes.map((r, i) => (
        <div key={i} className="flex items-center gap-2">
          <input className={`${rowInput} flex-1`} placeholder="subdomain or full host (optional)" value={r.host}
            onChange={(ev) => set({ routes: draft.routes.map((x, j) => (j === i ? { ...x, host: ev.target.value } : x)) })} />
          <input className={`${rowInput} w-24`} placeholder="port" value={r.port}
            onChange={(ev) => set({ routes: draft.routes.map((x, j) => (j === i ? { ...x, port: ev.target.value } : x)) })} />
          <label className="flex items-center gap-1 text-xs text-ink-muted">
            <input type="checkbox" checked={r.noBuffer}
              onChange={(ev) => set({ routes: draft.routes.map((x, j) => (j === i ? { ...x, noBuffer: ev.target.checked } : x)) })} />
            no buffering
          </label>
          <label className="flex items-center gap-1 text-xs text-ink-muted" title="Require Fog login before reaching this app">
            <input type="checkbox" checked={r.auth}
              onChange={(ev) => set({ routes: draft.routes.map((x, j) => (j === i ? { ...x, auth: ev.target.checked } : x)) })} />
            fog login
          </label>
          <label className="flex items-center gap-1 text-xs text-ink-muted" title="Only answer clients on the home network">
            <input type="checkbox" checked={r.lanOnly}
              onChange={(ev) => set({ routes: draft.routes.map((x, j) => (j === i ? { ...x, lanOnly: ev.target.checked } : x)) })} />
            LAN only
          </label>
          <RemoveBtn onClick={() => set({ routes: draft.routes.filter((_, j) => j !== i) })} />
        </div>
      ))}
      <AddBtn label="route" onClick={() => set({ routes: [...draft.routes, { host: "", port: "", noBuffer: false, auth: false, lanOnly: false }] })} />
    </div>
  );
}

export function PortsEditor({ draft, set }: EditorProps) {
  return (
    <div className="space-y-2">
      <Hint>
        Raw host ports for non-HTTP protocols (DNS, SMTP…). Web UIs belong in Routes. Published ports disable
        near-zero-downtime deploys — the app restarts on update.
      </Hint>
      {draft.ports.map((p, i) => (
        <div key={i} className="flex items-center gap-2">
          <input className={`${rowInput} w-32`} placeholder="container port" value={p.port}
            onChange={(ev) => set({ ports: draft.ports.map((x, j) => (j === i ? { ...x, port: ev.target.value } : x)) })} />
          <input className={`${rowInput} w-40`} placeholder="host port (= container)" value={p.hostPort}
            onChange={(ev) => set({ ports: draft.ports.map((x, j) => (j === i ? { ...x, hostPort: ev.target.value } : x)) })} />
          <select className={`${rowInput} w-20`} value={p.protocol}
            onChange={(ev) => set({ ports: draft.ports.map((x, j) => (j === i ? { ...x, protocol: ev.target.value as "tcp" | "udp" } : x)) })}>
            <option value="tcp">tcp</option>
            <option value="udp">udp</option>
          </select>
          <RemoveBtn onClick={() => set({ ports: draft.ports.filter((_, j) => j !== i) })} />
        </div>
      ))}
      <AddBtn label="port" onClick={() => set({ ports: [...draft.ports, { port: "", hostPort: "", protocol: "tcp" }] })} />
    </div>
  );
}

export function RuntimeEditor({ draft, set }: EditorProps) {
  return (
    <div className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Health check path (optional)" value={draft.healthPath} placeholder="/up"
          onChange={(e) => set({ healthPath: e.target.value })}
          hint="Enables health monitoring and near-zero-downtime deploys." />
        <Field label="Memory limit (optional)" value={draft.memoryLimit} placeholder="2g"
          onChange={(e) => set({ memoryLimit: e.target.value })} />
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        {draft.sourceType === "image" && (
          <label className="block">
            <span className="mb-1 block text-sm font-medium text-ink">Update policy</span>
            <select value={draft.updatePolicy}
              onChange={(e) => set({ updatePolicy: e.target.value as SpecDraft["updatePolicy"] })}
              className={`${control} w-full`}>
              <option value="manual">Manual — I update when I choose</option>
              <option value="notify">Notify — tell me when a new image ships</option>
              <option value="auto">Auto — snapshot, apply, verify</option>
            </select>
          </label>
        )}
        <p className="self-end pb-2 text-xs text-ink-muted">
          Backups — whether, when, and how this app is snapshotted — are set on its Drive tab.
        </p>
      </div>
    </div>
  );
}

export function NotesEditor({ draft, set }: EditorProps) {
  return (
    <label className="block">
      <span className="mb-1 block text-sm font-medium text-ink">Notes (optional)</span>
      <textarea className={`${controlArea} h-20 w-full`} value={draft.notes} placeholder="Post-install steps, runbook pointers…"
        onChange={(e) => set({ notes: e.target.value })} />
    </label>
  );
}

export function FilesEditor({ draft, set }: EditorProps) {
  return (
    <div className="space-y-2">
      <Hint>Rendered to disk and mounted read-only. Versioned with the app config.</Hint>
      {draft.files.map((f, i) => (
        <div key={i} className="space-y-1 rounded-md border border-hair p-2">
          <div className="flex gap-2">
            <input className={`${rowInput} w-40`} placeholder="filename" value={f.name}
              onChange={(ev) => set({ files: draft.files.map((x, j) => (j === i ? { ...x, name: ev.target.value } : x)) })} />
            <input className={`${rowInput} flex-1`} placeholder="/etc/app/config.yml (mount path)" value={f.mount}
              onChange={(ev) => set({ files: draft.files.map((x, j) => (j === i ? { ...x, mount: ev.target.value } : x)) })} />
            <RemoveBtn onClick={() => set({ files: draft.files.filter((_, j) => j !== i) })} />
          </div>
          <textarea className={`${controlArea} h-32 w-full font-mono text-xs`} value={f.content}
            onChange={(ev) => set({ files: draft.files.map((x, j) => (j === i ? { ...x, content: ev.target.value } : x)) })} />
        </div>
      ))}
      <AddBtn label="file" onClick={() => set({ files: [...draft.files, { name: "", mount: "", content: "" }] })} />
    </div>
  );
}

// The whole spec as one form: what creating an app needs, where nothing
// exists yet to read back.
// source=false leaves the Source section to the caller — the Add app doors
// put the repository or image field up top and fold the rest under here.
export default function SpecEditor({ draft, onChange, source = true, app = "" }: {
  draft: SpecDraft; onChange: (d: SpecDraft) => void; source?: boolean; app?: string;
}) {
  const set = (patch: Partial<SpecDraft>) => onChange({ ...draft, ...patch });
  const props = { draft, set };
  const [showFiles, setShowFiles] = useState(draft.files.length > 0);
  const [showPorts, setShowPorts] = useState(draft.ports.length > 0);
  const isGit = draft.sourceType === "git";

  return (
    <div className="space-y-4">
      {source && <SourceEditor {...props} />}
      {isGit && <Section title="Deploy"><DeployEditor {...props} /></Section>}
      {isGit && <Section title="Managed Postgres"><PostgresEditor {...props} /></Section>}
      <Section title="Environment"><EnvEditor {...props} /></Section>
      <Section title="Secrets"><SecretsEditor {...props} /></Section>
      <Section title="Volumes"><VolumesEditor {...props} app={app} /></Section>
      <Section title="App access"><AccessEditor {...props} /></Section>

      {draft.aux.length > 0 && (
        <Section title="Sidecar services"
          hint="Part of this app's template — they share its lifecycle, private network, and backups.">
          {draft.aux.map((a) => (
            <p key={a.name} className="text-sm text-ink-muted">
              <span className="font-medium text-ink">{a.name}</span>
              <span className="ml-2 font-mono text-xs text-ink-muted">{a.image}</span>
            </p>
          ))}
        </Section>
      )}

      <Section title="Routes"><RoutesEditor {...props} /></Section>

      {showPorts ? (
        <Section title="Published ports"><PortsEditor {...props} /></Section>
      ) : (
        <button type="button" onClick={() => setShowPorts(true)} className="text-xs text-ink-muted hover:text-ink">
          + Publish raw ports (DNS, SMTP…)
        </button>
      )}

      <RuntimeEditor {...props} />
      <NotesEditor {...props} />

      {showFiles ? (
        <Section title="Config files"><FilesEditor {...props} /></Section>
      ) : (
        <button type="button" onClick={() => setShowFiles(true)} className="text-xs text-ink-muted hover:text-ink">
          + Add config files…
        </button>
      )}
    </div>
  );
}
