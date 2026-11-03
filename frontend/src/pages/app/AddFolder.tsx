// Giving an app another folder in Drive — the Drive tab's "+".
//
// The Config tab can add the same volume as a docker bind, name and mount
// and all, but the question someone brings to the Drive tab is smaller:
// "let this app see that folder". So the dialog asks for the folder first,
// in the same picker Move uses, and fills in the rest — the volume's name
// from the folder's, and a mount point by the half of Drive it is in — for
// the operator to change or leave alone.
//
// Saving writes a new revision (the current spec plus this one volume,
// secrets untouched) and redeploys, because a container only gains a mount
// by being recreated.

import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { put, type AppDetail as AppDetailT, type Volume } from "../../api";
import AppFolderChoices from "../../components/AppFolderChoices";
import Button, { buttonClass } from "../../components/Button";
import Modal from "../../components/Modal";
import PathBrowser from "../../components/PathBrowser";
import Toggle from "../../components/Toggle";
import { appsKey } from "../../lib/useApps";
import { AREA_LABELS, AREA_WHY, CONTENT_DIR, driveAreaOf, filesCrumbs } from "../../lib/volumePlacement";
import { useAction } from "../../lib/useAction";
import SettingsLink from "../../components/SettingsLink";
import { control } from "../../components/Field";

const input = control;

// The same shape the server accepts (apps.nameRe): lowercase, digits and
// inner hyphens, at most 32.
const nameOk = (s: string) => /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/.test(s);

function slug(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 32).replace(/-+$/, "") || "folder";
}

export default function AddFolder({ app, detail, filesRoot, onSaved, onClose }: {
  app: string;
  detail: AppDetailT;
  // "" when Drive is unusable; the form says so rather than pretending.
  filesRoot: string;
  onSaved: () => Promise<unknown>;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const spec = detail.spec;
  // Volume names are unique across the app and its sidecars; mounts only
  // have to differ within the main container, which is where this goes.
  const taken = new Set([...(spec.volumes ?? []), ...(spec.aux ?? []).flatMap((a) => a.volumes ?? [])].map((v) => v.name));
  const mounts = new Set((spec.volumes ?? []).map((v) => v.mount.replace(/\/+$/, "")));

  const [to, setTo] = useState("");
  const [browsing, setBrowsing] = useState(false);
  // Name and mount follow the folder until the operator types in them.
  const [nameEdit, setNameEdit] = useState<string | null>(null);
  const [mountEdit, setMountEdit] = useState<string | null>(null);
  const [readOnly, setReadOnly] = useState(false);
  const { run, busy, error } = useAction();

  const target = to.trim().replace(/^\/+|\/+$/g, "");
  const area = driveAreaOf(target);
  const leaf = slug(target.split("/").filter(Boolean).pop() ?? "");
  let suggested = leaf;
  for (let i = 2; taken.has(suggested); i++) suggested = `${leaf.slice(0, 29)}-${i}`;
  const name = nameEdit ?? (target ? suggested : "");
  // A library under Content goes where media servers look for one; anything
  // else is the app's data.
  const mount = (mountEdit ?? (target ? `${area === "content" ? "/media" : "/data"}/${leaf}` : "")).trim();

  const problem =
    !target ? "" :
    !nameOk(name) ? "The name is lowercase letters, digits and hyphens, up to 32." :
    taken.has(name) ? `${app} already has a folder called ${name}.` :
    !mount.startsWith("/") ? "The path inside the app starts with /." :
    mounts.has(mount.replace(/\/+$/, "")) ? `${app} already has a folder at ${mount}.` :
    (spec.volumes ?? []).some((v) => v.files === target) ? `${app} already has this folder.` :
    "";

  const save = async () => {
    await run(async () => {
      const vol: Volume = { name, mount, files: target, ...(readOnly ? { readOnly: true } : {}) };
      await put(`/api/apps/${encodeURIComponent(app)}`, {
        spec: { ...spec, volumes: [...(spec.volumes ?? []), vol] },
        secrets: {},
        comment: `Add Drive folder ${target} at ${mount}`,
        deploy: true,
      });
      await Promise.all([onSaved(), qc.invalidateQueries({ queryKey: appsKey })]);
      onClose();
    }, "Could not add the folder");
  };

  return (
    <Modal title={`Add a folder to ${app}`} onClose={onClose}>
      <div className="space-y-4">
        {!filesRoot ? (
          <p className="text-sm text-warn">
            Drive isn't usable right now, so there is no folder to add. Fix it in <SettingsLink to="storage/drive" /> first.
          </p>
        ) : (
          <>
            <div className="space-y-2">
              <label className="block text-sm font-medium text-ink">Folder in Drive</label>
              <div className="flex items-center gap-2">
                <span className="shrink-0 font-mono text-xs text-ink-muted">Drive/</span>
                <input autoFocus className={`${input} flex-1`} value={to} placeholder={`${CONTENT_DIR}/…`}
                  onChange={(e) => setTo(e.target.value)} />
                <button type="button" onClick={() => setBrowsing(true)}
                  className={buttonClass()}>
                  Browse…
                </button>
              </div>
              <AppFolderChoices app={app} rel={target} onPick={setTo} />
            </div>

            {target && (
              <>
                <div className="space-y-1 rounded-lg border border-hair p-3">
                  <p className="flex flex-wrap items-center gap-x-1 text-sm text-ink">
                    {filesCrumbs(target).map((seg, i, all) => (
                      <span key={i} className="flex items-center gap-x-1">
                        {i > 0 && <span className="text-ink-faint">›</span>}
                        <span className={i === all.length - 1 ? "font-medium" : "text-ink-muted"}>{seg}</span>
                      </span>
                    ))}
                    <span className="ml-1 rounded bg-fog-800 px-1.5 py-0.5 text-[10px] font-medium text-ink-muted">
                      {AREA_LABELS[area]}
                    </span>
                  </p>
                  <p className="text-xs text-ink-muted">{AREA_WHY[area]}</p>
                </div>

                <div className="grid gap-3 sm:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
                  <label className="block space-y-1">
                    <span className="block text-sm font-medium text-ink">{app} sees it at</span>
                    <input className={`${input} w-full font-mono`} value={mount} onChange={(e) => setMountEdit(e.target.value)} />
                  </label>
                  <label className="block space-y-1">
                    <span className="block text-sm font-medium text-ink">Name</span>
                    <input className={`${input} w-full font-mono`} value={name} onChange={(e) => setNameEdit(e.target.value)} />
                  </label>
                </div>
                <Toggle checked={readOnly} onChange={setReadOnly} label={`Read-only — ${app} can see the files but not change them`} />
              </>
            )}

            {(problem || error) && <p className="text-sm text-warn">{problem || error}</p>}
            {target && !problem && (
              <p className="text-xs text-ink-muted">Fog saves this and redeploys {app}; it restarts to pick up the folder.</p>
            )}
          </>
        )}

        <div className="flex justify-end gap-2">
          <Button size="sm" variant="ghost" onClick={onClose}>Cancel</Button>
          <Button size="sm" onClick={save} disabled={busy || !filesRoot || !target || !!problem}>
            {busy ? "Adding…" : "Add and redeploy"}
          </Button>
        </div>
      </div>
      {browsing && (
        <PathBrowser
          title="Choose a folder in Drive"
          initial={target ? `${filesRoot}/${target}` : filesRoot}
          defaultOwner="1000:1000"
          onSelect={(p) => {
            setTo(p.startsWith(filesRoot) ? p.slice(filesRoot.length).replace(/^\/+/, "") : p);
            setBrowsing(false);
          }}
          onClose={() => setBrowsing(false)} />
      )}
    </Modal>
  );
}
