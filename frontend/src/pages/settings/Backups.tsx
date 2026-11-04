// Settings › Storage › Backups: the one place that answers "is everything
// backed up?" — every app's posture in one table, and the policy every app
// falls back to. Each app's own snapshots and schedule live on its Backups
// tab and link back here; the copy off the machine is Offsite, and what a
// rebuild needs is the Recovery kit.

import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { post, type FilesBackupStatus } from "../../api";
import Button from "../../components/Button";
import Field from "../../components/Field";
import SettingRow, { SettingRows } from "../../components/SettingRow";
import Toggle from "../../components/Toggle";
import Segmented from "../../components/Segmented";
import { Section } from "../../components/Section";
import { LoadError, Loading, StaleNote, errorText, phaseOf } from "../../components/Async";
import { formatBytes, timeAgo, timeUntil } from "../../lib/format";
import { paths } from "../../lib/paths";
import { useFlash } from "../../lib/useFlash";
import { useSettingToggle } from "../../lib/useSettingToggle";
import { useFilesStatus } from "../../lib/useFiles";
import { SettingsPage } from "./Frame";
import { useSettingsForm } from "./useSettingsForm";
import { backupsStatusQuery } from "../../queries";

export default function Backups() {
  return (
    <SettingsPage>
      <StatusSection />
      <PolicySection />
    </SettingsPage>
  );
}

// The fleet table. Stale rows are the point of the page — they get the
// loudest treatment and sort to the top; everything else reads in the
// order the sidebar lists apps.
function StatusSection() {
  const qc = useQueryClient();
  const [note, flash] = useFlash();
  const status = useQuery({
    ...backupsStatusQuery,
    refetchInterval: 30_000,
  });
  const filesSnapshot = useMutation({
    mutationFn: () => post("/api/files/snapshots"),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: backupsStatusQuery.queryKey });
      flash("Drive snapshot started");
    },
    onError: (e) => flash(errorText(e, "Snapshot failed to start")),
  });
  const backup = useMutation({
    mutationFn: (name: string) => post(`/api/apps/${name}/backups`),
    // Awaited: the row's "last" column is the proof, so the button says
    // "Backing up…" until the refreshed table is on screen.
    onSuccess: async (_d, name) => {
      await qc.invalidateQueries({ queryKey: backupsStatusQuery.queryKey });
      flash(`Backed up ${name} ✓`);
    },
    onError: (e) => flash(errorText(e, "Backup failed")),
  });

  const rows = [...(status.data?.apps ?? [])].sort((a, b) => Number(b.stale) - Number(a.stale));
  const phase = phaseOf(status, status.data?.apps.length);
  const files = status.data?.files;
  const stale = rows.filter((r) => r.stale).length + (files?.stale ? 1 : 0);
  const off = rows.filter((r) => !r.enabled).length;

  return (
    <Section
      id="status"
      title="Status"
      description={
        !status.data
          ? undefined
          : stale > 0
            ? `${stale} overdue — scheduled backups that have not landed.`
            : off > 0
              ? `Everything scheduled is current. ${off} app${off === 1 ? " is" : "s are"} not backed up at all.`
              : "Every app is backed up and current."
      }
      actions={note ? <span className="text-xs text-ink-muted">{note}</span> : undefined}
    >
      {files && (
        <FilesRow files={files} onSnapshot={() => filesSnapshot.mutate()} busy={filesSnapshot.isPending} />
      )}
      {phase === "loading" ? (
        <Loading label="Reading backup status…" />
      ) : phase === "error" ? (
        <LoadError label="Couldn't read backup status." error={status.error} onRetry={() => status.refetch()} />
      ) : phase === "empty" ? (
        <p className="text-sm text-ink-muted">No apps yet — Drive is the only thing to back up.</p>
      ) : (
        <div className="overflow-x-auto rounded-lg border border-veil-edge bg-veil">
          <table className="w-full text-left text-cell">
            <thead>
              <tr className="caps-label border-b border-hair-strong">
                <th className="px-4 py-2.5 font-medium">App</th>
                <th className="px-3 py-2.5 font-medium">Schedule</th>
                <th className="px-3 py-2.5 font-medium">Last backup</th>
                <th className="hidden px-3 py-2.5 font-medium md:table-cell">Size</th>
                <th className="hidden px-3 py-2.5 font-medium md:table-cell">Mode</th>
                <th className="hidden px-3 py-2.5 font-medium md:table-cell">Offsite</th>
                <th className="px-3 py-2.5" />
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => {
                const busy = backup.isPending && backup.variables === r.app;
                return (
                  <tr key={r.app} className={`border-b border-divider last:border-0 ${r.stale ? "bg-warn/10" : ""}`}>
                    <td className="px-4 py-2.5">
                      <Link to={paths.app(r.app, "drive")} className="font-medium text-ink hover:underline">{r.app}</Link>
                    </td>
                    <td className="px-3 py-2.5 text-xs">
                      {!r.enabled ? (
                        <span className="text-ink-muted">off</span>
                      ) : r.hourUTC === null ? (
                        <span className="text-warn">enabled, no hour set</span>
                      ) : (
                        <span className="text-ink-muted">
                          daily {String(r.hourUTC).padStart(2, "0")}:00 UTC
                          {r.nextAt && <span className="text-ink-muted"> · next {timeUntil(r.nextAt)}</span>}
                        </span>
                      )}
                    </td>
                    <td className="px-3 py-2.5 text-xs">
                      {r.lastAt ? (
                        <span className={r.stale ? "font-medium text-warn" : "text-ink-muted"}>
                          {r.stale ? "⚠ " : ""}{timeAgo(r.lastAt)}
                          {r.lastVerifiedAt && <span className="ml-1.5 rounded-full bg-accent/10 px-2 py-px text-[11px] font-medium text-accent">Verified</span>}
                        </span>
                      ) : r.stale ? (
                        <span className="font-medium text-warn">⚠ never</span>
                      ) : r.hourUTC !== null ? (
                        // Scheduled but never run: the first backup is still
                        // ahead of it, not overdue behind it.
                        <span className="text-ink-muted">awaiting first</span>
                      ) : (
                        <span className="text-ink-muted">never</span>
                      )}
                    </td>
                    <td className="hidden px-3 py-2.5 text-xs text-ink-muted md:table-cell">
                      {r.lastSizeBytes ? formatBytes(r.lastSizeBytes) : "—"}
                    </td>
                    <td className="hidden px-3 py-2.5 text-xs text-ink-muted md:table-cell">{r.mode}</td>
                    <td className="hidden px-3 py-2.5 text-xs md:table-cell">
                      {!r.offsite ? (
                        <span className="text-ink-faint">—</span>
                      ) : r.offsite === "verified" ? (
                        <span className="text-accent" title="Fetched back from the locker and test-restored">✓ verified</span>
                      ) : r.offsite === "stored" ? (
                        <span className="text-accent">✓ copied</span>
                      ) : r.offsite === "uploading" ? (
                        <span className="text-accent">↑ uploading</span>
                      ) : (
                        <span className="text-ink-muted">pending</span>
                      )}
                    </td>
                    <td className="px-3 py-2.5 text-right">
                      <Button size="sm" onClick={() => backup.mutate(r.app)} disabled={backup.isPending}>
                        {busy ? "Backing up…" : "Back up now"}
                      </Button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {status.isError && status.data && <StaleNote what="this table" />}
    </Section>
  );
}

// The Files line of the fleet table: fog's own root, backed up nightly
// into the same store as the apps.
function FilesRow({ files, onSnapshot, busy }: { files: FilesBackupStatus; onSnapshot: () => void; busy: boolean }) {
  return (
    <div className={`flex flex-wrap items-center gap-x-4 gap-y-1 rounded-lg border px-4 py-3 text-sm ${files.stale ? "border-warn/30 bg-warn/10" : "border-veil-edge bg-veil"}`}>
      <Link to={paths.files()} className="font-medium text-ink hover:underline">Drive</Link>
      <span className="text-xs text-ink-muted">
        {!files.enabled ? (
          <span className="text-ink-muted">off</span>
        ) : files.hourUTC === null ? (
          <span className="text-warn">on, no hour set</span>
        ) : (
          <>nightly at {String(files.hourUTC).padStart(2, "0")}:15 UTC{files.nextAt ? ` · next ${timeUntil(files.nextAt)}` : ""}</>
        )}
      </span>
      <span className={`text-xs ${files.stale ? "font-medium text-warn" : "text-ink-muted"}`}>
        {files.running ? (
          "snapshot running…"
        ) : files.lastAt ? (
          <>
            {files.stale ? "⚠ " : ""}last {timeAgo(files.lastAt)} · {files.lastFiles?.toLocaleString()} files · {formatBytes(files.lastBytes ?? 0)}
            {files.lastNewBytes !== undefined && ` (${formatBytes(files.lastNewBytes)} new)`}
            {files.lastMode === "mirror" && " · mirror, no history"}
            {files.lastWarnings ? ` · ${files.lastWarnings} torn` : ""}
            {files.lastVerifiedAt ? " · verified" : ""}
            {files.offsite === "verified" ? " · ☁ verified offsite" : files.offsite === "stored" ? " · ☁ copied offsite" : files.offsite === "uploading" ? " · ↑ uploading offsite" : files.offsite === "pending" ? " · offsite pending" : ""}
          </>
        ) : files.enabled ? (
          <>{files.stale ? "⚠ " : ""}never</>
        ) : null}
      </span>
      {files.excluded > 0 && <span className="text-xs text-ink-muted">{files.excluded} excluded</span>}
      {files.error && <span className="text-xs text-warn">{files.error}</span>}
      <span className="ml-auto flex items-center gap-2">
        <span className="text-xs text-ink-muted">{files.snapshots} snapshot{files.snapshots === 1 ? "" : "s"}</span>
        <Button size="sm" onClick={onSnapshot} disabled={busy || files.running || !files.enabled}>
          {busy ? "Starting…" : "Snapshot now"}
        </Button>
      </span>
    </div>
  );
}

// Where backups are kept is changed in Drive's settings, beside Apps and
// Content, as a job that moves them or adopts what is already there — a
// bare setting would leave every snapshot behind. Here it is only named,
// with the way there.
function BackupLocation() {
  const status = useFilesStatus();
  const p = status.data?.placements.find((x) => x.folder === "Backups");
  return (
    <div>
      <span className="field-label mb-1.5 block">Backup directory</span>
      <p className="text-sm text-ink-muted">
        {p ? (
          <>
            <code className="font-mono text-ink-muted">{p.location}</code>
            {p.default && " — fog's default, on this machine"}
            {!p.bound && p.error && <span className="text-warn"> — ⚠ {p.error}</span>}
          </>
        ) : status.isError ? (
          "Could not read where backups are kept."
        ) : (
          "Reading…"
        )}
      </p>
      <p className="mt-1.5 text-hint text-ink-muted">
        <Link to={paths.settings("storage/drive", "placement")} className="underline">Change where backups are kept</Link> — on a NAS share, snapshots leave the box.
      </p>
    </div>
  );
}

const HISTORY = [
  { id: "month", label: "A month" },
  { id: "quarter", label: "3 months" },
  { id: "year", label: "A year" },
  { id: "two-years", label: "2 years" },
] as const;

// What the backups add up to, in a sentence: how far back they go and what
// they take on the disk. How they are stored is fog's business.
function HistoryLine() {
  const status = useQuery(backupsStatusQuery);
  const s = status.data?.store;
  if (!s) return null;
  return (
    <p className="text-sm text-ink-muted">
      {s.oldestAt ? <>Backups go back to {new Date(s.oldestAt).toLocaleDateString(undefined, { dateStyle: "long" })}</> : "No backups yet"}
      {s.usedBytes > 0 && <> · {formatBytes(s.usedBytes)} on the backup disk</>}
      {s.encrypted && " · encrypted"}
    </p>
  );
}

function PolicySection() {
  const { value, set, SaveButton, loading } = useSettingsForm(["backup_hour_utc"]);
  // The switches apply on click, like every switch: only the field above
  // waits for Save.
  const beforeUpdate = useSettingToggle("backup_before_update", { errorLabel: "Couldn't save the preference" });
  const drive = useSettingToggle("files_backup", { errorLabel: "Couldn't save the preference" });
  const history = useSettingToggle("backup_history", { errorLabel: "Couldn't save the preference" });
  const encrypt = useSettingToggle("backup_store_encrypt", { errorLabel: "Couldn't save the preference" });
  const status = useQuery(backupsStatusQuery);
  const pending = status.data?.store?.encryptPending;

  return (
    <Section
      id="policy"
      title="Policy"
      description="The defaults every app follows unless its own Backups → Schedule says otherwise."
    >
      <div className="max-w-lg space-y-4">
        {loading ? (
          <Loading label="Loading settings…" />
        ) : (
          <>
            <BackupLocation />
            <HistoryLine />
            <div className="grid grid-cols-2 gap-4">
              <Field label="Nightly backup hour (UTC)" value={value("backup_hour_utc")} onChange={set("backup_hour_utc")}
                placeholder="empty = off" />
            </div>
          </>
        )}
        {SaveButton}
      </div>
      <SettingRows>
        <SettingRow
          label="Keep history for"
          hint="Every night for the last two weeks, then one a week, then one a month. Each backup only costs what changed since the last, so a long history takes little room. If the backup disk fills up, the oldest go first."
          error={history.error}
        >
          <Segmented
            label="Keep history for"
            options={HISTORY}
            value={history.value === undefined ? undefined : ((history.value || "year") as (typeof HISTORY)[number]["id"])}
            disabled={history.isPending}
            onChange={(v) => history.set(v)}
          />
        </SettingRow>
        <SettingRow
          label="Encrypt backups on the backup disk"
          hint={pending
            ? "Fog applies this tonight, after the nightly backups."
            : "Anyone who can read the backup disk can then read nothing without this server's recovery kit. Off by default: an unencrypted disk can be restored even if the recovery kit is lost."}
          error={encrypt.error}
        >
          <Toggle
            checked={encrypt.value === "on"}
            disabled={encrypt.value === undefined || encrypt.isPending}
            onChange={(on) => encrypt.set(on ? "on" : "off")}
            aria-label="Encrypt backups on the backup disk"
          />
        </SettingRow>
        <SettingRow
          label="Snapshot apps before updates and rollbacks"
          hint="Applies to apps with backups on. An app can override this on its own Backups → Schedule."
          error={beforeUpdate.error}
        >
          <Toggle
            checked={beforeUpdate.value !== undefined && beforeUpdate.value !== "off"}
            disabled={beforeUpdate.value === undefined || beforeUpdate.isPending}
            onChange={(on) => beforeUpdate.set(on ? "on" : "off")}
            aria-label="Snapshot apps before updates and rollbacks"
          />
        </SettingRow>
        <SettingRow
          label={<>Include <Link to={paths.files()} className="underline">Drive</Link> in nightly backups</>}
          hint="Every night is a full copy that costs only what changed. Any file or folder can be left out from its detail pane."
          error={drive.error}
        >
          <Toggle
            checked={drive.value !== undefined && drive.value !== "off"}
            disabled={drive.value === undefined || drive.isPending}
            onChange={(on) => drive.set(on ? "on" : "off")}
            aria-label="Include Drive in nightly backups"
          />
        </SettingRow>
      </SettingRows>
    </Section>
  );
}
