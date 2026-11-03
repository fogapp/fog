// Settings › Server › Docker. The engine every app runs in: its version
// and what it holds when it is there; the one-click installer, shared with
// the first-run wizard, when it isn't.

import { useQuery } from "@tanstack/react-query";
import DockerInstall from "../../components/DockerInstall";
import { StatusLine } from "../../components/Section";
import { formatBytes } from "../../lib/format";
import { useDependencies } from "./ContainerFacts";
import { SettingsPage } from "./Frame";
import { systemQuery } from "../../queries";

export default function Docker() {
  const system = useQuery({ ...systemQuery, refetchInterval: 30_000 });
  const deps = useDependencies();
  const d = system.data?.docker;
  const info = d?.info;
  const df = deps.data?.docker;
  return (
    <SettingsPage
      description="Every app is a container; without Docker nothing runs. Fog installs it on a Linux server and leaves it alone afterwards."
      status={
        !system.data ? (
          <StatusLine>{system.isError ? "Status unavailable." : "Checking…"}</StatusLine>
        ) : (
          <StatusLine tone={d?.available ? "ok" : "warn"}>
            {d?.available ? `Docker ${d.version?.Version ?? "running"}` : "Docker is not reachable."}
          </StatusLine>
        )
      }
    >
      <div className="space-y-4">
        <DockerInstall />
        {d?.available && (
          <dl className="grid gap-x-10 sm:grid-cols-2">
            {[
              ["Version", d.version?.Version ?? info?.ServerVersion ?? "—"],
              ["Containers", info ? `${info.ContainersRunning} running of ${info.Containers}` : "—"],
              ["Images", info ? `${info.Images}${df?.imagesBytes !== undefined ? ` · ${formatBytes(df.imagesBytes)} on disk` : ""}` : "—"],
              ["Reclaimable", df?.reclaimableBytes !== undefined ? formatBytes(df.reclaimableBytes) : "—"],
              ["Host", info ? `${info.OperatingSystem} · ${info.NCPU} ${info.NCPU === 1 ? "core" : "cores"} · ${formatBytes(info.MemTotal)}` : "—"],
            ].map(([k, v]) => (
              <div key={k} className="flex items-baseline justify-between gap-4 border-b border-divider py-2">
                <dt className="text-hint text-ink-muted">{k}</dt>
                <dd className="text-right text-cell text-ink">{v}</dd>
              </div>
            ))}
          </dl>
        )}
        {d?.available && df?.reclaimableBytes !== undefined && df.reclaimableBytes > 0 && (
          <p className="text-hint text-ink-muted">
            Reclaimable is image layers and build cache that nothing running uses — old versions apps have moved on from.
          </p>
        )}
      </div>
    </SettingsPage>
  );
}
