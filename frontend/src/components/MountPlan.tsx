// The install pane's answer to "where does all of this end up?": one card,
// under the app's name, listing every folder the app will mount and where
// each one will live. Nothing to type on the default path — the app's own
// data goes in its folder under Apps, a library it reads goes under
// Content, both are created on install — but every line says which of the
// two it is and what the app will see it as, and every line can be changed
// by anyone who keeps a library somewhere else already.
//
// The frame is the app's own folder in Drive, because that is the answer to
// the question most of the rows are asking. Rows that point somewhere else
// say so on their own line.

import { cleanHostPath, hostAccessVerdict } from "../lib/hostAccess";
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  get,
  type CatalogEntry, type CatalogOption, type FileDetail, type Volume,
} from "../api";
import AppFolderChoices from "./AppFolderChoices";
import PathBrowser from "./PathBrowser";
import { filesStatKey, useFilesStatus } from "../lib/useFiles";
import {
  AREA_LABELS, AREA_WHY, APPS_DIR, CONTENT_DIR, appDataFolder, appFolderFor, decodePlacement,
  defaultContentFolder, driveAreaOf, encodePlacement, filesCrumbs, placementOf,
  type Placement, type PlacementContext,
} from "../lib/volumePlacement";
import { nasQuery } from "../queries";
import SettingsLink from "./SettingsLink";
import { control } from "./Field";
import { buttonClass } from "./Button";

const rowInput = control;

// One folder the app will mount, as the pane reasons about it.
export interface PlannedMount {
  // "service|name" — the key placements are stored under.
  key: string;
  service: string;
  volume: Volume;
  // The catalog's own words for it, when the entry asks about this folder.
  option?: CatalogOption;
}

const mountKey = (service: string, name: string) => `${service}|${name}`;

// planMounts lists the folders worth showing: everything the app's own
// container mounts, plus any sidecar folder the entry asks a question
// about. The rest of a sidecar's storage is plumbing — a Postgres cluster,
// a Valkey dump — and gets one sentence at the foot of the card instead of
// a row of its own.
export function planMounts(entry: CatalogEntry): { rows: PlannedMount[]; sidecars: number } {
  const asked = new Set(
    (entry.options ?? []).filter((o) => o.target.startsWith("volume:")).map((o) => o.target.slice(7)),
  );
  const optionFor = (name: string) =>
    (entry.options ?? []).find((o) => o.target === `volume:${name}`);

  const rows: PlannedMount[] = (entry.spec.volumes ?? []).map((v) => ({
    key: mountKey("", v.name), service: "", volume: v, option: optionFor(v.name),
  }));
  let sidecars = 0;
  for (const aux of entry.spec.aux ?? []) {
    for (const v of aux.volumes ?? []) {
      if (asked.has(v.name)) {
        rows.push({ key: mountKey(aux.name, v.name), service: aux.name, volume: v, option: optionFor(v.name) });
      } else {
        sidecars++;
      }
    }
  }
  return { rows, sidecars };
}

// defaultPlacementFor is where a mount goes when the operator has said
// nothing: what the template names, or — for a volume that names nowhere —
// the app's own folder in Drive, Apps/<App>/<volume> (apps.AppDataFolder
// on the server names the first half). Every row is sent explicitly, so
// what the pane showed is what gets stored. Encoded the way placements travel.
export function defaultPlacementFor(m: PlannedMount, app: string): string {
  if (m.volume.files) return `files:${m.volume.files}`;
  if (m.volume.host) return `host:${m.volume.host}`;
  // Sidecar storage stays managed; only the app's own volumes get a folder.
  if (m.service) return "";
  return `files:${appDataFolder(app || "app", m.volume.name)}`;
}

export default function MountPlan({ entry, app, placements, onChange }: {
  entry: CatalogEntry;
  // The app's name as it will be created — the folder its data goes in.
  app: string;
  // Explicit choices, keyed by mountKey. A mount absent from here takes
  // defaultPlacementFor, so renaming the app moves every default with it.
  placements: Record<string, string>;
  onChange: (next: Record<string, string>) => void;
}) {
  const { rows, sidecars } = planMounts(entry);
  const filesStatus = useFilesStatus();
  const filesRoot = filesStatus.data?.ok ? filesStatus.data.root : "";
  const filesKnown = !!filesStatus.data || filesStatus.isError;
  const nas = useQuery(nasQuery);
  const ctx: PlacementContext = { filesRoot, shares: nas.data?.shares ?? [], peerSubs: [] };
  const folder = appFolderFor(app || entry.id);

  if (rows.length === 0 && sidecars === 0) {
    return (
      <div className="rounded-lg border border-hair p-3 text-xs text-ink-muted">
        {entry.title} keeps nothing on disk — there is no folder to place.
      </div>
    );
  }

  return (
    <div className="rounded-lg border border-hair">
      <div className="border-b border-hair px-3 py-2.5">
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
          <span className="text-sm font-medium text-ink">Where its files live</span>
          <span className="flex-1" />
          <span className="font-mono text-xs text-ink-muted">
            Drive / {APPS_DIR} / <span className="text-ink">{folder.slice(APPS_DIR.length + 1)}</span>
          </span>
        </div>
        {/* Said once, at the top, rather than on every row that agrees
            with it: the rows that don't say so themselves. */}
        <p className="mt-1 text-xs text-ink-muted">
          {entry.title}'s own folder in Drive, made on install — what's inside it belongs to this app and
          travels in its backups. Libraries it only reads live in {CONTENT_DIR}, where other apps can
          reach them too.
        </p>
      </div>
      <div className="divide-y divide-hair">
        {rows.map((m) => (
          <MountRow
            key={m.key} m={m} app={app} entry={entry} ctx={ctx}
            filesRoot={filesRoot} filesKnown={filesKnown} filesError={filesStatus.data?.error}
            value={placements[m.key] ?? defaultPlacementFor(m, app)}
            onChange={(v) => onChange({ ...placements, [m.key]: v })}
            acked={placements[ackKey(m.key)] === "1"}
            onAck={(yes) => onChange({ ...placements, [ackKey(m.key)]: yes ? "1" : "" })}
          />
        ))}
      </div>
      {sidecars > 0 && (
        <p className="border-t border-hair px-3 py-2 text-xs text-ink-muted">
          {sidecars === 1 ? "One more folder holds" : `${sidecars} more folders hold`} the database and cache{" "}
          {entry.title} runs alongside itself. Fog keeps {sidecars === 1 ? "it" : "them"} in its own
          private directory, and {sidecars === 1 ? "it travels" : "they travel"} in this app's backups.
        </p>
      )}
    </div>
  );
}

// ackKey is where the operator's acknowledgement of a host folder for row
// key travels among the placements (buildInstallPayload reads it).
export const ackKey = (key: string) => "ack|" + key;

function MountRow({ m, app, entry, ctx, filesRoot, filesKnown, filesError, value, onChange, acked, onAck }: {
  acked: boolean;
  onAck: (yes: boolean) => void;
  m: PlannedMount;
  app: string;
  entry: CatalogEntry;
  ctx: PlacementContext;
  filesRoot: string;
  filesKnown: boolean;
  filesError?: string;
  value: string;
  onChange: (v: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [browsing, setBrowsing] = useState<"" | "files" | "host">("");
  const placement: Placement = value ? decodePlacement(value, ctx) : placementOf(m.volume, ctx);
  const set = (p: Placement) => onChange(encodePlacement(p));
  const rel = placement.kind === "files" ? placement.rel : "";
  const mount = m.volume.mount;
  const label = m.option?.label ?? titled(m.volume.name);
  const own = appFolderFor(app || entry.id);

  // Whether the folder is already there, and which apps share it — one
  // Content/Media across the arr stack is the point, and worth saying.
  const stat = useQuery({
    queryKey: filesStatKey(rel),
    queryFn: () => get<FileDetail>(`/api/files/stat?path=${encodeURIComponent(rel)}`),
    enabled: !!filesRoot && !!rel,
    retry: false,
  });
  const sharedWith = (stat.data?.mountedBy ?? []).filter(Boolean);

  const where =
    placement.kind === "files" ? filesCrumbs(placement.rel || "…")
    : placement.kind === "nas" ? [`NAS: ${placement.share.name}`, ...placement.sub.split("/").filter(Boolean)]
    : placement.kind === "host" ? [placement.path || "…"]
    : placement.kind === "peer" ? [`${placement.sub.peer ?? "peer"}'s ${placement.sub.title}`]
    : ["Fog-managed"];

  const sentence = (() => {
    switch (placement.kind) {
      case "files":
        return (
          <>
            {stat.isSuccess
              ? sharedWith.length > 0
                ? `Already there, used by ${sharedWith.join(", ")} — ${entry.title} will share it. `
                : "Already there — the app starts with what's in it. "
              : filesRoot && rel
                ? "Created on install. "
                : ""}
            {/* The card's header already says what the app's own folder
                is; only a row that leaves it needs the sentence. */}
            {!isUnder(placement.rel, own) && AREA_WHY[driveAreaOf(placement.rel)]}
          </>
        );
      case "nas":
        return <>On the NAS share {placement.share.name}; Fog mounts it before the app starts, and it is not in Fog's backups.</>;
      case "host":
        return <>A folder on this server, outside Drive — not in Fog's backups.</>;
      case "peer":
        return <>A read-only copy of {placement.sub.peer ?? "a peer"}'s share.</>;
      case "managed":
        return <>Private to the app, under Fog's data directory, in its backups — not browsable from Drive.</>;
    }
  })();

  const area = placement.kind === "files" ? driveAreaOf(placement.rel) : undefined;
  const mode = placement.kind === "nas" ? `nas:${placement.share.name}` : placement.kind;

  return (
    <div className="px-3 py-2.5">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="flex flex-wrap items-center gap-x-2 text-sm font-medium text-ink">
            {label}
            {area ? (
              <Badge tone={area === "content" ? "content" : "app"}>{AREA_LABELS[area]}</Badge>
            ) : placement.kind === "managed" ? (
              <Badge tone="off">Fog-managed</Badge>
            ) : (
              <Badge tone="off">Outside Drive</Badge>
            )}
          </p>
          <p className="mt-0.5 flex flex-wrap items-center gap-x-1 text-sm text-ink">
            <FolderGlyph kind={placement.kind} />
            {where.map((seg, i) => (
              <span key={i} className="flex items-center gap-x-1">
                {i > 0 && <span className="text-ink-faint">›</span>}
                <span className={i === where.length - 1 ? "font-medium" : "text-ink-muted"}>{seg}</span>
              </span>
            ))}
          </p>
          {mount && (
            <p className="mt-0.5 font-mono text-[11px] text-ink-muted">
              {entry.title} sees it as {mount}{m.volume.readOnly ? " (read-only)" : ""}
              {m.service ? ` · ${m.service}` : ""}
            </p>
          )}
          <p className="mt-1 text-xs text-ink-muted">{m.option?.hint ? m.option.hint + " " : ""}{sentence}</p>
          {placement.kind === "files" && filesKnown && !filesRoot && (
            <p className="mt-1 text-xs text-warn">
              Drive isn't usable right now{filesError ? ` (${filesError})` : ""} — fix it in
              <SettingsLink to="storage/drive" /> before installing, or put this folder somewhere else.
            </p>
          )}
        </div>
        <button type="button" onClick={() => setOpen(!open)}
          className={buttonClass("secondary", "sm")}>
          {open ? "Done" : "Change"}
        </button>
      </div>

      {open && (
        <div className="mt-3 space-y-2 border-t border-hair pt-3">
          <div className="flex flex-wrap gap-1.5">
            <Choice on={mode === "files" && driveAreaOf(rel) === "app"} disabled={filesKnown && !filesRoot}
              title={filesKnown && !filesRoot ? (filesError || "Drive isn't usable right now — see Settings → Storage → Drive.") : undefined}
              onClick={() => set({ kind: "files", rel: appDataFolder(app || entry.id, m.volume.name) })}>
              {entry.title}'s own folder
            </Choice>
            <Choice on={mode === "files" && driveAreaOf(rel) === "content"} disabled={filesKnown && !filesRoot}
              title={filesKnown && !filesRoot ? (filesError || "Drive isn't usable right now — see Settings → Storage → Drive.") : undefined}
              onClick={() => set({ kind: "files", rel: defaultContentFolder(m.option?.key ?? m.volume.name) })}>
              A shared library in {CONTENT_DIR}
            </Choice>
            {ctx.shares.map((s) => (
              <Choice key={s.name} on={mode === `nas:${s.name}`} onClick={() => set({ kind: "nas", share: s, sub: "" })}>
                NAS: {s.name}
              </Choice>
            ))}
            <Choice on={mode === "host"} onClick={() => set({ kind: "host", path: placement.kind === "host" ? placement.path : "" })}>
              Elsewhere on the server
            </Choice>
          </div>
          {placement.kind === "files" && (
            <>
              <div className="flex items-center gap-2">
                <span className="shrink-0 font-mono text-xs text-ink-muted">Drive/</span>
                <input className={`${rowInput} flex-1`} value={placement.rel} placeholder={`${CONTENT_DIR}/…`}
                  onChange={(e) => set({ kind: "files", rel: e.target.value })} />
                {filesRoot && <BrowseBtn onClick={() => setBrowsing("files")} />}
              </div>
              {filesRoot && <AppFolderChoices app={app} rel={placement.rel} onPick={(r) => set({ kind: "files", rel: r })} />}
            </>
          )}
          {placement.kind === "nas" && (
            <div className="flex items-center gap-2">
              <span className="shrink-0 font-mono text-xs text-ink-muted">{placement.share.mountPoint}/</span>
              <input className={`${rowInput} flex-1`} value={placement.sub} placeholder="folder on the share (optional)"
                onChange={(e) => set({ kind: "nas", share: placement.share, sub: e.target.value.replace(/^\/+/, "") })} />
            </div>
          )}
          {placement.kind === "host" && (
            <div className="flex items-center gap-2">
              <input className={`${rowInput} flex-1`} value={placement.path} placeholder="/absolute/host/path"
                onChange={(e) => set({ kind: "host", path: e.target.value })} />
              <BrowseBtn onClick={() => setBrowsing("host")} />
            </div>
          )}
          {placement.kind === "host" && placement.path.trim() && (() => {
            const v = hostAccessVerdict(placement.path, false, { shares: ctx.shares.map((s) => s.mountPoint), filesRoot, app });
            if (v.refused) return <p className="text-xs text-err">{cleanHostPath(placement.path)} is {v.refused}. Fog never mounts it.</p>;
            if (!v.needsAck) return null;
            return (
              <label className="flex items-center gap-1.5 text-xs text-warn">
                <input type="checkbox" checked={acked} onChange={(e) => onAck(e.target.checked)} />
                Let {entry.title} read and change everything in {cleanHostPath(placement.path)} — {v.needsAck}
              </label>
            );
          })()}
          {placement.kind === "files" && placement.rel && driveAreaOf(placement.rel) === "drive" && (
            <p className="text-xs text-ink-muted">
              Any folder in Drive works. {APPS_DIR} is where an app's own data belongs and {CONTENT_DIR} is
              where shared libraries do, and only a folder under {APPS_DIR}/{appFolderFor(app || entry.id).slice(APPS_DIR.length + 1)} travels
              in this app's backups.
            </p>
          )}
        </div>
      )}
      {browsing !== "" && (
        <PathBrowser
          title={browsing === "files" ? "Choose a folder in Drive" : "Choose a host folder"}
          initial={browsing === "files" ? (rel && filesRoot ? `${filesRoot}/${rel}` : filesRoot) : placement.kind === "host" ? placement.path : "/"}
          defaultOwner="1000:1000"
          onSelect={(p) => { set(placementOf({ host: p }, ctx)); setBrowsing(""); }}
          onClose={() => setBrowsing("")} />
      )}
    </div>
  );
}

function isUnder(rel: string, dir: string): boolean {
  return rel === dir || rel.startsWith(dir + "/");
}

function titled(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : "Data";
}

function Badge({ tone, children }: { tone: "app" | "content" | "off"; children: React.ReactNode }) {
  const cls =
    tone === "content" ? "bg-accent/10 text-accent"
    : tone === "app" ? "bg-fog-800 text-ink-muted"
    : "bg-fog-800 text-ink-muted";
  return <span className={`rounded px-1.5 py-0.5 text-[10px] font-medium ${cls}`}>{children}</span>;
}

function Choice({ on, disabled, title, onClick, children }: {
  on: boolean; disabled?: boolean; title?: string; onClick: () => void; children: React.ReactNode;
}) {
  return (
    <button type="button" onClick={onClick} disabled={disabled} title={title}
      className={`rounded-md px-2.5 py-1 text-xs font-medium disabled:cursor-not-allowed disabled:opacity-50 ${
        on ? "bg-accent text-accent-on" : "border border-edge text-ink-muted hover:bg-fog-800"
      }`}>
      {children}
    </button>
  );
}

function BrowseBtn({ onClick }: { onClick: () => void }) {
  return (
    <button type="button" onClick={onClick}
      className={buttonClass()}>
      Browse…
    </button>
  );
}

// A small folder mark, toned by where the folder is: the accent for Drive
// (the same tone the Drive page gives folders), fog for anywhere else.
function FolderGlyph({ kind }: { kind: Placement["kind"] }) {
  const tone = kind === "files" || kind === "peer" ? "text-accent" : "text-ink-muted";
  return (
    <svg viewBox="0 0 24 24" className={`h-4 w-4 shrink-0 ${tone}`} fill="currentColor" aria-hidden>
      <path d="M3 6.5A2.5 2.5 0 0 1 5.5 4h3.6a2.5 2.5 0 0 1 1.77.73L12.6 6.5h5.9A2.5 2.5 0 0 1 21 9v8.5a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 17.5v-11Z" />
    </svg>
  );
}
