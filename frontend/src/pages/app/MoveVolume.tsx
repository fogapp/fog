// Moving one of an app's folders somewhere else in Drive.
//
// The question is "where should this live?", and it deserves an answer in
// those terms: the folder it is in now, the folders it could be in, and one
// sentence about what changes if it moves. The Config tab can still edit
// the same field as a docker bind, but nobody should have to go there to
// say "put my films in Content/Movies".
//
// Fog does the whole move: the app stops, the files travel, the spec is
// rewritten, and the app starts again. That has to be one operation — Drive
// refuses to move a folder an app mounts, and an app pointed at a folder
// its files have left is worse than either half alone.

import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { post, type FilesJob } from "../../api";
import AppFolderChoices from "../../components/AppFolderChoices";
import Button, { buttonClass } from "../../components/Button";
import Modal from "../../components/Modal";
import PathBrowser from "../../components/PathBrowser";
import { Spinner } from "../../components/Async";
import { appsKey } from "../../lib/useApps";
import { filesKey } from "../../lib/useFiles";
import { useJob } from "../../lib/useJob";
import type { AppVolume } from "../../lib/useAppData";
import {
  APPS_DIR, AREA_LABELS, AREA_WHY, CONTENT_DIR, appFolderFor, driveAreaOf, filesCrumbs, placementLabel,
} from "../../lib/volumePlacement";
import { appKey } from "../../lib/useAppData";
import { useAction } from "../../lib/useAction";
import SettingsLink from "../../components/SettingsLink";
import { control } from "../../components/Field";

const input = control;

type Mode = "move" | "adopt";

export default function MoveVolume({ app, v, filesRoot, onClose }: {
  app: string;
  v: AppVolume;
  // "" when Drive is unusable; the form says so rather than pretending.
  filesRoot: string;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [to, setTo] = useState(v.rel ?? "");
  const [mode, setMode] = useState<Mode>("move");
  const [browsing, setBrowsing] = useState(false);
  const [started, setStarted] = useState<FilesJob | null>(null);
  const { run, busy, error } = useAction();

  // While the move runs, the app is stopping, moving and starting; the page
  // behind this dialog is watching the same app, so both stay true together.
  const job =
    useJob("files", started?.id, {
      initial: started ?? undefined,
      onDone: () => {
        qc.invalidateQueries({ queryKey: filesKey });
        qc.invalidateQueries({ queryKey: appsKey });
        qc.invalidateQueries({ queryKey: appKey(app) });
      },
    }).data ?? started;

  const target = to.trim().replace(/^\/+|\/+$/g, "");
  const area = driveAreaOf(target);
  const ownFolder = appFolderFor(app);
  const unchanged = target === (v.rel ?? "");

  const start = async () => {
    await run(async () => {
      const res = await post<FilesJob>(
        `/api/apps/${encodeURIComponent(app)}/volumes/${encodeURIComponent(v.volume.name)}/move`,
        { service: v.service, to: target, mode },
      );
      setStarted(res);
    }, "Could not start the move");
  };

  const title = `Move ${v.volume.name}`;

  if (job) {
    return (
      <Modal title={title} onClose={onClose}>
        <div className="space-y-3">
          {job.state === "running" ? (
            <p className="flex items-center gap-2 text-sm text-ink">
              <Spinner /> {job.current || "working"}…
            </p>
          ) : job.state === "done" ? (
            <p className="text-sm text-ink">
              {v.volume.name} now lives at <span className="font-mono text-ink">Drive/{target}</span>, and {app} is
              pointed at it.
            </p>
          ) : (
            <p className="text-sm text-warn">{job.error || `The move ${job.state}.`}</p>
          )}
          {job.state === "running" && job.total > 0 && (
            <div className="h-1.5 overflow-hidden rounded-full bg-fog-800">
              <div className="h-full bg-accent" style={{ width: `${Math.min(100, Math.round((job.done / job.total) * 100))}%` }} />
            </div>
          )}
          {job.state !== "running" && (
            <p className="text-xs text-ink-muted">
              {job.state === "done"
                ? "Drive and the app's Config tab both show the new folder."
                : "The app is stopped. Its files are still whole, at one end or the other — check Drive before trying again."}
            </p>
          )}
          <div className="flex justify-end">
            <Button size="sm" onClick={onClose} disabled={job.state === "running"}>
              {job.state === "running" ? "Working…" : "Done"}
            </Button>
          </div>
        </div>
      </Modal>
    );
  }

  return (
    <Modal title={title} onClose={onClose}>
      <div className="space-y-4">
        <p className="text-sm text-ink-muted">
          {app} sees this folder as <span className="font-mono text-ink">{v.volume.mount}</span>. It is
          in <span className="text-ink">{placementLabel(v.placement)}</span> now.
        </p>

        {!filesRoot ? (
          <p className="text-sm text-warn">
            Drive isn't usable right now, so there is nowhere to move this to. Fix it in <SettingsLink to="storage/drive" /> first.
          </p>
        ) : (
          <>
            <div className="space-y-2">
              <label className="block text-sm font-medium text-ink">Move it to</label>
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
                {area === "app" && !isUnder(target, ownFolder) && (
                  <p className="text-xs text-warn">
                    That is another app's folder under {APPS_DIR}. It will work, but it travels in that app's
                    backups, not {app}'s.
                  </p>
                )}
              </div>
            )}

            <fieldset className="space-y-2">
              <legend className="text-sm font-medium text-ink">What to do with the files that are there now</legend>
              <Radio on={mode === "move"} onClick={() => setMode("move")} label="Take them with it">
                Fog stops {app}, moves everything to the new folder, and starts it again. The new folder has
                to be empty.
              </Radio>
              <Radio on={mode === "adopt"} onClick={() => setMode("adopt")} label="Leave them and use what's already there">
                For a library that is already in the new folder. Nothing moves — {app} simply reads the new
                folder from now on, and what is in the old one stays where it is.
              </Radio>
            </fieldset>

            {error && <p className="text-sm text-warn">{error}</p>}
          </>
        )}

        <div className="flex justify-end gap-2">
          <Button size="sm" variant="ghost" onClick={onClose}>Cancel</Button>
          <Button size="sm" onClick={start} disabled={busy || !filesRoot || !target || unchanged}>
            {busy ? "Starting…" : mode === "move" ? "Move the files" : "Point it there"}
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

function isUnder(rel: string, dir: string): boolean {
  return rel === dir || rel.startsWith(dir + "/");
}

function Radio({ on, onClick, label, children }: {
  on: boolean; onClick: () => void; label: string; children: React.ReactNode;
}) {
  return (
    <button type="button" onClick={onClick}
      className={`flex w-full gap-2.5 rounded-lg border p-2.5 text-left ${
        on ? "border-accent bg-accent/5" : "border-hair hover:border-edge"
      }`}>
      <span className={`mt-0.5 h-3.5 w-3.5 shrink-0 rounded-full border ${on ? "border-accent bg-accent" : "border-edge"}`} />
      <span className="min-w-0">
        <span className="block text-sm font-medium text-ink">{label}</span>
        <span className="mt-0.5 block text-xs text-ink-muted">{children}</span>
      </span>
    </button>
  );
}
