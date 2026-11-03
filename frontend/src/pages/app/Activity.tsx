// The activity tab: what fog itself has done to this app, newest first —
// deploys, restarts, snapshots, restores, error triage. Every row is an
// audit entry said in a few words with a tone dot, the time, and who did
// it when it wasn't fog. The system-wide log is one link away.

import { Link } from "react-router-dom";
import type { AuditEntry } from "../../api";
import { Loading } from "../../components/Async";
import { auditTone } from "../../lib/audit";
import { paths } from "../../lib/paths";
import { useAppAudit } from "../../lib/useAppData";
import { Dot, Group, whenShort } from "./cockpit";

export function ActivityTab({ name }: { name: string }) {
  const audit = useAppAudit(name);
  return (
    <Group
      title="Fog activity"
      first
      right={<Link to={paths.settings("server/activity")} className="text-hint font-medium text-ink-muted hover:text-ink">All activity</Link>}
    >
      <Activity entries={audit.data} loading={!audit.data && !audit.isError} error={audit.isError} />
    </Group>
  );
}

function Activity({ entries, loading, error }: { entries?: AuditEntry[]; loading: boolean; error: boolean }) {
  if (loading) return <Loading label="Reading fog's activity on this app…" />;
  if (error) return <p className="text-hint text-warn">Couldn't read the activity log.</p>;
  const rows = entries ?? [];
  if (rows.length === 0) return <p className="text-hint text-ink-muted">Fog has not acted on this app yet.</p>;
  return (
    <div className="flex flex-col">
      {rows.map((e) => {
        const sev = auditTone(e.action);
        const actor = e.actor === "system" || e.actor === "webhook" || e.actor.startsWith("app:") ? "" : e.actor;
        return (
          <div key={e.id} className="flex items-center gap-3 border-b border-divider py-[9px]" title={`${e.action}${e.detail ? ` ${e.detail}` : ""} · ${e.actor}`}>
            <Dot tone={sev === "warn" ? "warn" : sev === "ok" ? "ok" : "off"} />
            <span className="w-32 shrink-0 font-mono text-hint text-ink-muted">{whenShort(e.at)}</span>
            <span className="min-w-0 truncate text-cell text-ink">{sentence(e)}</span>
            {actor && <span className="ml-auto shrink-0 text-hint text-ink-muted">{actor}</span>}
          </div>
        );
      })}
    </div>
  );
}

// The audit verb as a sentence in fog's vocabulary. The detail column is
// JSON when it carries anything; the words that matter are lifted out.
function sentence(e: AuditEntry): string {
  let d: Record<string, unknown> = {};
  try {
    if (e.detail) d = JSON.parse(e.detail) as Record<string, unknown>;
  } catch {
    // free text detail — appended below
  }
  const kind = typeof d.kind === "string" ? d.kind : "";
  switch (e.action) {
    case "app.create": return "Created";
    case "app.start": return "Started";
    case "app.stop": return "Stopped";
    case "app.restart": return "Restarted";
    case "app.deploy": return "Redeployed";
    case "app.rollback": return d.rev ? `Rolled back to rev ${d.rev}` : "Rolled back";
    case "app.update": return d.rev ? `Updated · rev ${d.rev}` : "Updated";
    case "app.update.skip-snapshot": return "Update started without its snapshot";
    case "app.deploy.cancel": return `Cancelled${typeof d.stage === "string" && d.stage ? ` while ${d.stage}` : ""}`;
    case "app.deploy.webhook": return "Deployed from a push";
    case "app.update-check.webhook": return "Update check requested by the app";
    case "app.deploykey.regenerate": return "Deploy key regenerated";
    case "app.delete": return d.deleteData ? "Deleted with its data" : "Deleted, data kept";
    case "backup.create": return `Snapshot taken${kind ? ` · ${kind}` : ""}${d.mode === "delegated" ? " · delegated" : ""}`;
    case "backup.restore": return "Snapshot restored";
    case "backup.restore.staged": return "Restore staged for the next boot";
    case "backup.restore.cancel": return "Restore cancelled";
    case "app.errors.resolve": return `Error group${d.group ? ` #${d.group}` : ""} resolved${d.nextDeploy ? " until the next deploy" : ""}`;
    case "app.errors.ignore": return `Error group${d.group ? ` #${d.group}` : ""} ignored`;
    case "app.errors.unresolve": return `Error group${d.group ? ` #${d.group}` : ""} reopened`;
    case "connect.request": return d.ok === false ? `Could not connect to ${e.subject}${d.app ? ` (${d.app})` : ""}` : `Connected to ${e.subject}${d.app ? ` (${d.app})` : ""}`;
    case "connect.serve": return d.ok === false ? `Refused a connection from ${e.subject}${d.app ? ` to ${d.app}` : ""}${d.reason ? `: ${d.reason}` : ""}` : `Connected ${e.subject}${d.from ? ` (${d.from})` : ""} to ${d.app ?? "an app here"}`;
    case "offsite.fetch": return `Snapshot fetched from ${typeof d.target === "string" ? d.target : "the locker"}`;
    default: {
      const rest = Object.entries(d).map(([k, v]) => `${k} ${String(v)}`).join(" · ");
      return `${e.action}${rest ? ` · ${rest}` : e.detail && !Object.keys(d).length ? ` · ${e.detail}` : ""}`;
    }
  }
}
