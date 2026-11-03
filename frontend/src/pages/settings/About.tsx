// Settings › Server › About: this box. What it calls itself, and the facts
// about the machine under it — version, platform, where its state lives —
// with anything about the environment that needs attention on top.

import { useQuery } from "@tanstack/react-query";
import { Loading } from "../../components/Async";
import Field from "../../components/Field";
import { formatBytes, timeAgo, versionLabel } from "../../lib/format";
import { SettingsPage } from "./Frame";
import { useSettingsForm } from "./useSettingsForm";
import { systemQuery } from "../../queries";

export default function About() {
  const { value, set, SaveButton, loading } = useSettingsForm(["server_name"]);
  const system = useQuery({
    ...systemQuery,
    refetchInterval: 30_000,
  });
  const d = system.data;

  return (
    <SettingsPage>
      <div className="max-w-lg space-y-4">
        {loading ? (
          <Loading label="Loading settings…" />
        ) : (
          <Field label="Server name" value={value("server_name")} onChange={set("server_name")}
            hint="How this fog names itself — in notifications, on the Connect map, and to peers." />
        )}
        {SaveButton}
      </div>
      {(d?.warnings ?? []).length > 0 && (
        <div className="space-y-2 rounded-lg border border-warn/30 bg-warn/10 p-4">
          <p className="text-sm font-semibold text-warn">Environment needs attention</p>
          {d!.warnings!.map((w, i) => (
            <p key={i} className="break-words font-mono text-xs text-warn">{w}</p>
          ))}
        </div>
      )}
      {/* Facts about the host wait for the host to state them. */}
      {!d ? (
        system.isError ? (
          <p className="text-sm text-warn">Couldn't read this server's status.</p>
        ) : (
          <Loading label="Reading server status…" />
        )
      ) : (
        <dl className="grid grid-cols-2 gap-x-6 gap-y-3 text-sm sm:grid-cols-3">
          <Fact label="fogd" value={versionLabel(d.version)} />
          <Fact label="Hostname" value={d.hostname} />
          <Fact label="Platform" value={`${d.os}/${d.arch}`} />
          <Fact label="Up since" value={`${new Date(d.startedAt).toLocaleString()} (${timeAgo(d.startedAt)})`} />
          <Fact label="State directory" value={d.dataDir} mono />
          <Fact
            label="Docker"
            value={d.docker.available ? (d.docker.version ? `${d.docker.version.Version}` : "available") : "not available"}
          />
          {d.docker.info && (
            <Fact label="Containers" value={`${d.docker.info.ContainersRunning} running of ${d.docker.info.Containers}`} />
          )}
          {d.disk && <Fact label="Disk" value={`${formatBytes(d.disk.freeBytes)} free of ${formatBytes(d.disk.totalBytes)}`} />}
        </dl>
      )}
    </SettingsPage>
  );
}

function Fact({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="min-w-0">
      <dt className="text-cell text-ink-muted">{label}</dt>
      <dd className={`mt-0.5 break-words text-ink ${mono ? "font-mono text-xs" : ""}`}>{value}</dd>
    </div>
  );
}

