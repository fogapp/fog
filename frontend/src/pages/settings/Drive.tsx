// Settings › Storage › Drive: where fog's own files live, how much is
// there, and the two things an operator does to it — pick another
// location, and ask for a fresh index — then where Apps and Content live
// within it, and where backups are kept, then the devices that mount it.
// Moving a populated root, Apps, Content or the backups is a job with a
// choice (move the files, or adopt what is already at the new place), never
// a silent setting write.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, post, put, type FilesJob, type Placement, type Settings } from "../../api";
import { LoadError, Loading, Spinner, errorText } from "../../components/Async";
import Button, { ButtonLink } from "../../components/Button";
import Field from "../../components/Field";
import PathBrowser from "../../components/PathBrowser";
import { Section } from "../../components/Section";
import SettingRow, { SettingRows } from "../../components/SettingRow";
import Toggle from "../../components/Toggle";
import { useSettingToggle } from "../../lib/useSettingToggle";
import { formatBytes, timeAgo } from "../../lib/format";
import { paths } from "../../lib/paths";
import { useFlash } from "../../lib/useFlash";
import { useJob } from "../../lib/useJob";
import { filesKey, useFilesStatus } from "../../lib/useFiles";
import DriveAccess from "./DriveAccess";
import { SettingsPage } from "./Frame";
import { disksQuery, settingsQuery } from "../../queries";

type DriveKey = "files_root" | "files_owner";

export default function Drive() {
  const qc = useQueryClient();
  const status = useFilesStatus({ refetchInterval: 10_000 });
  const settings = useQuery(settingsQuery);
  // Only what was touched is local; the rest reads through to the settings
  // query, so a refetch (the mode switch, a save elsewhere) never resets what
  // is being typed — and Save sends only these, so an untouched root never
  // goes through the root-change check at all.
  const [edits, setEdits] = useState<Partial<Pick<Settings, DriveKey>>>({});
  const [browsing, setBrowsing] = useState(false);
  const [error, setError] = useState("");
  const [needsMove, setNeedsMove] = useState<string | null>(null);
  const [jobId, setJobId] = useState<string | null>(null);
  const [saved, flashSaved] = useFlash(2000);

  const field = (k: DriveKey) => edits[k] ?? settings.data?.[k] ?? "";
  const edit = (k: DriveKey, v: string) => setEdits((cur) => ({ ...cur, [k]: v }));
  const root = field("files_root");
  const owner = field("files_owner");
  const setRoot = (v: string) => edit("files_root", v);

  // The disk list says which disk holds the Drive, so a moved root
  // refreshes it too.
  const refresh = () => Promise.all([
    qc.invalidateQueries({ queryKey: settingsQuery.queryKey }),
    qc.invalidateQueries({ queryKey: filesKey }),
    qc.invalidateQueries({ queryKey: disksQuery.queryKey }),
  ]);

  const save = useMutation({
    mutationFn: (body: Partial<Pick<Settings, DriveKey>>) => put("/api/settings", body),
    onSuccess: async (_, body) => {
      await refresh();
      // Drop what was saved, keeping anything typed while the save was out.
      setEdits((cur) => {
        const next = { ...cur };
        for (const k of Object.keys(body) as DriveKey[]) if (next[k]?.trim() === body[k]) delete next[k];
        return next;
      });
      setError("");
      setNeedsMove(null);
      flashSaved("Saved ✓");
    },
    onError: (e) => {
      // 409: the current root has files in it — offer the job instead.
      if (e instanceof ApiError && e.status === 409) {
        setNeedsMove(root.trim());
        setError("");
      } else {
        setError(errorText(e, "Save failed"));
      }
    },
  });

  const move = useMutation({
    mutationFn: (mode: "move" | "adopt") => post<FilesJob>("/api/files/root/move", { to: needsMove, mode }),
    onSuccess: (job) => {
      setJobId(job.id);
      setNeedsMove(null);
      setError("");
    },
    onError: (e) => setError(errorText(e, "Could not start the move")),
  });

  const reindex = useMutation({
    mutationFn: () => post("/api/files/reindex"),
    onSuccess: () => refresh(),
    onError: (e) => setError(errorText(e, "Could not start indexing")),
  });

  // A settled move has carried out (or failed) the root change the Save
  // asked for; either way the field should now show the stored root.
  const job = useJob<FilesJob>("files", jobId, {
    onDone: async () => {
      await refresh();
      setEdits(({ files_root: _, ...rest }) => rest);
    },
  });

  const submit = () => {
    const body: Partial<Pick<Settings, DriveKey>> = {};
    for (const [k, v] of Object.entries(edits) as [DriveKey, string][]) body[k] = v.trim();
    save.mutate(body);
  };

  const st = status.data;
  const loading = !settings.data && !settings.isError;
  const onSystemDisk = st?.fsMount === "/";

  return (
    <SettingsPage
      description="Fog's own folder tree: ordinary files at one location on disk, with a rebuildable index beside it."
      actions={
        <ButtonLink to={paths.files()}>Open Drive</ButtonLink>
      }
    >
      <Section id="location" title="Location">
        <div className="max-w-lg space-y-4">
          {status.isPending ? (
            <Loading label="Reading the files root…" />
          ) : status.isError ? (
            <LoadError label="Could not read the files root." error={status.error} onRetry={() => status.refetch()} />
          ) : st ? (
            <div className="space-y-1 rounded-lg border border-veil-edge bg-veil p-3 text-sm">
              <p className="font-mono text-xs text-ink">{st.root}</p>
              {st.ok ? (
                <p className="text-ink-muted">
                  {st.stats.files.toLocaleString()} file{st.stats.files === 1 ? "" : "s"} in {st.stats.dirs.toLocaleString()} folder{st.stats.dirs === 1 ? "" : "s"} ·{" "}
                  {formatBytes(st.stats.bytes)}
                  {st.freeBytes !== undefined && ` · ${formatBytes(st.freeBytes)} free`}
                </p>
              ) : (
                <p className="text-warn">⚠ {st.error}</p>
              )}
              <p className="text-xs text-ink-muted">
                {onSystemDisk ? "On the system disk." : `On ${st.fsMount}.`}{" "}
                {st.walking ? (
                  <span className="inline-flex items-center gap-1"><Spinner size="h-2.5 w-2.5" tone="border-fog-500" /> indexing…</span>
                ) : st.walkedAt ? (
                  `Indexed ${timeAgo(st.walkedAt)}.`
                ) : (
                  "Not indexed yet."
                )}
                {st.stats.unhashed > 0 && ` ${st.stats.unhashed.toLocaleString()} file${st.stats.unhashed === 1 ? "" : "s"} still to be hashed.`}
              </p>
            </div>
          ) : null}

          {loading ? (
            <Loading label="Loading settings…" />
          ) : (
            <>
              <div className="flex items-end gap-2">
                <div className="flex-1">
                  <Field
                    label="Drive location"
                    value={root}
                    onChange={(e) => setRoot(e.target.value)}
                    placeholder={st ? `(default: ${st.defaultRoot})` : "(default: Fog data dir)"}
                    hint="A folder on this server. Put it on a NAS share to keep large libraries off the system disk."
                  />
                </div>
                <Button className="mb-5 shrink-0" onClick={() => setBrowsing(true)}>Browse…</Button>
              </div>
              {browsing && (
                <PathBrowser title="Choose where files live" initial={root || st?.root} defaultOwner={owner || st?.owner}
                  onSelect={(p) => { setRoot(p); setBrowsing(false); }}
                  onClose={() => setBrowsing(false)} />
              )}
              <Field
                label="Owner of new files (uid:gid)"
                value={owner}
                onChange={(e) => edit("files_owner", e.target.value)}
                placeholder="1000:1000"
                hint="Applied to what Drive creates, so a folder handed to an app as a volume is usable without a chown."
              />
            </>
          )}

          {needsMove && (
            <div className="space-y-2 rounded-lg border border-warn/30 bg-warn/10 p-3 text-sm">
              <p className="text-warn">
                The current location has files in it. Move them to <code className="font-mono">{needsMove}</code>, or adopt whatever is already there and leave the current files where they are?
              </p>
              <div className="flex flex-wrap gap-2">
                <Button variant="primary" size="sm" onClick={() => move.mutate("move")} disabled={move.isPending}>Move files there</Button>
                <Button size="sm" onClick={() => move.mutate("adopt")} disabled={move.isPending}>Adopt that folder</Button>
                <Button variant="ghost" size="sm" onClick={() => setNeedsMove(null)}>Cancel</Button>
              </div>
            </div>
          )}

          {job.data && (
            <JobLine job={job.data} onDismiss={() => setJobId(null)} />
          )}

          {error && <p className="text-sm text-warn">{error}</p>}
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="primary" onClick={submit} disabled={save.isPending || loading || !settings.data}>
              {saved || (save.isPending ? "Saving…" : "Save")}
            </Button>
            <Button onClick={() => reindex.mutate()} disabled={reindex.isPending || !!st?.walking}>
              {st?.walking ? "Indexing…" : "Reindex"}
            </Button>
          </div>
        </div>
      </Section>
      <PlacementSection />
      <ListingSection />
      <DriveAccess />
    </SettingsPage>
  );
}

function JobLine({ job, onDismiss }: { job: FilesJob; onDismiss: () => void }) {
  const pct = job.total > 0 ? Math.min(100, Math.round((job.done / job.total) * 100)) : null;
  return (
    <div className="space-y-1 rounded-lg border border-veil-edge bg-veil p-3 text-sm">
      <div className="flex items-center gap-2">
        {job.state === "running" && <Spinner />}
        <span className={job.state === "failed" ? "text-warn" : "text-ink"}>
          {job.label}
          {job.state === "running" && job.current ? ` — ${job.current}` : ""}
          {job.state === "done" && " — done"}
          {job.state === "failed" && ` — failed: ${job.error}`}
          {job.state === "cancelled" && " — cancelled"}
        </span>
        {job.state !== "running" && (
          <Button variant="ghost" size="sm" className="ml-auto" onClick={onDismiss}>Dismiss</Button>
        )}
      </div>
      {job.state === "running" && pct !== null && (
        <div className="h-1.5 overflow-hidden rounded-full bg-fog-800" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
          <div className="h-full bg-accent" style={{ width: `${pct}%` }} />
        </div>
      )}
    </div>
  );
}

// Apps and Content can each live on a disk of their own — the apps' data on
// something small and fast, the libraries on something big — and still be
// the same two folders in Drive. Backups sit beside them: not in Drive, but
// the same question of which disk, and the same move-or-adopt answer. Each
// row says where its folder is now; Change opens the choice.
const placementHints: Record<Placement["folder"], string> = {
  Apps: "Each app's own data — settings and databases. Small, and quicker on a fast disk.",
  Content: "The household's libraries — films, photos, music. Large; room matters more than speed.",
  Backups: "Every app snapshot, Drive snapshot and recovery kit. On a NAS, they outlive this machine.",
};

function PlacementSection() {
  const qc = useQueryClient();
  const status = useFilesStatus();
  const [editing, setEditing] = useState<Placement["folder"] | null>(null);
  const [to, setTo] = useState("");
  const [browsing, setBrowsing] = useState(false);
  const [error, setError] = useState("");
  const [jobId, setJobId] = useState<string | null>(null);

  const start = useMutation({
    mutationFn: (body: { folder: Placement["folder"]; to: string; mode: "move" | "adopt" }) =>
      post<FilesJob>("/api/files/placement", body),
    onSuccess: (job) => {
      setJobId(job.id);
      setEditing(null);
      setTo("");
      setError("");
    },
    onError: (e) => setError(errorText(e, "Could not start the move")),
  });
  const job = useJob<FilesJob>("files", jobId, {
    onDone: () => Promise.all([
      qc.invalidateQueries({ queryKey: filesKey }),
      qc.invalidateQueries({ queryKey: disksQuery.queryKey }),
    ]),
  });
  const running = job.data?.state === "running" || start.isPending;
  const placements = status.data?.placements ?? [];
  const current = placements.find((p) => p.folder === editing);

  const open = (p: Placement) => {
    setEditing(p.folder);
    setTo(p.default ? "" : p.location ?? "");
    setError("");
  };
  const backups = editing === "Backups";
  const go = (mode: "move" | "adopt", dest = to.trim()) => {
    if (editing) start.mutate({ folder: editing, to: dest, mode });
  };

  return (
    <Section id="placement" title="Apps, Content and Backups">
      <div className="max-w-lg space-y-4">
        {status.isPending ? (
          <Loading label="Reading where Apps, Content and Backups live…" />
        ) : status.isError ? (
          <LoadError label="Could not read where Apps, Content and Backups live." error={status.error} onRetry={() => status.refetch()} />
        ) : (
          <SettingRows>
            {placements.map((p) => (
              <SettingRow key={p.folder} label={p.folder} hint={<>{placementHints[p.folder]}<PlacementWhere p={p} /></>}>
                <Button size="sm" onClick={() => open(p)} disabled={running || editing === p.folder}>Change…</Button>
              </SettingRow>
            ))}
          </SettingRows>
        )}

        {editing && current && (
          <div className="space-y-3 rounded-lg border border-veil-edge bg-veil p-3 text-sm">
            <div className="flex items-end gap-2">
              <div className="flex-1">
                <Field
                  label={backups ? "Where backups are kept" : `Where ${editing} lives`}
                  value={to}
                  onChange={(e) => setTo(e.target.value)}
                  placeholder="/mnt/nas/…"
                  hint={backups
                    ? "A folder on this server, outside the Drive location — usually on a NAS share, so backups leave the box."
                    : "A folder on this server, outside the Drive location — usually on a NAS share."}
                />
              </div>
              <Button className="mb-5 shrink-0" onClick={() => setBrowsing(true)}>Browse…</Button>
            </div>
            {browsing && (
              <PathBrowser title={backups ? "Choose where backups are kept" : `Choose where ${editing} lives`} initial={to || undefined}
                onSelect={(p) => { setTo(p); setBrowsing(false); }}
                onClose={() => setBrowsing(false)} />
            )}
            {backups ? (
              <p className="text-ink-muted">
                <strong className="font-medium text-ink">Move</strong> copies every backup there, then removes the old copy.{" "}
                <strong className="font-medium text-ink">Adopt</strong> uses the backups already there — copied by hand, or kept from before a rebuild — and leaves the old place alone. New backups wait until it's done.
              </p>
            ) : (
              <p className="text-ink-muted">
                <strong className="font-medium text-ink">Move</strong> copies everything in {editing} there, then removes the old copy.{" "}
                <strong className="font-medium text-ink">Adopt</strong> uses what is already there — a library that is on the NAS today. Apps that use {editing} stop until it's done.
              </p>
            )}
            <div className="flex flex-wrap gap-2">
              <Button variant="primary" size="sm" onClick={() => go("move")} disabled={running || !to.trim() || to.trim() === current.location}>Move files there</Button>
              <Button size="sm" onClick={() => go("adopt")} disabled={running || !to.trim() || to.trim() === current.location}>Adopt that folder</Button>
              {backups ? (
                !current.default && <Button size="sm" onClick={() => go("move", "")} disabled={running}>Move back to the default</Button>
              ) : current.location && (
                <Button size="sm" onClick={() => go("move", "")} disabled={running}>Move back into Drive</Button>
              )}
              <Button variant="ghost" size="sm" onClick={() => { setEditing(null); setError(""); }}>Cancel</Button>
            </div>
          </div>
        )}

        {job.data && <JobLine job={job.data} onDismiss={() => setJobId(null)} />}
        {error && <p className="text-sm text-warn">{error}</p>}
      </div>
    </Section>
  );
}

// Where one folder is, in the row's hint: inside Drive, or the location it
// is bound from with that disk's room — or why it is not there right now.
// Backups always name their directory, and say when it is fog's default.
function PlacementWhere({ p }: { p: Placement }) {
  const disk = p.fsMount === "/" ? "the system disk" : p.fsMount;
  const free = p.freeBytes !== undefined ? ` · ${formatBytes(p.freeBytes)} free` : "";
  const closed = p.folder === "Backups" ? "Backups, restores and verifies wait for it." : `Drive keeps ${p.folder} closed until it is back.`;
  return (
    <span className="mt-1 block text-xs">
      {p.folder === "Backups" && p.default && p.bound ? (
        <span className="text-ink-muted">
          In fog's data directory, <code className="font-mono text-ink-muted">{p.location}</code>, on {disk}{free}.
        </span>
      ) : !p.location ? (
        <span className="text-ink-muted">Inside the Drive location, on {disk}{free}.</span>
      ) : p.bound ? (
        <span className="text-ink-muted">
          <code className="font-mono text-ink-muted">{p.location}</code>, on {disk}{free}.
        </span>
      ) : (
        <span className="text-warn">
          ⚠ <code className="font-mono">{p.location}</code> is not available{p.error ? `: ${p.error}` : ""}. {closed}
        </span>
      )}
    </span>
  );
}

// What Drive's listing shows. These hide things that are really on disk,
// so each row says what disappears; like every switch, they apply on
// click, and the listing re-reads so the change shows at once.
function ListingSection() {
  const qc = useQueryClient();
  const onSaved = () => void qc.invalidateQueries({ queryKey: filesKey });
  const hidden = useSettingToggle("files_show_hidden", { errorLabel: "Couldn't save the preference", onSaved });
  const system = useSettingToggle("files_show_system", { errorLabel: "Couldn't save the preference", onSaved });
  return (
    <Section id="listing" title="What the listing shows">
      <SettingRows>
        <SettingRow label="Hidden files" hint="Names beginning with a dot — .env, .gitignore. Off by default." error={hidden.error}>
          <Toggle checked={hidden.value === "on"} disabled={hidden.value === undefined || hidden.isPending}
            onChange={(on) => hidden.set(on ? "on" : "off")} aria-label="Show hidden files" />
        </SettingRow>
        <SettingRow
          label="System files"
          hint="The bookkeeping other systems leave in every folder they touch: @eaDir from a Synology, Thumbs.db from Windows, .DS_Store from macOS. Fog never deletes these — it just keeps them out of the way."
          error={system.error}
        >
          <Toggle checked={system.value === "on"} disabled={system.value === undefined || system.isPending}
            onChange={(on) => system.set(on ? "on" : "off")} aria-label="Show system files" />
        </SettingRow>
      </SettingRows>
    </Section>
  );
}
